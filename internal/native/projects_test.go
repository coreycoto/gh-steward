package native

import (
	"context"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestProjectOwnerDiscoveryBindsExplicitScopeAndValidatesOwnerBeforeReturningPages(t *testing.T) {
	valid := `{"data":{"repositoryOwner":{"id":"U_planner","__typename":"User","login":"planner","projectsV2":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}`
	f := &fakeExecutor{result: Result{Stdout: []byte(valid)}}
	tpt := transport(f)
	scope := ProjectOwnerScope{Host: "github.com", Owner: "planner", OwnerType: "User"}
	cursor := "next-page"
	if _, err := tpt.ReadOwnerProjectsPage(context.Background(), scope, &cursor); err != nil {
		t.Fatal(err)
	}
	input, _ := contract.Decode(strings.NewReader(string(f.input)))
	variables, _ := contract.ObjectAt(input, "variables")
	query, _ := contract.String(input, "query")
	if variables["owner"] != "planner" || variables["cursor"] != cursor || !strings.Contains(query, "projectsV2(first:100,after:$cursor)") || strings.Contains(query, "repository(owner:") {
		t.Fatal("Project owner discovery inferred repository ownership or lost pagination", input)
	}
	before := f.calls
	for _, bad := range []ProjectOwnerScope{{Host: "else.example", Owner: "planner", OwnerType: "User"}, {Host: "github.com", Owner: "@me", OwnerType: "User"}, {Host: "github.com", Owner: "planner", OwnerType: "Repository"}} {
		if _, err := tpt.ReadOwnerProjectsPage(context.Background(), bad, nil); err == nil {
			t.Fatal("ambiguous discovery scope accepted", bad)
		}
	}
	if f.calls != before {
		t.Fatal("invalid discovery scope reached provider")
	}
	for _, pair := range [][2]string{{`"id":"U_planner"`, `"id":null`}, {`"login":"planner"`, `"login":"other"`}, {`"__typename":"User"`, `"__typename":"Organization"`}, {`"hasNextPage":false`, `"hasNextPage":null`}, {`"nodes":[]`, `"nodes":null`}} {
		f.result.Stdout = []byte(strings.Replace(valid, pair[0], pair[1], 1))
		if _, err := tpt.ReadOwnerProjectsPage(context.Background(), scope, nil); err == nil {
			t.Fatal("malformed owner discovery became a complete page", pair)
		}
	}
}
