package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/planning"
)

type closeoutTestProvider struct{ *backlogTestProvider }

func closeoutTestPolicy() CloseoutPolicy {
	project := ProjectScope{Host: "github.com", Owner: "example", OwnerType: "Organization", Number: 1, ID: "PVT_backlog", Title: "Backlog"}
	labels := map[string]string{"Initiative": "initiative", "Epic": "epic", "Research": "research", "Enhancement": "enhancement", "Bug": "bug", "Maintenance": "maintenance"}
	return CloseoutPolicy{
		ReviewBacklog: ReviewBacklogPolicy{
			Project:            project,
			SeverityToPriority: map[string]string{"critical": "Now", "high": "Now", "now": "Now", "medium": "Next", "next": "Next", "low": "Later", "later": "Later"},
			IssueTypeLabels:    labels,
		},
		GovernanceCheck: contract.Object{
			"prefix_labels": contract.Object{"Epic": "epic"}, "retired_labels": []any{},
			"status_field": "Status", "valid_statuses": []any{"Todo", "In Progress", "Done"},
			"priority_field": "Priority", "valid_priorities": []any{"Now", "Next", "Later"},
		},
		BacklogAudit: contract.Object{
			"schema_version":    int64(1),
			"scope":             contract.Object{"repo": testRepo().Object(), "project": contract.Object{"id": project.ID, "number": project.Number, "title": project.Title}},
			"taxonomy":          contract.Object{"issue_type_labels": contract.Object{"Epic": "epic"}},
			"excluded_prefixes": []any{}, "status_field": "Status", "valid_statuses": []any{"Todo", "In Progress", "Done"}, "done_statuses": []any{"Done"},
			"priority_field": "Priority", "priorities": contract.Object{"Now": "Now", "Next": "Next", "Later": "Later"}, "order_field": "Queue Order",
		},
	}
}

func closeoutTestInventory() contract.Object {
	inv := backlogTestInventory(true)
	graph, _ := contract.ObjectAt(inv, "issue_inventory")
	issues, _ := contract.Objects(graph, "issues")
	issues[0]["title"], issues[0]["labels"] = "Epic: Closeout", []any{"epic"}
	projectRows, _ := contract.Objects(inv, "projects")
	project := projectRows[0]
	project["items"] = []any{contract.Object{
		"item_id": "PVTI_backlog_1", "number": int64(1),
		"field_values": contract.Object{"Status": "Todo", "Priority": "Later", "Queue Order": float64(1024)}, "archived": false,
	}}
	inv["labels"] = append(inv["labels"].([]any), contract.Object{"id": int64(15), "node_id": "LA_epic", "name": "epic", "color": "abcdef", "description": "Epic"})
	return inv
}

func newCloseoutTestProvider(stateFile string) *closeoutTestProvider {
	base := &backlogTestProvider{stateFile: stateFile, state: contract.Object{"inventory": closeoutTestInventory(), "reads": int64(0), "writes": int64(0), "fail_first_write": false}}
	return &closeoutTestProvider{backlogTestProvider: base}
}

func (f *closeoutTestProvider) UpdateIssueComment(_ context.Context, _ string, number, commentID int64, body string) (contract.Object, error) {
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		comments, _ := contract.Objects(inventory, "comments")
		for _, comment := range comments {
			id, _ := contract.PositiveInteger(comment["id"])
			if id != commentID || comment["issue_number"] != number {
				continue
			}
			comment["body"] = body
			url := "https://github.com/example/widgets/issues/" + strconv.FormatInt(number, 10) + "#issuecomment-" + strconv.FormatInt(commentID, 10)
			comment["url"] = url
			return contract.Object{"id": commentID, "body": body, "html_url": url}, nil
		}
		return nil, errors.New("comment to update is absent")
	})
}

func (f *closeoutTestProvider) CreateIssueComment(_ context.Context, nonce string, number int64, body string) (contract.Object, error) {
	marker, err := native.OperationMarker(nonce)
	if err != nil {
		return nil, err
	}
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		comments, _ := contract.Objects(inventory, "comments")
		id := int64(501)
		for _, row := range comments {
			old, parseErr := contract.PositiveInteger(row["id"])
			if parseErr == nil && old >= id {
				id = old + 1
			}
		}
		fullBody := body + "\n\n" + marker
		url := "https://github.com/example/widgets/issues/" + strconv.FormatInt(number, 10) + "#issuecomment-" + strconv.FormatInt(id, 10)
		comments = append(comments, contract.Object{"id": id, "issue_number": number, "body": fullBody, "url": url})
		inventory["comments"] = backlogObjectsAsAny(comments)
		return contract.Object{"id": id, "body": fullBody, "html_url": url}, nil
	})
}

func closeoutTestSummary(t *testing.T, provider *closeoutTestProvider, policy CloseoutPolicy, withBacklog bool) contract.Object {
	t.Helper()
	rows := []any{}
	if withBacklog {
		rows = append(rows, contract.Object{"id": "followup", "title": "Record closeout rationale", "issue_type": "Epic", "severity": "low", "summary": "Record the reviewed closeout rationale.", "blocking": false, "destination": "backlog", "existing_issue": int64(1)})
	}
	normalized, err := planning.NormalizeReviewCloseoutFindings(contract.Object{"schema_version": int64(1), "mode": "epic-closeout", "scope": "Release readiness", "epic_issue_number": int64(1), "findings": rows})
	if err != nil {
		t.Fatal(err)
	}
	baseRequest := BacklogInventoryRequest{Projects: []ProjectScope{policy.ReviewBacklog.Project}, IncludeRelationships: true}
	baseRequest.CommentMarkers = []string{closeoutAuditMarker}
	raw, err := provider.BacklogInventory(context.Background(), baseRequest)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := normalizeBacklogInventory(raw, testRepo(), baseRequest)
	if err != nil {
		t.Fatal(err)
	}
	delta := contract.Object{"repo": testRepo().Object(), "schema_version": int64(1), "scope": normalized["scope"], "proposal_count": int64(0), "proposals": []any{}}
	backlogFindingCount := int64(0)
	if withBacklog {
		delta, err = closeoutBacklogDelta(contract.Object{}, normalized, policy.ReviewBacklog, inventory, testRepo())
		if err != nil {
			t.Fatal(err)
		}
		backlogFindingCount = 1
	}
	epicState := contract.Object{"issue_number": int64(1), "title": "Epic: Closeout", "open_child_numbers": []any{}, "blocked_by_numbers": []any{}, "derived_findings": []any{}}
	built, err := planning.BuildReviewCloseoutAudit(contract.Object{
		"repo": testRepo().Object(), "normalized_input": normalized, "generated_at": "2026-10-04T12:00:00Z",
		"backlog_delta": delta, "backlog_finding_count": backlogFindingCount, "blocking_reasons": []any{}, "epic_state": epicState,
	})
	if err != nil {
		t.Fatal(err)
	}
	summary, _ := contract.ObjectAt(built, "summary")
	summary, err = closeoutLiveSummary(summary, policy, inventory, testRepo())
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

func closeoutTestEngine(root string, provider BacklogProvider) apply.Engine {
	return apply.Engine{Root: root, Repository: testRepo(), Command: ReviewCloseoutCommand, Adapter: Closeout{Provider: provider}}
}

func resetCloseoutTestProvider(t *testing.T, provider *closeoutTestProvider) {
	t.Helper()
	state, err := provider.load()
	if err != nil {
		t.Fatal(err)
	}
	state["reads"], state["writes"] = int64(0), int64(0)
	if err = provider.save(state); err != nil {
		t.Fatal(err)
	}
}

func TestCloseoutReplaysTypedNestedAndLateBoundCommentWithoutDuplicateWrites(t *testing.T) {
	provider := newCloseoutTestProvider("")
	policy := closeoutTestPolicy()
	summary := closeoutTestSummary(t, provider, policy, true)
	plan, err := PrepareReviewCloseout(context.Background(), provider, testRepo(), summary, policy, time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Operations) != 2 || plan.Operations[0].Kind != "issue-comment-upsert" || plan.Operations[1].ID != "closeout:comment" {
		t.Fatalf("closeout action set = %#v", plan.Operations)
	}
	resetCloseoutTestProvider(t, provider)
	root := t.TempDir()
	result, err := closeoutTestEngine(root, provider).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatalf("closeout apply = %#v, %v", result, err)
	}
	state, _ := provider.load()
	writes, _ := contract.Integer(state["writes"])
	comments, _ := contract.Objects(backlogStateInventory(state), "comments")
	if writes != 2 || len(comments) != 2 {
		t.Fatalf("writes/comments = %d/%d, want nested rationale plus closeout comment", writes, len(comments))
	}
	var closeoutBody string
	for _, comment := range comments {
		if strings.Contains(comment["body"].(string), closeoutAuditMarker) {
			closeoutBody = comment["body"].(string)
		}
	}
	if !strings.Contains(closeoutBody, "- backlog follow-ups applied: 1") || !strings.Contains(closeoutBody, "- closeout ready: yes") {
		t.Fatalf("late-bound closeout body = %q", closeoutBody)
	}
	if _, err = closeoutTestEngine(root, provider).Apply(context.Background(), plan.Object()); err != nil {
		t.Fatal(err)
	}
	state, _ = provider.load()
	if writes, _ = contract.Integer(state["writes"]); writes != 2 {
		t.Fatalf("terminal replay repeated nested or closeout write: %d", writes)
	}
}

func TestCloseoutRejectsStaleSummaryMalformedPolicyAndInventoryDrift(t *testing.T) {
	provider := newCloseoutTestProvider("")
	policy := closeoutTestPolicy()
	summary := closeoutTestSummary(t, provider, policy, false)
	plan, err := PrepareReviewCloseout(context.Background(), provider, testRepo(), summary, policy, time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	badPlan, _ := contract.ParsePlan(plan.Object())
	badPlan.Data["summary"].(map[string]any)["closeout_ready"] = false
	badPlan.SHA256, _ = contract.Digest(badPlan.Unsigned())
	if _, err := closeoutOperations(badPlan); err == nil {
		t.Fatal("accepted a hash-consistent but stale live closeout verdict")
	}
	badPolicy := policy.Object()
	delete(badPolicy, "backlog_audit")
	if _, err := ParseCloseoutPolicy(badPolicy, testRepo()); err == nil {
		t.Fatal("accepted a missing audit policy field")
	}
	resetCloseoutTestProvider(t, provider)
	provider.driftRead = 1
	if _, err := closeoutTestEngine(t.TempDir(), provider).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("closeout dispatched across complete source drift: %v", err)
	}
	state, _ := provider.load()
	if writes, _ := contract.Integer(state["writes"]); writes != 0 {
		t.Fatalf("drifted closeout inventory allowed %d writes", writes)
	}
}

func TestCloseoutUnknownWithoutDurableNativeAckDoesNotReplay(t *testing.T) {
	provider := newCloseoutTestProvider("")
	policy := closeoutTestPolicy()
	summary := closeoutTestSummary(t, provider, policy, false)
	plan, err := PrepareReviewCloseout(context.Background(), provider, testRepo(), summary, policy, time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	resetCloseoutTestProvider(t, provider)
	state, _ := provider.load()
	state["fail_first_write"] = true
	_ = provider.save(state)
	root := t.TempDir()
	if _, err = closeoutTestEngine(root, provider).Apply(context.Background(), plan.Object()); err == nil {
		t.Fatal("lost final comment ACK was reported complete")
	}
	if _, err = closeoutTestEngine(root, provider).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "no write was retried") {
		t.Fatalf("unknown final comment without ACK was not blocked: %v", err)
	}
	state, _ = provider.load()
	if writes, _ := contract.Integer(state["writes"]); writes != 1 {
		t.Fatalf("unknown final comment was replayed: %d writes", writes)
	}
}

func TestCloseoutFreshProcessVerifiesCapturedFinalAckAfterNestedCompletion(t *testing.T) {
	if os.Getenv("STEWARD_CLOSEOUT_HELPER") != "" {
		statePath, planPath, root := os.Getenv("STEWARD_CLOSEOUT_STATE"), os.Getenv("STEWARD_CLOSEOUT_PLAN"), os.Getenv("STEWARD_CLOSEOUT_ROOT")
		crashRead, _ := strconv.Atoi(os.Getenv("STEWARD_CLOSEOUT_CRASH_READ"))
		provider := newCloseoutTestProvider(statePath)
		provider.crashRead = crashRead
		file, err := os.Open(planPath)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := contract.Decode(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = closeoutTestEngine(root, provider).Apply(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
		return
	}
	provider := newCloseoutTestProvider("")
	policy := closeoutTestPolicy()
	summary := closeoutTestSummary(t, provider, policy, true)
	plan, err := PrepareReviewCloseout(context.Background(), provider, testRepo(), summary, policy, time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	statePath, planPath := filepath.Join(root, "provider.json"), filepath.Join(root, "plan.json")
	resetCloseoutTestProvider(t, provider)
	provider.stateFile = statePath
	if err = provider.save(provider.state); err != nil {
		t.Fatal(err)
	}
	encoded, err := contract.Canonical(plan.Object())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(planPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(crashRead string) error {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCloseoutFreshProcessVerifiesCapturedFinalAckAfterNestedCompletion$")
		cmd.Env = append(os.Environ(), "STEWARD_CLOSEOUT_HELPER=1", "STEWARD_CLOSEOUT_STATE="+statePath, "STEWARD_CLOSEOUT_PLAN="+planPath, "STEWARD_CLOSEOUT_ROOT="+root, "STEWARD_CLOSEOUT_CRASH_READ="+crashRead)
		return cmd.Run()
	}
	if err = invoke("4"); err == nil {
		t.Fatal("writer process did not crash after final ACK persistence")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 73 {
		t.Fatalf("writer process failed before the intended ACK crash: %v", err)
	}
	if err = invoke("0"); err != nil {
		t.Fatalf("fresh process could not finish captured final ACK: %v", err)
	}
	stateFile, err := os.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := contract.Decode(stateFile)
	_ = stateFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	if writes, _ := contract.Integer(state["writes"]); writes != 2 {
		t.Fatalf("fresh process replayed completed nested/final writes: %d", writes)
	}
	if err = invoke("0"); err != nil {
		t.Fatalf("terminal replay failed: %v", err)
	}
	stateFile, _ = os.Open(statePath)
	state, _ = contract.Decode(stateFile)
	_ = stateFile.Close()
	if writes, _ := contract.Integer(state["writes"]); writes != 2 {
		t.Fatalf("terminal replay dispatched additional writes: %d", writes)
	}
}
