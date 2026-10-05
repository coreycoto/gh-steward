package workflow

import (
	"errors"
	"sort"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

// Retain the complete collection, not a failed ref lookup or a caller-authored
// exists=false flag. The server's retention setting is evidence of current
// policy, not evidence of who removed an absent branch.
func normalizeBranchCleanupEvidence(value any, repo contract.Repository, nodeID, defaultBranch string) (contract.Object, map[string]string, error) {
	raw, ok := value.(contract.Object)
	if !ok || !executionKeys(raw, "inventory", "delete_branch_on_merge") {
		return nil, nil, errors.New("cleanup branch evidence lacks its complete inventory and retention policy")
	}
	autoDelete, err := contract.Bool(raw, "delete_branch_on_merge")
	if err != nil {
		return nil, nil, err
	}
	collection, err := contract.ObjectAt(raw, "inventory")
	if err != nil || !executionKeys(collection, "repo", "branches", "provenance") || !same(collection["repo"], repo.Object()) ||
		!same(collection["provenance"], contract.Object{"live": true, "complete": true, "source": "github_api", "repository_node_id": nodeID}) {
		return nil, nil, errors.New("cleanup branch evidence is not complete live evidence for the exact repository incarnation")
	}
	rows, err := contract.Objects(collection, "branches")
	if err != nil {
		return nil, nil, err
	}
	refs := map[string]string{}
	branches := make([]any, 0, len(rows))
	for _, row := range rows {
		if !executionKeys(row, "name", "sha") {
			return nil, nil, errors.New("cleanup branch collection has unsupported or missing ref fields")
		}
		name, err := contract.Nonempty(row, "name")
		if err != nil || native.ValidateBranchName(name) != nil {
			return nil, nil, errors.New("cleanup branch collection has an invalid name")
		}
		sha, err := native.CommitOID(row["sha"])
		if _, duplicate := refs[name]; err != nil || duplicate {
			return nil, nil, errors.New("cleanup branch collection has an invalid SHA or repeated name")
		}
		refs[name] = sha
		branches = append(branches, contract.Object{"name": name, "sha": sha})
	}
	if _, present := refs[defaultBranch]; !present {
		return nil, nil, errors.New("complete cleanup branch evidence does not contain the current default branch")
	}
	sort.Slice(branches, func(i, j int) bool {
		return branches[i].(contract.Object)["name"].(string) < branches[j].(contract.Object)["name"].(string)
	})
	return contract.Object{"inventory": contract.Object{"repo": repo.Object(), "branches": branches,
		"provenance": collection["provenance"]}, "delete_branch_on_merge": autoDelete}, refs, nil
}

// This typed outcome remains in the signed plan and terminal plan proof even
// though it has no mutation, dispatch identity or deletion acknowledgement.
func branchCleanupAbsences(inventory contract.Object) ([]any, error) {
	rows, err := contract.Objects(inventory, "branches")
	if err != nil {
		return nil, err
	}
	absences := []any{}
	var digest string
	for _, row := range rows {
		if row["branch"] != nil {
			continue
		}
		evidence, err := contract.ObjectAt(inventory, "branch_evidence")
		if err != nil {
			return nil, errors.New("absent cleanup branch requires positive complete inventory and current retention evidence")
		}
		if digest == "" {
			collection, _ := contract.ObjectAt(evidence, "inventory")
			digest, err = contract.Digest(collection)
			if err != nil {
				return nil, err
			}
		}
		pr, _ := contract.ObjectAt(row, "pull_request")
		absences = append(absences, contract.Object{"kind": "already_absent", "selection": row["selection"],
			"repository_node_id": inventory["repository_node_id"], "pull_request_id": pr["id"],
			"branch_inventory_sha256": digest, "delete_branch_on_merge": evidence["delete_branch_on_merge"]})
	}
	return absences, nil
}
