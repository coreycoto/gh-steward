package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// BacklogMutationsCommand applies an issue-centric, caller-authored set of
// issue, Project, milestone and relationship changes through reviewed v2 plans.
const BacklogMutationsCommand = "backlog-mutations-apply"

// BacklogMutations uses the common durable Backlog adapter and native provider
// seam. Its Operations method accepts the generic mutation command only after
// backlogOperations has re-derived the typed primitives from intent and source.
type BacklogMutations = Backlog

type backlogMutationReference struct {
	kind     string
	issue    int64
	clientID string
}

type backlogMutationProject struct {
	EnsureMembership bool
	Title            string
	Status           *string
	Priority         *string
	QueueOrderSet    bool
	QueueOrder       *float64
}

type backlogMutationEntry struct {
	Index               int
	IssueNumber         int64
	ClientID            string
	Existing            bool
	TitlePresent        bool
	Title               string
	BodyPresent         bool
	Body                string
	LabelsPresent       bool
	Labels              []string
	MilestonePresent    bool
	Milestone           *string
	ProjectPresent      bool
	Project             *backlogMutationProject
	ProjectScope        *ProjectScope
	ParentPresent       bool
	Parent              *backlogMutationReference
	BlockedByPresent    bool
	BlockedBy           []backlogMutationReference
	AllowDuplicateTitle bool
	CreationMarker      string
	CreateOperationID   string
	ProjectMembershipID string
}

// PrepareBacklogMutations captures complete live issue/milestone/label and
// explicitly selected Project evidence. intent is the legacy schema-version-1
// authored object with an issues list; projects are caller-resolved six-field
// ProjectScope identities, never inferred from a consumer title default.
func PrepareBacklogMutations(ctx context.Context, provider BacklogProvider, repo contract.Repository, intent contract.Object, projects []ProjectScope, now time.Time) (contract.Plan, error) {
	if provider == nil {
		return contract.Plan{}, errors.New("backlog mutation preparation requires a provider")
	}
	entries, err := normalizeBacklogMutationIntent(intent)
	if err != nil {
		return contract.Plan{}, err
	}
	requestProjects, err := backlogMutationProjectScopes(entries, projects, repo)
	if err != nil {
		return contract.Plan{}, err
	}
	relationships := backlogMutationTouchesRelationships(entries)
	request := BacklogInventoryRequest{Projects: requestProjects, IncludeRelationships: relationships}
	raw, err := provider.BacklogInventory(ctx, request)
	if err != nil {
		return contract.Plan{}, err
	}
	inventory, err := normalizeBacklogInventory(raw, repo, request)
	if err != nil {
		return contract.Plan{}, err
	}
	if err = validateBacklogMutationIntent(entries, inventory); err != nil {
		return contract.Plan{}, err
	}
	data := contract.Object{"intent": intent, "inventory_request": request.Object(), "inventory": inventory}
	sources := backlogPlanSources(inventory, request)
	plan := contract.Plan{SchemaVersion: contract.MachineSchemaVersion, Command: BacklogMutationsCommand, Repository: repo, CapturedAt: now.UTC().Format(time.RFC3339Nano), Sources: sources, Data: data}
	ops, err := backlogOperations(plan)
	if err != nil {
		return contract.Plan{}, err
	}
	return contract.PreparePlan(BacklogMutationsCommand, repo, sources, data, ops, now)
}

func normalizeBacklogMutationIntent(intent contract.Object) ([]backlogMutationEntry, error) {
	if !sameKeys(intent, keySet("schema_version", "issues")) {
		return nil, errors.New("backlog mutation intent must contain only schema_version and issues")
	}
	version, err := contract.Integer(intent["schema_version"])
	if err != nil || version != backlogSchemaVersion {
		return nil, fmt.Errorf("backlog mutation intent must declare schema_version %d", backlogSchemaVersion)
	}
	rows, err := contract.Objects(intent, "issues")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("backlog mutation intent must contain at least one issue entry")
	}
	entries := make([]backlogMutationEntry, 0, len(rows))
	for index, raw := range rows {
		entry, err := normalizeBacklogMutationEntry(raw, index)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func normalizeBacklogMutationEntry(raw contract.Object, index int) (backlogMutationEntry, error) {
	prefix := fmt.Sprintf("issues[%d]", index)
	allowed := keySet("issue_number", "client_id", "title", "body", "labels", "milestone", "project", "relationships", "allow_duplicate_title")
	for key := range raw {
		if !allowed[key] {
			return backlogMutationEntry{}, fmt.Errorf("%s contains unsupported field %q", prefix, key)
		}
	}
	e := backlogMutationEntry{Index: index, Labels: []string{}}
	if value, exists := raw["allow_duplicate_title"]; exists {
		flag, ok := value.(bool)
		if !ok {
			return e, fmt.Errorf("%s.allow_duplicate_title must be a boolean", prefix)
		}
		e.AllowDuplicateTitle = flag
	}
	if value, exists := raw["issue_number"]; exists && value != nil {
		var err error
		e.IssueNumber, err = contract.PositiveInteger(value)
		if err != nil {
			return e, fmt.Errorf("%s.issue_number must be a positive integer", prefix)
		}
		e.Existing = true
	}
	if value, exists := raw["client_id"]; exists && value != nil {
		clientID, ok := value.(string)
		if !ok {
			return e, fmt.Errorf("%s.client_id must be a non-empty string", prefix)
		}
		e.ClientID = strings.TrimSpace(clientID)
	}
	if e.Existing && e.ClientID != "" {
		return e, fmt.Errorf("%s cannot combine issue_number and client_id", prefix)
	}
	if e.Existing == (e.ClientID != "") {
		return e, fmt.Errorf("%s must set issue_number for an existing issue or client_id for a new issue", prefix)
	}
	if value, exists := raw["title"]; exists {
		title, ok := value.(string)
		if !ok || strings.TrimSpace(title) == "" {
			return e, fmt.Errorf("%s.title must be a non-empty string when present", prefix)
		}
		e.TitlePresent, e.Title = true, strings.TrimSpace(title)
	}
	if value, exists := raw["body"]; exists {
		body, ok := value.(string)
		if !ok {
			return e, fmt.Errorf("%s.body must be a string when present", prefix)
		}
		e.BodyPresent, e.Body = true, strings.TrimRightFunc(body, unicode.IsSpace)+"\n"
	}
	if value, exists := raw["labels"]; exists {
		labels, err := contract.Strings(value)
		if err != nil {
			return e, fmt.Errorf("%s.labels must be a list of strings", prefix)
		}
		seen := map[string]bool{}
		for i := range labels {
			labels[i] = strings.TrimSpace(labels[i])
			if labels[i] == "" || seen[labels[i]] {
				return e, fmt.Errorf("%s.labels must contain unique non-empty strings", prefix)
			}
			seen[labels[i]] = true
		}
		sort.Strings(labels)
		e.LabelsPresent, e.Labels = true, labels
	}
	if value, exists := raw["milestone"]; exists {
		e.MilestonePresent = true
		if value != nil {
			name, ok := value.(string)
			if !ok || strings.TrimSpace(name) == "" {
				return e, fmt.Errorf("%s.milestone must be a non-empty string or null", prefix)
			}
			name = strings.TrimSpace(name)
			e.Milestone = &name
		}
	}
	if value, exists := raw["project"]; exists {
		projectRaw, ok := value.(map[string]any)
		if !ok {
			return e, fmt.Errorf("%s.project must be an object when present", prefix)
		}
		project, err := normalizeBacklogMutationProject(projectRaw, prefix+".project")
		if err != nil {
			return e, err
		}
		e.ProjectPresent, e.Project = true, project
	}
	if value, exists := raw["relationships"]; exists {
		relationships, ok := value.(map[string]any)
		if !ok {
			return e, fmt.Errorf("%s.relationships must be an object when present", prefix)
		}
		for key := range relationships {
			if key != "parent" && key != "blocked_by" {
				return e, fmt.Errorf("%s.relationships contains unsupported field %q", prefix, key)
			}
		}
		if value, exists := relationships["parent"]; exists {
			ref, err := normalizeBacklogMutationReference(value, true, prefix+".relationships.parent")
			if err != nil {
				return e, err
			}
			e.ParentPresent, e.Parent = true, ref
		}
		if value, exists := relationships["blocked_by"]; exists {
			rawRefs, ok := value.([]any)
			if !ok {
				return e, fmt.Errorf("%s.relationships.blocked_by must be a list when present", prefix)
			}
			e.BlockedByPresent = true
			seen := map[string]bool{}
			for i, item := range rawRefs {
				ref, err := normalizeBacklogMutationReference(item, false, fmt.Sprintf("%s.relationships.blocked_by[%d]", prefix, i))
				if err != nil {
					return e, err
				}
				key := backlogMutationReferenceKey(*ref)
				if seen[key] {
					return e, fmt.Errorf("%s.relationships.blocked_by must not contain duplicates", prefix)
				}
				seen[key] = true
				e.BlockedBy = append(e.BlockedBy, *ref)
			}
			sort.Slice(e.BlockedBy, func(i, j int) bool {
				return backlogMutationReferenceKey(e.BlockedBy[i]) < backlogMutationReferenceKey(e.BlockedBy[j])
			})
		}
		if !e.ParentPresent && !e.BlockedByPresent {
			return e, fmt.Errorf("%s.relationships does not request a relationship change", prefix)
		}
	}
	if !e.Existing && (!e.TitlePresent || !e.BodyPresent) {
		return e, fmt.Errorf("%s must set title and body when creating a new issue", prefix)
	}
	if e.Existing && !e.TitlePresent && !e.BodyPresent && !e.LabelsPresent && !e.MilestonePresent && !e.ProjectPresent && !e.ParentPresent && !e.BlockedByPresent {
		return e, fmt.Errorf("%s does not request any changes", prefix)
	}
	return e, nil
}

func normalizeBacklogMutationProject(raw contract.Object, field string) (*backlogMutationProject, error) {
	allowed := keySet("ensure_membership", "title", "status", "priority", "queue_order")
	for key := range raw {
		if !allowed[key] {
			return nil, fmt.Errorf("%s contains unsupported field %q", field, key)
		}
	}
	out := &backlogMutationProject{}
	if value, exists := raw["ensure_membership"]; exists {
		flag, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("%s.ensure_membership must be a boolean", field)
		}
		out.EnsureMembership = flag
	}
	if value, exists := raw["title"]; exists && value != nil {
		title, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%s.title must be a string or null", field)
		}
		out.Title = strings.TrimSpace(title)
	}
	if value, exists := raw["status"]; exists && value != nil {
		status, ok := value.(string)
		if !ok || (status != "Todo" && status != "In Progress" && status != "Done") {
			return nil, fmt.Errorf("%s.status must be Todo, In Progress or Done", field)
		}
		out.Status = &status
	}
	if value, exists := raw["priority"]; exists && value != nil {
		priority, ok := value.(string)
		if !ok || (priority != "Now" && priority != "Next" && priority != "Later") {
			return nil, fmt.Errorf("%s.priority must be Now, Next or Later", field)
		}
		out.Priority = &priority
	}
	if value, exists := raw["queue_order"]; exists {
		out.QueueOrderSet = true
		if value != nil {
			number, err := contract.Number(value)
			if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
				return nil, fmt.Errorf("%s.queue_order must be a finite number or null", field)
			}
			out.QueueOrder = &number
		}
		if out.Priority == nil {
			return nil, fmt.Errorf("%s.queue_order requires priority", field)
		}
	}
	if out.Status != nil || out.Priority != nil || out.QueueOrderSet {
		out.EnsureMembership = true
	}
	if !out.EnsureMembership && out.Status == nil && out.Priority == nil && !out.QueueOrderSet {
		return nil, fmt.Errorf("%s does not request a Project change", field)
	}
	return out, nil
}

func normalizeBacklogMutationReference(raw any, allowClear bool, field string) (*backlogMutationReference, error) {
	if raw == nil {
		if allowClear {
			return nil, nil
		}
		return nil, fmt.Errorf("%s does not allow null references", field)
	}
	object, ok := raw.(map[string]any)
	if !ok || len(object) == 0 {
		return nil, fmt.Errorf("%s must be null or an issue_number/client_id object", field)
	}
	for key := range object {
		if key != "issue_number" && key != "client_id" {
			return nil, fmt.Errorf("%s contains unsupported repository-qualified reference field %q", field, key)
		}
	}
	if value, exists := object["issue_number"]; exists && value != nil {
		if _, hasClient := object["client_id"]; hasClient {
			return nil, fmt.Errorf("%s must set exactly one of issue_number or client_id", field)
		}
		number, err := contract.PositiveInteger(value)
		if err != nil {
			return nil, fmt.Errorf("%s.issue_number must be a positive integer", field)
		}
		return &backlogMutationReference{kind: "issue_number", issue: number}, nil
	}
	if value, exists := object["client_id"]; exists {
		clientID, ok := value.(string)
		clientID = strings.TrimSpace(clientID)
		if ok && clientID != "" && object["issue_number"] == nil {
			return &backlogMutationReference{kind: "client_id", clientID: clientID}, nil
		}
	}
	return nil, fmt.Errorf("%s must set exactly one of issue_number or client_id", field)
}

func backlogMutationReferenceKey(ref backlogMutationReference) string {
	if ref.kind == "issue_number" {
		return "issue:" + fmt.Sprint(ref.issue)
	}
	return "client:" + ref.clientID
}

func backlogMutationProjectScopes(entries []backlogMutationEntry, supplied []ProjectScope, repo contract.Repository) ([]ProjectScope, error) {
	requested := map[string]ProjectScope{}
	for index := range entries {
		e := &entries[index]
		if !e.ProjectPresent || e.Project == nil {
			continue
		}
		var matches []ProjectScope
		for _, scope := range supplied {
			if e.Project.Title == "" || scope.Title == e.Project.Title {
				matches = append(matches, scope)
			}
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("issue entry %d Project selection must resolve to one explicit full Project scope", e.Index)
		}
		scope := matches[0]
		if err := validateProjectScope(scope, repo); err != nil {
			return nil, err
		}
		key := strings.ToLower(scope.Host + "/" + scope.Owner + "/" + fmt.Sprint(scope.Number))
		requested[key] = scope
		e.ProjectScope = &scope
	}
	if len(requested) != len(supplied) {
		return nil, errors.New("explicit Project scopes must exactly cover Project changes in the authored payload")
	}
	out := make([]ProjectScope, 0, len(requested))
	for _, scope := range requested {
		out = append(out, scope)
	}
	sort.Slice(out, func(i, j int) bool {
		if strings.ToLower(out[i].Owner) != strings.ToLower(out[j].Owner) {
			return strings.ToLower(out[i].Owner) < strings.ToLower(out[j].Owner)
		}
		return out[i].Number < out[j].Number
	})
	if err := validateInventoryRequest(BacklogInventoryRequest{Projects: out}); err != nil {
		return nil, err
	}
	return out, nil
}

func backlogMutationTouchesRelationships(entries []backlogMutationEntry) bool {
	for _, e := range entries {
		if e.ParentPresent || e.BlockedByPresent {
			return true
		}
	}
	return false
}

func backlogMutationFieldOperationID(index int, field, suffix string) string {
	id := backlogMutationEntryID(index) + ":issue:" + field
	if suffix != "" {
		id += ":" + suffix
	}
	return id
}

func backlogMutationLabelDigest(label string) string {
	digest := sha256.Sum256([]byte(label))
	return hex.EncodeToString(digest[:])
}
