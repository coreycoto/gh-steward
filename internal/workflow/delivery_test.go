package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

const (
	deliveryTestHeadSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	deliveryTestBaseSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	deliveryTestMergeSHA = "cccccccccccccccccccccccccccccccccccccccc"
)

type deliveryTestProvider struct {
	*governanceFixtureProvider
	crashDeliveryRead     int64
	failCreateAfterAccept bool
}

func deliveryTestPolicy(open bool, comment bool) contract.Object {
	state := "CLOSED"
	status := "Done"
	if open {
		state, status = "OPEN", "In Progress"
	}
	var workComment any
	if comment {
		workComment = contract.Object{"marker": "<!-- gh-steward:work-start:1 -->", "body": "Work started for PR {{pull_request_number}}: {{pull_request_url}}"}
	}
	return contract.Object{
		"issue_number": int64(1), "issue_state": state, "project": executionTestProject().Object(),
		"status_field": "Status", "target_status": status, "work_comment": workComment,
	}
}

func deliveryTestDraft() native.PullRequestDraft {
	return native.PullRequestDraft{Title: "Reviewed delivery", Body: "Reviewed work item #1", HeadBranch: "codex/issue-1", BaseBranch: "main", HeadSHA: deliveryTestHeadSHA, BaseSHA: deliveryTestBaseSHA}
}

func deliveryTestLinkedBody() string {
	return "Issue: #1\nCloses #1\n<!-- execution-linked-issue:1 -->\nLink state: auto-link-expected\n<!-- execution-link-state:auto-link-expected -->"
}

func deliveryTestFinishPolicy(keep bool) contract.Object {
	return contract.Object{
		"pull_request_number": int64(3), "merge_method": "squash", "keep_branch": keep,
		"execution_policy": executionTestPolicy().Object(), "execution_state": deliveryTestPolicy(false, false),
	}
}

func newDeliveryTestProvider(initialIssueState string, autoDelete bool) *deliveryTestProvider {
	inventory := backlogTestInventory(true)
	inventory["labels"] = []any{}
	graph, _ := contract.ObjectAt(inventory, "issue_inventory")
	issues, _ := contract.Objects(graph, "issues")
	issues[0]["state"] = initialIssueState
	state := contract.Object{
		"inventory": inventory, "reads": int64(0), "writes": int64(0), "fail_first_write": false,
		"delivery_reads": int64(0), "delivery_writes": int64(0), "auto_delete_branch": autoDelete,
		"branches": contract.Object{
			"main":          contract.Object{"repo": testRepo().Object(), "repository_node_id": "R_widgets", "id": "REF_main", "name": "main", "sha": deliveryTestBaseSHA},
			"codex/issue-1": contract.Object{"repo": testRepo().Object(), "repository_node_id": "R_widgets", "id": "REF_codex_issue_1", "name": "codex/issue-1", "sha": deliveryTestHeadSHA},
		},
		"head_pull_requests": []any{}, "pull_request": nil,
		"required_checks": []any{contract.Object{"name": "Tests", "bucket": "pass", "state": "SUCCESS", "workflow": "CI", "link": "https://github.com/example/widgets/actions/runs/1"}},
		"base_dependents": []any{}, "next_pull_request_number": int64(3),
	}
	base := &backlogTestProvider{state: state}
	return &deliveryTestProvider{governanceFixtureProvider: &governanceFixtureProvider{backlogTestProvider: base}}
}

func deliveryTestRepositoryNode() contract.Object {
	repository := testRepo().Object()
	repository["id"] = "R_widgets"
	return repository
}

func deliveryTestPullRequest(state string) contract.Object {
	merged, mergeState, mergeSHA := false, "CLEAN", any(nil)
	if state == "MERGED" {
		merged, mergeState, mergeSHA = true, "MERGED", deliveryTestMergeSHA
	}
	return contract.Object{
		"number": int64(3), "repository": testRepo().FullName(), "id": "PR_3",
		"url": testRepo().URL + "/pull/3", "baseRefName": "main", "headRefName": "codex/issue-1",
		"state": state, "mergeStateStatus": mergeState, "title": "Reviewed delivery", "body": deliveryTestLinkedBody(),
		"isDraft": false, "merged": merged, "headRefOid": deliveryTestHeadSHA, "reviewDecision": "",
		"merge_commit_sha": mergeSHA, "labels": []any{}, "author": nil, "head_repository": testRepo().Object(),
		"head_repository_node_id": "R_widgets", "closing_issue_numbers": []any{int64(1)},
	}
}

func (f *deliveryTestProvider) saveDeliveryCounter(state contract.Object, key string, count int64) error {
	state[key] = count
	return f.backlogTestProvider.save(state)
}

func (f *deliveryTestProvider) DeliveryInventory(ctx context.Context, request DeliveryInventoryRequest) (contract.Object, error) {
	canonical, parsed, err := parseDeliveryPolicy(request.Kind, request.Policy, testRepo())
	if err != nil || !same(canonical, request.Policy) {
		return nil, errors.New("delivery test provider received a noncanonical request")
	}
	statePolicy, err := contract.ObjectAt(canonical, "execution_state")
	if err != nil {
		return nil, err
	}
	_, govPolicy, err := parseGovernancePolicy(GovernanceExecutionState, statePolicy, testRepo())
	if err != nil {
		return nil, err
	}
	backlogRequest, err := governanceInventoryRequest(GovernanceExecutionState, statePolicy, govPolicy, testRepo(), "")
	if err != nil {
		return nil, err
	}
	state, err := f.backlogTestProvider.load()
	if err != nil {
		return nil, err
	}
	if len(backlogRequest.Projects) == 0 {
		inventory, err := contract.ObjectAt(state, "inventory")
		if err != nil {
			return nil, err
		}
		inventory["projects"] = []any{}
		if err := f.backlogTestProvider.save(state); err != nil {
			return nil, err
		}
	}
	reads, err := contract.Integer(state["delivery_reads"])
	if err != nil {
		return nil, err
	}
	reads++
	if err := f.saveDeliveryCounter(state, "delivery_reads", reads); err != nil {
		return nil, err
	}
	if f.crashDeliveryRead > 0 && reads == f.crashDeliveryRead {
		os.Exit(73)
	}
	backlog, err := f.BacklogInventory(ctx, backlogRequest)
	if err != nil {
		return nil, err
	}
	state, err = f.backlogTestProvider.load()
	if err != nil {
		return nil, err
	}
	branches, err := contract.ObjectAt(state, "branches")
	if err != nil {
		return nil, err
	}
	result := contract.Object{
		"repo": testRepo().Object(), "repository_node_id": "R_widgets", "default_branch": "main",
		"auto_delete_branch": state["auto_delete_branch"], "backlog_inventory": backlog,
		"head_branch": nil, "base_branch": nil, "head_pull_requests": nil, "pull_request": nil,
		"required_checks": []any{}, "base_dependents": nil,
		"provenance": contract.Object{"live": true, "complete": true, "source": "github_api", "repository_node_id": "R_widgets", "request": request.Object()},
	}
	if request.Kind == DeliveryOpenPR {
		draft, _ := contract.ObjectAt(canonical, "draft")
		headName, _ := contract.Nonempty(draft, "head_branch")
		baseName, _ := contract.Nonempty(draft, "base_branch")
		head, headExists := branches[headName]
		base, baseExists := branches[baseName]
		if !headExists || head == nil || !baseExists || base == nil {
			return nil, errors.New("open delivery branch is missing")
		}
		result["head_branch"], result["base_branch"] = head, base
		result["head_pull_requests"] = contract.Object{"repo": testRepo().Object(), "repository_node_id": "R_widgets", "head_branch": headName,
			"pull_requests": state["head_pull_requests"], "provenance": contract.Object{"live": true, "complete": true, "source": "github_api"}}
	} else {
		pr, err := contract.ObjectAt(state, "pull_request")
		if err != nil {
			return nil, err
		}
		result["pull_request"] = pr
		if rawBranch, exists := branches[fmt.Sprint(pr["headRefName"])]; exists {
			result["head_branch"] = rawBranch
		}
		result["required_checks"] = state["required_checks"]
		if parsed.keepBranch == false {
			result["base_dependents"] = contract.Object{"repo": testRepo().Object(), "repository_node_id": "R_widgets", "base_branch": pr["headRefName"],
				"pull_requests": state["base_dependents"], "provenance": contract.Object{"live": true, "complete": true, "source": "github_api"}}
		}
	}
	return result, nil
}

func deliveryTestPRRow(nonce string, draft native.PullRequestDraft, number int64) contract.Object {
	marker, _ := native.OperationMarker(nonce)
	body := draft.Body + "\n\n" + marker
	repository := deliveryTestRepositoryNode()
	return contract.Object{
		"id": fmt.Sprintf("PR_%d", number), "number": number, "url": fmt.Sprintf("%s/pull/%d", testRepo().URL, number),
		"title": draft.Title, "body": body, "state": "OPEN", "isDraft": draft.Draft, "merged": false,
		"headRefName": draft.HeadBranch, "baseRefName": draft.BaseBranch, "headRefOid": draft.HeadSHA, "baseRefOid": draft.BaseSHA,
		"maintainerCanModify": false, "repository": repository, "headRepository": repository,
	}
}

func (f *deliveryTestProvider) mutateDelivery(update func(contract.Object) (contract.Object, error)) (contract.Object, error) {
	state, err := f.backlogTestProvider.load()
	if err != nil {
		return nil, err
	}
	writes, err := contract.Integer(state["writes"])
	if err != nil {
		return nil, err
	}
	deliveryWrites, err := contract.Integer(state["delivery_writes"])
	if err != nil {
		return nil, err
	}
	state["writes"], state["delivery_writes"] = writes+1, deliveryWrites+1
	result, updateErr := update(state)
	if err := f.backlogTestProvider.save(state); err != nil {
		return nil, err
	}
	return result, updateErr
}

func (f *deliveryTestProvider) CreatePullRequest(_ context.Context, nonce string, draft native.PullRequestDraft, repositoryNodeID string) (contract.Object, error) {
	if repositoryNodeID != "R_widgets" {
		return nil, errors.New("unexpected delivery repository identity")
	}
	ack, err := f.mutateDelivery(func(state contract.Object) (contract.Object, error) {
		branches, _ := contract.ObjectAt(state, "branches")
		head, _ := contract.ObjectAt(branches, draft.HeadBranch)
		base, _ := contract.ObjectAt(branches, draft.BaseBranch)
		if head["sha"] != draft.HeadSHA || base["sha"] != draft.BaseSHA {
			return nil, errors.New("delivery test branch changed")
		}
		number, _ := contract.PositiveInteger(state["next_pull_request_number"])
		pr := deliveryTestPRRow(nonce, draft, number)
		rows, _ := contract.Objects(contract.Object{"rows": state["head_pull_requests"]}, "rows")
		rows = append(rows, pr)
		state["head_pull_requests"] = backlogObjectsAsAny(rows)
		state["next_pull_request_number"] = number + 1
		return contract.Object{"clientMutationId": nonce, "pullRequest": pr}, nil
	})
	if err != nil {
		return nil, err
	}
	if f.failCreateAfterAccept {
		return nil, errors.New("delivery provider accepted pull request creation but response was lost")
	}
	return ack, nil
}

func (f *deliveryTestProvider) MergePullRequest(_ context.Context, number int64, headSHA, _ string) (contract.Object, error) {
	return f.mutateDelivery(func(state contract.Object) (contract.Object, error) {
		pr, err := contract.ObjectAt(state, "pull_request")
		if err != nil || pr["number"] != number || pr["headRefOid"] != headSHA || pr["merged"] != false || pr["state"] != "OPEN" {
			return nil, errors.New("delivery test merge identity changed")
		}
		pr["state"], pr["merged"], pr["mergeStateStatus"], pr["merge_commit_sha"] = "MERGED", true, "MERGED", deliveryTestMergeSHA
		if pr["baseRefName"] == "main" {
			issue, err := executionRemoteIssue(state["inventory"].(map[string]any), 1)
			if err != nil {
				return nil, err
			}
			issue["state"] = "CLOSED"
		}
		if state["auto_delete_branch"] == true {
			branches, _ := contract.ObjectAt(state, "branches")
			delete(branches, fmt.Sprint(pr["headRefName"]))
		}
		return contract.Object{"merged": true, "sha": deliveryTestMergeSHA}, nil
	})
}

func (f *deliveryTestProvider) DeleteBranch(_ context.Context, nonce, name, expectedSHA, repositoryNodeID string) (contract.Object, error) {
	return f.mutateDelivery(func(state contract.Object) (contract.Object, error) {
		if repositoryNodeID != "R_widgets" {
			return nil, errors.New("delivery test deletion has another repository identity")
		}
		branches, _ := contract.ObjectAt(state, "branches")
		branch, err := contract.ObjectAt(branches, name)
		if err != nil || branch["sha"] != expectedSHA {
			return nil, errors.New("delivery test branch lease is stale")
		}
		delete(branches, name)
		stdout := "To " + testRepo().URL + ".git\n-\t:refs/heads/" + name + "\t[deleted]\nDone\n"
		return contract.Object{"exit_code": int64(0), "stdout": stdout, "stderr": "", "remote_url": testRepo().URL + ".git", "ref": "refs/heads/" + name, "expected_sha": expectedSHA}, nil
	})
}

func (f *deliveryTestProvider) SetIssueState(_ context.Context, _ string, number int64, target string) (contract.Object, error) {
	return f.backlogTestProvider.mutate(func(inventory contract.Object) (contract.Object, error) {
		issue, err := executionRemoteIssue(inventory, number)
		if err != nil {
			return nil, err
		}
		issue["state"] = target
		labels := []any{}
		for _, label := range issue["labels"].([]any) {
			labels = append(labels, contract.Object{"name": label})
		}
		return contract.Object{"node_id": issue["id"], "number": issue["number"], "title": issue["title"], "body": issue["body"],
			"state": strings.ToLower(target), "html_url": issue["url"], "labels": labels, "milestone": nil}, nil
	})
}

func (f *deliveryTestProvider) AddProjectIssue(_ context.Context, nonce, projectID, issueNodeID string) (contract.Object, error) {
	return f.backlogTestProvider.mutate(func(inventory contract.Object) (contract.Object, error) {
		issue, err := executionRemoteIssue(inventory, 1)
		if err != nil || issue["id"] != issueNodeID {
			return nil, errors.New("delivery Project membership changed issue identity")
		}
		projects, _ := contract.Objects(inventory, "projects")
		if len(projects) != 1 || projects[0]["project"].(map[string]any)["id"] != projectID {
			return nil, errors.New("delivery Project membership changed Project identity")
		}
		project := projects[0]
		items, _ := contract.Objects(project, "items")
		for _, item := range items {
			if item["number"] == int64(1) {
				return nil, errors.New("delivery Project membership already exists")
			}
		}
		items = append(items, contract.Object{"item_id": "ITEM_1", "number": int64(1), "field_values": contract.Object{}, "archived": false})
		project["items"] = backlogObjectsAsAny(items)
		return contract.Object{"clientMutationId": nonce, "id": "ITEM_1", "isArchived": false,
			"content":     contract.Object{"id": issueNodeID, "number": int64(1), "url": issue["url"]},
			"fieldValues": contract.Object{"nodes": []any{}, "pageInfo": contract.Object{"hasNextPage": false, "endCursor": nil}}}, nil
	})
}

func (f *deliveryTestProvider) SetProjectField(_ context.Context, nonce, projectID, itemID string, field ProjectField, value ProjectFieldValue) (contract.Object, error) {
	return f.backlogTestProvider.mutate(func(inventory contract.Object) (contract.Object, error) {
		projects, _ := contract.Objects(inventory, "projects")
		if len(projects) != 1 || projects[0]["project"].(map[string]any)["id"] != projectID {
			return nil, errors.New("delivery Project field changed Project identity")
		}
		items, _ := contract.Objects(projects[0], "items")
		for _, item := range items {
			if item["item_id"] != itemID {
				continue
			}
			fields, _ := contract.ObjectAt(item, "field_values")
			if value.Text != nil {
				fields[string(field)] = *value.Text
			} else if value.Number != nil {
				fields[string(field)] = *value.Number
			} else if value.Clear {
				delete(fields, string(field))
			} else {
				return nil, errors.New("delivery Project field value is empty")
			}
			return contract.Object{"clientMutationId": nonce, "projectV2Item": contract.Object{"id": itemID}}, nil
		}
		return nil, errors.New("delivery Project field item is missing")
	})
}

func deliveryTestEngine(root string, provider DeliveryProvider) apply.Engine {
	return apply.Engine{Root: root, Repository: testRepo(), Command: DeliveryCommand, Adapter: Delivery{Provider: provider}}
}

func setDeliveryFinishState(t *testing.T, provider *deliveryTestProvider, autoDelete bool) {
	t.Helper()
	state, err := provider.backlogTestProvider.load()
	if err != nil {
		t.Fatal(err)
	}
	state["auto_delete_branch"] = autoDelete
	state["pull_request"] = deliveryTestPullRequest("OPEN")
	if err := provider.backlogTestProvider.save(state); err != nil {
		t.Fatal(err)
	}
}

func resetDeliveryTestState(t *testing.T, provider *deliveryTestProvider) {
	t.Helper()
	state, err := provider.backlogTestProvider.load()
	if err != nil {
		t.Fatal(err)
	}
	state["reads"], state["writes"], state["delivery_reads"], state["delivery_writes"] = int64(0), int64(0), int64(0), int64(0)
	if err := provider.backlogTestProvider.save(state); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryOpenPRComposesLateBoundGovernanceAndDoesNotReplay(t *testing.T) {
	provider := newDeliveryTestProvider("CLOSED", false)
	policy := contract.Object{"draft": deliveryTestDraft().Object(), "execution_state": deliveryTestPolicy(true, true)}
	plan, err := PrepareDelivery(context.Background(), provider, testRepo(), DeliveryOpenPR, policy, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []string{"pull-request-create", "issue-state-set", "project-membership-add", "project-field-set", "issue-comment-upsert"}
	if len(plan.Operations) != len(wantKinds) {
		t.Fatalf("open-pr operations = %#v", plan.Operations)
	}
	for i, kind := range wantKinds {
		if plan.Operations[i].Kind != kind {
			t.Fatalf("open-pr operation %d kind = %q; want %q", i, plan.Operations[i].Kind, kind)
		}
	}
	resetDeliveryTestState(t, provider)
	root := t.TempDir()
	result, err := deliveryTestEngine(root, provider).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatalf("open-pr apply = %#v, %v", result, err)
	}
	state, err := provider.backlogTestProvider.load()
	if err != nil {
		t.Fatal(err)
	}
	writes, _ := contract.Integer(state["writes"])
	if writes != int64(len(wantKinds)) {
		t.Fatalf("open-pr writes = %d; want %d", writes, len(wantKinds))
	}
	issue, _ := executionRemoteIssue(state["inventory"].(map[string]any), 1)
	if issue["state"] != "OPEN" {
		t.Fatalf("open-pr issue state = %v", issue["state"])
	}
	project := state["inventory"].(map[string]any)["projects"].([]any)[0].(map[string]any)
	items := project["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["field_values"].(map[string]any)["Status"] != "In Progress" {
		t.Fatalf("open-pr Project state = %#v", items)
	}
	comments, _ := contract.Objects(state["inventory"].(map[string]any), "comments")
	if len(comments) != 1 || !strings.Contains(comments[0]["body"].(string), "PR 3: https://github.com/example/widgets/pull/3") || !strings.Contains(comments[0]["body"].(string), "<!-- gh-steward:work-start:1 -->") {
		t.Fatalf("late-bound work comment = %#v", comments)
	}
	if _, err := deliveryTestEngine(root, provider).Apply(context.Background(), plan.Object()); err != nil {
		t.Fatalf("terminal open-pr replay = %v", err)
	}
	state, _ = provider.backlogTestProvider.load()
	writes, _ = contract.Integer(state["writes"])
	if writes != int64(len(wantKinds)) {
		t.Fatalf("terminal replay repeated writes: %d", writes)
	}
}

func TestDeliveryFinishMergesThenProjectsAndLeasesBranchDeletion(t *testing.T) {
	provider := newDeliveryTestProvider("OPEN", false)
	setDeliveryFinishState(t, provider, false)
	plan, err := PrepareDelivery(context.Background(), provider, testRepo(), DeliveryFinish, deliveryTestFinishPolicy(false), time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []string{"pull-request-merge", "project-membership-add", "project-field-set", "branch-delete"}
	if len(plan.Operations) != len(wantKinds) {
		t.Fatalf("finish operations = %#v", plan.Operations)
	}
	for i, kind := range wantKinds {
		if plan.Operations[i].Kind != kind {
			t.Fatalf("finish operation %d kind = %q; want %q", i, plan.Operations[i].Kind, kind)
		}
	}
	resetDeliveryTestState(t, provider)
	root := t.TempDir()
	result, err := deliveryTestEngine(root, provider).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatalf("finish apply = %#v, %v", result, err)
	}
	state, err := provider.backlogTestProvider.load()
	if err != nil {
		t.Fatal(err)
	}
	writes, _ := contract.Integer(state["writes"])
	if writes != int64(len(wantKinds)) {
		t.Fatalf("finish writes = %d; want %d", writes, len(wantKinds))
	}
	pr := state["pull_request"].(map[string]any)
	if pr["state"] != "MERGED" || pr["merged"] != true {
		t.Fatalf("finish pull request state = %#v", pr)
	}
	issue, _ := executionRemoteIssue(state["inventory"].(map[string]any), 1)
	if issue["state"] != "CLOSED" {
		t.Fatalf("merge did not project the selected auto-closed issue: %v", issue["state"])
	}
	branches, _ := contract.ObjectAt(state, "branches")
	if _, exists := branches["codex/issue-1"]; exists {
		t.Fatal("leased branch deletion did not remove the exact reviewed ref")
	}
}

func TestDeliveryAutoDeleteAndBranchRetentionGates(t *testing.T) {
	provider := newDeliveryTestProvider("OPEN", true)
	setDeliveryFinishState(t, provider, true)
	if _, err := PrepareDelivery(context.Background(), provider, testRepo(), DeliveryFinish, deliveryTestFinishPolicy(true), time.Now()); err == nil {
		t.Fatal("keep_branch=true was accepted with repository auto-delete enabled")
	}
	state, _ := provider.backlogTestProvider.load()
	if writes, _ := contract.Integer(state["writes"]); writes != 0 {
		t.Fatalf("retention preflight wrote before rejection: %d", writes)
	}
	policy := deliveryTestFinishPolicy(false)
	plan, err := PrepareDelivery(context.Background(), provider, testRepo(), DeliveryFinish, policy, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range plan.Operations {
		if op.Kind == "branch-delete" {
			t.Fatal("server auto-delete plan added a second branch deletion")
		}
	}
	result, err := deliveryTestEngine(t.TempDir(), provider).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatalf("server auto-delete apply = %#v, %v", result, err)
	}
	state, _ = provider.backlogTestProvider.load()
	branches, _ := contract.ObjectAt(state, "branches")
	if _, exists := branches["codex/issue-1"]; exists {
		t.Fatal("server auto-delete merge did not produce the reviewed absent-branch after-state")
	}
	for _, rows := range []any{
		[]any{contract.Object{"id": "PR_4", "number": int64(4), "url": testRepo().URL + "/pull/4", "baseRefName": "codex/issue-1", "headRefName": "stacked", "state": "OPEN", "isDraft": true, "merged": false}},
	} {
		dependentProvider := newDeliveryTestProvider("OPEN", false)
		setDeliveryFinishState(t, dependentProvider, false)
		state, _ := dependentProvider.backlogTestProvider.load()
		state["base_dependents"] = rows
		dependentProvider.backlogTestProvider.save(state)
		if _, err := PrepareDelivery(context.Background(), dependentProvider, testRepo(), DeliveryFinish, deliveryTestFinishPolicy(false), time.Now()); err == nil {
			t.Fatal("head branch with an open dependent pull request was accepted for removal")
		}
	}
}

func TestDeliveryCannotRemoveRepositoryDefaultHeadBranch(t *testing.T) {
	for _, autoDelete := range []bool{false, true} {
		t.Run(fmt.Sprintf("auto_delete_%t", autoDelete), func(t *testing.T) {
			provider := newDeliveryTestProvider("OPEN", autoDelete)
			setDeliveryFinishState(t, provider, autoDelete)
			state, err := provider.backlogTestProvider.load()
			if err != nil {
				t.Fatal(err)
			}
			pr, _ := contract.ObjectAt(state, "pull_request")
			pr["headRefName"], pr["headRefOid"] = "main", deliveryTestBaseSHA
			if err := provider.backlogTestProvider.save(state); err != nil {
				t.Fatal(err)
			}
			if _, err := PrepareDelivery(context.Background(), provider, testRepo(), DeliveryFinish, deliveryTestFinishPolicy(false), time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)); err == nil || !strings.Contains(err.Error(), "default branch") {
				t.Fatalf("default-branch removal was not rejected during prepare: %v", err)
			}
			state, err = provider.backlogTestProvider.load()
			if err != nil {
				t.Fatal(err)
			}
			if writes, _ := contract.Integer(state["writes"]); writes != 0 {
				t.Fatalf("default-branch rejection allowed %d writes", writes)
			}

			validProvider := newDeliveryTestProvider("OPEN", autoDelete)
			setDeliveryFinishState(t, validProvider, autoDelete)
			plan, err := PrepareDelivery(context.Background(), validProvider, testRepo(), DeliveryFinish, deliveryTestFinishPolicy(false), time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			stored, err := contract.Clone(plan.Data)
			if err != nil {
				t.Fatal(err)
			}
			inventory, _ := contract.ObjectAt(stored, "inventory")
			inventory["head_branch"] = contract.Object{"repo": testRepo().Object(), "repository_node_id": "R_widgets", "id": "REF_main", "name": "main", "sha": deliveryTestBaseSHA}
			pr, _ = contract.ObjectAt(inventory, "pull_request")
			pr["headRefName"], pr["headRefOid"] = "main", deliveryTestBaseSHA
			dependents, _ := contract.ObjectAt(inventory, "base_dependents")
			dependents["base_branch"] = "main"
			plan.Data = stored
			if _, err := deliveryOperations(plan); err == nil || !strings.Contains(err.Error(), "default branch") {
				t.Fatalf("default-branch removal was not rejected during plan re-derivation: %v", err)
			}
		})
	}
}

func TestDeliveryMalformedChecksAndForeignClosingIssueAreRejected(t *testing.T) {
	for name, update := range map[string]func(contract.Object){
		"failed checks": func(state contract.Object) {
			state["required_checks"] = []any{contract.Object{"name": "Tests", "bucket": "fail", "state": "FAILURE", "workflow": "CI", "link": "https://github.com/example/widgets/actions/runs/1"}}
		},
		"outside issue": func(state contract.Object) {
			pr := state["pull_request"].(map[string]any)
			pr["closing_issue_numbers"] = []any{int64(1), int64(2)}
		},
		"wrong review": func(state contract.Object) {
			state["pull_request"].(map[string]any)["reviewDecision"] = "CHANGES_REQUESTED"
		},
	} {
		t.Run(name, func(t *testing.T) {
			provider := newDeliveryTestProvider("OPEN", false)
			setDeliveryFinishState(t, provider, false)
			state, _ := provider.backlogTestProvider.load()
			update(state)
			provider.backlogTestProvider.save(state)
			if _, err := PrepareDelivery(context.Background(), provider, testRepo(), DeliveryFinish, deliveryTestFinishPolicy(false), time.Now()); err == nil {
				t.Fatal("malformed finish evidence was accepted")
			}
			state, _ = provider.backlogTestProvider.load()
			if writes, _ := contract.Integer(state["writes"]); writes != 0 {
				t.Fatalf("rejected finish evidence allowed %d writes", writes)
			}
		})
	}
}

func TestDeliveryUnknownCreateWithoutACKNeverReplays(t *testing.T) {
	provider := newDeliveryTestProvider("OPEN", false)
	policy := contract.Object{"draft": deliveryTestDraft().Object(), "execution_state": deliveryTestPolicy(true, false)}
	policy["execution_state"].(map[string]any)["project"] = nil
	plan, err := PrepareDelivery(context.Background(), provider, testRepo(), DeliveryOpenPR, policy, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil || len(plan.Operations) != 1 {
		t.Fatalf("single-create plan = %d operations, %v", len(plan.Operations), err)
	}
	resetDeliveryTestState(t, provider)
	provider.failCreateAfterAccept = true
	root := t.TempDir()
	if _, err := deliveryTestEngine(root, provider).Apply(context.Background(), plan.Object()); err == nil {
		t.Fatal("lost create response was reported completed")
	}
	state, _ := provider.backlogTestProvider.load()
	if writes, _ := contract.Integer(state["writes"]); writes != 1 {
		t.Fatalf("first unknown creation count = %d", writes)
	}
	if _, err := deliveryTestEngine(root, provider).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "no durable native acknowledgement") {
		t.Fatalf("unknown creation was not held for manual reconciliation: %v", err)
	}
	state, _ = provider.backlogTestProvider.load()
	if writes, _ := contract.Integer(state["writes"]); writes != 1 {
		t.Fatalf("ambiguous creation was replayed: %d writes", writes)
	}
}

func TestDeliveryFreshProcessRecoversDurableLateBoundGovernanceACKWithoutReplay(t *testing.T) {
	if os.Getenv("STEWARD_DELIVERY_HELPER") != "" {
		statePath, planPath, root := os.Getenv("STEWARD_DELIVERY_STATE"), os.Getenv("STEWARD_DELIVERY_PLAN"), os.Getenv("STEWARD_DELIVERY_ROOT")
		crashRead := int64(0)
		if os.Getenv("STEWARD_DELIVERY_CRASH_READ") != "" {
			_, _ = fmt.Sscan(os.Getenv("STEWARD_DELIVERY_CRASH_READ"), &crashRead)
		}
		provider := &deliveryTestProvider{governanceFixtureProvider: &governanceFixtureProvider{backlogTestProvider: &backlogTestProvider{stateFile: statePath}}, crashDeliveryRead: crashRead}
		file, err := os.Open(planPath)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := contract.Decode(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := deliveryTestEngine(root, provider).Apply(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
		return
	}

	provider := newDeliveryTestProvider("OPEN", false)
	policy := contract.Object{"draft": deliveryTestDraft().Object(), "execution_state": deliveryTestPolicy(true, true)}
	plan, err := PrepareDelivery(context.Background(), provider, testRepo(), DeliveryOpenPR, policy, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil || len(plan.Operations) < 3 || plan.Operations[0].Kind != "pull-request-create" {
		t.Fatalf("recovery plan should compose PR creation with governance child operations: ops=%d err=%v", len(plan.Operations), err)
	}
	root := t.TempDir()
	statePath, planPath := filepath.Join(root, "remote.json"), filepath.Join(root, "plan.json")
	resetDeliveryTestState(t, provider)
	provider.backlogTestProvider.stateFile = statePath
	if err := provider.backlogTestProvider.save(provider.backlogTestProvider.state); err != nil {
		t.Fatal(err)
	}
	encoded, err := contract.Canonical(plan.Object())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(crashRead string) *exec.ExitError {
		command := exec.Command(os.Args[0], "-test.run=^TestDeliveryFreshProcessRecoversDurableLateBoundGovernanceACKWithoutReplay$")
		command.Env = append(os.Environ(), "STEWARD_DELIVERY_HELPER=1", "STEWARD_DELIVERY_STATE="+statePath, "STEWARD_DELIVERY_PLAN="+planPath, "STEWARD_DELIVERY_ROOT="+root, "STEWARD_DELIVERY_CRASH_READ="+crashRead)
		output, runErr := command.CombinedOutput()
		if runErr == nil {
			return nil
		}
		if exit, ok := runErr.(*exec.ExitError); ok {
			if crashRead != "4" || exit.ExitCode() != 73 {
				t.Fatalf("delivery helper failed at crash read %s with %d: %s", crashRead, exit.ExitCode(), output)
			}
			return exit
		}
		t.Fatalf("delivery helper could not run: %v", runErr)
		return nil
	}
	if invoke("4") == nil {
		t.Fatal("writer process did not crash after a late-bound governance ACK persistence")
	}
	if invoke("0") != nil {
		t.Fatal("fresh-process governance recovery did not complete")
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
	if writes, _ := contract.Integer(state["writes"]); writes != int64(len(plan.Operations)) {
		t.Fatalf("fresh-process recovery replayed or omitted a delivery primitive: writes=%d want=%d", writes, len(plan.Operations))
	}
	if invoke("0") != nil {
		t.Fatal("terminal fresh-process delivery replay did not remain complete")
	}
	stateFile, _ = os.Open(statePath)
	state, err = contract.Decode(stateFile)
	_ = stateFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	if writes, _ := contract.Integer(state["writes"]); writes != int64(len(plan.Operations)) {
		t.Fatalf("terminal replay repeated completed delivery write: writes=%d want=%d", writes, len(plan.Operations))
	}
}
