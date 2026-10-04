package snapshot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

type mergeReaderFixture struct {
	repository, run, pr    contract.Object
	checks                 []contract.Object
	readCount              int
	failChecks, driftFinal bool
}

func (f *mergeReaderFixture) ReadRepository(context.Context) (contract.Object, error) {
	f.readCount++
	r, _ := contract.Clone(f.repository)
	if f.driftFinal && f.readCount > 1 {
		r["id"] = "R_recreated"
	}
	return r, nil
}
func (f *mergeReaderFixture) ReadPullRequest(context.Context, int64) (contract.Object, error) {
	r, _ := contract.Clone(f.repository)
	pr, _ := contract.Clone(f.pr)
	return contract.Object{"repository": r, "pull_request": pr}, nil
}
func (f *mergeReaderFixture) RequiredChecks(context.Context, int64) ([]contract.Object, error) {
	if f.failChecks {
		return nil, errors.New("checks unavailable")
	}
	return f.checks, nil
}
func (f *mergeReaderFixture) ReadWorkflowRun(context.Context, int64) (contract.Object, error) {
	return contract.Clone(f.run)
}
func mergeReaderSource() *mergeReaderFixture {
	r := contract.Object{"id": "R_widgets", "nameWithOwner": "example/widgets", "url": "https://github.com/example/widgets", "defaultBranchRef": contract.Object{"name": "main"}}
	head := strings.Repeat("a", 40)
	run := contract.Object{"id": int64(8), "run_attempt": int64(1), "workflow_id": int64(88), "name": "CI", "event": "pull_request", "status": "completed", "conclusion": "success", "head_sha": head, "head_branch": "codex/change", "repository": contract.Object{"full_name": "example/widgets", "node_id": "R_widgets"}, "pull_requests": []any{contract.Object{"number": int64(3), "head": contract.Object{"sha": head, "ref": "codex/change"}}}}
	pr := contract.Object{"id": "PR_3", "number": int64(3), "url": "https://github.com/example/widgets/pull/3", "title": "Change", "body": "Closes #1", "state": "OPEN", "merged": false, "isDraft": false, "baseRefName": "main", "headRefName": "codex/change", "headRefOid": head, "mergeStateStatus": "CLEAN", "reviewDecision": "APPROVED", "mergeCommit": nil, "author": contract.Object{"login": "automation[bot]", "__typename": "Bot"}, "headRepository": contract.Object{"nameWithOwner": "example/widgets", "url": "https://github.com/example/widgets"}, "labels": contract.Object{"nodes": []any{contract.Object{"name": "automerge"}}, "pageInfo": contract.Object{"hasNextPage": false}}}
	return &mergeReaderFixture{repository: r, run: run, pr: pr, checks: []contract.Object{{"name": "tests", "bucket": "pass", "state": "SUCCESS", "workflow": "CI", "link": ""}}}
}
func TestMergeInventoryQualifiesSubmittedEventWithCurrentRunAndExactPR(t *testing.T) {
	f := mergeReaderSource()
	event := contract.Object{"workflow_run": f.run}
	source, err := MergeInventory(context.Background(), f, repo(), event)
	if err != nil {
		t.Fatal(err)
	}
	pr, _ := contract.ObjectAt(source, "pull_request")
	author, _ := contract.ObjectAt(pr, "author")
	if pr["headRefOid"] != strings.Repeat("a", 40) || author["is_bot"] != true || f.readCount != 2 {
		t.Fatal(source, f.readCount)
	}
	for _, change := range []string{"run-attempt", "head", "foreign-repository", "repository-node", "partial-labels", "checks-failed", "final-incarnation"} {
		t.Run(change, func(t *testing.T) {
			f := mergeReaderSource()
			submitted, _ := contract.Clone(f.run)
			event := contract.Object{"workflow_run": submitted}
			switch change {
			case "run-attempt":
				f.run["run_attempt"] = int64(2)
			case "head":
				f.run["head_sha"] = strings.Repeat("c", 40)
			case "foreign-repository":
				r, _ := contract.ObjectAt(f.run, "repository")
				r["full_name"] = "foreign/widgets"
			case "repository-node":
				r, _ := contract.ObjectAt(f.run, "repository")
				r["node_id"] = "R_recreated"
			case "partial-labels":
				labels, _ := contract.ObjectAt(f.pr, "labels")
				labels["pageInfo"] = contract.Object{"hasNextPage": true, "endCursor": "next"}
			case "checks-failed":
				f.failChecks = true
			case "final-incarnation":
				f.driftFinal = true
			}
			if _, err := MergeInventory(context.Background(), f, repo(), event); err == nil {
				t.Fatal("incomplete or changed source accepted")
			}
		})
	}
}
func TestMergedPRUsesTerminalCommitAndAllowsDeletedHeadRepository(t *testing.T) {
	f := mergeReaderSource()
	f.pr["state"], f.pr["merged"], f.pr["mergeStateStatus"], f.pr["headRepository"] = "MERGED", true, "UNKNOWN", nil
	f.pr["mergeCommit"] = contract.Object{"oid": strings.Repeat("b", 40)}
	pr, err := NormalizeMergePullRequest(f.pr, repo(), 3)
	if err != nil || pr["mergeStateStatus"] != "MERGED" || pr["merge_commit_sha"] != strings.Repeat("b", 40) {
		t.Fatal(pr, err)
	}
	f.pr["mergeCommit"] = nil
	if _, err := NormalizeMergePullRequest(f.pr, repo(), 3); err == nil {
		t.Fatal("merged state without commit accepted")
	}
}
