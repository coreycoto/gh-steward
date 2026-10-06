package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/progress"
)

func executionTestPolicy() ExecutionPolicy {
	return ExecutionPolicy{
		Statuses:    ExecutionStatusPolicy{Done: "Done", Active: "In Progress", Todo: "Todo"},
		StatusField: "Status", PRLinkMarkerPrefix: "<!-- execution-pr-link:",
		PRLinkNumberPattern:     `PR: #(?P<number>[1-9][0-9]*)`,
		LinkedIssueMarkerPrefix: "<!-- execution-linked-issue:", LinkStateMarkerPrefix: "<!-- execution-link-state:",
	}
}

func executionTestProject() ProjectScope {
	return ProjectScope{Host: "github.com", Owner: "example", OwnerType: "Organization", Number: 1, ID: "PVT_backlog", Title: "Backlog"}
}

func executionTestPR(state string) contract.Object {
	merged := state == "MERGED"
	var mergedAt any
	if merged {
		mergedAt = "2026-10-04T12:00:00Z"
	}
	return contract.Object{
		"id": "PR_3", "number": int64(3), "title": "Reviewed change",
		"url":   "https://github.com/example/widgets/pull/3",
		"body":  "Issue: #17\nCloses #17\n<!-- execution-linked-issue:17 -->\nLink state: auto-link-expected\n<!-- execution-link-state:auto-link-expected -->",
		"state": state, "is_draft": false, "is_merged": merged, "merged_at": mergedAt,
		"head_branch": "codex/issue-17", "base_branch": "main", "head_sha": strings.Repeat("a", 40),
		"head_repository_owner": "example", "head_repository": testRepo().Object(),
		"closing_issue_numbers": []any{int64(17)},
	}
}

func executionTestRawInventory(policy ExecutionPolicy, selector ExecutionSelector, prState string, withProject, linkedComment bool) contract.Object {
	repo := testRepo()
	issue := contract.Object{
		"id": "I_17", "number": int64(17), "title": "Task: execution workflow", "body": "Work item body",
		"state": "CLOSED", "url": "https://github.com/example/widgets/issues/17", "labels": []any{}, "milestone": nil,
		"project_items": []any{},
	}
	issueInventory := contract.Object{
		"repo": repo.Object(), "issues": []any{issue}, "issue_count": 1, "generated_at": "2026-10-04T12:00:00Z",
		"provenance": contract.Object{"live": true, "complete": true, "issues_live": true, "issues_source": "github_api", "issue_state": "all", "repository_node_id": "R_widgets", "relationships_requested": false},
	}
	pr := executionTestPR(prState)
	comments := []any{}
	byIssue := contract.Object{"17": nil}
	pullRequests := []any{}
	pullRequestsSource := "not_requested"
	if linkedComment {
		comments = append(comments, contract.Object{
			"id": int64(31), "issue_number": int64(17),
			"body": "Linked PR: https://github.com/example/widgets/pull/3\nPR: #3\nHead: codex/issue-17\nBase: main\nLinked at: 2026-10-04T12:00:00Z\n" + executionTestPRLinkMarker(policy, 17),
			"url":  "https://github.com/example/widgets/issues/17#issuecomment-31",
		})
		byIssue["17"] = contract.Object{"id": "PR_3", "number": int64(3)}
		pullRequests = append(pullRequests, contract.Object{"id": "PR_3", "number": int64(3)})
		pullRequestsSource = "github_api"
	}
	linked := contract.Object{
		"repo": repo.Object(), "issue_numbers": []any{int64(17)}, "by_issue": byIssue,
		"comments": comments, "pull_requests": pullRequests, "unresolved_links": []any{}, "generated_at": "2026-10-04T12:00:00Z",
		"provenance": contract.Object{"live": true, "complete": true, "comments_source": "github_api", "pull_requests_source": pullRequestsSource},
	}
	var project any
	projectSource := "not_requested"
	if withProject {
		projectSource = "github_project_api"
		project = contract.Object{
			"repo":    repo.Object(),
			"project": contract.Object{"id": "PVT_backlog", "number": int64(1), "title": "Backlog", "owner_login": "example", "owner_type": "Organization", "host": "github.com"},
			"fields_by_name": contract.Object{
				"Status": contract.Object{"id": "FIELD_status", "name": "Status", "data_type": "SINGLE_SELECT", "options_by_name": contract.Object{
					"Todo": contract.Object{"id": "OPT_status_todo", "name": "Todo"}, "In Progress": contract.Object{"id": "OPT_status_progress", "name": "In Progress"}, "Done": contract.Object{"id": "OPT_status_done", "name": "Done"},
				}},
			},
			"items": []any{}, "generated_at": "2026-10-04T12:00:00Z",
			"provenance": contract.Object{"live": true, "complete": true, "source": "github_project_api"},
		}
	}
	var pullRequest any
	pullRequestSource := "not_requested"
	if selector.PullRequestNumber > 0 || linkedComment {
		pullRequest = pr
		pullRequestSource = "github_api"
	}
	return contract.Object{
		"repo": repo.Object(), "repository_node_id": "R_widgets", "default_branch": "main",
		"issue_inventory": issueInventory, "pull_request": pullRequest, "linked_pr_inventory": linked,
		"branch_inventory": nil, "project_inventory": project,
		"provenance": contract.Object{
			"live": true, "complete": true, "issue_state": "all", "repository_node_id": "R_widgets", "default_branch_source": "github_api",
			"pull_request_source": pullRequestSource, "linked_pr_source": "github_api", "branch_source": "not_requested", "project_source": projectSource,
			"selector": selector.Object(), "policy": policy.Object(),
		},
	}
}

func executionTestPRLinkMarker(policy ExecutionPolicy, issue int64) string {
	return fmt.Sprintf("%s issue=%d -->", policy.PRLinkMarkerPrefix, issue)
}

type executionRemoteState struct {
	Inventory contract.Object `json:"inventory"`
	Reads     int64           `json:"reads"`
	Writes    int64           `json:"writes"`
	NextID    int64           `json:"next_id"`
}

type executionTestProvider struct {
	statePath          string
	memory             *executionRemoteState
	failAfterWrite     bool
	crashBeforeAck     bool
	crashInventoryRead int64
}

func newExecutionTestProvider(inventory contract.Object) *executionTestProvider {
	return &executionTestProvider{memory: &executionRemoteState{Inventory: inventory, NextID: 50}}
}

func (f *executionTestProvider) load() (*executionRemoteState, error) {
	if f.statePath == "" {
		if f.memory == nil {
			return nil, errors.New("execution test remote has no state")
		}
		return f.memory, nil
	}
	data, err := os.ReadFile(f.statePath)
	if err != nil {
		return nil, err
	}
	decoded, err := contract.Decode(strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	state := &executionRemoteState{}
	state.Inventory, err = contract.ObjectAt(decoded, "inventory")
	if err != nil {
		return nil, err
	}
	state.Reads, err = contract.Integer(decoded["reads"])
	if err != nil {
		return nil, err
	}
	state.Writes, err = contract.Integer(decoded["writes"])
	if err != nil {
		return nil, err
	}
	state.NextID, err = contract.Integer(decoded["next_id"])
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (f *executionTestProvider) save(state *executionRemoteState) error {
	if f.statePath == "" {
		f.memory = state
		return nil
	}
	object := contract.Object{"inventory": state.Inventory, "reads": state.Reads, "writes": state.Writes, "next_id": state.NextID}
	data, err := contract.Canonical(object)
	if err != nil {
		return err
	}
	return os.WriteFile(f.statePath, data, 0600)
}

func (f *executionTestProvider) seedFile(path string, inventory contract.Object) error {
	f.statePath = path
	return f.save(&executionRemoteState{Inventory: inventory, NextID: 50})
}

func (f *executionTestProvider) ExecutionInventory(_ context.Context, request ExecutionInventoryRequest) (contract.Object, error) {
	state, err := f.load()
	if err != nil {
		return nil, err
	}
	state.Reads++
	if err := f.save(state); err != nil {
		return nil, err
	}
	if f.crashInventoryRead > 0 && state.Reads == f.crashInventoryRead {
		os.Exit(73)
	}
	result, err := contract.Clone(state.Inventory)
	if err != nil {
		return nil, err
	}
	provenance, err := contract.ObjectAt(result, "provenance")
	if err != nil {
		return nil, err
	}
	provenance["selector"] = request.Selector.Object()
	provenance["policy"] = request.Policy.Object()
	return result, nil
}

func (f *executionTestProvider) mutate(change func(*executionRemoteState) (contract.Object, error)) (contract.Object, error) {
	state, err := f.load()
	if err != nil {
		return nil, err
	}
	state.Writes++
	result, changeErr := change(state)
	if err := f.save(state); err != nil {
		return nil, err
	}
	if f.crashBeforeAck {
		os.Exit(73)
	}
	if changeErr != nil {
		return nil, changeErr
	}
	if f.failAfterWrite {
		return nil, errors.New("provider accepted execution write but response was lost")
	}
	return result, nil
}

func executionRemoteIssue(inventory contract.Object, number int64) (contract.Object, error) {
	graph, err := contract.ObjectAt(inventory, "issue_inventory")
	if err != nil {
		return nil, err
	}
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		n, err := contract.PositiveInteger(issue["number"])
		if err == nil && n == number {
			return issue, nil
		}
	}
	return nil, fmt.Errorf("issue #%d is absent from execution test inventory", number)
}

func executionIssueMutationAck(issue contract.Object) contract.Object {
	labels := []any{}
	for _, raw := range issue["labels"].([]any) {
		labels = append(labels, contract.Object{"name": raw})
	}
	var milestone any
	if issue["milestone"] != nil {
		milestone = contract.Object{"title": issue["milestone"]}
	}
	return contract.Object{
		"node_id": issue["id"], "number": issue["number"], "title": issue["title"], "body": issue["body"],
		"state": strings.ToLower(issue["state"].(string)), "html_url": issue["url"], "labels": labels, "milestone": milestone,
	}
}

func (f *executionTestProvider) ReopenIssue(_ context.Context, _ string, number int64) (contract.Object, error) {
	return f.mutate(func(state *executionRemoteState) (contract.Object, error) {
		issue, err := executionRemoteIssue(state.Inventory, number)
		if err != nil {
			return nil, err
		}
		issue["state"] = "OPEN"
		return executionIssueMutationAck(issue), nil
	})
}

func (f *executionTestProvider) CloseIssue(_ context.Context, _ string, number int64) (contract.Object, error) {
	return f.mutate(func(state *executionRemoteState) (contract.Object, error) {
		issue, err := executionRemoteIssue(state.Inventory, number)
		if err != nil {
			return nil, err
		}
		issue["state"] = "CLOSED"
		return executionIssueMutationAck(issue), nil
	})
}

func (f *executionTestProvider) CreateIssueComment(_ context.Context, nonce string, number int64, body string) (contract.Object, error) {
	return f.mutate(func(state *executionRemoteState) (contract.Object, error) {
		_, err := executionRemoteIssue(state.Inventory, number)
		if err != nil {
			return nil, err
		}
		marker, err := native.OperationMarker(nonce)
		if err != nil {
			return nil, err
		}
		body += "\n\n" + marker
		id := state.NextID
		state.NextID++
		url := fmt.Sprintf("https://github.com/example/widgets/issues/%d#issuecomment-%d", number, id)
		comment := contract.Object{"id": id, "issue_number": number, "body": body, "url": url}
		linked, err := contract.ObjectAt(state.Inventory, "linked_pr_inventory")
		if err != nil {
			return nil, err
		}
		comments, err := contract.Objects(linked, "comments")
		if err != nil {
			return nil, err
		}
		comments = append(comments, comment)
		linked["comments"] = backlogObjectsAsAny(comments)
		pr, err := contract.ObjectAt(state.Inventory, "pull_request")
		if err != nil {
			return nil, err
		}
		byIssue, err := contract.ObjectAt(linked, "by_issue")
		if err != nil {
			return nil, err
		}
		byIssue[strconv.FormatInt(number, 10)] = contract.Object{"id": pr["id"], "number": pr["number"]}
		linked["pull_requests"] = []any{contract.Object{"id": pr["id"], "number": pr["number"]}}
		prov, _ := contract.ObjectAt(linked, "provenance")
		prov["pull_requests_source"] = "github_api"
		return contract.Object{"id": id, "body": body, "html_url": url}, nil
	})
}

func (f *executionTestProvider) UpdateIssueComment(_ context.Context, nonce string, number, commentID int64, body string) (contract.Object, error) {
	return f.mutate(func(state *executionRemoteState) (contract.Object, error) {
		linked, err := contract.ObjectAt(state.Inventory, "linked_pr_inventory")
		if err != nil {
			return nil, err
		}
		comments, err := contract.Objects(linked, "comments")
		if err != nil {
			return nil, err
		}
		for _, comment := range comments {
			id, _ := contract.PositiveInteger(comment["id"])
			if id == commentID && comment["issue_number"] == number {
				comment["body"] = body
				pr, _ := contract.ObjectAt(state.Inventory, "pull_request")
				byIssue, _ := contract.ObjectAt(linked, "by_issue")
				byIssue[strconv.FormatInt(number, 10)] = contract.Object{"id": pr["id"], "number": pr["number"]}
				linked["pull_requests"] = []any{contract.Object{"id": pr["id"], "number": pr["number"]}}
				prov, _ := contract.ObjectAt(linked, "provenance")
				prov["pull_requests_source"] = "github_api"
				return contract.Object{"id": commentID, "body": body, "html_url": comment["url"]}, nil
			}
		}
		return nil, errors.New("execution test comment was absent")
	})
}

func (f *executionTestProvider) AddProjectIssue(_ context.Context, nonce, projectID, issueNodeID string) (contract.Object, error) {
	return f.mutate(func(state *executionRemoteState) (contract.Object, error) {
		project, err := contract.ObjectAt(state.Inventory, "project_inventory")
		if err != nil {
			return nil, err
		}
		metadata, _ := contract.ObjectAt(project, "project")
		if metadata["id"] != projectID {
			return nil, errors.New("execution test Project identity changed")
		}
		issue, err := executionRemoteIssue(state.Inventory, 17)
		if err != nil || issue["id"] != issueNodeID {
			return nil, errors.New("execution test Project issue identity changed")
		}
		itemID := "ITEM_17"
		items, err := contract.Objects(project, "items")
		if err != nil {
			return nil, err
		}
		items = append(items, contract.Object{"item_id": itemID, "number": int64(17), "field_values": contract.Object{}, "archived": false})
		project["items"] = backlogObjectsAsAny(items)
		return contract.Object{
			"clientMutationId": nonce, "id": itemID, "isArchived": false,
			"content":     contract.Object{"id": issueNodeID, "number": int64(17), "url": issue["url"]},
			"fieldValues": contract.Object{"nodes": []any{}, "pageInfo": contract.Object{"hasNextPage": false, "endCursor": nil}},
		}, nil
	})
}

func (f *executionTestProvider) SetProjectField(_ context.Context, nonce, projectID, itemID string, field ProjectField, value ProjectFieldValue) (contract.Object, error) {
	return f.mutate(func(state *executionRemoteState) (contract.Object, error) {
		project, err := contract.ObjectAt(state.Inventory, "project_inventory")
		if err != nil {
			return nil, err
		}
		metadata, _ := contract.ObjectAt(project, "project")
		if metadata["id"] != projectID || value.Text == nil || value.Number != nil || value.Clear {
			return nil, errors.New("execution test Project field request is malformed")
		}
		items, err := contract.Objects(project, "items")
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if item["item_id"] == itemID {
				fields, _ := contract.ObjectAt(item, "field_values")
				fields[string(field)] = *value.Text
				return contract.Object{"clientMutationId": nonce, "projectV2Item": contract.Object{"id": itemID}}, nil
			}
		}
		return nil, errors.New("execution test Project item was absent")
	})
}

func (f *executionTestProvider) counts() (int64, int64, error) {
	state, err := f.load()
	if err != nil {
		return 0, 0, err
	}
	return state.Reads, state.Writes, nil
}

func executionPlan(t *testing.T, selector ExecutionSelector, prState string, withProject, linkedComment bool) (contract.Plan, *executionTestProvider) {
	t.Helper()
	policy := executionTestPolicy()
	provider := newExecutionTestProvider(executionTestRawInventory(policy, selector, prState, withProject, linkedComment))
	plan, err := PrepareExecutionSync(context.Background(), provider, testRepo(), selector, policy, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return plan, provider
}

func executionEngine(root string, provider ExecutionProvider) apply.Engine {
	return apply.Engine{Root: root, Repository: testRepo(), Command: ExecutionSyncCommand, Adapter: ExecutionSync{Provider: provider}}
}

func TestResolveExecutionIssueNumberCrossChecksCompleteClosingReferences(t *testing.T) {
	policy := executionTestPolicy()
	for _, test := range []struct {
		name    string
		body    string
		closing []any
		want    int64
		wantErr bool
	}{
		{name: "marker precedence", body: "<!-- execution-linked-issue:17 --> Closes #18", closing: []any{int64(17), int64(18)}, want: 17},
		{name: "single complete reference", body: "No explicit issue", closing: []any{int64(17)}, want: 17},
		{name: "body conflicts with references", body: "Issue: #17", closing: []any{int64(18)}, wantErr: true},
		{name: "ambiguous references", body: "No explicit issue", closing: []any{int64(17), int64(18)}, wantErr: true},
		{name: "duplicate reference", body: "Closes #17", closing: []any{int64(17), int64(17)}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pr := contract.Object{"body": test.body, "closing_issue_numbers": test.closing}
			got, err := ResolveExecutionIssueNumber(pr, policy)
			if (err != nil) != test.wantErr || !test.wantErr && got != test.want {
				t.Fatalf("ResolveExecutionIssueNumber()=(%d,%v), want %d error=%v", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestExecutionReDerivesIssueProjectMembershipAndStatusAsSeparateWrites(t *testing.T) {
	selector := ExecutionSelector{IssueNumber: 17, Project: ptrProject(executionTestProject())}
	plan, provider := executionPlan(t, selector, "OPEN", true, true)
	ops, err := (ExecutionSync{}).Operations(plan)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []string{"issue-reopen", "project-membership-add", "project-field-set"}
	if len(ops) != len(wantKinds) {
		t.Fatalf("execution primitives = %#v", ops)
	}
	for i, want := range wantKinds {
		if ops[i].Kind != want {
			t.Fatalf("primitive %d kind = %q, want %q", i, ops[i].Kind, want)
		}
	}
	root := t.TempDir()
	result, err := executionEngine(root, provider).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatalf("Apply() = (%v,%v)", result, err)
	}
	_, writes, err := provider.counts()
	if err != nil || writes != 3 {
		t.Fatalf("native writes = %d, err %v; want separate issue, membership, status writes", writes, err)
	}
	if _, err := executionEngine(root, provider).Apply(context.Background(), plan.Object()); err != nil {
		t.Fatal("terminal replay failed", err)
	}
	_, replayWrites, _ := provider.counts()
	if replayWrites != writes {
		t.Fatalf("completed writes replayed: %d -> %d", writes, replayWrites)
	}
}

func TestExecutionMergedCompletionRequiresDefaultBranchClosingEvidence(t *testing.T) {
	policy := executionTestPolicy()
	selector := ExecutionSelector{IssueNumber: 17, Project: ptrProject(executionTestProject())}
	for _, test := range []struct {
		name              string
		issueState        string
		baseBranch        string
		closingIssues     []any
		projectMembership bool
		wantKinds         []string
	}{
		{name: "closed issue remains done", issueState: "CLOSED", baseBranch: "main", closingIssues: []any{}, wantKinds: []string{"project-membership-add", "project-field-set"}},
		{name: "open issue with default-base closing reference", issueState: "OPEN", baseBranch: "main", closingIssues: []any{int64(17)}, wantKinds: []string{"issue-close", "project-membership-add", "project-field-set"}},
		{name: "reference-only PR does not add membership", issueState: "OPEN", baseBranch: "main", closingIssues: []any{}, wantKinds: []string{}},
		{name: "non-default PR preserves existing membership and status", issueState: "OPEN", baseBranch: "release/next", closingIssues: []any{int64(17)}, projectMembership: true, wantKinds: []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			inventory := executionTestRawInventory(policy, selector, "MERGED", true, true)
			graph, _ := contract.ObjectAt(inventory, "issue_inventory")
			issues, _ := contract.Objects(graph, "issues")
			issues[0]["state"] = test.issueState
			pr, _ := contract.ObjectAt(inventory, "pull_request")
			pr["base_branch"] = test.baseBranch
			pr["closing_issue_numbers"] = test.closingIssues
			if test.projectMembership {
				project, _ := contract.ObjectAt(inventory, "project_inventory")
				project["items"] = []any{contract.Object{
					"item_id": "ITEM_17", "number": int64(17),
					"field_values": contract.Object{"Status": "In Progress"}, "archived": false,
				}}
			}
			provider := newExecutionTestProvider(inventory)
			plan, err := PrepareExecutionSync(context.Background(), provider, testRepo(), selector, policy, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal("prepare merged execution plan", err)
			}
			ops, err := (ExecutionSync{}).Operations(plan)
			if err != nil {
				t.Fatal("rederive merged execution operations", err)
			}
			gotKinds := make([]string, 0, len(ops))
			for _, op := range ops {
				gotKinds = append(gotKinds, op.Kind)
			}
			if !reflect.DeepEqual(gotKinds, test.wantKinds) {
				t.Fatalf("merged operations = %v, want %v: %#v", gotKinds, test.wantKinds, ops)
			}
			if test.issueState == "OPEN" && test.baseBranch == "main" && len(test.closingIssues) == 1 && ops[len(ops)-1].After["value"] != "Done" {
				t.Fatalf("verified default-base closing reference did not set configured done status: %#v", ops)
			}
			if len(ops) == 0 {
				project, _ := contract.ObjectAt(plan.Data["inventory"].(map[string]any), "project_inventory")
				items, _ := contract.Objects(project, "items")
				if test.projectMembership && (len(items) != 1 || items[0]["field_values"].(map[string]any)["Status"] != "In Progress") {
					t.Fatalf("partial merged PR changed existing Project state: %#v", items)
				}
			}
		})
	}
}

func TestExecutionRejectsObsoleteMergedCompletionPlanAndJournal(t *testing.T) {
	policy := executionTestPolicy()
	selector := ExecutionSelector{IssueNumber: 17, Project: ptrProject(executionTestProject())}
	inventory := executionTestRawInventory(policy, selector, "MERGED", true, true)
	graph, _ := contract.ObjectAt(inventory, "issue_inventory")
	issues, _ := contract.Objects(graph, "issues")
	issues[0]["state"] = "OPEN"
	pr, _ := contract.ObjectAt(inventory, "pull_request")
	pr["body"] = "Issue: #17\nRefs #17\n<!-- execution-linked-issue:17 -->\nLink state: auto-link-expected\n<!-- execution-link-state:auto-link-expected -->"
	pr["closing_issue_numbers"] = []any{}
	provider := newExecutionTestProvider(inventory)
	current, err := PrepareExecutionSync(context.Background(), provider, testRepo(), selector, policy, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal("prepare reference-only execution plan", err)
	}
	obsoleteOps := obsoleteMergedCompletionOperations()
	capturedAt, err := time.Parse(time.RFC3339Nano, current.CapturedAt)
	if err != nil {
		t.Fatal(err)
	}
	obsolete, err := contract.PreparePlan(current.Command, current.Repository, current.Sources, current.Data, obsoleteOps, capturedAt)
	if err != nil {
		t.Fatal("construct obsolete v2 plan", err)
	}

	root := t.TempDir()
	journal, err := progress.Open(root, obsolete.Repository, obsolete.Command, obsolete.SHA256)
	if err != nil {
		t.Fatal("open obsolete plan journal", err)
	}
	for _, op := range obsoleteOps {
		intent := contract.Object{"id": op.ID, "kind": op.Kind, "target": op.Target, "before": op.Before, "after": op.After}
		if _, err := journal.Execute(op.ID, intent, func(operationID string) (contract.Object, error) {
			return contract.Object{"operation_id": operationID}, nil
		}); err != nil {
			journal.Close()
			t.Fatal("seed completed legacy journal", err)
		}
	}
	journalPath := journal.Path()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	journalBefore, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executionEngine(root, provider).Apply(context.Background(), obsolete.Object()); err == nil {
		t.Fatal("obsolete inferred close/status operations were accepted")
	}
	journalAfter, err := os.ReadFile(journalPath)
	if err != nil || !bytes.Equal(journalBefore, journalAfter) {
		t.Fatalf("obsolete journal was rewritten while rejecting its plan: err=%v", err)
	}
	_, writes, err := provider.counts()
	if err != nil || writes != 0 {
		t.Fatalf("obsolete plan resumed writes: writes=%d err=%v", writes, err)
	}
}

func obsoleteMergedCompletionOperations() []contract.Operation {
	membershipRef := contract.Object{"from_operation": "execution:project-membership"}
	return []contract.Operation{
		{
			ID: "execution:issue-state", Kind: "issue-close",
			Target: contract.Object{"issue_number": int64(17), "issue_id": "I_17"},
			Before: contract.Object{"issue_number": int64(17), "issue_id": "I_17", "state": "OPEN"},
			After:  contract.Object{"issue_number": int64(17), "issue_id": "I_17", "state": "CLOSED"},
		},
		{
			ID: "execution:project-membership", Kind: "project-membership-add",
			Target: contract.Object{"project_id": "PVT_backlog", "issue_number": int64(17), "issue_node_id": "I_17"},
			Before: contract.Object{"present": false, "project_id": "PVT_backlog", "issue_number": int64(17), "issue_node_id": "I_17"},
			After:  contract.Object{"present": true, "project_id": "PVT_backlog", "issue_number": int64(17), "issue_node_id": "I_17"},
		},
		{
			ID: "execution:project-status", Kind: "project-field-set",
			Target: contract.Object{"project_id": "PVT_backlog", "issue_number": int64(17), "issue_id": "I_17", "item_ref": membershipRef, "field_name": "Status"},
			Before: contract.Object{"present": true, "item_ref": membershipRef, "field_name": "Status", "value": contract.Object{"from_membership": "execution:project-membership", "field_name": "Status"}},
			After:  contract.Object{"present": true, "item_ref": membershipRef, "field_name": "Status", "value": "Done"},
		},
	}
}

func TestExecutionDirectPRMarkerUpsertUsesCapturedAcknowledgement(t *testing.T) {
	selector := ExecutionSelector{PullRequestNumber: 3, SkipProjectSync: true}
	plan, provider := executionPlan(t, selector, "OPEN", false, false)
	ops, err := (ExecutionSync{}).Operations(plan)
	if err != nil || len(ops) != 2 || ops[0].Kind != "issue-comment-upsert" || ops[1].Kind != "issue-reopen" {
		t.Fatalf("derived direct PR operations = %#v, err %v", ops, err)
	}
	root := t.TempDir()
	if _, err := executionEngine(root, provider).Apply(context.Background(), plan.Object()); err != nil {
		t.Fatal("direct PR marker upsert failed", err)
	}
	_, writes, _ := provider.counts()
	if writes != 2 {
		t.Fatalf("marker comment and issue-state transition should be separate native primitives, got %d writes", writes)
	}
}

func TestExecutionPreflightRejectsRepositoryAndPullRequestHeadDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(contract.Object)
	}{
		{name: "repository incarnation", mutate: func(inv contract.Object) {
			inv["repository_node_id"] = "R_recreated"
			graph, _ := contract.ObjectAt(inv, "issue_inventory")
			prov, _ := contract.ObjectAt(graph, "provenance")
			prov["repository_node_id"] = "R_recreated"
		}},
		{name: "pull request head SHA", mutate: func(inv contract.Object) {
			pr, _ := contract.ObjectAt(inv, "pull_request")
			pr["head_sha"] = strings.Repeat("b", 40)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			selector := ExecutionSelector{IssueNumber: 17, SkipProjectSync: true}
			plan, provider := executionPlan(t, selector, "OPEN", false, true)
			state, err := provider.load()
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(state.Inventory)
			if err := provider.save(state); err != nil {
				t.Fatal(err)
			}
			if _, err := executionEngine(t.TempDir(), provider).Apply(context.Background(), plan.Object()); err == nil {
				t.Fatal("drifted complete execution inventory passed preflight")
			}
			_, writes, _ := provider.counts()
			if writes != 0 {
				t.Fatalf("drifted inventory authorized %d writes", writes)
			}
		})
	}
}

func ptrProject(project ProjectScope) *ProjectScope { return &project }

func TestExecutionFreshProcessUnknownRequiresACKAndRecoversACKWithoutReplay(t *testing.T) {
	for _, mode := range []string{"before-ack", "after-ack"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			selector := ExecutionSelector{IssueNumber: 17, SkipProjectSync: true}
			plan, _ := executionPlan(t, selector, "OPEN", false, true)
			inventory := executionTestRawInventory(executionTestPolicy(), selector, "OPEN", false, true)
			provider := &executionTestProvider{}
			if err := provider.seedFile(filepath.Join(root, "remote.json"), inventory); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestExecutionRestartChild$")
			child.Env = append(os.Environ(), "STEWARD_EXECUTION_TEST_ROOT="+root, "STEWARD_EXECUTION_TEST_MODE="+mode)
			if err := child.Run(); err == nil {
				t.Fatal("child did not interrupt the apply")
			}
			resumer := &executionTestProvider{statePath: filepath.Join(root, "remote.json")}
			result, err := executionEngine(root, resumer).Apply(context.Background(), plan.Object())
			_, writes, countErr := resumer.counts()
			if countErr != nil {
				t.Fatal(countErr)
			}
			if mode == "before-ack" {
				if err == nil || writes != 1 {
					t.Fatalf("unacknowledged unknown should fail closed without replay: result=%v err=%v writes=%d", result, err, writes)
				}
			} else if err != nil || result["status"] != "completed" || writes != 1 {
				t.Fatalf("exact ACK should recover in fresh process without replay: result=%v err=%v writes=%d", result, err, writes)
			}
		})
	}
}

func TestExecutionRestartChild(t *testing.T) {
	root := os.Getenv("STEWARD_EXECUTION_TEST_ROOT")
	mode := os.Getenv("STEWARD_EXECUTION_TEST_MODE")
	if root == "" || mode == "" {
		return
	}
	selector := ExecutionSelector{IssueNumber: 17, SkipProjectSync: true}
	plan, _ := executionPlan(t, selector, "OPEN", false, true)
	provider := &executionTestProvider{statePath: filepath.Join(root, "remote.json")}
	if mode == "before-ack" {
		provider.crashBeforeAck = true
	} else {
		// Apply preflight is read 1; the after-state read happens only after the
		// exact native ACK has already been fsynced into the journal.
		provider.crashInventoryRead = 2
	}
	_, _ = executionEngine(root, provider).Apply(context.Background(), plan.Object())
	t.Fatal("configured execution child returned without interruption")
}
