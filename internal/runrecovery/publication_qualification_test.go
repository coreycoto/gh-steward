package runrecovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicationQualificationBindsExactRetainedEvidence(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "completed")
	workflowSHA := strings.Repeat("9", 40)
	if err := ValidatePublicationQualification(proof, target, context, workflowSHA); err != nil {
		t.Fatalf("valid qualification rejected: %v", err)
	}
	if err := ValidatePublicationQualification(proof, target, context, strings.Repeat("8", 40)); err == nil {
		t.Fatal("qualification accepted a different trusted control source")
	}

	mutations := map[string]func(Object){
		"schema":          func(value Object) { value["schema_version"] = int64(2) },
		"workflow":        func(value Object) { value["workflow_file"] = "other.yml" },
		"workflow-id":     func(value Object) { value["workflow_id"] = int64(43) },
		"repository":      func(value Object) { value["repository"] = "other/repo" },
		"server":          func(value Object) { value["server_url"] = "https://example.test" },
		"run":             func(value Object) { value["run_id"] = int64(101) },
		"attempt":         func(value Object) { value["attempt"] = int64(2) },
		"run-name":        func(value Object) { value["run_name"] = "another run" },
		"recovery-key":    func(value Object) { value["recovery_key"] = "another-key" },
		"attempt-target":  func(value Object) { value["attempt_target"] = Object{"scope": "another"} },
		"workflow-sha":    func(value Object) { value["workflow_sha"] = strings.Repeat("8", 40) },
		"intent-bytes":    func(value Object) { value["raw_files"].(Object)["intent"] = strings.Repeat("0", 64) },
		"candidate-bytes": func(value Object) { value["raw_files"].(Object)["candidate"] = strings.Repeat("0", 64) },
		"patch-bytes":     func(value Object) { value["raw_files"].(Object)["patch"] = strings.Repeat("0", 64) },
		"result-bytes":    func(value Object) { value["raw_files"].(Object)["result"] = strings.Repeat("0", 64) },
		"event-bytes":     func(value Object) { value["raw_files"].(Object)["event"] = strings.Repeat("0", 64) },
		"upstream-id":     func(value Object) { value["upstream_artifacts"].(Object)["capture"].(Object)["id"] = int64(999) },
		"upstream-name":   func(value Object) { value["upstream_artifacts"].(Object)["proposal"].(Object)["name"] = "other" },
		"upstream-hash": func(value Object) {
			value["upstream_artifacts"].(Object)["capture"].(Object)["digest"] = "sha256:" + strings.Repeat("0", 64)
		},
		"artifact-id":   func(value Object) { value["candidate_artifact"].(Object)["id"] = int64(999) },
		"artifact-name": func(value Object) { value["candidate_artifact"].(Object)["name"] = "other" },
		"artifact-hash": func(value Object) {
			value["candidate_artifact"].(Object)["digest"] = "sha256:" + strings.Repeat("0", 64)
		},
		"job-id":      func(value Object) { value["verification_job_id"] = int64(999) },
		"extra-field": func(value Object) { value["unreviewed"] = true },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := cloneNativeObject(proof)
			value, err := LoadFileProof(changed["qualification_file"], "qualification")
			if err != nil {
				t.Fatal(err)
			}
			qualification := value.(Object)
			mutate(qualification)
			bytes, err := Canonical(qualification)
			if err != nil {
				t.Fatal(err)
			}
			changed["qualification_file"] = MakeFileProof(bytes)
			if err := ValidatePublicationQualification(changed, target, context, workflowSHA); err == nil {
				t.Fatal("changed qualification was accepted")
			}
		})
	}
}

func TestPublicationQualificationIsRequiredAfterAnyPositiveWriteACK(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "push-pending")
	if _, err := ValidatePublicationProof(proof, target, context, false); err != nil {
		t.Fatalf("initial unacknowledged push-pending state rejected: %v", err)
	}

	proof, target, context = makePublicationFixture(t, "completed")
	proof["qualification_file"] = nil
	if _, err := ValidatePublicationProof(proof, target, context, false); err == nil {
		t.Fatal("positive write acknowledgements without qualification were accepted")
	}

	proof, target, context = makeCreatedPRFixture(t)
	publication := context["publication"].(Object)
	publication["stage"] = "pr-pending"
	publication["pr_ack"], publication["verify_ack"] = nil, nil
	proof["pr_ack_file"], proof["verify_ack_file"] = nil, nil
	proof["qualification_file"] = nil
	context["phase"] = "dispatching"
	context["dispatch_steps"] = []any{"pull-request-create"}
	if _, err := ValidatePublicationProof(proof, target, context, false); err == nil {
		t.Fatal("pending PR after a positive branch-push acknowledgement lacked qualification")
	}
}

func TestPublicationQualificationRejectsRawFileAndVerificationAttemptDrift(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "completed")
	changed := cloneNativeObject(proof)
	patch, err := LoadRawFileProof(changed["patch_file"], "patch")
	if err != nil {
		t.Fatal(err)
	}
	changed["patch_file"] = MakeFileProof(append(patch, '\n'))
	if _, err := ValidatePublicationProof(changed, target, context, true); err == nil {
		t.Fatal("qualification accepted changed raw patch bytes")
	}

	changed = cloneNativeObject(proof)
	jobsValue, err := LoadFileProof(changed["candidate_jobs_file"], "jobs")
	if err != nil {
		t.Fatal(err)
	}
	jobs := jobsValue.([]any)
	job := jobs[0].(Object)["jobs"].([]any)[0].(Object)
	job["run_attempt"] = int64(2)
	changed["candidate_jobs_file"] = nativeJSONFileProof(t, jobs)
	if _, err := ValidatePublicationProof(changed, target, context, true); err == nil {
		t.Fatal("qualification accepted a verification job from a foreign attempt")
	}

	changed = cloneNativeObject(proof)
	changed["qualification_file"] = MakeFileProof([]byte(`{"schema_version":1}`))
	if _, err := ValidatePublicationProof(changed, target, context, true); err == nil {
		t.Fatal("incomplete qualification file was accepted")
	}

	changed = cloneNativeObject(proof)
	qualification, err := LoadRawFileProof(changed["qualification_file"], "qualification")
	if err != nil {
		t.Fatal(err)
	}
	changed["qualification_file"] = MakeFileProof(append([]byte(" "), qualification...))
	if _, err := ValidatePublicationProof(changed, target, context, true); err == nil {
		t.Fatal("noncanonical qualification bytes were accepted")
	}
}

func TestPublicationQualificationCreationIsCurrentOriginOnlyAndExclusive(t *testing.T) {
	proof, target, context := makePublicationFixture(t, "push-pending")
	workflowSHA := strings.Repeat("9", 40)
	qualification, err := publicationQualification(proof, target, context, workflowSHA)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := persistPublicationQualification(root, qualification); err != nil {
		t.Fatal(err)
	}
	first, err := ReadPackageFile(root, publicationPaths["qualification_file"])
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPublicationQualification(root, Object{"schema_version": int64(1)}); err == nil {
		t.Fatal("qualification writer replaced an existing immutable file")
	}
	second, err := ReadPackageFile(root, publicationPaths["qualification_file"])
	if err != nil || string(first) != string(second) {
		t.Fatal("failed replacement changed the persisted qualification")
	}

	if _, err := publicationQualification(proof, target, context, strings.Repeat("8", 40)); err == nil {
		t.Fatal("qualification creation accepted another control source")
	}
	foreign := cloneNativeObject(context)
	foreign["workflow_run_id"] = int64(101)
	if _, err := publicationQualification(proof, target, foreign, workflowSHA); err == nil {
		t.Fatal("qualification creation accepted another workflow run")
	}
	completed, target, context := makePublicationFixture(t, "completed")
	if _, err := publicationQualification(completed, target, context, workflowSHA); err == nil {
		t.Fatal("qualification constructor accepted a publication after a positive write ACK")
	}

	unsafeRoot := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(unsafeRoot, "publication")); err != nil {
		t.Fatal(err)
	}
	if err := persistPublicationQualification(unsafeRoot, qualification); err == nil {
		t.Fatal("qualification writer followed a symbolic package directory")
	}
}
