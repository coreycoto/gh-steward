package workflow

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/snapshot"
)

// NativeBacklog binds a reviewed workflow to explicit Project identities.
// Native responses remain unmodified so the adapter can retain exact ACKs.
type NativeBacklog struct {
	Transport *native.Transport
	Projects  []ProjectScope
}

func nativeBacklogScope(scope ProjectScope) native.ProjectScope {
	return native.ProjectScope{Host: scope.Host, Owner: scope.Owner, OwnerType: scope.OwnerType, Number: scope.Number, ID: scope.ID}
}

func (n NativeBacklog) BacklogInventory(ctx context.Context, request BacklogInventoryRequest) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("native backlog requires a transport")
	}
	projects := make([]native.ProjectScope, 0, len(request.Projects))
	for _, scope := range request.Projects {
		if err := validateProjectScope(scope, n.Transport.Repository); err != nil {
			return nil, err
		}
		projects = append(projects, nativeBacklogScope(scope))
	}
	source, err := (snapshot.Service{Reader: n.Transport, Repository: n.Transport.Repository}).BacklogInventory(ctx, projects, request.CommentMarkers, request.IncludeRelationships)
	if err != nil {
		return nil, err
	}
	rows, err := contract.Objects(source, "projects")
	if err != nil || len(rows) != len(request.Projects) {
		return nil, errors.New("native backlog Project inventory has incomplete scope coverage")
	}
	for index, row := range rows {
		metadata, err := contract.ObjectAt(row, "project")
		if err != nil || metadata["title"] != request.Projects[index].Title {
			return nil, errors.New("native backlog Project title differs from the reviewed scope")
		}
	}
	provenance, err := contract.ObjectAt(source, "provenance")
	if err != nil {
		return nil, err
	}
	// The normalized source independently verified these exact identities;
	// retain the workflow's typed spelling rather than the lower-level fields.
	provenance["project_scopes"] = projectScopesAny(request.Projects)
	provenance["comment_markers"] = stringSliceAny(request.CommentMarkers)
	return source, nil
}

func (n NativeBacklog) scope(projectID string) (native.ProjectScope, error) {
	if n.Transport == nil || projectID == "" {
		return native.ProjectScope{}, errors.New("native backlog requires an explicit Project identity")
	}
	var result native.ProjectScope
	found := false
	for _, scope := range n.Projects {
		if scope.ID != projectID {
			continue
		}
		if found {
			return result, errors.New("native backlog Project identity is ambiguous")
		}
		if err := validateProjectScope(scope, n.Transport.Repository); err != nil {
			return result, err
		}
		result, found = nativeBacklogScope(scope), true
	}
	if !found {
		return result, errors.New("Project mutation is outside the reviewed backlog scopes")
	}
	return result, nil
}

func (n NativeBacklog) validateMutation(nonce string) error {
	if n.Transport == nil {
		return errors.New("native backlog requires a transport")
	}
	_, err := native.OperationMarker(nonce)
	return err
}

func (n NativeBacklog) CreateIssue(ctx context.Context, nonce string, draft IssueDraft) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	labels := make([]any, len(draft.Labels))
	for index, name := range draft.Labels {
		labels[index] = name
	}
	return n.Transport.CreateIssue(ctx, contract.Object{"title": draft.Title, "body": draft.Body, "labels": labels}, nonce)
}
func (n NativeBacklog) UpdateIssue(ctx context.Context, nonce string, number int64, patch IssuePatch) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	changes := contract.Object{}
	if patch.Title != nil {
		changes["title"] = *patch.Title
	}
	if patch.Body != nil {
		changes["body"] = *patch.Body
	}
	if patch.Labels != nil {
		labels := make([]any, len(*patch.Labels))
		for index, name := range *patch.Labels {
			labels[index] = name
		}
		changes["labels"] = labels
	}
	if patch.ClearMilestone && patch.MilestoneNumber != nil {
		return nil, errors.New("milestone assignment and clearing are mutually exclusive")
	}
	if patch.ClearMilestone {
		changes["milestone"] = nil
	} else if patch.MilestoneNumber != nil {
		changes["milestone"] = *patch.MilestoneNumber
	}
	return n.Transport.UpdateIssue(ctx, number, changes)
}

func (n NativeBacklog) CreateMilestone(ctx context.Context, nonce string, draft MilestoneDraft) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	var due any
	if draft.DueOn != "" {
		due = draft.DueOn
	}
	return n.Transport.CreateMilestone(ctx, contract.Object{"title": draft.Title, "description": draft.Description, "due_on": due}, nonce)
}
func (n NativeBacklog) UpdateMilestone(ctx context.Context, nonce string, number int64, patch MilestonePatch) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	changes := contract.Object{}
	if patch.Description != nil {
		changes["description"] = *patch.Description
	}
	if patch.DueOn != nil {
		if *patch.DueOn == "" {
			changes["due_on"] = nil
		} else {
			changes["due_on"] = *patch.DueOn
		}
	}
	return n.Transport.UpdateMilestone(ctx, number, changes)
}
func (n NativeBacklog) CreateIssueComment(ctx context.Context, nonce string, number int64, body string) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	return n.Transport.CreateIssueComment(ctx, number, body, nonce)
}
func (n NativeBacklog) UpdateIssueComment(ctx context.Context, nonce string, number, commentID int64, body string) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	return n.Transport.UpdateIssueComment(ctx, number, commentID, body)
}

func (n NativeBacklog) AddProjectIssue(ctx context.Context, nonce, projectID, issueNodeID string) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	scope, err := n.scope(projectID)
	if err != nil {
		return nil, err
	}
	issues, err := (snapshot.Service{Reader: n.Transport, Repository: n.Transport.Repository}).Issues(ctx, "all", false)
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		if issue["id"] != issueNodeID {
			continue
		}
		number, _ := contract.PositiveInteger(issue["number"])
		return n.Transport.AddProjectIssue(ctx, scope, number, nonce)
	}
	return nil, errors.New("Project membership issue node is outside the selected repository")
}

func (n NativeBacklog) SetProjectField(ctx context.Context, nonce, projectID, itemID string, field ProjectField, value ProjectFieldValue) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	scope, err := n.scope(projectID)
	if err != nil {
		return nil, err
	}
	choices := 0
	var scalar any
	if value.Clear {
		choices++
	}
	if value.Text != nil {
		choices++
		scalar = *value.Text
	}
	if value.Number != nil {
		choices++
		if math.IsNaN(*value.Number) || math.IsInf(*value.Number, 0) {
			return nil, errors.New("Project field numeric value must be finite")
		}
		scalar = *value.Number
	}
	if choices != 1 {
		return nil, errors.New("Project field value requires exactly one typed choice")
	}
	project, err := (snapshot.Service{Reader: n.Transport, Repository: n.Transport.Repository}).Project(ctx, scope)
	if err != nil {
		return nil, err
	}
	fields, err := contract.ObjectAt(project, "fields_by_name")
	if err != nil {
		return nil, err
	}
	definition, err := contract.ObjectAt(fields, string(field))
	if err != nil {
		return nil, fmt.Errorf("reviewed Project field %q does not exist", field)
	}
	fieldID, err := contract.Nonempty(definition, "id")
	if err != nil {
		return nil, err
	}
	items, err := contract.Objects(project, "items")
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item["item_id"] == itemID {
			number, _ := contract.PositiveInteger(item["number"])
			return n.Transport.SetProjectField(ctx, native.ProjectItemScope{Project: scope, ItemID: itemID, IssueNumber: number}, fieldID, scalar, nonce)
		}
	}
	return nil, errors.New("Project field item is outside the selected repository and Project")
}

func (n NativeBacklog) AddRelationship(ctx context.Context, nonce string, number, related int64, relation Relationship) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	return n.Transport.AddRelationship(ctx, number, related, string(relation))
}
func (n NativeBacklog) RemoveRelationship(ctx context.Context, nonce string, number, related int64, relation Relationship) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	return n.Transport.RemoveRelationship(ctx, number, related, string(relation))
}
