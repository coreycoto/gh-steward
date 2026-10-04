package runrecovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

type recoveryReaderFixture struct {
	reads    map[string]Object
	pages    map[string][]any
	archives map[int64][]byte
	calls    []string
	err      error
}

func (r *recoveryReaderFixture) Read(_ context.Context, endpoint string) (Object, error) {
	r.calls = append(r.calls, "GET "+endpoint)
	if r.err != nil {
		return nil, r.err
	}
	value, ok := r.reads[endpoint]
	if !ok {
		return nil, errors.New("fixture has no exact read endpoint")
	}
	return value, nil
}
func (r *recoveryReaderFixture) Pages(_ context.Context, endpoint string) ([]any, error) {
	r.calls = append(r.calls, "PAGES "+endpoint)
	if r.err != nil {
		return nil, r.err
	}
	value, ok := r.pages[endpoint]
	if !ok {
		return nil, errors.New("fixture has no exact pagination endpoint")
	}
	return value, nil
}
func (r *recoveryReaderFixture) Archive(_ context.Context, id int64) ([]byte, error) {
	r.calls = append(r.calls, fmt.Sprintf("ZIP %d", id))
	if r.err != nil {
		return nil, r.err
	}
	value, ok := r.archives[id]
	if !ok {
		return nil, errors.New("fixture has no exact archive ID")
	}
	return value, nil
}

func recoveryHistoryRun(id, attempt int64, title string) Object {
	return Object{"id": id, "run_attempt": attempt, "created_at": "2026-01-01T00:00:00Z", "display_title": title, "event": "workflow_dispatch", "workflow_id": int64(9), "head_branch": "main", "head_sha": strings.Repeat("a", 40), "status": "completed", "conclusion": "success"}
}

func testRecoveryEngine(t *testing.T, publication bool) *Engine {
	t.Helper()
	policy := Object{"schema_version": 1, "workflows": Object{"task.yml": Object{"mutator_step_alternatives": []any{[]any{"Apply reviewed change"}}, "reviewed_source_shas": []any{}, "allow_publication": publication, "plans": Object{}}}}
	repository := contract.Repository{Host: "github.com", Owner: "example", Name: "widgets", URL: "https://github.com/example/widgets"}
	engine, err := NewEngine(policy, repository)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestCompleteActionsInventoryRejectsChangedTotalsDuplicatesAndGaps(t *testing.T) {
	for _, pages := range [][]any{
		{},
		{Object{"total_count": 2, "workflow_runs": []any{Object{"id": 1}}}},
		{Object{"total_count": 2, "workflow_runs": []any{Object{"id": 1}}}, Object{"total_count": 3, "workflow_runs": []any{Object{"id": 2}}}},
		{Object{"total_count": 2, "workflow_runs": []any{Object{"id": 1}, Object{"id": 1}}}},
		{Object{"total_count": 1, "workflow_runs": []any{Object{"id": 0}}}},
	} {
		if _, err := completePages(pages, "workflow_runs"); err == nil {
			t.Fatal("incomplete or ambiguous Actions inventory accepted")
		}
	}
	rows, err := completePages([]any{Object{"total_count": 2, "workflow_runs": []any{Object{"id": 1}}}, Object{"total_count": 2, "workflow_runs": []any{Object{"id": 2}}}}, "workflow_runs")
	if err != nil || len(rows) != 2 {
		t.Fatalf("complete paginated history lost: %v", err)
	}
}

func TestRecoveryFreshRequiresCompletePriorHistoryAndOnlyReadsSelectedRepo(t *testing.T) {
	engine := testRecoveryEngine(t, false)
	temp := t.TempDir()
	reader := &recoveryReaderFixture{pages: map[string][]any{
		"repos/example/widgets/actions/workflows/task.yml/runs?per_page=100": {Object{"total_count": 1, "workflow_runs": []any{recoveryHistoryRun(10, 1, "Current")}}},
		"repos/example/widgets/actions/artifacts?per_page=100":               {Object{"total_count": 0, "artifacts": []any{}}},
	}}
	result, err := engine.Recover(context.Background(), reader, Invocation{Workflow: "task.yml", RunID: 10, Attempt: 1, RunName: "Current", RecoveryKey: "task", PackageRoot: filepath.Join(temp, "invocation"), RunnerTemp: temp})
	if err != nil || result["outcome"] != "fresh" {
		t.Fatalf("complete fresh invocation: %v %v", result, err)
	}
	if _, err := LoadJSON(filepath.Join(temp, "invocation", "settlement-chain.json")); err != nil {
		t.Fatal("fresh recovery lost its empty durable frontier", err)
	}
	for _, call := range reader.calls {
		if !strings.HasPrefix(call, "PAGES repos/example/widgets/") {
			t.Fatal("recovery crossed a selected repository read boundary", call)
		}
	}
}

func TestRecoveryNeverTreatsMissingOrSkippedLegacyEvidenceAsFresh(t *testing.T) {
	engine := testRecoveryEngine(t, false)
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-head-and-skipped-step", true: "read-failure"}[missing], func(t *testing.T) {
			temp := t.TempDir()
			old, current := recoveryHistoryRun(1, 1, "Old title"), recoveryHistoryRun(2, 1, "Current")
			reader := &recoveryReaderFixture{
				pages: map[string][]any{
					"repos/example/widgets/actions/workflows/task.yml/runs?per_page=100": {Object{"total_count": 2, "workflow_runs": []any{old, current}}},
					"repos/example/widgets/actions/artifacts?per_page=100":               {Object{"total_count": 0, "artifacts": []any{}}},
					"repos/example/widgets/actions/runs/1/attempts/1/jobs?per_page=100":  {Object{"total_count": 1, "jobs": []any{Object{"id": 77, "run_id": 1, "run_attempt": 1, "status": "completed", "steps": []any{Object{"number": 1, "name": "Apply reviewed change", "status": "completed", "conclusion": "skipped"}}}}}},
				}, reads: map[string]Object{"repos/example/widgets/actions/runs/1/attempts/1": old},
			}
			if missing {
				reader.err = errors.New("provider timeout")
			}
			result, err := engine.Recover(context.Background(), reader, Invocation{Workflow: "task.yml", RunID: 2, Attempt: 1, RunName: "Current", RecoveryKey: "task", PackageRoot: filepath.Join(temp, "invocation"), RunnerTemp: temp})
			if err != nil || result["outcome"] != "recovery_needed" {
				t.Fatalf("unqualified legacy attempt was bypassed: %v %v", result, err)
			}
			if _, err := os.Stat(filepath.Join(temp, "invocation", "recovery-needed.json")); err != nil {
				t.Fatal("held recovery has no durable reason")
			}
			for _, call := range reader.calls {
				if strings.Contains(call, "/jobs?") {
					t.Fatal("legacy attempt fetched job pages that cannot qualify recovery", call)
				}
			}
		})
	}
}

func TestCheckpointAcquisitionBudgetsApplyBeforeArchiveDownloads(t *testing.T) {
	engine := testRecoveryEngine(t, false)
	target, err := TargetObject("task.yml", engine.repository.URL, 9, "workflow-history-v2")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"count", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			observed := []Object{}
			attempts := map[int64]int64{}
			artifacts := []Object{}
			n := MaxCheckpointArtifacts + 1
			if kind == "bytes" {
				n = 10
			}
			for i := 1; i <= n+1; i++ {
				id := int64(i)
				run, err := NormalizeRun(recoveryHistoryRun(id, 1, "Current"))
				if err != nil {
					t.Fatal(err)
				}
				observed = append(observed, run)
				attempts[id] = 1
				if i <= n {
					metadata := Object{"id": id, "name": CheckpointArtifactName(target, id, 1), "expired": false}
					if kind == "bytes" {
						metadata["size_in_bytes"] = MaxArtifactBytes
					}
					artifacts = append(artifacts, metadata)
				}
			}
			reader := &recoveryReaderFixture{}
			if _, err := checkpointCandidates(context.Background(), reader, engine, target, observed, attempts, artifacts, int64(n+1), 1); err == nil || !strings.Contains(err.Error(), "aggregate") {
				t.Fatalf("unbounded historical acquisition: %v", err)
			}
			if len(reader.calls) != 0 {
				t.Fatal("budget rejection began downloading cumulative historical archives")
			}
		})
	}
}
