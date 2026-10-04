package runrecovery

import (
	"bytes"
	"errors"
	"fmt"
	"os"
)

var publicationQualificationFields = []string{
	"schema_version", "workflow_file", "workflow_id", "repository", "server_url", "run_id", "attempt", "run_name",
	"recovery_key", "attempt_target", "workflow_sha", "raw_files", "upstream_artifacts",
	"candidate_artifact", "verification_job_id",
}

var publicationQualificationRawFiles = []string{"intent", "candidate", "patch", "result", "event"}

// ValidatePublicationQualification checks the durable publisher qualification
// against the exact retained publication bytes and trusted workflow source.
// workflowSHA is supplied by the caller's trusted runtime, never by run API
// head_sha data.
func ValidatePublicationQualification(proof, target, runContext Object, workflowSHA string) error {
	if !publicationSHA40.MatchString(workflowSHA) {
		return errors.New("an exact trusted workflow source SHA is required for publication qualification")
	}
	if _, err := ValidatePublicationProof(proof, target, runContext, false); err != nil {
		return err
	}
	qualificationValue, err := LoadFileProof(proof["qualification_file"], "publication qualification")
	if err != nil {
		return err
	}
	qualification, err := Exact(qualificationValue, publicationQualificationFields, "publication qualification")
	if err != nil {
		return err
	}
	intentValue, err := LoadFileProof(proof["intent_file"], "publication intent")
	if err != nil {
		return err
	}
	intent, err := object(intentValue, "publication intent")
	if err != nil {
		return err
	}
	if qualification["workflow_sha"] != workflowSHA || intent["workflow_sha"] != workflowSHA {
		return errors.New("publication qualification differs from the trusted workflow source")
	}
	return nil
}

// publicationQualification creates the exact current-origin qualification.
// The only production caller is VerifyPublication, after its live acquisition
// gate succeeds and before the workflow can perform a publication write.
func publicationQualification(proof, target, runContext Object, workflowSHA string) (Object, error) {
	if proof["qualification_file"] != nil {
		return nil, errors.New("publication qualification already exists")
	}
	if !publicationSHA40.MatchString(workflowSHA) {
		return nil, errors.New("an exact trusted workflow source SHA is required")
	}
	intent, err := ValidatePublicationProof(proof, target, runContext, false)
	if err != nil {
		return nil, err
	}
	publication, err := object(runContext["publication"], "publication context")
	if err != nil || publication["stage"] != "push-pending" || workflowSHA != intent["workflow_sha"] {
		return nil, errors.New("only an unacknowledged current publication can create a qualification")
	}
	origin, err := object(proof["origin_run"], "publication origin run")
	if err != nil {
		return nil, err
	}
	runID, err := proofPositiveInteger(origin["id"])
	if err != nil {
		return nil, err
	}
	attempt, err := proofPositiveInteger(intent["origin_run_attempt"])
	if err != nil {
		return nil, err
	}
	contextRunID, runIDErr := proofPositiveInteger(runContext["workflow_run_id"])
	contextAttempt, attemptErr := proofPositiveInteger(runContext["workflow_run_attempt"])
	if runIDErr != nil || attemptErr != nil || contextRunID != runID || contextAttempt != attempt ||
		runContext["workflow_file"] != target["workflow_file"] || runContext["repository"] != target["repository"] ||
		runContext["run_name"] != origin["display_title"] || runContext["recovery_key"] != intent["recovery_key"] ||
		!nonemptyObject(runContext["attempt_target"]) {
		return nil, errors.New("publication qualification is not owned by the exact current workflow attempt")
	}
	return makePublicationQualification(proof, target, runContext, workflowSHA)
}

func makePublicationQualification(proof, target, runContext Object, workflowSHA string) (Object, error) {
	intentValue, err := LoadFileProof(proof["intent_file"], "publication intent")
	if err != nil {
		return nil, err
	}
	intent, err := object(intentValue, "publication intent")
	if err != nil {
		return nil, err
	}
	if !publicationSHA40.MatchString(workflowSHA) || intent["workflow_sha"] != workflowSHA {
		return nil, errors.New("publication qualification source SHA differs from its retained intent")
	}
	origin, err := object(proof["origin_run"], "publication origin run")
	if err != nil {
		return nil, err
	}
	candidateValue, err := LoadFileProof(proof["candidate_file"], "publication candidate")
	if err != nil {
		return nil, err
	}
	candidate, err := object(candidateValue, "publication candidate")
	if err != nil {
		return nil, err
	}
	metadataValue, err := LoadFileProof(proof["candidate_artifact_file"], "candidate artifact metadata")
	if err != nil {
		return nil, err
	}
	metadata, err := object(metadataValue, "candidate artifact metadata")
	if err != nil {
		return nil, err
	}
	artifact := Object{"id": metadata["id"], "name": metadata["name"], "digest": metadata["digest"]}
	jobID, err := publicationVerificationJobID(proof)
	if err != nil {
		return nil, err
	}
	rawFiles := Object{}
	for _, name := range publicationQualificationRawFiles {
		file, err := Exact(proof[name+"_file"], []string{"sha256", "base64"}, "publication "+name+" file proof")
		if err != nil || !IsSHA256(file["sha256"]) {
			return nil, fmt.Errorf("publication %s raw file hash is invalid", name)
		}
		rawFiles[name] = file["sha256"]
	}
	upstream, err := Exact(candidate["upstream_artifacts"], []string{"capture", "proposal"}, "publication upstream artifacts")
	if err != nil {
		return nil, err
	}
	upstreamCopy := Object{}
	for _, role := range []string{"capture", "proposal"} {
		ref, err := Exact(upstream[role], []string{"id", "name", "digest"}, "publication upstream "+role+" artifact")
		if err != nil {
			return nil, err
		}
		upstreamCopy[role] = ref
	}
	artifactID, idErr := proofPositiveInteger(artifact["id"])
	artifactName, nameOK := artifact["name"].(string)
	artifactDigest, digestOK := artifact["digest"].(string)
	if idErr != nil || !nameOK || !nativeNonempty(artifactName) || !digestOK || !publicationArtifactDigest.MatchString(artifactDigest) {
		return nil, errors.New("candidate artifact identity is invalid for qualification")
	}
	runID, idErr := proofPositiveInteger(origin["id"])
	attempt, attemptErr := proofPositiveInteger(intent["origin_run_attempt"])
	if idErr != nil || attemptErr != nil {
		return nil, errors.New("publication origin run identity is invalid for qualification")
	}
	attemptTarget, err := object(runContext["attempt_target"], "publication attempt target")
	if err != nil || len(attemptTarget) == 0 {
		return nil, errors.New("publication qualification requires an exact attempt target")
	}
	return Object{
		"schema_version": int64(1), "workflow_file": target["workflow_file"], "workflow_id": target["workflow_id"], "repository": target["repository"],
		"server_url": target["server_url"], "run_id": runID, "attempt": attempt, "run_name": origin["display_title"],
		"recovery_key": intent["recovery_key"], "attempt_target": attemptTarget, "workflow_sha": workflowSHA,
		"raw_files": rawFiles, "upstream_artifacts": upstreamCopy,
		"candidate_artifact":  Object{"id": artifactID, "name": artifactName, "digest": artifactDigest},
		"verification_job_id": jobID,
	}, nil
}

func validatePublicationQualification(proof, target, runContext, intent, origin Object) error {
	publication, err := object(runContext["publication"], "publication context")
	if err != nil {
		return err
	}
	if proof["qualification_file"] == nil {
		if publication["stage"] == "push-pending" && publication["push_ack"] == nil && publication["pr_ack"] == nil && publication["verify_ack"] == nil {
			return nil
		}
		return errors.New("publication with a positive write acknowledgement lacks its durable qualification")
	}
	raw, err := LoadRawFileProof(proof["qualification_file"], "publication qualification")
	if err != nil {
		return err
	}
	value, err := DecodeValue(raw)
	if err != nil {
		return err
	}
	qualification, err := Exact(value, publicationQualificationFields, "publication qualification")
	if err != nil {
		return err
	}
	canonical, err := Canonical(qualification)
	if err != nil || (!bytes.Equal(raw, canonical) && !bytes.Equal(raw, append(append([]byte(nil), canonical...), '\n'))) {
		return errors.New("publication qualification is not canonical JSON")
	}
	if !publicationIntegerIs(qualification["schema_version"], 1) {
		return errors.New("publication qualification uses an unsupported schema")
	}
	qualificationWorkflowSHA, workflowSHAOK := qualification["workflow_sha"].(string)
	runID, runIDErr := proofPositiveInteger(origin["id"])
	attempt, attemptErr := proofPositiveInteger(intent["origin_run_attempt"])
	contextWorkflowFile, workflowFileOK := runContext["workflow_file"].(string)
	contextRepository, repositoryOK := runContext["repository"].(string)
	contextRecoveryRun, recoveryRunExists := runContext["recovered_from_run_id"]
	contextRecoveryAttempt, recoveryAttemptExists := runContext["recovered_from_attempt"]
	if recoveryRunExists != recoveryAttemptExists {
		return errors.New("publication observer context has an incomplete source identity")
	}
	if recoveryRunExists {
		recoveredRun, recoveredRunErr := proofPositiveInteger(contextRecoveryRun)
		recoveredAttempt, recoveredAttemptErr := proofPositiveInteger(contextRecoveryAttempt)
		if recoveredRunErr != nil || recoveredAttemptErr != nil || recoveredRun != runID || recoveredAttempt != attempt {
			return errors.New("publication observer does not identify its exact qualified source attempt")
		}
	} else {
		contextRun, contextRunErr := proofPositiveInteger(runContext["workflow_run_id"])
		contextAttempt, contextAttemptErr := proofPositiveInteger(runContext["workflow_run_attempt"])
		if contextRunErr != nil || contextAttemptErr != nil || contextRun != runID || contextAttempt != attempt ||
			runContext["run_name"] != qualification["run_name"] {
			return errors.New("publication qualification differs from its current workflow attempt")
		}
	}
	if runIDErr != nil || attemptErr != nil || !workflowSHAOK || !publicationSHA40.MatchString(qualificationWorkflowSHA) ||
		!workflowFileOK || contextWorkflowFile != target["workflow_file"] || !repositoryOK || contextRepository != target["repository"] ||
		qualification["workflow_file"] != target["workflow_file"] || !Equal(qualification["workflow_id"], target["workflow_id"]) || qualification["repository"] != target["repository"] ||
		qualification["server_url"] != target["server_url"] || !publicationIntegerIs(qualification["run_id"], runID) ||
		!publicationIntegerIs(qualification["attempt"], attempt) || qualification["run_name"] != origin["display_title"] ||
		qualification["recovery_key"] != intent["recovery_key"] || qualification["workflow_sha"] != intent["workflow_sha"] ||
		!Equal(qualification["attempt_target"], runContext["attempt_target"]) || !nonemptyObject(qualification["attempt_target"]) {
		return errors.New("publication qualification differs from its exact workflow, attempt or trusted source")
	}
	rawFiles, err := Exact(qualification["raw_files"], publicationQualificationRawFiles, "publication qualification raw files")
	if err != nil {
		return err
	}
	for _, name := range publicationQualificationRawFiles {
		file, err := Exact(proof[name+"_file"], []string{"sha256", "base64"}, "publication "+name+" file proof")
		if err != nil || rawFiles[name] != file["sha256"] {
			return errors.New("publication qualification raw file hashes differ from the retained source bytes")
		}
	}
	candidateValue, err := LoadFileProof(proof["candidate_file"], "publication candidate")
	if err != nil {
		return err
	}
	candidate, err := object(candidateValue, "publication candidate")
	if err != nil {
		return err
	}
	upstream, err := Exact(candidate["upstream_artifacts"], []string{"capture", "proposal"}, "publication upstream artifacts")
	if err != nil || !Equal(qualification["upstream_artifacts"], upstream) {
		return errors.New("publication qualification upstream artifact identities changed")
	}
	metadataValue, err := LoadFileProof(proof["candidate_artifact_file"], "candidate artifact metadata")
	if err != nil {
		return err
	}
	metadata, err := object(metadataValue, "candidate artifact metadata")
	if err != nil {
		return err
	}
	qualifiedArtifact, err := Exact(qualification["candidate_artifact"], []string{"id", "name", "digest"}, "qualified candidate artifact")
	if err != nil || !Equal(qualifiedArtifact, Object{"id": metadata["id"], "name": metadata["name"], "digest": metadata["digest"]}) {
		return errors.New("publication qualification candidate artifact identity changed")
	}
	jobID, err := publicationVerificationJobID(proof)
	if err != nil || !publicationIntegerIs(qualification["verification_job_id"], jobID) {
		return errors.New("publication qualification verification job identity changed")
	}
	return nil
}

func publicationVerificationJobID(proof Object) (int64, error) {
	value, err := LoadFileProof(proof["candidate_jobs_file"], "candidate workflow jobs")
	if err != nil {
		return 0, err
	}
	pages, err := array(value, "candidate workflow jobs pages")
	if err != nil {
		return 0, err
	}
	jobs, err := completePages(pages, "jobs")
	if err != nil {
		return 0, err
	}
	var selected int64
	matches := 0
	candidateValue, err := LoadFileProof(proof["candidate_file"], "publication candidate")
	if err != nil {
		return 0, err
	}
	candidate, err := object(candidateValue, "publication candidate")
	if err != nil {
		return 0, err
	}
	runID, runIDErr := proofPositiveInteger(candidate["origin_run_id"])
	attempt, attemptErr := proofPositiveInteger(candidate["origin_run_attempt"])
	if runIDErr != nil || attemptErr != nil {
		return 0, errors.New("publication candidate has no exact attempt identity")
	}
	for _, job := range jobs {
		if job["name"] != "Verify publication candidate" {
			continue
		}
		matches++
		id, idErr := proofPositiveInteger(job["id"])
		if idErr != nil || runIDErr != nil || attemptErr != nil || !publicationIntegerIs(job["run_id"], runID) ||
			!publicationIntegerIs(job["run_attempt"], attempt) || job["status"] != "completed" || job["conclusion"] != "success" {
			return 0, errors.New("publication verification job is not a unique successful job for the exact attempt")
		}
		selected = id
	}
	if matches != 1 || selected < 1 {
		return 0, errors.New("publication has no unique successful verification job identity")
	}
	return selected, nil
}

func persistPublicationQualification(root string, value Object) error {
	data, err := Canonical(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > MaxFileBytes {
		return errors.New("publication qualification exceeds its file bound")
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Mkdir("publication", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	parentInfo, err := directory.Lstat("publication")
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("publication qualification parent is not a real directory")
	}
	parent, err := directory.OpenRoot("publication")
	if err != nil {
		return err
	}
	defer parent.Close()
	openedDirectory, err := parent.Open(".")
	if err != nil {
		return errors.New("publication qualification parent could not be verified")
	}
	openedInfo, statErr := openedDirectory.Stat()
	openedCloseErr := openedDirectory.Close()
	if statErr != nil || openedCloseErr != nil || !openedInfo.IsDir() || !os.SameFile(parentInfo, openedInfo) {
		return errors.New("publication qualification parent changed during verification")
	}
	child, err := parent.OpenFile("qualification.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("publication qualification already exists or cannot be created")
	}
	n, writeErr := child.Write(data)
	syncErr := child.Sync()
	fileCloseErr := child.Close()
	if writeErr != nil || n != len(data) || syncErr != nil || fileCloseErr != nil {
		return errors.New("publication qualification could not be durably written")
	}
	syncDirectory, err := parent.Open(".")
	if err != nil {
		return errors.New("publication qualification directory could not be opened for synchronization")
	}
	defer syncDirectory.Close()
	if err := syncDirectory.Sync(); err != nil {
		return errors.New("publication qualification directory could not be durably synchronized")
	}
	return nil
}

func nonemptyObject(value any) bool {
	object, ok := value.(map[string]any)
	return ok && len(object) > 0
}
