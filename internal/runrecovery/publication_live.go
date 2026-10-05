package runrecovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// VerifyPublication is a read-only gate over the caller's persisted intent.
// New writes require the current trusted workflow source. A recovered intent
// may only continue read-only PR observation after its saved positive ACKs.
func (e *Engine) VerifyPublication(ctx context.Context, reader ActionsReader, invocation Invocation, workflowSHA string) (Object, error) {
	if !settlementSHA40.MatchString(workflowSHA) {
		return nil, errors.New("an exact trusted workflow source SHA is required")
	}
	root, err := runnerDirectory(invocation.PackageRoot, invocation.RunnerTemp, false, false)
	if err != nil {
		return nil, err
	}
	runContext, err := e.invocationContext(root, invocation)
	if err != nil {
		return nil, err
	}
	cutover, err := e.previewOnlyCutoverAtRoot(root, invocation.Workflow)
	if err != nil {
		return nil, err
	}
	if cutover {
		return nil, errors.New("preview-only history cutover disables publication")
	}
	if !e.workflowAllowsPublication(invocation.Workflow) {
		return nil, errors.New("consumer policy does not allow this publication workflow")
	}
	runs, _, target, _, _, err := e.invocationHistory(ctx, reader, invocation)
	if err != nil {
		return nil, err
	}
	origin, err := publicationOrigin(runs, runContext)
	if err != nil {
		return nil, err
	}
	proof, err := BuildPublicationProof(root, origin, runContext, target, false)
	if err != nil {
		return nil, err
	}
	if runContext["recovered_from_run_id"] != nil || runContext["recovered_from_attempt"] != nil {
		data, err := ReadPackageFile(root, "recovery-source.json")
		if err != nil {
			return nil, err
		}
		sourceValue, err := DecodeValue(data)
		if err != nil {
			return nil, err
		}
		source, err := Exact(sourceValue, []string{"run_id", "attempt", "run", "artifact", "context_file", "plans", "publication", "policy_files"}, "original publication recovery source")
		if err != nil {
			return nil, err
		}
		sourceContextValue, err := LoadFileProof(source["context_file"], "original publication source context")
		if err != nil {
			return nil, err
		}
		sourceContext, err := object(sourceContextValue, "original publication source context")
		if err != nil {
			return nil, err
		}
		sourceProof, err := object(source["publication"], "original publication source proof")
		if err != nil {
			return nil, err
		}
		if _, err := ValidatePublicationProof(sourceProof, target, sourceContext, false); err != nil {
			return nil, err
		}
		sourceQualificationValue, err := LoadFileProof(sourceProof["qualification_file"], "original publication qualification")
		if err != nil {
			return nil, err
		}
		sourceQualification, err := object(sourceQualificationValue, "original publication qualification")
		if err != nil {
			return nil, err
		}
		sourceWorkflowSHA, _ := sourceQualification["workflow_sha"].(string)
		if err := ValidatePublicationQualification(sourceProof, target, sourceContext, sourceWorkflowSHA); err != nil {
			return nil, err
		}
		publication, err := object(runContext["publication"], "recovered publication context")
		if err != nil {
			return nil, err
		}
		sourcePublication, err := object(sourceContext["publication"], "original publication context")
		if err != nil {
			return nil, err
		}
		if runContext["phase"] != "prepared" || sourceContext["phase"] != "prepared" ||
			publication["stage"] != "pr-verify-pending" || sourcePublication["stage"] != "pr-verify-pending" ||
			!Equal(source["run_id"], runContext["recovered_from_run_id"]) || !Equal(source["attempt"], runContext["recovered_from_attempt"]) ||
			!Equal(sourceContext["attempt_target"], runContext["attempt_target"]) || sourceContext["recovery_key"] != runContext["recovery_key"] {
			return nil, errors.New("recovered publication cannot authorize another write or change its original target")
		}
		for _, field := range publicationProofFields {
			if field != "verify_ack_file" && !Equal(sourceProof[field], proof[field]) {
				return nil, errors.New("recovered publication changed original candidate, intent or positive write ACKs")
			}
		}
		workflowSHA = ""
	} else if !Equal(origin["id"], invocation.RunID) || !exactInt(runContext["publication"].(Object)["origin_run_attempt"], invocation.Attempt) {
		return nil, errors.New("new publication is not owned by the exact current workflow attempt")
	}
	if proof["qualification_file"] != nil && runContext["recovered_from_run_id"] == nil && runContext["recovered_from_attempt"] == nil {
		if err := ValidatePublicationQualification(proof, target, runContext, workflowSHA); err != nil {
			return nil, err
		}
	}
	if err := VerifyPublicationAcquisition(ctx, reader, e.repository, root, proof, target, runContext, false, workflowSHA); err != nil {
		return nil, err
	}
	if proof["qualification_file"] == nil {
		qualification, err := publicationQualification(proof, target, runContext, workflowSHA)
		if err != nil {
			return nil, err
		}
		if err := persistPublicationQualification(root, qualification); err != nil {
			return nil, err
		}
		proof, err = BuildPublicationProof(root, origin, runContext, target, false)
		if err != nil {
			return nil, err
		}
		if err := ValidatePublicationQualification(proof, target, runContext, workflowSHA); err != nil {
			return nil, err
		}
	}
	return Object{"outcome": "verified", "candidate_sha256": proof["candidate_file"].(Object)["sha256"]}, nil
}

// VerifyPublicationAcquisition authenticates the retained candidate against
// GitHub before a trusted caller dispatches a publication or closes a receipt.
// The pure proof remains independently checkable after provider retention ends.
func VerifyPublicationAcquisition(ctx context.Context, reader ActionsReader, repository contract.Repository, root string, proof, target, runContext Object, requireCompleted bool, workflowSHA string) error {
	intent, err := ValidatePublicationProof(proof, target, runContext, requireCompleted)
	if err != nil {
		return err
	}
	if workflowSHA != "" && (!settlementSHA40.MatchString(workflowSHA) || intent["workflow_sha"] != workflowSHA) {
		return errors.New("candidate workflow source differs from the trusted publisher")
	}
	retainedPayload, err := ReadPackageFile(root, "publication/candidate-archive.zip")
	if err != nil {
		return err
	}
	return verifyPublicationCandidateLive(ctx, reader, repository, proof, retainedPayload)
}

func verifyPublicationCandidateLive(ctx context.Context, reader ActionsReader, repository contract.Repository, proof Object, retainedPayload []byte) error {
	candidateValue, err := LoadFileProof(proof["candidate_file"], "publication candidate")
	if err != nil {
		return err
	}
	candidate, err := object(candidateValue, "publication candidate")
	if err != nil {
		return err
	}
	retainedMetadataValue, err := LoadFileProof(proof["candidate_artifact_file"], "candidate artifact metadata")
	if err != nil {
		return err
	}
	retainedMetadata, err := object(retainedMetadataValue, "candidate artifact metadata")
	if err != nil {
		return err
	}
	id, err := positiveInteger(retainedMetadata["id"], "candidate artifact ID")
	if err != nil {
		return err
	}
	originID, err := positiveInteger(candidate["origin_run_id"], "candidate origin run ID")
	if err != nil {
		return err
	}
	originAttempt, err := positiveInteger(candidate["origin_run_attempt"], "candidate origin attempt")
	if err != nil {
		return err
	}
	metadata, err := reader.Read(ctx, fmt.Sprintf("repos/%s/actions/artifacts/%d", repository.FullName(), id))
	if err != nil {
		return err
	}
	name := fmt.Sprintf("gh-steward-candidate-%d-%d", originID, originAttempt)
	artifact, err := artifactIdentity(metadata, name, originID)
	if err != nil || !Equal(metadata["id"], retainedMetadata["id"]) || metadata["digest"] != retainedMetadata["digest"] {
		return errors.New("retained candidate artifact differs from GitHub metadata")
	}
	liveRun, liveRunErr := contract.ObjectAt(metadata, "workflow_run")
	retainedRun, retainedRunErr := contract.ObjectAt(retainedMetadata, "workflow_run")
	if liveRunErr != nil || retainedRunErr != nil || liveRun["head_sha"] != retainedRun["head_sha"] {
		return errors.New("candidate artifact source differs from GitHub metadata")
	}
	payload, err := downloadArtifact(ctx, reader, artifact)
	if err != nil {
		return err
	}
	if !bytes.Equal(payload, retainedPayload) {
		return errors.New("retained candidate ZIP differs from the uploaded artifact")
	}
	pages, err := reader.Pages(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", repository.FullName(), originID, originAttempt))
	if err != nil {
		return err
	}
	jobs, err := completePages(pages, "jobs")
	if err != nil {
		return err
	}
	matches := 0
	for _, job := range jobs {
		if job["name"] != candidate["verification_job_name"] {
			continue
		}
		matches++
		if !exactInt(job["run_id"], originID) || !exactInt(job["run_attempt"], originAttempt) || job["status"] != "completed" || job["conclusion"] != "success" {
			return errors.New("candidate verification job did not succeed for the exact attempt")
		}
	}
	if matches != 1 {
		return errors.New("candidate has no unique successful verification job")
	}
	retainedJobsValue, err := LoadFileProof(proof["candidate_jobs_file"], "candidate verification jobs")
	if err != nil {
		return err
	}
	retainedPages, err := array(retainedJobsValue, "candidate jobs pages")
	if err != nil {
		return err
	}
	retainedJobs, err := completePages(retainedPages, "jobs")
	if err != nil {
		return err
	}
	// Other jobs can progress while the publisher runs. Bind the immutable
	// successful candidate job, rather than requiring identical unrelated jobs.
	var retainedVerification Object
	for _, job := range retainedJobs {
		if job["name"] == candidate["verification_job_name"] {
			retainedVerification = job
		}
	}
	for _, job := range jobs {
		if job["name"] == candidate["verification_job_name"] && (retainedVerification == nil || !Equal(job["id"], retainedVerification["id"])) {
			return errors.New("retained verification job differs from GitHub identity")
		}
	}
	upstream, err := object(candidate["upstream_artifacts"], "candidate upstream artifacts")
	if err != nil {
		return err
	}
	for _, key := range []string{"capture", "proposal"} {
		ref, err := Exact(upstream[key], []string{"id", "name", "digest"}, "upstream "+key+" artifact")
		if err != nil {
			return err
		}
		id, err := positiveInteger(ref["id"], "upstream artifact ID")
		if err != nil {
			return err
		}
		name, err := contract.Nonempty(ref, "name")
		if err != nil {
			return err
		}
		metadata, err := reader.Read(ctx, fmt.Sprintf("repos/%s/actions/artifacts/%d", repository.FullName(), id))
		if err != nil {
			return err
		}
		actual, err := artifactIdentity(metadata, name, originID)
		if err != nil || !Equal(ref, actual) {
			return errors.New("candidate upstream artifact differs from GitHub identity")
		}
	}
	return nil
}
