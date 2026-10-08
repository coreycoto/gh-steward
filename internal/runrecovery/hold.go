package runrecovery

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/coreycoto/gh-steward/internal/native"
)

func diagnosticArtifactName(target Object, id, attempt int64) string {
	stem := strings.TrimSuffix(strings.TrimSuffix(fmt.Sprint(target["workflow_file"]), ".yaml"), ".yml")
	return fmt.Sprintf("gh-steward-recovery-diagnostic-%s-run-%d-attempt-%d", stem, id, attempt)
}

// A stopped recovery invocation is a no-dispatch fact, never a completed plan
// or a substitute for the earlier attempt that caused its hold.
func (e *Engine) undispatchedHoldRecord(ctx context.Context, reader ActionsReader, run, packet, target, artifact Object, attempt int64, payload []byte) (Object, error) {
	files, err := archiveFiles(payload, false)
	if err != nil || len(files) != 2 || files["manifest.json"] == nil || files["recovery-report.json"] == nil {
		return nil, errors.New("recovery hold requires only its exact original manifest and report")
	}
	executed, ok := reader.(ExecutedWorkflowReader)
	if !ok {
		return nil, errors.New("recovery hold requires the authenticated executed-workflow witness")
	}
	id := mustPositive(run["id"])
	node, err := executed.WorkflowRunFile(ctx, fmt.Sprint(target["workflow_file"]), id, fmt.Sprint(packet["node_id"]))
	if err != nil {
		return nil, err
	}
	sha, err := native.WorkflowRunFileCommit(e.repository, fmt.Sprint(target["workflow_file"]), id, fmt.Sprint(packet["node_id"]), node)
	if err != nil {
		return nil, err
	}
	source, err := reader.Read(ctx, fmt.Sprintf("repos/%s/contents/.github/workflows/%s?ref=%s", e.repository.FullName(), target["workflow_file"], sha))
	if err != nil {
		return nil, err
	}
	pages, err := reader.Pages(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", e.repository.FullName(), id, attempt))
	if err != nil {
		return nil, err
	}
	proofs := Object{"diagnostic/manifest.json": MakeFileProof(files["manifest.json"]), "diagnostic/recovery-report.json": MakeFileProof(files["recovery-report.json"])}
	for path, value := range map[string]Object{"events/source-run.json": packet, "events/executed-workflow.json": node, "events/workflow-source.json": source, "events/current-jobs.json": {"run_id": id, "attempt": attempt, "pages": pages}} {
		raw, err := Canonical(value)
		if err != nil {
			return nil, err
		}
		proofs[path] = MakeFileProof(raw)
	}
	report, err := DecodeValue(files["recovery-report.json"])
	if err != nil {
		return nil, err
	}
	manifest, err := DecodeValue(files["manifest.json"])
	if err != nil {
		return nil, err
	}
	r, err := object(report, "original recovery report")
	if err != nil {
		return nil, err
	}
	m, err := object(manifest, "original diagnostic manifest")
	if err != nil {
		return nil, err
	}
	record := Object{"run_id": id, "attempt": attempt, "run": run, "artifact": artifact, "context_file": nil, "attempt_target": Object{"recovery_key": r["recovery_key"], "identity": Object{"event_sha256": m["raw_event_sha256"]}}, "settlement": Object{"kind": "undispatched_hold", "phase": "undispatched", "policy_files": proofs}}
	return e.ValidateSettlementRecord(record, target)
}

func (e *Engine) validateUndispatchedHold(record, target Object) error {
	settled, err := Exact(record["settlement"], []string{"kind", "phase", "policy_files"}, "undispatched recovery hold")
	if err != nil || settled["phase"] != "undispatched" || record["context_file"] != nil {
		return errors.New("recovery hold cannot invent a completed operation or context")
	}
	proofs, err := object(settled["policy_files"], "recovery hold proof files")
	if err != nil {
		return err
	}
	reader := &policyReader{proofs: proofs, used: map[string]bool{}}
	manifest, _, err := reader.read("diagnostic/manifest.json", "original recovery hold manifest")
	if err != nil {
		return err
	}
	manifest, err = Exact(manifest, []string{"schema_version", "diagnostic_only", "repository", "workflow_file", "run_name", "workflow_ref", "workflow_sha", "head_sha", "event_name", "raw_event_sha256", "run_id", "attempt", "report"}, "recovery hold manifest")
	if err != nil {
		return err
	}
	run, _ := object(record["run"], "source run")
	id, attempt := mustPositive(record["run_id"]), mustPositive(record["attempt"])
	workflow := fmt.Sprint(target["workflow_file"])
	if !exactInt(manifest["schema_version"], 1) || manifest["diagnostic_only"] != true || manifest["repository"] != e.repository.FullName() || manifest["workflow_file"] != workflow || manifest["run_name"] != run["display_title"] || manifest["head_sha"] != run["head_sha"] || manifest["event_name"] != run["event"] || !exactInt(manifest["run_id"], id) || !exactInt(manifest["attempt"], attempt) || !IsSHA256(manifest["raw_event_sha256"]) || manifest["workflow_ref"] != e.repository.FullName()+"/.github/workflows/"+workflow+"@refs/heads/"+fmt.Sprint(run["head_branch"]) {
		return errors.New("recovery hold manifest differs from its exact immutable invocation")
	}
	reportRef, err := Exact(manifest["report"], []string{"path", "sha256"}, "original recovery report reference")
	if err != nil {
		return err
	}
	report, reportBytes, err := reader.read("diagnostic/recovery-report.json", "original recovery report")
	if err != nil {
		return err
	}
	report, err = Exact(report, []string{"schema_version", "status", "workflow_file", "repository", "recovery_key", "reason"}, "original recovery report")
	if err != nil {
		return err
	}
	key, keyOK := report["recovery_key"].(string)
	reason, reasonOK := report["reason"].(string)
	if len(reportBytes) > 4096 || reportRef["path"] != "recovery-report.json" || reportRef["sha256"] != SHA256(reportBytes) || !exactInt(report["schema_version"], 1) || report["status"] != "recovery_needed" || report["repository"] != e.repository.FullName() || report["workflow_file"] != workflow || !keyOK || !contextKeyPattern.MatchString(key) || !reasonOK || len(reason) == 0 || len(reason) > 2048 || strings.ContainsAny(reason, "\r\n\x00") || !Equal(record["attempt_target"], Object{"recovery_key": key, "identity": Object{"event_sha256": manifest["raw_event_sha256"]}}) {
		return errors.New("recovery hold lost its original report or exact event target")
	}
	packet, _, err := reader.read("events/source-run.json", "exact terminal recovery hold run")
	if err != nil {
		return err
	}
	identity, err := NormalizeRun(packet)
	if err != nil || !Equal(identity, run) || packet["status"] != "completed" || !exactInt(packet["run_attempt"], attempt) {
		return errors.New("recovery hold lacks its exact terminal attempt")
	}
	node, _, err := reader.read("events/executed-workflow.json", "executed recovery hold workflow")
	if err != nil {
		return err
	}
	sha, err := native.WorkflowRunFileCommit(e.repository, workflow, id, fmt.Sprint(packet["node_id"]), node)
	definition, defErr := object(node["workflow"], "executed workflow")
	if err != nil || defErr != nil || !Equal(definition["databaseId"], target["workflow_id"]) || !exactInt(node["runAttempt"], attempt) || node["event"] != run["event"] || manifest["workflow_sha"] != sha {
		return errors.New("recovery hold source differs from its authenticated executed workflow")
	}
	source, _, err := reader.read("events/workflow-source.json", "qualified recovery hold source")
	if err != nil {
		return err
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(strings.ReplaceAll(fmt.Sprint(source["content"]), "\n", ""))
	if err != nil {
		return err
	}
	wf, err := e.workflowPolicy(workflow)
	if err != nil {
		return err
	}
	policy := wf.plans["workflow-noop"].approval.noop
	if policy == nil {
		return errors.New("recovery hold has no qualified exhaustive mutation source")
	}
	if digest := SHA256(raw); digest != policy.sourceSHA256 {
		policy = policy.previous[digest]
	}
	if policy == nil {
		return errors.New("recovery hold workflow source has not been explicitly qualified")
	}
	if err := validateNoopWorkflowSource(source, workflow, policy.sourceSHA256); err != nil {
		return err
	}
	jobs, _, err := reader.read("events/current-jobs.json", "complete recovery hold jobs")
	if err != nil {
		return err
	}
	if err := validateNoopJobs(jobs, policy, id, attempt); err != nil {
		return err
	}
	return validatePolicyFilesUsed(proofs, reader)
}
