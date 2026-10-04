package workflow

import (
	"errors"
	"fmt"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/planning"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// normalizeBacklogInventory rejects partial or cross-repository sources and
// strips volatile timestamps so fresh-process comparisons are semantic.
func normalizeBacklogInventory(raw contract.Object, repo contract.Repository, request BacklogInventoryRequest) (contract.Object, error) {
	if err := validateInventoryRequest(request); err != nil {
		return nil, err
	}
	for _, scope := range request.Projects {
		if err := validateProjectScope(scope, repo); err != nil {
			return nil, err
		}
	}
	rawRepo, err := contract.ObjectAt(raw, "repo")
	if err != nil {
		return nil, err
	}
	actualRepo, err := contract.ParseRepository(rawRepo)
	if err != nil || actualRepo != repo {
		return nil, errors.New("backlog inventory targets another repository")
	}
	prov, err := contract.ObjectAt(raw, "provenance")
	if err != nil {
		return nil, err
	}
	if prov["live"] != true || prov["complete"] != true || prov["issue_state"] != "all" {
		return nil, errors.New("backlog apply requires complete live all-state inventory")
	}
	if _, err = contract.Nonempty(prov, "repository_node_id"); err != nil {
		return nil, err
	}
	graphRaw, err := contract.ObjectAt(raw, "issue_inventory")
	if err != nil {
		return nil, err
	}
	graph, err := normalizeBacklogIssueInventory(graphRaw, repo, request.IncludeRelationships)
	if err != nil {
		return nil, err
	}
	outerNode, _ := contract.Nonempty(prov, "repository_node_id")
	graphProv, _ := contract.ObjectAt(graph, "provenance")
	if graphProv["repository_node_id"] != outerNode {
		return nil, errors.New("backlog inventory repository node identities disagree")
	}
	if prov["milestones_source"] != "github_api" {
		return nil, errors.New("backlog inventory requires complete live repository milestones")
	}
	milestonesRaw, err := contract.Objects(raw, "milestones")
	if err != nil {
		return nil, err
	}
	milestones, err := normalizeMilestones(milestonesRaw, repo)
	if err != nil {
		return nil, err
	}
	if prov["labels_source"] != "github_api" {
		return nil, errors.New("backlog inventory requires complete live repository labels")
	}
	labelsRaw, err := contract.Objects(raw, "labels")
	if err != nil {
		return nil, err
	}
	labels, err := normalizeLabels(labelsRaw)
	if err != nil {
		return nil, err
	}

	if request.IncludeRelationships {
		if prov["relationships_source"] != "github_api" {
			return nil, errors.New("backlog relationship writes require complete live topology")
		}
		if _, err = planning.RelationshipTopology(graph); err != nil {
			return nil, err
		}
	} else if prov["relationships_source"] != "not_requested" && prov["relationships_source"] != "github_api" {
		return nil, errors.New("backlog relationship source provenance is invalid")
	}

	comments, err := normalizeBacklogComments(raw, repo, graph, request, prov)
	if err != nil {
		return nil, err
	}
	projects, err := normalizeBacklogProjects(raw, repo, request, prov, graph)
	if err != nil {
		return nil, err
	}
	graph["provenance"] = contract.Object{"live": true, "complete": true, "issue_state": "all", "repository_node_id": outerNode}
	return contract.Object{
		"repo": repo.Object(), "issue_inventory": graph, "milestones": milestones,
		"comments": comments, "labels": labels, "projects": projects,
		"provenance": contract.Object{"live": true, "complete": true, "issue_state": "all", "repository_node_id": outerNode,
			"milestones_source": "github_api", "labels_source": "github_api",
			"comments_source": func() string {
				if len(request.CommentMarkers) > 0 {
					return "github_api"
				}
				return "not_requested"
			}(),
			"comment_markers": stringSliceAny(request.CommentMarkers),
			"relationships_source": func() string {
				if request.IncludeRelationships {
					return "github_api"
				}
				return "not_requested"
			}(),
			"project_scopes": projectScopesAny(request.Projects),
		},
	}, nil
}

// Metadata-only requests must not require, invent or authorize topology. Both
// a wider captured graph and the native metadata source can be reduced to this
// reviewed facet; its identity and completeness still require positive evidence.
func normalizeBacklogIssueInventory(raw contract.Object, repo contract.Repository, relationships bool) (contract.Object, error) {
	if relationships {
		return relationshipInventory(raw, repo)
	}
	provenance, err := contract.ObjectAt(raw, "provenance")
	if err != nil {
		return nil, err
	}
	for key, expected := range (contract.Object{"issues_live": true, "issues_source": "github_api"}) {
		if value, exists := provenance[key]; exists && value != expected {
			return nil, errors.New("backlog issue metadata source is not complete live GitHub evidence")
		}
	}
	nodeID, err := contract.Nonempty(provenance, "repository_node_id")
	if err != nil {
		return nil, err
	}
	rows, err := contract.Objects(raw, "issues")
	if err != nil {
		return nil, err
	}
	if value, exists := raw["issue_count"]; exists {
		count, err := contract.Integer(value)
		if err != nil || count != int64(len(rows)) {
			return nil, errors.New("backlog issue metadata count differs from the complete inventory")
		}
	}
	items := make([]any, 0, len(rows))
	for _, row := range rows {
		item := contract.Object{}
		for _, key := range []string{"id", "number", "title", "body", "state", "url", "labels", "milestone"} {
			value, exists := row[key]
			if !exists {
				return nil, fmt.Errorf("complete backlog issue metadata is missing %s", key)
			}
			item[key] = value
		}
		items = append(items, item)
	}
	return normalizeExecutionIssueInventory(contract.Object{"repo": raw["repo"], "issues": items,
		"provenance": contract.Object{"live": provenance["live"], "complete": provenance["complete"], "issue_state": provenance["issue_state"], "repository_node_id": nodeID}}, repo, nodeID)
}

func normalizeMilestones(rows []contract.Object, repo contract.Repository) ([]any, error) {
	out := make([]any, 0, len(rows))
	numbers, ids, titles := map[int64]bool{}, map[string]bool{}, map[string]bool{}
	for _, row := range rows {
		number, e := contract.PositiveInteger(row["number"])
		if e != nil || numbers[number] {
			return nil, errors.New("milestone inventory has an invalid or duplicate number")
		}
		id, e := contract.PositiveInteger(row["id"])
		if e != nil || ids[strconv.FormatInt(id, 10)] {
			return nil, errors.New("milestone inventory has an invalid or duplicate stable ID")
		}
		nodeID, e := contract.Nonempty(row, "node_id")
		if e != nil {
			return nil, e
		}
		title, e := contract.Nonempty(row, "title")
		if e != nil || titles[title] {
			return nil, errors.New("milestone inventory has a missing or duplicate title")
		}
		state, e := contract.String(row, "state")
		if e != nil {
			return nil, e
		}
		state = strings.ToLower(state)
		if state != "open" && state != "closed" {
			return nil, errors.New("milestone state is invalid")
		}
		description, ok := row["description"]
		if !ok || (description != nil && func() bool { _, ok := description.(string); return !ok }()) {
			return nil, errors.New("milestone description must be explicit string or null")
		}
		if description == nil {
			description = ""
		}
		dueOn, ok := row["due_on"]
		if !ok || (dueOn != nil && func() bool { _, ok := dueOn.(string); return !ok }()) {
			return nil, errors.New("milestone due date must be explicit string or null")
		}
		if dueOn != nil {
			stamp, err := time.Parse(time.RFC3339Nano, dueOn.(string))
			if err != nil {
				return nil, errors.New("milestone due date must be RFC3339 or null")
			}
			dueOn = stamp.UTC().Format(time.RFC3339Nano)
		}
		urlRaw, e := contract.Nonempty(row, "url")
		if e != nil || !validMilestoneURL(urlRaw, repo, number) {
			return nil, errors.New("milestone URL does not match repository identity")
		}
		numbers[number], ids[strconv.FormatInt(id, 10)], titles[title] = true, true, true
		out = append(out, contract.Object{"id": id, "node_id": nodeID, "number": number, "title": title, "description": description, "state": state, "due_on": dueOn, "url": urlRaw})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].(map[string]any)["number"].(int64) < out[j].(map[string]any)["number"].(int64)
	})
	return out, nil
}

func validMilestoneURL(raw string, repo contract.Repository, number int64) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return strings.EqualFold(u.Hostname(), repo.Host) && strings.EqualFold(u.EscapedPath(), fmt.Sprintf("/%s/milestone/%d", repo.FullName(), number))
}

func normalizeLabels(rows []contract.Object) ([]any, error) {
	out := make([]any, 0, len(rows))
	names, ids, nodes := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, row := range rows {
		id, e := contract.PositiveInteger(row["id"])
		if e != nil || ids[strconv.FormatInt(id, 10)] {
			return nil, errors.New("label inventory has invalid or duplicate ID")
		}
		node, e := contract.Nonempty(row, "node_id")
		if e != nil || nodes[node] {
			return nil, errors.New("label inventory has invalid or duplicate node ID")
		}
		name, e := contract.Nonempty(row, "name")
		if e != nil || names[name] {
			return nil, errors.New("label inventory has invalid or duplicate name")
		}
		color, e := contract.Nonempty(row, "color")
		if e != nil {
			return nil, e
		}
		if len(color) != 6 {
			return nil, errors.New("label color must be six hexadecimal digits")
		}
		if _, e = strconv.ParseUint(color, 16, 24); e != nil {
			return nil, errors.New("label color must be six hexadecimal digits")
		}
		description, ok := row["description"]
		if !ok || (description != nil && func() bool { _, ok := description.(string); return !ok }()) {
			return nil, errors.New("label description must be explicit string or null")
		}
		if description == nil {
			description = ""
		}
		ids[strconv.FormatInt(id, 10)], nodes[node], names[name] = true, true, true
		out = append(out, contract.Object{"id": id, "node_id": node, "name": name, "color": strings.ToLower(color), "description": description})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].(map[string]any)["name"].(string) < out[j].(map[string]any)["name"].(string)
	})
	return out, nil
}

func normalizeBacklogComments(raw contract.Object, repo contract.Repository, graph contract.Object, request BacklogInventoryRequest, prov contract.Object) ([]any, error) {
	rows, err := contract.Objects(raw, "comments")
	if err != nil {
		return nil, err
	}
	if len(request.CommentMarkers) == 0 {
		if prov["comments_source"] != "not_requested" || len(rows) != 0 {
			return nil, errors.New("unexpected backlog comment inventory")
		}
		return []any{}, nil
	}
	if prov["comments_source"] != "github_api" {
		return nil, errors.New("backlog comments require complete live source evidence")
	}
	markers, err := contract.Strings(prov["comment_markers"])
	if err != nil || !same(stringSliceAny(markers), stringSliceAny(request.CommentMarkers)) {
		return nil, errors.New("backlog comment marker scope differs from request")
	}
	issueRows, _ := contract.Objects(graph, "issues")
	known := map[int64]bool{}
	for _, i := range issueRows {
		n, _ := contract.PositiveInteger(i["number"])
		known[n] = true
	}
	seen := map[int64]bool{}
	commentByMarker := map[string]bool{}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		id, e := contract.PositiveInteger(row["id"])
		if e != nil || seen[id] {
			return nil, errors.New("comment inventory has invalid or duplicate ID")
		}
		n, e := contract.PositiveInteger(row["issue_number"])
		if e != nil || !known[n] {
			return nil, errors.New("comment inventory issue identity is outside complete repository graph")
		}
		body, e := contract.String(row, "body")
		if e != nil {
			return nil, e
		}
		urlRaw, e := contract.Nonempty(row, "url")
		if e != nil || !validCommentURL(urlRaw, repo, n, id) {
			return nil, errors.New("comment URL does not match repository and comment identity")
		}
		matched := false
		for _, marker := range request.CommentMarkers {
			if strings.Contains(body, marker) {
				matched = true
				if commentByMarker[strconv.FormatInt(n, 10)+"\x00"+marker] {
					return nil, errors.New("comment marker identifies multiple comments on one issue")
				}
				commentByMarker[strconv.FormatInt(n, 10)+"\x00"+marker] = true
			}
		}
		if !matched {
			return nil, errors.New("comment inventory contains a row outside requested marker scope")
		}
		seen[id] = true
		out = append(out, contract.Object{"id": id, "issue_number": n, "body": body, "url": urlRaw})
	}
	sort.Slice(out, func(i, j int) bool {
		a := out[i].(map[string]any)
		b := out[j].(map[string]any)
		if a["issue_number"].(int64) != b["issue_number"].(int64) {
			return a["issue_number"].(int64) < b["issue_number"].(int64)
		}
		return a["id"].(int64) < b["id"].(int64)
	})
	return out, nil
}

func validCommentURL(raw string, repo contract.Repository, issue, comment int64) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") || u.RawQuery != "" || !strings.EqualFold(u.Hostname(), repo.Host) || !strings.EqualFold(u.EscapedPath(), fmt.Sprintf("/%s/issues/%d", repo.FullName(), issue)) {
		return false
	}
	return u.Fragment == "issuecomment-"+strconv.FormatInt(comment, 10)
}

func normalizeBacklogProjects(raw contract.Object, repo contract.Repository, request BacklogInventoryRequest, prov contract.Object, graph contract.Object) ([]any, error) {
	rows, err := contract.Objects(raw, "projects")
	if err != nil {
		return nil, err
	}
	if len(rows) != len(request.Projects) {
		return nil, errors.New("backlog Project inventory does not cover requested scopes")
	}
	declared, err := contract.Objects(prov, "project_scopes")
	if err != nil || len(declared) != len(request.Projects) {
		return nil, errors.New("backlog Project source scope provenance is incomplete")
	}
	for i, p := range request.Projects {
		if !same(declared[i], p.Object()) {
			return nil, errors.New("backlog Project source scope differs from reviewed request")
		}
	}
	out := make([]any, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		r, err := contract.ObjectAt(row, "repo")
		if err != nil {
			return nil, err
		}
		actual, err := contract.ParseRepository(r)
		if err != nil || actual != repo {
			return nil, errors.New("backlog Project snapshot targets another repository")
		}
		p, err := contract.ObjectAt(row, "project")
		if err != nil {
			return nil, err
		}
		id, err := contract.Nonempty(p, "id")
		if err != nil {
			return nil, err
		}
		var scope *ProjectScope
		for i := range request.Projects {
			if request.Projects[i].ID == id {
				scope = &request.Projects[i]
				break
			}
		}
		if scope == nil || seen[id] {
			return nil, errors.New("backlog Project snapshot is outside the requested identity set")
		}
		if p["title"] != scope.Title || p["owner_login"] != scope.Owner || p["owner_type"] != scope.OwnerType || p["host"] != scope.Host {
			return nil, errors.New("backlog Project identity changed")
		}
		num, err := contract.PositiveInteger(p["number"])
		if err != nil || num != scope.Number {
			return nil, errors.New("backlog Project number changed")
		}
		projectProv, err := contract.ObjectAt(row, "provenance")
		if err != nil || projectProv["live"] != true || projectProv["complete"] != true || projectProv["source"] != "github_project_api" {
			return nil, errors.New("backlog Project snapshot is not complete live evidence")
		}
		fields, err := contract.ObjectAt(row, "fields_by_name")
		if err != nil {
			return nil, err
		}
		items, err := contract.Objects(row, "items")
		if err != nil {
			return nil, err
		}
		knownIssues := map[int64]bool{}
		for _, issue := range mustObjects(graph, "issues") {
			n, _ := contract.PositiveInteger(issue["number"])
			knownIssues[n] = true
		}
		itemIDs, itemNumbers := map[string]bool{}, map[int64]bool{}
		normItems := make([]any, 0, len(items))
		for _, item := range items {
			itemID, e := contract.Nonempty(item, "item_id")
			if e != nil || itemIDs[itemID] {
				return nil, errors.New("Project item has invalid or duplicate identity")
			}
			n, e := contract.PositiveInteger(item["number"])
			if e != nil || itemNumbers[n] || !knownIssues[n] {
				return nil, errors.New("Project issue membership is invalid or duplicated")
			}
			values, e := contract.ObjectAt(item, "field_values")
			if e != nil {
				return nil, e
			}
			archived, e := contract.Bool(item, "archived")
			if e != nil {
				return nil, e
			}
			itemIDs[itemID], itemNumbers[n] = true, true
			normItems = append(normItems, contract.Object{"item_id": itemID, "number": n, "field_values": values, "archived": archived})
		}
		sort.Slice(normItems, func(i, j int) bool {
			return normItems[i].(map[string]any)["number"].(int64) < normItems[j].(map[string]any)["number"].(int64)
		})
		meta := contract.Object{"id": id, "number": num, "title": scope.Title, "owner_login": scope.Owner, "owner_type": scope.OwnerType, "host": scope.Host}
		seen[id] = true
		out = append(out, contract.Object{"project": meta, "fields_by_name": fields, "items": normItems})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].(map[string]any)["project"].(map[string]any)["id"].(string) < out[j].(map[string]any)["project"].(map[string]any)["id"].(string)
	})
	return out, nil
}

func stringSliceAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func projectScopesAny(values []ProjectScope) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v.Object()
	}
	return out
}

func validateStoredBacklogInventory(raw contract.Object, repo contract.Repository, request BacklogInventoryRequest) (contract.Object, error) {
	r, err := contract.ObjectAt(raw, "repo")
	if err != nil {
		return nil, err
	}
	actual, err := contract.ParseRepository(r)
	if err != nil || actual != repo {
		return nil, errors.New("captured backlog inventory targets another repository")
	}
	prov, err := contract.ObjectAt(raw, "provenance")
	if err != nil {
		return nil, err
	}
	if prov["live"] != true || prov["complete"] != true || prov["issue_state"] != "all" {
		return nil, errors.New("captured backlog inventory is not complete live all-state evidence")
	}
	graphRaw, err := contract.ObjectAt(raw, "issue_inventory")
	if err != nil {
		return nil, err
	}
	graph, err := normalizeBacklogIssueInventory(graphRaw, repo, request.IncludeRelationships)
	if err != nil {
		return nil, err
	}
	if prov["repository_node_id"] != graph["provenance"].(map[string]any)["repository_node_id"] {
		return nil, errors.New("captured backlog repository node identities disagree")
	}
	if prov["milestones_source"] != "github_api" || prov["labels_source"] != "github_api" {
		return nil, errors.New("captured backlog milestone or label source is not live and complete")
	}
	milestoneRows, err := contract.Objects(raw, "milestones")
	if err != nil {
		return nil, err
	}
	milestones, err := normalizeMilestones(milestoneRows, repo)
	if err != nil {
		return nil, err
	}
	labelRows, err := contract.Objects(raw, "labels")
	if err != nil {
		return nil, err
	}
	labels, err := normalizeLabels(labelRows)
	if err != nil {
		return nil, err
	}
	if request.IncludeRelationships {
		if prov["relationships_source"] != "github_api" {
			return nil, errors.New("captured backlog relationship topology is incomplete")
		}
		if _, err = planning.RelationshipTopology(graph); err != nil {
			return nil, err
		}
	} else if prov["relationships_source"] != "not_requested" {
		return nil, errors.New("captured backlog relationship scope changed")
	}
	if len(request.CommentMarkers) > 0 {
		if prov["comments_source"] != "github_api" {
			return nil, errors.New("captured backlog comments are not live and complete")
		}
		markers, err := contract.Strings(prov["comment_markers"])
		if err != nil || !same(stringSliceAny(markers), stringSliceAny(request.CommentMarkers)) {
			return nil, errors.New("captured backlog comment scope changed")
		}
	} else if prov["comments_source"] != "not_requested" {
		return nil, errors.New("captured backlog comment scope changed")
	}
	comments, err := contract.Objects(raw, "comments")
	if err != nil {
		return nil, err
	}
	commentRows := make([]any, 0, len(comments))
	known := map[int64]bool{}
	for _, issue := range mustObjects(graph, "issues") {
		n, _ := contract.PositiveInteger(issue["number"])
		known[n] = true
	}
	ids := map[int64]bool{}
	matched := map[string]bool{}
	for _, comment := range comments {
		n, e := contract.PositiveInteger(comment["issue_number"])
		if e != nil || !known[n] {
			return nil, errors.New("captured comment references an issue outside complete inventory")
		}
		id, e := contract.PositiveInteger(comment["id"])
		if e != nil || ids[id] {
			return nil, errors.New("captured comments have invalid or duplicate IDs")
		}
		body, e := contract.String(comment, "body")
		if e != nil {
			return nil, e
		}
		link, e := contract.Nonempty(comment, "url")
		if e != nil || !validCommentURL(link, repo, n, id) {
			return nil, errors.New("captured comment URL has invalid identity")
		}
		inside := false
		for _, marker := range request.CommentMarkers {
			if strings.Contains(body, marker) {
				k := strconv.FormatInt(n, 10) + "\x00" + marker
				if matched[k] {
					return nil, errors.New("captured comment marker is ambiguous")
				}
				matched[k] = true
				inside = true
			}
		}
		if !inside {
			return nil, errors.New("captured comment is outside requested marker scope")
		}
		ids[id] = true
		commentRows = append(commentRows, contract.Object{"id": id, "issue_number": n, "body": body, "url": link})
	}
	sort.Slice(commentRows, func(i, j int) bool {
		a := commentRows[i].(map[string]any)
		b := commentRows[j].(map[string]any)
		if a["issue_number"].(int64) != b["issue_number"].(int64) {
			return a["issue_number"].(int64) < b["issue_number"].(int64)
		}
		return a["id"].(int64) < b["id"].(int64)
	})
	projects, err := validateStoredProjects(raw, repo, request, prov, graph)
	if err != nil {
		return nil, err
	}
	graph["provenance"] = contract.Object{"live": true, "complete": true, "issue_state": "all", "repository_node_id": prov["repository_node_id"]}
	return contract.Object{"repo": repo.Object(), "issue_inventory": graph, "milestones": milestones, "comments": commentRows, "labels": labels, "projects": projects, "provenance": contract.Object{"live": true, "complete": true, "issue_state": "all", "repository_node_id": prov["repository_node_id"], "milestones_source": "github_api", "labels_source": "github_api", "comments_source": prov["comments_source"], "comment_markers": stringSliceAny(request.CommentMarkers), "relationships_source": prov["relationships_source"], "project_scopes": projectScopesAny(request.Projects)}}, nil
}

func validateStoredProjects(raw contract.Object, repo contract.Repository, request BacklogInventoryRequest, prov contract.Object, graph contract.Object) ([]any, error) {
	rows, err := contract.Objects(raw, "projects")
	if err != nil {
		return nil, err
	}
	declared, err := contract.Objects(prov, "project_scopes")
	if err != nil || len(rows) != len(request.Projects) || len(declared) != len(request.Projects) {
		return nil, errors.New("captured Project inventory scope is incomplete")
	}
	for i, p := range request.Projects {
		if !same(declared[i], p.Object()) {
			return nil, errors.New("captured Project inventory scope changed")
		}
	}
	seen := map[string]bool{}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		project, err := contract.ObjectAt(row, "project")
		if err != nil {
			return nil, err
		}
		id, err := contract.Nonempty(project, "id")
		if err != nil || seen[id] {
			return nil, errors.New("captured Project identity is invalid or duplicated")
		}
		var scope *ProjectScope
		for i := range request.Projects {
			if request.Projects[i].ID == id {
				scope = &request.Projects[i]
				break
			}
		}
		if scope == nil {
			return nil, errors.New("captured Project is outside reviewed scopes")
		}
		num, err := contract.PositiveInteger(project["number"])
		if err != nil || num != scope.Number || project["title"] != scope.Title || project["owner_login"] != scope.Owner || project["owner_type"] != scope.OwnerType || project["host"] != scope.Host {
			return nil, errors.New("captured Project identity changed")
		}
		fields, err := contract.ObjectAt(row, "fields_by_name")
		if err != nil {
			return nil, err
		}
		items, err := contract.Objects(row, "items")
		if err != nil {
			return nil, err
		}
		knownIssues := map[int64]bool{}
		for _, issue := range mustObjects(graph, "issues") {
			n, _ := contract.PositiveInteger(issue["number"])
			knownIssues[n] = true
		}
		itemIDs, itemNums := map[string]bool{}, map[int64]bool{}
		norm := []any{}
		for _, item := range items {
			itemID, e := contract.Nonempty(item, "item_id")
			if e != nil || itemIDs[itemID] {
				return nil, errors.New("captured Project item identity is invalid")
			}
			n, e := contract.PositiveInteger(item["number"])
			if e != nil || itemNums[n] || !knownIssues[n] {
				return nil, errors.New("captured Project issue membership is invalid")
			}
			fv, e := contract.ObjectAt(item, "field_values")
			if e != nil {
				return nil, e
			}
			arch, e := contract.Bool(item, "archived")
			if e != nil {
				return nil, e
			}
			itemIDs[itemID], itemNums[n] = true, true
			norm = append(norm, contract.Object{"item_id": itemID, "number": n, "field_values": fv, "archived": arch})
		}
		sort.Slice(norm, func(i, j int) bool {
			return norm[i].(map[string]any)["number"].(int64) < norm[j].(map[string]any)["number"].(int64)
		})
		seen[id] = true
		out = append(out, contract.Object{"project": contract.Object{"id": id, "number": num, "title": scope.Title, "owner_login": scope.Owner, "owner_type": scope.OwnerType, "host": scope.Host}, "fields_by_name": fields, "items": norm})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].(map[string]any)["project"].(map[string]any)["id"].(string) < out[j].(map[string]any)["project"].(map[string]any)["id"].(string)
	})
	return out, nil
}
