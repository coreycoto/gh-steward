package workflow

import (
	"context"
	"errors"
	"fmt"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/snapshot"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
)

func mustObjects(o contract.Object, key string) []contract.Object {
	rows, _ := contract.Objects(o, key)
	return rows
}

func stringPtr(v string) *string { return &v }

func floatPtr(v float64) *float64 { return &v }

func fieldSlug(name string) string { return strings.ToLower(strings.ReplaceAll(name, " ", "-")) }

func stripOperationMarker(body string) string {
	markerStart := strings.LastIndex(body, "<!-- gh-steward:operation:")
	if markerStart < 0 {
		return body
	}
	markerEnd := strings.Index(body[markerStart:], " -->")
	if markerEnd < 0 {
		return body
	}
	if strings.TrimSpace(body[markerStart+markerEnd+len(" -->"):]) != "" {
		return body
	}
	prefix := strings.TrimRight(body[:markerStart], "\n")
	return prefix + "\n"
}

func backlogRelationshipTarget(issueRef contract.Object, related int64, kind Relationship) contract.Object {
	target := contract.Object{"relation": string(kind)}
	if kind == BlockedBy {
		target["issue_ref"], target["related_issue_number"] = issueRef, related
		return target
	}
	if kind == Child {
		target["issue_number"], target["related_issue_ref"] = related, issueRef
		return target
	}
	target["issue_ref"], target["related_issue_number"] = issueRef, related
	return target
}

func backlogFieldDefinition(project contract.Object, name string) (contract.Object, error) {
	fields, err := contract.ObjectAt(project, "fields_by_name")
	if err != nil {
		return nil, err
	}
	return contract.ObjectAt(fields, name)
}

func validateProjectField(project contract.Object, name string, value ProjectFieldValue) error {
	def, err := backlogFieldDefinition(project, name)
	if err != nil {
		return fmt.Errorf("review Project is missing %q field definition", name)
	}
	if err := validateProjectFieldInventory(project); err != nil {
		return fmt.Errorf("review Project field inventory is invalid: %w", err)
	}
	kind, err := contract.Nonempty(def, "data_type")
	if err != nil {
		return err
	}
	selected := 0
	if value.Text != nil {
		selected++
	}
	if value.Number != nil {
		selected++
	}
	if value.Clear {
		selected++
	}
	if selected != 1 {
		return errors.New("Project field value must select exactly one typed representation")
	}
	if value.Text != nil {
		if kind != "SINGLE_SELECT" {
			return fmt.Errorf("Project field %q must be a single-select field", name)
		}
		options, err := contract.ObjectAt(def, "options_by_name")
		if err != nil {
			return err
		}
		if _, found := options[*value.Text]; !found {
			return fmt.Errorf("Project field %q does not define option %q", name, *value.Text)
		}
	}
	if value.Number != nil {
		if kind != "NUMBER" || math.IsNaN(*value.Number) || math.IsInf(*value.Number, 0) {
			return fmt.Errorf("Project field %q must be a finite numeric field", name)
		}
	}
	if value.Clear && (name != "Queue Order" || kind != "NUMBER") {
		return fmt.Errorf("Project field %q cannot be cleared under review policy", name)
	}
	return nil
}

func backlogObjectsAsAny(rows []contract.Object) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

func backlogIssueByNumber(rows []contract.Object, number int64) contract.Object {
	for _, row := range rows {
		found, err := contract.PositiveInteger(row["number"])
		if err == nil && found == number {
			return row
		}
	}
	return nil
}

func backlogAppendUniqueNumber(values []any, n int64) []any {
	for _, v := range values {
		old, e := contract.PositiveInteger(v)
		if e == nil && old == n {
			return values
		}
	}
	values = append(values, n)
	sort.Slice(values, func(i, j int) bool {
		a, _ := contract.PositiveInteger(values[i])
		b, _ := contract.PositiveInteger(values[j])
		return a < b
	})
	return values
}

func backlogRemoveNumber(values []any, n int64) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		current, err := contract.PositiveInteger(value)
		if err != nil || current != n {
			out = append(out, value)
		}
	}
	return out
}

func backlogCompletedPrefix(receipts []contract.Object, exclude string) []contract.Object {
	out := []contract.Object{}
	for _, r := range receipts {
		if r["status"] == "completed" && r["id"] != exclude {
			out = append(out, r)
		}
	}
	return out
}

func backlogReceiptByID(receipts []contract.Object, id string) (contract.Object, error) {
	var found contract.Object
	for _, receipt := range receipts {
		if receipt["id"] != id || receipt["status"] != "completed" {
			continue
		}
		if found != nil {
			return nil, errors.New("backlog operation has duplicate completed receipts")
		}
		found = receipt
	}
	if found == nil {
		return nil, fmt.Errorf("backlog prerequisite %q has no completed receipt", id)
	}
	return found, nil
}

func backlogResolveIssueIdentity(ref contract.Object, p contract.Plan, receipts []contract.Object) (int64, string, error) {
	if number, err := contract.PositiveInteger(ref["issue_number"]); err == nil {
		inventory, err := contract.ObjectAt(p.Data, "inventory")
		if err != nil {
			return 0, "", err
		}
		graph, err := contract.ObjectAt(inventory, "issue_inventory")
		if err != nil {
			return 0, "", err
		}
		issue := backlogIssueByNumber(mustObjects(graph, "issues"), number)
		if issue == nil {
			return 0, "", errors.New("reviewed issue reference is absent from complete inventory")
		}
		id, err := contract.Nonempty(issue, "id")
		return number, id, err
	}
	createdBy, err := contract.Nonempty(ref, "created_by")
	if err != nil {
		return 0, "", errors.New("issue reference needs a positive issue number or prior create operation")
	}
	var createOp *contract.Operation
	ops, err := backlogOperations(p)
	if err != nil {
		return 0, "", err
	}
	for i := range ops {
		if ops[i].ID == createdBy && ops[i].Kind == "issue-create" {
			createOp = &ops[i]
			break
		}
	}
	if createOp == nil {
		return 0, "", errors.New("issue reference names no derived create primitive")
	}
	receipt, err := backlogReceiptByID(receipts, createdBy)
	if err != nil {
		return 0, "", err
	}
	result, err := contract.ObjectAt(receipt, "result")
	if err != nil {
		return 0, "", err
	}
	if err = (&Backlog{}).ValidateReceipt(p, *createOp, result); err != nil {
		return 0, "", err
	}
	provider, err := contract.ObjectAt(result, "provider_result")
	if err != nil {
		return 0, "", err
	}
	number, err := contract.PositiveInteger(provider["number"])
	if err != nil {
		return 0, "", err
	}
	nodeID, err := contract.Nonempty(provider, "node_id")
	return number, nodeID, err
}

func backlogResolveItemID(target contract.Object, p contract.Plan, receipts []contract.Object) (string, error) {
	ref, err := contract.ObjectAt(target, "item_ref")
	if err != nil {
		return "", err
	}
	if id, err := contract.Nonempty(ref, "item_id"); err == nil {
		return id, nil
	}
	operationID, err := contract.Nonempty(ref, "from_operation")
	if err != nil {
		return "", errors.New("reviewed Project item reference is malformed")
	}
	var member *contract.Operation
	ops, err := backlogOperations(p)
	if err != nil {
		return "", err
	}
	for i := range ops {
		if ops[i].ID == operationID && ops[i].Kind == "project-membership-add" {
			member = &ops[i]
			break
		}
	}
	if member == nil {
		return "", errors.New("Project item reference names no derived membership primitive")
	}
	receipt, err := backlogReceiptByID(receipts, operationID)
	if err != nil {
		return "", err
	}
	result, err := contract.ObjectAt(receipt, "result")
	if err != nil {
		return "", err
	}
	if err = (&Backlog{}).ValidateReceipt(p, *member, result); err != nil {
		return "", err
	}
	raw, err := contract.ObjectAt(result, "provider_result")
	if err != nil {
		return "", err
	}
	itemID, err := contract.Nonempty(raw, "id")
	if err != nil {
		return "", err
	}
	return itemID, nil
}

func backlogResolveRelationshipEndpoints(target contract.Object, p contract.Plan, receipts []contract.Object) (int64, int64, string, error) {
	kind, err := contract.Nonempty(target, "relation")
	if err != nil {
		return 0, 0, "", err
	}
	if sourceRef, sourceOK := target["issue_ref"].(map[string]any); sourceOK {
		if relatedRef, relatedOK := target["related_issue_ref"].(map[string]any); relatedOK {
			source, _, err := backlogResolveIssueIdentity(sourceRef, p, receipts)
			if err != nil {
				return 0, 0, "", err
			}
			related, _, err := backlogResolveIssueIdentity(relatedRef, p, receipts)
			if err != nil || source == related {
				return 0, 0, "", errors.New("relationship endpoints must identify distinct reviewed issues")
			}
			if kind != string(BlockedBy) && kind != string(Child) {
				return 0, 0, "", errors.New("unsupported backlog relationship kind")
			}
			return source, related, kind, nil
		}
	}
	if kind == string(BlockedBy) {
		ref, err := contract.ObjectAt(target, "issue_ref")
		if err != nil {
			return 0, 0, "", err
		}
		issue, _, err := backlogResolveIssueIdentity(ref, p, receipts)
		if err != nil {
			return 0, 0, "", err
		}
		related, err := contract.PositiveInteger(target["related_issue_number"])
		return issue, related, kind, err
	}
	if kind == string(Child) {
		parent, err := contract.PositiveInteger(target["issue_number"])
		if err != nil {
			return 0, 0, "", err
		}
		ref, err := contract.ObjectAt(target, "related_issue_ref")
		if err != nil {
			return 0, 0, "", err
		}
		child, _, err := backlogResolveIssueIdentity(ref, p, receipts)
		return parent, child, kind, err
	}
	return 0, 0, "", errors.New("unsupported backlog relationship kind")
}

func backlogCreatedIssue(raw contract.Object, repo contract.Repository) (contract.Object, error) {
	copy, err := contract.Clone(raw)
	if err != nil {
		return nil, err
	}
	nodeID, err := contract.Nonempty(copy, "node_id")
	if err != nil {
		return nil, err
	}
	copy["id"] = nodeID
	url, err := contract.Nonempty(copy, "html_url")
	if err != nil {
		return nil, err
	}
	copy["url"] = url
	if copy["body"] == nil {
		copy["body"] = ""
	}
	// REST mutation responses return labels as an array while the reusable
	// issue snapshot validator consumes a complete GraphQL connection. Preserve
	// the provider response in the ACK and adapt only this validation copy.
	if labels, ok := copy["labels"].([]any); ok {
		nodes := make([]any, 0, len(labels))
		for _, rawLabel := range labels {
			label, ok := rawLabel.(map[string]any)
			if !ok {
				return nil, errors.New("issue acknowledgement label is malformed")
			}
			name, err := contract.Nonempty(label, "name")
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, contract.Object{"name": name})
		}
		copy["labels"] = contract.Object{"nodes": nodes, "pageInfo": contract.Object{"hasNextPage": false, "endCursor": nil}}
	}
	state, err := contract.Nonempty(copy, "state")
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(state) {
	case "open":
		copy["state"] = "OPEN"
	case "closed":
		copy["state"] = "CLOSED"
	default:
		return nil, errors.New("issue acknowledgement has an invalid state")
	}
	normalized, err := snapshot.NormalizeIssue(copy, repo, false)
	if err != nil {
		return nil, err
	}
	normalized["blocked_by_numbers"], normalized["parent_number"], normalized["child_numbers"] = []any{}, nil, []any{}
	return normalized, nil
}

func backlogCreatedComment(raw contract.Object, repo contract.Repository, issue int64) (contract.Object, error) {
	id, err := contract.PositiveInteger(raw["id"])
	if err != nil {
		return nil, err
	}
	body, err := contract.String(raw, "body")
	if err != nil {
		return nil, err
	}
	link, err := contract.Nonempty(raw, "html_url")
	if err != nil || !validCommentURL(link, repo, issue, id) {
		return nil, errors.New("comment acknowledgement URL does not match repository, issue and comment identity")
	}
	return contract.Object{"id": id, "issue_number": issue, "body": body, "url": link}, nil
}

func backlogCreatedMilestone(raw contract.Object, repo contract.Repository) (contract.Object, error) {
	return snapshot.NormalizeMilestone(raw, repo)
}

func backlogProjectItemAck(raw contract.Object, repo contract.Repository) (string, contract.Object, int64, string, error) {
	itemID, err := contract.Nonempty(raw, "id")
	if err != nil {
		return "", nil, 0, "", err
	}
	archived, err := contract.Bool(raw, "isArchived")
	if err != nil || archived {
		return "", nil, 0, "", errors.New("new Project membership acknowledgement is archived or malformed")
	}
	content, err := contract.ObjectAt(raw, "content")
	if err != nil {
		return "", nil, 0, "", err
	}
	number, err := contract.PositiveInteger(content["number"])
	if err != nil {
		return "", nil, 0, "", err
	}
	nodeID, err := contract.Nonempty(content, "id")
	if err != nil {
		return "", nil, 0, "", err
	}
	url, err := contract.Nonempty(content, "url")
	if err != nil || !validIssueWebURL(url, repo, number) {
		return "", nil, 0, "", errors.New("Project membership content URL has foreign identity")
	}
	fields, err := snapshot.NormalizeFieldValues(raw)
	if err != nil {
		return "", nil, 0, "", err
	}
	return itemID, fields, number, nodeID, nil
}

func validIssueWebURL(raw string, repo contract.Repository, number int64) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && (u.Port() == "" || u.Port() == "443") && u.RawQuery == "" && u.Fragment == "" && strings.EqualFold(u.Hostname(), repo.Host) && strings.EqualFold(u.EscapedPath(), fmt.Sprintf("/%s/issues/%d", repo.FullName(), number))
}

func backlogExpectedDueOn(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, errors.New("reviewed milestone due date must be string or null")
	}
	stamp, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return nil, errors.New("reviewed milestone due date is not RFC3339")
	}
	return stamp.UTC().Format(time.RFC3339Nano), nil
}

func validateBacklogProviderResult(p contract.Plan, op contract.Operation, nonce string, result contract.Object) error {
	if result == nil {
		return errors.New("native backlog mutation returned no acknowledgement object")
	}
	marker, err := native.OperationMarker(nonce)
	if err != nil {
		return err
	}
	switch op.Kind {
	case "issue-create":
		issue, err := backlogCreatedIssue(result, p.Repository)
		if err != nil {
			return err
		}
		body, _ := contract.String(op.Target, "body")
		labels, err := contract.Strings(op.Target["labels"])
		if err != nil {
			return err
		}
		wantBody := body + "\n\n" + marker
		if issue["title"] != op.Target["title"] || issue["body"] != wantBody || issue["state"] != "OPEN" || issue["milestone"] != nil || !same(issue["labels"], stringSliceAny(labels)) {
			return errors.New("issue create acknowledgement differs from reviewed title, body, labels or open state")
		}
	case "issue-comment-upsert":
		issue, err := contract.PositiveInteger(op.Target["issue_number"])
		if err != nil {
			return err
		}
		body, err := contract.String(op.Target, "body")
		if err != nil {
			return err
		}
		wantID, hasID := op.Target["comment_id"]
		if hasID && wantID != nil {
			id, err := contract.PositiveInteger(wantID)
			if err != nil {
				return err
			}
			got, err := contract.PositiveInteger(result["id"])
			if err != nil || got != id {
				return errors.New("updated rationale comment acknowledgement changed comment identity")
			}
		} else {
			body += "\n\n" + marker
		}
		gotBody, err := contract.String(result, "body")
		if err != nil || gotBody != body {
			return errors.New("comment acknowledgement differs from reviewed body")
		}
		id, err := contract.PositiveInteger(result["id"])
		if err != nil {
			return err
		}
		rawURL, err := contract.Nonempty(result, "html_url")
		if err != nil || !validCommentURL(rawURL, p.Repository, issue, id) {
			return errors.New("comment acknowledgement has foreign identity")
		}
	case "project-membership-add":
		if result["clientMutationId"] != nonce {
			return errors.New("Project membership acknowledgement lacks the durable operation identity")
		}
		if _, _, _, _, err = backlogProjectItemAck(result, p.Repository); err != nil {
			return err
		}
	case "project-field-set":
		if result["clientMutationId"] != nonce {
			return errors.New("Project field acknowledgement lacks the durable operation identity")
		}
		item, err := contract.ObjectAt(result, "projectV2Item")
		if err != nil {
			return err
		}
		if _, err = contract.Nonempty(item, "id"); err != nil {
			return err
		}
	case "issue-milestone-set":
		issue, err := backlogCreatedIssue(result, p.Repository)
		if err != nil {
			return err
		}
		number, err := contract.PositiveInteger(op.Target["issue_number"])
		if err != nil {
			return err
		}
		node, err := contract.Nonempty(op.Target, "issue_id")
		if err != nil {
			return err
		}
		if issue["number"] != number || issue["id"] != node || issue["state"] != "OPEN" || issue["milestone"] != op.After["milestone"] {
			return errors.New("issue milestone acknowledgement differs from reviewed issue and assignment")
		}
	case "issue-update":
		issue, err := backlogCreatedIssue(result, p.Repository)
		if err != nil {
			return err
		}
		field, err := contract.Nonempty(op.Target, "field")
		if err != nil || (field != "title" && field != "body" && field != "labels" && field != "milestone") {
			return errors.New("issue update acknowledgement has an unsupported field")
		}
		after, err := contract.ObjectAt(op.After, "issue")
		if err != nil || !same(issue[field], after[field]) {
			return errors.New("issue update acknowledgement differs from its reviewed field value")
		}
		before, err := contract.ObjectAt(op.Before, "issue")
		if err != nil {
			return err
		}
		if state, exists := before["state"]; exists && issue["state"] != state {
			return errors.New("issue update acknowledgement changed issue state")
		}
		if id, exists := before["id"]; exists && issue["id"] != id {
			return errors.New("issue update acknowledgement changed issue identity")
		}
		ref, err := contract.ObjectAt(op.Target, "issue_ref")
		if err != nil {
			return err
		}
		if creatorID, created := ref["created_by"].(string); created {
			var creationMarker string
			for _, creator := range p.Operations {
				if creator.ID == creatorID && creator.Kind == "issue-create" {
					creationMarker, _ = creator.Target["creation_marker"].(string)
				}
			}
			if creationMarker == "" || !strings.Contains(fmt.Sprint(issue["body"]), creationMarker) {
				return errors.New("issue update acknowledgement does not identify the reviewed created issue")
			}
		}
		if number, exists := op.Target["issue_number"]; exists {
			wantNumber, err := contract.PositiveInteger(number)
			if err != nil || issue["number"] != wantNumber {
				return errors.New("issue update acknowledgement changed issue number")
			}
		}
	case "milestone-create", "milestone-update":
		milestone, err := backlogCreatedMilestone(result, p.Repository)
		if err != nil {
			return err
		}
		description, err := contract.String(op.Target, "description")
		if err != nil {
			return err
		}
		if op.Kind == "milestone-create" {
			description += "\n\n" + marker
		}
		due, err := backlogExpectedDueOn(op.Target["due_on"])
		if err != nil {
			return err
		}
		if milestone["title"] != op.Target["title"] || milestone["description"] != description || milestone["due_on"] != due || milestone["state"] != "open" {
			return errors.New("milestone acknowledgement differs from reviewed title, description, due date or state")
		}
		if op.Kind == "milestone-update" {
			number, err := contract.PositiveInteger(op.Before["number"])
			if err != nil {
				return err
			}
			if milestone["number"] != number || milestone["id"] != op.Before["id"] || milestone["node_id"] != op.Before["node_id"] {
				return errors.New("updated milestone acknowledgement changed immutable identity")
			}
		}
	case "issue-relationship-add", "issue-relationship-remove":
		if result["acknowledged"] != true {
			return errors.New("relationship provider response did not acknowledge the mutation")
		}
	default:
		return fmt.Errorf("unsupported backlog provider acknowledgement kind %q", op.Kind)
	}
	return nil
}

func (a Backlog) dispatchProvider(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, error) {
	ops, err := backlogOperations(p)
	if err != nil {
		return nil, err
	}
	if !containsOperation(ops, op) {
		return nil, errors.New("backlog dispatch primitive is outside derived plan")
	}
	expected, err := backlogExpectedInventory(p, receipts)
	if err != nil {
		return nil, err
	}
	switch op.Kind {
	case "issue-create":
		title, err := contract.Nonempty(op.Target, "title")
		if err != nil {
			return nil, err
		}
		body, err := contract.String(op.Target, "body")
		if err != nil {
			return nil, err
		}
		labels, err := contract.Strings(op.Target["labels"])
		if err != nil {
			return nil, err
		}
		return a.Provider.CreateIssue(ctx, nonce, IssueDraft{Title: title, Body: body, Labels: labels})
	case "issue-comment-upsert":
		issue, err := contract.PositiveInteger(op.Target["issue_number"])
		if err != nil {
			return nil, err
		}
		graph, _ := contract.ObjectAt(expected, "issue_inventory")
		current := backlogIssueByNumber(mustObjects(graph, "issues"), issue)
		if current == nil || current["id"] != op.Target["issue_id"] || current["state"] != op.Before["issue_state"] {
			return nil, fmt.Errorf("comment target issue identity or state changed before dispatch: issue=%#v expected_id=%#v expected_state=%#v", current, op.Target["issue_id"], op.Before["issue_state"])
		}
		body, err := contract.String(op.Target, "body")
		if err != nil {
			return nil, err
		}
		if id, ok := op.Target["comment_id"]; ok && id != nil {
			commentID, err := contract.PositiveInteger(id)
			if err != nil {
				return nil, err
			}
			return a.Provider.UpdateIssueComment(ctx, nonce, issue, commentID, body)
		}
		return a.Provider.CreateIssueComment(ctx, nonce, issue, body)
	case "milestone-create":
		title, err := contract.Nonempty(op.Target, "title")
		if err != nil {
			return nil, err
		}
		description, err := contract.String(op.Target, "description")
		if err != nil {
			return nil, err
		}
		due, err := contract.String(op.Target, "due_on")
		if err != nil {
			return nil, err
		}
		return a.Provider.CreateMilestone(ctx, nonce, MilestoneDraft{Title: title, Description: description, DueOn: due})
	case "milestone-update":
		number, err := contract.PositiveInteger(op.Target["number"])
		if err != nil {
			return nil, err
		}
		description, err := contract.String(op.Target, "description")
		if err != nil {
			return nil, err
		}
		due, err := contract.String(op.Target, "due_on")
		if err != nil {
			return nil, err
		}
		return a.Provider.UpdateMilestone(ctx, nonce, number, MilestonePatch{Description: &description, DueOn: &due})
	case "issue-milestone-set":
		number, err := contract.PositiveInteger(op.Target["issue_number"])
		if err != nil {
			return nil, err
		}
		graph, _ := contract.ObjectAt(expected, "issue_inventory")
		current := backlogIssueByNumber(mustObjects(graph, "issues"), number)
		if current == nil || current["id"] != op.Target["issue_id"] || current["state"] != "OPEN" || current["milestone"] != op.Before["milestone"] {
			return nil, errors.New("quarter issue identity, state or milestone changed before dispatch")
		}
		patch := IssuePatch{}
		desired := op.After["milestone"]
		if desired == nil {
			patch.ClearMilestone = true
		} else {
			title, ok := desired.(string)
			if !ok {
				return nil, errors.New("reviewed quarter assignment must be a milestone title or null")
			}
			var found contract.Object
			for _, m := range mustObjects(expected, "milestones") {
				if m["title"] == title {
					found = m
					break
				}
			}
			if found == nil || found["state"] != "open" {
				return nil, errors.New("quarter assignment target milestone is absent or closed")
			}
			milestoneNumber, err := contract.PositiveInteger(found["number"])
			if err != nil {
				return nil, err
			}
			patch.MilestoneNumber = &milestoneNumber
		}
		return a.Provider.UpdateIssue(ctx, nonce, number, patch)
	case "issue-update":
		ref, err := contract.ObjectAt(op.Target, "issue_ref")
		if err != nil {
			return nil, err
		}
		number, nodeID, err := backlogResolveIssueIdentity(ref, p, receipts)
		if err != nil {
			return nil, err
		}
		graph, _ := contract.ObjectAt(expected, "issue_inventory")
		current := backlogIssueByNumber(mustObjects(graph, "issues"), number)
		before, err := contract.ObjectAt(op.Before, "issue")
		if err != nil || current == nil || current["id"] != nodeID {
			return nil, errors.New("backlog issue update target identity changed before dispatch")
		}
		for key, value := range before {
			if !same(current[key], value) {
				return nil, fmt.Errorf("backlog issue %s before-state changed before dispatch", key)
			}
		}
		field, err := contract.Nonempty(op.Target, "field")
		if err != nil {
			return nil, err
		}
		value, exists := op.Target["value"]
		if !exists {
			return nil, errors.New("backlog issue update omits its typed value")
		}
		patch := IssuePatch{}
		switch field {
		case "title":
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return nil, errors.New("backlog issue title update must be a non-empty string")
			}
			patch.Title = &text
		case "body":
			text, ok := value.(string)
			if !ok {
				return nil, errors.New("backlog issue body update must be a string")
			}
			patch.Body = &text
		case "labels":
			labels, err := contract.Strings(value)
			if err != nil {
				return nil, err
			}
			patch.Labels = &labels
		case "milestone":
			if value == nil {
				patch.ClearMilestone = true
			} else {
				title, ok := value.(string)
				if !ok {
					return nil, errors.New("backlog milestone assignment must be a title or null")
				}
				var milestone contract.Object
				for _, row := range mustObjects(expected, "milestones") {
					if row["title"] == title {
						milestone = row
						break
					}
				}
				if milestone == nil {
					return nil, errors.New("backlog milestone assignment is outside the complete inventory")
				}
				milestoneNumber, err := contract.PositiveInteger(milestone["number"])
				if err != nil {
					return nil, err
				}
				patch.MilestoneNumber = &milestoneNumber
			}
		default:
			return nil, errors.New("unsupported backlog issue update field")
		}
		return a.Provider.UpdateIssue(ctx, nonce, number, patch)
	case "project-membership-add":
		projectID, err := contract.Nonempty(op.Target, "project_id")
		if err != nil {
			return nil, err
		}
		ref, err := contract.ObjectAt(op.Target, "issue_ref")
		if err != nil {
			return nil, err
		}
		_, nodeID, err := backlogResolveIssueIdentity(ref, p, receipts)
		if err != nil {
			return nil, err
		}
		return a.Provider.AddProjectIssue(ctx, nonce, projectID, nodeID)
	case "project-field-set":
		projectID, err := contract.Nonempty(op.Target, "project_id")
		if err != nil {
			return nil, err
		}
		fieldName, err := contract.Nonempty(op.Target, "field_name")
		if err != nil {
			return nil, err
		}
		project, err := backlogProjectByID(expected, projectID)
		if err != nil {
			return nil, err
		}
		itemID, err := backlogResolveItemID(op.Target, p, receipts)
		if err != nil {
			return nil, err
		}
		valueRaw := op.After["value"]
		fieldValue := ProjectFieldValue{}
		if valueRaw == nil {
			fieldValue.Clear = true
		} else if text, ok := valueRaw.(string); ok {
			fieldValue.Text = &text
		} else {
			number, err := finiteNumber(valueRaw)
			if err != nil {
				return nil, err
			}
			fieldValue.Number = &number
		}
		if err = validateProjectField(project, fieldName, fieldValue); err != nil {
			return nil, err
		}
		return a.Provider.SetProjectField(ctx, nonce, projectID, itemID, ProjectField(fieldName), fieldValue)
	case "issue-relationship-add", "issue-relationship-remove":
		issue, related, kind, err := backlogResolveRelationshipEndpoints(op.Target, p, receipts)
		if err != nil {
			return nil, err
		}
		graph, _ := contract.ObjectAt(expected, "issue_inventory")
		rows, _ := contract.Objects(graph, "issues")
		aIssue, bIssue := backlogIssueByNumber(rows, issue), backlogIssueByNumber(rows, related)
		if aIssue == nil || bIssue == nil {
			return nil, errors.New("review relationship endpoint is outside complete issue inventory")
		}
		if op.Kind == "issue-relationship-add" && kind == string(BlockedBy) {
			values, _ := contract.Array(aIssue, "blocked_by_numbers")
			for _, v := range values {
				n, _ := contract.PositiveInteger(v)
				if n == related {
					return nil, errors.New("review dependency already exists before dispatch")
				}
			}
		} else if op.Kind == "issue-relationship-add" && kind == string(Child) && bIssue["parent_number"] != nil {
			return nil, errors.New("review child issue already has a parent before dispatch")
		}
		if op.Kind == "issue-relationship-remove" && kind == string(BlockedBy) {
			values, _ := contract.Array(aIssue, "blocked_by_numbers")
			found := false
			for _, v := range values {
				n, _ := contract.PositiveInteger(v)
				found = found || n == related
			}
			if !found {
				return nil, errors.New("review dependency is already absent before dispatch")
			}
		} else if op.Kind == "issue-relationship-remove" && kind == string(Child) && bIssue["parent_number"] != issue {
			return nil, errors.New("review hierarchy edge differs before dispatch")
		}
		if op.Kind == "issue-relationship-remove" {
			return a.Provider.RemoveRelationship(ctx, nonce, issue, related, Relationship(kind))
		}
		return a.Provider.AddRelationship(ctx, nonce, issue, related, Relationship(kind))
	default:
		return nil, fmt.Errorf("unsupported backlog primitive kind %q", op.Kind)
	}
}

func containsOperation(ops []contract.Operation, want contract.Operation) bool {
	for _, op := range ops {
		if op.ID == want.ID && same(op, want) {
			return true
		}
	}
	return false
}
