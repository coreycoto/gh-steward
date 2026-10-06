package runrecovery

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func reviewedExecutionFixture(t *testing.T) (*Engine, string, Object, Object, Object) {
	t.Helper()
	return reviewedPlanSetFixture(t, "execution", true)
}

func reviewedPlanSetFixture(t *testing.T, profile string, withReview bool) (*Engine, string, Object, Object, Object) {
	t.Helper()
	policy := reviewedDispatchPolicy([]any{})
	sync := policy["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)["sync"].(Object)
	command := "execution-sync"
	if profile == "quarter" {
		command = "batch-apply"
	}
	sync["domain_profile"], sync["command"] = profile, command
	sync["allowed_operation_kinds"] = []any{"issue-comment-upsert"}
	sync["approval"].(Object)["required_inputs"] = Object{"operation": "apply"}
	repo := policyTestRepository(t)
	e, err := NewEngine(policy, repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contract.PreparePlan(command, repo,
		Object{"github": Object{"live": true, "complete": true}},
		Object{"selector": Object{"issue_number": int64(17), "pull_request_number": int64(0), "skip_project_sync": true, "project": nil}},
		[]contract.Operation{}, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	target := Object{"plans": []any{Object{"name": "sync", "command": command, "sha256": plan.SHA256}}}
	targetBytes, err := Canonical(target)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	event := Object{"repository": Object{"full_name": repo.FullName()}, "inputs": Object{
		"approved-plan-sha": plan.SHA256, "reviewed-plan-run-id": "77", "operation": "apply",
	}}
	if err := persistPackageJSON(root, "events/dispatch-event.json", event); err != nil {
		t.Fatal(err)
	}
	eventBytes, err := ReadPackageFile(root, "events/dispatch-event.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.InitializeContext(root, Object{
		"workflow_file": "task.yml", "repository": repo.FullName(), "recovery_key": "plan-set-" + SHA256(targetBytes),
		"run_name": "Execution", "run_id": int64(100), "attempt": int64(1),
		"attempt_target": target, "dispatch_steps": []any{"Apply reviewed plan"},
	}); err != nil {
		t.Fatal(err)
	}
	review := Object{
		"schema_version": 1, "workflow_file": "task.yml", "repository": repo.FullName(),
		"plan_name": "sync", "command": command, "plan_sha256": plan.SHA256,
		"approval_input": "approved-plan-sha", "workflow_event": "workflow_dispatch",
		"workflow_run_id": int64(100), "workflow_run_attempt": int64(1), "reviewed_plan_run_id": int64(77),
		"event_path": "events/dispatch-event.json", "event_sha256": SHA256(eventBytes),
	}
	input := Object{
		"relative": "plans/sync.json", "name": "sync", "command": command, "repository": repo.FullName(),
		"plan": plan.Object(),
	}
	if withReview {
		if err := persistPackageJSON(root, "reviews/sync.json", review); err != nil {
			t.Fatal(err)
		}
		reviewBytes, err := ReadPackageFile(root, "reviews/sync.json")
		if err != nil {
			t.Fatal(err)
		}
		input["review_path"], input["review_sha256"] = "reviews/sync.json", SHA256(reviewBytes)
	}
	context, err := e.RecordContextPlan(root, input)
	if err != nil {
		t.Fatalf("accepted execution plan-set policy rejected its exact manual review at registration: %v", err)
	}
	return e, root, context, context["plans"].([]any)[0].(Object), plan.Object()
}

func TestReviewedPlanSetDispatchRequiresApprovalBeforePersistingTransition(t *testing.T) {
	for _, profile := range []string{"execution", "quarter"} {
		t.Run(profile, func(t *testing.T) {
			e, root, context, entry, plan := reviewedPlanSetFixture(t, profile, false)
			if context["phase"] != "prepared" || entry["status"] != "prepared" {
				t.Fatal("unreviewed preview did not remain prepared")
			}
			if err := e.ValidateRecoveryPlan(context, entry, plan, root); err != nil {
				t.Fatalf("plan-only preview was rejected: %v", err)
			}
			before, err := ReadPackageFile(root, "run-context.json")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.MarkContextPlan(root, "sync", "dispatching"); err == nil || !strings.Contains(err.Error(), "exact approval event") {
				t.Fatalf("unreviewed preview entered dispatch: %v", err)
			}
			after, err := ReadPackageFile(root, "run-context.json")
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected transition changed persisted context: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "journals")); !os.IsNotExist(err) {
				t.Fatalf("rejected transition created a journal: %v", err)
			}
		})
	}
}

func TestReviewedPlanSetDispatchPersistsRecoverableApprovedTransition(t *testing.T) {
	for _, profile := range []string{"execution", "quarter"} {
		t.Run(profile, func(t *testing.T) {
			e, root, _, _, plan := reviewedPlanSetFixture(t, profile, true)
			context, err := e.MarkContextPlan(root, "sync", "dispatching")
			if err != nil {
				t.Fatal(err)
			}
			entry := context["plans"].([]any)[0].(Object)
			if context["phase"] != "dispatching" || entry["status"] != "dispatching" {
				t.Fatal("reviewed transition did not enter dispatch")
			}
			if err := e.ValidateRecoveryPlan(context, entry, plan, root); err != nil {
				t.Fatalf("persisted dispatch lost approval: %v", err)
			}
			if _, err := e.MarkContextPlan(root, "sync", "dispatching"); err != nil {
				t.Fatalf("unchanged approved transition was not idempotent: %v", err)
			}
		})
	}
}

func TestReviewedExecutionDispatchRegistersAndPreservesOriginalApprovalAfterInterruption(t *testing.T) {
	e, root, context, entry, plan := reviewedExecutionFixture(t)
	for _, phase := range []string{"prepared", "dispatching", "completed"} {
		t.Run(phase, func(t *testing.T) {
			current := cloneNativeObject(context)
			currentEntry := cloneNativeObject(entry)
			current["phase"], currentEntry["status"] = phase, phase
			current["plans"] = []any{currentEntry}
			if err := e.ValidateRecoveryPlan(current, currentEntry, plan, root); err != nil {
				t.Fatalf("original approval rejected in %s: %v", phase, err)
			}
			current["recovered_from_run_id"], current["recovered_from_attempt"] = int64(100), int64(1)
			current["workflow_run_id"], current["workflow_run_attempt"] = int64(101), int64(2)
			if err := e.ValidateRecoveryPlan(current, currentEntry, plan, root); err != nil {
				t.Fatalf("observer lost original approval in %s: %v", phase, err)
			}
		})
	}
}

func TestReviewedExecutionDispatchRejectsChangedApprovalAndSourceEvidence(t *testing.T) {
	for _, mutation := range []string{"plan-sha", "reviewed-run", "operation", "repository", "origin", "extra-review-field", "review-bytes", "missing-dispatched-review"} {
		t.Run(mutation, func(t *testing.T) {
			e, root, context, entry, plan := reviewedExecutionFixture(t)
			event, err := LoadJSON(filepath.Join(root, "events/dispatch-event.json"))
			if err != nil {
				t.Fatal(err)
			}
			review, err := LoadJSON(filepath.Join(root, "reviews/sync.json"))
			if err != nil {
				t.Fatal(err)
			}
			eventObject, reviewObject := event.(Object), review.(Object)
			switch mutation {
			case "plan-sha":
				eventObject["inputs"].(Object)["approved-plan-sha"] = strings.Repeat("a", 64)
			case "reviewed-run":
				reviewObject["reviewed_plan_run_id"] = int64(78)
			case "operation":
				eventObject["inputs"].(Object)["operation"] = "prepare"
			case "repository":
				eventObject["repository"] = Object{"full_name": "other/widgets"}
			case "origin":
				reviewObject["workflow_run_id"] = int64(99)
			case "extra-review-field":
				reviewObject["implicit_approval"] = true
			case "missing-dispatched-review":
				context["phase"], entry["status"] = "dispatching", "dispatching"
				delete(entry, "review_path")
				delete(entry, "review_sha256")
			}
			if err := persistPackageJSON(root, "events/dispatch-event.json", eventObject); err != nil {
				t.Fatal(err)
			}
			eventBytes, _ := ReadPackageFile(root, "events/dispatch-event.json")
			reviewObject["event_sha256"] = SHA256(eventBytes)
			if err := persistPackageJSON(root, "reviews/sync.json", reviewObject); err != nil {
				t.Fatal(err)
			}
			reviewBytes, _ := ReadPackageFile(root, "reviews/sync.json")
			if mutation != "missing-dispatched-review" {
				entry["review_sha256"] = SHA256(reviewBytes)
			}
			if mutation == "review-bytes" {
				entry["review_sha256"] = strings.Repeat("0", 64)
			}
			if err := e.ValidateRecoveryPlan(context, entry, plan, root); err == nil {
				t.Fatalf("changed %s retained execution authority", mutation)
			}
		})
	}
}
