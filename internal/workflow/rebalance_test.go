package workflow

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/snapshot"
)

var rebalanceTestRepo = contract.Repository{Host: "github.com", Owner: "sample-org", Name: "sample-project", URL: "https://github.com/sample-org/sample-project"}

type fakeRebalanceProvider struct {
	statePath          string
	state              contract.Object
	driftIssue         int64
	driftFinalIssue    int64
	driftLinked        bool
	driftAddMember     bool
	driftRemoveMember  bool
	totalWrites        int64
	armCrashAfterWrite bool
	exitOnCrashRead    bool
}

func (f *fakeRebalanceProvider) readState() (contract.Object, error) {
	if f.statePath == "" {
		return f.state, nil
	}
	file, err := os.Open(f.statePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return contract.Decode(file)
}

func (f *fakeRebalanceProvider) saveState(state contract.Object) error {
	if f.statePath == "" {
		f.state = state
		return nil
	}
	data, err := contract.Canonical(state)
	if err != nil {
		return err
	}
	temp := f.statePath + ".tmp"
	if err := os.WriteFile(temp, data, 0600); err != nil {
		return err
	}
	return os.Rename(temp, f.statePath)
}

func (f *fakeRebalanceProvider) ReadSources(_ context.Context, scope native.ProjectScope) (contract.Object, contract.Object, error) {
	state, err := f.readState()
	if err != nil {
		return nil, nil, err
	}
	if state["crash_next_read"] == true {
		if f.exitOnCrashRead {
			os.Exit(73)
		}
		delete(state, "crash_next_read")
		if err := f.saveState(state); err != nil {
			return nil, nil, err
		}
	}
	graph, _ := contract.ObjectAt(state, "graph")
	project, _ := contract.ObjectAt(state, "project")
	projectObject, _ := contract.ObjectAt(project, "project")
	projectNumber, numberErr := contract.PositiveInteger(projectObject["number"])
	if graph["repo"] == nil || projectObject["id"] != scope.ID || numberErr != nil || projectNumber != scope.Number || projectObject["owner_login"] != scope.Owner || projectObject["owner_type"] != scope.OwnerType || projectObject["host"] != scope.Host {
		return nil, nil, fmt.Errorf("fake source was read with a foreign Project target")
	}
	graphCopy, err := contract.Clone(graph)
	if err != nil {
		return nil, nil, err
	}
	projectCopy, err := contract.Clone(project)
	return graphCopy, projectCopy, err
}

func (f *fakeRebalanceProvider) ReadLinkedPullRequests(_ context.Context, numbers []int64, policy contract.Object) (contract.Object, error) {
	state, err := f.readState()
	if err != nil {
		return nil, err
	}
	linked, _ := contract.ObjectAt(state, "linked_by_issue")
	byIssue := contract.Object{}
	for _, number := range numbers {
		key := strconv.FormatInt(number, 10)
		byIssue[key] = linked[key]
	}
	return contract.Object{
		"repo":             rebalanceTestRepo.Object(),
		"issue_numbers":    int64Values(numbers),
		"by_issue":         byIssue,
		"comments":         []any{},
		"pull_requests":    []any{},
		"unresolved_links": []any{},
		"provenance":       contract.Object{"live": true, "complete": true},
		"policy":           policy,
	}, nil
}

func (f *fakeRebalanceProvider) SetProjectField(_ context.Context, target native.ProjectItemScope, fieldID string, value any, operationID string) (contract.Object, error) {
	state, err := f.readState()
	if err != nil {
		return nil, err
	}
	graph, _ := contract.ObjectAt(state, "graph")
	project, _ := contract.ObjectAt(state, "project")
	issue, item, err := sourceTarget(graph, project, target.IssueNumber)
	if err != nil || target.Project.ID != "PVT_sample" || target.ItemID != item["item_id"] || fieldID == "" {
		return nil, errorsForTest("foreign field target")
	}
	fieldName := ""
	definitions, _ := contract.ObjectAt(project, "fields_by_name")
	for name, raw := range definitions {
		definition, _ := raw.(map[string]any)
		if definition["id"] == fieldID {
			fieldName = name
		}
	}
	if fieldName == "" {
		return nil, errorsForTest("unknown field definition")
	}
	objectOrEmpty(item, "field_values")[fieldName] = value
	objectOrEmpty(issue, "field_values")[fieldName] = value
	objectOrEmpty(objectOrEmpty(issue, "project_item"), "field_values")[fieldName] = value
	if err := f.recordWrite(state, "field:"+target.ItemID+":"+fieldName+":"+operationID); err != nil {
		return nil, err
	}
	return contract.Object{"clientMutationId": operationID, "projectV2Item": contract.Object{"id": target.ItemID}}, nil
}

func (f *fakeRebalanceProvider) UpdateIssue(_ context.Context, number int64, changes contract.Object) (contract.Object, error) {
	state, err := f.readState()
	if err != nil {
		return nil, err
	}
	graph, _ := contract.ObjectAt(state, "graph")
	project, _ := contract.ObjectAt(state, "project")
	issue, item, err := sourceTarget(graph, project, number)
	if err != nil || changes["state"] != "closed" || issue["state"] != "OPEN" {
		return nil, errorsForTest("foreign issue close target")
	}
	issue["state"], item["state"] = "CLOSED", "CLOSED"
	if err := f.recordWrite(state, fmt.Sprintf("close:%d", number)); err != nil {
		return nil, err
	}
	return contract.Object{"number": number, "state": "closed", "html_url": issue["url"], "id": int64(5000 + number), "node_id": issue["id"]}, nil
}

func (f *fakeRebalanceProvider) ArchiveProjectItem(_ context.Context, target native.ProjectItemScope, archived bool, operationID string) (contract.Object, error) {
	state, err := f.readState()
	if err != nil {
		return nil, err
	}
	graph, _ := contract.ObjectAt(state, "graph")
	project, _ := contract.ObjectAt(state, "project")
	issue, item, err := sourceTarget(graph, project, target.IssueNumber)
	if err != nil || target.ItemID != item["item_id"] || issue["id"] == "" || !archived || item["state"] != "CLOSED" {
		return nil, errorsForTest("foreign or ineligible archive target")
	}
	item["archived"] = true
	objectOrEmpty(issue, "project_item")["archived"] = true
	if err := f.recordWrite(state, "archive:"+target.ItemID+":"+operationID); err != nil {
		return nil, err
	}
	return contract.Object{"clientMutationId": operationID, "item": contract.Object{"id": target.ItemID}}, nil
}

func errorsForTest(message string) error { return fmt.Errorf("%s", message) }

func (f *fakeRebalanceProvider) recordWrite(state contract.Object, operation string) error {
	writes, _ := contract.Integer(state["writes"])
	state["writes"] = writes + 1
	log, _ := state["write_log"].([]any)
	state["write_log"] = append(log, operation)
	if f.driftIssue > 0 && writes == 0 {
		graph, _ := contract.ObjectAt(state, "graph")
		project, _ := contract.ObjectAt(state, "project")
		issue, item, err := sourceTarget(graph, project, f.driftIssue)
		if err != nil {
			return err
		}
		objectOrEmpty(item, "field_values")["Queue Order"] = 91.25
		objectOrEmpty(issue, "field_values")["Queue Order"] = 91.25
		objectOrEmpty(objectOrEmpty(issue, "project_item"), "field_values")["Queue Order"] = 91.25
	}
	if f.driftFinalIssue > 0 && f.totalWrites > 0 && writes+1 == f.totalWrites {
		graph, _ := contract.ObjectAt(state, "graph")
		project, _ := contract.ObjectAt(state, "project")
		issue, item, err := sourceTarget(graph, project, f.driftFinalIssue)
		if err != nil {
			return err
		}
		objectOrEmpty(item, "field_values")["Queue Order"] = 92.25
		objectOrEmpty(issue, "field_values")["Queue Order"] = 92.25
		objectOrEmpty(objectOrEmpty(issue, "project_item"), "field_values")["Queue Order"] = 92.25
	}
	if f.driftAddMember && writes == 0 {
		if err := addFakeProjectMember(state, 99); err != nil {
			return err
		}
	}
	if f.driftRemoveMember && writes == 0 {
		removeFakeProjectMember(state, 4)
	}
	if f.driftLinked && writes == 0 {
		linked, _ := contract.ObjectAt(state, "linked_by_issue")
		linked["3"] = contract.Object{"number": int64(33), "state": "closed", "is_merged": false, "is_draft": false}
	}
	if f.armCrashAfterWrite && writes == 0 {
		state["crash_next_read"] = true
	}
	return f.saveState(state)
}

func addFakeProjectMember(state contract.Object, number int64) error {
	graph, _ := contract.ObjectAt(state, "graph")
	project, _ := contract.ObjectAt(state, "project")
	issues, _ := contract.Objects(graph, "issues")
	items, _ := contract.Objects(project, "items")
	var issueSource, itemSource contract.Object
	for _, issue := range issues {
		if n, _ := contract.PositiveInteger(issue["number"]); n == 3 {
			issueSource = issue
		}
	}
	for _, item := range items {
		if n, _ := contract.PositiveInteger(item["number"]); n == 3 {
			itemSource = item
		}
	}
	issue, err := contract.Clone(issueSource)
	if err != nil {
		return err
	}
	item, err := contract.Clone(itemSource)
	if err != nil {
		return err
	}
	id, itemID := fmt.Sprintf("ISSUE_NODE_%d", number), fmt.Sprintf("ITEM_%d", number)
	url := fmt.Sprintf("https://github.com/sample-org/sample-project/issues/%d", number)
	issue["number"], issue["id"], issue["url"], issue["title"] = number, id, url, "Task: Added member"
	item["number"], item["item_id"], item["url"], item["title"] = number, itemID, url, issue["title"]
	issue["project_item"] = contract.Object{"item_id": itemID, "content_type": "Issue", "field_values": item["field_values"], "archived": item["archived"]}
	issues = append(issues, issue)
	items = append(items, item)
	graph["issues"], project["items"] = objectValues(issues), objectValues(items)
	return nil
}

func removeFakeProjectMember(state contract.Object, number int64) {
	graph, _ := contract.ObjectAt(state, "graph")
	project, _ := contract.ObjectAt(state, "project")
	issues, _ := contract.Objects(graph, "issues")
	items, _ := contract.Objects(project, "items")
	keptIssues := []contract.Object{}
	for _, issue := range issues {
		n, _ := contract.PositiveInteger(issue["number"])
		if n != number {
			keptIssues = append(keptIssues, issue)
		}
	}
	keptItems := []contract.Object{}
	for _, item := range items {
		n, _ := contract.PositiveInteger(item["number"])
		if n != number {
			keptItems = append(keptItems, item)
		}
	}
	graph["issues"], project["items"] = objectValues(keptIssues), objectValues(keptItems)
}

func objectValues(objects []contract.Object) []any {
	values := make([]any, len(objects))
	for index, object := range objects {
		values[index] = object
	}
	return values
}

func (f *fakeRebalanceProvider) writeCount(t *testing.T) int64 {
	t.Helper()
	state, err := f.readState()
	if err != nil {
		t.Fatal(err)
	}
	count, err := contract.Integer(state["writes"])
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func newRebalanceFixture(t *testing.T) (*fakeRebalanceProvider, native.ProjectScope, snapshot.QueuePolicy, contract.Object, contract.Object) {
	t.Helper()
	file, err := os.Open("../../testdata/planning/baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	base, err := contract.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	graph, _ := contract.ObjectAt(base, "issue_graph")
	project, _ := contract.ObjectAt(base, "project_snapshot")
	projectMeta, _ := contract.ObjectAt(project, "project")
	projectMeta["owner_type"], projectMeta["host"], projectMeta["closed"], projectMeta["public"] = "Organization", "github.com", false, true
	graph["project"], graph["provenance"] = projectMeta, contract.Object{"live": true, "complete": true, "issue_state": "all", "repository_node_id": "REPO_NODE_SAMPLE", "project_joins_live": true}
	project["provenance"] = contract.Object{"live": true, "complete": true}
	fields := contract.Object{}
	fields["Status"] = contract.Object{"id": "FIELD_STATUS", "name": "Status", "data_type": "SINGLE_SELECT", "options_by_name": contract.Object{"Todo": contract.Object{"id": "OPT_TODO", "name": "Todo"}, "In Progress": contract.Object{"id": "OPT_PROGRESS", "name": "In Progress"}, "Done": contract.Object{"id": "OPT_DONE", "name": "Done"}}}
	fields["Priority"] = contract.Object{"id": "FIELD_PRIORITY", "name": "Priority", "data_type": "SINGLE_SELECT", "options_by_name": contract.Object{"Now": contract.Object{"id": "OPT_NOW", "name": "Now"}, "Next": contract.Object{"id": "OPT_NEXT", "name": "Next"}, "Later": contract.Object{"id": "OPT_LATER", "name": "Later"}}}
	fields["Queue Order"] = contract.Object{"id": "FIELD_ORDER", "name": "Queue Order", "data_type": "NUMBER", "options_by_name": contract.Object{}}
	project["fields_by_name"] = fields
	issues, _ := contract.Objects(graph, "issues")
	items, _ := contract.Objects(project, "items")
	for index, issue := range issues {
		number, _ := contract.PositiveInteger(issue["number"])
		item := items[index]
		id := fmt.Sprintf("ITEM_%d", number)
		url := fmt.Sprintf("https://github.com/sample-org/sample-project/issues/%d", number)
		issue["id"], issue["url"], issue["labels"], issue["repository"] = fmt.Sprintf("ISSUE_NODE_%d", number), url, []any{}, "sample-org/sample-project"
		item["content_type"], item["repository"], item["archived"] = "Issue", "sample-org/sample-project", false
		item["url"] = url
		item["item_id"] = id
		issue["in_project"] = true
		issue["field_values"] = item["field_values"]
		issue["project_item"] = contract.Object{"item_id": id, "content_type": "Issue", "field_values": item["field_values"], "archived": false}
	}
	graph["issues"], project["items"] = graph["issues"], project["items"]
	policy := snapshot.QueuePolicy{StatusField: "Status", PriorityField: "Priority", OrderField: "Queue Order", DoneStatuses: []string{"Done"}, Priorities: map[string]string{"Now": "Now", "Next": "Next", "Later": "Later"}}
	decisions, _ := contract.ObjectAt(base, "rebalance_payload")
	options := contract.Object{"queue_mode": "minimal", "rank_step": int64(10), "status_values": contract.Object{"Done": "Done"}}
	return &fakeRebalanceProvider{state: contract.Object{"graph": graph, "project": project, "linked_by_issue": contract.Object{}, "writes": int64(0), "write_log": []any{}}}, native.ProjectScope{Host: "github.com", Owner: "sample-org", OwnerType: "Organization", Number: 7, ID: "PVT_sample"}, policy, decisions, options
}

func preparedRebalance(t *testing.T, provider RebalanceProvider, scope native.ProjectScope, policy snapshot.QueuePolicy, decisions, options contract.Object) contract.Plan {
	t.Helper()
	plan, err := PrepareRebalance(context.Background(), provider, rebalanceTestRepo, scope, policy, decisions, options, contract.Object{}, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func rebalanceEngine(root string, provider RebalanceProvider, repo contract.Repository) apply.Engine {
	return apply.Engine{Root: root, Repository: repo, Command: RebalanceCommand, Adapter: RebalanceAdapter{Provider: provider}}
}

func writePlan(t *testing.T, path string, plan contract.Plan) {
	t.Helper()
	data, err := contract.Canonical(plan.Object())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRebalanceAppliesReviewedPrimitivesAndTerminalReplay(t *testing.T) {
	provider, scope, policy, decisions, options := newRebalanceFixture(t)
	plan := preparedRebalance(t, provider, scope, policy, decisions, options)
	ops, err := (RebalanceAdapter{}).Operations(plan)
	if err != nil || len(ops) == 0 {
		t.Fatal("expected a nonempty reviewed primitive plan", len(ops), err)
	}
	root := t.TempDir()
	result, err := rebalanceEngine(root, provider, rebalanceTestRepo).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" || provider.writeCount(t) != int64(len(ops)) {
		t.Fatal("reviewed rebalance did not complete exactly once", result, err, provider.writeCount(t), len(ops))
	}
	if _, err := rebalanceEngine(root, provider, rebalanceTestRepo).Apply(context.Background(), plan.Object()); err != nil || provider.writeCount(t) != int64(len(ops)) {
		t.Fatal("terminal replay repeated a reviewed mutation", err)
	}
	state, _ := provider.readState()
	project, _ := contract.ObjectAt(state, "project")
	items, _ := contract.Objects(project, "items")
	items[2]["field_values"].(map[string]any)["Queue Order"] = 17.75
	if err := provider.saveState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := rebalanceEngine(root, provider, rebalanceTestRepo).Apply(context.Background(), plan.Object()); err == nil || provider.writeCount(t) != int64(len(ops)) {
		t.Fatal("terminal replay hid unrelated full-inventory drift", err)
	}
}

func TestRebalanceRejectsUntouchedInventoryDriftAfterFirstPrimitive(t *testing.T) {
	for _, mode := range []string{"queue-field", "member-added", "member-removed"} {
		t.Run(mode, func(t *testing.T) {
			provider, scope, policy, decisions, options := newRebalanceFixture(t)
			switch mode {
			case "queue-field":
				provider.driftIssue = 4
			case "member-added":
				provider.driftAddMember = true
			case "member-removed":
				provider.driftRemoveMember = true
			}
			plan := preparedRebalance(t, provider, scope, policy, decisions, options)
			result, err := rebalanceEngine(t.TempDir(), provider, rebalanceTestRepo).Apply(context.Background(), plan.Object())
			if err == nil || result != nil || provider.writeCount(t) != 1 {
				t.Fatal("unrelated full-source drift allowed continued writes", result, err, provider.writeCount(t))
			}
		})
	}
}

func TestRebalanceRejectsDriftIntroducedByLastPrimitive(t *testing.T) {
	provider, scope, policy, decisions, options := newRebalanceFixture(t)
	plan := preparedRebalance(t, provider, scope, policy, decisions, options)
	provider.totalWrites = int64(len(plan.Operations))
	provider.driftFinalIssue = 1
	result, err := rebalanceEngine(t.TempDir(), provider, rebalanceTestRepo).Apply(context.Background(), plan.Object())
	if err == nil || result != nil || provider.writeCount(t) != int64(len(plan.Operations)) {
		t.Fatal("terminal inventory drift introduced by the last primitive reported success", result, err, provider.writeCount(t))
	}
}

func TestRebalanceRejectsAlteredPrimitiveBeforeAnyWrite(t *testing.T) {
	provider, scope, policy, decisions, options := newRebalanceFixture(t)
	plan := preparedRebalance(t, provider, scope, policy, decisions, options)
	plan.Operations[0].After["value"] = "forged"
	plan.SHA256, _ = contract.Digest(plan.Unsigned())
	if _, err := rebalanceEngine(t.TempDir(), provider, rebalanceTestRepo).Apply(context.Background(), plan.Object()); err == nil || provider.writeCount(t) != 0 {
		t.Fatal("altered operation bypassed domain recomputation", err)
	}
}

func TestRebalanceRequiresMergedPRSourceAndProjectsArchivePrerequisites(t *testing.T) {
	provider, scope, policy, decisions, options := newRebalanceFixture(t)
	decisions["now_issue_numbers"] = []any{int64(1), int64(2)}
	decisions["next_issue_numbers"] = []any{}
	decisions["archive_issue_numbers"] = []any{int64(3)}
	options["linked_pr_policy"] = contract.Object{"marker_prefix": "Linked PR:", "pr_number_pattern": `#(?P<number>[1-9][0-9]*)`}
	state, _ := provider.readState()
	state["linked_by_issue"] = contract.Object{"3": contract.Object{"number": int64(33), "state": "MERGED", "is_merged": true, "is_draft": false}}
	_ = provider.saveState(state)
	plan := preparedRebalance(t, provider, scope, policy, decisions, options)
	ops, err := (RebalanceAdapter{}).Operations(plan)
	if err != nil {
		t.Fatal(err)
	}
	var archiveOps []contract.Operation
	for _, op := range ops {
		if op.Target["issue_number"] == int64(3) {
			archiveOps = append(archiveOps, op)
		}
	}
	if len(archiveOps) != 3 || archiveOps[0].Kind != "issue-close" || archiveOps[1].Kind != "project-field" || archiveOps[2].Kind != "project-item-archive" {
		t.Fatalf("archive must be ordered close, Done status, archive: %#v", archiveOps)
	}
	if archiveOps[1].Before["issue_state"] != "CLOSED" || archiveOps[2].Before["issue_state"] != "CLOSED" || !same(archiveOps[2].Before["status"], contract.Object{"present": true, "value": "Done"}) {
		t.Fatalf("archive prerequisites did not use projected typed before-state: %#v", archiveOps)
	}
	root := t.TempDir()
	result, err := rebalanceEngine(root, provider, rebalanceTestRepo).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatal("merged linked-PR archive did not complete", err, result)
	}
}

func TestRebalanceLinkedPRDriftStopsAfterFirstPrimitive(t *testing.T) {
	provider, scope, policy, decisions, options := newRebalanceFixture(t)
	decisions["now_issue_numbers"] = []any{int64(1), int64(2)}
	decisions["next_issue_numbers"] = []any{}
	decisions["archive_issue_numbers"] = []any{int64(3)}
	options["linked_pr_policy"] = contract.Object{"marker_prefix": "Linked PR:", "pr_number_pattern": `#(?P<number>[1-9][0-9]*)`}
	state, _ := provider.readState()
	state["linked_by_issue"] = contract.Object{"3": contract.Object{"number": int64(33), "state": "MERGED", "is_merged": true, "is_draft": false}}
	_ = provider.saveState(state)
	provider.driftLinked = true
	plan := preparedRebalance(t, provider, scope, policy, decisions, options)
	result, err := rebalanceEngine(t.TempDir(), provider, rebalanceTestRepo).Apply(context.Background(), plan.Object())
	if err == nil || result != nil || provider.writeCount(t) != 1 {
		t.Fatal("linked PR source drift did not stop further writes", result, err, provider.writeCount(t))
	}
}

func TestRebalanceFreshProcessCrashAfterAcknowledgementDoesNotReplay(t *testing.T) {
	if os.Getenv("STEWARD_REBALANCE_CRASH_STATE") != "" {
		statePath := os.Getenv("STEWARD_REBALANCE_CRASH_STATE")
		planPath := os.Getenv("STEWARD_REBALANCE_CRASH_PLAN")
		file, err := os.Open(planPath)
		if err != nil {
			os.Exit(91)
		}
		raw, err := contract.Decode(file)
		file.Close()
		if err != nil {
			os.Exit(91)
		}
		plan, err := contract.ParsePlan(raw)
		if err != nil {
			os.Exit(91)
		}
		provider := &fakeRebalanceProvider{statePath: statePath, armCrashAfterWrite: true, exitOnCrashRead: true}
		_, applyErr := rebalanceEngine(os.Getenv("STEWARD_REBALANCE_CRASH_ROOT"), provider, plan.Repository).Apply(context.Background(), plan.Object())
		if applyErr != nil {
			fmt.Fprintln(os.Stderr, applyErr)
			os.Exit(94)
		}
		os.Exit(92)
	}
	provider, scope, policy, decisions, options := newRebalanceFixture(t)
	plan := preparedRebalance(t, provider, scope, policy, decisions, options)
	if len(plan.Operations) < 2 || plan.Operations[0].Kind != "project-field" {
		t.Fatal("fresh-process recovery fixture requires a first field primitive and a later operation")
	}
	root := t.TempDir()
	statePath, planPath := filepath.Join(t.TempDir(), "remote.json"), filepath.Join(t.TempDir(), "plan.json")
	state, _ := provider.readState()
	if err := provider.saveState(state); err != nil {
		t.Fatal(err)
	}
	fileProvider := &fakeRebalanceProvider{state: state, statePath: statePath}
	if err := fileProvider.saveState(state); err != nil {
		t.Fatal(err)
	}
	writePlan(t, planPath, plan)
	child := exec.Command(os.Args[0], "-test.run=^TestRebalanceFreshProcessCrashAfterAcknowledgementDoesNotReplay$")
	child.Env = append(os.Environ(), "STEWARD_REBALANCE_CRASH_STATE="+statePath, "STEWARD_REBALANCE_CRASH_PLAN="+planPath, "STEWARD_REBALANCE_CRASH_ROOT="+root)
	childOutput, childErr := child.CombinedOutput()
	if childErr == nil {
		t.Fatalf("child process did not interrupt after its durable acknowledgement: %s", childOutput)
	} else if exit, ok := childErr.(*exec.ExitError); !ok || exit.ExitCode() != 73 {
		t.Fatalf("child exited at the wrong boundary: %v, output %s", childErr, childOutput)
	}
	resumed := &fakeRebalanceProvider{statePath: statePath}
	if resumed.writeCount(t) != 1 {
		t.Fatal("interrupted child did not perform exactly the first primitive")
	}
	result, err := rebalanceEngine(root, resumed, rebalanceTestRepo).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" || resumed.writeCount(t) != int64(len(plan.Operations)) {
		t.Fatal("fresh-process resume repeated or skipped a primitive", result, err, resumed.writeCount(t), len(plan.Operations))
	}
	state, _ = resumed.readState()
	log, _ := contract.Array(state, "write_log")
	if len(log) != len(plan.Operations) {
		t.Fatalf("fresh-process resume has duplicated write log entries: %#v", log)
	}
}
