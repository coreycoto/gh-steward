package snapshot

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

type projectOwnerFixture struct {
	fakeReader
	pages []contract.Object
}

func (f *projectOwnerFixture) ReadOwnerProjectsPage(_ context.Context, _ native.ProjectOwnerScope, _ *string) (contract.Object, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.pages) == 0 {
		return nil, errors.New("remaining Projects page unavailable")
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

func ownerProject(n int64) contract.Object {
	return contract.Object{"id": fmt.Sprintf("P_%d", n), "number": n, "title": "Backlog", "url": fmt.Sprintf("https://github.com/users/planner/projects/%d", n), "closed": false, "public": false}
}
func ownerProjectsPage(projects ...any) contract.Object {
	return contract.Object{"id": "U_planner", "login": "planner", "__typename": "User", "projectsV2": connection(projects...)}
}
func ownerScope() native.ProjectOwnerScope {
	return native.ProjectOwnerScope{Host: "github.com", Owner: "planner", OwnerType: "User"}
}
func ownerService(f *projectOwnerFixture) Service { return Service{Reader: f, Repository: repo()} }

func TestOwnerProjectDiscoveryRetainsCompleteAccessiblePagesAndDistinctTitles(t *testing.T) {
	page := ownerProjectsPage(ownerProject(14))
	page["projectsV2"].(map[string]any)["pageInfo"] = contract.Object{"hasNextPage": true, "endCursor": "next"}
	f := &projectOwnerFixture{pages: []contract.Object{page, ownerProjectsPage(ownerProject(2))}}
	got, err := ownerService(f).OwnerProjects(context.Background(), ownerScope())
	if err != nil || f.calls != 2 {
		t.Fatal(got, err)
	}
	rows, _ := contract.Array(got, "projects")
	first, _ := contract.ObjectAt(contract.Object{"first": rows[0]}, "first")
	proof, _ := contract.ObjectAt(got, "provenance")
	if len(rows) != 2 || first["number"] != int64(2) || proof["complete"] != true || proof["visibility"] != "accessible" || proof["owner_node_id"] != "U_planner" {
		t.Fatal("discovery lost scope, accessible completeness or deterministic order", got)
	}
	// Equal titles are preserved so the consumer can reject ambiguity explicitly.
	f.pages = []contract.Object{ownerProjectsPage()}
	got, err = ownerService(f).OwnerProjects(context.Background(), ownerScope())
	if err != nil {
		t.Fatal(err)
	}
	rows, _ = contract.Array(got, "projects")
	if len(rows) != 0 {
		t.Fatal("empty complete owner discovery was fabricated", got)
	}
}

func TestOwnerProjectDiscoveryRejectsIncompletePaginationAndIdentityDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(contract.Object, contract.Object)
	}{
		{"duplicate", func(a, b contract.Object) { b["projectsV2"] = a["projectsV2"] }},
		{"owner ID drift", func(a, b contract.Object) { b["id"] = "U_recreated" }},
		{"owner login drift", func(a, b contract.Object) { b["login"] = "other" }},
		{"owner type drift", func(a, b contract.Object) { b["__typename"] = "Organization" }},
		{"cursor loop", func(a, b contract.Object) {
			b["projectsV2"].(map[string]any)["pageInfo"] = contract.Object{"hasNextPage": true, "endCursor": "next"}
		}},
		{"missing page", func(a, b contract.Object) { b["projectsV2"] = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := ownerProjectsPage(ownerProject(14)), ownerProjectsPage(ownerProject(2))
			a["projectsV2"].(map[string]any)["pageInfo"] = contract.Object{"hasNextPage": true, "endCursor": "next"}
			tc.mutate(a, b)
			f := &projectOwnerFixture{pages: []contract.Object{a, b}}
			if _, err := ownerService(f).OwnerProjects(context.Background(), ownerScope()); err == nil {
				t.Fatal("incomplete or drifting discovery accepted")
			}
		})
	}
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"id", ""}, {"number", float64(2.5)}, {"url", "https://github.com/orgs/planner/projects/14"}, {"title", ""}, {"closed", nil}, {"public", "false"},
	} {
		item := ownerProject(14)
		item[tc.key] = tc.value
		f := &projectOwnerFixture{pages: []contract.Object{ownerProjectsPage(item)}}
		if _, err := ownerService(f).OwnerProjects(context.Background(), ownerScope()); err == nil {
			t.Fatal("malformed Project metadata accepted", tc)
		}
	}
}
