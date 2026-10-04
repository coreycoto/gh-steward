package governance

import (
	"errors"
	"fmt"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// VerifyRecoveryAttestation only accepts an explicit historical no-write
// record whose exact run, artifact, sentinel and log identities are supplied.
// The caller provides a complete artifact inventory; this function does not
// inspect local files and cannot turn fresh failures into attestations.
func VerifyRecoveryAttestation(policy, snapshot contract.Object) (contract.Object, error) {
	expectedEvent, err := requiredString(policy, "expected_event")
	if err != nil {
		return nil, err
	}
	expectedPath, err := requiredString(policy, "expected_workflow_path")
	if err != nil {
		return nil, err
	}
	expectedPhase, err := requiredString(policy, "expected_phase")
	if err != nil {
		return nil, err
	}
	artifactName, err := requiredString(policy, "artifact_name")
	if err != nil {
		return nil, err
	}
	entry, err := object(snapshot, "attestation")
	if err != nil {
		return nil, err
	}
	repository, err := requiredString(entry, "repository")
	if err != nil || !repositoryNamePattern.MatchString(repository) {
		return nil, errors.New("recovery attestation repository identity is invalid")
	}
	prNumber, err := positiveInteger(entry, "pr_number")
	if err != nil {
		return nil, err
	}
	runID, err := positiveInteger(entry, "run_id")
	if err != nil {
		return nil, err
	}
	jobID, err := positiveInteger(entry, "job_id")
	if err != nil {
		return nil, err
	}
	artifactID, err := positiveInteger(entry, "artifact_id")
	if err != nil {
		return nil, err
	}
	runAttempt, err := positiveInteger(entry, "run_attempt")
	if err != nil || runAttempt != 1 {
		return nil, errors.New("only reviewed first-attempt preflight failures are supported")
	}
	phase, err := requiredString(entry, "phase")
	if err != nil || phase != expectedPhase {
		return nil, errors.New("recovery attestation phase is not supported by caller policy")
	}
	review, err := requiredString(entry, "review")
	if err != nil || strings.TrimSpace(review) == "" {
		return nil, errors.New("recovery attestation requires a review record")
	}
	headSHA, err := strictLowerSHA1(entry, "head_sha")
	if err != nil {
		return nil, err
	}
	baseSHA, err := strictLowerSHA1(entry, "base_sha")
	if err != nil {
		return nil, err
	}
	artifactDigest, ok := strictLowerSHA256(entry["artifact_digest"], true)
	if !ok {
		return nil, errors.New("recovery attestation artifact digest is invalid")
	}
	sentinelDigest, ok := strictLowerSHA256(entry["sentinel_sha256"], false)
	if !ok {
		return nil, errors.New("recovery attestation sentinel digest is invalid")
	}
	logDigest, ok := strictLowerSHA256(entry["log_sha256"], false)
	if !ok {
		return nil, errors.New("recovery attestation log digest is invalid")
	}

	run, err := object(snapshot, "run")
	if err != nil {
		return nil, err
	}
	if id, err := positiveInteger(run, "id"); err != nil || id != runID {
		return nil, errors.New("historical run identity/outcome differs from the reviewed attestation")
	}
	if attempt, err := positiveInteger(run, "run_attempt"); err != nil || attempt != 1 {
		return nil, errors.New("historical run identity/outcome differs from the reviewed attestation")
	}
	runHead, runHeadErr := strictLowerSHA1(run, "head_sha")
	status, _ := run["status"].(string)
	conclusion, _ := run["conclusion"].(string)
	event, _ := run["event"].(string)
	path, _ := run["path"].(string)
	if runHeadErr != nil || runHead != headSHA || event != expectedEvent || status != "completed" || conclusion != "failure" || path != expectedPath {
		return nil, errors.New("historical run identity/outcome differs from the reviewed attestation")
	}
	headRepository, err := object(run, "head_repository")
	if err != nil {
		return nil, err
	}
	runRepository, err := requiredString(headRepository, "full_name")
	if err != nil || runRepository != repository {
		return nil, errors.New("historical recovery repository mismatch")
	}
	pullRequests, err := objects(run, "pull_requests")
	if err != nil || len(pullRequests) != 1 {
		return nil, errors.New("historical recovery requires one exact pull request/base association")
	}
	linkedPR := pullRequests[0]
	linkedNumber, err := positiveInteger(linkedPR, "number")
	if err != nil || linkedNumber != prNumber {
		return nil, errors.New("historical recovery PR/base mismatch")
	}
	base, err := object(linkedPR, "base")
	if err != nil {
		return nil, err
	}
	linkedBase, linkedBaseErr := strictLowerSHA1(base, "sha")
	if linkedBaseErr != nil || linkedBase != baseSHA {
		return nil, errors.New("historical recovery PR/base mismatch")
	}

	listing, err := object(snapshot, "artifact_listing")
	if err != nil {
		return nil, err
	}
	artifacts, err := objects(listing, "artifacts")
	if err != nil || len(artifacts) != 1 {
		return nil, errors.New("historical recovery requires one preserved artifact")
	}
	artifact := artifacts[0]
	observedArtifactID, err := positiveInteger(artifact, "id")
	expired, expiredOK := artifact["expired"].(bool)
	observedDigest, digestOK := artifact["digest"].(string)
	name, nameOK := artifact["name"].(string)
	if err != nil || observedArtifactID != artifactID || !expiredOK || expired || !digestOK || observedDigest != artifactDigest || !nameOK || name != artifactName {
		return nil, errors.New("historical recovery artifact identity/digest mismatch")
	}

	files, err := object(snapshot, "artifact_files")
	if err != nil {
		return nil, err
	}
	complete, err := contract.Bool(files, "complete")
	if err != nil || !complete {
		return nil, errors.New("historical recovery artifact inventory is incomplete")
	}
	rootSymlink, err := contract.Bool(files, "root_is_symlink")
	if err != nil || rootSymlink {
		return nil, errors.New("historical recovery artifact root must not be a symlink")
	}
	entries, err := objects(files, "entries")
	if err != nil || len(entries) != 1 {
		return nil, errors.New("historical no-write artifact must contain only its sentinel")
	}
	file := entries[0]
	filePath, _ := file["path"].(string)
	kind, _ := file["kind"].(string)
	fileDigest, _ := file["sha256"].(string)
	symlink, symlinkOK := file["symlink"].(bool)
	if filePath != "recovery-state.json" || kind != "file" || !symlinkOK || symlink || fileDigest != sentinelDigest {
		return nil, errors.New("historical no-write artifact must contain only its reviewed sentinel")
	}
	sentinel, err := object(snapshot, "sentinel")
	if err != nil {
		return nil, err
	}
	if schema, err := contract.Integer(sentinel["schema_version"]); err != nil || schema != 1 || sentinel["state"] != "unknown_project_writes" || len(sentinel) != 2 {
		return nil, errors.New("historical recovery sentinel shape mismatch")
	}
	logSymlink, err := contract.Bool(snapshot, "log_is_symlink")
	if err != nil || logSymlink {
		return nil, errors.New("historical recovery job log must be a regular file")
	}
	providedLogDigest, ok := strictLowerSHA256(snapshot["log_sha256"], false)
	if !ok || providedLogDigest != logDigest {
		return nil, errors.New("historical recovery job log differs from the reviewed evidence")
	}

	return contract.Object{
		"verified": true, "state": "no_project_writes",
		"repository": repository, "pr_number": prNumber, "run_id": runID,
		"run_attempt": runAttempt, "job_id": jobID, "artifact_id": artifactID,
		"head_sha": headSHA, "base_sha": baseSHA, "phase": phase,
	}, nil
}

func strictLowerSHA1(o contract.Object, key string) (string, error) {
	value, ok := o[key].(string)
	if !ok || !sha1Pattern.MatchString(value) || strings.ToLower(value) != value {
		return "", fmt.Errorf("invalid recovery revision: %s", key)
	}
	return value, nil
}

func strictLowerSHA256(raw any, tagged bool) (string, bool) {
	value, ok := raw.(string)
	if !ok || strings.ToLower(value) != value {
		return "", false
	}
	if tagged {
		return value, sha256TaggedPattern.MatchString(value)
	}
	return value, sha256Pattern.MatchString(value)
}
