package runrecovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type holdReaderFixture struct {
	*recoveryReaderFixture
	witness Object
}

func (r *holdReaderFixture) WorkflowRunFile(context.Context, string, int64, string) (Object, error) {
	return r.witness, nil
}

func stoppedHoldFixture(t *testing.T) (*Engine, *holdReaderFixture, Invocation, Object, Object) {
	t.Helper()
	f := newInterruptedNoopFixture(t)
	f.upload(t, "-handoff-00")
	hold := recoveryHistoryRun(11, 1, "Stopped during recovery")
	hold["head_sha"], hold["node_id"], hold["conclusion"] = strings.Repeat("b", 40), "WFR_11", "failure"
	current := recoveryHistoryRun(12, 1, "Next")
	current["status"], current["conclusion"] = "in_progress", nil
	f.reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"] = []any{Object{"total_count": 3, "workflow_runs": []any{current, hold, f.source}}}
	f.reader.reads["repos/example/widgets/actions/runs/11/attempts/1"] = hold
	f.reader.pages["repos/example/widgets/actions/runs/11/attempts/1/jobs?per_page=100"] = []any{Object{"total_count": 1, "jobs": []any{Object{"id": 1101, "run_id": 11, "run_attempt": 1, "name": "Mutation job", "status": "completed", "conclusion": "skipped", "steps": []any{}}}}}
	base := "https://github.com/example/widgets"
	witness := Object{"id": "WFR_11", "databaseId": 11, "event": "workflow_dispatch", "runAttempt": 1, "url": base + "/actions/runs/11", "workflow": Object{"id": "W_9", "databaseId": 9, "url": base + "/actions/workflows/task.yml"}, "file": Object{"id": "WF_11", "path": ".github/workflows/task.yml", "repositoryName": "example/widgets", "repositoryFileUrl": base + "/blob/" + strings.Repeat("b", 40) + "/.github/workflows/task.yml", "url": base + "/actions/runs/11/workflow", "run": Object{"id": "WFR_11", "databaseId": 11}}}
	report := Object{"schema_version": 1, "status": "recovery_needed", "workflow_file": "task.yml", "repository": "example/widgets", "recovery_key": "event-noop-" + strings.Repeat("c", 64), "reason": "earliest unsettled attempt has no unique exact recovery artifact"}
	manifest := Object{"schema_version": 1, "diagnostic_only": true, "repository": "example/widgets", "workflow_file": "task.yml", "run_name": hold["display_title"], "workflow_ref": "example/widgets/.github/workflows/task.yml@refs/heads/main", "workflow_sha": strings.Repeat("b", 40), "head_sha": hold["head_sha"], "event_name": hold["event"], "raw_event_sha256": strings.Repeat("c", 64), "run_id": 11, "attempt": 1, "report": Object{"path": "recovery-report.json", "sha256": ""}}
	temp := t.TempDir()
	inv := Invocation{Workflow: "task.yml", RunID: 12, Attempt: 1, RunName: "Next", RecoveryKey: "next", PackageRoot: filepath.Join(temp, "package"), RunnerTemp: temp}
	return f.engine, &holdReaderFixture{f.reader, witness}, inv, manifest, report
}

func uploadStoppedHold(t *testing.T, r *holdReaderFixture, manifest, report Object, extra ...recoveryZipEntry) (Object, []byte) {
	t.Helper()
	reportBytes, err := Canonical(report)
	if err != nil {
		t.Fatal(err)
	}
	manifest["report"].(Object)["sha256"] = SHA256(reportBytes)
	manifestBytes, err := Canonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	entries := append([]recoveryZipEntry{{name: "manifest.json", data: manifestBytes}, {name: "recovery-report.json", data: reportBytes}}, extra...)
	payload := recoveryZip(t, entries...)
	metadata := Object{"id": 502, "name": "gh-steward-recovery-diagnostic-task-run-11-attempt-1", "expired": false, "digest": "sha256:" + SHA256(payload), "workflow_run": Object{"id": 11, "head_sha": strings.Repeat("b", 40)}}
	r.archives[502], r.reads["repos/example/widgets/actions/artifacts/502"] = payload, metadata
	page := r.pages["repos/example/widgets/actions/artifacts?per_page=100"][0].(Object)
	page["artifacts"] = append(page["artifacts"].([]any), metadata)
	page["total_count"] = len(page["artifacts"].([]any))
	return metadata, payload
}

func TestRecoveryPassesAStoppedHoldAfterItsOriginalPredecessor(t *testing.T) {
	e, r, inv, manifest, report := stoppedHoldFixture(t)
	_, original := uploadStoppedHold(t, r, manifest, report)
	result, err := e.Recover(context.Background(), r, inv)
	if err != nil || result["outcome"] != "fresh" {
		t.Fatalf("a positively undispatched recovery hold blocked the next attempt: %v %v", result, err)
	}
	value, err := LoadJSON(filepath.Join(inv.PackageRoot, "settlement-chain.json"))
	if err != nil {
		t.Fatal(err)
	}
	chain := value.(Object)
	records := chain["settlements"].([]any)
	if len(records) != 2 || records[0].(Object)["settlement"].(Object)["kind"] != "undispatched_noop" || records[1].(Object)["settlement"].(Object)["kind"] != "undispatched_hold" {
		t.Fatal("lost the distinct ordered original and stopped-run facts")
	}
	record := records[1].(Object)
	if record["context_file"] != nil || record["settlement"].(Object)["phase"] != "undispatched" || string(r.archives[502]) != string(original) {
		t.Fatal("recovery rewrote or invented historical execution evidence")
	}
	for _, name := range []string{"plans", "journal", "apply-results", "run-context.json"} {
		if _, err := os.Lstat(filepath.Join(inv.PackageRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("invented %s", name)
		}
	}
	observed, attempts := []Object{}, map[int64]int64{}
	for _, id := range []int64{10, 11} {
		run, err := NormalizeRun(r.reads[fmt.Sprintf("repos/example/widgets/actions/runs/%d/attempts/1", id)])
		if err != nil {
			t.Fatal(err)
		}
		observed, attempts[id] = append(observed, run), 1
	}
	if _, err := e.ValidateChain(chain, chain["target"], observed, attempts); err != nil {
		t.Fatalf("future checkpoint validation lost original stopped-run evidence: %v", err)
	}
	proofs := record["settlement"].(Object)["policy_files"].(Object)
	retained, err := LoadRawFileProof(proofs["diagnostic/manifest.json"], "original diagnostic manifest")
	originalFiles, archiveErr := archiveFiles(original, false)
	if err != nil || archiveErr != nil || string(retained) != string(originalFiles["manifest.json"]) {
		t.Fatal("original manifest bytes changed")
	}
	proofs["diagnostic/recovery-report.json"] = MakeFileProof([]byte(`{"status":"completed"}`))
	if _, err := e.ValidateSettlementRecord(record, chain["target"]); err == nil {
		t.Fatal("retained diagnostic report could be rewritten")
	}
}

func TestStoppedHoldRecoveryRejectsAmbiguousOrDispatchedEvidence(t *testing.T) {
	for _, name := range []string{"started-mutator", "missing-mutator", "incomplete-jobs", "active-attempt", "foreign-manifest", "wrong-attempt", "wrong-report-target", "unknown-status", "wrong-event", "wrong-workflow-ref", "wrong-control-sha", "missing-witness", "moving-source", "foreign-workflow-id", "changed-source", "extra-journal", "duplicate-artifact", "expired-artifact", "corrupt-archive", "missing-predecessor"} {
		t.Run(name, func(t *testing.T) {
			e, r, inv, manifest, report := stoppedHoldFixture(t)
			page := r.pages["repos/example/widgets/actions/runs/11/attempts/1/jobs?per_page=100"][0].(Object)
			job := page["jobs"].([]any)[0].(Object)
			extra := []recoveryZipEntry{}
			switch name {
			case "started-mutator":
				job["conclusion"], job["steps"] = "failure", []any{Object{"name": "Apply reviewed change", "status": "completed", "conclusion": "success"}}
			case "missing-mutator":
				page["jobs"], page["total_count"] = []any{}, 0
			case "incomplete-jobs":
				page["total_count"] = 2
			case "active-attempt":
				r.reads["repos/example/widgets/actions/runs/11/attempts/1"]["status"] = "in_progress"
			case "foreign-manifest":
				manifest["repository"] = "other/widgets"
			case "wrong-attempt":
				manifest["attempt"] = 2
			case "wrong-report-target":
				report["workflow_file"] = "another.yml"
			case "unknown-status":
				report["status"] = "completed"
			case "wrong-event":
				manifest["event_name"] = "push"
			case "wrong-workflow-ref":
				manifest["workflow_ref"] = "other/widgets/.github/workflows/task.yml@refs/heads/main"
			case "wrong-control-sha":
				manifest["workflow_sha"] = strings.Repeat("d", 40)
			case "missing-witness":
				r.witness = nil
			case "moving-source":
				r.witness["file"].(Object)["repositoryFileUrl"] = "https://github.com/example/widgets/blob/main/.github/workflows/task.yml"
			case "foreign-workflow-id":
				r.witness["workflow"].(Object)["databaseId"] = 19
			case "changed-source":
				r.reads["repos/example/widgets/contents/.github/workflows/task.yml?ref="+strings.Repeat("b", 40)]["content"] = "bm90IHRoZSByZXZpZXdlZCBzb3VyY2U="
			case "extra-journal":
				extra = append(extra, recoveryZipEntry{name: "journal/unknown.json", data: []byte(`{"unknown_write":true}`)})
			case "missing-predecessor":
				delete(r.archives, 501)
			}
			metadata, _ := uploadStoppedHold(t, r, manifest, report, extra...)
			switch name {
			case "duplicate-artifact":
				p := r.pages["repos/example/widgets/actions/artifacts?per_page=100"][0].(Object)
				copy := Object{}
				for k, v := range metadata {
					copy[k] = v
				}
				copy["id"] = 503
				p["artifacts"] = append(p["artifacts"].([]any), copy)
				p["total_count"] = 3
			case "expired-artifact":
				metadata["expired"] = true
			case "corrupt-archive":
				r.archives[502] = []byte("changed")
			}
			result, err := e.Recover(context.Background(), r, inv)
			if err != nil || result["outcome"] != "recovery_needed" {
				t.Fatalf("ambiguous %s escaped hold: %v %v", name, result, err)
			}
		})
	}
}
