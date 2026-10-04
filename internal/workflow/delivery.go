package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

const DeliveryCommand = "delivery-apply"

const (
	DeliveryOpenPR = "open-pr"
	DeliveryFinish = "finish"
)

// DeliveryInventoryRequest pins one closed delivery kind and its normalized
// explicit policy. The provider derives the exact facets needed from these
// typed inputs and returns their live provenance with the result.
type DeliveryInventoryRequest struct {
	Kind   string
	Policy contract.Object
}

func (r DeliveryInventoryRequest) Object() contract.Object {
	return contract.Object{"kind": r.Kind, "policy": r.Policy}
}

// DeliveryProvider combines explicit live delivery evidence with correlated
// typed primitives. Every method returns the provider's native acknowledgement
// unchanged; durable persistence is performed by the apply engine adapter.
type DeliveryProvider interface {
	GovernanceProvider
	DeliveryInventory(context.Context, DeliveryInventoryRequest) (contract.Object, error)
	CreatePullRequest(context.Context, string, native.PullRequestDraft, string) (contract.Object, error)
	MergePullRequest(context.Context, int64, string, string) (contract.Object, error)
	DeleteBranch(context.Context, string, string, string, string) (contract.Object, error)
}

type deliveryPolicy struct {
	draft             *native.PullRequestDraft
	executionState    contract.Object
	pullRequestNumber int64
	mergeMethod       string
	keepBranch        bool
	executionPolicy   ExecutionPolicy
}

// ParseDeliveryPolicy validates a closed consumer-authored policy before any
// provider reads. It normalizes only typed identities and preserves exact text.
func ParseDeliveryPolicy(kind string, raw contract.Object, repo contract.Repository) (contract.Object, error) {
	canonical, _, err := parseDeliveryPolicy(kind, raw, repo)
	return canonical, err
}

func parseDeliveryPolicy(kind string, raw contract.Object, repo contract.Repository) (contract.Object, deliveryPolicy, error) {
	var parsed deliveryPolicy
	if raw == nil {
		return nil, parsed, errors.New("delivery policy is required")
	}
	copyRaw, err := contract.Clone(raw)
	if err != nil {
		return nil, parsed, err
	}
	switch kind {
	case DeliveryOpenPR:
		if !sameKeys(copyRaw, keySet("draft", "execution_state")) {
			return nil, parsed, errors.New("open-pr policy requires exactly draft and execution_state")
		}
		draftRaw, err := contract.ObjectAt(copyRaw, "draft")
		if err != nil || !sameKeys(draftRaw, keySet("title", "body", "head_branch", "base_branch", "head_sha", "base_sha", "draft")) {
			return nil, parsed, errors.New("open-pr draft must contain exactly the seven typed pull request fields")
		}
		draft := native.PullRequestDraft{}
		if draft.Title, err = contract.Nonempty(draftRaw, "title"); err != nil {
			return nil, parsed, err
		}
		if draft.Body, err = contract.String(draftRaw, "body"); err != nil {
			return nil, parsed, err
		}
		if draft.HeadBranch, err = contract.Nonempty(draftRaw, "head_branch"); err != nil {
			return nil, parsed, err
		}
		if draft.BaseBranch, err = contract.Nonempty(draftRaw, "base_branch"); err != nil {
			return nil, parsed, err
		}
		if draft.HeadSHA, err = contract.String(draftRaw, "head_sha"); err != nil {
			return nil, parsed, err
		}
		if draft.BaseSHA, err = contract.String(draftRaw, "base_sha"); err != nil {
			return nil, parsed, err
		}
		if draft.Draft, err = contract.Bool(draftRaw, "draft"); err != nil {
			return nil, parsed, err
		}
		if err = draft.Validate(); err != nil {
			return nil, parsed, err
		}
		parsed.draft = &draft
		stateRaw, err := contract.ObjectAt(copyRaw, "execution_state")
		if err != nil {
			return nil, parsed, err
		}
		state, _, err := parseGovernancePolicy(GovernanceExecutionState, stateRaw, repo)
		if err != nil {
			return nil, parsed, err
		}
		if body, hasTokens, tokenErr := deliveryCommentTemplate(state); tokenErr != nil {
			return nil, parsed, tokenErr
		} else if hasTokens && kind != DeliveryOpenPR {
			return nil, parsed, errors.New("pull request delivery tokens are supported only while opening a pull request")
		} else if hasTokens && body == "" {
			return nil, parsed, errors.New("delivery comment template must be nonempty")
		}
		parsed.executionState = state
		copyRaw["draft"] = draft.Object()
		copyRaw["execution_state"] = state
	case DeliveryFinish:
		if !sameKeys(copyRaw, keySet("pull_request_number", "merge_method", "keep_branch", "execution_policy", "execution_state")) {
			return nil, parsed, errors.New("finish policy has unsupported or missing fields")
		}
		if parsed.pullRequestNumber, err = contract.PositiveInteger(copyRaw["pull_request_number"]); err != nil {
			return nil, parsed, err
		}
		if parsed.mergeMethod, err = contract.Nonempty(copyRaw, "merge_method"); err != nil {
			return nil, parsed, err
		}
		parsed.mergeMethod = strings.ToLower(parsed.mergeMethod)
		if parsed.mergeMethod != "merge" && parsed.mergeMethod != "squash" && parsed.mergeMethod != "rebase" {
			return nil, parsed, errors.New("merge_method must be merge, squash or rebase")
		}
		if parsed.keepBranch, err = contract.Bool(copyRaw, "keep_branch"); err != nil {
			return nil, parsed, err
		}
		policyRaw, err := contract.ObjectAt(copyRaw, "execution_policy")
		if err != nil {
			return nil, parsed, err
		}
		if parsed.executionPolicy, err = parseExecutionPolicy(policyRaw); err != nil {
			return nil, parsed, fmt.Errorf("invalid execution_policy: %w", err)
		}
		stateRaw, err := contract.ObjectAt(copyRaw, "execution_state")
		if err != nil {
			return nil, parsed, err
		}
		state, _, err := parseGovernancePolicy(GovernanceExecutionState, stateRaw, repo)
		if err != nil {
			return nil, parsed, err
		}
		if _, hasTokens, tokenErr := deliveryCommentTemplate(state); tokenErr != nil {
			return nil, parsed, tokenErr
		} else if hasTokens {
			return nil, parsed, errors.New("pull request delivery tokens are supported only while opening a pull request")
		}
		parsed.executionState = state
		copyRaw["execution_policy"] = parsed.executionPolicy.Object()
		copyRaw["execution_state"] = state
	default:
		return nil, parsed, fmt.Errorf("unsupported delivery kind %q", kind)
	}
	return copyRaw, parsed, nil
}

// DeliveryRequestFromPlan reconstructs the exact source-read request needed to
// independently qualify or apply the reviewed plan.
func DeliveryRequestFromPlan(plan contract.Plan) (DeliveryInventoryRequest, error) {
	var request DeliveryInventoryRequest
	if plan.Command != DeliveryCommand || !sameKeys(plan.Data, keySet("kind", "policy", "inventory_request", "inventory")) {
		return request, errors.New("delivery plan has unsupported command or data fields")
	}
	kind, err := contract.Nonempty(plan.Data, "kind")
	if err != nil {
		return request, err
	}
	policy, err := contract.ObjectAt(plan.Data, "policy")
	if err != nil {
		return request, err
	}
	canonical, _, err := parseDeliveryPolicy(kind, policy, plan.Repository)
	if err != nil || !same(canonical, policy) {
		return request, errors.New("delivery policy is not canonical for the typed kind")
	}
	request = DeliveryInventoryRequest{Kind: kind, Policy: canonical}
	stored, err := contract.ObjectAt(plan.Data, "inventory_request")
	if err != nil || !same(stored, request.Object()) {
		return DeliveryInventoryRequest{}, errors.New("delivery inventory request differs from the typed policy")
	}
	return request, nil
}

// PrepareDelivery freezes one typed PR creation or manual finish action with
// complete issue, Project and native pull request evidence.
func PrepareDelivery(ctx context.Context, provider DeliveryProvider, repo contract.Repository, kind string, policy contract.Object, now time.Time) (contract.Plan, error) {
	if provider == nil {
		return contract.Plan{}, errors.New("delivery preparation requires a provider")
	}
	canonicalPolicy, _, err := parseDeliveryPolicy(kind, policy, repo)
	if err != nil {
		return contract.Plan{}, err
	}
	request := DeliveryInventoryRequest{Kind: kind, Policy: canonicalPolicy}
	raw, err := provider.DeliveryInventory(ctx, request)
	if err != nil {
		return contract.Plan{}, err
	}
	inventory, err := normalizeDeliveryInventory(raw, repo, request)
	if err != nil {
		return contract.Plan{}, err
	}
	sources, err := deliverySources(inventory, repo)
	if err != nil {
		return contract.Plan{}, err
	}
	data := contract.Object{"kind": kind, "policy": canonicalPolicy, "inventory_request": request.Object(), "inventory": inventory}
	provisional := contract.Plan{Command: DeliveryCommand, Repository: repo, Data: data, Sources: sources, CapturedAt: now.UTC().Format(time.RFC3339Nano)}
	operations, err := deliveryOperations(provisional)
	if err != nil {
		return contract.Plan{}, err
	}
	return contract.PreparePlan(DeliveryCommand, repo, sources, data, operations, now)
}

// Delivery is the durable apply adapter for composed pull request lifecycle
// operations. The outer plan owns one hash and journal for every child write.
type Delivery struct{ Provider DeliveryProvider }

var _ apply.Adapter = Delivery{}
var _ apply.AcknowledgingAdapter = Delivery{}

func (d Delivery) Operations(plan contract.Plan) ([]contract.Operation, error) {
	return deliveryOperations(plan)
}

func deliveryOperations(plan contract.Plan) ([]contract.Operation, error) {
	prepared, err := deliveryPlan(plan)
	if err != nil {
		return nil, err
	}
	return prepared.operations, nil
}

type deliveryPrepared struct {
	kind          string
	policy        contract.Object
	parsed        deliveryPolicy
	request       DeliveryInventoryRequest
	inventory     contract.Object
	govBase       contract.Object
	govPlan       contract.Plan
	govOps        []contract.Operation
	operations    []contract.Operation
	mergeOp       *contract.Operation
	createOp      *contract.Operation
	deleteOp      *contract.Operation
	selectedIssue int64
}

func deliveryPlan(plan contract.Plan) (deliveryPrepared, error) {
	var out deliveryPrepared
	if plan.Command != DeliveryCommand || !sameKeys(plan.Data, keySet("kind", "policy", "inventory_request", "inventory")) {
		return out, errors.New("delivery plan has unsupported command or data shape")
	}
	if _, err := time.Parse(time.RFC3339Nano, plan.CapturedAt); err != nil {
		return out, errors.New("delivery captured_at must be RFC3339")
	}
	var err error
	out.kind, err = contract.Nonempty(plan.Data, "kind")
	if err != nil {
		return out, err
	}
	policyRaw, err := contract.ObjectAt(plan.Data, "policy")
	if err != nil {
		return out, err
	}
	out.policy, out.parsed, err = parseDeliveryPolicy(out.kind, policyRaw, plan.Repository)
	if err != nil || !same(out.policy, policyRaw) {
		return out, errors.New("delivery policy is not canonical for its typed kind")
	}
	out.request = DeliveryInventoryRequest{Kind: out.kind, Policy: out.policy}
	requestRaw, err := contract.ObjectAt(plan.Data, "inventory_request")
	if err != nil || !same(requestRaw, out.request.Object()) {
		return out, errors.New("delivery inventory request differs from typed kind and policy")
	}
	inventoryRaw, err := contract.ObjectAt(plan.Data, "inventory")
	if err != nil {
		return out, err
	}
	out.inventory, err = validateStoredDeliveryInventory(inventoryRaw, plan.Repository, out.request)
	if err != nil {
		return out, fmt.Errorf("delivery inventory is invalid: %w", err)
	}
	if !same(out.inventory, inventoryRaw) {
		return out, errors.New("delivery inventory is not canonical for complete requested evidence")
	}
	sources, err := deliverySources(out.inventory, plan.Repository)
	if err != nil || !same(plan.Sources, sources) {
		return out, errors.New("delivery source declarations differ from captured live inventory")
	}
	out.govBase, err = deliveryGovernanceBase(out.kind, out.parsed, out.inventory)
	if err != nil {
		return out, err
	}
	out.govPlan, out.govOps, err = deliveryGovernancePlan(plan, out.policy, out.govBase, nil, true)
	if err != nil {
		return out, err
	}
	if out.kind == DeliveryOpenPR {
		draft := *out.parsed.draft
		head, _ := contract.ObjectAt(out.inventory, "head_branch")
		base, _ := contract.ObjectAt(out.inventory, "base_branch")
		if head["name"] != draft.HeadBranch || head["sha"] != draft.HeadSHA || base["name"] != draft.BaseBranch || base["sha"] != draft.BaseSHA {
			return out, errors.New("reviewed pull request draft does not match both current complete branch heads")
		}
		prs, _ := contract.ObjectAt(out.inventory, "head_pull_requests")
		rows, _ := contract.Objects(prs, "pull_requests")
		for _, existing := range rows {
			if existing["state"] == "OPEN" {
				return out, errors.New("an open pull request already exists for the reviewed head branch")
			}
		}
		op := contract.Operation{ID: "delivery:pull-request:create", Kind: "pull-request-create",
			Target: contract.Object{"repository_node_id": out.inventory["repository_node_id"], "draft": draft.Object()},
			Before: contract.Object{"exists": false, "head_branch": draft.HeadBranch, "head_sha": draft.HeadSHA, "base_branch": draft.BaseBranch, "base_sha": draft.BaseSHA, "title": nil},
			After:  contract.Object{"exists": true, "head_branch": draft.HeadBranch, "head_sha": draft.HeadSHA, "base_branch": draft.BaseBranch, "base_sha": draft.BaseSHA, "title": draft.Title}}
		out.createOp = &op
		out.operations = append(out.operations, op)
		out.operations = append(out.operations, out.govOps...)
		return out, nil
	}
	pr, _ := contract.ObjectAt(out.inventory, "pull_request")
	number, _ := contract.PositiveInteger(pr["number"])
	statePolicy, err := contract.ObjectAt(out.policy, "execution_state")
	if err != nil {
		return out, err
	}
	issueNumber, err := contract.PositiveInteger(statePolicy["issue_number"])
	if err != nil {
		return out, err
	}
	out.selectedIssue = issueNumber
	if number != out.parsed.pullRequestNumber {
		return out, errors.New("finish pull request number differs from complete selected snapshot")
	}
	if err := deliveryManualFinishGate(out.parsed, out.inventory, plan.Repository); err != nil {
		return out, err
	}
	autoDelete := out.inventory["auto_delete_branch"] == true
	if out.parsed.keepBranch && autoDelete {
		return out, errors.New("repository auto-delete policy conflicts with the reviewed keep_branch choice")
	}
	if !out.parsed.keepBranch {
		branch, err := contract.ObjectAt(out.inventory, "head_branch")
		if err != nil {
			return out, errors.New("reviewed branch deletion requires complete head-branch identity")
		}
		branchName, err := contract.Nonempty(branch, "name")
		if err != nil {
			return out, err
		}
		if branchName == out.inventory["default_branch"] {
			return out, errors.New("the reviewed pull request head is the repository default branch and cannot be removed")
		}
		dependents, _ := contract.ObjectAt(out.inventory, "base_dependents")
		rows, _ := contract.Objects(dependents, "pull_requests")
		if len(rows) != 0 {
			return out, errors.New("head branch is the base of another open pull request and cannot be removed")
		}
	}
	branch, _ := contract.ObjectAt(out.inventory, "head_branch")
	issue, exists := deliveryIssueByNumber(mustObject(out.inventory, "backlog_inventory"), issueNumber)
	if !exists {
		return out, errors.New("selected issue is absent from complete all-state inventory")
	}
	issueBeforeState := fmt.Sprint(issue["state"])
	issueAfterState := issueBeforeState
	if deliveryAutoClosesSelected(pr, issueNumber, fmt.Sprint(out.inventory["default_branch"])) {
		issueAfterState = "CLOSED"
	}
	merge := contract.Operation{ID: "delivery:pull-request:merge", Kind: "pull-request-merge",
		Target: contract.Object{"pull_request_number": number, "node_id": pr["id"], "url": pr["url"], "head_branch": pr["headRefName"], "head_sha": pr["headRefOid"], "base_branch": pr["baseRefName"], "merge_method": out.parsed.mergeMethod, "repository_node_id": out.inventory["repository_node_id"], "auto_delete_branch": autoDelete, "close_selected_issue": deliveryAutoClosesSelected(pr, issueNumber, fmt.Sprint(out.inventory["default_branch"]))},
		Before: contract.Object{"merged": false, "state": "OPEN", "branch_exists": true, "branch_sha": branch["sha"], "selected_issue_state": issueBeforeState},
		After:  contract.Object{"merged": true, "state": "MERGED", "branch_exists": !(autoDelete && !out.parsed.keepBranch), "selected_issue_state": issueAfterState}}
	out.mergeOp = &merge
	out.operations = append(out.operations, merge)
	out.operations = append(out.operations, out.govOps...)
	if !out.parsed.keepBranch && !autoDelete {
		delete := contract.Operation{ID: "delivery:branch:delete", Kind: "branch-delete",
			Target: contract.Object{"name": branch["name"], "expected_sha": branch["sha"], "repository_node_id": out.inventory["repository_node_id"]},
			Before: contract.Object{"exists": true, "ref": branch}, After: contract.Object{"exists": false, "ref": nil}}
		out.deleteOp = &delete
		out.operations = append(out.operations, delete)
	}
	return out, nil
}

func deliverySources(inventory contract.Object, repo contract.Repository) (contract.Object, error) {
	requestRaw, err := contract.ObjectAt(inventory, "provenance")
	if err != nil {
		return nil, err
	}
	request, err := contract.ObjectAt(requestRaw, "request")
	if err != nil {
		return nil, err
	}
	kind, err := contract.Nonempty(request, "kind")
	if err != nil {
		return nil, err
	}
	policy, err := contract.ObjectAt(request, "policy")
	if err != nil {
		return nil, err
	}
	canonical, _, err := parseDeliveryPolicy(kind, policy, repo)
	if err != nil || !same(canonical, policy) {
		return nil, errors.New("delivery source request policy is not canonical")
	}
	stateRaw, _ := contract.ObjectAt(canonical, "execution_state")
	_, govPolicy, err := parseGovernancePolicy(GovernanceExecutionState, stateRaw, repo)
	if err != nil {
		return nil, err
	}
	backlogRequest, err := governanceInventoryRequest(GovernanceExecutionState, stateRaw, govPolicy, repo, "")
	if err != nil {
		return nil, err
	}
	backlog, _ := contract.ObjectAt(inventory, "backlog_inventory")
	facets := backlogPlanSources(backlog, backlogRequest)
	if len(backlogRequest.Projects) == 0 {
		projects, _ := contract.ObjectAt(facets, "projects")
		projects["source"] = "not_requested"
	}
	return contract.Object{
		"delivery": contract.Object{"source": "github_api", "live": true, "complete": true, "repository_node_id": inventory["repository_node_id"], "request": contract.Object{"kind": kind, "policy": canonical}},
		"backlog":  contract.Object{"source": "composite", "live": true, "complete": true, "facets": facets},
	}, nil
}

func normalizeDeliveryInventory(raw contract.Object, repo contract.Repository, request DeliveryInventoryRequest) (contract.Object, error) {
	return deliveryInventory(raw, repo, request, false)
}

func validateStoredDeliveryInventory(raw contract.Object, repo contract.Repository, request DeliveryInventoryRequest) (contract.Object, error) {
	return deliveryInventory(raw, repo, request, true)
}

func deliveryInventory(raw contract.Object, repo contract.Repository, request DeliveryInventoryRequest, stored bool) (contract.Object, error) {
	if raw == nil || !sameKeys(raw, keySet("repo", "repository_node_id", "default_branch", "auto_delete_branch", "backlog_inventory", "head_branch", "base_branch", "head_pull_requests", "pull_request", "required_checks", "base_dependents", "provenance")) {
		return nil, errors.New("delivery inventory has unsupported or missing fields")
	}
	copyRaw, err := contract.Clone(raw)
	if err != nil {
		return nil, err
	}
	repoRaw, _ := contract.ObjectAt(copyRaw, "repo")
	actualRepo, err := contract.ParseRepository(repoRaw)
	if err != nil || actualRepo != repo || !same(repoRaw, repo.Object()) {
		return nil, errors.New("delivery inventory targets another normalized repository")
	}
	repositoryNode, err := contract.Nonempty(copyRaw, "repository_node_id")
	if err != nil {
		return nil, err
	}
	defaultBranch, err := contract.Nonempty(copyRaw, "default_branch")
	if err != nil || native.ValidateBranchName(defaultBranch) != nil {
		return nil, errors.New("delivery inventory default branch is invalid")
	}
	if _, err := contract.Bool(copyRaw, "auto_delete_branch"); err != nil {
		return nil, err
	}
	prov, err := contract.ObjectAt(copyRaw, "provenance")
	if err != nil || !sameKeys(prov, keySet("live", "complete", "source", "repository_node_id", "request")) || prov["live"] != true || prov["complete"] != true || prov["source"] != "github_api" || prov["repository_node_id"] != repositoryNode || !same(prov["request"], request.Object()) {
		return nil, errors.New("delivery inventory lacks complete live matching request provenance")
	}
	stateRaw, err := contract.ObjectAt(request.Policy, "execution_state")
	if err != nil {
		return nil, err
	}
	canonicalState, statePolicy, err := parseGovernancePolicy(GovernanceExecutionState, stateRaw, repo)
	if err != nil || !same(canonicalState, stateRaw) {
		return nil, errors.New("delivery execution-state policy is not canonical")
	}
	backlogRequest, err := governanceInventoryRequest(GovernanceExecutionState, canonicalState, statePolicy, repo, "")
	if err != nil {
		return nil, err
	}
	backlogRaw, err := contract.ObjectAt(copyRaw, "backlog_inventory")
	if err != nil {
		return nil, err
	}
	var backlog contract.Object
	if stored {
		backlog, err = validateStoredGovernanceInventory(backlogRaw, repo, backlogRequest)
	} else {
		backlog, err = normalizeGovernanceInventory(backlogRaw, repo, backlogRequest)
	}
	if err != nil {
		return nil, fmt.Errorf("delivery backlog evidence: %w", err)
	}
	copyRaw["backlog_inventory"] = backlog
	if request.Kind == DeliveryOpenPR {
		if copyRaw["pull_request"] != nil || copyRaw["base_dependents"] != nil || copyRaw["required_checks"] == nil || copyRaw["head_pull_requests"] == nil {
			return nil, errors.New("open-pr inventory has finish-only or missing evidence")
		}
		draftRaw, _ := contract.ObjectAt(request.Policy, "draft")
		head, err := normalizeDeliveryBranch(copyRaw["head_branch"], repo, repositoryNode)
		if err != nil {
			return nil, fmt.Errorf("delivery head branch: %w", err)
		}
		base, err := normalizeDeliveryBranch(copyRaw["base_branch"], repo, repositoryNode)
		if err != nil {
			return nil, fmt.Errorf("delivery base branch: %w", err)
		}
		if head["name"] != draftRaw["head_branch"] || head["sha"] != draftRaw["head_sha"] || base["name"] != draftRaw["base_branch"] || base["sha"] != draftRaw["base_sha"] {
			return nil, errors.New("open-pr inventory branches differ from the exact authored draft heads")
		}
		headPRs, err := normalizeDeliveryHeadPRs(copyRaw["head_pull_requests"], repo, repositoryNode, head["name"].(string))
		if err != nil {
			return nil, err
		}
		checks, err := normalizeDeliveryChecks(copyRaw["required_checks"], false)
		if err != nil || len(checks) != 0 {
			return nil, errors.New("open-pr inventory must not carry required-check evidence")
		}
		copyRaw["head_branch"], copyRaw["base_branch"], copyRaw["head_pull_requests"], copyRaw["required_checks"] = head, base, headPRs, checks
	} else if request.Kind == DeliveryFinish {
		if copyRaw["base_branch"] != nil || copyRaw["head_pull_requests"] != nil || copyRaw["pull_request"] == nil {
			return nil, errors.New("finish inventory has open-pr-only or missing evidence")
		}
		pr, err := normalizeDeliveryPullRequest(copyRaw["pull_request"], repo, repositoryNode, request.Policy)
		if err != nil {
			return nil, err
		}
		if copyRaw["head_branch"] == nil {
			if pr["merged"] != true || request.Policy["keep_branch"] != false {
				return nil, errors.New("finish head branch is absent without an acknowledged merge and reviewed deletion")
			}
		} else {
			branch, err := normalizeDeliveryBranch(copyRaw["head_branch"], repo, repositoryNode)
			if err != nil {
				return nil, err
			}
			if branch["name"] != pr["headRefName"] || branch["sha"] != pr["headRefOid"] {
				return nil, errors.New("finish branch identity or SHA differs from the selected pull request head")
			}
			copyRaw["head_branch"] = branch
		}
		checks, err := normalizeDeliveryChecks(copyRaw["required_checks"], true)
		if err != nil {
			return nil, err
		}
		copyRaw["pull_request"], copyRaw["required_checks"] = pr, checks
		if request.Policy["keep_branch"] == true {
			if copyRaw["base_dependents"] != nil {
				return nil, errors.New("keep_branch inventory must not request base dependency evidence")
			}
		} else {
			dependents, err := normalizeDeliveryDependents(copyRaw["base_dependents"], repo, repositoryNode, fmt.Sprint(pr["headRefName"]))
			if err != nil {
				return nil, err
			}
			copyRaw["base_dependents"] = dependents
		}
	} else {
		return nil, errors.New("unsupported delivery inventory kind")
	}
	return copyRaw, nil
}

func normalizeDeliveryBranch(raw any, repo contract.Repository, repositoryNode string) (contract.Object, error) {
	branch, ok := raw.(map[string]any)
	if !ok || !sameKeys(branch, keySet("repo", "repository_node_id", "id", "name", "sha")) {
		return nil, errors.New("branch evidence has unsupported or missing fields")
	}
	actual, err := contract.ParseRepository(mustObject(branch, "repo"))
	if err != nil || actual != repo || branch["repository_node_id"] != repositoryNode {
		return nil, errors.New("branch evidence belongs to another repository incarnation")
	}
	id, err := contract.Nonempty(branch, "id")
	if err != nil {
		return nil, err
	}
	name, err := contract.Nonempty(branch, "name")
	if err != nil || native.ValidateBranchName(name) != nil {
		return nil, errors.New("branch evidence has an invalid name")
	}
	sha, err := native.CommitOID(branch["sha"])
	if err != nil {
		return nil, err
	}
	return contract.Object{"repo": repo.Object(), "repository_node_id": repositoryNode, "id": id, "name": name, "sha": sha}, nil
}

func normalizeDeliveryHeadPRs(raw any, repo contract.Repository, repositoryNode, head string) (contract.Object, error) {
	collection, ok := raw.(map[string]any)
	if !ok || !sameKeys(collection, keySet("repo", "repository_node_id", "head_branch", "pull_requests", "provenance")) {
		return nil, errors.New("head pull request collection has unsupported or missing fields")
	}
	actual, err := contract.ParseRepository(mustObject(collection, "repo"))
	if err != nil || actual != repo || collection["repository_node_id"] != repositoryNode || collection["head_branch"] != head {
		return nil, errors.New("head pull request collection scope differs from reviewed branch")
	}
	prov, err := contract.ObjectAt(collection, "provenance")
	if err != nil || !sameKeys(prov, keySet("live", "complete", "source")) || prov["live"] != true || prov["complete"] != true || prov["source"] != "github_api" {
		return nil, errors.New("head pull request collection is not complete live evidence")
	}
	rows, err := contract.Objects(collection, "pull_requests")
	if err != nil {
		return nil, err
	}
	seenNumber, seenNode := map[int64]bool{}, map[string]bool{}
	for _, row := range rows {
		if !sameKeys(row, keySet("id", "number", "url", "title", "body", "state", "isDraft", "merged", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "maintainerCanModify", "repository", "headRepository")) {
			return nil, errors.New("head pull request row has unsupported or missing fields")
		}
		n, e := contract.PositiveInteger(row["number"])
		if e != nil || seenNumber[n] {
			return nil, errors.New("head pull request collection has duplicate or invalid number")
		}
		node, e := contract.Nonempty(row, "id")
		if e != nil || seenNode[node] {
			return nil, errors.New("head pull request collection has duplicate or invalid immutable identity")
		}
		if e = (&native.Transport{Repository: repo}).ValidatePullRequestURL(row["url"], n); e != nil {
			return nil, e
		}
		if row["headRefName"] != head || (row["state"] != "OPEN" && row["state"] != "CLOSED" && row["state"] != "MERGED") {
			return nil, errors.New("head pull request row has a foreign branch or invalid lifecycle state")
		}
		merged, e := contract.Bool(row, "merged")
		if e != nil || merged != (row["state"] == "MERGED") {
			return nil, errors.New("head pull request lifecycle facts disagree")
		}
		for _, key := range []string{"isDraft", "maintainerCanModify"} {
			if _, e := contract.Bool(row, key); e != nil {
				return nil, e
			}
		}
		for _, key := range []string{"title", "body", "baseRefName"} {
			if _, e := contract.String(row, key); e != nil {
				return nil, e
			}
		}
		if native.ValidateBranchName(row["baseRefName"].(string)) != nil {
			return nil, errors.New("head pull request has invalid base branch")
		}
		if _, e = native.CommitOID(row["headRefOid"]); e != nil {
			return nil, e
		}
		if _, e = native.CommitOID(row["baseRefOid"]); e != nil {
			return nil, e
		}
		for _, key := range []string{"repository", "headRepository"} {
			r, e := contract.ObjectAt(row, key)
			if e != nil || r["id"] != repositoryNode {
				return nil, errors.New("head pull request has a foreign repository incarnation")
			}
			parsed, e := contract.ParseRepository(r)
			if e != nil || parsed != repo {
				return nil, errors.New("head pull request has a foreign repository scope")
			}
		}
		seenNumber[n], seenNode[node] = true, true
	}
	rows = append([]contract.Object(nil), rows...)
	sort.Slice(rows, func(i, j int) bool {
		a, _ := contract.PositiveInteger(rows[i]["number"])
		b, _ := contract.PositiveInteger(rows[j]["number"])
		return a < b
	})
	return contract.Object{"repo": repo.Object(), "repository_node_id": repositoryNode, "head_branch": head, "pull_requests": backlogObjectsAsAny(rows), "provenance": contract.Object{"live": true, "complete": true, "source": "github_api"}}, nil
}

func normalizeDeliveryPullRequest(raw any, repo contract.Repository, repositoryNode string, policy contract.Object) (contract.Object, error) {
	pr, ok := raw.(map[string]any)
	keys := keySet("number", "repository", "id", "url", "baseRefName", "headRefName", "state", "mergeStateStatus", "title", "body", "isDraft", "merged", "headRefOid", "reviewDecision", "merge_commit_sha", "labels", "author", "head_repository", "head_repository_node_id", "closing_issue_numbers")
	if !ok || !sameKeys(pr, keys) {
		return nil, errors.New("selected pull request has unsupported or missing normalized fields")
	}
	stateRaw, err := contract.ObjectAt(policy, "execution_state")
	if err != nil {
		return nil, err
	}
	issueNumber, err := contract.PositiveInteger(stateRaw["issue_number"])
	if err != nil {
		return nil, err
	}
	number, err := contract.PositiveInteger(pr["number"])
	if err != nil || number != func() int64 { n, _ := contract.PositiveInteger(policy["pull_request_number"]); return n }() {
		return nil, errors.New("selected pull request number differs from policy")
	}
	if pr["repository"] != repo.FullName() {
		return nil, errors.New("selected pull request repository differs from target")
	}
	if _, err = contract.Nonempty(pr, "id"); err != nil {
		return nil, err
	}
	if err = (&native.Transport{Repository: repo}).ValidatePullRequestURL(pr["url"], number); err != nil {
		return nil, err
	}
	for _, key := range []string{"baseRefName", "headRefName"} {
		name, e := contract.Nonempty(pr, key)
		if e != nil || native.ValidateBranchName(name) != nil {
			return nil, errors.New("selected pull request has invalid branch name")
		}
	}
	if _, err = contract.String(pr, "title"); err != nil {
		return nil, err
	}
	if _, err = contract.String(pr, "body"); err != nil {
		return nil, err
	}
	if pr["state"] != "OPEN" && pr["state"] != "CLOSED" && pr["state"] != "MERGED" {
		return nil, errors.New("selected pull request has invalid lifecycle state")
	}
	merged, err := contract.Bool(pr, "merged")
	if err != nil || merged != (pr["state"] == "MERGED") {
		return nil, errors.New("selected pull request lifecycle facts disagree")
	}
	if _, err = contract.Bool(pr, "isDraft"); err != nil {
		return nil, err
	}
	headSHA, err := native.CommitOID(pr["headRefOid"])
	if err != nil {
		return nil, err
	}
	pr["headRefOid"] = headSHA
	if _, err = contract.Nonempty(pr, "mergeStateStatus"); err != nil {
		return nil, err
	}
	review, err := contract.String(pr, "reviewDecision")
	if err != nil {
		return nil, err
	}
	if pr["merge_commit_sha"] != nil {
		if _, err = native.CommitOID(pr["merge_commit_sha"]); err != nil {
			return nil, err
		}
	}
	labels, err := contract.Objects(pr, "labels")
	if err != nil {
		return nil, err
	}
	for _, label := range labels {
		if !sameKeys(label, keySet("name")) {
			return nil, errors.New("selected pull request label row has unsupported fields")
		}
		if _, err = contract.Nonempty(label, "name"); err != nil {
			return nil, err
		}
	}
	if pr["author"] != nil {
		author, e := contract.ObjectAt(pr, "author")
		if e != nil || !sameKeys(author, keySet("login", "is_bot")) {
			return nil, errors.New("selected pull request author is malformed")
		}
		if _, e = contract.Nonempty(author, "login"); e != nil {
			return nil, e
		}
		if _, e = contract.Bool(author, "is_bot"); e != nil {
			return nil, e
		}
	}
	if pr["head_repository"] == nil {
		return nil, errors.New("selected pull request head repository is unavailable")
	}
	headRepo, err := contract.ParseRepository(mustObject(pr, "head_repository"))
	if err != nil || headRepo != repo {
		return nil, errors.New("finish requires a same-repository pull request head")
	}
	if pr["head_repository_node_id"] != repositoryNode {
		return nil, errors.New("finish pull request head belongs to another repository incarnation")
	}
	closing, err := contract.Array(pr, "closing_issue_numbers")
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	closingNumbers := make([]int64, 0, len(closing))
	for _, rawNumber := range closing {
		n, e := contract.PositiveInteger(rawNumber)
		if e != nil || seen[n] {
			return nil, errors.New("pull request closing references must be unique positive issue numbers")
		}
		if n != issueNumber {
			return nil, errors.New("finish pull request closes an issue outside the selected execution issue")
		}
		seen[n] = true
		closingNumbers = append(closingNumbers, n)
	}
	sort.Slice(closingNumbers, func(i, j int) bool { return closingNumbers[i] < closingNumbers[j] })
	pr["closing_issue_numbers"] = int64SliceAsAny(closingNumbers)
	// Preserve canonical optional values while rejecting state that the manual
	// finish gate cannot positively interpret.
	pr["reviewDecision"] = review
	return pr, nil
}

func normalizeDeliveryChecks(raw any, required bool) ([]any, error) {
	array, ok := raw.([]any)
	if !ok {
		return nil, errors.New("required-check evidence must be an explicit complete array")
	}
	rows, err := contract.Objects(contract.Object{"checks": array}, "checks")
	if err != nil {
		return nil, err
	}
	if required && len(rows) == 0 {
		return nil, errors.New("manual finish requires at least one complete required check")
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if !sameKeys(row, keySet("name", "bucket", "state", "workflow", "link")) {
			return nil, errors.New("required check has unsupported or missing fields")
		}
		for _, key := range []string{"name", "bucket", "state"} {
			if _, err := contract.Nonempty(row, key); err != nil {
				return nil, err
			}
		}
		for _, key := range []string{"workflow", "link"} {
			if _, err := contract.String(row, key); err != nil {
				return nil, err
			}
		}
		identity, err := contract.Digest(contract.Object{"name": row["name"], "workflow": row["workflow"], "link": row["link"]})
		if err != nil || seen[identity] {
			return nil, errors.New("required-check evidence has duplicate identity")
		}
		seen[identity] = true
		if required && (row["bucket"] != "pass" || !deliveryPassingCheckState(fmt.Sprint(row["state"]))) {
			return nil, fmt.Errorf("required check %q is not a consistent passing native state", row["name"])
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, _ := contract.Canonical(rows[i])
		b, _ := contract.Canonical(rows[j])
		return string(a) < string(b)
	})
	return backlogObjectsAsAny(rows), nil
}

func deliveryPassingCheckState(state string) bool {
	switch strings.ToUpper(state) {
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return true
	default:
		return false
	}
}

func normalizeDeliveryDependents(raw any, repo contract.Repository, repositoryNode, branch string) (contract.Object, error) {
	collection, ok := raw.(map[string]any)
	if !ok || !sameKeys(collection, keySet("repo", "repository_node_id", "base_branch", "pull_requests", "provenance")) {
		return nil, errors.New("base dependent collection has unsupported or missing fields")
	}
	actual, err := contract.ParseRepository(mustObject(collection, "repo"))
	if err != nil || actual != repo || collection["repository_node_id"] != repositoryNode || collection["base_branch"] != branch {
		return nil, errors.New("base dependent collection scope differs from reviewed head branch")
	}
	prov, err := contract.ObjectAt(collection, "provenance")
	if err != nil || !sameKeys(prov, keySet("live", "complete", "source")) || prov["live"] != true || prov["complete"] != true || prov["source"] != "github_api" {
		return nil, errors.New("base dependent collection is not complete live evidence")
	}
	rows, err := contract.Objects(collection, "pull_requests")
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	for _, row := range rows {
		if !sameKeys(row, keySet("id", "number", "url", "baseRefName", "headRefName", "state", "isDraft", "merged")) {
			return nil, errors.New("base dependent row has unsupported or missing fields")
		}
		n, e := contract.PositiveInteger(row["number"])
		if e != nil || seen[n] {
			return nil, errors.New("base dependent collection has duplicate PR identity")
		}
		if e = (&native.Transport{Repository: repo}).ValidatePullRequestURL(row["url"], n); e != nil {
			return nil, e
		}
		if row["baseRefName"] != branch || row["state"] != "OPEN" || row["merged"] != false {
			return nil, errors.New("base dependent row is not an open pull request on the selected head branch")
		}
		if _, e = contract.Nonempty(row, "id"); e != nil {
			return nil, e
		}
		if _, e = contract.Nonempty(row, "headRefName"); e != nil {
			return nil, e
		}
		if _, e = contract.Bool(row, "isDraft"); e != nil {
			return nil, e
		}
		seen[n] = true
	}
	sort.Slice(rows, func(i, j int) bool {
		a, _ := contract.PositiveInteger(rows[i]["number"])
		b, _ := contract.PositiveInteger(rows[j]["number"])
		return a < b
	})
	return contract.Object{"repo": repo.Object(), "repository_node_id": repositoryNode, "base_branch": branch, "pull_requests": backlogObjectsAsAny(rows), "provenance": contract.Object{"live": true, "complete": true, "source": "github_api"}}, nil
}

func deliveryCommentTemplate(state contract.Object) (string, bool, error) {
	comment, exists := state["work_comment"]
	if !exists || comment == nil {
		return "", false, nil
	}
	work, err := contract.ObjectAt(state, "work_comment")
	if err != nil {
		return "", false, err
	}
	body, err := contract.String(work, "body")
	if err != nil {
		return "", false, err
	}
	allowed := strings.NewReplacer("{{pull_request_url}}", "", "{{pull_request_number}}", "")
	residual := allowed.Replace(body)
	if strings.Contains(residual, "{{") || strings.Contains(residual, "}}") {
		return "", false, errors.New("delivery work comment contains an unsupported template token")
	}
	hasTokens := residual != body
	if hasTokens && strings.TrimSpace(residual) == "" {
		return body, true, errors.New("delivery work comment template must retain authored text")
	}
	return body, hasTokens, nil
}

const (
	deliveryURLSentinel    = "__GH_STEWARD_DELIVERY_PULL_REQUEST_URL__"
	deliveryNumberSentinel = "__GH_STEWARD_DELIVERY_PULL_REQUEST_NUMBER__"
)

func deliveryExecutionStateWithValues(policy contract.Object, pullRequestAck contract.Object, placeholders bool) (contract.Object, error) {
	state, err := contract.ObjectAt(policy, "execution_state")
	if err != nil {
		return nil, err
	}
	out, err := contract.Clone(state)
	if err != nil {
		return nil, err
	}
	body, hasTokens, err := deliveryCommentTemplate(out)
	if err != nil || !hasTokens {
		return out, err
	}
	if placeholders {
		if strings.Contains(body, deliveryURLSentinel) || strings.Contains(body, deliveryNumberSentinel) {
			return nil, errors.New("delivery work comment collides with a reserved template sentinel")
		}
		body = strings.ReplaceAll(body, "{{pull_request_url}}", deliveryURLSentinel)
		body = strings.ReplaceAll(body, "{{pull_request_number}}", deliveryNumberSentinel)
	} else {
		if pullRequestAck == nil {
			return nil, errors.New("delivery work comment requires a completed pull request creation acknowledgement")
		}
		providerResult, err := contract.ObjectAt(pullRequestAck, "provider_result")
		if err != nil {
			return nil, err
		}
		createdPullRequest, err := contract.ObjectAt(providerResult, "pullRequest")
		if err != nil {
			return nil, err
		}
		url, err := contract.Nonempty(createdPullRequest, "url")
		if err != nil {
			return nil, err
		}
		number, err := contract.PositiveInteger(createdPullRequest["number"])
		if err != nil {
			return nil, err
		}
		body = strings.ReplaceAll(body, "{{pull_request_url}}", url)
		body = strings.ReplaceAll(body, "{{pull_request_number}}", fmt.Sprint(number))
	}
	work, _ := contract.ObjectAt(out, "work_comment")
	work["body"] = body
	return out, nil
}

func deliveryGovernanceBase(kind string, policy deliveryPolicy, inventory contract.Object) (contract.Object, error) {
	backlog, err := contract.ObjectAt(inventory, "backlog_inventory")
	if err != nil {
		return nil, err
	}
	base, err := contract.Clone(backlog)
	if err != nil {
		return nil, err
	}
	if kind != DeliveryFinish {
		return base, nil
	}
	pr, _ := contract.ObjectAt(inventory, "pull_request")
	issueNumber, err := contract.PositiveInteger(policy.executionState["issue_number"])
	if err != nil {
		return nil, err
	}
	if deliveryAutoClosesSelected(pr, issueNumber, fmt.Sprint(inventory["default_branch"])) {
		graph, _ := contract.ObjectAt(base, "issue_inventory")
		issues, _ := contract.Objects(graph, "issues")
		issue := backlogIssueByNumber(issues, issueNumber)
		if issue == nil {
			return nil, errors.New("selected issue is absent from complete all-state inventory")
		}
		issue["state"] = "CLOSED"
	}
	return base, nil
}

func deliveryGovernancePlan(outer contract.Plan, policy contract.Object, baseline contract.Object, createACK contract.Object, usePlaceholders bool) (contract.Plan, []contract.Operation, error) {
	var empty contract.Plan
	state, err := deliveryExecutionStateWithValues(policy, createACK, usePlaceholders)
	if err != nil {
		return empty, nil, err
	}
	canonical, parsed, err := parseGovernancePolicy(GovernanceExecutionState, state, outer.Repository)
	if err != nil {
		return empty, nil, err
	}
	request, err := governanceInventoryRequest(GovernanceExecutionState, canonical, parsed, outer.Repository, "")
	if err != nil {
		return empty, nil, err
	}
	data := contract.Object{"kind": GovernanceExecutionState, "policy": canonical, "inventory_request": request.Object(), "inventory": baseline}
	plan := contract.Plan{Command: GovernanceCommand, Repository: outer.Repository, CapturedAt: outer.CapturedAt, Data: data, Sources: governanceSources(baseline, request)}
	ops, err := governanceOperations(plan)
	if err != nil {
		return empty, nil, err
	}
	if usePlaceholders {
		for i := range ops {
			for _, field := range []string{"body"} {
				if value, ok := ops[i].Target[field].(string); ok {
					ops[i].Target[field] = strings.ReplaceAll(strings.ReplaceAll(value, deliveryURLSentinel, "{{pull_request_url}}"), deliveryNumberSentinel, "{{pull_request_number}}")
				}
				if value, ok := ops[i].After[field].(string); ok {
					ops[i].After[field] = strings.ReplaceAll(strings.ReplaceAll(value, deliveryURLSentinel, "{{pull_request_url}}"), deliveryNumberSentinel, "{{pull_request_number}}")
				}
			}
		}
	}
	return plan, ops, nil
}

func deliveryManualFinishGate(policy deliveryPolicy, inventory contract.Object, repo contract.Repository) error {
	pr, err := contract.ObjectAt(inventory, "pull_request")
	if err != nil {
		return err
	}
	if pr["state"] != "OPEN" || pr["merged"] != false || pr["isDraft"] != false {
		return errors.New("manual finish requires an unmerged, open, non-draft pull request")
	}
	if pr["mergeStateStatus"] != "CLEAN" {
		return errors.New("manual finish requires GitHub mergeStateStatus CLEAN")
	}
	if pr["reviewDecision"] != "" && pr["reviewDecision"] != "APPROVED" {
		return errors.New("manual finish requires no required review or an approved review decision")
	}
	headRepo, err := contract.ParseRepository(mustObject(pr, "head_repository"))
	if err != nil || headRepo != repo {
		return errors.New("manual finish requires a same-repository head")
	}
	issueNumber, err := contract.PositiveInteger(policy.executionState["issue_number"])
	if err != nil {
		return err
	}
	_, exists := deliveryIssueByNumber(mustObject(inventory, "backlog_inventory"), issueNumber)
	if !exists {
		return errors.New("selected linked issue is absent from complete all-state inventory")
	}
	linkEvidence := contract.Object{"body": pr["body"], "base_branch": pr["baseRefName"], "closing_issue_numbers": pr["closing_issue_numbers"]}
	facts, err := executionLinkFacts(policy.executionPolicy, issueNumber, fmt.Sprint(inventory["default_branch"]), linkEvidence)
	if err != nil || facts["parsed_issue_number"] != issueNumber {
		return errors.New("selected pull request body does not identify the selected issue")
	}
	closing, err := contract.Array(pr, "closing_issue_numbers")
	if err != nil {
		return err
	}
	for _, raw := range closing {
		n, err := contract.PositiveInteger(raw)
		if err != nil || n != issueNumber {
			return errors.New("selected pull request would auto-close an issue outside the reviewed issue")
		}
	}
	checks, err := normalizeDeliveryChecks(inventory["required_checks"], true)
	if err != nil {
		return err
	}
	if len(checks) == 0 {
		return errors.New("manual finish requires at least one passing required check")
	}
	return nil
}

func deliveryAutoClosesSelected(pr contract.Object, issue int64, defaultBranch string) bool {
	if pr["baseRefName"] != defaultBranch {
		return false
	}
	rows, _ := contract.Array(pr, "closing_issue_numbers")
	for _, raw := range rows {
		if n, err := contract.PositiveInteger(raw); err == nil && n == issue {
			return true
		}
	}
	return false
}

func deliveryIssueByNumber(inventory contract.Object, number int64) (contract.Object, bool) {
	graph, err := contract.ObjectAt(inventory, "issue_inventory")
	if err != nil {
		return nil, false
	}
	rows, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, false
	}
	issue := backlogIssueByNumber(rows, number)
	return issue, issue != nil
}

func int64SliceAsAny(values []int64) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func (d Delivery) ValidateAcknowledgement(plan contract.Plan, op contract.Operation, ack contract.Object) error {
	prepared, err := deliveryPlan(plan)
	if err != nil {
		return err
	}
	if !containsOperation(prepared.operations, op) {
		return errors.New("delivery acknowledgement is outside the re-derived primitive list")
	}
	allowed := keySet("kind", "primitive_id", "repository", "operation_id", "target", "before", "after", "acknowledged", "provider_result", "after_verified", "pull_request_ack")
	for key := range ack {
		if !allowed[key] {
			return fmt.Errorf("delivery acknowledgement has unsupported field %q", key)
		}
	}
	if proof, present := ack["pull_request_ack"]; present {
		proofObject, ok := proof.(map[string]any)
		if !ok || prepared.createOp == nil || !deliveryPolicyHasTemplate(prepared.policy) {
			return errors.New("delivery acknowledgement carries an unrequested pull request proof")
		}
		if err := (Delivery{}).ValidateAcknowledgement(plan, *prepared.createOp, proofObject); err != nil {
			return fmt.Errorf("completed pull request proof is invalid: %w", err)
		}
	} else if prepared.createOp != nil && deliveryPolicyHasTemplate(prepared.policy) && op.Kind != "pull-request-create" {
		return errors.New("templated delivery child acknowledgement omits the exact pull request proof")
	}
	if ack["kind"] != op.Kind || ack["primitive_id"] != op.ID || ack["acknowledged"] != true || !same(ack["repository"], plan.Repository.Object()) || !same(ack["target"], op.Target) || !same(ack["before"], op.Before) || !same(ack["after"], op.After) {
		return errors.New("delivery acknowledgement differs from reviewed operation")
	}
	if value, present := ack["after_verified"]; present && value != true {
		return errors.New("delivery acknowledgement has an invalid after-state flag")
	}
	nonce, err := contract.Nonempty(ack, "operation_id")
	if err != nil {
		return err
	}
	if _, err = native.OperationMarker(nonce); err != nil {
		return err
	}
	providerResult, err := contract.ObjectAt(ack, "provider_result")
	if err != nil {
		return err
	}
	switch op.Kind {
	case "pull-request-create":
		if prepared.createOp == nil || !same(*prepared.createOp, op) {
			return errors.New("delivery create acknowledgement is outside open-pr intent")
		}
		draftRaw, _ := contract.ObjectAt(op.Target, "draft")
		draft, err := deliveryDraft(draftRaw)
		if err != nil {
			return err
		}
		repositoryNode, err := contract.Nonempty(op.Target, "repository_node_id")
		if err != nil {
			return err
		}
		if providerResult["clientMutationId"] != nonce {
			return errors.New("pull request creation acknowledgement has another native mutation identity")
		}
		createdPullRequest, err := contract.ObjectAt(providerResult, "pullRequest")
		if err != nil {
			return err
		}
		return (&native.Transport{Repository: plan.Repository}).ValidateCreatedPullRequest(createdPullRequest, draft, repositoryNode, nonce)
	case "pull-request-merge":
		if prepared.mergeOp == nil || !same(*prepared.mergeOp, op) || providerResult["merged"] != true {
			return errors.New("delivery merge acknowledgement does not identify the reviewed candidate")
		}
		sha, err := native.CommitOID(providerResult["sha"])
		if err != nil {
			return err
		}
		_ = sha // This is GitHub's merge commit SHA; the request was separately pinned to the reviewed head SHA.
	case "branch-delete":
		if prepared.deleteOp == nil || !same(*prepared.deleteOp, op) {
			return errors.New("delivery branch deletion acknowledgement is outside reviewed cleanup intent")
		}
		name, _ := contract.Nonempty(op.Target, "name")
		expectedSHA, _ := native.CommitOID(op.Target["expected_sha"])
		if err := native.ValidateBranchDeletion(providerResult, plan.Repository, name, expectedSHA); err != nil {
			return err
		}
	case "issue-state-set", "project-membership-add", "project-field-set", "project-field-set-number", "issue-comment-upsert":
		childAck, err := contract.ObjectAt(providerResult, "governance_ack")
		if err != nil {
			return err
		}
		createAck, err := deliveryAcknowledgedPullRequestForOperation(prepared, plan, op, ack)
		if err != nil {
			return err
		}
		childPlan, childOps, err := deliveryGovernancePlan(plan, prepared.policy, prepared.govBase, createAck, false)
		if err != nil {
			return err
		}
		childOp, ok := deliveryFindOperation(childOps, op.ID)
		if !ok {
			return errors.New("delivery governance acknowledgement has no re-derived child operation")
		}
		if !deliveryEquivalentGovernanceOperation(childOp, op, prepared.policy, createAck) {
			return errors.New("delivery governance child operation differs from the reviewed late-bound descriptor")
		}
		return (Governance{}).ValidateAcknowledgement(childPlan, childOp, childAck)
	default:
		return fmt.Errorf("unsupported delivery acknowledgement kind %q", op.Kind)
	}
	return nil
}

func (d Delivery) ValidateReceipt(plan contract.Plan, op contract.Operation, result contract.Object) error {
	if err := d.ValidateAcknowledgement(plan, op, result); err != nil {
		return err
	}
	if result["after_verified"] != true {
		return errors.New("delivery receipt lacks complete independently verified after-state")
	}
	return nil
}

func (d Delivery) Preflight(ctx context.Context, plan contract.Plan, receipts []contract.Object) error {
	if d.Provider == nil {
		return errors.New("delivery apply requires a provider")
	}
	request, err := DeliveryRequestFromPlan(plan)
	if err != nil {
		return err
	}
	expected, err := deliveryExpectedInventory(plan, receipts)
	if err != nil {
		return err
	}
	raw, err := d.Provider.DeliveryInventory(ctx, request)
	if err != nil {
		return err
	}
	actual, err := normalizeDeliveryInventory(raw, plan.Repository, request)
	if err != nil {
		return err
	}
	if !same(expected, actual) {
		return errors.New("complete live delivery inventory drifted from reviewed projected state")
	}
	return nil
}

func (d Delivery) Dispatch(ctx context.Context, plan contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, error) {
	return d.DispatchAcknowledged(ctx, plan, op, nonce, receipts, func(contract.Object) error { return nil })
}

func (d Delivery) DispatchAcknowledged(ctx context.Context, plan contract.Plan, op contract.Operation, nonce string, receipts []contract.Object, persist func(contract.Object) error) (contract.Object, error) {
	if d.Provider == nil {
		return nil, errors.New("delivery apply requires a provider")
	}
	if _, err := native.OperationMarker(nonce); err != nil {
		return nil, err
	}
	prepared, err := deliveryPlan(plan)
	if err != nil {
		return nil, err
	}
	if !containsOperation(prepared.operations, op) {
		return nil, errors.New("delivery dispatch primitive is outside re-derived plan")
	}
	if persist == nil {
		return nil, errors.New("delivery acknowledgement persistence callback is required")
	}
	if op.Kind == "issue-state-set" || op.Kind == "project-membership-add" || op.Kind == "project-field-set" || op.Kind == "project-field-set-number" || op.Kind == "issue-comment-upsert" {
		return d.dispatchGovernanceAcknowledged(ctx, plan, prepared, op, nonce, receipts, persist)
	}
	providerResult, err := d.dispatchDeliveryPrimitive(ctx, plan, prepared, op, nonce)
	if err != nil {
		return nil, err
	}
	ack := deliveryAcknowledgement(plan, op, nonce, providerResult, nil)
	if err := d.ValidateAcknowledgement(plan, op, ack); err != nil {
		return nil, fmt.Errorf("native delivery acknowledgement failed validation: %w", err)
	}
	if err := persist(ack); err != nil {
		return nil, err
	}
	return d.verifyDeliveryAfter(ctx, plan, prepared, op, ack, receipts)
}

func (d Delivery) dispatchGovernanceAcknowledged(ctx context.Context, plan contract.Plan, prepared deliveryPrepared, op contract.Operation, nonce string, receipts []contract.Object, persist func(contract.Object) error) (contract.Object, error) {
	createAck, err := deliveryCompletedCreateAcknowledgement(prepared, receipts)
	if err != nil {
		return nil, err
	}
	childPlan, childOps, err := deliveryGovernancePlan(plan, prepared.policy, prepared.govBase, createAck, false)
	if err != nil {
		return nil, err
	}
	childOp, ok := deliveryFindOperation(childOps, op.ID)
	if !ok {
		return nil, errors.New("delivery child primitive is not the reviewed execution-state action")
	}
	if !deliveryEquivalentGovernanceOperation(childOp, op, prepared.policy, createAck) {
		return nil, fmt.Errorf("delivery child primitive differs from reviewed action: outer=%#v child=%#v", op, childOp)
	}
	childReceipts, err := deliveryChildReceipts(prepared.govOps, receipts)
	if err != nil {
		return nil, err
	}
	inner, err := (Governance{Provider: d.Provider}).DispatchAcknowledged(ctx, childPlan, childOp, nonce, childReceipts, func(childAck contract.Object) error {
		ack := deliveryAcknowledgement(plan, op, nonce, contract.Object{"governance_ack": childAck}, createAck)
		if err := d.ValidateAcknowledgement(plan, op, ack); err != nil {
			return err
		}
		return persist(ack)
	})
	if err != nil {
		return nil, err
	}
	ack := deliveryAcknowledgement(plan, op, nonce, contract.Object{"governance_ack": inner}, createAck)
	return d.verifyDeliveryAfter(ctx, plan, prepared, op, ack, receipts)
}

func (d Delivery) dispatchDeliveryPrimitive(ctx context.Context, plan contract.Plan, prepared deliveryPrepared, op contract.Operation, nonce string) (contract.Object, error) {
	switch op.Kind {
	case "pull-request-create":
		draftRaw, _ := contract.ObjectAt(op.Target, "draft")
		draft, err := deliveryDraft(draftRaw)
		if err != nil {
			return nil, err
		}
		repositoryNode, _ := contract.Nonempty(op.Target, "repository_node_id")
		return d.Provider.CreatePullRequest(ctx, nonce, draft, repositoryNode)
	case "pull-request-merge":
		number, _ := contract.PositiveInteger(op.Target["pull_request_number"])
		return d.Provider.MergePullRequest(ctx, number, fmt.Sprint(op.Target["head_sha"]), fmt.Sprint(op.Target["merge_method"]))
	case "branch-delete":
		return d.Provider.DeleteBranch(ctx, nonce, fmt.Sprint(op.Target["name"]), fmt.Sprint(op.Target["expected_sha"]), fmt.Sprint(op.Target["repository_node_id"]))
	default:
		return nil, fmt.Errorf("unsupported delivery primitive %q", op.Kind)
	}
}

func (d Delivery) verifyDeliveryAfter(ctx context.Context, plan contract.Plan, prepared deliveryPrepared, op contract.Operation, ack contract.Object, receipts []contract.Object) (contract.Object, error) {
	prior, err := deliveryExpectedInventory(plan, receipts)
	if err != nil {
		return nil, err
	}
	projected, err := deliveryProjectAcknowledgement(prepared, plan, prior, op, ack, receipts)
	if err != nil {
		return nil, err
	}
	request, err := DeliveryRequestFromPlan(plan)
	if err != nil {
		return nil, err
	}
	raw, err := d.Provider.DeliveryInventory(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("delivery write was acknowledged but independent after-state read failed: %w", err)
	}
	actual, err := normalizeDeliveryInventory(raw, plan.Repository, request)
	if err != nil {
		return nil, fmt.Errorf("delivery write was acknowledged but after-state is invalid: %w", err)
	}
	if !same(projected, actual) {
		return nil, errors.New("delivery write was acknowledged but complete after-state differs from exact projection")
	}
	result, err := contract.Clone(ack)
	if err != nil {
		return nil, err
	}
	result["after_verified"] = true
	return result, nil
}

func (d Delivery) Observe(ctx context.Context, plan contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, contract.Object, error) {
	if d.Provider == nil {
		return nil, nil, errors.New("delivery apply requires a provider")
	}
	prepared, err := deliveryPlan(plan)
	if err != nil {
		return nil, nil, err
	}
	var ack contract.Object
	for _, receipt := range receipts {
		if receipt["id"] == op.ID && receipt["status"] == "unknown" && receipt["operation_id"] == nonce {
			ack, _ = receipt["acknowledgement"].(map[string]any)
		}
	}
	if ack == nil {
		return nil, nil, errors.New("ambiguous delivery write has no durable native acknowledgement; no mutation was retried")
	}
	if err := d.ValidateAcknowledgement(plan, op, ack); err != nil {
		return nil, nil, err
	}
	prior := deliveryCompletedReceipts(receipts, op.ID)
	expected, err := deliveryExpectedInventory(plan, prior)
	if err != nil {
		return nil, nil, err
	}
	projected, err := deliveryProjectAcknowledgement(prepared, plan, expected, op, ack, prior)
	if err != nil {
		return nil, nil, err
	}
	request, err := DeliveryRequestFromPlan(plan)
	if err != nil {
		return nil, nil, err
	}
	raw, err := d.Provider.DeliveryInventory(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	actual, err := normalizeDeliveryInventory(raw, plan.Repository, request)
	if err != nil {
		return nil, nil, err
	}
	if !same(projected, actual) {
		return nil, nil, errors.New("captured delivery acknowledgement does not match complete live after-state")
	}
	result, err := contract.Clone(ack)
	if err != nil {
		return nil, nil, err
	}
	result["after_verified"] = true
	digest, err := contract.Digest(ack)
	if err != nil {
		return nil, nil, err
	}
	return result, contract.Object{"positive_identity": true, "after_state_verified": true, "operation_id": nonce, "reference": "journal:native-acknowledgement:" + digest}, nil
}

func deliveryAcknowledgement(plan contract.Plan, op contract.Operation, nonce string, providerResult, pullRequestACK contract.Object) contract.Object {
	ack := contract.Object{"kind": op.Kind, "primitive_id": op.ID, "repository": plan.Repository.Object(), "operation_id": nonce, "target": op.Target, "before": op.Before, "after": op.After, "acknowledged": true, "provider_result": providerResult}
	policy, _ := contract.ObjectAt(plan.Data, "policy")
	if deliveryPolicyHasTemplate(policy) && op.Kind != "pull-request-create" {
		ack["pull_request_ack"] = pullRequestACK
	}
	return ack
}

func deliveryExpectedInventory(plan contract.Plan, receipts []contract.Object) (contract.Object, error) {
	prepared, err := deliveryPlan(plan)
	if err != nil {
		return nil, err
	}
	expected, err := contract.Clone(prepared.inventory)
	if err != nil {
		return nil, err
	}
	for index, receipt := range receipts {
		if receipt["status"] != "completed" {
			continue
		}
		if index >= len(prepared.operations) || receipt["id"] != prepared.operations[index].ID {
			return nil, errors.New("delivery journal contains an operation outside exact reviewed order")
		}
		op := prepared.operations[index]
		result, err := contract.ObjectAt(receipt, "result")
		if err != nil {
			return nil, err
		}
		if result["operation_id"] != receipt["operation_id"] {
			return nil, errors.New("delivery completion has another operation identity")
		}
		if err = (Delivery{}).ValidateReceipt(plan, op, result); err != nil {
			return nil, err
		}
		expected, err = deliveryProjectAcknowledgement(prepared, plan, expected, op, result, receipts[:index])
		if err != nil {
			return nil, err
		}
	}
	return expected, nil
}

func deliveryProjectAcknowledgement(prepared deliveryPrepared, plan contract.Plan, inventory contract.Object, op contract.Operation, ack contract.Object, priorReceipts []contract.Object) (contract.Object, error) {
	if err := (Delivery{}).ValidateAcknowledgement(plan, op, ack); err != nil {
		return nil, err
	}
	out, err := contract.Clone(inventory)
	if err != nil {
		return nil, err
	}
	providerResult, err := contract.ObjectAt(ack, "provider_result")
	if err != nil {
		return nil, err
	}
	switch op.Kind {
	case "pull-request-create":
		collection, _ := contract.ObjectAt(out, "head_pull_requests")
		rows, _ := contract.Objects(collection, "pull_requests")
		pr, err := contract.ObjectAt(providerResult, "pullRequest")
		if err != nil {
			return nil, err
		}
		pr, err = contract.Clone(pr)
		if err != nil {
			return nil, err
		}
		for _, existing := range rows {
			n, _ := contract.PositiveInteger(existing["number"])
			created, _ := contract.PositiveInteger(pr["number"])
			if n == created || existing["id"] == pr["id"] {
				return nil, errors.New("created pull request acknowledgement collides with complete head inventory")
			}
		}
		rows = append(rows, pr)
		sort.Slice(rows, func(i, j int) bool {
			a, _ := contract.PositiveInteger(rows[i]["number"])
			b, _ := contract.PositiveInteger(rows[j]["number"])
			return a < b
		})
		collection["pull_requests"] = backlogObjectsAsAny(rows)
	case "pull-request-merge":
		pr, _ := contract.ObjectAt(out, "pull_request")
		if pr["id"] != op.Target["node_id"] || pr["headRefOid"] != op.Target["head_sha"] || pr["merged"] != false || pr["state"] != "OPEN" {
			return nil, errors.New("merge acknowledgement before-state differs from projected pull request")
		}
		sha, _ := native.CommitOID(providerResult["sha"])
		pr["state"], pr["merged"], pr["mergeStateStatus"], pr["merge_commit_sha"] = "MERGED", true, "MERGED", sha
		if op.Target["close_selected_issue"] == true {
			issue, exists := deliveryIssueByNumber(mustObject(out, "backlog_inventory"), prepared.selectedIssue)
			if !exists {
				return nil, errors.New("selected issue disappeared before merge projection")
			}
			issue["state"] = "CLOSED"
		}
		if op.Target["auto_delete_branch"] == true && prepared.parsed.keepBranch == false {
			out["head_branch"] = nil
		}
	case "branch-delete":
		branch, err := contract.ObjectAt(out, "head_branch")
		if err != nil || branch["name"] != op.Target["name"] || branch["sha"] != op.Target["expected_sha"] {
			return nil, errors.New("branch deletion acknowledgement before-state differs from complete ref inventory")
		}
		out["head_branch"] = nil
	case "issue-state-set", "project-membership-add", "project-field-set", "project-field-set-number", "issue-comment-upsert":
		innerAck, err := contract.ObjectAt(providerResult, "governance_ack")
		if err != nil {
			return nil, err
		}
		createAck, err := deliveryAcknowledgedPullRequestForOperation(prepared, plan, op, ack)
		if err != nil {
			return nil, err
		}
		childPlan, childOps, err := deliveryGovernancePlan(plan, prepared.policy, prepared.govBase, createAck, false)
		if err != nil {
			return nil, err
		}
		childOp, ok := deliveryFindOperation(childOps, op.ID)
		if !ok || !deliveryEquivalentGovernanceOperation(childOp, op, prepared.policy, createAck) {
			return nil, errors.New("governance receipt differs from the re-derived execution-state child")
		}
		childReceipts, err := deliveryChildReceipts(prepared.govOps, priorReceipts)
		if err != nil {
			return nil, err
		}
		childResult := contract.Object{"operation_id": innerAck["operation_id"]}
		for k, v := range innerAck {
			childResult[k] = v
		}
		// This synthetic receipt is only used to project the durable child ACK;
		// the outer operation is marked complete only after its fresh full
		// delivery inventory matches that projection.
		childResult["after_verified"] = true
		childReceipt := contract.Object{"id": childOp.ID, "status": "completed", "operation_id": innerAck["operation_id"], "result": childResult}
		childReceipts = append(childReceipts, childReceipt)
		backlog, err := governanceExpectedInventory(childPlan, childReceipts)
		if err != nil {
			return nil, err
		}
		out["backlog_inventory"] = backlog
	default:
		return nil, fmt.Errorf("unsupported delivery projection %q", op.Kind)
	}
	return out, nil
}

func deliveryCompletedReceipts(receipts []contract.Object, exclude string) []contract.Object {
	out := []contract.Object{}
	for _, receipt := range receipts {
		if receipt["status"] == "completed" && receipt["id"] != exclude {
			out = append(out, receipt)
		}
	}
	return out
}

func deliveryChildReceipts(operations []contract.Operation, receipts []contract.Object) ([]contract.Object, error) {
	byID := map[string]contract.Object{}
	for _, receipt := range receipts {
		if receipt["status"] != "completed" {
			continue
		}
		for _, op := range operations {
			if receipt["id"] != op.ID {
				continue
			}
			outer, err := contract.ObjectAt(receipt, "result")
			if err != nil {
				return nil, err
			}
			wrapper, err := contract.ObjectAt(outer, "provider_result")
			if err != nil {
				return nil, err
			}
			inner, err := contract.ObjectAt(wrapper, "governance_ack")
			if err != nil {
				return nil, err
			}
			verifiedInner, err := contract.Clone(inner)
			if err != nil {
				return nil, err
			}
			// A completed outer receipt proves the nested child passed its own
			// independent after-state check, including when it was recovered from
			// an ACK persisted before the read.
			verifiedInner["after_verified"] = true
			byID[op.ID] = contract.Object{"id": op.ID, "status": "completed", "operation_id": inner["operation_id"], "result": verifiedInner}
		}
	}
	out := []contract.Object{}
	for _, op := range operations {
		receipt := byID[op.ID]
		if receipt == nil {
			break
		}
		out = append(out, receipt)
	}
	if len(out) != len(byID) {
		return nil, errors.New("delivery nested governance receipts are not an exact prefix")
	}
	return out, nil
}

func deliveryFindOperation(operations []contract.Operation, id string) (contract.Operation, bool) {
	for _, op := range operations {
		if op.ID == id {
			return op, true
		}
	}
	return contract.Operation{}, false
}

func deliveryOperationHasTemplate(op contract.Operation) bool {
	body, _ := op.Target["body"].(string)
	return strings.Contains(body, "{{pull_request_url}}") || strings.Contains(body, "{{pull_request_number}}") || op.Kind == "issue-comment-upsert" && (strings.Contains(fmt.Sprint(op.After["body"]), "{{pull_request_url}}") || strings.Contains(fmt.Sprint(op.After["body"]), "{{pull_request_number}}"))
}

func deliveryPolicyHasTemplate(policy contract.Object) bool {
	state, err := contract.ObjectAt(policy, "execution_state")
	if err != nil {
		return false
	}
	_, hasTokens, err := deliveryCommentTemplate(state)
	return err == nil && hasTokens
}

func deliveryAcknowledgedPullRequestForOperation(prepared deliveryPrepared, plan contract.Plan, op contract.Operation, ack contract.Object) (contract.Object, error) {
	if !deliveryPolicyHasTemplate(prepared.policy) {
		if _, present := ack["pull_request_ack"]; present {
			return nil, errors.New("untemplated delivery carries an unexpected pull request proof")
		}
		return nil, nil
	}
	createACK, err := contract.ObjectAt(ack, "pull_request_ack")
	if err != nil {
		return nil, errors.New("late-bound work comment acknowledgement omits the exact completed pull request proof")
	}
	if prepared.createOp == nil {
		return nil, errors.New("late-bound delivery token has no reviewed pull request creation")
	}
	if err := (Delivery{}).ValidateAcknowledgement(plan, *prepared.createOp, createACK); err != nil {
		return nil, fmt.Errorf("late-bound pull request proof is invalid: %w", err)
	}
	return createACK, nil
}

func deliveryCompletedCreateAcknowledgement(prepared deliveryPrepared, receipts []contract.Object) (contract.Object, error) {
	if prepared.createOp == nil {
		return nil, nil
	}
	for _, receipt := range receipts {
		if receipt["id"] != prepared.createOp.ID || receipt["status"] != "completed" {
			continue
		}
		return contract.ObjectAt(receipt, "result")
	}
	return nil, nil
}

func deliveryEquivalentGovernanceOperation(child, outer contract.Operation, policy contract.Object, createACK contract.Object) bool {
	if same(child, outer) {
		return true
	}
	if child.ID != outer.ID || child.Kind != outer.Kind {
		return false
	}
	childCopy, err := contract.Clone(contract.Object{"id": child.ID, "kind": child.Kind, "target": child.Target, "before": child.Before, "after": child.After})
	if err != nil {
		return false
	}
	outerCopy, err := contract.Clone(contract.Object{"id": outer.ID, "kind": outer.Kind, "target": outer.Target, "before": outer.Before, "after": outer.After})
	if err != nil {
		return false
	}
	if !deliveryOperationHasTemplate(outer) {
		return false
	}
	childTarget, _ := contract.ObjectAt(childCopy, "target")
	outerTarget, _ := contract.ObjectAt(outerCopy, "target")
	childAfter, _ := contract.ObjectAt(childCopy, "after")
	outerAfter, _ := contract.ObjectAt(outerCopy, "after")
	if child.Kind != "issue-comment-upsert" {
		return false
	}
	state, _ := contract.ObjectAt(policy, "execution_state")
	work, _ := contract.ObjectAt(state, "work_comment")
	template, _ := contract.String(work, "body")
	providerResult, err := contract.ObjectAt(createACK, "provider_result")
	if err != nil {
		return false
	}
	createdPullRequest, err := contract.ObjectAt(providerResult, "pullRequest")
	if err != nil {
		return false
	}
	url, err := contract.Nonempty(createdPullRequest, "url")
	if err != nil {
		return false
	}
	number, err := contract.PositiveInteger(createdPullRequest["number"])
	if err != nil {
		return false
	}
	rendered := strings.ReplaceAll(strings.ReplaceAll(template, "{{pull_request_url}}", url), "{{pull_request_number}}", fmt.Sprint(number))
	expectedBody := strings.TrimSpace(rendered) + "\n\n" + fmt.Sprint(outerTarget["marker"]) + "\n"
	if childTarget["body"] != expectedBody || childAfter["body"] != expectedBody {
		return false
	}
	childTarget["body"], childAfter["body"] = outerTarget["body"], outerAfter["body"]
	return same(childCopy, outerCopy)
}

func deliveryDraft(raw contract.Object) (native.PullRequestDraft, error) {
	var draft native.PullRequestDraft
	if !sameKeys(raw, keySet("title", "body", "head_branch", "base_branch", "head_sha", "base_sha", "draft")) {
		return draft, errors.New("delivery draft has unsupported or missing fields")
	}
	var err error
	if draft.Title, err = contract.Nonempty(raw, "title"); err != nil {
		return draft, err
	}
	if draft.Body, err = contract.String(raw, "body"); err != nil {
		return draft, err
	}
	if draft.HeadBranch, err = contract.Nonempty(raw, "head_branch"); err != nil {
		return draft, err
	}
	if draft.BaseBranch, err = contract.Nonempty(raw, "base_branch"); err != nil {
		return draft, err
	}
	if draft.HeadSHA, err = contract.String(raw, "head_sha"); err != nil {
		return draft, err
	}
	if draft.BaseSHA, err = contract.String(raw, "base_sha"); err != nil {
		return draft, err
	}
	if draft.Draft, err = contract.Bool(raw, "draft"); err != nil {
		return draft, err
	}
	return draft, draft.Validate()
}
