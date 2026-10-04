package runrecovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func publicationLiveReader(t *testing.T, proof Object) *recoveryReaderFixture {
	t.Helper()
	value, err := LoadFileProof(proof["candidate_artifact_file"], "candidate artifact")
	if err != nil {
		t.Fatal(err)
	}
	metadata := value.(Object)
	value, err = LoadFileProof(proof["candidate_jobs_file"], "candidate jobs")
	if err != nil {
		t.Fatal(err)
	}
	jobs := value.([]any)
	archive, err := LoadRawFileProof(proof["candidate_archive_file"], "candidate archive")
	if err != nil {
		t.Fatal(err)
	}
	value, err = LoadFileProof(proof["candidate_file"], "candidate")
	if err != nil {
		t.Fatal(err)
	}
	candidate := value.(Object)
	runID, _ := positiveInteger(candidate["origin_run_id"], "origin run")
	attempt, _ := positiveInteger(candidate["origin_run_attempt"], "origin attempt")
	repository := candidate["repository"].(string)
	reader := &recoveryReaderFixture{reads: map[string]Object{fmt.Sprintf("repos/%s/actions/artifacts/300", repository): metadata}, pages: map[string][]any{fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", repository, runID, attempt): jobs}, archives: map[int64][]byte{300: archive}}
	for _, raw := range candidate["upstream_artifacts"].(Object) {
		ref := raw.(Object)
		id, _ := positiveInteger(ref["id"], "upstream artifact ID")
		reader.reads[fmt.Sprintf("repos/%s/actions/artifacts/%d", repository, id)] = Object{"id": id, "name": ref["name"], "digest": ref["digest"], "expired": false, "workflow_run": Object{"id": runID, "head_sha": proof["origin_run"].(Object)["head_sha"]}}
	}
	return reader
}

func TestVerifyPublicationCreatesImmutableQualificationAfterLiveGate(t *testing.T) {
	proof, target, runContext := makePublicationFixture(t, "push-pending")
	temp := t.TempDir()
	root := filepath.Join(temp, "package")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	writePublicationFixture(t, root, proof)
	runContext["schema_version"] = int64(1)
	if err := persistPackageJSON(root, "run-context.json", runContext); err != nil {
		t.Fatal(err)
	}
	repository := contract.Repository{Host: "github.com", Owner: "sample", Name: "repo", URL: "https://github.com/sample/repo"}
	policy := Object{"schema_version": 1, "workflows": Object{"automation.yml": Object{
		"mutator_step_alternatives": []any{[]any{"Publish candidate"}}, "reviewed_source_shas": []any{},
		"allow_publication": true, "plans": Object{},
	}}}
	engine, err := NewEngine(policy, repository)
	if err != nil {
		t.Fatal(err)
	}
	reader := publicationLiveReader(t, proof)
	run := cloneNativeObject(proof["origin_run"].(Object))
	run["run_attempt"], run["status"] = int64(1), "in_progress"
	reader.pages["repos/sample/repo/actions/workflows/automation.yml/runs?per_page=100"] = []any{Object{"total_count": int64(1), "workflow_runs": []any{run}}}
	reader.pages["repos/sample/repo/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(0), "artifacts": []any{}}}
	invocation := Invocation{
		Workflow: "automation.yml", RunName: runContext["run_name"].(string), RecoveryKey: runContext["recovery_key"].(string),
		PackageRoot: root, RunnerTemp: temp, RunID: 100, Attempt: 1,
	}
	workflowSHA := strings.Repeat("9", 40)
	result, err := engine.VerifyPublication(context.Background(), reader, invocation, workflowSHA)
	if err != nil || result["outcome"] != "verified" {
		t.Fatalf("live qualified publication failed: %v %v", result, err)
	}
	filename := filepath.Join(root, filepath.FromSlash(publicationPaths["qualification_file"]))
	first, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal("successful live gate did not persist qualification", err)
	}
	if err := ValidatePublicationQualification(func() Object {
		built, buildErr := BuildPublicationProof(root, proof["origin_run"].(Object), runContext, target, false)
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		return built
	}(), target, runContext, workflowSHA); err != nil {
		t.Fatalf("new qualification does not validate: %v", err)
	}
	result, err = engine.VerifyPublication(context.Background(), reader, invocation, workflowSHA)
	if err != nil || result["outcome"] != "verified" {
		t.Fatalf("revalidation of existing qualification failed: %v %v", result, err)
	}
	second, err := os.ReadFile(filename)
	if err != nil || string(first) != string(second) {
		t.Fatal("repeat verification rewrote the immutable qualification")
	}
}

func TestVerifyPublicationDoesNotQualifyBeforeLiveEvidencePasses(t *testing.T) {
	proof, _, runContext := makePublicationFixture(t, "push-pending")
	temp := t.TempDir()
	root := filepath.Join(temp, "package")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	writePublicationFixture(t, root, proof)
	runContext["schema_version"] = int64(1)
	if err := persistPackageJSON(root, "run-context.json", runContext); err != nil {
		t.Fatal(err)
	}
	repository := contract.Repository{Host: "github.com", Owner: "sample", Name: "repo", URL: "https://github.com/sample/repo"}
	policy := Object{"schema_version": 1, "workflows": Object{"automation.yml": Object{
		"mutator_step_alternatives": []any{[]any{"Publish candidate"}}, "reviewed_source_shas": []any{},
		"allow_publication": true, "plans": Object{},
	}}}
	engine, err := NewEngine(policy, repository)
	if err != nil {
		t.Fatal(err)
	}
	reader := publicationLiveReader(t, proof)
	run := cloneNativeObject(proof["origin_run"].(Object))
	run["run_attempt"], run["status"] = int64(1), "in_progress"
	reader.pages["repos/sample/repo/actions/workflows/automation.yml/runs?per_page=100"] = []any{Object{"total_count": int64(1), "workflow_runs": []any{run}}}
	reader.pages["repos/sample/repo/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(0), "artifacts": []any{}}}
	reader.reads["repos/sample/repo/actions/artifacts/300"]["digest"] = "sha256:" + strings.Repeat("0", 64)
	invocation := Invocation{Workflow: "automation.yml", RunName: runContext["run_name"].(string), RecoveryKey: runContext["recovery_key"].(string), PackageRoot: root, RunnerTemp: temp, RunID: 100, Attempt: 1}
	if _, err := engine.VerifyPublication(context.Background(), reader, invocation, strings.Repeat("9", 40)); err == nil {
		t.Fatal("publication with a changed uploaded artifact was qualified")
	}
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(publicationPaths["qualification_file"]))); !os.IsNotExist(err) {
		t.Fatal("failed live acquisition wrote a qualification")
	}
	metadata, err := LoadFileProof(proof["candidate_artifact_file"], "candidate artifact")
	if err != nil {
		t.Fatal(err)
	}
	reader.reads["repos/sample/repo/actions/artifacts/300"] = metadata.(Object)
	if _, err := engine.VerifyPublication(context.Background(), reader, invocation, strings.Repeat("8", 40)); err == nil {
		t.Fatal("publication with a different trusted workflow source was accepted")
	}
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(publicationPaths["qualification_file"]))); !os.IsNotExist(err) {
		t.Fatal("wrong trusted workflow source wrote a qualification")
	}
}

func TestPublicationLiveGateBindsTrustedControlAndUploadedCandidate(t *testing.T) {
	proof, target, runContext := makePublicationFixture(t, "completed")
	root := t.TempDir()
	writePublicationFixture(t, root, proof)
	repository := contract.Repository{Host: "github.com", Owner: "sample", Name: "repo", URL: "https://github.com/sample/repo"}
	reader := publicationLiveReader(t, proof)
	if err := VerifyPublicationAcquisition(context.Background(), reader, repository, root, proof, target, runContext, true, strings.Repeat("9", 40)); err != nil {
		t.Fatal("qualified unequal control/run-head sources rejected", err)
	}
	reader = publicationLiveReader(t, proof)
	if err := VerifyPublicationAcquisition(context.Background(), reader, repository, root, proof, target, runContext, true, strings.Repeat("8", 40)); err == nil || len(reader.calls) != 0 {
		t.Fatal("wrong trusted workflow source accepted or reached provider")
	}
	for _, mutation := range []string{"provider-digest", "raw-upload", "failed-job", "upstream-ref"} {
		t.Run(mutation, func(t *testing.T) {
			reader := publicationLiveReader(t, proof)
			switch mutation {
			case "provider-digest":
				reader.reads["repos/sample/repo/actions/artifacts/300"]["digest"] = "sha256:" + strings.Repeat("0", 64)
			case "raw-upload":
				reader.archives[300] = append(append([]byte{}, reader.archives[300]...), 'x')
			case "failed-job":
				reader.pages["repos/sample/repo/actions/runs/100/attempts/1/jobs?per_page=100"][0].(Object)["jobs"].([]any)[0].(Object)["conclusion"] = "failure"
			case "upstream-ref":
				reader.reads["repos/sample/repo/actions/artifacts/301"]["digest"] = "sha256:" + strings.Repeat("0", 64)
			}
			if err := VerifyPublicationAcquisition(context.Background(), reader, repository, root, proof, target, runContext, true, strings.Repeat("9", 40)); err == nil {
				t.Fatal("publication accepted changed provider evidence")
			}
		})
	}
}
