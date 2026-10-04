package planning

import (
	"reflect"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestNormalizeQuarterPlanGoldenAndLegacyBacklogDelta(t *testing.T) {
	plan := readObject(t, "../../testdata/planning/quarter-plan.json")
	normalized, err := NormalizeQuarterPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, normalized, "../../testdata/planning/quarter-plan-normalized.golden.json")

	legacy := contract.Object{
		"schema_version": int64(1), "quarter": "2026 Q1",
		"items": []any{
			contract.Object{"title": "Enhancement: Reliability", "epic": nil, "priority": "Next"},
			contract.Object{"title": "Epic: Runtime", "epic": "Initiative: Platform", "priority": nil},
		},
	}
	delta, err := BuildQuarterPlanBacklogDelta(legacy)
	if err != nil {
		t.Fatal(err)
	}
	issues, _ := contract.Objects(delta, "issues")
	if len(issues) != 2 || issues[0]["title"] != "Enhancement: Reliability" || issues[1]["type"] != "Epic" || issues[1]["epic"] != "Initiative: Platform" {
		t.Fatalf("legacy backlog delta was not normalized and title-sorted: %#v", delta)
	}
	if issues[0]["status"] != nil || issues[0]["priority"] != "Next" {
		t.Fatalf("legacy issue defaults changed: %#v", issues[0])
	}
}

func TestQuarterPlanStrictSchemaAndLegacyHierarchy(t *testing.T) {
	plan := readObject(t, "../../testdata/planning/quarter-plan.json")
	for _, version := range []any{true, "1", 1.0, int64(2), nil} {
		bad, err := contract.Clone(plan)
		if err != nil {
			t.Fatal(err)
		}
		bad["schema_version"] = version
		if _, err := NormalizeQuarterPlan(bad); err == nil {
			t.Fatalf("accepted schema_version %#v", version)
		}
	}
	bad, _ := contract.Clone(plan)
	bad["commit_issue_numbers"].([]any)[0] = true
	if _, err := NormalizeQuarterPlan(bad); err == nil {
		t.Fatal("accepted boolean issue number")
	}
	bad = contract.Object{"schema_version": int64(1), "quarter": "2026 Q1", "items": []any{
		contract.Object{"title": "Initiative: Core", "epic": "Epic: Bad parent"},
	}}
	if _, err := NormalizeQuarterPlan(bad); err == nil {
		t.Fatal("accepted parent for an Initiative")
	}
	bad = contract.Object{"schema_version": int64(1), "quarter": "2026 Q1", "items": []any{
		contract.Object{"title": "Epic: Runtime", "epic": "Bug: Wrong parent"},
	}}
	if _, err := NormalizeQuarterPlan(bad); err == nil {
		t.Fatal("accepted a non-Initiative parent for an Epic")
	}
}

func TestBuildQuarterPlanDeltaUsesCompleteSnapshotAndPreparesScopedOperations(t *testing.T) {
	input := readObject(t, "../../testdata/planning/quarter-delta-input.json")
	result, err := BuildQuarterPlanDelta(input)
	if err != nil {
		t.Fatal(err)
	}
	delta := nested(t, result, "delta")
	if !reflect.DeepEqual(delta["current_commit_issue_numbers"], []any{int64(1), int64(4)}) || !reflect.DeepEqual(delta["add_issue_numbers"], []any{int64(2)}) || !reflect.DeepEqual(delta["keep_issue_numbers"], []any{int64(1)}) || !reflect.DeepEqual(delta["remove_issue_numbers"], []any{int64(4)}) {
		t.Fatalf("quarter assignment sets = %#v", delta)
	}
	if delta["target_milestone_description"] != "Quarter: 2026 Q1\nDate range: 2026-01-01 through 2026-03-31\nQuarter goals:\n- Ship the migration.\nActive initiatives:\n- Reliability\nUse only for issues explicitly committed to this quarter.\nRecord quarter rationale in the issue body or a maintainer comment." {
		t.Fatalf("milestone description = %q", delta["target_milestone_description"])
	}
	summary := nested(t, delta, "summary")
	if summary["milestone_finding_count"] != 2 {
		t.Fatalf("milestone summary = %#v", summary)
	}
	prep := nested(t, delta, "review_preparation")
	operations, _ := contract.Objects(prep, "operations")
	if len(operations) != 3 || operations[0]["id"] != "milestone:2026 Q1" || operations[1]["id"] != "quarter-assignment:2" || operations[2]["id"] != "quarter-assignment:4" {
		t.Fatalf("review operations = %#v", operations)
	}
	if got := nested(t, operations[1], "before"); got["milestone"] != nil || got["rationale_comment_ids"].([]any) == nil {
		t.Fatalf("new assignment before state lost explicit null/empty inventory: %#v", got)
	}
	if nested(t, operations[2], "after")["milestone"] != nil {
		t.Fatal("clear operation must preserve explicit null after-state")
	}
	if _, ok := prep["approval"]; ok {
		t.Fatal("domain review preparation must not act as approval")
	}
}

func TestQuarterPreviewDoesNotInferAuthorityFromNonemptyOrIncompleteSnapshots(t *testing.T) {
	input := readObject(t, "../../testdata/planning/quarter-delta-input.json")
	seeded, _ := contract.Clone(input)
	seeded["source_evidence"].(map[string]any)["issues"].(map[string]any)["live"] = false
	result, err := BuildQuarterPlanDelta(seeded)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := nested(t, result, "delta")["review_preparation"]; exists {
		t.Fatal("seed snapshot became apply-eligible because it was nonempty")
	}
	incomplete, _ := contract.Clone(input)
	issues := incomplete["issue_graph"].(map[string]any)["issues"].([]any)
	delete(issues[0].(map[string]any), "url")
	result, err = BuildQuarterPlanDelta(incomplete)
	if err != nil {
		t.Fatal(err)
	}
	delta := nested(t, result, "delta")
	if got := delta["current_commit_issue_numbers"].([]any); len(got) != 0 {
		t.Fatalf("partial issue inventory produced assignments: %#v", got)
	}
	if _, exists := delta["review_preparation"]; exists {
		t.Fatal("incomplete issue inventory produced review operations")
	}
}

func TestRunRoutesNestedQuarterAndReviewDomainInputs(t *testing.T) {
	quarterInput := readObject(t, "../../testdata/planning/quarter-delta-input.json")
	quarterResult, err := Run("quarter-plan-delta", map[string]contract.Object{"input": quarterInput})
	if err != nil {
		t.Fatal(err)
	}
	if nested(t, quarterResult, "delta")["quarter"] != "2026 Q1" {
		t.Fatalf("quarter Run result = %#v", quarterResult)
	}
	reviewInput := readObject(t, "../../testdata/planning/review-backlog-input.json")
	reviewResult, err := Run("review-backlog-delta", map[string]contract.Object{"input": reviewInput})
	if err != nil {
		t.Fatal(err)
	}
	if nested(t, reviewResult, "delta")["proposal_count"] != 1 {
		t.Fatalf("review Run result = %#v", reviewResult)
	}
	findings, err := Run("review-closeout-findings-validate", map[string]contract.Object{"payload": readObject(t, "../../testdata/planning/review-closeout-findings.json")})
	if err != nil || findings["mode"] != "epic-closeout" {
		t.Fatalf("closeout Run result = %#v, %v", findings, err)
	}
}

func TestReviewFindingNormalizationGoldenAndStrictReferences(t *testing.T) {
	payload := readObject(t, "../../testdata/planning/review-findings.json")
	normalized, err := NormalizeReviewFindings(payload)
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, normalized, "../../testdata/planning/review-findings-normalized.golden.json")
	for _, test := range []struct {
		field string
		value any
	}{{"existing_issue", true}, {"existing_issue", 1.5}, {"parent_issue_number", false}, {"blocked_by_issue_numbers", []any{2.5}}} {
		bad, _ := contract.Clone(payload)
		bad["findings"].([]any)[0].(map[string]any)[test.field] = test.value
		if _, err := NormalizeReviewFindings(bad); err == nil {
			t.Errorf("accepted invalid %s value %#v", test.field, test.value)
		}
	}
	bad, _ := contract.Clone(payload)
	bad["schema_version"] = true
	if _, err := NormalizeReviewFindings(bad); err == nil {
		t.Fatal("accepted boolean schema version")
	}
	bad, _ = contract.Clone(payload)
	bad["findings"].([]any)[1].(map[string]any)["backlog_title"] = "Different group"
	if _, err := NormalizeReviewFindings(bad); err == nil {
		t.Fatal("accepted grouped findings with conflicting backlog titles")
	}
}

func TestReviewBacklogDeltaUsesExplicitPolicyAndCatalog(t *testing.T) {
	input := readObject(t, "../../testdata/planning/review-backlog-input.json")
	raw := readObject(t, "../../testdata/planning/review-findings.json")
	normalized, err := NormalizeReviewFindings(raw)
	if err != nil {
		t.Fatal(err)
	}
	input["findings"] = normalized
	result, err := BuildReviewBacklogDelta(input)
	if err != nil {
		t.Fatal(err)
	}
	delta := nested(t, result, "delta")
	proposals, _ := contract.Objects(delta, "proposals")
	if len(proposals) != 2 || proposals[0]["title"] != "Bug: Parser stabilization" || proposals[0]["action"] != "reuse-open" || proposals[0]["issue_number"] != int64(42) {
		t.Fatalf("exact open issue matching changed: %#v", proposals)
	}
	if proposals[0]["priority"] != "Now" || !reflect.DeepEqual(proposals[0]["blocked_by_issue_numbers"], []int64{3, 4}) || proposals[0]["parent_issue_number"] != int64(12) {
		t.Fatalf("grouped proposal aggregation changed: %#v", proposals[0])
	}
	if proposals[1]["action"] != "create" || proposals[1]["priority"] != "Later" {
		t.Fatalf("unmatched finding proposal = %#v", proposals[1])
	}
	if len(proposals[1]["candidate_matches"].([]any)) != 2 {
		t.Fatalf("candidate matching omitted related issues: %#v", proposals[1]["candidate_matches"])
	}
	if delta["issue_catalog_mode"] != "live" {
		t.Fatalf("explicit catalog mode changed: %#v", delta["issue_catalog_mode"])
	}
	if _, exists := delta["review"]; exists {
		t.Fatal("pure builder minted an apply review")
	}
	missingPolicy, _ := contract.Clone(input)
	delete(missingPolicy, "policy")
	if _, err := BuildReviewBacklogDelta(missingPolicy); err == nil {
		t.Fatal("missing consumer priority/label policy was guessed")
	}
}

func TestLegacyReviewBacklogRequiresExplicitConsumerMappings(t *testing.T) {
	payload := contract.Object{"schema_version": int64(1), "findings": []any{
		contract.Object{"title": "Validate tokens", "summary": "Reject invalid tokens", "kind": "bug", "severity": "high"},
	}}
	policy := contract.Object{
		"legacy_kind_to_type":         contract.Object{"bug": "Bug"},
		"legacy_kind_to_label":        contract.Object{"bug": "bug"},
		"legacy_severity_to_priority": contract.Object{"high": "Now", "medium": "Next", "low": "Later"},
	}
	delta, err := BuildLegacyReviewBacklogDelta(payload, policy)
	if err != nil {
		t.Fatal(err)
	}
	issue := delta["issues"].([]any)[0].(map[string]any)
	if issue["title"] != "Bug: Validate tokens" || issue["priority"] != "Now" || issue["status"] != "Todo" {
		t.Fatalf("legacy result = %#v", issue)
	}
}

func TestCloseoutNormalizationAuditAndEpicState(t *testing.T) {
	payload := readObject(t, "../../testdata/planning/review-closeout-findings.json")
	normalized, err := NormalizeReviewCloseoutFindings(payload)
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, normalized, "../../testdata/planning/review-closeout-findings-normalized.golden.json")
	findings, _ := contract.Objects(normalized, "findings")
	ordered := SortReviewCloseoutFindings(findings)
	if ordered[0]["id"] != "finding-1" || ordered[1]["id"] != "finding-2" {
		t.Fatalf("closeout order = %#v", ordered)
	}
	backlog := DeriveReviewCloseoutBacklogFindings(findings, "Release readiness")
	if got := backlog["findings"].([]any); len(got) != 1 || got[0].(map[string]any)["existing_issue"] != int64(77) {
		t.Fatalf("derived backlog findings = %#v", backlog)
	}
	graph := contract.Object{"issues": []any{
		contract.Object{"number": int64(99), "title": "Epic: Release", "state": "OPEN", "child_numbers": []any{int64(100), int64(101)}, "blocked_by_numbers": []any{int64(102), int64(103)}},
		contract.Object{"number": int64(100), "state": "CLOSED"}, contract.Object{"number": int64(101), "state": "OPEN"},
		contract.Object{"number": int64(102), "state": "CLOSED"}, contract.Object{"number": int64(103), "state": "OPEN"},
	}}
	epic, err := EpicIssueState(graph, 99)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(epic["open_child_numbers"], []any{int64(101)}) || !reflect.DeepEqual(epic["blocked_by_numbers"], []any{int64(103)}) {
		t.Fatalf("epic live state = %#v", epic)
	}
	audit, err := BuildReviewCloseoutAudit(contract.Object{
		"repo":             nested(t, readObject(t, "../../testdata/planning/baseline.json"), "repo"),
		"normalized_input": normalized, "generated_at": "2026-01-01T00:00:00Z",
		"backlog_delta": contract.Object{"proposal_count": int64(1)}, "backlog_finding_count": int64(1),
		"blocking_reasons": []any{"The epic still has an open child."}, "epic_state": epic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nested(t, audit, "summary")["closeout_ready"] != false || nested(t, audit, "verdict")["closeout_ready"] != false {
		t.Fatalf("closeout blockers were dropped: %#v", audit)
	}
	governance, err := MatchingGovernanceFindings(contract.Object{"findings": []any{
		contract.Object{"number": int64(99), "code": "child", "severity": "warning", "message": "Open child"},
		contract.Object{"number": int64(101), "code": "other"},
	}}, 99, "relationship-audit")
	if err != nil || len(governance) != 1 || governance[0].(map[string]any)["source"] != "relationship-audit" {
		t.Fatalf("governance matching = %#v, %v", governance, err)
	}
}

func TestCloseoutRawReferencesAndRoutingFailClosed(t *testing.T) {
	payload := readObject(t, "../../testdata/planning/review-closeout-findings.json")
	for _, test := range []struct {
		field string
		value any
	}{{"existing_issue", true}, {"parent_issue_number", 2.5}, {"blocked_by_issue_numbers", []any{true}}} {
		bad, _ := contract.Clone(payload)
		bad["findings"].([]any)[1].(map[string]any)[test.field] = test.value
		if _, err := NormalizeReviewCloseoutFindings(bad); err == nil {
			t.Errorf("accepted invalid closeout reference %s=%#v", test.field, test.value)
		}
	}
	bad, _ := contract.Clone(payload)
	bad["findings"].([]any)[0].(map[string]any)["destination"] = "none"
	if _, err := NormalizeReviewCloseoutFindings(bad); err == nil {
		t.Fatal("accepted a blocking finding with destination none")
	}
}
