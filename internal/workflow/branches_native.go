package workflow

import (
	"context"
	"errors"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/snapshot"
)

type NativeBranchCleanup struct{ Transport *native.Transport }

func (n NativeBranchCleanup) BranchCleanupInventory(ctx context.Context, rawSelection contract.Object) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("native branch cleanup requires a transport")
	}
	selection, err := ParseBranchCleanupSelection(rawSelection, n.Transport.Repository)
	if err != nil {
		return nil, err
	}
	repository, err := n.Transport.ReadRepository(ctx)
	if err != nil {
		return nil, err
	}
	defaultRef, err := contract.ObjectAt(repository, "defaultBranchRef")
	if err != nil {
		return nil, err
	}
	defaultBranch, err := contract.Nonempty(defaultRef, "name")
	if err != nil {
		return nil, err
	}
	branchInventory, err := n.Transport.BranchInventory(ctx)
	if err != nil {
		return nil, err
	}
	provenance, err := contract.ObjectAt(branchInventory, "provenance")
	if err != nil || provenance["repository_node_id"] != repository["id"] {
		return nil, errors.New("cleanup branch collection repository incarnation changed")
	}
	branchRows, err := contract.Objects(branchInventory, "branches")
	if err != nil {
		return nil, err
	}
	byName := map[string]contract.Object{}
	for _, branch := range branchRows {
		byName[branch["name"].(string)] = branch
	}
	rows, err := contract.Objects(selection, "branches")
	if err != nil {
		return nil, err
	}
	candidates := []any{}
	for _, row := range rows {
		name := row["name"].(string)
		candidate := contract.Object{"selection": row, "branch": nil}
		if observed := byName[name]; observed != nil {
			branch, err := n.Transport.ReadBranch(ctx, name)
			if err != nil {
				return nil, err
			}
			if branch["repository_node_id"] != repository["id"] || branch["sha"] != observed["sha"] {
				return nil, errors.New("cleanup branch changed during complete inventory")
			}
			candidate["branch"] = branch
		}
		number, _ := contract.PositiveInteger(row["pull_request_number"])
		selected, err := n.Transport.ReadPullRequest(ctx, number)
		if err != nil {
			return nil, err
		}
		prRepo, err := contract.ObjectAt(selected, "repository")
		if err != nil || prRepo["id"] != repository["id"] {
			return nil, errors.New("cleanup PR repository incarnation changed")
		}
		raw, err := contract.ObjectAt(selected, "pull_request")
		if err != nil {
			return nil, err
		}
		pr, err := snapshot.NormalizeMergePullRequest(raw, n.Transport.Repository, number)
		if err != nil {
			return nil, err
		}
		candidate["pull_request"] = pr
		dependents, err := n.Transport.ReadOpenPullRequestsForBase(ctx, name)
		if err != nil {
			return nil, err
		}
		if dependents["repository_node_id"] != repository["id"] {
			return nil, errors.New("cleanup dependency collection repository incarnation changed")
		}
		candidate["base_dependents"] = dependents
		candidates = append(candidates, candidate)
	}
	final, err := n.Transport.ReadRepository(ctx)
	if err != nil {
		return nil, err
	}
	finalDefault, err := contract.ObjectAt(final, "defaultBranchRef")
	if err != nil || final["id"] != repository["id"] || finalDefault["name"] != defaultBranch {
		return nil, errors.New("cleanup repository identity or default branch changed during inventory")
	}
	return contract.Object{"repo": n.Transport.Repository.Object(), "repository_node_id": repository["id"], "default_branch": defaultBranch, "branches": candidates,
		"provenance": contract.Object{"live": true, "complete": true, "source": "github_api", "repository_node_id": repository["id"], "selection": selection}}, nil
}

func (n NativeBranchCleanup) DeleteBranch(ctx context.Context, nonce, branch, expectedSHA, repositoryNodeID string) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("native branch cleanup requires a transport")
	}
	return n.Transport.DeleteBranch(ctx, branch, expectedSHA, repositoryNodeID, nonce)
}
