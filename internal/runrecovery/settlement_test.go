package runrecovery

import (
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func settlementTestTarget(t *testing.T, repositoryURL string) Object {
	t.Helper()
	target, err := TargetObject("task.yml", repositoryURL, 9, "workflow-history-v2")
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func settlementTestEngine(t *testing.T, reviewedSHAs []any) *Engine {
	t.Helper()
	policy := Object{
		"schema_version": 1,
		"workflows": Object{
			"task.yml": Object{
				"mutator_step_alternatives": []any{[]any{"Apply reviewed change"}},
				"reviewed_source_shas":      reviewedSHAs,
				"allow_publication":         false,
				"plans":                     Object{},
			},
		},
	}
	repository, err := contract.ParseRepository(Object{
		"owner": "example", "name": "widgets", "url": "https://github.com/example/widgets",
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(policy, repository)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestTargetBindsCanonicalGitHubOriginIntoArtifactNamespace(t *testing.T) {
	githubTarget := settlementTestTarget(t, "https://github.com/example/widgets")
	for _, workflow := range []string{"task.yml", "task.yaml"} {
		if _, err := TargetObject(workflow, "https://github.com/example/widgets", 9, "workflow-history-v2"); err != nil {
			t.Errorf("valid workflow filename %q was rejected: %v", workflow, err)
		}
	}
	enterpriseTarget, err := TargetObject("task.yml", "https://github.example/example/widgets", 9, "workflow-history-v2")
	if err != nil {
		t.Fatal(err)
	}
	if githubTarget["server_url"] != "https://github.com" || enterpriseTarget["server_url"] != "https://github.example" {
		t.Fatalf("target does not retain canonical origin-only hosts: %#v, %#v", githubTarget, enterpriseTarget)
	}
	if RecoveryArtifactName(githubTarget, 10, 1) == RecoveryArtifactName(enterpriseTarget, 10, 1) {
		t.Fatal("different GitHub hosts shared a recovery artifact identity")
	}
	engine := settlementTestEngine(t, []any{})
	if _, err := engine.validateTarget(enterpriseTarget); err == nil {
		t.Fatal("engine accepted a target on another GitHub host")
	}
	for _, url := range []string{
		"http://github.com/example/widgets",
		"https://github.com:443/example/widgets",
		"https://user@github.com/example/widgets",
		"https://github.com/example/widgets?ref=main",
	} {
		if _, err := TargetObject("task.yml", url, 9, "workflow-history-v2"); err == nil {
			t.Fatalf("noncanonical repository URL accepted: %q", url)
		}
	}
}

func TestNormalizeInventorySortsAndRejectsForkedOrMalformedHistory(t *testing.T) {
	target := settlementTestTarget(t, "https://github.com/example/widgets")
	first := recoveryHistoryRun(10, 2, "Earlier")
	second := recoveryHistoryRun(20, 1, "Later")
	rows, latest, err := NormalizeInventory([]any{second, first}, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !exactInt(rows[0]["id"], 10) || !exactInt(rows[1]["id"], 20) || latest[10] != 2 || latest[20] != 1 {
		t.Fatalf("history inventory was not sorted and complete: %#v %#v", rows, latest)
	}
	if _, _, err := NormalizeInventory([]any{first, first}, target); err == nil {
		t.Fatal("duplicate workflow run identity was accepted")
	}
	foreign := cloneNativeObject(second)
	foreign["workflow_id"] = int64(10)
	if _, _, err := NormalizeInventory([]any{foreign}, target); err == nil {
		t.Fatal("foreign workflow history was accepted")
	}
	fractional := cloneNativeObject(first)
	fractional["run_attempt"] = "2.0"
	if _, _, err := NormalizeInventory([]any{fractional}, target); err == nil {
		t.Fatal("non-integer attempt spelling was accepted")
	}
}

func TestPendingAttemptsIncludesEveryUncoveredAttemptInStableOrder(t *testing.T) {
	target := settlementTestTarget(t, "https://github.com/example/widgets")
	chain, err := EmptyChain(target)
	if err != nil {
		t.Fatal(err)
	}
	earlierRun, err := NormalizeRun(recoveryHistoryRun(10, 2, "Earlier"))
	if err != nil {
		t.Fatal(err)
	}
	currentRun, err := NormalizeRun(recoveryHistoryRun(20, 3, "Current"))
	if err != nil {
		t.Fatal(err)
	}
	earlier := earlierRun
	current := currentRun
	observed := []Object{current, earlier}
	latest := map[int64]int64{10: 2, 20: 3}
	pending, err := PendingAttempts(chain, observed, latest, 20, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []Object{
		{"run": earlier, "attempt": int64(1)}, {"run": earlier, "attempt": int64(2)},
		{"run": current, "attempt": int64(1)}, {"run": current, "attempt": int64(2)},
	}
	if !Equal(pending, want) {
		t.Fatalf("pending attempts are missing or out of order: %#v", pending)
	}
	if _, err := PendingAttempts(chain, observed, latest, 20, 2); err == nil {
		t.Fatal("current attempt differing from complete history was accepted")
	}
}

func TestSettlementRejectsUnsupportedLegacyNoDispatchProof(t *testing.T) {
	engine := settlementTestEngine(t, []any{})
	target := settlementTestTarget(t, "https://github.com/example/widgets")
	run, err := NormalizeRun(recoveryHistoryRun(42, 1, "Legacy run"))
	if err != nil {
		t.Fatal(err)
	}
	record := Object{
		"run_id": int64(42), "attempt": int64(1), "run": run,
		"attempt_target": Object{"recovery_key": nil, "identity": nil}, "artifact": nil, "context_file": nil,
		"settlement": Object{"kind": "no_dispatch", "phase": "completed", "mutator_steps": []any{}},
	}
	if _, err := engine.ValidateSettlementRecord(record, target); err == nil {
		t.Fatal("unsupported legacy no-dispatch proof settled history")
	}
}

func TestEmptyChainRejectsHostSubstitutionAndDigestTampering(t *testing.T) {
	engine := settlementTestEngine(t, []any{})
	githubTarget := settlementTestTarget(t, "https://github.com/example/widgets")
	enterpriseTarget, err := TargetObject("task.yml", "https://github.example/example/widgets", 9, "workflow-history-v2")
	if err != nil {
		t.Fatal(err)
	}
	chain, err := EmptyChain(githubTarget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ValidateChain(chain, enterpriseTarget, nil, map[int64]int64{}); err == nil {
		t.Fatal("checkpoint from another host was accepted")
	}
	changed, err := cloneObject(chain)
	if err != nil {
		t.Fatal(err)
	}
	changed["sha256"] = strings.Repeat("0", 64)
	if _, err := engine.ValidateChain(changed, githubTarget, nil, map[int64]int64{}); err == nil {
		t.Fatal("tampered checkpoint digest was accepted")
	}
}
