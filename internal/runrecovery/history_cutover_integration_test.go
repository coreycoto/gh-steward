package runrecovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func historyCutoverNoopOptions(t *testing.T, engine *Engine, reader *recoveryReaderFixture, baseline Object, temp string, runID, attempt int64) NoopOptions {
	t.Helper()
	invocation := Invocation{Workflow: "task.yml", RunID: runID, Attempt: attempt, RunName: "Current", RecoveryKey: "task", PackageRoot: filepath.Join(temp, "invocation"), RunnerTemp: temp}
	result, err := engine.RecoverWithHistoryCutover(context.Background(), reader, invocation, baseline)
	if err != nil || (result["outcome"] != "fresh" && result["outcome"] != "terminal") {
		t.Fatalf("reviewed cutover recovery did not produce a safe no-op observation: %#v %v", result, err)
	}
	workflowSHA := strings.Repeat("b", 40)
	event := Object{"repository": Object{"full_name": "example/widgets", "html_url": "https://github.com/example/widgets"}, "inputs": Object{}}
	if err := persistPackageJSON(invocation.PackageRoot, "events/trigger-event.json", event); err != nil {
		t.Fatal(err)
	}
	eventBytes, err := ReadPackageFile(invocation.PackageRoot, "events/trigger-event.json")
	if err != nil {
		t.Fatal(err)
	}
	attemptTarget := Object{"event_sha256": SHA256(eventBytes)}
	if _, err := engine.InitializeContext(invocation.PackageRoot, Object{"workflow_file": "task.yml", "repository": "example/widgets", "recovery_key": "task", "run_name": "Current", "run_id": runID, "attempt": attempt, "attempt_target": attemptTarget, "dispatch_steps": []any{}, "trusted_source_sha": workflowSHA}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SetContextPhase(invocation.PackageRoot, "prepared", ""); err != nil {
		t.Fatal(err)
	}
	if err := persistPackageJSON(invocation.PackageRoot, noopDecisionPath, Object{
		"schema_version": 1, "decision": "no-change", "repository": "example/widgets", "server_url": "https://github.com",
		"workflow_file": "task.yml", "run_id": runID, "attempt": attempt, "recovery_key": "task", "attempt_target": attemptTarget,
		"workflow_sha": workflowSHA, "event_name": "workflow_dispatch", "event_sha256": SHA256(eventBytes),
	}); err != nil {
		t.Fatal(err)
	}
	return NoopOptions{Invocation: invocation, WorkflowSHA: workflowSHA, ToolVersion: "test"}
}

func addHistoryCutoverReview(t *testing.T, engine *Engine, baseline Object) string {
	return addHistoryCutoverReviewForWorkflow(t, engine, "task.yml", baseline)
}

func addHistoryCutoverReviewForWorkflow(t *testing.T, engine *Engine, workflow string, baseline Object) string {
	t.Helper()
	digest, err := HistoryCutoverDigest(baseline)
	if err != nil {
		t.Fatal(err)
	}
	policy, ok := engine.workflows[workflow]
	if !ok {
		t.Fatalf("fixture engine has no %s workflow policy", workflow)
	}
	policy.historyCutoverReviews[digest] = true
	engine.workflows[workflow] = policy
	return digest
}

func TestHistoryCutoverNoopCheckpointRetainsBoundaryAcrossFreshProcess(t *testing.T) {
	legacy := recoveryHistoryRun(5, 1, "Legacy quarantined run")
	legacyAttempt := historyCutoverAttemptResponse(legacy, 1, legacy["created_at"].(string))
	baseline := captureHistoryCutoverFixture(t, historyCutoverFixture([]Object{legacy}, nil, map[string]Object{
		historyCutoverAttemptEndpoint(5, 1): legacyAttempt,
	}, nil))
	variant := cloneHistoryCutoverObject(t, baseline)
	variant["state_reads"] = []any{Object{
		"endpoint": "repos/example/widgets/issues/1",
		"object":   Object{"number": int64(1), "state": "open", "body": "reviewed point-in-time state"},
	}}
	variant = resealHistoryCutoverFixture(t, variant)
	variantDigest, err := HistoryCutoverDigest(variant)
	if err != nil || variantDigest == baseline["sha256"] || !Equal(variant["run_inventory"], baseline["run_inventory"]) || !Equal(variant["attempts"], baseline["attempts"]) {
		t.Fatalf("cutover digest variant changed legacy identities or attempts: %#v %v", variant, err)
	}
	digest := fmt.Sprint(baseline["sha256"])
	engine := noopEngine(t)
	addHistoryCutoverReview(t, engine, baseline)
	addHistoryCutoverReview(t, engine, variant)
	reader := readerWithCurrentNoop(10, 1)
	currentRows := reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)
	current := currentRows["workflow_runs"].([]any)[0].(Object)
	currentRows["total_count"] = int64(2)
	currentRows["workflow_runs"] = []any{legacy, current}
	temp := t.TempDir()
	options := historyCutoverNoopOptions(t, engine, reader, baseline, temp, 10, 1)
	if result, err := engine.FinishNoop(context.Background(), reader, options); err != nil || result["outcome"] != "completed" {
		t.Fatalf("native workflow no-op did not qualify under the reviewed cutover: %#v %v", result, err)
	}
	run := reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)["workflow_runs"].([]any)[1].(Object)
	run["status"], run["conclusion"] = "completed", "success"
	mainArtifact, handoffPayload := uploadFixturePackage(t, reader, engine, options.Invocation, 501)
	reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(1), "artifacts": []any{mainArtifact}}}
	finalized, err := engine.Finalize(context.Background(), reader, FinalizeOptions{Invocation: options.Invocation, ArtifactID: 501, ArtifactDigest: mainArtifact["digest"].(string), Checkpoint: filepath.Join(temp, "checkpoint")})
	if err != nil || finalized["outcome"] != "checkpoint" {
		t.Fatalf("native no-op did not advance its reviewed cutover checkpoint: %#v %v", finalized, err)
	}
	checkpointBytes, err := os.ReadFile(finalized["checkpoint_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	checkpointChain, err := DecodeValue(checkpointBytes)
	if err != nil || !exactInt(checkpointChain.(Object)["schema_version"], historyCutoverSettlementSchemaVersion) || checkpointChain.(Object)["history_cutover"].(Object)["sha256"] != digest {
		t.Fatalf("native no-op checkpoint stripped or changed its history boundary: %#v %v", checkpointChain, err)
	}
	target, err := eTargetForCutoverTest(t, baseline)
	if err != nil {
		t.Fatal(err)
	}
	checkpointPayload := recoveryZip(t, recoveryZipEntry{name: "settlement-chain.json", data: checkpointBytes})
	checkpointArtifact := Object{
		"id": int64(502), "name": CheckpointArtifactName(target, 10, 1), "expired": false,
		"digest": "sha256:" + SHA256(checkpointPayload), "workflow_run": Object{"id": int64(10), "head_sha": strings.Repeat("a", 40)},
	}
	reader.reads["repos/example/widgets/actions/artifacts/502"] = checkpointArtifact
	reader.archives[502] = checkpointPayload
	handoffArtifact := Object{
		"id": int64(503), "name": RecoveryArtifactName(target, 10, 1) + "-handoff-00", "expired": false,
		"digest": "sha256:" + SHA256(handoffPayload), "workflow_run": Object{"id": int64(10), "head_sha": strings.Repeat("a", 40)},
	}
	reader.reads["repos/example/widgets/actions/artifacts/503"] = handoffArtifact
	reader.archives[503] = handoffPayload
	reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(3), "artifacts": []any{mainArtifact, checkpointArtifact, handoffArtifact}}}
	transportTemp := t.TempDir()
	transportInvocation := options.Invocation
	transportInvocation.PackageRoot, transportInvocation.RunnerTemp = filepath.Join(transportTemp, "transport"), transportTemp
	transported, err := engine.AcquireHandoffFor(context.Background(), reader, transportInvocation, 503, handoffArtifact["digest"].(string), "transport")
	if err != nil || transported["outcome"] != "acquired" || transported["purpose"] != "transport" {
		t.Fatalf("completed schema-6 workflow-noop transport was not accepted: %#v %v", transported, err)
	}
	transportedContext, err := LoadJSON(filepath.Join(transportInvocation.PackageRoot, "run-context.json"))
	if err != nil || transportedContext.(Object)["phase"] != "completed" {
		t.Fatalf("transport did not retain its inert completed no-op context: %#v %v", transportedContext, err)
	}
	transportedPlan, err := LoadJSON(filepath.Join(transportInvocation.PackageRoot, "plans", "workflow-noop.json"))
	if err != nil || transportedPlan.(Object)["command"] != "workflow-noop" {
		t.Fatalf("transport did not retain its exact no-op plan: %#v %v", transportedPlan, err)
	}
	current = recoveryHistoryRun(11, 1, "Current")
	current["status"], current["conclusion"] = "in_progress", nil
	previous := recoveryHistoryRun(10, 1, "Current")
	reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"] = []any{Object{"total_count": int64(3), "workflow_runs": []any{legacy, previous, current}}}

	// A new Engine and scratch root model a process restart. The same reviewed
	// baseline and uploaded checkpoint must remain selectable without rebuilding
	// history from a policy-only time cutoff.
	restarted := noopEngine(t)
	addHistoryCutoverReview(t, restarted, baseline)
	addHistoryCutoverReview(t, restarted, variant)
	secondTemp := t.TempDir()
	secondInvocation := Invocation{Workflow: "task.yml", RunID: 11, Attempt: 1, RunName: "Current", RecoveryKey: "task", PackageRoot: filepath.Join(secondTemp, "invocation"), RunnerTemp: secondTemp}
	result, err := restarted.RecoverWithHistoryCutover(context.Background(), reader, secondInvocation, baseline)
	if err != nil || result["outcome"] != "fresh" || result["mode"] != historyCutoverScope {
		t.Fatalf("fresh process did not retain the reviewed native prefix: %#v %v", result, err)
	}
	resumedChain, err := LoadJSON(filepath.Join(secondInvocation.PackageRoot, "settlement-chain.json"))
	if err != nil || resumedChain.(Object)["history_cutover"].(Object)["sha256"] != digest || !Equal(resumedChain.(Object)["history_cutover"], baseline) || len(resumedChain.(Object)["settlements"].([]any)) != 1 {
		t.Fatalf("fresh-process recovery lost the no-op chain or baseline: %#v %v", resumedChain, err)
	}
	legacyAttempts, err := objectArray(resumedChain.(Object)["history_cutover"].(Object)["attempts"], "retained legacy attempts")
	if err != nil || len(legacyAttempts) != 1 || legacyAttempts[0]["outcome"] != "unknown" || legacyAttempts[0]["handling"] != "quarantined-never-replay" {
		t.Fatalf("fresh-process recovery changed or dropped the quarantined legacy attempt: %#v %v", legacyAttempts, err)
	}
	for _, path := range []string{"run-context.json", "plans", "journal"} {
		if _, err := os.Lstat(filepath.Join(secondInvocation.PackageRoot, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Fatalf("fresh next-run recovery restored executable handoff state %s: %v", path, err)
		}
	}
	_, _, currentTarget, observed, latest, err := restarted.invocationHistory(context.Background(), reader, secondInvocation)
	if err != nil {
		t.Fatal(err)
	}
	swapped := cloneHistoryCutoverObject(t, resumedChain.(Object))
	swapped["history_cutover"] = variant
	unsigned := Object{}
	for _, field := range cutoverChainFields[:len(cutoverChainFields)-1] {
		unsigned[field] = swapped[field]
	}
	unsignedBytes, err := Canonical(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	swapped["sha256"] = SHA256(unsignedBytes)
	if _, err := restarted.ValidateChain(swapped, currentTarget, observed, latest); err == nil {
		t.Fatal("native no-op proof produced under one reviewed baseline was accepted under another allowlisted baseline")
	}

	ordinaryTemp := t.TempDir()
	ordinary := noopEngine(t)
	addHistoryCutoverReview(t, ordinary, baseline)
	ordinaryInvocation := Invocation{Workflow: "task.yml", RunID: 11, Attempt: 1, RunName: "Current", RecoveryKey: "task", PackageRoot: filepath.Join(ordinaryTemp, "invocation"), RunnerTemp: ordinaryTemp}
	if result, err := ordinary.Recover(context.Background(), reader, ordinaryInvocation); err != nil || result["outcome"] != "fresh" {
		t.Fatalf("ordinary recovery did not authenticate the reviewed embedded boundary: %#v %v", result, err)
	}

	changed := cloneHistoryCutoverObject(t, baseline)
	changed["workflow"].(Object)["name"] = "Edited snapshot metadata"
	// The raw baseline remains structurally valid, but its new digest has not been reviewed.
	resealHistoryCutoverFixture(t, changed)
	changedTemp := t.TempDir()
	changedInvocation := Invocation{Workflow: "task.yml", RunID: 11, Attempt: 1, RunName: "Current", RecoveryKey: "task", PackageRoot: filepath.Join(changedTemp, "invocation"), RunnerTemp: changedTemp}
	if result, err := restarted.RecoverWithHistoryCutover(context.Background(), reader, changedInvocation, changed); err != nil || result["outcome"] != "recovery_needed" {
		t.Fatalf("unreviewed baseline replacement was not held: %#v %v", result, err)
	}

	stripped := cloneHistoryCutoverObject(t, resumedChain.(Object))
	delete(stripped, "history_cutover")
	_, _, target, observed, latest, err = restarted.invocationHistory(context.Background(), reader, secondInvocation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ValidateChain(stripped, target, observed, latest); err == nil {
		t.Fatal("schema-6 boundary stripping was accepted")
	}
}

func eTargetForCutoverTest(t *testing.T, baseline Object) (Object, error) {
	t.Helper()
	target, err := object(baseline["target"], "baseline target")
	if err != nil {
		return nil, err
	}
	return ValidateTarget(target)
}

func TestHistoryCutoverHoldsQuarantinedRerunsAndUnsettledNativeFailures(t *testing.T) {
	oldRun := recoveryHistoryRun(101, 1, "Rerun history")
	attempt := historyCutoverAttemptResponse(oldRun, 1, "2026-01-01T00:00:00Z")
	baseline := captureHistoryCutoverFixture(t, historyCutoverFixture([]Object{oldRun}, nil, map[string]Object{historyCutoverAttemptEndpoint(101, 1): attempt}, nil))
	engine := testRecoveryEngine(t, false)
	addHistoryCutoverReview(t, engine, baseline)
	rerun := recoveryHistoryRun(101, 2, "Rerun history")
	rerun["status"], rerun["conclusion"] = "in_progress", nil
	reader := &recoveryReaderFixture{
		pages: map[string][]any{
			"repos/example/widgets/actions/workflows/task.yml/runs?per_page=100": {Object{"total_count": int64(1), "workflow_runs": []any{rerun}}},
			"repos/example/widgets/actions/artifacts?per_page=100":               {Object{"total_count": int64(0), "artifacts": []any{}}},
		},
	}
	temp := t.TempDir()
	invocation := Invocation{Workflow: "task.yml", RunID: 101, Attempt: 2, RunName: "Rerun history", RecoveryKey: "task", PackageRoot: filepath.Join(temp, "invocation"), RunnerTemp: temp}
	if result, err := engine.RecoverWithHistoryCutover(context.Background(), reader, invocation, baseline); err != nil || result["outcome"] != "recovery_needed" {
		t.Fatalf("rerun immediately above an unknown baseline attempt was treated as settled: %#v %v", result, err)
	}

	// A new post-cutover attempt is likewise not inferred as a no-op merely
	// because its predecessor is terminal; absent the exact native checkpoint,
	// its source proof remains pending and recovery must hold.
	empty := captureHistoryCutoverFixture(t, historyCutoverFixture(nil, nil, nil, nil))
	failureEngine := testRecoveryEngine(t, false)
	addHistoryCutoverReview(t, failureEngine, empty)
	failedCurrent := recoveryHistoryRun(202, 2, "Unsettled predecessor")
	failedCurrent["status"], failedCurrent["conclusion"] = "in_progress", nil
	failureReader := &recoveryReaderFixture{pages: map[string][]any{
		"repos/example/widgets/actions/workflows/task.yml/runs?per_page=100": {Object{"total_count": int64(1), "workflow_runs": []any{failedCurrent}}},
		"repos/example/widgets/actions/artifacts?per_page=100":               {Object{"total_count": int64(0), "artifacts": []any{}}},
	}}
	failureTemp := t.TempDir()
	failureInvocation := Invocation{Workflow: "task.yml", RunID: 202, Attempt: 2, RunName: "Unsettled predecessor", RecoveryKey: "task", PackageRoot: filepath.Join(failureTemp, "invocation"), RunnerTemp: failureTemp}
	if result, err := failureEngine.RecoverWithHistoryCutover(context.Background(), failureReader, failureInvocation, empty); err != nil || result["outcome"] != "recovery_needed" {
		t.Fatalf("new native predecessor without a checkpoint was presumed settled: %#v %v", result, err)
	}
}

func TestHistoryCutoverHoldsValidInterruptedSourceWithoutRestoringExecutionFiles(t *testing.T) {
	engine, reader, _, metadata := preparedHandoffFixture(t)
	files, err := archiveFiles(reader.archives[31], false)
	if err != nil {
		t.Fatal(err)
	}
	contextValue, err := DecodeValue(files["run-context.json"])
	if err != nil {
		t.Fatal(err)
	}
	runContext := contextValue.(Object)
	runContext["phase"] = "dispatching"
	entry := runContext["plans"].([]any)[0].(Object)
	entry["status"] = "dispatching"
	planValue, err := DecodeValue(files["plans/sync.json"])
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contract.ParsePlan(planValue.(Object))
	if err != nil {
		t.Fatal(err)
	}
	intent := Object{
		"id": plan.Operations[0].ID, "kind": plan.Operations[0].Kind, "target": plan.Operations[0].Target,
		"before": plan.Operations[0].Before, "after": plan.Operations[0].After,
	}
	intentBytes, err := Canonical(intent)
	if err != nil {
		t.Fatal(err)
	}
	journalID := entry["journal_id"].(string)
	journal := Object{
		"identity": Object{"schema_version": int64(2), "repository": plan.Repository.Object(), "command": plan.Command, "plan_sha256": plan.SHA256},
		"steps": []any{Object{
			"id": intent["id"], "intent": intent, "intent_sha256": SHA256(intentBytes), "operation_id": strings.Repeat("c", 32),
			"status": "unknown", "result": nil, "started_at": "2026-10-04T12:00:00Z", "completed_at": nil,
			"acknowledgement": nil, "observation": nil,
		}},
		"result": nil,
	}
	contextBytes, err := Canonical(runContext)
	if err != nil {
		t.Fatal(err)
	}
	journalBytes, err := Canonical(journal)
	if err != nil {
		t.Fatal(err)
	}
	files["run-context.json"] = contextBytes
	files["journal/"+journalID+".json"] = journalBytes
	entries := make([]recoveryZipEntry, 0, len(files))
	for name, data := range files {
		entries = append(entries, recoveryZipEntry{name: name, data: data})
	}
	payload := recoveryZip(t, entries...)
	legacy := recoveryHistoryRun(5, 1, "Legacy quarantined run")
	baseline := captureHistoryCutoverFixture(t, historyCutoverFixture([]Object{legacy}, nil, map[string]Object{
		historyCutoverAttemptEndpoint(5, 1): historyCutoverAttemptResponse(legacy, 1, legacy["created_at"].(string)),
	}, nil))
	addHistoryCutoverReview(t, engine, baseline)
	target, err := eTargetForCutoverTest(t, baseline)
	if err != nil {
		t.Fatal(err)
	}
	sourceRun := recoveryHistoryRun(90, 1, "Current")
	sourceRun["status"], sourceRun["conclusion"] = "completed", "failure"
	currentRun := recoveryHistoryRun(91, 1, "Current")
	currentRun["status"], currentRun["conclusion"] = "in_progress", nil
	name := RecoveryArtifactName(target, 90, 1)
	metadata["name"], metadata["digest"] = name, "sha256:"+SHA256(payload)
	metadata["workflow_run"] = Object{"id": int64(90), "head_sha": sourceRun["head_sha"]}
	reader.reads["repos/example/widgets/actions/artifacts/31"] = metadata
	reader.reads["repos/example/widgets/actions/runs/90/attempts/1"] = sourceRun
	reader.archives[31] = payload
	reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"] = []any{Object{
		"total_count": int64(3), "workflow_runs": []any{legacy, sourceRun, currentRun},
	}}
	reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{
		"total_count": int64(1), "artifacts": []any{metadata},
	}}
	artifact, err := artifactIdentity(metadata, name, 90)
	if err != nil {
		t.Fatal(err)
	}
	sourceIdentity, err := NormalizeRun(sourceRun)
	if err != nil {
		t.Fatal(err)
	}
	invocationTemp := t.TempDir()
	invocation := Invocation{
		Workflow: "task.yml", RunID: 91, Attempt: 1, RunName: "Current", RecoveryKey: "task",
		PackageRoot: filepath.Join(invocationTemp, "invocation"), RunnerTemp: invocationTemp,
	}
	inspection, err := engine.inspectHistoricalPackage(context.Background(), reader, invocation, []Object{legacy, sourceRun, currentRun}, sourceIdentity, target, artifact, 1, payload, false)
	if err != nil || inspection["kind"] != "source" {
		t.Fatalf("fixture is not a valid interrupted native source package: %#v %v", inspection, err)
	}
	result, err := engine.RecoverWithHistoryCutover(context.Background(), reader, invocation, baseline)
	if err != nil || result["outcome"] != "recovery_needed" {
		t.Fatalf("schema-6 mode restored executable source instead of holding: %#v %v", result, err)
	}
	for _, path := range []string{"run-context.json", "plans/sync.json", "journal/" + journalID + ".json"} {
		if _, err := os.Lstat(filepath.Join(invocation.PackageRoot, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Fatalf("schema-6 hold restored source execution file %s: %v", path, err)
		}
	}
}

func TestHistoryCutoverRejectsCompletedPublicationSettlement(t *testing.T) {
	engine, reader, options, target := finalizerPublicationFixture(t)
	finalized, err := engine.Finalize(context.Background(), reader, options)
	if err != nil || finalized["outcome"] != "checkpoint" {
		t.Fatalf("fixture did not produce a valid completed publication settlement: %#v %v", finalized, err)
	}
	legacyChainValue, err := LoadJSON(finalized["checkpoint_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	legacyChain := legacyChainValue.(Object)
	record := legacyChain["settlements"].([]any)[0].(Object)
	baselineReader := &recoveryReaderFixture{
		reads: map[string]Object{
			"repos/sample/repo/actions/workflows/automation.yml": Object{
				"id": int64(42), "path": ".github/workflows/automation.yml", "name": "Automation", "state": "active",
			},
		},
		pages: map[string][]any{
			"repos/sample/repo/actions/workflows/automation.yml/runs?per_page=100": {Object{"total_count": int64(0), "workflow_runs": []any{}}},
			"repos/sample/repo/actions/artifacts?per_page=100":                     {Object{"total_count": int64(0), "artifacts": []any{}}},
		},
	}
	baseline, err := CaptureHistoryCutover(context.Background(), baselineReader, engine.repository, "automation.yml", nil)
	if err != nil || !Equal(target, baseline["target"]) {
		t.Fatalf("capture fixture cutover baseline for target: %#v %v", baseline, err)
	}
	addHistoryCutoverReviewForWorkflow(t, engine, "automation.yml", baseline)
	_, _, observedTarget, observed, latest, err := engine.invocationHistory(context.Background(), reader, options.Invocation)
	if err != nil || !Equal(target, observedTarget) {
		t.Fatalf("read exact terminal source history: %#v %v", observedTarget, err)
	}
	chain, err := HistoryCutoverChain(target, baseline)
	if err != nil {
		t.Fatal(err)
	}
	chain["inventory"] = []any{Object{"run": record["run"], "settled_attempt": record["attempt"]}}
	chain["settlements"] = []any{record}
	unsigned := Object{}
	for _, field := range cutoverChainFields[:len(cutoverChainFields)-1] {
		unsigned[field] = chain[field]
	}
	unsignedBytes, err := Canonical(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	chain["sha256"] = SHA256(unsignedBytes)
	if _, err := engine.ValidateChain(chain, target, observed, latest); err == nil {
		t.Fatal("completed publication settlement entered preview-only history cutover checkpoint")
	}
}

func TestHistoryCutoverBlocksHandoffAndJournalExecutionPaths(t *testing.T) {
	engine, reader, invocation, metadata := preparedHandoffFixture(t)
	baseline := captureHistoryCutoverFixture(t, historyCutoverFixture(nil, nil, nil, nil))
	addHistoryCutoverReview(t, engine, baseline)
	target, err := eTargetForCutoverTest(t, baseline)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := HistoryCutoverChain(target, baseline)
	if err != nil {
		t.Fatal(err)
	}
	files, err := archiveFiles(reader.archives[31], false)
	if err != nil {
		t.Fatal(err)
	}
	chainBytes, err := Canonical(chain)
	if err != nil {
		t.Fatal(err)
	}
	files["settlement-chain.json"] = chainBytes
	entries := make([]recoveryZipEntry, 0, len(files))
	for name, data := range files {
		entries = append(entries, recoveryZipEntry{name: name, data: data})
	}
	payload := recoveryZip(t, entries...)
	metadata["digest"] = "sha256:" + SHA256(payload)
	reader.archives[31] = payload
	for _, purpose := range []string{"apply", "transport"} {
		t.Run(purpose, func(t *testing.T) {
			temp := t.TempDir()
			call := invocation
			call.PackageRoot, call.RunnerTemp = filepath.Join(temp, "invocation"), temp
			result, err := engine.AcquireHandoffFor(context.Background(), reader, call, 31, metadata["digest"].(string), purpose)
			if err == nil || result != nil {
				t.Fatalf("schema-6 handoff accepted executable context for purpose %s: %#v %v", purpose, result, err)
			}
		})
	}

	// Independently exercise the direct journal installation and capture APIs
	// with an otherwise valid executable handoff package already extracted.
	engine2, reader2, invocation2, metadata2 := preparedHandoffFixture(t)
	if _, err := engine2.AcquireHandoff(context.Background(), reader2, invocation2, 31, metadata2["digest"].(string)); err != nil {
		t.Fatal(err)
	}
	addHistoryCutoverReview(t, engine2, baseline)
	chain2, err := HistoryCutoverChain(target, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPackageJSON(invocation2.PackageRoot, "settlement-chain.json", chain2); err != nil {
		t.Fatal(err)
	}
	applyRoot := t.TempDir()
	journalRoot := filepath.Join(applyRoot, ".artifacts", "gh-steward", "journals")
	if err := os.MkdirAll(journalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := engine2.InstallRestoredJournal(invocation2.PackageRoot, journalRoot, "sync"); err == nil {
		t.Fatal("schema-6 recovery package installed an executable native journal")
	}
	if entries, err := os.ReadDir(journalRoot); err != nil || len(entries) != 0 {
		t.Fatalf("rejected schema-6 journal installation wrote engine state: %#v %v", entries, err)
	}
	if _, err := engine2.MarkContextPlan(invocation2.PackageRoot, "sync", "dispatching"); err == nil {
		t.Fatal("schema-6 package advanced an executable plan")
	}
	if _, err := engine2.CaptureJournal(invocation2.PackageRoot, journalRoot, "sync"); err == nil {
		t.Fatal("schema-6 package captured executable journal progress")
	}
}

func TestHoldRecoveryPersistsOnlyGenericLocalMarker(t *testing.T) {
	engine := testRecoveryEngine(t, false)
	temp := t.TempDir()
	invocation := Invocation{Workflow: "task.yml", RunID: 11, Attempt: 2, RunName: "Current", RecoveryKey: "task", PackageRoot: filepath.Join(temp, "invocation"), RunnerTemp: temp}
	result, err := engine.HoldRecovery(invocation, "history cutover review could not be verified: unsafe provider detail")
	if err != nil || result["outcome"] != "recovery_needed" {
		t.Fatalf("review-read failure did not become a local hold: %#v %v", result, err)
	}
	marker, err := LoadJSON(filepath.Join(invocation.PackageRoot, "recovery-needed.json"))
	if err != nil || marker.(Object)["reason"] != "history cutover review could not be verified" {
		t.Fatalf("local hold retained untrusted provider error detail: %#v %v", marker, err)
	}
	if _, err := os.Lstat(filepath.Join(invocation.PackageRoot, "settlement-chain.json")); !os.IsNotExist(err) {
		t.Fatal("local hold fabricated a settlement checkpoint")
	}
}
