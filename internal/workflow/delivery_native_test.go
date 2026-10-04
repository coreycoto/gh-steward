package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

// The fixture returns provider-shaped GraphQL connections, paginated REST
// arrays and native gh check output. It rejects every write rather than
// stubbing the normalized inventory that the bridge is responsible for proving.
type deliveryNativeFixture struct {
	merged, deleted, dependent, malformedChecks, branchDrift bool
	foreignClosing, changedClosingHead, retentionDrift       bool
	omitHeadID, headIdentityDrift                            bool
	failFacet                                                string
	recreateAfterRepo, repoReads, deliveryRepoReads          int
	calls                                                    []string
}

func (f *deliveryNativeFixture) pullRequest() contract.Object {
	headRepository := testRepo().Object()
	headRepository["id"] = "R_widgets"
	if f.omitHeadID {
		delete(headRepository, "id")
	} else if f.headIdentityDrift {
		headRepository["id"] = "R_recreated"
	}
	pr := contract.Object{"id": "PR_3", "number": int64(3), "url": testRepo().URL + "/pull/3", "title": "Reviewed change", "body": "Closes #17", "state": "OPEN", "isDraft": false, "baseRefName": "main", "headRefName": "codex/issue-17", "headRefOid": strings.Repeat("a", 40), "mergeStateStatus": "CLEAN", "reviewDecision": nil, "merged": false, "mergedAt": nil, "mergeCommit": nil, "author": contract.Object{"login": "dev", "__typename": "User"}, "headRepository": testRepo().Object(), "labels": executionNativeConnection()}
	pr["headRepository"] = headRepository
	if f.merged {
		pr["state"], pr["merged"], pr["mergedAt"], pr["mergeCommit"] = "MERGED", true, "2026-01-01T00:00:00Z", contract.Object{"oid": strings.Repeat("c", 40)}
	}
	return pr
}

func (f *deliveryNativeFixture) Execute(_ context.Context, _ string, args []string, input []byte, _ string, _ []string) (native.Result, error) {
	if len(args) < 2 {
		return native.Result{}, fmt.Errorf("incomplete fixture command")
	}
	f.calls = append(f.calls, strings.Join(args, " "))
	var value any
	if args[0] == "pr" && args[1] == "checks" {
		if f.malformedChecks {
			return native.Result{ExitCode: 8, Stdout: []byte("[]")}, nil
		}
		value = []any{contract.Object{"name": "Tests", "bucket": "pass", "state": "SUCCESS", "link": "https://github.com/example/widgets/actions/runs/1", "workflow": "CI"}}
	} else if args[0] != "api" {
		return native.Result{}, fmt.Errorf("unexpected native command: %v", args)
	} else if args[1] == "graphql" {
		body, err := contract.Decode(strings.NewReader(string(input)))
		if err != nil {
			return native.Result{}, err
		}
		query := body["query"].(string)
		if strings.Contains(query, "mutation") {
			return native.Result{}, fmt.Errorf("read inventory attempted mutation")
		}
		variables, err := contract.ObjectAt(body, "variables")
		if err != nil {
			return native.Result{}, err
		}
		r := executionNativeRepo("R_widgets")
		switch {
		case strings.Contains(query, "deleteBranchOnMerge"):
			f.deliveryRepoReads++
			r["deleteBranchOnMerge"] = f.retentionDrift && f.deliveryRepoReads > 1
		case strings.Contains(query, "ref(qualifiedName"):
			name := strings.TrimPrefix(variables["ref"].(string), "refs/heads/")
			sha := strings.Repeat("a", 40)
			if name == "main" || f.branchDrift {
				sha = strings.Repeat("b", 40)
			}
			r["ref"] = contract.Object{"id": "REF_" + name, "name": name, "prefix": "refs/heads/", "target": contract.Object{"oid": sha}}
		case strings.Contains(query, "headRefName:$head"):
			r["pullRequests"] = executionNativeConnection()
		case strings.Contains(query, "baseRefName:$base"):
			rows := []any{}
			if f.dependent {
				rows = append(rows, contract.Object{"id": "PR_4", "number": int64(4), "url": testRepo().URL + "/pull/4", "baseRefName": "codex/issue-17", "headRefName": "codex/issue-18", "state": "OPEN", "isDraft": true, "merged": false})
			}
			r["pullRequests"] = executionNativeConnection(rows...)
		case strings.Contains(query, "closingIssuesReferences"):
			pr := f.pullRequest()
			if f.changedClosingHead {
				pr["headRefOid"] = strings.Repeat("d", 40)
			}
			qualified := testRepo().Object()
			if f.foreignClosing {
				qualified = contract.Object{"nameWithOwner": "foreign/widgets", "url": "https://github.com/foreign/widgets"}
			}
			pr["closingIssuesReferences"] = executionNativeConnection(contract.Object{"id": "I_17", "number": int64(17), "url": testRepo().URL + "/issues/17", "repository": qualified})
			r["pullRequest"] = pr
		case strings.Contains(query, "pullRequest(number"):
			r["pullRequest"] = f.pullRequest()
		case strings.Contains(query, "issues(first"):
			r["issues"] = executionNativeConnection(contract.Object{"id": "I_17", "number": int64(17), "title": "Task: reviewed change", "body": "", "state": "OPEN", "url": testRepo().URL + "/issues/17", "labels": executionNativeConnection(), "milestone": nil})
		default:
			f.repoReads++
			if f.recreateAfterRepo > 0 && f.repoReads >= f.recreateAfterRepo {
				r["id"] = "R_recreated"
			}
		}
		value = contract.Object{"data": contract.Object{"repository": r}}
	} else {
		for index, arg := range args {
			if arg == "--method" && index+1 < len(args) && args[index+1] != "GET" {
				return native.Result{}, fmt.Errorf("read inventory attempted REST mutation")
			}
		}
		endpoint := args[1]
		if f.failFacet != "" && strings.Contains(endpoint, f.failFacet) {
			return native.Result{ExitCode: 1}, nil
		}
		switch {
		case strings.Contains(endpoint, "/branches?"):
			rows := []any{contract.Object{"name": "main", "commit": contract.Object{"sha": strings.Repeat("b", 40)}}}
			if !f.deleted {
				rows = append(rows, contract.Object{"name": "codex/issue-17", "commit": contract.Object{"sha": strings.Repeat("a", 40)}})
			}
			value = []any{rows}
		case strings.Contains(endpoint, "/milestones?"), strings.Contains(endpoint, "/labels?"), strings.Contains(endpoint, "/issues/comments?"):
			value = []any{[]any{}}
		default:
			return native.Result{}, fmt.Errorf("unexpected native endpoint: %s", endpoint)
		}
	}
	encoded, err := contract.Canonical(value)
	return native.Result{Stdout: encoded}, err
}

func deliveryBridgeState(state string) contract.Object {
	return contract.Object{"issue_number": int64(17), "issue_state": state, "project": nil, "status_field": "Status", "target_status": "Done", "work_comment": nil}
}
func deliveryBridgeOpenPolicy() contract.Object {
	return contract.Object{"draft": (native.PullRequestDraft{Title: "Reviewed change", Body: "Closes #17", HeadBranch: "codex/issue-17", BaseBranch: "main", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}).Object(), "execution_state": deliveryBridgeState("OPEN")}
}
func deliveryBridgeFinishPolicy(keep bool) contract.Object {
	return contract.Object{"pull_request_number": int64(3), "merge_method": "squash", "keep_branch": keep, "execution_policy": executionBridgePolicy().Object(), "execution_state": deliveryBridgeState("CLOSED")}
}
func deliveryBridge(f *deliveryNativeFixture) NativeDelivery {
	return NativeDelivery{NativeGovernance: NativeGovernance{NativeBacklog: NativeBacklog{Transport: bridgeTransport(f)}}}
}

func TestNativeDeliveryInventoryFeedsOpenAndFinishReviewedPlans(t *testing.T) {
	for _, kind := range []string{DeliveryOpenPR, DeliveryFinish} {
		for _, keep := range []bool{false, true} {
			f := &deliveryNativeFixture{}
			policy := deliveryBridgeOpenPolicy()
			if kind == DeliveryFinish {
				policy = deliveryBridgeFinishPolicy(keep)
			}
			plan, err := PrepareDelivery(context.Background(), deliveryBridge(f), testRepo(), kind, policy, time.Now())
			if err != nil || plan.Command != DeliveryCommand || len(plan.Operations) == 0 {
				t.Fatal("provider-shaped source does not feed the delivery contract", kind, keep, err)
			}
		}
	}
}

func TestNativeDeliveryDoesNotTurnFailedOrChangedSourcesIntoCleanEvidence(t *testing.T) {
	for name, fixture := range map[string]*deliveryNativeFixture{
		"failed branches":                  {failFacet: "/branches?"},
		"failed issue metadata":            {failFacet: "/labels?"},
		"branch changed since collection":  {branchDrift: true},
		"foreign closing issue":            {foreignClosing: true},
		"different closing head":           {changedClosingHead: true},
		"missing head repository identity": {omitHeadID: true},
		"changed head repository identity": {headIdentityDrift: true},
		"failed pending-check collection":  {malformedChecks: true},
		"retention setting changed":        {retentionDrift: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := deliveryBridge(fixture).DeliveryInventory(context.Background(), DeliveryInventoryRequest{Kind: DeliveryFinish, Policy: deliveryBridgeFinishPolicy(false)}); err == nil {
				t.Fatal("incomplete or drifting native inventory accepted")
			}
		})
	}
	f := &deliveryNativeFixture{dependent: true}
	if _, err := PrepareDelivery(context.Background(), deliveryBridge(f), testRepo(), DeliveryFinish, deliveryBridgeFinishPolicy(false), time.Now()); err == nil {
		t.Fatal("open draft dependent did not protect its base branch")
	}
	f = &deliveryNativeFixture{}
	bad := deliveryBridgeOpenPolicy()
	bad["unreviewed"] = true
	if _, err := deliveryBridge(f).DeliveryInventory(context.Background(), DeliveryInventoryRequest{Kind: DeliveryOpenPR, Policy: bad}); err == nil || len(f.calls) != 0 {
		t.Fatal("invalid policy reached native IO", err, f.calls)
	}
}
