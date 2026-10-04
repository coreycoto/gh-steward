package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func backlogMutationOperations(p contract.Plan, inventory contract.Object, request BacklogInventoryRequest) ([]contract.Operation, error) {
	if !sameKeys(p.Data, keySet("intent", "inventory_request", "inventory")) {
		return nil, errors.New("backlog mutation plan has unsupported data fields")
	}
	intent, err := contract.ObjectAt(p.Data, "intent")
	if err != nil {
		return nil, err
	}
	entries, err := normalizeBacklogMutationIntent(intent)
	if err != nil {
		return nil, err
	}
	projects, err := backlogMutationProjectScopes(entries, request.Projects, p.Repository)
	if err != nil || !same(projectScopesAny(projects), projectScopesAny(request.Projects)) {
		return nil, errors.New("backlog mutation Project selections differ from the exact captured scopes")
	}
	if request.IncludeRelationships != backlogMutationTouchesRelationships(entries) || len(request.CommentMarkers) != 0 {
		return nil, errors.New("backlog mutation source request differs from the authored intent")
	}
	if err = validateBacklogMutationIntent(entries, inventory); err != nil {
		return nil, err
	}
	if !same(p.Sources, backlogPlanSources(inventory, request)) {
		return nil, errors.New("backlog mutation source declarations differ from captured inventory")
	}
	issues := backlogMutationIssues(inventory)
	projectRows, _ := contract.Objects(inventory, "projects")
	projectByID := map[string]contract.Object{}
	for _, row := range projectRows {
		project, _ := contract.ObjectAt(row, "project")
		if id, err := contract.Nonempty(project, "id"); err == nil {
			projectByID[id] = row
		}
	}
	createIDs := map[string]string{}
	for i := range entries {
		if entries[i].Existing {
			continue
		}
		entries[i].CreateOperationID = backlogMutationCreateID(entries[i].Index)
		createIDs[entries[i].ClientID] = entries[i].CreateOperationID
		marker, err := backlogMutationCreateMarker(intent, p.Repository, entries[i].ClientID)
		if err != nil {
			return nil, err
		}
		entries[i].CreationMarker = marker
	}
	ops := []contract.Operation{}
	for i := range entries {
		e := &entries[i]
		issueRef := backlogMutationIssueReference(*e)
		if !e.Existing {
			body := backlogMutationCreatedBody(e.Body, e.CreationMarker)
			labels := e.Labels
			if !e.LabelsPresent {
				labels = []string{}
			}
			ops = append(ops, contract.Operation{ID: e.CreateOperationID, Kind: "issue-create",
				Target: contract.Object{"client_id": e.ClientID, "creation_marker": e.CreationMarker, "title": e.Title, "body": body, "labels": stringSliceAny(labels)},
				Before: contract.Object{"exists": false, "creation_marker": e.CreationMarker, "title": nil, "body": nil, "labels": nil, "state": nil, "milestone": nil},
				After:  contract.Object{"exists": true, "creation_marker": e.CreationMarker, "title": e.Title, "body": body, "labels": stringSliceAny(labels), "state": "OPEN", "milestone": nil}})
		}
		issue := contract.Object{}
		if e.Existing {
			captured := backlogIssueByNumber(issues, e.IssueNumber)
			issue, err = contract.Clone(captured)
			if err != nil {
				return nil, err
			}
		} else {
			initialLabels := append([]string{}, e.Labels...)
			issue = contract.Object{"state": "OPEN", "title": e.Title, "labels": stringSliceAny(initialLabels), "milestone": nil}
		}
		if e.Existing {
			if e.TitlePresent && issue["title"] != e.Title {
				ops = append(ops, backlogMutationIssueUpdate(*e, issueRef, "title", e.Title, issue, ""))
				issue["title"] = e.Title
			}
			if e.BodyPresent && issue["body"] != e.Body {
				ops = append(ops, backlogMutationIssueUpdate(*e, issueRef, "body", e.Body, issue, ""))
				issue["body"] = e.Body
			}
			if e.LabelsPresent {
				current := backlogMutationLabels(issue["labels"])
				desired := append([]string{}, e.Labels...)
				currentSet, desiredSet := stringSet(current), stringSet(desired)
				for _, label := range desired {
					if currentSet[label] {
						continue
					}
					next := append(append([]string{}, current...), label)
					sort.Strings(next)
					ops = append(ops, backlogMutationIssueUpdate(*e, issueRef, "labels", stringSliceAny(next), issue, "add:"+backlogMutationLabelDigest(label)))
					current, currentSet, issue["labels"] = next, stringSet(next), stringSliceAny(next)
				}
				for _, label := range append([]string{}, current...) {
					if desiredSet[label] {
						continue
					}
					next := []string{}
					for _, present := range current {
						if present != label {
							next = append(next, present)
						}
					}
					ops = append(ops, backlogMutationIssueUpdate(*e, issueRef, "labels", stringSliceAny(next), issue, "remove:"+backlogMutationLabelDigest(label)))
					current, currentSet, issue["labels"] = next, stringSet(next), stringSliceAny(next)
				}
			}
		}
		if e.MilestonePresent {
			currentMilestone := any(nil)
			if e.Existing {
				currentMilestone = issue["milestone"]
			}
			desiredMilestone := any(nil)
			if e.Milestone != nil {
				desiredMilestone = *e.Milestone
			}
			if !same(currentMilestone, desiredMilestone) {
				ops = append(ops, backlogMutationIssueUpdate(*e, issueRef, "milestone", desiredMilestone, issue, ""))
				issue["milestone"] = desiredMilestone
			}
		}
		if e.ProjectPresent && e.Project != nil && e.ProjectScope != nil {
			projectRow := projectByID[e.ProjectScope.ID]
			if projectRow == nil {
				return nil, errors.New("backlog Project scope is absent from the complete inventory")
			}
			items, _ := contract.Objects(projectRow, "items")
			var item contract.Object
			if e.Existing {
				for _, candidate := range items {
					if number, _ := contract.PositiveInteger(candidate["number"]); number == e.IssueNumber {
						item = candidate
						break
					}
				}
			}
			if item != nil && item["archived"] == true {
				return nil, fmt.Errorf("issue entry %d selects an archived Project item", e.Index)
			}
			membershipID := ""
			itemRef := contract.Object{}
			if item != nil {
				itemID, err := contract.Nonempty(item, "item_id")
				if err != nil {
					return nil, err
				}
				itemRef["item_id"] = itemID
			} else if e.Project.EnsureMembership {
				membershipID = backlogMutationEntryID(e.Index) + ":project:membership"
				e.ProjectMembershipID = membershipID
				var issueNode any
				if e.Existing {
					issueNode = issue["id"]
				}
				ops = append(ops, contract.Operation{ID: membershipID, Kind: "project-membership-add",
					Target: contract.Object{"project_id": e.ProjectScope.ID, "issue_ref": issueRef},
					Before: contract.Object{"present": false, "project_id": e.ProjectScope.ID, "issue_node_id": issueNode},
					After:  contract.Object{"present": true, "project_id": e.ProjectScope.ID, "issue_node_id": issueNode}})
				itemRef["from_operation"] = membershipID
			}
			if fieldValues, ok := e.Project.fieldValues(); ok {
				projectFields, _ := contract.ObjectAt(projectRow, "fields_by_name")
				for _, fieldName := range []string{"Status", "Priority", "Queue Order"} {
					desired, touched := fieldValues[fieldName]
					if !touched {
						continue
					}
					before := any(nil)
					if item != nil {
						values, _ := contract.ObjectAt(item, "field_values")
						before = values[fieldName]
						if same(before, desired) {
							continue
						}
					} else {
						before = contract.Object{"from_membership": membershipID, "field_name": fieldName}
					}
					definition, err := contract.ObjectAt(projectFields, fieldName)
					if err != nil {
						return nil, fmt.Errorf("Project is missing required field %q", fieldName)
					}
					_ = definition
					value := backlogMutationProjectFieldValue(fieldName, desired)
					if err = validateProjectField(projectRow, fieldName, value); err != nil {
						return nil, err
					}
					ops = append(ops, contract.Operation{ID: backlogMutationEntryID(e.Index) + ":project:field:" + fieldSlug(fieldName), Kind: "project-field-set",
						Target: contract.Object{"project_id": e.ProjectScope.ID, "project_title": e.ProjectScope.Title, "issue_ref": issueRef, "item_ref": itemRef, "field_name": fieldName},
						Before: contract.Object{"present": true, "item_ref": itemRef, "field_name": fieldName, "value": before},
						After:  contract.Object{"present": true, "item_ref": itemRef, "field_name": fieldName, "value": desired}})
				}
			}
		}
		relationshipOps, err := backlogMutationRelationshipOperations(*e, issue, issueRef, createIDs)
		if err != nil {
			return nil, err
		}
		ops = append(ops, relationshipOps...)
	}
	return ops, nil
}

func (p backlogMutationProject) fieldValues() (map[string]any, bool) {
	values := map[string]any{}
	if p.Status != nil {
		values["Status"] = *p.Status
	}
	if p.Priority != nil {
		values["Priority"] = *p.Priority
	}
	if p.QueueOrderSet {
		if p.QueueOrder == nil {
			values["Queue Order"] = nil
		} else {
			values["Queue Order"] = *p.QueueOrder
		}
	}
	return values, len(values) > 0
}

func backlogMutationProjectFieldValue(name string, value any) ProjectFieldValue {
	if value == nil {
		return ProjectFieldValue{Clear: name == "Queue Order"}
	}
	if text, ok := value.(string); ok {
		return ProjectFieldValue{Text: &text}
	}
	number, _ := contract.Number(value)
	return ProjectFieldValue{Number: &number}
}

func backlogMutationIssueUpdate(e backlogMutationEntry, issueRef contract.Object, field string, value any, current contract.Object, suffix string) contract.Operation {
	issue := contract.Object{"state": current["state"]}
	if id, ok := current["id"]; ok {
		issue["id"] = id
	}
	if current["title"] != nil {
		issue["title"] = current["title"]
	}
	if current["body"] != nil {
		issue["body"] = current["body"]
	}
	if labels, ok := current["labels"]; ok {
		issue["labels"] = labels
	}
	if milestone, ok := current["milestone"]; ok {
		issue["milestone"] = milestone
	}
	before := contract.Object{"issue": issue}
	afterIssue, _ := contract.Clone(issue)
	afterIssue[field] = value
	target := contract.Object{"issue_ref": issueRef, "field": field, "value": value}
	if e.Existing {
		target["issue_number"] = e.IssueNumber
	}
	return contract.Operation{ID: backlogMutationFieldOperationID(e.Index, field, suffix), Kind: "issue-update", Target: target, Before: before, After: contract.Object{"issue": afterIssue}}
}

func backlogMutationIssueReference(entry backlogMutationEntry) contract.Object {
	if entry.Existing {
		return contract.Object{"issue_number": entry.IssueNumber}
	}
	return contract.Object{"created_by": entry.CreateOperationID}
}

func backlogMutationEntryID(index int) string {
	return fmt.Sprintf("backlog-mutations:entry:%06d", index+1)
}
func backlogMutationCreateID(index int) string { return backlogMutationEntryID(index) + ":create" }

func backlogMutationCreatedBody(body, marker string) string {
	content := strings.TrimRight(body, "\n")
	if content == "" {
		return marker + "\n"
	}
	return content + "\n\n" + marker + "\n"
}

func backlogMutationLabels(raw any) []string {
	values, _ := contract.Strings(raw)
	sort.Strings(values)
	return values
}

func stringSet(values []string) map[string]bool {
	set := map[string]bool{}
	for _, value := range values {
		set[value] = true
	}
	return set
}

func backlogMutationReferenceObject(ref backlogMutationReference, creates map[string]string) (contract.Object, error) {
	if ref.kind == "issue_number" {
		return contract.Object{"issue_number": ref.issue}, nil
	}
	createID, exists := creates[ref.clientID]
	if !exists {
		return nil, fmt.Errorf("backlog mutation reference client_id %q does not resolve to a prior create", ref.clientID)
	}
	return contract.Object{"created_by": createID}, nil
}

func backlogMutationRelationshipOperations(entry backlogMutationEntry, issue contract.Object, issueRef contract.Object, creates map[string]string) ([]contract.Operation, error) {
	var removals, additions []contract.Operation
	if !entry.ParentPresent && !entry.BlockedByPresent {
		return nil, nil
	}
	appendEdge := func(relation string, left, right contract.Object, remove bool, discriminator string) error {
		target := contract.Object{"relation": relation, "issue_ref": left, "related_issue_ref": right}
		digest, err := contract.Digest(target)
		if err != nil {
			return err
		}
		kind, before, after := "issue-relationship-add", false, true
		group := &additions
		if remove {
			kind, before, after, group = "issue-relationship-remove", true, false, &removals
		}
		*group = append(*group, contract.Operation{ID: backlogMutationEntryID(entry.Index) + ":relationship:" + discriminator + ":" + digest[:16], Kind: kind, Target: target,
			Before: contract.Object{"present": before, "relation": relation}, After: contract.Object{"present": after, "relation": relation}})
		return nil
	}
	if entry.BlockedByPresent {
		current := map[int64]bool{}
		if entry.Existing {
			values, err := contract.Array(issue, "blocked_by_numbers")
			if err != nil {
				return nil, err
			}
			for _, raw := range values {
				number, err := contract.PositiveInteger(raw)
				if err != nil {
					return nil, err
				}
				current[number] = true
			}
		}
		desired := map[string]backlogMutationReference{}
		for _, ref := range entry.BlockedBy {
			desired[backlogMutationReferenceKey(ref)] = ref
		}
		for number := range current {
			ref := backlogMutationNumberRef(number)
			if _, keep := desired[backlogMutationReferenceKey(ref)]; keep {
				continue
			}
			right, _ := backlogMutationReferenceObject(ref, creates)
			if err := appendEdge(string(BlockedBy), issueRef, right, true, "blocked-remove"); err != nil {
				return nil, err
			}
		}
		refs := append([]backlogMutationReference{}, entry.BlockedBy...)
		sort.Slice(refs, func(i, j int) bool {
			return backlogMutationReferenceKey(refs[i]) < backlogMutationReferenceKey(refs[j])
		})
		for _, ref := range refs {
			if ref.kind == "issue_number" && current[ref.issue] {
				continue
			}
			right, err := backlogMutationReferenceObject(ref, creates)
			if err != nil {
				return nil, err
			}
			if err = appendEdge(string(BlockedBy), issueRef, right, false, "blocked-add"); err != nil {
				return nil, err
			}
		}
	}
	if entry.ParentPresent {
		var oldParent any
		if entry.Existing {
			oldParent = issue["parent_number"]
		}
		oldNumber := int64(0)
		if oldParent != nil {
			var err error
			oldNumber, err = contract.PositiveInteger(oldParent)
			if err != nil {
				return nil, err
			}
		}
		var newParent *backlogMutationReference
		if entry.Parent != nil {
			newParent = entry.Parent
		}
		if oldNumber > 0 && (newParent == nil || newParent.kind != "issue_number" || newParent.issue != oldNumber) {
			oldRef := backlogMutationNumberRef(oldNumber)
			left, _ := backlogMutationReferenceObject(oldRef, creates)
			if err := appendEdge(string(Child), left, issueRef, true, "parent-remove"); err != nil {
				return nil, err
			}
		}
		if newParent != nil && (oldNumber == 0 || newParent.kind != "issue_number" || newParent.issue != oldNumber) {
			left, err := backlogMutationReferenceObject(*newParent, creates)
			if err != nil {
				return nil, err
			}
			if err = appendEdge(string(Child), left, issueRef, false, "parent-add"); err != nil {
				return nil, err
			}
		}
	}
	return append(removals, additions...), nil
}

func backlogMutationIssues(inventory contract.Object) []contract.Object {
	graph, _ := contract.ObjectAt(inventory, "issue_inventory")
	rows, _ := contract.Objects(graph, "issues")
	return rows
}

func validateBacklogMutationIntent(entries []backlogMutationEntry, inventory contract.Object) error {
	issues := backlogMutationIssues(inventory)
	byNumber, open, openTitles := map[int64]contract.Object{}, map[int64]bool{}, map[string][]int64{}
	for _, issue := range issues {
		number, err := contract.PositiveInteger(issue["number"])
		if err != nil {
			return err
		}
		byNumber[number] = issue
		if backlogMutationIsOpen(issue) {
			open[number] = true
			title, err := contract.String(issue, "title")
			if err != nil {
				return err
			}
			openTitles[strings.TrimSpace(title)] = append(openTitles[strings.TrimSpace(title)], number)
		}
	}
	labels, milestones := map[string]bool{}, map[string]contract.Object{}
	for _, label := range mustObjects(inventory, "labels") {
		name, err := contract.Nonempty(label, "name")
		if err != nil {
			return err
		}
		labels[name] = true
	}
	for _, milestone := range mustObjects(inventory, "milestones") {
		title, err := contract.Nonempty(milestone, "title")
		if err != nil {
			return err
		}
		milestones[title] = milestone
	}
	seenIssues, seenClients, createTitles := map[int64]bool{}, map[string]bool{}, map[string]bool{}
	priorClients := map[string]bool{}
	for _, entry := range entries {
		if entry.Existing {
			if seenIssues[entry.IssueNumber] {
				return fmt.Errorf("duplicate issue_number entry: %d", entry.IssueNumber)
			}
			seenIssues[entry.IssueNumber] = true
			if byNumber[entry.IssueNumber] == nil {
				return fmt.Errorf("unknown issue_number: %d", entry.IssueNumber)
			}
		} else {
			if seenClients[entry.ClientID] {
				return fmt.Errorf("duplicate client_id entry: %s", entry.ClientID)
			}
			seenClients[entry.ClientID] = true
			for _, existing := range openTitles[entry.Title] {
				if !entry.AllowDuplicateTitle {
					return fmt.Errorf("create title matches existing open issue #%d: %s", existing, entry.Title)
				}
			}
			if createTitles[entry.Title] && !entry.AllowDuplicateTitle {
				return fmt.Errorf("create title is duplicated in plan: %s", entry.Title)
			}
			createTitles[entry.Title] = true
		}
		if entry.LabelsPresent {
			for _, label := range entry.Labels {
				if !labels[label] {
					return fmt.Errorf("issue entry %d references unknown repository label %q", entry.Index, label)
				}
			}
		}
		if entry.MilestonePresent && entry.Milestone != nil {
			milestone := milestones[*entry.Milestone]
			if milestone == nil || milestone["state"] != "open" {
				return fmt.Errorf("issue entry %d selects an absent or closed milestone %q", entry.Index, *entry.Milestone)
			}
		}
		if entry.ProjectPresent && entry.Project != nil {
			fieldValues, touched := entry.Project.fieldValues()
			if touched {
				if entry.ProjectScope == nil {
					return errors.New("Project mutation has no exact captured scope")
				}
				project, err := backlogProjectByID(inventory, entry.ProjectScope.ID)
				if err != nil {
					return err
				}
				for field, desired := range fieldValues {
					if err := validateProjectField(project, field, backlogMutationProjectFieldValue(field, desired)); err != nil {
						return err
					}
				}
			}
		}
		if entry.ParentPresent && entry.Parent != nil {
			if err := validateBacklogMutationReference(*entry.Parent, entry, byNumber, open, priorClients); err != nil {
				return err
			}
		}
		if entry.BlockedByPresent {
			for _, ref := range entry.BlockedBy {
				if err := validateBacklogMutationReference(ref, entry, byNumber, open, priorClients); err != nil {
					return err
				}
			}
		}
		if !entry.Existing {
			priorClients[entry.ClientID] = true
		}
	}
	if backlogMutationTouchesRelationships(entries) {
		if err := validateBacklogMutationTopology(entries, issues); err != nil {
			return err
		}
	}
	return nil
}

func backlogMutationCreateMarker(intent contract.Object, repo contract.Repository, clientID string) (string, error) {
	markerData, err := contract.Canonical(contract.Object{"repository": repo.Object(), "intent": intent, "client_id": clientID})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(markerData)
	return "<!-- gh-steward-backlog-mutation:" + hex.EncodeToString(digest[:]) + " -->", nil
}

func backlogMutationHasCycle(graph map[string]map[string]bool) bool {
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) bool
	visit = func(node string) bool {
		if visiting[node] {
			return true
		}
		if visited[node] {
			return false
		}
		visiting[node] = true
		neighbors := make([]string, 0, len(graph[node]))
		for next := range graph[node] {
			neighbors = append(neighbors, next)
		}
		sort.Strings(neighbors)
		for _, next := range neighbors {
			if visit(next) {
				return true
			}
		}
		delete(visiting, node)
		visited[node] = true
		return false
	}
	nodes := make([]string, 0, len(graph))
	for node := range graph {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		if visit(node) {
			return true
		}
	}
	return false
}

func backlogMutationReferenceNode(ref backlogMutationReference) string {
	return backlogMutationReferenceKey(ref)
}

func backlogMutationNumberRef(number int64) backlogMutationReference {
	return backlogMutationReference{kind: "issue_number", issue: number}
}

func backlogMutationIsOpen(issue contract.Object) bool {
	return strings.EqualFold(fmt.Sprint(issue["state"]), "OPEN")
}

func backlogMutationNumberNode(number int64) string { return "issue:" + strconv.FormatInt(number, 10) }

func validateBacklogMutationReference(ref backlogMutationReference, source backlogMutationEntry, byNumber map[int64]contract.Object, open map[int64]bool, priorClients map[string]bool) error {
	sourceNode := backlogMutationNumberNode(source.IssueNumber)
	if !source.Existing {
		sourceNode = "client:" + source.ClientID
	}
	targetNode := backlogMutationReferenceNode(ref)
	if targetNode == sourceNode {
		return fmt.Errorf("issue entry %d contains a self-referencing relationship", source.Index)
	}
	if ref.kind == "issue_number" {
		if byNumber[ref.issue] == nil || !open[ref.issue] {
			return fmt.Errorf("issue entry %d references an unknown or non-open issue #%d", source.Index, ref.issue)
		}
		return nil
	}
	if !priorClients[ref.clientID] {
		return fmt.Errorf("issue entry %d contains unresolved or forward client_id reference %q", source.Index, ref.clientID)
	}
	return nil
}

func validateBacklogMutationTopology(entries []backlogMutationEntry, issues []contract.Object) error {
	types := []string{"hierarchy", "dependencies"}
	for _, kind := range types {
		before, after := map[string]map[string]bool{}, map[string]map[string]bool{}
		add := func(graph map[string]map[string]bool, source, target string) {
			if graph[source] == nil {
				graph[source] = map[string]bool{}
			}
			graph[source][target] = true
			if graph[target] == nil {
				graph[target] = map[string]bool{}
			}
		}
		for _, issue := range issues {
			n, _ := contract.PositiveInteger(issue["number"])
			node := backlogMutationNumberNode(n)
			if kind == "hierarchy" {
				if issue["parent_number"] != nil {
					p, _ := contract.PositiveInteger(issue["parent_number"])
					add(before, node, backlogMutationNumberNode(p))
				}
			} else {
				refs, _ := contract.Array(issue, "blocked_by_numbers")
				for _, raw := range refs {
					p, _ := contract.PositiveInteger(raw)
					add(before, node, backlogMutationNumberNode(p))
				}
			}
		}
		for node, edges := range before {
			for target := range edges {
				add(after, node, target)
			}
		}
		anchors := map[string]bool{}
		for _, entry := range entries {
			node := backlogMutationNumberNode(entry.IssueNumber)
			if !entry.Existing {
				node = "client:" + entry.ClientID
			}
			if kind == "hierarchy" && entry.ParentPresent {
				anchors[node] = true
				delete(after, node)
				if entry.Parent != nil {
					target := backlogMutationReferenceNode(*entry.Parent)
					anchors[target] = true
					add(after, node, target)
				}
			}
			if kind == "dependencies" && entry.BlockedByPresent {
				anchors[node] = true
				delete(after, node)
				for _, ref := range entry.BlockedBy {
					target := backlogMutationReferenceNode(ref)
					anchors[target] = true
					add(after, node, target)
				}
			}
		}
		if len(anchors) == 0 {
			continue
		}
		connected := map[string]bool{}
		for anchor := range anchors {
			connected[anchor] = true
		}
		changed := true
		for changed {
			changed = false
			for source, edges := range before {
				for target := range edges {
					if connected[source] || connected[target] {
						if !connected[source] {
							connected[source] = true
							changed = true
						}
						if !connected[target] {
							connected[target] = true
							changed = true
						}
					}
				}
			}
		}
		relevant := map[string]map[string]bool{}
		for source, edges := range after {
			for target := range edges {
				if connected[source] || connected[target] {
					add(relevant, source, target)
				}
			}
		}
		if backlogMutationHasCycle(relevant) {
			return fmt.Errorf("%s relationship changes introduce a cycle", kind)
		}
	}
	return nil
}
