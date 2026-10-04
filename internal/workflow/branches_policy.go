package workflow

import (
	"errors"
	"sort"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

const BranchCleanupCommand = "branch-cleanup-apply"

// ParseBranchCleanupSelection is pure. Candidate names and heads are explicit
// inputs; the live reviewed adapter separately proves merged PR and base-use
// eligibility. An empty selection remains an inspectable no-op plan.
func ParseBranchCleanupSelection(raw contract.Object, repo contract.Repository) (contract.Object, error) {
	if _, err := contract.ParseRepository(repo.Object()); err != nil {
		return nil, err
	}
	if !executionKeys(raw, "branches") {
		return nil, errors.New("branch cleanup selection requires only branches")
	}
	rows, err := contract.Objects(raw, "branches")
	if err != nil {
		return nil, err
	}
	if len(rows) > 1000 {
		return nil, errors.New("branch cleanup selection exceeds supported size")
	}
	seen := map[string]bool{}
	selected := make([]contract.Object, 0, len(rows))
	for _, row := range rows {
		if !executionKeys(row, "name", "sha", "pull_request_number") {
			return nil, errors.New("branch cleanup candidate requires exactly name, sha and pull_request_number")
		}
		name, err := contract.Nonempty(row, "name")
		if err != nil {
			return nil, err
		}
		if err := native.ValidateBranchName(name); err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, errors.New("branch cleanup selection repeats a branch")
		}
		sha, err := native.CommitOID(row["sha"])
		if err != nil {
			return nil, err
		}
		number, err := contract.PositiveInteger(row["pull_request_number"])
		if err != nil {
			return nil, err
		}
		seen[name] = true
		selected = append(selected, contract.Object{"name": name, "sha": sha, "pull_request_number": number})
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i]["name"].(string) < selected[j]["name"].(string) })
	values := make([]any, 0, len(selected))
	for _, row := range selected {
		values = append(values, row)
	}
	return contract.Object{"branches": values}, nil
}
