package workflow

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/governance"
	"github.com/coreycoto/gh-steward/internal/native"
)

const ExecutionSyncCommand = "execution-sync"

// ExecutionSelector is exact consumer scope. Project is required when Project
// synchronization is enabled and must be nil when the caller explicitly skips it.
type ExecutionSelector struct {
	IssueNumber       int64
	PullRequestNumber int64
	SkipProjectSync   bool
	Project           *ProjectScope
}

func (s ExecutionSelector) Object() contract.Object {
	var project any
	if s.Project != nil {
		project = s.Project.Object()
	}
	return contract.Object{
		"issue_number": s.IssueNumber, "pull_request_number": s.PullRequestNumber,
		"skip_project_sync": s.SkipProjectSync, "project": project,
	}
}

type ExecutionStatusPolicy struct{ Done, Active, Todo string }

type ExecutionPolicy struct {
	Statuses                ExecutionStatusPolicy
	StatusField             string
	PRLinkMarkerPrefix      string
	PRLinkNumberPattern     string
	LinkedIssueMarkerPrefix string
	LinkStateMarkerPrefix   string
}

func (p ExecutionPolicy) Object() contract.Object {
	return contract.Object{
		"statuses":     contract.Object{"done": p.Statuses.Done, "active": p.Statuses.Active, "todo": p.Statuses.Todo},
		"status_field": p.StatusField, "pr_link_marker_prefix": p.PRLinkMarkerPrefix,
		"pr_link_number_pattern":     p.PRLinkNumberPattern,
		"linked_issue_marker_prefix": p.LinkedIssueMarkerPrefix,
		"link_state_marker_prefix":   p.LinkStateMarkerPrefix,
	}
}

type ExecutionInventoryRequest struct {
	Selector ExecutionSelector
	Policy   ExecutionPolicy
}

func (r ExecutionInventoryRequest) Object() contract.Object {
	return contract.Object{"selector": r.Selector.Object(), "policy": r.Policy.Object()}
}

// ExecutionProvider is the typed I/O seam for this workflow. The returned
// provider responses are retained verbatim as durable native acknowledgements.
type ExecutionProvider interface {
	ExecutionInventory(context.Context, ExecutionInventoryRequest) (contract.Object, error)
	ReopenIssue(context.Context, string, int64) (contract.Object, error)
	CloseIssue(context.Context, string, int64) (contract.Object, error)
	CreateIssueComment(context.Context, string, int64, string) (contract.Object, error)
	UpdateIssueComment(context.Context, string, int64, int64, string) (contract.Object, error)
	AddProjectIssue(context.Context, string, string, string) (contract.Object, error)
	SetProjectField(context.Context, string, string, string, ProjectField, ProjectFieldValue) (contract.Object, error)
}

func validateExecutionSelector(selector ExecutionSelector, repo contract.Repository) error {
	issueSelected := selector.IssueNumber > 0
	prSelected := selector.PullRequestNumber > 0
	if issueSelected == prSelected || selector.IssueNumber < 0 || selector.PullRequestNumber < 0 {
		return errors.New("execution selector requires exactly one positive issue or pull request number")
	}
	if selector.SkipProjectSync {
		if selector.Project != nil {
			return errors.New("execution selector cannot scope a Project when project synchronization is skipped")
		}
		return nil
	}
	if selector.Project == nil {
		return errors.New("execution selector requires an exact Project scope unless project sync is explicitly skipped")
	}
	return validateProjectScope(*selector.Project, repo)
}

func validateExecutionPolicy(policy ExecutionPolicy) error {
	for key, value := range map[string]string{
		"done status": policy.Statuses.Done, "active status": policy.Statuses.Active,
		"todo status": policy.Statuses.Todo, "Project status field": policy.StatusField,
		"PR link marker prefix": policy.PRLinkMarkerPrefix, "PR number pattern": policy.PRLinkNumberPattern,
		"linked issue marker prefix": policy.LinkedIssueMarkerPrefix, "link state marker prefix": policy.LinkStateMarkerPrefix,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("execution policy requires an explicit %s", key)
		}
	}
	if policy.Statuses.Done == policy.Statuses.Active || policy.Statuses.Done == policy.Statuses.Todo || policy.Statuses.Active == policy.Statuses.Todo {
		return errors.New("execution status policy values must be distinct")
	}
	re, err := regexp.Compile(policy.PRLinkNumberPattern)
	if err != nil || re.SubexpIndex("number") < 1 {
		return errors.New("PR link number pattern must compile and define a named number group")
	}
	return nil
}

func parseExecutionSelector(raw contract.Object, repo contract.Repository) (ExecutionSelector, error) {
	if !executionKeys(raw, "issue_number", "pull_request_number", "skip_project_sync", "project") {
		return ExecutionSelector{}, errors.New("execution selector has unsupported or missing fields")
	}
	issue, err := contract.Integer(raw["issue_number"])
	if err != nil {
		return ExecutionSelector{}, err
	}
	pr, err := contract.Integer(raw["pull_request_number"])
	if err != nil {
		return ExecutionSelector{}, err
	}
	skip, err := contract.Bool(raw, "skip_project_sync")
	if err != nil {
		return ExecutionSelector{}, err
	}
	selector := ExecutionSelector{IssueNumber: issue, PullRequestNumber: pr, SkipProjectSync: skip}
	if raw["project"] != nil {
		projectRaw, err := contract.ObjectAt(raw, "project")
		if err != nil {
			return selector, err
		}
		project, err := backlogParseProjectScope(projectRaw)
		if err != nil {
			return selector, err
		}
		selector.Project = &project
	}
	return selector, validateExecutionSelector(selector, repo)
}

func parseExecutionPolicy(raw contract.Object) (ExecutionPolicy, error) {
	if !executionKeys(raw, "statuses", "status_field", "pr_link_marker_prefix", "pr_link_number_pattern", "linked_issue_marker_prefix", "link_state_marker_prefix") {
		return ExecutionPolicy{}, errors.New("execution policy has unsupported or missing fields")
	}
	statuses, err := contract.ObjectAt(raw, "statuses")
	if err != nil || !executionKeys(statuses, "done", "active", "todo") {
		return ExecutionPolicy{}, errors.New("execution policy statuses must contain only done, active and todo")
	}
	policy := ExecutionPolicy{}
	policy.Statuses.Done, err = contract.Nonempty(statuses, "done")
	if err != nil {
		return policy, err
	}
	policy.Statuses.Active, err = contract.Nonempty(statuses, "active")
	if err != nil {
		return policy, err
	}
	policy.Statuses.Todo, err = contract.Nonempty(statuses, "todo")
	if err != nil {
		return policy, err
	}
	if policy.StatusField, err = contract.Nonempty(raw, "status_field"); err != nil {
		return policy, err
	}
	if policy.PRLinkMarkerPrefix, err = contract.Nonempty(raw, "pr_link_marker_prefix"); err != nil {
		return policy, err
	}
	if policy.PRLinkNumberPattern, err = contract.Nonempty(raw, "pr_link_number_pattern"); err != nil {
		return policy, err
	}
	if policy.LinkedIssueMarkerPrefix, err = contract.Nonempty(raw, "linked_issue_marker_prefix"); err != nil {
		return policy, err
	}
	if policy.LinkStateMarkerPrefix, err = contract.Nonempty(raw, "link_state_marker_prefix"); err != nil {
		return policy, err
	}
	return policy, validateExecutionPolicy(policy)
}

func executionKeys(raw contract.Object, names ...string) bool {
	if len(raw) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := raw[name]; !ok {
			return false
		}
	}
	return true
}

func executionObjectKeys(raw contract.Object) []string {
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ResolveExecutionIssueNumber applies the retained PR-body precedence and
// cross-checks it against GitHub's complete closing-reference list. The bridge
// uses this same pure resolver before collecting the issue-scoped marker source.
func ResolveExecutionIssueNumber(pr contract.Object, policy ExecutionPolicy) (int64, error) {
	if err := validateExecutionPolicy(policy); err != nil {
		return 0, err
	}
	body, err := contract.String(pr, "body")
	if err != nil {
		return 0, err
	}
	closingRaw, ok := pr["closing_issue_numbers"]
	if !ok {
		return 0, errors.New("selected pull request requires complete closing issue references")
	}
	closing, ok := closingRaw.([]any)
	if !ok {
		return 0, errors.New("pull request closing issue references must be an array")
	}
	closingSet := map[int64]bool{}
	for _, raw := range closing {
		n, err := contract.PositiveInteger(raw)
		if err != nil || closingSet[n] {
			return 0, errors.New("pull request closing issue references must be unique positive issue numbers")
		}
		closingSet[n] = true
	}
	bodyNumber := executionIssueFromPRBody(body, policy.LinkedIssueMarkerPrefix)
	if bodyNumber > 0 {
		if len(closingSet) > 0 && !closingSet[bodyNumber] {
			return 0, errors.New("pull request body issue identity conflicts with complete closing references")
		}
		return bodyNumber, nil
	}
	if len(closingSet) == 1 {
		for number := range closingSet {
			return number, nil
		}
	}
	return 0, errors.New("pull request does not identify one unambiguous linked issue")
}

func executionIssueFromPRBody(body, linkedIssueMarkerPrefix string) int64 {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(regexp.QuoteMeta(linkedIssueMarkerPrefix) + `([0-9]+)\s*-->`),
		regexp.MustCompile(`(?i)\bIssue:\s*#([0-9]+)\b`),
		regexp.MustCompile(`(?i)\b(?:Closes|Refs)\s+#([0-9]+)\b`),
	}
	for _, pattern := range patterns {
		match := pattern.FindStringSubmatch(body)
		if len(match) != 2 {
			continue
		}
		number, err := strconv.ParseInt(match[1], 10, 64)
		if err == nil && number > 0 {
			return number
		}
	}
	return 0
}

// PrepareExecutionSync captures one fully scoped source set and composes an
// exact non-replaying plan for issue state, link-marker and Project Status writes.
func PrepareExecutionSync(ctx context.Context, provider ExecutionProvider, repo contract.Repository, selector ExecutionSelector, policy ExecutionPolicy, now time.Time) (contract.Plan, error) {
	if provider == nil {
		return contract.Plan{}, errors.New("execution synchronization requires a provider")
	}
	if err := validateExecutionSelector(selector, repo); err != nil {
		return contract.Plan{}, err
	}
	if err := validateExecutionPolicy(policy); err != nil {
		return contract.Plan{}, err
	}
	request := ExecutionInventoryRequest{Selector: selector, Policy: policy}
	raw, err := provider.ExecutionInventory(ctx, request)
	if err != nil {
		return contract.Plan{}, err
	}
	inventory, err := normalizeExecutionInventory(raw, repo, request)
	if err != nil {
		return contract.Plan{}, err
	}
	data := contract.Object{"selector": selector.Object(), "policy": policy.Object(), "inventory": inventory}
	sources := executionPlanSources(inventory)
	p := contract.Plan{Command: ExecutionSyncCommand, Repository: repo, Sources: sources, Data: data, CapturedAt: now.UTC().Format(time.RFC3339Nano)}
	ops, err := executionOperations(p)
	if err != nil {
		return contract.Plan{}, err
	}
	return contract.PreparePlan(ExecutionSyncCommand, repo, sources, data, ops, now)
}

// ExecutionRequestFromPlan reconstructs the exact selector and caller policy
// stored in a reviewed plan. Native adapters use it to recover Project scope
// during apply without ambient defaults.
func ExecutionRequestFromPlan(plan contract.Plan) (ExecutionInventoryRequest, error) {
	if plan.Command != ExecutionSyncCommand {
		return ExecutionInventoryRequest{}, errors.New("execution request requires an execution-sync plan")
	}
	selectorRaw, err := contract.ObjectAt(plan.Data, "selector")
	if err != nil {
		return ExecutionInventoryRequest{}, err
	}
	selector, err := parseExecutionSelector(selectorRaw, plan.Repository)
	if err != nil {
		return ExecutionInventoryRequest{}, err
	}
	policyRaw, err := contract.ObjectAt(plan.Data, "policy")
	if err != nil {
		return ExecutionInventoryRequest{}, err
	}
	policy, err := parseExecutionPolicy(policyRaw)
	if err != nil {
		return ExecutionInventoryRequest{}, err
	}
	return ExecutionInventoryRequest{Selector: selector, Policy: policy}, nil
}

func normalizeExecutionInventory(raw contract.Object, repo contract.Repository, request ExecutionInventoryRequest) (contract.Object, error) {
	canonical := executionKeys(raw, "repo", "repository_node_id", "default_branch", "issue_inventory", "issue", "pull_request", "linked_pr_inventory", "branch_inventory", "project_inventory", "provenance")
	if !canonical && !executionKeys(raw, "repo", "repository_node_id", "default_branch", "issue_inventory", "pull_request", "linked_pr_inventory", "branch_inventory", "project_inventory", "provenance") {
		return nil, errors.New("execution inventory has unsupported or missing fields")
	}
	if err := validateExecutionSelector(request.Selector, repo); err != nil {
		return nil, err
	}
	if err := validateExecutionPolicy(request.Policy); err != nil {
		return nil, err
	}
	rawRepo, err := contract.ObjectAt(raw, "repo")
	if err != nil {
		return nil, err
	}
	actualRepo, err := contract.ParseRepository(rawRepo)
	if err != nil || actualRepo != repo {
		return nil, errors.New("execution inventory targets another repository")
	}
	repositoryNodeID, err := contract.Nonempty(raw, "repository_node_id")
	if err != nil {
		return nil, err
	}
	defaultBranch, err := contract.Nonempty(raw, "default_branch")
	if err != nil {
		return nil, err
	}
	provenance, err := contract.ObjectAt(raw, "provenance")
	if err != nil || !executionKeys(provenance, "live", "complete", "issue_state", "repository_node_id", "default_branch_source", "pull_request_source", "linked_pr_source", "branch_source", "project_source", "selector", "policy") {
		return nil, errors.New("execution source provenance has unsupported or missing fields")
	}
	if provenance["live"] != true || provenance["complete"] != true || provenance["issue_state"] != "all" || provenance["repository_node_id"] != repositoryNodeID || provenance["default_branch_source"] != "github_api" || !same(provenance["selector"], request.Selector.Object()) || !same(provenance["policy"], request.Policy.Object()) {
		return nil, errors.New("execution source is incomplete, stale or outside the reviewed selector/policy")
	}
	graphRaw, err := contract.ObjectAt(raw, "issue_inventory")
	if err != nil {
		return nil, err
	}
	graph, err := normalizeExecutionIssueInventory(graphRaw, repo, repositoryNodeID)
	if err != nil {
		return nil, err
	}

	pr, hasPR, err := normalizeExecutionPullRequest(raw["pull_request"], repo)
	if err != nil {
		return nil, fmt.Errorf("normalize execution inventory pull request: %w", err)
	}
	issues := mustObjects(graph, "issues")
	issueNumber := request.Selector.IssueNumber
	if request.Selector.PullRequestNumber > 0 {
		if !hasPR || pr["number"] != request.Selector.PullRequestNumber {
			return nil, errors.New("requested pull request is absent from its exact live inventory")
		}
		issueNumber, err = ResolveExecutionIssueNumber(pr, request.Policy)
		if err != nil {
			return nil, err
		}
	}
	issue := backlogIssueByNumber(issues, issueNumber)
	if issue == nil {
		return nil, errors.New("execution selector issue is absent from complete all-state inventory")
	}
	if canonical && !same(raw["issue"], issue) {
		return nil, errors.New("canonical execution selected issue differs from complete all-state identity")
	}
	linked, linkedPRNumber, err := normalizeExecutionLinkedPR(raw["linked_pr_inventory"], repo, issueNumber, request.Policy)
	if err != nil {
		return nil, err
	}
	if request.Selector.IssueNumber > 0 {
		if linkedPRNumber == 0 {
			if hasPR {
				return nil, errors.New("issue selector pull request is not supported by the latest complete linked marker")
			}
		} else if !hasPR || linkedPRNumber != pr["number"] || linked["latest_pull_request_id"] != pr["id"] {
			return nil, errors.New("issue selector pull request differs from latest complete linked marker")
		}
	} else if linked["issue_number"] != issueNumber {
		return nil, errors.New("PR selector linked marker inventory targets another issue")
	}

	branchInventory, branchExists, err := normalizeExecutionBranches(raw["branch_inventory"], provenance["branch_source"], repo, repositoryNodeID, pr, hasPR)
	if err != nil {
		return nil, err
	}
	transition, err := executionTransition(request.Policy, issue, pr, hasPR, branchExists, defaultBranch)
	if err != nil {
		return nil, err
	}
	var project any
	projectSource := "not_requested"
	if hasPR && !request.Selector.SkipProjectSync {
		project, err = normalizeExecutionProject(raw["project_inventory"], repo, *request.Selector.Project, request.Policy, transition, graph)
		if err != nil {
			return nil, err
		}
		projectSource = "github_project_api"
	} else if raw["project_inventory"] != nil || provenance["project_source"] != "not_requested" {
		return nil, errors.New("execution Project inventory was supplied outside the requested scope")
	}
	if provenance["pull_request_source"] != func() string {
		if request.Selector.PullRequestNumber > 0 || linkedPRNumber > 0 {
			return "github_api"
		}
		return "not_requested"
	}() || provenance["linked_pr_source"] != "github_api" || provenance["branch_source"] != func() string {
		if transition["sync_state"] == "closed-unmerged-branch-live" || transition["sync_state"] == "closed-unmerged-branch-deleted" {
			return "github_api"
		}
		return "not_requested"
	}() || provenance["project_source"] != projectSource {
		return nil, errors.New("execution source provenance does not match required state-dependent facets")
	}
	var normalizedPR any
	if hasPR {
		normalizedPR = pr
	}
	return contract.Object{
		"repo": repo.Object(), "repository_node_id": repositoryNodeID, "default_branch": defaultBranch,
		"issue_inventory": graph, "issue": issue, "pull_request": normalizedPR,
		"linked_pr_inventory": linked, "branch_inventory": branchInventory, "project_inventory": project,
		"provenance": contract.Object{"live": true, "complete": true, "issue_state": "all", "repository_node_id": repositoryNodeID,
			"default_branch_source": "github_api", "pull_request_source": provenance["pull_request_source"],
			"linked_pr_source": "github_api", "branch_source": provenance["branch_source"], "project_source": projectSource,
			"selector": request.Selector.Object(), "policy": request.Policy.Object()},
	}, nil
}

func normalizeExecutionIssueInventory(raw contract.Object, repo contract.Repository, repositoryNodeID string) (contract.Object, error) {
	canonical := executionKeys(raw, "repo", "issues", "provenance")
	if !canonical && !executionKeys(raw, "repo", "issues", "issue_count", "generated_at", "provenance") {
		return nil, errors.New("execution issue inventory has unsupported or missing fields")
	}
	rawRepo, err := contract.ObjectAt(raw, "repo")
	if err != nil {
		return nil, err
	}
	actualRepo, err := contract.ParseRepository(rawRepo)
	if err != nil || actualRepo != repo {
		return nil, errors.New("execution issue inventory targets another repository")
	}
	provenance, err := contract.ObjectAt(raw, "provenance")
	if err != nil {
		return nil, errors.New("execution issue inventory provenance is incomplete")
	}
	if canonical {
		if !executionKeys(provenance, "live", "complete", "issue_state", "repository_node_id") {
			return nil, errors.New("canonical execution issue inventory provenance is malformed")
		}
		if provenance["live"] != true || provenance["complete"] != true || provenance["issue_state"] != "all" || provenance["repository_node_id"] != repositoryNodeID {
			return nil, errors.New("canonical execution issue inventory has another completeness or repository identity")
		}
	} else if !executionKeys(provenance, "live", "complete", "issues_live", "issues_source", "issue_state", "repository_node_id", "relationships_requested") {
		return nil, errors.New("execution issue inventory provenance is incomplete")
	}
	if !canonical && (provenance["live"] != true || provenance["complete"] != true || provenance["issues_live"] != true || provenance["issues_source"] != "github_api" || provenance["issue_state"] != "all" || provenance["repository_node_id"] != repositoryNodeID || provenance["relationships_requested"] != false) {
		return nil, errors.New("execution requires complete all-state live issue metadata without implied relationship scope")
	}
	rows, err := contract.Objects(raw, "issues")
	if err != nil {
		return nil, err
	}
	if !canonical {
		count, err := contract.Integer(raw["issue_count"])
		if err != nil || count < 0 || count != int64(len(rows)) {
			return nil, errors.New("execution issue count does not match complete inventory")
		}
		if _, err := contract.Nonempty(raw, "generated_at"); err != nil {
			return nil, err
		}
	}
	seenNumbers, seenIDs := map[int64]bool{}, map[string]bool{}
	issues := make([]any, 0, len(rows))
	for _, row := range rows {
		if !canonical && !executionKeys(row, "id", "number", "title", "body", "state", "url", "labels", "milestone", "project_items") || canonical && !executionKeys(row, "id", "number", "title", "body", "state", "url", "labels", "milestone") {
			return nil, errors.New("execution issue row has unsupported or missing fields")
		}
		id, err := contract.Nonempty(row, "id")
		if err != nil || seenIDs[id] {
			return nil, errors.New("execution issue inventory has duplicate or invalid node identity")
		}
		number, err := contract.PositiveInteger(row["number"])
		if err != nil || seenNumbers[number] {
			return nil, errors.New("execution issue inventory has duplicate or invalid issue number")
		}
		if _, err := contract.Nonempty(row, "title"); err != nil {
			return nil, err
		}
		if _, err := contract.String(row, "body"); err != nil {
			return nil, err
		}
		if row["state"] != "OPEN" && row["state"] != "CLOSED" {
			return nil, errors.New("execution issue inventory has invalid state")
		}
		if err := (&native.Transport{Repository: repo}).ValidateIssueURL(row["url"], number); err != nil {
			return nil, err
		}
		labels, err := contract.Strings(row["labels"])
		if err != nil {
			return nil, err
		}
		milestone, exists := row["milestone"]
		if !exists || (milestone != nil && func() bool { _, ok := milestone.(string); return !ok }()) {
			return nil, errors.New("execution issue milestone must be explicit string or null")
		}
		if !canonical {
			if _, err := contract.Array(row, "project_items"); err != nil {
				return nil, err
			}
		}
		seenNumbers[number], seenIDs[id] = true, true
		issues = append(issues, contract.Object{"id": id, "number": number, "title": row["title"], "body": row["body"], "state": row["state"], "url": row["url"], "labels": stringSliceAny(labels), "milestone": milestone})
	}
	sort.Slice(issues, func(i, j int) bool {
		return issues[i].(map[string]any)["number"].(int64) < issues[j].(map[string]any)["number"].(int64)
	})
	return contract.Object{"repo": repo.Object(), "issues": issues, "provenance": contract.Object{"live": true, "complete": true, "issue_state": "all", "repository_node_id": repositoryNodeID}}, nil
}

func normalizeExecutionPullRequest(raw any, repo contract.Repository) (contract.Object, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	pr, ok := raw.(map[string]any)
	if !ok || !executionKeys(pr, "id", "number", "title", "url", "body", "state", "is_draft", "is_merged", "merged_at", "head_branch", "base_branch", "head_sha", "head_repository_owner", "head_repository", "closing_issue_numbers") {
		return nil, false, fmt.Errorf("execution pull request has unsupported or missing fields: %v", executionObjectKeys(pr))
	}
	id, err := contract.Nonempty(pr, "id")
	if err != nil {
		return nil, false, err
	}
	number, err := contract.PositiveInteger(pr["number"])
	if err != nil {
		return nil, false, err
	}
	link, err := contract.Nonempty(pr, "url")
	if err != nil || (&native.Transport{Repository: repo}).ValidatePullRequestURL(link, number) != nil {
		return nil, false, errors.New("execution pull request URL does not match repository identity")
	}
	for _, key := range []string{"title", "body", "state", "head_branch", "base_branch"} {
		if _, err := contract.Nonempty(pr, key); err != nil {
			return nil, false, err
		}
	}
	state := pr["state"]
	if state != "OPEN" && state != "CLOSED" && state != "MERGED" {
		return nil, false, errors.New("execution pull request state is invalid")
	}
	draft, err := contract.Bool(pr, "is_draft")
	if err != nil {
		return nil, false, err
	}
	merged, err := contract.Bool(pr, "is_merged")
	if err != nil || merged != (state == "MERGED") {
		return nil, false, errors.New("execution pull request merge state is inconsistent")
	}
	mergedAt, exists := pr["merged_at"]
	if !exists || (mergedAt != nil) != merged {
		return nil, false, errors.New("execution pull request merge timestamp is incomplete")
	}
	if mergedAt != nil {
		stamp, ok := mergedAt.(string)
		if !ok {
			return nil, false, errors.New("execution pull request merge timestamp is invalid")
		}
		if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
			return nil, false, errors.New("execution pull request merge timestamp is invalid")
		}
	}
	headSHA, err := contract.Nonempty(pr, "head_sha")
	if err != nil || !regexp.MustCompile(`^[a-fA-F0-9]{40}$`).MatchString(headSHA) {
		return nil, false, errors.New("execution pull request head revision is invalid")
	}
	ownerRaw, ownerExists := pr["head_repository_owner"]
	if !ownerExists || (ownerRaw != nil && func() bool { _, ok := ownerRaw.(string); return !ok }()) {
		return nil, false, errors.New("execution pull request head repository owner must be explicit string or null")
	}
	owner, _ := ownerRaw.(string)
	var headRepo any
	if pr["head_repository"] != nil {
		rawHead, ok := pr["head_repository"].(map[string]any)
		if !ok || !executionKeys(rawHead, "host", "owner", "name", "url", "nameWithOwner") {
			return nil, false, errors.New("execution pull request head repository is malformed")
		}
		headRepoParsed, err := contract.ParseRepository(rawHead)
		if err != nil {
			return nil, false, err
		}
		headRepo = headRepoParsed.Object()
		parsedOwner := headRepoParsed.Owner
		if owner != "" && !strings.EqualFold(owner, parsedOwner) {
			return nil, false, errors.New("pull request head repository and owner identity disagree")
		}
		owner = parsedOwner
	}
	closingRaw, ok := pr["closing_issue_numbers"].([]any)
	if !ok {
		return nil, false, errors.New("execution pull request closing references are incomplete")
	}
	closingSet := map[int64]bool{}
	closing := []any{}
	for _, rawNumber := range closingRaw {
		n, err := contract.PositiveInteger(rawNumber)
		if err != nil || closingSet[n] {
			return nil, false, errors.New("execution pull request closing references must be unique positive issues")
		}
		closingSet[n] = true
		closing = append(closing, n)
	}
	sort.Slice(closing, func(i, j int) bool { return closing[i].(int64) < closing[j].(int64) })
	return contract.Object{
		"id": id, "number": number, "title": pr["title"], "url": link, "body": pr["body"], "state": state,
		"is_draft": draft, "is_merged": merged, "merged_at": mergedAt,
		"head_branch": pr["head_branch"], "base_branch": pr["base_branch"], "head_sha": strings.ToLower(headSHA),
		"head_repository_owner": func() any {
			if owner == "" {
				return nil
			}
			return owner
		}(), "head_repository": headRepo, "closing_issue_numbers": closing,
	}, true, nil
}

func normalizeExecutionLinkedPR(raw any, repo contract.Repository, issueNumber int64, policy ExecutionPolicy) (contract.Object, int64, error) {
	linkedRaw, ok := raw.(map[string]any)
	if !ok {
		return nil, 0, errors.New("execution linked-PR inventory has unsupported or missing fields")
	}
	canonical := executionKeys(linkedRaw, "repo", "issue_number", "comments", "latest_comment_id", "latest_pull_request_number", "latest_pull_request_id", "provenance")
	if canonical {
		declaredRepo, err := contract.ObjectAt(linkedRaw, "repo")
		if err != nil {
			return nil, 0, err
		}
		actual, err := contract.ParseRepository(declaredRepo)
		if err != nil || actual != repo {
			return nil, 0, errors.New("canonical execution linked-PR inventory targets another repository")
		}
		declaredIssue, err := contract.PositiveInteger(linkedRaw["issue_number"])
		if err != nil || declaredIssue != issueNumber {
			return nil, 0, errors.New("canonical execution linked-PR inventory targets another issue")
		}
		prov, err := contract.ObjectAt(linkedRaw, "provenance")
		if err != nil || !executionKeys(prov, "live", "complete", "source", "pull_requests_source") || prov["live"] != true || prov["complete"] != true || prov["source"] != "github_api" {
			return nil, 0, errors.New("canonical execution linked-PR provenance is malformed")
		}
		byIssue := contract.Object{strconv.FormatInt(issueNumber, 10): nil}
		pullRequests := []any{}
		latestNumber, latestID := linkedRaw["latest_pull_request_number"], linkedRaw["latest_pull_request_id"]
		if latestNumber != nil || latestID != nil {
			if latestNumber == nil || latestID == nil {
				return nil, 0, errors.New("canonical execution linked-PR identity is incomplete")
			}
			n, err := contract.PositiveInteger(latestNumber)
			if err != nil {
				return nil, 0, err
			}
			id, err := contract.Nonempty(contract.Object{"id": latestID}, "id")
			if err != nil {
				return nil, 0, err
			}
			pullRequest := contract.Object{"number": n, "id": id}
			byIssue[strconv.FormatInt(issueNumber, 10)] = pullRequest
			pullRequests = append(pullRequests, pullRequest)
		}
		linkedRaw = contract.Object{
			"repo": repo.Object(), "issue_numbers": []any{issueNumber}, "by_issue": byIssue,
			"comments": linkedRaw["comments"], "pull_requests": pullRequests, "unresolved_links": []any{},
			"provenance": contract.Object{"live": true, "complete": true, "comments_source": "github_api", "pull_requests_source": prov["pull_requests_source"]},
		}
	} else if !executionKeys(linkedRaw, "repo", "issue_numbers", "by_issue", "comments", "pull_requests", "unresolved_links", "generated_at", "provenance") {
		return nil, 0, errors.New("execution linked-PR inventory has unsupported or missing fields")
	}
	var declaredCanonical contract.Object
	if canonical {
		declaredCanonical, _ = raw.(map[string]any)
	}
	rawRepo, _ := contract.ObjectAt(linkedRaw, "repo")
	actualRepo, err := contract.ParseRepository(rawRepo)
	if err != nil || actualRepo != repo {
		return nil, 0, errors.New("execution linked-PR inventory targets another repository")
	}
	prov, err := contract.ObjectAt(linkedRaw, "provenance")
	if err != nil || !executionKeys(prov, "live", "complete", "comments_source", "pull_requests_source") || prov["live"] != true || prov["complete"] != true || prov["comments_source"] != "github_api" {
		return nil, 0, errors.New("execution linked-PR inventory is not complete live comment evidence")
	}
	if _, err := contract.Nonempty(linkedRaw, "generated_at"); err != nil && !canonical {
		return nil, 0, err
	}
	issues, err := contract.Array(linkedRaw, "issue_numbers")
	if err != nil || len(issues) != 1 {
		return nil, 0, errors.New("execution linked-PR inventory must cover exactly one selected issue")
	}
	listedIssue, err := contract.PositiveInteger(issues[0])
	if err != nil || listedIssue != issueNumber {
		return nil, 0, errors.New("execution linked-PR inventory issue scope changed")
	}
	commentsRaw, err := contract.Objects(linkedRaw, "comments")
	if err != nil {
		return nil, 0, err
	}
	comments := make([]any, 0, len(commentsRaw))
	commentIDs := map[int64]bool{}
	for _, comment := range commentsRaw {
		if !executionKeys(comment, "id", "issue_number", "body", "url") {
			return nil, 0, errors.New("execution comment row has unsupported or missing fields")
		}
		id, e := contract.PositiveInteger(comment["id"])
		if e != nil || commentIDs[id] {
			return nil, 0, errors.New("execution comment inventory has duplicate or invalid identity")
		}
		n, e := contract.PositiveInteger(comment["issue_number"])
		if e != nil || n != issueNumber {
			return nil, 0, errors.New("execution comment inventory contains a foreign issue")
		}
		body, e := contract.String(comment, "body")
		if e != nil {
			return nil, 0, e
		}
		link, e := contract.Nonempty(comment, "url")
		if e != nil || !validCommentURL(link, repo, issueNumber, id) {
			return nil, 0, errors.New("execution comment URL has foreign identity")
		}
		commentIDs[id] = true
		comments = append(comments, contract.Object{"id": id, "issue_number": n, "body": body, "url": link})
	}
	sort.Slice(comments, func(i, j int) bool {
		return comments[i].(map[string]any)["id"].(int64) < comments[j].(map[string]any)["id"].(int64)
	})
	pattern, _ := regexp.Compile(policy.PRLinkNumberPattern)
	var latest contract.Object
	var latestNumber int64
	for _, comment := range comments {
		row := comment.(map[string]any)
		body := row["body"].(string)
		if !strings.Contains(body, policy.PRLinkMarkerPrefix) {
			continue
		}
		matches := pattern.FindAllStringSubmatch(body, -1)
		if len(matches) == 0 {
			return nil, 0, errors.New("execution PR link marker has no qualified PR number")
		}
		var current int64
		for _, match := range matches {
			n, e := strconv.ParseInt(match[pattern.SubexpIndex("number")], 10, 64)
			if e != nil || n < 1 || (current != 0 && current != n) {
				return nil, 0, errors.New("execution PR link marker has ambiguous or invalid number")
			}
			current = n
		}
		if latest == nil || row["id"].(int64) > latest["id"].(int64) {
			latest, latestNumber = row, current
		}
	}
	expectedPRSource := "not_requested"
	if latest != nil {
		expectedPRSource = "github_api"
	}
	if prov["pull_requests_source"] != expectedPRSource {
		return nil, 0, errors.New("execution linked-PR source provenance does not match marker resolution")
	}
	byIssue, err := contract.ObjectAt(linkedRaw, "by_issue")
	if err != nil || len(byIssue) != 1 {
		return nil, 0, errors.New("execution linked-PR resolution must cover the selected issue only")
	}
	key := strconv.FormatInt(issueNumber, 10)
	resolved := byIssue[key]
	unresolved, err := contract.Objects(linkedRaw, "unresolved_links")
	if err != nil || len(unresolved) > 0 {
		return nil, 0, errors.New("latest execution PR link is unresolved in the complete pull-request inventory")
	}
	var resolvedNumber any
	var resolvedID any
	if latest == nil {
		if resolved != nil {
			return nil, 0, errors.New("linked-PR resolver found a pull request without a source marker")
		}
	} else {
		resolvedPR, ok := resolved.(map[string]any)
		if !ok {
			return nil, 0, errors.New("latest linked-PR marker has no resolved pull request")
		}
		n, e := contract.PositiveInteger(resolvedPR["number"])
		if e != nil || n != latestNumber {
			return nil, 0, errors.New("latest linked-PR marker and resolved pull request differ")
		}
		id, e := contract.Nonempty(resolvedPR, "id")
		if e != nil {
			return nil, 0, e
		}
		resolvedNumber, resolvedID = n, id
	}
	prs, err := contract.Objects(linkedRaw, "pull_requests")
	if err != nil {
		return nil, 0, err
	}
	if latest == nil && len(prs) != 0 || latest != nil && len(prs) != 1 {
		return nil, 0, errors.New("linked-PR inventory does not exactly cover the latest marker target")
	}
	if latest != nil {
		n, e := contract.PositiveInteger(prs[0]["number"])
		if e != nil || n != latestNumber || prs[0]["id"] != resolvedID {
			return nil, 0, errors.New("linked-PR page and by-issue selection disagree")
		}
	}
	latestCommentID := func() any {
		if latest == nil {
			return nil
		}
		return latest["id"]
	}()
	if declaredCanonical != nil && (!same(declaredCanonical["latest_comment_id"], latestCommentID) || !same(declaredCanonical["latest_pull_request_number"], resolvedNumber) || !same(declaredCanonical["latest_pull_request_id"], resolvedID)) {
		return nil, 0, errors.New("canonical execution linked-PR summary disagrees with its complete comment source")
	}
	return contract.Object{"repo": repo.Object(), "issue_number": issueNumber, "comments": comments, "latest_comment_id": latestCommentID, "latest_pull_request_number": resolvedNumber, "latest_pull_request_id": resolvedID, "provenance": contract.Object{"live": true, "complete": true, "source": "github_api", "pull_requests_source": expectedPRSource}}, latestNumber, nil
}

func normalizeExecutionBranches(raw any, source any, repo contract.Repository, repositoryNodeID string, pr contract.Object, hasPR bool) (any, *bool, error) {
	required := hasPR && !pr["is_merged"].(bool) && pr["state"] == "CLOSED"
	if !required {
		if raw != nil || source != "not_requested" {
			return nil, nil, errors.New("branch inventory was supplied outside the closed-unmerged transition")
		}
		return nil, nil, nil
	}
	if source != "github_api" {
		return nil, nil, errors.New("closed-unmerged execution sync requires complete live branch evidence")
	}
	if pr["head_repository"] == nil {
		return nil, nil, errors.New("closed-unmerged PR has no live head repository; branch absence cannot be established")
	}
	headRepo, _ := contract.ObjectAt(pr, "head_repository")
	fullName, err := contract.Nonempty(headRepo, "nameWithOwner")
	if err != nil || !strings.EqualFold(fullName, repo.FullName()) {
		return nil, nil, errors.New("closed-unmerged fork head is outside the complete repository branch inventory")
	}
	branchRaw, ok := raw.(map[string]any)
	if !ok || !executionKeys(branchRaw, "repo", "branches", "provenance") {
		return nil, nil, errors.New("execution branch inventory has unsupported or missing fields")
	}
	r, err := contract.ObjectAt(branchRaw, "repo")
	if err != nil {
		return nil, nil, err
	}
	actual, err := contract.ParseRepository(r)
	if err != nil || actual != repo {
		return nil, nil, errors.New("execution branch inventory targets another repository")
	}
	prov, err := contract.ObjectAt(branchRaw, "provenance")
	if err != nil || !executionKeys(prov, "live", "complete", "source", "repository_node_id") || prov["live"] != true || prov["complete"] != true || prov["source"] != "github_api" || prov["repository_node_id"] != repositoryNodeID {
		return nil, nil, errors.New("execution branch inventory is not complete live repository evidence")
	}
	rows, err := contract.Objects(branchRaw, "branches")
	if err != nil {
		return nil, nil, err
	}
	branches := make([]any, 0, len(rows))
	seen := map[string]bool{}
	headBranch, _ := contract.Nonempty(pr, "head_branch")
	exists := false
	for _, row := range rows {
		if !executionKeys(row, "name", "sha") {
			return nil, nil, errors.New("branch inventory row has unsupported or missing fields")
		}
		name, e := contract.Nonempty(row, "name")
		if e != nil || seen[name] {
			return nil, nil, errors.New("branch inventory contains duplicate or invalid name")
		}
		sha, e := contract.Nonempty(row, "sha")
		if e != nil || !regexp.MustCompile(`^[a-fA-F0-9]{40}$`).MatchString(sha) {
			return nil, nil, errors.New("branch inventory contains invalid head SHA")
		}
		seen[name] = true
		if name == headBranch {
			exists = true
		}
		branches = append(branches, contract.Object{"name": name, "sha": strings.ToLower(sha)})
	}
	sort.Slice(branches, func(i, j int) bool {
		return branches[i].(map[string]any)["name"].(string) < branches[j].(map[string]any)["name"].(string)
	})
	return contract.Object{"repo": repo.Object(), "branches": branches, "provenance": contract.Object{"live": true, "complete": true, "source": "github_api", "repository_node_id": repositoryNodeID}}, &exists, nil
}

func normalizeExecutionProject(raw any, repo contract.Repository, scope ProjectScope, policy ExecutionPolicy, transition contract.Object, graph contract.Object) (contract.Object, error) {
	projectRaw, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("execution Project snapshot is required by reviewed selector")
	}
	request := BacklogInventoryRequest{Projects: []ProjectScope{scope}}
	wrapper := contract.Object{"repo": repo.Object(), "projects": []any{projectRaw}, "provenance": contract.Object{"project_scopes": projectScopesAny(request.Projects)}}
	projectRows, err := normalizeBacklogProjects(wrapper, repo, request, wrapper["provenance"].(map[string]any), graph)
	if err != nil {
		return nil, err
	}
	project := projectRows[0].(map[string]any)
	status, _ := transition["final_status"].(string)
	if status != "" {
		if err := validateProjectField(project, policy.StatusField, ProjectFieldValue{Text: &status}); err != nil {
			return nil, err
		}
	}
	return contract.Object{"repo": repo.Object(), "project": project["project"], "fields_by_name": project["fields_by_name"], "items": project["items"], "provenance": contract.Object{"live": true, "complete": true, "source": "github_project_api"}}, nil
}

func executionTransition(policy ExecutionPolicy, issue contract.Object, pr contract.Object, hasPR bool, branchExists *bool, defaultBranch string) (contract.Object, error) {
	var rawPR any
	if hasPR {
		rawPR = pr
	}
	snapshot := contract.Object{"issue": issue, "pull_request": rawPR, "default_branch": defaultBranch}
	if branchExists != nil {
		snapshot["branch_exists"] = *branchExists
	}
	return governance.ExecutionTransition(contract.Object{"statuses": contract.Object{"done": policy.Statuses.Done, "active": policy.Statuses.Active, "todo": policy.Statuses.Todo}}, snapshot)
}

func executionLinkFacts(policy ExecutionPolicy, issueNumber int64, defaultBranch string, pr contract.Object) (contract.Object, error) {
	return governance.ExecutionLinkFacts(contract.Object{
		"linked_issue_marker_prefix": policy.LinkedIssueMarkerPrefix,
		"link_state_marker_prefix":   policy.LinkStateMarkerPrefix,
	}, contract.Object{"issue_number": issueNumber, "default_branch": defaultBranch, "pull_request": pr})
}

func executionPlanSources(inventory contract.Object) contract.Object {
	prov := inventory["provenance"].(map[string]any)
	return contract.Object{
		"issues":       contract.Object{"source": "github_api", "live": true, "complete": true, "state": "all"},
		"pull_request": contract.Object{"source": prov["pull_request_source"], "live": true, "complete": true},
		"linked_prs":   contract.Object{"source": "github_api", "live": true, "complete": true},
		"branches":     contract.Object{"source": prov["branch_source"], "live": true, "complete": true},
		"project":      contract.Object{"source": prov["project_source"], "live": true, "complete": true},
	}
}

type ExecutionSync struct{ Provider ExecutionProvider }

func (e ExecutionSync) Operations(plan contract.Plan) ([]contract.Operation, error) {
	return executionOperations(plan)
}

func executionOperations(plan contract.Plan) ([]contract.Operation, error) {
	if plan.Command != ExecutionSyncCommand || !executionKeys(plan.Data, "selector", "policy", "inventory") {
		return nil, errors.New("execution adapter requires the canonical sync command and data shape")
	}
	selectorRaw, err := contract.ObjectAt(plan.Data, "selector")
	if err != nil {
		return nil, err
	}
	selector, err := parseExecutionSelector(selectorRaw, plan.Repository)
	if err != nil {
		return nil, err
	}
	policyRaw, err := contract.ObjectAt(plan.Data, "policy")
	if err != nil {
		return nil, err
	}
	policy, err := parseExecutionPolicy(policyRaw)
	if err != nil {
		return nil, err
	}
	inventoryRaw, err := contract.ObjectAt(plan.Data, "inventory")
	if err != nil {
		return nil, err
	}
	inventory, err := normalizeExecutionInventory(inventoryRaw, plan.Repository, ExecutionInventoryRequest{Selector: selector, Policy: policy})
	if err != nil {
		return nil, err
	}
	if !same(plan.Sources, executionPlanSources(inventory)) {
		return nil, errors.New("execution source declarations differ from captured live inventory")
	}
	issue, _ := contract.ObjectAt(inventory, "issue")
	pr, hasPR := inventory["pull_request"].(map[string]any)
	if !hasPR {
		return []contract.Operation{}, nil
	}
	issueNumber, _ := contract.PositiveInteger(issue["number"])
	facts, err := executionLinkFacts(policy, issueNumber, inventory["default_branch"].(string), pr)
	if err != nil || facts["parsed_issue_number"] != issueNumber {
		return nil, errors.New("selected pull request body does not identify the reviewed issue")
	}
	transition, err := executionTransition(policy, issue, pr, true, func() *bool {
		if value, ok := inventory["branch_inventory"].(map[string]any); ok {
			branches, _ := contract.Objects(value, "branches")
			for _, row := range branches {
				if row["name"] == pr["head_branch"] {
					present := true
					return &present
				}
			}
			present := false
			return &present
		}
		return nil
	}(), inventory["default_branch"].(string))
	if err != nil {
		return nil, err
	}
	ops := []contract.Operation{}
	if selector.PullRequestNumber > 0 {
		if op, needed, err := executionLinkCommentOperation(inventory, issue, pr, issueNumber, policy, plan.CapturedAt); err != nil {
			return nil, err
		} else if needed {
			ops = append(ops, op)
		}
	}
	actionList, err := contract.Array(transition, "actions")
	if err != nil {
		return nil, err
	}
	for _, actionRaw := range actionList {
		action, ok := actionRaw.(string)
		if !ok {
			return nil, errors.New("execution transition returned an invalid action")
		}
		state, _ := contract.String(issue, "state")
		var after string
		switch action {
		case "reopen-issue":
			after = "OPEN"
		case "close-issue":
			after = "CLOSED"
		default:
			return nil, fmt.Errorf("unsupported execution transition action %q", action)
		}
		if state != after {
			kind := "issue-reopen"
			if after == "CLOSED" {
				kind = "issue-close"
			}
			ops = append(ops, contract.Operation{
				ID: "execution:issue-state", Kind: kind,
				Target: contract.Object{"issue_number": issueNumber, "issue_id": issue["id"]},
				Before: contract.Object{"issue_number": issueNumber, "issue_id": issue["id"], "state": state},
				After:  contract.Object{"issue_number": issueNumber, "issue_id": issue["id"], "state": after},
			})
		}
	}
	if selector.SkipProjectSync || transition["final_status"] == nil {
		return ops, nil
	}
	project, err := contract.ObjectAt(inventory, "project_inventory")
	if err != nil {
		return nil, err
	}
	projectMeta, _ := contract.ObjectAt(project, "project")
	projectID, _ := contract.Nonempty(projectMeta, "id")
	items, _ := contract.Objects(project, "items")
	var item contract.Object
	for _, candidate := range items {
		if n, e := contract.PositiveInteger(candidate["number"]); e == nil && n == issueNumber {
			if candidate["archived"] == true {
				return nil, errors.New("execution Project membership is archived and cannot be reused")
			}
			item = candidate
		}
	}
	itemRef := contract.Object{}
	if item == nil {
		membershipID := "execution:project-membership"
		ops = append(ops, contract.Operation{
			ID: membershipID, Kind: "project-membership-add",
			Target: contract.Object{"project_id": projectID, "issue_number": issueNumber, "issue_node_id": issue["id"]},
			Before: contract.Object{"present": false, "project_id": projectID, "issue_number": issueNumber, "issue_node_id": issue["id"]},
			After:  contract.Object{"present": true, "project_id": projectID, "issue_number": issueNumber, "issue_node_id": issue["id"]},
		})
		itemRef = contract.Object{"from_operation": membershipID}
	} else {
		itemRef = contract.Object{"item_id": item["item_id"]}
	}
	fields, _ := contract.ObjectAt(item, "field_values")
	desiredStatus, _ := transition["final_status"].(string)
	if fields[policy.StatusField] != desiredStatus {
		before := any(fields[policy.StatusField])
		if item == nil {
			before = contract.Object{"from_membership": "execution:project-membership", "field_name": policy.StatusField}
		}
		ops = append(ops, contract.Operation{
			ID: "execution:project-status", Kind: "project-field-set",
			Target: contract.Object{"project_id": projectID, "issue_number": issueNumber, "issue_id": issue["id"], "item_ref": itemRef, "field_name": policy.StatusField},
			Before: contract.Object{"present": true, "item_ref": itemRef, "field_name": policy.StatusField, "value": before},
			After:  contract.Object{"present": true, "item_ref": itemRef, "field_name": policy.StatusField, "value": desiredStatus},
		})
	}
	return ops, nil
}

func executionLinkCommentOperation(inventory, issue, pr contract.Object, issueNumber int64, policy ExecutionPolicy, capturedAt string) (contract.Operation, bool, error) {
	linked, err := contract.ObjectAt(inventory, "linked_pr_inventory")
	if err != nil {
		return contract.Operation{}, false, err
	}
	comments, err := contract.Objects(linked, "comments")
	if err != nil {
		return contract.Operation{}, false, err
	}
	marker := fmt.Sprintf("%s issue=%d -->", policy.PRLinkMarkerPrefix, issueNumber)
	var latest contract.Object
	for _, comment := range comments {
		if strings.Contains(comment["body"].(string), marker) && (latest == nil || comment["id"].(int64) > latest["id"].(int64)) {
			latest = comment
		}
	}
	prNumber, _ := contract.PositiveInteger(pr["number"])
	if latest != nil {
		body := latest["body"].(string)
		pattern, _ := regexp.Compile(policy.PRLinkNumberPattern)
		matches := pattern.FindAllStringSubmatch(body, -1)
		if len(matches) > 0 {
			linkedNumber, err := strconv.ParseInt(matches[len(matches)-1][pattern.SubexpIndex("number")], 10, 64)
			if err == nil && linkedNumber == prNumber {
				return contract.Operation{}, false, nil
			}
		}
	}
	url, _ := contract.String(pr, "url")
	head, _ := contract.String(pr, "head_branch")
	base, _ := contract.String(pr, "base_branch")
	body := fmt.Sprintf("Linked PR: %s\nPR: #%d\nHead: %s\nBase: %s\nLinked at: %s\n%s", url, prNumber, head, base, capturedAt, marker)
	prID, _ := contract.Nonempty(pr, "id")
	target := contract.Object{"issue_number": issueNumber, "issue_id": issue["id"], "marker": marker, "body": body, "pull_request_number": prNumber, "pull_request_id": prID}
	before := contract.Object{
		"issue_number": issueNumber, "issue_id": issue["id"], "marker": marker,
		"comment_id": nil, "body": nil,
		"pull_request_number": linked["latest_pull_request_number"],
		"pull_request_id":     linked["latest_pull_request_id"],
	}
	if latest != nil {
		target["comment_id"] = latest["id"]
		before["comment_id"], before["body"] = latest["id"], latest["body"]
	}
	return contract.Operation{
		ID: "execution:link-comment", Kind: "issue-comment-upsert", Target: target, Before: before,
		After: contract.Object{"issue_number": issueNumber, "issue_id": issue["id"], "marker": marker, "comment_id": target["comment_id"], "body": body, "pull_request_number": prNumber, "pull_request_id": prID},
	}, true, nil
}

func (e ExecutionSync) ValidateReceipt(plan contract.Plan, op contract.Operation, receipt contract.Object) error {
	if err := e.ValidateAcknowledgement(plan, op, receipt); err != nil {
		return err
	}
	if receipt["after_verified"] != true {
		return errors.New("execution receipt lacks complete verified after-state")
	}
	return nil
}

func (e ExecutionSync) ValidateAcknowledgement(plan contract.Plan, op contract.Operation, ack contract.Object) error {
	ops, err := executionOperations(plan)
	if err != nil {
		return err
	}
	if !containsOperation(ops, op) {
		return errors.New("execution acknowledgement primitive is outside derived plan")
	}
	allowed := map[string]bool{"kind": true, "primitive_id": true, "repository": true, "operation_id": true, "target": true, "before": true, "after": true, "acknowledged": true, "provider_result": true, "after_verified": true}
	for key := range ack {
		if !allowed[key] {
			return fmt.Errorf("execution acknowledgement has unsupported field %q", key)
		}
	}
	if verified, ok := ack["after_verified"]; ok && verified != true {
		return errors.New("execution acknowledgement has invalid after-state verification")
	}
	if ack["kind"] != op.Kind || ack["primitive_id"] != op.ID || ack["acknowledged"] != true || !same(ack["target"], op.Target) || !same(ack["before"], op.Before) || !same(ack["after"], op.After) {
		return errors.New("execution acknowledgement differs from reviewed primitive")
	}
	repository, err := contract.ObjectAt(ack, "repository")
	if err != nil {
		return err
	}
	repo, err := contract.ParseRepository(repository)
	if err != nil || repo != plan.Repository {
		return errors.New("execution acknowledgement belongs to another repository")
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
	return validateExecutionProviderResult(plan, op, nonce, result, nil)
}

func (e ExecutionSync) Preflight(ctx context.Context, plan contract.Plan, receipts []contract.Object) error {
	if e.Provider == nil {
		return errors.New("execution adapter requires a provider")
	}
	request, expected, err := executionExpected(plan, receipts)
	if err != nil {
		return err
	}
	raw, err := e.Provider.ExecutionInventory(ctx, request)
	if err != nil {
		return err
	}
	actual, err := normalizeExecutionInventory(raw, plan.Repository, request)
	if err != nil {
		return err
	}
	if !same(expected, actual) {
		return errors.New("complete live execution inventory drifted from reviewed state")
	}
	return nil
}

func (e ExecutionSync) Dispatch(ctx context.Context, plan contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, error) {
	return nil, errors.New("execution writes require durable native acknowledgement persistence")
}

func (e ExecutionSync) DispatchAcknowledged(ctx context.Context, plan contract.Plan, op contract.Operation, nonce string, receipts []contract.Object, persist func(contract.Object) error) (contract.Object, error) {
	if e.Provider == nil {
		return nil, errors.New("execution adapter requires a provider")
	}
	if _, err := native.OperationMarker(nonce); err != nil {
		return nil, err
	}
	request, prior, err := executionExpected(plan, receipts)
	if err != nil {
		return nil, err
	}
	ops, err := executionOperations(plan)
	if err != nil || !containsOperation(ops, op) {
		return nil, errors.New("execution dispatch primitive is outside the derived plan")
	}
	providerResult, err := e.dispatchExecution(ctx, plan, op, nonce, receipts)
	if err != nil {
		return nil, err
	}
	ack := contract.Object{"kind": op.Kind, "primitive_id": op.ID, "repository": plan.Repository.Object(), "operation_id": nonce, "target": op.Target, "before": op.Before, "after": op.After, "acknowledged": true, "provider_result": providerResult}
	if err = e.ValidateAcknowledgement(plan, op, ack); err != nil {
		return nil, err
	}
	if err = validateExecutionProviderResult(plan, op, nonce, providerResult, receipts); err != nil {
		return nil, err
	}
	if persist == nil {
		return nil, errors.New("execution acknowledgement persistence callback is required")
	}
	if err = persist(ack); err != nil {
		return nil, err
	}
	projected, err := projectExecutionAcknowledgement(prior, plan, op, ack, receipts)
	if err != nil {
		return nil, fmt.Errorf("captured execution acknowledgement could not be projected: %w", err)
	}
	raw, err := e.Provider.ExecutionInventory(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("execution write was acknowledged but after-state read failed: %w", err)
	}
	actual, err := normalizeExecutionInventory(raw, plan.Repository, request)
	if err != nil {
		return nil, fmt.Errorf("execution write was acknowledged but after-state is invalid: %w", err)
	}
	if !same(projected, actual) {
		return nil, errors.New("execution write was acknowledged but complete after-state differs from primitive")
	}
	ack["after_verified"] = true
	return ack, nil
}

func (e ExecutionSync) Observe(ctx context.Context, plan contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, contract.Object, error) {
	if e.Provider == nil {
		return nil, nil, errors.New("execution adapter requires a provider")
	}
	var ack contract.Object
	for _, receipt := range receipts {
		if receipt["id"] == op.ID && receipt["status"] == "unknown" && receipt["operation_id"] == nonce {
			ack, _ = receipt["acknowledgement"].(map[string]any)
		}
	}
	if ack == nil || ack["operation_id"] != nonce {
		return nil, nil, errors.New("ambiguous execution write has no captured native acknowledgement; no write was retried")
	}
	if err := e.ValidateAcknowledgement(plan, op, ack); err != nil {
		return nil, nil, err
	}
	request, prior, err := executionExpected(plan, receipts)
	if err != nil {
		return nil, nil, err
	}
	projected, err := projectExecutionAcknowledgement(prior, plan, op, ack, executionCompletedPrefix(receipts, op.ID))
	if err != nil {
		return nil, nil, err
	}
	raw, err := e.Provider.ExecutionInventory(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	actual, err := normalizeExecutionInventory(raw, plan.Repository, request)
	if err != nil {
		return nil, nil, err
	}
	if !same(projected, actual) {
		return nil, nil, errors.New("captured execution acknowledgement differs from complete live after-state")
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

func (e ExecutionSync) dispatchExecution(ctx context.Context, plan contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, error) {
	selectorRaw, _ := contract.ObjectAt(plan.Data, "selector")
	selector, err := parseExecutionSelector(selectorRaw, plan.Repository)
	if err != nil {
		return nil, err
	}
	switch op.Kind {
	case "issue-reopen", "issue-close":
		n, err := contract.PositiveInteger(op.Target["issue_number"])
		if err != nil {
			return nil, err
		}
		if op.Kind == "issue-reopen" {
			return e.Provider.ReopenIssue(ctx, nonce, n)
		}
		return e.Provider.CloseIssue(ctx, nonce, n)
	case "issue-comment-upsert":
		n, err := contract.PositiveInteger(op.Target["issue_number"])
		if err != nil {
			return nil, err
		}
		body, err := contract.String(op.Target, "body")
		if err != nil {
			return nil, err
		}
		if commentID, err := contract.PositiveInteger(op.Target["comment_id"]); err == nil {
			return e.Provider.UpdateIssueComment(ctx, nonce, n, commentID, body)
		}
		return e.Provider.CreateIssueComment(ctx, nonce, n, body)
	case "project-membership-add":
		projectID, err := contract.Nonempty(op.Target, "project_id")
		if err != nil {
			return nil, err
		}
		node, err := contract.Nonempty(op.Target, "issue_node_id")
		if err != nil {
			return nil, err
		}
		if selector.SkipProjectSync || selector.Project == nil || selector.Project.ID != projectID {
			return nil, errors.New("Project membership is outside the reviewed execution scope")
		}
		return e.Provider.AddProjectIssue(ctx, nonce, projectID, node)
	case "project-field-set":
		projectID, err := contract.Nonempty(op.Target, "project_id")
		if err != nil {
			return nil, err
		}
		field, err := contract.Nonempty(op.Target, "field_name")
		if err != nil {
			return nil, err
		}
		itemID, err := executionResolveItemID(op.Target, plan, receipts)
		if err != nil {
			return nil, err
		}
		status, err := contract.String(op.After, "value")
		if err != nil {
			return nil, err
		}
		return e.Provider.SetProjectField(ctx, nonce, projectID, itemID, ProjectField(field), ProjectFieldValue{Text: &status})
	default:
		return nil, fmt.Errorf("unsupported execution primitive %q", op.Kind)
	}
}

func validateExecutionProviderResult(plan contract.Plan, op contract.Operation, nonce string, result contract.Object, receipts []contract.Object) error {
	if result == nil {
		return errors.New("native execution mutation returned no acknowledgement object")
	}
	switch op.Kind {
	case "issue-reopen", "issue-close":
		issue, err := backlogCreatedIssue(result, plan.Repository)
		if err != nil {
			return err
		}
		number, numberErr := contract.PositiveInteger(issue["number"])
		targetNumber, targetErr := contract.PositiveInteger(op.Target["issue_number"])
		if numberErr != nil || targetErr != nil || number != targetNumber || issue["id"] != op.Target["issue_id"] || issue["state"] != op.After["state"] {
			return errors.New("issue state acknowledgement differs from reviewed identity and target state")
		}
		inventory, _ := contract.ObjectAt(plan.Data, "inventory")
		prior, _ := contract.ObjectAt(inventory, "issue")
		for _, key := range []string{"title", "body", "url", "labels", "milestone"} {
			if !same(issue[key], prior[key]) {
				return fmt.Errorf("issue state acknowledgement changed unrelated %s", key)
			}
		}
	case "issue-comment-upsert":
		marker, err := native.OperationMarker(nonce)
		if err != nil {
			return err
		}
		body, err := contract.String(op.Target, "body")
		if err != nil {
			return err
		}
		wantID, _ := contract.PositiveInteger(op.Target["comment_id"])
		if wantID == 0 {
			body += "\n\n" + marker
		}
		id, err := contract.PositiveInteger(result["id"])
		if err != nil || wantID > 0 && id != wantID {
			return errors.New("execution link comment acknowledgement changed identity")
		}
		gotBody, err := contract.String(result, "body")
		if err != nil || gotBody != body {
			return errors.New("execution link comment acknowledgement differs from reviewed body")
		}
		issueNumber, _ := contract.PositiveInteger(op.Target["issue_number"])
		link, err := contract.Nonempty(result, "html_url")
		if err != nil || !validCommentURL(link, plan.Repository, issueNumber, id) {
			return errors.New("execution link comment acknowledgement has foreign identity")
		}
	case "project-membership-add":
		if result["clientMutationId"] != nonce {
			return errors.New("execution Project membership acknowledgement lacks its native mutation identity")
		}
		itemID, _, number, node, err := backlogProjectItemAck(result, plan.Repository)
		if err != nil {
			return err
		}
		targetNumber, targetErr := contract.PositiveInteger(op.Target["issue_number"])
		if itemID == "" || targetErr != nil || number != targetNumber || node != op.Target["issue_node_id"] {
			return errors.New("execution Project membership acknowledgement targets another issue")
		}
	case "project-field-set":
		if result["clientMutationId"] != nonce {
			return errors.New("execution Project field acknowledgement lacks its native mutation identity")
		}
		item, err := contract.ObjectAt(result, "projectV2Item")
		if err != nil {
			return err
		}
		itemID, err := contract.Nonempty(item, "id")
		if err != nil {
			return err
		}
		ref, err := contract.ObjectAt(op.Target, "item_ref")
		if err != nil {
			return err
		}
		wantItem, itemErr := contract.Nonempty(ref, "item_id")
		if itemErr == nil && itemID != wantItem {
			return errors.New("execution Project field acknowledgement targets another item")
		}
		if itemErr != nil {
			if _, err := contract.Nonempty(ref, "from_operation"); err != nil {
				return errors.New("execution Project field acknowledgement has no qualified item identity")
			}
			if len(receipts) > 0 {
				wantItem, err = executionResolveItemID(op.Target, plan, receipts)
				if err != nil || itemID != wantItem {
					return errors.New("execution Project field acknowledgement targets another item")
				}
			}
		}
	default:
		return fmt.Errorf("unsupported execution acknowledgement kind %q", op.Kind)
	}
	return nil
}

func executionExpected(plan contract.Plan, receipts []contract.Object) (ExecutionInventoryRequest, contract.Object, error) {
	selectorRaw, err := contract.ObjectAt(plan.Data, "selector")
	if err != nil {
		return ExecutionInventoryRequest{}, nil, err
	}
	selector, err := parseExecutionSelector(selectorRaw, plan.Repository)
	if err != nil {
		return ExecutionInventoryRequest{}, nil, err
	}
	policyRaw, err := contract.ObjectAt(plan.Data, "policy")
	if err != nil {
		return ExecutionInventoryRequest{}, nil, err
	}
	policy, err := parseExecutionPolicy(policyRaw)
	if err != nil {
		return ExecutionInventoryRequest{}, nil, err
	}
	request := ExecutionInventoryRequest{Selector: selector, Policy: policy}
	inventoryRaw, err := contract.ObjectAt(plan.Data, "inventory")
	if err != nil {
		return request, nil, err
	}
	expected, err := normalizeExecutionInventory(inventoryRaw, plan.Repository, request)
	if err != nil {
		return request, nil, err
	}
	ops, err := executionOperations(plan)
	if err != nil {
		return request, nil, err
	}
	byID := map[string]contract.Operation{}
	for _, op := range ops {
		byID[op.ID] = op
	}
	prefix := []contract.Object{}
	for _, receipt := range receipts {
		if receipt["status"] != "completed" {
			continue
		}
		id, err := contract.Nonempty(receipt, "id")
		if err != nil {
			return request, nil, err
		}
		op, ok := byID[id]
		if !ok {
			return request, nil, errors.New("execution journal contains a primitive outside derived plan")
		}
		result, err := contract.ObjectAt(receipt, "result")
		if err != nil {
			return request, nil, err
		}
		if err = (ExecutionSync{}).ValidateReceipt(plan, op, result); err != nil {
			return request, nil, err
		}
		if receipt["operation_id"] != result["operation_id"] {
			return request, nil, errors.New("execution receipt does not match durable nonce")
		}
		expected, err = projectExecutionAcknowledgement(expected, plan, op, result, prefix)
		if err != nil {
			return request, nil, err
		}
		prefix = append(prefix, receipt)
	}
	return request, expected, nil
}

func projectExecutionAcknowledgement(inventory contract.Object, plan contract.Plan, op contract.Operation, ack contract.Object, receipts []contract.Object) (contract.Object, error) {
	if err := (ExecutionSync{}).ValidateAcknowledgement(plan, op, ack); err != nil {
		return nil, err
	}
	out, err := contract.Clone(inventory)
	if err != nil {
		return nil, err
	}
	providerResult, _ := contract.ObjectAt(ack, "provider_result")
	graph, _ := contract.ObjectAt(out, "issue_inventory")
	issues, _ := contract.Objects(graph, "issues")
	switch op.Kind {
	case "issue-reopen", "issue-close":
		n, _ := contract.PositiveInteger(op.Target["issue_number"])
		issue := backlogIssueByNumber(issues, n)
		if issue == nil || issue["id"] != op.Target["issue_id"] || issue["state"] != op.Before["state"] {
			return nil, errors.New("execution issue state before-state differs from projected inventory")
		}
		updated, err := backlogCreatedIssue(providerResult, plan.Repository)
		if err != nil || updated["state"] != op.After["state"] {
			return nil, errors.New("execution issue state acknowledgement has the wrong target state")
		}
		issue["state"] = op.After["state"]
		selected, err := contract.ObjectAt(out, "issue")
		if err != nil || selected["id"] != issue["id"] {
			return nil, errors.New("execution selected issue projection has another identity")
		}
		selected["state"] = op.After["state"]
	case "issue-comment-upsert":
		linked, _ := contract.ObjectAt(out, "linked_pr_inventory")
		comments, _ := contract.Objects(linked, "comments")
		n, _ := contract.PositiveInteger(op.Target["issue_number"])
		issue := backlogIssueByNumber(issues, n)
		if issue == nil || issue["id"] != op.Target["issue_id"] {
			return nil, errors.New("execution link comment issue identity changed")
		}
		commentID, _ := contract.PositiveInteger(op.Target["comment_id"])
		if commentID > 0 {
			var old contract.Object
			for _, comment := range comments {
				if comment["id"] == commentID {
					old = comment
				}
			}
			if old == nil || old["issue_number"] != n || old["body"] != op.Before["body"] {
				return nil, errors.New("execution link comment before-state differs from projected inventory")
			}
		}
		created, err := backlogCreatedComment(providerResult, plan.Repository, n)
		if err != nil {
			return nil, err
		}
		filtered := []contract.Object{}
		for _, comment := range comments {
			if commentID > 0 && comment["id"] == commentID {
				continue
			}
			filtered = append(filtered, comment)
		}
		filtered = append(filtered, created)
		sort.Slice(filtered, func(i, j int) bool { return filtered[i]["id"].(int64) < filtered[j]["id"].(int64) })
		linked["comments"] = backlogObjectsAsAny(filtered)
		linked["latest_comment_id"] = created["id"]
		linked["latest_pull_request_number"] = op.Target["pull_request_number"]
		linked["latest_pull_request_id"] = op.Target["pull_request_id"]
		linkedProv, _ := contract.ObjectAt(linked, "provenance")
		linkedProv["pull_requests_source"] = "github_api"
	case "project-membership-add":
		projectID, _ := contract.Nonempty(op.Target, "project_id")
		project, err := contract.ObjectAt(out, "project_inventory")
		if err != nil || project == nil {
			return nil, errors.New("execution Project membership is outside reviewed scope")
		}
		meta, _ := contract.ObjectAt(project, "project")
		if meta["id"] != projectID || op.Before["present"] != false || op.After["present"] != true {
			return nil, errors.New("execution Project membership identity or intent changed")
		}
		itemID, fields, issueNumber, nodeID, err := backlogProjectItemAck(providerResult, plan.Repository)
		if err != nil {
			return nil, err
		}
		targetNumber, targetErr := contract.PositiveInteger(op.Target["issue_number"])
		if targetErr != nil || issueNumber != targetNumber || nodeID != op.Target["issue_node_id"] {
			return nil, errors.New("execution Project membership acknowledgement targets another issue")
		}
		items, _ := contract.Objects(project, "items")
		for _, item := range items {
			n, _ := contract.PositiveInteger(item["number"])
			if n == issueNumber || item["item_id"] == itemID {
				return nil, errors.New("execution Project membership already exists in projected inventory")
			}
		}
		items = append(items, contract.Object{"item_id": itemID, "number": issueNumber, "field_values": fields, "archived": false})
		sort.Slice(items, func(i, j int) bool { return items[i]["number"].(int64) < items[j]["number"].(int64) })
		project["items"] = backlogObjectsAsAny(items)
	case "project-field-set":
		project, _ := contract.ObjectAt(out, "project_inventory")
		itemID, err := executionResolveItemID(op.Target, plan, receipts)
		if err != nil {
			return nil, err
		}
		items, _ := contract.Objects(project, "items")
		var item contract.Object
		for _, candidate := range items {
			if candidate["item_id"] == itemID {
				item = candidate
			}
		}
		if item == nil {
			return nil, errors.New("execution Project Status item is absent from projected inventory")
		}
		values, _ := contract.ObjectAt(item, "field_values")
		field := fmt.Sprint(op.Target["field_name"])
		before := op.Before["value"]
		if sentinel, ok := before.(map[string]any); ok && sentinel["from_membership"] != nil {
			// The completed membership ACK is the captured before-state source for
			// an item that did not exist in the reviewed inventory.
		} else if current, exists := values[field]; exists != (before != nil) || !same(current, before) {
			return nil, errors.New("execution Project Status before-state differs from projection")
		}
		if providerResult["clientMutationId"] != ack["operation_id"] {
			return nil, errors.New("execution Project Status acknowledgement has another nonce")
		}
		values[field] = op.After["value"]
	default:
		return nil, fmt.Errorf("unsupported execution projection kind %q", op.Kind)
	}
	return out, nil
}

func executionResolveItemID(target contract.Object, plan contract.Plan, receipts []contract.Object) (string, error) {
	ref, err := contract.ObjectAt(target, "item_ref")
	if err != nil {
		return "", err
	}
	if itemID, err := contract.Nonempty(ref, "item_id"); err == nil {
		return itemID, nil
	}
	from, err := contract.Nonempty(ref, "from_operation")
	if err != nil || from != "execution:project-membership" {
		return "", errors.New("execution Project item reference is malformed")
	}
	for _, receipt := range receipts {
		if receipt["id"] == from && receipt["status"] == "completed" {
			result, err := contract.ObjectAt(receipt, "result")
			if err != nil {
				return "", err
			}
			ops, err := executionOperations(plan)
			if err != nil {
				return "", err
			}
			var membership *contract.Operation
			for _, op := range ops {
				if op.ID == from && op.Kind == "project-membership-add" {
					copy := op
					membership = &copy
					break
				}
			}
			if membership == nil {
				return "", errors.New("execution Project Status references no derived membership primitive")
			}
			if err = (ExecutionSync{}).ValidateReceipt(plan, *membership, result); err != nil {
				return "", err
			}
			provider, err := contract.ObjectAt(result, "provider_result")
			if err != nil {
				return "", err
			}
			return contract.Nonempty(provider, "id")
		}
	}
	return "", errors.New("execution Project Status prerequisite membership has no completed receipt")
}

func executionCompletedPrefix(receipts []contract.Object, exclude string) []contract.Object {
	out := []contract.Object{}
	for _, receipt := range receipts {
		if receipt["status"] == "completed" && receipt["id"] != exclude {
			out = append(out, receipt)
		}
	}
	return out
}
