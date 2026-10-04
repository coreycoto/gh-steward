package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func refuseNativeGH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	trace := filepath.Join(dir, "invoked")
	stub := filepath.Join(dir, "gh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf called > \"$STEWARD_TEST_GH_TRACE\"\nexit 97\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_PATH", stub)
	t.Setenv("STEWARD_TEST_GH_TRACE", trace)
	return trace
}

func TestApplyRejectsMissingApprovalAlteredScopeAndUnusedSelectorsBeforeGH(t *testing.T) {
	root := checkout(t)
	trace := refuseNativeGH(t)
	repo := contract.Repository{Host: "github.com", Owner: "example", Name: "widgets", URL: "https://github.com/example/widgets"}
	for _, family := range []string{"relationships", "backlog", "backlog-mutations", "review", "quarter", "merge", "execution", "artifacts", "governance", "closeout", "delivery", "branches"} {
		t.Run(family, func(t *testing.T) {
			plan, err := contract.PreparePlan(nativeWorkflowCommand(family), repo,
				contract.Object{"inventory": contract.Object{"live": true, "complete": true}}, contract.Object{}, nil, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name      string
				input     contract.Object
				arguments []string
			}{
				{"missing approval", plan.Object(), nil},
				{"different approval", plan.Object(), []string{"--approve-plan-sha", strings.Repeat("0", 64)}},
				{"selector override", plan.Object(), []string{"--approve-plan-sha", plan.SHA256, "--project-number", "1"}},
				{"state override", plan.Object(), []string{"--approve-plan-sha", plan.SHA256, "--state", "all"}},
				{"extra input", plan.Object(), []string{"--approve-plan-sha", plan.SHA256, "--input", "payload=payload.json"}},
				{"v1 plan", contract.Object{"schema_version": 1}, []string{"--approve-plan-sha", plan.SHA256}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if err := writeFile(root, "plan.json", tc.input); err != nil {
						t.Fatal(err)
					}
					if err := writeFile(root, "payload.json", contract.Object{}); err != nil {
						t.Fatal(err)
					}
					var out, stderr bytes.Buffer
					args := append([]string{family, "apply", "--repo-root", root, "--input", "plan=plan.json"}, tc.arguments...)
					err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), args)
					if err == nil || out.Len() != 0 {
						t.Fatalf("invalid apply emitted success: %v %s", err, out.String())
					}
				})
			}
			for _, command := range []string{"another-command", nativeWorkflowCommand(family)} {
				target := repo
				if command == nativeWorkflowCommand(family) {
					target.Owner = "foreign"
					target.URL = "https://github.com/foreign/widgets"
				}
				other, err := contract.PreparePlan(command, target, plan.Sources, plan.Data, nil, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if err := writeFile(root, "plan.json", other.Object()); err != nil {
					t.Fatal(err)
				}
				var out, stderr bytes.Buffer
				err = (Runner{Out: &out, Err: &stderr}).Run(context.Background(), []string{family, "apply", "--repo-root", root, "--input", "plan=plan.json", "--approve-plan-sha", other.SHA256})
				if err == nil || out.Len() != 0 {
					t.Fatal("hash-consistent foreign command/repository accepted")
				}
			}
		})
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("rejected apply invoked native gh", err)
	}
}

func TestPrepareRejectsUnusedFlagsAndInputsBeforeGH(t *testing.T) {
	root := checkout(t)
	trace := refuseNativeGH(t)
	if err := writeFile(root, "payload.json", contract.Object{}); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"relationships", "backlog", "backlog-mutations", "review", "quarter", "merge", "execution", "artifacts", "governance", "closeout", "delivery", "branches"} {
		for _, tail := range [][]string{{"--state", "all"}, {"--join-project"}, {"--input", "ignored=payload.json"}} {
			var out, stderr bytes.Buffer
			err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), append([]string{family, "prepare", "--repo-root", root}, tail...))
			if err == nil || out.Len() != 0 {
				t.Fatalf("unused preparation selector accepted: %s %v", family, tail)
			}
		}
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("rejected preparation invoked native gh", err)
	}
}

func TestNativeSnapshotReadFailureDoesNotEmitSuccessfulEnvelope(t *testing.T) {
	root := checkout(t)
	refuseNativeGH(t)
	if err := writeFile(root, "artifact.json", contract.Object{"run_id": int64(8)}); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(root, "linked.json", contract.Object{"issue_numbers": []any{int64(17)}}); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(root, "policy.json", contract.Object{"marker_prefix": "<!-- linked-pr -->", "pr_number_pattern": `PR #(?P<number>[0-9]+)`}); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(root, "selector.json", contract.Object{"issue_number": int64(17), "pull_request_number": int64(0), "skip_project_sync": true, "project": nil}); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(root, "execution-policy.json", contract.Object{"statuses": contract.Object{"done": "Done", "active": "In Progress", "todo": "Todo"}, "status_field": "Status", "pr_link_marker_prefix": "<!-- pr:", "pr_link_number_pattern": `PR #(?P<number>[0-9]+)`, "linked_issue_marker_prefix": "<!-- issue:", "link_state_marker_prefix": "<!-- link:"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"snapshot", "projects", "--project-owner", "planner", "--project-owner-type", "User"},
		{"snapshot", "artifacts", "--input", "payload=artifact.json"},
		{"snapshot", "linked-prs", "--input", "payload=linked.json", "--policy", "policy.json"},
		{"snapshot", "execution", "--input", "selector=selector.json", "--policy", "execution-policy.json"},
	} {
		var out, stderr bytes.Buffer
		err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), append(args, "--repo-root", root))
		if err == nil || out.Len() != 0 {
			t.Fatalf("failed live snapshot became success: %v %s", err, out.String())
		}
	}
}

func TestProjectDiscoveryRejectsUnusedSelectorsBeforeGH(t *testing.T) {
	root := checkout(t)
	trace := refuseNativeGH(t)
	if err := writeFile(root, "object.json", contract.Object{}); err != nil {
		t.Fatal(err)
	}
	for _, tail := range [][]string{{"--project-number", "14"}, {"--project-id", "P_14"}, {"--state", "all"}, {"--join-project"}, {"--input", "payload=object.json"}, {"--policy", "object.json"}} {
		var out, stderr bytes.Buffer
		args := append([]string{"snapshot", "projects", "--repo-root", root, "--project-owner", "planner", "--project-owner-type", "User"}, tail...)
		if err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), args); err == nil || out.Len() != 0 {
			t.Fatal("unused Project discovery selector accepted", tail, err)
		}
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("rejected Project discovery invoked provider", err)
	}
}

func TestExecutionSnapshotRejectsOverridesAndIgnoredInputsBeforeGH(t *testing.T) {
	root := checkout(t)
	trace := refuseNativeGH(t)
	if err := writeFile(root, "object.json", contract.Object{}); err != nil {
		t.Fatal(err)
	}
	for _, tail := range [][]string{{"--state", "all"}, {"--project-number", "1"}, {"--input", "ignored=object.json"}, {"--approve-plan-sha", strings.Repeat("a", 64)}} {
		var out, stderr bytes.Buffer
		args := append([]string{"snapshot", "execution", "--repo-root", root, "--input", "selector=object.json", "--policy", "object.json"}, tail...)
		if err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), args); err == nil || out.Len() != 0 {
			t.Fatal("snapshot selector override accepted", tail, err)
		}
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("rejected read called native GH", err)
	}
}

func TestGovernancePrepareRejectsUnspecifiedPolicyBeforeGH(t *testing.T) {
	root := checkout(t)
	trace := refuseNativeGH(t)
	for _, payload := range []contract.Object{
		{"kind": "label-palette"},
		{"kind": "label-palette", "policy": contract.Object{}, "auto_approve": true},
		{"kind": "unknown", "policy": contract.Object{}},
		{"kind": "label-palette", "policy": false},
		{"kind": "label-palette", "policy": contract.Object{"labels": []any{}}},
		{"kind": "bootstrap-backlog", "policy": contract.Object{"project": "implicit"}},
	} {
		if err := writeFile(root, "request.json", payload); err != nil {
			t.Fatal(err)
		}
		var out, stderr bytes.Buffer
		err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), []string{"governance", "prepare", "--repo-root", root, "--input", "payload=request.json"})
		if err == nil || out.Len() != 0 {
			t.Fatal("unspecified governance policy emitted a qualified plan", err, out.String())
		}
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("unqualified governance request invoked native gh", err)
	}
}

func TestBacklogMutationPrepareRequiresExplicitTypedProjectScopesBeforeGH(t *testing.T) {
	root := checkout(t)
	trace := refuseNativeGH(t)
	if err := writeFile(root, "payload.json", contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"issue_number": int64(17), "title": "Edited"}}}); err != nil {
		t.Fatal(err)
	}
	for _, projects := range []contract.Object{
		{}, {"projects": nil}, {"projects": []any{}, "default_owner": "planner"},
		{"projects": []any{contract.Object{"title": "Backlog"}}},
		{"projects": []any{contract.Object{"host": "github.com", "owner": "planner", "owner_type": "User", "number": float64(2.5), "id": "P_14", "title": "Backlog"}}},
	} {
		if err := writeFile(root, "projects.json", projects); err != nil {
			t.Fatal(err)
		}
		var out, stderr bytes.Buffer
		err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), []string{"backlog-mutations", "prepare", "--repo-root", root, "--input", "payload=payload.json", "--input", "projects=projects.json"})
		if err == nil || out.Len() != 0 {
			t.Fatal("unqualified Project scopes produced a mutation plan", projects, err)
		}
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatal("invalid Project scopes reached provider", err)
	}
}
