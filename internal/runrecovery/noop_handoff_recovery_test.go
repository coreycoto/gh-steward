package runrecovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type interruptedNoopFixture struct {
	engine     *Engine
	reader     *recoveryReaderFixture
	original   NoopOptions
	source     Object
	invocation Invocation
	metadata   Object
}

func newInterruptedNoopFixture(t *testing.T) interruptedNoopFixture {
	t.Helper()
	e := noopEngine(t)
	r := readerWithCurrentNoop(10, 1)
	endpoint := "repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"
	source := r.pages[endpoint][0].(Object)["workflow_runs"].([]any)[0].(Object)
	source["head_sha"] = strings.Repeat("b", 40)
	original := startNoopFixture(t, e, r, t.TempDir(), 10, 1, "ineligible-trigger")
	source["status"], source["conclusion"] = "completed", "failure"
	r.reads["repos/example/widgets/actions/runs/10/attempts/1"] = source
	jobs := r.pages["repos/example/widgets/actions/runs/10/attempts/1/jobs?per_page=100"][0].(Object)["jobs"].([]any)
	jobs[0].(Object)["status"], jobs[0].(Object)["conclusion"] = "completed", "skipped"
	current := recoveryHistoryRun(11, 1, "Next")
	current["status"], current["conclusion"] = "in_progress", nil
	r.pages[endpoint] = []any{Object{"total_count": 2, "workflow_runs": []any{current, source}}}
	temp := t.TempDir()
	inv := Invocation{Workflow: "task.yml", RunID: 11, Attempt: 1, RunName: "Next", RecoveryKey: "next-event", PackageRoot: filepath.Join(temp, "package"), RunnerTemp: temp}
	return interruptedNoopFixture{engine: e, reader: r, original: original, source: source, invocation: inv}
}

func (f *interruptedNoopFixture) upload(t *testing.T, suffix string) []byte {
	t.Helper()
	metadata, payload := uploadFixturePackage(t, f.reader, f.engine, f.original.Invocation, 501)
	metadata["name"] = metadata["name"].(string) + suffix
	metadata["workflow_run"].(Object)["head_sha"] = f.source["head_sha"]
	f.reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": 1, "artifacts": []any{metadata}}}
	f.metadata = metadata
	return payload
}

func TestRecoveryReconcilesAnUndispatchedNativeNoopHandoff(t *testing.T) {
	for _, suffix := range []string{"-handoff-00", ""} {
		t.Run("artifact"+suffix, func(t *testing.T) {
			f := newInterruptedNoopFixture(t)
			originalBytes := f.upload(t, suffix)
			result, err := f.engine.Recover(context.Background(), f.reader, f.invocation)
			if err != nil || result["outcome"] != "fresh" {
				t.Fatalf("verified unstarted no-op blocked new work: %v %v", result, err)
			}
			chainValue, err := LoadJSON(filepath.Join(f.invocation.PackageRoot, "settlement-chain.json"))
			if err != nil {
				t.Fatal(err)
			}
			chain := chainValue.(Object)
			records := chain["settlements"].([]any)
			if len(records) != 1 || records[0].(Object)["settlement"].(Object)["kind"] != "undispatched_noop" {
				t.Fatalf("unstarted source was not retained distinctly: %v", records)
			}
			if string(f.reader.archives[501]) != string(originalBytes) {
				t.Fatal("original handoff was modified")
			}
			record := records[0].(Object)
			originalContext, err := ReadPackageFile(f.original.PackageRoot, "run-context.json")
			if err != nil {
				t.Fatal(err)
			}
			retainedContext, err := LoadRawFileProof(record["context_file"], "original context")
			if err != nil || !Equal(originalContext, retainedContext) {
				t.Fatal("source context was reclassified")
			}
			for _, name := range []string{"plans", "journal", "apply-results", "run-context.json"} {
				if _, err := os.Lstat(filepath.Join(f.invocation.PackageRoot, name)); !os.IsNotExist(err) {
					t.Fatalf("reconciliation invented execution evidence: %s", name)
				}
			}
			normalized, err := NormalizeRun(f.source)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.engine.ValidateChain(chain, chain["target"], []Object{normalized}, map[int64]int64{10: 1}); err != nil {
				t.Fatalf("checkpoint cannot retain distinct no-dispatch fact: %v", err)
			}
			// A future caller cannot rewrite fresh job evidence into success or claim
			// this fact was a completed native operation.
			settled := record["settlement"].(Object)
			settled["phase"] = "completed"
			if _, err := f.engine.ValidateSettlementRecord(record, chain["target"]); err == nil {
				t.Fatal("undispatched fact claimed completed business work")
			}
		})
	}
}

func mutateNoopJSON(t *testing.T, f interruptedNoopFixture, path string, change func(Object)) {
	t.Helper()
	value, err := LoadJSON(filepath.Join(f.original.PackageRoot, path))
	if err != nil {
		t.Fatal(err)
	}
	change(value.(Object))
	if err := persistPackageJSON(f.original.PackageRoot, path, value); err != nil {
		t.Fatal(err)
	}
}

func TestUndispatchedNoopRecoveryKeepsIncompleteOrDispatchedWorkHeld(t *testing.T) {
	cases := []string{"active-attempt", "started-job", "missing-job", "duplicate-job", "incomplete-jobs", "wrong-job-attempt", "missing-source", "changed-source", "control-head", "unknown-decision", "wrong-decision-event", "wrong-observation-run", "wrong-observation-target", "wrong-prefix", "open-frontier", "missing-decision", "unreferenced-candidate", "plan-file", "journal-file", "dispatch-inventory", "legacy-phase", "publication", "expired-artifact", "duplicate-handoff", "corrupt-archive", "wrong-artifact-head", "wrong-context-target", "unbound-event-target"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			f := newInterruptedNoopFixture(t)
			page := f.reader.pages["repos/example/widgets/actions/runs/10/attempts/1/jobs?per_page=100"][0].(Object)
			job := page["jobs"].([]any)[0].(Object)
			switch name {
			case "active-attempt":
				f.source["status"] = "in_progress"
			case "started-job":
				job["conclusion"] = "failure"
				job["steps"].([]any)[0].(Object)["conclusion"] = "success"
			case "missing-job":
				page["jobs"], page["total_count"] = []any{}, 0
			case "duplicate-job":
				copy := Object{}
				for k, v := range job {
					copy[k] = v
				}
				copy["id"] = 1002
				page["jobs"], page["total_count"] = []any{job, copy}, 2
			case "incomplete-jobs":
				page["total_count"] = 2
			case "wrong-job-attempt":
				job["run_attempt"] = 2
			case "missing-source":
				delete(f.reader.reads, "repos/example/widgets/contents/.github/workflows/task.yml?ref="+strings.Repeat("b", 40))
			case "changed-source":
				f.reader.reads["repos/example/widgets/contents/.github/workflows/task.yml?ref="+strings.Repeat("b", 40)]["content"] = "bm90IHRoZSByZXZpZXdlZCBzb3VyY2U="
			case "control-head":
				mutateNoopJSON(t, f, "run-context.json", func(v Object) { v["trusted_source_sha"] = strings.Repeat("c", 40) })
			case "unknown-decision":
				mutateNoopJSON(t, f, noopDecisionPath, func(v Object) { v["decision"] = "assumed-safe" })
			case "wrong-decision-event":
				mutateNoopJSON(t, f, noopDecisionPath, func(v Object) { v["event_sha256"] = strings.Repeat("0", 64) })
			case "wrong-observation-run":
				mutateNoopJSON(t, f, "recovery-observation.json", func(v Object) { v["run"].(Object)["head_branch"] = "other" })
			case "wrong-observation-target":
				mutateNoopJSON(t, f, "recovery-observation.json", func(v Object) { v["target"].(Object)["workflow_id"] = 99 })
			case "wrong-prefix":
				mutateNoopJSON(t, f, "recovery-observation.json", func(v Object) { v["chain_sha256"] = strings.Repeat("0", 64) })
			case "open-frontier":
				mutateNoopJSON(t, f, "recovery-observation.json", func(v Object) { v["prepared_frontier_sha256"] = strings.Repeat("0", 64) })
			case "missing-decision":
				if err := os.Remove(filepath.Join(f.original.PackageRoot, noopDecisionPath)); err != nil {
					t.Fatal(err)
				}
			case "unreferenced-candidate", "plan-file", "journal-file":
				path := map[string]string{"unreferenced-candidate": "proposals/candidate.json", "plan-file": "plans/unlisted.json", "journal-file": "journal/unlisted.json"}[name]
				if err := persistPackageJSON(f.original.PackageRoot, path, Object{"unknown_write": true}); err != nil {
					t.Fatal(err)
				}
			case "dispatch-inventory":
				mutateNoopJSON(t, f, "run-context.json", func(v Object) { v["dispatch_steps"] = []any{"Apply reviewed change"} })
			case "legacy-phase":
				mutateNoopJSON(t, f, "run-context.json", func(v Object) { v["phase"] = "noop" })
			case "publication":
				mutateNoopJSON(t, f, "run-context.json", func(v Object) { v["publication"] = nil })
			case "wrong-context-target":
				mutateNoopJSON(t, f, "run-context.json", func(v Object) { v["attempt_target"] = Object{"event_sha256": strings.Repeat("0", 64)} })
			case "unbound-event-target":
				for _, path := range []string{"run-context.json", noopDecisionPath} {
					mutateNoopJSON(t, f, path, func(v Object) { v["attempt_target"] = Object{"event_sha256": strings.Repeat("0", 64)} })
				}
			}
			f.upload(t, "-handoff-00")
			switch name {
			case "expired-artifact":
				f.metadata["expired"] = true
			case "duplicate-handoff":
				duplicate := Object{}
				for k, v := range f.metadata {
					duplicate[k] = v
				}
				duplicate["id"] = 502
				f.reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": 2, "artifacts": []any{f.metadata, duplicate}}}
			case "corrupt-archive":
				f.reader.archives[501] = []byte("corrupt")
			case "wrong-artifact-head":
				f.metadata["workflow_run"].(Object)["head_sha"] = strings.Repeat("c", 40)
			}
			result, err := f.engine.Recover(context.Background(), f.reader, f.invocation)
			if err != nil || result["outcome"] != "recovery_needed" {
				t.Fatalf("%s was allowed: %v %v", name, result, err)
			}
		})
	}
}

func TestUndispatchedNoopRequiresExplicitlyRetainedHistoricalSource(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprint(retained), func(t *testing.T) {
			f := newInterruptedNoopFixture(t)
			policy := f.engine.workflows["task.yml"].plans["workflow-noop"].approval.noop
			oldDigest := policy.sourceSHA256
			if retained {
				copy := *policy
				policy.previous = map[string]*noopPolicy{oldDigest: &copy}
			}
			policy.sourceSHA256 = strings.Repeat("c", 64)
			f.upload(t, "-handoff-00")
			result, err := f.engine.Recover(context.Background(), f.reader, f.invocation)
			expected := "recovery_needed"
			if retained {
				expected = "fresh"
			}
			if err != nil || result["outcome"] != expected {
				t.Fatalf("historical source retention: %v %v", result, err)
			}
		})
	}
}

func TestUndispatchedNoopRecoveryRetainsAnOrderedPredecessorPrefix(t *testing.T) {
	f := newInterruptedNoopFixture(t)
	f.upload(t, "-handoff-00")
	endpoint := "repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"
	second := f.reader.pages[endpoint][0].(Object)["workflow_runs"].([]any)[0].(Object)
	second["display_title"], second["head_sha"] = "Current", strings.Repeat("b", 40)
	secondRoot := t.TempDir()
	secondOptions := startNoopFixture(t, f.engine, f.reader, secondRoot, 11, 1, "ineligible-trigger")
	second["status"], second["conclusion"] = "completed", "failure"
	f.reader.reads["repos/example/widgets/actions/runs/11/attempts/1"] = second
	firstJobs := f.reader.pages["repos/example/widgets/actions/runs/10/attempts/1/jobs?per_page=100"][0].(Object)["jobs"].([]any)[0].(Object)
	secondJob := Object{}
	for k, v := range firstJobs {
		secondJob[k] = v
	}
	secondJob["id"], secondJob["run_id"] = 1101, 11
	f.reader.pages["repos/example/widgets/actions/runs/11/attempts/1/jobs?per_page=100"] = []any{Object{"total_count": 1, "jobs": []any{secondJob}}}
	secondMetadata, _ := uploadFixturePackage(t, f.reader, f.engine, secondOptions.Invocation, 502)
	secondMetadata["name"] = secondMetadata["name"].(string) + "-handoff-00"
	secondMetadata["workflow_run"].(Object)["head_sha"] = second["head_sha"]
	f.reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": 2, "artifacts": []any{secondMetadata, f.metadata}}}
	third := recoveryHistoryRun(12, 1, "Third")
	third["status"], third["conclusion"] = "in_progress", nil
	f.reader.pages[endpoint] = []any{Object{"total_count": 3, "workflow_runs": []any{third, second, f.source}}}
	temp := t.TempDir()
	inv := Invocation{Workflow: "task.yml", RunID: 12, Attempt: 1, RunName: "Third", RecoveryKey: "third-event", PackageRoot: filepath.Join(temp, "package"), RunnerTemp: temp}
	result, err := f.engine.Recover(context.Background(), f.reader, inv)
	if err != nil || result["outcome"] != "fresh" {
		t.Fatalf("ordered undispatched sources could not continue: %v %v", result, err)
	}
	value, err := LoadJSON(filepath.Join(inv.PackageRoot, "settlement-chain.json"))
	if err != nil {
		t.Fatal(err)
	}
	chain := value.(Object)
	rows := chain["settlements"].([]any)
	if len(rows) != 2 || !exactInt(rows[0].(Object)["run_id"], 10) || !exactInt(rows[1].(Object)["run_id"], 11) {
		t.Fatalf("source history was reordered or lost: %v", rows)
	}
	if err := validateNoopFrontier(rows[1].(Object), []any{}); err == nil {
		t.Fatal("second source bypassed its nonempty predecessor prefix")
	}
}
