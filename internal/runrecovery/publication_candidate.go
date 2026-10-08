package runrecovery

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// AcquirePublicationCandidate retains a verifier's immutable output before a
// publisher prepares its intent. It grants no mutation authority. Recovered
// publication uses its original retained packet instead of acquiring a new one.
func (e *Engine) AcquirePublicationCandidate(ctx context.Context, reader ActionsReader, inv Invocation, artifactID int64, digest, workflowSHA string) (Object, error) {
	if artifactID < 1 || !settlementArtifactDigest.MatchString(digest) || !settlementSHA40.MatchString(workflowSHA) ||
		!nonemptyString(inv.RunName) || !nonemptyString(inv.RecoveryKey) || !e.workflowAllowsPublication(inv.Workflow) {
		return nil, errors.New("publication acquisition requires a scoped workflow and exact invocation, upload and control identities")
	}
	root, err := runnerDirectory(inv.PackageRoot, inv.RunnerTemp, false, false)
	if err != nil {
		return nil, err
	}
	files, err := retainedPackageFiles(root)
	if err != nil || len(files) != 2 {
		return nil, errors.New("publication acquisition requires only a fresh recovery frontier and observation")
	}
	for _, relative := range files {
		if relative != "settlement-chain.json" && relative != "recovery-observation.json" {
			return nil, errors.New("publication acquisition cannot replace retained context, recovery or execution evidence")
		}
	}
	runs, run, target, observed, attempts, err := e.invocationHistory(ctx, reader, inv)
	if err != nil {
		return nil, err
	}
	active := false
	for _, current := range runs {
		if exactInt(current["id"], inv.RunID) && current["status"] == "in_progress" {
			active = true
		}
	}
	if !active {
		return nil, errors.New("publication acquisition requires the currently executing workflow attempt")
	}
	chainValue, err := LoadJSON(filepath.Join(root, "settlement-chain.json"))
	if err != nil {
		return nil, err
	}
	chain, err := e.ValidateChain(chainValue, target, observed, attempts)
	if err != nil {
		return nil, err
	}
	pending, err := PendingAttempts(chain, observed, attempts, inv.RunID, inv.Attempt)
	if err != nil || len(pending) != 0 {
		return nil, errors.New("publication acquisition cannot bypass an unsettled predecessor")
	}
	observationValue, err := LoadJSON(filepath.Join(root, "recovery-observation.json"))
	if err != nil {
		return nil, err
	}
	observation, err := Exact(observationValue, noopObservationFields, "publication recovery observation")
	if err != nil {
		return nil, err
	}
	prefixDigest, err := settlementPrefixDigest(chain["settlements"])
	if err != nil {
		return nil, err
	}
	frontierDigest, err := preparedFrontierDigest(chain["prepared_frontier"])
	if err != nil {
		return nil, err
	}
	emptyFrontierDigest, err := preparedFrontierDigest([]any{})
	if err != nil {
		return nil, err
	}
	if !exactInt(observation["schema_version"], 1) || observation["outcome"] != "fresh" ||
		!Equal(observation["target"], target) || !Equal(observation["run"], run) || !exactInt(observation["run_id"], inv.RunID) ||
		!exactInt(observation["attempt"], inv.Attempt) || observation["recovery_key"] != inv.RecoveryKey || observation["chain_sha256"] != prefixDigest ||
		observation["prepared_frontier_sha256"] != frontierDigest || frontierDigest != emptyFrontierDigest {
		return nil, errors.New("publication acquisition lost its exact fresh recovery observation")
	}
	metadata, err := reader.Read(ctx, fmt.Sprintf("repos/%s/actions/artifacts/%d", e.repository.FullName(), artifactID))
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("gh-steward-candidate-%d-%d", inv.RunID, inv.Attempt)
	artifact, err := artifactIdentity(metadata, name, inv.RunID)
	metadataRun, runErr := object(metadata["workflow_run"], "publication candidate run")
	if err != nil || runErr != nil || !exactInt(artifact["id"], artifactID) || artifact["digest"] != digest || metadataRun["head_sha"] != run["head_sha"] {
		return nil, errors.New("publication candidate differs from its exact current upload receipt or source")
	}
	payload, err := downloadArtifact(ctx, reader, artifact)
	if err != nil {
		return nil, err
	}
	// The archive is retained as one proof file, so the regular-file limit also
	// applies to its compressed bytes, independently of expanded ZIP limits.
	if len(payload) > MaxFileBytes {
		return nil, errors.New("publication candidate ZIP exceeds its retained file bound")
	}
	archive, err := archiveFiles(payload, false)
	if err != nil || len(archive) != len(publicationCandidateArchiveFiles) {
		return nil, errors.New("publication candidate has an unsupported archive inventory")
	}
	for _, relative := range publicationCandidateArchiveFiles {
		if _, exists := archive[relative]; !exists {
			return nil, errors.New("publication candidate archive omits an exact required file")
		}
	}
	candidateValue, err := DecodeValue(archive[publicationPaths["candidate_file"]])
	if err != nil {
		return nil, err
	}
	candidate, err := Exact(candidateValue, publicationCandidateFields, "acquired publication candidate")
	if err != nil {
		return nil, err
	}
	if candidate["workflow_sha"] != workflowSHA || candidate["event_name"] != run["event"] ||
		candidate["source_repository"] != e.repository.FullName() || !publicationSourceBranch(candidate["source_ref"]) ||
		!publicationBranch(candidate["base_ref"], false) || !publicationSHA40.MatchString(fmt.Sprint(candidate["source_sha"])) ||
		!publicationSHA40.MatchString(fmt.Sprint(candidate["base_sha"])) ||
		candidate["trigger_event_sha256"] != SHA256(archive[publicationPaths["event_file"]]) ||
		candidate["patch_sha256"] != SHA256(archive[publicationPaths["patch_file"]]) ||
		candidate["result_sha256"] != SHA256(archive[publicationPaths["result_file"]]) {
		return nil, errors.New("publication candidate source, event or raw file identity differs from the trusted invocation")
	}
	if _, err := DecodeValue(archive[publicationPaths["event_file"]]); err != nil {
		return nil, err
	}
	if _, err := DecodeValue(archive[publicationPaths["result_file"]]); err != nil {
		return nil, err
	}
	jobs, err := reader.Pages(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", e.repository.FullName(), inv.RunID, inv.Attempt))
	if err != nil {
		return nil, err
	}
	metadataBytes, err := Canonical(metadata)
	if err != nil || len(metadataBytes) > MaxFileBytes {
		return nil, errors.New("publication candidate metadata is invalid or oversized")
	}
	jobsBytes, err := Canonical(jobs)
	if err != nil || len(jobsBytes) > MaxFileBytes {
		return nil, errors.New("publication candidate jobs are invalid or oversized")
	}
	retained := Object{
		"candidate_file":          MakeFileProof(archive[publicationPaths["candidate_file"]]),
		"patch_file":              MakeFileProof(archive[publicationPaths["patch_file"]]),
		"result_file":             MakeFileProof(archive[publicationPaths["result_file"]]),
		"event_file":              MakeFileProof(archive[publicationPaths["event_file"]]),
		"candidate_artifact_file": MakeFileProof(metadataBytes), "candidate_archive_file": MakeFileProof(payload),
		"candidate_jobs_file": MakeFileProof(jobsBytes),
	}
	intent, err := cloneObject(candidate)
	if err != nil {
		return nil, err
	}
	intent["candidate_sha256"] = SHA256(archive[publicationPaths["candidate_file"]])
	if err := validatePublicationCandidate(retained, intent, run, target, inv.RunID); err != nil {
		return nil, err
	}
	if err := verifyPublicationCandidateLive(ctx, reader, e.repository, retained, payload); err != nil {
		return nil, err
	}
	// Acquisition failures above preserve the frontier without leaving a partial
	// candidate. A filesystem failure below remains visible and cannot be retried
	// over an existing packet or interpreted as publication qualification.
	packet := map[string][]byte{
		publicationPaths["candidate_file"]:          archive[publicationPaths["candidate_file"]],
		publicationPaths["patch_file"]:              archive[publicationPaths["patch_file"]],
		publicationPaths["result_file"]:             archive[publicationPaths["result_file"]],
		publicationPaths["event_file"]:              archive[publicationPaths["event_file"]],
		publicationPaths["candidate_artifact_file"]: metadataBytes,
		publicationPaths["candidate_archive_file"]:  payload,
		publicationPaths["candidate_jobs_file"]:     jobsBytes,
	}
	for relative, raw := range packet {
		if err := persistPackageFile(root, relative, raw); err != nil {
			return nil, err
		}
	}
	return Object{"outcome": "acquired", "artifact_name": name, "candidate_sha256": intent["candidate_sha256"]}, nil
}
