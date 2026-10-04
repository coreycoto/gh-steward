package runrecovery

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var (
	publicationSHA40          = regexp.MustCompile(`^[0-9a-f]{40}$`)
	publicationArtifactDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	publicationUUID           = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

var (
	publicationProofFields  = []string{"schema_version", "origin_run", "intent_file", "patch_file", "result_file", "event_file", "push_ack_file", "pr_ack_file", "verify_ack_file", "candidate_file", "candidate_artifact_file", "candidate_archive_file", "candidate_jobs_file", "qualification_file"}
	publicationRunFields    = []string{"id", "created_at", "display_title", "event", "workflow_id", "head_branch", "head_sha"}
	publicationIntentFields = []string{
		"schema_version", "repository", "repository_node_id", "workflow_file", "recovery_key", "run_name",
		"origin_run_id", "origin_run_attempt", "workflow_sha", "candidate_sha256", "event_name", "trigger_event_sha256", "source_repository",
		"source_ref", "source_sha", "target_pr", "mode", "base_ref", "base_sha", "head_ref",
		"expected_old_sha", "new_sha", "publisher_login", "title", "body", "draft", "nonce",
		"patch_sha256", "result_sha256", "files", "created_at",
	}
	publicationContextFields = []string{"intent_sha256", "origin_run_id", "origin_run_attempt", "stage", "push_ack", "pr_ack", "verify_ack"}
)

var publicationPaths = map[string]string{
	"intent_file":             "publication/intent.json",
	"patch_file":              "publication/patch.diff",
	"result_file":             "publication/result.json",
	"event_file":              "events/trigger-event.json",
	"push_ack_file":           "publication/push-ack.json",
	"pr_ack_file":             "publication/pr-ack.json",
	"verify_ack_file":         "publication/verify-ack.json",
	"candidate_file":          "publication/candidate.json",
	"candidate_artifact_file": "publication/candidate-artifact.json",
	"candidate_archive_file":  "publication/candidate-archive.zip",
	"candidate_jobs_file":     "publication/candidate-jobs.json",
	"qualification_file":      "publication/qualification.json",
}

var publicationCandidateFields = []string{
	"schema_version", "repository", "server_url", "workflow_file", "origin_run_id", "origin_run_attempt",
	"workflow_sha", "event_name", "trigger_event_sha256", "source_repository", "source_ref", "source_sha",
	"base_ref", "base_sha", "candidate_tree_sha", "patch_sha256", "result_sha256", "upstream_artifacts",
	"verification_job_name",
}

var publicationCandidateArchiveFiles = []string{
	"publication/candidate.json", "publication/patch.diff", "publication/result.json", "events/trigger-event.json",
}

// ValidatePublicationProof verifies candidate handoff files, persisted intent,
// ref lease, and positive push/PR/verification acknowledgements for one attempt.
func ValidatePublicationProof(proof any, target, context Object, requireCompleted bool) (Object, error) {
	retained, err := Exact(proof, publicationProofFields, "publication proof")
	if err != nil {
		return nil, err
	}
	if !publicationIntegerIs(retained["schema_version"], 2) {
		return nil, errors.New("publication proof uses an unsupported schema")
	}
	publication, err := Exact(context["publication"], publicationContextFields, "publication context")
	if err != nil {
		return nil, err
	}
	workflowFile, repository, workflowID, err := publicationTarget(target)
	if err != nil {
		return nil, err
	}
	origin, err := Exact(retained["origin_run"], publicationRunFields, "publication origin run")
	if err != nil {
		return nil, err
	}
	originID, idErr := proofPositiveInteger(origin["id"])
	originWorkflowID, workflowErr := proofPositiveInteger(origin["workflow_id"])
	headSHA, shaOK := origin["head_sha"].(string)
	_, branchOK := origin["head_branch"].(string)
	if idErr != nil || workflowErr != nil || originWorkflowID != workflowID ||
		!shaOK || !publicationSHA40.MatchString(headSHA) || !branchOK ||
		!nativeNonemptyValue(origin["created_at"]) || !nativeNonemptyValue(origin["display_title"]) || !nativeNonemptyValue(origin["event"]) {
		return nil, errors.New("publication origin does not identify the exact workflow")
	}

	for _, name := range []string{"intent", "patch", "result", "event"} {
		if _, err := decodePublicationRawProof(retained[name+"_file"], name); err != nil {
			return nil, err
		}
	}
	intentValue, err := LoadFileProof(retained["intent_file"], "publication intent")
	if err != nil {
		return nil, err
	}
	intent, err := Exact(intentValue, publicationIntentFields, "publication intent")
	if err != nil {
		return nil, err
	}
	if !publicationIntegerIs(intent["schema_version"], 2) || intent["repository"] != repository ||
		intent["workflow_file"] != workflowFile || intent["recovery_key"] != context["recovery_key"] ||
		!nativeNonemptyValue(intent["recovery_key"]) || intent["run_name"] != origin["display_title"] ||
		!publicationIntegerIs(intent["origin_run_id"], originID) ||
		!publicationPositiveEqual(publication["origin_run_id"], intent["origin_run_id"]) ||
		!publicationPositiveEqual(publication["origin_run_attempt"], intent["origin_run_attempt"]) ||
		!publicationPositive(intent["origin_run_attempt"]) {
		return nil, errors.New("publication intent differs from its exact origin, target or semantic digest")
	}
	intentBytes, err := Canonical(intent)
	if err != nil || publication["intent_sha256"] != SHA256(intentBytes) {
		return nil, errors.New("publication intent differs from its exact origin, target or semantic digest")
	}
	eventName, eventOK := intent["event_name"].(string)
	originEvent, originEventOK := origin["event"].(string)
	if !eventOK || (eventName != "pull_request_target" && eventName != "schedule" && eventName != "workflow_dispatch") ||
		!originEventOK || eventName != originEvent {
		return nil, errors.New("publication intent event differs from the exact origin")
	}
	if err := validatePublicationCandidate(retained, intent, origin, target, originID); err != nil {
		return nil, err
	}

	for _, field := range []string{"source_sha", "base_sha", "new_sha"} {
		value, ok := intent[field].(string)
		if !ok || !publicationSHA40.MatchString(value) {
			return nil, errors.New("publication intent has malformed source, branch or publisher identity")
		}
	}
	if oldSHA := intent["expected_old_sha"]; oldSHA != nil {
		text, ok := oldSHA.(string)
		if !ok || !publicationSHA40.MatchString(text) {
			return nil, errors.New("publication intent has malformed source, branch or publisher identity")
		}
	}
	for _, field := range []string{"repository_node_id", "publisher_login", "created_at"} {
		if !nativeNonemptyValue(intent[field]) {
			return nil, errors.New("publication intent has malformed source, branch or publisher identity")
		}
	}
	sourceRepository, sourceRepoOK := intent["source_repository"].(string)
	if !sourceRepoOK || !strings.EqualFold(sourceRepository, repository) ||
		!publicationSourceBranch(intent["source_ref"]) || !publicationBranch(intent["head_ref"], true) ||
		!publicationBranch(intent["base_ref"], false) {
		return nil, errors.New("publication intent has malformed source, branch or publisher identity")
	}
	title, titleOK := intent["title"].(string)
	_, bodyOK := intent["body"].(string)
	_, draftOK := intent["draft"].(bool)
	nonce, nonceOK := intent["nonce"].(string)
	if !titleOK || strings.TrimSpace(title) == "" || !bodyOK || !draftOK || !nonceOK || !publicationUUID.MatchString(nonce) {
		return nil, errors.New("publication intent has malformed source, branch or publisher identity")
	}
	for _, pair := range []struct{ intent, proof string }{
		{"patch_sha256", "patch_file"}, {"result_sha256", "result_file"}, {"trigger_event_sha256", "event_file"},
	} {
		digest, ok := intent[pair.intent].(string)
		fileProof, okProof := retained[pair.proof].(map[string]any)
		if !ok || !IsSHA256(digest) || !okProof || digest != fileProof["sha256"] {
			return nil, errors.New("publication source file differs from its captured intent")
		}
	}

	resultValue, err := LoadFileProof(retained["result_file"], "publication result")
	if err != nil {
		return nil, err
	}
	if _, err := LoadFileProof(retained["event_file"], "publication event"); err != nil {
		return nil, err
	}
	files, ok := intent["files"].([]any)
	if !ok || len(files) == 0 {
		return nil, errors.New("publication lacks its exact verified changed-file inventory")
	}
	fileNames := make([]string, 0, len(files))
	for _, raw := range files {
		path, ok := raw.(string)
		if !ok || !publicationPath(path) {
			return nil, errors.New("publication lacks its exact verified changed-file inventory")
		}
		fileNames = append(fileNames, path)
	}
	if !sort.StringsAreSorted(fileNames) {
		return nil, errors.New("publication lacks its exact verified changed-file inventory")
	}
	for index := 1; index < len(fileNames); index++ {
		if fileNames[index] == fileNames[index-1] {
			return nil, errors.New("publication lacks its exact verified changed-file inventory")
		}
	}
	result, ok := resultValue.(map[string]any)
	verification, verifyOK := result["verification"].([]any)
	if !ok || result["status"] != "patched" || !Equal(result["changed_files"], files) || !verifyOK || len(verification) == 0 {
		return nil, errors.New("publication lacks its exact verified changed-file inventory")
	}

	mode, modeOK := intent["mode"].(string)
	targetPR := intent["target_pr"]
	if !modeOK || (mode != "update-existing-pr" && mode != "create-pr") ||
		(mode == "update-existing-pr") != isObject(targetPR) {
		return nil, errors.New("publication PR mode differs from its exact target")
	}
	sourceRef, _ := intent["source_ref"].(string)
	headRef, _ := intent["head_ref"].(string)
	sourceSHA, _ := intent["source_sha"].(string)
	baseSHA, _ := intent["base_sha"].(string)
	if (mode == "update-existing-pr" &&
		(intent["expected_old_sha"] != sourceSHA || sourceRef != headRef)) ||
		(mode == "create-pr" && (intent["expected_old_sha"] != nil || sourceSHA != baseSHA)) {
		return nil, errors.New("publication source branch lease differs from its exact update or create mode")
	}
	if mode == "update-existing-pr" {
		pr, err := Exact(targetPR, []string{"number", "url", "head_repository", "head_ref", "head_sha", "base_repository", "base_ref", "author_login", "title", "body", "draft"}, "existing PR target")
		if err != nil {
			return nil, err
		}
		prNumber, numberErr := proofPositiveInteger(pr["number"])
		prURL, urlOK := pr["url"].(string)
		headRepo, headRepoOK := pr["head_repository"].(string)
		baseRepo, baseRepoOK := pr["base_repository"].(string)
		headRef, headRefOK := pr["head_ref"].(string)
		headCommit, headCommitOK := pr["head_sha"].(string)
		baseRef, baseRefOK := pr["base_ref"].(string)
		_, authorOK := pr["author_login"].(string)
		_, prTitleOK := pr["title"].(string)
		_, prBodyOK := pr["body"].(string)
		_, prDraftOK := pr["draft"].(bool)
		if numberErr != nil || !urlOK || prURL != fmt.Sprintf("https://github.com/%s/pull/%d", repository, prNumber) ||
			!headRepoOK || !strings.EqualFold(headRepo, repository) || !baseRepoOK || !strings.EqualFold(baseRepo, repository) ||
			!headRefOK || headRef != strings.TrimPrefix(intent["head_ref"].(string), "refs/heads/") ||
			!headCommitOK || headCommit != intent["source_sha"] || !baseRefOK || baseRef != intent["base_ref"] ||
			!authorOK || !nativeNonemptyValue(pr["author_login"]) || !prTitleOK || !prBodyOK || !prDraftOK {
			return nil, errors.New("existing PR target differs from the original source and base")
		}
	} else if targetPR != nil {
		return nil, errors.New("new PR publication unexpectedly identifies an existing target")
	}

	acks := make(map[string]any, 3)
	for _, name := range []string{"push", "pr", "verify"} {
		retainedAck := retained[name+"_ack_file"]
		if retainedAck == nil {
			acks[name] = nil
			continue
		}
		ack, err := LoadFileProof(retainedAck, "publication "+name+" ACK")
		if err != nil {
			return nil, err
		}
		ackObject, ok := ack.(map[string]any)
		if !ok || !publicationIntegerIs(ackObject["schema_version"], 1) {
			return nil, errors.New("publication acknowledgement uses an invalid schema")
		}
		if !Equal(ack, publication[name+"_ack"]) {
			return nil, errors.New("publication receipt bytes differ from the context")
		}
		acks[name] = ack
	}
	push, _ := acks["push"].(map[string]any)
	prAck, _ := acks["pr"].(map[string]any)
	verifyAck, _ := acks["verify"].(map[string]any)
	if push != nil {
		expected := Object{
			"schema_version": 1, "nonce": intent["nonce"], "repository": repository,
			"repository_node_id": intent["repository_node_id"], "ref": intent["head_ref"],
			"expected_old_sha": intent["expected_old_sha"], "new_sha": intent["new_sha"], "positive_push_ack": true,
		}
		if !Equal(push, expected) || push["positive_push_ack"] != true {
			return nil, errors.New("push acknowledgement differs from the exact ref lease")
		}
	}
	if prAck != nil {
		expected := Object{
			"schema_version": 1, "nonce": intent["nonce"], "repository": repository,
			"repository_node_id": intent["repository_node_id"], "head_ref": strings.TrimPrefix(intent["head_ref"].(string), "refs/heads/"),
			"head_sha": intent["new_sha"], "base_ref": intent["base_ref"], "base_sha": intent["base_sha"],
			"publisher_login": intent["publisher_login"], "title": intent["title"], "body": intent["body"], "draft": intent["draft"],
		}
		prNumber, numberErr := proofPositiveInteger(prAck["number"])
		prURL, urlOK := prAck["url"].(string)
		prFields := nativeSliceSet([]string{"schema_version", "nonce", "repository", "repository_node_id", "head_ref", "head_sha", "base_ref", "base_sha", "publisher_login", "title", "body", "draft", "url", "number"})
		if mode != "create-pr" || numberErr != nil || !urlOK ||
			prURL != fmt.Sprintf("https://github.com/%s/pull/%d", repository, prNumber) ||
			!nativeHasExactSet(prAck, prFields) ||
			!Equal(publicationWithoutURLAndNumber(prAck), expected) {
			return nil, errors.New("created PR acknowledgement differs from the exact intent")
		}
	}
	if verifyAck != nil {
		var identity map[string]any
		if prAck != nil {
			identity = prAck
		} else if isObject(targetPR) {
			identity = targetPR.(map[string]any)
		} else {
			return nil, errors.New("PR verification lacks a positively identified PR")
		}
		_, numberErr := proofPositiveInteger(identity["number"])
		identityURL, urlOK := identity["url"].(string)
		identityTitle, titleOK := identity["title"].(string)
		identityBody, bodyOK := identity["body"].(string)
		identityDraft, draftOK := identity["draft"].(bool)
		author := intent["publisher_login"]
		if prAck == nil {
			author = identity["author_login"]
		}
		expected := Object{
			"schema_version": 1, "nonce": intent["nonce"], "repository": repository,
			"repository_node_id": intent["repository_node_id"], "url": identityURL, "number": identity["number"],
			"head_ref": strings.TrimPrefix(intent["head_ref"].(string), "refs/heads/"), "head_sha": intent["new_sha"],
			"base_ref": intent["base_ref"], "author_login": author, "title": identityTitle, "body": identityBody,
			"draft": identityDraft, "state": "OPEN", "verified": true,
		}
		if numberErr != nil || !urlOK || !titleOK || !bodyOK || !draftOK || !nativeHasExactSet(verifyAck, nativeSliceSet([]string{"schema_version", "nonce", "repository", "repository_node_id", "url", "number", "head_ref", "head_sha", "base_ref", "author_login", "title", "body", "draft", "state", "verified"})) ||
			!Equal(verifyAck, expected) || verifyAck["verified"] != true || !publicationPositive(verifyAck["number"]) {
			return nil, errors.New("PR observation differs from the positively acknowledged target")
		}
	}

	stage, stageOK := publication["stage"].(string)
	dispatchSteps := []any{}
	switch stage {
	case "push-pending":
		dispatchSteps = []any{"branch-push"}
	case "pr-pending":
		dispatchSteps = []any{"pull-request-create"}
	case "pr-verify-pending", "completed":
	default:
		stageOK = false
	}
	if !stageOK || !Equal(context["dispatch_steps"], dispatchSteps) || !Equal(context["plans"], []any{}) {
		return nil, errors.New("publication stage differs from the trusted mutation-step inventory")
	}
	validReceipts := false
	switch stage {
	case "push-pending":
		validReceipts = push == nil && prAck == nil && verifyAck == nil
	case "pr-pending":
		validReceipts = mode == "create-pr" && push != nil && prAck == nil && verifyAck == nil
	case "pr-verify-pending":
		validReceipts = push != nil && (mode != "create-pr" || prAck != nil) && verifyAck == nil
	case "completed":
		validReceipts = push != nil && (mode != "create-pr" || prAck != nil) && verifyAck != nil
	}
	if !validReceipts || (mode == "update-existing-pr" && prAck != nil) {
		return nil, errors.New("publication stage lacks its exact positive acknowledgements")
	}
	phase, phaseOK := context["phase"].(string)
	if requireCompleted && (stage != "completed" || phase != "completed") {
		return nil, errors.New("publication lacks complete terminal proof")
	}
	if !phaseOK || (phase != "prepared" && phase != "dispatching" && phase != "completed") ||
		((phase == "completed") != (stage == "completed")) {
		return nil, errors.New("publication phase differs from its persisted stage")
	}
	if err := validatePublicationQualification(retained, target, context, intent, origin); err != nil {
		return nil, err
	}
	return intent, nil
}

func validatePublicationCandidate(retained, intent, origin, target Object, originID int64) error {
	candidateValue, err := LoadFileProof(retained["candidate_file"], "publication candidate")
	if err != nil {
		return err
	}
	candidate, err := Exact(candidateValue, publicationCandidateFields, "publication candidate")
	if err != nil {
		return err
	}
	candidateFileProof, err := Exact(retained["candidate_file"], []string{"sha256", "base64"}, "publication candidate file")
	if err != nil {
		return err
	}
	originAttempt, attemptErr := proofPositiveInteger(intent["origin_run_attempt"])
	treeSHA, treeOK := candidate["candidate_tree_sha"].(string)
	if attemptErr != nil || !publicationIntegerIs(candidate["schema_version"], 1) ||
		candidate["repository"] != intent["repository"] || candidate["repository"] != target["repository"] ||
		candidate["server_url"] != target["server_url"] || candidate["workflow_file"] != target["workflow_file"] ||
		!publicationIntegerIs(candidate["origin_run_id"], originID) || !publicationIntegerIs(candidate["origin_run_attempt"], originAttempt) ||
		!publicationSHAEqual(candidate["workflow_sha"], intent["workflow_sha"]) ||
		candidate["event_name"] != intent["event_name"] || candidate["trigger_event_sha256"] != intent["trigger_event_sha256"] ||
		candidate["source_repository"] != intent["source_repository"] || candidate["source_ref"] != intent["source_ref"] ||
		candidate["source_sha"] != intent["source_sha"] || candidate["base_ref"] != intent["base_ref"] ||
		candidate["base_sha"] != intent["base_sha"] || candidate["patch_sha256"] != intent["patch_sha256"] ||
		candidate["result_sha256"] != intent["result_sha256"] || candidate["verification_job_name"] != "Verify publication candidate" ||
		!treeOK || !publicationSHA40.MatchString(treeSHA) || !IsSHA256(candidate["patch_sha256"]) || !IsSHA256(candidate["result_sha256"]) ||
		!IsSHA256(intent["candidate_sha256"]) || candidateFileProof["sha256"] != intent["candidate_sha256"] {
		return errors.New("publication candidate differs from its exact intent, origin or workflow target")
	}
	if err := validateCandidateUpstreamArtifacts(candidate["upstream_artifacts"]); err != nil {
		return err
	}

	archiveBytes, err := decodePublicationRawProof(retained["candidate_archive_file"], "candidate archive")
	if err != nil {
		return err
	}
	metadataValue, err := LoadFileProof(retained["candidate_artifact_file"], "candidate artifact metadata")
	if err != nil {
		return err
	}
	metadata, ok := metadataValue.(map[string]any)
	if !ok {
		return errors.New("candidate artifact metadata is not an object")
	}
	metadataID, metadataIDErr := proofPositiveInteger(metadata["id"])
	metadataName, metadataNameOK := metadata["name"].(string)
	metadataDigest, metadataDigestOK := metadata["digest"].(string)
	workflowRun, workflowRunOK := metadata["workflow_run"].(map[string]any)
	workflowRunID, workflowRunIDErr := proofPositiveInteger(workflowRun["id"])
	if metadataIDErr != nil || !metadataNameOK || metadataName != fmt.Sprintf("gh-steward-candidate-%d-%d", originID, originAttempt) ||
		!metadataDigestOK || metadataDigest != "sha256:"+SHA256(archiveBytes) || metadata["expired"] != false ||
		!workflowRunOK || workflowRunIDErr != nil || workflowRunID != originID || !publicationSHAEqual(workflowRun["head_sha"], origin["head_sha"]) {
		return errors.New("candidate artifact metadata differs from its exact workflow run, archive or digest")
	}
	upstreamArtifacts := candidate["upstream_artifacts"].(map[string]any)
	for _, role := range []string{"capture", "proposal"} {
		upstream := upstreamArtifacts[role].(map[string]any)
		upstreamID, _ := proofPositiveInteger(upstream["id"])
		if upstreamID == metadataID || upstream["name"] == metadataName {
			return errors.New("candidate artifact duplicates an upstream artifact identity")
		}
	}
	if err := validateCandidateArchive(archiveBytes, retained); err != nil {
		return err
	}

	jobsValue, err := LoadFileProof(retained["candidate_jobs_file"], "candidate workflow jobs")
	if err != nil {
		return err
	}
	pages, ok := jobsValue.([]any)
	if !ok || len(pages) == 0 || len(pages) > MaxArtifactEntries {
		return errors.New("candidate workflow jobs must be complete raw API pages")
	}
	jobs, err := completePages(pages, "jobs")
	if err != nil {
		return fmt.Errorf("candidate workflow jobs are incomplete: %w", err)
	}
	if len(jobs) > MaxArtifactEntries {
		return errors.New("candidate workflow jobs exceed the retained evidence limit")
	}
	verifyJobs := 0
	for _, job := range jobs {
		jobName, nameOK := job["name"].(string)
		if !nameOK || !nativeNonempty(jobName) {
			return errors.New("candidate workflow job has no exact name")
		}
		for field, expected := range map[string]int64{"run_id": originID, "run_attempt": originAttempt} {
			if raw, exists := job[field]; exists {
				number, err := proofPositiveInteger(raw)
				if err != nil || number != expected {
					return errors.New("candidate workflow job belongs to another exact attempt")
				}
			}
		}
		if jobName == "Verify publication candidate" {
			verifyJobs++
			if !publicationIntegerIs(job["run_id"], originID) || !publicationIntegerIs(job["run_attempt"], originAttempt) ||
				job["status"] != "completed" || job["conclusion"] != "success" {
				return errors.New("candidate verification job did not complete successfully")
			}
		}
	}
	if verifyJobs != 1 {
		return errors.New("candidate workflow lacks exactly one successful verification job")
	}
	return nil
}

func validateCandidateUpstreamArtifacts(value any) error {
	artifacts, err := Exact(value, []string{"capture", "proposal"}, "candidate upstream artifacts")
	if err != nil {
		return err
	}
	seenIDs, seenNames := map[int64]bool{}, map[string]bool{}
	for _, role := range []string{"capture", "proposal"} {
		artifact, err := Exact(artifacts[role], []string{"id", "name", "digest"}, "candidate "+role+" artifact")
		if err != nil {
			return err
		}
		id, idErr := proofPositiveInteger(artifact["id"])
		name, nameOK := artifact["name"].(string)
		digest, digestOK := artifact["digest"].(string)
		if idErr != nil || !nameOK || !nativeNonempty(name) || seenIDs[id] || seenNames[name] || !digestOK || !publicationArtifactDigest.MatchString(digest) {
			return errors.New("candidate upstream artifact identity is invalid or duplicated")
		}
		seenIDs[id], seenNames[name] = true, true
	}
	return nil
}

func validateCandidateArchive(archive []byte, retained Object) error {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) != len(publicationCandidateArchiveFiles) {
		return errors.New("candidate archive is invalid or has an unsupported file inventory")
	}
	expected := make(map[string][]byte, len(publicationCandidateArchiveFiles))
	for _, key := range []string{"candidate_file", "patch_file", "result_file", "event_file"} {
		data, err := decodePublicationRawProof(retained[key], key)
		if err != nil {
			return err
		}
		archivePath := map[string]string{
			"candidate_file": "publication/candidate.json", "patch_file": "publication/patch.diff",
			"result_file": "publication/result.json", "event_file": "events/trigger-event.json",
		}[key]
		expected[archivePath] = data
	}
	seen := make(map[string]bool, len(reader.File))
	total := uint64(0)
	for _, entry := range reader.File {
		name := entry.Name
		data, allowed := expected[name]
		if !allowed || seen[name] || entry.FileInfo().IsDir() || !entry.Mode().IsRegular() || entry.UncompressedSize64 > MaxFileBytes {
			return errors.New("candidate archive contains an unexpected or unsafe file")
		}
		seen[name] = true
		total += entry.UncompressedSize64
		if total > MaxArtifactBytes {
			return errors.New("candidate archive exceeds its uncompressed size limit")
		}
		stream, err := entry.Open()
		if err != nil {
			return errors.New("candidate archive file cannot be opened")
		}
		content, readErr := io.ReadAll(io.LimitReader(stream, MaxFileBytes+1))
		closeErr := stream.Close()
		if readErr != nil || closeErr != nil || len(content) > MaxFileBytes || !bytes.Equal(content, data) {
			return errors.New("candidate archive contents differ from the retained source bytes")
		}
	}
	if len(seen) != len(expected) {
		return errors.New("candidate archive omits a required source file")
	}
	return nil
}

func publicationSHAEqual(value any, expected any) bool {
	text, ok := value.(string)
	want, wantOK := expected.(string)
	return ok && wantOK && publicationSHA40.MatchString(text) && text == want
}

// BuildPublicationProof snapshots only the known publication files beneath a
// package root, retaining exact bytes and validating the publication contract.
func BuildPublicationProof(root string, originRun, context, target Object, requireCompleted bool) (Object, error) {
	proof := Object{"schema_version": 2, "origin_run": originRun}
	for _, field := range []string{"intent_file", "patch_file", "result_file", "event_file", "candidate_file", "candidate_artifact_file", "candidate_archive_file", "candidate_jobs_file", "push_ack_file", "pr_ack_file", "verify_ack_file", "qualification_file"} {
		relative := publicationPaths[field]
		data, exists, err := readOptionalPublicationFile(root, relative)
		if err != nil {
			return nil, fmt.Errorf("read publication %s: %w", field, err)
		}
		if !exists {
			if field == "push_ack_file" || field == "pr_ack_file" || field == "verify_ack_file" || field == "qualification_file" {
				proof[field] = nil
				continue
			}
			return nil, fmt.Errorf("publication source file is missing: %s", relative)
		}
		proof[field] = MakeFileProof(data)
	}
	if _, err := ValidatePublicationProof(proof, target, context, requireCompleted); err != nil {
		return nil, err
	}
	return proof, nil
}

// ValidateRecoveredPublicationProof proves that read-only observer verification
// completed after durable positive push/PR acknowledgements. It never repeats
// an uncertain publication write.
func ValidateRecoveredPublicationProof(proof any, target Object) error {
	root, err := Exact(proof, []string{"source", "observer"}, "recovered publication proof")
	if err != nil {
		return err
	}
	workflowFile, repository, workflowID, err := publicationTarget(target)
	if err != nil {
		return err
	}
	canonicalTarget, err := ValidateTarget(target)
	if err != nil {
		return err
	}
	source, err := publicationAttempt(root["source"], canonicalTarget, workflowFile, repository, workflowID, "source")
	if err != nil {
		return err
	}
	observer, err := publicationAttempt(root["observer"], canonicalTarget, workflowFile, repository, workflowID, "observer")
	if err != nil {
		return err
	}
	if source.runID == observer.runID && source.attempt == observer.attempt {
		return errors.New("publication recovery source and observer are the same workflow attempt")
	}
	if source.runID == observer.runID && source.attempt >= observer.attempt {
		return errors.New("publication recovery observer does not follow its source attempt")
	}
	if source.runID != observer.runID && source.createdAt > observer.createdAt {
		return errors.New("publication recovery observer precedes its source run")
	}
	if source.artifactID == observer.artifactID {
		return errors.New("publication recovery source and observer share an artifact")
	}
	if !Equal(source.policyFiles, observer.policyFiles) {
		return errors.New("publication observer changed the exact approval or event policy files")
	}
	sourceContext, err := publicationAttemptContext(source, workflowFile, repository)
	if err != nil {
		return err
	}
	observerContext, err := publicationAttemptContext(observer, workflowFile, repository)
	if err != nil {
		return err
	}
	if _, err := ValidatePublicationProof(source.publication, target, sourceContext, false); err != nil {
		return fmt.Errorf("source publication proof is invalid: %w", err)
	}
	if _, err := ValidatePublicationProof(observer.publication, target, observerContext, true); err != nil {
		return fmt.Errorf("observer publication proof is invalid: %w", err)
	}
	if sourceContext["phase"] != "prepared" ||
		sourceContext["publication"].(map[string]any)["stage"] != "pr-verify-pending" ||
		!Equal(sourceContext["dispatch_steps"], []any{}) ||
		observerContext["recovery_key"] != sourceContext["recovery_key"] ||
		!Equal(observerContext["attempt_target"], sourceContext["attempt_target"]) ||
		!publicationIntegerIs(observerContext["recovered_from_run_id"], source.runID) ||
		!publicationIntegerIs(observerContext["recovered_from_attempt"], source.attempt) {
		return errors.New("publication recovery cannot replay an unknown write or loses its original target")
	}
	for _, field := range []string{"schema_version", "origin_run", "intent_file", "patch_file", "result_file", "event_file", "candidate_file", "candidate_artifact_file", "candidate_archive_file", "candidate_jobs_file", "push_ack_file", "pr_ack_file", "qualification_file"} {
		if !Equal(source.publication[field], observer.publication[field]) {
			return errors.New("publication observer changed retained intent or positive write acknowledgements")
		}
	}
	return nil
}

type publicationAttemptRecord struct {
	runID, attempt, artifactID int64
	createdAt                  string
	run                        Object
	plans                      []any
	policyFiles                Object
	contextFile                any
	publication                Object
}

func publicationTarget(target Object) (workflowFile, repository string, workflowID int64, err error) {
	target, err = ValidateTarget(target)
	if err != nil {
		return "", "", 0, fmt.Errorf("publication target is invalid: %w", err)
	}
	workflowFile = target["workflow_file"].(string)
	repository = target["repository"].(string)
	workflowID, _ = proofPositiveInteger(target["workflow_id"])
	return workflowFile, repository, workflowID, nil
}

func publicationAttempt(value any, target Object, workflowFile, repository string, workflowID int64, name string) (publicationAttemptRecord, error) {
	attempt, err := Exact(value, []string{"run_id", "attempt", "run", "artifact", "context_file", "plans", "publication", "policy_files"}, name+" publication attempt")
	if err != nil {
		return publicationAttemptRecord{}, err
	}
	runID, err := proofPositiveInteger(attempt["run_id"])
	if err != nil {
		return publicationAttemptRecord{}, fmt.Errorf("%s publication run ID is invalid", name)
	}
	attemptNumber, err := proofPositiveInteger(attempt["attempt"])
	if err != nil {
		return publicationAttemptRecord{}, fmt.Errorf("%s publication attempt number is invalid", name)
	}
	run, err := Exact(attempt["run"], publicationRunFields, name+" publication run")
	if err != nil {
		return publicationAttemptRecord{}, err
	}
	actualRunID, runIDErr := proofPositiveInteger(run["id"])
	actualWorkflowID, workflowIDErr := proofPositiveInteger(run["workflow_id"])
	headSHA, shaOK := run["head_sha"].(string)
	_, branchOK := run["head_branch"].(string)
	createdAt, createdOK := run["created_at"].(string)
	if runIDErr != nil || actualRunID != runID || workflowIDErr != nil || actualWorkflowID != workflowID ||
		!shaOK || !publicationSHA40.MatchString(headSHA) || !branchOK || !createdOK || !nativeNonempty(createdAt) ||
		!nativeNonemptyValue(run["display_title"]) || !nativeNonemptyValue(run["event"]) {
		return publicationAttemptRecord{}, fmt.Errorf("%s publication run differs from its exact workflow attempt", name)
	}
	artifact, err := Exact(attempt["artifact"], []string{"name", "id", "digest"}, name+" publication artifact")
	if err != nil {
		return publicationAttemptRecord{}, err
	}
	artifactID, err := proofPositiveInteger(artifact["id"])
	artifactName, nameOK := artifact["name"].(string)
	artifactDigest, digestOK := artifact["digest"].(string)
	expectedName := RecoveryArtifactName(target, runID, attemptNumber)
	if err != nil || !nameOK || artifactName != expectedName || !digestOK || !observerArtifactID.MatchString(artifactDigest) {
		return publicationAttemptRecord{}, fmt.Errorf("%s publication artifact differs from its actual workflow attempt", name)
	}
	plans, plansOK := attempt["plans"].([]any)
	publication, publicationOK := attempt["publication"].(map[string]any)
	if !plansOK || len(plans) != 0 || !publicationOK {
		return publicationAttemptRecord{}, fmt.Errorf("%s publication attempt has plans or no publication proof", name)
	}
	policyFiles, err := observerPolicyFiles(attempt["policy_files"], name)
	if err != nil {
		return publicationAttemptRecord{}, err
	}
	return publicationAttemptRecord{
		runID: runID, attempt: attemptNumber, artifactID: artifactID, createdAt: createdAt,
		run: run, plans: plans, policyFiles: policyFiles, contextFile: attempt["context_file"], publication: publication,
	}, nil
}

func publicationAttemptContext(attempt publicationAttemptRecord, workflowFile, repository string) (Object, error) {
	value, err := LoadFileProof(attempt.contextFile, "publication recovery context")
	if err != nil {
		return nil, err
	}
	context, ok := value.(map[string]any)
	runName, _ := attempt.run["display_title"].(string)
	if !ok || !publicationIntegerIs(context["schema_version"], 1) || context["workflow_file"] != workflowFile ||
		context["repository"] != repository || !publicationIntegerIs(context["workflow_run_id"], attempt.runID) ||
		!publicationIntegerIs(context["workflow_run_attempt"], attempt.attempt) || context["run_name"] != runName ||
		!observerNonemptyObject(context["attempt_target"]) || !Equal(context["plans"], []any{}) {
		return nil, errors.New("publication recovery context differs from its exact workflow attempt")
	}
	return context, nil
}

func publicationIntegerIs(value any, expected int64) bool {
	number, err := contract.Integer(value)
	return err == nil && number == expected
}

func publicationPositive(value any) bool {
	_, err := proofPositiveInteger(value)
	return err == nil
}

func publicationPositiveEqual(left, right any) bool {
	leftNumber, leftErr := proofPositiveInteger(left)
	rightNumber, rightErr := proofPositiveInteger(right)
	return leftErr == nil && rightErr == nil && leftNumber == rightNumber
}

func publicationBranch(value any, heads bool) bool {
	text, ok := value.(string)
	if !ok || !nativeNonempty(text) {
		return false
	}
	if heads {
		return strings.HasPrefix(text, "refs/heads/") && strings.Count(text, "refs/heads/") == 1 && text != "refs/heads/"
	}
	return !strings.HasPrefix(text, "refs/")
}

func publicationSourceBranch(value any) bool {
	text, ok := value.(string)
	return ok && nativeNonempty(text) && strings.HasPrefix(text, "refs/heads/")
}

func publicationPath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return path.Clean(value) == value
}

func isObject(value any) bool { _, ok := value.(map[string]any); return ok }

func publicationWithoutURLAndNumber(value Object) Object {
	copy := make(Object, len(value)-2)
	for field, item := range value {
		if field != "url" && field != "number" {
			copy[field] = item
		}
	}
	return copy
}

func decodePublicationRawProof(value any, name string) ([]byte, error) {
	proof, err := Exact(value, []string{"sha256", "base64"}, name+" byte proof")
	if err != nil {
		return nil, err
	}
	digest, digestOK := proof["sha256"].(string)
	encoded, encodedOK := proof["base64"].(string)
	if !digestOK || !IsSHA256(digest) || !encodedOK || len(encoded) > base64.StdEncoding.EncodedLen(MaxFileBytes) {
		return nil, fmt.Errorf("%s lacks its bounded original byte digest", name)
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) > MaxFileBytes || base64.StdEncoding.EncodeToString(data) != encoded || SHA256(data) != digest {
		return nil, fmt.Errorf("%s bytes differ from their retained digest", name)
	}
	return data, nil
}

func readOptionalPublicationFile(root, relative string) ([]byte, bool, error) {
	_, err := PackageFile(root, relative)
	if err == nil {
		data, readErr := ReadPackageFile(root, relative)
		return data, readErr == nil, readErr
	}
	absolute, absErr := filepath.Abs(root)
	if absErr != nil {
		return nil, false, absErr
	}
	rootInfo, rootErr := os.Lstat(absolute)
	if rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("publication root must be an existing real directory")
	}
	current := absolute
	parts := strings.Split(relative, "/")
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil, false, nil
		}
		if statErr != nil {
			return nil, false, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, false, errors.New("publication file has a symbolic ancestor or target")
		}
		if index+1 < len(parts) && !info.IsDir() {
			return nil, false, errors.New("publication file parent is not a directory")
		}
		if index+1 == len(parts) {
			return nil, false, fmt.Errorf("publication file is unsafe: %s", relative)
		}
	}
	return nil, false, fmt.Errorf("publication file is unsafe: %s", relative)
}
