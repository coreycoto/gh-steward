package workflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/snapshot"
)

// NativeExecution uses the same typed issue/comment/Project primitives as the
// reviewed backlog adapter. It does not infer a Project owner from the repo.
type NativeExecution struct{ NativeBacklog }

// SnapshotExecution exposes the complete typed read contract independently
// of plan preparation. Inspection never creates intent or a dispatch journal.
func SnapshotExecution(ctx context.Context, provider ExecutionProvider, repo contract.Repository, selector ExecutionSelector, policy ExecutionPolicy) (contract.Object, error) {
	if provider == nil {
		return nil, errors.New("execution snapshot requires a provider")
	}
	if err := validateExecutionSelector(selector, repo); err != nil {
		return nil, err
	}
	if err := validateExecutionPolicy(policy); err != nil {
		return nil, err
	}
	request := ExecutionInventoryRequest{Selector: selector, Policy: policy}
	raw, err := provider.ExecutionInventory(ctx, request)
	if err != nil {
		return nil, err
	}
	return normalizeExecutionInventory(raw, repo, request)
}

func (n NativeExecution) executionPR(ctx context.Context, number int64, repositoryNodeID string) (contract.Object, error) {
	source, err := n.Transport.ReadPullRequest(ctx, number)
	if err != nil {
		return nil, err
	}
	repository, err := contract.ObjectAt(source, "repository")
	if err != nil || repository["id"] != repositoryNodeID {
		return nil, errors.New("execution pull request repository incarnation changed")
	}
	raw, err := contract.ObjectAt(source, "pull_request")
	if err != nil {
		return nil, err
	}
	pr, err := snapshot.NormalizeMergePullRequest(raw, n.Transport.Repository, number)
	if err != nil {
		return nil, err
	}
	closing, err := n.Transport.ReadPullRequestClosingIssues(ctx, number)
	if err != nil {
		return nil, err
	}
	if err := n.Transport.VerifyClosingReferenceIdentity(closing, raw, repositoryNodeID); err != nil {
		return nil, err
	}
	mergedAt, exists := raw["mergedAt"]
	if !exists {
		return nil, errors.New("execution pull request nullable merge time is unavailable")
	}
	var head, owner any
	if raw := pr["head_repository"]; raw != nil {
		parsed, err := contract.ParseRepository(raw.(map[string]any))
		if err != nil {
			return nil, err
		}
		head = parsed.Object()
		owner = parsed.Owner
	}
	return contract.Object{"id": pr["id"], "number": number, "url": pr["url"], "title": pr["title"], "body": pr["body"], "state": pr["state"], "is_draft": pr["isDraft"], "is_merged": pr["merged"], "merged_at": mergedAt, "base_branch": pr["baseRefName"], "head_branch": pr["headRefName"], "head_sha": pr["headRefOid"], "head_repository_owner": owner, "head_repository": head, "closing_issue_numbers": closing["closing_issue_numbers"]}, nil
}

func (n NativeExecution) ExecutionInventory(ctx context.Context, request ExecutionInventoryRequest) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("native execution requires a transport")
	}
	repo := n.Transport.Repository
	if err := validateExecutionSelector(request.Selector, repo); err != nil {
		return nil, err
	}
	if err := validateExecutionPolicy(request.Policy); err != nil {
		return nil, err
	}
	s := snapshot.Service{Reader: n.Transport, Repository: repo}
	current, err := s.CurrentRepository(ctx)
	if err != nil {
		return nil, err
	}
	rawRepo, err := contract.ObjectAt(current, "repo")
	if err != nil {
		return nil, err
	}
	nodeID, err := contract.Nonempty(rawRepo, "id")
	if err != nil {
		return nil, err
	}
	defaultBranch, err := contract.Nonempty(current, "default_branch")
	if err != nil {
		return nil, err
	}
	issues, err := s.IssueInventory(ctx, "all")
	if err != nil {
		return nil, err
	}
	issueProvenance, err := contract.ObjectAt(issues, "provenance")
	if err != nil || issueProvenance["repository_node_id"] != nodeID {
		return nil, errors.New("execution issue inventory repository incarnation changed")
	}
	var pr contract.Object
	issueNumber := request.Selector.IssueNumber
	prNumber := request.Selector.PullRequestNumber
	if prNumber > 0 {
		pr, err = n.executionPR(ctx, prNumber, nodeID)
		if err != nil {
			return nil, err
		}
		issueNumber, err = ResolveExecutionIssueNumber(pr, request.Policy)
		if err != nil {
			return nil, err
		}
	}
	if issueNumber < 1 {
		return nil, errors.New("execution selection requires a positive issue or pull request number")
	}
	linked, err := s.LinkedPullRequests(ctx, []int64{issueNumber}, contract.Object{"marker_prefix": request.Policy.PRLinkMarkerPrefix, "pr_number_pattern": request.Policy.PRLinkNumberPattern})
	if err != nil {
		return nil, err
	}
	unresolved, err := contract.Array(linked, "unresolved_links")
	if err != nil || len(unresolved) > 0 {
		return nil, errors.New("execution linked pull request is unresolved")
	}
	if pr == nil {
		byIssue, err := contract.ObjectAt(linked, "by_issue")
		if err != nil {
			return nil, err
		}
		if raw := byIssue[strconv.FormatInt(issueNumber, 10)]; raw != nil {
			selected, ok := raw.(map[string]any)
			if !ok {
				return nil, errors.New("execution linked pull request must be normalized")
			}
			prNumber, err = contract.PositiveInteger(selected["number"])
			if err != nil {
				return nil, err
			}
			pr, err = n.executionPR(ctx, prNumber, nodeID)
			if err != nil {
				return nil, err
			}
		}
	}
	var branches, project contract.Object
	if pr != nil && pr["state"] == "CLOSED" && pr["is_merged"] == false {
		head, err := contract.ObjectAt(pr, "head_repository")
		if err != nil {
			return nil, errors.New("closed unmerged pull request has no provable head repository")
		}
		identity, err := contract.ParseRepository(head)
		if err != nil || identity != repo {
			return nil, errors.New("closed unmerged fork head requires separately qualified branch evidence")
		}
		branches, err = n.Transport.BranchInventory(ctx)
		if err != nil {
			return nil, err
		}
	}
	if pr != nil && !request.Selector.SkipProjectSync {
		if request.Selector.Project == nil {
			return nil, errors.New("execution Project sync requires an exact reviewed Project")
		}
		project, err = s.Project(ctx, nativeBacklogScope(*request.Selector.Project))
		if err != nil {
			return nil, err
		}
		metadata, err := contract.ObjectAt(project, "project")
		if err != nil || metadata["title"] != request.Selector.Project.Title {
			return nil, errors.New("execution Project title differs from the selected scope")
		}
	}
	final, err := s.CurrentRepository(ctx)
	if err != nil {
		return nil, err
	}
	finalRepo, err := contract.ObjectAt(final, "repo")
	if err != nil || finalRepo["id"] != nodeID || final["default_branch"] != defaultBranch {
		return nil, errors.New("execution repository identity or default branch changed during inventory")
	}
	provenance := contract.Object{"live": true, "complete": true, "issue_state": "all", "repository_node_id": nodeID, "default_branch_source": "github_api", "pull_request_source": "not_requested", "linked_pr_source": "github_api", "branch_source": "not_requested", "project_source": "not_requested", "selector": request.Selector.Object(), "policy": request.Policy.Object()}
	if pr != nil {
		provenance["pull_request_source"] = "github_api"
	}
	if branches != nil {
		provenance["branch_source"] = "github_api"
	}
	if project != nil {
		provenance["project_source"] = "github_project_api"
	}
	result := contract.Object{"repo": repo.Object(), "repository_node_id": nodeID, "default_branch": defaultBranch, "issue_inventory": issues, "pull_request": nil, "linked_pr_inventory": linked, "branch_inventory": nil, "project_inventory": nil, "provenance": provenance}
	if pr != nil {
		result["pull_request"] = pr
	}
	if branches != nil {
		result["branch_inventory"] = branches
	}
	if project != nil {
		result["project_inventory"] = project
	}
	if b, err := contract.Canonical(result); err != nil || len(b) > contract.MaxJSONBytes {
		return nil, fmt.Errorf("execution inventory exceeds complete supported envelope")
	}
	return result, nil
}

func (n NativeExecution) ReopenIssue(ctx context.Context, nonce string, number int64) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	return n.Transport.ReopenExecutionIssue(ctx, nonce, number)
}
func (n NativeExecution) CloseIssue(ctx context.Context, nonce string, number int64) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	return n.Transport.CloseExecutionIssue(ctx, nonce, number)
}
