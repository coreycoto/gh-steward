package workflow

import (
	"context"
	"errors"
	"fmt"
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

type backlogTestProvider struct {
	stateFile string
	state     contract.Object
	crashRead int
	driftRead int
}

func backlogTestInventory(withProject bool) contract.Object {
	repo := testRepo()
	issue := contract.Object{
		"id": "I_1", "number": int64(1), "title": "Task one", "body": "Initial body", "state": "OPEN",
		"url": "https://github.com/example/widgets/issues/1", "labels": []any{}, "milestone": nil,
		"blocked_by_numbers": []any{}, "child_numbers": []any{}, "parent_number": nil,
	}
	graph := contract.Object{
		"repo": repo.Object(), "issues": []any{issue},
		"provenance": contract.Object{"live": true, "complete": true, "issue_state": "all", "repository_node_id": "R_widgets"},
	}
	projects := []any{}
	if withProject {
		project := contract.Object{
			"id": "PVT_backlog", "number": int64(1), "title": "Backlog", "owner_login": "example",
			"owner_type": "Organization", "host": "github.com",
		}
		fields := contract.Object{
			"Status": contract.Object{"id": "FIELD_status", "name": "Status", "data_type": "SINGLE_SELECT", "options_by_name": contract.Object{
				"Todo": contract.Object{"id": "OPT_status_todo", "name": "Todo"}, "In Progress": contract.Object{"id": "OPT_status_progress", "name": "In Progress"}, "Done": contract.Object{"id": "OPT_status_done", "name": "Done"},
			}},
			"Priority": contract.Object{"id": "FIELD_priority", "name": "Priority", "data_type": "SINGLE_SELECT", "options_by_name": contract.Object{
				"Now": contract.Object{"id": "OPT_priority_now", "name": "Now"}, "Next": contract.Object{"id": "OPT_priority_next", "name": "Next"}, "Later": contract.Object{"id": "OPT_priority_later", "name": "Later"},
			}},
			"Queue Order": contract.Object{"id": "FIELD_order", "name": "Queue Order", "data_type": "NUMBER", "options_by_name": contract.Object{}},
		}
		projects = []any{contract.Object{
			"repo": repo.Object(), "project": project, "fields_by_name": fields, "items": []any{},
			"provenance": contract.Object{"live": true, "complete": true, "source": "github_project_api"},
		}}
	}
	labels := []any{}
	if withProject {
		labels = []any{contract.Object{"id": int64(14), "node_id": "LA_bug", "name": "bug", "color": "ff0000", "description": "Bug"}}
	}
	return contract.Object{
		"repo": repo.Object(), "issue_inventory": graph, "milestones": []any{}, "comments": []any{},
		"labels": labels, "projects": projects,
		"provenance": contract.Object{
			"live": true, "complete": true, "issue_state": "all", "repository_node_id": "R_widgets",
			"milestones_source": "github_api", "labels_source": "github_api", "comments_source": "not_requested",
			"relationships_source": "not_requested", "comment_markers": []any{}, "project_scopes": []any{},
		},
	}
}

func newBacklogTestProvider(withProject bool) *backlogTestProvider {
	return &backlogTestProvider{state: contract.Object{
		"inventory": backlogTestInventory(withProject), "reads": int64(0), "writes": int64(0), "fail_first_write": false,
	}}
}

func (f *backlogTestProvider) load() (contract.Object, error) {
	if f.stateFile == "" {
		return f.state, nil
	}
	r, err := os.Open(f.stateFile)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return contract.Decode(r)
}

func (f *backlogTestProvider) save(state contract.Object) error {
	f.state = state
	if f.stateFile == "" {
		return nil
	}
	data, err := contract.Canonical(state)
	if err != nil {
		return err
	}
	return os.WriteFile(f.stateFile, data, 0600)
}

func backlogStateInventory(state contract.Object) contract.Object {
	return state["inventory"].(map[string]any)
}

func (f *backlogTestProvider) BacklogInventory(_ context.Context, request BacklogInventoryRequest) (contract.Object, error) {
	state, err := f.load()
	if err != nil {
		return nil, err
	}
	reads, err := contract.Integer(state["reads"])
	if err != nil {
		return nil, err
	}
	state["reads"] = reads + 1
	readCount := int(reads + 1)
	inventory := backlogStateInventory(state)
	if f.driftRead == readCount {
		graph, _ := contract.ObjectAt(inventory, "issue_inventory")
		issues, _ := contract.Objects(graph, "issues")
		issues[0]["body"] = "Concurrent unrelated edit"
	}
	if err := f.save(state); err != nil {
		return nil, err
	}
	if f.crashRead == readCount {
		os.Exit(73)
	}
	out, err := contract.Clone(inventory)
	if err != nil {
		return nil, err
	}
	provenance, _ := contract.ObjectAt(out, "provenance")
	markers := append([]string{}, request.CommentMarkers...)
	provenance["comment_markers"] = stringSliceAny(markers)
	provenance["comments_source"] = "not_requested"
	if len(markers) > 0 {
		provenance["comments_source"] = "github_api"
		comments, _ := contract.Objects(out, "comments")
		selected := []any{}
		for _, comment := range comments {
			body, _ := comment["body"].(string)
			for _, marker := range markers {
				if strings.Contains(body, marker) {
					selected = append(selected, comment)
					break
				}
			}
		}
		out["comments"] = selected
	} else {
		out["comments"] = []any{}
	}
	provenance["relationships_source"] = "not_requested"
	if request.IncludeRelationships {
		provenance["relationships_source"] = "github_api"
	}
	provenance["project_scopes"] = projectScopesAny(request.Projects)
	return out, nil
}

func (f *backlogTestProvider) mutate(update func(contract.Object) (contract.Object, error)) (contract.Object, error) {
	state, err := f.load()
	if err != nil {
		return nil, err
	}
	writes, err := contract.Integer(state["writes"])
	if err != nil {
		return nil, err
	}
	state["writes"] = writes + 1
	result, err := update(backlogStateInventory(state))
	if saveErr := f.save(state); saveErr != nil {
		return nil, saveErr
	}
	if state["fail_first_write"] == true && writes+1 == 1 {
		return nil, errors.New("provider accepted write but response was lost")
	}
	return result, err
}

func (f *backlogTestProvider) CreateMilestone(_ context.Context, nonce string, draft MilestoneDraft) (contract.Object, error) {
	marker, err := native.OperationMarker(nonce)
	if err != nil {
		return nil, err
	}
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		due := any(nil)
		if draft.DueOn != "" {
			due = draft.DueOn
		}
		description := draft.Description + "\n\n" + marker
		row := contract.Object{"id": int64(901), "node_id": "M_901", "number": int64(9), "title": draft.Title, "description": description, "state": "open", "due_on": due, "url": "https://github.com/example/widgets/milestone/9"}
		inventory["milestones"] = append(inventory["milestones"].([]any), row)
		return contract.Object{"id": int64(901), "node_id": "M_901", "number": int64(9), "title": draft.Title, "description": description, "state": "open", "due_on": due, "html_url": "https://github.com/example/widgets/milestone/9"}, nil
	})
}

func (f *backlogTestProvider) UpdateIssue(_ context.Context, _ string, number int64, patch IssuePatch) (contract.Object, error) {
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		graph, _ := contract.ObjectAt(inventory, "issue_inventory")
		issues, _ := contract.Objects(graph, "issues")
		var issue contract.Object
		for _, row := range issues {
			if got, _ := contract.PositiveInteger(row["number"]); got == number {
				issue = row
				break
			}
		}
		if issue == nil || patch.MilestoneNumber == nil {
			return nil, errors.New("test provider expected a milestone assignment")
		}
		milestones, _ := contract.Objects(inventory, "milestones")
		var milestoneTitle string
		for _, milestone := range milestones {
			if got, _ := contract.PositiveInteger(milestone["number"]); got == *patch.MilestoneNumber {
				milestoneTitle = milestone["title"].(string)
			}
		}
		if milestoneTitle == "" {
			return nil, errors.New("assignment references missing milestone")
		}
		issue["milestone"] = milestoneTitle
		labels := []any{}
		for _, label := range issue["labels"].([]any) {
			labels = append(labels, contract.Object{"name": label})
		}
		return contract.Object{
			"id": number, "node_id": issue["id"], "number": number, "title": issue["title"], "body": issue["body"],
			"state": "open", "html_url": issue["url"], "labels": labels,
			"milestone": contract.Object{"number": *patch.MilestoneNumber, "title": milestoneTitle},
		}, nil
	})
}

func (f *backlogTestProvider) CreateIssueComment(_ context.Context, nonce string, number int64, body string) (contract.Object, error) {
	marker, err := native.OperationMarker(nonce)
	if err != nil {
		return nil, err
	}
	return f.mutate(func(inventory contract.Object) (contract.Object, error) {
		fullBody := body + "\n\n" + marker
		id := int64(501)
		link := fmt.Sprintf("https://github.com/example/widgets/issues/%d#issuecomment-%d", number, id)
		comments, _ := contract.Objects(inventory, "comments")
		comments = append(comments, contract.Object{"id": id, "issue_number": number, "body": fullBody, "url": link})
		inventory["comments"] = backlogObjectsAsAny(comments)
		return contract.Object{"id": id, "body": fullBody, "html_url": link}, nil
	})
}

func (*backlogTestProvider) CreateIssue(context.Context, string, IssueDraft) (contract.Object, error) {
	return nil, errors.New("unexpected issue create in this fixture")
}
func (*backlogTestProvider) UpdateMilestone(context.Context, string, int64, MilestonePatch) (contract.Object, error) {
	return nil, errors.New("unexpected milestone update in this fixture")
}
func (*backlogTestProvider) UpdateIssueComment(context.Context, string, int64, int64, string) (contract.Object, error) {
	return nil, errors.New("unexpected comment update in this fixture")
}
func (*backlogTestProvider) AddProjectIssue(context.Context, string, string, string) (contract.Object, error) {
	return nil, errors.New("unexpected Project membership write in this fixture")
}
func (*backlogTestProvider) SetProjectField(context.Context, string, string, string, ProjectField, ProjectFieldValue) (contract.Object, error) {
	return nil, errors.New("unexpected Project field write in this fixture")
}
func (*backlogTestProvider) AddRelationship(context.Context, string, int64, int64, Relationship) (contract.Object, error) {
	return nil, errors.New("unexpected relationship write in this fixture")
}
func (*backlogTestProvider) RemoveRelationship(context.Context, string, int64, int64, Relationship) (contract.Object, error) {
	return nil, errors.New("unexpected relationship write in this fixture")
}

func backlogQuarterPlan() contract.Object {
	return contract.Object{
		"schema_version": int64(1), "quarter": "2026 Q4", "quarter_goals": []any{"Finish migration"}, "active_tracks": []any{"Reliability"},
		"commit_issue_numbers": []any{int64(1)}, "issue_rationale": contract.Object{"1": "This issue is required for the quarter goal."},
	}
}

func prepareBacklogQuarter(t *testing.T, provider *backlogTestProvider) contract.Plan {
	t.Helper()
	plan, err := PrepareQuarterBacklog(context.Background(), provider, testRepo(), backlogQuarterPlan(), time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func backlogEngine(root string, provider *backlogTestProvider) apply.Engine {
	return apply.Engine{Root: root, Repository: testRepo(), Command: QuarterBacklogCommand, Adapter: Backlog{Provider: provider}}
}

func resetBacklogTestState(t *testing.T, provider *backlogTestProvider) {
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

func TestQuarterBacklogApplyReplaysFromCapturedAcknowledgementsWithoutDuplicateWrites(t *testing.T) {
	provider := newBacklogTestProvider(false)
	plan := prepareBacklogQuarter(t, provider)
	if len(plan.Operations) != 3 || plan.Operations[0].Kind != "milestone-create" || plan.Operations[1].Kind != "issue-comment-upsert" || plan.Operations[2].Kind != "issue-milestone-set" {
		t.Fatalf("quarter plan primitives = %#v", plan.Operations)
	}
	resetBacklogTestState(t, provider)
	root := t.TempDir()
	result, err := backlogEngine(root, provider).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatalf("apply result = %#v, error = %v", result, err)
	}
	state, _ := provider.load()
	writes, _ := contract.Integer(state["writes"])
	if writes != 3 {
		t.Fatalf("provider writes = %v, want 3", state["writes"])
	}
	inventory := backlogStateInventory(state)
	issues, _ := contract.Objects(inventory["issue_inventory"].(map[string]any), "issues")
	if issues[0]["milestone"] != "2026 Q4" {
		t.Fatalf("quarter issue milestone = %v", issues[0]["milestone"])
	}
	milestones, _ := contract.Objects(inventory, "milestones")
	if len(milestones) != 1 || !strings.Contains(milestones[0]["description"].(string), "<!-- gh-steward:operation:") {
		t.Fatalf("created milestone did not retain native acknowledgement marker: %#v", milestones)
	}
	comments, _ := contract.Objects(inventory, "comments")
	if len(comments) != 1 || !strings.Contains(comments[0]["body"].(string), "quarter-rationale quarter=2026 Q4") || !strings.Contains(comments[0]["body"].(string), "<!-- gh-steward:operation:") {
		t.Fatalf("rationale comment did not retain marker and durable nonce: %#v", comments)
	}
	if _, err = backlogEngine(root, provider).Apply(context.Background(), plan.Object()); err != nil {
		t.Fatal(err)
	}
	state, _ = provider.load()
	writes, _ = contract.Integer(state["writes"])
	if writes != 3 {
		t.Fatalf("terminal replay dispatched completed primitives: %v writes", state["writes"])
	}
}

func TestReviewBacklogPreparationRebuildsIssueAndProjectPrimitivesFromPolicy(t *testing.T) {
	provider := newBacklogTestProvider(true)
	policy := ReviewBacklogPolicy{
		Project:            ProjectScope{Host: "github.com", Owner: "example", OwnerType: "Organization", Number: 1, ID: "PVT_backlog", Title: "Backlog"},
		SeverityToPriority: map[string]string{"critical": "Now", "high": "Now", "now": "Now", "medium": "Next", "next": "Next", "low": "Later", "later": "Later"},
		IssueTypeLabels:    map[string]string{"Initiative": "initiative", "Epic": "epic", "Research": "question", "Enhancement": "enhancement", "Bug": "bug", "Maintenance": "maintenance"},
	}
	findings := contract.Object{"schema_version": int64(1), "scope": "parser review", "findings": []any{contract.Object{
		"id": "parser-guard", "title": "Add parser guard", "issue_type": "Bug", "severity": "high", "summary": "Truncated input needs a guard.",
	}}}
	plan, err := PrepareReviewBacklog(context.Background(), provider, testRepo(), findings, policy, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, op := range plan.Operations {
		kinds[op.Kind]++
	}
	if kinds["issue-create"] != 1 || kinds["project-membership-add"] != 1 || kinds["project-field-set"] != 3 {
		t.Fatalf("review operations were not derived from explicit policy and complete source: %#v", kinds)
	}
	providerBad := newBacklogTestProvider(true)
	labels := backlogStateInventory(providerBad.state)["labels"].([]any)
	labels[0].(map[string]any)["name"] = "different-label"
	if _, err = PrepareReviewBacklog(context.Background(), providerBad, testRepo(), findings, policy, time.Now()); err == nil || !strings.Contains(err.Error(), "absent from the complete live label catalog") {
		t.Fatalf("review prepared with an absent authored label: %v", err)
	}
}

func TestBacklogApplyRejectsReauthoredPrimitiveAndUnknownWithoutAckNeverRetries(t *testing.T) {
	provider := newBacklogTestProvider(false)
	plan := prepareBacklogQuarter(t, provider)
	resetBacklogTestState(t, provider)
	plan.Operations[0].Target["title"] = "unreviewed title"
	plan.SHA256, _ = contract.Digest(plan.Unsigned())
	if _, err := backlogEngine(t.TempDir(), provider).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "differ from domain intent") {
		t.Fatalf("hash-consistent caller operation accepted: %v", err)
	}
	state, _ := provider.load()
	writes, _ := contract.Integer(state["writes"])
	if writes != 0 {
		t.Fatalf("altered plan dispatched %v writes", state["writes"])
	}

	provider = newBacklogTestProvider(false)
	plan = prepareBacklogQuarter(t, provider)
	resetBacklogTestState(t, provider)
	state, _ = provider.load()
	state["fail_first_write"] = true
	_ = provider.save(state)
	root := t.TempDir()
	if _, err := backlogEngine(root, provider).Apply(context.Background(), plan.Object()); err == nil {
		t.Fatal("provider write with missing response was claimed completed")
	}
	if _, err := backlogEngine(root, provider).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "no write was retried") {
		t.Fatalf("unknown write without captured acknowledgement did not fail closed: %v", err)
	}
	state, _ = provider.load()
	writes, _ = contract.Integer(state["writes"])
	if writes != 1 {
		t.Fatalf("unknown write without acknowledgement was replayed: %v writes", state["writes"])
	}
}

func TestBacklogCompleteInventoryDriftStopsLaterAndTerminalWrites(t *testing.T) {
	for _, driftRead := range []int{3, 7} {
		provider := newBacklogTestProvider(false)
		plan := prepareBacklogQuarter(t, provider)
		resetBacklogTestState(t, provider)
		provider.driftRead = driftRead
		_, err := backlogEngine(t.TempDir(), provider).Apply(context.Background(), plan.Object())
		if err == nil || !strings.Contains(err.Error(), "drifted") {
			t.Fatalf("complete inventory drift at read %d accepted: %v", driftRead, err)
		}
		state, _ := provider.load()
		want := int64(1)
		if driftRead == 7 {
			want = 3
		}
		writes, _ := contract.Integer(state["writes"])
		if writes != want {
			t.Fatalf("drift at read %d allowed %v writes; want %d", driftRead, state["writes"], want)
		}
	}
}

func TestBacklogFreshProcessRecoversCapturedAckAndDoesNotReplayCompletedOperations(t *testing.T) {
	if os.Getenv("STEWARD_BACKLOG_HELPER") != "" {
		statePath := os.Getenv("STEWARD_BACKLOG_STATE")
		planPath := os.Getenv("STEWARD_BACKLOG_PLAN")
		root := os.Getenv("STEWARD_BACKLOG_ROOT")
		crashRead, _ := strconv.Atoi(os.Getenv("STEWARD_BACKLOG_CRASH_READ"))
		provider := &backlogTestProvider{stateFile: statePath, crashRead: crashRead}
		file, err := os.Open(planPath)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := contract.Decode(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = backlogEngine(root, provider).Apply(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
		return
	}

	provider := newBacklogTestProvider(false)
	plan := prepareBacklogQuarter(t, provider)
	root := t.TempDir()
	statePath := filepath.Join(root, "provider-state.json")
	planPath := filepath.Join(root, "plan.json")
	resetBacklogTestState(t, provider)
	provider.stateFile = statePath
	if err := provider.save(provider.state); err != nil {
		t.Fatal(err)
	}
	encodedPlan, err := contract.Canonical(plan.Object())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(planPath, encodedPlan, 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(crashRead string) *exec.ExitError {
		cmd := exec.Command(os.Args[0], "-test.run=^TestBacklogFreshProcessRecoversCapturedAckAndDoesNotReplayCompletedOperations$")
		cmd.Env = append(os.Environ(), "STEWARD_BACKLOG_HELPER=1", "STEWARD_BACKLOG_STATE="+statePath, "STEWARD_BACKLOG_PLAN="+planPath, "STEWARD_BACKLOG_ROOT="+root, "STEWARD_BACKLOG_CRASH_READ="+crashRead)
		output, runErr := cmd.CombinedOutput()
		if runErr == nil {
			return nil
		}
		if exit, ok := runErr.(*exec.ExitError); ok {
			if crashRead != "2" || exit.ExitCode() != 73 {
				t.Fatalf("helper failed (crashRead=%s, code=%d): %s", crashRead, exit.ExitCode(), output)
			}
			return exit
		}
		t.Fatalf("helper could not run: %v", runErr)
		return nil
	}
	if invoke("2") == nil {
		t.Fatal("first process did not crash after durable acknowledgement persistence")
	}
	if invoke("0") != nil {
		t.Fatal("recovery process did not complete")
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
	writes, _ := contract.Integer(state["writes"])
	if writes != 3 {
		t.Fatalf("fresh process replayed acknowledged/completed operations: %v writes", state["writes"])
	}
	if invoke("0") != nil {
		t.Fatal("terminal fresh-process replay failed")
	}
	stateFile, _ = os.Open(statePath)
	state, err = contract.Decode(stateFile)
	_ = stateFile.Close()
	writes, _ = contract.Integer(state["writes"])
	if err != nil || writes != 3 {
		t.Fatalf("terminal fresh process repeated completed writes: state=%#v err=%v", state, err)
	}
}
