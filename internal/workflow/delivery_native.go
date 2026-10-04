package workflow

import (
	"context"
	"errors"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/snapshot"
)

// NativeDelivery composes the existing typed governance primitives with PR
// creation, exact-head merge and an explicit Git ref lease. It installs no
// extension, changes no authentication, and never retries a provider mutation.
type NativeDelivery struct{ NativeGovernance }

func (n NativeDelivery) DeliveryInventory(ctx context.Context, request DeliveryInventoryRequest) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("native delivery requires a transport")
	}
	if request.Kind != "open-pr" && request.Kind != "finish" {
		return nil, errors.New("delivery requires a supported typed kind")
	}
	policy, err := ParseDeliveryPolicy(request.Kind, request.Policy, n.Transport.Repository)
	if err != nil {
		return nil, err
	}
	request.Policy = policy
	state, err := contract.ObjectAt(request.Policy, "execution_state")
	if err != nil {
		return nil, err
	}
	canonical, parsed, err := parseGovernancePolicy("execution-state", state, n.Transport.Repository)
	if err != nil {
		return nil, err
	}
	backlogRequest, err := governanceInventoryRequest("execution-state", canonical, parsed, n.Transport.Repository, "")
	if err != nil {
		return nil, err
	}
	repository, err := n.Transport.ReadDeliveryRepository(ctx)
	if err != nil {
		return nil, err
	}
	defaultRef, _ := contract.ObjectAt(repository, "defaultBranchRef")
	backlog, err := n.BacklogInventory(ctx, backlogRequest)
	if err != nil {
		return nil, err
	}
	provenance, err := contract.ObjectAt(backlog, "provenance")
	if err != nil || provenance["repository_node_id"] != repository["id"] {
		return nil, errors.New("delivery backlog repository incarnation changed")
	}
	result := contract.Object{"repo": n.Transport.Repository.Object(), "repository_node_id": repository["id"], "default_branch": defaultRef["name"], "auto_delete_branch": repository["deleteBranchOnMerge"],
		"backlog_inventory": backlog, "head_branch": nil, "base_branch": nil, "head_pull_requests": nil, "pull_request": nil, "required_checks": []any{}, "base_dependents": nil,
		"provenance": contract.Object{"live": true, "complete": true, "source": "github_api", "repository_node_id": repository["id"], "request": request.Object()}}
	if request.Kind == "open-pr" {
		draft, err := contract.ObjectAt(request.Policy, "draft")
		if err != nil {
			return nil, err
		}
		for _, choice := range []struct{ input, output string }{{"head_branch", "head_branch"}, {"base_branch", "base_branch"}} {
			name, err := contract.Nonempty(draft, choice.input)
			if err != nil {
				return nil, err
			}
			branch, err := n.Transport.ReadBranch(ctx, name)
			if err != nil {
				return nil, err
			}
			if branch["repository_node_id"] != repository["id"] {
				return nil, errors.New("delivery branch repository incarnation changed")
			}
			result[choice.output] = branch
		}
		head, _ := contract.Nonempty(draft, "head_branch")
		prs, err := n.Transport.ReadPullRequestsForHead(ctx, head)
		if err != nil {
			return nil, err
		}
		if prs["repository_node_id"] != repository["id"] {
			return nil, errors.New("delivery head PR inventory repository incarnation changed")
		}
		result["head_pull_requests"] = prs
	} else {
		number, err := contract.PositiveInteger(request.Policy["pull_request_number"])
		if err != nil {
			return nil, err
		}
		selected, err := n.Transport.ReadPullRequest(ctx, number)
		if err != nil {
			return nil, err
		}
		prRepo, err := contract.ObjectAt(selected, "repository")
		if err != nil || prRepo["id"] != repository["id"] {
			return nil, errors.New("delivery PR repository incarnation changed")
		}
		raw, err := contract.ObjectAt(selected, "pull_request")
		if err != nil {
			return nil, err
		}
		pr, err := snapshot.NormalizeMergePullRequest(raw, n.Transport.Repository, number)
		if err != nil {
			return nil, err
		}
		headRepository, err := contract.ObjectAt(raw, "headRepository")
		if err != nil || headRepository["id"] != repository["id"] {
			return nil, errors.New("delivery head repository incarnation is unavailable or changed")
		}
		pr["head_repository_node_id"] = headRepository["id"]
		closing, err := n.Transport.ReadPullRequestClosingIssues(ctx, number)
		if err != nil {
			return nil, err
		}
		if err := n.Transport.VerifyClosingReferenceIdentity(closing, raw, repository["id"].(string)); err != nil {
			return nil, err
		}
		pr["closing_issue_numbers"] = closing["closing_issue_numbers"]
		result["pull_request"] = pr
		if request.Policy["keep_branch"] == false {
			if err := requireNoOtherOpenBranchHeads(ctx, n.Transport, repository["id"].(string), pr["headRefName"].(string), number); err != nil {
				return nil, err
			}
			dependents, err := n.Transport.ReadOpenPullRequestsForBase(ctx, pr["headRefName"].(string))
			if err != nil {
				return nil, err
			}
			if dependents["repository_node_id"] != repository["id"] {
				return nil, errors.New("delivery base dependency collection repository incarnation changed")
			}
			result["base_dependents"] = dependents
		}
		checks, err := n.Transport.RequiredChecks(ctx, number)
		if err != nil {
			return nil, err
		}
		values := make([]any, 0, len(checks))
		for _, check := range checks {
			values = append(values, check)
		}
		result["required_checks"] = values
		branches, err := n.Transport.BranchInventory(ctx)
		if err != nil {
			return nil, err
		}
		prov, err := contract.ObjectAt(branches, "provenance")
		if err != nil || prov["repository_node_id"] != repository["id"] {
			return nil, errors.New("delivery branch collection repository incarnation changed")
		}
		rows, err := contract.Objects(branches, "branches")
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row["name"] != pr["headRefName"] {
				continue
			}
			branch, err := n.Transport.ReadBranch(ctx, row["name"].(string))
			if err != nil {
				return nil, err
			}
			if branch["repository_node_id"] != repository["id"] || branch["sha"] != row["sha"] {
				return nil, errors.New("delivery branch changed during complete inventory")
			}
			result["head_branch"] = branch
		}
	}
	final, err := n.Transport.ReadDeliveryRepository(ctx)
	if err != nil {
		return nil, err
	}
	finalDefault, _ := contract.ObjectAt(final, "defaultBranchRef")
	if final["id"] != repository["id"] || finalDefault["name"] != defaultRef["name"] || final["deleteBranchOnMerge"] != repository["deleteBranchOnMerge"] {
		return nil, errors.New("delivery repository identity, default branch or retention policy changed during inventory")
	}
	return result, nil
}

func (n NativeDelivery) CreatePullRequest(ctx context.Context, nonce string, draft native.PullRequestDraft, repositoryNodeID string) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	return n.Transport.CreatePullRequest(ctx, draft, repositoryNodeID, nonce)
}

func (n NativeDelivery) MergePullRequest(ctx context.Context, number int64, sha, method string) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("native delivery requires a transport")
	}
	return n.Transport.MergePullRequest(ctx, number, sha, method)
}

func (n NativeDelivery) DeleteBranch(ctx context.Context, nonce, branch, expectedSHA, repositoryNodeID string) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	return n.Transport.DeleteBranch(ctx, branch, expectedSHA, repositoryNodeID, nonce)
}
