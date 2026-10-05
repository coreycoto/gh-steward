package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// Exercise the real public prepare/extract/apply boundaries with a native gh
// subprocess. The fixture accepts complete reads and rejects every mutation.
func TestBranchCleanupCLINativeAutoDeletionHasNoWriteOrDeletionReceipt(t *testing.T) {
	root, fixtures := checkout(t), t.TempDir()
	repository := contract.Object{"id": "R_widgets", "name": "widgets", "nameWithOwner": "example/widgets", "url": "https://github.com/example/widgets",
		"owner": contract.Object{"login": "example", "__typename": "Organization"}, "defaultBranchRef": contract.Object{"name": "main"}, "deleteBranchOnMerge": true,
		"pullRequests": contract.Object{"nodes": []any{}, "pageInfo": contract.Object{"hasNextPage": false, "endCursor": nil}}}
	repository["pullRequest"] = contract.Object{"id": "PR_3", "number": int64(3), "url": "https://github.com/example/widgets/pull/3", "title": "Merged change", "body": "",
		"state": "MERGED", "merged": true, "isDraft": false, "baseRefName": "main", "headRefName": "codex/change", "headRefOid": strings.Repeat("a", 40),
		"mergeStateStatus": "MERGED", "reviewDecision": nil, "mergeCommit": contract.Object{"oid": strings.Repeat("b", 40)}, "author": contract.Object{"login": "dev", "__typename": "User"},
		"headRepository": contract.Object{"id": "R_widgets", "nameWithOwner": "example/widgets", "url": "https://github.com/example/widgets"},
		"labels":         contract.Object{"nodes": []any{}, "pageInfo": contract.Object{"hasNextPage": false}}}
	if err := writeFile(fixtures, "graphql.json", contract.Object{"data": contract.Object{"repository": repository}}); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(fixtures, "gh")
	if err := os.WriteFile(stub, []byte(`#!/bin/sh
set -eu
[ "$GH_HOST" = github.com ] && [ "$GH_REPO" = github.com/example/widgets ]
[ "$1" = api ] || exit 97
case "$2" in
  graphql)
    query=$(cat)
    case "$query" in *mutation*) exit 97 ;; esac
    printf 'read\n' >> "$STEWARD_TEST_FIXTURES/reads"
    cat "$STEWARD_TEST_FIXTURES/graphql.json"
    ;;
  'repos/example/widgets/branches?per_page=100')
    [ "$3" = --hostname ] && [ "$4" = github.com ] && [ "$5" = --method ] && [ "$6" = GET ]
    [ "$7" = --paginate ] && [ "$8" = --slurp ]
    printf 'read\n' >> "$STEWARD_TEST_FIXTURES/reads"
    printf '[[{"name":"main","commit":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}]]\n'
    ;;
  *) exit 97 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_PATH", stub)
	t.Setenv("STEWARD_TEST_FIXTURES", fixtures)
	for _, key := range []string{"GH_HOST", "GH_REPO", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(key, "")
	}
	if err := writeFile(root, "branches.json", contract.Object{"branches": []any{contract.Object{"name": "codex/change", "sha": strings.Repeat("a", 40), "pull_request_number": int64(3)}}}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) contract.Object {
		t.Helper()
		var out, stderr bytes.Buffer
		if err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), append(args, "--repo-root", root)); err != nil {
			t.Fatalf("native branches CLI %v: %v; %s", args, err, stderr.String())
		}
		result, err := contract.Decode(&out)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	prepared := run("branches", "prepare", "--input", "payload=branches.json", "--out", "prepared.json")
	plan, err := contract.ObjectAt(prepared, "data")
	if err != nil || prepared["command"] != "branch-cleanup-prepare" || !sameJSON(plan["operations"], []any{}) {
		t.Fatal("native preparation lost the no-op", prepared, err)
	}
	absences, err := contract.Objects(plan["data"].(contract.Object), "already_absent")
	if err != nil || len(absences) != 1 || absences[0]["kind"] != "already_absent" || absences[0]["delete_branch_on_merge"] != true {
		t.Fatal("native envelope lacks truthful absence", absences, err)
	}
	run("plan", "extract", "--repo", "https://github.com/example/widgets", "--input", "envelope=prepared.json", "--outer-command", "branch-cleanup-prepare", "--plan-command", "branch-cleanup-apply", "--out", "plan.json")
	readsBefore, err := os.ReadFile(filepath.Join(fixtures, "reads"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		applied := run("branches", "apply", "--input", "plan=plan.json", "--approve-plan-sha", plan["sha256"].(string))
		data, err := contract.ObjectAt(applied, "data")
		if err != nil || applied["command"] != "branch-cleanup-apply" || data["status"] != "completed" || !sameJSON(data["receipts"], []any{}) {
			t.Fatal("no-op apply fabricated a deletion receipt", applied, err)
		}
	}
	readsAfter, err := os.ReadFile(filepath.Join(fixtures, "reads"))
	if err != nil || len(readsAfter) <= len(readsBefore) {
		t.Fatal("no-op apply omitted live revalidation", err)
	}
}

func sameJSON(a, b any) bool {
	left, leftErr := contract.Canonical(a)
	right, rightErr := contract.Canonical(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}
