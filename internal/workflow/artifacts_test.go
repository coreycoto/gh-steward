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

type artifactDeletionFixture struct {
	inventory         contract.Object
	file              string
	deleteCalls       []int64
	reads             int
	driftRead         int
	crashRead         int
	failRead          int
	failAfterMutation int
	leaveAfterAck     bool
	crashBeforeAck    bool
}

func artifactInventoryFixture(names ...string) contract.Object {
	artifacts := make([]any, 0, len(names))
	for index, name := range names {
		artifacts = append(artifacts, contract.Object{"id": int64(index + 1), "name": name, "size_in_bytes": int64(100 + index), "expired": false})
	}
	return contract.Object{
		"repository":  contract.Object{"host": "github.com", "owner": "example", "name": "widgets", "url": "https://github.com/example/widgets", "node_id": "R_widgets"},
		"run":         contract.Object{"id": int64(123), "repository_node_id": "R_widgets"},
		"total_count": int64(len(artifacts)), "artifacts": artifacts,
		"provenance": contract.Object{"source": "github_api", "live": true, "complete": true},
	}
}

func (f *artifactDeletionFixture) load() error {
	if f.file == "" {
		return nil
	}
	file, err := os.Open(f.file)
	if err != nil {
		return err
	}
	defer file.Close()
	state, err := contract.Decode(file)
	if err != nil {
		return err
	}
	f.inventory, err = contract.ObjectAt(state, "inventory")
	if err != nil {
		return err
	}
	rawCalls, err := contract.Array(state, "delete_calls")
	if err != nil {
		return err
	}
	f.deleteCalls = nil
	for _, raw := range rawCalls {
		id, err := contract.PositiveInteger(raw)
		if err != nil {
			return err
		}
		f.deleteCalls = append(f.deleteCalls, id)
	}
	return nil
}

func (f *artifactDeletionFixture) save() error {
	if f.file == "" {
		return nil
	}
	data, err := contract.Canonical(contract.Object{"inventory": f.inventory, "delete_calls": positiveValues(f.deleteCalls)})
	if err != nil {
		return err
	}
	return os.WriteFile(f.file, data, 0600)
}

func positiveValues(values []int64) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func (f *artifactDeletionFixture) RunArtifactInventory(_ context.Context, runID int64) (contract.Object, error) {
	if err := f.load(); err != nil {
		return nil, err
	}
	f.reads++
	if f.crashRead == f.reads {
		os.Exit(87)
	}
	if f.failRead == f.reads {
		return nil, errors.New("independent after-state unavailable")
	}
	if runID != 123 {
		return nil, errors.New("foreign run")
	}
	if f.driftRead == f.reads {
		rows, _ := contract.Array(f.inventory, "artifacts")
		rows = append(rows, contract.Object{"id": int64(999), "name": "unrelated-new-artifact", "size_in_bytes": int64(50), "expired": false})
		f.inventory["artifacts"], f.inventory["total_count"] = rows, int64(len(rows))
		if err := f.save(); err != nil {
			return nil, err
		}
	}
	return contract.Clone(f.inventory)
}

func (f *artifactDeletionFixture) DeleteRunArtifact(_ context.Context, runID, artifactID int64) (contract.Object, error) {
	if err := f.load(); err != nil {
		return nil, err
	}
	if runID != 123 {
		return nil, errors.New("foreign run")
	}
	f.deleteCalls = append(f.deleteCalls, artifactID)
	if !f.leaveAfterAck {
		rows, _ := contract.Objects(f.inventory, "artifacts")
		remaining := make([]any, 0, len(rows))
		found := false
		for _, row := range rows {
			id, _ := contract.PositiveInteger(row["id"])
			if id == artifactID {
				found = true
				continue
			}
			remaining = append(remaining, row)
		}
		if !found {
			return nil, errors.New("artifact not found in run")
		}
		f.inventory["artifacts"], f.inventory["total_count"] = remaining, int64(len(remaining))
	}
	if err := f.save(); err != nil {
		return nil, err
	}
	if f.crashBeforeAck {
		os.Exit(86)
	}
	if f.failAfterMutation == len(f.deleteCalls) {
		return nil, errors.New("ambiguous delete outcome")
	}
	return contract.Object{"run_id": runID, "artifact_id": artifactID, "status_code": int64(204), "no_content": true}, nil
}

func artifactEngine(root string, provider RunArtifactDeletionProvider) apply.Engine {
	return apply.Engine{Root: root, Repository: testRepo(), Command: RunArtifactDeleteCommand, Adapter: RunArtifactDeletionAdapter{Provider: provider}}
}

func prepareArtifactDeletion(t *testing.T, f *artifactDeletionFixture, selection RunArtifactSelection) contract.Plan {
	t.Helper()
	plan, err := PrepareRunArtifactDeletion(context.Background(), f, testRepo(), 123, selection, time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPrepareAndApplyDeleteEveryExactNameMatchAndReportMissingNames(t *testing.T) {
	f := &artifactDeletionFixture{inventory: artifactInventoryFixture("keep", "target", "target")}
	plan := prepareArtifactDeletion(t, f, RunArtifactSelection{Names: []string{"target", "missing"}, IgnoreMissing: false})
	if plan.Command != RunArtifactDeleteCommand || len(plan.Operations) != 2 {
		t.Fatal("plan did not select every exact name match", plan.Operations)
	}
	first, firstErr := contract.PositiveInteger(plan.Operations[0].Target["artifact_id"])
	second, secondErr := contract.PositiveInteger(plan.Operations[1].Target["artifact_id"])
	if firstErr != nil || secondErr != nil || first != 2 || second != 3 {
		t.Fatal("plan did not select every exact name match in deterministic order", plan.Operations)
	}
	missing, err := MissingRunArtifactNames(plan)
	if err != nil || !same(missing, []string{"missing"}) {
		t.Fatal("missing names were not retained", missing, err)
	}
	result, err := artifactEngine(t.TempDir(), f).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" {
		t.Fatal(result, err)
	}
	if len(f.deleteCalls) != 2 || f.deleteCalls[0] != 2 || f.deleteCalls[1] != 3 {
		t.Fatal("selected deletion primitives were not individually dispatched", f.deleteCalls)
	}
	rows, _ := contract.Objects(f.inventory, "artifacts")
	if len(rows) != 1 || rows[0]["name"] != "keep" {
		t.Fatal("unselected artifact changed", rows)
	}
}

func TestArtifactSelectionRejectsDuplicateNamesAndPreparationNeverInfersSelection(t *testing.T) {
	f := &artifactDeletionFixture{inventory: artifactInventoryFixture("target")}
	for _, selection := range []RunArtifactSelection{{}, {Names: []string{"target", "target"}}, {Names: []string{"  "}}} {
		if _, err := PrepareRunArtifactDeletion(context.Background(), f, testRepo(), 123, selection, time.Now()); err == nil {
			t.Fatal("missing or ambiguous consumer selection accepted", selection)
		}
	}
	if f.reads != 0 {
		t.Fatal("invalid selection reached the provider", f.reads)
	}
}

func TestArtifactDeletionBlocksFullInventoryDriftBeforeBetweenAndAfterWrites(t *testing.T) {
	t.Run("before-first-write", func(t *testing.T) {
		f := &artifactDeletionFixture{inventory: artifactInventoryFixture("target")}
		plan := prepareArtifactDeletion(t, f, RunArtifactSelection{Names: []string{"target"}})
		f.driftRead = f.reads + 1
		if _, err := artifactEngine(t.TempDir(), f).Apply(context.Background(), plan.Object()); err == nil || len(f.deleteCalls) != 0 {
			t.Fatal("source drift did not block the first delete", err, f.deleteCalls)
		}
	})
	t.Run("between-deletes", func(t *testing.T) {
		f := &artifactDeletionFixture{inventory: artifactInventoryFixture("target", "target")}
		plan := prepareArtifactDeletion(t, f, RunArtifactSelection{Names: []string{"target"}})
		// Read 1 prepared the plan; reads 2-4 verify and complete the first
		// delete; read 5 is the complete preflight for the second primitive.
		f.driftRead = f.reads + 4
		if _, err := artifactEngine(t.TempDir(), f).Apply(context.Background(), plan.Object()); err == nil || len(f.deleteCalls) != 1 {
			t.Fatal("inter-operation drift allowed later deletion", err, f.deleteCalls)
		}
	})
	t.Run("terminal-read", func(t *testing.T) {
		f := &artifactDeletionFixture{inventory: artifactInventoryFixture("target", "target")}
		plan := prepareArtifactDeletion(t, f, RunArtifactSelection{Names: []string{"target"}})
		// Read 8 is the terminal complete-inventory verification after both
		// independent 204 and after-state receipts.
		f.driftRead = f.reads + 7
		result, err := artifactEngine(t.TempDir(), f).Apply(context.Background(), plan.Object())
		if err == nil || result != nil || len(f.deleteCalls) != 2 {
			t.Fatal("terminal source drift produced false success", result, err, f.deleteCalls)
		}
	})
}

func TestArtifactApplyReplayAndChangedOperationsDoNotRedispatch(t *testing.T) {
	f := &artifactDeletionFixture{inventory: artifactInventoryFixture("target")}
	plan := prepareArtifactDeletion(t, f, RunArtifactSelection{Names: []string{"target"}})
	engine := artifactEngine(t.TempDir(), f)
	for range 2 {
		result, err := engine.Apply(context.Background(), plan.Object())
		if err != nil || result["status"] != "completed" {
			t.Fatal(result, err)
		}
	}
	if len(f.deleteCalls) != 1 {
		t.Fatal("completed delete replayed", f.deleteCalls)
	}

	badOperations := append([]contract.Operation{}, plan.Operations...)
	badOperations[0].Target = contract.Object{"run_id": int64(123), "artifact_id": int64(999), "name": "target"}
	tampered, err := contract.PreparePlan(RunArtifactDeleteCommand, testRepo(), plan.Sources, plan.Data, badOperations, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	other := &artifactDeletionFixture{inventory: artifactInventoryFixture("target")}
	if _, err := artifactEngine(t.TempDir(), other).Apply(context.Background(), tampered.Object()); err == nil || len(other.deleteCalls) != 0 {
		t.Fatal("self-consistent but altered operation was authorized", err, other.deleteCalls)
	}
}

func TestArtifactAckAloneDoesNotProveIndependentAfterState(t *testing.T) {
	f := &artifactDeletionFixture{inventory: artifactInventoryFixture("target"), leaveAfterAck: true}
	plan := prepareArtifactDeletion(t, f, RunArtifactSelection{Names: []string{"target"}})
	engine := artifactEngine(t.TempDir(), f)
	if _, err := engine.Apply(context.Background(), plan.Object()); err == nil || len(f.deleteCalls) != 1 {
		t.Fatal("provider acknowledgement without after-state was accepted", err, f.deleteCalls)
	}
	if _, err := engine.Apply(context.Background(), plan.Object()); err == nil || len(f.deleteCalls) != 1 {
		t.Fatal("acknowledged operation with stale after-state was replayed", err, f.deleteCalls)
	}
}

func TestRunArtifactCrashRecoveryRequiresPersisted204Acknowledgement(t *testing.T) {
	if mode := os.Getenv("STEWARD_ARTIFACT_CRASH_MODE"); mode != "" {
		root := os.Getenv("STEWARD_ARTIFACT_CRASH_ROOT")
		planFile, err := os.Open(filepath.Join(root, "plan.json"))
		if err != nil {
			t.Fatal(err)
		}
		defer planFile.Close()
		plan, err := contract.Decode(planFile)
		if err != nil {
			t.Fatal(err)
		}
		provider := &artifactDeletionFixture{file: filepath.Join(root, "provider.json")}
		if mode == "before-ack" {
			provider.crashBeforeAck = true
		} else {
			provider.crashRead = 3 // Engine preflight, dispatch preflight, independent after-state.
		}
		_, err = artifactEngine(root, provider).Apply(context.Background(), plan)
		t.Fatalf("crash fixture returned without interruption: %v", err)
		return
	}

	for _, mode := range []string{"before-ack", "after-ack"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			provider := &artifactDeletionFixture{inventory: artifactInventoryFixture("target"), file: filepath.Join(root, "provider.json")}
			if err := provider.save(); err != nil {
				t.Fatal(err)
			}
			plan := prepareArtifactDeletion(t, provider, RunArtifactSelection{Names: []string{"target"}})
			data, _ := contract.Canonical(plan.Object())
			if err := os.WriteFile(filepath.Join(root, "plan.json"), data, 0600); err != nil {
				t.Fatal(err)
			}

			command := exec.Command(os.Args[0], "-test.run=^TestRunArtifactCrashRecoveryRequiresPersisted204Acknowledgement$")
			command.Env = append(os.Environ(), "STEWARD_ARTIFACT_CRASH_MODE="+mode, "STEWARD_ARTIFACT_CRASH_ROOT="+root)
			err := command.Run()
			exit, ok := err.(*exec.ExitError)
			want := 86
			if mode == "after-ack" {
				want = 87
			}
			if !ok || exit.ExitCode() != want {
				t.Fatal("crash subprocess did not stop at the intended boundary", err)
			}

			fresh := &artifactDeletionFixture{file: provider.file}
			result, applyErr := artifactEngine(root, fresh).Apply(context.Background(), plan.Object())
			if err := fresh.load(); err != nil {
				t.Fatal("could not inspect fixture's durable write count", err)
			}
			if mode == "before-ack" {
				if applyErr == nil || !strings.Contains(applyErr.Error(), "no retained 204 acknowledgement") || result != nil {
					t.Fatal("unknown deletion without an ACK was not blocked", result, applyErr)
				}
			} else if applyErr != nil || result == nil || result["status"] != "completed" {
				t.Fatal("durable ACK did not recover from exact independent after-state", result, applyErr)
			}
			if len(fresh.deleteCalls) != 1 {
				t.Fatal("interrupted delete was redispatched", fresh.deleteCalls)
			}
		})
	}
}

func TestArtifactUnknownWithoutAckStopsLaterPrimitives(t *testing.T) {
	f := &artifactDeletionFixture{inventory: artifactInventoryFixture("target", "target"), failAfterMutation: 2}
	plan := prepareArtifactDeletion(t, f, RunArtifactSelection{Names: []string{"target"}})
	engine := artifactEngine(t.TempDir(), f)
	if _, err := engine.Apply(context.Background(), plan.Object()); err == nil || len(f.deleteCalls) != 2 {
		t.Fatal("ambiguous second delete did not stop the plan", err, f.deleteCalls)
	}
	if _, err := engine.Apply(context.Background(), plan.Object()); err == nil || len(f.deleteCalls) != 2 {
		t.Fatal("unknown second delete without ACK was replayed", err, f.deleteCalls)
	}
}

func TestMissingArtifactIgnorePolicyIsCapturedExactly(t *testing.T) {
	f := &artifactDeletionFixture{inventory: artifactInventoryFixture("target")}
	plan := prepareArtifactDeletion(t, f, RunArtifactSelection{Names: []string{"missing"}, IgnoreMissing: true})
	selection, _ := contract.ObjectAt(plan.Data, "selection")
	if selection["ignore_missing"] != true || len(plan.Operations) != 0 {
		t.Fatal("missing-name policy or no-op selection was lost", plan.Object())
	}
	if _, err := artifactEngine(t.TempDir(), f).Apply(context.Background(), plan.Object()); err != nil {
		t.Fatal("complete no-op plan failed", err)
	}
	missing, err := MissingRunArtifactNames(plan)
	if err != nil || fmt.Sprint(missing) != "[missing]" {
		t.Fatal(missing, err)
	}
}
