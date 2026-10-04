package native

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func deliveryDraft() PullRequestDraft {
	return PullRequestDraft{Title: "Reviewed change", Body: "Closes #17", HeadBranch: "codex/issue-17",
		BaseBranch: "main", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
		Draft: true}
}

func TestDeliveryRepositoryCapturesServerOwnedRetentionPolicy(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		repo := target().Object()
		repo["id"] = "R_1"
		repo["defaultBranchRef"] = contract.Object{"name": "main"}
		repo["deleteBranchOnMerge"] = automatic
		exec := &sequenceExecutor{responses: []Result{labelResult(contract.Object{"repository": repo})}}
		actual, err := sequence(exec).ReadDeliveryRepository(context.Background())
		if err != nil || actual["deleteBranchOnMerge"] != automatic {
			t.Fatal("repository retention policy missing", actual, err)
		}
		if !strings.Contains(exec.inputs[0]["query"].(string), "deleteBranchOnMerge") {
			t.Fatal("policy was not independently read")
		}
	}
	for _, value := range []any{nil, "false", json.Number("0")} {
		repo := target().Object()
		repo["id"] = "R_1"
		repo["defaultBranchRef"] = contract.Object{"name": "main"}
		repo["deleteBranchOnMerge"] = value
		exec := &sequenceExecutor{responses: []Result{labelResult(contract.Object{"repository": repo})}}
		if _, err := sequence(exec).ReadDeliveryRepository(context.Background()); err == nil {
			t.Fatal("untyped retention policy accepted", value)
		}
	}
}

func deliveryBranch(name, sha string) Result {
	repo := target().Object()
	repo["id"] = "R_1"
	repo["owner"] = contract.Object{"login": "example", "__typename": "User"}
	repo["ref"] = contract.Object{"id": "REF_" + name, "name": name, "prefix": "refs/heads/", "target": contract.Object{"oid": sha}}
	return labelResult(contract.Object{"repository": repo})
}

func createdDeliveryPullRequest(nonce string) contract.Object {
	d := deliveryDraft()
	marker, _ := OperationMarker(nonce)
	repo := target().Object()
	repo["id"] = "R_1"
	return contract.Object{"id": "PR_23", "number": int64(23), "url": "https://github.com/example/widgets/pull/23",
		"title": d.Title, "body": d.Body + "\n\n" + marker, "state": "OPEN", "isDraft": d.Draft, "merged": false,
		"headRefName": d.HeadBranch, "baseRefName": d.BaseBranch, "headRefOid": d.HeadSHA, "baseRefOid": d.BaseSHA,
		"maintainerCanModify": false, "repository": repo, "headRepository": repo}
}

func deliveryResponses(ack contract.Object) []Result {
	d := deliveryDraft()
	return []Result{deliveryBranch(d.HeadBranch, d.HeadSHA), deliveryBranch(d.BaseBranch, d.BaseSHA), labelResult(contract.Object{"createPullRequest": ack})}
}

func TestCreatedPullRequestRetainsTypedNativeProofForTheReviewedHeads(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	pr := createdDeliveryPullRequest(nonce)
	ack := contract.Object{"clientMutationId": nonce, "pullRequest": pr, "provider_extra": "preserve"}
	f := &sequenceExecutor{responses: deliveryResponses(ack)}
	result, err := sequence(f).CreatePullRequest(context.Background(), deliveryDraft(), "R_1", nonce)
	if err != nil || result["provider_extra"] != "preserve" || result["clientMutationId"] != nonce {
		t.Fatal("created PR did not retain native proof", result, err)
	}
	if len(f.calls) != 3 || !strings.Contains(f.inputs[2]["query"].(string), "CreatePullRequestInput!") {
		t.Fatal("typed creation did not follow the two exact branch reads", f.calls)
	}
	input := f.inputs[2]["variables"].(map[string]any)["input"].(map[string]any)
	if input["clientMutationId"] != nonce || input["repositoryId"] != "R_1" || input["headRepositoryId"] != "R_1" || input["headRefName"] != "codex/issue-17" || input["baseRefName"] != "main" || input["body"] != pr["body"] || input["draft"] != true || len(input) != 8 {
		t.Fatal("reviewed creation scope changed", input)
	}
	if strings.Contains(f.inputs[2]["query"].(string), "expectedHeadOid") {
		t.Fatal("unsupported atomic create-head guard was invented")
	}
	if err := sequence(f).ValidateCreatedPullRequest(pr, deliveryDraft(), "R_1", nonce); err != nil {
		t.Fatal("durable created-PR acknowledgement did not validate independently", err)
	}
}

func TestPullRequestCreationRejectsDriftBeforeMutation(t *testing.T) {
	d := deliveryDraft()
	nonce := strings.Repeat("a", 32)
	for _, change := range []string{"head", "base", "repository", "missing_branch", "wrong_branch", "not_commit"} {
		t.Run(change, func(t *testing.T) {
			responses := deliveryResponses(contract.Object{})
			switch change {
			case "head":
				responses[0] = deliveryBranch(d.HeadBranch, strings.Repeat("c", 40))
			case "base":
				responses[1] = deliveryBranch(d.BaseBranch, strings.Repeat("c", 40))
			case "repository":
				responses[0].Stdout = []byte(strings.Replace(string(responses[0].Stdout), `"id":"R_1"`, `"id":"R_recreated"`, 1))
			case "missing_branch":
				repo := target().Object()
				repo["id"], repo["ref"] = "R_1", nil
				responses[0] = labelResult(contract.Object{"repository": repo})
			case "wrong_branch":
				responses[0] = deliveryBranch("other", d.HeadSHA)
			case "not_commit":
				responses[0] = deliveryBranch(d.HeadBranch, "")
			}
			f := &sequenceExecutor{responses: responses}
			if _, err := sequence(f).CreatePullRequest(context.Background(), d, "R_1", nonce); err == nil {
				t.Fatal("changed or incomplete branch evidence authorized creation")
			}
			for _, input := range f.inputs {
				if strings.Contains(input["query"].(string), "mutation(") {
					t.Fatal("creation was dispatched after branch drift")
				}
			}
		})
	}
}

func TestPullRequestCreationRejectsUnqualifiedOrChangedAcknowledgementWithoutRetry(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	for _, change := range []string{"nonce", "missing_nonce", "missing_pr", "id", "number", "foreign_url", "repository", "head_repository", "head_sha", "base_sha", "head_branch", "base_branch", "title", "body", "state", "merged", "draft", "partial"} {
		t.Run(change, func(t *testing.T) {
			pr := createdDeliveryPullRequest(nonce)
			ack := contract.Object{"clientMutationId": nonce, "pullRequest": pr}
			switch change {
			case "nonce":
				ack["clientMutationId"] = strings.Repeat("b", 32)
			case "missing_nonce":
				delete(ack, "clientMutationId")
			case "missing_pr":
				delete(ack, "pullRequest")
			case "id":
				pr["id"] = ""
			case "number":
				pr["number"] = true
			case "foreign_url":
				pr["url"] = "https://github.com/other/widgets/pull/23"
			case "repository":
				pr["repository"] = contract.Object{"id": "R_foreign", "nameWithOwner": "example/widgets", "url": target().URL}
			case "head_repository":
				pr["headRepository"] = nil
			case "head_sha":
				pr["headRefOid"] = strings.Repeat("c", 40)
			case "base_sha":
				pr["baseRefOid"] = strings.Repeat("c", 40)
			case "head_branch":
				pr["headRefName"] = "other"
			case "base_branch":
				pr["baseRefName"] = "other"
			case "title":
				pr["title"] = "Another change"
			case "body":
				pr["body"] = "Closes #18"
			case "state":
				pr["state"] = "CLOSED"
			case "merged":
				pr["merged"] = true
			case "draft":
				pr["isDraft"] = false
			}
			responses := deliveryResponses(ack)
			if change == "partial" {
				data, err := contract.Decode(strings.NewReader(string(responses[2].Stdout)))
				if err != nil {
					t.Fatal(err)
				}
				data["errors"] = []any{contract.Object{"message": "partial permission failure"}}
				encoded, _ := contract.Canonical(data)
				responses[2].Stdout = encoded
			}
			f := &sequenceExecutor{responses: responses}
			if _, err := sequence(f).CreatePullRequest(context.Background(), deliveryDraft(), "R_1", nonce); err == nil {
				t.Fatal("uncorrelated or changed creation acknowledged", change)
			}
			if len(f.calls) != 3 {
				t.Fatal("creation was retried or extra calls obscured the native failure", f.calls)
			}
		})
	}
}

func TestPullRequestDraftAndBranchSyntaxRejectBeforeNativeIO(t *testing.T) {
	for _, name := range []string{"", "@", "-head", "/head", "head/", "head.", "head..tail", "head@{1}", "head//tail", "fork:head", "head\n", "head\x7f", "head~1", "head^", "head?", "head*", "head[", "head\\tail", ".hidden", "head/.hidden", "head.lock", "head/tail.lock"} {
		if err := ValidateBranchName(name); err == nil {
			t.Fatal("invalid branch name accepted", name)
		}
	}
	for _, name := range []string{"main", "codex/issue-17", "feature/issue.with.dots", "release/v0.1.0"} {
		if err := ValidateBranchName(name); err != nil {
			t.Fatal("ordinary branch name rejected", name, err)
		}
	}
	for _, change := range []string{"title", "body_marker", "head", "base", "same_branch", "head_sha", "base_sha", "nonce", "repository"} {
		d := deliveryDraft()
		nonce, repoID := strings.Repeat("a", 32), "R_1"
		switch change {
		case "title":
			d.Title = " "
		case "body_marker":
			d.Body = "<!-- gh-steward:operation:reserved -->"
		case "head":
			d.HeadBranch = "fork:head"
		case "base":
			d.BaseBranch = ""
		case "same_branch":
			d.HeadBranch = d.BaseBranch
		case "head_sha":
			d.HeadSHA = "moving-ref"
		case "base_sha":
			d.BaseSHA = "moving-ref"
		case "nonce":
			nonce = "unqualified"
		case "repository":
			repoID = ""
		}
		f := &sequenceExecutor{}
		if _, err := sequence(f).CreatePullRequest(context.Background(), d, repoID, nonce); err == nil || len(f.calls) != 0 {
			t.Fatal("unqualified draft invoked native IO", change, err, f.calls)
		}
	}
}

func deliveryHeadPage(prs []any, next string) Result {
	repo := target().Object()
	repo["id"] = "R_1"
	repo["owner"] = contract.Object{"login": "example", "__typename": "User"}
	page := contract.Object{"hasNextPage": next != ""}
	if next != "" {
		page["endCursor"] = next
	}
	repo["pullRequests"] = contract.Object{"nodes": prs, "pageInfo": page}
	return labelResult(contract.Object{"repository": repo})
}

func TestHeadPullRequestReadIncludesEveryPageAndLifecycleState(t *testing.T) {
	open := createdDeliveryPullRequest(strings.Repeat("a", 32))
	merged := createdDeliveryPullRequest(strings.Repeat("b", 32))
	merged["id"], merged["number"], merged["url"], merged["state"], merged["merged"] = "PR_22", int64(22), "https://github.com/example/widgets/pull/22", "MERGED", true
	f := &sequenceExecutor{responses: []Result{deliveryHeadPage([]any{open}, "second"), deliveryHeadPage([]any{merged}, "")}}
	result, err := sequence(f).ReadPullRequestsForHead(context.Background(), "codex/issue-17")
	if err != nil {
		t.Fatal(err)
	}
	prs, err := contract.Objects(result, "pull_requests")
	if err != nil || len(prs) != 2 || prs[0]["id"] != "PR_22" || prs[1]["id"] != "PR_23" || result["repository_node_id"] != "R_1" {
		t.Fatal("complete head PR collection was lost", result, err)
	}
	vars := f.inputs[1]["variables"].(map[string]any)
	if len(f.calls) != 2 || vars["head"] != "codex/issue-17" || vars["cursor"] != "second" || !strings.Contains(f.inputs[0]["query"].(string), "states:[OPEN,CLOSED,MERGED]") {
		t.Fatal("head scope or pagination changed", f.inputs)
	}
}

func TestHeadPullRequestReadRejectsTruncationScopeAndIdentityDrift(t *testing.T) {
	for _, kind := range []string{"repeated_number", "repeated_id", "repository_drift", "foreign_head", "foreign_url", "different_branch", "malformed_state", "malformed_head", "cursor_loop", "missing_page"} {
		t.Run(kind, func(t *testing.T) {
			pr := createdDeliveryPullRequest(strings.Repeat("a", 32))
			next := createdDeliveryPullRequest(strings.Repeat("b", 32))
			next["id"], next["number"], next["url"] = "PR_24", int64(24), "https://github.com/example/widgets/pull/24"
			switch kind {
			case "repeated_number":
				next["number"], next["url"] = int64(23), pr["url"]
			case "repeated_id":
				next["id"] = pr["id"]
			case "foreign_head":
				next["headRepository"] = contract.Object{"id": "R_foreign", "nameWithOwner": "other/widgets", "url": "https://github.com/other/widgets"}
			case "foreign_url":
				next["url"] = "https://github.com/other/widgets/pull/24"
			case "different_branch":
				next["headRefName"] = "other"
			case "malformed_state":
				next["state"] = "MERGED"
			case "malformed_head":
				next["headRefOid"] = "moving-head"
			}
			responses := []Result{deliveryHeadPage([]any{pr}, "second"), deliveryHeadPage([]any{next}, "")}
			switch kind {
			case "repository_drift":
				responses[1].Stdout = []byte(strings.Replace(string(responses[1].Stdout), `"id":"R_1"`, `"id":"R_recreated"`, -1))
			case "cursor_loop":
				responses[1] = deliveryHeadPage([]any{next}, "second")
			case "missing_page":
				responses[1].Stdout = []byte(strings.Replace(string(responses[1].Stdout), `"pageInfo":{"hasNextPage":false}`, `"pageInfo":null`, 1))
			}
			f := &sequenceExecutor{responses: responses}
			if _, err := sequence(f).ReadPullRequestsForHead(context.Background(), "codex/issue-17"); err == nil {
				t.Fatal("incomplete or foreign head PR collection accepted", kind)
			}
			if len(f.calls) > 2 {
				t.Fatal("pagination failure retried", f.calls)
			}
		})
	}
}
