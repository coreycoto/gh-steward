package snapshot

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

type backlogReader struct {
	*fakeReader
	repositoryIDs []string
	paths         []string
}

func (f *backlogReader) ReadRepository(context.Context) (contract.Object, error) {
	identity := repo().Object()
	identity["id"] = "R_widgets"
	if len(f.repositoryIDs) > 0 {
		identity["id"] = f.repositoryIDs[0]
		f.repositoryIDs = f.repositoryIDs[1:]
	}
	return identity, f.err
}
func (f *backlogReader) RESTPages(ctx context.Context, path string) ([]any, error) {
	f.paths = append(f.paths, path)
	return f.fakeReader.RESTPages(ctx, path)
}

func rawMilestone(number int64, state string) contract.Object {
	return contract.Object{"number": number, "id": 10000 + number, "node_id": fmt.Sprintf("M_%d", number), "html_url": fmt.Sprintf("https://github.com/example/widgets/milestone/%d", number), "title": fmt.Sprintf("Quarter %d", number), "description": nil, "state": state, "due_on": nil}
}
func rawLabel(number int64, name string) contract.Object {
	return contract.Object{"id": number, "node_id": fmt.Sprintf("L_%d", number), "name": name, "color": "AAbbCC", "description": nil, "url": "https://api.github.com/repos/example/widgets/labels/" + url.PathEscape(name)}
}
func rawBacklogComment(number, id int64, body string) contract.Object {
	return contract.Object{"id": id, "body": body, "issue_url": fmt.Sprintf("https://api.github.com/repos/example/widgets/issues/%d", number), "html_url": fmt.Sprintf("https://github.com/example/widgets/issues/%d#issuecomment-%d", number, id)}
}
func backlogFixture() *backlogReader {
	closed := issue(2)
	closed["state"] = "CLOSED"
	f := &backlogReader{fakeReader: &fakeReader{issuePages: []contract.Object{issuePage(issue(1), closed)}, relations: map[string][]any{}}}
	for _, number := range []int{1, 2} {
		for _, section := range []string{"sub_issues", "dependencies/blocked_by"} {
			f.relations[fmt.Sprintf("repos/example/widgets/issues/%d/%s?per_page=100", number, section)] = []any{[]any{}}
		}
	}
	f.relations["repos/example/widgets/milestones?state=all&per_page=100"] = []any{[]any{rawMilestone(4, "closed")}, []any{rawMilestone(3, "open")}}
	f.relations["repos/example/widgets/labels?per_page=100"] = []any{[]any{rawLabel(2, "type:bug")}, []any{rawLabel(1, "type:feature")}}
	f.relations["repos/example/widgets/issues/comments?per_page=100"] = []any{[]any{rawBacklogComment(1, 31, "ordinary discussion")}, []any{rawBacklogComment(2, 32, "<!-- quarter-rationale quarter=2026 Q4 -->\nRationale"), rawBacklogComment(99, 33, "<!-- quarter-rationale quarter=2026 Q4 -->\nPR discussion")}}
	return f
}
func backlogService(f *backlogReader) Service { return Service{Reader: f, Repository: repo()} }

func TestBacklogCaptureIncludesClosedIssuesAndAllListPages(t *testing.T) {
	f := backlogFixture()
	got, err := backlogService(f).BacklogInventory(context.Background(), nil, []string{"<!-- quarter-rationale"}, true)
	if err != nil {
		t.Fatal(err)
	}
	graph, _ := contract.ObjectAt(got, "issue_inventory")
	issues, _ := contract.Objects(graph, "issues")
	milestones, _ := contract.Objects(got, "milestones")
	labels, _ := contract.Objects(got, "labels")
	comments, _ := contract.Objects(got, "comments")
	if len(issues) != 2 || issues[1]["state"] != "CLOSED" || len(milestones) != 2 || milestones[0]["number"] != int64(3) || milestones[0]["description"] != "" || len(labels) != 2 || labels[0]["color"] != "aabbcc" || len(comments) != 1 || comments[0]["id"] != int64(32) {
		t.Fatal("source inventory was omitted/coerced", got)
	}
	provenance, _ := contract.ObjectAt(got, "provenance")
	if provenance["repository_node_id"] != "R_widgets" || provenance["comments_source"] != "github_api" || provenance["relationships_source"] != "github_api" || len(f.paths) != 7 {
		t.Fatal("source completeness/scope evidence missing", provenance, f.paths)
	}
	if _, present := graph["generated_at"]; present {
		t.Fatal("ephemeral time entered the captured precondition")
	}
}

func TestBacklogOptionalCommentsAreExplicitAndUnrequestedReadsAreSkipped(t *testing.T) {
	f := backlogFixture()
	got, err := backlogService(f).BacklogInventory(context.Background(), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	provenance, _ := contract.ObjectAt(got, "provenance")
	if provenance["comments_source"] != "not_requested" || provenance["relationships_source"] != "not_requested" || len(f.paths) != 2 {
		t.Fatal("optional source was inferred", provenance, f.paths)
	}
	for _, path := range f.paths {
		if path != "repos/example/widgets/milestones?state=all&per_page=100" && path != "repos/example/widgets/labels?per_page=100" {
			t.Fatal("unrequested comment or relationship source was queried", path)
		}
	}
	graph, err := contract.ObjectAt(got, "issue_inventory")
	if err != nil {
		t.Fatal(err)
	}
	graphProvenance, err := contract.ObjectAt(graph, "provenance")
	if err != nil || graphProvenance["relationships_requested"] != false {
		t.Fatal("metadata inventory implied relationship coverage", graphProvenance, err)
	}
	for _, markers := range [][]string{{""}, {"marker", "marker"}} {
		if _, err := backlogService(backlogFixture()).BacklogInventory(context.Background(), nil, markers, false); err == nil {
			t.Fatal("ambiguous marker scope accepted", markers)
		}
	}
}

func TestBacklogProseMarkerCaseMatchingIsExplicitAndRetainsBodies(t *testing.T) {
	for _, folded := range []bool{false, true} {
		f := backlogFixture()
		f.relations["repos/example/widgets/issues/comments?per_page=100"] = []any{[]any{
			rawBacklogComment(1, 31, "QUARTER commitment rationale"),
			rawBacklogComment(2, 32, "Quarter milestone target: 2026 Q4"),
			rawBacklogComment(1, 33, "quarter rationale"),
		}}
		capture := backlogService(f).BacklogInventory
		if folded {
			capture = backlogService(f).BacklogInventoryCaseInsensitiveComments
		}
		got, err := capture(context.Background(), []native.ProjectScope{}, []string{"quarter"}, false)
		if err != nil {
			t.Fatal(err)
		}
		comments, _ := contract.Objects(got, "comments")
		want := 1
		if folded {
			want = 3
		}
		if len(comments) != want {
			t.Fatal("prose comments were lost", comments, folded)
		}
		if folded && comments[0]["body"] != "QUARTER commitment rationale" {
			t.Fatal("source prose was rewritten")
		}
		provenance, _ := contract.ObjectAt(got, "provenance")
		if folded && provenance["comment_case_insensitive"] != true {
			t.Fatal("matching mode lacks provenance", provenance)
		}
		if !folded {
			if _, present := provenance["comment_case_insensitive"]; present {
				t.Fatal("default reviewed workflow contract changed")
			}
		}
	}
	if _, err := backlogService(backlogFixture()).BacklogInventoryCaseInsensitiveComments(context.Background(), nil, []string{"quarter", "QUARTER"}, false); err == nil {
		t.Fatal("equivalent case-insensitive markers accepted")
	}
}

func TestBacklogCaptureRejectsMissingForeignDuplicateAndDriftedSources(t *testing.T) {
	tests := []struct {
		name string
		edit func(*backlogReader)
	}{
		{"incomplete milestones", func(f *backlogReader) { f.relations["repos/example/widgets/milestones?state=all&per_page=100"] = nil }},
		{"duplicate milestone", func(f *backlogReader) {
			f.relations["repos/example/widgets/milestones?state=all&per_page=100"] = []any{[]any{rawMilestone(3, "open"), rawMilestone(3, "closed")}}
		}},
		{"foreign milestone", func(f *backlogReader) {
			m := rawMilestone(3, "open")
			m["html_url"] = "https://github.com/foreign/widgets/milestone/3"
			f.relations["repos/example/widgets/milestones?state=all&per_page=100"] = []any{[]any{m}}
		}},
		{"malformed milestone", func(f *backlogReader) {
			m := rawMilestone(3, "open")
			m["due_on"] = "2026-02-31"
			f.relations["repos/example/widgets/milestones?state=all&per_page=100"] = []any{[]any{m}}
		}},
		{"duplicate label", func(f *backlogReader) {
			f.relations["repos/example/widgets/labels?per_page=100"] = []any{[]any{rawLabel(1, "bug"), rawLabel(2, "bug")}}
		}},
		{"foreign label", func(f *backlogReader) {
			label := rawLabel(1, "bug")
			label["url"] = "https://api.github.com/repos/foreign/widgets/labels/bug"
			f.relations["repos/example/widgets/labels?per_page=100"] = []any{[]any{label}}
		}},
		{"foreign comment", func(f *backlogReader) {
			comment := rawBacklogComment(1, 31, "quarter-rationale")
			comment["issue_url"] = "https://api.github.com/repos/foreign/widgets/issues/1"
			f.relations["repos/example/widgets/issues/comments?per_page=100"] = []any{[]any{comment}}
		}},
		{"duplicate comment", func(f *backlogReader) {
			comment := rawBacklogComment(1, 31, "quarter-rationale")
			f.relations["repos/example/widgets/issues/comments?per_page=100"] = []any{[]any{comment, comment}}
		}},
		{"repository replaced", func(f *backlogReader) { f.repositoryIDs = []string{"R_widgets", "R_replaced"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := backlogFixture()
			test.edit(f)
			if _, err := backlogService(f).BacklogInventory(context.Background(), nil, []string{"quarter-rationale"}, true); err == nil {
				t.Fatal("incomplete or drifted source accepted")
			}
		})
	}
}
