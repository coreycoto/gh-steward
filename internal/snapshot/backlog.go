package snapshot

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

func NormalizeMilestone(raw contract.Object, repo contract.Repository) (contract.Object, error) {
	number, err := contract.PositiveInteger(raw["number"])
	if err != nil {
		return nil, err
	}
	id, err := contract.PositiveInteger(raw["id"])
	if err != nil {
		return nil, err
	}
	nodeID, err := contract.Nonempty(raw, "node_id")
	if err != nil {
		return nil, err
	}
	validator := &native.Transport{Repository: repo}
	if err := validator.ValidateMilestoneURL(raw["html_url"], number); err != nil {
		return nil, err
	}
	title, err := contract.Nonempty(raw, "title")
	if err != nil {
		return nil, err
	}
	description, present := raw["description"]
	if !present {
		return nil, errors.New("milestone nullable description missing")
	}
	if description == nil {
		description = ""
	}
	if _, ok := description.(string); !ok {
		return nil, errors.New("milestone description must be string or null")
	}
	state, err := contract.Nonempty(raw, "state")
	if err != nil || (state != "open" && state != "closed") {
		return nil, errors.New("milestone state invalid")
	}
	due, present := raw["due_on"]
	if !present {
		return nil, errors.New("milestone nullable due date missing")
	}
	if due != nil {
		text, ok := due.(string)
		if !ok {
			return nil, errors.New("milestone due date invalid")
		}
		stamp, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return nil, errors.New("milestone due date invalid")
		}
		due = stamp.UTC().Format(time.RFC3339Nano)
	}
	return contract.Object{"id": id, "node_id": nodeID, "number": number, "title": title, "description": description, "state": state, "due_on": due, "url": raw["html_url"]}, nil
}

var labelColor = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)

func normalizeLabel(raw contract.Object, repo contract.Repository) (contract.Object, error) {
	id, err := contract.PositiveInteger(raw["id"])
	if err != nil {
		return nil, err
	}
	nodeID, err := contract.Nonempty(raw, "node_id")
	if err != nil {
		return nil, err
	}
	name, err := contract.Nonempty(raw, "name")
	if err != nil {
		return nil, err
	}
	color, err := contract.Nonempty(raw, "color")
	if err != nil || !labelColor.MatchString(color) {
		return nil, errors.New("label color invalid")
	}
	description, present := raw["description"]
	if !present {
		return nil, errors.New("label nullable description missing")
	}
	if description == nil {
		description = ""
	}
	if _, ok := description.(string); !ok {
		return nil, errors.New("label description invalid")
	}
	identity, err := contract.Nonempty(raw, "url")
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(identity)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return nil, errors.New("label identity must be a qualified API URL")
	}
	host, prefix := repo.Host, "/repos/"+repo.FullName()
	if host == "github.com" {
		host = "api.github.com"
	} else {
		prefix = "/api/v3" + prefix
	}
	parts := strings.SplitN(u.Path, "/labels/", 2)
	if !strings.EqualFold(u.Hostname(), host) || len(parts) != 2 || !strings.EqualFold(parts[0], prefix) || parts[1] != name {
		return nil, errors.New("label identity targets another repository or name")
	}
	return contract.Object{"id": id, "node_id": nodeID, "name": name, "color": strings.ToLower(color), "description": description}, nil
}

func (s Service) readRepositoryNode(ctx context.Context) (string, error) {
	identity, err := s.Reader.ReadRepository(ctx)
	if err != nil {
		return "", err
	}
	if err := s.validateRepository(identity); err != nil {
		return "", err
	}
	return contract.Nonempty(identity, "id")
}

// BacklogInventory captures the actual sources used by a composed review or
// quarter apply. Every REST list is paginated. Optional comment/Project facets
// are explicit; an inaccessible or incomplete source is never an empty list.
func (s Service) BacklogInventory(ctx context.Context, projects []native.ProjectScope, markers []string, includeRelationships bool) (contract.Object, error) {
	return s.captureBacklog(ctx, projects, markers, includeRelationships, false)
}

// BacklogInventoryCaseInsensitiveComments is a read-only inventory mode for
// caller policies that match prose markers without case sensitivity. Reviewed
// mutation workflows retain the exact literal marker contract above.
func (s Service) BacklogInventoryCaseInsensitiveComments(ctx context.Context, projects []native.ProjectScope, markers []string, includeRelationships bool) (contract.Object, error) {
	return s.captureBacklog(ctx, projects, markers, includeRelationships, true)
}

func (s Service) captureBacklog(ctx context.Context, projects []native.ProjectScope, markers []string, includeRelationships, caseInsensitive bool) (contract.Object, error) {
	if s.Reader == nil {
		return nil, errors.New("backlog inventory requires a reader")
	}
	nodeID, err := s.readRepositoryNode(ctx)
	if err != nil {
		return nil, err
	}
	markerSet := map[string]bool{}
	for _, marker := range markers {
		key := marker
		if caseInsensitive {
			key = strings.ToLower(marker)
		}
		if strings.TrimSpace(marker) == "" || markerSet[key] {
			return nil, errors.New("comment markers must be explicit and unique")
		}
		markerSet[key] = true
	}
	markers = append([]string{}, markers...)
	sort.Strings(markers)
	var graph contract.Object
	if includeRelationships {
		graph, err = s.IssueGraph(ctx, "all", nil)
	} else {
		graph, err = s.IssueInventory(ctx, "all")
	}
	if err != nil {
		return nil, err
	}
	provenance, err := contract.ObjectAt(graph, "provenance")
	if err != nil || provenance["repository_node_id"] != nodeID {
		return nil, errors.New("repository identity changed during backlog capture")
	}
	delete(graph, "generated_at")
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	issueNumbers := map[int64]bool{}
	for _, issue := range issues {
		n, _ := contract.PositiveInteger(issue["number"])
		issueNumbers[n] = true
	}
	prefix := "repos/" + s.Repository.FullName()
	milestonePages, err := s.Reader.RESTPages(ctx, prefix+"/milestones?state=all&per_page=100")
	if err != nil {
		return nil, err
	}
	rawMilestones, err := arrayObjects(milestonePages)
	if err != nil {
		return nil, err
	}
	milestones := []any{}
	seenMilestones, seenMilestoneIDs := map[int64]bool{}, map[string]bool{}
	for _, raw := range rawMilestones {
		milestone, err := NormalizeMilestone(raw, s.Repository)
		if err != nil {
			return nil, err
		}
		n, _ := contract.PositiveInteger(milestone["number"])
		id, _ := contract.Nonempty(milestone, "node_id")
		if seenMilestones[n] || seenMilestoneIDs[id] {
			return nil, errors.New("milestone inventory contains duplicate identity")
		}
		seenMilestones[n], seenMilestoneIDs[id] = true, true
		milestones = append(milestones, milestone)
	}
	sort.Slice(milestones, func(i, j int) bool {
		a, _ := contract.PositiveInteger(milestones[i].(map[string]any)["number"])
		b, _ := contract.PositiveInteger(milestones[j].(map[string]any)["number"])
		return a < b
	})
	labelPages, err := s.Reader.RESTPages(ctx, prefix+"/labels?per_page=100")
	if err != nil {
		return nil, err
	}
	rawLabels, err := arrayObjects(labelPages)
	if err != nil {
		return nil, err
	}
	labels := []any{}
	seenLabels, seenLabelIDs := map[string]bool{}, map[string]bool{}
	for _, raw := range rawLabels {
		label, err := normalizeLabel(raw, s.Repository)
		if err != nil {
			return nil, err
		}
		name, _ := contract.Nonempty(label, "name")
		id, _ := contract.Nonempty(label, "node_id")
		if seenLabels[name] || seenLabelIDs[id] {
			return nil, errors.New("label inventory contains duplicate identity")
		}
		seenLabels[name], seenLabelIDs[id] = true, true
		labels = append(labels, label)
	}
	sort.Slice(labels, func(i, j int) bool {
		return labels[i].(map[string]any)["name"].(string) < labels[j].(map[string]any)["name"].(string)
	})
	comments := []any{}
	if len(markers) > 0 {
		pages, err := s.Reader.RESTPages(ctx, prefix+"/issues/comments?per_page=100")
		if err != nil {
			return nil, err
		}
		rawComments, err := arrayObjects(pages)
		if err != nil {
			return nil, err
		}
		seen := map[int64]bool{}
		for _, raw := range rawComments {
			comment, err := commentIdentity(raw, s.Repository)
			if err != nil {
				return nil, err
			}
			id, _ := contract.PositiveInteger(comment["id"])
			if seen[id] {
				return nil, errors.New("comment inventory contains duplicate identity")
			}
			seen[id] = true
			n, _ := contract.PositiveInteger(comment["issue_number"])
			if !issueNumbers[n] {
				continue // Repository issue-comment listing includes PR comments.
			}
			body, _ := contract.String(comment, "body")
			for _, marker := range markers {
				matches := strings.Contains(body, marker)
				if caseInsensitive {
					matches = strings.Contains(strings.ToLower(body), strings.ToLower(marker))
				}
				if matches {
					comments = append(comments, comment)
					break
				}
			}
		}
		sort.Slice(comments, func(i, j int) bool {
			a, _ := contract.PositiveInteger(comments[i].(map[string]any)["id"])
			b, _ := contract.PositiveInteger(comments[j].(map[string]any)["id"])
			return a < b
		})
	}
	projectRows, projectScopes := []any{}, []any{}
	seenProjects := map[string]bool{}
	for _, scope := range projects {
		project, err := s.Project(ctx, scope)
		if err != nil {
			return nil, err
		}
		metadata, _ := contract.ObjectAt(project, "project")
		id, _ := contract.Nonempty(metadata, "id")
		if seenProjects[id] {
			return nil, errors.New("backlog Project scopes contain duplicate identity")
		}
		seenProjects[id] = true
		scope.ID = id
		delete(project, "generated_at")
		projectRows = append(projectRows, project)
		projectScopes = append(projectScopes, scope.Object())
	}
	finalID, err := s.readRepositoryNode(ctx)
	if err != nil {
		return nil, err
	}
	if finalID != nodeID {
		return nil, errors.New("repository immutable identity changed during backlog capture")
	}
	commentsSource, relationshipsSource := "not_requested", "not_requested"
	if len(markers) > 0 {
		commentsSource = "github_api"
	}
	if includeRelationships {
		relationshipsSource = "github_api"
	}
	out := contract.Object{"repo": s.Repository.Object(), "issue_inventory": graph, "milestones": milestones, "comments": comments, "labels": labels, "projects": projectRows, "provenance": contract.Object{"live": true, "complete": true, "issue_state": "all", "repository_node_id": nodeID, "milestones_source": "github_api", "labels_source": "github_api", "comments_source": commentsSource, "relationships_source": relationshipsSource, "comment_markers": markers, "project_scopes": projectScopes}}
	if caseInsensitive {
		out["provenance"].(map[string]any)["comment_case_insensitive"] = true
	}
	encoded, err := contract.Canonical(out)
	if err != nil || len(encoded) > contract.MaxJSONBytes {
		return nil, fmt.Errorf("backlog inventory exceeds the supported complete envelope")
	}
	return out, nil
}
