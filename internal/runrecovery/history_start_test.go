package runrecovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scopedEngine(t *testing.T, old Object) *Engine {
	t.Helper()
	start, err := NormalizeRun(old)
	if err != nil {
		t.Fatal(err)
	}
	policy := Object{"schema_version": 1, "workflows": Object{"task.yml": Object{
		"mutator_step_alternatives": []any{[]any{"Apply reviewed change"}},
		"reviewed_source_shas":      []any{}, "allow_publication": false, "plans": Object{}, "history_start": start,
	}}}
	engine, err := NewEngine(policy, testRecoveryEngine(t, false).repository)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func scopedReader(runs ...Object) *recoveryReaderFixture {
	return &recoveryReaderFixture{pages: map[string][]any{
		"repos/example/widgets/actions/workflows/task.yml/runs?per_page=100": {Object{"total_count": len(runs), "workflow_runs": objectRows(runs)}},
		"repos/example/widgets/actions/artifacts?per_page=100":               {Object{"total_count": 0, "artifacts": []any{}}},
	}}
}

func scopedInvocation(t *testing.T, id, attempt int64, title string) Invocation {
	t.Helper()
	temp := t.TempDir()
	return Invocation{Workflow: "task.yml", RunID: id, Attempt: attempt, RunName: title, RecoveryKey: "task", PackageRoot: filepath.Join(temp, "invocation"), RunnerTemp: temp}
}

func TestHistoryStartAllowsNewWorkWithoutSettlingExcludedRuns(t *testing.T) {
	old, current := recoveryHistoryRun(1, 3, "Old unknown"), recoveryHistoryRun(2, 1, "Current")
	engine := scopedEngine(t, old)
	reader := scopedReader(old, current)
	inv := scopedInvocation(t, 2, 1, "Current")
	result, err := engine.Recover(context.Background(), reader, inv)
	if err != nil || result["outcome"] != "fresh" {
		t.Fatalf("new native work blocked: %#v %v", result, err)
	}
	value, err := LoadJSON(filepath.Join(inv.PackageRoot, "settlement-chain.json"))
	if err != nil {
		t.Fatal(err)
	}
	chain := value.(Object)
	if !exactInt(chain["schema_version"], scopedSettlementSchemaVersion) || !Equal(chain["history_start"], engine.workflows["task.yml"].historyStart) || len(chain["inventory"].([]any)) != 0 || len(chain["settlements"].([]any)) != 0 {
		t.Fatal("excluded history was changed into a completion record", chain)
	}
	for _, call := range reader.calls {
		if strings.Contains(call, "/attempts/") || strings.HasPrefix(call, "ZIP ") {
			t.Fatal("migration evidence was unnecessarily acquired", call)
		}
	}
	defaultResult, err := testRecoveryEngine(t, false).Recover(context.Background(), reader, scopedInvocation(t, 2, 1, "Current"))
	if err != nil || defaultResult["outcome"] != "recovery_needed" {
		t.Fatal("no boundary silently dropped unknown history", defaultResult, err)
	}
}

func TestHistoryStartDoesNotBypassNewUnknownWork(t *testing.T) {
	old := recoveryHistoryRun(1, 1, "Old")
	result, err := scopedEngine(t, old).Recover(context.Background(), scopedReader(old, recoveryHistoryRun(2, 1, "Interrupted"), recoveryHistoryRun(3, 1, "Current")), scopedInvocation(t, 3, 1, "Current"))
	if err != nil || result["outcome"] != "recovery_needed" {
		t.Fatal("missing native evidence was treated as fresh", result, err)
	}
}

func TestHistoryStartHoldsMissingChangedActiveForeignAndReplayedHistory(t *testing.T) {
	for _, mode := range []string{"missing", "changed", "active", "foreign", "rerun", "partial", "chronology"} {
		t.Run(mode, func(t *testing.T) {
			old, current := recoveryHistoryRun(1, 1, "Old"), recoveryHistoryRun(2, 1, "Current")
			engine := scopedEngine(t, old)
			inv := scopedInvocation(t, 2, 1, "Current")
			switch mode {
			case "changed":
				old["head_sha"] = strings.Repeat("c", 40)
			case "active":
				old["status"] = "in_progress"
			case "foreign":
				old["workflow_id"] = int64(10)
			case "rerun":
				old["run_attempt"] = int64(2)
				inv.RunID, inv.Attempt, inv.RunName = 1, 2, "Old"
			case "chronology":
				current["created_at"] = "2020-01-01T00:00:00Z"
			}
			reader := scopedReader(old, current)
			if mode == "missing" {
				reader = scopedReader(current)
			}
			if mode == "partial" {
				reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)["total_count"] = 3
			}
			result, err := engine.Recover(context.Background(), reader, inv)
			if err != nil || result["outcome"] != "recovery_needed" {
				t.Fatal("invalid scope was admitted", result, err)
			}
			for _, path := range []string{"settlement-chain.json", "run-context.json", "plans", "journal"} {
				if _, err := os.Stat(filepath.Join(inv.PackageRoot, path)); !os.IsNotExist(err) {
					t.Fatal("held history created execution state", path, err)
				}
			}
		})
	}
}

func TestHistoryStartPolicyAndCheckpointMustMatchExactly(t *testing.T) {
	old := recoveryHistoryRun(1, 1, "Old")
	engine := scopedEngine(t, old)
	target, _ := TargetObject("task.yml", engine.repository.URL, 9, "workflow-history-v2")
	current, _ := NormalizeRun(recoveryHistoryRun(2, 1, "Current"))
	chain, err := engine.emptyChain(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ValidateChain(chain, target, []Object{current}, map[int64]int64{2: 1}); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int64{4, 5, 6, 7} {
		copy := cloneNativeObject(chain)
		copy["schema_version"] = version
		if _, err := engine.ValidateChain(copy, target, []Object{current}, map[int64]int64{2: 1}); err == nil {
			t.Fatal("prior recovery format admitted", version)
		}
	}
	copy := cloneNativeObject(chain)
	copy["history_start"].(Object)["head_sha"] = strings.Repeat("c", 40)
	if _, err := engine.ValidateChain(copy, target, []Object{current}, map[int64]int64{2: 1}); err == nil {
		t.Fatal("changed boundary admitted")
	}
	oldID, _ := NormalizeRun(old)
	if _, err := engine.ValidateChain(chain, target, []Object{oldID, current}, map[int64]int64{1: 1, 2: 1}); err == nil {
		t.Fatal("excluded attempt entered recovery inventory")
	}
	if err := engine.validateTargetForContext("task.yml", engine.repository.FullName(), oldID); err == nil {
		t.Fatal("excluded source was resumable")
	}
	for _, field := range []string{"history_cutover_reviews", "history_cutover_review_issue", "history_promotion_review_issue", "legacy_import_reviews"} {
		policy := Object{"schema_version": 1, "workflows": Object{"task.yml": Object{"mutator_step_alternatives": []any{[]any{"Apply"}}, "reviewed_source_shas": []any{}, "allow_publication": false, "plans": Object{}, field: []any{}}}}
		if _, err := NewEngine(policy, engine.repository); err == nil {
			t.Fatal("retired review contract silently accepted", field)
		}
	}
}

func TestHistoryStartCannotAdvancePastAnExistingNativeCheckpoint(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprint("scoped=", scoped), func(t *testing.T) {
			old := recoveryHistoryRun(1, 1, "Old")
			first := testRecoveryEngine(t, false)
			if scoped {
				first = scopedEngine(t, old)
			}
			target, _ := TargetObject("task.yml", first.repository.URL, 9, "workflow-history-v2")
			chain, _ := first.emptyChain(target)
			root := t.TempDir()
			if err := persistPackageJSON(root, "settlement-chain.json", chain); err != nil {
				t.Fatal(err)
			}
			payload := recoveryPackageZIP(t, root)
			metadata := Object{"id": int64(9), "name": CheckpointArtifactName(target, 2, 1), "expired": false, "size_in_bytes": len(payload), "digest": "sha256:" + SHA256(payload), "workflow_run": Object{"id": int64(2)}}
			reader := scopedReader(old, recoveryHistoryRun(2, 1, "Native"), recoveryHistoryRun(3, 1, "Current"))
			reader.archives = map[int64][]byte{9: payload}
			reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": 1, "artifacts": []any{metadata}}}
			later := scopedEngine(t, recoveryHistoryRun(2, 1, "Native"))
			result, err := later.Recover(context.Background(), reader, scopedInvocation(t, 3, 1, "Current"))
			if err != nil || result["outcome"] != "recovery_needed" || !strings.Contains(fmt.Sprint(result["reason"]), "cannot advance") {
				t.Fatal("advanced boundary hid existing native work", result, err)
			}
		})
	}
}

func TestHistoryStartEnforcesAcquisitionBudgetsBeforeDownloading(t *testing.T) {
	engine := scopedEngine(t, recoveryHistoryRun(3, 1, "Old"))
	target, _ := TargetObject("task.yml", engine.repository.URL, 9, "workflow-history-v2")
	for _, mode := range []string{"size", "total-bytes", "count"} {
		t.Run(mode, func(t *testing.T) {
			count, size := 1, int64(MaxArtifactBytes)+1
			if mode == "total-bytes" {
				count, size = 9, int64(MaxArtifactBytes)
			} else if mode == "count" {
				count, size = MaxCheckpointArtifacts+1, 0
			}
			artifacts := make([]Object, count)
			for i := range artifacts {
				artifacts[i] = Object{"id": int64(i + 1), "name": CheckpointArtifactName(target, 2, int64(i+1)), "expired": false, "size_in_bytes": size, "digest": "sha256:" + strings.Repeat("a", 64), "workflow_run": Object{"id": int64(2)}}
			}
			reader := scopedReader()
			if _, err := engine.scopeArtifacts(context.Background(), reader, target, artifacts); err == nil {
				t.Fatal("unbounded historical checkpoint acquisition accepted")
			}
			if len(reader.calls) != 0 {
				t.Fatal("download started before inventory budgets were checked", reader.calls)
			}
		})
	}
}
