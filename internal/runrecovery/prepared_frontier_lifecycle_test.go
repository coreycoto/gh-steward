package runrecovery

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/workflow"
)

const (
	preparedLifecycleWorkflow    = "task.yml"
	preparedLifecycleWorkflowID  = int64(9)
	preparedLifecycleRunID       = int64(100)
	preparedLifecycleRunName     = "Prepared lifecycle"
	preparedLifecycleRecoveryKey = "execution-17"
	preparedLifecycleWorkflowSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type preparedLifecycle struct {
	engine       *Engine
	reader       *recoveryReaderFixture
	repository   contract.Repository
	target       Object
	plan         Object
	workflowData []byte
	policyBytes  []byte
	eventData    []byte
	workflowSHA  string
	artifacts    []Object
	nextArtifact int64
}

func newPreparedLifecycle(t *testing.T, operationCount int) *preparedLifecycle {
	t.Helper()
	repository := policyTestRepository(t)
	workflowData := []byte("name: Prepared recovery fixture\non:\n  workflow_dispatch:\njobs:\n  apply:\n    steps:\n      - name: Apply execution\n        run: true\n")
	workflowDigest := SHA256(workflowData)
	preparedRecovery := Object{
		"workflow_source_sha256": workflowDigest,
		"mutators":               []any{Object{"job": "apply", "steps": []any{"Apply execution"}}},
	}
	policy := Object{"schema_version": 1, "workflows": Object{preparedLifecycleWorkflow: Object{
		"mutator_step_alternatives": []any{[]any{"Apply execution"}}, "reviewed_source_shas": []any{}, "allow_publication": false,
		"plans": Object{"execution": Object{"command": "execution-sync", "domain_profile": "execution",
			"allowed_operation_kinds": []any{"issue-comment-upsert"}, "attempt_target": "execution",
			"approval": Object{"kind": "git-slop-execution"},
			"event":    Object{"kind": "execution_event", "path": "events/dispatch-event.json"}, "parent_merge": nil,
			"prepared_recovery": preparedRecovery,
		}},
	}}}
	policyBytes, err := Canonical(policy)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(policy, repository)
	if err != nil {
		t.Fatal(err)
	}
	number := int64(17)
	selector := Object{"issue_number": number, "pull_request_number": int64(0), "skip_project_sync": true, "project": nil}
	operations := make([]contract.Operation, 0, operationCount)
	for index := 0; index < operationCount; index++ {
		operations = append(operations, contract.Operation{
			ID:     fmt.Sprintf("execution:comment-%d", index+1),
			Kind:   "issue-comment-upsert",
			Target: Object{"issue_number": number, "marker": fmt.Sprintf("<!-- prepared-%d -->", index+1)},
			Before: Object{"body": nil}, After: Object{"body": fmt.Sprintf("Reviewed work state %d", index+1)},
		})
	}
	planValue, err := contract.PreparePlan("execution-sync", repository,
		Object{"github": Object{"live": true, "complete": true}}, Object{"selector": selector}, operations,
		time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	event := Object{"repository": Object{"full_name": repository.FullName()}, "inputs": Object{"issue_number": "17", "pr_number": ""}}
	eventData, err := Canonical(event)
	if err != nil {
		t.Fatal(err)
	}
	workflowBlob := sha1.New()
	fmt.Fprintf(workflowBlob, "blob %d%c", len(workflowData), 0)
	_, _ = workflowBlob.Write(workflowData)
	workflowPacket := Object{
		"type": "file", "path": ".github/workflows/" + preparedLifecycleWorkflow,
		"encoding": "base64", "content": base64.StdEncoding.EncodeToString(workflowData),
		"sha": hex.EncodeToString(workflowBlob.Sum(nil)),
	}
	reader := &recoveryReaderFixture{reads: map[string]Object{
		fmt.Sprintf("repos/%s/contents/.github/workflows/%s?ref=%s", repository.FullName(), preparedLifecycleWorkflow, preparedLifecycleWorkflowSHA): workflowPacket,
	}, pages: map[string][]any{}, archives: map[int64][]byte{}}
	return &preparedLifecycle{
		engine: engine, reader: reader, repository: repository, plan: planValue.Object(), workflowData: workflowData,
		policyBytes: policyBytes, eventData: eventData, workflowSHA: preparedLifecycleWorkflowSHA,
		nextArtifact: 800,
	}
}

func (fixture *preparedLifecycle) newProcessEngine(t *testing.T) *Engine {
	t.Helper()
	value, err := DecodeValue(fixture.policyBytes)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := object(value, "fresh-process recovery policy")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(policy, fixture.repository)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func (fixture *preparedLifecycle) setAttempt(attempt int64, current bool) Object {
	run := recoveryHistoryRun(preparedLifecycleRunID, attempt, preparedLifecycleRunName)
	run["created_at"] = "2026-10-04T12:00:00Z"
	run["head_sha"] = strings.Repeat("b", 40)
	run["run_attempt"] = attempt
	if current {
		run["status"], run["conclusion"] = "in_progress", nil
		for prior := int64(1); prior < attempt; prior++ {
			packet := cloneNativeObject(run)
			packet["run_attempt"], packet["status"], packet["conclusion"] = prior, "completed", "failure"
			fixture.reader.reads[fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d", fixture.repository.FullName(), preparedLifecycleRunID, prior)] = packet
		}
	} else {
		run["status"], run["conclusion"] = "completed", "failure"
	}
	runsEndpoint := fmt.Sprintf("repos/%s/actions/workflows/%s/runs?per_page=100", fixture.repository.FullName(), preparedLifecycleWorkflow)
	fixture.reader.pages[runsEndpoint] = []any{Object{"total_count": int64(1), "workflow_runs": []any{run}}}
	fixture.reader.pages[fmt.Sprintf("repos/%s/actions/artifacts?per_page=100", fixture.repository.FullName())] = []any{Object{
		"total_count": int64(len(fixture.artifacts)), "artifacts": append([]any{}, objectsFrom(fixture.artifacts)...),
	}}
	if !current {
		packet := cloneNativeObject(run)
		packet["run_attempt"] = attempt
		fixture.reader.reads[fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d", fixture.repository.FullName(), preparedLifecycleRunID, attempt)] = packet
	}
	return run
}

func objectsFrom(rows []Object) []any {
	copyRows := make([]any, len(rows))
	for index, row := range rows {
		copyRows[index] = row
	}
	return copyRows
}

func (fixture *preparedLifecycle) addArtifact(t *testing.T, name string, attempt int64, payload []byte) Object {
	t.Helper()
	fixture.nextArtifact++
	metadata := Object{
		"id": fixture.nextArtifact, "name": name, "expired": false,
		"size_in_bytes": len(payload), "digest": "sha256:" + SHA256(payload),
		"workflow_run": Object{"id": preparedLifecycleRunID, "head_sha": strings.Repeat("b", 40)},
	}
	fixture.artifacts = append(fixture.artifacts, metadata)
	fixture.reader.reads[fmt.Sprintf("repos/%s/actions/artifacts/%d", fixture.repository.FullName(), fixture.nextArtifact)] = metadata
	fixture.reader.archives[fixture.nextArtifact] = payload
	_ = attempt // Encoded in the immutable artifact name; retained for readable call sites.
	return metadata
}

func (fixture *preparedLifecycle) invocation(attempt int64, rootTemp string) Invocation {
	return Invocation{
		Workflow: preparedLifecycleWorkflow, RunID: preparedLifecycleRunID, Attempt: attempt,
		RunName: preparedLifecycleRunName, RecoveryKey: preparedLifecycleRecoveryKey,
		PackageRoot: filepath.Join(rootTemp, "package"), RunnerTemp: rootTemp,
	}
}

func realTestPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func (fixture *preparedLifecycle) prepareFirstAttempt(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "events"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := persistPackageFile(root, "events/dispatch-event.json", fixture.eventData); err != nil {
		t.Fatal(err)
	}
	contextValue, err := fixture.engine.InitializeContext(root, Object{
		"workflow_file": preparedLifecycleWorkflow, "repository": fixture.repository.FullName(),
		"recovery_key": preparedLifecycleRecoveryKey, "run_name": preparedLifecycleRunName,
		"run_id": preparedLifecycleRunID, "attempt": int64(1),
		"attempt_target": Object{"plan_sha256": fixture.plan["sha256"], "target": Object{"kind": "issue", "number": int64(17)}, "event_sha256": SHA256(fixture.eventData)},
		"dispatch_steps": []any{"Apply execution"}, "trusted_source_sha": fixture.workflowSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = contextValue
	review := Object{
		"schema_version": int64(1), "workflow_file": preparedLifecycleWorkflow, "repository": fixture.repository.FullName(),
		"workflow_run_id": preparedLifecycleRunID, "workflow_run_attempt": int64(1), "name": "execution",
		"command": "execution-sync", "plan_sha256": fixture.plan["sha256"], "target": Object{"kind": "issue", "number": int64(17)},
		"event_path": "events/dispatch-event.json", "event_sha256": SHA256(fixture.eventData),
	}
	reviewBytes, err := Canonical(review)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPackageFile(root, "reviews/execution.json", reviewBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.RecordContextPlan(root, Object{
		"relative": "plans/execution.json", "name": "execution", "command": "execution-sync",
		"repository": fixture.repository.FullName(), "plan": fixture.plan,
		"review_path": "reviews/execution.json", "review_sha256": SHA256(reviewBytes),
	}); err != nil {
		t.Fatal(err)
	}
}

func (fixture *preparedLifecycle) qualifyCurrent(t *testing.T, attempt int64, invocation Invocation, jobStatus string) Object {
	t.Helper()
	endpoint := fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", fixture.repository.FullName(), preparedLifecycleRunID, attempt)
	fixture.reader.pages[endpoint] = []any{Object{"total_count": int64(1), "jobs": []any{Object{
		"id": int64(1000 + attempt), "run_id": preparedLifecycleRunID, "run_attempt": attempt, "name": "apply", "status": jobStatus,
		"steps": []any{Object{"number": int64(1), "name": "Apply execution", "status": "completed", "conclusion": "skipped"}},
	}}}}
	result, err := fixture.engine.QualifyPrepared(context.Background(), fixture.reader, PreparedQualificationOptions{Invocation: invocation, WorkflowSHA: fixture.workflowSHA})
	if err != nil || result["outcome"] != "qualified" {
		t.Fatalf("attempt %d was not source-qualified: %#v %v", attempt, result, err)
	}
	return result
}

func (fixture *preparedLifecycle) finalizePending(t *testing.T, attempt int64, invocation Invocation, packageRoot string) (Object, []byte) {
	t.Helper()
	payload := recoveryPackageZIP(t, packageRoot)
	normal := fixture.addArtifact(t, RecoveryArtifactName(mustTarget(t, fixture), preparedLifecycleRunID, attempt), attempt, payload)
	result, err := fixture.engine.Finalize(context.Background(), fixture.reader, FinalizeOptions{
		Invocation: invocation, ArtifactID: mustPositive(normal["id"]), ArtifactDigest: fmt.Sprint(normal["digest"]),
		Checkpoint: filepath.Join(invocation.RunnerTemp, fmt.Sprintf("checkpoint-%d", attempt)), WorkflowSHA: fixture.workflowSHA,
	})
	if err != nil || result["outcome"] != "checkpoint" {
		t.Fatalf("pending attempt %d did not produce an immutable checkpoint: %#v %v", attempt, result, err)
	}
	checkpointPayload := recoveryPackageZIP(t, filepath.Dir(fmt.Sprint(result["checkpoint_path"])))
	fixture.addArtifact(t, fmt.Sprint(result["checkpoint_name"]), attempt, checkpointPayload)
	return result, payload
}

func mustTarget(t *testing.T, fixture *preparedLifecycle) Object {
	t.Helper()
	target, err := TargetObject(preparedLifecycleWorkflow, fixture.repository.URL, preparedLifecycleWorkflowID, "workflow-history-v2")
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func (fixture *preparedLifecycle) recoverAndObserve(t *testing.T, attempt int64, rootTemp string) (Invocation, Object) {
	t.Helper()
	fixture.engine = fixture.newProcessEngine(t)
	current := fixture.setAttempt(attempt, true)
	invocation := fixture.invocation(attempt, rootTemp)
	result, err := fixture.engine.Recover(context.Background(), fixture.reader, invocation)
	if err != nil || result["outcome"] != "resumed" {
		t.Fatalf("attempt %d did not resume its exact prepared frontier: %#v %v", attempt, result, err)
	}
	observed, err := fixture.engine.ObserveSourceContext(invocation.PackageRoot, Object{
		"current_run_id": preparedLifecycleRunID, "current_attempt": attempt, "current_run_name": preparedLifecycleRunName,
		"workflow_file": preparedLifecycleWorkflow, "repository": fixture.repository.FullName(), "trusted_source_sha": fixture.workflowSHA,
	})
	if err != nil || !exactInt(observed["recovered_from_run_id"], preparedLifecycleRunID) || !exactInt(observed["recovered_from_attempt"], attempt-1) {
		t.Fatalf("attempt %d changed its direct source lineage: %#v %v", attempt, observed, err)
	}
	applyRoot := filepath.Join(realTestPath(t, rootTemp), "native-apply")
	if err := os.MkdirAll(filepath.Join(applyRoot, ".artifacts", "gh-steward", "journals"), 0700); err != nil {
		t.Fatal(err)
	}
	installed, err := fixture.engine.InstallRestoredJournal(invocation.PackageRoot, filepath.Join(applyRoot, ".artifacts", "gh-steward", "journals"), "execution")
	if err != nil {
		t.Fatalf("attempt %d could not inspect its exact restored journal state: %#v %v", attempt, installed, err)
	}
	source, err := readRecoverySource(invocation.PackageRoot)
	if err != nil {
		t.Fatal(err)
	}
	if source["prepared_qualification"] != nil {
		if installed["outcome"] != "no-journal-required" {
			t.Fatalf("attempt %d fabricated or required absent native progress: %#v", attempt, installed)
		}
	} else if installed["journal_id"] != mustPlanEntry(t, invocation.PackageRoot, "execution")["journal_id"] {
		t.Fatalf("attempt %d did not install its exact positively journaled progress: %#v", attempt, installed)
	}
	_ = current
	return invocation, observed
}

type preparedLifecycleAdapter struct {
	stopAfter  string
	dispatches map[string]int
	observed   []string
}

func (adapter *preparedLifecycleAdapter) Operations(plan contract.Plan) ([]contract.Operation, error) {
	return append([]contract.Operation{}, plan.Operations...), nil
}

func (adapter *preparedLifecycleAdapter) ValidateAcknowledgement(plan contract.Plan, operation contract.Operation, ack contract.Object) error {
	expected := lifecycleAcknowledgement(plan, operation, fmt.Sprint(ack["operation_id"]))
	if !Equal(ack, expected) {
		return errors.New("fake provider acknowledgement differs from exact reviewed operation")
	}
	return nil
}

func (adapter *preparedLifecycleAdapter) ValidateReceipt(plan contract.Plan, operation contract.Operation, result contract.Object) error {
	expected := lifecycleAcknowledgement(plan, operation, fmt.Sprint(result["operation_id"]))
	if !Equal(result["acknowledged"], expected["acknowledged"]) || !Equal(result["after_verified"], true) {
		return errors.New("fake provider read receipt is not positively verified")
	}
	for key, value := range expected {
		if !Equal(result[key], value) {
			return errors.New("fake provider result differs from its exact native acknowledgement")
		}
	}
	return nil
}

func (adapter *preparedLifecycleAdapter) Preflight(context.Context, contract.Plan, []contract.Object) error {
	return nil
}

func (adapter *preparedLifecycleAdapter) Dispatch(context.Context, contract.Plan, contract.Operation, string, []contract.Object) (contract.Object, error) {
	return nil, errors.New("acknowledging adapter must own the dispatch path")
}

func (adapter *preparedLifecycleAdapter) Observe(_ context.Context, plan contract.Plan, operation contract.Operation, operationID string, _ []contract.Object) (contract.Object, contract.Object, error) {
	adapter.observed = append(adapter.observed, operation.ID)
	result := lifecycleAcknowledgement(plan, operation, operationID)
	result["after_verified"] = true
	evidence := contract.Object{"positive_identity": true, "after_state_verified": true, "operation_id": operationID, "reference": "fake-read-only-provider-observation"}
	return result, evidence, nil
}

func (adapter *preparedLifecycleAdapter) DispatchAcknowledged(_ context.Context, plan contract.Plan, operation contract.Operation, operationID string, _ []contract.Object, persist func(contract.Object) error) (contract.Object, error) {
	if adapter.dispatches == nil {
		adapter.dispatches = map[string]int{}
	}
	adapter.dispatches[operation.ID]++
	ack := lifecycleAcknowledgement(plan, operation, operationID)
	if err := persist(ack); err != nil {
		return nil, err
	}
	if operation.ID == adapter.stopAfter {
		return nil, errors.New("intentional interruption after durable native acknowledgement")
	}
	result := contract.Object{}
	for key, value := range ack {
		result[key] = value
	}
	result["after_verified"] = true
	return result, nil
}

func lifecycleAcknowledgement(plan contract.Plan, operation contract.Operation, operationID string) contract.Object {
	return contract.Object{
		"kind": operation.Kind, "primitive_id": operation.ID, "repository": plan.Repository.Object(),
		"operation_id": operationID, "target": operation.Target, "before": operation.Before,
		"after": operation.After, "acknowledged": true, "provider_result": contract.Object{"status": "accepted"},
	}
}

func finishPreparedExecution(t *testing.T, fixture *preparedLifecycle, invocation Invocation, runTemp string, adapter *preparedLifecycleAdapter) Object {
	t.Helper()
	plan, err := fixture.engine.loadContextPlan(invocation.PackageRoot, mustRunContext(t, invocation.PackageRoot), mustPlanEntry(t, invocation.PackageRoot, "execution"))
	if err != nil {
		t.Fatal(err)
	}
	planParsed, err := contract.ParsePlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.MarkContextPlan(invocation.PackageRoot, "execution", "dispatching"); err != nil {
		t.Fatal(err)
	}
	applyRoot := filepath.Join(realTestPath(t, runTemp), "native-apply")
	result, err := (apply.Engine{Root: applyRoot, Repository: fixture.repository, Command: "execution-sync", Adapter: adapter}).Apply(context.Background(), planParsed.Object())
	if err != nil {
		t.Fatalf("actual native apply engine did not resume its exact plan: %v", err)
	}
	journalRoot := filepath.Join(applyRoot, ".artifacts", "gh-steward", "journals")
	if _, err := fixture.engine.CaptureJournal(invocation.PackageRoot, journalRoot, "execution"); err != nil {
		t.Fatal(err)
	}
	if err := persistNativeApplyResult(invocation.PackageRoot, "execution", plan, result); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.MarkContextPlan(invocation.PackageRoot, "execution", "completed"); err != nil {
		t.Fatal(err)
	}
	return result
}

func mustRunContext(t *testing.T, root string) Object {
	t.Helper()
	value, err := LoadJSON(filepath.Join(root, "run-context.json"))
	if err != nil {
		t.Fatal(err)
	}
	return value.(Object)
}

func mustPlanEntry(t *testing.T, root, name string) Object {
	t.Helper()
	entry, err := contextPlanByName(mustRunContext(t, root), name)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func finalizeCompletedAttempt(t *testing.T, fixture *preparedLifecycle, attempt int64, invocation Invocation) (Object, []byte) {
	t.Helper()
	payload := recoveryPackageZIP(t, invocation.PackageRoot)
	metadata := fixture.addArtifact(t, RecoveryArtifactName(mustTarget(t, fixture), preparedLifecycleRunID, attempt), attempt, payload)
	result, err := fixture.engine.Finalize(context.Background(), fixture.reader, FinalizeOptions{
		Invocation: invocation, ArtifactID: mustPositive(metadata["id"]), ArtifactDigest: fmt.Sprint(metadata["digest"]),
		Checkpoint: filepath.Join(invocation.RunnerTemp, fmt.Sprintf("terminal-checkpoint-%d", attempt)), WorkflowSHA: fixture.workflowSHA,
	})
	if err != nil || result["outcome"] != "checkpoint" {
		t.Fatalf("completed native execution did not create a terminal checkpoint: %#v %v", result, err)
	}
	checkpointPayload := recoveryPackageZIP(t, filepath.Dir(fmt.Sprint(result["checkpoint_path"])))
	fixture.addArtifact(t, fmt.Sprint(result["checkpoint_name"]), attempt, checkpointPayload)
	return result, checkpointPayload
}

func TestPreparedFrontierRepeatedNoDispatchAndExactTerminalResume(t *testing.T) {
	fixture := newPreparedLifecycle(t, 1)
	rootTemp1 := t.TempDir()
	fixture.setAttempt(1, true)
	inv1 := fixture.invocation(1, rootTemp1)
	if result, err := fixture.engine.Recover(context.Background(), fixture.reader, inv1); err != nil || result["outcome"] != "fresh" {
		t.Fatalf("initial source attempt did not start fresh: %#v %v", result, err)
	}
	fixture.prepareFirstAttempt(t, inv1.PackageRoot)
	if _, err := fixture.engine.MarkContextPlan(inv1.PackageRoot, "execution", "dispatching"); err != nil {
		t.Fatal(err)
	}
	fixture.qualifyCurrent(t, 1, inv1, "in_progress")
	_, firstPayload := fixture.finalizePending(t, 1, inv1, inv1.PackageRoot)
	if len(fixture.artifacts) != 2 || fixture.artifacts[0]["name"] != RecoveryArtifactName(mustTarget(t, fixture), preparedLifecycleRunID, 1) {
		t.Fatal("first pending source did not retain one normal upload and one frontier checkpoint")
	}

	rootTemp2 := t.TempDir()
	fixture.setAttempt(2, true)
	inv2, observed2 := fixture.recoverAndObserve(t, 2, rootTemp2)
	if observed2["recovered_from_attempt"] != int64(1) {
		t.Fatal("first observer did not preserve the original source attempt")
	}
	if _, err := fixture.engine.MarkContextPlan(inv2.PackageRoot, "execution", "dispatching"); err != nil {
		t.Fatal(err)
	}
	fixture.qualifyCurrent(t, 2, inv2, "in_progress")
	_, secondPayload := fixture.finalizePending(t, 2, inv2, inv2.PackageRoot)
	if len(fixture.artifacts) != 4 || bytes.Equal(firstPayload, secondPayload) {
		t.Fatal("repeated no-dispatch attempt did not upload its own independently bound evidence")
	}

	rootTemp3 := t.TempDir()
	fixture.setAttempt(3, true)
	inv3, observed3 := fixture.recoverAndObserve(t, 3, rootTemp3)
	if observed3["recovered_from_attempt"] != int64(2) {
		t.Fatal("second observer was relabeled to point directly to the original attempt")
	}
	adapter := &preparedLifecycleAdapter{dispatches: map[string]int{}}
	finishPreparedExecution(t, fixture, inv3, rootTemp3, adapter)
	if adapter.dispatches["execution:comment-1"] != 1 {
		t.Fatalf("terminal resume did not make exactly one native dispatch: %#v", adapter.dispatches)
	}
	_, terminalPayload := finalizeCompletedAttempt(t, fixture, 3, inv3)
	closed, err := CheckpointFromArchive(terminalPayload)
	if err != nil {
		t.Fatal(err)
	}
	closedChain := closed
	if len(closedChain["prepared_frontier"].([]any)) != 0 || len(closedChain["prepared_terminal_proofs"].([]any)) != 1 {
		t.Fatalf("actual terminal receipts did not close the exact prepared frontier: %#v", closedChain)
	}
	settlements := closedChain["settlements"].([]any)
	if len(settlements) != 3 || !exactInt(settlements[0].(Object)["attempt"], 1) || !exactInt(settlements[1].(Object)["attempt"], 2) || !exactInt(settlements[2].(Object)["attempt"], 3) {
		t.Fatalf("terminal closure did not preserve each original attempt chronologically: %#v", settlements)
	}
	for index := 0; index < 2; index++ {
		settlement := settlements[index].(Object)["settlement"].(Object)
		if settlement["kind"] != "prepared_terminal" {
			t.Fatalf("prepared source attempt %d was represented as an invented execution terminal: %#v", index+1, settlement)
		}
	}

	rootTemp4 := t.TempDir()
	fixture.setAttempt(4, true)
	fixture.engine = fixture.newProcessEngine(t)
	inv4 := fixture.invocation(4, rootTemp4)
	result, err := fixture.engine.Recover(context.Background(), fixture.reader, inv4)
	if err != nil || result["outcome"] != "terminal" {
		t.Fatalf("newer closed sidecar did not win alongside older open sidecars: %#v %v", result, err)
	}
	selected, err := fixture.engine.SelectCheckpoint(
		checkpointChains(t, fixture), mustTarget(t, fixture),
		mustObserved(t, fixture.engine, fixture.reader, inv4), map[int64]int64{preparedLifecycleRunID: 4},
	)
	if err != nil || len(selected["prepared_frontier"].([]any)) != 0 || len(selected["settlements"].([]any)) != 3 {
		t.Fatalf("checkpoint selection did not choose the exact longest closed history: %#v %v", selected, err)
	}
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestPreparedFrontierRejectsChangedMissingAndNullPlanOrigins(t *testing.T) {
	fixture := newPreparedLifecycle(t, 1)
	rootTemp1 := t.TempDir()
	fixture.setAttempt(1, true)
	inv1 := fixture.invocation(1, rootTemp1)
	if result, err := fixture.engine.Recover(context.Background(), fixture.reader, inv1); err != nil || result["outcome"] != "fresh" {
		t.Fatalf("initial source attempt did not start fresh: %#v %v", result, err)
	}
	fixture.prepareFirstAttempt(t, inv1.PackageRoot)
	if _, err := fixture.engine.MarkContextPlan(inv1.PackageRoot, "execution", "dispatching"); err != nil {
		t.Fatal(err)
	}
	fixture.qualifyCurrent(t, 1, inv1, "in_progress")
	fixture.finalizePending(t, 1, inv1, inv1.PackageRoot)

	rootTemp2 := t.TempDir()
	fixture.setAttempt(2, true)
	fixture.engine = fixture.newProcessEngine(t)
	inv2, _ := fixture.recoverAndObserve(t, 2, rootTemp2)
	if _, err := fixture.engine.MarkContextPlan(inv2.PackageRoot, "execution", "dispatching"); err != nil {
		t.Fatal(err)
	}
	fixture.qualifyCurrent(t, 2, inv2, "in_progress")
	fixture.finalizePending(t, 2, inv2, inv2.PackageRoot)
	checkpoint := fixture.artifacts[len(fixture.artifacts)-1]
	chain, err := CheckpointFromArchive(fixture.reader.archives[mustPositive(checkpoint["id"])])
	if err != nil {
		t.Fatal(err)
	}
	if len(chain["prepared_frontier"].([]any)) != 2 {
		t.Fatal("fixture did not retain both source attempts in its prepared frontier")
	}
	fixture.setAttempt(3, true)
	inv3 := fixture.invocation(3, t.TempDir())
	_, _, target, observed, latest, err := fixture.engine.invocationHistory(context.Background(), fixture.reader, inv3)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		change func(Object)
	}{
		{name: "cross-run origin", change: func(context Object) { context["plan_origin_run_id"] = int64(999) }},
		{name: "missing origin member", change: func(context Object) { delete(context, "plan_origin_attempt") }},
		{name: "explicit null origin", change: func(context Object) {
			context["plan_origin_run_id"], context["plan_origin_attempt"] = nil, nil
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutated, err := cloneObject(chain)
			if err != nil {
				t.Fatal(err)
			}
			frontier := mutated["prepared_frontier"].([]any)
			entry := frontier[1].(Object)
			source := entry["source"].(Object)
			contextValue, err := LoadFileProof(source["context_file"], "prepared source context")
			if err != nil {
				t.Fatal(err)
			}
			context, err := object(contextValue, "prepared source context")
			if err != nil {
				t.Fatal(err)
			}
			mutation.change(context)
			contextBytes, err := Canonical(context)
			if err != nil {
				t.Fatal(err)
			}
			source["context_file"] = MakeFileProof(contextBytes)
			mutated, err = resealPreparedChain(mutated)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.engine.ValidateChain(mutated, target, observed, latest); err == nil {
				t.Fatalf("prepared frontier accepted %s", mutation.name)
			}
		})
	}
}

func TestPreparedFrontierPreservesCompletedMergeParentReceipts(t *testing.T) {
	repository := policyTestRepository(t)
	policy, err := cloneObject(completedMergePolicy())
	if err != nil {
		t.Fatal(err)
	}
	workflowPolicy := policy["workflows"].(Object)["merge-on-green.yml"].(Object)
	branchCleanupPolicy := workflowPolicy["plans"].(Object)["branch-cleanup"].(Object)
	workflowData := []byte("name: Merge On Green\non:\n  workflow_run:\njobs:\n  branch-cleanup:\n    steps:\n      - name: Apply exact branch cleanup\n        run: true\n")
	workflowDigest := SHA256(workflowData)
	workflowSHA := strings.Repeat("c", 40)
	branchCleanupPolicy["prepared_recovery"] = Object{
		"workflow_source_sha256": workflowDigest,
		"mutators":               []any{Object{"job": "branch-cleanup", "steps": []any{"Apply exact branch cleanup"}}},
	}
	engine, err := NewEngine(policy, repository)
	if err != nil {
		t.Fatal(err)
	}
	temp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := filepath.Join(temp, "source-package")
	if err := os.Mkdir(sourceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	parentJournalRoot := filepath.Join(temp, "parent-journal")
	if err := os.Mkdir(parentJournalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	target, err := TargetObject("merge-on-green.yml", repository.URL, 9, "workflow-history-v2")
	if err != nil {
		t.Fatal(err)
	}
	workflowPacket := workflowPacketForTest("merge-on-green.yml", workflowData)
	reader := &recoveryReaderFixture{reads: map[string]Object{
		fmt.Sprintf("repos/%s/contents/.github/workflows/merge-on-green.yml?ref=%s", repository.FullName(), workflowSHA): workflowPacket,
	}, pages: map[string][]any{}, archives: map[int64][]byte{}}
	sourceRun := recoveryHistoryRun(100, 1, "Merge On Green")
	sourceRun["run_attempt"], sourceRun["status"], sourceRun["conclusion"] = int64(1), "in_progress", nil
	workflowRunsEndpoint := fmt.Sprintf("repos/%s/actions/workflows/merge-on-green.yml/runs?per_page=100", repository.FullName())
	artifactEndpoint := fmt.Sprintf("repos/%s/actions/artifacts?per_page=100", repository.FullName())
	reader.pages[workflowRunsEndpoint] = []any{Object{"total_count": int64(1), "workflow_runs": []any{sourceRun}}}
	reader.pages[artifactEndpoint] = []any{Object{"total_count": int64(0), "artifacts": []any{}}}
	reader.reads["repos/example/widgets/actions/runs/100/attempts/1"] = sourceRun
	invocation1 := Invocation{
		Workflow: "merge-on-green.yml", RunID: 100, Attempt: 1, RunName: "Merge On Green", RecoveryKey: "merge-ci-8-pr-3",
		PackageRoot: sourceRoot, RunnerTemp: temp,
	}
	if result, err := engine.Recover(context.Background(), reader, invocation1); err != nil || result["outcome"] != "fresh" {
		t.Fatalf("initial composed source attempt did not start fresh: %#v %v", result, err)
	}
	_, mergeInventory := composedMergeSource(repository)
	provider := &composedCleanupProvider{repository: repository, mergeInventory: mergeInventory}
	parentPlan, parentEntry, parentPlanBytes, parentJournalBytes, parentResultBytes := prepareComposedParent(t, engine, sourceRoot, parentJournalRoot, provider)
	prepareComposedCleanup(t, engine, sourceRoot, provider, repository)
	runContext, err := engine.readRunContext(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	runContext["trusted_source_sha"] = workflowSHA
	if err := persistPackageJSON(sourceRoot, "run-context.json", runContext); err != nil {
		t.Fatal(err)
	}
	if err := engine.validateRunContext(runContext); err != nil {
		t.Fatalf("completed merge plus prepared cleanup context is invalid: %v", err)
	}
	sourceContextBytes, err := ReadPackageFile(sourceRoot, "run-context.json")
	if err != nil {
		t.Fatal(err)
	}
	if runContext["phase"] != "prepared" || runContext["plans"].([]any)[0].(Object)["status"] != "completed" || runContext["plans"].([]any)[1].(Object)["status"] != "prepared" {
		t.Fatalf("fixture is not the exact completed-parent/prepared-cleanup phase: %#v", runContext)
	}
	if _, _, err := engine.preparedPlanProofs(sourceRoot, runContext); err != nil {
		t.Fatalf("completed-parent/prepared-cleanup plan proof setup is invalid: %v", err)
	}

	reader.pages[fmt.Sprintf("repos/%s/actions/runs/100/attempts/1/jobs?per_page=100", repository.FullName())] = []any{Object{
		"total_count": int64(1), "jobs": []any{Object{
			"id": int64(400), "run_id": int64(100), "run_attempt": int64(1), "name": "branch-cleanup", "status": "in_progress",
			"steps": []any{Object{"number": int64(1), "name": "Apply exact branch cleanup", "status": "completed", "conclusion": "skipped"}},
		}},
	}}
	if _, err := engine.QualifyPrepared(context.Background(), reader, PreparedQualificationOptions{Invocation: invocation1, WorkflowSHA: workflowSHA}); err != nil {
		t.Fatalf("composed completed-parent/prepared-cleanup source was not qualified: %v", err)
	}
	preparedPayload := recoveryPackageZIP(t, sourceRoot)
	addComposedRecoveryArtifact(t, reader, artifactEndpoint, 500, RecoveryArtifactName(target, 100, 1), preparedPayload, sourceRun)
	pending, err := engine.Finalize(context.Background(), reader, FinalizeOptions{
		Invocation: invocation1, ArtifactID: 500, ArtifactDigest: "sha256:" + SHA256(preparedPayload),
		Checkpoint: filepath.Join(temp, "prepared-checkpoint"), WorkflowSHA: workflowSHA,
	})
	if err != nil || pending["outcome"] != "checkpoint" {
		t.Fatalf("qualified composed prepared attempt did not create a frontier checkpoint: %#v %v", pending, err)
	}
	preparedCheckpoint := recoveryPackageZIP(t, filepath.Dir(fmt.Sprint(pending["checkpoint_path"])))
	addComposedRecoveryArtifact(t, reader, artifactEndpoint, 501, fmt.Sprint(pending["checkpoint_name"]), preparedCheckpoint, sourceRun)
	preparedChain, err := CheckpointFromArchive(preparedCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	frontier := preparedChain["prepared_frontier"].([]any)
	preparedSource := frontier[len(frontier)-1].(Object)["source"].(Object)
	if !Equal(preparedSource["context_file"], MakeFileProof(sourceContextBytes)) {
		t.Fatal("prepared frontier changed the exact original completed-parent source-context bytes")
	}
	if !Equal(parentPlan["sha256"], parentEntry["sha256"]) {
		t.Fatal("source parent merge plan identity changed during prepared qualification")
	}

	currentRun := recoveryHistoryRun(100, 2, "Merge On Green")
	currentRun["run_attempt"], currentRun["status"], currentRun["conclusion"] = int64(2), "in_progress", nil
	reader.pages[workflowRunsEndpoint] = []any{Object{"total_count": int64(1), "workflow_runs": []any{currentRun}}}
	failedSourceRun := cloneNativeObject(sourceRun)
	failedSourceRun["run_attempt"], failedSourceRun["status"], failedSourceRun["conclusion"] = int64(1), "completed", "failure"
	reader.reads["repos/example/widgets/actions/runs/100/attempts/1"] = failedSourceRun
	observerTemp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	observerRoot := filepath.Join(observerTemp, "observer-package")
	invocation2 := Invocation{
		Workflow: "merge-on-green.yml", RunID: 100, Attempt: 2, RunName: "Merge On Green", RecoveryKey: "merge-ci-8-pr-3",
		PackageRoot: observerRoot, RunnerTemp: observerTemp,
	}
	engine, err = NewEngine(policy, repository)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := engine.Recover(context.Background(), reader, invocation2)
	if err != nil || recovered["outcome"] != "resumed" {
		t.Fatalf("fresh-process observer did not restore the exact prepared source: %#v %v", recovered, err)
	}
	retainedSource, err := readRecoverySource(observerRoot)
	if err != nil {
		t.Fatal(err)
	}
	retainedContextBytes, err := LoadRawFileProof(retainedSource["context_file"], "retained original source context")
	if err != nil || !bytes.Equal(retainedContextBytes, sourceContextBytes) ||
		!Equal(retainedSource["context_file"], preparedSource["context_file"]) {
		t.Fatalf("observer changed the raw original source-context bytes: %v", err)
	}
	if _, err := engine.ObserveSourceContext(observerRoot, Object{
		"current_run_id": int64(100), "current_attempt": int64(2), "current_run_name": "Merge On Green",
		"workflow_file": "merge-on-green.yml", "repository": repository.FullName(), "trusted_source_sha": workflowSHA,
	}); err != nil {
		t.Fatalf("prepared source observer context failed: %v", err)
	}
	for relative, want := range map[string][]byte{
		"plans/merge.json": parentPlanBytes, fmt.Sprintf("journal/%s.json", parentEntry["journal_id"]): parentJournalBytes,
		"apply-results/merge.json": parentResultBytes,
	} {
		got, err := ReadPackageFile(observerRoot, relative)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("prepared recovery changed completed parent evidence at %s: %v", relative, err)
		}
	}

	journalRoot := filepath.Join(observerTemp, ".artifacts", "gh-steward", "journals")
	if err := os.MkdirAll(journalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if installed, err := engine.InstallRestoredJournal(observerRoot, journalRoot, "branch-cleanup"); err != nil || installed["outcome"] != "no-journal-required" {
		t.Fatalf("prepared cleanup observer invented or required native progress: %#v %v", installed, err)
	}
	if _, err := engine.MarkContextPlan(observerRoot, "branch-cleanup", "dispatching"); err != nil {
		t.Fatal(err)
	}
	cleanupEntry := mustPlanEntry(t, observerRoot, "branch-cleanup")
	cleanupPlanValue, err := engine.loadContextPlan(observerRoot, mustRunContext(t, observerRoot), cleanupEntry)
	if err != nil {
		t.Fatal(err)
	}
	cleanupPlan, err := contract.ParsePlan(cleanupPlanValue)
	if err != nil {
		t.Fatal(err)
	}
	applyRoot := filepath.Join(observerTemp, "cleanup-apply")
	if err := os.MkdirAll(applyRoot, 0700); err != nil {
		t.Fatal(err)
	}
	cleanupResult, err := (apply.Engine{
		Root: applyRoot, Repository: repository, Command: workflow.BranchCleanupCommand,
		Adapter: workflow.BranchCleanup{Provider: provider},
	}).Apply(context.Background(), cleanupPlan.Object())
	if err != nil || cleanupResult["status"] != "completed" || provider.branchDeleteWrites != 1 {
		t.Fatalf("fake native cleanup did not complete exactly once: %#v %v writes=%d", cleanupResult, err, provider.branchDeleteWrites)
	}
	if _, err := engine.CaptureJournal(observerRoot, filepath.Join(applyRoot, ".artifacts", "gh-steward", "journals"), "branch-cleanup"); err != nil {
		t.Fatal(err)
	}
	if err := persistNativeApplyResult(observerRoot, "branch-cleanup", cleanupPlan.Object(), cleanupResult); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.MarkContextPlan(observerRoot, "branch-cleanup", "completed"); err != nil {
		t.Fatal(err)
	}
	for relative, want := range map[string][]byte{
		"plans/merge.json": parentPlanBytes, fmt.Sprintf("journal/%s.json", parentEntry["journal_id"]): parentJournalBytes,
		"apply-results/merge.json": parentResultBytes,
	} {
		got, err := ReadPackageFile(observerRoot, relative)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("terminal cleanup changed completed parent evidence at %s: %v", relative, err)
		}
	}
	terminalPayload := recoveryPackageZIP(t, observerRoot)
	currentRun["status"], currentRun["conclusion"] = "in_progress", nil
	reader.pages[fmt.Sprintf("repos/%s/actions/runs/100/attempts/2/jobs?per_page=100", repository.FullName())] = []any{Object{"total_count": int64(1), "jobs": []any{Object{
		"id": int64(401), "run_id": int64(100), "run_attempt": int64(2), "name": "branch-cleanup", "status": "in_progress",
		"steps": []any{Object{"number": int64(1), "name": "Apply exact branch cleanup", "status": "completed", "conclusion": "success"}},
	}}}}
	addComposedRecoveryArtifact(t, reader, artifactEndpoint, 502, RecoveryArtifactName(target, 100, 2), terminalPayload, currentRun)
	terminal, err := engine.Finalize(context.Background(), reader, FinalizeOptions{
		Invocation: invocation2, ArtifactID: 502, ArtifactDigest: "sha256:" + SHA256(terminalPayload),
		Checkpoint: filepath.Join(observerTemp, "terminal-checkpoint"), WorkflowSHA: workflowSHA,
	})
	if err != nil || terminal["outcome"] != "checkpoint" {
		t.Fatalf("actual native cleanup receipts did not close the prepared frontier: %#v %v", terminal, err)
	}
	closed, err := CheckpointFromArchive(recoveryPackageZIP(t, filepath.Dir(fmt.Sprint(terminal["checkpoint_path"]))))
	if err != nil {
		t.Fatal(err)
	}
	settlements := closed["settlements"].([]any)
	observerPlanProofs := settlements[len(settlements)-1].(Object)["settlement"].(Object)["plans"].([]any)
	var retainedParent Object
	for _, raw := range observerPlanProofs {
		plan, _ := object(raw, "terminal composed plan")
		if plan["name"] == "merge" {
			retainedParent = plan
		}
	}
	if retainedParent == nil {
		t.Fatal("terminal observer omitted its completed parent merge receipts")
	}
	for field, want := range map[string][]byte{
		"plan_file": parentPlanBytes, "journal_file": parentJournalBytes, "apply_result_file": parentResultBytes,
	} {
		got, err := LoadRawFileProof(retainedParent[field], "terminal completed parent receipt")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("terminal chain changed completed parent %s bytes: %v", field, err)
		}
	}
}

func workflowPacketForTest(workflowFile string, raw []byte) Object {
	gitBlob := sha1.New()
	fmt.Fprintf(gitBlob, "blob %d%c", len(raw), 0)
	_, _ = gitBlob.Write(raw)
	return Object{
		"type": "file", "path": ".github/workflows/" + workflowFile, "encoding": "base64",
		"content": base64.StdEncoding.EncodeToString(raw), "sha": hex.EncodeToString(gitBlob.Sum(nil)),
	}
}

func addComposedRecoveryArtifact(t *testing.T, reader *recoveryReaderFixture, endpoint string, id int64, name string, payload []byte, run Object) Object {
	t.Helper()
	metadata := Object{
		"id": id, "name": name, "expired": false, "size_in_bytes": len(payload), "digest": "sha256:" + SHA256(payload),
		"workflow_run": Object{"id": run["id"], "head_sha": run["head_sha"]},
	}
	reader.reads[fmt.Sprintf("repos/example/widgets/actions/artifacts/%d", id)] = metadata
	reader.archives[id] = payload
	existing := []any{}
	if pages := reader.pages[endpoint]; len(pages) > 0 {
		page, _ := object(pages[0], "composed artifact fixture page")
		rows, _ := array(page["artifacts"], "composed artifact fixture rows")
		existing = append(existing, rows...)
	}
	existing = append(existing, metadata)
	reader.pages[endpoint] = []any{Object{"total_count": int64(len(existing)), "artifacts": existing}}
	return metadata
}

func checkpointChains(t *testing.T, fixture *preparedLifecycle) []Object {
	t.Helper()
	chains := []Object{}
	for _, artifact := range fixture.artifacts {
		if !strings.HasSuffix(fmt.Sprint(artifact["name"]), "-checkpoint-00") {
			continue
		}
		payload := fixture.reader.archives[mustPositive(artifact["id"])]
		chain, err := CheckpointFromArchive(payload)
		if err != nil {
			t.Fatal(err)
		}
		chains = append(chains, chain)
	}
	return chains
}

func mustObserved(t *testing.T, engine *Engine, reader *recoveryReaderFixture, invocation Invocation) []Object {
	t.Helper()
	_, _, _, observed, _, err := engine.invocationHistory(context.Background(), reader, invocation)
	if err != nil {
		t.Fatal(err)
	}
	return observed
}

func TestPreparedFrontierPartialAcknowledgementResumesWithoutRedispatch(t *testing.T) {
	fixture := newPreparedLifecycle(t, 2)
	rootTemp1 := t.TempDir()
	inv1 := fixture.invocation(1, rootTemp1)
	fixture.setAttempt(1, true)
	if result, err := fixture.engine.Recover(context.Background(), fixture.reader, inv1); err != nil || result["outcome"] != "fresh" {
		t.Fatalf("initial source attempt did not start fresh: %#v %v", result, err)
	}
	fixture.prepareFirstAttempt(t, inv1.PackageRoot)
	if _, err := fixture.engine.MarkContextPlan(inv1.PackageRoot, "execution", "dispatching"); err != nil {
		t.Fatal(err)
	}
	fixture.qualifyCurrent(t, 1, inv1, "in_progress")
	fixture.finalizePending(t, 1, inv1, inv1.PackageRoot)

	rootTemp2 := t.TempDir()
	fixture.setAttempt(2, true)
	inv2, _ := fixture.recoverAndObserve(t, 2, rootTemp2)
	plan := mustRunContext(t, inv2.PackageRoot)
	entry := mustPlanEntry(t, inv2.PackageRoot, "execution")
	planValue, err := fixture.engine.loadContextPlan(inv2.PackageRoot, plan, entry)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := contract.ParsePlan(planValue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.MarkContextPlan(inv2.PackageRoot, "execution", "dispatching"); err != nil {
		t.Fatal(err)
	}
	firstID, secondID := parsed.Operations[0].ID, parsed.Operations[1].ID
	adapter := &preparedLifecycleAdapter{stopAfter: secondID, dispatches: map[string]int{}}
	applyRoot := filepath.Join(realTestPath(t, rootTemp2), "native-apply")
	if _, err := (apply.Engine{Root: applyRoot, Repository: fixture.repository, Command: "execution-sync", Adapter: adapter}).Apply(context.Background(), parsed.Object()); err == nil {
		t.Fatal("intentional observer interruption was reported as terminal")
	}
	if adapter.dispatches[firstID] != 1 || adapter.dispatches[secondID] != 1 {
		t.Fatalf("interrupted engine did not persist exactly the expected two dispatches: %#v", adapter.dispatches)
	}
	journalRoot := filepath.Join(applyRoot, ".artifacts", "gh-steward", "journals")
	if _, err := fixture.engine.CaptureJournal(inv2.PackageRoot, journalRoot, "execution"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.QualifyPrepared(context.Background(), fixture.reader, PreparedQualificationOptions{Invocation: inv2, WorkflowSHA: fixture.workflowSHA}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(inv2.PackageRoot, preparedQualificationPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("positive native journal progress was reclassified as a prepared no-dispatch source")
	}
	fixture.finalizePending(t, 2, inv2, inv2.PackageRoot)

	rootTemp3 := t.TempDir()
	fixture.setAttempt(3, true)
	inv3, observed := fixture.recoverAndObserve(t, 3, rootTemp3)
	if observed["recovered_from_attempt"] != int64(2) {
		t.Fatal("journaled observer source did not retain direct intermediate lineage")
	}
	installRoot := filepath.Join(realTestPath(t, rootTemp3), "native-apply")
	journalRoot = filepath.Join(installRoot, ".artifacts", "gh-steward", "journals")
	if err := os.MkdirAll(journalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	installed, err := fixture.engine.InstallRestoredJournal(inv3.PackageRoot, journalRoot, "execution")
	if err != nil || installed["journal_id"] != mustPlanEntry(t, inv3.PackageRoot, "execution")["journal_id"] {
		t.Fatalf("fresh observer process did not install the exact positive source journal: %#v %v", installed, err)
	}
	if _, err := fixture.engine.MarkContextPlan(inv3.PackageRoot, "execution", "dispatching"); err != nil {
		t.Fatal(err)
	}
	resumeAdapter := &preparedLifecycleAdapter{dispatches: map[string]int{}}
	resumedResult, err := (apply.Engine{Root: installRoot, Repository: fixture.repository, Command: "execution-sync", Adapter: resumeAdapter}).Apply(context.Background(), parsed.Object())
	if err != nil {
		t.Fatalf("actual apply engine did not reconcile and resume exact partial progress: %v", err)
	}
	if resumeAdapter.dispatches[firstID] != 0 || resumeAdapter.dispatches[secondID] != 0 || len(resumeAdapter.observed) != 1 || resumeAdapter.observed[0] != secondID {
		t.Fatalf("fresh observer redispatched saved work or failed to observe the exact unknown identity: dispatches=%#v observed=%#v", resumeAdapter.dispatches, resumeAdapter.observed)
	}
	if _, err := fixture.engine.CaptureJournal(inv3.PackageRoot, filepath.Join(installRoot, ".artifacts", "gh-steward", "journals"), "execution"); err != nil {
		t.Fatal(err)
	}
	if err := persistNativeApplyResult(inv3.PackageRoot, "execution", parsed.Object(), resumedResult); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.engine.MarkContextPlan(inv3.PackageRoot, "execution", "completed"); err != nil {
		t.Fatal(err)
	}
	_, terminalPayload := finalizeCompletedAttempt(t, fixture, 3, inv3)
	terminalChain, err := CheckpointFromArchive(terminalPayload)
	if err != nil {
		t.Fatal(err)
	}
	sources := terminalChain["prepared_terminal_proofs"].([]any)[0].(Object)["sources"].([]any)
	journaled := sources[1].(Object)["source"].(Object)["plans"].([]any)[0].(Object)["journal_file"]
	originalSourceJournal, err := LoadRawFileProof(journaled, "partial source journal")
	if err != nil {
		t.Fatal(err)
	}
	observerPlan := terminalChain["settlements"].([]any)[2].(Object)["settlement"].(Object)["plans"].([]any)[0].(Object)
	observerJournal, err := LoadRawFileProof(observerPlan["journal_file"], "terminal observer journal")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(originalSourceJournal, observerJournal) {
		t.Fatal("terminal observer did not preserve its appended observation as a new receipt while retaining source bytes")
	}
}
