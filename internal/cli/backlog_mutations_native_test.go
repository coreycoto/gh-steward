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

// Exercise the public CLI through a separate native gh process. The fixtures
// retain GitHub's GraphQL and REST envelopes rather than a workflow fake's
// normalized source, so the test also checks the producer/consumer boundary.
func TestBacklogMutationCLINativeEnvelopeAndFreshRunnerReplay(t *testing.T) {
	root := checkout(t)
	fixtures := t.TempDir()
	for _, phase := range []string{"before", "after"} {
		issue := contract.Object{
			"id": "I_17", "number": int64(17), "title": phase,
			"body": "Reviewed body", "state": "OPEN",
			"url":       "https://github.com/example/widgets/issues/17",
			"labels":    contract.Object{"nodes": []any{}, "pageInfo": contract.Object{"hasNextPage": false}},
			"milestone": nil,
		}
		repo := contract.Object{
			"id": "R_1", "name": "widgets", "nameWithOwner": "example/widgets",
			"url":              "https://github.com/example/widgets",
			"owner":            contract.Object{"login": "example", "__typename": "Organization"},
			"defaultBranchRef": contract.Object{"name": "main"},
			"issues":           contract.Object{"nodes": []any{issue}, "pageInfo": contract.Object{"hasNextPage": false, "endCursor": nil}},
		}
		if err := writeFile(fixtures, "graphql-"+phase+".json", contract.Object{"data": contract.Object{"repository": repo}}); err != nil {
			t.Fatal(err)
		}
		rest := contract.Object{
			"number": int64(17), "id": int64(10017), "node_id": "I_17",
			"html_url": "https://github.com/example/widgets/issues/17",
			"title":    phase, "body": "Reviewed body", "state": "open", "labels": []any{}, "milestone": nil,
		}
		if err := writeFile(fixtures, "rest-"+phase+".json", rest); err != nil {
			t.Fatal(err)
		}
	}

	stub := filepath.Join(fixtures, "gh")
	if err := os.WriteFile(stub, []byte(`#!/bin/sh
set -eu
[ "$GH_HOST" = github.com ] && [ "$GH_REPO" = github.com/example/widgets ]
[ "$1" = api ] || exit 97
phase=before
[ ! -f "$STEWARD_TEST_FIXTURES/updated" ] || phase=after
case "$2" in
  graphql)
    cat > "$STEWARD_TEST_FIXTURES/graphql-input.json"
    cat "$STEWARD_TEST_FIXTURES/graphql-$phase.json"
    ;;
  'repos/example/widgets/milestones?state=all&per_page=100'|'repos/example/widgets/labels?per_page=100')
    [ "$3" = --hostname ] && [ "$4" = github.com ] && [ "$5" = --method ] && [ "$6" = GET ]
    [ "$7" = --paginate ] && [ "$8" = --slurp ]
    printf '[[]]\n'
    ;;
  repos/example/widgets/issues/17)
    [ "$3" = --hostname ] && [ "$4" = github.com ] && [ "$5" = --method ]
    case "$6" in
      GET) cat "$STEWARD_TEST_FIXTURES/rest-$phase.json" ;;
      PATCH)
        [ "$7" = --input ] && [ "$8" = - ]
        cat > "$STEWARD_TEST_FIXTURES/patch-input.json"
        printf 'write\n' >> "$STEWARD_TEST_FIXTURES/writes"
        touch "$STEWARD_TEST_FIXTURES/updated"
        cat "$STEWARD_TEST_FIXTURES/rest-after.json"
        ;;
      *) exit 97 ;;
    esac
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
	if err := writeFile(root, "intent.json", contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"issue_number": int64(17), "title": "after"}}}); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(root, "projects.json", contract.Object{"projects": []any{}}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) contract.Object {
		t.Helper()
		var out, stderr bytes.Buffer
		if err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), append(args, "--repo-root", root)); err != nil {
			t.Fatalf("native CLI %v: %v; %s", args, err, stderr.String())
		}
		result, err := contract.Decode(&out)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	prepared := run("backlog-mutations", "prepare", "--input", "payload=intent.json", "--input", "projects=projects.json", "--out", "plan.json")
	if prepared["command"] != "backlog-mutations-prepare" {
		t.Fatal("preparation lost its command", prepared)
	}
	rawPlan, err := contract.ObjectAt(prepared, "data")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contract.ParsePlan(rawPlan)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Operations) != 1 || plan.Operations[0].Kind != "issue-update" {
		t.Fatal("native source did not derive one scoped issue update", plan.Operations)
	}
	saved, err := loadObject(root, "plan.json")
	if err != nil {
		t.Fatal(err)
	}
	if saved["command"] != prepared["command"] {
		t.Fatal("saved prepare result differs from stdout", saved)
	}
	// A new Runner and native process for each apply proves that durable receipts
	// survive the caller's process lifetime and prevent a second PATCH. Pass the
	// exact --out result envelope without extracting or rewriting its nested plan.
	for i := 0; i < 2; i++ {
		applied := run("backlog-mutations", "apply", "--input", "plan=plan.json", "--approve-plan-sha", plan.SHA256)
		data, err := contract.ObjectAt(applied, "data")
		if err != nil {
			t.Fatal(err)
		}
		receipts, err := contract.Objects(data, "receipts")
		if err != nil || applied["command"] != "backlog-mutations-apply" || data["command"] != "backlog-mutations-apply" || data["status"] != "completed" || len(receipts) != 1 {
			t.Fatal("apply lost its completed receipt envelope", applied, err)
		}
	}
	patch, err := loadObject(fixtures, "patch-input.json")
	if err != nil || len(patch) != 1 || patch["title"] != "after" {
		t.Fatal("native PATCH exceeded the reviewed scope", patch, err)
	}
	writes, err := os.ReadFile(filepath.Join(fixtures, "writes"))
	if err != nil || strings.TrimSpace(string(writes)) != "write" {
		t.Fatal("fresh Runner replay dispatched a second mutation", string(writes), err)
	}
}
