package workflow

import (
	"errors"
	"fmt"
	"github.com/coreycoto/gh-steward/internal/contract"
	"sort"
	"strings"
)

func backlogProjectAcknowledgement(inventory contract.Object, p contract.Plan, op contract.Operation, ack contract.Object, receipts []contract.Object) (contract.Object, error) {
	if err := (&Backlog{}).ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, err
	}
	out, err := contract.Clone(inventory)
	if err != nil {
		return nil, err
	}
	raw, err := contract.ObjectAt(ack, "provider_result")
	if err != nil {
		return nil, err
	}
	graph, _ := contract.ObjectAt(out, "issue_inventory")
	issues, _ := contract.Objects(graph, "issues")
	milestones, _ := contract.Objects(out, "milestones")
	comments, _ := contract.Objects(out, "comments")
	switch op.Kind {
	case "issue-create":
		issue, err := backlogCreatedIssue(raw, p.Repository)
		if err != nil {
			return nil, err
		}
		delete(issue, "project_items")
		provenance, err := contract.ObjectAt(out, "provenance")
		if err != nil {
			return nil, err
		}
		if provenance["relationships_source"] == "not_requested" {
			for _, key := range []string{"blocked_by_numbers", "child_numbers", "parent_number"} {
				delete(issue, key)
			}
		} else {
			issue["blocked_by_numbers"], issue["child_numbers"], issue["parent_number"] = []any{}, []any{}, nil
		}
		for _, old := range issues {
			if old["number"] == issue["number"] || strings.Contains(fmt.Sprint(old["body"]), fmt.Sprint(op.Target["creation_marker"])) {
				return nil, errors.New("created issue acknowledgement collides with captured inventory")
			}
		}
		if op.Before["exists"] != false || op.After["exists"] != true || issue["title"] != op.After["title"] || !strings.Contains(issue["body"].(string), fmt.Sprint(op.After["creation_marker"])) {
			return nil, errors.New("created issue acknowledgement does not satisfy reviewed creation intent")
		}
		issues = append(issues, issue)
	case "issue-milestone-set":
		n, err := contract.PositiveInteger(op.Target["issue_number"])
		if err != nil {
			return nil, err
		}
		issue := backlogIssueByNumber(issues, n)
		if issue == nil {
			return nil, errors.New("milestone receipt issue disappeared")
		}
		if issue["id"] != op.Target["issue_id"] || issue["state"] != op.Before["issue_state"] || issue["milestone"] != op.Before["milestone"] {
			return nil, errors.New("quarter milestone assignment before-state differs from projected inventory")
		}
		if title, ok := op.After["milestone"].(string); ok {
			var open bool
			for _, milestone := range milestones {
				if milestone["title"] == title && milestone["state"] == "open" {
					open = true
				}
			}
			if !open {
				return nil, errors.New("quarter assignment target is not an open projected milestone")
			}
		}
		issue["milestone"] = op.After["milestone"]
	case "issue-update":
		ref, err := contract.ObjectAt(op.Target, "issue_ref")
		if err != nil {
			return nil, err
		}
		number, nodeID, err := backlogResolveIssueIdentity(ref, p, receipts)
		if err != nil {
			return nil, err
		}
		issue := backlogIssueByNumber(issues, number)
		if issue == nil || issue["id"] != nodeID {
			return nil, errors.New("issue update receipt target is absent or has another identity")
		}
		before, err := contract.ObjectAt(op.Before, "issue")
		if err != nil {
			return nil, err
		}
		for key, expected := range before {
			if !same(issue[key], expected) {
				return nil, fmt.Errorf("issue update %s before-state differs from projected inventory", key)
			}
		}
		after, err := contract.ObjectAt(op.After, "issue")
		if err != nil {
			return nil, err
		}
		for key, value := range after {
			issue[key] = value
		}
	case "milestone-create", "milestone-update":
		milestone, err := backlogCreatedMilestone(raw, p.Repository)
		if err != nil {
			return nil, err
		}
		filtered := []contract.Object{}
		matched := false
		for _, old := range milestones {
			if old["title"] == op.Target["title"] || old["number"] == milestone["number"] {
				if op.Kind != "milestone-update" || old["title"] != op.Target["title"] || old["number"] != op.Before["number"] || old["id"] != op.Before["id"] || old["node_id"] != op.Before["node_id"] || old["description"] != op.Before["description"] || old["due_on"] != op.Before["due_on"] || old["state"] != op.Before["state"] {
					return nil, errors.New("milestone acknowledgement before-state differs from projected inventory")
				}
				matched = true
				continue
			}
			filtered = append(filtered, old)
		}
		if (op.Kind == "milestone-update") != matched {
			return nil, errors.New("milestone create or update does not match projected existence")
		}
		milestones = append(filtered, milestone)
	case "issue-comment-upsert":
		n, err := contract.PositiveInteger(op.Target["issue_number"])
		if err != nil {
			return nil, err
		}
		comment, err := backlogCreatedComment(raw, p.Repository, n)
		if err != nil {
			return nil, err
		}
		graph, _ := contract.ObjectAt(out, "issue_inventory")
		issue := backlogIssueByNumber(mustObjects(graph, "issues"), n)
		if issue == nil || issue["id"] != op.Target["issue_id"] || issue["state"] != op.Before["issue_state"] {
			return nil, errors.New("backlog comment issue identity or state changed")
		}
		var existing contract.Object
		for _, old := range comments {
			if old["issue_number"] == n && strings.Contains(old["body"].(string), fmt.Sprint(op.Target["marker"])) {
				if existing != nil {
					return nil, errors.New("backlog comment marker is ambiguous in projected inventory")
				}
				existing = old
			}
		}
		commentID := op.Target["comment_id"]
		if commentID == nil {
			if existing != nil || op.Before["body"] != nil {
				return nil, errors.New("new comment operation has an unexpected projected marker")
			}
		} else if existing == nil || existing["id"] != commentID || existing["body"] != op.Before["body"] {
			return nil, errors.New("comment update before-state differs from projected inventory")
		}
		filtered := []contract.Object{}
		for _, old := range comments {
			if old["issue_number"] == n && old["id"] == op.Target["comment_id"] && op.Target["comment_id"] != nil {
				continue
			}
			if old["issue_number"] == n && strings.Contains(old["body"].(string), fmt.Sprint(op.Target["marker"])) {
				continue
			}
			filtered = append(filtered, old)
		}
		comments = append(filtered, comment)
	case "project-membership-add":
		projectID, _ := contract.Nonempty(op.Target, "project_id")
		project, err := backlogProjectByID(out, projectID)
		if err != nil {
			return nil, err
		}
		itemID, fields, n, nodeID, err := backlogProjectItemAck(raw, p.Repository)
		if err != nil {
			return nil, err
		}
		issueRef, err := contract.ObjectAt(op.Target, "issue_ref")
		if err != nil {
			return nil, err
		}
		wantNumber, wantNode, err := backlogResolveIssueIdentity(issueRef, p, receipts)
		if err != nil {
			return nil, err
		}
		if wantNumber > 0 && n != wantNumber || wantNode != "" && nodeID != wantNode {
			return nil, errors.New("Project membership receipt targets another issue")
		}
		if op.Before["present"] != false || op.After["present"] != true {
			return nil, errors.New("Project membership receipt differs from reviewed presence transition")
		}
		items, _ := contract.Objects(project, "items")
		for _, old := range items {
			if old["number"] == n || old["item_id"] == itemID {
				return nil, errors.New("Project membership acknowledgement duplicates a captured item")
			}
		}
		items = append(items, contract.Object{"item_id": itemID, "number": n, "field_values": fields, "archived": false})
		sort.Slice(items, func(i, j int) bool {
			a, _ := contract.PositiveInteger(items[i]["number"])
			b, _ := contract.PositiveInteger(items[j]["number"])
			return a < b
		})
		project["items"] = backlogObjectsAsAny(items)
	case "project-field-set":
		projectID, _ := contract.Nonempty(op.Target, "project_id")
		project, err := backlogProjectByID(out, projectID)
		if err != nil {
			return nil, err
		}
		itemID, err := backlogResolveItemID(op.Target, p, receipts)
		if err != nil {
			return nil, err
		}
		items, _ := contract.Objects(project, "items")
		var item contract.Object
		for _, candidate := range items {
			if candidate["item_id"] == itemID {
				item = candidate
				break
			}
		}
		if item == nil {
			return nil, errors.New("Project field receipt item is missing")
		}
		issueRef, err := contract.ObjectAt(op.Target, "issue_ref")
		if err != nil {
			return nil, err
		}
		issueNumber, _, err := backlogResolveIssueIdentity(issueRef, p, receipts)
		if err != nil {
			return nil, err
		}
		itemNumber, err := contract.PositiveInteger(item["number"])
		if err != nil || itemNumber != issueNumber {
			return nil, errors.New("Project field receipt targets another issue")
		}
		values, _ := contract.ObjectAt(item, "field_values")
		field := fmt.Sprint(op.Target["field_name"])
		before := op.Before["value"]
		if sentinel, ok := before.(map[string]any); ok && sentinel["from_membership"] != nil {
			// Membership creates an item whose default fields are provider-owned.
			// Its immutable native ACK and the immediate complete inventory read
			// bind those defaults before this separately reviewed field mutation.
		} else if current, exists := values[field]; exists != (before != nil) || !same(current, before) {
			return nil, errors.New("Project field before-state differs from projected inventory")
		}
		if op.After["value"] == nil {
			delete(values, field)
		} else {
			values[field] = op.After["value"]
		}
	case "issue-relationship-add", "issue-relationship-remove":
		issueNumber, related, kind, err := backlogResolveRelationshipEndpoints(op.Target, p, receipts)
		if err != nil {
			return nil, err
		}
		if kind == string(BlockedBy) {
			issue := backlogIssueByNumber(issues, issueNumber)
			if issue == nil {
				return nil, errors.New("dependency target issue is missing")
			}
			values, err := contract.Array(issue, "blocked_by_numbers")
			if err != nil {
				return nil, err
			}
			found := false
			for _, value := range values {
				current, _ := contract.PositiveInteger(value)
				found = found || current == related
			}
			if op.Kind == "issue-relationship-add" {
				if found {
					return nil, errors.New("dependency edge already exists in projected before-state")
				}
				issue["blocked_by_numbers"] = backlogAppendUniqueNumber(values, related)
			} else {
				if !found {
					return nil, errors.New("dependency edge is absent in projected before-state")
				}
				issue["blocked_by_numbers"] = backlogRemoveNumber(values, related)
			}
		} else if kind == string(Child) {
			parentIssue := backlogIssueByNumber(issues, issueNumber)
			child := backlogIssueByNumber(issues, related)
			if parentIssue == nil || child == nil {
				return nil, errors.New("hierarchy endpoint is missing")
			}
			children, err := contract.Array(parentIssue, "child_numbers")
			if err != nil {
				return nil, err
			}
			found := false
			for _, value := range children {
				current, _ := contract.PositiveInteger(value)
				if current == related {
					found = true
				}
			}
			if op.Kind == "issue-relationship-add" {
				if child["parent_number"] != nil || found {
					return nil, errors.New("child already has a parent in projected before-state")
				}
				parentIssue["child_numbers"] = backlogAppendUniqueNumber(children, related)
				child["parent_number"] = issueNumber
			} else {
				if child["parent_number"] != issueNumber || !found {
					return nil, errors.New("hierarchy edge differs from projected before-state")
				}
				parentIssue["child_numbers"] = backlogRemoveNumber(children, related)
				child["parent_number"] = nil
			}
		} else {
			return nil, errors.New("unsupported projected relationship")
		}
	default:
		return nil, fmt.Errorf("unsupported backlog primitive kind %q", op.Kind)
	}
	sort.Slice(issues, func(i, j int) bool {
		a, _ := contract.PositiveInteger(issues[i]["number"])
		b, _ := contract.PositiveInteger(issues[j]["number"])
		return a < b
	})
	sort.Slice(milestones, func(i, j int) bool {
		a, _ := contract.PositiveInteger(milestones[i]["number"])
		b, _ := contract.PositiveInteger(milestones[j]["number"])
		return a < b
	})
	sort.Slice(comments, func(i, j int) bool {
		aIssue, _ := contract.PositiveInteger(comments[i]["issue_number"])
		bIssue, _ := contract.PositiveInteger(comments[j]["issue_number"])
		if aIssue != bIssue {
			return aIssue < bIssue
		}
		aID, _ := contract.PositiveInteger(comments[i]["id"])
		bID, _ := contract.PositiveInteger(comments[j]["id"])
		return aID < bID
	})
	graph["issues"] = backlogObjectsAsAny(issues)
	out["milestones"] = backlogObjectsAsAny(milestones)
	out["comments"] = backlogObjectsAsAny(comments)
	return out, nil
}
