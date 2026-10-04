package workflow

import (
	"context"
	"errors"
	"fmt"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/planning"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PrepareReviewBacklog re-runs the deterministic review planner over a
// complete live issue catalog and the caller's explicit taxonomy and Project.
func PrepareReviewBacklog(ctx context.Context, provider BacklogProvider, repo contract.Repository, findings contract.Object, policy ReviewBacklogPolicy, now time.Time) (contract.Plan, error) {
	if provider == nil {
		return contract.Plan{}, errors.New("review backlog preparation requires a provider")
	}
	if err := policy.validate(repo); err != nil {
		return contract.Plan{}, err
	}
	if _, err := planning.NormalizeReviewFindings(findings); err != nil {
		return contract.Plan{}, err
	}
	relationships, err := findingsRequireRelationships(findings)
	if err != nil {
		return contract.Plan{}, err
	}
	baseRequest := BacklogInventoryRequest{Projects: []ProjectScope{policy.Project}, IncludeRelationships: relationships}
	raw, err := provider.BacklogInventory(ctx, baseRequest)
	if err != nil {
		return contract.Plan{}, err
	}
	base, err := normalizeBacklogInventory(raw, repo, baseRequest)
	if err != nil {
		return contract.Plan{}, err
	}
	data := contract.Object{"findings": findings, "policy": policy.Object()}
	delta, err := reviewDelta(data, base, repo)
	if err != nil {
		return contract.Plan{}, err
	}
	markers, err := reviewCommentMarkers(delta)
	if err != nil {
		return contract.Plan{}, err
	}
	request := baseRequest
	request.CommentMarkers = markers
	inventory := base
	if len(markers) > 0 {
		raw, err = provider.BacklogInventory(ctx, request)
		if err != nil {
			return contract.Plan{}, err
		}
		inventory, err = normalizeBacklogInventory(raw, repo, request)
		if err != nil {
			return contract.Plan{}, err
		}
		if !same(sourceFacet(base, "issue_inventory", "milestones", "labels", "projects"), sourceFacet(inventory, "issue_inventory", "milestones", "labels", "projects")) {
			return contract.Plan{}, errors.New("backlog inventory drifted while preparing review comments")
		}
	}
	if err = validateReviewLabels(delta, inventory); err != nil {
		return contract.Plan{}, err
	}
	data["inventory"], data["inventory_request"] = inventory, request.Object()
	sources := backlogPlanSources(inventory, request)
	p := contract.Plan{Command: ReviewBacklogCommand, Repository: repo, Data: data, Sources: sources, CapturedAt: now.UTC().Format(time.RFC3339Nano)}
	ops, err := backlogOperations(p)
	if err != nil {
		return contract.Plan{}, err
	}
	return contract.PreparePlan(ReviewBacklogCommand, repo, sources, data, ops, now)
}

func findingsRequireRelationships(findings contract.Object) (bool, error) {
	normalized, err := planning.NormalizeReviewFindings(findings)
	if err != nil {
		return false, err
	}
	rows, _ := contract.Objects(normalized, "findings")
	for _, row := range rows {
		if row["parent_issue_number"] != nil {
			return true, nil
		}
		blocked, e := contract.Array(row, "blocked_by_issue_numbers")
		if e != nil {
			return false, e
		}
		if len(blocked) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func reviewDelta(data, inventory contract.Object, repo contract.Repository) (contract.Object, error) {
	findings, err := contract.ObjectAt(data, "findings")
	if err != nil {
		return nil, err
	}
	policy, err := contract.ObjectAt(data, "policy")
	if err != nil {
		return nil, err
	}
	project, err := contract.ObjectAt(policy, "project")
	if err != nil {
		return nil, err
	}
	scope, err := backlogParseProjectScope(project)
	if err != nil {
		return nil, err
	}
	graph, err := contract.ObjectAt(inventory, "issue_inventory")
	if err != nil {
		return nil, err
	}
	rows, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	catalog := make([]any, 0, len(rows))
	for _, row := range rows {
		catalog = append(catalog, contract.Object{"number": row["number"], "title": row["title"], "state": row["state"]})
	}
	priority, err := contract.ObjectAt(policy, "severity_to_priority")
	if err != nil {
		return nil, err
	}
	labels, err := contract.ObjectAt(policy, "issue_type_labels")
	if err != nil {
		return nil, err
	}
	input := contract.Object{"findings": findings, "repo": repo.Object(), "project_title": scope.Title, "issue_catalog_mode": "live", "issue_catalog": catalog, "policy": contract.Object{"severity_to_priority": priority, "issue_type_labels": labels}}
	result, err := planning.BuildReviewBacklogDelta(input)
	if err != nil {
		return nil, err
	}
	// Planner values are in-memory Go structures and may contain typed slices
	// such as []int64. Normalize through the machine JSON contract before the
	// execution-state composer reads arrays and objects strictly.
	result, err = contract.Clone(result)
	if err != nil {
		return nil, err
	}
	return contract.ObjectAt(result, "delta")
}

func reviewCommentMarkers(delta contract.Object) ([]string, error) {
	proposals, err := contract.Objects(delta, "proposals")
	if err != nil {
		return nil, err
	}
	markers := []string{}
	for _, proposal := range proposals {
		if proposal["action"] == "reuse-open" {
			marker, err := reviewCommentMarker(delta, proposal)
			if err != nil {
				return nil, err
			}
			markers = append(markers, marker)
		}
	}
	sort.Strings(markers)
	return markers, nil
}

func reviewCommentMarker(delta, proposal contract.Object) (string, error) {
	scope := delta["scope"]
	digest, err := backlogPythonMarkerDigest(contract.Object{"scope": scope, "proposal": proposal})
	if err != nil {
		return "", err
	}
	return "<!-- gh-steward-review:" + digest + " -->", nil
}

func reviewCreationMarker(delta, proposal contract.Object, repo contract.Repository) (string, error) {
	identity := contract.Object{"host": repo.Host, "owner": repo.Owner, "name": repo.Name, "url": repo.URL}
	digest, err := backlogPythonMarkerDigest(contract.Object{"repo": identity, "scope": delta["scope"], "proposal": proposal})
	if err != nil {
		return "", err
	}
	return "<!-- gh-steward-review-create:" + digest + " -->", nil
}

func reviewCommentBody(proposal contract.Object, capturedAt string) (string, error) {
	findings, err := contract.Objects(proposal, "findings")
	if err != nil {
		return "", err
	}
	priority, err := contract.String(proposal, "priority")
	if err != nil {
		return "", err
	}
	lines := []string{fmt.Sprintf("## Review backlog update %s", capturedAt), "", fmt.Sprintf("- priority recommendation: %s", priority), fmt.Sprintf("- findings: %d", len(findings)), ""}
	for _, finding := range findings {
		summary, err := contract.Nonempty(finding, "summary")
		if err != nil {
			return "", err
		}
		lines = append(lines, "- "+summary)
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func reviewCreationBody(proposal contract.Object, marker string) (string, error) {
	body, err := contract.String(proposal, "body")
	if err != nil {
		return "", err
	}
	body = strings.TrimRight(body, "\n")
	related, err := contract.Array(proposal, "related_closed_issues")
	if err != nil {
		return "", err
	}
	if len(related) > 0 {
		body += "\n\n## Related Closed Issues\n\n"
		for _, raw := range related {
			n, e := contract.PositiveInteger(raw)
			if e != nil {
				return "", e
			}
			body += "- #" + strconv.FormatInt(n, 10) + "\n"
		}
	}
	return body + "\n\n" + marker + "\n", nil
}

func validateReviewLabels(delta, inventory contract.Object) error {
	defs, err := contract.Objects(inventory, "labels")
	if err != nil {
		return err
	}
	available := map[string]bool{}
	for _, d := range defs {
		available[d["name"].(string)] = true
	}
	proposals, err := contract.Objects(delta, "proposals")
	if err != nil {
		return err
	}
	for _, p := range proposals {
		labels, err := contract.Strings(p["labels"])
		if err != nil {
			return err
		}
		for _, label := range labels {
			if !available[label] {
				return fmt.Errorf("reviewed issue label %q is absent from the complete live label catalog", label)
			}
		}
	}
	return nil
}

func reviewBacklogOperations(p contract.Plan, inventory contract.Object, request BacklogInventoryRequest) ([]contract.Operation, error) {
	policyRaw, err := contract.ObjectAt(p.Data, "policy")
	if err != nil {
		return nil, err
	}
	policy, err := parseReviewBacklogPolicy(policyRaw, p.Repository)
	if err != nil {
		return nil, err
	}
	projectScope := policy.Project
	if len(request.Projects) != 1 || request.Projects[0] != projectScope {
		return nil, errors.New("review backlog Project policy and inventory scope differ")
	}
	delta, err := reviewDelta(p.Data, inventory, p.Repository)
	if err != nil {
		return nil, err
	}
	expectedMarkers, err := reviewCommentMarkers(delta)
	if err != nil {
		return nil, err
	}
	if !same(expectedMarkers, request.CommentMarkers) {
		return nil, errors.New("review comment source markers differ from derived proposal set")
	}
	if err = validateReviewLabels(delta, inventory); err != nil {
		return nil, err
	}
	if request.IncludeRelationships {
		if _, err = planning.RelationshipTopology(inventory["issue_inventory"].(map[string]any)); err != nil {
			return nil, err
		}
	} else {
		needs, err := deltaRequiresRelationships(delta)
		if err != nil {
			return nil, err
		}
		if needs {
			return nil, errors.New("review relationship policy omitted required complete topology")
		}
	}
	if request.IncludeRelationships {
		if err = validateReviewTopology(delta, inventory); err != nil {
			return nil, err
		}
	}
	proposals, err := contract.Objects(delta, "proposals")
	if err != nil {
		return nil, err
	}
	project, err := backlogProjectByID(inventory, projectScope.ID)
	if err != nil {
		return nil, err
	}
	queueOrders, err := reviewQueueOrders(proposals, project)
	if err != nil {
		return nil, err
	}
	issueRows, _ := contract.Objects(inventory["issue_inventory"].(map[string]any), "issues")
	issuesByNumber := map[int64]contract.Object{}
	for _, row := range issueRows {
		n, _ := contract.PositiveInteger(row["number"])
		issuesByNumber[n] = row
	}
	projectItems := map[int64]contract.Object{}
	for _, item := range mustObjects(project, "items") {
		n, _ := contract.PositiveInteger(item["number"])
		projectItems[n] = item
	}
	commentRows := mustObjects(inventory, "comments")
	ops := []contract.Operation{}
	for index, proposal := range proposals {
		clientID := fmt.Sprintf("review-%04d", index+1)
		action, _ := proposal["action"].(string)
		issueNumber, _ := contract.PositiveInteger(proposal["issue_number"])
		issueRef := contract.Object{}
		if action == "create" || action == "create-follow-up" {
			marker, err := reviewCreationMarker(delta, proposal, p.Repository)
			if err != nil {
				return nil, err
			}
			body, err := reviewCreationBody(proposal, marker)
			if err != nil {
				return nil, err
			}
			title, _ := contract.String(proposal, "title")
			labels, _ := contract.Strings(proposal["labels"])
			sort.Strings(labels)
			ops = append(ops, contract.Operation{ID: fmt.Sprintf("review-proposal:%04d:create", index+1), Kind: "issue-create", Target: contract.Object{"proposal_index": index, "client_id": clientID, "creation_marker": marker, "title": title, "body": body, "labels": stringSliceAny(labels)}, Before: contract.Object{"exists": false, "creation_marker": marker, "title": nil, "body": nil, "labels": []any{}, "state": nil, "milestone": nil}, After: contract.Object{"exists": true, "creation_marker": marker, "title": title, "body": body, "labels": stringSliceAny(labels), "state": "OPEN", "milestone": nil}})
			issueRef = contract.Object{"created_by": fmt.Sprintf("review-proposal:%04d:create", index+1)}
		} else if action == "reuse-open" {
			issue := issuesByNumber[issueNumber]
			if issue == nil || issue["state"] != "OPEN" || issue["title"] != proposal["title"] {
				return nil, errors.New("review reuse target differs from its complete issue inventory")
			}
			issueRef = contract.Object{"issue_number": issueNumber}
			marker, err := reviewCommentMarker(delta, proposal)
			if err != nil {
				return nil, err
			}
			body, err := reviewCommentBody(proposal, p.CapturedAt)
			if err != nil {
				return nil, err
			}
			body = marker + "\n" + strings.TrimSpace(body) + "\n"
			var existing contract.Object
			for _, comment := range commentRows {
				n, _ := contract.PositiveInteger(comment["issue_number"])
				if n == issueNumber && strings.Contains(comment["body"].(string), marker) {
					existing = comment
					break
				}
			}
			if existing == nil || stripOperationMarker(existing["body"].(string)) != body {
				var commentID any
				if existing != nil {
					commentID = existing["id"]
				}
				ops = append(ops, contract.Operation{ID: fmt.Sprintf("review-proposal:%04d:comment", index+1), Kind: "issue-comment-upsert", Target: contract.Object{"proposal_index": index, "issue_number": issueNumber, "issue_id": issue["id"], "marker": marker, "comment_id": commentID, "body": body}, Before: contract.Object{"issue_number": issueNumber, "issue_id": issue["id"], "issue_state": issue["state"], "marker": marker, "comment_id": commentID, "body": func() any {
					if existing != nil {
						return existing["body"]
					}
					return nil
				}()}, After: contract.Object{"issue_number": issueNumber, "issue_id": issue["id"], "issue_state": issue["state"], "marker": marker, "body": body}})
			}
		} else {
			return nil, errors.New("review planner returned an unsupported proposal action")
		}
		if action == "reuse-open" {
			if issuesByNumber[issueNumber] == nil {
				return nil, errors.New("review proposal issue is absent")
			}
		}
		var existingProjectItem contract.Object
		if action == "reuse-open" {
			existingProjectItem = projectItems[issueNumber]
			if existingProjectItem != nil && existingProjectItem["archived"] == true {
				return nil, errors.New("review proposal has an archived Project item that cannot be safely reused")
			}
		}
		membershipID := fmt.Sprintf("review-proposal:%04d:project-membership", index+1)
		itemRef := contract.Object{}
		if existingProjectItem != nil {
			itemID, _ := contract.Nonempty(existingProjectItem, "item_id")
			itemRef = contract.Object{"item_id": itemID}
		} else {
			target := contract.Object{"proposal_index": index, "client_id": clientID, "project_id": projectScope.ID, "issue_ref": issueRef}
			var issueNode any
			if action == "reuse-open" {
				issueNode = issuesByNumber[issueNumber]["id"]
			}
			ops = append(ops, contract.Operation{ID: membershipID, Kind: "project-membership-add", Target: target, Before: contract.Object{"present": false, "project_id": projectScope.ID, "issue_node_id": issueNode}, After: contract.Object{"present": true, "project_id": projectScope.ID, "issue_node_id": issueNode}})
			itemRef = contract.Object{"from_operation": membershipID}
		}
		fields := []struct {
			name   string
			value  ProjectFieldValue
			intent any
		}{{"Status", ProjectFieldValue{Text: stringPtr("Todo")}, "Todo"}, {"Priority", ProjectFieldValue{Text: stringPtr(fmt.Sprint(proposal["priority"]))}, proposal["priority"]}, {"Queue Order", ProjectFieldValue{Number: floatPtr(queueOrders[index])}, queueOrders[index]}}
		var initialFields contract.Object
		if existingProjectItem != nil {
			initialFields, _ = contract.ObjectAt(existingProjectItem, "field_values")
		}
		for _, field := range fields {
			if err := validateProjectField(project, field.name, field.value); err != nil {
				return nil, err
			}
			if existingProjectItem != nil && same(initialFields[field.name], field.intent) {
				continue
			}
			fieldTarget := contract.Object{"proposal_index": index, "project_id": projectScope.ID, "project_title": projectScope.Title, "issue_ref": issueRef, "item_ref": itemRef, "field_name": field.name}
			beforeValue := any(initialFields[field.name])
			if existingProjectItem == nil {
				beforeValue = contract.Object{"from_membership": membershipID, "field_name": field.name}
			}
			ops = append(ops, contract.Operation{ID: fmt.Sprintf("review-proposal:%04d:project-field:%s", index+1, fieldSlug(field.name)), Kind: "project-field-set", Target: fieldTarget, Before: contract.Object{"present": true, "item_ref": itemRef, "field_name": field.name, "value": beforeValue}, After: contract.Object{"present": true, "item_ref": itemRef, "field_name": field.name, "value": field.intent}})
		}
		blockers, err := contract.Array(proposal, "blocked_by_issue_numbers")
		if err != nil {
			return nil, err
		}
		for _, raw := range blockers {
			blocker, err := contract.PositiveInteger(raw)
			if err != nil || issuesByNumber[blocker] == nil {
				return nil, errors.New("review proposal blocker is outside the complete issue inventory")
			}
			issueCurrent := issuesByNumber[issueNumber]
			present := false
			if issueCurrent != nil {
				list, _ := contract.Array(contract.Object{"values": issueCurrent["blocked_by_numbers"]}, "values")
				for _, item := range list {
					n, _ := contract.PositiveInteger(item)
					if n == blocker {
						present = true
					}
				}
			}
			if !present {
				ops = append(ops, contract.Operation{ID: fmt.Sprintf("review-proposal:%04d:relationship-blocker:%d", index+1, blocker), Kind: "issue-relationship-add", Target: backlogRelationshipTarget(issueRef, blocker, BlockedBy), Before: contract.Object{"present": false, "relation": string(BlockedBy)}, After: contract.Object{"present": true, "relation": string(BlockedBy)}})
			}
		}
		if proposal["parent_issue_number"] != nil {
			parent, err := contract.PositiveInteger(proposal["parent_issue_number"])
			if err != nil || issuesByNumber[parent] == nil {
				return nil, errors.New("review proposal parent is outside complete issue inventory")
			}
			issueCurrent := issuesByNumber[issueNumber]
			var currentParent any
			if issueCurrent != nil {
				currentParent = issueCurrent["parent_number"]
			}
			if currentParent != nil && currentParent != parent {
				return nil, errors.New("review parent hint cannot silently reparent an existing issue")
			}
			if currentParent == nil {
				ops = append(ops, contract.Operation{ID: fmt.Sprintf("review-proposal:%04d:relationship-parent", index+1), Kind: "issue-relationship-add", Target: backlogRelationshipTarget(issueRef, parent, Child), Before: contract.Object{"present": false, "relation": string(Child)}, After: contract.Object{"present": true, "relation": string(Child)}})
			}
		}
	}
	return ops, nil
}

func commentStateForMarker(inventory contract.Object, issue int64, marker string) (contract.Object, error) {
	rows, err := contract.Objects(inventory, "comments")
	if err != nil {
		return nil, err
	}
	var found contract.Object
	for _, row := range rows {
		n, _ := contract.PositiveInteger(row["issue_number"])
		if n == issue && strings.Contains(row["body"].(string), marker) {
			if found != nil {
				return nil, errors.New("comment marker is ambiguous on target issue")
			}
			found = row
		}
	}
	return found, nil
}

func backlogProjectByID(inventory contract.Object, id string) (contract.Object, error) {
	rows, err := contract.Objects(inventory, "projects")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		p, err := contract.ObjectAt(row, "project")
		if err != nil {
			return nil, err
		}
		if p["id"] == id {
			return row, nil
		}
	}
	return nil, errors.New("review Project is missing from complete inventory")
}

func reviewQueueOrders(proposals []contract.Object, project contract.Object) (map[int]float64, error) {
	items, err := contract.Objects(project, "items")
	if err != nil {
		return nil, err
	}
	byNumber := map[int64]contract.Object{}
	bandMax := map[string]float64{}
	for _, item := range items {
		n, e := contract.PositiveInteger(item["number"])
		if e != nil {
			return nil, e
		}
		byNumber[n] = item
		fields, e := contract.ObjectAt(item, "field_values")
		if e != nil {
			return nil, e
		}
		priority, _ := fields["Priority"].(string)
		status, _ := fields["Status"].(string)
		if priority == "" || status == "Done" {
			continue
		}
		raw, ok := fields["Queue Order"]
		if !ok || raw == nil {
			continue
		}
		value, e := finiteNumber(raw)
		if e != nil {
			return nil, e
		}
		if value > bandMax[priority] {
			bandMax[priority] = value
		}
	}
	orders := map[int]float64{}
	added := map[string]int{}
	for index, p := range proposals {
		priority, err := contract.Nonempty(p, "priority")
		if err != nil {
			return nil, err
		}
		issueNumber, _ := contract.PositiveInteger(p["issue_number"])
		if issueNumber > 0 {
			if item := byNumber[issueNumber]; item != nil {
				fields, _ := contract.ObjectAt(item, "field_values")
				if fields["Priority"] == priority {
					if value, e := finiteNumber(fields["Queue Order"]); e == nil {
						orders[index] = value
						continue
					}
				}
			}
		}
		orders[index] = bandMax[priority] + float64(added[priority]+1)*1024
		added[priority]++
	}
	return orders, nil
}

func deltaRequiresRelationships(delta contract.Object) (bool, error) {
	rows, err := contract.Objects(delta, "proposals")
	if err != nil {
		return false, err
	}
	for _, p := range rows {
		if p["parent_issue_number"] != nil {
			return true, nil
		}
		blockers, e := contract.Array(p, "blocked_by_issue_numbers")
		if e != nil {
			return false, e
		}
		if len(blockers) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func validateReviewTopology(delta, inventory contract.Object) error {
	graph, err := contract.ObjectAt(inventory, "issue_inventory")
	if err != nil {
		return err
	}
	topology, err := planning.RelationshipTopology(graph)
	if err != nil {
		return err
	}
	blocked, _ := contract.ObjectAt(topology, "blocked_by")
	parents, _ := contract.ObjectAt(topology, "parent")
	rows, err := contract.Objects(delta, "proposals")
	if err != nil {
		return err
	}
	issues, _ := contract.Objects(graph, "issues")
	max := int64(0)
	byNum := map[int64]contract.Object{}
	for _, i := range issues {
		n, _ := contract.PositiveInteger(i["number"])
		byNum[n] = i
		if n > max {
			max = n
		}
	}
	for index, p := range rows {
		n, e := contract.PositiveInteger(p["issue_number"])
		if e != nil || n == 0 {
			if p["action"] != "create" && p["action"] != "create-follow-up" {
				return errors.New("review topology target identity is malformed")
			}
			n = max + int64(index) + 1
			blocked[strconv.FormatInt(n, 10)] = []any{}
			parents[strconv.FormatInt(n, 10)] = nil
		}
		key := strconv.FormatInt(n, 10)
		currentBlockers, _ := contract.Array(contract.Object{"v": blocked[key]}, "v")
		set := map[int64]bool{}
		for _, b := range currentBlockers {
			x, e := contract.PositiveInteger(b)
			if e == nil {
				set[x] = true
			}
		}
		blockers, _ := contract.Array(p, "blocked_by_issue_numbers")
		for _, v := range blockers {
			b, e := contract.PositiveInteger(v)
			if e != nil || byNum[b] == nil {
				return errors.New("review topology references unknown blocker")
			}
			set[b] = true
		}
		next := []any{}
		for b := range set {
			next = append(next, b)
		}
		sort.Slice(next, func(i, j int) bool { return next[i].(int64) < next[j].(int64) })
		blocked[key] = next
		if p["parent_issue_number"] != nil {
			parent, e := contract.PositiveInteger(p["parent_issue_number"])
			if e != nil || byNum[parent] == nil {
				return errors.New("review topology references unknown parent")
			}
			old := parents[key]
			if old != nil && old != parent {
				return errors.New("review hints cannot silently reparent an existing issue")
			}
			parents[key] = parent
		}
	}
	if cycles, e := planning.FindDirectedCycles(blocked); e != nil || len(cycles) > 0 {
		return errors.New("review backlog introduces a dependency cycle")
	}
	if cycles, e := planning.FindDirectedCycles(parents); e != nil || len(cycles) > 0 {
		return errors.New("review backlog introduces a hierarchy cycle")
	}
	return nil
}
