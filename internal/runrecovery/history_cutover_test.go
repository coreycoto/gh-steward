package runrecovery

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var historyCutoverTestRepository = contract.Repository{
	Host: "github.com", Owner: "example", Name: "widgets", URL: "https://github.com/example/widgets",
}

func historyCutoverAttemptEndpoint(runID, attempt int64) string {
	return fmt.Sprintf("repos/example/widgets/actions/runs/%d/attempts/%d", runID, attempt)
}

func historyCutoverFixture(runs, artifacts []Object, attempts, extraReads map[string]Object) *recoveryReaderFixture {
	workflow := Object{"id": int64(9), "path": ".github/workflows/task.yml", "name": "Task", "state": "active"}
	reads := map[string]Object{"repos/example/widgets/actions/workflows/task.yml": workflow}
	for endpoint, value := range attempts {
		reads[endpoint] = value
	}
	for endpoint, value := range extraReads {
		reads[endpoint] = value
	}
	return &recoveryReaderFixture{
		reads: reads,
		pages: map[string][]any{
			"repos/example/widgets/actions/workflows/task.yml/runs?per_page=100": {
				Object{"total_count": int64(len(runs)), "workflow_runs": objectRows(runs)},
			},
			"repos/example/widgets/actions/artifacts?per_page=100": {
				Object{"total_count": int64(len(artifacts)), "artifacts": objectRows(artifacts)},
			},
		},
	}
}

func historyCutoverAttemptResponse(run Object, attempt int64, createdAt string) Object {
	response, _ := cloneObject(run)
	response["run_attempt"] = attempt
	response["created_at"] = createdAt
	response["status"] = "completed"
	response["conclusion"] = "success"
	response["attempt_response_marker"] = fmt.Sprintf("raw-response-%d", attempt)
	return response
}

func captureHistoryCutoverFixture(t *testing.T, reader *recoveryReaderFixture, stateReads ...string) Object {
	t.Helper()
	baseline, err := CaptureHistoryCutover(context.Background(), reader, historyCutoverTestRepository, "task.yml", stateReads)
	if err != nil {
		t.Fatalf("capture reviewed-history candidate: %v", err)
	}
	return baseline
}

func resealHistoryCutoverFixture(t *testing.T, baseline Object) Object {
	t.Helper()
	unsigned := Object{}
	for _, field := range historyCutoverFields[:len(historyCutoverFields)-1] {
		unsigned[field] = baseline[field]
	}
	canonical, err := Canonical(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	baseline["sha256"] = SHA256(canonical)
	return baseline
}

func TestCaptureHistoryCutoverAcceptsCompleteEmptyAndFullHistory(t *testing.T) {
	t.Run("empty history", func(t *testing.T) {
		reader := historyCutoverFixture(nil, nil, nil, nil)
		baseline := captureHistoryCutoverFixture(t, reader)
		if err := assertValidHistoryCutover(t, baseline); err != nil {
			t.Fatal(err)
		}
		if rows := baseline["run_inventory"].([]any); len(rows) != 0 {
			t.Fatalf("empty history gained rows: %#v", rows)
		}
		if rows := baseline["attempts"].([]any); len(rows) != 0 {
			t.Fatalf("empty history gained attempts: %#v", rows)
		}
	})

	t.Run("full history preserves exact attempt responses", func(t *testing.T) {
		run := recoveryHistoryRun(101, 2, "Rerun history")
		run["created_at"] = "2026-03-17T10:00:00Z"
		attemptOne := historyCutoverAttemptResponse(run, 1, "2026-03-17T10:00:00Z")
		attemptTwo := historyCutoverAttemptResponse(run, 2, "2026-03-18T09:30:00Z")
		reads := map[string]Object{
			historyCutoverAttemptEndpoint(101, 1): attemptOne,
			historyCutoverAttemptEndpoint(101, 2): attemptTwo,
		}
		reader := historyCutoverFixture([]Object{run}, nil, reads, nil)
		baseline := captureHistoryCutoverFixture(t, reader)
		if err := assertValidHistoryCutover(t, baseline); err != nil {
			t.Fatal(err)
		}

		attempts, err := objectArray(baseline["attempts"], "test attempts")
		if err != nil || len(attempts) != 2 {
			t.Fatalf("captured exact attempts: %v (%d rows)", err, len(attempts))
		}
		for index, want := range []Object{attemptOne, attemptTwo} {
			response, err := object(attempts[index]["response"], "captured raw attempt")
			if err != nil || !Equal(response, want) {
				t.Fatalf("attempt %d raw response was normalized or changed: %#v, %v", index+1, response, err)
			}
		}
		if attempts[0]["response"].(Object)["created_at"] != "2026-03-17T10:00:00Z" || attempts[1]["response"].(Object)["created_at"] != "2026-03-18T09:30:00Z" {
			t.Fatal("GitHub rerun attempt creation timestamps were replaced by the original run-list timestamp")
		}
		for _, item := range attempts {
			if item["outcome"] != "unknown" || item["handling"] != "quarantined-never-replay" {
				t.Fatalf("captured attempt is not explicitly quarantined: %#v", item)
			}
		}
	})
}

func assertValidHistoryCutover(t *testing.T, value any) error {
	t.Helper()
	_, err := ValidateHistoryCutover(value)
	return err
}

func TestCaptureHistoryCutoverRejectsNonterminalForeignOrIncompleteEvidence(t *testing.T) {
	run := recoveryHistoryRun(101, 1, "Old run")
	response := historyCutoverAttemptResponse(run, 1, run["created_at"].(string))
	tests := []struct {
		name   string
		reader *recoveryReaderFixture
	}{
		{name: "active run", reader: func() *recoveryReaderFixture {
			active := cloneHistoryCutoverObject(t, run)
			active["status"] = "in_progress"
			return historyCutoverFixture([]Object{active}, nil, map[string]Object{historyCutoverAttemptEndpoint(101, 1): response}, nil)
		}()},
		{name: "incomplete exact attempt", reader: func() *recoveryReaderFixture {
			inProgress := cloneHistoryCutoverObject(t, response)
			inProgress["status"] = "in_progress"
			return historyCutoverFixture([]Object{run}, nil, map[string]Object{historyCutoverAttemptEndpoint(101, 1): inProgress}, nil)
		}()},
		{name: "foreign workflow", reader: func() *recoveryReaderFixture {
			foreign := cloneHistoryCutoverObject(t, run)
			foreign["workflow_id"] = int64(10)
			foreignResponse := cloneHistoryCutoverObject(t, response)
			foreignResponse["workflow_id"] = int64(10)
			return historyCutoverFixture([]Object{foreign}, nil, map[string]Object{historyCutoverAttemptEndpoint(101, 1): foreignResponse}, nil)
		}()},
		{name: "mutated immutable attempt field", reader: func() *recoveryReaderFixture {
			changed := cloneHistoryCutoverObject(t, response)
			changed["head_sha"] = strings.Repeat("b", 40)
			return historyCutoverFixture([]Object{run}, nil, map[string]Object{historyCutoverAttemptEndpoint(101, 1): changed}, nil)
		}()},
		{name: "missing exact attempt response", reader: historyCutoverFixture([]Object{run}, nil, nil, nil)},
		{name: "attempt timestamps move backwards", reader: func() *recoveryReaderFixture {
			rerun := recoveryHistoryRun(101, 2, "Old run")
			rerun["created_at"] = "2026-03-17T10:00:00Z"
			first := historyCutoverAttemptResponse(rerun, 1, "2026-03-18T10:00:00Z")
			second := historyCutoverAttemptResponse(rerun, 2, "2026-03-18T09:00:00Z")
			return historyCutoverFixture([]Object{rerun}, nil, map[string]Object{
				historyCutoverAttemptEndpoint(101, 1): first,
				historyCutoverAttemptEndpoint(101, 2): second,
			}, nil)
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := CaptureHistoryCutover(context.Background(), test.reader, historyCutoverTestRepository, "task.yml", nil); err == nil {
				t.Fatal("unverified workflow history was accepted as a reviewed cutover")
			}
		})
	}
}

func cloneHistoryCutoverObject(t *testing.T, value Object) Object {
	t.Helper()
	copyValue, err := cloneObject(value)
	if err != nil {
		t.Fatal(err)
	}
	return copyValue
}

func TestCaptureHistoryCutoverRejectsExistingNativeArtifactsAndOversizeBaseline(t *testing.T) {
	t.Run("existing native recovery artifact", func(t *testing.T) {
		target, err := TargetObject("task.yml", historyCutoverTestRepository.URL, 9, "workflow-history-v2")
		if err != nil {
			t.Fatal(err)
		}
		artifact := Object{"id": int64(7), "name": recoveryArtifactPrefix(target) + "-run-101-attempt-1"}
		reader := historyCutoverFixture(nil, []Object{artifact}, nil, nil)
		if _, err := CaptureHistoryCutover(context.Background(), reader, historyCutoverTestRepository, "task.yml", nil); err == nil {
			t.Fatal("cutover erased existing native recovery lineage")
		}
	})

	t.Run("oversize baseline", func(t *testing.T) {
		endpoint := "repos/example/widgets/issues/1"
		reader := historyCutoverFixture(nil, nil, nil, map[string]Object{endpoint: {"body": strings.Repeat("x", MaxCheckpointBytes+1)}})
		if _, err := CaptureHistoryCutover(context.Background(), reader, historyCutoverTestRepository, "task.yml", []string{endpoint}); err == nil {
			t.Fatal("cutover silently truncated or accepted an oversize review artifact")
		}
	})
}

func TestValidateHistoryCutoverRejectsTamperingAndNativeLineage(t *testing.T) {
	reader := historyCutoverFixture([]Object{recoveryHistoryRun(101, 1, "Old run")}, nil, map[string]Object{
		historyCutoverAttemptEndpoint(101, 1): historyCutoverAttemptResponse(recoveryHistoryRun(101, 1, "Old run"), 1, "2026-01-01T00:00:00Z"),
	}, nil)
	baseline := captureHistoryCutoverFixture(t, reader)

	t.Run("digest mismatch", func(t *testing.T) {
		changed := cloneHistoryCutoverObject(t, baseline)
		changed["scope"] = "production"
		if err := assertValidHistoryCutover(t, changed); err == nil {
			t.Fatal("tampered digest accepted")
		}
	})

	t.Run("quarantined row cannot become settled", func(t *testing.T) {
		changed := cloneHistoryCutoverObject(t, baseline)
		rows := changed["attempts"].([]any)
		rows[0].(Object)["outcome"] = "success"
		if _, err := ValidateHistoryCutover(resealHistoryCutoverFixture(t, changed)); err == nil {
			t.Fatal("resealed unknown attempt was accepted as settled")
		}
	})

	t.Run("native lineage cannot be embedded", func(t *testing.T) {
		changed := cloneHistoryCutoverObject(t, baseline)
		artifacts := changed["artifact_inventory"].([]any)
		artifacts = append(artifacts, Object{"id": int64(1), "name": recoveryArtifactPrefix(baseline["target"].(Object)) + "-run-101-attempt-1"})
		changed["artifact_inventory"] = artifacts
		if _, err := ValidateHistoryCutover(resealHistoryCutoverFixture(t, changed)); err == nil {
			t.Fatal("baseline with old native recovery lineage was accepted")
		}
	})
}

func historyCutoverPolicy(digest string, reviewIssue Object) Object {
	workflow := Object{
		"mutator_step_alternatives": []any{[]any{"Apply reviewed change"}},
		"reviewed_source_shas":      []any{},
		"allow_publication":         false,
		"plans":                     Object{},
	}
	if digest != "" {
		workflow["history_cutover_reviews"] = []any{digest}
	}
	if reviewIssue != nil {
		workflow["history_cutover_review_issue"] = reviewIssue
	}
	return Object{"schema_version": int64(1), "workflows": Object{"task.yml": workflow}}
}

func TestHistoryCutoverReviewChannelsAreExactAndMutuallyExclusive(t *testing.T) {
	baseline := captureHistoryCutoverFixture(t, historyCutoverFixture(nil, nil, nil, nil))
	digest := fmt.Sprint(baseline["sha256"])
	target := baseline["target"]

	t.Run("static exact digest", func(t *testing.T) {
		engine, err := NewEngine(historyCutoverPolicy(digest, nil), historyCutoverTestRepository)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.ValidateReviewedHistoryCutover(baseline, target); err != nil {
			t.Fatalf("exact reviewed digest rejected: %v", err)
		}
		chain, err := HistoryCutoverChain(target, baseline)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.ValidateChain(chain, target, nil, map[int64]int64{}); err != nil {
			t.Fatalf("exact static review does not admit the cutover chain: %v", err)
		}
		if _, err := engine.ValidateReviewedHistoryCutover(baseline, mustDifferentCutoverTarget(t, target)); err == nil {
			t.Fatal("static digest was accepted for a different target")
		}
	})

	t.Run("dynamic issue review", func(t *testing.T) {
		engine, err := NewEngine(historyCutoverPolicy("", Object{"number": int64(11), "trusted_logins": []any{"reviewer"}}), historyCutoverTestRepository)
		if err != nil {
			t.Fatal(err)
		}
		if issues := engine.HistoryCutoverReviewIssues(); !Equal(issues, map[string]any{"task.yml": Object{"number": int64(11), "trusted_logins": []any{"reviewer"}}}) {
			t.Fatalf("configured issue review route changed: %#v", issues)
		}
		if _, err := engine.ValidateReviewedHistoryCutover(baseline, target); err == nil {
			t.Fatal("unadmitted dynamic digest became trusted")
		}
		if err := engine.AdmitHistoryCutoverReview("task.yml", digest); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.ValidateReviewedHistoryCutover(baseline, target); err != nil {
			t.Fatalf("admitted exact dynamic digest was rejected: %v", err)
		}
		changed := cloneHistoryCutoverObject(t, baseline)
		workflow, err := object(changed["workflow"], "changed workflow")
		if err != nil {
			t.Fatal(err)
		}
		workflow["state"] = "disabled"
		changed = resealHistoryCutoverFixture(t, changed)
		if changed["sha256"] == digest {
			t.Fatal("test mutation did not change the exact baseline digest")
		}
		if _, err := engine.ValidateReviewedHistoryCutover(changed, target); err == nil {
			t.Fatal("dynamic route admitted a baseline with a different exact digest")
		}
	})

	t.Run("static and dynamic routes cannot be combined", func(t *testing.T) {
		if _, err := NewEngine(historyCutoverPolicy(digest, Object{"number": int64(11), "trusted_logins": []any{"reviewer"}}), historyCutoverTestRepository); err == nil {
			t.Fatal("mutually exclusive static and issue review routes were accepted")
		}
	})

	t.Run("no review route", func(t *testing.T) {
		engine, err := NewEngine(historyCutoverPolicy("", nil), historyCutoverTestRepository)
		if err != nil {
			t.Fatal(err)
		}
		chain, err := HistoryCutoverChain(target, baseline)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.ValidateChain(chain, target, nil, map[int64]int64{}); err == nil {
			t.Fatal("unreviewed history cutover chain was accepted")
		}
	})
}

func mustDifferentCutoverTarget(t *testing.T, value any) Object {
	t.Helper()
	parsed, err := ValidateTarget(value)
	if err != nil {
		t.Fatal(err)
	}
	target := cloneHistoryCutoverObject(t, parsed)
	target["workflow_id"] = int64(10)
	return target
}

func TestHistoryCutoverChainKeepsUnknownAttemptsOutsideNativeSettlementAndInventory(t *testing.T) {
	run := recoveryHistoryRun(101, 2, "Old rerun")
	attempts := map[string]Object{
		historyCutoverAttemptEndpoint(101, 1): historyCutoverAttemptResponse(run, 1, "2026-01-01T00:00:00Z"),
		historyCutoverAttemptEndpoint(101, 2): historyCutoverAttemptResponse(run, 2, "2026-01-02T00:00:00Z"),
	}
	baseline := captureHistoryCutoverFixture(t, historyCutoverFixture([]Object{run}, nil, attempts, nil))
	chain, err := HistoryCutoverChain(baseline["target"], baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !exactInt(chain["schema_version"], historyCutoverSettlementSchemaVersion) {
		t.Fatalf("cutover chain schema = %#v", chain["schema_version"])
	}
	if got := len(chain["inventory"].([]any)); got != 0 {
		t.Fatalf("quarantined run copied into native inventory (%d rows)", got)
	}
	if got := len(chain["settlements"].([]any)); got != 0 {
		t.Fatalf("quarantined attempt copied into native settlements (%d rows)", got)
	}
	rows := baseline["attempts"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected two retained unknown attempts, got %d", len(rows))
	}
	for _, raw := range rows {
		row := raw.(Object)
		if row["outcome"] != "unknown" || row["handling"] != "quarantined-never-replay" {
			t.Fatalf("cutover changed historical attempt meaning: %#v", row)
		}
	}
}

func TestCutoverUsesNormalizedObservedRowsAndLeavesNewRunsAndRerunsPending(t *testing.T) {
	baselineRun := recoveryHistoryRun(101, 2, "Legacy rerun")
	baseline := captureHistoryCutoverFixture(t, historyCutoverFixture([]Object{baselineRun}, nil, map[string]Object{
		historyCutoverAttemptEndpoint(101, 1): historyCutoverAttemptResponse(baselineRun, 1, "2026-01-01T00:00:00Z"),
		historyCutoverAttemptEndpoint(101, 2): historyCutoverAttemptResponse(baselineRun, 2, "2026-01-02T00:00:00Z"),
	}, nil))
	target := baseline["target"]
	chain, err := HistoryCutoverChain(target, baseline)
	if err != nil {
		t.Fatal(err)
	}

	newRun := recoveryHistoryRun(202, 1, "First run after cutover")
	newRunTwo := recoveryHistoryRun(303, 1, "Another run after cutover")
	newRunTwo["head_sha"] = strings.Repeat("c", 40)
	observed, latest, err := NormalizeInventory([]any{baselineRun, newRun, newRunTwo}, target)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range observed {
		if _, exists := row["run_attempt"]; exists {
			t.Fatalf("invocationHistory's normalized immutable row retained run_attempt: %#v", row)
		}
	}
	if latest[101] != 2 || latest[202] != 1 || latest[303] != 1 {
		t.Fatalf("latest attempts were not kept in their separate inventory: %#v", latest)
	}

	// GitHub's immutable workflow row is unchanged across reruns. The later
	// latest-attempt map records the new attempt separately from that identity.
	latest[101] = 3
	latest[202] = 1
	latest[303] = 1
	if _, err := testRecoveryEngine(t, false).ValidateChain(chain, target, observed, latest); err == nil {
		t.Fatal("unreviewed cutover unexpectedly passed default policy")
	}
	engine, err := NewEngine(historyCutoverPolicy(fmt.Sprint(baseline["sha256"]), nil), historyCutoverTestRepository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ValidateChain(chain, target, observed, latest); err != nil {
		t.Fatalf("valid complete immutable history with a separate rerun high-water rejected: %v", err)
	}
	pending, err := PendingAttempts(chain, observed, latest, 202, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("post-cutover rerun and new run were silently included in the cutoff: %#v", pending)
	}
	firstRun, firstErr := object(pending[0]["run"], "first pending run")
	secondRun, secondErr := object(pending[1]["run"], "second pending run")
	if firstErr != nil || secondErr != nil || !exactInt(pending[0]["attempt"], 3) || !exactInt(firstRun["id"], 101) || !exactInt(pending[1]["attempt"], 1) || !exactInt(secondRun["id"], 303) {
		t.Fatalf("post-cutover rerun and new run were silently included in the cutoff: %#v", pending)
	}
}

func TestDefaultSchemaFourAndFiveChainsRemainStrictWithoutCutoverPolicy(t *testing.T) {
	engine := testRecoveryEngine(t, false)
	target, err := TargetObject("task.yml", historyCutoverTestRepository.URL, 9, "workflow-history-v2")
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []int64{settlementSchemaVersion, legacySettlementSchemaVersion} {
		t.Run(fmt.Sprintf("schema %d", schema), func(t *testing.T) {
			chain, err := EmptyChain(target)
			if err != nil {
				t.Fatal(err)
			}
			chain["schema_version"] = schema
			unsigned := Object{}
			for _, field := range chainFields[:len(chainFields)-1] {
				unsigned[field] = chain[field]
			}
			canonical, err := Canonical(unsigned)
			if err != nil {
				t.Fatal(err)
			}
			chain["sha256"] = SHA256(canonical)
			if _, err := engine.ValidateChain(chain, target, nil, map[int64]int64{}); err != nil {
				t.Fatalf("default schema %d chain changed under cutover feature: %v", schema, err)
			}
		})
	}
}
