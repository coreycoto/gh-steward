package runrecovery

import (
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestRecoveredTerminalProofRequiresDistinctMonotonicObserver(t *testing.T) {
	proof, target := makeRecoveredTerminalProof(t)
	plans, err := ValidateRecoveredTerminalProof(proof, target)
	if err != nil {
		t.Fatalf("valid source/observer proof rejected: %v", err)
	}
	if len(plans) != 1 || plans[0]["command"] != "branch-delete" {
		t.Fatalf("unexpected decoded observer plans: %#v", plans)
	}

	sameAttempt := cloneObserverProof(proof)
	source := sameAttempt["source"].(Object)
	observer := sameAttempt["observer"].(Object)
	observer["run_id"], observer["attempt"] = source["run_id"], source["attempt"]
	observer["run"].(Object)["id"] = source["run_id"]
	observer["artifact"].(Object)["name"] = observerArtifactName(target, source["run_id"].(int64), source["attempt"].(int64))
	if _, err := ValidateRecoveredTerminalProof(sameAttempt, target); err == nil {
		t.Fatal("same workflow attempt was accepted as both source and observer")
	}

	sharedArtifact := cloneObserverProof(proof)
	sharedArtifact["observer"].(Object)["artifact"].(Object)["id"] = sharedArtifact["source"].(Object)["artifact"].(Object)["id"]
	if _, err := ValidateRecoveredTerminalProof(sharedArtifact, target); err == nil {
		t.Fatal("source artifact relabeled as observer evidence was accepted")
	}

	relabeledArtifact := cloneObserverProof(proof)
	relabeledArtifact["observer"].(Object)["artifact"].(Object)["name"] = relabeledArtifact["source"].(Object)["artifact"].(Object)["name"]
	if _, err := ValidateRecoveredTerminalProof(relabeledArtifact, target); err == nil {
		t.Fatal("source artifact name relabeled as observer evidence was accepted")
	}
}

func TestRecoveredTerminalProofRejectsChangedSourceAcknowledgementAndJournalHash(t *testing.T) {
	proof, target := makeRecoveredTerminalProof(t)
	changed := cloneObserverProof(proof)
	source := changed["source"].(Object)
	sourcePlan := source["plans"].([]any)[0].(Object)
	journalValue, err := LoadFileProof(sourcePlan["journal_file"], "source journal")
	if err != nil {
		t.Fatal(err)
	}
	journal := journalValue.(Object)
	step := journal["steps"].([]any)[0].(Object)
	step["acknowledgement"].(Object)["target"] = Object{"name": "different-branch", "sha": "cccccccccccccccccccccccccccccccccccccccc"}
	sourcePlan["journal_file"] = nativeJSONFileProof(t, journal)
	if _, err := ValidateRecoveredTerminalProof(changed, target); err == nil {
		t.Fatal("observer that changed the original acknowledgement was accepted")
	}

	changed, _ = makeRecoveredTerminalProof(t)
	source = changed["source"].(Object)
	sourcePlan = source["plans"].([]any)[0].(Object)
	sourcePlan["journal_id"] = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := ValidateRecoveredTerminalProof(changed, target); err == nil {
		t.Fatal("source journal proof with an altered identity hash was accepted")
	}
}

func TestRecoveredTerminalProofRejectsNoPersistedDispatchAndLexicalNumbers(t *testing.T) {
	proof, target := makeRecoveredTerminalProof(t)
	prepared := cloneObserverProof(proof)
	source := prepared["source"].(Object)
	sourcePlan := source["plans"].([]any)[0].(Object)
	sourcePlan["journal_file"] = nil
	contextValue, err := LoadFileProof(source["context_file"], "source context")
	if err != nil {
		t.Fatal(err)
	}
	context := contextValue.(Object)
	context["plans"].([]any)[0].(Object)["status"] = "prepared"
	source["context_file"] = nativeJSONFileProof(t, context)
	if _, err := ValidateRecoveredTerminalProof(prepared, target); err == nil {
		t.Fatal("recovery without any durable dispatch identity was accepted")
	}

	proof, target = makeRecoveredTerminalProof(t)
	source = proof["source"].(Object)
	contextProof, err := decodePublicationRawProof(source["context_file"], "source context")
	if err != nil {
		t.Fatal(err)
	}
	contextRaw, err := LoadFileProof(source["context_file"], "source context")
	if err != nil {
		t.Fatal(err)
	}
	contextBytes, err := Canonical(contextRaw)
	if err != nil {
		t.Fatal(err)
	}
	lexical := string(contextBytes)
	lexical = replaceFirst(lexical, `"schema_version":1`, `"schema_version":1.0`)
	source["context_file"] = MakeFileProof([]byte(lexical))
	if _, err := ValidateRecoveredTerminalProof(proof, target); err == nil {
		t.Fatal("fractional spelling of the context schema version was accepted")
	}
	_ = contextProof
}

func TestRecoveredTerminalProofRejectsApplyResultOnDispatchingSource(t *testing.T) {
	proof, target := makeRecoveredTerminalProof(t)
	source := proof["source"].(Object)
	sourcePlan := source["plans"].([]any)[0].(Object)
	observerPlan := proof["observer"].(Object)["plans"].([]any)[0].(Object)
	sourcePlan["apply_result_file"] = observerPlan["apply_result_file"]
	if _, err := ValidateRecoveredTerminalProof(proof, target); err == nil {
		t.Fatal("dispatching source plan with an injected terminal apply result was accepted")
	}
}

func TestRecoveredTerminalProofCannotAlterCompletedSourceReceipt(t *testing.T) {
	proof, target := makeRecoveredTerminalProof(t)
	source := proof["source"].(Object)
	sourcePlan := source["plans"].([]any)[0].(Object)
	observerPlan := proof["observer"].(Object)["plans"].([]any)[0].(Object)
	sourcePlan["journal_file"] = observerPlan["journal_file"]
	if _, err := ValidateRecoveredTerminalProof(proof, target); err != nil {
		t.Fatalf("unchanged completed source receipt should remain valid: %v", err)
	}

	proof, target = makeRecoveredTerminalProof(t)
	source = proof["source"].(Object)
	sourcePlan = source["plans"].([]any)[0].(Object)
	observerPlan = proof["observer"].(Object)["plans"].([]any)[0].(Object)
	completedJournal, err := LoadFileProof(observerPlan["journal_file"], "completed observer journal")
	if err != nil {
		t.Fatal(err)
	}
	completedJournal.(Object)["steps"].([]any)[0].(Object)["completed_at"] = "2026-10-04T12:03:00Z"
	sourcePlan["journal_file"] = nativeJSONFileProof(t, completedJournal)
	if _, err := ValidateRecoveredTerminalProof(proof, target); err == nil {
		t.Fatal("observer that changed a completed source receipt was accepted")
	}
}

func TestRecoveredTerminalProofPreservesCompletedParentApplyResultBytes(t *testing.T) {
	proof, target := makeRecoveredTerminalProof(t)
	parentPlan, parentProof := makeNativeTerminalProof(t, []contract.Operation{nativeTestOperation("completed-parent", "branch-delete")}, nativeAckBranchDeletion)
	parentProof["name"] = "completed-parent"

	source := proof["source"].(Object)
	observer := proof["observer"].(Object)
	source["plans"] = append(source["plans"].([]any), parentProof)
	observer["plans"] = append(observer["plans"].([]any), cloneNativeObject(parentProof))

	sourceContextValue, err := LoadFileProof(source["context_file"], "source context")
	if err != nil {
		t.Fatal(err)
	}
	sourceContext := sourceContextValue.(Object)
	parentContextPlan := Object{
		"name": "completed-parent", "command": parentPlan.Command, "sha256": parentPlan.SHA256,
		"journal_id": parentProof["journal_id"], "status": "completed",
	}
	sourceContext["plans"] = append(sourceContext["plans"].([]any), parentContextPlan)
	source["context_file"] = nativeJSONFileProof(t, sourceContext)

	observerContextValue, err := LoadFileProof(observer["context_file"], "observer context")
	if err != nil {
		t.Fatal(err)
	}
	observerContext := observerContextValue.(Object)
	observerContextPlan := cloneNativeObject(parentContextPlan)
	observerContext["plans"] = append(observerContext["plans"].([]any), observerContextPlan)
	observer["context_file"] = nativeJSONFileProof(t, observerContext)

	if _, err := ValidateRecoveredTerminalProof(proof, target); err != nil {
		t.Fatalf("unchanged completed parent result bytes should remain valid: %v", err)
	}

	changed := cloneObserverProof(proof)
	completedParent := changed["observer"].(Object)["plans"].([]any)[1].(Object)
	applyValue, err := LoadFileProof(completedParent["apply_result_file"], "completed parent apply result")
	if err != nil {
		t.Fatal(err)
	}
	applyResult := applyValue.(Object)
	applyResult["tool_version"] = "0.1.0+equivalent-receipt"
	completedParent["apply_result_file"] = nativeJSONFileProof(t, applyResult)
	if _, err := ValidateTerminalPlanProof(completedParent); err != nil {
		t.Fatalf("substituted completed parent result should remain semantically valid: %v", err)
	}
	if _, err := ValidateRecoveredTerminalProof(changed, target); err == nil {
		t.Fatal("observer substituted byte-different completed parent apply-result evidence")
	}
}

func TestRecoveredTerminalProofPreservesAndValidatesPolicyFiles(t *testing.T) {
	proof, target := makeRecoveredTerminalProof(t)
	policyFile := MakeFileProof([]byte(`{"approved":true}`))
	proof["source"].(Object)["policy_files"] = Object{"reviews/plan.json": policyFile}
	proof["observer"].(Object)["policy_files"] = Object{"reviews/plan.json": policyFile}
	if _, err := ValidateRecoveredTerminalProof(proof, target); err != nil {
		t.Fatalf("matching raw policy sidecars were rejected: %v", err)
	}
	missing := cloneObserverProof(proof)
	delete(missing["source"].(Object), "policy_files")
	if _, err := ValidateRecoveredTerminalProof(missing, target); err == nil {
		t.Fatal("attempt without explicit policy_files map was accepted")
	}

	changed := cloneObserverProof(proof)
	changed["observer"].(Object)["policy_files"].(Object)["reviews/plan.json"] = MakeFileProof([]byte(`{"approved":false}`))
	if _, err := ValidateRecoveredTerminalProof(changed, target); err == nil {
		t.Fatal("observer policy sidecar that differs from source was accepted")
	}

	unsafe := cloneObserverProof(proof)
	unsafe["observer"].(Object)["policy_files"].(Object)["../plan.json"] = policyFile
	if _, err := ValidateRecoveredTerminalProof(unsafe, target); err == nil {
		t.Fatal("traversal in policy sidecar path was accepted")
	}

	malformed := cloneObserverProof(proof)
	malformed["observer"].(Object)["policy_files"].(Object)["events/dispatch-event.json"] = Object{"sha256": strings.Repeat("0", 64), "base64": "e30="}
	if _, err := ValidateRecoveredTerminalProof(malformed, target); err == nil {
		t.Fatal("policy sidecar with a mismatched raw-byte digest was accepted")
	}
}

func TestRecoveredTerminalProofRejectsForgedCrossRunPlanOrigin(t *testing.T) {
	proof, target := makeRecoveredTerminalProof(t)
	observer := proof["observer"].(Object)
	contextValue, err := LoadFileProof(observer["context_file"], "observer context")
	if err != nil {
		t.Fatal(err)
	}
	context := contextValue.(Object)
	context["plan_origin_run_id"], context["plan_origin_attempt"] = int64(999), int64(2)
	observer["context_file"] = nativeJSONFileProof(t, context)
	if _, err := ValidateRecoveredTerminalProof(proof, target); err == nil {
		t.Fatal("terminal observer with a forged cross-run native plan origin was accepted")
	}
}

func TestRecoveryArtifactNamespaceBindsCanonicalHost(t *testing.T) {
	github := Object{"workflow_file": "automation.yml", "repository": "sample/repo", "server_url": "https://github.com", "workflow_id": int64(42), "recovery_key": "workflow-history-v2"}
	enterprise := Object{"workflow_file": "automation.yml", "repository": "sample/repo", "server_url": "https://github.example", "workflow_id": int64(42), "recovery_key": "workflow-history-v2"}
	if RecoveryArtifactName(github, 100, 2) == RecoveryArtifactName(enterprise, 100, 2) {
		t.Fatal("different GitHub hosts share a recovery artifact namespace")
	}
}

func makeRecoveredTerminalProof(t *testing.T) (Object, Object) {
	t.Helper()
	operation := nativeTestOperation("branch-1", "branch-delete")
	plan, terminalProof := makeNativeTerminalProof(t, []contract.Operation{operation}, nativeAckAcknowledged)
	observerJournalValue, err := LoadFileProof(terminalProof["journal_file"], "observer journal")
	if err != nil {
		t.Fatal(err)
	}
	observerJournal := observerJournalValue.(Object)
	observerSteps := observerJournal["steps"].([]any)
	sourceStep := cloneNativeObject(observerSteps[0].(Object))
	sourceStep["status"], sourceStep["result"] = "unknown", nil
	delete(sourceStep, "completed_at")
	sourceJournal := Object{
		"identity": observerJournal["identity"], "steps": []any{sourceStep}, "result": nil,
	}

	target := Object{"workflow_file": "automation.yml", "repository": "sample/repo", "server_url": "https://github.com", "workflow_id": int64(42), "recovery_key": "workflow-history-v2"}
	recoveryKey := "run-100-attempt-2"
	attemptTarget := Object{"scope": "one-reviewed-attempt"}
	planContext := Object{
		"name": "branch-deletion", "command": plan.Command, "sha256": plan.SHA256,
		"journal_id": terminalProof["journal_id"], "status": "dispatching",
	}
	completedContextPlan := cloneNativeObject(planContext)
	completedContextPlan["status"] = "completed"
	sourceRun := observerRun(100, 42, "2026-10-04T12:00:00Z", "original run")
	observerRunValue := cloneNativeObject(sourceRun)
	artifactDigest := "sha256:" + strings.Repeat("d", 64)
	sourceAttemptNumber, observerAttemptNumber := int64(2), int64(3)
	sourceProofPlan := Object{
		"name": "branch-deletion", "command": plan.Command, "plan_sha256": plan.SHA256,
		"journal_id": terminalProof["journal_id"], "plan_file": terminalProof["plan_file"],
		"journal_file": nativeJSONFileProof(t, sourceJournal),
	}
	sourceContext := Object{
		"schema_version": 1, "workflow_file": target["workflow_file"], "repository": target["repository"],
		"workflow_run_id": int64(100), "workflow_run_attempt": sourceAttemptNumber,
		"run_name": sourceRun["display_title"], "recovery_key": recoveryKey, "attempt_target": attemptTarget,
		"plans": []any{planContext}, "phase": "dispatching",
	}
	observerContext := Object{
		"schema_version": 1, "workflow_file": target["workflow_file"], "repository": target["repository"],
		"workflow_run_id": int64(100), "workflow_run_attempt": observerAttemptNumber,
		"run_name": observerRunValue["display_title"], "recovery_key": recoveryKey, "attempt_target": attemptTarget,
		"recovered_from_run_id": int64(100), "recovered_from_attempt": sourceAttemptNumber,
		"plans": []any{completedContextPlan}, "phase": "completed",
	}
	sourceArtifact := Object{
		"name": RecoveryArtifactName(target, 100, 2), "id": int64(200), "digest": artifactDigest,
	}
	observerArtifact := Object{
		"name": RecoveryArtifactName(target, 100, 3), "id": int64(201), "digest": artifactDigest,
	}
	sourceAttempt := Object{
		"run_id": int64(100), "attempt": sourceAttemptNumber, "run": sourceRun, "artifact": sourceArtifact,
		"context_file": nativeJSONFileProof(t, sourceContext), "plans": []any{sourceProofPlan}, "policy_files": Object{},
	}
	observerAttempt := Object{
		"run_id": int64(100), "attempt": observerAttemptNumber, "run": observerRunValue, "artifact": observerArtifact,
		"context_file": nativeJSONFileProof(t, observerContext), "plans": []any{terminalProof}, "policy_files": Object{},
	}
	return Object{"source": sourceAttempt, "observer": observerAttempt}, target
}

func observerRun(id, workflowID int64, createdAt, displayTitle string) Object {
	return Object{
		"id": id, "created_at": createdAt, "display_title": displayTitle, "event": "workflow_dispatch",
		"workflow_id": workflowID, "head_branch": "main", "head_sha": strings.Repeat("e", 40),
	}
}

func observerArtifactName(target Object, runID, attempt int64) string {
	return RecoveryArtifactName(target, runID, attempt)
}

func cloneObserverProof(value Object) Object {
	result := cloneNativeObject(value)
	for _, key := range []string{"source", "observer"} {
		result[key] = cloneNativeObject(value[key].(Object))
	}
	return result
}

func replaceFirst(value, old, replacement string) string {
	return strings.Replace(value, old, replacement, 1)
}
