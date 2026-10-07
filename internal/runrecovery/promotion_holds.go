package runrecovery

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

const maxPromotionHeldAttempts = 16
const maxHeldDiagnosticBytes = 256 << 10
const heldAttemptDisposition = "diagnostic-only-no-dispatch"

var heldAttemptFields = []string{"schema_version", "disposition", "run", "attempt", "artifact", "archive_base64", "workflow_run", "executed_workflow", "workflow_source", "jobs"}

func diagnosticArtifactName(target Object, id int64) string {
	workflow := fmt.Sprint(target["workflow_file"])
	stem := strings.TrimSuffix(strings.TrimSuffix(workflow, ".yml"), ".yaml")
	return fmt.Sprintf("gh-steward-recovery-diagnostic-%s-run-%d-attempt-1", stem, id)
}

func heldRunIdentity(response, target Object) (Object, error) {
	run, err := NormalizeRun(response)
	if err != nil || !Equal(run["workflow_id"], target["workflow_id"]) || !exactInt(response["run_attempt"], 1) || response["status"] != "completed" || response["conclusion"] != "failure" || !nonemptyString(response["node_id"]) || response["path"] != ".github/workflows/"+fmt.Sprint(target["workflow_file"]) || (response["event"] != "workflow_dispatch" && response["event"] != "workflow_run") {
		return nil, recoveryError("held proof requires an exact failed first attempt from dispatch or workflow_run")
	}
	repository, err := object(response["repository"], "held run repository")
	base := fmt.Sprintf("%s/%s", target["server_url"], target["repository"])
	if err != nil || repository["full_name"] != target["repository"] || repository["html_url"] != base || response["html_url"] != fmt.Sprintf("%s/actions/runs/%d", base, mustPositive(run["id"])) {
		return nil, recoveryError("held run response belongs to another repository")
	}
	return run, nil
}

func heldWorkflowSourceDigest(packet Object) (string, error) {
	content, ok := packet["content"].(string)
	if !ok || len(content) > maxHeldDiagnosticBytes*2 {
		return "", recoveryError("held workflow source is missing or oversized")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(strings.ReplaceAll(content, "\n", ""))
	if err != nil || len(raw) > maxHeldDiagnosticBytes {
		return "", recoveryError("held workflow source is invalid or oversized")
	}
	return SHA256(raw), nil
}

func heldNoopPolicy(policy workflowPolicy, digest string) (*noopPolicy, error) {
	plan, ok := policy.plans["workflow-noop"]
	if !ok || plan.profile != "workflow-noop" || plan.approval.noop == nil {
		return nil, recoveryError("held proof requires the trusted exhaustive workflow-noop source policy")
	}
	source := plan.approval.noop
	if source.sourceSHA256 == digest {
		return source, nil
	}
	if previous := source.previous[digest]; previous != nil {
		return previous, nil
	}
	return nil, recoveryError("executed held workflow is outside the reviewed exhaustive mutation inventory")
}

// Retain the original diagnostic ZIP, not a fabricated context, plan or receipt.
// The two supported manifest layouts contain the same identity and report hash.
func validateHeldDiagnostic(payload []byte, target, run Object, workflowSHA, event string) error {
	if len(payload) > maxHeldDiagnosticBytes {
		return recoveryError("held diagnostic archive exceeds its explicit bound")
	}
	files, err := archiveFiles(payload, false)
	if err != nil || len(files) < 2 || len(files) > 3 || len(files["manifest.json"]) == 0 || len(files["recovery-report.json"]) == 0 {
		return recoveryError("held diagnostic must retain only its manifest, report and optional checksums")
	}
	for name, raw := range files {
		if (name != "manifest.json" && name != "recovery-report.json" && name != "SHA256SUMS") || len(raw) > 64<<10 {
			return recoveryError("held diagnostic contains execution evidence, an unsupported file or oversized data")
		}
	}
	value, err := DecodeValue(files["manifest.json"])
	manifest, objectErr := object(value, "held diagnostic manifest")
	if err != nil || objectErr != nil {
		return recoveryError("held diagnostic manifest is malformed")
	}
	var source Object
	if _, nested := manifest["source"]; nested {
		manifest, err = Exact(manifest, []string{"schema_version", "diagnostic_only", "kind", "report", "run", "source"}, "held nested diagnostic manifest")
		if err != nil || !nonemptyString(manifest["kind"]) {
			return recoveryError("held nested diagnostic manifest has an unsupported shape")
		}
		owner, err := Exact(manifest["run"], []string{"id", "attempt"}, "held diagnostic run")
		if err != nil || !Equal(owner["id"], run["id"]) || !exactInt(owner["attempt"], 1) {
			return recoveryError("held diagnostic manifest belongs to another exact attempt")
		}
		source, err = Exact(manifest["source"], []string{"repository", "workflow_file", "workflow_name", "workflow_ref", "workflow_sha", "head_sha", "event_name", "raw_event_sha256"}, "held diagnostic source")
		if err != nil || !nonemptyString(source["workflow_name"]) {
			return recoveryError("held diagnostic source is incomplete")
		}
	} else {
		manifest, err = Exact(manifest, []string{"schema_version", "diagnostic_only", "repository", "workflow_file", "run_name", "workflow_ref", "workflow_sha", "head_sha", "event_name", "raw_event_sha256", "run_id", "attempt", "report"}, "held flat diagnostic manifest")
		if err != nil || !Equal(manifest["run_id"], run["id"]) || !exactInt(manifest["attempt"], 1) || manifest["run_name"] != run["display_title"] {
			return recoveryError("held diagnostic manifest belongs to another exact attempt")
		}
		source = manifest
	}
	ref, ok := source["workflow_ref"].(string)
	refPrefix := fmt.Sprintf("%s/.github/workflows/%s@refs/", target["repository"], target["workflow_file"])
	if !exactInt(manifest["schema_version"], 1) || manifest["diagnostic_only"] != true || source["repository"] != target["repository"] || source["workflow_file"] != target["workflow_file"] || source["workflow_sha"] != workflowSHA || source["head_sha"] != run["head_sha"] || source["event_name"] != event || !IsSHA256(source["raw_event_sha256"]) || !ok || !strings.HasPrefix(ref, refPrefix) || len(ref) <= len(refPrefix) || strings.ContainsAny(ref, "\r\n") {
		return recoveryError("held diagnostic differs from its authenticated workflow and run identity")
	}
	reportRef, err := Exact(manifest["report"], []string{"path", "sha256"}, "held diagnostic report reference")
	if err != nil || reportRef["path"] != "recovery-report.json" || reportRef["sha256"] != SHA256(files["recovery-report.json"]) {
		return recoveryError("held diagnostic report bytes differ from the retained manifest")
	}
	value, err = DecodeValue(files["recovery-report.json"])
	report, shapeErr := Exact(value, []string{"schema_version", "status", "reason", "repository", "workflow_file", "recovery_key"}, "held diagnostic report")
	if err != nil || shapeErr != nil || !exactInt(report["schema_version"], 1) || report["status"] != "recovery_needed" || !nonemptyString(report["reason"]) || !nonemptyString(report["recovery_key"]) || report["repository"] != target["repository"] || report["workflow_file"] != target["workflow_file"] {
		return recoveryError("held diagnostic report is not an exact recovery hold")
	}
	if checksums, exists := files["SHA256SUMS"]; exists {
		seen := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(string(checksums)), "\n") {
			parts := strings.Fields(line)
			if len(parts) != 2 || (parts[1] != "manifest.json" && parts[1] != "recovery-report.json") || seen[parts[1]] || parts[0] != SHA256(files[parts[1]]) {
				return recoveryError("held diagnostic checksums are incomplete or disagree with exact bytes")
			}
			seen[parts[1]] = true
		}
		// Some diagnostic producers write report-only checksums before the
		// manifest. The authenticated ZIP digest already binds both raw files;
		// the manifest also binds the report. Every declared checksum must match.
		if !seen["recovery-report.json"] {
			return recoveryError("held diagnostic checksums are incomplete")
		}
	}
	return nil
}

func validateHeldAttempt(value any, target Object, policy *workflowPolicy) (Object, error) {
	proof, err := Exact(value, heldAttemptFields, "reviewed held attempt")
	if err != nil || !exactInt(proof["schema_version"], 1) || !exactInt(proof["attempt"], 1) || proof["disposition"] != heldAttemptDisposition {
		return nil, recoveryError("held proof has an unsupported schema, attempt or disposition")
	}
	response, err := object(proof["workflow_run"], "held workflow run")
	run, runErr := heldRunIdentity(response, target)
	if err != nil || runErr != nil || !Equal(proof["run"], run) {
		return nil, recoveryError("held proof differs from its exact immutable run")
	}
	id, workflow := mustPositive(run["id"]), fmt.Sprint(target["workflow_file"])
	node, err := object(proof["executed_workflow"], "held executed workflow")
	repository, repoErr := contract.ParseRepository(Object{"nameWithOwner": target["repository"], "url": fmt.Sprintf("%s/%s", target["server_url"], target["repository"])})
	if err != nil || repoErr != nil {
		return nil, recoveryError("held executed workflow witness is malformed")
	}
	sha, err := native.WorkflowRunFileCommit(repository, workflow, id, fmt.Sprint(response["node_id"]), node)
	definition, _ := object(node["workflow"], "held workflow definition")
	if err != nil || !exactInt(node["runAttempt"], 1) || node["event"] != response["event"] || !Equal(definition["databaseId"], target["workflow_id"]) {
		return nil, recoveryError("held executed workflow witness differs from the exact first attempt")
	}
	source, err := object(proof["workflow_source"], "held workflow source")
	digest, digestErr := heldWorkflowSourceDigest(source)
	if err != nil || digestErr != nil || validateNoopWorkflowSource(source, workflow, digest) != nil {
		return nil, recoveryError("held executed workflow source bytes are unqualified")
	}
	artifact, err := Exact(proof["artifact"], []string{"id", "name", "digest"}, "held diagnostic upload identity")
	if err != nil || artifact["name"] != diagnosticArtifactName(target, id) {
		return nil, recoveryError("held proof has a foreign diagnostic upload")
	}
	if _, err := positiveInteger(artifact["id"], "held diagnostic upload ID"); err != nil {
		return nil, err
	}
	encoded, ok := proof["archive_base64"].(string)
	if !ok || len(encoded) > base64.StdEncoding.EncodedLen(maxHeldDiagnosticBytes) {
		return nil, recoveryError("held diagnostic archive is missing or oversized")
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || artifact["digest"] != "sha256:"+SHA256(payload) || validateHeldDiagnostic(payload, target, run, sha, fmt.Sprint(response["event"])) != nil {
		return nil, recoveryError("held diagnostic archive bytes or identity are invalid")
	}
	jobs, err := Exact(proof["jobs"], []string{"run_id", "attempt", "pages"}, "held jobs proof")
	if err != nil || !exactInt(jobs["run_id"], id) || !exactInt(jobs["attempt"], 1) {
		return nil, recoveryError("held jobs proof belongs to another attempt")
	}
	pages, err := array(jobs["pages"], "held jobs pages")
	rows, rowsErr := completePages(pages, "jobs")
	if err != nil || rowsErr != nil || len(rows) == 0 || len(rows) > 256 {
		return nil, recoveryError("held jobs inventory is incomplete or oversized")
	}
	for _, row := range rows {
		if !exactInt(row["run_id"], id) || !exactInt(row["run_attempt"], 1) || row["status"] != "completed" || !nonemptyString(row["conclusion"]) || row["conclusion"] == "cancelled" {
			return nil, recoveryError("held jobs proof includes active, cancelled or foreign jobs")
		}
	}
	if policy != nil {
		qualified, err := heldNoopPolicy(*policy, digest)
		if err != nil {
			return nil, err
		}
		if err := validateNoopJobs(jobs, qualified, id, 1); err != nil {
			return nil, err
		}
	}
	return proof, nil
}

func promotionHeldAttempts(p Object) []Object {
	rows, _ := objectArray(p["held_attempts"], "promotion held attempts")
	return rows
}

func validatePromotionHeldAttempts(p Object, policy *workflowPolicy) error {
	if !exactInt(p["schema_version"], 3) {
		return nil
	}
	rows, err := objectArray(p["held_attempts"], "promotion held attempts")
	if err != nil || len(rows) == 0 || len(rows) > maxPromotionHeldAttempts {
		return recoveryError("held promotion requires a bounded nonempty exact attempt inventory")
	}
	checkpoint, target := p["preview_checkpoint"].(Object), p["target"].(Object)
	baseline, err := ValidateHistoryCutover(checkpoint["history_cutover"])
	if err != nil {
		return err
	}
	covered, _, err := promotionCheckpointHistory(checkpoint, baseline)
	if err != nil {
		return err
	}
	previous := int64(0)
	for _, row := range rows {
		proof, err := validateHeldAttempt(row, target, policy)
		if err != nil {
			return err
		}
		id := mustPositive(proof["run"].(Object)["id"])
		if id <= previous {
			return recoveryError("held attempt inventory is repeated or unordered")
		}
		previous = id
		for _, run := range covered {
			if exactInt(run["id"], id) {
				return recoveryError("held proof cannot reclassify quarantined history or a native settlement")
			}
		}
	}
	return nil
}

func (e *Engine) captureHeldAttempt(ctx context.Context, reader ActionsReader, target Object, id int64, artifacts []Object) (Object, error) {
	sourceReader, ok := reader.(ExecutedWorkflowReader)
	if !ok {
		return nil, recoveryError("held proof requires authenticated executed-workflow file discovery")
	}
	workflow := fmt.Sprint(target["workflow_file"])
	response, err := reader.Read(ctx, fmt.Sprintf("repos/%s/actions/runs/%d", e.repository.FullName(), id))
	run, runErr := heldRunIdentity(response, target)
	if err != nil || runErr != nil || !exactInt(run["id"], id) {
		return nil, recoveryError("held run identity could not be verified")
	}
	node, err := sourceReader.WorkflowRunFile(ctx, workflow, id, fmt.Sprint(response["node_id"]))
	if err != nil {
		return nil, recoveryError("held executed-workflow witness could not be verified")
	}
	sha, err := native.WorkflowRunFileCommit(e.repository, workflow, id, fmt.Sprint(response["node_id"]), node)
	if err != nil {
		return nil, err
	}
	source, err := reader.Read(ctx, fmt.Sprintf("repos/%s/contents/.github/workflows/%s?ref=%s", e.repository.FullName(), workflow, sha))
	if err != nil {
		return nil, recoveryError("held executed workflow bytes could not be read")
	}
	var metadata Object
	for _, row := range artifacts {
		owner, _ := object(row["workflow_run"], "held upload owner")
		name, _ := row["name"].(string)
		if exactInt(owner["id"], id) && strings.HasPrefix(name, "gh-steward-recovery-") && name != diagnosticArtifactName(target, id) {
			return nil, recoveryError("held attempt has competing native execution evidence")
		}
		if name == diagnosticArtifactName(target, id) {
			if metadata != nil {
				return nil, recoveryError("held diagnostic upload identity is ambiguous")
			}
			metadata = row
		}
	}
	artifact, err := artifactIdentity(metadata, diagnosticArtifactName(target, id), id)
	if err != nil {
		return nil, recoveryError("held diagnostic upload is missing, expired or unqualified")
	}
	owner, _ := object(metadata["workflow_run"], "held diagnostic owner")
	if owner["head_sha"] != run["head_sha"] {
		return nil, recoveryError("held diagnostic upload differs from its immutable run source")
	}
	if size, exists := metadata["size_in_bytes"]; exists {
		n, err := contract.Integer(size)
		if err != nil || n < 0 || n > maxHeldDiagnosticBytes {
			return nil, recoveryError("held diagnostic upload exceeds its size bound")
		}
	}
	payload, err := downloadArtifact(ctx, reader, artifact)
	if err != nil {
		return nil, recoveryError("held diagnostic upload bytes could not be verified")
	}
	pages, err := reader.Pages(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/1/jobs?per_page=100", e.repository.FullName(), id))
	if err != nil {
		return nil, recoveryError("held complete jobs could not be read")
	}
	proof := Object{"schema_version": int64(1), "disposition": heldAttemptDisposition, "run": run, "attempt": int64(1), "artifact": artifact, "archive_base64": base64.StdEncoding.EncodeToString(payload), "workflow_run": response, "executed_workflow": node, "workflow_source": source, "jobs": Object{"run_id": id, "attempt": int64(1), "pages": pages}}
	proof, err = cloneObject(proof)
	if err != nil {
		return nil, err
	}
	policy := e.workflows[workflow]
	return validateHeldAttempt(proof, target, &policy)
}

// Raw responses remain sealed. Fresh checks compare only required run identity
// and terminal status, not unrelated mutable repository metadata in that read.
func heldAttemptComparison(proof Object) Object {
	result := Object{}
	for key, value := range proof {
		if key != "workflow_run" && key != "workflow_source" {
			result[key] = value
		}
	}
	run := proof["workflow_run"].(Object)
	result["workflow_run"] = Object{"run": proof["run"], "node_id": run["node_id"], "event": run["event"], "path": run["path"], "status": run["status"], "conclusion": run["conclusion"], "run_attempt": run["run_attempt"]}
	// Private Contents responses include expiring download URLs. The typed
	// executed-file witness, exact Git blob and content bytes identify source;
	// download transport metadata is retained but grants no source identity.
	source := proof["workflow_source"].(Object)
	digest, _ := heldWorkflowSourceDigest(source)
	result["workflow_source"] = Object{"type": source["type"], "path": source["path"], "git_blob_sha": source["sha"], "content_sha256": digest}
	return result
}

func (e *Engine) recheckPromotionHeldAttempts(ctx context.Context, reader ActionsReader, p Object) error {
	rows := promotionHeldAttempts(p)
	if len(rows) == 0 {
		return nil
	}
	artifacts, err := CompleteRepositoryArtifacts(ctx, reader, e.repository)
	if err != nil {
		return recoveryError("held promotion upload inventory could not be rechecked")
	}
	for _, proof := range rows {
		live, err := e.captureHeldAttempt(ctx, reader, p["target"].(Object), mustPositive(proof["run"].(Object)["id"]), artifacts)
		if err != nil {
			return recoveryError("held promotion evidence could not be rechecked: %s", err)
		}
		if !Equal(heldAttemptComparison(live), heldAttemptComparison(proof)) {
			return recoveryError("held promotion evidence differs from the exact reviewed proof")
		}
	}
	return nil
}

func capturePromotionHoldIDs(ids []int64) ([]int64, error) {
	ordered := append([]int64{}, ids...)
	if len(ordered) > maxPromotionHeldAttempts {
		return nil, recoveryError("promotion accepts at most sixteen exact held first attempts")
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for i, id := range ordered {
		if id < 1 || (i > 0 && id == ordered[i-1]) {
			return nil, recoveryError("promotion held run IDs must be positive and unique")
		}
	}
	return ordered, nil
}
