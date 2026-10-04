package runrecovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func preparedHandoffFixture(t *testing.T) (*Engine, *recoveryReaderFixture, Invocation, Object) {
	t.Helper()
	repo := policyTestRepository(t)
	e, err := NewEngine(reviewedDispatchPolicy([]any{}), repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contract.PreparePlan("batch-apply", repo, Object{"github": Object{"live": true, "complete": true}}, Object{"scope": "reviewed"}, []contract.Operation{nativeTestOperation("remove-old-branch", "branch-delete")}, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	target := Object{"plans": []any{Object{"name": "sync", "command": plan.Command, "sha256": plan.SHA256}}}
	encoded, err := Canonical(target)
	if err != nil {
		t.Fatal(err)
	}
	key := "plan-set-" + SHA256(encoded)
	event := Object{"repository": Object{"full_name": "example/widgets"}, "inputs": Object{"approved-plan-sha": plan.SHA256, "reviewed-plan-run-id": "77", "continue": true, "release-channel": "stable"}}
	eventBytes, err := Canonical(event)
	if err != nil {
		t.Fatal(err)
	}
	review := Object{"schema_version": 1, "workflow_file": "task.yml", "repository": "example/widgets", "plan_name": "sync", "command": plan.Command, "plan_sha256": plan.SHA256, "approval_input": "approved-plan-sha", "workflow_event": "workflow_dispatch", "workflow_run_id": int64(90), "workflow_run_attempt": int64(1), "reviewed_plan_run_id": int64(77), "event_path": "events/dispatch-event.json", "event_sha256": SHA256(eventBytes)}
	reviewBytes, err := Canonical(review)
	if err != nil {
		t.Fatal(err)
	}
	entry := Object{"name": "sync", "path": "plans/sync.json", "command": plan.Command, "sha256": plan.SHA256, "journal_id": journalIDForPlan(plan.Object()), "status": "prepared", "review_path": "reviews/sync.json", "review_sha256": SHA256(reviewBytes)}
	runContext := Object{"schema_version": 1, "workflow_file": "task.yml", "repository": "example/widgets", "recovery_key": key, "run_name": "Current", "workflow_run_id": int64(90), "workflow_run_attempt": int64(1), "attempt_target": target, "phase": "prepared", "dispatch_steps": []any{"Apply reviewed plan"}, "plans": []any{entry}}
	temp := t.TempDir()
	run := recoveryHistoryRun(90, 1, "Current")
	workflowTarget, err := TargetObject("task.yml", repo.URL, 9, "workflow-history-v2")
	if err != nil {
		t.Fatal(err)
	}
	chain, err := EmptyChain(workflowTarget)
	if err != nil {
		t.Fatal(err)
	}
	contextBytes, err := Canonical(runContext)
	if err != nil {
		t.Fatal(err)
	}
	chainBytes, err := Canonical(chain)
	if err != nil {
		t.Fatal(err)
	}
	planBytes, err := Canonical(plan.Object())
	if err != nil {
		t.Fatal(err)
	}
	payload := recoveryZip(t, recoveryZipEntry{name: "run-context.json", data: contextBytes}, recoveryZipEntry{name: "settlement-chain.json", data: chainBytes}, recoveryZipEntry{name: "plans/sync.json", data: planBytes}, recoveryZipEntry{name: "events/dispatch-event.json", data: eventBytes}, recoveryZipEntry{name: "reviews/sync.json", data: reviewBytes})
	metadata := Object{"id": int64(31), "name": RecoveryArtifactName(workflowTarget, 90, 1) + "-handoff-00", "expired": false, "digest": "sha256:" + SHA256(payload), "workflow_run": Object{"id": int64(90), "head_sha": strings.Repeat("a", 40)}}
	reader := &recoveryReaderFixture{reads: map[string]Object{"repos/example/widgets/actions/artifacts/31": metadata}, pages: map[string][]any{"repos/example/widgets/actions/workflows/task.yml/runs?per_page=100": {Object{"total_count": 1, "workflow_runs": []any{run}}}}, archives: map[int64][]byte{31: payload}}
	return e, reader, Invocation{Workflow: "task.yml", RunID: 90, Attempt: 1, RunName: "Current", RecoveryKey: key, PackageRoot: filepath.Join(temp, "invocation"), RunnerTemp: temp}, metadata
}

func TestPreparedApplyHandoffRequiresExactPlanReviewAndCompleteFrontier(t *testing.T) {
	for _, mutation := range []string{"none", "wrong-key", "changed-plan", "started-journal", "prior-run", "missing-review"} {
		t.Run(mutation, func(t *testing.T) {
			e, r, inv, metadata := preparedHandoffFixture(t)
			switch mutation {
			case "wrong-key":
				inv.RecoveryKey = "different"
			case "prior-run":
				r.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"] = []any{Object{"total_count": 2, "workflow_runs": []any{recoveryHistoryRun(1, 1, "Prior"), recoveryHistoryRun(90, 1, "Current")}}}
			case "changed-plan", "started-journal", "missing-review":
				files, err := archiveFiles(r.archives[31], false)
				if err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "changed-plan":
					files["plans/sync.json"] = []byte(`{"schema_version":2,"changed":true}`)
				case "missing-review":
					delete(files, "reviews/sync.json")
				case "started-journal":
					contextValue, err := DecodeValue(files["run-context.json"])
					if err != nil {
						t.Fatal(err)
					}
					id := contextValue.(Object)["plans"].([]any)[0].(Object)["journal_id"].(string)
					files["journal/"+id+".json"] = []byte(`{"steps":[{"status":"unknown"}]}`)
				}
				entries := []recoveryZipEntry{}
				for name, data := range files {
					entries = append(entries, recoveryZipEntry{name: name, data: data})
				}
				r.archives[31] = recoveryZip(t, entries...)
				metadata["digest"] = "sha256:" + SHA256(r.archives[31])
			}
			result, err := e.AcquireHandoff(context.Background(), r, inv, 31, metadata["digest"].(string))
			if mutation == "none" {
				if err != nil || result["purpose"] != "apply" {
					t.Fatalf("valid exact prepared handoff rejected: %v %v", result, err)
				}
			} else if err == nil {
				t.Fatal("handoff authorized an unreviewed plan or unsettled predecessor")
			}
		})
	}
}

func TestHandoffUsesImmutableUploadIdentityBeforeConfinedExtraction(t *testing.T) {
	for _, mutation := range []string{"none", "name", "digest", "source", "unsafe-zip", "held", "unsettled", "apply-without-context"} {
		t.Run(mutation, func(t *testing.T) {
			engine := testRecoveryEngine(t, false)
			temp := t.TempDir()
			target, err := TargetObject("task.yml", "https://github.com/example/widgets", 9, "workflow-history-v2")
			if err != nil {
				t.Fatal(err)
			}
			chain, err := EmptyChain(target)
			if err != nil {
				t.Fatal(err)
			}
			data, err := Canonical(chain)
			if err != nil {
				t.Fatal(err)
			}
			entries := []recoveryZipEntry{{name: "settlement-chain.json", data: data}}
			if mutation == "unsafe-zip" {
				entries = append(entries, recoveryZipEntry{name: "../outside", data: []byte("bad")})
			}
			if mutation == "held" {
				entries = append(entries, recoveryZipEntry{name: "recovery-needed.json", data: []byte(`{"status":"recovery_needed"}`)})
			}
			payload := recoveryZip(t, entries...)
			name := RecoveryArtifactName(target, 10, 1) + "-handoff-00"
			run := recoveryHistoryRun(10, 1, "Current")
			metadata := Object{"id": int64(31), "name": name, "expired": false, "digest": "sha256:" + SHA256(payload), "workflow_run": Object{"id": int64(10), "head_sha": run["head_sha"]}}
			reader := &recoveryReaderFixture{reads: map[string]Object{"repos/example/widgets/actions/artifacts/31": metadata}, pages: map[string][]any{"repos/example/widgets/actions/workflows/task.yml/runs?per_page=100": {Object{"total_count": 1, "workflow_runs": []any{run}}}}, archives: map[int64][]byte{31: payload}}
			digest := metadata["digest"].(string)
			switch mutation {
			case "name":
				metadata["name"] = "similar-but-unreviewed"
			case "digest":
				digest = "sha256:" + SHA256([]byte("different upload"))
			case "source":
				metadata["workflow_run"].(Object)["id"] = int64(11)
			case "unsettled":
				reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"] = []any{Object{"total_count": 2, "workflow_runs": []any{recoveryHistoryRun(1, 1, "Prior"), run}}}
			}
			invocation := Invocation{Workflow: "task.yml", RunID: 10, Attempt: 1, RunName: "Current", RecoveryKey: "task", PackageRoot: filepath.Join(temp, "invocation"), RunnerTemp: temp}
			purpose := "transport"
			if mutation == "apply-without-context" {
				purpose = "apply"
			}
			result, err := engine.AcquireHandoffFor(context.Background(), reader, invocation, 31, digest, purpose)
			if mutation == "none" {
				if err != nil || result["outcome"] != "acquired" || result["artifact_name"] != name {
					t.Fatalf("qualified handoff failed: %v %v", result, err)
				}
				if _, err := ReadPackageFile(invocation.PackageRoot, "settlement-chain.json"); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unqualified handoff accepted")
			}
			if _, err := os.Stat(filepath.Join(temp, "outside")); !os.IsNotExist(err) {
				t.Fatal("handoff ZIP escaped its invocation package")
			}
		})
	}
}
