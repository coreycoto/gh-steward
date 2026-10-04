// Package snapshot normalizes complete native reads. It never substitutes
// checked-in seeds for failed live reads or treats inaccessible data as empty.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

type Reader interface {
	ReadRepository(context.Context) (contract.Object, error)
	ReadIssuesPage(context.Context, string, *string, bool) (contract.Object, error)
	ReadProject(context.Context, native.ProjectScope) (contract.Object, error)
	ReadProjectPage(context.Context, native.ProjectScope, string, *string) (contract.Object, error)
	RESTPages(context.Context, string) ([]any, error)
}

type Service struct {
	Reader     Reader
	Repository contract.Repository
	Now        func() time.Time
}

func (s Service) stamp() string {
	if s.Now != nil {
		return s.Now().UTC().Format(time.RFC3339Nano)
	}
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func (s Service) validateRepository(raw contract.Object) error {
	r, err := contract.ParseRepository(raw)
	if err != nil || r != s.Repository {
		return errors.New("snapshot repository does not match its selected target")
	}
	return nil
}

func (s Service) CurrentRepository(ctx context.Context) (contract.Object, error) {
	r, err := s.Reader.ReadRepository(ctx)
	if err != nil {
		return nil, err
	}
	if err = s.validateRepository(r); err != nil {
		return nil, err
	}
	owner, err := contract.ObjectAt(r, "owner")
	if err != nil {
		return nil, err
	}
	login, err := contract.Nonempty(owner, "login")
	if err != nil || !strings.EqualFold(login, s.Repository.Owner) {
		return nil, errors.New("repository owner does not match target")
	}
	branch := ""
	if r["defaultBranchRef"] != nil {
		b, err := contract.ObjectAt(r, "defaultBranchRef")
		if err != nil {
			return nil, err
		}
		branch, err = contract.Nonempty(b, "name")
		if err != nil {
			return nil, err
		}
	}
	return contract.Object{"repo": r, "default_branch": branch, "generated_at": s.stamp(), "provenance": contract.Object{"live": true, "complete": true, "source": "github_api"}}, nil
}

func collectionPage(all *[]contract.Object, root contract.Object, key string, cursor **string, seen map[string]bool) (bool, error) {
	nodes, page, err := native.Connection(root, key)
	if err != nil {
		return false, err
	}
	*all = append(*all, nodes...)
	if len(*all) > 100000 {
		return false, errors.New("live collection exceeds supported size; completeness is unproven")
	}
	if page["hasNextPage"] != true {
		return false, nil
	}
	next, err := contract.Nonempty(page, "endCursor")
	if err != nil || seen[next] {
		return false, errors.New("live pagination cursor did not advance")
	}
	seen[next] = true
	*cursor = &next
	return true, nil
}

func (s Service) Issues(ctx context.Context, state string, includeProjects bool) ([]contract.Object, error) {
	issues, _, err := s.readIssues(ctx, state, includeProjects)
	return issues, err
}

// IssueInventory captures only complete issue metadata. It does not imply that
// dependency topology was queried, which lets execution sync avoid unused
// relationship calls for every issue in the repository.
func (s Service) IssueInventory(ctx context.Context, state string) (contract.Object, error) {
	issues, repositoryNodeID, err := s.readIssues(ctx, state, false)
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(issues))
	for _, issue := range issues {
		items = append(items, issue)
	}
	return contract.Object{"repo": s.Repository.Object(), "issues": items, "issue_count": len(items), "generated_at": s.stamp(),
		"provenance": contract.Object{"live": true, "complete": true, "issues_live": true, "issues_source": "github_api", "issue_state": strings.ToLower(state), "repository_node_id": repositoryNodeID, "relationships_requested": false}}, nil
}

func (s Service) readIssues(ctx context.Context, state string, includeProjects bool) ([]contract.Object, string, error) {
	var all []contract.Object
	var cursor *string
	repositoryNodeID := ""
	seen := map[string]bool{}
	for {
		r, err := s.Reader.ReadIssuesPage(ctx, state, cursor, includeProjects)
		if err != nil {
			return nil, "", err
		}
		if err = s.validateRepository(r); err != nil {
			return nil, "", err
		}
		id, err := contract.Nonempty(r, "id")
		if err != nil {
			return nil, "", err
		}
		if repositoryNodeID != "" && repositoryNodeID != id {
			return nil, "", errors.New("repository immutable node identity changed during pagination")
		}
		repositoryNodeID = id
		more, err := collectionPage(&all, r, "issues", &cursor, seen)
		if err != nil {
			return nil, "", err
		}
		if !more {
			break
		}
	}
	out := make([]contract.Object, 0, len(all))
	identities := map[int64]bool{}
	nodeIDs := map[string]bool{}
	for _, raw := range all {
		issue, err := NormalizeIssue(raw, s.Repository, includeProjects)
		if err != nil {
			return nil, "", err
		}
		n, _ := contract.PositiveInteger(issue["number"])
		id, _ := contract.Nonempty(issue, "id")
		if identities[n] || nodeIDs[id] {
			return nil, "", errors.New("issue inventory contains duplicate identity")
		}
		identities[n] = true
		nodeIDs[id] = true
		out = append(out, issue)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := contract.PositiveInteger(out[i]["number"])
		b, _ := contract.PositiveInteger(out[j]["number"])
		return a < b
	})
	return out, repositoryNodeID, nil
}

func names(raw contract.Object, key, scalar string) ([]any, error) {
	nodes, err := native.CompleteConnection(raw, key)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(nodes))
	seen := map[string]bool{}
	for _, node := range nodes {
		name, err := contract.Nonempty(node, scalar)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("%s has duplicate %s", key, scalar)
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].(string) < out[j].(string) })
	return out, nil
}

func issueURL(raw any, repo contract.Repository, number int64, allowAPI bool) error {
	str, ok := raw.(string)
	if !ok {
		return errors.New("issue identity requires a URL")
	}
	u, err := url.Parse(str)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("issue URL is not a qualified HTTPS identity")
	}
	expected := fmt.Sprintf("/%s/issues/%d", repo.FullName(), number)
	host := strings.ToLower(u.Hostname())
	if allowAPI && ((repo.Host == "github.com" && host == "api.github.com") || host == repo.Host) {
		api := fmt.Sprintf("/repos/%s/issues/%d", repo.FullName(), number)
		if strings.EqualFold(u.EscapedPath(), api) || (repo.Host != "github.com" && strings.EqualFold(u.EscapedPath(), "/api/v3"+api)) {
			return nil
		}
	}
	if host != repo.Host || !strings.EqualFold(u.EscapedPath(), expected) {
		return errors.New("issue URL belongs to a different repository or issue")
	}
	return nil
}

func NormalizeIssue(raw contract.Object, repo contract.Repository, includeProjects bool) (contract.Object, error) {
	id, err := contract.Nonempty(raw, "id")
	if err != nil {
		return nil, err
	}
	number, err := contract.PositiveInteger(raw["number"])
	if err != nil {
		return nil, err
	}
	if err = issueURL(raw["url"], repo, number, false); err != nil {
		return nil, err
	}
	for _, key := range []string{"title", "body", "state"} {
		if _, err := contract.String(raw, key); err != nil {
			return nil, err
		}
	}
	if raw["state"] != "OPEN" && raw["state"] != "CLOSED" {
		return nil, errors.New("issue state must be OPEN or CLOSED")
	}
	labels, err := names(raw, "labels", "name")
	if err != nil {
		return nil, err
	}
	var milestone any
	if _, ok := raw["milestone"]; !ok {
		return nil, errors.New("issue milestone nullable field is missing")
	}
	if raw["milestone"] != nil {
		m, err := contract.ObjectAt(raw, "milestone")
		if err != nil {
			return nil, err
		}
		title, err := contract.String(m, "title")
		if err != nil {
			return nil, err
		}
		milestone = title
	}
	items := []any{}
	if includeProjects {
		nodes, err := native.CompleteConnection(raw, "projectItems")
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, node := range nodes {
			id, err := contract.Nonempty(node, "id")
			if err != nil || seen[id] {
				return nil, errors.New("Project membership identity missing or duplicated")
			}
			seen[id] = true
			project, err := contract.ObjectAt(node, "project")
			if err != nil {
				return nil, err
			}
			for _, key := range []string{"id", "title", "url"} {
				if _, err := contract.Nonempty(project, key); err != nil {
					return nil, err
				}
			}
			if _, err := contract.PositiveInteger(project["number"]); err != nil {
				return nil, err
			}
			items = append(items, contract.Object{"id": id, "project": project})
		}
	}
	return contract.Object{"id": id, "number": number, "title": raw["title"], "body": raw["body"], "state": raw["state"], "url": raw["url"], "labels": labels, "milestone": milestone, "project_items": items}, nil
}

func (s Service) projectCollection(ctx context.Context, p native.ProjectScope, key string) ([]contract.Object, error) {
	var all []contract.Object
	var cursor *string
	seen := map[string]bool{}
	for {
		page, err := s.Reader.ReadProjectPage(ctx, p, key, cursor)
		if err != nil {
			return nil, err
		}
		more, err := collectionPage(&all, page, key, &cursor, seen)
		if err != nil {
			return nil, err
		}
		if !more {
			return all, nil
		}
	}
}

func NormalizeFields(raw []contract.Object) (contract.Object, error) {
	out := contract.Object{}
	seenID := map[string]bool{}
	for _, field := range raw {
		id, err := contract.Nonempty(field, "id")
		if err != nil || seenID[id] {
			return nil, errors.New("Project field identities missing or duplicated")
		}
		seenID[id] = true
		name, err := contract.Nonempty(field, "name")
		if err != nil {
			return nil, err
		}
		if _, exists := out[name]; exists {
			return nil, errors.New("Project field names are ambiguous")
		}
		dataType, err := contract.Nonempty(field, "dataType")
		if err != nil {
			return nil, err
		}
		options := contract.Object{}
		if dataType == "SINGLE_SELECT" || dataType == "MULTI_SELECT" {
			if _, exists := field["options"]; !exists {
				return nil, errors.New("queried Project option definitions are missing")
			}
		}
		if rawOptions, exists := field["options"]; exists {
			a, ok := rawOptions.([]any)
			if !ok {
				return nil, errors.New("Project options must be an array")
			}
			seenOptionID := map[string]bool{}
			for _, entry := range a {
				o, ok := entry.(map[string]any)
				if !ok {
					return nil, errors.New("Project option must be an object")
				}
				n, err := contract.Nonempty(o, "name")
				if err != nil {
					return nil, err
				}
				optionID, err := contract.Nonempty(o, "id")
				if err != nil || seenOptionID[optionID] {
					return nil, errors.New("Project option identity missing or duplicated")
				}
				if _, exists := options[n]; exists {
					return nil, errors.New("Project option names are ambiguous")
				}
				seenOptionID[optionID] = true
				options[n] = o
			}
		}
		definition := contract.Object{"id": id, "name": name, "data_type": dataType, "options_by_name": options}
		if dataType == "ITERATION" {
			configuration, err := contract.ObjectAt(field, "configuration")
			if err != nil {
				return nil, err
			}
			retained := contract.Object{}
			seenIterations := map[string]bool{}
			for _, key := range []string{"iterations", "completedIterations"} {
				rows, err := contract.Objects(configuration, key)
				if err != nil {
					return nil, err
				}
				values := []any{}
				for _, row := range rows {
					iterationID, err := contract.Nonempty(row, "id")
					if err != nil || seenIterations[iterationID] {
						return nil, errors.New("Project iteration identity missing or duplicated")
					}
					seenIterations[iterationID] = true
					title, err := contract.String(row, "title")
					if err != nil {
						return nil, err
					}
					start, err := contract.Nonempty(row, "startDate")
					if err != nil {
						return nil, err
					}
					if _, err := time.Parse("2006-01-02", start); err != nil {
						return nil, errors.New("Project iteration start date invalid")
					}
					duration, err := contract.PositiveInteger(row["duration"])
					if err != nil {
						return nil, err
					}
					values = append(values, contract.Object{"id": iterationID, "title": title, "startDate": start, "duration": duration})
				}
				retained[key] = values
			}
			definition["configuration"] = retained
		}
		out[name] = definition
	}
	return out, nil
}

func NormalizeFieldValues(item contract.Object) (contract.Object, error) {
	nodes, err := native.CompleteConnection(item, "fieldValues")
	if err != nil {
		return nil, err
	}
	scalars := map[string]string{"ProjectV2ItemFieldTextValue": "text", "ProjectV2ItemFieldNumberValue": "number", "ProjectV2ItemFieldDateValue": "date", "ProjectV2ItemFieldSingleSelectValue": "name", "ProjectV2ItemFieldIterationValue": "title"}
	out := contract.Object{}
	seen := map[string]bool{}
	for _, node := range nodes {
		typename, err := contract.Nonempty(node, "__typename")
		if err != nil {
			return nil, err
		}
		scalar, supported := scalars[typename]
		if !supported {
			continue
		}
		field, err := contract.ObjectAt(node, "field")
		if err != nil {
			return nil, err
		}
		name, err := contract.Nonempty(field, "name")
		if err != nil || seen[name] {
			return nil, errors.New("Project field value identity missing or duplicated")
		}
		seen[name] = true
		value, exists := node[scalar]
		if !exists {
			return nil, errors.New("queried Project field scalar is missing")
		}
		if value == nil {
			continue
		}
		if scalar == "number" {
			if _, err := contract.Number(value); err != nil {
				return nil, err
			}
		} else {
			str, ok := value.(string)
			if !ok {
				return nil, errors.New("queried Project field scalar must be a string")
			}
			if str == "" {
				continue
			}
		}
		out[name] = value
	}
	return out, nil
}

func (s Service) Project(ctx context.Context, p native.ProjectScope) (contract.Object, error) {
	metadata, err := s.Reader.ReadProject(ctx, p)
	if err != nil {
		return nil, err
	}
	metadata, err = contract.Clone(metadata)
	if err != nil {
		return nil, err
	}
	id, err := contract.Nonempty(metadata, "id")
	if err != nil {
		return nil, err
	}
	p.ID = id
	metadata["owner_login"] = p.Owner
	metadata["owner_type"] = p.OwnerType
	metadata["host"] = p.Host
	rawFields, err := s.projectCollection(ctx, p, "fields")
	if err != nil {
		return nil, err
	}
	fields, err := NormalizeFields(rawFields)
	if err != nil {
		return nil, err
	}
	rawItems, err := s.projectCollection(ctx, p, "items")
	if err != nil {
		return nil, err
	}
	items := []any{}
	seenIDs := map[string]bool{}
	seenNumbers := map[int64]bool{}
	for _, raw := range rawItems {
		itemID, err := contract.Nonempty(raw, "id")
		if err != nil || seenIDs[itemID] {
			return nil, errors.New("Project item identity missing or duplicated")
		}
		seenIDs[itemID] = true
		values, err := NormalizeFieldValues(raw)
		if err != nil {
			return nil, err
		}
		archived, err := contract.Bool(raw, "isArchived")
		if err != nil {
			return nil, err
		}
		content, exists := raw["content"]
		if !exists {
			return nil, errors.New("Project item nullable content is missing")
		}
		if content == nil {
			continue
		}
		issue, ok := content.(map[string]any)
		if !ok {
			return nil, errors.New("Project item content must be object or null")
		}
		typename, err := contract.Nonempty(issue, "__typename")
		if err != nil {
			return nil, err
		}
		if typename == "DraftIssue" || typename == "PullRequest" {
			continue
		}
		if typename != "Issue" {
			return nil, errors.New("unsupported Project item content type")
		}
		repo, err := contract.ObjectAt(issue, "repository")
		if err != nil {
			return nil, err
		}
		identity, err := contract.ParseRepository(repo)
		if err != nil || identity.Host != s.Repository.Host {
			return nil, errors.New("Project issue repository identity is invalid")
		}
		normalized, err := NormalizeIssue(issue, identity, false)
		if err != nil {
			return nil, err
		}
		assignees, err := names(issue, "assignees", "login")
		if err != nil {
			return nil, err
		}
		if identity != s.Repository {
			continue
		}
		n, _ := contract.PositiveInteger(normalized["number"])
		if seenNumbers[n] {
			return nil, errors.New("Project contains duplicate selected issue memberships")
		}
		seenNumbers[n] = true
		items = append(items, contract.Object{"item_id": itemID, "content_type": typename, "number": n, "title": normalized["title"], "state": normalized["state"], "url": normalized["url"], "repository": identity.FullName(), "labels": normalized["labels"], "assignees": assignees, "milestone": normalized["milestone"], "field_values": values, "archived": archived})
	}
	out := contract.Object{"repo": s.Repository.Object(), "project": metadata, "fields_by_name": fields, "items": items, "generated_at": s.stamp(), "provenance": contract.Object{"source": "github_project_api", "live": true, "complete": true}}
	if data, err := contract.Canonical(out); err != nil || len(data) > contract.MaxJSONBytes {
		return nil, errors.New("Project snapshot exceeds supported complete envelope")
	}
	return out, nil
}

func arrayObjects(pages []any, keys ...string) ([]contract.Object, error) {
	if len(pages) == 0 {
		return nil, errors.New("REST read returned no complete pages")
	}
	out := []contract.Object{}
	for _, page := range pages {
		var entries []any
		switch p := page.(type) {
		case []any:
			entries = p
		case map[string]any:
			for _, key := range keys {
				if value, ok := p[key]; ok {
					entries, ok = value.([]any)
					if !ok {
						return nil, errors.New("REST list property must be an array")
					}
					break
				}
			}
			if entries == nil {
				return nil, errors.New("REST list envelope is missing its declared collection")
			}
		default:
			return nil, errors.New("REST page must be a list or declared collection object")
		}
		for _, entry := range entries {
			obj, ok := entry.(map[string]any)
			if !ok {
				return nil, errors.New("REST collection entries must be objects")
			}
			out = append(out, obj)
		}
	}
	return out, nil
}

func (s Service) RelationshipNumbers(ctx context.Context, number int64, kind string, openOnly bool) ([]any, error) {
	if number < 1 {
		return nil, errors.New("issue number must be positive")
	}
	path := "sub_issues"
	keys := []string{"items"}
	if kind == "blocked-by" {
		path = "dependencies/blocked_by"
		keys = []string{"blocked_by", "dependencies", "items"}
	} else if kind != "children" {
		return nil, errors.New("unsupported relationship collection")
	}
	pages, err := s.Reader.RESTPages(ctx, fmt.Sprintf("repos/%s/issues/%d/%s?per_page=100", s.Repository.FullName(), number, path))
	if err != nil {
		return nil, err
	}
	entries, err := arrayObjects(pages, keys...)
	if err != nil {
		return nil, err
	}
	numbers := map[int64]bool{}
	for _, entry := range entries {
		n, err := contract.PositiveInteger(entry["number"])
		if err != nil {
			return nil, err
		}
		foundURL := false
		for _, key := range []string{"url", "html_url"} {
			if raw, exists := entry[key]; exists && raw != nil {
				if err = issueURL(raw, s.Repository, n, true); err != nil {
					return nil, err
				}
				foundURL = true
			}
		}
		if !foundURL {
			return nil, errors.New("relationship issue requires repository-qualified identity")
		}
		state, err := contract.Nonempty(entry, "state")
		if err != nil || (strings.ToUpper(state) != "OPEN" && strings.ToUpper(state) != "CLOSED") {
			return nil, errors.New("relationship issue state is invalid")
		}
		if openOnly && strings.ToUpper(state) == "CLOSED" {
			continue
		}
		numbers[n] = true
	}
	ns := make([]int64, 0, len(numbers))
	for n := range numbers {
		ns = append(ns, n)
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i] < ns[j] })
	out := make([]any, 0, len(ns))
	for _, n := range ns {
		out = append(out, n)
	}
	return out, nil
}

func (s Service) IssueGraph(ctx context.Context, state string, project contract.Object) (contract.Object, error) {
	includeProjects := project != nil
	issues, repositoryNodeID, err := s.readIssues(ctx, state, includeProjects)
	if err != nil {
		return nil, err
	}
	projectByNumber := map[int64]contract.Object{}
	if project != nil {
		projectRepo, err := contract.ObjectAt(project, "repo")
		if err != nil {
			return nil, err
		}
		if err = s.validateRepository(projectRepo); err != nil {
			return nil, err
		}
		prov, err := contract.ObjectAt(project, "provenance")
		if err != nil || prov["live"] != true || prov["complete"] != true {
			return nil, errors.New("issue graph joins require complete live Project evidence")
		}
		items, err := contract.Objects(project, "items")
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			n, err := contract.PositiveInteger(item["number"])
			if err != nil {
				return nil, err
			}
			projectByNumber[n] = item
		}
	}
	parents := map[int64]int64{}
	blocking := map[int64][]any{}
	for _, issue := range issues {
		n, _ := contract.PositiveInteger(issue["number"])
		children, err := s.RelationshipNumbers(ctx, n, "children", false)
		if err != nil {
			return nil, err
		}
		blockers, err := s.RelationshipNumbers(ctx, n, "blocked-by", strings.ToLower(state) == "open")
		if err != nil {
			return nil, err
		}
		issue["child_numbers"] = children
		issue["blocked_by_numbers"] = blockers
		for _, raw := range children {
			child, _ := contract.PositiveInteger(raw)
			if _, exists := parents[child]; !exists {
				parents[child] = n
			}
		}
		for _, raw := range blockers {
			blocker, _ := contract.PositiveInteger(raw)
			blocking[blocker] = append(blocking[blocker], n)
		}
	}
	items := []any{}
	for _, issue := range issues {
		n, _ := contract.PositiveInteger(issue["number"])
		var parent any
		if p, ok := parents[n]; ok {
			parent = p
		}
		issue["parent_number"] = parent
		refs := blocking[n]
		if refs == nil {
			refs = []any{}
		}
		issue["blocking_numbers"] = refs
		item, present := projectByNumber[n]
		issue["in_project"] = present
		issue["field_values"] = contract.Object{}
		issue["project_item"] = nil
		if present {
			issue["field_values"] = item["field_values"]
			issue["project_item"] = contract.Object{"item_id": item["item_id"], "content_type": item["content_type"], "field_values": item["field_values"], "archived": item["archived"]}
		}
		issue["prefix"] = nil
		title := fmt.Sprint(issue["title"])
		if i := strings.Index(title, ":"); i > 0 {
			issue["prefix"] = strings.TrimSpace(title[:i])
		}
		items = append(items, issue)
	}
	out := contract.Object{"repo": s.Repository.Object(), "issues": items, "issue_count": len(items), "generated_at": s.stamp(), "provenance": contract.Object{"issues_source": "github_api", "issues_live": true, "issue_state": strings.ToLower(state), "repository_node_id": repositoryNodeID, "project_joins_source": "not_requested", "project_joins_live": false, "live": true, "complete": true}}
	if project != nil {
		out["project"] = project["project"]
		prov := out["provenance"].(map[string]any)
		prov["project_joins_source"] = "github_project_api"
		prov["project_joins_live"] = true
	}
	if data, err := contract.Canonical(out); err != nil || len(data) > contract.MaxJSONBytes {
		return nil, errors.New("issue graph exceeds supported complete envelope")
	}
	return out, nil
}
