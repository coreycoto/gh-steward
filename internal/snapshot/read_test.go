package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

func repo() contract.Repository {
	return contract.Repository{Host: "github.com", Owner: "example", Name: "widgets", URL: "https://github.com/example/widgets"}
}
func connection(nodes ...any) contract.Object {
	return contract.Object{"nodes": nodes, "pageInfo": contract.Object{"hasNextPage": false}}
}
func issue(n int64) contract.Object {
	return contract.Object{"id": fmt.Sprintf("I_%d", n), "number": n, "title": fmt.Sprintf("Task: item %d", n), "body": "", "state": "OPEN", "url": fmt.Sprintf("https://github.com/example/widgets/issues/%d", n), "labels": connection(), "milestone": nil, "projectItems": connection()}
}

type fakeReader struct {
	issuePages    []contract.Object
	project       contract.Object
	fields, items []contract.Object
	relations     map[string][]any
	calls         int
	err           error
}

func (f *fakeReader) ReadRepository(context.Context) (contract.Object, error) {
	return repo().Object(), f.err
}
func (f *fakeReader) ReadIssuesPage(_ context.Context, _ string, cursor *string, projects bool) (contract.Object, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.issuePages) == 0 {
		return nil, errors.New("no remaining fixture pages")
	}
	p := f.issuePages[0]
	f.issuePages = f.issuePages[1:]
	return p, nil
}
func (f *fakeReader) ReadProject(context.Context, native.ProjectScope) (contract.Object, error) {
	return f.project, f.err
}
func (f *fakeReader) ReadProjectPage(_ context.Context, _ native.ProjectScope, section string, _ *string) (contract.Object, error) {
	r := contract.Object{}
	switch section {
	case "fields":
		r[section] = connection(objects(f.fields)...)
	case "items":
		r[section] = connection(objects(f.items)...)
	}
	return r, f.err
}
func (f *fakeReader) RESTPages(_ context.Context, path string) ([]any, error) {
	return f.relations[path], f.err
}
func objects(o []contract.Object) []any {
	a := []any{}
	for _, v := range o {
		a = append(a, v)
	}
	return a
}
func service(f *fakeReader) Service {
	return Service{Reader: f, Repository: repo(), Now: func() time.Time { return time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC) }}
}
func issuePage(entries ...any) contract.Object {
	r := repo().Object()
	r["id"] = "R_widgets"
	r["issues"] = connection(entries...)
	return r
}

func TestIssuesReadAllPagesAndRejectsDuplicateOrLoopingIdentity(t *testing.T) {
	p := issuePage(issue(101))
	p["issues"].(map[string]any)["pageInfo"] = contract.Object{"hasNextPage": true, "endCursor": "next"}
	f := &fakeReader{issuePages: []contract.Object{p, issuePage(issue(1))}}
	got, err := service(f).Issues(context.Background(), "open", false)
	if err != nil || len(got) != 2 || got[0]["number"] != int64(1) || f.calls != 2 {
		t.Fatal("outer pagination lost issues", got, err)
	}
	f.issuePages = []contract.Object{p, p}
	if _, err := service(f).Issues(context.Background(), "open", false); err == nil {
		t.Fatal("cursor cycle accepted")
	}
	f.issuePages = []contract.Object{issuePage(issue(1), issue(1))}
	if _, err := service(f).Issues(context.Background(), "open", false); err == nil {
		t.Fatal("duplicate identity accepted")
	}
}

func TestIssueMetadataNullableScalarsAndExactHundredLabels(t *testing.T) {
	i := issue(1)
	labels := []any{}
	for n := 0; n < 100; n++ {
		labels = append(labels, contract.Object{"name": fmt.Sprintf("label-%d", n)})
	}
	i["labels"] = connection(labels...)
	got, err := NormalizeIssue(i, repo(), false)
	if err != nil || got["body"] != "" || got["milestone"] != nil || len(got["labels"].([]any)) != 100 {
		t.Fatal("valid nullable/exact boundary rejected", err)
	}
	i["labels"].(map[string]any)["pageInfo"] = contract.Object{"hasNextPage": true, "endCursor": "missing"}
	if _, err := NormalizeIssue(i, repo(), false); err == nil {
		t.Fatal("incomplete nested labels accepted")
	}
	for _, key := range []string{"id", "number", "title", "body", "state", "url", "labels", "milestone"} {
		bad := issue(1)
		delete(bad, key)
		if _, err := NormalizeIssue(bad, repo(), false); err == nil {
			t.Fatal("missing issue field accepted", key)
		}
	}
	for _, n := range []any{true, "1", json.Number("1.0"), json.Number("1e0"), -1} {
		bad := issue(1)
		bad["number"] = n
		if _, err := NormalizeIssue(bad, repo(), false); err == nil {
			t.Fatal("bad number accepted", n)
		}
	}
	bad := issue(1)
	bad["url"] = "https://github.com/other/widgets/issues/1"
	if _, err := NormalizeIssue(bad, repo(), false); err == nil {
		t.Fatal("foreign issue accepted")
	}
}

func TestMembershipAccessIsOnlyRequiredWhenRequested(t *testing.T) {
	i := issue(1)
	delete(i, "projectItems")
	if _, err := NormalizeIssue(i, repo(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeIssue(i, repo(), true); err == nil {
		t.Fatal("inaccessible requested membership became empty")
	}
	i["projectItems"] = connection(contract.Object{"id": "M_1", "project": contract.Object{"id": "P_1", "number": 1, "title": "Queue", "url": "https://github.com/users/example/projects/1"}})
	if _, err := NormalizeIssue(i, repo(), true); err != nil {
		t.Fatal(err)
	}
}

func fieldValue(name, kind, key string, value any) contract.Object {
	return contract.Object{"__typename": kind, key: value, "field": contract.Object{"id": "F_" + name, "name": name}}
}
func TestFieldValueUnionsPreserveZeroAndFractionsAndRejectMalformedSuccess(t *testing.T) {
	v := contract.Object{"fieldValues": connection(fieldValue("zero", "ProjectV2ItemFieldNumberValue", "number", json.Number("0")), fieldValue("fraction", "ProjectV2ItemFieldNumberValue", "number", json.Number("2.5")), fieldValue("empty", "ProjectV2ItemFieldTextValue", "text", ""), fieldValue("unset", "ProjectV2ItemFieldTextValue", "text", nil), contract.Object{"__typename": "ProjectV2ItemFieldLabelValue"})}
	got, err := NormalizeFieldValues(v)
	if err != nil || len(got) != 2 || got["zero"] != json.Number("0") || got["fraction"] != json.Number("2.5") {
		t.Fatal("union values changed", got, err)
	}
	for _, bad := range []contract.Object{fieldValue("rank", "ProjectV2ItemFieldNumberValue", "number", true), fieldValue("rank", "ProjectV2ItemFieldNumberValue", "number", "2"), fieldValue("rank", "ProjectV2ItemFieldTextValue", "text", 17), {"__typename": "ProjectV2ItemFieldTextValue", "field": contract.Object{"name": "rank"}}, {"__typename": nil}} {
		if _, err := NormalizeFieldValues(contract.Object{"fieldValues": connection(bad)}); err == nil {
			t.Fatal("malformed field union accepted", bad)
		}
	}
}

func TestProjectNormalizesSelectedRepoAndValidatesDiscardedItems(t *testing.T) {
	p := contract.Object{"id": "P_1", "number": 1, "title": "Queue", "url": "https://github.com/users/example/projects/1", "closed": false, "public": false}
	i := issue(7)
	i["__typename"] = "Issue"
	i["repository"] = repo().Object()
	i["assignees"] = connection()
	f := &fakeReader{project: p, fields: []contract.Object{{"id": "F_1", "name": "Rank", "dataType": "NUMBER"}}, items: []contract.Object{{"id": "M_7", "isArchived": false, "fieldValues": connection(fieldValue("Rank", "ProjectV2ItemFieldNumberValue", "number", json.Number("0"))), "content": i}, {"id": "M_deleted", "isArchived": false, "fieldValues": connection(), "content": nil}, {"id": "M_draft", "isArchived": false, "fieldValues": connection(), "content": contract.Object{"__typename": "DraftIssue"}}}}
	got, err := service(f).Project(context.Background(), native.ProjectScope{Host: "github.com", Owner: "example", OwnerType: "User", Number: 1})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := contract.Objects(got, "items")
	number, _ := contract.PositiveInteger(items[0]["number"])
	if len(items) != 1 || number != 7 {
		t.Fatal("nullable/non-issue normalization changed", items)
	}
	f.items[1]["fieldValues"] = contract.Object{"nodes": []any{}, "pageInfo": contract.Object{"hasNextPage": true, "endCursor": "more"}}
	if _, err := service(f).Project(context.Background(), native.ProjectScope{}); err == nil {
		t.Fatal("malformed discarded item data became complete")
	}
}

func TestRelationshipsValidateSameNumberOtherRepoAndAllPages(t *testing.T) {
	path := "repos/example/widgets/issues/1/dependencies/blocked_by?per_page=100"
	f := &fakeReader{relations: map[string][]any{path: {[]any{contract.Object{"number": 2, "state": "open", "url": "https://api.github.com/repos/example/widgets/issues/2"}}, []any{contract.Object{"number": 3, "state": "closed", "html_url": "https://github.com/example/widgets/issues/3"}}}}}
	got, err := service(f).RelationshipNumbers(context.Background(), 1, "blocked-by", true)
	if err != nil || len(got) != 1 || got[0] != int64(2) {
		t.Fatal(got, err)
	}
	f.relations[path] = []any{[]any{contract.Object{"number": 2, "state": "open", "url": "https://api.github.com/repos/other/widgets/issues/2"}}}
	if _, err := service(f).RelationshipNumbers(context.Background(), 1, "blocked-by", false); err == nil {
		t.Fatal("cross-repository relationship accepted")
	}
	f.err = errors.New("auth denied")
	if _, err := service(f).RelationshipNumbers(context.Background(), 1, "blocked-by", false); err == nil {
		t.Fatal("failed live read became seed")
	}
}

func TestQueueUsesExplicitConsumerVocabularyAndActionableChildren(t *testing.T) {
	policy, err := ParseQueuePolicy(contract.Object{"status_field": "Progress", "priority_field": "Band", "order_field": "Rank", "done_statuses": []any{"Complete"}, "priorities": contract.Object{"Immediate": "Now", "Upcoming": "Next", "Someday": "Later"}, "excluded_prefixes": []any{"Portfolio"}})
	if err != nil {
		t.Fatal(err)
	}
	rows := []any{}
	for n := int64(1); n <= 4; n++ {
		i := issue(n)
		i["in_project"] = true
		i["field_values"] = contract.Object{"Progress": "Working", "Band": "Immediate", "Rank": n * 10}
		i["child_numbers"] = []any{}
		i["blocked_by_numbers"] = []any{}
		rows = append(rows, i)
	}
	rows[0].(map[string]any)["child_numbers"] = []any{int64(2)}
	rows[2].(map[string]any)["title"] = "Portfolio: aggregate"
	rows[3].(map[string]any)["field_values"].(map[string]any)["Progress"] = "Complete"
	graph := contract.Object{"repo": repo().Object(), "issues": rows, "project": contract.Object{"id": "P_1"}, "provenance": contract.Object{"live": true}}
	got, err := service(&fakeReader{}).Queue(graph, policy)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := contract.Objects(got, "items")
	if len(items) != 3 || got["excluded_item_count"] != 1 || items[0]["is_actionable"] != false || items[1]["is_actionable"] != true || items[2]["is_done"] != true {
		t.Fatal("consumer policy/actionability changed", got)
	}
	if items[0]["field_values"].(map[string]any)["Priority"] != "Now" {
		t.Fatal("semantic priority mapping lost")
	}
	if _, err := service(&fakeReader{}).Queue(graph, QueuePolicy{}); err == nil {
		t.Fatal("policy was inferred")
	}
}

func TestMalformedJoinEnvelopeFailsWithoutPanic(t *testing.T) {
	f := &fakeReader{issuePages: []contract.Object{issuePage()}}
	if _, err := service(f).IssueGraph(context.Background(), "open", contract.Object{"repo": "bad"}); err == nil {
		t.Fatal("bad join accepted")
	}
	if _, err := contract.Decode(strings.NewReader(`{"v":NaN}`)); err == nil {
		t.Fatal("nonfinite JSON accepted")
	}
}
