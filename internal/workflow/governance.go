package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/governance"
	"github.com/coreycoto/gh-steward/internal/native"
)

const GovernanceCommand = "governance-apply"

const (
	GovernanceBootstrapBacklog  = "bootstrap-backlog"
	GovernanceFix               = "governance-fix"
	GovernanceLabelPalette      = "label-palette"
	GovernanceDriftIssue        = "drift-issue"
	GovernanceExecutionState    = "execution-state"
	governanceSchemaVersion     = int64(1)
	governanceAuditMarkerPrefix = "<!-- gh-steward:governance:"
	governanceLegacyAuditMarker = "<!-- gh-steward:governance-audit -->"
)

// LabelDraft is the closed input accepted by native repository label creation.
type LabelDraft struct{ Name, Color, Description string }

// GovernanceProvider combines complete backlog snapshots with the small set of
// retained governance mutations. Implementations return exact provider ACKs.
type GovernanceProvider interface {
	BacklogProvider
	CreateLabel(context.Context, string, LabelDraft) (contract.Object, error)
	UpdateLabel(context.Context, string, string, string, string, string) (contract.Object, error)
	SetIssueState(context.Context, string, int64, string) (contract.Object, error)
}

type governancePolicy struct {
	project             *ProjectScope
	typedLabels         map[string]string
	selectedIssues      map[int64]bool
	statusField         string
	targetStatus        string
	validStatuses       map[string]bool
	governanceLabels    map[string]bool
	retiredLabels       map[string]bool
	milestones          []MilestoneDraft
	palette             contract.Object
	driftTitle          string
	driftBody           string
	driftMaintenance    string
	driftSummary        string
	driftUnresolved     bool
	driftCloseWhenClean bool
	driftTodoStatus     string
	driftDoneStatus     string
	driftPriorityField  string
	driftPriorityValue  string
	driftQueueField     string
	driftQueueStep      float64
	executionIssue      int64
	executionIssueState string
	executionWorkMarker string
	executionWorkBody   string
}

// PrepareGovernance captures the exact all-state repository evidence used to
// derive a typed governance plan. Consumer policy and defaults are explicit
// inputs; no repository or Project naming convention is embedded here.
func PrepareGovernance(ctx context.Context, provider GovernanceProvider, repo contract.Repository, kind string, policy contract.Object, now time.Time) (contract.Plan, error) {
	if provider == nil {
		return contract.Plan{}, errors.New("governance preparation requires a provider")
	}
	if kind != GovernanceBootstrapBacklog && kind != GovernanceFix && kind != GovernanceLabelPalette && kind != GovernanceDriftIssue && kind != GovernanceExecutionState {
		return contract.Plan{}, fmt.Errorf("unsupported governance kind %q", kind)
	}
	normalizedPolicy, parsed, err := parseGovernancePolicy(kind, policy, repo)
	if err != nil {
		return contract.Plan{}, err
	}
	capturedAt := now.UTC().Format(time.RFC3339Nano)
	request, err := governanceInventoryRequest(kind, normalizedPolicy, parsed, repo, capturedAt)
	if err != nil {
		return contract.Plan{}, err
	}
	raw, err := provider.BacklogInventory(ctx, request)
	if err != nil {
		return contract.Plan{}, err
	}
	inventory, err := normalizeGovernanceInventory(raw, repo, request)
	if err != nil {
		return contract.Plan{}, err
	}
	data := contract.Object{"kind": kind, "policy": normalizedPolicy, "inventory_request": request.Object(), "inventory": inventory}
	sources := governanceSources(inventory, request)
	plan := contract.Plan{Command: GovernanceCommand, Repository: repo, Data: data, Sources: sources, CapturedAt: capturedAt}
	operations, err := governanceOperations(plan)
	if err != nil {
		return contract.Plan{}, err
	}
	return contract.PreparePlan(GovernanceCommand, repo, sources, data, operations, now)
}

func parseGovernancePolicy(kind string, raw contract.Object, repo contract.Repository) (contract.Object, governancePolicy, error) {
	var out governancePolicy
	if raw == nil {
		return nil, out, errors.New("governance policy is required")
	}
	allowed := map[string]bool{}
	switch kind {
	case GovernanceBootstrapBacklog:
		allowed = keySet("project", "typed_issue_labels", "issue_numbers", "valid_statuses", "status_field", "target_status")
	case GovernanceFix:
		allowed = keySet("governance_label_by_prefix", "governance_labels", "retired_labels", "milestones")
	case GovernanceLabelPalette:
		allowed = keySet("labels")
	case GovernanceDriftIssue:
		allowed = keySet("title", "body", "maintenance_label", "summary_markdown", "unresolved_manual_drift", "close_when_clean", "project", "status_field", "todo_status", "done_status", "priority_field", "priority_value", "queue_order_field", "queue_order_step")
	case GovernanceExecutionState:
		allowed = keySet("issue_number", "issue_state", "project", "status_field", "target_status", "work_comment")
	}
	if !sameKeys(raw, allowed) {
		return nil, out, fmt.Errorf("%s policy has unsupported or missing fields", kind)
	}
	copyRaw, err := contract.Clone(raw)
	if err != nil {
		return nil, out, err
	}
	switch kind {
	case GovernanceBootstrapBacklog:
		projectRaw, err := contract.ObjectAt(raw, "project")
		if err != nil {
			return nil, out, err
		}
		project, err := backlogParseProjectScope(projectRaw)
		if err != nil {
			return nil, out, err
		}
		if err = validateProjectScope(project, repo); err != nil {
			return nil, out, err
		}
		out.project = &project
		out.typedLabels, err = governanceStringMap(raw, "typed_issue_labels", true)
		if err != nil {
			return nil, out, err
		}
		for prefix := range out.typedLabels {
			if strings.TrimSpace(prefix) != prefix || prefix == "" || strings.Contains(prefix, ":") {
				return nil, out, errors.New("typed issue label prefixes must be nonempty exact prefixes without colons")
			}
		}
		issueValues, err := contract.Array(raw, "issue_numbers")
		if err != nil {
			return nil, out, err
		}
		out.selectedIssues = map[int64]bool{}
		for _, value := range issueValues {
			n, err := contract.PositiveInteger(value)
			if err != nil || out.selectedIssues[n] {
				return nil, out, errors.New("issue_numbers must contain unique positive integers")
			}
			out.selectedIssues[n] = true
		}
		statuses, err := contract.Strings(raw["valid_statuses"])
		if err != nil || len(statuses) == 0 {
			return nil, out, errors.New("valid_statuses must be a nonempty string array")
		}
		out.validStatuses = map[string]bool{}
		for _, status := range statuses {
			if strings.TrimSpace(status) == "" || out.validStatuses[status] {
				return nil, out, errors.New("valid_statuses must be unique nonempty strings")
			}
			out.validStatuses[status] = true
		}
		out.statusField, err = contract.Nonempty(raw, "status_field")
		if err != nil {
			return nil, out, err
		}
		out.targetStatus, err = contract.Nonempty(raw, "target_status")
		if err != nil || !out.validStatuses[out.targetStatus] {
			return nil, out, errors.New("target_status must be included in valid_statuses")
		}
	case GovernanceFix:
		out.typedLabels, err = governanceStringMap(raw, "governance_label_by_prefix", true)
		if err != nil {
			return nil, out, err
		}
		out.governanceLabels, err = governanceStringSet(raw, "governance_labels", false)
		if err != nil {
			return nil, out, err
		}
		out.retiredLabels, err = governanceStringSet(raw, "retired_labels", true)
		if err != nil {
			return nil, out, err
		}
		for prefix, label := range out.typedLabels {
			if prefix == "" || strings.TrimSpace(prefix) != prefix || strings.Contains(prefix, ":") || !out.governanceLabels[label] {
				return nil, out, errors.New("governance_label_by_prefix must map exact prefixes to listed governance labels")
			}
		}
		rows, err := contract.Objects(raw, "milestones")
		if err != nil || len(rows) != 2 {
			return nil, out, errors.New("governance-fix policy requires exactly the current and next milestone targets")
		}
		seen := map[string]bool{}
		for _, row := range rows {
			if !sameKeys(row, keySet("title", "due_on", "description")) {
				return nil, out, errors.New("milestone target must contain only title, due_on and description")
			}
			title, err := contract.Nonempty(row, "title")
			if err != nil || seen[title] {
				return nil, out, errors.New("milestone target titles must be unique and nonempty")
			}
			seen[title] = true
			dueOn, err := contract.Nonempty(row, "due_on")
			if err != nil {
				return nil, out, err
			}
			stamp, err := time.Parse(time.RFC3339Nano, dueOn)
			if err != nil {
				return nil, out, errors.New("milestone due_on must be RFC3339")
			}
			description, err := contract.String(row, "description")
			if err != nil || strings.Contains(description, "<!-- gh-steward:operation:") {
				return nil, out, errors.New("milestone description must be text without reserved operation markers")
			}
			out.milestones = append(out.milestones, MilestoneDraft{Title: title, DueOn: stamp.UTC().Format(time.RFC3339Nano), Description: description})
		}
		sort.Slice(out.milestones, func(i, j int) bool { return out.milestones[i].Title < out.milestones[j].Title })
	case GovernanceLabelPalette:
		rows, err := contract.Objects(raw, "labels")
		if err != nil || len(rows) == 0 {
			return nil, out, errors.New("label-palette policy requires a nonempty preferred labels array")
		}
		for _, row := range rows {
			allowedEntry := keySet("name", "semantic_role", "platform_owner", "governance_controlled", "target_color", "current_reference_color", "description", "notes")
			for key := range row {
				if !allowedEntry[key] {
					return nil, out, fmt.Errorf("label palette entry has unsupported field %q", key)
				}
			}
			for _, key := range []string{"name", "semantic_role", "platform_owner", "target_color", "current_reference_color", "description"} {
				if _, err := contract.Nonempty(row, key); err != nil {
					return nil, out, err
				}
			}
			if _, err := contract.Bool(row, "governance_controlled"); err != nil {
				return nil, out, err
			}
		}
		out.palette = contract.Object{"labels": copyRaw["labels"]}
	case GovernanceDriftIssue:
		out.driftTitle, err = contract.Nonempty(raw, "title")
		if err != nil {
			return nil, out, err
		}
		out.driftBody, err = contract.String(raw, "body")
		if err != nil || strings.TrimSpace(out.driftBody) == "" || strings.Contains(out.driftBody, "<!-- gh-steward:operation:") || containsGovernanceAuditMarker(out.driftBody) {
			return nil, out, errors.New("drift issue body must be nonempty text without reserved operation markers")
		}
		out.driftMaintenance, err = contract.Nonempty(raw, "maintenance_label")
		if err != nil {
			return nil, out, err
		}
		out.driftSummary, err = contract.String(raw, "summary_markdown")
		if err != nil || strings.Contains(out.driftSummary, "<!-- gh-steward:operation:") || containsGovernanceAuditMarker(out.driftSummary) {
			return nil, out, errors.New("summary_markdown must be text without reserved operation markers")
		}
		out.driftUnresolved, err = contract.Bool(raw, "unresolved_manual_drift")
		if err != nil {
			return nil, out, err
		}
		out.driftCloseWhenClean, err = contract.Bool(raw, "close_when_clean")
		if err != nil {
			return nil, out, err
		}
		if raw["project"] != nil {
			projectRaw, err := contract.ObjectAt(raw, "project")
			if err != nil {
				return nil, out, errors.New("drift project must be null or an exact Project scope")
			}
			project, err := backlogParseProjectScope(projectRaw)
			if err != nil {
				return nil, out, err
			}
			if err = validateProjectScope(project, repo); err != nil {
				return nil, out, err
			}
			out.project = &project
		}
		out.statusField, err = contract.Nonempty(raw, "status_field")
		if err != nil {
			return nil, out, err
		}
		out.driftTodoStatus, err = contract.Nonempty(raw, "todo_status")
		if err != nil {
			return nil, out, err
		}
		out.driftDoneStatus, err = contract.Nonempty(raw, "done_status")
		if err != nil || out.driftDoneStatus == out.driftTodoStatus {
			return nil, out, errors.New("drift todo_status and done_status must be distinct")
		}
		out.driftPriorityField, err = contract.Nonempty(raw, "priority_field")
		if err != nil {
			return nil, out, err
		}
		out.driftPriorityValue, err = contract.Nonempty(raw, "priority_value")
		if err != nil {
			return nil, out, err
		}
		out.driftQueueField, err = contract.Nonempty(raw, "queue_order_field")
		if err != nil {
			return nil, out, err
		}
		out.driftQueueStep, err = contract.Number(raw["queue_order_step"])
		if err != nil || math.IsNaN(out.driftQueueStep) || math.IsInf(out.driftQueueStep, 0) || out.driftQueueStep <= 0 {
			return nil, out, errors.New("queue_order_step must be a finite positive number")
		}
	case GovernanceExecutionState:
		out.executionIssue, err = contract.PositiveInteger(raw["issue_number"])
		if err != nil {
			return nil, out, errors.New("execution-state issue_number must be positive")
		}
		out.executionIssueState, err = contract.Nonempty(raw, "issue_state")
		if err != nil || (out.executionIssueState != "OPEN" && out.executionIssueState != "CLOSED") {
			return nil, out, errors.New("execution-state issue_state must be OPEN or CLOSED")
		}
		if raw["project"] != nil {
			projectRaw, err := contract.ObjectAt(raw, "project")
			if err != nil {
				return nil, out, errors.New("execution-state project must be null or an exact Project scope")
			}
			project, err := backlogParseProjectScope(projectRaw)
			if err != nil {
				return nil, out, err
			}
			if err = validateProjectScope(project, repo); err != nil {
				return nil, out, err
			}
			out.project = &project
		}
		out.statusField, err = contract.Nonempty(raw, "status_field")
		if err != nil {
			return nil, out, err
		}
		out.targetStatus, err = contract.Nonempty(raw, "target_status")
		if err != nil {
			return nil, out, err
		}
		if raw["work_comment"] != nil {
			comment, err := contract.ObjectAt(raw, "work_comment")
			if err != nil || !sameKeys(comment, keySet("marker", "body")) {
				return nil, out, errors.New("execution-state work_comment must be null or contain exactly marker and body")
			}
			out.executionWorkMarker, err = contract.Nonempty(comment, "marker")
			if err != nil || strings.ContainsAny(out.executionWorkMarker, "\r\n") || strings.Contains(out.executionWorkMarker, "<!-- gh-steward:operation:") {
				return nil, out, errors.New("execution-state work marker must be a single-line nonreserved identity")
			}
			out.executionWorkBody, err = contract.Nonempty(comment, "body")
			if err != nil || strings.Contains(out.executionWorkBody, "<!-- gh-steward:operation:") || strings.Contains(out.executionWorkBody, out.executionWorkMarker) {
				return nil, out, errors.New("execution-state work body must be nonempty text without reserved markers")
			}
		}
	}
	return copyRaw, out, nil
}

func governanceInventoryRequest(kind string, raw contract.Object, policy governancePolicy, repo contract.Repository, capturedAt string) (BacklogInventoryRequest, error) {
	request := BacklogInventoryRequest{}
	if (kind == GovernanceBootstrapBacklog || kind == GovernanceDriftIssue || kind == GovernanceExecutionState) && policy.project != nil {
		request.Projects = []ProjectScope{*policy.project}
	}
	if kind == GovernanceDriftIssue {
		marker, err := governanceMarker(repo, kind, raw, capturedAt)
		if err != nil {
			return request, err
		}
		request.CommentMarkers = []string{marker}
	}
	if kind == GovernanceExecutionState && policy.executionWorkMarker != "" {
		request.CommentMarkers = []string{policy.executionWorkMarker}
	}
	if err := validateInventoryRequest(request); err != nil {
		return request, err
	}
	return request, nil
}

func governanceSources(inventory contract.Object, request BacklogInventoryRequest) contract.Object {
	return backlogPlanSources(inventory, request)
}

func normalizeGovernanceInventory(raw contract.Object, repo contract.Repository, request BacklogInventoryRequest) (contract.Object, error) {
	canonical, err := normalizeBacklogInventory(raw, repo, request)
	if err != nil {
		return nil, err
	}
	return normalizeGovernanceCanonicalInventory(canonical)
}

func validateStoredGovernanceInventory(raw contract.Object, repo contract.Repository, request BacklogInventoryRequest) (contract.Object, error) {
	labels, err := governanceStoredLabels(raw)
	if err != nil {
		return nil, err
	}
	validationCopy, err := contract.Clone(raw)
	if err != nil {
		return nil, err
	}
	// The common inventory validator owns issue, milestone, Project, comment,
	// relationship and provenance checks. Labels are independently validated
	// below under the GraphQL-global-ID contract (no REST database ID required).
	validationCopy["labels"] = []any{}
	canonical, err := validateStoredBacklogInventory(validationCopy, repo, request)
	if err != nil {
		return nil, err
	}
	canonical["labels"] = labels
	return normalizeGovernanceCanonicalInventory(canonical)
}

func normalizeGovernanceCanonicalInventory(inventory contract.Object) (contract.Object, error) {
	out, err := contract.Clone(inventory)
	if err != nil {
		return nil, err
	}
	graph, err := contract.ObjectAt(out, "issue_inventory")
	if err != nil {
		return nil, err
	}
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		body, _ := contract.String(issue, "body")
		issue["body"] = stripOperationMarker(body)
	}
	graph["issues"] = backlogObjectsAsAny(issues)
	milestones, err := contract.Objects(out, "milestones")
	if err != nil {
		return nil, err
	}
	for _, milestone := range milestones {
		description, _ := contract.String(milestone, "description")
		milestone["description"] = stripOperationMarker(description)
	}
	comments, err := contract.Objects(out, "comments")
	if err != nil {
		return nil, err
	}
	for _, comment := range comments {
		body, _ := contract.String(comment, "body")
		comment["body"] = stripOperationMarker(body)
	}
	labels, err := governanceCanonicalLabels(out)
	if err != nil {
		return nil, err
	}
	out["issue_inventory"], out["milestones"], out["comments"], out["labels"] = graph, backlogObjectsAsAny(milestones), backlogObjectsAsAny(comments), labels
	return out, nil
}

func governanceStoredLabels(inventory contract.Object) ([]any, error) {
	rows, err := contract.Objects(inventory, "labels")
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	seenNames, seenIDs := map[string]bool{}, map[string]bool{}
	for _, row := range rows {
		if len(row) != 4 {
			return nil, errors.New("governance labels must contain only global identity, name, color and description")
		}
		node, err := contract.Nonempty(row, "node_id")
		if err != nil || seenIDs[node] {
			return nil, errors.New("governance label global IDs must be present and unique")
		}
		name, err := contract.Nonempty(row, "name")
		if err != nil || seenNames[name] {
			return nil, errors.New("governance label names must be present and unique")
		}
		color, err := contract.Nonempty(row, "color")
		if err != nil || len(color) != 6 {
			return nil, errors.New("governance label color must be six hexadecimal digits")
		}
		if _, err = strconv.ParseUint(color, 16, 24); err != nil {
			return nil, errors.New("governance label color must be six hexadecimal digits")
		}
		description, err := contract.String(row, "description")
		if err != nil {
			return nil, err
		}
		seenIDs[node], seenNames[name] = true, true
		out = append(out, contract.Object{"node_id": node, "name": name, "color": strings.ToLower(color), "description": description})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].(map[string]any)["name"].(string) < out[j].(map[string]any)["name"].(string)
	})
	return out, nil
}

func governanceCanonicalLabels(inventory contract.Object) ([]any, error) {
	rows, err := contract.Objects(inventory, "labels")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || len(rows[0]) == 4 {
		return governanceStoredLabels(inventory)
	}
	labels, err := normalizeLabels(rows)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(labels))
	for _, value := range labels {
		row := value.(contract.Object)
		out = append(out, contract.Object{"node_id": row["node_id"], "name": row["name"], "color": row["color"], "description": row["description"]})
	}
	return governanceStoredLabels(contract.Object{"labels": out})
}

func governanceRawLabels(raw contract.Object, repo contract.Repository) ([]any, error) {
	rows, err := contract.Objects(raw, "labels")
	if err != nil {
		return nil, err
	}
	labels, err := normalizeLabels(rows)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(labels))
	for _, value := range labels {
		row := value.(contract.Object)
		out = append(out, contract.Object{"node_id": row["node_id"], "name": row["name"], "color": row["color"], "description": row["description"]})
	}
	return out, nil
}

func governanceStringMap(raw contract.Object, key string, nonempty bool) (map[string]string, error) {
	values, err := contract.ObjectAt(raw, key)
	if err != nil || (nonempty && len(values) == 0) {
		return nil, fmt.Errorf("%s must be a nonempty string map", key)
	}
	out := make(map[string]string, len(values))
	for name, value := range values {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%s keys and values must be nonempty strings", key)
		}
		out[name] = text
	}
	return out, nil
}

func governanceStringSet(raw contract.Object, key string, allowEmpty bool) (map[string]bool, error) {
	values, err := contract.Strings(raw[key])
	if err != nil || (!allowEmpty && len(values) == 0) {
		return nil, fmt.Errorf("%s must be a string array", key)
	}
	out := map[string]bool{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || out[value] {
			return nil, fmt.Errorf("%s entries must be unique nonempty strings", key)
		}
		out[value] = true
	}
	return out, nil
}

func keySet(keys ...string) map[string]bool {
	out := map[string]bool{}
	for _, key := range keys {
		out[key] = true
	}
	return out
}
func sameKeys(raw contract.Object, allowed map[string]bool) bool {
	if len(raw) != len(allowed) {
		return false
	}
	for key := range raw {
		if !allowed[key] {
			return false
		}
	}
	return true
}

func governanceMarker(repo contract.Repository, kind string, policy contract.Object, captured string) (string, error) {
	material := contract.Object{"repository": repo.Object(), "kind": kind, "policy": policy, "captured_at": captured}
	digest, err := contract.Digest(material)
	if err != nil {
		return "", err
	}
	return "<!-- gh-steward:governance:" + digest[:24] + " -->", nil
}

func governanceRequestFromPlan(p contract.Plan) (BacklogInventoryRequest, error) {
	kind, policy, err := governancePlanPolicy(p)
	if err != nil {
		return BacklogInventoryRequest{}, err
	}
	requestRaw, err := contract.ObjectAt(p.Data, "inventory_request")
	if err != nil {
		return BacklogInventoryRequest{}, err
	}
	request, err := parseInventoryRequest(requestRaw)
	if err != nil {
		return request, err
	}
	_, parsed, err := parseGovernancePolicy(kind, policy, p.Repository)
	if err != nil {
		return request, err
	}
	want, err := governanceInventoryRequest(kind, policy, parsed, p.Repository, p.CapturedAt)
	if err != nil || !same(request.Object(), want.Object()) {
		return request, errors.New("governance inventory request differs from typed policy")
	}
	return request, nil
}

func governancePlanPolicy(p contract.Plan) (string, contract.Object, error) {
	if p.Command != GovernanceCommand || !sameKeys(p.Data, keySet("kind", "policy", "inventory_request", "inventory")) {
		return "", nil, errors.New("governance plan has unsupported or missing data")
	}
	kind, err := contract.Nonempty(p.Data, "kind")
	if err != nil {
		return "", nil, err
	}
	policy, err := contract.ObjectAt(p.Data, "policy")
	if err != nil {
		return "", nil, err
	}
	return kind, policy, nil
}

func governanceOperations(p contract.Plan) ([]contract.Operation, error) {
	kind, rawPolicy, err := governancePlanPolicy(p)
	if err != nil {
		return nil, err
	}
	policyRaw, policy, err := parseGovernancePolicy(kind, rawPolicy, p.Repository)
	if err != nil {
		return nil, err
	}
	request, err := governanceRequestFromPlan(p)
	if err != nil {
		return nil, err
	}
	inventoryRaw, err := contract.ObjectAt(p.Data, "inventory")
	if err != nil {
		return nil, err
	}
	inventory, err := validateStoredGovernanceInventory(inventoryRaw, p.Repository, request)
	if err != nil {
		return nil, err
	}
	if !same(inventory, inventoryRaw) || !same(p.Sources, governanceSources(inventory, request)) {
		return nil, errors.New("governance sources differ from canonical captured inventory")
	}
	return deriveGovernanceOperations(kind, policyRaw, policy, inventory, p.Repository, p.CapturedAt)
}

func deriveGovernanceOperations(kind string, raw contract.Object, policy governancePolicy, inventory contract.Object, repo contract.Repository, captured string) ([]contract.Operation, error) {
	switch kind {
	case GovernanceBootstrapBacklog:
		return governanceBootstrapOperations(policy, inventory)
	case GovernanceFix:
		return governanceFixOperations(policy, inventory)
	case GovernanceLabelPalette:
		return governanceLabelOperations(policy, inventory)
	case GovernanceDriftIssue:
		return governanceDriftOperations(policy, inventory, repo, raw, captured)
	case GovernanceExecutionState:
		return governanceExecutionStateOperations(policy, inventory, policy.executionWorkBody)
	default:
		return nil, fmt.Errorf("unsupported governance kind %q", kind)
	}
}

func governanceExecutionStateOperations(policy governancePolicy, inventory contract.Object, workCommentBody string) ([]contract.Operation, error) {
	graph, err := contract.ObjectAt(inventory, "issue_inventory")
	if err != nil {
		return nil, err
	}
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	issue := backlogIssueByNumber(issues, policy.executionIssue)
	if issue == nil {
		return nil, fmt.Errorf("execution-state issue #%d is absent from complete all-state inventory", policy.executionIssue)
	}
	ops := []contract.Operation{}
	if issue["state"] != policy.executionIssueState {
		ops = append(ops, governanceStateOperation(issue, policy.executionIssueState))
		ops[len(ops)-1].ID = fmt.Sprintf("execution-state:issue:%d", policy.executionIssue)
	}
	if policy.project != nil {
		project, err := governanceProject(inventory, *policy.project)
		if err != nil {
			return nil, err
		}
		if err = governanceValidateProjectOption(project, policy.statusField, policy.targetStatus); err != nil {
			return nil, err
		}
		item := governanceProjectItem(project, policy.executionIssue)
		if item != nil && item["archived"] == true {
			return nil, errors.New("execution-state Project item is archived and cannot be safely synchronized")
		}
		itemID := ""
		var current any
		membershipID := "execution-state:project-membership"
		if item == nil {
			ops = append(ops, contract.Operation{ID: membershipID, Kind: "project-membership-add", Target: contract.Object{"project_id": policy.project.ID, "issue_number": policy.executionIssue, "issue_node_id": issue["id"]}, Before: contract.Object{"present": false}, After: contract.Object{"present": true}})
			current = contract.Object{"from_membership": membershipID, "field_name": policy.statusField}
		} else {
			itemID, _ = contract.Nonempty(item, "item_id")
			fields, _ := contract.ObjectAt(item, "field_values")
			current = fields[policy.statusField]
		}
		if item == nil || current != policy.targetStatus {
			ops = append(ops, governanceProjectFieldOperation(membershipIDIfMissing(item, membershipID), policy.project.ID, policy.executionIssue, itemID, policy.statusField, policy.targetStatus, current))
		}
	}
	if policy.executionWorkMarker != "" {
		if strings.Contains(workCommentBody, "{{pull_request_url}}") || strings.Contains(workCommentBody, "{{pull_request_number}}") {
			return nil, errors.New("execution-state work comment contains an unresolved delivery token")
		}
		body := strings.TrimSpace(workCommentBody) + "\n\n" + policy.executionWorkMarker + "\n"
		comments, err := contract.Objects(inventory, "comments")
		if err != nil {
			return nil, err
		}
		var found contract.Object
		for _, comment := range comments {
			number, _ := contract.PositiveInteger(comment["issue_number"])
			if number == policy.executionIssue && strings.Contains(fmt.Sprint(comment["body"]), policy.executionWorkMarker) {
				if found != nil {
					return nil, errors.New("execution-state work marker is ambiguous on its selected issue")
				}
				found = comment
			}
		}
		if found == nil || found["body"] != body {
			var commentID any
			if found != nil {
				commentID = found["id"]
			}
			ops = append(ops, contract.Operation{ID: "execution-state:work-comment", Kind: "issue-comment-upsert", Target: contract.Object{"issue_number": policy.executionIssue, "issue_id": issue["id"], "marker": policy.executionWorkMarker, "comment_id": commentID, "body": body}, Before: contract.Object{"issue_number": policy.executionIssue, "issue_id": issue["id"], "marker": policy.executionWorkMarker, "comment_id": commentID, "body": func() any {
				if found != nil {
					return found["body"]
				}
				return nil
			}()}, After: contract.Object{"issue_number": policy.executionIssue, "issue_id": issue["id"], "marker": policy.executionWorkMarker, "body": body}})
		}
	}
	return ops, nil
}

func membershipIDIfMissing(item contract.Object, id string) string {
	if item == nil {
		return id
	}
	return ""
}

func governanceBootstrapOperations(policy governancePolicy, inventory contract.Object) ([]contract.Operation, error) {
	project, err := governanceProject(inventory, *policy.project)
	if err != nil {
		return nil, err
	}
	if err = governanceValidateProjectOption(project, policy.statusField, policy.targetStatus); err != nil {
		return nil, err
	}
	graph, _ := contract.ObjectAt(inventory, "issue_inventory")
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	if len(policy.selectedIssues) > 0 {
		for number := range policy.selectedIssues {
			if backlogIssueByNumber(issues, number) == nil {
				return nil, fmt.Errorf("selected issue #%d is absent from complete all-state inventory", number)
			}
		}
	}
	labelNames := governanceLabelNames(inventory)
	ops := []contract.Operation{}
	for _, issue := range issues {
		number, _ := contract.PositiveInteger(issue["number"])
		if len(policy.selectedIssues) > 0 && !policy.selectedIssues[number] || issue["state"] != "OPEN" {
			continue
		}
		label, typed := policy.typedLabels[governanceIssuePrefix(fmt.Sprint(issue["title"]))]
		if !typed {
			continue
		}
		if !labelNames[label] {
			return nil, fmt.Errorf("typed issue policy label %q is absent from complete repository labels", label)
		}
		item := governanceProjectItem(project, number)
		if item == nil {
			membershipID := fmt.Sprintf("bootstrap:project-membership:%d", number)
			ops = append(ops, contract.Operation{ID: membershipID, Kind: "project-membership-add", Target: contract.Object{"project_id": policy.project.ID, "issue_number": number, "issue_node_id": issue["id"]}, Before: contract.Object{"present": false}, After: contract.Object{"present": true}})
			ops = append(ops, governanceProjectFieldOperation(membershipID, policy.project.ID, number, "", policy.statusField, policy.targetStatus, map[string]any{"from_membership": membershipID}))
			continue
		}
		fields, _ := contract.ObjectAt(item, "field_values")
		current, present := fields[policy.statusField]
		if !present || !policy.validStatuses[fmt.Sprint(current)] {
			ops = append(ops, governanceProjectFieldOperation("", policy.project.ID, number, item["item_id"].(string), policy.statusField, policy.targetStatus, current))
		}
	}
	return ops, nil
}

func governanceFixOperations(policy governancePolicy, inventory contract.Object) ([]contract.Operation, error) {
	labels := governanceLabelNames(inventory)
	for label := range policy.governanceLabels {
		if !labels[label] {
			return nil, fmt.Errorf("governance label %q is absent from complete repository labels", label)
		}
	}
	for label := range policy.retiredLabels {
		if !labels[label] { /* retired label may be absent after cleanup */
		}
	}
	graph, _ := contract.ObjectAt(inventory, "issue_inventory")
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	ops := []contract.Operation{}
	for _, issue := range issues {
		if issue["state"] != "OPEN" {
			continue
		}
		number, _ := contract.PositiveInteger(issue["number"])
		current, err := contract.Strings(issue["labels"])
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for _, name := range current {
			set[name] = true
		}
		for retired := range policy.retiredLabels {
			delete(set, retired)
		}
		if expected, recognized := policy.typedLabels[governanceIssuePrefix(fmt.Sprint(issue["title"]))]; recognized {
			for label := range policy.governanceLabels {
				if label != expected {
					delete(set, label)
				}
			}
			set[expected] = true
		}
		after := sortedBoolMap(set)
		before := sortedStringsSet(current)
		if !same(before, after) {
			ops = append(ops, contract.Operation{ID: fmt.Sprintf("governance:issue-labels:%d", number), Kind: "issue-label-set", Target: contract.Object{"issue_number": number, "issue_id": issue["id"]}, Before: contract.Object{"labels": before}, After: contract.Object{"labels": after}})
		}
	}
	milestoneRows, err := contract.Objects(inventory, "milestones")
	if err != nil {
		return nil, err
	}
	byTitle := map[string]contract.Object{}
	for _, row := range milestoneRows {
		if row["state"] == "open" {
			byTitle[fmt.Sprint(row["title"])] = row
		}
	}
	for _, target := range policy.milestones {
		current := byTitle[target.Title]
		if current == nil {
			ops = append(ops, contract.Operation{ID: "governance:milestone:create:" + governanceIDPart(target.Title), Kind: "milestone-create", Target: contract.Object{"title": target.Title, "description": target.Description, "due_on": target.DueOn}, Before: contract.Object{"exists": false, "title": nil, "description": nil, "due_on": nil}, After: contract.Object{"exists": true, "title": target.Title, "description": target.Description, "due_on": target.DueOn}})
			continue
		}
		if strings.TrimSpace(fmt.Sprint(current["description"])) == strings.TrimSpace(target.Description) && sameQuarterDate(current["due_on"], target.DueOn) {
			continue
		}
		ops = append(ops, contract.Operation{ID: "governance:milestone:update:" + governanceIDPart(target.Title), Kind: "milestone-update", Target: contract.Object{"number": current["number"], "title": target.Title, "description": target.Description, "due_on": target.DueOn}, Before: contract.Object{"exists": true, "number": current["number"], "id": current["id"], "node_id": current["node_id"], "description": current["description"], "due_on": current["due_on"], "state": current["state"]}, After: contract.Object{"exists": true, "number": current["number"], "description": target.Description, "due_on": target.DueOn, "state": "open"}})
	}
	return ops, nil
}

func governanceLabelOperations(policy governancePolicy, inventory contract.Object) ([]contract.Operation, error) {
	live := contract.Object{"labels": inventory["labels"]}
	diff, err := governance.LabelPaletteDiff(policy.palette, live)
	if err != nil {
		return nil, err
	}
	entries, _ := contract.Objects(diff, "labels")
	liveRows, _ := contract.Objects(inventory, "labels")
	byName := map[string]contract.Object{}
	for _, row := range liveRows {
		byName[row["name"].(string)] = row
	}
	ops := []contract.Operation{}
	for _, entry := range entries {
		action, _ := contract.String(entry, "action")
		if action != "create" && action != "update" {
			continue
		}
		name, _ := contract.String(entry, "name")
		color, _ := contract.String(entry, "target_color")
		description, _ := contract.String(entry, "target_description")
		if action == "create" {
			ops = append(ops, contract.Operation{ID: "governance:label:create:" + governanceIDPart(name), Kind: "label-create", Target: contract.Object{"name": name, "color": strings.ToLower(color), "description": description}, Before: contract.Object{"exists": false, "name": nil, "color": nil, "description": nil}, After: contract.Object{"exists": true, "name": name, "color": strings.ToLower(color), "description": description}})
			continue
		}
		current := byName[name]
		if current == nil {
			return nil, errors.New("label palette update target disappeared from captured inventory")
		}
		ops = append(ops, contract.Operation{ID: "governance:label:update:" + governanceIDPart(name), Kind: "label-update", Target: contract.Object{"node_id": current["node_id"], "name": name, "color": strings.ToLower(color), "description": description}, Before: contract.Object{"exists": true, "node_id": current["node_id"], "name": name, "color": current["color"], "description": current["description"]}, After: contract.Object{"exists": true, "node_id": current["node_id"], "name": name, "color": strings.ToLower(color), "description": description}})
	}
	return ops, nil
}

func governanceDriftOperations(policy governancePolicy, inventory contract.Object, repo contract.Repository, rawPolicy contract.Object, captured string) ([]contract.Operation, error) {
	graph, _ := contract.ObjectAt(inventory, "issue_inventory")
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	var issue contract.Object
	for _, row := range issues {
		if row["title"] == policy.driftTitle && (issue == nil || governanceNumber(row["number"]) < governanceNumber(issue["number"])) {
			issue = row
		}
	}
	ops := []contract.Operation{}
	creationMarker, err := governanceMarker(repo, GovernanceDriftIssue, rawPolicy, captured)
	if err != nil {
		return nil, err
	}
	ref := contract.Object{}
	if policy.driftUnresolved {
		if issue == nil {
			if !governanceLabelNames(inventory)[policy.driftMaintenance] {
				return nil, fmt.Errorf("drift maintenance label %q is absent from complete repository labels", policy.driftMaintenance)
			}
			body := strings.TrimSpace(policy.driftBody) + "\n\n" + creationMarker
			createID := "drift:issue:create"
			ops = append(ops, contract.Operation{ID: createID, Kind: "issue-create", Target: contract.Object{"title": policy.driftTitle, "body": body, "labels": []any{policy.driftMaintenance}, "creation_marker": creationMarker}, Before: contract.Object{"exists": false, "title": nil, "body": nil, "state": nil, "labels": nil, "creation_marker": nil}, After: contract.Object{"exists": true, "title": policy.driftTitle, "body": body, "state": "OPEN", "labels": []any{policy.driftMaintenance}, "creation_marker": creationMarker}})
			ref = contract.Object{"create_operation": createID, "creation_marker": creationMarker}
		} else {
			ref = governanceIssueReference(issue)
			if issue["state"] == "CLOSED" {
				ops = append(ops, governanceStateOperation(issue, "OPEN"))
			}
		}
	} else if issue != nil && policy.driftCloseWhenClean {
		ref = governanceIssueReference(issue)
	}
	if ref != nil && len(ref) > 0 && (policy.driftUnresolved || policy.driftCloseWhenClean) {
		commentBody := governanceCommentBody(policy.driftSummary, captured, creationMarker)
		comment := governanceCommentByMarker(mustObjects(inventory, "comments"), ref, creationMarker)
		before := contract.Object{"comment_id": nil, "body": nil}
		target := contract.Object{"issue_ref": ref, "issue_number": ref["issue_number"], "issue_id": ref["issue_id"], "marker": creationMarker, "body": commentBody, "comment_id": nil}
		if comment != nil {
			before = contract.Object{"comment_id": comment["id"], "body": comment["body"]}
			target["comment_id"] = comment["id"]
		}
		ops = append(ops, contract.Operation{ID: "drift:comment", Kind: "issue-comment-upsert", Target: target, Before: before, After: contract.Object{"comment_id": target["comment_id"], "body": commentBody}})
	}
	if !policy.driftUnresolved && issue != nil && policy.driftCloseWhenClean {
		if issue["state"] != "CLOSED" {
			ops = append(ops, governanceStateOperation(issue, "CLOSED"))
		}
	}
	if policy.project != nil && ref != nil && len(ref) > 0 {
		project, err := governanceProject(inventory, *policy.project)
		if err != nil {
			return nil, err
		}
		status := policy.driftTodoStatus
		if !policy.driftUnresolved {
			status = policy.driftDoneStatus
		}
		if err = governanceValidateProjectOption(project, policy.statusField, status); err != nil {
			return nil, err
		}
		if policy.driftUnresolved {
			if err = governanceValidateProjectOption(project, policy.driftPriorityField, policy.driftPriorityValue); err != nil {
				return nil, err
			}
		}
		var item contract.Object
		if issue != nil {
			item = governanceProjectItem(project, governanceNumber(issue["number"]))
		}
		if item == nil {
			membershipID := "drift:project-membership"
			target := contract.Object{"project_id": policy.project.ID, "issue_ref": ref}
			if number, ok := ref["issue_number"]; ok {
				target["issue_number"] = number
				target["issue_node_id"] = ref["issue_id"]
			} else {
				target["create_operation"] = ref["create_operation"]
			}
			ops = append(ops, contract.Operation{ID: membershipID, Kind: "project-membership-add", Target: target, Before: contract.Object{"present": false}, After: contract.Object{"present": true}})
			statusOp := governanceProjectFieldOperation(membershipID, policy.project.ID, 0, "", policy.statusField, status, map[string]any{"from_membership": membershipID})
			statusOp.Target["issue_ref"] = ref
			ops = append(ops, statusOp)
			if policy.driftUnresolved {
				priorityOp := governanceProjectFieldOperation(membershipID, policy.project.ID, 0, "", policy.driftPriorityField, policy.driftPriorityValue, map[string]any{"from_membership": membershipID})
				priorityOp.Target["issue_ref"] = ref
				ops = append(ops, priorityOp)
				queueValue, err := governanceNextQueueOrder(project, policy.driftPriorityField, policy.driftPriorityValue, policy.driftQueueField, policy.driftQueueStep)
				if err != nil {
					return nil, err
				}
				queueOp := governanceProjectNumberOperation(membershipID, policy.project.ID, 0, "", policy.driftQueueField, queueValue, map[string]any{"from_membership": membershipID})
				queueOp.Target["issue_ref"] = ref
				ops = append(ops, queueOp)
			}
		} else {
			fields, _ := contract.ObjectAt(item, "field_values")
			number := governanceNumber(issue["number"])
			if fields[policy.statusField] != status {
				ops = append(ops, governanceProjectFieldOperation("", policy.project.ID, number, item["item_id"].(string), policy.statusField, status, fields[policy.statusField]))
			}
			if policy.driftUnresolved && fields[policy.driftPriorityField] != policy.driftPriorityValue {
				ops = append(ops, governanceProjectFieldOperation("", policy.project.ID, number, item["item_id"].(string), policy.driftPriorityField, policy.driftPriorityValue, fields[policy.driftPriorityField]))
			}
		}
	}
	return ops, nil
}

func governanceCommentBody(summary, captured, marker string) string {
	stamp, err := time.Parse(time.RFC3339Nano, captured)
	if err != nil {
		return ""
	}
	return "## Governance audit " + stamp.UTC().Format(time.RFC3339Nano) + "\n\n" + strings.TrimSpace(summary) + "\n\n" + marker + "\n"
}

func governanceCommentByMarker(rows []contract.Object, ref contract.Object, marker string) contract.Object {
	var number int64
	if n, err := contract.PositiveInteger(ref["issue_number"]); err == nil {
		number = n
	} else {
		for _, row := range rows {
			if strings.Contains(fmt.Sprint(row["body"]), marker) {
				number = governanceNumber(row["issue_number"])
				break
			}
		}
	}
	for _, row := range rows {
		if row["issue_number"] == number && strings.Contains(fmt.Sprint(row["body"]), marker) {
			return row
		}
	}
	return nil
}

func containsGovernanceAuditMarker(value string) bool {
	return strings.Contains(value, governanceAuditMarkerPrefix) || strings.Contains(value, governanceLegacyAuditMarker)
}

func governanceIssueReference(issue contract.Object) contract.Object {
	return contract.Object{"issue_number": issue["number"], "issue_id": issue["id"]}
}
func governanceStateOperation(issue contract.Object, state string) contract.Operation {
	number := governanceNumber(issue["number"])
	return contract.Operation{ID: fmt.Sprintf("drift:issue-state:%d:%s", number, strings.ToLower(state)), Kind: "issue-state-set", Target: contract.Object{"issue_number": number, "issue_id": issue["id"], "state": state}, Before: contract.Object{"state": issue["state"]}, After: contract.Object{"state": state}}
}

func governanceNumber(value any) int64 {
	number, _ := contract.Integer(value)
	return number
}
func governanceProjectFieldOperation(membershipID, projectID string, number int64, itemID, field, value string, before any) contract.Operation {
	target := contract.Object{"project_id": projectID, "issue_number": number, "item_id": itemID, "field_name": field, "value": value, "membership_operation": membershipID}
	return contract.Operation{ID: "project:field:" + fieldSlug(field) + ":" + fmt.Sprint(number) + ":" + governanceIDPart(membershipID), Kind: "project-field-set", Target: target, Before: contract.Object{"value": before}, After: contract.Object{"value": value}}
}
func governanceProjectNumberOperation(membershipID, projectID string, number int64, itemID, field string, value float64, before any) contract.Operation {
	target := contract.Object{"project_id": projectID, "issue_number": number, "item_id": itemID, "field_name": field, "number_value": value, "membership_operation": membershipID}
	return contract.Operation{ID: "project:field:" + fieldSlug(field) + ":" + fmt.Sprint(number) + ":" + governanceIDPart(membershipID), Kind: "project-field-set-number", Target: target, Before: contract.Object{"value": before}, After: contract.Object{"value": value}}
}

func governanceIssuePrefix(title string) string {
	prefix, _, found := strings.Cut(title, ":")
	if !found {
		return ""
	}
	return strings.TrimSpace(prefix)
}
func governanceIDPart(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
func sortedBoolMap(values map[string]bool) []any {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return stringSliceAny(keys)
}
func sortedStringsSet(values []string) []any {
	copy := append([]string{}, values...)
	sort.Strings(copy)
	out := []any{}
	for _, value := range copy {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}
func governanceLabelNames(inventory contract.Object) map[string]bool {
	out := map[string]bool{}
	rows, _ := contract.Objects(inventory, "labels")
	for _, row := range rows {
		if name, ok := row["name"].(string); ok {
			out[name] = true
		}
	}
	return out
}
func governanceProject(inventory contract.Object, scope ProjectScope) (contract.Object, error) {
	rows, err := contract.Objects(inventory, "projects")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		p, _ := contract.ObjectAt(row, "project")
		number, numberErr := contract.PositiveInteger(p["number"])
		if numberErr == nil && p["id"] == scope.ID && number == scope.Number && p["title"] == scope.Title && p["owner_login"] == scope.Owner && p["owner_type"] == scope.OwnerType && p["host"] == scope.Host {
			return row, nil
		}
	}
	return nil, errors.New("complete governance inventory lacks the exact requested Project")
}
func governanceProjectItem(project contract.Object, number int64) contract.Object {
	items, _ := contract.Objects(project, "items")
	for _, item := range items {
		itemNumber, err := contract.PositiveInteger(item["number"])
		if err == nil && itemNumber == number && item["archived"] != true {
			return item
		}
	}
	return nil
}
func governanceValidateProjectOption(project contract.Object, field, option string) error {
	value := ProjectFieldValue{Text: &option}
	return validateProjectField(project, field, value)
}
func governanceNextQueueOrder(project contract.Object, priorityField, priority string, queueField string, step float64) (float64, error) {
	if err := validateProjectField(project, queueField, ProjectFieldValue{Number: &step}); err != nil {
		return 0, err
	}
	max := 0.0
	for _, item := range mustObjects(project, "items") {
		values, _ := contract.ObjectAt(item, "field_values")
		if values[priorityField] != priority {
			continue
		}
		if raw, ok := values[queueField]; ok {
			n, err := finiteNumber(raw)
			if err != nil {
				return 0, errors.New("queue order inventory contains nonnumeric value")
			}
			if n > max {
				max = n
			}
		}
	}
	return max + step, nil
}
func sameQuarterDate(current any, target string) bool {
	old, ok := current.(string)
	return ok && len(old) >= 10 && len(target) >= 10 && old[:10] == target[:10]
}

// Governance rederives the approved typed operations and applies each with a
// durable ACK before reading and comparing the complete after-state.
type Governance struct{ Provider GovernanceProvider }

var _ apply.Adapter = Governance{}
var _ apply.AcknowledgingAdapter = Governance{}

func (a Governance) Operations(p contract.Plan) ([]contract.Operation, error) {
	return governanceOperations(p)
}

func (a Governance) ValidateReceipt(p contract.Plan, op contract.Operation, result contract.Object) error {
	if err := a.ValidateAcknowledgement(p, op, result); err != nil {
		return err
	}
	if result["after_verified"] != true {
		return errors.New("governance receipt lacks complete verified after-state")
	}
	return nil
}

func (a Governance) ValidateAcknowledgement(p contract.Plan, op contract.Operation, ack contract.Object) error {
	if p.Command != GovernanceCommand {
		return errors.New("governance acknowledgement targets another command")
	}
	operations, err := governanceOperations(p)
	if err != nil {
		return err
	}
	if !containsOperation(operations, op) {
		return errors.New("governance acknowledgement is outside derived operations")
	}
	allowed := keySet("kind", "primitive_id", "repository", "operation_id", "target", "before", "after", "acknowledged", "provider_result", "after_verified")
	for key := range ack {
		if !allowed[key] {
			return fmt.Errorf("governance acknowledgement has unsupported field %q", key)
		}
	}
	if ack["kind"] != op.Kind || ack["primitive_id"] != op.ID || ack["acknowledged"] != true || !same(ack["target"], op.Target) || !same(ack["before"], op.Before) || !same(ack["after"], op.After) {
		return errors.New("governance acknowledgement differs from reviewed operation")
	}
	if value, ok := ack["after_verified"]; ok && value != true {
		return errors.New("governance acknowledgement has invalid after-state flag")
	}
	repository, err := contract.ObjectAt(ack, "repository")
	if err != nil {
		return err
	}
	repo, err := contract.ParseRepository(repository)
	if err != nil || repo != p.Repository {
		return errors.New("governance acknowledgement belongs to another repository")
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
	return validateGovernanceProviderResult(p, op, nonce, providerResult)
}

func (a Governance) Preflight(ctx context.Context, p contract.Plan, receipts []contract.Object) error {
	if a.Provider == nil {
		return errors.New("governance adapter requires a provider")
	}
	request, err := governanceRequestFromPlan(p)
	if err != nil {
		return err
	}
	expected, err := governanceExpectedInventory(p, receipts)
	if err != nil {
		return err
	}
	raw, err := a.Provider.BacklogInventory(ctx, request)
	if err != nil {
		return err
	}
	actual, err := normalizeGovernanceInventory(raw, p.Repository, request)
	if err != nil {
		return err
	}
	if !same(expected, actual) {
		return errors.New("complete live governance inventory drifted from reviewed state")
	}
	return nil
}

func (a Governance) Dispatch(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, error) {
	return a.DispatchAcknowledged(ctx, p, op, nonce, receipts, func(contract.Object) error { return nil })
}

func (a Governance) DispatchAcknowledged(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object, persist func(contract.Object) error) (contract.Object, error) {
	if a.Provider == nil {
		return nil, errors.New("governance adapter requires a provider")
	}
	if _, err := native.OperationMarker(nonce); err != nil {
		return nil, err
	}
	if !containsOperation(mustGovernanceOperations(p), op) {
		return nil, errors.New("governance dispatch primitive is outside derived plan")
	}
	expected, err := governanceExpectedInventory(p, receipts)
	if err != nil {
		return nil, err
	}
	providerResult, err := a.dispatchGovernance(ctx, p, op, nonce, receipts, expected)
	if err != nil {
		return nil, err
	}
	ack := contract.Object{"kind": op.Kind, "primitive_id": op.ID, "repository": p.Repository.Object(), "operation_id": nonce, "target": op.Target, "before": op.Before, "after": op.After, "acknowledged": true, "provider_result": providerResult}
	if err = a.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, err
	}
	if persist == nil {
		return nil, errors.New("governance acknowledgement persistence callback is required")
	}
	if err = persist(ack); err != nil {
		return nil, err
	}
	projected, err := governanceProjectAcknowledgement(expected, p, op, ack, receipts)
	if err != nil {
		return nil, fmt.Errorf("captured governance acknowledgement could not be projected: %w", err)
	}
	request, err := governanceRequestFromPlan(p)
	if err != nil {
		return nil, err
	}
	raw, err := a.Provider.BacklogInventory(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("governance write was acknowledged but after-state read failed: %w", err)
	}
	actual, err := normalizeGovernanceInventory(raw, p.Repository, request)
	if err != nil {
		return nil, fmt.Errorf("governance write was acknowledged but after-state is invalid: %w", err)
	}
	if !same(projected, actual) {
		return nil, errors.New("governance write was acknowledged but complete after-state differs from primitive")
	}
	ack["after_verified"] = true
	return ack, nil
}

func (a Governance) Observe(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, contract.Object, error) {
	if a.Provider == nil {
		return nil, nil, errors.New("governance adapter requires a provider")
	}
	var ack contract.Object
	for _, receipt := range receipts {
		if receipt["id"] == op.ID && receipt["status"] == "unknown" && receipt["operation_id"] == nonce {
			ack, _ = receipt["acknowledgement"].(map[string]any)
		}
	}
	if ack == nil || ack["operation_id"] != nonce {
		return nil, nil, errors.New("ambiguous governance write has no durable native acknowledgement; no mutation was retried")
	}
	if err := a.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, nil, err
	}
	prior, err := governanceExpectedInventory(p, receipts)
	if err != nil {
		return nil, nil, err
	}
	projected, err := governanceProjectAcknowledgement(prior, p, op, ack, governanceCompletedPrefix(receipts, op.ID))
	if err != nil {
		return nil, nil, err
	}
	request, err := governanceRequestFromPlan(p)
	if err != nil {
		return nil, nil, err
	}
	raw, err := a.Provider.BacklogInventory(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	actual, err := normalizeGovernanceInventory(raw, p.Repository, request)
	if err != nil {
		return nil, nil, err
	}
	if !same(projected, actual) {
		return nil, nil, errors.New("acknowledged governance operation does not match complete live after-state")
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

func (a Governance) dispatchGovernance(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object, inventory contract.Object) (contract.Object, error) {
	if a.Provider == nil {
		return nil, errors.New("governance adapter requires a provider")
	}
	switch op.Kind {
	case "label-create":
		return a.Provider.CreateLabel(ctx, nonce, LabelDraft{Name: fmt.Sprint(op.Target["name"]), Color: fmt.Sprint(op.Target["color"]), Description: fmt.Sprint(op.Target["description"])})
	case "label-update":
		return a.Provider.UpdateLabel(ctx, nonce, fmt.Sprint(op.Target["node_id"]), fmt.Sprint(op.Target["name"]), fmt.Sprint(op.Target["color"]), fmt.Sprint(op.Target["description"]))
	case "issue-label-set":
		number, err := contract.PositiveInteger(op.Target["issue_number"])
		if err != nil {
			return nil, err
		}
		labels, err := contract.Strings(op.After["labels"])
		if err != nil {
			return nil, err
		}
		return a.Provider.UpdateIssue(ctx, nonce, number, IssuePatch{Labels: &labels})
	case "milestone-create":
		return a.Provider.CreateMilestone(ctx, nonce, MilestoneDraft{Title: fmt.Sprint(op.Target["title"]), Description: fmt.Sprint(op.Target["description"]), DueOn: fmt.Sprint(op.Target["due_on"])})
	case "milestone-update":
		number, err := contract.PositiveInteger(op.Target["number"])
		if err != nil {
			return nil, err
		}
		description, due := fmt.Sprint(op.Target["description"]), fmt.Sprint(op.Target["due_on"])
		return a.Provider.UpdateMilestone(ctx, nonce, number, MilestonePatch{Description: &description, DueOn: &due})
	case "issue-create":
		labels, err := contract.Strings(op.Target["labels"])
		if err != nil {
			return nil, err
		}
		return a.Provider.CreateIssue(ctx, nonce, IssueDraft{Title: fmt.Sprint(op.Target["title"]), Body: fmt.Sprint(op.Target["body"]), Labels: labels})
	case "issue-comment-upsert":
		number, err := governanceResolveIssueNumber(op.Target, inventory, p, receipts)
		if err != nil {
			return nil, err
		}
		body := fmt.Sprint(op.Target["body"])
		if op.Target["comment_id"] != nil {
			commentID, err := contract.PositiveInteger(op.Target["comment_id"])
			if err != nil {
				return nil, err
			}
			return a.Provider.UpdateIssueComment(ctx, nonce, number, commentID, body)
		}
		return a.Provider.CreateIssueComment(ctx, nonce, number, body)
	case "issue-state-set":
		number, err := contract.PositiveInteger(op.Target["issue_number"])
		if err != nil {
			return nil, err
		}
		return a.Provider.SetIssueState(ctx, nonce, number, fmt.Sprint(op.Target["state"]))
	case "project-membership-add":
		projectID, err := contract.Nonempty(op.Target, "project_id")
		if err != nil {
			return nil, err
		}
		number, nodeID, err := governanceResolveIssueTarget(op.Target, inventory, p, receipts)
		if err != nil {
			return nil, err
		}
		if nodeID == "" {
			graph, _ := contract.ObjectAt(inventory, "issue_inventory")
			issue := backlogIssueByNumber(mustObjects(graph, "issues"), number)
			if issue == nil {
				return nil, errors.New("Project membership issue is absent from projected complete inventory")
			}
			nodeID = fmt.Sprint(issue["id"])
		}
		return a.Provider.AddProjectIssue(ctx, nonce, projectID, nodeID)
	case "project-field-set", "project-field-set-number":
		projectID, err := contract.Nonempty(op.Target, "project_id")
		if err != nil {
			return nil, err
		}
		number, _, err := governanceResolveIssueTarget(op.Target, inventory, p, receipts)
		if err != nil {
			return nil, err
		}
		project, err := governanceProjectByID(inventory, projectID)
		if err != nil {
			return nil, err
		}
		item := governanceProjectItem(project, number)
		if item == nil {
			return nil, errors.New("Project field target item is absent from projected inventory")
		}
		field, err := contract.Nonempty(op.Target, "field_name")
		if err != nil {
			return nil, err
		}
		value := ProjectFieldValue{}
		if op.Kind == "project-field-set-number" {
			n, err := finiteNumber(op.Target["number_value"])
			if err != nil {
				return nil, err
			}
			value.Number = &n
		} else {
			text, err := contract.Nonempty(op.Target, "value")
			if err != nil {
				return nil, err
			}
			value.Text = &text
		}
		return a.Provider.SetProjectField(ctx, nonce, projectID, fmt.Sprint(item["item_id"]), ProjectField(field), value)
	default:
		return nil, fmt.Errorf("unsupported governance operation %q", op.Kind)
	}
}

func mustGovernanceOperations(p contract.Plan) []contract.Operation {
	ops, _ := governanceOperations(p)
	return ops
}

func governanceExpectedInventory(p contract.Plan, receipts []contract.Object) (contract.Object, error) {
	request, err := governanceRequestFromPlan(p)
	if err != nil {
		return nil, err
	}
	raw, err := contract.ObjectAt(p.Data, "inventory")
	if err != nil {
		return nil, err
	}
	expected, err := validateStoredGovernanceInventory(raw, p.Repository, request)
	if err != nil {
		return nil, err
	}
	ops, err := governanceOperations(p)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		op, ok := byID[id]
		if !ok {
			return nil, errors.New("governance receipt is outside derived plan")
		}
		result, err := contract.ObjectAt(receipt, "result")
		if err != nil {
			return nil, err
		}
		if err = (&Governance{}).ValidateReceipt(p, op, result); err != nil {
			return nil, err
		}
		if receipt["operation_id"] != result["operation_id"] {
			return nil, errors.New("governance completion has another dispatch identity")
		}
		expected, err = governanceProjectAcknowledgement(expected, p, op, result, prefix)
		if err != nil {
			return nil, err
		}
		prefix = append(prefix, receipt)
	}
	return expected, nil
}

func governanceCompletedPrefix(receipts []contract.Object, exclude string) []contract.Object {
	out := []contract.Object{}
	for _, receipt := range receipts {
		if receipt["status"] == "completed" && receipt["id"] != exclude {
			out = append(out, receipt)
		}
	}
	return out
}

func governanceResolveIssueNumber(target, inventory contract.Object, p contract.Plan, receipts []contract.Object) (int64, error) {
	number, _, err := governanceResolveIssueTarget(target, inventory, p, receipts)
	return number, err
}

func governanceResolveIssueTarget(target, inventory contract.Object, p contract.Plan, receipts []contract.Object) (int64, string, error) {
	if n, err := contract.PositiveInteger(target["issue_number"]); err == nil {
		graph, _ := contract.ObjectAt(inventory, "issue_inventory")
		issue := backlogIssueByNumber(mustObjects(graph, "issues"), n)
		if issue == nil {
			return 0, "", errors.New("governance issue target is absent from complete projected inventory")
		}
		if node, ok := target["issue_id"].(string); ok && node != "" && issue["id"] != node {
			return 0, "", errors.New("governance issue node identity changed")
		}
		if node, ok := target["issue_node_id"].(string); ok && node != "" && issue["id"] != node {
			return 0, "", errors.New("governance Project issue node identity changed")
		}
		return n, fmt.Sprint(issue["id"]), nil
	}
	ref, err := contract.ObjectAt(target, "issue_ref")
	if err != nil {
		return 0, "", errors.New("governance operation has no resolvable issue identity")
	}
	if n, err := contract.PositiveInteger(ref["issue_number"]); err == nil {
		issueTarget := contract.Object{"issue_number": n, "issue_id": ref["issue_id"]}
		return governanceResolveIssueTarget(issueTarget, inventory, p, receipts)
	}
	createID, err := contract.Nonempty(ref, "create_operation")
	if err != nil {
		return 0, "", errors.New("governance issue reference must name a creation operation")
	}
	for _, receipt := range receipts {
		if receipt["id"] != createID || receipt["status"] != "completed" {
			continue
		}
		result, err := contract.ObjectAt(receipt, "result")
		if err != nil {
			return 0, "", err
		}
		raw, err := contract.ObjectAt(result, "provider_result")
		if err != nil {
			return 0, "", err
		}
		issue, err := backlogCreatedIssue(raw, p.Repository)
		if err != nil {
			return 0, "", err
		}
		n, err := contract.PositiveInteger(issue["number"])
		if err != nil {
			return 0, "", err
		}
		return n, fmt.Sprint(issue["id"]), nil
	}
	return 0, "", errors.New("governance issue creation prerequisite has no completed durable receipt")
}

func governanceProjectByID(inventory contract.Object, id string) (contract.Object, error) {
	rows, err := contract.Objects(inventory, "projects")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		project, _ := contract.ObjectAt(row, "project")
		if project["id"] == id {
			return row, nil
		}
	}
	return nil, errors.New("governance Project is absent from complete inventory")
}

func validateGovernanceProviderResult(p contract.Plan, op contract.Operation, nonce string, result contract.Object) error {
	if result == nil {
		return errors.New("governance provider returned no acknowledgement")
	}
	marker, err := native.OperationMarker(nonce)
	if err != nil {
		return err
	}
	switch op.Kind {
	case "label-create", "label-update":
		if result["clientMutationId"] != nonce {
			return errors.New("label provider acknowledgement has another durable mutation identity")
		}
		label, err := contract.ObjectAt(result, "label")
		if err != nil {
			return err
		}
		repository, err := contract.ObjectAt(label, "repository")
		if err != nil {
			return err
		}
		inventory, err := contract.ObjectAt(p.Data, "inventory")
		if err != nil {
			return err
		}
		provenance, err := contract.ObjectAt(inventory, "provenance")
		if err != nil {
			return err
		}
		if repository["id"] != provenance["repository_node_id"] || !strings.EqualFold(fmt.Sprint(repository["nameWithOwner"]), p.Repository.FullName()) || repository["url"] != p.Repository.URL {
			return errors.New("label acknowledgement belongs to another repository incarnation")
		}
		node, err := contract.Nonempty(label, "id")
		if err != nil {
			return err
		}
		if op.Kind == "label-update" && node != op.Target["node_id"] {
			return errors.New("updated label acknowledgement changed immutable global identity")
		}
		if label["name"] != op.Target["name"] || strings.ToLower(fmt.Sprint(label["color"])) != strings.ToLower(fmt.Sprint(op.Target["color"])) {
			return errors.New("label acknowledgement differs from reviewed name or color")
		}
		description, present := label["description"]
		if !present {
			return errors.New("label acknowledgement omitted nullable description")
		}
		if description == nil {
			description = ""
		}
		if description != op.Target["description"] {
			return errors.New("label acknowledgement differs from reviewed description")
		}
	case "issue-label-set", "issue-state-set", "issue-create":
		issue, err := backlogCreatedIssue(result, p.Repository)
		if err != nil {
			return err
		}
		if op.Kind == "issue-create" {
			body := fmt.Sprint(op.Target["body"]) + "\n\n" + marker
			labels, _ := contract.Strings(op.Target["labels"])
			if issue["title"] != op.Target["title"] || issue["body"] != body || issue["state"] != "OPEN" || issue["milestone"] != nil || !same(issue["labels"], stringSliceAny(labels)) || !strings.Contains(body, fmt.Sprint(op.Target["creation_marker"])) {
				return errors.New("created drift issue acknowledgement differs from reviewed identity or content")
			}
			break
		}
		number, err := contract.PositiveInteger(op.Target["issue_number"])
		if err != nil || issue["number"] != number || issue["id"] != op.Target["issue_id"] {
			return errors.New("governance issue acknowledgement has foreign issue identity")
		}
		if op.Kind == "issue-label-set" {
			labels, err := contract.Strings(op.After["labels"])
			if err != nil {
				return err
			}
			if !same(issue["labels"], stringSliceAny(labels)) {
				return errors.New("issue label acknowledgement differs from exact reviewed label set")
			}
		} else if issue["state"] != op.After["state"] {
			return errors.New("issue state acknowledgement differs from reviewed state")
		}
	case "milestone-create", "milestone-update":
		milestone, err := backlogCreatedMilestone(result, p.Repository)
		if err != nil {
			return err
		}
		description := fmt.Sprint(op.Target["description"])
		if op.Kind == "milestone-create" {
			description += "\n\n" + marker
		}
		if milestone["title"] != op.Target["title"] || milestone["description"] != description || !sameQuarterDate(milestone["due_on"], fmt.Sprint(op.Target["due_on"])) || milestone["state"] != "open" {
			return errors.New("milestone acknowledgement differs from reviewed title, description, date or state")
		}
		if op.Kind == "milestone-update" {
			if milestone["number"] != op.Before["number"] || milestone["id"] != op.Before["id"] || milestone["node_id"] != op.Before["node_id"] {
				return errors.New("updated milestone acknowledgement changed immutable identity")
			}
		}
	case "issue-comment-upsert":
		number, err := contract.PositiveInteger(op.Target["issue_number"])
		if err != nil {
			number, err = governanceIssueNumberFromReference(op.Target)
		}
		if err != nil { // A create-operation reference is resolved after its durable prerequisite receipt.
			if _, refErr := contract.ObjectAt(op.Target, "issue_ref"); refErr != nil {
				return err
			}
		}
		body := fmt.Sprint(op.Target["body"])
		if op.Target["comment_id"] == nil {
			body += "\n\n" + marker
		} else {
			id, e := contract.PositiveInteger(result["id"])
			if e != nil || id != op.Target["comment_id"] {
				return errors.New("updated governance audit comment changed identity")
			}
		}
		got, err := contract.String(result, "body")
		if err != nil || got != body {
			return errors.New("governance audit comment acknowledgement differs from reviewed body")
		}
		id, err := contract.PositiveInteger(result["id"])
		if err != nil {
			return err
		}
		if number > 0 {
			rawURL, err := contract.Nonempty(result, "html_url")
			if err != nil || !validCommentURL(rawURL, p.Repository, number, id) {
				return errors.New("governance audit comment has foreign URL identity")
			}
		}
	case "project-membership-add":
		if result["clientMutationId"] != nonce {
			return errors.New("Project membership acknowledgement lacks durable mutation identity")
		}
		if _, _, _, _, err := backlogProjectItemAck(result, p.Repository); err != nil {
			return err
		}
	case "project-field-set", "project-field-set-number":
		if result["clientMutationId"] != nonce {
			return errors.New("Project field acknowledgement lacks durable mutation identity")
		}
		item, err := contract.ObjectAt(result, "projectV2Item")
		if err != nil {
			return err
		}
		itemID, err := contract.Nonempty(item, "id")
		if err != nil {
			return err
		}
		if targetID, ok := op.Target["item_id"].(string); ok && targetID != "" && itemID != targetID {
			return errors.New("Project field acknowledgement targets another item")
		}
	default:
		return fmt.Errorf("unsupported governance acknowledgement kind %q", op.Kind)
	}
	return nil
}

func governanceIssueNumberFromReference(target contract.Object) (int64, error) {
	ref, err := contract.ObjectAt(target, "issue_ref")
	if err != nil {
		return 0, errors.New("governance comment has no exact issue reference")
	}
	return contract.PositiveInteger(ref["issue_number"])
}

func governanceProjectAcknowledgement(inventory contract.Object, p contract.Plan, op contract.Operation, ack contract.Object, receipts []contract.Object) (contract.Object, error) {
	if err := (&Governance{}).ValidateAcknowledgement(p, op, ack); err != nil {
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
	graph, _ := contract.ObjectAt(out, "issue_inventory")
	issues, _ := contract.Objects(graph, "issues")
	milestones, _ := contract.Objects(out, "milestones")
	comments, _ := contract.Objects(out, "comments")
	labels, _ := contract.Objects(out, "labels")
	switch op.Kind {
	case "label-create", "label-update":
		label, _ := contract.ObjectAt(providerResult, "label")
		nodeID, _ := contract.Nonempty(label, "id")
		newRow := contract.Object{"node_id": nodeID, "name": op.Target["name"], "color": strings.ToLower(fmt.Sprint(op.Target["color"])), "description": op.Target["description"]}
		filtered := []contract.Object{}
		matched := false
		for _, old := range labels {
			if old["name"] == op.Target["name"] || old["node_id"] == op.Before["node_id"] {
				if op.Kind != "label-update" || !same(old, contract.Object{"node_id": op.Before["node_id"], "name": op.Before["name"], "color": op.Before["color"], "description": op.Before["description"]}) {
					return nil, errors.New("label before-state differs from projected inventory")
				}
				if op.Kind == "label-create" {
					return nil, errors.New("new label collides with projected name or global identity")
				}
				matched = true
				continue
			}
			filtered = append(filtered, old)
		}
		if op.Kind == "label-update" && !matched {
			return nil, errors.New("label update target disappeared from projected inventory")
		}
		labels = append(filtered, newRow)
	case "issue-create":
		issue, err := backlogCreatedIssue(providerResult, p.Repository)
		if err != nil {
			return nil, err
		}
		issue["body"] = stripOperationMarker(fmt.Sprint(issue["body"]))
		for _, old := range issues {
			if old["number"] == issue["number"] || old["title"] == op.Target["title"] {
				return nil, errors.New("created governance issue collides with captured issue inventory")
			}
		}
		if issue["body"] != op.Target["body"] || issue["title"] != op.Target["title"] || !strings.Contains(fmt.Sprint(issue["body"]), fmt.Sprint(op.Target["creation_marker"])) {
			return nil, errors.New("created issue did not retain reviewed creation correlation")
		}
		issues = append(issues, issue)
	case "issue-label-set", "issue-state-set":
		number, _ := contract.PositiveInteger(op.Target["issue_number"])
		issue := backlogIssueByNumber(issues, number)
		if issue == nil || issue["id"] != op.Target["issue_id"] {
			return nil, errors.New("governance issue disappeared or changed identity")
		}
		if op.Kind == "issue-label-set" {
			if !same(issue["labels"], op.Before["labels"]) {
				return nil, errors.New("issue label before-state differs from projected inventory")
			}
			issue["labels"] = op.After["labels"]
		} else {
			if issue["state"] != op.Before["state"] {
				return nil, errors.New("issue state before-state differs from projected inventory")
			}
			issue["state"] = op.After["state"]
		}
	case "milestone-create", "milestone-update":
		milestone, err := backlogCreatedMilestone(providerResult, p.Repository)
		if err != nil {
			return nil, err
		}
		milestone["description"] = stripOperationMarker(fmt.Sprint(milestone["description"]))
		filtered, matched := []contract.Object{}, false
		for _, old := range milestones {
			if old["title"] == op.Target["title"] || old["number"] == milestone["number"] {
				if op.Kind == "milestone-create" || old["number"] != op.Before["number"] || old["id"] != op.Before["id"] || old["node_id"] != op.Before["node_id"] || old["description"] != op.Before["description"] || old["due_on"] != op.Before["due_on"] || old["state"] != op.Before["state"] {
					return nil, errors.New("milestone before-state differs from projected inventory")
				}
				matched = true
				continue
			}
			filtered = append(filtered, old)
		}
		if (op.Kind == "milestone-update") != matched {
			return nil, errors.New("milestone create/update does not match projected existence")
		}
		if milestone["title"] != op.Target["title"] || milestone["description"] != op.Target["description"] {
			return nil, errors.New("milestone ACK projection differs from reviewed content")
		}
		milestones = append(filtered, milestone)
	case "issue-comment-upsert":
		number, _, err := governanceResolveIssueTarget(op.Target, out, p, receipts)
		if err != nil {
			return nil, err
		}
		issue := backlogIssueByNumber(issues, number)
		if issue == nil {
			return nil, errors.New("governance comment target issue is missing")
		}
		comment, err := backlogCreatedComment(providerResult, p.Repository, number)
		if err != nil {
			return nil, err
		}
		comment["body"] = stripOperationMarker(fmt.Sprint(comment["body"]))
		marker := fmt.Sprint(op.Target["marker"])
		var marked contract.Object
		for _, old := range comments {
			if old["issue_number"] == number && strings.Contains(fmt.Sprint(old["body"]), marker) {
				if marked != nil {
					return nil, errors.New("governance audit comment marker is ambiguous")
				}
				marked = old
			}
		}
		if op.Target["comment_id"] == nil {
			if marked != nil || op.Before["comment_id"] != nil {
				return nil, errors.New("new governance comment collides with projected marker")
			}
		} else if marked == nil || marked["id"] != op.Before["comment_id"] || marked["body"] != op.Before["body"] {
			return nil, errors.New("governance comment update before-state differs from projected inventory")
		}
		filtered := []contract.Object{}
		for _, old := range comments {
			if old["issue_number"] == number && old["id"] == op.Target["comment_id"] && op.Target["comment_id"] != nil {
				continue
			}
			if old["issue_number"] == number && strings.Contains(fmt.Sprint(old["body"]), marker) {
				continue
			}
			filtered = append(filtered, old)
		}
		if comment["body"] != op.Target["body"] {
			return nil, errors.New("governance comment ACK differs from reviewed content")
		}
		comments = append(filtered, comment)
	case "project-membership-add":
		projectID, _ := contract.Nonempty(op.Target, "project_id")
		project, err := governanceProjectByID(out, projectID)
		if err != nil {
			return nil, err
		}
		number, nodeID, err := governanceResolveIssueTarget(op.Target, out, p, receipts)
		if err != nil {
			return nil, err
		}
		itemID, fields, gotNumber, gotNode, err := backlogProjectItemAck(providerResult, p.Repository)
		if err != nil {
			return nil, err
		}
		if number != gotNumber || nodeID != gotNode {
			return nil, errors.New("Project membership acknowledgement targets another issue")
		}
		if governanceProjectItem(project, number) != nil {
			return nil, errors.New("Project membership already exists in projected inventory")
		}
		for _, item := range mustObjects(project, "items") {
			if item["number"] == number {
				return nil, errors.New("archived Project membership prevents safe re-add")
			}
		}
		items, _ := contract.Objects(project, "items")
		items = append(items, contract.Object{"item_id": itemID, "number": number, "field_values": fields, "archived": false})
		sort.Slice(items, func(i, j int) bool {
			return governanceNumber(items[i]["number"]) < governanceNumber(items[j]["number"])
		})
		project["items"] = backlogObjectsAsAny(items)
	case "project-field-set", "project-field-set-number":
		projectID, _ := contract.Nonempty(op.Target, "project_id")
		project, err := governanceProjectByID(out, projectID)
		if err != nil {
			return nil, err
		}
		number, _, err := governanceResolveIssueTarget(op.Target, out, p, receipts)
		if err != nil {
			return nil, err
		}
		item := governanceProjectItem(project, number)
		if item == nil {
			return nil, errors.New("Project field item is missing from projected inventory")
		}
		field := fmt.Sprint(op.Target["field_name"])
		itemID, err := contract.Nonempty(item, "item_id")
		if err != nil {
			return nil, err
		}
		projectResult, _ := contract.ObjectAt(providerResult, "projectV2Item")
		if projectResult["id"] != itemID {
			return nil, errors.New("Project field acknowledgement targets another item")
		}
		values, _ := contract.ObjectAt(item, "field_values")
		before := op.Before["value"]
		if membershipID, created := op.Target["membership_operation"].(string); created && membershipID != "" {
			if _, err := governanceCompletedReceipt(receipts, membershipID); err != nil {
				return nil, errors.New("Project field operation lacks its acknowledged membership prerequisite")
			}
			if sentinel, ok := before.(map[string]any); !ok || sentinel["from_membership"] != membershipID {
				return nil, errors.New("Project field membership-derived before-state changed")
			}
		} else {
			current, present := values[field]
			if present != (before != nil) || !same(current, before) {
				return nil, errors.New("Project field before-state differs from projected inventory")
			}
		}
		if op.Kind == "project-field-set-number" {
			values[field] = op.After["value"]
		} else {
			values[field] = op.After["value"]
		}
	default:
		return nil, fmt.Errorf("unsupported governance projection kind %q", op.Kind)
	}
	sort.Slice(issues, func(i, j int) bool {
		return governanceNumber(issues[i]["number"]) < governanceNumber(issues[j]["number"])
	})
	sort.Slice(milestones, func(i, j int) bool {
		return governanceNumber(milestones[i]["number"]) < governanceNumber(milestones[j]["number"])
	})
	sort.Slice(labels, func(i, j int) bool { return labels[i]["name"].(string) < labels[j]["name"].(string) })
	sort.Slice(comments, func(i, j int) bool {
		if governanceNumber(comments[i]["issue_number"]) != governanceNumber(comments[j]["issue_number"]) {
			return governanceNumber(comments[i]["issue_number"]) < governanceNumber(comments[j]["issue_number"])
		}
		return governanceNumber(comments[i]["id"]) < governanceNumber(comments[j]["id"])
	})
	graph["issues"], out["milestones"], out["labels"], out["comments"] = backlogObjectsAsAny(issues), backlogObjectsAsAny(milestones), backlogObjectsAsAny(labels), backlogObjectsAsAny(comments)
	return out, nil
}

func governanceCompletedReceipt(receipts []contract.Object, id string) (contract.Object, error) {
	for _, receipt := range receipts {
		if receipt["id"] == id && receipt["status"] == "completed" {
			return receipt, nil
		}
	}
	return nil, fmt.Errorf("governance prerequisite %q is incomplete", id)
}
