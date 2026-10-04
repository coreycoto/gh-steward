package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

type projectOwnerReader interface {
	ReadOwnerProjectsPage(context.Context, native.ProjectOwnerScope, *string) (contract.Object, error)
}

// OwnerProjects preserves accessible-collection completeness and owner identity
// across pages. It does not claim visibility into inaccessible private Projects.
func (s Service) OwnerProjects(ctx context.Context, scope native.ProjectOwnerScope) (contract.Object, error) {
	if err := scope.Validate(s.Repository); err != nil {
		return nil, err
	}
	reader, ok := s.Reader.(projectOwnerReader)
	if !ok {
		return nil, errors.New("native Project discovery is unavailable")
	}
	projects := []contract.Object{}
	seenCursors, seenIDs, seenNumbers := map[string]bool{}, map[string]bool{}, map[int64]bool{}
	var cursor *string
	ownerID := ""
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber >= 1000 {
			return nil, errors.New("Project discovery exceeded its complete pagination budget")
		}
		page, err := reader.ReadOwnerProjectsPage(ctx, scope, cursor)
		if err != nil {
			return nil, err
		}
		id, err := contract.Nonempty(page, "id")
		login, loginErr := contract.Nonempty(page, "login")
		if err != nil || loginErr != nil || !strings.EqualFold(login, scope.Owner) || page["__typename"] != scope.OwnerType || (ownerID != "" && ownerID != id) {
			return nil, errors.New("Project owner identity changed or differs from scope")
		}
		ownerID = id
		nodes, _, err := native.Connection(page, "projectsV2")
		if err != nil {
			return nil, err
		}
		for _, project := range nodes {
			if len(project) != 6 {
				return nil, errors.New("Project discovery metadata is incomplete or unsupported")
			}
			number, err := contract.PositiveInteger(project["number"])
			id, idErr := contract.Nonempty(project, "id")
			if err != nil || idErr != nil || seenIDs[id] || seenNumbers[number] {
				return nil, errors.New("Project discovery returned an invalid or duplicate identity")
			}
			base := "users"
			if scope.OwnerType == "Organization" {
				base = "orgs"
			}
			expectedURL := fmt.Sprintf("https://%s/%s/%s/projects/%d", strings.ToLower(scope.Host), base, strings.ToLower(scope.Owner), number)
			if value, err := contract.String(project, "url"); err != nil || !strings.EqualFold(value, expectedURL) {
				return nil, errors.New("Project discovery URL differs from its owner and number")
			}
			if _, err = contract.Nonempty(project, "title"); err != nil {
				return nil, err
			}
			for _, key := range []string{"closed", "public"} {
				if _, err = contract.Bool(project, key); err != nil {
					return nil, err
				}
			}
			seenIDs[id], seenNumbers[number] = true, true
		}
		more, err := collectionPage(&projects, page, "projectsV2", &cursor, seenCursors)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
	}
	sort.Slice(projects, func(i, j int) bool {
		a, _ := contract.PositiveInteger(projects[i]["number"])
		b, _ := contract.PositiveInteger(projects[j]["number"])
		return a < b
	})
	rows := make([]any, len(projects))
	for i, project := range projects {
		rows[i] = project
	}
	return contract.Object{"owner": contract.Object{"host": scope.Host, "login": scope.Owner, "owner_type": scope.OwnerType, "id": ownerID}, "projects": rows, "generated_at": s.stamp(), "provenance": contract.Object{"live": true, "complete": true, "source": "github_api", "visibility": "accessible", "owner_node_id": ownerID, "owner_login": scope.Owner, "owner_type": scope.OwnerType}}, nil
}
