package runrecovery

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
)

// An undispatched no-op is a reconciliation fact, not a completed native plan.
// Its context and decision remain the original uploaded bytes. Only the exact
// source and positively skipped mutation jobs are read afresh.
func (e *Engine) undispatchedNoopRecord(ctx context.Context, reader ActionsReader, root string, run, target, artifact, runContext Object) (Object, error) {
	decisionBytes, err := ReadPackageFile(root, noopDecisionPath)
	if err != nil {
		return nil, err
	}
	decisionValue, err := DecodeValue(decisionBytes)
	if err != nil {
		return nil, err
	}
	decision, err := exactWithOptional(decisionValue, noopDecisionFields, []string{"proposal", "previews"}, "undispatched no-op decision")
	if err != nil {
		return nil, err
	}
	expected := map[string]bool{"run-context.json": true, "settlement-chain.json": true, "recovery-observation.json": true, "events/trigger-event.json": true, noopDecisionPath: true}
	if decision["proposal"] != nil {
		proposal, err := Exact(decision["proposal"], []string{"candidate_sha256", "review_sha256"}, "inert proposal")
		if err != nil {
			return nil, err
		}
		expected["proposals/candidate.json"] = true
		if proposal["review_sha256"] != nil {
			expected["proposals/review.json"] = true
		}
	}
	previews, err := noopPreviewReferences(decision)
	if err != nil {
		return nil, err
	}
	for _, preview := range previews {
		expected[preview["path"].(string)] = true
	}
	files, err := retainedPackageFiles(root)
	if err != nil || len(files) != len(expected) {
		return nil, errors.New("undispatched no-op has missing or preexisting execution evidence")
	}
	proofs := Object{}
	for _, path := range files {
		if !expected[path] {
			return nil, errors.New("undispatched no-op cannot relabel plan, journal, publication or other execution files")
		}
		if path != "run-context.json" && path != "settlement-chain.json" {
			proof, err := fileProofPath(root, path, "original no-op evidence")
			if err != nil {
				return nil, err
			}
			proofs[path] = proof
		}
	}
	if runContext["trusted_source_sha"] != run["head_sha"] {
		return nil, errors.New("undispatched no-op control source differs from its immutable run head")
	}
	id, attempt := mustPositive(run["id"]), mustPositive(runContext["workflow_run_attempt"])
	source, err := reader.Read(ctx, fmt.Sprintf("repos/%s/contents/.github/workflows/%s?ref=%s", e.repository.FullName(), target["workflow_file"], run["head_sha"]))
	if err != nil {
		return nil, err
	}
	pages, err := reader.Pages(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", e.repository.FullName(), id, attempt))
	if err != nil {
		return nil, err
	}
	for path, value := range map[string]Object{"events/workflow-source.json": source, "events/current-jobs.json": {"run_id": id, "attempt": attempt, "pages": pages}} {
		raw, err := Canonical(value)
		if err != nil {
			return nil, err
		}
		proofs[path] = MakeFileProof(raw)
	}
	contextFile, err := fileProofPath(root, "run-context.json", "original no-op context")
	if err != nil {
		return nil, err
	}
	record := Object{
		"run_id": id, "attempt": attempt, "run": run, "artifact": artifact, "context_file": contextFile,
		"attempt_target": Object{"recovery_key": runContext["recovery_key"], "identity": runContext["attempt_target"]},
		"settlement":     Object{"kind": "undispatched_noop", "phase": "undispatched", "policy_files": proofs},
	}
	return e.ValidateSettlementRecord(record, target)
}

func (e *Engine) validateUndispatchedNoop(record, target Object) error {
	settled, err := Exact(record["settlement"], []string{"kind", "phase", "policy_files"}, "undispatched no-op fact")
	if err != nil || settled["phase"] != "undispatched" {
		return errors.New("undispatched no-op cannot claim a completed operation")
	}
	runContext, err := contextFromProof(record["context_file"], "original no-op context")
	if err != nil {
		return err
	}
	if err := e.validateRunContext(runContext); err != nil {
		return err
	}
	plans, _ := array(runContext["plans"], "no-op plans")
	steps, _ := stringsArray(runContext["dispatch_steps"], "no-op dispatch", false)
	run, _ := object(record["run"], "no-op run")
	attemptTarget, _ := object(record["attempt_target"], "no-op target")
	if runContext["phase"] != "prepared" || len(plans) != 0 || len(steps) != 0 || runContext["trusted_source_sha"] != run["head_sha"] ||
		runContext["workflow_file"] != target["workflow_file"] || runContext["repository"] != target["repository"] ||
		!Equal(runContext["workflow_run_id"], record["run_id"]) || !Equal(runContext["workflow_run_attempt"], record["attempt"]) ||
		runContext["run_name"] != run["display_title"] || runContext["recovery_key"] != attemptTarget["recovery_key"] || !Equal(runContext["attempt_target"], attemptTarget["identity"]) {
		return errors.New("undispatched no-op lacks its exact prepared empty source context")
	}
	for _, field := range []string{"publication", "parent_merge", "recovered_from_run_id", "recovered_from_attempt", "branch_cleanup_outcome"} {
		if _, exists := runContext[field]; exists {
			return errors.New("undispatched no-op cannot replace publication or recovery evidence")
		}
	}
	proofs, err := object(settled["policy_files"], "undispatched no-op files")
	if err != nil {
		return err
	}
	reader := &policyReader{proofs: proofs, used: map[string]bool{}}
	decision, decisionBytes, err := reader.read(noopDecisionPath, "original no-op decision")
	if err != nil {
		return err
	}
	_, eventBytes, err := reader.read("events/trigger-event.json", "original no-op event")
	if err != nil {
		return err
	}
	observation, observationBytes, err := reader.read("recovery-observation.json", "original no-op history observation")
	if err != nil {
		return err
	}
	if !Equal(observation["run"], run) || !Equal(observation["target"], target) {
		return errors.New("undispatched no-op history belongs to another source run or workflow")
	}
	source, sourceBytes, err := reader.read("events/workflow-source.json", "no-op control source")
	if err != nil {
		return err
	}
	rawSource, err := base64.StdEncoding.Strict().DecodeString(fmt.Sprint(source["content"]))
	if err != nil {
		return errors.New("undispatched no-op control source is malformed")
	}
	_, jobsBytes, err := reader.read("events/current-jobs.json", "exact no-op jobs")
	if err != nil {
		return err
	}
	data := Object{
		"status": "undispatched", "decision": decision["decision"], "proposal": decision["proposal"], "previews": decision["previews"],
		"decision_sha256": SHA256(decisionBytes), "event_sha256": SHA256(eventBytes), "observation_sha256": SHA256(observationBytes), "chain_sha256": observation["chain_sha256"],
		"workflow_api_sha256": SHA256(sourceBytes), "workflow_source_sha256": SHA256(rawSource), "jobs_sha256": SHA256(jobsBytes),
		"run_id": record["run_id"], "attempt": record["attempt"], "workflow_file": target["workflow_file"], "recovery_key": runContext["recovery_key"],
		"attempt_target": runContext["attempt_target"], "workflow_sha": run["head_sha"], "event_name": run["event"], "recovery_outcome": observation["outcome"],
	}
	if _, err := e.validateNoopEvidence(runContext, data, reader); err != nil {
		return err
	}
	return validatePolicyFilesUsed(proofs, reader)
}

func undispatchedNoopPrefix(record Object) (any, error) {
	settled, _ := object(record["settlement"], "settlement")
	proofs, _ := object(settled["policy_files"], "no-op files")
	value, err := LoadFileProof(proofs["recovery-observation.json"], "original no-op observation")
	if err != nil {
		return nil, err
	}
	observation, err := object(value, "original no-op observation")
	if err != nil {
		return nil, err
	}
	return observation["chain_sha256"], nil
}
