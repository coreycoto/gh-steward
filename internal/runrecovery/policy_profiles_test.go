package runrecovery

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func executionProfileFixture(t *testing.T, targetKind, eventKind string) (*Engine, string, Object, Object, Object) {
	t.Helper()
	repo := policyTestRepository(t)
	policy := Object{"schema_version": 1, "workflows": Object{"task.yml": Object{
		"mutator_step_alternatives": []any{[]any{"Apply execution"}}, "reviewed_source_shas": []any{}, "allow_publication": false,
		"plans": Object{"execution": Object{"command": "execution-sync", "domain_profile": "execution",
			"allowed_operation_kinds": []any{"issue-comment-upsert"}, "attempt_target": "execution",
			"approval": Object{"kind": "git-slop-execution"}, "event": Object{"kind": "execution_event", "path": "events/dispatch-event.json"}, "parent_merge": nil}},
	}}}
	e, err := NewEngine(policy, repo)
	if err != nil {
		t.Fatal(err)
	}
	number := int64(17)
	selector := Object{"issue_number": int64(0), "pull_request_number": int64(0), "skip_project_sync": true, "project": nil}
	if targetKind == "issue" {
		selector["issue_number"] = number
	} else {
		selector["pull_request_number"] = number
	}
	operation := contract.Operation{ID: "execution:link-comment", Kind: "issue-comment-upsert", Target: Object{"issue_number": number, "marker": "<!-- work -->"}, Before: Object{"body": nil}, After: Object{"body": "Reviewed work state"}}
	plan, err := contract.PreparePlan("execution-sync", repo, Object{"github": Object{"live": true, "complete": true}}, Object{"selector": selector}, []contract.Operation{operation}, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	event := Object{"repository": Object{"full_name": repo.FullName()}}
	if eventKind == "pull-request" {
		event["action"], event["number"], event["pull_request"] = "synchronize", number, Object{"number": number}
	} else {
		inputs := Object{"issue_number": "", "pr_number": ""}
		if targetKind == "issue" {
			inputs["issue_number"] = fmt.Sprint(number)
		} else {
			inputs["pr_number"] = fmt.Sprint(number)
		}
		event["inputs"] = inputs
	}
	eventBytes, err := Canonical(event)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPackageFile(root, "events/dispatch-event.json", eventBytes); err != nil {
		t.Fatal(err)
	}
	target := Object{"kind": targetKind, "number": number}
	ctx, err := e.InitializeContext(root, Object{"workflow_file": "task.yml", "repository": repo.FullName(), "recovery_key": "execution-17", "run_name": "Execution", "run_id": int64(100), "attempt": int64(1), "attempt_target": Object{"plan_sha256": plan.SHA256, "target": target, "event_sha256": SHA256(eventBytes)}, "dispatch_steps": []any{"Apply execution"}})
	if err != nil {
		t.Fatal(err)
	}
	review := Object{"schema_version": 1, "workflow_file": "task.yml", "repository": repo.FullName(), "workflow_run_id": int64(100), "workflow_run_attempt": int64(1), "name": "execution", "command": "execution-sync", "plan_sha256": plan.SHA256, "target": target, "event_path": "events/dispatch-event.json", "event_sha256": SHA256(eventBytes)}
	reviewBytes, err := Canonical(review)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPackageFile(root, "reviews/execution.json", reviewBytes); err != nil {
		t.Fatal(err)
	}
	ctx, err = e.RecordContextPlan(root, Object{"relative": "plans/execution.json", "name": "execution", "command": "execution-sync", "repository": repo.FullName(), "plan": plan.Object(), "review_path": "reviews/execution.json", "review_sha256": SHA256(reviewBytes)})
	if err != nil {
		t.Fatal(err)
	}
	entry := ctx["plans"].([]any)[0].(Object)
	return e, root, ctx, entry, plan.Object()
}

func TestExecutionRecoveryBindsIssueManualPRAndAutomaticPREvents(t *testing.T) {
	for _, selection := range [][2]string{{"issue", "manual"}, {"pull-request", "manual"}, {"pull-request", "pull-request"}} {
		t.Run(strings.Join(selection[:], "/"), func(t *testing.T) {
			e, root, ctx, entry, plan := executionProfileFixture(t, selection[0], selection[1])
			if err := e.ValidateRecoveryPlan(ctx, entry, plan, root); err != nil {
				t.Fatalf("valid exact execution event rejected: %v", err)
			}
			ctx["recovered_from_run_id"], ctx["recovered_from_attempt"] = int64(100), int64(1)
			ctx["workflow_run_id"], ctx["workflow_run_attempt"] = int64(101), int64(2)
			if err := e.ValidateRecoveryPlan(ctx, entry, plan, root); err != nil {
				t.Fatalf("observer lost the immutable original event/review: %v", err)
			}
		})
	}
}

func TestExecutionRecoveryRejectsForeignEventRetargetingAndChangedApproval(t *testing.T) {
	for _, mutation := range []string{"event-bytes", "target-kind", "target-number", "review-plan", "review-origin", "review-target"} {
		t.Run(mutation, func(t *testing.T) {
			e, root, ctx, entry, plan := executionProfileFixture(t, "issue", "manual")
			switch mutation {
			case "event-bytes":
				if err := persistPackageJSON(root, "events/dispatch-event.json", Object{"repository": Object{"full_name": "foreign/repo"}}); err != nil {
					t.Fatal(err)
				}
			case "target-kind":
				ctx["attempt_target"].(Object)["target"] = Object{"kind": "pull-request", "number": int64(17)}
			case "target-number":
				ctx["attempt_target"].(Object)["target"] = Object{"kind": "issue", "number": int64(18)}
			default:
				value, err := LoadJSON(filepath.Join(root, "reviews/execution.json"))
				if err != nil {
					t.Fatal(err)
				}
				review := value.(Object)
				switch mutation {
				case "review-plan":
					review["plan_sha256"] = strings.Repeat("a", 64)
				case "review-origin":
					review["workflow_run_id"] = int64(99)
				case "review-target":
					review["target"] = Object{"kind": "issue", "number": int64(18)}
				}
				raw, err := Canonical(review)
				if err != nil {
					t.Fatal(err)
				}
				entry["review_sha256"] = SHA256(raw)
				if err := persistPackageFile(root, "reviews/execution.json", raw); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.ValidateRecoveryPlan(ctx, entry, plan, root); err == nil {
				t.Fatalf("execution recovery accepted %s", mutation)
			}
		})
	}
}

func TestGovernanceRecoveryRequiresExactReviewedOperationOrder(t *testing.T) {
	for _, mutation := range []string{"none", "status", "plan", "order", "extra-plan"} {
		t.Run(mutation, func(t *testing.T) {
			repo := policyTestRepository(t)
			policy := Object{"schema_version": 1, "workflows": Object{"task.yml": Object{
				"mutator_step_alternatives": []any{[]any{"Apply labels"}}, "reviewed_source_shas": []any{}, "allow_publication": false,
				"plans": Object{"labels": Object{"command": "governance-apply", "domain_profile": "governance", "allowed_operation_kinds": []any{"label-create"}, "attempt_target": "single_plan_sha256", "approval": Object{"kind": "git-slop-governance"}, "event": nil, "parent_merge": nil}},
			}}}
			e, err := NewEngine(policy, repo)
			if err != nil {
				t.Fatal(err)
			}
			ops := []contract.Operation{}
			for _, name := range []string{"bug", "maintenance"} {
				ops = append(ops, contract.Operation{ID: "governance:label:create:" + name, Kind: "label-create", Target: Object{"name": name, "color": "a1b2c3", "description": name}, Before: Object{"exists": false, "name": nil, "color": nil, "description": nil}, After: Object{"exists": true, "name": name, "color": "a1b2c3", "description": name}})
			}
			plan, err := contract.PreparePlan("governance-apply", repo, Object{"github": Object{"live": true, "complete": true}}, Object{"kind": "label-palette"}, ops, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			review := Object{"status": "approved", "approved_plans": []any{Object{"name": "labels", "plan_sha256": plan.SHA256, "operation_ids": []any{ops[0].ID, ops[1].ID}}}}
			approved := review["approved_plans"].([]any)[0].(Object)
			switch mutation {
			case "status":
				review["status"] = "declined"
			case "plan":
				approved["plan_sha256"] = strings.Repeat("a", 64)
			case "order":
				approved["operation_ids"] = []any{ops[1].ID, ops[0].ID}
			case "extra-plan":
				review["approved_plans"] = append(review["approved_plans"].([]any), approved)
			}
			raw, err := Canonical(review)
			if err != nil {
				t.Fatal(err)
			}
			if err := persistPackageFile(root, "reviews/labels.json", raw); err != nil {
				t.Fatal(err)
			}
			entry := Object{"name": "labels", "command": plan.Command, "sha256": plan.SHA256, "review_path": "reviews/labels.json", "review_sha256": SHA256(raw)}
			ctx := Object{"workflow_file": "task.yml", "repository": repo.FullName(), "phase": "prepared", "attempt_target": Object{"plan_sha256": plan.SHA256}}
			err = e.ValidateRecoveryPlan(ctx, entry, plan.Object(), root)
			if mutation == "none" && err != nil {
				t.Fatalf("exact ordered governance review rejected: %v", err)
			} else if mutation != "none" && err == nil {
				t.Fatalf("governance recovery accepted %s", mutation)
			}
		})
	}
}
