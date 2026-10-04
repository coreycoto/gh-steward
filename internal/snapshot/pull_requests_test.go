package snapshot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func linkFixturePolicy() contract.Object {
	return contract.Object{"marker_prefix": "<!-- work-pr-link", "pr_number_pattern": `PR:\s*#(?P<number>\d+)`}
}
func commentFixture(id, issue, pr int64) contract.Object {
	return contract.Object{"id": id, "body": fmt.Sprintf("<!-- work-pr-link -->\nPR: #%d", pr), "issue_url": fmt.Sprintf("https://api.github.com/repos/example/widgets/issues/%d", issue), "html_url": fmt.Sprintf("https://github.com/example/widgets/issues/%d#issuecomment-%d", issue, id)}
}
func prFixture(number int64) contract.Object {
	return contract.Object{"number": number, "node_id": fmt.Sprintf("PR_%d", number), "html_url": fmt.Sprintf("https://github.com/example/widgets/pull/%d", number), "title": "Reviewed implementation", "body": nil, "state": "closed", "draft": false, "merged_at": "2026-10-04T00:00:00Z", "head": contract.Object{"ref": "topic/work", "sha": strings.Repeat("a", 40), "repo": nil}, "base": contract.Object{"ref": "main", "repo": contract.Object{"full_name": "example/widgets", "html_url": "https://github.com/example/widgets"}}}
}
func linkReader() *fakeReader {
	return &fakeReader{relations: map[string][]any{
		"repos/example/widgets/issues/comments?per_page=100": {[]any{commentFixture(11, 1, 20)}, []any{commentFixture(12, 1, 21), commentFixture(13, 2, 22)}},
		"repos/example/widgets/pulls?state=all&per_page=100": {[]any{prFixture(20)}, []any{prFixture(21)}},
	}}
}

func TestCompleteLinkedPRInventoryUsesLatestQualifiedCommentAndAllPages(t *testing.T) {
	got, err := service(linkReader()).LinkedPullRequests(context.Background(), []int64{1, 2}, linkFixturePolicy())
	if err != nil {
		t.Fatal(err)
	}
	byIssue, _ := contract.ObjectAt(got, "by_issue")
	one, _ := contract.ObjectAt(byIssue, "1")
	n, _ := contract.PositiveInteger(one["number"])
	if n != 21 || one["is_merged"] != true || one["state"] != "MERGED" || one["head_repository_owner"] != nil {
		t.Fatal("latest link/merged/deleted head normalization changed", got)
	}
	if len(got["comments"].([]any)) != 3 || len(got["pull_requests"].([]any)) != 1 || len(got["unresolved_links"].([]any)) != 1 {
		t.Fatal("full scoped inventory or known unresolved link lost", got)
	}
}

func TestLinkedPRReadFailuresAndIdentityAmbiguitiesNeverBecomeAbsentLinks(t *testing.T) {
	commentPath := "repos/example/widgets/issues/comments?per_page=100"
	prPath := "repos/example/widgets/pulls?state=all&per_page=100"
	for _, alter := range []func(*fakeReader){
		func(f *fakeReader) { f.err = errors.New("Project-independent repository read denied") },
		func(f *fakeReader) { f.relations[commentPath] = nil },
		func(f *fakeReader) {
			c := commentFixture(11, 1, 20)
			c["issue_url"] = "https://api.github.com/repos/other/widgets/issues/1"
			f.relations[commentPath] = []any{[]any{c}}
		},
		func(f *fakeReader) {
			f.relations[commentPath] = []any{[]any{commentFixture(11, 1, 20), commentFixture(11, 1, 21)}}
		},
		func(f *fakeReader) {
			c := commentFixture(11, 1, 20)
			c["body"] = "<!-- work-pr-link --> PR: #20 PR: #21"
			f.relations[commentPath] = []any{[]any{c}}
		},
		func(f *fakeReader) {
			pr := prFixture(21)
			pr["html_url"] = "https://github.com/other/widgets/pull/21"
			f.relations[prPath] = []any{[]any{pr}}
		},
		func(f *fakeReader) {
			pr := prFixture(21)
			pr["base"].(map[string]any)["repo"].(map[string]any)["full_name"] = "other/widgets"
			f.relations[prPath] = []any{[]any{pr}}
		},
		func(f *fakeReader) { pr := prFixture(21); pr["merged_at"] = ""; f.relations[prPath] = []any{[]any{pr}} },
		func(f *fakeReader) { f.relations[prPath] = []any{[]any{prFixture(21)}, []any{prFixture(21)}} },
	} {
		f := linkReader()
		alter(f)
		if _, err := service(f).LinkedPullRequests(context.Background(), []int64{1}, linkFixturePolicy()); err == nil {
			t.Fatal("invalid complete linkage emitted success")
		}
	}
	if _, err := service(linkReader()).LinkedPullRequests(context.Background(), []int64{1}, contract.Object{}); err == nil {
		t.Fatal("consumer marker policy inferred")
	}
}

func TestKnownNoLinkedMarkerIsACompleteObservation(t *testing.T) {
	c := commentFixture(11, 1, 20)
	c["body"] = "A regular discussion"
	f := &fakeReader{relations: map[string][]any{"repos/example/widgets/issues/comments?per_page=100": {[]any{c}}}}
	got, err := service(f).LinkedPullRequests(context.Background(), []int64{1}, linkFixturePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if links := got["by_issue"].(map[string]any); len(links) != 1 || links["1"] != nil || got["provenance"].(map[string]any)["pull_requests_source"] != "not_requested" {
		t.Fatal("no-link provenance misclassified", got)
	}
}
