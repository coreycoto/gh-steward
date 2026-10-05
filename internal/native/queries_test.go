package native

import (
	"context"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestRepositoryQueriesBindVariablesAndOmitUnrequestedProjects(t *testing.T) {
	f := &fakeExecutor{result: Result{Stdout: []byte(`{"data":{"repository":{"id":"R_1","nameWithOwner":"example/widgets","url":"https://github.com/example/widgets","issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}`)}}
	tpt := transport(f)
	if _, err := tpt.ReadIssuesPage(context.Background(), "open", nil, false); err != nil {
		t.Fatal(err)
	}
	input, err := contract.Decode(strings.NewReader(string(f.input)))
	if err != nil {
		t.Fatal(err)
	}
	vars, _ := contract.ObjectAt(input, "variables")
	query, _ := contract.String(input, "query")
	if vars["owner"] != "example" || vars["name"] != "widgets" || strings.Contains(query, "projectItems") {
		t.Fatal("query leaked Project scope or failed to bind repository")
	}
	if _, err := tpt.ReadIssuesPage(context.Background(), "open", nil, true); err != nil {
		t.Fatal(err)
	}
	input, _ = contract.Decode(strings.NewReader(string(f.input)))
	query, _ = contract.String(input, "query")
	if !strings.Contains(query, "projectItems") {
		t.Fatal("requested memberships omitted")
	}
	before := f.calls
	if _, err := tpt.ReadIssuesPage(context.Background(), "OPEN] rogue", nil, false); err == nil || f.calls != before {
		t.Fatal("caller-controlled query text was dispatched")
	}
}

func TestRawGraphQLAndForeignRepositoryResultsAreRejected(t *testing.T) {
	f := &fakeExecutor{}
	tpt := transport(f)
	for _, args := range [][]string{{"api", "graphql", "-f", "query=query{repository(owner:\"other\",name:\"widgets\"){id}}"}, {"api", "graphql", "--input", "-"}} {
		if _, err := tpt.run(context.Background(), args, nil, false); err == nil {
			t.Fatal("raw GraphQL accepted")
		}
	}
	if f.calls != 0 {
		t.Fatal("raw request reached transport")
	}
	f.result.Stdout = []byte(`{"data":{"repository":{"id":"R_2","nameWithOwner":"other/widgets","url":"https://github.com/other/widgets"}}}`)
	if _, err := tpt.ReadRepository(context.Background()); err == nil {
		t.Fatal("foreign repository result accepted")
	}
	f.result.Stdout = []byte(`{"data":{"repository":{"id":"R_1","nameWithOwner":"example/widgets","url":"https://github.com/example/widgets","issue":{"number":17,"url":"https://github.com/other/widgets/issues/17"}}}}`)
	if _, err := tpt.ReadIssue(context.Background(), 17, false); err == nil {
		t.Fatal("foreign issue identity accepted")
	}
}

func TestRepositoryReadNormalizesTypedGraphQLOwnerWithoutWeakeningIdentity(t *testing.T) {
	valid := `{"data":{"repository":{"id":"R_1","name":"widgets","nameWithOwner":"example/widgets","url":"https://github.com/example/widgets","owner":{"login":"example","__typename":"User"},"defaultBranchRef":{"name":"main"}}}}`
	f := &fakeExecutor{result: Result{Stdout: []byte(valid)}}
	tpt := transport(f)
	r, err := tpt.ReadRepository(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := contract.ObjectAt(r, "owner")
	if err != nil || owner["login"] != "example" {
		t.Fatal("raw native owner envelope was not preserved", r, err)
	}
	for _, mutation := range [][2]string{
		{`"login":"example"`, `"login":"other"`},
		{`"login":"example"`, `"login":false`},
		{`"__typename":"User"`, `"__typename":"Bot"`},
		{`"__typename":"User"`, `"__typename":null`},
		{`"name":"widgets"`, `"name":false`},
	} {
		f.result.Stdout = []byte(strings.Replace(valid, mutation[0], mutation[1], 1))
		if _, err := tpt.ReadRepository(context.Background()); err == nil {
			t.Fatal("malformed or conflicting native identity accepted", mutation)
		}
	}
}

func TestProjectScopeIsExplicitAndAllResultIdentitiesAreChecked(t *testing.T) {
	f := &fakeExecutor{result: Result{Stdout: []byte(`{"data":{"repositoryOwner":{"__typename":"User","login":"planner","projectV2":{"id":"P_14","number":14,"title":"Backlog","url":"https://github.com/users/planner/projects/14","closed":false,"public":false}}}}`)}}
	tpt := transport(f)
	p := ProjectScope{Host: "github.com", Owner: "planner", OwnerType: "User", Number: 14, ID: "P_14"}
	if _, err := tpt.ReadProject(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	input, _ := contract.Decode(strings.NewReader(string(f.input)))
	vars, _ := contract.ObjectAt(input, "variables")
	if vars["owner"] != "planner" {
		t.Fatal("broader Project owner was inferred from repository")
	}
	before := f.calls
	for _, bad := range []ProjectScope{{Host: "else.example", Owner: "planner", OwnerType: "User", Number: 14}, {Host: "github.com", Owner: "@me", OwnerType: "User", Number: 14}, {Host: "github.com", Owner: "planner", OwnerType: "Repository", Number: 14}, {Host: "github.com", Owner: "planner", OwnerType: "User", Number: 0}} {
		if _, err := tpt.ReadProject(context.Background(), bad); err == nil {
			t.Fatal("invalid scope accepted", bad)
		}
	}
	if f.calls != before {
		t.Fatal("invalid Project scope reached native execution")
	}
	for _, pair := range [][2]string{{`"login":"planner"`, `"login":"other"`}, {`"id":"P_14"`, `"id":"P_other"`}, {`"number":14`, `"number":15`}, {`users/planner/projects/14`, `orgs/planner/projects/14`}, {`"public":false`, `"public":null`}} {
		original := f.result.Stdout
		f.result.Stdout = []byte(strings.Replace(string(original), pair[0], pair[1], 1))
		if _, err := tpt.ReadProject(context.Background(), p); err == nil {
			t.Fatal("mismatched Project accepted", pair)
		}
		f.result.Stdout = original
	}
}

func TestProjectPageQueriesSeparateAdjacentSelections(t *testing.T) {
	const result = `{"data":{"repositoryOwner":{"__typename":"User","login":"planner","projectV2":{"id":"P_14","number":14,"title":"Backlog","url":"https://github.com/users/planner/projects/14","closed":false,"public":false}}}}`
	cursor := "after-first-page"
	tests := []struct {
		name             string
		section          string
		selection        string
		pagination       string
		additionalChecks []string
	}{
		{
			name:       "fields",
			section:    "fields",
			selection:  "public fields(first:100,after:$cursor)",
			pagination: "pageInfo{hasNextPage endCursor}",
			additionalChecks: []string{
				"... on ProjectV2FieldCommon{id name dataType}",
				"... on ProjectV2MultiSelectField{options:multiSelectOptions{id name}}",
			},
		},
		{
			name:       "items",
			section:    "items",
			selection:  "public items(first:100,after:$cursor,archivedStates:[ARCHIVED,NOT_ARCHIVED])",
			pagination: "pageInfo{hasNextPage endCursor}",
			additionalChecks: []string{
				"fieldValues(first:100)",
				"content{__typename ... on Issue",
				"pageInfo{hasNextPage}",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := &fakeExecutor{result: Result{Stdout: []byte(result)}}
			tpt := transport(f)
			p := ProjectScope{Host: "github.com", Owner: "planner", OwnerType: "User", Number: 14, ID: "P_14"}
			if _, err := tpt.ReadProjectPage(context.Background(), p, test.section, &cursor); err != nil {
				t.Fatal(err)
			}

			input, err := contract.Decode(strings.NewReader(string(f.input)))
			if err != nil {
				t.Fatal(err)
			}
			variables, err := contract.ObjectAt(input, "variables")
			if err != nil {
				t.Fatal(err)
			}
			query, err := contract.String(input, "query")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(query, test.selection) {
				t.Fatalf("selection boundary missing in generated query: %s", query)
			}
			if strings.Contains(query, "publicfields") || strings.Contains(query, "publicitems") {
				t.Fatalf("adjacent Project selections were concatenated: %s", query)
			}
			if variables["owner"] != "planner" || variables["cursor"] != cursor {
				t.Fatalf("query did not bind the explicit owner and cursor: %#v", variables)
			}
			number, err := contract.PositiveInteger(variables["number"])
			if err != nil || number != p.Number {
				t.Fatalf("query did not bind the selected Project number: %#v (%v)", variables["number"], err)
			}
			if !strings.Contains(query, "... on ProjectV2Owner{projectV2(number:$number){") {
				t.Fatalf("query lost the typed explicit Project owner selection: %s", query)
			}
			if !strings.Contains(query, test.pagination) {
				t.Fatalf("query lost the complete pagination metadata: %s", query)
			}
			for _, fragment := range test.additionalChecks {
				if !strings.Contains(query, fragment) {
					t.Fatalf("query omitted expected Project field or item selection %q: %s", fragment, query)
				}
			}
		})
	}
}
