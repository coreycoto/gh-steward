package runrecovery

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

const (
	legacyEvidenceDomain = "gh-steward-legacy-evidence-v1\n"
	legacyMaxRows        = 256
	legacyMaxSources     = 512
	legacyMaxJobs        = 1024
	legacyMaxProofBytes  = 32 * 1024 * 1024
)

var (
	legacyEvidenceFields = []string{"schema_version", "payload", "signature"}
	legacyReviewFields   = []string{"schema_version", "target", "run_id", "attempt", "evidence_sha256", "archive_public_key", "workflow_sha", "sources", "mutators", "non_mutator_jobs", "artifacts"}
	legacyPayloadFields  = []string{"schema_version", "target", "run", "attempt", "attempt_target", "run_packet", "jobs_packet", "workflow_sha", "event_file", "inputs", "sources", "intended_effects", "receipts", "observations", "artifacts"}
	legacyReportFields   = []string{"schema_version", "target", "run", "attempt", "attempt_target", "classification", "reasons", "evidence", "review", "intent_proven", "sha256"}
	legacySHA40Pattern   = settlementSHA40
	legacyEffectPattern  = settlementArtifactDigest
)

type legacyReview struct {
	object        Object
	sources       []Object
	mutators      []Object
	nonMutator    []string
	artifacts     []Object
	archivePubKey ed25519.PublicKey
}

type legacyJobEvidence struct {
	complete   bool
	noDispatch bool
}

// VerifyLegacyEvidence verifies one signed, exact-attempt archival packet and
// its retained human review. It performs no source or provider reads.
func VerifyLegacyEvidence(evidence Object, review Object) (Object, error) {
	if evidence == nil || review == nil {
		return nil, errors.New("legacy evidence and review must be objects")
	}
	root, err := Exact(evidence, legacyEvidenceFields, "legacy signed evidence")
	if err != nil || !exactInt(root["schema_version"], 1) {
		return nil, errors.New("legacy signed evidence has an unsupported shape or schema")
	}
	payload, err := Exact(root["payload"], legacyPayloadFields, "legacy evidence payload")
	if err != nil || !exactInt(payload["schema_version"], 1) {
		return nil, errors.New("legacy evidence payload has an unsupported shape or schema")
	}
	payloadBytes, err := Canonical(payload)
	if err != nil || len(payloadBytes) > MaxFileBytes {
		return nil, errors.New("legacy evidence payload exceeds the 8 MiB retained-file bound")
	}

	checkedReview, err := validateLegacyReview(review)
	if err != nil {
		return nil, err
	}
	evidenceBytes, err := Canonical(evidence)
	if err != nil || len(evidenceBytes) > MaxFileBytes {
		return nil, errors.New("legacy signed evidence exceeds the 8 MiB retained-file bound")
	}
	if !IsSHA256(checkedReview.object["evidence_sha256"]) || SHA256(evidenceBytes) != checkedReview.object["evidence_sha256"] {
		return nil, errors.New("legacy review is not bound to this exact signed evidence packet")
	}
	signatureText, ok := root["signature"].(string)
	if !ok || strings.ContainsAny(signatureText, "\r\n") {
		return nil, errors.New("legacy evidence signature is malformed")
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(signatureText)
	if err != nil || base64.StdEncoding.EncodeToString(signature) != signatureText || len(signature) != ed25519.SignatureSize {
		return nil, errors.New("legacy evidence signature is not canonical Ed25519 data")
	}
	if !ed25519.Verify(checkedReview.archivePubKey, append([]byte(legacyEvidenceDomain), payloadBytes...), signature) {
		return nil, errors.New("legacy evidence signature does not verify against the reviewed archival key")
	}

	target, err := ValidateTarget(payload["target"])
	if err != nil {
		return nil, fmt.Errorf("legacy target is invalid: %w", err)
	}
	if !Equal(target, checkedReview.object["target"]) {
		return nil, errors.New("legacy review target differs from the signed evidence")
	}
	run, err := NormalizeRun(payload["run"])
	if err != nil {
		return nil, fmt.Errorf("legacy immutable run is invalid: %w", err)
	}
	attempt, err := positiveInteger(payload["attempt"], "legacy attempt")
	if err != nil {
		return nil, err
	}
	runID, _ := positiveInteger(run["id"], "legacy run ID")
	targetWorkflowID, _ := positiveInteger(target["workflow_id"], "legacy target workflow ID")
	runWorkflowID, _ := positiveInteger(run["workflow_id"], "legacy run workflow ID")
	fullRun, err := object(payload["run"], "legacy full API run")
	if err != nil || !exactInt(fullRun["run_attempt"], attempt) || fullRun["status"] != "completed" {
		return nil, errors.New("legacy full API run is not the reviewed completed attempt")
	}
	if runID != mustPositive(checkedReview.object["run_id"]) || attempt != mustPositive(checkedReview.object["attempt"]) ||
		runWorkflowID != targetWorkflowID {
		return nil, errors.New("legacy review, run, target or attempt identity differs")
	}
	workflowSHA, ok := payload["workflow_sha"].(string)
	if !ok || !legacySHA40Pattern.MatchString(workflowSHA) || workflowSHA != checkedReview.object["workflow_sha"] {
		return nil, errors.New("legacy workflow source SHA differs from its exact review")
	}
	packet, err := object(payload["run_packet"], "legacy exact-attempt run packet")
	if err != nil {
		return nil, err
	}
	packetIdentity, err := NormalizeRun(packet)
	if err != nil || !Equal(packetIdentity, run) || !exactInt(packet["run_attempt"], attempt) || packet["status"] != "completed" {
		return nil, errors.New("legacy exact-attempt API packet differs from the reviewed completed run")
	}
	attemptTarget, err := validateAttemptTarget(payload["attempt_target"], false)
	if err != nil {
		return nil, fmt.Errorf("legacy attempt target is invalid: %w", err)
	}

	sourceState, err := validateLegacySources(payload["sources"], checkedReview, target, payload["workflow_sha"])
	if err != nil {
		return nil, err
	}
	eventValid, err := validateLegacyEvent(payload["event_file"], payload["inputs"], run, target)
	if err != nil {
		return nil, err
	}
	effectHashes, effectsValid, err := validateLegacyEffects(payload["intended_effects"])
	if err != nil {
		return nil, err
	}
	artifactValid, err := validateLegacyArtifacts(payload["artifacts"], checkedReview, run, runID, attempt)
	if err != nil {
		return nil, err
	}
	jobs, err := validateLegacyJobs(payload["jobs_packet"], checkedReview, runID, attempt)
	if err != nil {
		return nil, err
	}
	intentProven := sourceState && eventValid && effectsValid && jobs.complete && artifactValid

	receiptState, err := validateLegacyReceipts(payload["receipts"], target, runID, attempt, attemptTarget, effectHashes, effectsValid)
	if err != nil {
		return nil, err
	}
	observed, err := validateLegacyObservations(payload["observations"], effectHashes, effectsValid)
	if err != nil {
		return nil, err
	}
	for effect := range receiptState.sdkObserved {
		observed[effect] = true
	}
	if jobs.noDispatch && (receiptState.hasProviderReceipt || len(observed) != 0) {
		return nil, errors.New("legacy effect evidence conflicts with the exhaustive skipped-mutator proof")
	}

	classification := "unresolved"
	reasons := make([]any, 0, 8)
	switch {
	case intentProven && jobs.noDispatch:
		classification = "no_dispatch_proven"
	case intentProven && receiptState.complete:
		classification = "terminal_receipt_proven"
	case intentProven && len(observed) != 0:
		classification = "effect_observed"
	default:
		if !sourceState {
			reasons = append(reasons, "authenticated_source_closure_incomplete")
		}
		if !eventValid {
			reasons = append(reasons, "original_event_or_inputs_missing")
		}
		if !effectsValid {
			reasons = append(reasons, "intended_effects_missing")
		}
		if !jobs.complete {
			reasons = append(reasons, "complete_jobs_missing")
		}
		if !artifactValid {
			reasons = append(reasons, "artifact_witnesses_missing")
		}
		if intentProven && !receiptState.complete && len(observed) == 0 {
			reasons = append(reasons, "complete_terminal_receipts_missing")
		} else if intentProven && !receiptState.complete && len(observed) != 0 {
			reasons = append(reasons, "terminal_receipts_incomplete_observation_retained")
		}
	}

	reportEvidence, err := cloneObject(evidence)
	if err != nil {
		return nil, err
	}
	reportReview, err := cloneObject(review)
	if err != nil {
		return nil, err
	}
	reportTarget, err := cloneObject(target)
	if err != nil {
		return nil, err
	}
	reportRun, err := cloneObject(run)
	if err != nil {
		return nil, err
	}
	reportAttemptTarget, err := cloneObject(attemptTarget)
	if err != nil {
		return nil, err
	}
	report := Object{
		"schema_version": int64(1), "target": reportTarget, "run": reportRun, "attempt": attempt,
		"attempt_target": reportAttemptTarget, "classification": classification, "reasons": reasons,
		"evidence": reportEvidence, "review": reportReview, "intent_proven": intentProven,
	}
	unsigned, err := Canonical(report)
	if err != nil || len(unsigned) > MaxCheckpointBytes {
		return nil, errors.New("legacy verification report exceeds the 8 MiB checkpoint bound")
	}
	report["sha256"] = SHA256(unsigned)
	complete, err := Canonical(report)
	if err != nil || len(complete) > MaxCheckpointBytes {
		return nil, errors.New("legacy verification report with its digest exceeds the 8 MiB checkpoint bound")
	}
	return report, nil
}

// ValidateLegacyReport re-verifies the signed evidence and compares every
// canonical report field, including its self-hash.
func ValidateLegacyReport(value any) (Object, error) {
	report, err := Exact(value, legacyReportFields, "legacy evidence report")
	if err != nil || !exactInt(report["schema_version"], 1) {
		return nil, errors.New("legacy evidence report has an unsupported shape or schema")
	}
	canonical, err := Canonical(report)
	if err != nil || len(canonical) > MaxCheckpointBytes {
		return nil, errors.New("legacy evidence report exceeds the 8 MiB checkpoint bound")
	}
	evidence, err := object(report["evidence"], "legacy report evidence")
	if err != nil {
		return nil, err
	}
	review, err := object(report["review"], "legacy report review")
	if err != nil {
		return nil, err
	}
	verified, err := VerifyLegacyEvidence(evidence, review)
	if err != nil {
		return nil, err
	}
	if !Equal(report, verified) {
		return nil, errors.New("legacy evidence report differs from its recomputed canonical report")
	}
	return verified, nil
}

// LegacyIntendedEffects returns only effects from an authenticated report that
// proved the complete original intent and its exact-attempt witnesses.
func LegacyIntendedEffects(report Object) ([]Object, error) {
	verified, err := ValidateLegacyReport(report)
	if err != nil {
		return nil, err
	}
	if verified["intent_proven"] != true {
		return nil, errors.New("legacy report does not prove complete original intent")
	}
	payload := verified["evidence"].(Object)["payload"].(Object)
	rows := payload["intended_effects"].([]any)
	effects := make([]Object, 0, len(rows))
	for _, row := range rows {
		copyEffect, err := cloneObject(row.(Object))
		if err != nil {
			return nil, err
		}
		effects = append(effects, copyEffect)
	}
	return effects, nil
}

func validateLegacyReview(value Object) (legacyReview, error) {
	review, err := Exact(value, legacyReviewFields, "legacy exact-attempt review")
	if err != nil || !exactInt(review["schema_version"], 1) {
		return legacyReview{}, errors.New("legacy review has an unsupported shape or schema")
	}
	if _, err := ValidateTarget(review["target"]); err != nil {
		return legacyReview{}, fmt.Errorf("legacy review target is invalid: %w", err)
	}
	if _, err := positiveInteger(review["run_id"], "legacy review run ID"); err != nil {
		return legacyReview{}, err
	}
	if _, err := positiveInteger(review["attempt"], "legacy review attempt"); err != nil {
		return legacyReview{}, err
	}
	if !IsSHA256(review["evidence_sha256"]) {
		return legacyReview{}, errors.New("legacy review evidence digest is invalid")
	}
	keyText, ok := review["archive_public_key"].(string)
	if !ok || strings.ContainsAny(keyText, "\r\n") {
		return legacyReview{}, errors.New("legacy reviewed archival public key is malformed")
	}
	key, err := base64.StdEncoding.Strict().DecodeString(keyText)
	if err != nil || base64.StdEncoding.EncodeToString(key) != keyText || len(key) != ed25519.PublicKeySize {
		return legacyReview{}, errors.New("legacy reviewed archival key is not canonical Ed25519 data")
	}
	workflowSHA, ok := review["workflow_sha"].(string)
	if !ok || !legacySHA40Pattern.MatchString(workflowSHA) {
		return legacyReview{}, errors.New("legacy reviewed workflow SHA is invalid")
	}
	reviewBytes, err := Canonical(review)
	if err != nil || len(reviewBytes) > MaxFileBytes {
		return legacyReview{}, errors.New("legacy review exceeds the 8 MiB retained-file bound")
	}

	sourceRows, err := array(review["sources"], "reviewed legacy sources")
	if err != nil || len(sourceRows) > legacyMaxSources {
		return legacyReview{}, errors.New("legacy review source inventory is malformed or oversized")
	}
	sources := make([]Object, 0, len(sourceRows))
	seenSources := map[string]bool{}
	for _, raw := range sourceRows {
		entry, err := Exact(raw, []string{"path", "sha256"}, "reviewed legacy source")
		if err != nil {
			return legacyReview{}, err
		}
		path, ok := entry["path"].(string)
		if !ok || !legacySafeSourcePath(path) || !IsSHA256(entry["sha256"]) || seenSources[path] {
			return legacyReview{}, errors.New("legacy review source path or digest is invalid or duplicated")
		}
		seenSources[path] = true
		sources = append(sources, entry)
	}

	mutatorRows, err := array(review["mutators"], "reviewed legacy mutators")
	if err != nil || len(mutatorRows) > legacyMaxJobs {
		return legacyReview{}, errors.New("legacy review mutator inventory is malformed or oversized")
	}
	mutators := make([]Object, 0, len(mutatorRows))
	jobNames := map[string]bool{}
	stepCount := 0
	for _, raw := range mutatorRows {
		entry, err := Exact(raw, []string{"job", "steps"}, "reviewed legacy mutator")
		if err != nil {
			return legacyReview{}, err
		}
		name, ok := entry["job"].(string)
		steps, err := legacyUniqueStrings(entry["steps"], "reviewed legacy mutation steps", true)
		if !ok || !nonemptyString(name) || jobNames[name] || err != nil || len(steps) > 512 {
			return legacyReview{}, errors.New("legacy review mutator identity or step inventory is invalid")
		}
		jobNames[name] = true
		stepCount += len(steps)
		mutators = append(mutators, Object{"job": name, "steps": steps})
	}
	if stepCount > legacyMaxJobs*512 {
		return legacyReview{}, errors.New("legacy review mutation-step inventory is oversized")
	}
	nonMutator, err := legacyUniqueStrings(review["non_mutator_jobs"], "reviewed legacy non-mutator jobs", false)
	if err != nil || len(nonMutator) > legacyMaxJobs || len(jobNames)+len(nonMutator) > legacyMaxJobs {
		return legacyReview{}, errors.New("legacy review non-mutator inventory is malformed or oversized")
	}
	for _, name := range nonMutator {
		if jobNames[name] {
			return legacyReview{}, errors.New("legacy review job inventory repeats a mutator job")
		}
		jobNames[name] = true
	}

	artifactRows, err := array(review["artifacts"], "reviewed legacy artifacts")
	if err != nil || len(artifactRows) > legacyMaxRows {
		return legacyReview{}, errors.New("legacy review artifact inventory is malformed or oversized")
	}
	artifacts := make([]Object, 0, len(artifactRows))
	seenArtifactIDs, seenArtifactNames := map[int64]bool{}, map[string]bool{}
	for _, raw := range artifactRows {
		entry, err := Exact(raw, []string{"name", "id", "digest"}, "reviewed legacy artifact")
		if err != nil {
			return legacyReview{}, err
		}
		name, ok := entry["name"].(string)
		id, idErr := positiveInteger(entry["id"], "reviewed legacy artifact ID")
		digest, digestOK := entry["digest"].(string)
		if !ok || !nonemptyString(name) || idErr != nil || !digestOK || !legacyEffectPattern.MatchString(digest) || seenArtifactIDs[id] || seenArtifactNames[name] {
			return legacyReview{}, errors.New("legacy review artifact identity is invalid or duplicated")
		}
		seenArtifactIDs[id], seenArtifactNames[name] = true, true
		artifacts = append(artifacts, entry)
	}
	return legacyReview{object: review, sources: sources, mutators: mutators, nonMutator: nonMutator, artifacts: artifacts, archivePubKey: ed25519.PublicKey(key)}, nil
}

func legacyUniqueStrings(value any, name string, nonempty bool) ([]string, error) {
	values, err := stringsArray(value, name, nonempty)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(values))
	for _, item := range values {
		if seen[item] {
			return nil, errors.New("legacy review contains a duplicate string identity")
		}
		seen[item] = true
	}
	return values, nil
}

func legacySafeSourcePath(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\\r\n\x00") || strings.HasPrefix(value, "/") || strings.Contains(value, "//") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validateLegacySources(value any, review legacyReview, target Object, workflowSHA any) (bool, error) {
	rows, err := array(value, "legacy authenticated source closure")
	if err != nil || len(rows) > legacyMaxSources {
		return false, errors.New("legacy authenticated source inventory is malformed or oversized")
	}
	sources := make([]Object, 0, len(rows))
	reviewRows := make([]Object, 0, len(rows))
	seen := map[string]bool{}
	var totalBytes int64
	rootPath := ".github/workflows/" + target["workflow_file"].(string)
	rootFound := false
	for _, raw := range rows {
		entry, err := Exact(raw, []string{"path", "commit", "file"}, "legacy authenticated source")
		if err != nil {
			return false, err
		}
		path, ok := entry["path"].(string)
		commit, commitOK := entry["commit"].(string)
		if !ok || !legacySafeSourcePath(path) || !commitOK || !legacySHA40Pattern.MatchString(commit) || seen[path] {
			return false, errors.New("legacy authenticated source identity is invalid or duplicated")
		}
		seen[path] = true
		rawBytes, err := LoadRawFileProof(entry["file"], "legacy authenticated source")
		if err != nil {
			return false, err
		}
		totalBytes += int64(len(rawBytes))
		if totalBytes > legacyMaxProofBytes {
			return false, errors.New("legacy authenticated sources exceed their aggregate byte bound")
		}
		proof, _ := object(entry["file"], "legacy source file proof")
		sources = append(sources, entry)
		reviewRows = append(reviewRows, Object{"path": path, "sha256": proof["sha256"]})
		if path == rootPath {
			rootFound = true
			if commit != workflowSHA {
				return false, errors.New("legacy root workflow source commit differs from the reviewed workflow SHA")
			}
		}
	}
	if !Equal(reviewRows, review.sources) {
		return false, errors.New("legacy review source inventory differs from the signed executable closure")
	}
	return len(sources) > 0 && rootFound, nil
}

func validateLegacyEvent(eventProof, inputValue any, run, target Object) (bool, error) {
	if eventProof == nil || inputValue == nil {
		return false, nil
	}
	inputs, err := object(inputValue, "legacy preserved workflow inputs")
	if err != nil {
		return false, errors.New("legacy preserved workflow inputs must be an object or null")
	}
	eventValue, err := LoadFileProof(eventProof, "legacy original workflow event")
	if err != nil {
		return false, err
	}
	event, err := object(eventValue, "legacy original workflow event")
	if err != nil {
		return false, err
	}
	repository, err := object(event["repository"], "legacy event repository")
	if err != nil || repository["full_name"] != target["repository"] {
		return false, errors.New("legacy original event does not identify the exact target repository")
	}
	repositoryURL, urlOK := repository["html_url"].(string)
	serverURL, _ := target["server_url"].(string)
	repositoryName, _ := target["repository"].(string)
	if !urlOK || repositoryURL != serverURL+"/"+repositoryName {
		return false, nil
	}
	eventInputs := Object{}
	switch run["event"] {
	case "workflow_dispatch":
		if event["inputs"] == nil {
			return false, nil
		}
		eventInputs, err = object(event["inputs"], "legacy workflow-dispatch inputs")
		if err != nil {
			return false, errors.New("legacy workflow-dispatch inputs are malformed")
		}
	case "repository_dispatch":
		if event["client_payload"] == nil {
			return false, nil
		}
		eventInputs, err = object(event["client_payload"], "legacy repository-dispatch client payload")
		if err != nil {
			return false, errors.New("legacy repository-dispatch client payload is malformed")
		}
	default:
		if event["inputs"] != nil {
			eventInputs, err = object(event["inputs"], "legacy original event inputs")
			if err != nil {
				return false, errors.New("legacy original event inputs are malformed")
			}
		}
	}
	if !Equal(inputs, eventInputs) {
		return false, errors.New("legacy preserved inputs differ from the exact original event")
	}
	return true, nil
}

func validateLegacyEffects(value any) (map[string]bool, bool, error) {
	if value == nil {
		return map[string]bool{}, false, nil
	}
	rows, err := array(value, "legacy intended effects")
	if err != nil || len(rows) > legacyMaxRows {
		return nil, false, errors.New("legacy intended effects are malformed or oversized")
	}
	hashes := make(map[string]bool, len(rows))
	for _, raw := range rows {
		operation, err := object(raw, "legacy intended operation")
		if err != nil || len(operation) == 0 {
			return nil, false, errors.New("legacy intended operation must be a nonempty object")
		}
		canonical, err := Canonical(operation)
		if err != nil || len(canonical) > MaxFileBytes {
			return nil, false, errors.New("legacy intended operation exceeds its canonical size bound")
		}
		digest := SHA256(canonical)
		if hashes[digest] {
			return nil, false, errors.New("legacy intended operations contain duplicate canonical effects")
		}
		hashes[digest] = true
	}
	return hashes, true, nil
}

func validateLegacyArtifacts(value any, review legacyReview, run Object, runID, attempt int64) (bool, error) {
	rows, err := array(value, "legacy complete artifact witnesses")
	if err != nil || len(rows) > legacyMaxRows {
		return false, errors.New("legacy artifact witness inventory is malformed or oversized")
	}
	if len(rows) != len(review.artifacts) {
		return false, errors.New("legacy review artifact inventory differs from the signed packet")
	}
	seenIDs, seenNames := map[int64]bool{}, map[string]bool{}
	var totalBytes int64
	var totalExpanded int64
	var totalEntries int
	for index, raw := range rows {
		entry, err := Exact(raw, []string{"metadata", "archive", "origin"}, "legacy artifact witness")
		if err != nil {
			return false, err
		}
		metadata, err := validateLegacyArtifactMetadata(entry["metadata"])
		if err != nil {
			return false, err
		}
		reviewed := review.artifacts[index]
		id, _ := positiveInteger(metadata["id"], "legacy artifact ID")
		name, nameOK := metadata["name"].(string)
		digest, digestOK := metadata["digest"].(string)
		if !nameOK || !digestOK || name != reviewed["name"] || digest != reviewed["digest"] || id != mustPositive(reviewed["id"]) || seenIDs[id] || seenNames[name] {
			return false, errors.New("legacy artifact metadata differs from its exact reviewed identity")
		}
		seenIDs[id], seenNames[name] = true, true
		workflowRun := metadata["workflow_run"].(Object)
		if !exactInt(workflowRun["id"], runID) || workflowRun["head_sha"] != run["head_sha"] {
			return false, errors.New("legacy artifact metadata belongs to another exact run or source head")
		}
		if workflowRunRunID, exists := workflowRun["run_id"]; exists && !exactInt(workflowRunRunID, runID) {
			return false, errors.New("legacy artifact metadata has a conflicting workflow run ID")
		}
		attemptValue, rootAttempt := metadata["run_attempt"]
		workflowAttempt, workflowAttemptPresent := workflowRun["run_attempt"]
		if (!rootAttempt && !workflowAttemptPresent) ||
			(rootAttempt && !exactInt(attemptValue, attempt)) ||
			(workflowAttemptPresent && !exactInt(workflowAttempt, attempt)) {
			return false, errors.New("legacy artifact metadata lacks its exact attempt identity")
		}
		origin, ok := entry["origin"].(string)
		if !ok || (origin != "actions_artifact" && origin != "authoritative_archive") {
			return false, errors.New("legacy artifact witness has an unsupported byte origin")
		}
		expired, _ := metadata["expired"].(bool)
		if expired && origin != "authoritative_archive" {
			return false, errors.New("expired legacy artifact bytes require authoritative archive provenance")
		}
		rawBytes, err := LoadRawFileProof(entry["archive"], "legacy artifact archive")
		if err != nil {
			return false, err
		}
		totalBytes += int64(len(rawBytes))
		if totalBytes > legacyMaxProofBytes || digest != "sha256:"+SHA256(rawBytes) {
			return false, errors.New("legacy artifact archive bytes differ from the exact Actions digest")
		}
		files, err := archiveFiles(rawBytes, false)
		if err != nil {
			return false, fmt.Errorf("legacy artifact archive is incomplete or invalid: %w", err)
		}
		totalEntries += len(files)
		for _, file := range files {
			totalExpanded += int64(len(file))
		}
		if totalExpanded > legacyMaxProofBytes || totalEntries > MaxArtifactEntries {
			return false, errors.New("legacy artifact archives exceed their aggregate expansion bound")
		}
	}
	// An empty inventory is complete when the signed packet and the exact
	// review both attest that no Actions artifacts were required.
	return true, nil
}

func validateLegacyArtifactMetadata(value any) (Object, error) {
	metadata, err := exactWithOptional(value,
		[]string{"id", "name", "digest", "expired", "workflow_run"},
		[]string{"run_attempt", "node_id", "size_in_bytes", "url", "archive_download_url", "created_at", "updated_at", "expires_at"},
		"legacy Actions artifact metadata")
	if err != nil {
		return nil, err
	}
	if _, err := positiveInteger(metadata["id"], "legacy artifact ID"); err != nil {
		return nil, err
	}
	if !nonemptyString(metadata["name"]) || !legacyEffectPattern.MatchString(fmt.Sprint(metadata["digest"])) {
		return nil, errors.New("legacy Actions artifact metadata identity is invalid")
	}
	if _, ok := metadata["expired"].(bool); !ok {
		return nil, errors.New("legacy Actions artifact expiry state is invalid")
	}
	for _, field := range []string{"node_id", "url", "archive_download_url", "created_at", "updated_at", "expires_at"} {
		if raw, exists := metadata[field]; exists && raw != nil {
			if _, ok := raw.(string); !ok {
				return nil, errors.New("legacy Actions artifact metadata has an invalid optional string")
			}
		}
	}
	if size, exists := metadata["size_in_bytes"]; exists {
		value, err := contract.Integer(size)
		if err != nil || value < 0 {
			return nil, errors.New("legacy Actions artifact size is invalid")
		}
	}
	if attempt, exists := metadata["run_attempt"]; exists {
		if _, err := positiveInteger(attempt, "legacy artifact attempt"); err != nil {
			return nil, err
		}
	}
	workflowRun, err := exactWithOptional(metadata["workflow_run"],
		[]string{"id", "head_sha"},
		[]string{"run_id", "run_attempt", "repository_id", "head_repository_id", "head_branch", "run_number", "event", "display_title"},
		"legacy artifact workflow run")
	if err != nil {
		return nil, err
	}
	if _, err := positiveInteger(workflowRun["id"], "legacy artifact workflow run ID"); err != nil {
		return nil, err
	}
	if runID, exists := workflowRun["run_id"]; exists {
		if _, err := positiveInteger(runID, "legacy artifact workflow run ID"); err != nil {
			return nil, err
		}
	}
	if attempt, exists := workflowRun["run_attempt"]; exists {
		if _, err := positiveInteger(attempt, "legacy artifact workflow attempt"); err != nil {
			return nil, err
		}
	}
	for _, field := range []string{"head_branch", "event", "display_title"} {
		if raw, exists := workflowRun[field]; exists && raw != nil {
			if _, ok := raw.(string); !ok {
				return nil, errors.New("legacy artifact workflow metadata has an invalid optional string")
			}
		}
	}
	for _, field := range []string{"repository_id", "head_repository_id", "run_number"} {
		if raw, exists := workflowRun[field]; exists {
			if _, err := positiveInteger(raw, "legacy artifact workflow numeric identity"); err != nil {
				return nil, err
			}
		}
	}
	if !legacySHA40Pattern.MatchString(fmt.Sprint(workflowRun["head_sha"])) {
		return nil, errors.New("legacy artifact workflow head SHA is invalid")
	}
	return metadata, nil
}

func validateLegacyJobs(value any, review legacyReview, runID, attempt int64) (legacyJobEvidence, error) {
	packet, err := Exact(value, []string{"run_id", "attempt", "pages"}, "legacy complete jobs packet")
	if err != nil {
		return legacyJobEvidence{}, err
	}
	if !exactInt(packet["run_id"], runID) || !exactInt(packet["attempt"], attempt) {
		return legacyJobEvidence{}, errors.New("legacy jobs packet belongs to another exact run attempt")
	}
	pages, err := array(packet["pages"], "legacy paginated jobs")
	if err != nil || len(pages) > legacyMaxRows {
		return legacyJobEvidence{}, errors.New("legacy jobs pages are malformed or oversized")
	}
	if len(pages) == 0 {
		return legacyJobEvidence{}, nil
	}
	var total int64 = -1
	var rows []Object
	seenIDs := map[int64]bool{}
	for _, rawPage := range pages {
		page, err := object(rawPage, "legacy jobs page")
		if err != nil {
			return legacyJobEvidence{}, err
		}
		count, err := contract.Integer(page["total_count"])
		if err != nil || count < 0 || (total >= 0 && total != count) {
			return legacyJobEvidence{}, errors.New("legacy jobs pagination has inconsistent totals")
		}
		total = count
		jobs, err := contract.Objects(page, "jobs")
		if err != nil {
			return legacyJobEvidence{}, err
		}
		for _, job := range jobs {
			id, err := positiveInteger(job["id"], "legacy job ID")
			if err != nil || seenIDs[id] {
				return legacyJobEvidence{}, errors.New("legacy jobs pagination contains an invalid or duplicate job ID")
			}
			seenIDs[id] = true
			if !exactInt(job["run_id"], runID) {
				return legacyJobEvidence{}, errors.New("legacy job belongs to another workflow run")
			}
			if runAttempt, exists := job["run_attempt"]; exists && !exactInt(runAttempt, attempt) {
				return legacyJobEvidence{}, errors.New("legacy job belongs to another workflow attempt")
			}
			rows = append(rows, job)
		}
	}
	if total < int64(len(rows)) {
		return legacyJobEvidence{}, errors.New("legacy jobs pagination exceeds its declared total")
	}
	if total != int64(len(rows)) {
		return legacyJobEvidence{}, nil
	}
	jobs, err := completePages(pages, "jobs")
	if err != nil {
		return legacyJobEvidence{}, err
	}
	expected := map[string]bool{}
	for _, mutator := range review.mutators {
		expected[mutator["job"].(string)] = true
	}
	for _, name := range review.nonMutator {
		expected[name] = true
	}
	actual := map[string]Object{}
	allCompleted := true
	for _, job := range jobs {
		name, ok := job["name"].(string)
		if !ok || !nonemptyString(name) || !expected[name] || actual[name] != nil {
			return legacyJobEvidence{}, errors.New("legacy jobs do not match the reviewed complete job inventory")
		}
		actual[name] = job
		if job["status"] != "completed" {
			allCompleted = false
		}
	}
	if len(actual) != len(expected) {
		return legacyJobEvidence{}, nil
	}
	if !allCompleted {
		return legacyJobEvidence{}, nil
	}
	mutators := make([]Object, len(review.mutators))
	copy(mutators, review.mutators)
	policy := &noopPolicy{mutators: mutators}
	noDispatch := false
	stepsComplete := true
	for _, mutator := range review.mutators {
		job := actual[mutator["job"].(string)]
		if job["conclusion"] == "skipped" {
			continue
		}
		stepRows, ok := job["steps"].([]any)
		if !ok {
			stepsComplete = false
			continue
		}
		seenSteps := map[string]int{}
		for _, rawStep := range stepRows {
			step, err := object(rawStep, "legacy mutation step")
			if err != nil {
				return legacyJobEvidence{}, err
			}
			if stepName, ok := step["name"].(string); ok {
				seenSteps[stepName]++
			}
		}
		for _, rawStepName := range mutator["steps"].([]string) {
			if seenSteps[rawStepName] > 1 {
				return legacyJobEvidence{}, errors.New("legacy job repeats a reviewed mutator step")
			}
			if seenSteps[rawStepName] == 0 {
				stepsComplete = false
			}
		}
	}
	if !stepsComplete {
		return legacyJobEvidence{}, nil
	}
	jobsPacket := Object{"run_id": runID, "attempt": attempt, "pages": pages}
	if err := validateNoopJobs(jobsPacket, policy, runID, attempt); err == nil {
		noDispatch = len(review.mutators) > 0
	}
	return legacyJobEvidence{complete: true, noDispatch: noDispatch}, nil
}

type legacyReceiptState struct {
	complete           bool
	hasProviderReceipt bool
	sdkObserved        map[string]bool
}

func validateLegacyReceipts(value any, target Object, runID, attempt int64, attemptTarget Object, hashes map[string]bool, effectsValid bool) (legacyReceiptState, error) {
	rows, err := array(value, "legacy provider receipts")
	if err != nil || len(rows) > legacyMaxRows {
		return legacyReceiptState{}, errors.New("legacy receipt inventory is malformed or oversized")
	}
	providers, sdk := map[string]bool{}, map[string]bool{}
	var totalReceiptBytes int64
	for _, raw := range rows {
		entry, err := Exact(raw, []string{"effect_sha256", "kind", "receipt"}, "legacy effect receipt")
		if err != nil {
			return legacyReceiptState{}, err
		}
		effect, ok := entry["effect_sha256"].(string)
		if !ok || !IsSHA256(effect) || (effectsValid && !hashes[effect]) {
			return legacyReceiptState{}, errors.New("legacy receipt references an unknown or malformed intended effect")
		}
		kind, ok := entry["kind"].(string)
		if !ok || (kind != "provider_receipt" && kind != "sdk_observation") {
			return legacyReceiptState{}, errors.New("legacy receipt kind is unsupported")
		}
		rawReceipt, err := LoadRawFileProof(entry["receipt"], "legacy raw receipt")
		if err != nil {
			return legacyReceiptState{}, err
		}
		totalReceiptBytes += int64(len(rawReceipt))
		if totalReceiptBytes > legacyMaxProofBytes {
			return legacyReceiptState{}, errors.New("legacy receipt evidence exceeds its aggregate byte bound")
		}
		decoded, err := DecodeValue(rawReceipt)
		if err != nil {
			return legacyReceiptState{}, err
		}
		if kind == "provider_receipt" {
			if providers[effect] || sdk[effect] {
				return legacyReceiptState{}, errors.New("legacy receipts duplicate an intended effect")
			}
			receipt, err := Exact(decoded, []string{"schema_version", "target", "run_id", "attempt", "attempt_target", "effect_sha256", "outcome", "provider_receipt_id"}, "legacy provider receipt")
			if err != nil || !exactInt(receipt["schema_version"], 1) || !Equal(receipt["target"], target) ||
				!exactInt(receipt["run_id"], runID) || !exactInt(receipt["attempt"], attempt) || !Equal(receipt["attempt_target"], attemptTarget) ||
				receipt["effect_sha256"] != effect || receipt["outcome"] != "completed" || !nonemptyString(receipt["provider_receipt_id"]) {
				return legacyReceiptState{}, errors.New("legacy provider receipt does not bind its exact completed effect")
			}
			providers[effect] = true
		} else {
			if providers[effect] || sdk[effect] {
				return legacyReceiptState{}, errors.New("legacy receipts duplicate an intended effect")
			}
			receipt, err := object(decoded, "legacy SDK observation receipt")
			if err != nil || len(receipt) == 0 {
				return legacyReceiptState{}, errors.New("legacy SDK observation receipt must be a nonempty object")
			}
			sdk[effect] = true
		}
	}
	complete := effectsValid && len(hashes) != 0 && len(providers) == len(hashes)
	if complete {
		for effect := range hashes {
			if !providers[effect] {
				complete = false
				break
			}
		}
	}
	return legacyReceiptState{complete: complete, hasProviderReceipt: len(providers) != 0, sdkObserved: sdk}, nil
}

func validateLegacyObservations(value any, hashes map[string]bool, effectsValid bool) (map[string]bool, error) {
	rows, err := array(value, "legacy effect observations")
	if err != nil || len(rows) > legacyMaxRows {
		return nil, errors.New("legacy observation inventory is malformed or oversized")
	}
	observed := map[string]bool{}
	var totalObservationBytes int64
	for _, raw := range rows {
		entry, err := Exact(raw, []string{"effect_sha256", "evidence"}, "legacy effect observation")
		if err != nil {
			return nil, err
		}
		effect, ok := entry["effect_sha256"].(string)
		if !ok || !IsSHA256(effect) || (effectsValid && !hashes[effect]) || observed[effect] {
			return nil, errors.New("legacy observation references a duplicate, unknown or malformed effect")
		}
		rawEvidence, err := LoadRawFileProof(entry["evidence"], "legacy effect observation")
		if err != nil {
			return nil, err
		}
		totalObservationBytes += int64(len(rawEvidence))
		if totalObservationBytes > legacyMaxProofBytes {
			return nil, errors.New("legacy observation evidence exceeds its aggregate byte bound")
		}
		value, err := DecodeValue(rawEvidence)
		if err != nil {
			return nil, err
		}
		evidence, err := object(value, "legacy effect observation evidence")
		if err != nil || len(evidence) == 0 {
			return nil, errors.New("legacy observation evidence must be a nonempty JSON object")
		}
		observed[effect] = true
	}
	return observed, nil
}
