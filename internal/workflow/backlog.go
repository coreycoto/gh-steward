package workflow

import (
	"context"
	"errors"
	"fmt"
	"github.com/coreycoto/gh-steward/internal/contract"
	"strconv"
	"strings"
)

const (
	ReviewBacklogCommand  = "review-backlog-apply"
	QuarterBacklogCommand = "quarter-backlog-apply"
	backlogSchemaVersion  = int64(1)
)

// ProjectScope is an explicit ProjectV2 identity. The title is retained as a
// human-readable selection key; writes bind to ID, owner, host and number.
type ProjectScope struct {
	Host      string
	Owner     string
	OwnerType string
	Number    int64
	ID        string
	Title     string
}

func (p ProjectScope) Object() contract.Object {
	return contract.Object{"host": p.Host, "owner": p.Owner, "owner_type": p.OwnerType, "number": p.Number, "id": p.ID, "title": p.Title}
}

// BacklogInventoryRequest is captured in the reviewed plan and reconstructed
// before every source read. Issues and milestones are always complete; the
// remaining facets are requested only by explicit reviewed intent.
type BacklogInventoryRequest struct {
	Projects             []ProjectScope
	CommentMarkers       []string
	IncludeRelationships bool
}

func (r BacklogInventoryRequest) Object() contract.Object {
	projects := make([]any, 0, len(r.Projects))
	for _, project := range r.Projects {
		projects = append(projects, project.Object())
	}
	markers := make([]any, 0, len(r.CommentMarkers))
	for _, marker := range r.CommentMarkers {
		markers = append(markers, marker)
	}
	return contract.Object{"projects": projects, "comment_markers": markers, "include_relationships": r.IncludeRelationships}
}

type IssueDraft struct {
	Title, Body string
	Labels      []string
}

type IssuePatch struct {
	Title           *string
	Body            *string
	Labels          *[]string
	MilestoneNumber *int64
	ClearMilestone  bool
}

type MilestoneDraft struct{ Title, Description, DueOn string }

type MilestonePatch struct {
	Description *string
	DueOn       *string
}

type ProjectField string

const (
	ProjectStatusField     ProjectField = "Status"
	ProjectPriorityField   ProjectField = "Priority"
	ProjectQueueOrderField ProjectField = "Queue Order"
)

// ProjectFieldValue is a closed union: exactly one of Text, Number or Clear is
// selected. It prevents free-form Project field mutation payloads.
type ProjectFieldValue struct {
	Text   *string
	Number *float64
	Clear  bool
}

type Relationship string

const (
	BlockedBy Relationship = "blocked-by"
	Child     Relationship = "child"
)

// BacklogProvider is the only I/O seam. Inventory returns normalized source
// snapshots; each write returns the exact native response that is journaled
// before any independent after-state read.
type BacklogProvider interface {
	BacklogInventory(context.Context, BacklogInventoryRequest) (contract.Object, error)
	CreateIssue(context.Context, string, IssueDraft) (contract.Object, error)
	UpdateIssue(context.Context, string, int64, IssuePatch) (contract.Object, error)
	CreateMilestone(context.Context, string, MilestoneDraft) (contract.Object, error)
	UpdateMilestone(context.Context, string, int64, MilestonePatch) (contract.Object, error)
	CreateIssueComment(context.Context, string, int64, string) (contract.Object, error)
	UpdateIssueComment(context.Context, string, int64, int64, string) (contract.Object, error)
	AddProjectIssue(context.Context, string, string, string) (contract.Object, error)
	SetProjectField(context.Context, string, string, string, ProjectField, ProjectFieldValue) (contract.Object, error)
	AddRelationship(context.Context, string, int64, int64, Relationship) (contract.Object, error)
	RemoveRelationship(context.Context, string, int64, int64, Relationship) (contract.Object, error)
}

// ReviewBacklogPolicy supplies all taxonomy and Project selection choices.
// Empty or incomplete mappings are rejected; this package supplies no consumer
// defaults.
type ReviewBacklogPolicy struct {
	Project            ProjectScope
	SeverityToPriority map[string]string
	IssueTypeLabels    map[string]string
}

func (p ReviewBacklogPolicy) Object() contract.Object {
	priority := contract.Object{}
	for k, v := range p.SeverityToPriority {
		priority[k] = v
	}
	labels := contract.Object{}
	for k, v := range p.IssueTypeLabels {
		labels[k] = v
	}
	return contract.Object{"project": p.Project.Object(), "severity_to_priority": priority, "issue_type_labels": labels}
}

func validateProjectScope(p ProjectScope, repo contract.Repository) error {
	if !strings.EqualFold(p.Host, repo.Host) || p.Owner == "" || p.Title == "" || p.ID == "" || p.Number < 1 || (p.OwnerType != "User" && p.OwnerType != "Organization") {
		return errors.New("backlog Project scope requires an exact host, owner type, owner, number, ID and title")
	}
	return nil
}

func (p ReviewBacklogPolicy) validate(repo contract.Repository) error {
	if err := validateProjectScope(p.Project, repo); err != nil {
		return err
	}
	if len(p.SeverityToPriority) != 7 || len(p.IssueTypeLabels) != 6 {
		return errors.New("review policy taxonomy maps must contain exactly the supported severity and issue types")
	}
	for _, severity := range []string{"critical", "high", "now", "medium", "next", "low", "later"} {
		priority, ok := p.SeverityToPriority[severity]
		if !ok || (priority != "Now" && priority != "Next" && priority != "Later") {
			return fmt.Errorf("review policy must explicitly map severity %q to Now, Next or Later", severity)
		}
	}
	for _, issueType := range []string{"Initiative", "Epic", "Research", "Enhancement", "Bug", "Maintenance"} {
		if label := p.IssueTypeLabels[issueType]; strings.TrimSpace(label) == "" {
			return fmt.Errorf("review policy must explicitly provide a label for %q", issueType)
		}
	}
	return nil
}

func requestObject(request BacklogInventoryRequest) contract.Object { return request.Object() }

func backlogParseProjectScope(raw contract.Object) (ProjectScope, error) {
	host, e := contract.Nonempty(raw, "host")
	if e != nil {
		return ProjectScope{}, e
	}
	owner, e := contract.Nonempty(raw, "owner")
	if e != nil {
		return ProjectScope{}, e
	}
	typ, e := contract.Nonempty(raw, "owner_type")
	if e != nil {
		return ProjectScope{}, e
	}
	number, e := contract.PositiveInteger(raw["number"])
	if e != nil {
		return ProjectScope{}, e
	}
	id, e := contract.Nonempty(raw, "id")
	if e != nil {
		return ProjectScope{}, e
	}
	title, e := contract.Nonempty(raw, "title")
	if e != nil {
		return ProjectScope{}, e
	}
	return ProjectScope{Host: host, Owner: owner, OwnerType: typ, Number: number, ID: id, Title: title}, nil
}

func parseInventoryRequest(raw contract.Object) (BacklogInventoryRequest, error) {
	projects, err := contract.Objects(raw, "projects")
	if err != nil {
		return BacklogInventoryRequest{}, err
	}
	request := BacklogInventoryRequest{}
	for _, row := range projects {
		scope, err := backlogParseProjectScope(row)
		if err != nil {
			return request, err
		}
		request.Projects = append(request.Projects, scope)
	}
	markers, err := contract.Strings(raw["comment_markers"])
	if err != nil {
		return request, err
	}
	request.CommentMarkers = markers
	request.IncludeRelationships, err = contract.Bool(raw, "include_relationships")
	if err != nil {
		return request, err
	}
	if err := validateInventoryRequest(request); err != nil {
		return request, err
	}
	return request, nil
}

func validateInventoryRequest(r BacklogInventoryRequest) error {
	seen := map[string]bool{}
	for _, p := range r.Projects {
		key := strings.ToLower(p.Host + "/" + p.Owner + "/" + strconv.FormatInt(p.Number, 10))
		if p.Host == "" || p.Owner == "" || (p.OwnerType != "User" && p.OwnerType != "Organization") || p.ID == "" || p.Title == "" || p.Number < 1 || seen[key] {
			return errors.New("inventory Project scopes must be unique and exact")
		}
		seen[key] = true
	}
	seen = map[string]bool{}
	for _, marker := range r.CommentMarkers {
		if marker == "" || seen[marker] {
			return errors.New("inventory comment markers must be nonempty and unique")
		}
		seen[marker] = true
	}
	return nil
}

func parseReviewBacklogPolicy(raw contract.Object, repo contract.Repository) (ReviewBacklogPolicy, error) {
	if len(raw) != 3 {
		return ReviewBacklogPolicy{}, errors.New("review backlog policy has unsupported or missing fields")
	}
	projectRaw, err := contract.ObjectAt(raw, "project")
	if err != nil {
		return ReviewBacklogPolicy{}, err
	}
	project, err := backlogParseProjectScope(projectRaw)
	if err != nil {
		return ReviewBacklogPolicy{}, err
	}
	priorityRaw, err := contract.ObjectAt(raw, "severity_to_priority")
	if err != nil {
		return ReviewBacklogPolicy{}, err
	}
	labelRaw, err := contract.ObjectAt(raw, "issue_type_labels")
	if err != nil {
		return ReviewBacklogPolicy{}, err
	}
	priority, labels := map[string]string{}, map[string]string{}
	for key, value := range priorityRaw {
		text, ok := value.(string)
		if !ok {
			return ReviewBacklogPolicy{}, errors.New("severity policy values must be strings")
		}
		priority[key] = text
	}
	for key, value := range labelRaw {
		text, ok := value.(string)
		if !ok {
			return ReviewBacklogPolicy{}, errors.New("issue type labels must be strings")
		}
		labels[key] = text
	}
	policy := ReviewBacklogPolicy{Project: project, SeverityToPriority: priority, IssueTypeLabels: labels}
	return policy, policy.validate(repo)
}

func sourceProvenance(raw contract.Object, key string) (contract.Object, error) {
	prov, err := contract.ObjectAt(raw, "provenance")
	if err != nil {
		return nil, err
	}
	return contract.ObjectAt(prov, key)
}
