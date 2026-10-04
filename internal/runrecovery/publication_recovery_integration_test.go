package runrecovery

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recoveryPackageZIP(t *testing.T, root string) []byte {
	t.Helper()
	files, err := retainedPackageFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]recoveryZipEntry, 0, len(files))
	for _, relative := range files {
		raw, err := ReadPackageFile(root, relative)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, recoveryZipEntry{name: relative, data: raw})
	}
	return recoveryZip(t, entries...)
}

func TestRecoveredPublicationPreservesQualificationAcrossControlSourceAndFreshJobHandoff(t *testing.T) {
	proof, target, sourceContext := makePublicationFixture(t, "pr-verify-pending")
	repo := policyTestRepository(t)
	repo.Owner, repo.Name, repo.URL = "sample", "repo", "https://github.com/sample/repo"
	e, err := NewEngine(publicationAcquisitionPolicy(), repo)
	if err != nil {
		t.Fatal(err)
	}
	sourceTemp := t.TempDir()
	sourceRoot := filepath.Join(sourceTemp, "original")
	if err := os.Mkdir(sourceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	writePublicationFixture(t, sourceRoot, proof)
	sourceContext["schema_version"] = 1
	sourceContext["trusted_source_sha"] = strings.Repeat("9", 40)
	if err := persistPackageJSON(sourceRoot, "run-context.json", sourceContext); err != nil {
		t.Fatal(err)
	}
	chain, err := EmptyChain(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPackageJSON(sourceRoot, "settlement-chain.json", chain); err != nil {
		t.Fatal(err)
	}
	sourceQualification, err := ReadPackageFile(sourceRoot, publicationPaths["qualification_file"])
	if err != nil {
		t.Fatal(err)
	}
	sourcePayload := recoveryPackageZIP(t, sourceRoot)
	sourceRun := cloneNativeObject(proof["origin_run"].(Object))
	sourceRun["run_attempt"], sourceRun["status"], sourceRun["conclusion"] = int64(1), "completed", "failure"
	currentRun := cloneNativeObject(sourceRun)
	currentRun["id"], currentRun["created_at"], currentRun["display_title"] = int64(101), "2026-10-04T12:10:00Z", "Publication observer"
	currentRun["head_sha"], currentRun["status"], currentRun["conclusion"] = strings.Repeat("8", 40), "in_progress", nil
	sourceMetadata := Object{"id": int64(500), "name": RecoveryArtifactName(target, 100, 1), "expired": false,
		"size_in_bytes": len(sourcePayload), "digest": "sha256:" + SHA256(sourcePayload),
		"workflow_run": Object{"id": int64(100), "head_sha": sourceRun["head_sha"]}}
	r := publicationLiveReader(t, proof)
	r.pages["repos/sample/repo/actions/workflows/automation.yml/runs?per_page=100"] = []any{Object{"total_count": 2, "workflow_runs": []any{currentRun, sourceRun}}}
	r.pages["repos/sample/repo/actions/artifacts?per_page=100"] = []any{Object{"total_count": 1, "artifacts": []any{sourceMetadata}}}
	r.reads["repos/sample/repo/actions/runs/100/attempts/1"] = sourceRun
	r.reads["repos/sample/repo/actions/artifacts/500"] = sourceMetadata
	r.archives[500] = sourcePayload
	prepareTemp := t.TempDir()
	inv := Invocation{Workflow: "automation.yml", RunID: 101, Attempt: 1, RunName: "Publication observer", RecoveryKey: sourceContext["recovery_key"].(string), PackageRoot: filepath.Join(prepareTemp, "prepared"), RunnerTemp: prepareTemp}
	result, err := e.Recover(context.Background(), r, inv)
	if err != nil || result["outcome"] != "resumed" {
		t.Fatalf("qualified positive-write publication did not restore: %v %v", result, err)
	}
	sourceBytes, err := ReadPackageFile(inv.PackageRoot, "recovery-source.json")
	if err != nil {
		t.Fatal(err)
	}
	handoffPayload := recoveryPackageZIP(t, inv.PackageRoot)
	handoffMetadata := Object{"id": int64(501), "name": RecoveryArtifactName(target, 101, 1) + "-handoff-00", "expired": false,
		"digest": "sha256:" + SHA256(handoffPayload), "workflow_run": Object{"id": int64(101), "head_sha": currentRun["head_sha"]}}
	r.reads["repos/sample/repo/actions/artifacts/501"], r.archives[501] = handoffMetadata, handoffPayload
	applyTemp := t.TempDir()
	inv.PackageRoot, inv.RunnerTemp = filepath.Join(applyTemp, "publisher"), applyTemp
	if _, err := e.AcquireHandoffFor(context.Background(), r, inv, 501, handoffMetadata["digest"].(string), "transport"); err != nil {
		t.Fatalf("separate publisher job could not acquire original qualified packet: %v", err)
	}
	if _, err := e.VerifyPublication(context.Background(), r, inv, strings.Repeat("8", 40)); err != nil {
		t.Fatalf("new control source could not revalidate original read-only publication: %v", err)
	}
	currentQualification, err := ReadPackageFile(inv.PackageRoot, publicationPaths["qualification_file"])
	if err != nil || !bytes.Equal(currentQualification, sourceQualification) {
		t.Fatal("observer rewrote the original qualification", err)
	}
	currentSource, err := ReadPackageFile(inv.PackageRoot, "recovery-source.json")
	if err != nil || !bytes.Equal(currentSource, sourceBytes) {
		t.Fatal("observer rewrote source proof", err)
	}
	// The consumer adds only a read-only verified PR observation, then finalizes
	// the exact uploaded normal artifact. There is no provider write in this test.
	completedProof, _, _ := makePublicationFixture(t, "completed")
	verifyBytes, err := LoadRawFileProof(completedProof["verify_ack_file"], "read-only PR verification")
	if err != nil {
		t.Fatal(err)
	}
	verifyValue, err := DecodeValue(verifyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPackageFile(inv.PackageRoot, publicationPaths["verify_ack_file"], verifyBytes); err != nil {
		t.Fatal(err)
	}
	contextValue, err := LoadJSON(filepath.Join(inv.PackageRoot, "run-context.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := contextValue.(Object)
	ctx["phase"] = "completed"
	ctx["publication"].(Object)["stage"], ctx["publication"].(Object)["verify_ack"] = "completed", verifyValue
	if err := persistPackageJSON(inv.PackageRoot, "run-context.json", ctx); err != nil {
		t.Fatal(err)
	}
	payload := recoveryPackageZIP(t, inv.PackageRoot)
	metadata := Object{"id": int64(502), "name": RecoveryArtifactName(target, 101, 1), "expired": false,
		"digest": "sha256:" + SHA256(payload), "workflow_run": Object{"id": int64(101), "head_sha": currentRun["head_sha"]}}
	r.reads["repos/sample/repo/actions/artifacts/502"], r.archives[502] = metadata, payload
	options := FinalizeOptions{Invocation: inv, ArtifactID: 502, ArtifactDigest: metadata["digest"].(string), Checkpoint: filepath.Join(applyTemp, "checkpoint"), WorkflowSHA: strings.Repeat("8", 40)}
	result, err = e.Finalize(context.Background(), r, options)
	if err != nil || result["outcome"] != "checkpoint" {
		t.Fatalf("source-bound publication observer did not finalize: %v %v", result, err)
	}
	checkpointValue, err := LoadJSON(result["checkpoint_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	settlements := checkpointValue.(Object)["settlements"].([]any)
	if len(settlements) != 2 || !exactInt(settlements[0].(Object)["run_id"], 100) || !exactInt(settlements[1].(Object)["run_id"], 101) {
		t.Fatalf("publication checkpoint did not retain original/observer order: %v", settlements)
	}
	for _, call := range r.calls {
		if !strings.HasPrefix(call, "GET ") && !strings.HasPrefix(call, "PAGES ") && !strings.HasPrefix(call, "ZIP ") {
			t.Fatal(fmt.Sprintf("publication recovery dispatched an unexpected operation: %s", call))
		}
	}
}
