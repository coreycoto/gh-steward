package runrecovery

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicationProofVerifiesRawIntentLeaseAndACKs(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "completed")
	intent, err := ValidatePublicationProof(proof, target, context, true)
	if err != nil {
		t.Fatalf("valid publication proof rejected: %v", err)
	}
	intentValue, err := LoadFileProof(proof["intent_file"], "publication intent")
	if err != nil {
		t.Fatal(err)
	}
	if intentValue.(Object)["workflow_sha"] == proof["origin_run"].(Object)["head_sha"] {
		t.Fatal("fixture must distinguish control workflow SHA from the run's code head SHA")
	}
	if intent["repository"] != "sample/repo" || intent["stage"] != nil {
		t.Fatalf("unexpected publication intent: %#v", intent)
	}

	badLease := clonePublicationFixture(proof, context)
	publication := badLease.context["publication"].(Object)
	ackValue, err := LoadFileProof(badLease.proof["push_ack_file"], "push ack")
	if err != nil {
		t.Fatal(err)
	}
	ack := ackValue.(Object)
	ack["expected_old_sha"] = strings.Repeat("f", 40)
	ackBytes, err := Canonical(ack)
	if err != nil {
		t.Fatal(err)
	}
	badLease.proof["push_ack_file"] = MakeFileProof(ackBytes)
	publication["push_ack"] = ack
	if _, err := ValidatePublicationProof(badLease.proof, target, badLease.context, true); err == nil {
		t.Fatal("push acknowledgement with another expected-old-SHA lease was accepted")
	}

	badPatch := clonePublicationFixture(proof, context)
	patchBytes, err := decodePublicationRawProof(badPatch.proof["patch_file"], "patch")
	if err != nil {
		t.Fatal(err)
	}
	badPatch.proof["patch_file"] = MakeFileProof(append(patchBytes, []byte("# changed\n")...))
	if _, err := ValidatePublicationProof(badPatch.proof, target, badPatch.context, true); err == nil {
		t.Fatal("patch bytes that differ from their captured intent were accepted")
	}
}

func TestPublicationProofRejectsDuplicateLexicalAndMissingTerminalEvidence(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "completed")
	intentProof, err := decodePublicationRawProof(proof["intent_file"], "intent")
	if err != nil {
		t.Fatal(err)
	}
	duplicate := strings.Replace(string(intentProof), `"schema_version":2`, `"schema_version":2,"schema_version":2`, 1)
	changed := clonePublicationFixture(proof, context)
	changed.proof["intent_file"] = MakeFileProof([]byte(duplicate))
	if _, err := ValidatePublicationProof(changed.proof, target, changed.context, true); err == nil {
		t.Fatal("duplicate intent key was accepted")
	}

	intentRaw, err := decodePublicationRawProof(proof["intent_file"], "intent")
	if err != nil {
		t.Fatal(err)
	}
	changed = clonePublicationFixture(proof, context)
	lexical := strings.Replace(string(intentRaw), `"schema_version":2`, `"schema_version":2.0`, 1)
	changed.proof["intent_file"] = MakeFileProof([]byte(lexical))
	if _, err := ValidatePublicationProof(changed.proof, target, changed.context, true); err == nil {
		t.Fatal("fractional spelling of the publication schema version was accepted")
	}

	changed = clonePublicationFixture(proof, context)
	changed.proof["verify_ack_file"] = nil
	changed.context["publication"].(Object)["verify_ack"] = nil
	if _, err := ValidatePublicationProof(changed.proof, target, changed.context, true); err == nil {
		t.Fatal("completed publication without positive verification ACK was accepted")
	}
}

func TestPublicationProofRequiresV2AndHostBoundTarget(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "completed")
	for _, unsupported := range []any{int64(1), float64(2)} {
		changed := clonePublicationFixture(proof, context)
		changed.proof["schema_version"] = unsupported
		if _, err := ValidatePublicationProof(changed.proof, target, changed.context, true); err == nil {
			t.Fatalf("publication proof schema version %v was accepted", unsupported)
		}
	}
	withoutHost := clonePublicationFixture(proof, context)
	delete(withoutHost.target, "server_url")
	if _, err := ValidatePublicationProof(withoutHost.proof, withoutHost.target, withoutHost.context, true); err == nil {
		t.Fatal("publication target without its canonical host was accepted")
	}

	wrongRepositoryURL := clonePublicationFixture(proof, context)
	wrongRepositoryURL.target["server_url"] = "https://github.com/other/repo"
	if _, err := ValidatePublicationProof(wrongRepositoryURL.proof, wrongRepositoryURL.target, wrongRepositoryURL.context, true); err == nil {
		t.Fatal("publication target URL that identifies another repository was accepted")
	}
}

func TestPublicationCandidateBindsRawArtifactArchiveAndVerificationJob(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "completed")
	changed := clonePublicationFixture(proof, context)
	changedCandidate, err := LoadFileProof(changed.proof["candidate_file"], "candidate")
	if err != nil {
		t.Fatal(err)
	}
	changedCandidate.(Object)["source_sha"] = strings.Repeat("9", 40)
	changed.proof["candidate_file"] = nativeJSONFileProof(t, changedCandidate)
	if _, err := ValidatePublicationProof(changed.proof, target, changed.context, true); err == nil {
		t.Fatal("candidate bytes that differ from intent and archived candidate were accepted")
	}

	changed = clonePublicationFixture(proof, context)
	metadata, err := LoadFileProof(changed.proof["candidate_artifact_file"], "candidate artifact")
	if err != nil {
		t.Fatal(err)
	}
	metadata.(Object)["expired"] = true
	changed.proof["candidate_artifact_file"] = nativeJSONFileProof(t, metadata)
	if _, err := ValidatePublicationProof(changed.proof, target, changed.context, true); err == nil {
		t.Fatal("expired candidate artifact metadata was accepted")
	}

	changed = clonePublicationFixture(proof, context)
	jobs, err := LoadFileProof(changed.proof["candidate_jobs_file"], "candidate jobs")
	if err != nil {
		t.Fatal(err)
	}
	jobs.([]any)[0].(Object)["jobs"].([]any)[0].(Object)["conclusion"] = "failure"
	changed.proof["candidate_jobs_file"] = nativeJSONFileProof(t, jobs)
	if _, err := ValidatePublicationProof(changed.proof, target, changed.context, true); err == nil {
		t.Fatal("failed candidate verification job was accepted")
	}

	changed = clonePublicationFixture(proof, context)
	candidateBytes, err := decodePublicationRawProof(changed.proof["candidate_file"], "candidate")
	if err != nil {
		t.Fatal(err)
	}
	patchBytes, err := decodePublicationRawProof(changed.proof["patch_file"], "patch")
	if err != nil {
		t.Fatal(err)
	}
	resultBytes, err := decodePublicationRawProof(changed.proof["result_file"], "result")
	if err != nil {
		t.Fatal(err)
	}
	eventBytes, err := decodePublicationRawProof(changed.proof["event_file"], "event")
	if err != nil {
		t.Fatal(err)
	}
	archive := publicationCandidateArchive(t, candidateBytes, patchBytes, resultBytes, eventBytes, true)
	changed.proof["candidate_archive_file"] = MakeFileProof(archive)
	metadata, err = LoadFileProof(changed.proof["candidate_artifact_file"], "candidate artifact")
	if err != nil {
		t.Fatal(err)
	}
	metadata.(Object)["digest"] = "sha256:" + SHA256(archive)
	changed.proof["candidate_artifact_file"] = nativeJSONFileProof(t, metadata)
	if _, err := ValidatePublicationProof(changed.proof, target, changed.context, true); err == nil {
		t.Fatal("candidate ZIP with an unreviewed extra file was accepted")
	}

	changed = clonePublicationFixture(proof, context)
	metadata, err = LoadFileProof(changed.proof["candidate_artifact_file"], "candidate artifact")
	if err != nil {
		t.Fatal(err)
	}
	metadata.(Object)["digest"] = "sha256:" + strings.Repeat("0", 64)
	changed.proof["candidate_artifact_file"] = nativeJSONFileProof(t, metadata)
	if _, err := ValidatePublicationProof(changed.proof, target, changed.context, true); err == nil {
		t.Fatal("candidate archive whose raw bytes differ from the provider metadata digest was accepted")
	}
}

func TestPublicationProofSeparatesRawBytesFromSemanticIntentDigest(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "completed")
	intentValue, err := LoadFileProof(proof["intent_file"], "intent")
	if err != nil {
		t.Fatal(err)
	}
	pretty, err := json.MarshalIndent(intentValue, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	proof["intent_file"] = MakeFileProof(append(pretty, '\n'))
	if err := refreshPublicationQualification(proof, target, context); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePublicationProof(proof, target, context, true); err != nil {
		t.Fatalf("semantically identical intent with distinct retained bytes rejected: %v", err)
	}
}

func TestPublicationProofValidatesCreatedPRAcknowledgement(t *testing.T) {
	proof, target, context := makeCreatedPRFixture(t)
	if _, err := ValidatePublicationProof(proof, target, context, true); err != nil {
		t.Fatalf("valid created PR acknowledgement rejected: %v", err)
	}

	changed := clonePublicationFixture(proof, context)
	prValue, err := LoadFileProof(changed.proof["pr_ack_file"], "PR ACK")
	if err != nil {
		t.Fatal(err)
	}
	prAck := prValue.(Object)
	prAck["base_sha"] = strings.Repeat("f", 40)
	changed.proof["pr_ack_file"] = nativeJSONFileProof(t, prAck)
	changed.context["publication"].(Object)["pr_ack"] = prAck
	if _, err := ValidatePublicationProof(changed.proof, changed.target, changed.context, true); err == nil {
		t.Fatal("created PR acknowledgement with a different base revision was accepted")
	}
}

func TestPublicationProofUsesGenericPullRequestActionKind(t *testing.T) {
	proof, target, context := makeCreatedPRFixture(t)
	publication := context["publication"].(Object)
	publication["stage"] = "pr-pending"
	publication["pr_ack"], publication["verify_ack"] = nil, nil
	proof["pr_ack_file"], proof["verify_ack_file"] = nil, nil
	context["phase"] = "dispatching"
	context["dispatch_steps"] = []any{"pull-request-create"}
	if _, err := ValidatePublicationProof(proof, target, context, false); err != nil {
		t.Fatalf("valid pending pull request creation proof rejected: %v", err)
	}
	context["dispatch_steps"] = []any{"create-dependency-remediation-pull-request"}
	if _, err := ValidatePublicationProof(proof, target, context, false); err == nil {
		t.Fatal("legacy descriptive publication action label was accepted")
	}
}

func TestPublicationProofUsesGenericBranchPushActionKind(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "push-pending")
	if _, err := ValidatePublicationProof(proof, target, context, false); err != nil {
		t.Fatalf("valid pending branch push proof rejected: %v", err)
	}
	context["dispatch_steps"] = []any{"push-dependency-remediation-branch"}
	if _, err := ValidatePublicationProof(proof, target, context, false); err == nil {
		t.Fatal("legacy descriptive branch action label was accepted")
	}
}

func TestPublicationProofEnforcesModeSpecificSourceBranchLease(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "completed")
	rewritePublicationCandidateAndIntent(t, proof, context, func(intent Object) {
		intent["source_ref"] = "refs/heads/another-branch"
	}, func(candidate Object) {
		candidate["source_ref"] = "refs/heads/another-branch"
	})
	if _, err := ValidatePublicationProof(proof, target, context, true); err == nil {
		t.Fatal("existing-PR update with a source branch outside the leased PR head was accepted")
	}

	proof, target, context = makeCreatedPRFixture(t)
	rewritePublicationCandidateAndIntent(t, proof, context, func(intent Object) {
		intent["expected_old_sha"] = strings.Repeat("f", 40)
	}, nil)
	if _, err := ValidatePublicationProof(proof, target, context, true); err == nil {
		t.Fatal("new-PR publication with a ref lease was accepted")
	}

	proof, target, context = makeCreatedPRFixture(t)
	rewritePublicationCandidateAndIntent(t, proof, context, func(intent Object) {
		intent["source_sha"] = strings.Repeat("8", 40)
	}, func(candidate Object) {
		candidate["source_sha"] = strings.Repeat("8", 40)
	})
	if _, err := ValidatePublicationProof(proof, target, context, true); err == nil {
		t.Fatal("new-PR publication whose source revision differs from its base was accepted")
	}
}

func TestPublicationProofRequiresEveryDurableWriteACKAndRejectsBooleanCoercion(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "completed")
	proof["push_ack_file"] = nil
	context["publication"].(Object)["push_ack"] = nil
	if _, err := ValidatePublicationProof(proof, target, context, true); err == nil {
		t.Fatal("completed publication without a positive push ACK was accepted")
	}

	proof, target, context = makeCreatedPRFixture(t)
	proof["pr_ack_file"] = nil
	context["publication"].(Object)["pr_ack"] = nil
	if _, err := ValidatePublicationProof(proof, target, context, true); err == nil {
		t.Fatal("created PR without a positive creation ACK was accepted")
	}

	proof, target, context = makePublicationFixture(t, "completed")
	pushValue, err := LoadFileProof(proof["push_ack_file"], "push ACK")
	if err != nil {
		t.Fatal(err)
	}
	push := pushValue.(Object)
	push["positive_push_ack"] = int64(1)
	proof["push_ack_file"] = nativeJSONFileProof(t, push)
	context["publication"].(Object)["push_ack"] = push
	if _, err := ValidatePublicationProof(proof, target, context, true); err == nil {
		t.Fatal("numeric one was coerced to a positive boolean push ACK")
	}
}

func TestBuildPublicationProofUsesConfinedRawFiles(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "completed")
	root := t.TempDir()
	writePublicationFixture(t, root, proof)
	built, err := BuildPublicationProof(root, proof["origin_run"].(Object), context, target, true)
	if err != nil {
		t.Fatalf("valid package could not produce publication proof: %v", err)
	}
	if !Equal(built["intent_file"], proof["intent_file"]) || !Equal(built["patch_file"], proof["patch_file"]) {
		t.Fatal("builder changed exact retained publication bytes")
	}

	preparedProof, preparedTarget, preparedContext := makePublicationFixture(t, "pr-verify-pending")
	preparedRoot := t.TempDir()
	writePublicationFixture(t, preparedRoot, preparedProof)
	prepared, err := BuildPublicationProof(preparedRoot, preparedProof["origin_run"].(Object), preparedContext, preparedTarget, false)
	if err != nil {
		t.Fatalf("builder rejected missing optional ACK files before observer verification: %v", err)
	}
	if prepared["verify_ack_file"] != nil || prepared["pr_ack_file"] != nil {
		t.Fatal("builder invented publication acknowledgements for absent files")
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "patch.diff"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, publicationPaths["patch_file"])); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "patch.diff"), filepath.Join(root, publicationPaths["patch_file"])); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPublicationProof(root, proof["origin_run"].(Object), context, target, true); err == nil {
		t.Fatal("symbolic publication source file was followed")
	}

	if err := os.Remove(filepath.Join(root, publicationPaths["patch_file"])); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, publicationPaths["patch_file"]), make([]byte, MaxFileBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPublicationProof(root, proof["origin_run"].(Object), context, target, true); err == nil {
		t.Fatal("oversized publication file was read")
	}
}

func TestRecoveredPublicationProofPreservesWriteReceiptsAndAddsObservation(t *testing.T) {
	proof, target := makeRecoveredPublicationFixture(t)
	if err := ValidateRecoveredPublicationProof(proof, target); err != nil {
		t.Fatalf("valid read-only publication recovery was rejected: %v", err)
	}

	withPolicyFiles := cloneRecoveredPublicationFixture(proof)
	policySidecar := MakeFileProof([]byte(`{"approval":"reviewed"}`))
	withPolicyFiles["source"].(Object)["policy_files"] = Object{"reviews/publish.json": policySidecar}
	withPolicyFiles["observer"].(Object)["policy_files"] = Object{"reviews/publish.json": policySidecar}
	if err := ValidateRecoveredPublicationProof(withPolicyFiles, target); err != nil {
		t.Fatalf("matching policy sidecars were rejected: %v", err)
	}
	withPolicyFiles["observer"].(Object)["policy_files"].(Object)["reviews/publish.json"] = MakeFileProof([]byte(`{"approval":"changed"}`))
	if err := ValidateRecoveredPublicationProof(withPolicyFiles, target); err == nil {
		t.Fatal("publication observer that changed an approval sidecar was accepted")
	}

	changed := cloneRecoveredPublicationFixture(proof)
	source := changed["source"].(Object)
	observer := changed["observer"].(Object)
	sourcePublication := source["publication"].(Object)
	observerPublication := observer["publication"].(Object)
	sourcePublication["push_ack_file"] = MakeFileProof([]byte(`{"schema_version":1}`))
	observerPublication["push_ack_file"] = MakeFileProof([]byte(`{"schema_version":1}`))
	if err := ValidateRecoveredPublicationProof(changed, target); err == nil {
		t.Fatal("recovery that replaced the durable positive push ACK was accepted")
	}
}

type publicationFixture struct {
	proof   Object
	target  Object
	context Object
}

func makePublicationFixture(t *testing.T, stage string) (Object, Object, Object) {
	t.Helper()
	target := Object{"workflow_file": "automation.yml", "repository": "sample/repo", "server_url": "https://github.com", "workflow_id": int64(42), "recovery_key": "workflow-history-v2"}
	origin := Object{
		"id": int64(100), "created_at": "2026-10-04T12:00:00Z", "display_title": "publication run",
		"event": "schedule", "workflow_id": int64(42), "head_branch": "main", "head_sha": strings.Repeat("e", 40),
	}
	patch := []byte("diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n")
	result := Object{"status": "patched", "changed_files": []any{"README.md"}, "verification": []any{"git diff --check"}}
	resultBytes, err := Canonical(result)
	if err != nil {
		t.Fatal(err)
	}
	event := []byte(`{"event":"schedule"}`)
	sourceSHA, baseSHA, newSHA := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("d", 40)
	controlSHA := strings.Repeat("9", 40)
	patchProof, resultProof, eventProof := MakeFileProof(patch), MakeFileProof(resultBytes), MakeFileProof(event)
	prTarget := Object{
		"number": int64(12), "url": "https://github.com/sample/repo/pull/12", "head_repository": "sample/repo",
		"head_ref": "automation/update", "head_sha": sourceSHA, "base_repository": "sample/repo",
		"base_ref": "main", "author_login": "reviewer", "title": "Update reviewed files",
		"body": "Validated changes", "draft": false,
	}
	intent := Object{
		"schema_version": 2, "repository": "sample/repo", "repository_node_id": "R_sample_repo",
		"workflow_file": target["workflow_file"], "recovery_key": "publication-run-100", "run_name": origin["display_title"],
		"origin_run_id": int64(100), "origin_run_attempt": int64(1), "workflow_sha": controlSHA, "event_name": "schedule",
		"trigger_event_sha256": eventProof["sha256"], "source_repository": "sample/repo", "source_ref": "refs/heads/automation/update",
		"source_sha": sourceSHA, "target_pr": prTarget, "mode": "update-existing-pr", "base_ref": "main", "base_sha": baseSHA,
		"head_ref": "refs/heads/automation/update", "expected_old_sha": sourceSHA, "new_sha": newSHA,
		"publisher_login": "publisher", "title": "Update reviewed files", "body": "Validated changes", "draft": false,
		"nonce": "123e4567-e89b-42d3-a456-426614174000", "patch_sha256": patchProof["sha256"],
		"result_sha256": resultProof["sha256"], "files": []any{"README.md"}, "created_at": "2026-10-04T12:01:00Z",
	}
	candidate := Object{
		"schema_version": 1, "repository": "sample/repo", "server_url": target["server_url"],
		"workflow_file": target["workflow_file"], "origin_run_id": int64(100), "origin_run_attempt": int64(1),
		"workflow_sha": controlSHA, "event_name": "schedule", "trigger_event_sha256": eventProof["sha256"],
		"source_repository": intent["source_repository"], "source_ref": intent["source_ref"], "source_sha": intent["source_sha"],
		"base_ref": intent["base_ref"], "base_sha": intent["base_sha"], "candidate_tree_sha": strings.Repeat("f", 40),
		"patch_sha256": patchProof["sha256"], "result_sha256": resultProof["sha256"],
		"upstream_artifacts": Object{
			"capture":  Object{"id": int64(301), "name": "capture-100", "digest": "sha256:" + strings.Repeat("1", 64)},
			"proposal": Object{"id": int64(302), "name": "proposal-100", "digest": "sha256:" + strings.Repeat("2", 64)},
		},
		"verification_job_name": "Verify publication candidate",
	}
	candidateBytes, err := Canonical(candidate)
	if err != nil {
		t.Fatal(err)
	}
	intent["candidate_sha256"] = SHA256(candidateBytes)
	candidateProof := MakeFileProof(candidateBytes)
	candidateArchive := publicationCandidateArchive(t, candidateBytes, patch, resultBytes, event)
	candidateArchiveProof := MakeFileProof(candidateArchive)
	candidateArtifact := Object{
		"id": int64(300), "name": "gh-steward-candidate-100-1", "digest": "sha256:" + SHA256(candidateArchive),
		"expired": false, "url": "https://api.github.com/repos/sample/repo/actions/artifacts/300",
		"workflow_run": Object{"id": int64(100), "head_sha": origin["head_sha"], "head_branch": "main", "event": "schedule"},
	}
	candidateArtifactProof := nativeJSONFileProof(t, candidateArtifact)
	candidateJobs := []any{Object{
		"total_count": int64(2), "jobs": []any{
			Object{
				"id": int64(401), "run_id": int64(100), "run_attempt": int64(1), "name": "Verify publication candidate",
				"status": "completed", "conclusion": "success",
			},
			Object{"id": int64(402), "name": "Independent documentation check", "status": "in_progress"},
		},
	}}
	candidateJobsProof := nativeJSONFileProof(t, candidateJobs)
	intentProof := nativeJSONFileProof(t, intent)
	pushAck := Object{
		"schema_version": 1, "nonce": intent["nonce"], "repository": intent["repository"],
		"repository_node_id": intent["repository_node_id"], "ref": intent["head_ref"],
		"expected_old_sha": intent["expected_old_sha"], "new_sha": intent["new_sha"], "positive_push_ack": true,
	}
	verifyAck := Object{
		"schema_version": 1, "nonce": intent["nonce"], "repository": intent["repository"],
		"repository_node_id": intent["repository_node_id"], "url": prTarget["url"], "number": prTarget["number"],
		"head_ref": "automation/update", "head_sha": newSHA, "base_ref": "main", "author_login": prTarget["author_login"],
		"title": prTarget["title"], "body": prTarget["body"], "draft": false, "state": "OPEN", "verified": true,
	}
	acks := map[string]any{"push": pushAck, "pr": nil, "verify": verifyAck}
	ackProofs := make(map[string]any, 3)
	for name, value := range acks {
		if value == nil {
			ackProofs[name+"_ack_file"] = nil
		} else {
			ackProofs[name+"_ack_file"] = nativeJSONFileProof(t, value)
		}
	}
	publication := Object{
		"intent_sha256": intentProof["sha256"], "origin_run_id": int64(100), "origin_run_attempt": int64(1),
		"stage": stage, "push_ack": pushAck, "pr_ack": nil, "verify_ack": verifyAck,
	}
	if stage == "pr-verify-pending" {
		publication["verify_ack"] = nil
		ackProofs["verify_ack_file"] = nil
	}
	dispatchSteps := []any{}
	if stage == "push-pending" {
		dispatchSteps = []any{"branch-push"}
		publication["push_ack"] = nil
		publication["pr_ack"] = nil
		publication["verify_ack"] = nil
		ackProofs["push_ack_file"] = nil
		ackProofs["pr_ack_file"] = nil
		ackProofs["verify_ack_file"] = nil
	}
	context := Object{
		"recovery_key": intent["recovery_key"], "phase": "completed", "dispatch_steps": dispatchSteps,
		"plans": []any{}, "publication": publication,
	}
	if stage == "pr-verify-pending" {
		context["phase"] = "prepared"
	}
	if stage == "push-pending" {
		context["phase"] = "dispatching"
	}
	proof := Object{
		"schema_version": 2,
		"origin_run":     origin, "intent_file": intentProof, "patch_file": patchProof,
		"result_file": resultProof, "event_file": eventProof,
		"push_ack_file": ackProofs["push_ack_file"], "pr_ack_file": ackProofs["pr_ack_file"],
		"verify_ack_file": ackProofs["verify_ack_file"],
		"candidate_file":  candidateProof, "candidate_artifact_file": candidateArtifactProof,
		"candidate_archive_file": candidateArchiveProof, "candidate_jobs_file": candidateJobsProof,
		"qualification_file": nil,
	}
	context["workflow_file"], context["repository"] = target["workflow_file"], target["repository"]
	context["workflow_run_id"], context["workflow_run_attempt"] = int64(100), int64(1)
	context["run_name"], context["attempt_target"] = origin["display_title"], Object{"publication_origin": int64(100)}
	if stage != "push-pending" {
		if err := refreshPublicationQualification(proof, target, context); err != nil {
			t.Fatal(err)
		}
	}
	return proof, target, context
}

func makeRecoveredPublicationFixture(t *testing.T) (Object, Object) {
	t.Helper()
	sourceProof, target, sourceContext := makePublicationFixture(t, "pr-verify-pending")
	observerProof, _, observerContext := makePublicationFixture(t, "completed")
	observerContext["recovery_key"] = sourceContext["recovery_key"]
	observerContext["schema_version"] = 1
	observerContext["phase"] = "completed"
	observerContext["workflow_file"] = target["workflow_file"]
	observerContext["repository"] = target["repository"]
	observerContext["workflow_run_id"] = int64(201)
	observerContext["workflow_run_attempt"] = int64(1)
	observerContext["run_name"] = "observer run"
	observerContext["attempt_target"] = Object{"scope": "same-publication"}
	observerContext["recovered_from_run_id"] = int64(100)
	observerContext["recovered_from_attempt"] = int64(1)
	sourceContext["workflow_file"] = target["workflow_file"]
	sourceContext["schema_version"] = 1
	sourceContext["repository"] = target["repository"]
	sourceContext["workflow_run_id"] = int64(100)
	sourceContext["workflow_run_attempt"] = int64(1)
	sourceContext["run_name"] = "publication run"
	sourceContext["attempt_target"] = Object{"scope": "same-publication"}
	sourceContext["phase"] = "prepared"
	sourceRun := cloneNativeObject(sourceProof["origin_run"].(Object))
	sourceRun["created_at"] = "2026-10-04T12:05:00Z"
	observerRun := cloneNativeObject(sourceRun)
	observerRun["id"], observerRun["created_at"], observerRun["display_title"] = int64(201), "2026-10-04T12:06:00Z", "observer run"
	if err := refreshPublicationQualification(sourceProof, target, sourceContext); err != nil {
		t.Fatal(err)
	}
	if err := refreshPublicationQualification(observerProof, target, observerContext); err != nil {
		t.Fatal(err)
	}
	artifactDigest := "sha256:" + strings.Repeat("f", 64)
	sourceAttempt := publicationAttemptProof(t, sourceProof, sourceContext, sourceRun, 1, 300, target, artifactDigest)
	observerAttempt := publicationAttemptProof(t, observerProof, observerContext, observerRun, 1, 301, target, artifactDigest)
	return Object{"source": sourceAttempt, "observer": observerAttempt}, target
}

func makeCreatedPRFixture(t *testing.T) (Object, Object, Object) {
	t.Helper()
	proof, target, context := makePublicationFixture(t, "completed")
	rewritePublicationCandidateAndIntent(t, proof, context, func(intent Object) {
		intent["mode"], intent["target_pr"] = "create-pr", nil
		intent["expected_old_sha"] = nil
		intent["source_sha"] = intent["base_sha"]
	}, func(candidate Object) {
		candidate["source_sha"] = candidate["base_sha"]
	})
	intentValue, err := LoadFileProof(proof["intent_file"], "publication intent")
	if err != nil {
		t.Fatal(err)
	}
	intent := intentValue.(Object)
	publication := context["publication"].(Object)
	intentProof := proof["intent_file"].(Object)
	publication["intent_sha256"] = intentProof["sha256"]
	pushValue, err := LoadFileProof(proof["push_ack_file"], "push acknowledgement")
	if err != nil {
		t.Fatal(err)
	}
	pushAck := pushValue.(Object)
	pushAck["expected_old_sha"] = nil
	proof["push_ack_file"] = nativeJSONFileProof(t, pushAck)
	publication["push_ack"] = pushAck
	prAck := Object{
		"schema_version": 1, "nonce": intent["nonce"], "repository": intent["repository"],
		"repository_node_id": intent["repository_node_id"], "head_ref": "automation/update", "head_sha": intent["new_sha"],
		"base_ref": intent["base_ref"], "base_sha": intent["base_sha"], "publisher_login": intent["publisher_login"],
		"title": intent["title"], "body": intent["body"], "draft": intent["draft"],
		"number": int64(13), "url": "https://github.com/sample/repo/pull/13",
	}
	verifyAck := Object{
		"schema_version": 1, "nonce": intent["nonce"], "repository": intent["repository"],
		"repository_node_id": intent["repository_node_id"], "url": prAck["url"], "number": prAck["number"],
		"head_ref": prAck["head_ref"], "head_sha": prAck["head_sha"], "base_ref": prAck["base_ref"],
		"author_login": intent["publisher_login"], "title": intent["title"], "body": intent["body"],
		"draft": intent["draft"], "state": "OPEN", "verified": true,
	}
	proof["pr_ack_file"] = nativeJSONFileProof(t, prAck)
	proof["verify_ack_file"] = nativeJSONFileProof(t, verifyAck)
	publication["pr_ack"], publication["verify_ack"] = prAck, verifyAck
	return proof, target, context
}

func rewritePublicationCandidateAndIntent(
	t *testing.T,
	proof, context Object,
	mutateIntent, mutateCandidate func(Object),
) {
	t.Helper()
	intentValue, err := LoadFileProof(proof["intent_file"], "publication intent")
	if err != nil {
		t.Fatal(err)
	}
	intent := intentValue.(Object)
	candidateValue, err := LoadFileProof(proof["candidate_file"], "publication candidate")
	if err != nil {
		t.Fatal(err)
	}
	candidate := candidateValue.(Object)
	if mutateIntent != nil {
		mutateIntent(intent)
	}
	if mutateCandidate != nil {
		mutateCandidate(candidate)
	}
	candidateBytes, err := Canonical(candidate)
	if err != nil {
		t.Fatal(err)
	}
	intent["candidate_sha256"] = SHA256(candidateBytes)
	proof["candidate_file"] = MakeFileProof(candidateBytes)

	patchBytes, err := decodePublicationRawProof(proof["patch_file"], "publication patch")
	if err != nil {
		t.Fatal(err)
	}
	resultBytes, err := decodePublicationRawProof(proof["result_file"], "publication result")
	if err != nil {
		t.Fatal(err)
	}
	eventBytes, err := decodePublicationRawProof(proof["event_file"], "publication event")
	if err != nil {
		t.Fatal(err)
	}
	archive := publicationCandidateArchive(t, candidateBytes, patchBytes, resultBytes, eventBytes)
	proof["candidate_archive_file"] = MakeFileProof(archive)
	metadataValue, err := LoadFileProof(proof["candidate_artifact_file"], "candidate artifact metadata")
	if err != nil {
		t.Fatal(err)
	}
	metadata := metadataValue.(Object)
	metadata["digest"] = "sha256:" + SHA256(archive)
	proof["candidate_artifact_file"] = nativeJSONFileProof(t, metadata)

	proof["intent_file"] = nativeJSONFileProof(t, intent)
	context["publication"].(Object)["intent_sha256"] = proof["intent_file"].(Object)["sha256"]
	if err := refreshPublicationQualification(proof, Object{"workflow_file": intent["workflow_file"], "repository": intent["repository"], "server_url": "https://github.com", "workflow_id": int64(42), "recovery_key": "workflow-history-v2"}, context); err != nil {
		t.Fatal(err)
	}
}

func publicationAttemptProof(t *testing.T, publication Object, context Object, run Object, attempt, artifactID int64, target Object, digest string) Object {
	t.Helper()
	proofCopy := cloneNativeObject(publication)
	ctxCopy := cloneNativeObject(context)
	name := RecoveryArtifactName(target, run["id"].(int64), attempt)
	return Object{
		"run_id": run["id"], "attempt": attempt, "run": run,
		"artifact":     Object{"name": name, "id": artifactID, "digest": digest},
		"context_file": nativeJSONFileProof(t, ctxCopy), "plans": []any{}, "publication": proofCopy, "policy_files": Object{},
	}
}

func publicationCandidateArchive(t *testing.T, candidate, patch, result, event []byte, includeExtra ...bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	files := []struct {
		name string
		data []byte
	}{
		{"publication/candidate.json", candidate},
		{"publication/patch.diff", patch},
		{"publication/result.json", result},
		{"events/trigger-event.json", event},
	}
	if len(includeExtra) > 0 && includeExtra[0] {
		files = append(files, struct {
			name string
			data []byte
		}{"unexpected.txt", []byte("unreviewed")})
	}
	for _, file := range files {
		entry, err := writer.Create(file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), buffer.Bytes()...)
}

func writePublicationFixture(t *testing.T, root string, proof Object) {
	t.Helper()
	for field, relative := range publicationPaths {
		fileProof := proof[field]
		if fileProof == nil {
			continue
		}
		data, err := decodePublicationRawProof(fileProof, field)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func refreshPublicationQualification(proof, target, context Object) error {
	proof["qualification_file"] = nil
	intentValue, err := LoadFileProof(proof["intent_file"], "publication intent")
	if err != nil {
		return err
	}
	intent, err := object(intentValue, "publication intent")
	if err != nil {
		return err
	}
	qualification, err := makePublicationQualification(proof, target, context, intent["workflow_sha"].(string))
	if err != nil {
		return err
	}
	data, err := Canonical(qualification)
	if err != nil {
		return err
	}
	proof["qualification_file"] = MakeFileProof(data)
	return nil
}

func clonePublicationFixture(proof, context Object) publicationFixture {
	return publicationFixture{proof: cloneNativeObject(proof), target: Object{"workflow_file": "automation.yml", "repository": "sample/repo", "server_url": "https://github.com", "workflow_id": int64(42), "recovery_key": "workflow-history-v2"}, context: cloneNativeObject(context)}
}

func cloneRecoveredPublicationFixture(value Object) Object {
	result := cloneNativeObject(value)
	for _, name := range []string{"source", "observer"} {
		attempt := cloneNativeObject(value[name].(Object))
		attempt["publication"] = cloneNativeObject(value[name].(Object)["publication"].(Object))
		result[name] = attempt
	}
	return result
}
