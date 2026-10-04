package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
)

func testRepo() contract.Repository {
	return contract.Repository{Host: "github.com", Owner: "example", Name: "widgets", URL: "https://github.com/example/widgets"}
}
func graphFixture() contract.Object {
	rows := []any{}
	for n := int64(1); n <= 4; n++ {
		rows = append(rows, contract.Object{"id": fmt.Sprintf("I_%d", n), "number": n, "title": fmt.Sprintf("Task: %d", n), "body": "authored body", "state": "OPEN", "url": fmt.Sprintf("https://github.com/example/widgets/issues/%d", n), "labels": []any{"task"}, "milestone": nil, "blocked_by_numbers": []any{}, "child_numbers": []any{}, "parent_number": nil})
	}
	return contract.Object{"repo": testRepo().Object(), "issues": rows, "provenance": contract.Object{"live": true, "complete": true, "issue_state": "all", "repository_node_id": "R_widgets"}}
}

type relationshipFixture struct {
	graph     contract.Object
	file      string
	writes    int
	failAt    int
	crashAt   int
	crashRead int
	failRead  int
	driftAt   int
	reads     int
}

func (f *relationshipFixture) load() error {
	if f.file == "" {
		return nil
	}
	r, e := os.Open(f.file)
	if e != nil {
		return e
	}
	defer r.Close()
	f.graph, e = contract.Decode(r)
	return e
}
func (f *relationshipFixture) save() error {
	if f.file == "" {
		return nil
	}
	b, e := contract.Canonical(f.graph)
	if e != nil {
		return e
	}
	return os.WriteFile(f.file, b, 0600)
}
func (f *relationshipFixture) IssueGraph(context.Context) (contract.Object, error) {
	if err := f.load(); err != nil {
		return nil, err
	}
	f.reads++
	if f.crashRead == f.reads {
		os.Exit(62)
	}
	if f.failRead == f.reads {
		return nil, errors.New("after-state read unavailable")
	}
	if f.driftAt == f.reads {
		rows, _ := contract.Objects(f.graph, "issues")
		rows[3]["body"] = "concurrent unrelated change"
		if err := f.save(); err != nil {
			return nil, err
		}
	}
	return contract.Clone(f.graph)
}
func (f *relationshipFixture) mutate(a, b int64, kind string, add bool) (contract.Object, error) {
	if err := f.load(); err != nil {
		return nil, err
	}
	f.writes++
	op := contract.Operation{Target: contract.Object{"issue_number": a, "related_issue_number": b, "relation": kind}, After: contract.Object{"present": add}}
	if err := projectEdge(f.graph, op); err != nil {
		return nil, err
	}
	if err := f.save(); err != nil {
		return nil, err
	}
	if f.crashAt == f.writes {
		os.Exit(61)
	}
	if f.failAt == f.writes {
		return nil, errors.New("connection failed after provider accepted write")
	}
	return contract.Object{"acknowledged": true}, nil
}
func (f *relationshipFixture) AddRelationship(_ context.Context, a, b int64, kind string) (contract.Object, error) {
	return f.mutate(a, b, kind, true)
}
func (f *relationshipFixture) RemoveRelationship(_ context.Context, a, b int64, kind string) (contract.Object, error) {
	return f.mutate(a, b, kind, false)
}
func authored(rows ...contract.Object) contract.Object {
	out := []any{}
	for _, r := range rows {
		out = append(out, r)
	}
	return contract.Object{"schema_version": 1, "issues": out}
}
func engine(root string, f *relationshipFixture) apply.Engine {
	return apply.Engine{Root: root, Repository: testRepo(), Command: RelationshipCommand, Adapter: Relationships{Provider: f}}
}

func TestReviewedRelationshipReparentingRemovesEdgesBeforeAdding(t *testing.T) {
	f := &relationshipFixture{graph: graphFixture()}
	rows, _ := contract.Objects(f.graph, "issues")
	rows[0]["child_numbers"] = []any{int64(2)}
	rows[1]["parent_number"] = int64(1)
	rows[1]["child_numbers"] = []any{int64(3)}
	rows[2]["parent_number"] = int64(2)
	payload := authored(contract.Object{"issue_number": int64(2), "desired_parent_issue_number": int64(3)}, contract.Object{"issue_number": int64(3), "desired_parent_issue_number": int64(1)})
	plan, err := PrepareRelationships(context.Background(), f, testRepo(), payload, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Operations) != 4 || plan.Operations[0].After["present"] != false || plan.Operations[1].After["present"] != false {
		t.Fatal("reparenting could introduce a temporary cycle", plan.Operations)
	}
	root := t.TempDir()
	result, err := engine(root, f).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatal(result, err)
	}
	if f.writes != 4 {
		t.Fatal("primitive inventory changed", f.writes)
	}
	if _, err = engine(root, f).Apply(context.Background(), plan.Object()); err != nil || f.writes != 4 {
		t.Fatal("completed writes were replayed", err, f.writes)
	}
}
func TestRelationshipInventoryDriftStopsBeforeNextWriteAndTerminal(t *testing.T) {
	for _, read := range []int{2, 4, 6} {
		f := &relationshipFixture{graph: graphFixture()}
		payload := authored(contract.Object{"issue_number": int64(2), "desired_blocked_by_issue_numbers": []any{int64(1)}}, contract.Object{"issue_number": int64(3), "desired_blocked_by_issue_numbers": []any{int64(1)}})
		plan, err := PrepareRelationships(context.Background(), f, testRepo(), payload, nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		f.driftAt = read
		if _, err = engine(t.TempDir(), f).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "drifted") {
			t.Fatal("unrelated complete-inventory drift accepted", read, err)
		}
		want := (read - 2) / 2
		if f.writes != want {
			t.Fatal("write occurred after drift", read, f.writes, want)
		}
	}
}
func TestRelationshipMissingMetadataIncompleteSourceAndAlteredIntentFailClosed(t *testing.T) {
	for _, change := range []func(contract.Object){func(g contract.Object) { g["provenance"].(map[string]any)["complete"] = false }, func(g contract.Object) {
		g["repo"] = contract.Object{"nameWithOwner": "other/widgets", "url": "https://github.com/other/widgets"}
	}, func(g contract.Object) { rows, _ := contract.Objects(g, "issues"); delete(rows[0], "body") }, func(g contract.Object) {
		rows, _ := contract.Objects(g, "issues")
		rows[0]["url"] = "https://github.com/other/widgets/issues/1"
	}, func(g contract.Object) { g["provenance"].(map[string]any)["issue_state"] = "open" }, func(g contract.Object) {
		delete(g["provenance"].(map[string]any), "repository_node_id")
	}} {
		f := &relationshipFixture{graph: graphFixture()}
		change(f.graph)
		if _, err := PrepareRelationships(context.Background(), f, testRepo(), authored(), nil, time.Now()); err == nil || f.writes != 0 {
			t.Fatal("invalid source prepared apply")
		}
	}
	f := &relationshipFixture{graph: graphFixture()}
	plan, err := PrepareRelationships(context.Background(), f, testRepo(), authored(contract.Object{"issue_number": int64(2), "desired_parent_issue_number": int64(1)}), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	plan.Operations[0].Target["related_issue_number"] = int64(4)
	plan.SHA256, _ = contract.Digest(plan.Unsigned())
	if _, err = engine(t.TempDir(), f).Apply(context.Background(), plan.Object()); err == nil || f.writes != 0 {
		t.Fatal("hash-consistent altered primitives dispatched")
	}
}
func TestAcceptedRelationshipWithAmbiguousCompletionNeverRetries(t *testing.T) {
	f := &relationshipFixture{graph: graphFixture(), failAt: 1}
	plan, err := PrepareRelationships(context.Background(), f, testRepo(), authored(contract.Object{"issue_number": int64(2), "desired_parent_issue_number": int64(1)}), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err = engine(root, f).Apply(context.Background(), plan.Object()); err == nil {
		t.Fatal("ambiguous result claimed completed")
	}
	f.failAt = 0
	if _, err = engine(root, f).Apply(context.Background(), plan.Object()); err == nil || !strings.Contains(err.Error(), "positive provider evidence") {
		t.Fatal("matching edge was accepted as operation identity", err)
	}
	if f.writes != 1 {
		t.Fatal("ambiguous write replayed", f.writes)
	}
}

func TestDurableNativeAcknowledgementRecoversInterruptedAfterStateRead(t *testing.T) {
	f := &relationshipFixture{graph: graphFixture()}
	plan, err := PrepareRelationships(context.Background(), f, testRepo(), authored(contract.Object{"issue_number": int64(2), "desired_parent_issue_number": int64(1)}), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.failRead = 3
	root := t.TempDir()
	if _, err = engine(root, f).Apply(context.Background(), plan.Object()); err == nil {
		t.Fatal("unverified after-state claimed completed")
	}
	f.failRead = 0
	if result, err := engine(root, f).Apply(context.Background(), plan.Object()); err != nil || result["status"] != "completed" || f.writes != 1 {
		t.Fatal("captured native acknowledgement could not recover without retry", result, err, f.writes)
	}
}

func TestRelationshipFreshProcessCrashPreservesAcceptedWriteAndReceipt(t *testing.T) {
	if os.Getenv("STEWARD_RELATIONSHIP_CRASH") != "" {
		root := os.Getenv("STEWARD_RELATIONSHIP_ROOT")
		f := &relationshipFixture{file: filepath.Join(root, "provider.json"), crashAt: 2}
		if os.Getenv("STEWARD_RELATIONSHIP_CRASH") == "after-ack" {
			f.crashAt = 0
			f.crashRead = 4
		}
		raw, err := os.Open(filepath.Join(root, "plan.json"))
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		plan, err := contract.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		_, err = engine(root, f).Apply(context.Background(), plan)
		t.Fatal("crash fixture failed to exit", err)
		return
	}
	for _, mode := range []string{"before-ack", "after-ack"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			f := &relationshipFixture{graph: graphFixture(), file: filepath.Join(root, "provider.json")}
			if err := f.save(); err != nil {
				t.Fatal(err)
			}
			plan, err := PrepareRelationships(context.Background(), f, testRepo(), authored(contract.Object{"issue_number": int64(2), "desired_parent_issue_number": int64(1)}, contract.Object{"issue_number": int64(3), "desired_parent_issue_number": int64(1)}), nil, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			body, _ := contract.Canonical(plan.Object())
			if err := os.WriteFile(filepath.Join(root, "plan.json"), body, 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(os.Args[0], "-test.run=^TestRelationshipFreshProcessCrashPreservesAcceptedWriteAndReceipt$")
			command.Env = append(os.Environ(), "STEWARD_RELATIONSHIP_CRASH="+mode, "STEWARD_RELATIONSHIP_ROOT="+root)
			err = command.Run()
			exit, ok := err.(*exec.ExitError)
			wantExit := 61
			if mode == "after-ack" {
				wantExit = 62
			}
			if !ok || exit.ExitCode() != wantExit {
				t.Fatal("fixture did not crash after second provider effect", err)
			}
			fresh := &relationshipFixture{file: filepath.Join(root, "provider.json")}
			result, err := engine(root, fresh).Apply(context.Background(), plan.Object())
			if mode == "after-ack" {
				if err != nil || result["status"] != "completed" {
					t.Fatal("durable accepted second primitive did not recover", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "positive provider evidence") {
				t.Fatal("unknown primitive continued", err)
			}
			if fresh.writes != 0 {
				t.Fatal("completed or accepted writes replayed")
			}
			if err := fresh.load(); err != nil {
				t.Fatal(err)
			}
			rows, _ := contract.Objects(fresh.graph, "issues")
			for _, i := range []int{1, 2} {
				n, _ := contract.PositiveInteger(rows[i]["parent_number"])
				if n != 1 {
					t.Fatal("accepted provider progress lost", rows)
				}
			}
		})
	}
}
