package planning

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func readObject(t *testing.T, path string) contract.Object {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value, err := contract.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func fixture(t *testing.T) contract.Object {
	t.Helper()
	return readObject(t, filepath.Join("..", "..", "testdata", "planning", "baseline.json"))
}

func nested(t *testing.T, root contract.Object, key string) contract.Object {
	t.Helper()
	value, err := contract.ObjectAt(root, key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func equalJSON(t *testing.T, got any, path string) {
	t.Helper()
	want := readObject(t, path)
	gotBytes, err := contract.Canonical(got)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := contract.Canonical(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", gotBytes, wantBytes)
	}
}

func TestRelationshipTopologyAndCanonicalCycles(t *testing.T) {
	data := fixture(t)
	topology, err := RelationshipTopology(nested(t, data, "issue_graph"))
	if err != nil {
		t.Fatal(err)
	}
	blockers := topology["blocked_by"].(contract.Object)
	if !reflect.DeepEqual(blockers["2"], []int64{1}) {
		t.Fatalf("issue 2 blockers = %#v", blockers["2"])
	}
	cycles, err := FindDirectedCycles(contract.Object{"1": []int64{2}, "2": []int64{3}, "3": []int64{1}, "4": []int64{4}})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]int64{{1, 2, 3}, {4}}
	if !reflect.DeepEqual(cycles, want) {
		t.Fatalf("cycles = %#v; want %#v", cycles, want)
	}
	sharing := contract.Object{"1": []int64{2, 3}, "2": []int64{3}, "3": []int64{1}}
	cycles, err = FindDirectedCycles(sharing)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]int64{{1, 2, 3}, {1, 3}}; !reflect.DeepEqual(cycles, want) {
		t.Fatalf("overlapping cycles = %#v; want %#v", cycles, want)
	}
}

func TestCycleSearchPrunesDenseAcyclicGraph(t *testing.T) {
	graph := contract.Object{}
	for from := int64(1); from <= 36; from++ {
		neighbors := []int64{}
		for to := from + 1; to <= 36; to++ {
			neighbors = append(neighbors, to)
		}
		graph[jsonNumber(from)] = neighbors
	}
	cycles, err := FindDirectedCycles(graph)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 0 {
		t.Fatalf("acyclic graph returned cycles: %#v", cycles)
	}
}

func TestRelationshipGraphRejectsIncompleteInconsistentAndNonintegerState(t *testing.T) {
	data := fixture(t)
	graph := nested(t, data, "issue_graph")
	bad, err := contract.Clone(graph)
	if err != nil {
		t.Fatal(err)
	}
	issues := bad["issues"].([]any)
	issues[0].(map[string]any)["parent_number"] = nil
	delete(issues[0].(map[string]any), "child_numbers")
	if _, err := RelationshipTopology(bad); err == nil {
		t.Fatal("missing child state was accepted")
	}
	bad, err = contract.Clone(graph)
	if err != nil {
		t.Fatal(err)
	}
	issues = bad["issues"].([]any)
	issues[0].(map[string]any)["child_numbers"] = []any{json.Number("3")}
	issues[2].(map[string]any)["parent_number"] = json.Number("2")
	if _, err := RelationshipTopology(bad); err == nil {
		t.Fatal("inconsistent parent/child state was accepted")
	}
	bad, err = contract.Clone(graph)
	if err != nil {
		t.Fatal(err)
	}
	issues = bad["issues"].([]any)
	issues[0].(map[string]any)["blocked_by_numbers"] = []any{true}
	if _, err := RelationshipTopology(bad); err == nil {
		t.Fatal("boolean blocker was accepted")
	}
}

func TestRelationshipValidationAndDelta(t *testing.T) {
	data := fixture(t)
	graph := nested(t, data, "issue_graph")
	payload := nested(t, data, "relationship_payload")
	normalized, err := ValidateRelationshipPayload(payload, graph, nil)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := BuildRelationshipDelta(normalized, graph)
	if err != nil {
		t.Fatal(err)
	}
	changes := delta["changes"].([]any)
	if len(changes) != 2 {
		t.Fatalf("change count = %d", len(changes))
	}
	if changes[0].(contract.Object)["type"] != "add_blocked_by" || changes[1].(contract.Object)["type"] != "set_parent" {
		t.Fatalf("change order = %#v", changes)
	}
	if delta["summary"].(contract.Object)["add_blocked_by_count"] != 1 {
		t.Fatalf("summary = %#v", delta["summary"])
	}
	if normalized["notes"] != "review local topology" {
		t.Fatalf("notes not normalized: %#v", normalized["notes"])
	}
	malformed, err := contract.Clone(normalized)
	if err != nil {
		t.Fatal(err)
	}
	malformed["issues"].([]any)[1].(map[string]any)["desired_parent_issue_number"] = "wrong"
	if _, err := BuildRelationshipDelta(malformed, graph); err == nil {
		t.Fatal("delta builder accepted malformed normalized parent state")
	}
}

func TestRelationshipOfflinePreviewSupportsLegacyIncompleteGraph(t *testing.T) {
	data := fixture(t)
	graph, err := contract.Clone(nested(t, data, "issue_graph"))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range graph["issues"].([]any) {
		delete(raw.(map[string]any), "child_numbers")
	}
	normalized, err := ValidateRelationshipPayload(nested(t, data, "relationship_payload"), graph, nil)
	if err != nil {
		t.Fatalf("offline preview rejected a legacy graph with omitted child edges: %v", err)
	}
	if _, err := BuildRelationshipDelta(normalized, graph); err != nil {
		t.Fatalf("delta builder rejected a legacy graph: %v", err)
	}
	if _, err := RelationshipTopology(graph); err == nil {
		t.Fatal("complete topology accepted the same incomplete graph")
	}
}

func TestRelationshipValidationRejectsCyclesForeignRefsAndBadNumbers(t *testing.T) {
	data := fixture(t)
	graph := nested(t, data, "issue_graph")
	cyclic := contract.Object{"schema_version": json.Number("1"), "issues": []any{contract.Object{"issue_number": json.Number("1"), "desired_blocked_by_issue_numbers": []any{json.Number("2")}}}}
	if _, err := ValidateRelationshipPayload(cyclic, graph, nil); err == nil {
		t.Fatal("new dependency cycle was accepted")
	}
	badInteger := contract.Object{"schema_version": 1, "issues": []any{contract.Object{"issue_number": true}}}
	if _, err := ValidateRelationshipPayload(badInteger, graph, nil); err == nil {
		t.Fatal("boolean issue reference was accepted")
	}
	badSchema := contract.Object{"schema_version": json.Number("1.0"), "issues": []any{contract.Object{"issue_number": json.Number("1")}}}
	if _, err := ValidateRelationshipPayload(badSchema, graph, nil); err == nil {
		t.Fatal("fractional schema version notation was accepted")
	}
	fractional := contract.Object{"schema_version": 1, "issues": []any{contract.Object{"issue_number": float64(1), "desired_blocked_by_issue_numbers": []any{float64(3)}}}}
	if _, err := ValidateRelationshipPayload(fractional, graph, nil); err == nil {
		t.Fatal("fractional issue reference was accepted")
	}
	foreign := contract.Object{"schema_version": 1, "issues": []any{contract.Object{"issue_number": "owner/repo#3"}}}
	if _, err := ValidateRelationshipPayload(foreign, graph, nil); err == nil {
		t.Fatal("foreign repository reference was accepted")
	}
}

func TestHierarchyTaxonomyIsCallerSuppliedAndOptional(t *testing.T) {
	data := fixture(t)
	graph := nested(t, data, "issue_graph")
	payload := contract.Object{"schema_version": 1, "issues": []any{contract.Object{"issue_number": 3, "desired_parent_issue_number": 1}}}
	if _, err := ValidateRelationshipPayload(payload, graph, nil); err != nil {
		t.Fatalf("taxonomy-free structural validation failed: %v", err)
	}
	policy := contract.Object{"Task": []any{"Task"}}
	if _, err := ValidateRelationshipPayload(payload, graph, policy); err != nil {
		t.Fatalf("generic supplied taxonomy rejected valid pair: %v", err)
	}
	if _, err := ValidateRelationshipPayload(payload, graph, contract.Object{}); err == nil {
		t.Fatal("explicit empty taxonomy accepted a parent assignment")
	}
}

func TestRelationshipAuditFindsMultipleParentsAndCycles(t *testing.T) {
	data := fixture(t)
	graph, err := contract.Clone(nested(t, data, "issue_graph"))
	if err != nil {
		t.Fatal(err)
	}
	issues := graph["issues"].([]any)
	issues[0].(map[string]any)["blocked_by_numbers"] = []any{json.Number("2")}
	issues[0].(map[string]any)["child_numbers"] = []any{json.Number("3")}
	issues[1].(map[string]any)["child_numbers"] = []any{json.Number("3")}
	audit, err := AuditRelationships(graph)
	if err != nil {
		t.Fatal(err)
	}
	if audit["summary"].(contract.Object)["finding_count"] != 2 {
		t.Fatalf("unexpected audit summary: %#v", audit["summary"])
	}
	findings := audit["findings"].([]contract.Object)
	if findings[0]["code"] != "multiple-parents" {
		t.Fatalf("first finding = %#v", findings[0])
	}
}

func TestSelectNextItemGoldenAndCandidateReason(t *testing.T) {
	data := fixture(t)
	snapshot := nested(t, data, "queue_snapshot")
	candidate := int64(2)
	got, err := SelectNextItem(snapshot, &candidate)
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, got, filepath.Join("..", "..", "testdata", "planning", "next-item.golden.json"))
	if got["selected"].(contract.Object)["number"] != int64(1) {
		t.Fatalf("selected = %#v", got["selected"])
	}
}

func TestSelectNextItemFallsThroughToNextButNeverLater(t *testing.T) {
	data := fixture(t)
	snapshot, err := contract.Clone(nested(t, data, "queue_snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	items := snapshot["items"].([]any)
	items[0].(map[string]any)["blocked_by_numbers"] = []any{json.Number("2")}
	got, err := SelectNextItem(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["all_now_blocked"] != true || got["selected"].(contract.Object)["number"] != int64(3) || got["selected_band"] != "Next" {
		t.Fatalf("unexpected fallback selection: %#v", got)
	}
	items[2].(map[string]any)["field_values"].(map[string]any)["Priority"] = "Later"
	got, err = SelectNextItem(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["selected"] != nil {
		t.Fatalf("Later item selected after earlier bands were blocked: %#v", got["selected"])
	}
}

func TestRankedRebalanceGoldenAndDependencyPrecedence(t *testing.T) {
	data := fixture(t)
	result, err := BuildRankedRebalanceSeed(nested(t, data, "issue_graph"), nested(t, data, "queue_snapshot"), nested(t, data, "project_snapshot"), nested(t, data, "ranking_policy"), contract.Object{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	equalJSON(t, result["decisions"], filepath.Join("..", "..", "testdata", "planning", "ranked-decisions.golden.json"))
	issues := result["report"].(contract.Object)["issues"].([]any)
	numbers := []int64{}
	for _, row := range issues {
		numbers = append(numbers, row.(contract.Object)["issue_number"].(int64))
	}
	if !reflect.DeepEqual(numbers, []int64{1, 3, 2}) {
		t.Fatalf("rank order violates dependency precedence: %v", numbers)
	}
}

func TestRankingHonorsZeroNowTargetAndRejectsWrongRepository(t *testing.T) {
	data := fixture(t)
	policy, err := contract.Clone(nested(t, data, "ranking_policy"))
	if err != nil {
		t.Fatal(err)
	}
	policy["band_targets"].(map[string]any)["now"] = json.Number("0")
	result, err := BuildRankedRebalanceSeed(nested(t, data, "issue_graph"), nested(t, data, "queue_snapshot"), nested(t, data, "project_snapshot"), policy, contract.Object{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result["decisions"].(contract.Object)["now_issue_numbers"].([]int64)) != 0 {
		t.Fatal("zero now target assigned items")
	}
	project, err := contract.Clone(nested(t, data, "project_snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	project["repo"].(map[string]any)["url"] = "https://example.com/sample-org/sample-project"
	project["repo"].(map[string]any)["host"] = "example.com"
	if _, err := BuildRankedRebalanceSeed(nested(t, data, "issue_graph"), nested(t, data, "queue_snapshot"), project, nested(t, data, "ranking_policy"), contract.Object{}, nil); err == nil {
		t.Fatal("repository host mismatch was accepted")
	}
	queue, err := contract.Clone(nested(t, data, "queue_snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	queue["project"].(map[string]any)["number"] = true
	if _, err := BuildRankedRebalanceSeed(nested(t, data, "issue_graph"), queue, nested(t, data, "project_snapshot"), nested(t, data, "ranking_policy"), contract.Object{}, nil); err == nil {
		t.Fatal("boolean project number was accepted")
	}
}

func TestRankingExclusionsAreExplicitAndGeneric(t *testing.T) {
	data := fixture(t)
	result, err := BuildRankedRebalanceSeed(nested(t, data, "issue_graph"), nested(t, data, "queue_snapshot"), nested(t, data, "project_snapshot"), nested(t, data, "ranking_policy"), contract.Object{}, []string{"Task"})
	if err != nil {
		t.Fatal(err)
	}
	decisions := result["decisions"].(contract.Object)
	if len(decisions["now_issue_numbers"].([]int64)) != 0 || len(decisions["archive_issue_numbers"].([]int64)) != 0 {
		t.Fatalf("explicit generic prefix exclusion was ignored: %#v", decisions)
	}
	if result["report"].(contract.Object)["summary"].(contract.Object)["excluded_count"] != 3 {
		t.Fatalf("exclusion summary = %#v", result["report"].(contract.Object)["summary"])
	}
}

func TestRankingOmitsItemsAlreadyMarkedFinished(t *testing.T) {
	data := fixture(t)
	queue, err := contract.Clone(nested(t, data, "queue_snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	queue["items"] = append(queue["items"].([]any), contract.Object{"number": json.Number("4"), "title": "Task: Completed", "prefix": "Task", "state": "CLOSED", "field_values": contract.Object{"Priority": "Later", "Queue Order": json.Number("40"), "Status": "Done"}, "blocked_by_numbers": []any{}, "child_numbers": []any{}, "actionable_child_numbers": []any{}, "project_item": contract.Object{"item_id": "ITEM_4"}})
	result, err := BuildRankedRebalanceSeed(nested(t, data, "issue_graph"), queue, nested(t, data, "project_snapshot"), nested(t, data, "ranking_policy"), contract.Object{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result["report"].(contract.Object)["summary"].(contract.Object)["active_issue_count"] != 3 {
		t.Fatalf("finished project item was ranked as active: %#v", result["report"].(contract.Object)["summary"])
	}
}

func TestRebalanceDecisionNormalizationIsStrict(t *testing.T) {
	data := fixture(t)
	payload := nested(t, data, "rebalance_payload")
	graph := nested(t, data, "issue_graph")
	project := nested(t, data, "project_snapshot")
	got, err := NormalizeRebalanceDecisions(payload, graph, project, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["now_issue_numbers"], []int64{2, 1}) {
		t.Fatalf("normalized decisions = %#v", got)
	}
	bad, err := contract.Clone(payload)
	if err != nil {
		t.Fatal(err)
	}
	bad["now_issue_numbers"] = []any{true}
	if _, err := NormalizeRebalanceDecisions(bad, graph, project, nil); err == nil {
		t.Fatal("boolean decision number was accepted")
	}
	bad, err = contract.Clone(payload)
	if err != nil {
		t.Fatal(err)
	}
	bad["now_issue_numbers"] = []any{json.Number("1.5")}
	if _, err := NormalizeRebalanceDecisions(bad, graph, project, nil); err == nil {
		t.Fatal("fractional decision number was accepted")
	}
	bad, err = contract.Clone(payload)
	if err != nil {
		t.Fatal(err)
	}
	bad["later_issue_numbers"] = []any{json.Number("2")}
	if _, err := NormalizeRebalanceDecisions(bad, graph, project, nil); err == nil {
		t.Fatal("duplicate band membership was accepted")
	}
	bad, err = contract.Clone(payload)
	if err != nil {
		t.Fatal(err)
	}
	bad["schema_version"] = true
	if _, err := NormalizeRebalanceDecisions(bad, graph, project, nil); err == nil {
		t.Fatal("boolean schema version was accepted")
	}
}

func TestRebalanceIgnoresProjectDraftCardsWithoutIssueNumbers(t *testing.T) {
	data := fixture(t)
	project, err := contract.Clone(nested(t, data, "project_snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	project["items"] = append(project["items"].([]any), contract.Object{"item_id": "DRAFT_ONLY", "title": "Idea card", "state": "OPEN", "field_values": contract.Object{}})
	if _, err := NormalizeRebalanceDecisions(nested(t, data, "rebalance_payload"), nested(t, data, "issue_graph"), project, nil); err != nil {
		t.Fatalf("draft project card without an issue number broke decision validation: %v", err)
	}
}

func TestRebalancePlanMinimalRanksArchiveAndQueueInventory(t *testing.T) {
	data := fixture(t)
	options := contract.Object{"queue_mode": "minimal", "rank_step": json.Number("10"), "linked_prs_by_issue": contract.Object{}}
	result, err := BuildRebalancePlan(nested(t, data, "rebalance_payload"), nested(t, data, "issue_graph"), nested(t, data, "queue_snapshot"), nested(t, data, "project_snapshot"), options, contract.Object{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	delta := result["delta"].(contract.Object)
	if got := delta["summary"].(contract.Object); got["change_count"] != 2 || got["queue_change_count"] != 2 || got["queue_change_ratio"] != 0.667 {
		t.Fatalf("unexpected summary: %#v", got)
	}
	if !reflect.DeepEqual(delta["summary"].(contract.Object)["local_compaction_bands"], []string{"Next"}) {
		t.Fatalf("compaction = %#v", delta["summary"].(contract.Object)["local_compaction_bands"])
	}
	if len(delta["archive_actions"].([]any)) != 1 || delta["archive_actions"].([]any)[0].(contract.Object)["can_archive"] != true {
		t.Fatalf("archive actions = %#v", delta["archive_actions"])
	}
	inventory := delta["queue_inventory"].(contract.Object)["items"].([]any)
	if len(inventory) != 3 || inventory[0].(contract.Object)["issue_number"] != int64(1) {
		t.Fatalf("queue inventory = %#v", inventory)
	}
}

func TestRebalancePlanReportsIneligibleArchiveAndRejectsBadOptions(t *testing.T) {
	data := fixture(t)
	payload, err := contract.Clone(nested(t, data, "rebalance_payload"))
	if err != nil {
		t.Fatal(err)
	}
	payload["now_issue_numbers"] = []any{json.Number("2")}
	payload["archive_issue_numbers"] = []any{json.Number("1"), json.Number("4")}
	options := contract.Object{"queue_mode": "normalize", "rank_step": json.Number("10")}
	result, err := BuildRebalancePlan(payload, nested(t, data, "issue_graph"), nested(t, data, "queue_snapshot"), nested(t, data, "project_snapshot"), options, contract.Object{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result["delta"].(contract.Object)["summary"].(contract.Object)["invalid_archive_count"] != 1 {
		t.Fatalf("ineligible archive was not counted: %#v", result["delta"].(contract.Object)["summary"])
	}
	options["queue_mode"] = "compact-everything"
	if _, err := BuildRebalancePlan(payload, nested(t, data, "issue_graph"), nested(t, data, "queue_snapshot"), nested(t, data, "project_snapshot"), options, contract.Object{}, nil); err == nil {
		t.Fatal("unsupported queue mode was accepted")
	}
}

func TestRunUsesDomainInputsAndRejectsUnknownCommand(t *testing.T) {
	data := fixture(t)
	result, err := Run("relationship-validate", map[string]contract.Object{"payload": nested(t, data, "relationship_payload"), "issue_graph": nested(t, data, "issue_graph")})
	if err != nil {
		t.Fatal(err)
	}
	if result["schema_version"] != int64(1) {
		t.Fatalf("unexpected result: %#v", result)
	}
	if _, err := Run("unknown", map[string]contract.Object{}); err == nil {
		t.Fatal("unknown command was accepted")
	}
}

func BenchmarkLCSForLargeQueue(b *testing.B) {
	left, right := make([]int64, 750), make([]int64, 750)
	for i := range left {
		left[i] = int64(i + 1)
		right[i] = int64(len(right) - i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result := lcs(left, right)
		if len(result) != 1 || result[0] != 1 {
			b.Fatal("queue anchor tie-breaking differs from the qualified reference", result)
		}
	}
}

func jsonNumber(value int64) string { return strconv.FormatInt(value, 10) }
