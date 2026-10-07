package runrecovery

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type heldReaderFixture struct {
	*recoveryReaderFixture
	node          Object
	beforeWitness func(int)
	witnessReads  int
}

func TestHeldPromotionSurvivesNativeInterruptionAndHostedCheckpoint(t *testing.T) {
	f, baseline, reader, legacy, held := promotionHeldFixture(t, true)
	p := captureHeldPromotion(t, f, baseline, reader)
	admitPromotionFixture(t, f.engine, p)
	f.setAttempt(1, true)
	addHeldHistoryToCurrent(f, reader, legacy, held)
	inv1 := f.invocation(1, t.TempDir())
	if result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), reader, inv1, p); err != nil || result["outcome"] != "fresh" {
		t.Fatal(result, err)
	}
	f.prepareFirstAttempt(t, inv1.PackageRoot)
	if _, err := f.engine.MarkContextPlan(inv1.PackageRoot, "execution", "dispatching"); err != nil {
		t.Fatal(err)
	}
	f.qualifyCurrent(t, 1, inv1, "in_progress")
	f.finalizePending(t, 1, inv1, inv1.PackageRoot)
	f.engine = f.newProcessEngine(t)
	admitPromotionFixture(t, f.engine, p)
	f.setAttempt(2, true)
	addHeldHistoryToCurrent(f, reader, legacy, held)
	root2 := t.TempDir()
	inv2 := f.invocation(2, root2)
	if result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), reader, inv2, p); err != nil || result["outcome"] != "resumed" {
		t.Fatal(result, err)
	}
	if _, err := f.engine.ObserveSourceContext(inv2.PackageRoot, Object{"current_run_id": preparedLifecycleRunID, "current_attempt": int64(2), "current_run_name": preparedLifecycleRunName, "workflow_file": "task.yml", "repository": "example/widgets", "trusted_source_sha": f.workflowSHA}); err != nil {
		t.Fatal(err)
	}
	adapter := &preparedLifecycleAdapter{dispatches: map[string]int{}}
	journalRoot := filepath.Join(realTestPath(t, root2), "native-apply", ".artifacts", "gh-steward", "journals")
	if err := os.MkdirAll(journalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if installed, err := f.engine.InstallRestoredJournal(inv2.PackageRoot, journalRoot, "execution"); err != nil || installed["outcome"] != "no-journal-required" {
		t.Fatal(installed, err)
	}
	finishPreparedExecution(t, f, inv2, root2, adapter)
	if adapter.dispatches["execution:comment-1"] != 1 {
		t.Fatal("native interruption changed dispatch count", adapter.dispatches)
	}
	_, payload := finalizeCompletedAttempt(t, f, 2, inv2)
	closed, err := CheckpointFromArchive(payload)
	if err != nil || !Equal(closed["history_promotion"], p) || len(closed["settlements"].([]any)) != 2 || len(closed["inventory"].([]any)) != 1 {
		t.Fatal("hold contaminated native checkpoint", closed, err)
	}
	retained, err := historyCutoverFromChain(closed)
	if err != nil || !Equal(retained, baseline) {
		t.Fatal("native checkpoint changed quarantine", err)
	}
	f.engine = f.newProcessEngine(t)
	admitPromotionFixture(t, f.engine, p)
	f.setAttempt(3, true)
	addHeldHistoryToCurrent(f, reader, legacy, held)
	// After actual native receipts exist, their retained reviewed ledger is enough;
	// losing the diagnostic download does not erase the positive sealed proof.
	delete(reader.archives, 70)
	if result, err := f.engine.Recover(context.Background(), reader, f.invocation(3, t.TempDir())); err != nil || result["outcome"] != "terminal" {
		t.Fatal(result, err)
	}
}

func (r *heldReaderFixture) WorkflowRunFile(_ context.Context, workflow string, id int64, nodeID string) (Object, error) {
	r.witnessReads++
	if r.beforeWitness != nil {
		r.beforeWitness(r.witnessReads)
	}
	if r.err != nil {
		return nil, r.err
	}
	if workflow != "task.yml" || id != 7 || nodeID != "WFR_7" {
		return nil, errors.New("foreign witness request")
	}
	return r.node, nil
}

func promotionHeldFixture(t *testing.T, nested bool) (*preparedLifecycle, Object, *heldReaderFixture, Object, Object) {
	t.Helper()
	f, baseline, _, legacy := promotionFixture(t)
	value, _ := DecodeValue(f.policyBytes)
	policy := value.(Object)
	plans := policy["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)
	plans["workflow-noop"] = Object{"command": "workflow-noop", "domain_profile": "workflow-noop", "allowed_operation_kinds": []any{}, "attempt_target": "workflow_noop",
		"approval": Object{"kind": "local-noop", "workflow_source_sha256": SHA256([]byte(noopWorkflowSource)), "mutators": []any{Object{"job": "Mutation job", "steps": []any{"Apply reviewed change"}}}},
		"event":    Object{"kind": "workflow_noop", "path": "events/trigger-event.json"}, "parent_merge": nil}
	var err error
	f.policyBytes, err = Canonical(policy)
	if err != nil {
		t.Fatal(err)
	}
	f.engine = f.newProcessEngine(t)
	held := recoveryHistoryRun(7, 1, "Held before initialization")
	held["conclusion"], held["node_id"], held["path"] = "failure", "WFR_7", ".github/workflows/task.yml"
	held["html_url"] = "https://github.com/example/widgets/actions/runs/7"
	held["repository"] = Object{"full_name": "example/widgets", "html_url": "https://github.com/example/widgets", "pushed_at": "2026-01-01T00:00:00Z"}
	if nested {
		held["event"] = "workflow_run"
	}
	// The authentic workflow commit deliberately differs from head_sha.
	workflowSHA := strings.Repeat("b", 40)
	node := Object{"id": "WFR_7", "databaseId": int64(7), "runAttempt": int64(1), "event": held["event"], "url": held["html_url"],
		"workflow": Object{"id": "W_9", "databaseId": int64(9), "url": "https://github.com/example/widgets/actions/workflows/task.yml"},
		"file":     Object{"id": "WF_7", "path": ".github/workflows/task.yml", "repositoryName": "example/widgets", "repositoryFileUrl": "https://github.com/example/widgets/blob/" + workflowSHA + "/.github/workflows/task.yml", "url": "https://github.com/example/widgets/actions/runs/7/workflow", "run": Object{"id": "WFR_7", "databaseId": int64(7)}}}
	r := &heldReaderFixture{recoveryReaderFixture: f.reader, node: node}
	r.reads["repos/example/widgets/actions/runs/7"] = held
	r.reads["repos/example/widgets/contents/.github/workflows/task.yml?ref="+workflowSHA] = noopSourcePacket()
	r.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"] = []any{Object{"total_count": int64(2), "workflow_runs": []any{legacy, held}}}
	r.pages["repos/example/widgets/actions/runs/7/attempts/1/jobs?per_page=100"] = []any{Object{"total_count": int64(2), "jobs": []any{
		Object{"id": int64(71), "run_id": int64(7), "run_attempt": int64(1), "name": "Recovery guard", "status": "completed", "conclusion": "failure", "steps": []any{Object{"name": "Recover", "status": "completed", "conclusion": "failure"}}},
		Object{"id": int64(72), "run_id": int64(7), "run_attempt": int64(1), "name": "Mutation job", "status": "completed", "conclusion": "skipped", "steps": []any{}},
	}}}
	report := Object{"schema_version": int64(1), "status": "recovery_needed", "reason": "reviewed state changed", "repository": "example/widgets", "workflow_file": "task.yml", "recovery_key": "event-noop-test"}
	reportBytes, _ := Canonical(report)
	source := Object{"repository": "example/widgets", "workflow_file": "task.yml", "workflow_ref": "example/widgets/.github/workflows/task.yml@refs/heads/main", "workflow_sha": workflowSHA, "head_sha": held["head_sha"], "event_name": held["event"], "raw_event_sha256": strings.Repeat("c", 64)}
	manifest := Object{"schema_version": int64(1), "diagnostic_only": true, "report": Object{"path": "recovery-report.json", "sha256": SHA256(reportBytes)}}
	if nested {
		source["workflow_name"] = "Task"
		manifest["kind"], manifest["run"], manifest["source"] = "example.recovery-diagnostic", Object{"id": int64(7), "attempt": int64(1)}, source
	} else {
		for k, v := range source {
			manifest[k] = v
		}
		manifest["run_name"], manifest["run_id"], manifest["attempt"] = held["display_title"], int64(7), int64(1)
	}
	manifestBytes, _ := Canonical(manifest)
	entries := []recoveryZipEntry{{name: "manifest.json", data: manifestBytes}, {name: "recovery-report.json", data: reportBytes}}
	if nested {
		entries = append(entries, recoveryZipEntry{name: "SHA256SUMS", data: []byte(SHA256(reportBytes) + "  recovery-report.json\n")})
	}
	payload := recoveryZip(t, entries...)
	r.archives[70] = payload
	metadata := Object{"id": int64(70), "name": diagnosticArtifactName(baseline["target"].(Object), 7), "expired": false, "digest": "sha256:" + SHA256(payload), "workflow_run": Object{"id": int64(7), "head_sha": held["head_sha"]}, "size_in_bytes": int64(len(payload))}
	r.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(1), "artifacts": []any{metadata}}}
	return f, baseline, r, legacy, held
}

func captureHeldPromotion(t *testing.T, f *preparedLifecycle, b Object, r ActionsReader) Object {
	t.Helper()
	p, err := f.engine.PreviewHistoryPromotion(context.Background(), r, b, nil, []string{"execution"}, []string{"repos/example/widgets/issues/17"}, 7)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func addHeldHistoryToCurrent(f *preparedLifecycle, r *heldReaderFixture, legacy, held Object) {
	packet := f.reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)
	packet["total_count"] = int64(3)
	packet["workflow_runs"] = append([]any{legacy, held}, packet["workflow_runs"].([]any)...)
	artifact := Object{"id": int64(70), "name": diagnosticArtifactName(Object{"workflow_file": "task.yml"}, 7), "expired": false, "digest": "sha256:" + SHA256(r.archives[70]), "workflow_run": Object{"id": int64(7), "head_sha": held["head_sha"]}}
	p := f.reader.pages["repos/example/widgets/actions/artifacts?per_page=100"][0].(Object)
	p["artifacts"] = append([]any{artifact}, p["artifacts"].([]any)...)
	p["total_count"] = mustNonnegative(p["total_count"]) + 1
}

func TestDiagnosticHoldPromotionRetainsQuarantineAndRequiresExplicitSelectionAndReview(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			f, b, r, legacy, held := promotionHeldFixture(t, nested)
			if _, err := f.engine.PreviewHistoryPromotion(context.Background(), r, b, nil, []string{"execution"}, []string{"repos/example/widgets/issues/17"}); err == nil {
				t.Fatal("unselected diagnostic silently covered a missing native receipt")
			}
			p := captureHeldPromotion(t, f, b, r)
			if !exactInt(p["schema_version"], 3) || len(promotionHeldAttempts(p)) != 1 || !Equal(p["preview_checkpoint"].(Object)["history_cutover"], b) || len(p["preview_checkpoint"].(Object)["inventory"].([]any)) != 0 || len(p["preview_checkpoint"].(Object)["settlements"].([]any)) != 0 {
				t.Fatal("hold rewrote quarantine or invented native completion")
			}
			if _, err := f.engine.ValidateReviewedHistoryPromotion(p, p["target"]); err == nil {
				t.Fatal("capture granted authority")
			}
			original, _ := Canonical(p)
			admitPromotionFixture(t, f.engine, p)
			f.setAttempt(1, true)
			addHeldHistoryToCurrent(f, r, legacy, held)
			// Tag publication changes metadata on the run's embedded repository too.
			held["repository"].(Object)["pushed_at"] = "2026-01-02T00:00:00Z"
			// Private download transport metadata can expire independently of the
			// authenticated immutable workflow file and its content bytes.
			r.reads["repos/example/widgets/contents/.github/workflows/task.yml?ref="+strings.Repeat("b", 40)]["download_url"] = "https://private.example/source?token=temporary"
			artifacts, err := CompleteRepositoryArtifacts(context.Background(), r, f.repository)
			if err != nil {
				t.Fatal(err)
			}
			live, err := f.engine.captureHeldAttempt(context.Background(), r, p["target"].(Object), 7, artifacts)
			if err != nil {
				t.Fatal("live proof", err)
			}
			if !Equal(heldAttemptComparison(live), heldAttemptComparison(promotionHeldAttempts(p)[0])) {
				t.Fatal("live proof differs", heldAttemptComparison(live), heldAttemptComparison(promotionHeldAttempts(p)[0]))
			}
			inv := f.invocation(1, t.TempDir())
			result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), r, inv, p)
			if err != nil || result["outcome"] != "fresh" || result["mode"] != "fresh-native" {
				t.Fatal(result, err)
			}
			value, err := LoadJSON(filepath.Join(inv.PackageRoot, "settlement-chain.json"))
			chain, _ := object(value, "retained chain")
			if err != nil || !Equal(chain["history_promotion"], p) || len(chain["inventory"].([]any)) != 0 || len(chain["settlements"].([]any)) != 0 {
				t.Fatal("held proof became a native settlement", value, err)
			}
			after, _ := Canonical(p)
			if string(original) != string(after) {
				t.Fatal("comparison rewrote sealed provider evidence")
			}
			for _, name := range []string{"run-context.json", "plans", "journal"} {
				if _, err := os.Lstat(filepath.Join(inv.PackageRoot, name)); !os.IsNotExist(err) {
					t.Fatal("read-only admission invented execution state", name, err)
				}
			}
			f.engine.ResetHistoryReviews("task.yml", "PROMOTION")
			if _, err := f.engine.ValidateReviewedHistoryPromotion(p, p["target"]); err == nil {
				t.Fatal("revoked hold review retained authority")
			}
		})
	}
}

func TestDiagnosticHoldRequiresAuthenticatedCompletePositiveNoDispatchEvidence(t *testing.T) {
	for _, name := range []string{"no-witness", "missing-file", "foreign-file", "moving-file", "wrong-event", "wrong-attempt", "partial-jobs", "missing-mutator", "started-mutator", "cancelled-job", "foreign-job", "duplicate-artifact", "expired-artifact", "bad-digest", "missing-archive", "extra-execution-file", "competing-native", "unknown-source", "quarantined-id", "rerun-id", "duplicate-id"} {
		t.Run(name, func(t *testing.T) {
			f, b, r, _, held := promotionHeldFixture(t, false)
			var reader ActionsReader = r
			ids := []int64{7}
			jobs := r.pages["repos/example/widgets/actions/runs/7/attempts/1/jobs?per_page=100"][0].(Object)
			mutator := jobs["jobs"].([]any)[1].(Object)
			artifacts := r.pages["repos/example/widgets/actions/artifacts?per_page=100"][0].(Object)
			metadata := artifacts["artifacts"].([]any)[0].(Object)
			switch name {
			case "no-witness":
				reader = r.recoveryReaderFixture
			case "missing-file":
				r.node["file"] = nil
			case "foreign-file":
				r.node["file"].(Object)["repositoryName"] = "other/widgets"
			case "moving-file":
				r.node["file"].(Object)["repositoryFileUrl"] = "https://github.com/example/widgets/blob/main/.github/workflows/task.yml"
			case "wrong-event":
				r.node["event"] = "push"
			case "wrong-attempt":
				r.node["runAttempt"] = int64(2)
			case "partial-jobs":
				jobs["total_count"] = int64(3)
			case "missing-mutator":
				jobs["total_count"], jobs["jobs"] = int64(1), []any{jobs["jobs"].([]any)[0]}
			case "started-mutator":
				mutator["conclusion"], mutator["steps"] = "failure", []any{Object{"name": "Apply reviewed change", "status": "completed", "conclusion": "failure"}}
			case "cancelled-job":
				mutator["conclusion"] = "cancelled"
			case "foreign-job":
				mutator["run_attempt"] = int64(2)
			case "duplicate-artifact":
				other := cloneNativeObject(metadata)
				other["id"] = int64(73)
				artifacts["total_count"], artifacts["artifacts"] = int64(2), []any{metadata, other}
			case "expired-artifact":
				metadata["expired"] = true
			case "bad-digest":
				metadata["digest"] = "sha256:" + strings.Repeat("a", 64)
			case "missing-archive":
				delete(r.archives, 70)
			case "extra-execution-file":
				files, err := archiveFiles(r.archives[70], false)
				if err != nil {
					t.Fatal(err)
				}
				payload := recoveryZip(t, recoveryZipEntry{name: "manifest.json", data: files["manifest.json"]}, recoveryZipEntry{name: "recovery-report.json", data: files["recovery-report.json"]}, recoveryZipEntry{name: "run-context.json", data: []byte("{}")})
				r.archives[70] = payload
				metadata["digest"], metadata["size_in_bytes"] = "sha256:"+SHA256(payload), int64(len(payload))
			case "competing-native":
				other := Object{"id": int64(73), "name": RecoveryArtifactName(b["target"], 7, 1), "workflow_run": Object{"id": int64(7)}}
				artifacts["total_count"], artifacts["artifacts"] = int64(2), []any{metadata, other}
			case "unknown-source":
				value, _ := DecodeValue(f.policyBytes)
				value.(Object)["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)["workflow-noop"].(Object)["approval"].(Object)["workflow_source_sha256"] = strings.Repeat("f", 64)
				f.policyBytes, _ = Canonical(value)
				f.engine = f.newProcessEngine(t)
			case "quarantined-id":
				ids = []int64{5}
			case "rerun-id":
				held["run_attempt"] = int64(2)
			case "duplicate-id":
				ids = []int64{7, 7}
			}
			if _, err := f.engine.PreviewHistoryPromotion(context.Background(), reader, b, nil, []string{"execution"}, []string{"repos/example/widgets/issues/17"}, ids...); err == nil {
				t.Fatal("unqualified hold was admitted")
			}
		})
	}
}

func TestReviewedHoldCannotBecomeSettlementReplayOrCoverNewAttempts(t *testing.T) {
	f, b, r, legacy, held := promotionHeldFixture(t, false)
	p := captureHeldPromotion(t, f, b, r)
	admitPromotionFixture(t, f.engine, p)
	chain, err := historyPromotionChain(p)
	if err != nil {
		t.Fatal(err)
	}
	current := recoveryHistoryRun(100, 1, "Current")
	legacyRun, _ := NormalizeRun(legacy)
	heldRun, _ := NormalizeRun(held)
	currentRun, _ := NormalizeRun(current)
	observed := []Object{legacyRun, heldRun, currentRun}
	latest := map[int64]int64{5: 1, 7: 1, 100: 1}
	if pending, err := PendingAttempts(chain, observed, latest, 100, 1); err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	if _, err := PendingAttempts(chain, observed, latest, 7, 1); err == nil {
		t.Fatal("held attempt became fresh work")
	}
	latest[7] = 2
	pending, err := PendingAttempts(chain, observed, latest, 100, 1)
	if err != nil || len(pending) != 1 || !exactInt(pending[0]["attempt"], 2) {
		t.Fatal("hold swallowed a later attempt", pending, err)
	}
	if _, err := PendingAttempts(chain, observed, latest, 7, 2); err == nil {
		t.Fatal("held rerun became native continuation")
	}
	latest[7] = 1
	if _, err := f.engine.ValidateChain(chain, p["target"], observed, latest); err != nil {
		t.Fatal(err)
	}
	chain["inventory"] = []any{Object{"run": heldRun, "settled_attempt": int64(1)}}
	resealPromotion(t, chain)
	if _, err := f.engine.ValidateChain(chain, p["target"], observed, latest); err == nil {
		t.Fatal("held proof became native inventory")
	}
	chain, _ = historyPromotionChain(p)
	if _, err := f.engine.ValidateChain(chain, p["target"], []Object{legacyRun, currentRun}, map[int64]int64{5: 1, 100: 1}); err == nil {
		t.Fatal("lost held run was ignored")
	}
}

func TestHoldEvidenceIsRecheckedDuringCaptureAndFirstFreshAdmission(t *testing.T) {
	for _, stage := range []string{"capture", "admission"} {
		t.Run(stage, func(t *testing.T) {
			f, b, r, legacy, held := promotionHeldFixture(t, false)
			if stage == "capture" {
				r.beforeWitness = func(count int) {
					if count == 2 {
						r.node["file"].(Object)["repositoryFileUrl"] = "https://github.com/example/widgets/blob/main/.github/workflows/task.yml"
					}
				}
				if _, err := f.engine.PreviewHistoryPromotion(context.Background(), r, b, nil, []string{"execution"}, []string{"repos/example/widgets/issues/17"}, 7); err == nil {
					t.Fatal("source changed during capture")
				}
				return
			}
			p := captureHeldPromotion(t, f, b, r)
			admitPromotionFixture(t, f.engine, p)
			f.setAttempt(1, true)
			addHeldHistoryToCurrent(f, r, legacy, held)
			delete(r.archives, 70)
			inv := f.invocation(1, t.TempDir())
			result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), r, inv, p)
			if err != nil || result["outcome"] != "recovery_needed" {
				t.Fatal("missing upload was ignored", result, err)
			}
			if _, err := os.Lstat(filepath.Join(inv.PackageRoot, "settlement-chain.json")); !os.IsNotExist(err) {
				t.Fatal("failed first admission advanced lineage", err)
			}
		})
	}
}

func TestHoldOfflineShapeDoesNotReplaceTrustedMutatorPolicy(t *testing.T) {
	f, b, r, _, _ := promotionHeldFixture(t, false)
	p := captureHeldPromotion(t, f, b, r)
	proof := promotionHeldAttempts(p)[0]
	jobs := proof["jobs"].(Object)["pages"].([]any)[0].(Object)
	mutator := jobs["jobs"].([]any)[1].(Object)
	mutator["conclusion"], mutator["steps"] = "success", []any{Object{"name": "Apply reviewed change", "status": "completed", "conclusion": "success"}}
	resealPromotion(t, p)
	admitPromotionFixture(t, f.engine, p)
	if _, err := f.engine.ValidateReviewedHistoryPromotion(p, p["target"]); err == nil {
		t.Fatal("even an exact review cannot relax trusted skipped-mutator policy")
	}
	p = captureHeldPromotion(t, f, b, r)
	proof = promotionHeldAttempts(p)[0]
	proof["archive_base64"] = base64.StdEncoding.EncodeToString([]byte("not the uploaded ZIP"))
	resealPromotion(t, p)
	if _, err := ValidateHistoryPromotion(p); err == nil {
		t.Fatal("re-sealing altered diagnostic bytes inherited upload identity")
	}
}

func TestHeldDiagnosticChecksumsPreserveBothProducerLayouts(t *testing.T) {
	_, baseline, reader, _, held := promotionHeldFixture(t, true)
	files, err := archiveFiles(reader.archives[70], false)
	if err != nil {
		t.Fatal(err)
	}
	reportLine := SHA256(files["recovery-report.json"]) + "  recovery-report.json\n"
	manifestLine := SHA256(files["manifest.json"]) + "  manifest.json\n"
	for _, tc := range []struct {
		name, sums string
		valid      bool
	}{
		{"report-only", reportLine, true}, {"both-files", reportLine + manifestLine, true},
		{"manifest-only", manifestLine, false}, {"duplicate-report", reportLine + reportLine, false},
		{"altered-report", strings.Repeat("f", 64) + "  recovery-report.json\n", false},
		{"foreign-file", reportLine + strings.Repeat("f", 64) + "  plan.json\n", false}, {"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := recoveryZip(t, recoveryZipEntry{name: "manifest.json", data: files["manifest.json"]}, recoveryZipEntry{name: "recovery-report.json", data: files["recovery-report.json"]}, recoveryZipEntry{name: "SHA256SUMS", data: []byte(tc.sums)})
			run, _ := NormalizeRun(held)
			err := validateHeldDiagnostic(payload, baseline["target"].(Object), run, strings.Repeat("b", 40), "workflow_run")
			if (err == nil) != tc.valid {
				t.Fatal("diagnostic checksum contract", err)
			}
		})
	}
}

func TestFailedPreparationJobRequiresPositiveSkippedMutationStep(t *testing.T) {
	for _, kind := range []string{"skipped", "missing", "duplicate", "started"} {
		t.Run(kind, func(t *testing.T) {
			f, b, r, _, _ := promotionHeldFixture(t, true)
			job := r.pages["repos/example/widgets/actions/runs/7/attempts/1/jobs?per_page=100"][0].(Object)["jobs"].([]any)[1].(Object)
			job["conclusion"] = "failure"
			step := Object{"name": "Apply reviewed change", "status": "completed", "conclusion": "skipped"}
			job["steps"] = []any{step}
			switch kind {
			case "missing":
				job["steps"] = []any{}
			case "duplicate":
				job["steps"] = []any{step, step}
			case "started":
				step["conclusion"] = "failure"
			}
			_, err := f.engine.PreviewHistoryPromotion(context.Background(), r, b, nil, []string{"execution"}, []string{"repos/example/widgets/issues/17"}, 7)
			if (err == nil) != (kind == "skipped") {
				t.Fatal("failed prepare was not positively qualified", err)
			}
		})
	}
}
