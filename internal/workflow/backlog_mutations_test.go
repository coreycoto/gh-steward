package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

type backlogMutationTestProvider struct{ *backlogTestProvider }

func backlogMutationProjectScope() ProjectScope {
	return ProjectScope{Host: "github.com", Owner: "example", OwnerType: "Organization", Number: 1, ID: "PVT_backlog", Title: "Backlog"}
}

func backlogMutationInventory() contract.Object {
	inventory := backlogTestInventory(true)
	graph, _ := contract.ObjectAt(inventory, "issue_inventory")
	first, _ := contract.Objects(graph, "issues")
	issue1 := first[0]
	issue1["parent_number"] = int64(2)
	issue1["blocked_by_numbers"] = []any{int64(2)}
	issue2 := contract.Object{"id": "I_2", "number": int64(2), "title": "Old parent", "body": "", "state": "OPEN", "url": "https://github.com/example/widgets/issues/2", "labels": []any{}, "milestone": nil, "blocked_by_numbers": []any{}, "child_numbers": []any{int64(1)}, "parent_number": nil}
	issue3 := contract.Object{"id": "I_3", "number": int64(3), "title": "Dependency", "body": "", "state": "OPEN", "url": "https://github.com/example/widgets/issues/3", "labels": []any{}, "milestone": nil, "blocked_by_numbers": []any{}, "child_numbers": []any{}, "parent_number": nil}
	graph["issues"] = []any{issue1, issue2, issue3}
	inventory["milestones"] = []any{contract.Object{"id": int64(404), "node_id": "M_404", "number": int64(4), "title": "Q4", "description": "Quarter four", "state": "open", "due_on": nil, "url": "https://github.com/example/widgets/milestone/4"}}
	return inventory
}

func newBacklogMutationTestProvider() *backlogMutationTestProvider {
	return &backlogMutationTestProvider{&backlogTestProvider{state: contract.Object{
		"inventory": backlogMutationInventory(), "reads": int64(0), "writes": int64(0), "fail_first_write": false,
	}}}
}

func (f *backlogMutationTestProvider) BacklogInventory(ctx context.Context, request BacklogInventoryRequest) (contract.Object, error) {
	inventory, err := f.backlogTestProvider.BacklogInventory(ctx, request)
	if err != nil {
		return nil, err
	}
	requested := map[string]bool{}
	for _, scope := range request.Projects {
		requested[scope.ID] = true
	}
	projects, err := contract.Objects(inventory, "projects")
	if err != nil {
		return nil, err
	}
	selected := make([]any, 0, len(projects))
	for _, row := range projects {
		project, err := contract.ObjectAt(row, "project")
		if err != nil {
			return nil, err
		}
		id, err := contract.String(project, "id")
		if err != nil {
			return nil, err
		}
		if requested[id] {
			selected = append(selected, row)
		}
	}
	inventory["projects"] = selected
	return inventory, nil
}

func backlogMutationIntentMixed() contract.Object {
	return contract.Object{"schema_version": int64(1), "issues": []any{
		contract.Object{"issue_number": int64(1), "title": "Task one edited", "body": "Edited body", "labels": []any{"bug"}, "milestone": "Q4",
			"project":       contract.Object{"ensure_membership": true, "title": "Backlog", "status": "In Progress"},
			"relationships": contract.Object{"parent": nil, "blocked_by": []any{contract.Object{"issue_number": int64(3)}}}},
		contract.Object{"client_id": "new-child", "title": "New child", "body": "Child details", "labels": []any{"bug"},
			"project":       contract.Object{"ensure_membership": true, "title": "Backlog", "status": "Todo"},
			"relationships": contract.Object{"parent": contract.Object{"issue_number": int64(1)}}},
	}}
}

func backlogMutationCreateIntent() contract.Object {
	return contract.Object{"schema_version": int64(1), "issues": []any{
		contract.Object{"client_id": "first", "title": "First created", "body": "First body", "project": contract.Object{"ensure_membership": true, "title": "Backlog", "status": "Todo"}},
		contract.Object{"client_id": "second", "title": "Second created", "body": "Second body", "project": contract.Object{"ensure_membership": true, "title": "Backlog", "status": "In Progress"}, "relationships": contract.Object{"parent": contract.Object{"client_id": "first"}}},
	}}
}

func prepareBacklogMutations(t *testing.T, provider BacklogProvider, intent contract.Object, projects []ProjectScope) contract.Plan {
	t.Helper()
	plan, err := PrepareBacklogMutations(context.Background(), provider, testRepo(), intent, projects, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func backlogMutationEngine(root string, provider BacklogProvider) apply.Engine {
	return apply.Engine{Root: root, Repository: testRepo(), Command: BacklogMutationsCommand, Adapter: BacklogMutations{Provider: provider}}
}

func TestBacklogMutationPreparationDerivesOrderedMixedPrimitives(t *testing.T) {
	provider := newBacklogMutationTestProvider()
	plan := prepareBacklogMutations(t, provider, backlogMutationIntentMixed(), []ProjectScope{backlogMutationProjectScope()})
	kinds := map[string]int{}
	for _, op := range plan.Operations {
		kinds[op.Kind]++
	}
	for _, kind := range []string{"issue-create", "issue-update", "project-membership-add", "project-field-set", "issue-relationship-add", "issue-relationship-remove"} {
		if kinds[kind] == 0 {
			t.Fatalf("typed primitive %q was omitted: %#v", kind, kinds)
		}
	}
	if kinds["issue-relationship-remove"] != 2 || kinds["issue-relationship-add"] != 2 {
		t.Fatalf("relationship delta did not preserve remove-before-add reparenting and dependency replacement: %#v", kinds)
	}
	var lastRemove, firstAdd int
	lastRemove, firstAdd = -1, len(plan.Operations)
	for i, op := range plan.Operations {
		if op.Kind == "issue-relationship-remove" {
			lastRemove = i
		}
		if op.Kind == "issue-relationship-add" && firstAdd > i {
			firstAdd = i
		}
	}
	if lastRemove < 0 || firstAdd <= lastRemove {
		t.Fatalf("relationship adds were not ordered after removals: %#v", plan.Operations)
	}
	for _, op := range plan.Operations {
		if op.Kind == "issue-relationship-add" || op.Kind == "issue-relationship-remove" {
			if _, err := contract.ObjectAt(op.Target, "issue_ref"); err != nil {
				t.Fatal(op.Target, err)
			}
			if _, err := contract.ObjectAt(op.Target, "related_issue_ref"); err != nil {
				t.Fatal(op.Target, err)
			}
		}
	}
	derived, err := (Backlog{}).Operations(plan)
	if err != nil || !same(derived, plan.Operations) {
		t.Fatalf("stored operations differ from pure re-derivation: %v", err)
	}
}

func TestBacklogMutationProjectScopeBindsMembershipOnlyAndFieldChanges(t *testing.T) {
	projectScope := backlogMutationProjectScope()
	t.Run("membership only", func(t *testing.T) {
		provider := newBacklogMutationTestProvider()
		intent := contract.Object{"schema_version": int64(1), "issues": []any{
			contract.Object{"issue_number": int64(1), "project": contract.Object{"ensure_membership": true, "title": "Backlog"}},
		}}
		plan := prepareBacklogMutations(t, provider, intent, []ProjectScope{projectScope})
		if len(plan.Operations) != 1 || plan.Operations[0].Kind != "project-membership-add" || plan.Operations[0].Target["project_id"] != projectScope.ID {
			t.Fatalf("membership-only intent did not derive the scoped membership operation: %#v", plan.Operations)
		}
		if _, err := backlogMutationEngine(t.TempDir(), provider).Apply(context.Background(), plan.Object()); err != nil {
			t.Fatal(err)
		}
		state, err := provider.load()
		if err != nil {
			t.Fatal(err)
		}
		project := mustObjects(backlogStateInventory(state), "projects")[0]
		items := mustObjects(project, "items")
		if len(items) != 1 || len(mustObject(items[0], "field_values")) != 0 {
			t.Fatalf("membership-only change did not preserve empty field state: %#v", items)
		}
	})
	t.Run("field mutation", func(t *testing.T) {
		provider := newBacklogMutationTestProvider()
		intent := contract.Object{"schema_version": int64(1), "issues": []any{
			contract.Object{"issue_number": int64(1), "project": contract.Object{"title": "Backlog", "status": "In Progress"}},
		}}
		plan := prepareBacklogMutations(t, provider, intent, []ProjectScope{projectScope})
		if len(plan.Operations) != 2 || plan.Operations[0].Kind != "project-membership-add" || plan.Operations[1].Kind != "project-field-set" {
			t.Fatalf("field intent did not derive scoped membership then field operations: %#v", plan.Operations)
		}
		if plan.Operations[0].Target["project_id"] != projectScope.ID || plan.Operations[1].Target["project_id"] != projectScope.ID || plan.Operations[1].Target["field_name"] != "Status" {
			t.Fatalf("field operations escaped captured Project scope: %#v", plan.Operations)
		}
		if _, err := backlogMutationEngine(t.TempDir(), provider).Apply(context.Background(), plan.Object()); err != nil {
			t.Fatal(err)
		}
		state, err := provider.load()
		if err != nil {
			t.Fatal(err)
		}
		project := mustObjects(backlogStateInventory(state), "projects")[0]
		items := mustObjects(project, "items")
		if len(items) != 1 || mustObject(items[0], "field_values")["Status"] != "In Progress" {
			t.Fatalf("scoped field mutation was not projected: %#v", items)
		}
	})
}

func TestBacklogMutationPreparationRejectsMalformedReferencesDuplicatesAndCycles(t *testing.T) {
	provider := newBacklogMutationTestProvider()
	project := []ProjectScope{backlogMutationProjectScope()}
	tests := []struct {
		name     string
		intent   contract.Object
		projects []ProjectScope
	}{
		{"duplicate client id", contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"client_id": "same", "title": "One", "body": "x"}, contract.Object{"client_id": "same", "title": "Two", "body": "y"}}}, nil},
		{"forward client reference", contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"client_id": "first", "title": "First", "body": "x", "relationships": contract.Object{"parent": contract.Object{"client_id": "later"}}}, contract.Object{"client_id": "later", "title": "Later", "body": "y"}}}, nil},
		{"relationship cycle", contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"issue_number": int64(1), "relationships": contract.Object{"parent": contract.Object{"issue_number": int64(2)}}}, contract.Object{"issue_number": int64(2), "relationships": contract.Object{"parent": contract.Object{"issue_number": int64(1)}}}}}, nil},
		{"unknown label", contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"issue_number": int64(1), "labels": []any{"not-in-repository"}}}}, nil},
		{"unresolved project scope", contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"issue_number": int64(1), "project": contract.Object{"ensure_membership": true, "title": "Backlog"}}}}, nil},
		{"unused project scope", contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"issue_number": int64(1), "body": "x"}}}, project},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := PrepareBacklogMutations(context.Background(), provider, testRepo(), test.intent, test.projects, time.Now())
			if err == nil {
				t.Fatal("invalid authored intent was accepted")
			}
		})
	}
}

func TestBacklogMutationCreationMarkerUsesStablePortableV2IntentHash(t *testing.T) {
	intent := contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"client_id": "created", "title": "Task é 😀", "body": "Body", "labels": []any{"bug"}}}}
	marker, err := backlogMutationCreateMarker(intent, testRepo(), "created")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := contract.Canonical(contract.Object{"repository": testRepo().Object(), "intent": intent, "client_id": "created"})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	want := "<!-- gh-steward-backlog-mutation:" + hex.EncodeToString(digest[:]) + " -->"
	if marker != want {
		t.Fatalf("marker=%q want canonical portable digest=%q", marker, want)
	}
	decoded, err := contract.Clone(intent)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := backlogMutationCreateMarker(decoded, testRepo(), "created")
	if err != nil || replayed != marker {
		t.Fatalf("clone changed marker: %q %v", replayed, err)
	}
	changed := contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"client_id": "created", "title": "Task é 😁", "body": "Body", "labels": []any{"bug"}}}}
	changedMarker, err := backlogMutationCreateMarker(changed, testRepo(), "created")
	if err != nil || changedMarker == marker {
		t.Fatalf("authored intent change did not change marker: %q %v", changedMarker, err)
	}
}

func TestBacklogMutationFreshProcessRecoversCreateProjectACKThenCompletesSecondCreateAndRelationship(t *testing.T) {
	if os.Getenv("STEWARD_BACKLOG_MUTATION_HELPER") != "" {
		provider, err := loadBacklogMutationProvider(os.Getenv("STEWARD_BACKLOG_MUTATION_STATE"), 4)
		if err != nil {
			t.Fatal(err)
		}
		planFile, err := os.Open(os.Getenv("STEWARD_BACKLOG_MUTATION_PLAN"))
		if err != nil {
			t.Fatal(err)
		}
		plan, err := contract.Decode(planFile)
		_ = planFile.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = backlogMutationEngine(os.Getenv("STEWARD_BACKLOG_MUTATION_ROOT"), provider).Apply(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
		return
	}
	base := newBacklogMutationTestProvider()
	plan := prepareBacklogMutations(t, base, backlogMutationCreateIntent(), []ProjectScope{backlogMutationProjectScope()})
	if len(plan.Operations) != 7 {
		t.Fatalf("create/project/relationship plan has %d operations: %#v", len(plan.Operations), plan.Operations)
	}
	root := t.TempDir()
	statePath, planPath := filepath.Join(root, "state.json"), filepath.Join(root, "plan.json")
	base.stateFile = statePath
	base.crashRead = 0
	if err := base.save(base.state); err != nil {
		t.Fatal(err)
	}
	encoded, err := contract.Canonical(plan.Object())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(planPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBacklogMutationFreshProcessRecoversCreateProjectACKThenCompletesSecondCreateAndRelationship$")
	cmd.Env = append(os.Environ(), "STEWARD_BACKLOG_MUTATION_HELPER=1", "STEWARD_BACKLOG_MUTATION_STATE="+statePath, "STEWARD_BACKLOG_MUTATION_PLAN="+planPath, "STEWARD_BACKLOG_MUTATION_ROOT="+root)
	output, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 73 {
		t.Fatalf("subprocess did not crash after durable Project ACK: %v\n%s", err, output)
	}
	resumed, err := loadBacklogMutationProvider(statePath, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := backlogMutationEngine(root, resumed).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatalf("fresh-process recovery=%#v err=%v", result, err)
	}
	state, err := resumed.load()
	if err != nil {
		t.Fatal(err)
	}
	writes, _ := contract.Integer(state["writes"])
	if writes != int64(len(plan.Operations)) {
		t.Fatalf("writes=%d want exactly %d (completed/ACKed work must not replay)", writes, len(plan.Operations))
	}
	if _, err = backlogMutationEngine(root, resumed).Apply(context.Background(), plan.Object()); err != nil {
		t.Fatal(err)
	}
	state, _ = resumed.load()
	writes, _ = contract.Integer(state["writes"])
	if writes != int64(len(plan.Operations)) {
		t.Fatalf("terminal replay dispatched writes: %d", writes)
	}
	graph, _ := contract.ObjectAt(backlogStateInventory(state), "issue_inventory")
	issues, _ := contract.Objects(graph, "issues")
	if len(issues) != 5 {
		t.Fatalf("created issues missing after recovery: %d", len(issues))
	}
}

func TestBacklogMutationApplyRejectsReauthoredPlanAndStopsOnCompleteInventoryDrift(t *testing.T) {
	t.Run("rehash does not authorize generic caller operations", func(t *testing.T) {
		provider := newBacklogMutationTestProvider()
		intent := contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"issue_number": int64(1), "body": "Reviewed body"}}}
		plan := prepareBacklogMutations(t, provider, intent, nil)
		state, _ := provider.load()
		state["reads"], state["writes"] = int64(0), int64(0)
		_ = provider.save(state)
		plan.Operations[0].Target["value"] = "unreviewed body"
		plan.SHA256, _ = contract.Digest(plan.Unsigned())
		if _, err := backlogMutationEngine(t.TempDir(), provider).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "differ from domain intent") {
			t.Fatalf("rehash-authorized caller operation accepted: %v", err)
		}
		state, _ = provider.load()
		writes, _ := contract.Integer(state["writes"])
		if writes != 0 {
			t.Fatalf("invalid reviewed plan dispatched %d writes", writes)
		}
	})
	t.Run("complete live drift stops later writes", func(t *testing.T) {
		provider := newBacklogMutationTestProvider()
		intent := contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"issue_number": int64(1), "body": "Reviewed body"}}}
		plan := prepareBacklogMutations(t, provider, intent, nil)
		state, _ := provider.load()
		state["reads"], state["writes"] = int64(0), int64(0)
		_ = provider.save(state)
		provider.driftRead = 3
		if _, err := backlogMutationEngine(t.TempDir(), provider).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "drifted") {
			t.Fatalf("terminal full-inventory drift was accepted: %v", err)
		}
		state, _ = provider.load()
		writes, _ := contract.Integer(state["writes"])
		if writes != 1 {
			t.Fatalf("inventory drift allowed %d writes", writes)
		}
	})
	t.Run("unknown without durable ACK is never retried", func(t *testing.T) {
		provider := newBacklogMutationTestProvider()
		intent := contract.Object{"schema_version": int64(1), "issues": []any{contract.Object{"client_id": "lost", "title": "Lost response", "body": "create"}}}
		plan := prepareBacklogMutations(t, provider, intent, nil)
		state, _ := provider.load()
		state["reads"], state["writes"], state["fail_first_write"] = int64(0), int64(0), true
		_ = provider.save(state)
		root := t.TempDir()
		if _, err := backlogMutationEngine(root, provider).Apply(context.Background(), plan.Object()); err == nil {
			t.Fatal("lost create response was reported completed")
		}
		if _, err := backlogMutationEngine(root, provider).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "no durable native acknowledgement") {
			t.Fatalf("ambiguous create was not held fail-closed: %v", err)
		}
		state, _ = provider.load()
		writes, _ := contract.Integer(state["writes"])
		if writes != 1 {
			t.Fatalf("unknown create was replayed: %d writes", writes)
		}
	})
}

func loadBacklogMutationProvider(path string, crashRead int) (*backlogMutationTestProvider, error) {
	base := &backlogTestProvider{stateFile: path, crashRead: crashRead}
	state, err := base.load()
	if err != nil {
		return nil, err
	}
	base.state = state
	return &backlogMutationTestProvider{base}, nil
}

func backlogMutationFindIssue(inventory contract.Object, number int64) (contract.Object, error) {
	graph, err := contract.ObjectAt(inventory, "issue_inventory")
	if err != nil {
		return nil, err
	}
	rows, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	issue := backlogIssueByNumber(rows, number)
	if issue == nil {
		return nil, fmt.Errorf("issue %d not found", number)
	}
	return issue, nil
}

func (f *backlogMutationTestProvider) rawIssue(inventory, issue contract.Object) contract.Object {
	labels := []any{}
	for _, raw := range issue["labels"].([]any) {
		labels = append(labels, contract.Object{"name": raw})
	}
	var milestone any
	if issue["milestone"] != nil {
		for _, row := range mustObjects(inventory, "milestones") {
			if row["title"] == issue["milestone"] {
				milestone = contract.Object{"number": row["number"], "title": row["title"]}
			}
		}
	}
	number, _ := contract.PositiveInteger(issue["number"])
	return contract.Object{"id": number + 10000, "node_id": issue["id"], "number": number, "title": issue["title"], "body": issue["body"], "state": "open", "html_url": issue["url"], "labels": labels, "milestone": milestone}
}

func (f *backlogMutationTestProvider) CreateIssue(_ context.Context, nonce string, draft IssueDraft) (contract.Object, error) {
	marker, err := native.OperationMarker(nonce)
	if err != nil {
		return nil, err
	}
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		graph, _ := contract.ObjectAt(inventory, "issue_inventory")
		rows, _ := contract.Objects(graph, "issues")
		var max int64
		for _, row := range rows {
			n, _ := contract.PositiveInteger(row["number"])
			if n > max {
				max = n
			}
		}
		number := max + 1
		labels := []any{}
		for _, name := range draft.Labels {
			labels = append(labels, name)
		}
		issue := contract.Object{"id": fmt.Sprintf("I_%d", number), "number": number, "title": draft.Title, "body": draft.Body + "\n\n" + marker, "state": "OPEN", "url": fmt.Sprintf("https://github.com/example/widgets/issues/%d", number), "labels": labels, "milestone": nil, "blocked_by_numbers": []any{}, "child_numbers": []any{}, "parent_number": nil}
		rows = append(rows, issue)
		graph["issues"] = backlogObjectsAsAny(rows)
		return f.rawIssue(inventory, issue), nil
	})
}

func (f *backlogMutationTestProvider) UpdateIssue(_ context.Context, _ string, number int64, patch IssuePatch) (contract.Object, error) {
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		issue, err := backlogMutationFindIssue(inventory, number)
		if err != nil {
			return nil, err
		}
		if patch.Title != nil {
			issue["title"] = *patch.Title
		}
		if patch.Body != nil {
			issue["body"] = *patch.Body
		}
		if patch.Labels != nil {
			labels := []any{}
			for _, name := range *patch.Labels {
				labels = append(labels, name)
			}
			issue["labels"] = labels
		}
		if patch.ClearMilestone {
			issue["milestone"] = nil
		} else if patch.MilestoneNumber != nil {
			var title string
			for _, row := range mustObjects(inventory, "milestones") {
				n, _ := contract.PositiveInteger(row["number"])
				if n == *patch.MilestoneNumber {
					title, _ = row["title"].(string)
				}
			}
			if title == "" {
				return nil, errors.New("milestone absent")
			}
			issue["milestone"] = title
		}
		return f.rawIssue(inventory, issue), nil
	})
}

func backlogMutationFieldNodes(values contract.Object) []any {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	nodes := []any{}
	for _, key := range keys {
		value := values[key]
		if key == "Queue Order" {
			nodes = append(nodes, contract.Object{"__typename": "ProjectV2ItemFieldNumberValue", "field": contract.Object{"name": key}, "number": value})
		} else {
			nodes = append(nodes, contract.Object{"__typename": "ProjectV2ItemFieldSingleSelectValue", "field": contract.Object{"name": key}, "name": value})
		}
	}
	return nodes
}

func (f *backlogMutationTestProvider) AddProjectIssue(_ context.Context, nonce, projectID, issueNodeID string) (contract.Object, error) {
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		projects, _ := contract.Objects(inventory, "projects")
		var project contract.Object
		for _, row := range projects {
			p, _ := contract.ObjectAt(row, "project")
			if p["id"] == projectID {
				project = row
			}
		}
		if project == nil {
			return nil, errors.New("project absent")
		}
		graph, _ := contract.ObjectAt(inventory, "issue_inventory")
		issues, _ := contract.Objects(graph, "issues")
		var issue contract.Object
		for _, row := range issues {
			if row["id"] == issueNodeID {
				issue = row
			}
		}
		if issue == nil {
			return nil, errors.New("issue absent")
		}
		n, _ := contract.PositiveInteger(issue["number"])
		items, _ := contract.Objects(project, "items")
		itemID := fmt.Sprintf("PVTI_%d", len(items)+1)
		values := contract.Object{}
		items = append(items, contract.Object{"item_id": itemID, "number": n, "field_values": values, "archived": false})
		project["items"] = backlogObjectsAsAny(items)
		return contract.Object{"clientMutationId": nonce, "id": itemID, "isArchived": false, "content": contract.Object{"number": n, "id": issueNodeID, "url": issue["url"]}, "fieldValues": contract.Object{"nodes": []any{}, "pageInfo": contract.Object{"hasNextPage": false, "endCursor": nil}}}, nil
	})
}

func (f *backlogMutationTestProvider) SetProjectField(_ context.Context, nonce, projectID, itemID string, field ProjectField, value ProjectFieldValue) (contract.Object, error) {
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		projects, _ := contract.Objects(inventory, "projects")
		var project contract.Object
		for _, row := range projects {
			p, _ := contract.ObjectAt(row, "project")
			if p["id"] == projectID {
				project = row
			}
		}
		if project == nil {
			return nil, errors.New("project absent")
		}
		items, _ := contract.Objects(project, "items")
		var item contract.Object
		for _, row := range items {
			if row["item_id"] == itemID {
				item = row
			}
		}
		if item == nil {
			return nil, errors.New("item absent")
		}
		values, _ := contract.ObjectAt(item, "field_values")
		if value.Clear {
			delete(values, string(field))
		} else if value.Text != nil {
			values[string(field)] = *value.Text
		} else if value.Number != nil {
			values[string(field)] = *value.Number
		} else {
			return nil, errors.New("empty field value")
		}
		return contract.Object{"clientMutationId": nonce, "projectV2Item": contract.Object{"id": itemID}}, nil
	})
}

func (f *backlogMutationTestProvider) relationship(issue, related int64, kind Relationship, remove bool) (contract.Object, error) {
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		a, err := backlogMutationFindIssue(inventory, issue)
		if err != nil {
			return nil, err
		}
		b, err := backlogMutationFindIssue(inventory, related)
		if err != nil {
			return nil, err
		}
		if kind == BlockedBy {
			values, _ := contract.Array(a, "blocked_by_numbers")
			if remove {
				a["blocked_by_numbers"] = backlogRemoveNumber(values, related)
			} else {
				a["blocked_by_numbers"] = backlogAppendUniqueNumber(values, related)
			}
		} else if kind == Child {
			children, _ := contract.Array(a, "child_numbers")
			if remove {
				a["child_numbers"] = backlogRemoveNumber(children, related)
				b["parent_number"] = nil
			} else {
				a["child_numbers"] = backlogAppendUniqueNumber(children, related)
				b["parent_number"] = issue
			}
		} else {
			return nil, errors.New("unsupported relationship")
		}
		return contract.Object{"acknowledged": true}, nil
	})
}
func (f *backlogMutationTestProvider) AddRelationship(_ context.Context, _ string, issue, related int64, kind Relationship) (contract.Object, error) {
	return f.relationship(issue, related, kind, false)
}
func (f *backlogMutationTestProvider) RemoveRelationship(_ context.Context, _ string, issue, related int64, kind Relationship) (contract.Object, error) {
	return f.relationship(issue, related, kind, true)
}

var _ BacklogProvider = (*backlogMutationTestProvider)(nil)
