package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

type executionNativeFixture struct {
	linked, closed, branchFailure, commentsFailure, foreignClosing, changedClosingHead, finalDrift bool
	headRepo                                                                                       any
	repoReads                                                                                      int
	calls                                                                                          []string
}

func TestExecutionSnapshotIsACompleteReadWithoutPlanOrDispatch(t *testing.T) {
	policy := executionTestPolicy()
	selector := ExecutionSelector{PullRequestNumber: 3, SkipProjectSync: true}
	provider := newExecutionTestProvider(executionTestRawInventory(policy, selector, "OPEN", false, true))
	result, err := SnapshotExecution(context.Background(), provider, testRepo(), selector, policy)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := contract.ObjectAt(result, "pull_request")
	if err != nil || pr["head_sha"] != strings.Repeat("a", 40) {
		t.Fatal("exact typed candidate missing", result, err)
	}
	issue, err := contract.ObjectAt(result, "issue")
	number, _ := contract.PositiveInteger(issue["number"])
	if err != nil || number != 17 {
		t.Fatal("linked issue identity missing", result, err)
	}
	_, writes, err := provider.counts()
	if err != nil || writes != 0 {
		t.Fatal("read snapshot dispatched a mutation", writes, err)
	}
	if result["operations"] != nil || result["sha256"] != nil || result["command"] != nil {
		t.Fatal("read snapshot became an apply plan", result)
	}
	bad := ExecutionSelector{IssueNumber: 17, PullRequestNumber: 3, SkipProjectSync: true}
	if _, err := SnapshotExecution(context.Background(), provider, testRepo(), bad, policy); err == nil {
		t.Fatal("ambiguous snapshot selector accepted")
	}
}

func executionNativeRepo(id string) contract.Object {
	r := testRepo().Object()
	r["id"] = id
	r["name"] = "widgets"
	r["owner"] = contract.Object{"login": "example", "__typename": "User"}
	r["defaultBranchRef"] = contract.Object{"name": "main"}
	return r
}
func executionNativeConnection(rows ...any) contract.Object {
	if rows == nil {
		rows = []any{}
	}
	return contract.Object{"nodes": rows, "pageInfo": contract.Object{"hasNextPage": false}}
}
func (f *executionNativeFixture) Execute(_ context.Context, _ string, args []string, input []byte, _ string, _ []string) (native.Result, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	var value any
	if args[1] == "graphql" {
		body, err := contract.Decode(strings.NewReader(string(input)))
		if err != nil {
			return native.Result{}, err
		}
		query := body["query"].(string)
		r := executionNativeRepo("R_widgets")
		switch {
		case strings.Contains(query, "closingIssuesReferences"):
			head := strings.Repeat("a", 40)
			if f.changedClosingHead {
				head = strings.Repeat("b", 40)
			}
			qualified := testRepo().Object()
			if f.foreignClosing {
				qualified = contract.Object{"nameWithOwner": "foreign/widgets", "url": "https://github.com/foreign/widgets"}
			}
			r["pullRequest"] = contract.Object{"id": "PR_3", "number": int64(3), "url": "https://github.com/example/widgets/pull/3", "headRefOid": head, "body": "Closes #17", "closingIssuesReferences": executionNativeConnection(contract.Object{"id": "I_17", "number": int64(17), "url": "https://github.com/example/widgets/issues/17", "repository": qualified})}
		case strings.Contains(query, "pullRequest(number"):
			state := "OPEN"
			if f.closed {
				state = "CLOSED"
			}
			head := f.headRepo
			if head == nil {
				head = testRepo().Object()
			}
			if head == "deleted" {
				head = nil
			}
			r["pullRequest"] = contract.Object{"id": "PR_3", "number": int64(3), "url": "https://github.com/example/widgets/pull/3", "title": "Reviewed change", "body": "Closes #17", "state": state, "isDraft": false, "baseRefName": "main", "headRefName": "codex/issue-17", "headRefOid": strings.Repeat("a", 40), "mergeStateStatus": "CLEAN", "reviewDecision": nil, "merged": false, "mergedAt": nil, "mergeCommit": nil, "author": contract.Object{"login": "dev", "__typename": "User"}, "headRepository": head, "labels": executionNativeConnection()}
		case strings.Contains(query, "issues(first"):
			r["issues"] = executionNativeConnection(contract.Object{"id": "I_17", "number": int64(17), "title": "Task: reviewed change", "body": "", "state": "OPEN", "url": "https://github.com/example/widgets/issues/17", "labels": executionNativeConnection(), "milestone": nil})
		default:
			f.repoReads++
			if f.finalDrift && f.repoReads > 1 {
				r["id"] = "R_recreated"
			}
		}
		value = contract.Object{"data": contract.Object{"repository": r}}
	} else {
		switch {
		case strings.Contains(args[1], "issues/comments?"):
			if f.commentsFailure {
				return native.Result{ExitCode: 1}, nil
			}
			comments := []any{}
			if f.linked {
				comments = append(comments, contract.Object{"id": int64(31), "issue_url": "https://api.github.com/repos/example/widgets/issues/17", "html_url": "https://github.com/example/widgets/issues/17#issuecomment-31", "body": "<!-- linked-pr --> PR #3"})
			}
			value = []any{comments}
		case strings.Contains(args[1], "pulls?"):
			state := "open"
			if f.closed {
				state = "closed"
			}
			value = []any{[]any{contract.Object{"node_id": "PR_3", "number": int64(3), "html_url": "https://github.com/example/widgets/pull/3", "title": "Reviewed change", "body": "Closes #17", "state": state, "draft": false, "merged_at": nil, "head": contract.Object{"ref": "codex/issue-17", "sha": strings.Repeat("a", 40), "repo": contract.Object{"owner": contract.Object{"login": "example"}}}, "base": contract.Object{"ref": "main", "repo": contract.Object{"full_name": "example/widgets", "html_url": "https://github.com/example/widgets"}}}}}
		case strings.Contains(args[1], "branches?"):
			if f.branchFailure {
				return native.Result{ExitCode: 1}, nil
			}
			value = []any{[]any{contract.Object{"name": "main", "commit": contract.Object{"sha": strings.Repeat("b", 40)}}}}
		default:
			return native.Result{}, fmt.Errorf("unexpected native endpoint: %s", args[1])
		}
	}
	encoded, err := contract.Canonical(value)
	return native.Result{Stdout: encoded}, err
}
func executionBridgePolicy() ExecutionPolicy {
	return ExecutionPolicy{Statuses: ExecutionStatusPolicy{Done: "Done", Active: "Active", Todo: "Todo"}, StatusField: "Status", PRLinkMarkerPrefix: "<!-- linked-pr -->", PRLinkNumberPattern: `PR #(?P<number>[0-9]+)`, LinkedIssueMarkerPrefix: "<!-- linked-issue:", LinkStateMarkerPrefix: "<!-- link-state:"}
}
func TestNativeExecutionBridgeQualifiesCompleteNoLinkAndClosedDeletedBranch(t *testing.T) {
	for _, linked := range []bool{false, true} {
		f := &executionNativeFixture{linked: linked, closed: true}
		n := NativeExecution{NativeBacklog: NativeBacklog{Transport: bridgeTransport(f)}}
		selector := ExecutionSelector{IssueNumber: 17, SkipProjectSync: true}
		inventory, err := n.ExecutionInventory(context.Background(), ExecutionInventoryRequest{Selector: selector, Policy: executionBridgePolicy()})
		if err != nil {
			t.Fatal(err)
		}
		if (inventory["pull_request"] != nil) != linked {
			t.Fatal(inventory)
		}
		if _, err := PrepareExecutionSync(context.Background(), n, testRepo(), selector, executionBridgePolicy(), time.Now()); err != nil {
			t.Fatal("native contract does not feed reviewed workflow", err)
		}
		for _, call := range f.calls {
			if strings.Contains(call, "sub_issues") || strings.Contains(call, "dependencies/") {
				t.Fatal("execution queried unused graph topology", call)
			}
		}
	}
}
func TestNativeExecutionBridgeNeverConvertsFailedOrForeignEvidenceToNoLink(t *testing.T) {
	for _, f := range []*executionNativeFixture{
		{linked: true, closed: true, branchFailure: true}, {commentsFailure: true},
		{linked: true, foreignClosing: true}, {linked: true, changedClosingHead: true}, {finalDrift: true},
		{linked: true, closed: true, headRepo: "deleted"},
		{linked: true, closed: true, headRepo: contract.Object{"nameWithOwner": "fork/widgets", "url": "https://github.com/fork/widgets"}},
	} {
		n := NativeExecution{NativeBacklog: NativeBacklog{Transport: bridgeTransport(f)}}
		if _, err := n.ExecutionInventory(context.Background(), ExecutionInventoryRequest{Selector: ExecutionSelector{IssueNumber: 17, SkipProjectSync: true}, Policy: executionBridgePolicy()}); err == nil {
			t.Fatal("incomplete/foreign execution inventory accepted")
		}
	}
	if _, err := (NativeExecution{}).ReopenIssue(context.Background(), strings.Repeat("a", 32), 17); err == nil {
		t.Fatal("missing transport accepted")
	}
}
