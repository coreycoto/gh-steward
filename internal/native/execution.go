package native

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// ReadPullRequestClosingIssues paginates the closing references while proving
// that every page belongs to the same exact PR head and repository incarnation.
// Cross-repository references cannot be reduced to local issue numbers.
func (t *Transport) ReadPullRequestClosingIssues(ctx context.Context, number int64) (contract.Object, error) {
	if number < 1 {
		return nil, errors.New("pull request number must be positive")
	}
	var cursor *string
	seenCursors, seenIDs, seenNumbers := map[string]bool{}, map[string]bool{}, map[int64]bool{}
	issues := []any{}
	var identity contract.Object
	for {
		variables := t.repoVariables()
		variables["number"], variables["cursor"] = number, cursor
		data, err := t.graphQL(ctx, `query($owner:String!,$name:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$name){`+repositoryFields+` pullRequest(number:$number){id number url headRefOid body closingIssuesReferences(first:100,after:$cursor){nodes{id number url repository{nameWithOwner url}}pageInfo{hasNextPage endCursor}}}}}`, variables)
		if err != nil {
			return nil, err
		}
		r, err := t.repositoryResult(data)
		if err != nil {
			return nil, err
		}
		pr, err := contract.ObjectAt(r, "pullRequest")
		if err != nil {
			return nil, errors.New("selected pull request closing references are unavailable")
		}
		n, err := contract.PositiveInteger(pr["number"])
		if err != nil || n != number {
			return nil, errors.New("closing references belong to another pull request")
		}
		if err := t.ValidatePullRequestURL(pr["url"], number); err != nil {
			return nil, err
		}
		id, err := contract.Nonempty(pr, "id")
		if err != nil {
			return nil, err
		}
		head, err := CommitOID(pr["headRefOid"])
		if err != nil {
			return nil, err
		}
		body, err := contract.String(pr, "body")
		if err != nil {
			return nil, err
		}
		current := contract.Object{"repository_node_id": r["id"], "pull_request_id": id, "pull_request_number": number, "head_sha": head, "body": body}
		if identity == nil {
			identity = current
		} else {
			a, _ := contract.Digest(identity)
			b, _ := contract.Digest(current)
			if a != b {
				return nil, errors.New("pull request identity or linkage changed during closing-reference pagination")
			}
		}
		nodes, page, err := Connection(pr, "closingIssuesReferences")
		if err != nil {
			return nil, err
		}
		for _, issue := range nodes {
			n, err := contract.PositiveInteger(issue["number"])
			if err != nil || seenNumbers[n] {
				return nil, errors.New("closing issue number is malformed or duplicated")
			}
			id, err := contract.Nonempty(issue, "id")
			if err != nil || seenIDs[id] {
				return nil, errors.New("closing issue immutable identity is malformed or duplicated")
			}
			if err := t.ValidateIssueURL(issue["url"], n); err != nil {
				return nil, err
			}
			repo, err := contract.ObjectAt(issue, "repository")
			if err != nil {
				return nil, err
			}
			qualified, err := contract.ParseRepository(repo)
			if err != nil || qualified != t.Repository {
				return nil, errors.New("cross-repository closing references are unsupported")
			}
			seenNumbers[n], seenIDs[id] = true, true
			issues = append(issues, contract.Object{"id": id, "number": n, "url": issue["url"]})
		}
		if len(issues) > 100000 {
			return nil, errors.New("closing issue collection exceeds supported size")
		}
		if page["hasNextPage"] != true {
			break
		}
		next, err := contract.Nonempty(page, "endCursor")
		if err != nil || seenCursors[next] {
			return nil, errors.New("closing-reference cursor did not advance")
		}
		seenCursors[next], cursor = true, &next
	}
	sort.Slice(issues, func(i, j int) bool {
		a, _ := contract.PositiveInteger(issues[i].(map[string]any)["number"])
		b, _ := contract.PositiveInteger(issues[j].(map[string]any)["number"])
		return a < b
	})
	numbers := []any{}
	for _, raw := range issues {
		numbers = append(numbers, raw.(map[string]any)["number"])
	}
	identity["issues"], identity["closing_issue_numbers"] = issues, numbers
	identity["repo"] = t.Repository.Object()
	identity["provenance"] = contract.Object{"live": true, "complete": true, "source": "github_api"}
	return identity, nil
}

// BranchInventory uses a complete paginated repository listing. A transport
// error is not evidence that an individual branch has been deleted.
func (t *Transport) BranchInventory(ctx context.Context) (contract.Object, error) {
	before, err := t.ReadRepository(ctx)
	if err != nil {
		return nil, err
	}
	pages, err := t.RESTPages(ctx, "repos/"+t.Repository.FullName()+"/branches?per_page=100")
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, errors.New("branch inventory has no complete page evidence")
	}
	branches := []any{}
	seen := map[string]bool{}
	for _, page := range pages {
		rows, ok := page.([]any)
		if !ok {
			return nil, errors.New("branch inventory page must be an array")
		}
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				return nil, errors.New("branch inventory row must be an object")
			}
			name, err := contract.Nonempty(row, "name")
			if err != nil || seen[name] || strings.TrimSpace(name) != name {
				return nil, errors.New("branch inventory identity is malformed or duplicated")
			}
			commit, err := contract.ObjectAt(row, "commit")
			if err != nil {
				return nil, err
			}
			sha, err := CommitOID(commit["sha"])
			if err != nil {
				return nil, err
			}
			seen[name] = true
			branches = append(branches, contract.Object{"name": name, "sha": sha})
		}
	}
	after, err := t.ReadRepository(ctx)
	if err != nil {
		return nil, err
	}
	if before["id"] != after["id"] {
		return nil, errors.New("repository incarnation changed during branch inventory")
	}
	sort.Slice(branches, func(i, j int) bool {
		return branches[i].(map[string]any)["name"].(string) < branches[j].(map[string]any)["name"].(string)
	})
	return contract.Object{"repo": t.Repository.Object(), "branches": branches, "provenance": contract.Object{"live": true, "complete": true, "source": "github_api", "repository_node_id": before["id"]}}, nil
}

func (t *Transport) ReopenExecutionIssue(ctx context.Context, nonce string, number int64) (contract.Object, error) {
	if _, err := OperationMarker(nonce); err != nil {
		return nil, err
	}
	return t.UpdateIssue(ctx, number, contract.Object{"state": "open", "state_reason": "reopened"})
}

func (t *Transport) CloseExecutionIssue(ctx context.Context, nonce string, number int64) (contract.Object, error) {
	if _, err := OperationMarker(nonce); err != nil {
		return nil, err
	}
	return t.UpdateIssue(ctx, number, contract.Object{"state": "closed", "state_reason": "completed"})
}

func (t *Transport) VerifyClosingReferenceIdentity(raw contract.Object, pr contract.Object, repositoryNodeID string) error {
	number, err := contract.PositiveInteger(pr["number"])
	if err != nil {
		return err
	}
	actual, err := contract.PositiveInteger(raw["pull_request_number"])
	if err != nil || actual != number || raw["repository_node_id"] != repositoryNodeID || raw["pull_request_id"] != pr["id"] || raw["head_sha"] != pr["headRefOid"] || raw["body"] != pr["body"] {
		return fmt.Errorf("closing references do not identify the exact selected pull request %d", number)
	}
	return nil
}
