// Package workflow composes reviewed rebalance intent with complete live
// issue and Project sources and durable native mutation receipts.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/planning"
	"github.com/coreycoto/gh-steward/internal/snapshot"
)

const RebalanceCommand = "rebalance-apply"

// RebalanceProvider exposes only typed, target-scoped sources and writes.
// Source reads return a complete Project and an all-state repository issue
// graph joined to that Project. Linked-PR evidence is separate because its
// marker policy is consumer supplied.
type RebalanceProvider interface {
	ReadSources(context.Context, native.ProjectScope) (contract.Object, contract.Object, error)
	ReadLinkedPullRequests(context.Context, []int64, contract.Object) (contract.Object, error)
	SetProjectField(context.Context, native.ProjectItemScope, string, any, string) (contract.Object, error)
	UpdateIssue(context.Context, int64, contract.Object) (contract.Object, error)
	ArchiveProjectItem(context.Context, native.ProjectItemScope, bool, string) (contract.Object, error)
}

// NativeRebalanceProvider is the concrete transport adapter used by the CLI.
// It deliberately requires an explicit ProjectScope on every operation.
type NativeRebalanceProvider struct{ Transport *native.Transport }

func (n NativeRebalanceProvider) ReadSources(ctx context.Context, scope native.ProjectScope) (contract.Object, contract.Object, error) {
	if n.Transport == nil {
		return nil, nil, errors.New("rebalance provider requires a native transport")
	}
	service := snapshot.Service{Reader: n.Transport, Repository: n.Transport.Repository}
	project, err := service.Project(ctx, scope)
	if err != nil {
		return nil, nil, err
	}
	graph, err := service.IssueGraph(ctx, "all", project)
	if err != nil {
		return nil, nil, err
	}
	return graph, project, nil
}

func (n NativeRebalanceProvider) ReadLinkedPullRequests(ctx context.Context, numbers []int64, policy contract.Object) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("rebalance provider requires a native transport")
	}
	return (snapshot.Service{Reader: n.Transport, Repository: n.Transport.Repository}).LinkedPullRequests(ctx, numbers, policy)
}

func (n NativeRebalanceProvider) SetProjectField(ctx context.Context, item native.ProjectItemScope, fieldID string, value any, operationID string) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("rebalance provider requires a native transport")
	}
	return n.Transport.SetProjectField(ctx, item, fieldID, value, operationID)
}
func (n NativeRebalanceProvider) UpdateIssue(ctx context.Context, number int64, changes contract.Object) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("rebalance provider requires a native transport")
	}
	return n.Transport.UpdateIssue(ctx, number, changes)
}
func (n NativeRebalanceProvider) ArchiveProjectItem(ctx context.Context, item native.ProjectItemScope, archived bool, operationID string) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("rebalance provider requires a native transport")
	}
	return n.Transport.ArchiveProjectItem(ctx, item, archived, operationID)
}

// PrepareRebalance captures complete live issue and Project snapshots, derives
// the queue through an explicit consumer policy, and prepares its exact write
// primitives. options requires queue_mode/rank_step; optional status_values
// maps only canonical status values that actually need writing to concrete
// Project options. linked_pr_policy explicitly enables live linked-PR reads.
func PrepareRebalance(ctx context.Context, provider RebalanceProvider, repo contract.Repository, scope native.ProjectScope, policy snapshot.QueuePolicy, decisions, options, backlogAudit contract.Object, now time.Time) (contract.Plan, error) {
	if provider == nil {
		return contract.Plan{}, errors.New("rebalance preparation requires a provider")
	}
	if scope.Host != repo.Host || scope.Owner == "" || (scope.OwnerType != "User" && scope.OwnerType != "Organization") || scope.Number < 1 {
		return contract.Plan{}, errors.New("rebalance preparation requires an explicit same-host Project target")
	}
	policyObject := queuePolicyObject(policy)
	if _, err := snapshot.ParseQueuePolicy(policyObject); err != nil {
		return contract.Plan{}, err
	}
	if decisions == nil || options == nil {
		return contract.Plan{}, errors.New("rebalance preparation requires explicit decisions and options")
	}
	var err error
	decisions, err = contract.Clone(decisions)
	if err != nil {
		return contract.Plan{}, err
	}
	options, err = contract.Clone(options)
	if err != nil {
		return contract.Plan{}, err
	}
	if _, supplied := options["linked_prs_by_issue"]; supplied {
		return contract.Plan{}, errors.New("linked pull-request state must come from the typed live source, not caller options")
	}
	if _, err := optionsStatusValues(options); err != nil {
		return contract.Plan{}, err
	}
	audit := backlogAudit
	if audit == nil {
		audit = contract.Object{}
	}
	audit, err = contract.Clone(audit)
	if err != nil {
		return contract.Plan{}, err
	}
	graph, project, err := provider.ReadSources(ctx, scope)
	if err != nil {
		return contract.Plan{}, err
	}
	project, scope, err = validateRebalanceSources(graph, project, repo, scope)
	if err != nil {
		return contract.Plan{}, err
	}
	data := contract.Object{
		"decision_payload": decisions,
		"options":          options,
		"queue_policy":     policyObject,
		"project_scope":    projectScopeObject(scope),
		"issue_graph":      graph,
		"project_snapshot": project,
		"backlog_audit":    audit,
	}
	if linkedPolicy, ok := options["linked_pr_policy"].(map[string]any); ok {
		issues, err := decisionIssueNumbers(decisions, "archive_issue_numbers")
		if err != nil {
			return contract.Plan{}, err
		}
		if len(issues) > 0 {
			linked, err := provider.ReadLinkedPullRequests(ctx, issues, linkedPolicy)
			if err != nil {
				return contract.Plan{}, err
			}
			if err := validateLinkedEvidence(linked, repo, issues); err != nil {
				return contract.Plan{}, err
			}
			linked["policy"] = linkedPolicy
			data["linked_pr_evidence"] = linked
			options["linked_prs_by_issue"] = linked["by_issue"]
			data["options"] = options
		}
	} else if _, exists := options["linked_pr_policy"]; exists {
		return contract.Plan{}, errors.New("options.linked_pr_policy must be an explicit object")
	}
	build, err := rebalanceBuild(repo, data)
	if err != nil {
		return contract.Plan{}, err
	}
	data["build"] = build
	operations, err := rebalanceOperations(repo, data, build)
	if err != nil {
		return contract.Plan{}, err
	}
	projectObject, _ := contract.ObjectAt(project, "project")
	sources := contract.Object{
		"issue_graph": contract.Object{"source": "github_api", "live": true, "complete": true, "state": "all", "project_join": "complete"},
		"project":     contract.Object{"source": "github_project_api", "live": true, "complete": true, "id": scope.ID, "number": scope.Number, "owner_login": scope.Owner, "owner_type": scope.OwnerType, "title": projectObject["title"]},
	}
	if evidence, exists := data["linked_pr_evidence"]; exists {
		e, _ := evidence.(map[string]any)
		prov, _ := e["provenance"].(map[string]any)
		sources["linked_pull_requests"] = contract.Object{"source": "github_api", "live": true, "complete": true, "issues": e["issue_numbers"], "policy": options["linked_pr_policy"], "state_filter": "all"}
		if prov["live"] != true || prov["complete"] != true {
			return contract.Plan{}, errors.New("linked pull-request source is not complete live evidence")
		}
	}
	return contract.PreparePlan(RebalanceCommand, repo, sources, data, operations, now)
}

func queuePolicyObject(p snapshot.QueuePolicy) contract.Object {
	priorities := contract.Object{}
	for key, value := range p.Priorities {
		priorities[key] = value
	}
	done := make([]any, len(p.DoneStatuses))
	for i, value := range p.DoneStatuses {
		done[i] = value
	}
	excluded := make([]any, len(p.ExcludedPrefixes))
	for i, value := range p.ExcludedPrefixes {
		excluded[i] = value
	}
	return contract.Object{"status_field": p.StatusField, "priority_field": p.PriorityField, "order_field": p.OrderField, "done_statuses": done, "priorities": priorities, "excluded_prefixes": excluded}
}

func projectScopeObject(p native.ProjectScope) contract.Object {
	return contract.Object{"host": p.Host, "owner_login": p.Owner, "owner_type": p.OwnerType, "number": p.Number, "id": p.ID}
}

func parseRebalanceProjectScope(raw contract.Object) (native.ProjectScope, error) {
	if len(raw) != 5 {
		return native.ProjectScope{}, errors.New("reviewed Project scope must contain its exact identity fields")
	}
	host, err := contract.Nonempty(raw, "host")
	if err != nil {
		return native.ProjectScope{}, err
	}
	owner, err := contract.Nonempty(raw, "owner_login")
	if err != nil {
		return native.ProjectScope{}, err
	}
	typ, err := contract.Nonempty(raw, "owner_type")
	if err != nil || (typ != "User" && typ != "Organization") {
		return native.ProjectScope{}, errors.New("reviewed Project owner type is unsupported")
	}
	number, err := contract.PositiveInteger(raw["number"])
	if err != nil {
		return native.ProjectScope{}, err
	}
	id, err := contract.Nonempty(raw, "id")
	if err != nil {
		return native.ProjectScope{}, err
	}
	return native.ProjectScope{Host: host, Owner: owner, OwnerType: typ, Number: number, ID: id}, nil
}

func validateRebalanceSources(graph, project contract.Object, repo contract.Repository, scope native.ProjectScope) (contract.Object, native.ProjectScope, error) {
	if graph == nil || project == nil {
		return nil, native.ProjectScope{}, errors.New("rebalance requires complete issue and Project snapshots")
	}
	for _, source := range []contract.Object{graph, project} {
		rawRepo, err := contract.ObjectAt(source, "repo")
		if err != nil {
			return nil, native.ProjectScope{}, err
		}
		identity, err := contract.ParseRepository(rawRepo)
		if err != nil || identity != repo {
			return nil, native.ProjectScope{}, errors.New("rebalance source belongs to another repository or host")
		}
		provenance, err := contract.ObjectAt(source, "provenance")
		if err != nil || provenance["live"] != true || provenance["complete"] != true {
			return nil, native.ProjectScope{}, errors.New("rebalance requires complete live issue and Project evidence")
		}
	}
	graphProvenance, _ := contract.ObjectAt(graph, "provenance")
	if graphProvenance["project_joins_live"] != true || graphProvenance["issue_state"] != "all" {
		return nil, native.ProjectScope{}, errors.New("rebalance requires live Project membership joins in the complete issue graph")
	}
	if _, err := contract.Nonempty(graphProvenance, "repository_node_id"); err != nil {
		return nil, native.ProjectScope{}, errors.New("rebalance requires the immutable live repository node identity")
	}
	projectObject, err := contract.ObjectAt(project, "project")
	if err != nil {
		return nil, native.ProjectScope{}, err
	}
	id, err := contract.Nonempty(projectObject, "id")
	if err != nil || (scope.ID != "" && scope.ID != id) {
		return nil, native.ProjectScope{}, errors.New("rebalance Project node identity changed")
	}
	number, err := contract.PositiveInteger(projectObject["number"])
	if err != nil || number != scope.Number {
		return nil, native.ProjectScope{}, errors.New("rebalance Project number does not match the explicit target")
	}
	owner, err := contract.Nonempty(projectObject, "owner_login")
	if err != nil || !strings.EqualFold(owner, scope.Owner) || projectObject["owner_type"] != scope.OwnerType || !strings.EqualFold(fmt.Sprint(projectObject["host"]), scope.Host) {
		return nil, native.ProjectScope{}, errors.New("rebalance Project owner or host changed")
	}
	if _, err := contract.Nonempty(projectObject, "title"); err != nil {
		return nil, native.ProjectScope{}, err
	}
	if _, err := contract.Nonempty(projectObject, "url"); err != nil {
		return nil, native.ProjectScope{}, err
	}
	if _, err := contract.Bool(projectObject, "closed"); err != nil {
		return nil, native.ProjectScope{}, err
	}
	if _, err := contract.Bool(projectObject, "public"); err != nil {
		return nil, native.ProjectScope{}, err
	}
	if err := validateProjectFieldInventory(project); err != nil {
		return nil, native.ProjectScope{}, err
	}
	scope.ID = id
	if err := validateProjectIssueJoin(graph, project, repo); err != nil {
		return nil, native.ProjectScope{}, err
	}
	copyProject, err := contract.Clone(project)
	if err != nil {
		return nil, native.ProjectScope{}, err
	}
	return copyProject, scope, nil
}

func validateProjectFieldInventory(project contract.Object) error {
	definitions, err := contract.ObjectAt(project, "fields_by_name")
	if err != nil {
		return err
	}
	fieldIDs := map[string]bool{}
	for name, raw := range definitions {
		definition, ok := raw.(map[string]any)
		if !ok || name == "" || definition["name"] != name {
			return errors.New("complete Project field inventory contains an invalid name")
		}
		id, err := contract.Nonempty(definition, "id")
		if err != nil || fieldIDs[id] {
			return errors.New("complete Project field inventory contains a missing or duplicate ID")
		}
		if _, err := contract.Nonempty(definition, "data_type"); err != nil {
			return err
		}
		fieldIDs[id] = true
		options, err := contract.ObjectAt(definition, "options_by_name")
		if err != nil {
			return err
		}
		optionIDs := map[string]bool{}
		for optionName, rawOption := range options {
			option, ok := rawOption.(map[string]any)
			if !ok || optionName == "" || option["name"] != optionName {
				return errors.New("complete Project field inventory contains an invalid option")
			}
			optionID, err := contract.Nonempty(option, "id")
			if err != nil || optionIDs[optionID] {
				return errors.New("complete Project field inventory contains a missing or duplicate option ID")
			}
			optionIDs[optionID] = true
		}
	}
	return nil
}

func validateProjectIssueJoin(graph, project contract.Object, repo contract.Repository) error {
	projectRows, err := contract.Objects(project, "items")
	if err != nil {
		return err
	}
	projectByNumber := map[int64]contract.Object{}
	projectIDs := map[string]bool{}
	projectRowsByID := map[string]contract.Object{}
	validator := native.Transport{Repository: repo}
	for _, item := range projectRows {
		if item["content_type"] != "Issue" {
			return errors.New("rebalance Project inventory contains a non-issue item")
		}
		n, err := contract.PositiveInteger(item["number"])
		if err != nil || projectByNumber[n] != nil {
			return errors.New("rebalance Project inventory has missing or duplicate issue numbers")
		}
		id, err := contract.Nonempty(item, "item_id")
		if err != nil || projectIDs[id] {
			return errors.New("rebalance Project inventory has missing or duplicate item IDs")
		}
		if item["state"] != "OPEN" && item["state"] != "CLOSED" {
			return errors.New("rebalance Project inventory has invalid issue state")
		}
		if _, err := contract.String(item, "title"); err != nil {
			return err
		}
		if err := validator.ValidateIssueURL(item["url"], n); err != nil {
			return err
		}
		if item["repository"] != repo.FullName() {
			return errors.New("rebalance Project inventory contains a foreign repository issue")
		}
		if _, err := contract.Bool(item, "archived"); err != nil {
			return err
		}
		fieldValues, err := contract.ObjectAt(item, "field_values")
		if err != nil {
			return err
		}
		definitions, err := contract.ObjectAt(project, "fields_by_name")
		if err != nil {
			return err
		}
		for name, value := range fieldValues {
			if value == nil {
				return errors.New("normalized Project field values cannot contain explicit null")
			}
			if _, exists := definitions[name]; !exists {
				return errors.New("Project item contains a field outside the complete definition inventory")
			}
		}
		projectIDs[id] = true
		projectByNumber[n] = item
		projectRowsByID[id] = item
	}
	graphRows, err := contract.Objects(graph, "issues")
	if err != nil {
		return err
	}
	graphByNumber := map[int64]contract.Object{}
	joined := map[int64]bool{}
	for _, issue := range graphRows {
		n, err := contract.PositiveInteger(issue["number"])
		if err != nil || graphByNumber[n] != nil {
			return errors.New("rebalance issue graph has missing or duplicate issue numbers")
		}
		if _, err := contract.Nonempty(issue, "id"); err != nil {
			return errors.New("rebalance issue graph has a missing immutable issue identity")
		}
		for _, key := range []string{"title", "body"} {
			if _, err := contract.String(issue, key); err != nil {
				return err
			}
		}
		labels, err := contract.Strings(issue["labels"])
		if err != nil {
			return err
		}
		labelNames := map[string]bool{}
		for _, label := range labels {
			if label == "" || labelNames[label] {
				return errors.New("rebalance issue labels are incomplete or duplicated")
			}
			labelNames[label] = true
		}
		if milestone, exists := issue["milestone"]; !exists {
			return errors.New("rebalance issue milestone nullability is missing")
		} else if milestone != nil {
			if _, ok := milestone.(string); !ok {
				return errors.New("rebalance issue milestone must be a title or null")
			}
		}
		if err := validator.ValidateIssueURL(issue["url"], n); err != nil {
			return err
		}
		graphByNumber[n] = issue
		if issue["state"] != "OPEN" && issue["state"] != "CLOSED" {
			return errors.New("rebalance issue graph has invalid issue state")
		}
		inProject, err := contract.Bool(issue, "in_project")
		if err != nil {
			return err
		}
		itemRaw, exists := issue["project_item"]
		if !inProject {
			if !exists || itemRaw != nil {
				return errors.New("nonmember issue has unexpected Project item evidence")
			}
			fields, err := contract.ObjectAt(issue, "field_values")
			if err != nil || len(fields) != 0 {
				return errors.New("nonmember issue has unexpected Project field values")
			}
			continue
		}
		item, ok := itemRaw.(map[string]any)
		if !ok {
			return errors.New("Project member issue is missing its live item identity")
		}
		projectRow := projectByNumber[n]
		if projectRow == nil {
			return errors.New("issue graph contains an unrepresented Project membership")
		}
		itemID, err := contract.Nonempty(item, "item_id")
		if err != nil || itemID != projectRow["item_id"] || joined[n] {
			return errors.New("issue graph Project membership identity changed")
		}
		archived, err := contract.Bool(item, "archived")
		if err != nil || archived != (projectRow["archived"] == true) {
			return errors.New("issue graph Project archive state differs from the full Project inventory")
		}
		fields, err := contract.ObjectAt(item, "field_values")
		if err != nil || !same(fields, projectRow["field_values"]) || !same(issue["field_values"], fields) {
			return errors.New("issue graph Project field values differ from the full Project inventory")
		}
		if issue["state"] != projectRow["state"] || issue["title"] != projectRow["title"] || issue["url"] != projectRow["url"] {
			return errors.New("issue graph and Project item content disagree")
		}
		joined[n] = true
	}
	if len(joined) != len(projectRowsByID) {
		return errors.New("complete issue graph omits a Project member")
	}
	projectObject, err := contract.ObjectAt(project, "project")
	if err != nil {
		return err
	}
	graphProject, err := contract.ObjectAt(graph, "project")
	if err != nil || !same(projectObject, graphProject) {
		return errors.New("issue graph and Project snapshot identities differ")
	}
	return nil
}

func decisionIssueNumbers(decisions contract.Object, key string) ([]int64, error) {
	values, err := contract.Array(decisions, key)
	if err != nil {
		return nil, err
	}
	result := make([]int64, 0, len(values))
	seen := map[int64]bool{}
	for _, value := range values {
		n, err := contract.PositiveInteger(value)
		if err != nil || seen[n] {
			return nil, fmt.Errorf("%s must contain unique positive issue numbers", key)
		}
		seen[n] = true
		result = append(result, n)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func validateLinkedEvidence(evidence contract.Object, repo contract.Repository, numbers []int64) error {
	provenance, err := contract.ObjectAt(evidence, "provenance")
	if err != nil || provenance["live"] != true || provenance["complete"] != true {
		return errors.New("linked pull-request evidence must be complete and live")
	}
	rawRepo, err := contract.ObjectAt(evidence, "repo")
	if err != nil {
		return err
	}
	identity, err := contract.ParseRepository(rawRepo)
	if err != nil || identity != repo {
		return errors.New("linked pull-request evidence belongs to another repository")
	}
	if !same(evidence["issue_numbers"], int64Values(numbers)) {
		return errors.New("linked pull-request evidence does not cover the reviewed archive issues")
	}
	byIssue, err := contract.ObjectAt(evidence, "by_issue")
	if err != nil || len(byIssue) != len(numbers) {
		return errors.New("linked pull-request evidence is incomplete for archive decisions")
	}
	for _, n := range numbers {
		key := strconv.FormatInt(n, 10)
		if _, exists := byIssue[key]; !exists {
			return errors.New("linked pull-request source must explicitly report each archive issue")
		}
		if entry := byIssue[key]; entry != nil {
			if _, ok := entry.(map[string]any); !ok {
				return errors.New("linked pull-request evidence contains a malformed normalized entry")
			}
		}
	}
	if _, err := contract.Array(evidence, "comments"); err != nil {
		return err
	}
	if _, err := contract.Array(evidence, "pull_requests"); err != nil {
		return err
	}
	if _, err := contract.Array(evidence, "unresolved_links"); err != nil {
		return err
	}
	return nil
}

func int64Values(numbers []int64) []any {
	values := make([]any, len(numbers))
	for i, n := range numbers {
		values[i] = n
	}
	return values
}

func rebalanceBuild(repo contract.Repository, data contract.Object) (contract.Object, error) {
	allowedData := map[string]bool{"decision_payload": true, "options": true, "queue_policy": true, "project_scope": true, "issue_graph": true, "project_snapshot": true, "backlog_audit": true, "linked_pr_evidence": true, "build": true}
	for key := range data {
		if !allowedData[key] {
			return nil, fmt.Errorf("reviewed rebalance data contains unsupported property %q", key)
		}
	}
	for _, key := range []string{"decision_payload", "options", "queue_policy", "project_scope", "issue_graph", "project_snapshot", "backlog_audit"} {
		if _, exists := data[key]; !exists {
			return nil, fmt.Errorf("reviewed rebalance data is missing %s", key)
		}
	}
	if len(data) < 7 || len(data) > 9 {
		return nil, errors.New("reviewed rebalance data has an unsupported shape")
	}
	linkedPolicyPresent := false
	if options, ok := data["options"].(map[string]any); ok {
		_, linkedPolicyPresent = options["linked_prs_by_issue"]
	}
	_, linkedEvidencePresent := data["linked_pr_evidence"]
	if linkedPolicyPresent != linkedEvidencePresent {
		return nil, errors.New("rebalance linked-PR decision map requires its captured live source evidence")
	}
	decisions, err := contract.ObjectAt(data, "decision_payload")
	if err != nil {
		return nil, err
	}
	options, err := contract.ObjectAt(data, "options")
	if err != nil {
		return nil, err
	}
	if _, err := optionsStatusValues(options); err != nil {
		return nil, err
	}
	policyRaw, err := contract.ObjectAt(data, "queue_policy")
	if err != nil {
		return nil, err
	}
	policy, err := snapshot.ParseQueuePolicy(policyRaw)
	if err != nil {
		return nil, err
	}
	graph, err := contract.ObjectAt(data, "issue_graph")
	if err != nil {
		return nil, err
	}
	project, err := contract.ObjectAt(data, "project_snapshot")
	if err != nil {
		return nil, err
	}
	scopeRaw, err := contract.ObjectAt(data, "project_scope")
	if err != nil {
		return nil, err
	}
	scope, err := parseRebalanceProjectScope(scopeRaw)
	if err != nil {
		return nil, err
	}
	if scope.Host != repo.Host {
		return nil, errors.New("reviewed Project is hosted on another repository host")
	}
	project, validatedScope, err := validateRebalanceSources(graph, project, repo, scope)
	if err != nil || validatedScope.ID != scope.ID {
		return nil, errors.New("reviewed issue and Project snapshots are invalid")
	}
	if _, err := contract.ObjectAt(data, "backlog_audit"); err != nil {
		return nil, err
	}
	if _, err := contract.ObjectAt(options, "linked_prs_by_issue"); err == nil {
		// The only acceptable linked-PR map is derived from the captured typed source.
		evidence, e := contract.ObjectAt(data, "linked_pr_evidence")
		if e != nil || !same(options["linked_prs_by_issue"], evidence["by_issue"]) {
			return nil, errors.New("rebalance linked-PR decisions differ from captured live evidence")
		}
	}
	if evidenceRaw, exists := data["linked_pr_evidence"]; exists {
		evidence, ok := evidenceRaw.(map[string]any)
		if !ok {
			return nil, errors.New("linked-PR source evidence must be an object")
		}
		numbers, err := decisionIssueNumbers(decisions, "archive_issue_numbers")
		if err != nil {
			return nil, err
		}
		if err := validateLinkedEvidence(evidence, repo, numbers); err != nil {
			return nil, err
		}
		policy, _ := contract.ObjectAt(data, "options")
		linkedPolicy, err := contract.ObjectAt(policy, "linked_pr_policy")
		if err != nil || !same(evidence["policy"], linkedPolicy) {
			return nil, errors.New("linked pull-request evidence policy changed")
		}
	}
	queue, err := (snapshot.Service{Repository: repo}).Queue(graph, policy)
	if err != nil {
		return nil, err
	}
	audit, err := contract.ObjectAt(data, "backlog_audit")
	if err != nil {
		return nil, err
	}
	built, err := planning.BuildRebalancePlan(decisions, graph, queue, project, options, audit, policy.ExcludedPrefixes)
	if err != nil {
		return nil, err
	}
	if projectObject, _ := contract.ObjectAt(project, "project"); projectObject["closed"] == true {
		if delta, _ := contract.ObjectAt(built, "delta"); lenMustHaveOps(delta) {
			return nil, errors.New("closed Projects cannot receive rebalance writes")
		}
	}
	return built, nil
}

func lenMustHaveOps(delta contract.Object) bool {
	changes, _ := contract.Array(delta, "changes")
	archives, _ := contract.Array(delta, "archive_actions")
	return len(changes) > 0 || len(archives) > 0
}

func optionsStatusValues(options contract.Object) (contract.Object, error) {
	allowed := map[string]bool{"queue_mode": true, "rank_step": true, "linked_prs_by_issue": true, "linked_pr_policy": true, "status_values": true}
	for key := range options {
		if !allowed[key] {
			return nil, fmt.Errorf("rebalance options contain unsupported property %q", key)
		}
	}
	if _, err := contract.Nonempty(options, "queue_mode"); err != nil {
		return nil, err
	}
	if _, err := contract.PositiveInteger(options["rank_step"]); err != nil {
		return nil, errors.New("rebalance options.rank_step must be a positive integer")
	}
	if raw, exists := options["status_values"]; exists {
		statusValues, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("rebalance options.status_values must be an object")
		}
		for key, value := range statusValues {
			if key != "Todo" && key != "Done" {
				return nil, errors.New("rebalance status_values supports only planner-emitted Todo and Done targets")
			}
			if _, ok := value.(string); !ok || strings.TrimSpace(value.(string)) == "" {
				return nil, errors.New("rebalance status_values entries must be nonempty consumer options")
			}
		}
	}
	if raw, exists := options["linked_pr_policy"]; exists {
		policy, ok := raw.(map[string]any)
		if !ok || len(policy) != 2 {
			return nil, errors.New("rebalance options.linked_pr_policy must contain only marker_prefix and pr_number_pattern")
		}
		for _, key := range []string{"marker_prefix", "pr_number_pattern"} {
			if _, err := contract.Nonempty(policy, key); err != nil {
				return nil, fmt.Errorf("rebalance linked-PR policy requires %s", key)
			}
		}
	}
	return objectOrEmpty(options, "status_values"), nil
}

func rebalanceOperations(repo contract.Repository, data, built contract.Object) ([]contract.Operation, error) {
	options, err := contract.ObjectAt(data, "options")
	if err != nil {
		return nil, err
	}
	statusValues, err := optionsStatusValues(options)
	if err != nil {
		return nil, err
	}
	policyRaw, _ := contract.ObjectAt(data, "queue_policy")
	policy, err := snapshot.ParseQueuePolicy(policyRaw)
	if err != nil {
		return nil, err
	}
	graph, _ := contract.ObjectAt(data, "issue_graph")
	project, _ := contract.ObjectAt(data, "project_snapshot")
	projectObject, _ := contract.ObjectAt(project, "project")
	projectID, err := contract.Nonempty(projectObject, "id")
	if err != nil {
		return nil, err
	}
	projectTitle, err := contract.Nonempty(projectObject, "title")
	if err != nil {
		return nil, err
	}
	projectNumber, err := contract.PositiveInteger(projectObject["number"])
	if err != nil {
		return nil, err
	}
	fields, err := contract.ObjectAt(project, "fields_by_name")
	if err != nil {
		return nil, err
	}
	statusField, err := rebalanceFieldDefinition(fields, policy.StatusField, "SINGLE_SELECT")
	if err != nil {
		return nil, err
	}
	priorityField, err := rebalanceFieldDefinition(fields, policy.PriorityField, "SINGLE_SELECT")
	if err != nil {
		return nil, err
	}
	orderField, err := rebalanceFieldDefinition(fields, policy.OrderField, "NUMBER")
	if err != nil {
		return nil, err
	}
	if policy.StatusField == policy.PriorityField || policy.StatusField == policy.OrderField || policy.PriorityField == policy.OrderField {
		return nil, errors.New("rebalance policy fields must be distinct")
	}
	statusOptions, err := contract.ObjectAt(statusField, "options_by_name")
	if err != nil {
		return nil, err
	}
	priorityOptions, err := contract.ObjectAt(priorityField, "options_by_name")
	if err != nil {
		return nil, err
	}
	priorityForBand := map[string]string{}
	for option, band := range policy.Priorities {
		if _, ok := priorityOptions[option]; !ok {
			return nil, fmt.Errorf("priority policy option %q is absent from the reviewed Project definition", option)
		}
		priorityForBand[band] = option
	}
	done := map[string]bool{}
	for _, value := range policy.DoneStatuses {
		if _, ok := statusOptions[value]; !ok {
			return nil, fmt.Errorf("Done status %q is absent from the reviewed Project definition", value)
		}
		done[value] = true
	}
	for canonical, valueRaw := range statusValues {
		value := valueRaw.(string)
		if _, ok := statusOptions[value]; !ok {
			return nil, fmt.Errorf("status_values.%s option %q is absent from the reviewed Project definition", canonical, value)
		}
		if canonical == "Done" && !done[value] {
			return nil, errors.New("status_values.Done must be a member of QueuePolicy.DoneStatuses")
		}
		if canonical == "Todo" && done[value] {
			return nil, errors.New("status_values.Todo cannot be configured as a Done status")
		}
	}
	projectItems, err := contract.Objects(project, "items")
	if err != nil {
		return nil, err
	}
	itemsByNumber := map[int64]contract.Object{}
	for _, item := range projectItems {
		n, _ := contract.PositiveInteger(item["number"])
		itemsByNumber[n] = item
	}
	issueRows, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	issuesByNumber := map[int64]contract.Object{}
	for _, issue := range issueRows {
		n, _ := contract.PositiveInteger(issue["number"])
		issuesByNumber[n] = issue
	}
	delta, err := contract.ObjectAt(built, "delta")
	if err != nil {
		return nil, err
	}
	operations := []contract.Operation{}
	appendField := func(number int64, item, issue contract.Object, name string, definition contract.Object, desired any) error {
		fields, err := contract.ObjectAt(item, "field_values")
		if err != nil {
			return err
		}
		present := false
		var current any
		if value, exists := fields[name]; exists {
			if value == nil {
				return fmt.Errorf("normalized Project field %s cannot contain explicit null", name)
			}
			present, current = true, value
		}
		if present && scalarEqual(current, desired, definition["data_type"]) {
			return nil
		}
		itemID, _ := contract.Nonempty(item, "item_id")
		issueID, _ := contract.Nonempty(issue, "id")
		issueURL, _ := contract.Nonempty(issue, "url")
		fieldID, _ := contract.Nonempty(definition, "id")
		fieldKey := "priority"
		if name == policy.OrderField {
			fieldKey = "queue-order"
		} else if name == policy.StatusField {
			fieldKey = "status"
		}
		before := contract.Object{"present": present, "value": current, "issue_state": issue["state"], "archived": item["archived"]}
		if !present {
			before["value"] = nil
		}
		after := contract.Object{"present": true, "value": desired}
		target := contract.Object{"issue_number": number, "issue_id": issueID, "issue_url": issueURL, "project_id": projectID, "project_number": projectNumber, "project_title": projectTitle, "project_owner_login": projectObject["owner_login"], "project_owner_type": projectObject["owner_type"], "project_host": projectObject["host"], "item_id": itemID, "field_id": fieldID, "field_name": name, "status_field": policy.StatusField}
		operations = append(operations, contract.Operation{ID: fmt.Sprintf("rebalance:%d:%s", number, fieldKey), Kind: "project-field", Target: target, Before: before, After: after})
		return nil
	}
	changes, err := contract.Objects(delta, "changes")
	if err != nil {
		return nil, err
	}
	for _, change := range changes {
		number, err := contract.PositiveInteger(change["issue_number"])
		if err != nil {
			return nil, err
		}
		item, issue := itemsByNumber[number], issuesByNumber[number]
		if item == nil || issue == nil || item["archived"] == true || issue["state"] != "OPEN" {
			return nil, fmt.Errorf("rebalance change issue #%d is no longer an active Project member", number)
		}
		priority, ok := change["target_priority"].(string)
		consumerPriority, mapped := priorityForBand[priority]
		if !ok || !mapped {
			return nil, errors.New("rebalance plan contains an unmapped semantic priority")
		}
		if err := appendField(number, item, issue, policy.PriorityField, priorityField, consumerPriority); err != nil {
			return nil, err
		}
		order, err := contract.Number(change["target_queue_order"])
		if err != nil || math.IsNaN(order) || math.IsInf(order, 0) {
			return nil, errors.New("rebalance target queue order is not a finite number")
		}
		if err := appendField(number, item, issue, policy.OrderField, orderField, change["target_queue_order"]); err != nil {
			return nil, err
		}
		currentCanonical := change["current_status"]
		desiredCanonical := "Todo"
		if target, exists := change["target_status"]; exists && target != nil {
			desiredCanonical, ok = target.(string)
			if !ok {
				return nil, errors.New("rebalance target status must be a string")
			}
		} else if currentCanonical == "Todo" || currentCanonical == "In Progress" || currentCanonical == "Done" {
			desiredCanonical = currentCanonical.(string)
		}
		currentValue, present := objectOrEmpty(item, "field_values")[policy.StatusField]
		if desiredCanonical == "Done" && present && done[fmt.Sprint(currentValue)] {
			continue
		}
		if present && currentValue == desiredCanonical {
			continue
		}
		consumerStatus, exists := statusValues[desiredCanonical]
		if !exists {
			return nil, fmt.Errorf("rebalance requires an explicit options.status_values.%s consumer mapping", desiredCanonical)
		}
		if err := appendField(number, item, issue, policy.StatusField, statusField, consumerStatus); err != nil {
			return nil, err
		}
	}
	archives, err := contract.Objects(delta, "archive_actions")
	if err != nil {
		return nil, err
	}
	for _, archive := range archives {
		if archive["can_archive"] != true {
			continue
		}
		number, err := contract.PositiveInteger(archive["issue_number"])
		if err != nil {
			return nil, err
		}
		item, issue := itemsByNumber[number], issuesByNumber[number]
		if item == nil || issue == nil {
			return nil, fmt.Errorf("archive issue #%d is missing from the complete reviewed source", number)
		}
		if item["archived"] == true {
			continue
		}
		if issue["state"] != "OPEN" && issue["state"] != "CLOSED" {
			return nil, errors.New("archive target issue state is incomplete")
		}
		status, present := objectOrEmpty(item, "field_values")[policy.StatusField]
		statusDone := present && done[fmt.Sprint(status)]
		stateClosed := issue["state"] == "CLOSED"
		if !stateClosed && !statusDone {
			// A merged linked PR may establish eligibility, but the selected issue
			// must have a normalized merged PR in the captured complete source.
			if !issueHasMergedLinkedPR(data, number) {
				return nil, errors.New("archive eligibility relies on linked-PR evidence without a captured live source")
			}
		}
		itemID, _ := contract.Nonempty(item, "item_id")
		issueID, _ := contract.Nonempty(issue, "id")
		issueURL, _ := contract.Nonempty(issue, "url")
		if issue["state"] == "OPEN" {
			operations = append(operations, contract.Operation{ID: fmt.Sprintf("rebalance:%d:close", number), Kind: "issue-close", Target: archiveTarget(number, issueID, issueURL, projectID, projectNumber, projectTitle, itemID, projectObject, policy.StatusField), Before: contract.Object{"state": "OPEN", "project_state": "OPEN", "archived": false}, After: contract.Object{"state": "CLOSED", "project_state": "CLOSED"}})
		}
		projectedStatus := fieldEvidence(item, policy.StatusField)
		if !statusDone {
			consumerStatus, exists := statusValues["Done"]
			if !exists {
				return nil, errors.New("rebalance archive requires explicit options.status_values.Done consumer mapping")
			}
			if !done[fmt.Sprint(consumerStatus)] {
				return nil, errors.New("archive Done mapping is not an accepted QueuePolicy.DoneStatuses value")
			}
			priorOperations := len(operations)
			if err := appendField(number, item, issue, policy.StatusField, statusField, consumerStatus); err != nil {
				return nil, err
			}
			if len(operations) > priorOperations {
				operations[len(operations)-1].Before["issue_state"] = "CLOSED"
			}
			projectedStatus = contract.Object{"present": true, "value": consumerStatus}
		}
		before := contract.Object{"archived": false, "issue_state": "CLOSED", "status": projectedStatus}
		operations = append(operations, contract.Operation{ID: fmt.Sprintf("rebalance:%d:archive", number), Kind: "project-item-archive", Target: archiveTarget(number, issueID, issueURL, projectID, projectNumber, projectTitle, itemID, projectObject, policy.StatusField), Before: before, After: contract.Object{"archived": true}})
	}
	for index := range operations {
		if index > 0 && operations[index-1].ID == operations[index].ID {
			return nil, errors.New("rebalance plan contains duplicate primitive identities")
		}
	}
	return operations, nil
}

func issueHasMergedLinkedPR(data contract.Object, number int64) bool {
	evidence, err := contract.ObjectAt(data, "linked_pr_evidence")
	if err != nil {
		return false
	}
	byIssue, err := contract.ObjectAt(evidence, "by_issue")
	if err != nil {
		return false
	}
	pr, ok := byIssue[strconv.FormatInt(number, 10)].(map[string]any)
	return ok && pr["is_merged"] == true
}

func archiveTarget(number int64, issueID, issueURL, projectID string, projectNumber int64, projectTitle, itemID string, project contract.Object, statusField string) contract.Object {
	return contract.Object{"issue_number": number, "issue_id": issueID, "issue_url": issueURL, "project_id": projectID, "project_number": projectNumber, "project_title": projectTitle, "project_owner_login": project["owner_login"], "project_owner_type": project["owner_type"], "project_host": project["host"], "item_id": itemID, "status_field": statusField}
}

func fieldEvidence(item contract.Object, field string) contract.Object {
	values := objectOrEmpty(item, "field_values")
	value, present := values[field]
	if !present {
		value = nil
	}
	return contract.Object{"present": present, "value": value}
}

func rebalanceFieldDefinition(fields contract.Object, name, expectedType string) (contract.Object, error) {
	definition, err := contract.ObjectAt(fields, name)
	if err != nil {
		return nil, fmt.Errorf("configured Project field %q is missing", name)
	}
	if definition["name"] != name || definition["data_type"] != expectedType {
		return nil, fmt.Errorf("configured Project field %q has an unexpected definition", name)
	}
	if _, err := contract.Nonempty(definition, "id"); err != nil {
		return nil, err
	}
	return definition, nil
}

func scalarEqual(left, right any, dataType any) bool {
	if dataType == "NUMBER" {
		a, errA := contract.Number(left)
		b, errB := contract.Number(right)
		return errA == nil && errB == nil && a == b
	}
	return same(left, right)
}

func objectOrEmpty(parent contract.Object, key string) contract.Object {
	if value, ok := parent[key].(map[string]any); ok {
		return value
	}
	return contract.Object{}
}

func operationScope(operation contract.Operation, graph, project contract.Object) (contract.Object, error) {
	target := operation.Target
	number, err := contract.PositiveInteger(target["issue_number"])
	if err != nil {
		return nil, err
	}
	issue, item, err := sourceTarget(graph, project, number)
	if err != nil {
		return nil, err
	}
	if target["item_id"] != item["item_id"] || target["issue_id"] != issue["id"] || target["issue_url"] != issue["url"] {
		return nil, errors.New("rebalance primitive no longer targets the captured Project issue")
	}
	projectObject, _ := contract.ObjectAt(project, "project")
	projectNumber, err := contract.PositiveInteger(projectObject["number"])
	targetProjectNumber, targetErr := contract.PositiveInteger(target["project_number"])
	if err != nil || targetErr != nil || target["project_id"] != projectObject["id"] || target["project_title"] != projectObject["title"] || targetProjectNumber != projectNumber || target["project_owner_login"] != projectObject["owner_login"] || target["project_owner_type"] != projectObject["owner_type"] || target["project_host"] != projectObject["host"] {
		return nil, errors.New("rebalance primitive targets another Project")
	}
	return contract.Object{"issue": issue, "item": item}, nil
}

func sourceTarget(graph, project contract.Object, number int64) (contract.Object, contract.Object, error) {
	var issue, item contract.Object
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, nil, err
	}
	for _, row := range issues {
		n, _ := contract.PositiveInteger(row["number"])
		if n == number {
			issue = row
			break
		}
	}
	items, err := contract.Objects(project, "items")
	if err != nil {
		return nil, nil, err
	}
	for _, row := range items {
		n, _ := contract.PositiveInteger(row["number"])
		if n == number {
			item = row
			break
		}
	}
	if issue == nil || item == nil {
		return nil, nil, errors.New("rebalance primitive target is absent from complete source inventories")
	}
	return issue, item, nil
}

func reviewedRebalanceData(p contract.Plan) (contract.Object, contract.Object, contract.Object, error) {
	if p.Command != RebalanceCommand {
		return nil, nil, nil, errors.New("rebalance adapter does not accept this command")
	}
	if _, ok := p.Sources["issue_graph"]; !ok {
		return nil, nil, nil, errors.New("rebalance plan has no live issue source")
	}
	for _, key := range []string{"issue_graph", "project"} {
		source, err := contract.ObjectAt(p.Sources, key)
		if err != nil || source["live"] != true || source["complete"] != true {
			return nil, nil, nil, fmt.Errorf("rebalance plan lacks complete live %s evidence", key)
		}
	}
	for key := range p.Sources {
		if key != "issue_graph" && key != "project" && key != "linked_pull_requests" {
			return nil, nil, nil, fmt.Errorf("rebalance plan contains unsupported source %q", key)
		}
	}
	build, err := rebalanceBuild(p.Repository, p.Data)
	if err != nil {
		return nil, nil, nil, err
	}
	stored, err := contract.ObjectAt(p.Data, "build")
	if err != nil || !same(stored, build) {
		return nil, nil, nil, errors.New("rebalance derived delta differs from captured reviewed intent")
	}
	graph, _ := contract.ObjectAt(p.Data, "issue_graph")
	project, _ := contract.ObjectAt(p.Data, "project_snapshot")
	if err := compareDataLinkedSource(p); err != nil {
		return nil, nil, nil, err
	}
	return build, graph, project, nil
}

func compareDataLinkedSource(p contract.Plan) error {
	linked, hasEvidence := p.Data["linked_pr_evidence"]
	source, hasSource := p.Sources["linked_pull_requests"]
	if hasEvidence != hasSource {
		return errors.New("rebalance linked-PR evidence and provenance disagree")
	}
	if hasEvidence {
		evidence, ok := linked.(map[string]any)
		if !ok {
			return errors.New("rebalance linked-PR evidence is malformed")
		}
		provenance, ok := source.(map[string]any)
		if !ok || provenance["source"] != "github_api" || provenance["live"] != true || provenance["complete"] != true || provenance["state_filter"] != "all" || !same(provenance["policy"], evidence["policy"]) || !same(provenance["issues"], evidence["issue_numbers"]) {
			return errors.New("rebalance linked-PR source provenance is invalid")
		}
	}
	return nil
}

// RebalanceAdapter implements the durable reviewed-plan workflow. It has no
// repository-wide defaults: the Project and its queue policy are captured in
// the plan, and every fresh read must match that exact identity.
type RebalanceAdapter struct{ Provider RebalanceProvider }

func (a RebalanceAdapter) Operations(p contract.Plan) ([]contract.Operation, error) {
	build, _, _, err := reviewedRebalanceData(p)
	if err != nil {
		return nil, err
	}
	return rebalanceOperations(p.Repository, p.Data, build)
}

func (a RebalanceAdapter) ValidateAcknowledgement(p contract.Plan, op contract.Operation, ack contract.Object) error {
	if err := validateRebalanceReceiptEnvelope(p, op, ack); err != nil {
		return err
	}
	if ack["acknowledged"] != true {
		return errors.New("rebalance acknowledgement is not a typed successful native response")
	}
	provider, err := contract.ObjectAt(ack, "provider_result")
	if err != nil {
		return err
	}
	return validateNativeRebalanceResult(p.Repository, op, ack["operation_id"].(string), provider)
}

func (a RebalanceAdapter) ValidateReceipt(p contract.Plan, op contract.Operation, result contract.Object) error {
	if err := a.ValidateAcknowledgement(p, op, result); err != nil {
		return err
	}
	if result["after_verified"] != true {
		return errors.New("rebalance primitive lacks an independent after-state verification")
	}
	return nil
}

func validateRebalanceReceiptEnvelope(p contract.Plan, op contract.Operation, result contract.Object) error {
	if result["kind"] != op.Kind || result["primitive_id"] != op.ID || !same(result["target"], op.Target) || !same(result["before"], op.Before) || !same(result["after"], op.After) {
		return errors.New("rebalance receipt differs from its reviewed primitive")
	}
	rawRepo, err := contract.ObjectAt(result, "repository")
	if err != nil {
		return err
	}
	repo, err := contract.ParseRepository(rawRepo)
	if err != nil || repo != p.Repository {
		return errors.New("rebalance receipt belongs to another repository or host")
	}
	nonce, err := contract.Nonempty(result, "operation_id")
	if err != nil {
		return err
	}
	_, err = native.OperationMarker(nonce)
	return err
}

func validateNativeRebalanceResult(repo contract.Repository, op contract.Operation, nonce string, provider contract.Object) error {
	switch op.Kind {
	case "project-field":
		if provider["clientMutationId"] != nonce {
			return errors.New("Project field response has no matching client mutation identity")
		}
		item, err := contract.ObjectAt(provider, "projectV2Item")
		if err != nil || item["id"] != op.Target["item_id"] {
			return errors.New("Project field response targets another Project item")
		}
	case "project-item-archive":
		if provider["clientMutationId"] != nonce {
			return errors.New("Project archive response has no matching client mutation identity")
		}
		item, err := contract.ObjectAt(provider, "item")
		if err != nil || item["id"] != op.Target["item_id"] {
			return errors.New("Project archive response targets another Project item")
		}
	case "issue-close":
		n, err := contract.PositiveInteger(provider["number"])
		want, _ := contract.PositiveInteger(op.Target["issue_number"])
		state := strings.ToUpper(fmt.Sprint(provider["state"]))
		if err != nil || n != want || state != "CLOSED" {
			return errors.New("issue close response does not match the reviewed issue and state")
		}
		validator := &native.Transport{Repository: repo}
		if err := validator.ValidateIssueURL(provider["html_url"], want); err != nil {
			return err
		}
		if _, err := contract.PositiveInteger(provider["id"]); err != nil {
			return err
		}
		nodeID, err := contract.Nonempty(provider, "node_id")
		if err != nil || nodeID != op.Target["issue_id"] {
			return errors.New("issue close response has another immutable issue identity")
		}
	default:
		return errors.New("rebalance receipt has an unsupported mutation kind")
	}
	return nil
}

func (a RebalanceAdapter) Preflight(ctx context.Context, p contract.Plan, receipts []contract.Object) error {
	if a.Provider == nil {
		return errors.New("rebalance apply requires a provider")
	}
	_, expectedGraph, expectedProject, err := a.projectedSources(p, receipts, nil)
	if err != nil {
		return err
	}
	scope, err := rebalanceProjectScope(p)
	if err != nil {
		return err
	}
	actualGraph, actualProject, err := a.Provider.ReadSources(ctx, scope)
	if err != nil {
		return err
	}
	actualProject, _, err = validateRebalanceSources(actualGraph, actualProject, p.Repository, scope)
	if err != nil {
		return err
	}
	if !same(rebalanceSourceInventory(expectedGraph, expectedProject), rebalanceSourceInventory(actualGraph, actualProject)) {
		return errors.New("complete reviewed rebalance issue or Project inventory drifted")
	}
	if err := a.preflightLinkedSources(ctx, p); err != nil {
		return err
	}
	return nil
}

func (a RebalanceAdapter) preflightLinkedSources(ctx context.Context, p contract.Plan) error {
	_, exists := p.Data["linked_pr_evidence"]
	if !exists {
		return nil
	}
	evidence, err := contract.ObjectAt(p.Data, "linked_pr_evidence")
	if err != nil {
		return err
	}
	options, err := contract.ObjectAt(p.Data, "options")
	if err != nil {
		return err
	}
	policy, err := contract.ObjectAt(options, "linked_pr_policy")
	if err != nil {
		return err
	}
	numbers := []int64{}
	requested, err := contract.Array(evidence, "issue_numbers")
	if err != nil {
		return err
	}
	for _, rawNumber := range requested {
		n, err := contract.PositiveInteger(rawNumber)
		if err != nil {
			return err
		}
		numbers = append(numbers, n)
	}
	actual, err := a.Provider.ReadLinkedPullRequests(ctx, numbers, policy)
	if err != nil {
		return err
	}
	if err := validateLinkedEvidence(actual, p.Repository, numbers); err != nil {
		return err
	}
	if !same(linkedInventory(evidence), linkedInventory(actual)) {
		return errors.New("complete reviewed linked-PR source inventory drifted")
	}
	return nil
}

func linkedInventory(evidence contract.Object) contract.Object {
	out, _ := contract.Clone(evidence)
	delete(out, "generated_at")
	delete(out, "policy")
	return out
}

func (a RebalanceAdapter) Dispatch(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, error) {
	return a.DispatchAcknowledged(ctx, p, op, nonce, receipts, func(contract.Object) error { return nil })
}

func (a RebalanceAdapter) DispatchAcknowledged(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object, persist func(contract.Object) error) (contract.Object, error) {
	if a.Provider == nil {
		return nil, errors.New("rebalance apply requires a provider")
	}
	if _, err := native.OperationMarker(nonce); err != nil {
		return nil, err
	}
	_, graph, project, err := a.projectedSources(p, receipts, nil)
	if err != nil {
		return nil, err
	}
	if _, err := operationScope(op, graph, project); err != nil {
		return nil, err
	}
	providerResult, err := a.dispatchNative(ctx, p, op, nonce)
	if err != nil {
		return nil, err
	}
	ack := rebalanceReceipt(p, op, nonce, providerResult)
	if err := a.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, err
	}
	if persist == nil {
		return nil, errors.New("durable native acknowledgement callback is required")
	}
	if err := persist(ack); err != nil {
		return nil, err
	}
	_, expectedGraph, expectedProject, err := a.projectedSources(p, receipts, &op)
	if err != nil {
		return nil, err
	}
	scope, err := rebalanceProjectScope(p)
	if err != nil {
		return nil, err
	}
	actualGraph, actualProject, err := a.Provider.ReadSources(ctx, scope)
	if err != nil {
		return nil, err
	}
	actualProject, _, err = validateRebalanceSources(actualGraph, actualProject, p.Repository, scope)
	if err != nil {
		return nil, err
	}
	if !same(rebalanceSourceInventory(expectedGraph, expectedProject), rebalanceSourceInventory(actualGraph, actualProject)) {
		return nil, errors.New("rebalance native response was acknowledged but its full after-state did not verify")
	}
	if err := a.preflightLinkedSources(ctx, p); err != nil {
		return nil, err
	}
	ack["after_verified"] = true
	return ack, nil
}

func (a RebalanceAdapter) dispatchNative(ctx context.Context, p contract.Plan, op contract.Operation, nonce string) (contract.Object, error) {
	target := op.Target
	number, err := contract.PositiveInteger(target["issue_number"])
	if err != nil {
		return nil, err
	}
	scope, err := parseRebalanceProjectScope(targetProjectScope(op, p.Repository))
	if err != nil {
		return nil, err
	}
	reviewedScope, err := rebalanceProjectScope(p)
	if err != nil {
		return nil, err
	}
	if scope != reviewedScope {
		return nil, errors.New("rebalance primitive Project identity differs from the reviewed scope")
	}
	item := native.ProjectItemScope{Project: scope, ItemID: mustString(target["item_id"]), IssueNumber: number}
	switch op.Kind {
	case "project-field":
		name, err := contract.Nonempty(target, "field_name")
		if err != nil {
			return nil, err
		}
		present, err := contract.Bool(op.After, "present")
		if err != nil || !present {
			return nil, errors.New("rebalance field writes must set an explicit consumer value")
		}
		value, exists := op.After["value"]
		if !exists || value == nil {
			return nil, errors.New("rebalance field writes cannot infer an unset value")
		}
		if name == "" {
			return nil, errors.New("rebalance field write has no field name")
		}
		return a.Provider.SetProjectField(ctx, item, mustString(target["field_id"]), value, nonce)
	case "issue-close":
		return a.Provider.UpdateIssue(ctx, number, contract.Object{"state": "closed"})
	case "project-item-archive":
		return a.Provider.ArchiveProjectItem(ctx, item, true, nonce)
	default:
		return nil, errors.New("rebalance operation kind is unsupported")
	}
}

func targetProjectScope(op contract.Operation, repo contract.Repository) contract.Object {
	return contract.Object{"host": repo.Host, "owner_login": op.Target["project_owner_login"], "owner_type": op.Target["project_owner_type"], "number": op.Target["project_number"], "id": op.Target["project_id"]}
}

func rebalanceProjectScope(p contract.Plan) (native.ProjectScope, error) {
	raw, err := contract.ObjectAt(p.Data, "project_scope")
	if err != nil {
		return native.ProjectScope{}, err
	}
	scope, err := parseRebalanceProjectScope(raw)
	if err != nil || scope.Host != p.Repository.Host {
		return native.ProjectScope{}, errors.New("rebalance Project scope is invalid for its repository")
	}
	return scope, nil
}

func mustString(raw any) string {
	value, _ := raw.(string)
	return value
}

func rebalanceReceipt(p contract.Plan, op contract.Operation, nonce string, providerResult contract.Object) contract.Object {
	return contract.Object{"kind": op.Kind, "primitive_id": op.ID, "repository": p.Repository.Object(), "operation_id": nonce, "target": op.Target, "before": op.Before, "after": op.After, "acknowledged": true, "provider_result": providerResult}
}

func (a RebalanceAdapter) Observe(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, contract.Object, error) {
	if a.Provider == nil {
		return nil, nil, errors.New("rebalance observation requires a provider")
	}
	var ack contract.Object
	for _, receipt := range receipts {
		if receipt["id"] == op.ID && receipt["status"] == "unknown" && receipt["operation_id"] == nonce {
			if raw, exists := receipt["acknowledgement"]; exists {
				ack, _ = raw.(map[string]any)
			}
		}
	}
	if ack == nil {
		return nil, nil, errors.New("ambiguous rebalance write has no durable typed native acknowledgement; no retry was attempted")
	}
	if err := a.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, nil, err
	}
	_, expectedGraph, expectedProject, err := a.projectedSources(p, receipts, &op)
	if err != nil {
		return nil, nil, err
	}
	scope, err := rebalanceProjectScope(p)
	if err != nil {
		return nil, nil, err
	}
	actualGraph, actualProject, err := a.Provider.ReadSources(ctx, scope)
	if err != nil {
		return nil, nil, err
	}
	actualProject, _, err = validateRebalanceSources(actualGraph, actualProject, p.Repository, scope)
	if err != nil {
		return nil, nil, err
	}
	if !same(rebalanceSourceInventory(expectedGraph, expectedProject), rebalanceSourceInventory(actualGraph, actualProject)) {
		return nil, nil, errors.New("durably acknowledged rebalance write does not match the complete expected after-state")
	}
	if err := a.preflightLinkedSources(ctx, p); err != nil {
		return nil, nil, err
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

func (a RebalanceAdapter) projectedSources(p contract.Plan, receipts []contract.Object, current *contract.Operation) (contract.Object, contract.Object, contract.Object, error) {
	build, graph, project, err := reviewedRebalanceData(p)
	if err != nil {
		return nil, nil, nil, err
	}
	graph, err = contract.Clone(graph)
	if err != nil {
		return nil, nil, nil, err
	}
	project, err = contract.Clone(project)
	if err != nil {
		return nil, nil, nil, err
	}
	ops, err := rebalanceOperations(p.Repository, p.Data, build)
	if err != nil {
		return nil, nil, nil, err
	}
	byID := map[string]contract.Operation{}
	for _, op := range ops {
		byID[op.ID] = op
	}
	for _, receipt := range receipts {
		if receipt["status"] != "completed" {
			continue
		}
		id, err := contract.Nonempty(receipt, "id")
		if err != nil {
			return nil, nil, nil, err
		}
		op, exists := byID[id]
		if !exists {
			return nil, nil, nil, errors.New("rebalance journal contains an operation outside reviewed intent")
		}
		result, err := contract.ObjectAt(receipt, "result")
		if err != nil || a.ValidateReceipt(p, op, result) != nil {
			return nil, nil, nil, errors.New("rebalance journal completion is not a valid typed receipt")
		}
		if err := operationBeforeMatches(graph, project, op); err != nil {
			return nil, nil, nil, err
		}
		if err := projectRebalanceAfter(graph, project, op); err != nil {
			return nil, nil, nil, err
		}
	}
	if current != nil {
		if err := operationBeforeMatches(graph, project, *current); err != nil {
			return nil, nil, nil, err
		}
		if err := projectRebalanceAfter(graph, project, *current); err != nil {
			return nil, nil, nil, err
		}
	}
	return build, graph, project, nil
}

func operationBeforeMatches(graph, project contract.Object, op contract.Operation) error {
	state, err := currentOperationScope(graph, project, op)
	if err != nil {
		return err
	}
	for key, value := range op.Before {
		if actual, exists := state[key]; !exists || !same(actual, value) {
			return fmt.Errorf("rebalance primitive %s before-state differs from its projected source", op.ID)
		}
	}
	return nil
}

func currentOperationScope(graph, project contract.Object, op contract.Operation) (contract.Object, error) {
	pair, err := operationScope(op, graph, project)
	if err != nil {
		return nil, err
	}
	issue, item := pair["issue"].(map[string]any), pair["item"].(map[string]any)
	fields, _ := contract.ObjectAt(item, "field_values")
	state := contract.Object{"issue_state": issue["state"], "archived": item["archived"]}
	switch op.Kind {
	case "project-field":
		name, _ := contract.Nonempty(op.Target, "field_name")
		state["present"] = fields[name] != nil
		if value, exists := fields[name]; exists {
			state["present"], state["value"] = true, value
		} else {
			state["present"], state["value"] = false, nil
		}
	case "issue-close":
		state["state"] = issue["state"]
		state["project_state"] = item["state"]
	case "project-item-archive":
		state["issue_state"] = issue["state"]
		state["status"] = fieldEvidence(item, mustString(op.Target["status_field"]))
	}
	return state, nil
}

func projectRebalanceAfter(graph, project contract.Object, op contract.Operation) error {
	number, err := contract.PositiveInteger(op.Target["issue_number"])
	if err != nil {
		return err
	}
	issue, item, err := sourceTarget(graph, project, number)
	if err != nil {
		return err
	}
	switch op.Kind {
	case "project-field":
		name, _ := contract.Nonempty(op.Target, "field_name")
		present, _ := contract.Bool(op.After, "present")
		if !present {
			delete(objectOrEmpty(item, "field_values"), name)
			delete(objectOrEmpty(issue, "field_values"), name)
			itemProjection := objectOrEmpty(issue, "project_item")
			delete(objectOrEmpty(itemProjection, "field_values"), name)
		} else {
			value := op.After["value"]
			objectOrEmpty(item, "field_values")[name] = value
			objectOrEmpty(issue, "field_values")[name] = value
			objectOrEmpty(objectOrEmpty(issue, "project_item"), "field_values")[name] = value
		}
	case "issue-close":
		issue["state"] = "CLOSED"
		item["state"] = "CLOSED"
	case "project-item-archive":
		item["archived"] = true
		objectOrEmpty(issue, "project_item")["archived"] = true
	default:
		return errors.New("unsupported rebalance projected primitive")
	}
	return nil
}

func rebalanceSourceInventory(graph, project contract.Object) contract.Object {
	cleanGraph, _ := contract.Clone(graph)
	cleanProject, _ := contract.Clone(project)
	delete(cleanGraph, "generated_at")
	delete(cleanProject, "generated_at")
	issues, _ := contract.Objects(cleanGraph, "issues")
	sort.Slice(issues, func(i, j int) bool {
		a, _ := contract.PositiveInteger(issues[i]["number"])
		b, _ := contract.PositiveInteger(issues[j]["number"])
		return a < b
	})
	issueValues := make([]any, len(issues))
	for index, issue := range issues {
		issueValues[index] = issue
	}
	cleanGraph["issues"] = issueValues
	items, _ := contract.Objects(cleanProject, "items")
	sort.Slice(items, func(i, j int) bool {
		a, _ := contract.PositiveInteger(items[i]["number"])
		b, _ := contract.PositiveInteger(items[j]["number"])
		return a < b
	})
	itemValues := make([]any, len(items))
	for index, item := range items {
		itemValues[index] = item
	}
	cleanProject["items"] = itemValues
	return contract.Object{"issue_graph": cleanGraph, "project_snapshot": cleanProject}
}

func (a RebalanceAdapter) OperationsResult(p contract.Plan) (contract.Object, error) {
	build, _, _, err := reviewedRebalanceData(p)
	if err != nil {
		return nil, err
	}
	return build, nil
}

func (a RebalanceAdapter) OperationsForPlan(p contract.Plan) ([]contract.Operation, error) {
	return a.Operations(p)
}

var _ apply.Adapter = RebalanceAdapter{}
var _ apply.AcknowledgingAdapter = RebalanceAdapter{}
var _ RebalanceProvider = NativeRebalanceProvider{}
