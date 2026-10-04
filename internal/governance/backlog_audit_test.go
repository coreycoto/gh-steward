package governance

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func backlogAuditFixture() (contract.Object, contract.Object) {
	repo := contract.Object{"nameWithOwner": "example-org/product", "url": "https://github.com/example-org/product"}
	project := contract.Object{"id": "PVT_123", "number": int64(17), "title": "Product Backlog"}
	policy := contract.Object{
		"schema_version":    int64(1),
		"scope":             contract.Object{"repo": repo, "project": project},
		"taxonomy":          contract.Object{"issue_type_labels": contract.Object{"Task": "task", "Bug": "bug", "Initiative": "initiative"}},
		"excluded_prefixes": []any{"Initiative"},
		"status_field":      "Workflow",
		"valid_statuses":    []any{"Backlog", "Active", "Complete"},
		"done_statuses":     []any{"Complete"},
		"priority_field":    "Rank",
		"priorities":        contract.Object{"Urgent": "Now", "Soon": "Next", "Later": "Later"},
		"order_field":       "Queue Order",
	}
	issue := func(number int64, title, state string, inProject bool, fields contract.Object, archived bool, blocked, children []any) contract.Object {
		projectItem := any(nil)
		if inProject {
			projectItem = contract.Object{"item_id": "PVTI_" + strconv.FormatInt(number, 10), "archived": archived, "field_values": fields}
		}
		return contract.Object{
			"number": number, "title": title, "state": state, "in_project": inProject,
			"field_values": fields, "project_item": projectItem,
			"blocked_by_numbers": blocked, "child_numbers": children,
		}
	}
	issues := []any{
		issue(1, "Task: parent", "OPEN", true, contract.Object{"Workflow": "Backlog", "Rank": "Urgent", "Queue Order": json.Number("0.5")}, false, []any{int64(2), int64(9)}, []any{int64(3), int64(9)}),
		issue(2, "Task: blocker", "OPEN", true, contract.Object{"Workflow": "Backlog", "Rank": "Soon", "Queue Order": json.Number("1")}, false, []any{}, []any{}),
		issue(3, "Task: child", "OPEN", true, contract.Object{"Workflow": "Active", "Rank": "Soon", "Queue Order": json.Number("2")}, false, []any{}, []any{}),
		issue(4, "Bug: duplicate rank", "OPEN", true, contract.Object{"Workflow": "Backlog", "Rank": "Urgent", "Queue Order": json.Number("0.5")}, false, []any{}, []any{}),
		issue(5, "Task: invalid values", "OPEN", true, contract.Object{"Workflow": "Paused", "Rank": "Critical", "Queue Order": "2"}, false, []any{}, []any{}),
		issue(6, "Initiative: excluded", "OPEN", true, contract.Object{"Workflow": "Unknown", "Rank": "Critical"}, false, []any{}, []any{}),
		issue(7, "Task: archived", "OPEN", true, contract.Object{"Workflow": "Unknown", "Rank": "Critical"}, true, []any{}, []any{}),
		issue(8, "Bug: missing membership", "OPEN", false, contract.Object{}, false, []any{}, []any{}),
		issue(9, "Task: closed issue", "CLOSED", false, contract.Object{}, false, []any{}, []any{}),
		issue(10, "Task: complete", "OPEN", true, contract.Object{"Workflow": "Complete", "Rank": "Soon", "Queue Order": json.Number("4.5")}, false, []any{}, []any{}),
		issue(11, "Task: zero rank", "OPEN", true, contract.Object{"Workflow": "Backlog", "Rank": "Soon", "Queue Order": json.Number("0")}, false, []any{}, []any{}),
		issue(12, "Task: missing rank", "OPEN", true, contract.Object{"Workflow": "Active", "Rank": "Later"}, false, []any{}, []any{}),
	}
	graph := contract.Object{
		"repo": repo, "project": project, "generated_at": "2026-10-04T17:00:00Z",
		"issues": issues,
	}
	return policy, graph
}

func TestBacklogAuditUsesExplicitConsumerPolicyAndPreservesQueueFindings(t *testing.T) {
	policy, graph := backlogAuditFixture()
	result, err := BacklogAudit(policy, graph, nil)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := contract.ObjectAt(result, "summary")
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int{"project_item_count": 9, "queue_item_count": 8, "excluded_item_count": 1, "open_issue_count": 11, "fractional_queue_order_count": 3} {
		if got := summary[key]; got != want {
			t.Errorf("summary.%s = %v, want %d", key, got, want)
		}
	}
	gaps, err := contract.ObjectAt(summary, "minimum_gap_by_priority")
	if err != nil {
		t.Fatal(err)
	}
	if gaps["Urgent"] != float64(0) || gaps["Soon"] != float64(1) || gaps["Later"] != nil {
		t.Fatalf("unexpected minimum gaps: %#v", gaps)
	}
	for _, expected := range []struct {
		code   string
		number int64
	}{{"blocked-now-item", 1}, {"parent-above-actionable-child", 1}, {"duplicate-queue-order", 1}, {"duplicate-queue-order", 4}, {"legacy-fractional-queue-order", 1}, {"legacy-fractional-queue-order", 4}, {"invalid-status", 5}, {"invalid-priority", 5}, {"invalid-queue-order", 5}, {"invalid-status", 6}, {"invalid-priority", 6}, {"missing-queue-order", 12}, {"missing-project-membership", 8}, {"done-open-issue", 10}} {
		if !findingExists(t, result, expected.code, expected.number) {
			t.Errorf("missing finding %s for issue #%d", expected.code, expected.number)
		}
	}
	findings, err := contract.Objects(result, "findings")
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding["code"] == "blocked-now-item" && finding["number"] == int64(1) {
			details, _ := contract.ObjectAt(finding, "details")
			blocked, _ := contract.Array(details, "blocked_by")
			if len(blocked) != 1 || blocked[0] != "2" {
				t.Fatalf("closed blocker should not appear in blocked-now evidence: %#v", finding)
			}
		}
		if finding["code"] == "parent-above-actionable-child" && finding["number"] == int64(1) {
			details, _ := contract.ObjectAt(finding, "details")
			children, _ := contract.Array(details, "actionable_children")
			if len(children) != 1 || children[0] != int64(3) {
				t.Fatalf("closed child should not appear in actionable-child evidence: %#v", finding)
			}
		}
	}
	for _, absent := range []struct {
		code   string
		number int64
	}{{"missing-project-membership", 9}, {"invalid-status", 7}, {"invalid-priority", 7}, {"missing-queue-order", 6}, {"missing-queue-order", 11}} {
		if findingExists(t, result, absent.code, absent.number) {
			t.Errorf("unexpected finding %s for issue #%d", absent.code, absent.number)
		}
	}
}

func TestBacklogAuditValidatesOptionalQueueSnapshotAgainstCapturedGraph(t *testing.T) {
	policy, graph := backlogAuditFixture()
	queue := contract.Object{
		"repo": graph["repo"], "project": graph["project"],
		"items": []any{
			contract.Object{"number": int64(1)}, contract.Object{"number": int64(2)}, contract.Object{"number": int64(3)},
			contract.Object{"number": int64(4)}, contract.Object{"number": int64(5)}, contract.Object{"number": int64(10)},
			contract.Object{"number": int64(11)}, contract.Object{"number": int64(12)},
		},
		"excluded_items": []any{contract.Object{"number": int64(6)}},
		"item_count":     int64(8), "excluded_item_count": int64(1),
	}
	if _, err := BacklogAudit(policy, graph, queue); err != nil {
		t.Fatalf("matching optional queue snapshot rejected: %v", err)
	}

	changed := clone(t, queue)
	changed["repo"] = contract.Object{"nameWithOwner": "other/project", "url": "https://github.com/other/project"}
	if _, err := BacklogAudit(policy, graph, changed); err == nil {
		t.Fatal("queue snapshot from another repository was accepted")
	}
	changed = clone(t, queue)
	changed["items"] = changed["items"].([]any)[:5]
	if _, err := BacklogAudit(policy, graph, changed); err == nil {
		t.Fatal("incomplete optional queue snapshot was accepted")
	}
}

func TestBacklogAuditRejectsAmbiguousScopeAndMalformedPolicy(t *testing.T) {
	policy, graph := backlogAuditFixture()
	changedPolicy := clone(t, policy)
	changedPolicy["schema_version"] = true
	if _, err := BacklogAudit(changedPolicy, graph, nil); err == nil {
		t.Fatal("boolean policy schema version was accepted")
	}
	changedPolicy = clone(t, policy)
	delete(changedPolicy, "excluded_prefixes")
	if _, err := BacklogAudit(changedPolicy, graph, nil); err == nil {
		t.Fatal("missing exclusion policy was accepted")
	}
	changedPolicy = clone(t, policy)
	changedPolicy["priorities"] = contract.Object{"Urgent": "Now", "Soon": "Now", "Later": "Later"}
	if _, err := BacklogAudit(changedPolicy, graph, nil); err == nil {
		t.Fatal("ambiguous priority mapping was accepted")
	}
	changedGraph := clone(t, graph)
	changedGraph["project"].(map[string]any)["id"] = "PVT_other"
	if _, err := BacklogAudit(policy, changedGraph, nil); err == nil {
		t.Fatal("issue graph joined to another Project was accepted")
	}
}

func TestBacklogAuditRejectsMalformedReferencesBeforeReporting(t *testing.T) {
	policy, graph := backlogAuditFixture()
	issues := graph["issues"].([]any)
	issues[0].(map[string]any)["blocked_by_numbers"] = []any{true}
	if _, err := BacklogAudit(policy, graph, nil); err == nil {
		t.Fatal("boolean issue reference was accepted")
	}
}
