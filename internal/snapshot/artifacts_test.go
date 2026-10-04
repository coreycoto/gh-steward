package snapshot

import (
	"context"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

type artifactReaderFixture struct {
	repository contract.Object
	run        contract.Object
	pages      []any
	pageErr    error
}

func (f *artifactReaderFixture) ReadRepository(context.Context) (contract.Object, error) {
	return contract.Clone(f.repository)
}
func (f *artifactReaderFixture) ReadWorkflowRun(context.Context, int64) (contract.Object, error) {
	return contract.Clone(f.run)
}
func (f *artifactReaderFixture) RunArtifactPages(context.Context, int64) ([]any, error) {
	if f.pageErr != nil {
		return nil, f.pageErr
	}
	data, err := contract.Canonical(contract.Object{"pages": f.pages})
	if err != nil {
		return nil, err
	}
	decoded, err := contract.Decode(strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	return contract.Array(decoded, "pages")
}

func artifactRepo() contract.Repository {
	return contract.Repository{Host: "github.com", Owner: "example", Name: "widgets", URL: "https://github.com/example/widgets"}
}

func artifactReader(artifacts ...contract.Object) *artifactReaderFixture {
	repo := artifactRepo()
	entries := make([]any, 0, len(artifacts))
	for _, artifact := range artifacts {
		entries = append(entries, artifact)
	}
	return &artifactReaderFixture{
		repository: contract.Object{"id": "R_widgets", "nameWithOwner": repo.FullName(), "url": repo.URL},
		run:        contract.Object{"id": int64(123), "repository": contract.Object{"full_name": repo.FullName(), "html_url": repo.URL, "node_id": "R_widgets"}},
		pages:      []any{contract.Object{"total_count": int64(len(entries)), "artifacts": entries}},
	}
}

func TestRunArtifactInventoryKeepsEveryPageAndNormalizesRunScopedIdentity(t *testing.T) {
	reader := artifactReader()
	first, second := []any{}, []any{}
	for id := int64(1); id <= 101; id++ {
		row := contract.Object{"id": id, "name": "build", "size_in_bytes": id * 10, "expired": false}
		if id <= 100 {
			first = append(first, row)
		} else {
			second = append(second, row)
		}
	}
	reader.pages = []any{
		contract.Object{"total_count": int64(101), "artifacts": first},
		contract.Object{"total_count": int64(101), "artifacts": second},
	}
	inventory, err := RunArtifactInventory(context.Background(), reader, artifactRepo(), 123)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := contract.Objects(inventory, "artifacts")
	if err != nil || len(artifacts) != 101 || artifacts[0]["id"] != int64(1) || artifacts[100]["id"] != int64(101) {
		t.Fatal("pagination was truncated or not normalized", len(artifacts), err)
	}
	if inventory["total_count"] != int64(101) || inventory["generated_at"] != nil {
		t.Fatal("inventory contains inconsistent count or unstable timestamp", inventory)
	}
	repo, _ := contract.ObjectAt(inventory, "repository")
	run, _ := contract.ObjectAt(inventory, "run")
	provenance, _ := contract.ObjectAt(inventory, "provenance")
	if repo["node_id"] != "R_widgets" || run["id"] != int64(123) || run["repository_node_id"] != "R_widgets" || provenance["live"] != true || provenance["complete"] != true {
		t.Fatal("inventory lost exact repository/run source identity", inventory)
	}
}

func TestRunArtifactInventoryAcceptsAnEmptyCompleteRun(t *testing.T) {
	inventory, err := RunArtifactInventory(context.Background(), artifactReader(), artifactRepo(), 123)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := contract.Objects(inventory, "artifacts")
	if err != nil || len(artifacts) != 0 || inventory["total_count"] != int64(0) {
		t.Fatal("empty complete inventory became a source failure", inventory, err)
	}
}

func TestRunArtifactInventoryRejectsIncompleteOrMalformedPages(t *testing.T) {
	for _, test := range []struct {
		name  string
		pages []any
	}{
		{name: "no-pages"},
		{name: "not-an-object", pages: []any{"bad"}},
		{name: "missing-artifacts", pages: []any{contract.Object{"total_count": int64(0)}}},
		{name: "boolean-total", pages: []any{contract.Object{"total_count": true, "artifacts": []any{}}}},
		{name: "fraction-total", pages: []any{contract.Object{"total_count": 1.5, "artifacts": []any{}}}},
		{name: "changed-total", pages: []any{contract.Object{"total_count": int64(1), "artifacts": []any{}}, contract.Object{"total_count": int64(2), "artifacts": []any{}}}},
		{name: "truncated", pages: []any{contract.Object{"total_count": int64(2), "artifacts": []any{contract.Object{"id": int64(1), "name": "build"}}}}},
		{name: "malformed-row", pages: []any{contract.Object{"total_count": int64(1), "artifacts": []any{true}}}},
		{name: "boolean-id", pages: []any{contract.Object{"total_count": int64(1), "artifacts": []any{contract.Object{"id": true, "name": "build"}}}}},
		{name: "missing-name", pages: []any{contract.Object{"total_count": int64(1), "artifacts": []any{contract.Object{"id": int64(1)}}}}},
		{name: "invalid-size", pages: []any{contract.Object{"total_count": int64(1), "artifacts": []any{contract.Object{"id": int64(1), "name": "build", "size_in_bytes": -1}}}}},
		{name: "invalid-expired", pages: []any{contract.Object{"total_count": int64(1), "artifacts": []any{contract.Object{"id": int64(1), "name": "build", "expired": "false"}}}}},
		{name: "foreign-nested-run", pages: []any{contract.Object{"total_count": int64(1), "artifacts": []any{contract.Object{"id": int64(1), "name": "build", "workflow_run": contract.Object{"id": int64(124)}}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := artifactReader()
			reader.pages = test.pages
			if _, err := RunArtifactInventory(context.Background(), reader, artifactRepo(), 123); err == nil {
				t.Fatal("incomplete or malformed artifact pages accepted")
			}
		})
	}

	duplicate := artifactReader()
	duplicate.pages = []any{
		contract.Object{"total_count": int64(2), "artifacts": []any{contract.Object{"id": int64(1), "name": "first"}}},
		contract.Object{"total_count": int64(2), "artifacts": []any{contract.Object{"id": int64(1), "name": "second"}}},
	}
	if _, err := RunArtifactInventory(context.Background(), duplicate, artifactRepo(), 123); err == nil {
		t.Fatal("duplicate artifact ID accepted")
	}
}

func TestRunArtifactInventoryRejectsRepositoryAndRunIncarnationDrift(t *testing.T) {
	for _, mutate := range []func(*artifactReaderFixture){
		func(f *artifactReaderFixture) { f.repository["id"] = "R_recreated" },
		func(f *artifactReaderFixture) { f.run["id"] = int64(124) },
		func(f *artifactReaderFixture) {
			repo, _ := contract.ObjectAt(f.run, "repository")
			repo["full_name"] = "foreign/widgets"
		},
		func(f *artifactReaderFixture) {
			repo, _ := contract.ObjectAt(f.run, "repository")
			repo["node_id"] = "R_recreated"
		},
		func(f *artifactReaderFixture) {
			repo, _ := contract.ObjectAt(f.run, "repository")
			repo["html_url"] = "https://other.example/example/widgets"
		},
	} {
		reader := artifactReader()
		mutate(reader)
		if _, err := RunArtifactInventory(context.Background(), reader, artifactRepo(), 123); err == nil {
			t.Fatal("foreign repository or run identity accepted")
		}
	}
}
