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
)

type governanceFixtureProvider struct{ *backlogTestProvider }

func governancePalettePolicy() contract.Object {
	return contract.Object{"labels": []any{
		contract.Object{"name": "managed-new", "semantic_role": "new work", "platform_owner": "repo-managed", "governance_controlled": true, "target_color": "A1B2C3", "current_reference_color": "A1B2C3", "description": "New managed label", "notes": ""},
		contract.Object{"name": "managed-stale", "semantic_role": "stale work", "platform_owner": "repo-managed", "governance_controlled": false, "target_color": "0A0B0C", "current_reference_color": "0A0B0C", "description": "Updated managed label", "notes": ""},
	}}
}

func governanceFixtureInventory() contract.Object {
	inventory := backlogTestInventory(false)
	inventory["labels"] = []any{contract.Object{"id": int64(77), "node_id": "LA_stale", "name": "managed-stale", "color": "111111", "description": "Old description"}}
	return inventory
}

func newGovernanceFixtureProvider(stateFile string) *governanceFixtureProvider {
	state := contract.Object{"inventory": governanceFixtureInventory(), "reads": int64(0), "writes": int64(0), "fail_first_write": false}
	base := &backlogTestProvider{state: state, stateFile: stateFile}
	return &governanceFixtureProvider{backlogTestProvider: base}
}

func (f *governanceFixtureProvider) CreateLabel(_ context.Context, nonce string, draft LabelDraft) (contract.Object, error) {
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		labels, _ := contract.Objects(inventory, "labels")
		for _, row := range labels {
			if row["name"] == draft.Name {
				return nil, errors.New("duplicate label")
			}
		}
		nodeID := "LA_" + strings.ReplaceAll(draft.Name, "-", "_")
		row := contract.Object{"id": int64(88), "node_id": nodeID, "name": draft.Name, "color": strings.ToLower(draft.Color), "description": draft.Description}
		labels = append(labels, row)
		inventory["labels"] = backlogObjectsAsAny(labels)
		return governanceLabelAck(nonce, nodeID, draft.Name, draft.Color, draft.Description), nil
	})
}

func (f *governanceFixtureProvider) UpdateLabel(_ context.Context, nonce, nodeID, name, color, description string) (contract.Object, error) {
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		labels, _ := contract.Objects(inventory, "labels")
		found := false
		for _, row := range labels {
			if row["node_id"] == nodeID && row["name"] == name {
				row["color"], row["description"] = strings.ToLower(color), description
				found = true
			}
		}
		if !found {
			return nil, errors.New("label identity changed")
		}
		inventory["labels"] = backlogObjectsAsAny(labels)
		return governanceLabelAck(nonce, nodeID, name, color, description), nil
	})
}

func governanceLabelAck(nonce, nodeID, name, color, description string) contract.Object {
	return contract.Object{"clientMutationId": nonce, "label": contract.Object{"id": nodeID, "name": name, "color": color, "description": description, "repository": contract.Object{"id": "R_widgets", "nameWithOwner": "example/widgets", "url": "https://github.com/example/widgets"}}}
}

func (f *governanceFixtureProvider) SetIssueState(context.Context, string, int64, string) (contract.Object, error) {
	return nil, errors.New("unexpected issue state write in label fixture")
}

func (f *governanceFixtureProvider) CreateIssueComment(_ context.Context, nonce string, number int64, body string) (contract.Object, error) {
	marker, err := native.OperationMarker(nonce)
	if err != nil {
		return nil, err
	}
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		comments, _ := contract.Objects(inventory, "comments")
		id := int64(501)
		for _, row := range comments {
			if old, parseErr := contract.PositiveInteger(row["id"]); parseErr == nil && old >= id {
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

func governanceEngine(root string, provider GovernanceProvider) apply.Engine {
	return apply.Engine{Root: root, Repository: testRepo(), Command: GovernanceCommand, Adapter: Governance{Provider: provider}}
}

func prepareGovernancePalette(t *testing.T, provider GovernanceProvider) contract.Plan {
	t.Helper()
	plan, err := PrepareGovernance(context.Background(), provider, testRepo(), GovernanceLabelPalette, governancePalettePolicy(), time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func governanceDriftPolicy() contract.Object {
	return contract.Object{
		"title": "Governance drift", "body": "## Drift\n\nLive governance needs review.",
		"maintenance_label": "maintenance", "summary_markdown": "The current policy has unresolved drift.",
		"unresolved_manual_drift": true, "close_when_clean": false, "project": nil,
		"status_field": "Status", "todo_status": "Todo", "done_status": "Done",
		"priority_field": "Priority", "priority_value": "Now", "queue_order_field": "Queue Order", "queue_order_step": 1024.0,
	}
}

func prepareGovernanceDrift(t *testing.T, provider GovernanceProvider, captured time.Time) contract.Plan {
	t.Helper()
	plan, err := PrepareGovernance(context.Background(), provider, testRepo(), GovernanceDriftIssue, governanceDriftPolicy(), captured)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func resetGovernanceFixture(t *testing.T, provider *governanceFixtureProvider) {
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

func TestGovernanceLabelPaletteReplaysFromTypedACKWithoutDuplicateWrites(t *testing.T) {
	provider := newGovernanceFixtureProvider("")
	plan := prepareGovernancePalette(t, provider)
	if len(plan.Operations) != 2 || plan.Operations[0].Kind != "label-create" || plan.Operations[1].Kind != "label-update" {
		t.Fatalf("palette operations = %#v", plan.Operations)
	}
	rows, _ := contract.Objects(plan.Data["inventory"].(map[string]any), "labels")
	if _, exists := rows[0]["id"]; exists {
		t.Fatalf("governance plan retained REST database ID: %#v", rows[0])
	}
	if rows[0]["node_id"] != "LA_stale" {
		t.Fatalf("governance label identity = %#v", rows[0])
	}
	resetGovernanceFixture(t, provider)
	root := t.TempDir()
	result, err := governanceEngine(root, provider).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatalf("apply = %#v, %v", result, err)
	}
	state, _ := provider.load()
	writes, _ := contract.Integer(state["writes"])
	if writes != 2 {
		t.Fatalf("writes=%d, want two typed label mutations", writes)
	}
	inventory := backlogStateInventory(state)
	labels, _ := contract.Objects(inventory, "labels")
	if len(labels) != 2 {
		t.Fatalf("labels after apply = %#v", labels)
	}
	if _, err = governanceEngine(root, provider).Apply(context.Background(), plan.Object()); err != nil {
		t.Fatal(err)
	}
	state, _ = provider.load()
	writes, _ = contract.Integer(state["writes"])
	if writes != 2 {
		t.Fatalf("terminal replay dispatched completed labels: %d writes", writes)
	}
}

func TestGovernancePlanRejectsPolicyTamperingAndCompleteInventoryDrift(t *testing.T) {
	provider := newGovernanceFixtureProvider("")
	plan := prepareGovernancePalette(t, provider)
	plan.Data["policy"].(map[string]any)["labels"].([]any)[0].(map[string]any)["target_color"] = "FFFFFF"
	plan.SHA256, _ = contract.Digest(plan.Unsigned())
	if _, err := governanceEngine(t.TempDir(), provider).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "differ from domain intent") {
		t.Fatalf("hash-consistent altered governance policy was accepted: %v", err)
	}
	state, _ := provider.load()
	writes, _ := contract.Integer(state["writes"])
	if writes != 0 {
		t.Fatalf("tampered policy dispatched %d writes", writes)
	}

	provider = newGovernanceFixtureProvider("")
	plan = prepareGovernancePalette(t, provider)
	resetGovernanceFixture(t, provider)
	provider.driftRead = 1
	if _, err := governanceEngine(t.TempDir(), provider).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("complete source drift was accepted: %v", err)
	}
	state, _ = provider.load()
	writes, _ = contract.Integer(state["writes"])
	if writes != 0 {
		t.Fatalf("source drift allowed %d writes", writes)
	}
}

func TestGovernanceDriftAuditMarkersArePerEvaluationAndReplayIsIdempotent(t *testing.T) {
	provider := newGovernanceFixtureProvider("")
	inventory := backlogStateInventory(provider.state)
	graph, _ := contract.ObjectAt(inventory, "issue_inventory")
	issues, _ := contract.Objects(graph, "issues")
	issues[0]["title"] = "Governance drift"
	inventory["labels"] = append(inventory["labels"].([]any), contract.Object{"id": int64(89), "node_id": "LA_maintenance", "name": "maintenance", "color": "c0ffee", "description": "Maintenance"})

	firstTime := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	first := prepareGovernanceDrift(t, provider, firstTime)
	firstRequest, _ := contract.ObjectAt(first.Data, "inventory_request")
	firstMarkers, _ := contract.Strings(firstRequest["comment_markers"])
	if len(firstMarkers) != 1 || !strings.HasPrefix(firstMarkers[0], governanceAuditMarkerPrefix) {
		t.Fatalf("first evaluation marker = %#v", firstMarkers)
	}
	root := t.TempDir()
	result, err := governanceEngine(root, provider).Apply(context.Background(), first.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatalf("first apply = %#v, %v", result, err)
	}
	firstState, _ := provider.load()
	firstComments, _ := contract.Objects(backlogStateInventory(firstState), "comments")
	if len(firstComments) != 1 || !strings.Contains(firstComments[0]["body"].(string), firstMarkers[0]) {
		t.Fatalf("first audit comment not captured: %#v", firstComments)
	}
	firstWrites, _ := contract.Integer(firstState["writes"])
	if _, err = governanceEngine(root, provider).Apply(context.Background(), first.Object()); err != nil {
		t.Fatal(err)
	}
	afterReplay, _ := provider.load()
	if writes, _ := contract.Integer(afterReplay["writes"]); writes != firstWrites {
		t.Fatalf("same-plan replay dispatched another write: before=%d after=%d", firstWrites, writes)
	}

	secondTime := firstTime.Add(24 * time.Hour)
	second := prepareGovernanceDrift(t, provider, secondTime)
	secondRequest, _ := contract.ObjectAt(second.Data, "inventory_request")
	secondMarkers, _ := contract.Strings(secondRequest["comment_markers"])
	if len(secondMarkers) != 1 || secondMarkers[0] == firstMarkers[0] {
		t.Fatalf("later evaluation reused historical marker: first=%#v second=%#v", firstMarkers, secondMarkers)
	}
	result, err = governanceEngine(root, provider).Apply(context.Background(), second.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatalf("second evaluation apply = %#v, %v", result, err)
	}
	secondState, _ := provider.load()
	secondComments, _ := contract.Objects(backlogStateInventory(secondState), "comments")
	if len(secondComments) != 2 {
		t.Fatalf("later evaluation did not append a history comment: %#v", secondComments)
	}
	seen := map[string]bool{}
	for _, comment := range secondComments {
		for _, marker := range []string{firstMarkers[0], secondMarkers[0]} {
			if strings.Contains(comment["body"].(string), marker) {
				seen[marker] = true
			}
		}
	}
	if len(seen) != 2 {
		t.Fatalf("audit history lost one evaluation marker: %#v", secondComments)
	}
	secondWrites, _ := contract.Integer(secondState["writes"])
	if _, err = governanceEngine(root, provider).Apply(context.Background(), second.Object()); err != nil {
		t.Fatal(err)
	}
	finalState, _ := provider.load()
	if writes, _ := contract.Integer(finalState["writes"]); writes != secondWrites {
		t.Fatalf("second-plan replay dispatched another write: before=%d after=%d", secondWrites, writes)
	}
}

type executionStateFixtureProvider struct{ *backlogTestProvider }

func (*executionStateFixtureProvider) CreateLabel(context.Context, string, LabelDraft) (contract.Object, error) {
	return nil, errors.New("unexpected label creation in execution-state fixture")
}

func (*executionStateFixtureProvider) UpdateLabel(context.Context, string, string, string, string, string) (contract.Object, error) {
	return nil, errors.New("unexpected label update in execution-state fixture")
}

func newExecutionStateFixtureProvider() *executionStateFixtureProvider {
	inventory := backlogTestInventory(true)
	graph, _ := contract.ObjectAt(inventory, "issue_inventory")
	issues, _ := contract.Objects(graph, "issues")
	issues[0]["state"] = "CLOSED"
	return &executionStateFixtureProvider{backlogTestProvider: &backlogTestProvider{state: contract.Object{"inventory": inventory, "reads": int64(0), "writes": int64(0), "fail_first_write": false}}}
}

func (f *executionStateFixtureProvider) SetIssueState(_ context.Context, _ string, number int64, state string) (contract.Object, error) {
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		graph, _ := contract.ObjectAt(inventory, "issue_inventory")
		issues, _ := contract.Objects(graph, "issues")
		for _, issue := range issues {
			if got, _ := contract.PositiveInteger(issue["number"]); got == number {
				issue["state"] = state
				return contract.Object{"number": number, "node_id": issue["id"], "html_url": issue["url"], "title": issue["title"], "body": issue["body"], "state": strings.ToLower(state), "labels": []any{}, "milestone": nil}, nil
			}
		}
		return nil, errors.New("execution-state fixture issue is absent")
	})
}

func (f *executionStateFixtureProvider) AddProjectIssue(_ context.Context, nonce, projectID, issueNodeID string) (contract.Object, error) {
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		projects, _ := contract.Objects(inventory, "projects")
		project := projects[0]
		metadata, _ := contract.ObjectAt(project, "project")
		if metadata["id"] != projectID {
			return nil, errors.New("execution-state fixture Project identity changed")
		}
		graph, _ := contract.ObjectAt(inventory, "issue_inventory")
		issue := backlogIssueByNumber(mustObjects(graph, "issues"), 1)
		if issue == nil || issue["id"] != issueNodeID {
			return nil, errors.New("execution-state fixture issue identity changed")
		}
		items, _ := contract.Objects(project, "items")
		items = append(items, contract.Object{"item_id": "PVTI_execution_1", "number": int64(1), "field_values": contract.Object{}, "archived": false})
		project["items"] = backlogObjectsAsAny(items)
		return contract.Object{"clientMutationId": nonce, "id": "PVTI_execution_1", "isArchived": false, "content": contract.Object{"id": issue["id"], "number": int64(1), "url": issue["url"]}, "fieldValues": contract.Object{"nodes": []any{}, "pageInfo": contract.Object{"hasNextPage": false, "endCursor": nil}}}, nil
	})
}

func (f *executionStateFixtureProvider) SetProjectField(_ context.Context, nonce, projectID, itemID string, field ProjectField, value ProjectFieldValue) (contract.Object, error) {
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		projects, _ := contract.Objects(inventory, "projects")
		project := projects[0]
		metadata, _ := contract.ObjectAt(project, "project")
		if metadata["id"] != projectID || value.Text == nil || value.Number != nil || value.Clear {
			return nil, errors.New("execution-state fixture field mutation is malformed")
		}
		items, _ := contract.Objects(project, "items")
		for _, item := range items {
			if item["item_id"] == itemID {
				fields, _ := contract.ObjectAt(item, "field_values")
				fields[string(field)] = *value.Text
				return contract.Object{"clientMutationId": nonce, "projectV2Item": contract.Object{"id": itemID}}, nil
			}
		}
		return nil, errors.New("execution-state fixture item is absent")
	})
}

func (f *executionStateFixtureProvider) CreateIssueComment(_ context.Context, nonce string, number int64, body string) (contract.Object, error) {
	marker, err := native.OperationMarker(nonce)
	if err != nil {
		return nil, err
	}
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		comments, _ := contract.Objects(inventory, "comments")
		id := int64(701)
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

func executionStateTestPolicy() contract.Object {
	project := ProjectScope{Host: "github.com", Owner: "example", OwnerType: "Organization", Number: 1, ID: "PVT_backlog", Title: "Backlog"}
	return contract.Object{"issue_number": int64(1), "issue_state": "OPEN", "project": project.Object(), "status_field": "Status", "target_status": "In Progress", "work_comment": contract.Object{"marker": "<!-- consumer:work-start:17 -->", "body": "Starting reviewed work for issue #1."}}
}

func TestGovernanceExecutionStateReplaysTypedStateProjectAndCommentWithoutDuplicateWrites(t *testing.T) {
	provider := newExecutionStateFixtureProvider()
	plan, err := PrepareGovernance(context.Background(), provider, testRepo(), GovernanceExecutionState, executionStateTestPolicy(), time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Operations) != 4 || plan.Operations[0].Kind != "issue-state-set" || plan.Operations[1].Kind != "project-membership-add" || plan.Operations[2].Kind != "project-field-set" || plan.Operations[3].Kind != "issue-comment-upsert" {
		t.Fatalf("execution-state operations = %#v", plan.Operations)
	}
	resetGovernanceFixture(t, &governanceFixtureProvider{backlogTestProvider: provider.backlogTestProvider})
	engine := governanceEngine(t.TempDir(), provider)
	result, err := engine.Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatalf("execution-state apply = %#v, %v", result, err)
	}
	state, _ := provider.load()
	writes, _ := contract.Integer(state["writes"])
	if writes != 4 {
		t.Fatalf("execution-state writes=%d, want four separately acknowledged primitives", writes)
	}
	inventory := backlogStateInventory(state)
	graph, _ := contract.ObjectAt(inventory, "issue_inventory")
	if issues := mustObjects(graph, "issues"); issues[0]["state"] != "OPEN" {
		t.Fatalf("issue state after execution transition = %#v", issues[0]["state"])
	}
	projects, _ := contract.Objects(inventory, "projects")
	items := mustObjects(projects[0], "items")
	if len(items) != 1 || mustObject(items[0], "field_values")["Status"] != "In Progress" {
		t.Fatalf("Project state after execution transition = %#v", projects[0])
	}
	comments, _ := contract.Objects(inventory, "comments")
	if len(comments) != 1 || !strings.Contains(comments[0]["body"].(string), "<!-- consumer:work-start:17 -->") {
		t.Fatalf("work comment after execution transition = %#v", comments)
	}
	if _, err = engine.Apply(context.Background(), plan.Object()); err != nil {
		t.Fatal(err)
	}
	state, _ = provider.load()
	if writes, _ = contract.Integer(state["writes"]); writes != 4 {
		t.Fatalf("terminal execution-state replay repeated writes: %d", writes)
	}
}

func TestGovernanceExecutionStateUnknownWithoutAckIsNeverRetried(t *testing.T) {
	provider := newExecutionStateFixtureProvider()
	plan, err := PrepareGovernance(context.Background(), provider, testRepo(), GovernanceExecutionState, executionStateTestPolicy(), time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	state, _ := provider.load()
	state["reads"], state["writes"], state["fail_first_write"] = int64(0), int64(0), true
	_ = provider.save(state)
	engine := governanceEngine(t.TempDir(), provider)
	if _, err = engine.Apply(context.Background(), plan.Object()); err == nil {
		t.Fatal("lost issue-state ACK was reported complete")
	}
	if _, err = engine.Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "no mutation was retried") {
		t.Fatalf("unknown issue-state without ACK was not blocked: %v", err)
	}
	state, _ = provider.load()
	if writes, _ := contract.Integer(state["writes"]); writes != 1 {
		t.Fatalf("unknown execution-state operation was replayed: %d writes", writes)
	}
}

func TestGovernanceUnknownWithoutACKNeverReplays(t *testing.T) {
	provider := newGovernanceFixtureProvider("")
	plan := prepareGovernancePalette(t, provider)
	resetGovernanceFixture(t, provider)
	state, _ := provider.load()
	state["fail_first_write"] = true
	if err := provider.save(state); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := governanceEngine(root, provider).Apply(context.Background(), plan.Object()); err == nil {
		t.Fatal("lost label ACK was marked complete")
	}
	if _, err := governanceEngine(root, provider).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "no durable native acknowledgement") {
		t.Fatalf("unknown label write did not fail closed: %v", err)
	}
	state, _ = provider.load()
	writes, _ := contract.Integer(state["writes"])
	if writes != 1 {
		t.Fatalf("unknown write was replayed: %d writes", writes)
	}
}

func TestGovernanceFreshProcessRecoversDurableACKWithoutReplay(t *testing.T) {
	if os.Getenv("GH_STEWARD_GOVERNANCE_CHILD") == "1" {
		planPath, statePath, journalRoot := os.Getenv("GH_STEWARD_GOVERNANCE_PLAN"), os.Getenv("GH_STEWARD_GOVERNANCE_STATE"), os.Getenv("GH_STEWARD_GOVERNANCE_JOURNAL")
		data, err := os.ReadFile(planPath)
		if err != nil {
			t.Fatal(err)
		}
		object, err := contract.Decode(strings.NewReader(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		provider := newGovernanceFixtureProvider(statePath)
		if os.Getenv("GH_STEWARD_GOVERNANCE_CRASH") == "1" {
			provider.crashRead = 2
		}
		result, err := governanceEngine(journalRoot, provider).Apply(context.Background(), object)
		if err != nil || result["status"] != "completed" {
			t.Fatalf("child apply = %#v, %v", result, err)
		}
		return
	}
	statePath := filepath.Join(t.TempDir(), "remote.json")
	provider := newGovernanceFixtureProvider(statePath)
	if err := provider.save(provider.state); err != nil {
		t.Fatal(err)
	}
	plan := prepareGovernancePalette(t, provider)
	resetGovernanceFixture(t, provider)
	planPath, err := writeGovernanceObject(t, plan.Object())
	if err != nil {
		t.Fatal(err)
	}
	journalRoot := filepath.Join(t.TempDir(), "journal")
	if err := os.MkdirAll(journalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	command := func(crash bool) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestGovernanceFreshProcessRecoversDurableACKWithoutReplay$")
		env := append([]string{}, os.Environ()...)
		env = append(env, "GH_STEWARD_GOVERNANCE_CHILD=1", "GH_STEWARD_GOVERNANCE_PLAN="+planPath, "GH_STEWARD_GOVERNANCE_STATE="+statePath, "GH_STEWARD_GOVERNANCE_JOURNAL="+journalRoot)
		if crash {
			env = append(env, "GH_STEWARD_GOVERNANCE_CRASH=1")
		}
		cmd.Env = env
		return cmd
	}
	crashOutput, crashErr := command(true).CombinedOutput()
	if crashErr == nil {
		t.Fatal("child did not crash after persisting native ACK")
	} else if exit, ok := crashErr.(*exec.ExitError); !ok || exit.ExitCode() != 73 {
		t.Fatalf("crash exit = %v, want 73; output=%s", crashErr, crashOutput)
	}
	if recoveryOutput, recoveryErr := command(false).CombinedOutput(); recoveryErr != nil {
		t.Fatalf("fresh recovery process failed: %v; output=%s", recoveryErr, recoveryOutput)
	}
	state, err := provider.load()
	if err != nil {
		t.Fatal(err)
	}
	writes, _ := contract.Integer(state["writes"])
	if writes != 2 {
		t.Fatalf("fresh process did not resume the one remaining palette mutation without replay: %d total writes", writes)
	}
}

func writeGovernanceObject(t *testing.T, object contract.Object) (string, error) {
	t.Helper()
	data, err := contract.Canonical(object)
	if err != nil {
		return "", err
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return "", err
	}
	return path, nil
}

var _ GovernanceProvider = (*governanceFixtureProvider)(nil)
