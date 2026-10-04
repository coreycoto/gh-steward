package snapshot

import (
	"context"
	"strings"
	"testing"
)

func TestPagedIssueSourceKeepsImmutableRepositoryIdentity(t *testing.T) {
	first := issuePage(issue(1))
	first["issues"].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "second"}
	second := issuePage(issue(2))
	second["id"] = "R_recreated"
	f := &fakeReader{issuePages: []map[string]any{first, second}}
	if _, err := service(f).Issues(context.Background(), "all", false); err == nil || !strings.Contains(err.Error(), "immutable node identity") {
		t.Fatal("mixed repository incarnations accepted", err)
	}
}

func TestQueueUnknownPriorityAndClosedActionabilityRemainExplicit(t *testing.T) {
	policy, err := ParseQueuePolicy(map[string]any{"status_field": "Progress", "priority_field": "Band", "order_field": "Rank", "done_statuses": []any{"Complete"}, "priorities": map[string]any{"Immediate": "Now", "Upcoming": "Next", "Someday": "Later"}, "excluded_prefixes": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	i := issue(1)
	i["in_project"] = true
	i["field_values"] = map[string]any{"Progress": "Working", "Band": "Unreviewed"}
	i["child_numbers"] = []any{}
	i["blocked_by_numbers"] = []any{}
	graph := map[string]any{"repo": repo().Object(), "issues": []any{i}, "provenance": map[string]any{"live": true}}
	if _, err := service(&fakeReader{}).Queue(graph, policy); err == nil {
		t.Fatal("unknown live option silently normalized")
	}
	i["field_values"].(map[string]any)["Band"] = "Immediate"
	i["state"] = "CLOSED"
	got, err := service(&fakeReader{}).Queue(graph, policy)
	if err != nil {
		t.Fatal(err)
	}
	row := got["items"].([]any)[0].(map[string]any)
	if row["is_actionable"] != false {
		t.Fatal("closed issue marked actionable", row)
	}
}
