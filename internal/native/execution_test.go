package native

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func executionRepoResponse(id string) Result {
	return response(fmt.Sprintf(`{"data":{"repository":{"id":%q,"nameWithOwner":"example/widgets","url":"https://github.com/example/widgets","defaultBranchRef":{"name":"main"}}}}`, id))
}

func closingPage(issue int64, next bool, cursor string) Result {
	return response(fmt.Sprintf(`{"data":{"repository":{"id":"R_widgets","nameWithOwner":"example/widgets","url":"https://github.com/example/widgets","pullRequest":{"id":"PR_3","number":3,"url":"https://github.com/example/widgets/pull/3","headRefOid":%q,"body":"Closes #17","closingIssuesReferences":{"nodes":[{"id":"I_%d","number":%d,"url":"https://github.com/example/widgets/issues/%d","repository":{"nameWithOwner":"example/widgets","url":"https://github.com/example/widgets"}}],"pageInfo":{"hasNextPage":%t,"endCursor":%q}}}}}}`, strings.Repeat("a", 40), issue, issue, issue, next, cursor))
}

func TestClosingReferencePaginationBindsEveryPageToExactRepositoryAndHead(t *testing.T) {
	s := &sequenceExecutor{responses: []Result{closingPage(17, true, "next"), closingPage(23, false, "")}}
	got, err := sequence(s).ReadPullRequestClosingIssues(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	numbers, err := contract.Array(got, "closing_issue_numbers")
	if err != nil || len(numbers) != 2 || len(s.calls) != 2 {
		t.Fatal(got, err)
	}
	second, _ := contract.ObjectAt(s.inputs[1], "variables")
	if second["cursor"] != "next" || second["number"] == nil || second["owner"] != "example" {
		t.Fatal(s.inputs)
	}
	for _, mutation := range []struct{ name, old, new string }{
		{"head", strings.Repeat("a", 40), strings.Repeat("b", 40)},
		{"incarnation", `"R_widgets"`, `"R_changed"`},
		{"pr node", `"PR_3"`, `"PR_4"`},
		{"body", `Closes #17`, `Closes #42`},
		{"foreign issue", `/issues/23`, `/issues/17`},
		{"foreign repo", `"nameWithOwner":"example/widgets"`, `"nameWithOwner":"foreign/widgets"`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			bad := closingPage(23, false, "")
			bad.Stdout = []byte(strings.Replace(string(bad.Stdout), mutation.old, mutation.new, -1))
			s := &sequenceExecutor{responses: []Result{closingPage(17, true, "next"), bad}}
			if _, err := sequence(s).ReadPullRequestClosingIssues(context.Background(), 3); err == nil {
				t.Fatal("changed source accepted")
			}
		})
	}
	for _, last := range []Result{closingPage(17, false, ""), closingPage(23, true, "next"), {ExitCode: 1}} {
		s := &sequenceExecutor{responses: []Result{closingPage(17, true, "next"), last}}
		if _, err := sequence(s).ReadPullRequestClosingIssues(context.Background(), 3); err == nil {
			t.Fatal("duplicate/loop/failed source accepted")
		}
	}
}

func TestBranchInventoryReadsEveryPageAndDoesNotTreatReadFailureAsAbsence(t *testing.T) {
	branch := fmt.Sprintf(`{"name":"codex/issue-17","commit":{"sha":%q}}`, strings.Repeat("a", 40))
	s := &sequenceExecutor{responses: []Result{executionRepoResponse("R_widgets"), response(`[[{"name":"main","commit":{"sha":"` + strings.Repeat("b", 40) + `"}}],[` + branch + `]]`), executionRepoResponse("R_widgets")}}
	got, err := sequence(s).BranchInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	branches, _ := contract.Objects(got, "branches")
	if len(branches) != 2 || branches[0]["name"] != "codex/issue-17" || !strings.Contains(strings.Join(s.calls[1], " "), "--paginate --slurp") {
		t.Fatal(got, s.calls)
	}
	for _, page := range []Result{response(`[]`), response(`[[` + branch + `],[` + branch + `]]`), response(`[[{"name":"branch","commit":{"sha":"bad"}}]]`), {ExitCode: 1}} {
		s := &sequenceExecutor{responses: []Result{executionRepoResponse("R_widgets"), page, executionRepoResponse("R_widgets")}}
		if _, err := sequence(s).BranchInventory(context.Background()); err == nil {
			t.Fatal("missing source became branch absence")
		}
	}
	s = &sequenceExecutor{responses: []Result{executionRepoResponse("R_widgets"), response(`[[]]`), executionRepoResponse("R_recreated")}}
	if _, err := sequence(s).BranchInventory(context.Background()); err == nil {
		t.Fatal("repository recreation accepted")
	}
	for _, operation := range []func(context.Context, string, int64) (contract.Object, error){sequence(&sequenceExecutor{}).ReopenExecutionIssue, sequence(&sequenceExecutor{}).CloseExecutionIssue} {
		if _, err := operation(context.Background(), "bad-nonce", 17); err == nil {
			t.Fatal("uncorrelated transition accepted")
		}
	}
}
