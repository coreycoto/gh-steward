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
	"github.com/coreycoto/gh-steward/internal/governance"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/planning"
)

const (
	ReviewCloseoutCommand = "review-closeout-apply"
	closeoutAuditMarker   = "<!-- review-closeout-audit-status -->"
	closeoutRendererV1    = int64(1)
)

// CloseoutPolicy keeps every repository-specific vocabulary choice in the
// caller's reviewed plan. The governance and backlog audit policies are
// required only for epic-closeout summaries.
type CloseoutPolicy struct {
	ReviewBacklog   ReviewBacklogPolicy
	GovernanceCheck contract.Object
	BacklogAudit    contract.Object
}

func (p CloseoutPolicy) Object() contract.Object {
	var governancePolicy, backlogPolicy any
	if p.GovernanceCheck != nil {
		governancePolicy, _ = contract.Clone(p.GovernanceCheck)
	}
	if p.BacklogAudit != nil {
		backlogPolicy, _ = contract.Clone(p.BacklogAudit)
	}
	return contract.Object{"review_backlog": p.ReviewBacklog.Object(), "governance_check": governancePolicy, "backlog_audit": backlogPolicy}
}

// ParseCloseoutPolicy accepts only the three public fields and validates the
// nested policies against the exact repository and Project selected by the
// caller.
func ParseCloseoutPolicy(raw contract.Object, repo contract.Repository) (CloseoutPolicy, error) {
	var policy CloseoutPolicy
	if !sameKeys(raw, keySet("review_backlog", "governance_check", "backlog_audit")) {
		return policy, errors.New("closeout policy has unsupported or missing fields")
	}
	reviewRaw, err := contract.ObjectAt(raw, "review_backlog")
	if err != nil {
		return policy, err
	}
	policy.ReviewBacklog, err = parseReviewBacklogPolicy(reviewRaw, repo)
	if err != nil {
		return policy, err
	}
	if raw["governance_check"] != nil {
		policy.GovernanceCheck, err = contract.ObjectAt(raw, "governance_check")
		if err != nil {
			return policy, errors.New("governance_check must be null or an exact policy object")
		}
		if !sameKeys(policy.GovernanceCheck, keySet("prefix_labels", "retired_labels", "status_field", "valid_statuses", "priority_field", "valid_priorities")) {
			return policy, errors.New("governance_check policy has unsupported or missing fields")
		}
		if _, err := governance.GovernanceCheck(policy.GovernanceCheck, contract.Object{"repo": repo.Object(), "project": nil, "issues": []any{}}); err != nil {
			return policy, fmt.Errorf("invalid governance_check policy: %w", err)
		}
	}
	if raw["backlog_audit"] != nil {
		policy.BacklogAudit, err = contract.ObjectAt(raw, "backlog_audit")
		if err != nil {
			return policy, errors.New("backlog_audit must be null or an exact policy object")
		}
		if !sameKeys(policy.BacklogAudit, keySet("schema_version", "scope", "taxonomy", "excluded_prefixes", "status_field", "valid_statuses", "done_statuses", "priority_field", "priorities", "order_field")) {
			return policy, errors.New("backlog_audit policy has unsupported or missing fields")
		}
		scope, err := contract.ObjectAt(policy.BacklogAudit, "scope")
		if err != nil || !sameKeys(scope, keySet("repo", "project")) {
			return policy, errors.New("backlog_audit scope must contain exactly repo and project")
		}
		scopeRepo, err := contract.ObjectAt(scope, "repo")
		if err != nil {
			return policy, err
		}
		parsedRepo, err := contract.ParseRepository(scopeRepo)
		if err != nil || parsedRepo != repo {
			return policy, errors.New("backlog_audit repository differs from closeout repository")
		}
		scopeProject, err := contract.ObjectAt(scope, "project")
		if err != nil || !sameKeys(scopeProject, keySet("id", "number", "title")) {
			return policy, errors.New("backlog_audit project scope must contain exactly id, number and title")
		}
		number, err := contract.PositiveInteger(scopeProject["number"])
		if err != nil || scopeProject["id"] != policy.ReviewBacklog.Project.ID || number != policy.ReviewBacklog.Project.Number || scopeProject["title"] != policy.ReviewBacklog.Project.Title {
			return policy, errors.New("backlog_audit Project differs from review backlog Project")
		}
		project := contract.Object{"id": policy.ReviewBacklog.Project.ID, "number": policy.ReviewBacklog.Project.Number, "title": policy.ReviewBacklog.Project.Title}
		if _, err := governance.BacklogAudit(policy.BacklogAudit, contract.Object{"repo": repo.Object(), "project": project, "issues": []any{}}, nil); err != nil {
			return policy, fmt.Errorf("invalid backlog_audit policy: %w", err)
		}
	}
	return policy, nil
}

// PrepareReviewCloseout re-derives the authored backlog delta and audit
// verdict from one complete live inventory. The final epic comment operation
// is a late-bound descriptor: its exact body is rendered only from the
// validated nested ACKs and their complete projected after-state.
func PrepareReviewCloseout(ctx context.Context, provider BacklogProvider, repo contract.Repository, summary contract.Object, policy CloseoutPolicy, now time.Time) (contract.Plan, error) {
	if provider == nil {
		return contract.Plan{}, errors.New("review closeout preparation requires a provider")
	}
	canonicalPolicy, err := ParseCloseoutPolicy(policy.Object(), repo)
	if err != nil {
		return contract.Plan{}, err
	}
	if err = validateCloseoutSummary(summary, repo); err != nil {
		return contract.Plan{}, err
	}
	request, err := closeoutInventoryRequest(summary, canonicalPolicy)
	if err != nil {
		return contract.Plan{}, err
	}
	raw, err := provider.BacklogInventory(ctx, request)
	if err != nil {
		return contract.Plan{}, err
	}
	inventory, err := normalizeBacklogInventory(raw, repo, request)
	if err != nil {
		return contract.Plan{}, err
	}
	data := contract.Object{"summary": summary, "policy": canonicalPolicy.Object(), "inventory_request": request.Object(), "inventory": inventory}
	sources := backlogPlanSources(inventory, request)
	plan := contract.Plan{Command: ReviewCloseoutCommand, Repository: repo, Data: data, Sources: sources, CapturedAt: now.UTC().Format(time.RFC3339Nano)}
	operations, err := closeoutOperations(plan)
	if err != nil {
		return contract.Plan{}, err
	}
	return contract.PreparePlan(ReviewCloseoutCommand, repo, sources, data, operations, now)
}

type closeoutPrepared struct {
	summary      contract.Object
	policy       CloseoutPolicy
	request      BacklogInventoryRequest
	inventory    contract.Object
	nestedPlan   contract.Plan
	nestedOps    []contract.Operation
	operations   []contract.Operation
	closeoutOp   *contract.Operation
	normalized   contract.Object
	backlogDelta contract.Object
}

func closeoutOperations(plan contract.Plan) ([]contract.Operation, error) {
	prepared, err := parseCloseoutPlan(plan)
	if err != nil {
		return nil, err
	}
	return prepared.operations, nil
}

func parseCloseoutPlan(plan contract.Plan) (closeoutPrepared, error) {
	var result closeoutPrepared
	var err error
	if plan.Command != ReviewCloseoutCommand || !sameKeys(plan.Data, keySet("summary", "policy", "inventory_request", "inventory")) {
		return result, errors.New("closeout plan has unsupported command or data fields")
	}
	if _, err := time.Parse(time.RFC3339Nano, plan.CapturedAt); err != nil {
		return result, errors.New("closeout captured_at must be RFC3339")
	}
	result.summary, err = contract.ObjectAt(plan.Data, "summary")
	if err != nil {
		return result, err
	}
	policyRaw, err := contract.ObjectAt(plan.Data, "policy")
	if err != nil {
		return result, err
	}
	result.policy, err = ParseCloseoutPolicy(policyRaw, plan.Repository)
	if err != nil {
		return result, err
	}
	if err := validateCloseoutSummary(result.summary, plan.Repository); err != nil {
		return result, err
	}
	result.request, err = closeoutInventoryRequest(result.summary, result.policy)
	if err != nil {
		return result, err
	}
	requestRaw, err := contract.ObjectAt(plan.Data, "inventory_request")
	if err != nil {
		return result, err
	}
	storedRequest, err := parseInventoryRequest(requestRaw)
	if err != nil || !same(storedRequest.Object(), result.request.Object()) {
		return result, errors.New("closeout inventory request differs from reviewed summary and explicit policy")
	}
	inventoryRaw, err := contract.ObjectAt(plan.Data, "inventory")
	if err != nil {
		return result, err
	}
	result.inventory, err = validateStoredBacklogInventory(inventoryRaw, plan.Repository, result.request)
	if err != nil {
		return result, err
	}
	if !same(result.inventory, inventoryRaw) || !same(plan.Sources, backlogPlanSources(result.inventory, result.request)) {
		return result, errors.New("closeout sources differ from canonical captured inventory")
	}
	result.normalized, err = closeoutNormalizedFindings(result.summary)
	if err != nil {
		return result, err
	}
	result.backlogDelta, err = closeoutBacklogDelta(result.summary, result.normalized, result.policy.ReviewBacklog, result.inventory, plan.Repository)
	if err != nil {
		return result, err
	}
	backlog, _ := contract.ObjectAt(result.summary, "backlog")
	storedDelta, _ := contract.ObjectAt(backlog, "delta")
	if !same(result.backlogDelta, storedDelta) {
		return result, errors.New("closeout backlog delta differs from re-derived authored intent")
	}
	if err = validateReviewLabels(result.backlogDelta, result.inventory); err != nil {
		return result, err
	}
	derived, err := closeoutLiveSummary(result.summary, result.policy, result.inventory, plan.Repository)
	if err != nil {
		return result, err
	}
	if !same(derived, result.summary) {
		return result, errors.New("closeout report drifted from complete live Project, governance or relationship evidence")
	}
	innerRequest := result.request
	innerRequest.CommentMarkers, err = reviewCommentMarkers(result.backlogDelta)
	if err != nil {
		return result, err
	}
	innerRequest.IncludeRelationships, err = deltaRequiresRelationships(result.backlogDelta)
	if err != nil {
		return result, err
	}
	result.nestedPlan, err = closeoutNestedPlan(plan, result.summary, result.policy, result.inventory, innerRequest, result.backlogDelta)
	if err != nil {
		return result, err
	}
	proposals, err := contract.Objects(result.backlogDelta, "proposals")
	if err != nil {
		return result, err
	}
	if len(proposals) > 0 {
		result.nestedOps, err = backlogOperations(result.nestedPlan)
		if err != nil {
			return result, err
		}
	}
	result.operations = append([]contract.Operation{}, result.nestedOps...)
	if result.summary["mode"] == "epic-closeout" {
		op, err := closeoutCommentOperation(plan, result.summary, result.policy, result.inventory, result.nestedOps)
		if err != nil {
			return result, err
		}
		result.closeoutOp = &op
		result.operations = append(result.operations, op)
	}
	return result, nil
}

func validateCloseoutSummary(summary contract.Object, repo contract.Repository) error {
	if summary == nil {
		return errors.New("closeout summary is required")
	}
	mode, err := contract.String(summary, "mode")
	if err != nil || (mode != "review" && mode != "epic-closeout") {
		return errors.New("closeout summary mode must be review or epic-closeout")
	}
	keys := keySet("repo", "schema_version", "mode", "scope", "generated_at", "epic_issue_number", "finding_count", "blocking_finding_count", "backlog", "blocking_reasons", "findings")
	if mode == "review" {
		keys["status"] = true
	} else {
		keys["epic_state"], keys["closeout_ready"] = true, true
	}
	if !sameKeys(summary, keys) {
		return errors.New("closeout summary has unsupported or missing fields")
	}
	version, versionErr := contract.Integer(summary["schema_version"])
	if versionErr != nil || version != 1 {
		return errors.New("closeout summary schema_version must be 1")
	}
	rawRepo, err := contract.ObjectAt(summary, "repo")
	if err != nil {
		return err
	}
	actualRepo, err := contract.ParseRepository(rawRepo)
	if err != nil || actualRepo != repo {
		return errors.New("closeout summary targets another repository")
	}
	if _, err = contract.Nonempty(summary, "scope"); err != nil {
		return err
	}
	generatedAt, err := contract.Nonempty(summary, "generated_at")
	if err != nil {
		return err
	}
	if _, err = time.Parse(time.RFC3339Nano, generatedAt); err != nil {
		return errors.New("closeout summary generated_at must be RFC3339")
	}
	if mode == "review" {
		if summary["epic_issue_number"] != nil {
			return errors.New("review summary epic_issue_number must be null")
		}
		if _, err := contract.Nonempty(summary, "status"); err != nil {
			return err
		}
	} else {
		if _, err := contract.PositiveInteger(summary["epic_issue_number"]); err != nil {
			return errors.New("epic-closeout summary requires a positive epic issue number")
		}
		if _, err := contract.Bool(summary, "closeout_ready"); err != nil {
			return err
		}
		if _, err := contract.ObjectAt(summary, "epic_state"); err != nil {
			return err
		}
	}
	findings, err := contract.Objects(summary, "findings")
	if err != nil {
		return err
	}
	count, err := contract.Integer(summary["finding_count"])
	if err != nil || count != int64(len(findings)) {
		return errors.New("closeout finding_count differs from findings")
	}
	blockingCount := int64(0)
	for _, finding := range findings {
		if !sameKeys(finding, keySet("id", "title", "canonical_title", "severity", "summary", "blocking", "destination", "target_paths", "body", "files", "evidence", "notes", "issue_type", "group_key", "backlog_title", "canonical_backlog_title", "existing_issue", "blocked_by_issue_numbers", "parent_issue_number")) {
			return errors.New("closeout finding has unsupported or missing fields")
		}
		if finding["blocking"] == true {
			blockingCount++
		}
	}
	reportedBlocking, err := contract.Integer(summary["blocking_finding_count"])
	if err != nil || reportedBlocking != blockingCount {
		return errors.New("closeout blocking_finding_count differs from findings")
	}
	if _, err := contract.Strings(summary["blocking_reasons"]); err != nil {
		return err
	}
	backlog, err := contract.ObjectAt(summary, "backlog")
	if err != nil || !sameKeys(backlog, keySet("finding_count", "proposal_count", "delta")) {
		return errors.New("closeout backlog summary has unsupported or missing fields")
	}
	backlogCount := int64(0)
	for _, finding := range findings {
		if finding["destination"] == "backlog" {
			backlogCount++
		}
	}
	storedBacklogCount, err := contract.Integer(backlog["finding_count"])
	if err != nil || storedBacklogCount != backlogCount {
		return errors.New("closeout backlog finding_count differs from findings")
	}
	delta, err := contract.ObjectAt(backlog, "delta")
	if err != nil {
		return err
	}
	proposalCount, err := contract.Integer(backlog["proposal_count"])
	if err != nil || proposalCount < 0 {
		return errors.New("closeout backlog proposal_count must be nonnegative")
	}
	proposals, err := contract.Objects(delta, "proposals")
	if err != nil || int64(len(proposals)) != proposalCount {
		return errors.New("closeout backlog proposal_count differs from delta")
	}
	expectedDeltaKeys := keySet("repo", "schema_version", "scope", "proposal_count", "proposals")
	if len(proposals) > 0 {
		expectedDeltaKeys["issue_catalog_mode"] = true
	}
	deltaVersion, deltaVersionErr := contract.Integer(delta["schema_version"])
	if !sameKeys(delta, expectedDeltaKeys) || delta["scope"] != summary["scope"] || deltaVersionErr != nil || deltaVersion != 1 || len(proposals) > 0 && delta["issue_catalog_mode"] != "live" {
		return errors.New("closeout backlog delta has unsupported schema or scope")
	}
	deltaRepoRaw, err := contract.ObjectAt(delta, "repo")
	if err != nil {
		return err
	}
	deltaRepo, err := contract.ParseRepository(deltaRepoRaw)
	if err != nil || deltaRepo != repo {
		return errors.New("closeout backlog delta targets another repository")
	}
	return nil
}

func closeoutNormalizedFindings(summary contract.Object) (contract.Object, error) {
	input := contract.Object{"schema_version": summary["schema_version"], "mode": summary["mode"], "scope": summary["scope"], "findings": summary["findings"]}
	if summary["epic_issue_number"] != nil {
		input["epic_issue_number"] = summary["epic_issue_number"]
	}
	normalized, err := planning.NormalizeReviewCloseoutFindings(input)
	if err != nil {
		return nil, err
	}
	findings, err := contract.Objects(normalized, "findings")
	if err != nil {
		return nil, err
	}
	findings = planning.SortReviewCloseoutFindings(findings)
	if !same(findings, summary["findings"]) {
		return nil, errors.New("closeout summary findings are not canonical")
	}
	normalized["findings"] = backlogObjectsAsAny(findings)
	return normalized, nil
}

func closeoutBacklogDelta(summary, normalized contract.Object, policy ReviewBacklogPolicy, inventory contract.Object, repo contract.Repository) (contract.Object, error) {
	findings, err := contract.Objects(normalized, "findings")
	if err != nil {
		return nil, err
	}
	derived := planning.DeriveReviewCloseoutBacklogFindings(findings, fmt.Sprint(normalized["scope"]))
	rows, err := contract.Objects(derived, "findings")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return contract.Object{"repo": repo.Object(), "schema_version": int64(1), "scope": normalized["scope"], "proposal_count": int64(0), "proposals": []any{}}, nil
	}
	return reviewDelta(contract.Object{"findings": derived, "policy": policy.Object()}, inventory, repo)
}

func closeoutInventoryRequest(summary contract.Object, policy CloseoutPolicy) (BacklogInventoryRequest, error) {
	backlog, err := contract.ObjectAt(summary, "backlog")
	if err != nil {
		return BacklogInventoryRequest{}, err
	}
	delta, err := contract.ObjectAt(backlog, "delta")
	if err != nil {
		return BacklogInventoryRequest{}, err
	}
	markers, err := reviewCommentMarkers(delta)
	if err != nil {
		return BacklogInventoryRequest{}, err
	}
	if summary["mode"] == "epic-closeout" {
		markers = append(markers, closeoutAuditMarker)
	}
	sort.Strings(markers)
	request := BacklogInventoryRequest{Projects: []ProjectScope{policy.ReviewBacklog.Project}, CommentMarkers: markers}
	if summary["mode"] == "epic-closeout" {
		request.IncludeRelationships = true
	} else {
		findings, err := closeoutNormalizedFindings(summary)
		if err != nil {
			return request, err
		}
		backlogFindings := planning.DeriveReviewCloseoutBacklogFindings(mustObjects(findings, "findings"), fmt.Sprint(findings["scope"]))
		rows, _ := contract.Objects(backlogFindings, "findings")
		if len(rows) > 0 {
			request.IncludeRelationships, err = findingsRequireRelationships(backlogFindings)
			if err != nil {
				return request, err
			}
		}
	}
	if err := validateInventoryRequest(request); err != nil {
		return request, err
	}
	return request, nil
}

func closeoutNestedPlan(plan contract.Plan, summary contract.Object, policy CloseoutPolicy, inventory contract.Object, request BacklogInventoryRequest, delta contract.Object) (contract.Plan, error) {
	markers, err := reviewCommentMarkers(delta)
	if err != nil {
		return contract.Plan{}, err
	}
	request.CommentMarkers = markers
	innerInventory, err := closeoutFilterComments(inventory, request)
	if err != nil {
		return contract.Plan{}, err
	}
	findings, err := closeoutNormalizedFindings(summary)
	if err != nil {
		return contract.Plan{}, err
	}
	backlogFindings := planning.DeriveReviewCloseoutBacklogFindings(mustObjects(findings, "findings"), fmt.Sprint(findings["scope"]))
	return contract.Plan{
		Command: ReviewBacklogCommand, Repository: plan.Repository, CapturedAt: plan.CapturedAt,
		Data:    contract.Object{"findings": backlogFindings, "policy": policy.ReviewBacklog.Object(), "inventory_request": request.Object(), "inventory": innerInventory},
		Sources: backlogPlanSources(innerInventory, request),
	}, nil
}

func closeoutFilterComments(inventory contract.Object, request BacklogInventoryRequest) (contract.Object, error) {
	out, err := contract.Clone(inventory)
	if err != nil {
		return nil, err
	}
	rows, err := contract.Objects(out, "comments")
	if err != nil {
		return nil, err
	}
	filtered := []contract.Object{}
	for _, row := range rows {
		body, _ := contract.String(row, "body")
		for _, marker := range request.CommentMarkers {
			if strings.Contains(body, marker) {
				filtered = append(filtered, row)
				break
			}
		}
	}
	prov, err := contract.ObjectAt(out, "provenance")
	if err != nil {
		return nil, err
	}
	if len(request.CommentMarkers) == 0 {
		prov["comments_source"] = "not_requested"
	} else {
		prov["comments_source"] = "github_api"
	}
	if request.IncludeRelationships {
		prov["relationships_source"] = "github_api"
	} else {
		prov["relationships_source"] = "not_requested"
		graph, err := contract.ObjectAt(out, "issue_inventory")
		if err != nil {
			return nil, err
		}
		issues, err := contract.Objects(graph, "issues")
		if err != nil {
			return nil, err
		}
		for _, issue := range issues {
			delete(issue, "blocked_by_numbers")
			delete(issue, "child_numbers")
			delete(issue, "parent_number")
		}
	}
	prov["comment_markers"] = stringSliceAny(request.CommentMarkers)
	out["comments"] = backlogObjectsAsAny(filtered)
	return out, nil
}

func closeoutJoinProject(inventory contract.Object, scope ProjectScope) (contract.Object, error) {
	graphRaw, err := contract.ObjectAt(inventory, "issue_inventory")
	if err != nil {
		return nil, err
	}
	graph, err := contract.Clone(graphRaw)
	if err != nil {
		return nil, err
	}
	project, err := governanceProject(inventory, scope)
	if err != nil {
		return nil, err
	}
	items, err := contract.Objects(project, "items")
	if err != nil {
		return nil, err
	}
	byNumber := map[int64]contract.Object{}
	for _, item := range items {
		number, err := contract.PositiveInteger(item["number"])
		if err != nil || byNumber[number] != nil {
			return nil, errors.New("closeout Project inventory has missing or duplicate issue membership")
		}
		byNumber[number] = item
	}
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		number, _ := contract.PositiveInteger(issue["number"])
		item := byNumber[number]
		if item == nil {
			issue["in_project"] = false
			issue["field_values"] = contract.Object{}
			issue["project_item"] = nil
			continue
		}
		fields, err := contract.ObjectAt(item, "field_values")
		if err != nil {
			return nil, err
		}
		itemID, err := contract.Nonempty(item, "item_id")
		if err != nil {
			return nil, err
		}
		archived, err := contract.Bool(item, "archived")
		if err != nil {
			return nil, err
		}
		issue["in_project"], issue["field_values"] = true, fields
		issue["project_item"] = contract.Object{"item_id": itemID, "archived": archived, "field_values": fields}
	}
	graph["issues"] = backlogObjectsAsAny(issues)
	graph["project"] = project["project"]
	provenance, err := contract.ObjectAt(graph, "provenance")
	if err != nil {
		return nil, err
	}
	provenance["project_joins_live"] = true
	return graph, nil
}

func closeoutOpenGraph(graph contract.Object) (contract.Object, error) {
	out, err := contract.Clone(graph)
	if err != nil {
		return nil, err
	}
	issues, err := contract.Objects(out, "issues")
	if err != nil {
		return nil, err
	}
	openNumbers := map[int64]bool{}
	open := []contract.Object{}
	for _, issue := range issues {
		if issue["state"] != "OPEN" {
			continue
		}
		number, _ := contract.PositiveInteger(issue["number"])
		openNumbers[number] = true
		open = append(open, issue)
	}
	for _, issue := range open {
		for _, key := range []string{"child_numbers", "blocked_by_numbers"} {
			values, err := contract.Array(issue, key)
			if err != nil {
				return nil, err
			}
			filtered := []any{}
			for _, raw := range values {
				number, err := contract.PositiveInteger(raw)
				if err != nil {
					return nil, err
				}
				if openNumbers[number] {
					filtered = append(filtered, number)
				}
			}
			issue[key] = filtered
		}
		blockers := []any{}
		issueNumber, _ := contract.PositiveInteger(issue["number"])
		for _, candidate := range open {
			children, _ := contract.Array(candidate, "blocked_by_numbers")
			for _, raw := range children {
				number, _ := contract.PositiveInteger(raw)
				if number == issueNumber {
					other, _ := contract.PositiveInteger(candidate["number"])
					blockers = append(blockers, other)
				}
			}
		}
		issue["blocking_numbers"] = blockers
	}
	out["issues"] = backlogObjectsAsAny(open)
	return out, nil
}

func closeoutLiveSummary(summary contract.Object, policy CloseoutPolicy, inventory contract.Object, repo contract.Repository) (contract.Object, error) {
	out, err := contract.Clone(summary)
	if err != nil {
		return nil, err
	}
	if summary["mode"] != "epic-closeout" {
		return out, nil
	}
	graph, err := closeoutJoinProject(inventory, policy.ReviewBacklog.Project)
	if err != nil {
		return nil, err
	}
	openGraph, err := closeoutOpenGraph(graph)
	if err != nil {
		return nil, err
	}
	epicNumber, err := contract.PositiveInteger(summary["epic_issue_number"])
	if err != nil {
		return nil, err
	}
	epicState, err := planning.EpicIssueState(openGraph, epicNumber)
	if err != nil {
		return nil, err
	}
	project, _ := contract.ObjectAt(openGraph, "project")
	openIssues, _ := contract.Objects(openGraph, "issues")
	governancePayload, err := governance.GovernanceCheck(policy.GovernanceCheck, contract.Object{"repo": repo.Object(), "project": project, "issues": backlogObjectsAsAny(openIssues)})
	if err != nil {
		return nil, err
	}
	backlogPayload, err := governance.BacklogAudit(policy.BacklogAudit, openGraph, nil)
	if err != nil {
		return nil, err
	}
	derivedGovernance, err := planning.MatchingGovernanceFindings(governancePayload, epicNumber, "governance-check")
	if err != nil {
		return nil, err
	}
	derivedBacklog, err := planning.MatchingGovernanceFindings(backlogPayload, epicNumber, "backlog-audit")
	if err != nil {
		return nil, err
	}
	derived := append(derivedGovernance, derivedBacklog...)
	epicState["derived_findings"] = derived
	reasons := []any{}
	findings, _ := contract.Objects(summary, "findings")
	for _, finding := range findings {
		if finding["blocking"] == true {
			reasons = append(reasons, finding["summary"])
		}
	}
	children, _ := contract.Array(epicState, "open_child_numbers")
	if len(children) > 0 {
		reasons = append(reasons, "Epic still has open child issues: "+closeoutIssueList(children)+".")
	}
	blockers, _ := contract.Array(epicState, "blocked_by_numbers")
	if len(blockers) > 0 {
		reasons = append(reasons, "Epic is blocked by open dependencies: "+closeoutIssueList(blockers)+".")
	}
	for _, finding := range derived {
		row := finding.(map[string]any)
		reasons = append(reasons, fmt.Sprintf("[%s] %s", row["source"], row["message"]))
	}
	out["epic_state"], out["blocking_reasons"] = epicState, reasons
	out["closeout_ready"] = len(reasons) == 0
	return out, nil
}

func closeoutIssueList(values []any) string {
	parts := make([]string, 0, len(values))
	for _, raw := range values {
		number, err := contract.PositiveInteger(raw)
		if err == nil {
			parts = append(parts, fmt.Sprintf("#%d", number))
		}
	}
	return strings.Join(parts, ", ")
}

func closeoutCommentOperation(plan contract.Plan, summary contract.Object, policy CloseoutPolicy, inventory contract.Object, nested []contract.Operation) (contract.Operation, error) {
	graph, err := contract.ObjectAt(inventory, "issue_inventory")
	if err != nil {
		return contract.Operation{}, err
	}
	epicNumber, err := contract.PositiveInteger(summary["epic_issue_number"])
	if err != nil {
		return contract.Operation{}, err
	}
	issue := backlogIssueByNumber(mustObjects(graph, "issues"), epicNumber)
	if issue == nil || issue["state"] != "OPEN" {
		return contract.Operation{}, errors.New("closeout target must remain the reviewed open epic")
	}
	issueID, err := contract.Nonempty(issue, "id")
	if err != nil {
		return contract.Operation{}, err
	}
	var current contract.Object
	for _, comment := range mustObjects(inventory, "comments") {
		if comment["issue_number"] == epicNumber && strings.Contains(fmt.Sprint(comment["body"]), closeoutAuditMarker) {
			if current != nil {
				return contract.Operation{}, errors.New("closeout audit marker is ambiguous on target epic")
			}
			current = comment
		}
	}
	var commentID, body any
	if current != nil {
		commentID, body = current["id"], current["body"]
	}
	ids := make([]any, 0, len(nested))
	serialized := make([]any, 0, len(nested))
	for _, op := range nested {
		ids = append(ids, op.ID)
		serialized = append(serialized, contract.Object{"id": op.ID, "kind": op.Kind, "target": op.Target, "before": op.Before, "after": op.After})
	}
	return contract.Operation{
		ID: "closeout:comment", Kind: "issue-comment-upsert",
		Target: contract.Object{"issue_number": epicNumber, "issue_id": issueID, "marker": closeoutAuditMarker, "comment_id": commentID},
		Before: contract.Object{"issue_state": "OPEN", "comment_id": commentID, "body": body, "renderer_version": nil, "captured_at": nil, "summary": nil, "policy": nil, "nested_operation_ids": nil, "nested_operations": nil, "proposal_action_count": nil},
		After:  contract.Object{"renderer_version": closeoutRendererV1, "captured_at": plan.CapturedAt, "summary": summary, "policy": policy.Object(), "nested_operation_ids": ids, "nested_operations": serialized, "proposal_action_count": nestedProposalCount(summary)},
	}, nil
}

func nestedProposalCount(summary contract.Object) int64 {
	backlog, _ := contract.ObjectAt(summary, "backlog")
	count, _ := contract.Integer(backlog["proposal_count"])
	return count
}

func renderCloseoutEpicBody(summary contract.Object, actionCount int64, capturedAt string) (string, error) {
	if _, err := time.Parse(time.RFC3339Nano, capturedAt); err != nil {
		return "", errors.New("closeout render time must be RFC3339")
	}
	findings, _ := contract.Integer(summary["finding_count"])
	blocking, _ := contract.Integer(summary["blocking_finding_count"])
	mode, _ := contract.Nonempty(summary, "mode")
	scope, _ := contract.Nonempty(summary, "scope")
	lines := []string{
		"## Review / closeout audit " + capturedAt,
		"",
		"- mode: " + mode,
		"- scope: " + scope,
		fmt.Sprintf("- findings: %d", findings),
		fmt.Sprintf("- blocking findings: %d", blocking),
		fmt.Sprintf("- backlog follow-ups applied: %d", actionCount),
	}
	if mode == "review" {
		status, err := contract.Nonempty(summary, "status")
		if err != nil {
			return "", err
		}
		lines = append(lines, "- status: "+status)
	} else {
		ready, err := contract.Bool(summary, "closeout_ready")
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("- closeout ready: %s", map[bool]string{true: "yes", false: "no"}[ready]))
		epic, err := contract.ObjectAt(summary, "epic_state")
		if err != nil {
			return "", err
		}
		children, _ := contract.Array(epic, "open_child_numbers")
		blockers, _ := contract.Array(epic, "blocked_by_numbers")
		childText, blockerText := closeoutIssueList(children), closeoutIssueList(blockers)
		if childText == "" {
			childText = "none"
		}
		if blockerText == "" {
			blockerText = "none"
		}
		lines = append(lines, "- open child issues: "+childText, "- open blockers: "+blockerText)
	}
	reasons, err := contract.Strings(summary["blocking_reasons"])
	if err != nil {
		return "", err
	}
	if len(reasons) > 0 {
		lines = append(lines, "", "Blocking reasons:")
		for _, reason := range reasons {
			lines = append(lines, "- "+reason)
		}
	}
	content := strings.Join(lines, "\n")
	return closeoutAuditMarker + "\n" + strings.TrimSpace(content) + "\n", nil
}

func closeoutNestedPrefix(prepared closeoutPrepared, receipts []contract.Object, through string) ([]contract.Object, error) {
	byID := map[string]contract.Operation{}
	for _, op := range prepared.nestedOps {
		byID[op.ID] = op
	}
	prefix := []contract.Object{}
	for _, receipt := range receipts {
		id, err := contract.Nonempty(receipt, "id")
		if err != nil {
			return nil, err
		}
		if id == through {
			break
		}
		if receipt["status"] != "completed" {
			return nil, errors.New("closeout nested action set is not completely acknowledged before the final comment")
		}
		op, ok := byID[id]
		if !ok {
			return nil, errors.New("closeout receipt is outside nested action set")
		}
		result, err := contract.ObjectAt(receipt, "result")
		if err != nil {
			return nil, err
		}
		if receipt["operation_id"] != result["operation_id"] {
			return nil, errors.New("closeout nested receipt changed its durable operation identity")
		}
		if err = (&Backlog{}).ValidateReceipt(prepared.nestedPlan, op, result); err != nil {
			return nil, err
		}
		prefix = append(prefix, receipt)
	}
	if through == "closeout:comment" && len(prefix) != len(prepared.nestedOps) {
		return nil, errors.New("closeout comment cannot precede incomplete nested actions")
	}
	return prefix, nil
}

func closeoutExpectedInventory(prepared closeoutPrepared, receipts []contract.Object) (contract.Object, error) {
	expected, err := contract.Clone(prepared.inventory)
	if err != nil {
		return nil, err
	}
	prefix := []contract.Object{}
	byID := map[string]contract.Operation{}
	for _, op := range prepared.operations {
		byID[op.ID] = op
	}
	for _, receipt := range receipts {
		id, err := contract.Nonempty(receipt, "id")
		if err != nil {
			return nil, err
		}
		if receipt["status"] != "completed" {
			return nil, errors.New("closeout projected state requires positive completion for every preceding operation")
		}
		op, ok := byID[id]
		if !ok {
			return nil, errors.New("closeout journal receipt is outside derived action set")
		}
		result, err := contract.ObjectAt(receipt, "result")
		if err != nil {
			return nil, err
		}
		if receipt["operation_id"] != result["operation_id"] {
			return nil, errors.New("closeout completion differs from durable dispatch identity")
		}
		if id == "closeout:comment" {
			if len(prefix) != len(prepared.nestedOps) {
				return nil, errors.New("closeout comment receipt precedes incomplete nested operations")
			}
			expected, err = closeoutProjectCommentAcknowledgement(expected, prepared, op, result, prefix)
		} else {
			if !containsOperation(prepared.nestedOps, op) {
				return nil, errors.New("closeout nested receipt is outside re-derived backlog actions")
			}
			expected, err = backlogProjectAcknowledgement(expected, prepared.nestedPlan, op, result, prefix)
			prefix = append(prefix, receipt)
		}
		if err != nil {
			return nil, err
		}
	}
	return expected, nil
}

func closeoutProjectCommentAcknowledgement(inventory contract.Object, prepared closeoutPrepared, op contract.Operation, ack contract.Object, nestedReceipts []contract.Object) (contract.Object, error) {
	if err := validateCloseoutAcknowledgementEnvelope(prepared, op, ack); err != nil {
		return nil, err
	}
	projected, err := contract.Clone(inventory)
	if err != nil {
		return nil, err
	}
	number, _ := contract.PositiveInteger(op.Target["issue_number"])
	issue := backlogIssueByNumber(mustObjects(mustObject(projected, "issue_inventory"), "issues"), number)
	if issue == nil || issue["id"] != op.Target["issue_id"] || issue["state"] != "OPEN" {
		return nil, errors.New("closeout comment target identity or open state differs from projected after-state")
	}
	liveSummary, err := closeoutLiveSummary(prepared.summary, prepared.policy, projected, prepared.nestedPlan.Repository)
	if err != nil {
		return nil, err
	}
	if _, err = closeoutNestedPrefix(prepared, nestedReceipts, "closeout:comment"); err != nil {
		return nil, err
	}
	body, err := renderCloseoutEpicBody(liveSummary, nestedProposalCount(prepared.summary), prepared.nestedPlan.CapturedAt)
	if err != nil {
		return nil, err
	}
	providerResult, err := contract.ObjectAt(ack, "provider_result")
	if err != nil {
		return nil, err
	}
	comment, err := backlogCreatedComment(providerResult, prepared.nestedPlan.Repository, number)
	if err != nil {
		return nil, err
	}
	operationID, _ := contract.Nonempty(ack, "operation_id")
	marker, err := native.OperationMarker(operationID)
	if err != nil {
		return nil, err
	}
	wantBody := body
	if op.Target["comment_id"] == nil {
		wantBody += "\n\n" + marker
	}
	if comment["body"] != wantBody {
		return nil, errors.New("closeout comment ACK differs from deterministic late-bound body")
	}
	comments, err := contract.Objects(projected, "comments")
	if err != nil {
		return nil, err
	}
	commentID := comment["id"]
	oldID := op.Target["comment_id"]
	found := false
	filtered := make([]contract.Object, 0, len(comments)+1)
	for _, old := range comments {
		isMarker := old["issue_number"] == number && strings.Contains(fmt.Sprint(old["body"]), closeoutAuditMarker)
		isID := old["id"] == oldID && oldID != nil
		if isMarker {
			if oldID == nil || old["id"] != oldID || old["body"] != op.Before["body"] {
				return nil, errors.New("closeout marked comment before-state differs from projection")
			}
			found = true
		}
		if isID {
			found = true
			continue
		}
		if isMarker {
			continue
		}
		filtered = append(filtered, old)
	}
	if (oldID != nil) != found || oldID != nil && commentID != oldID {
		return nil, errors.New("closeout comment ACK changed the reviewed comment identity")
	}
	projected["comments"] = backlogObjectsAsAny(append(filtered, comment))
	return projected, nil
}

func mustObject(object contract.Object, key string) contract.Object {
	value, _ := contract.ObjectAt(object, key)
	return value
}

// Closeout implements the durable adapter. Nested review-backlog primitives
// keep their existing typed provider contracts; the final comment persists its
// untouched native ACK before an independent complete inventory read.
type Closeout struct{ Provider BacklogProvider }

func (a Closeout) Operations(plan contract.Plan) ([]contract.Operation, error) {
	return closeoutOperations(plan)
}

func (a Closeout) ValidateAcknowledgement(plan contract.Plan, op contract.Operation, ack contract.Object) error {
	prepared, err := parseCloseoutPlan(plan)
	if err != nil {
		return err
	}
	if containsOperation(prepared.nestedOps, op) {
		return (&Backlog{}).ValidateAcknowledgement(prepared.nestedPlan, op, ack)
	}
	if prepared.closeoutOp == nil || !same(*prepared.closeoutOp, op) {
		return errors.New("closeout acknowledgement is outside the derived operation set")
	}
	return validateCloseoutAcknowledgementEnvelope(prepared, op, ack)
}

func validateCloseoutAcknowledgementEnvelope(prepared closeoutPrepared, op contract.Operation, ack contract.Object) error {
	allowed := keySet("kind", "primitive_id", "repository", "operation_id", "target", "before", "after", "acknowledged", "provider_result", "after_verified")
	for key := range ack {
		if !allowed[key] {
			return errors.New("closeout acknowledgement has unsupported fields")
		}
	}
	if len(ack) != len(allowed) && len(ack) != len(allowed)-1 {
		return errors.New("closeout acknowledgement has unsupported or missing fields")
	}
	for key := range allowed {
		if key == "after_verified" {
			continue
		}
		if _, exists := ack[key]; !exists {
			return errors.New("closeout acknowledgement is missing a required field")
		}
	}
	if ack["kind"] != op.Kind || ack["primitive_id"] != op.ID || ack["acknowledged"] != true || !same(ack["target"], op.Target) || !same(ack["before"], op.Before) || !same(ack["after"], op.After) {
		return errors.New("closeout acknowledgement does not match the reviewed final operation")
	}
	if value, exists := ack["after_verified"]; exists && value != nil && value != true {
		return errors.New("closeout acknowledgement after-state flag is invalid")
	}
	repository, err := contract.ObjectAt(ack, "repository")
	if err != nil {
		return err
	}
	repo, err := contract.ParseRepository(repository)
	if err != nil || repo != prepared.nestedPlan.Repository {
		return errors.New("closeout acknowledgement belongs to another repository")
	}
	nonce, err := contract.Nonempty(ack, "operation_id")
	if err != nil {
		return err
	}
	if _, err = native.OperationMarker(nonce); err != nil {
		return err
	}
	result, err := contract.ObjectAt(ack, "provider_result")
	if err != nil {
		return err
	}
	number, _ := contract.PositiveInteger(op.Target["issue_number"])
	commentID, err := contract.PositiveInteger(result["id"])
	if err != nil {
		return err
	}
	if prior := op.Target["comment_id"]; prior != nil && prior != commentID {
		return errors.New("closeout comment ACK changed existing comment identity")
	}
	body, err := contract.String(result, "body")
	if err != nil || !strings.Contains(body, closeoutAuditMarker) {
		return errors.New("closeout comment ACK does not contain its reserved marker")
	}
	url, err := contract.Nonempty(result, "html_url")
	if err != nil || !validCommentURL(url, repo, number, commentID) {
		return errors.New("closeout comment ACK URL has foreign issue/comment identity")
	}
	return nil
}

func (a Closeout) ValidateReceipt(plan contract.Plan, op contract.Operation, result contract.Object) error {
	if err := a.ValidateAcknowledgement(plan, op, result); err != nil {
		return err
	}
	if result["after_verified"] != true {
		return errors.New("closeout receipt lacks a verified complete after-state")
	}
	return nil
}

func (a Closeout) Preflight(ctx context.Context, plan contract.Plan, receipts []contract.Object) error {
	if a.Provider == nil {
		return errors.New("closeout adapter requires a provider")
	}
	prepared, err := parseCloseoutPlan(plan)
	if err != nil {
		return err
	}
	expected, err := closeoutExpectedInventory(prepared, receipts)
	if err != nil {
		return err
	}
	raw, err := a.Provider.BacklogInventory(ctx, prepared.request)
	if err != nil {
		return err
	}
	actual, err := normalizeBacklogInventory(raw, plan.Repository, prepared.request)
	if err != nil {
		return err
	}
	if !same(expected, actual) {
		return errors.New("complete closeout inventory drifted from reviewed and acknowledged state")
	}
	return nil
}

func (a Closeout) Dispatch(ctx context.Context, plan contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, error) {
	return nil, errors.New("closeout writes require durable native acknowledgement persistence")
}

func (a Closeout) DispatchAcknowledged(ctx context.Context, plan contract.Plan, op contract.Operation, nonce string, receipts []contract.Object, persist func(contract.Object) error) (contract.Object, error) {
	if a.Provider == nil || persist == nil {
		return nil, errors.New("closeout dispatch requires provider and durable ACK callback")
	}
	prepared, err := parseCloseoutPlan(plan)
	if err != nil {
		return nil, err
	}
	if containsOperation(prepared.nestedOps, op) {
		return (Backlog{Provider: a.Provider}).DispatchAcknowledged(ctx, prepared.nestedPlan, op, nonce, receipts, persist)
	}
	if prepared.closeoutOp == nil || !same(*prepared.closeoutOp, op) {
		return nil, errors.New("closeout dispatch operation is outside derived plan")
	}
	if _, err := native.OperationMarker(nonce); err != nil {
		return nil, err
	}
	prior, err := closeoutExpectedInventory(prepared, receipts)
	if err != nil {
		return nil, err
	}
	live, err := closeoutLiveSummary(prepared.summary, prepared.policy, prior, plan.Repository)
	if err != nil {
		return nil, err
	}
	body, err := renderCloseoutEpicBody(live, nestedProposalCount(prepared.summary), plan.CapturedAt)
	if err != nil {
		return nil, err
	}
	number, _ := contract.PositiveInteger(op.Target["issue_number"])
	issue := backlogIssueByNumber(mustObjects(mustObject(prior, "issue_inventory"), "issues"), number)
	if issue == nil || issue["id"] != op.Target["issue_id"] || issue["state"] != "OPEN" {
		return nil, errors.New("closeout epic identity or state drifted before comment write")
	}
	var providerResult contract.Object
	if commentID, exists := op.Target["comment_id"]; exists && commentID != nil {
		id, err := contract.PositiveInteger(commentID)
		if err != nil {
			return nil, err
		}
		providerResult, err = a.Provider.UpdateIssueComment(ctx, nonce, number, id, body)
	} else {
		providerResult, err = a.Provider.CreateIssueComment(ctx, nonce, number, body)
	}
	if err != nil {
		return nil, err
	}
	ack := contract.Object{"kind": op.Kind, "primitive_id": op.ID, "repository": plan.Repository.Object(), "operation_id": nonce, "target": op.Target, "before": op.Before, "after": op.After, "acknowledged": true, "provider_result": providerResult}
	if err = a.ValidateAcknowledgement(plan, op, ack); err != nil {
		return nil, err
	}
	if err = persist(ack); err != nil {
		return nil, err
	}
	projected, err := closeoutProjectCommentAcknowledgement(prior, prepared, op, ack, receipts)
	if err != nil {
		return nil, err
	}
	raw, err := a.Provider.BacklogInventory(ctx, prepared.request)
	if err != nil {
		return nil, fmt.Errorf("closeout comment was acknowledged but after-state read failed: %w", err)
	}
	actual, err := normalizeBacklogInventory(raw, plan.Repository, prepared.request)
	if err != nil {
		return nil, fmt.Errorf("closeout comment was acknowledged but after-state is invalid: %w", err)
	}
	if !same(projected, actual) {
		return nil, errors.New("closeout comment was acknowledged but complete after-state differs from reviewed body")
	}
	ack["after_verified"] = true
	return ack, nil
}

func (a Closeout) Observe(ctx context.Context, plan contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, contract.Object, error) {
	if a.Provider == nil {
		return nil, nil, errors.New("closeout adapter requires a provider")
	}
	prepared, err := parseCloseoutPlan(plan)
	if err != nil {
		return nil, nil, err
	}
	var ack contract.Object
	for _, receipt := range receipts {
		if receipt["id"] == op.ID && receipt["status"] == "unknown" && receipt["operation_id"] == nonce {
			ack, _ = receipt["acknowledgement"].(map[string]any)
		}
	}
	if ack == nil || ack["operation_id"] != nonce {
		return nil, nil, errors.New("ambiguous closeout dispatch has no durable native acknowledgement; no write was retried")
	}
	if err = a.ValidateAcknowledgement(plan, op, ack); err != nil {
		return nil, nil, err
	}
	if containsOperation(prepared.nestedOps, op) {
		return (Backlog{Provider: a.Provider}).Observe(ctx, prepared.nestedPlan, op, nonce, receipts)
	}
	priorReceipts, err := closeoutNestedPrefix(prepared, receipts, op.ID)
	if err != nil {
		return nil, nil, err
	}
	prior, err := closeoutExpectedInventory(prepared, priorReceipts)
	if err != nil {
		return nil, nil, err
	}
	projected, err := closeoutProjectCommentAcknowledgement(prior, prepared, op, ack, priorReceipts)
	if err != nil {
		return nil, nil, err
	}
	raw, err := a.Provider.BacklogInventory(ctx, prepared.request)
	if err != nil {
		return nil, nil, err
	}
	actual, err := normalizeBacklogInventory(raw, plan.Repository, prepared.request)
	if err != nil {
		return nil, nil, err
	}
	if !same(projected, actual) {
		return nil, nil, errors.New("captured closeout ACK does not match deterministic body and complete live after-state")
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

var _ apply.Adapter = Closeout{}
var _ apply.AcknowledgingAdapter = Closeout{}
