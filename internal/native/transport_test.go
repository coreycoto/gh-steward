package native

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

type fakeExecutor struct {
	calls  int
	args   []string
	root   string
	env    []string
	input  []byte
	result Result
	err    error
}

func (f *fakeExecutor) Execute(_ context.Context, _ string, args []string, input []byte, root string, env []string) (Result, error) {
	f.calls++
	f.args = args
	f.root = root
	f.env = env
	f.input = input
	return f.result, f.err
}
func target() contract.Repository {
	return contract.Repository{Host: "github.com", Owner: "example", Name: "widgets", URL: "https://github.com/example/widgets"}
}
func transport(f *fakeExecutor) *Transport {
	return &Transport{Root: "/synthetic/checkout", Repository: target(), Executable: "gh", Executor: f, Environment: []string{"PATH=/synthetic/bin"}, Timeout: time.Second}
}

func TestGraphQLHasExplicitHostCheckoutAndRejectsPartialErrors(t *testing.T) {
	f := &fakeExecutor{result: Result{Stdout: []byte(`{"data":{"repository":{"id":"R_1","nameWithOwner":"example/widgets","url":"https://github.com/example/widgets"}},"errors":[]}`)}}
	tpt := transport(f)
	if _, err := tpt.ReadRepository(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.root != "/synthetic/checkout" || !reflect.DeepEqual(f.args, []string{"api", "graphql", "--hostname", "github.com", "--input", "-"}) || envValue(f.env, "GH_REPO") != "github.com/example/widgets" {
		t.Fatal("native target scope lost")
	}
	f.result.Stdout = []byte(`{"data":{"repository":{"id":"R_1"}},"errors":[{"message":"inaccessible field"}]}`)
	if _, err := tpt.ReadRepository(context.Background()); err == nil {
		t.Fatal("partial GraphQL data accepted")
	}
}

func TestAmbientAndRESTTargetEscapesStopBeforeNativeCalls(t *testing.T) {
	f := &fakeExecutor{}
	tpt := transport(f)
	for _, endpoint := range []string{"repos/other/widgets/issues", "repos/example/other/issues", "https://github.com/repos/example/widgets/issues", "//other.example/repos/example/widgets/issues", "repos/example/widgets/../issues", "/repos/example/widgets/%2e%2e/issues", "repos/example/widgets/%2fother", "repos/example/widgets/issues\\other"} {
		if _, err := tpt.REST(context.Background(), "POST", endpoint, contract.Object{}); err == nil {
			t.Fatalf("escaping target accepted: %s", endpoint)
		}
	}
	tpt.Environment = append(tpt.Environment, "GH_HOST=other.example")
	if _, err := tpt.ReadRepository(context.Background()); err == nil {
		t.Fatal("foreign ambient host accepted")
	}
	if f.calls != 0 {
		t.Fatal("unsafe request dispatched")
	}
	if err := CheckEnvironment(target(), []string{"GH_REPO=other/widgets"}); err == nil {
		t.Fatal("foreign ambient repository accepted")
	}
}

func TestExplicitScopeOverridesAreRejectedBeforeDispatch(t *testing.T) {
	f := &fakeExecutor{result: Result{Stdout: []byte(`{}`)}}
	tpt := transport(f)
	for _, args := range [][]string{{"api", "graphql", "--hostname", "other.example"}, {"api", "graphql", "--hostname=other.example"}, {"issue", "view", "17", "--repo", "other/widgets"}, {"pr", "view", "17", "-Rother/widgets"}, {"issue", "edit", "17", "--repo=https://other.example/example/widgets"}, {"api", "repos/other/widgets/issues"}, {"project", "item-add", "14"}, {"pr", "view", "17", "--repo"}} {
		if _, err := tpt.run(context.Background(), args, nil, false); err == nil {
			t.Fatal("explicit scope escape accepted", args)
		}
	}
	if f.calls != 0 {
		t.Fatal("unsafe explicit target reached native execution")
	}
	if _, err := tpt.run(context.Background(), []string{"pr", "view", "17", "--json", "number"}, nil, false); err != nil {
		t.Fatal(err)
	}
	if f.args[len(f.args)-1] != "github.com/example/widgets" {
		t.Fatal("unscoped command did not receive explicit repository")
	}
}

func TestPaginationRetainsAllPagesAndRejectsTruncatedNestedConnections(t *testing.T) {
	f := &fakeExecutor{result: Result{Stdout: []byte(`[[{"id":1}],[{"id":101}]]`)}}
	pages, err := transport(f).RESTPages(context.Background(), "repos/example/widgets/issues/17/comments?per_page=100")
	if err != nil || len(pages) != 2 {
		t.Fatal("pagination lost a page", err)
	}
	for _, data := range []string{`{"labels":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}`, `{"labels":{"nodes":[],"pageInfo":{}}}`, `{"labels":{"nodes":[],"pageInfo":{"hasNextPage":false}}}`} {
		o, _ := contract.Decode(strings.NewReader(data))
		_, err := CompleteConnection(o, "labels")
		if data != `{"labels":{"nodes":[],"pageInfo":{"hasNextPage":false}}}` && err == nil {
			t.Fatal("incomplete connection accepted")
		}
		if data == `{"labels":{"nodes":[],"pageInfo":{"hasNextPage":false}}}` && err != nil {
			t.Fatal(err)
		}
	}
	f.err = errors.New("network timeout")
	if _, err := transport(f).RESTPages(context.Background(), "repos/example/widgets/issues"); err == nil {
		t.Fatal("network failure became an empty inventory")
	}
}
