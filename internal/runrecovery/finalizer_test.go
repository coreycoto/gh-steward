package runrecovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func finalizerPublicationFixture(t *testing.T) (*Engine, *recoveryReaderFixture, FinalizeOptions, Object) {
	t.Helper()
	proof, target, runContext := makePublicationFixture(t, "completed")
	rootTemp := t.TempDir()
	root := filepath.Join(rootTemp, "invocation")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	writePublicationFixture(t, root, proof)
	origin := proof["origin_run"].(Object)
	for key, value := range (Object{"schema_version": 1, "workflow_file": target["workflow_file"], "repository": target["repository"], "workflow_run_id": int64(100), "workflow_run_attempt": int64(1), "run_name": origin["display_title"], "attempt_target": Object{"publication_origin": int64(100)}}) {
		runContext[key] = value
	}
	if err := persistPackageJSON(root, "run-context.json", runContext); err != nil {
		t.Fatal(err)
	}
	chain, err := EmptyChain(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPackageJSON(root, "settlement-chain.json", chain); err != nil {
		t.Fatal(err)
	}
	policy := Object{"schema_version": 1, "workflows": Object{"automation.yml": Object{"mutator_step_alternatives": []any{[]any{"Publish candidate"}}, "reviewed_source_shas": []any{}, "allow_publication": true, "plans": Object{}}}}
	repository := contract.Repository{Host: "github.com", Owner: "sample", Name: "repo", URL: "https://github.com/sample/repo"}
	engine, err := NewEngine(policy, repository)
	if err != nil {
		t.Fatal(err)
	}
	reader := publicationLiveReader(t, proof)
	run := cloneNativeObject(origin)
	run["run_attempt"], run["status"] = int64(1), "in_progress"
	reader.pages["repos/sample/repo/actions/workflows/automation.yml/runs?per_page=100"] = []any{Object{"total_count": 1, "workflow_runs": []any{run}}}
	files, err := retainedPackageFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]recoveryZipEntry, 0, len(files))
	for _, name := range files {
		data, err := ReadPackageFile(root, name)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, recoveryZipEntry{name: name, data: data})
	}
	archive := recoveryZip(t, entries...)
	artifactName := RecoveryArtifactName(target, 100, 1)
	reader.reads["repos/sample/repo/actions/artifacts/500"] = Object{"id": int64(500), "name": artifactName, "expired": false, "digest": "sha256:" + SHA256(archive), "workflow_run": Object{"id": int64(100), "head_sha": origin["head_sha"]}}
	reader.archives[500] = archive
	intentValue, err := LoadFileProof(proof["intent_file"], "publication intent")
	if err != nil {
		t.Fatal(err)
	}
	options := FinalizeOptions{Invocation: Invocation{Workflow: "automation.yml", RunID: 100, Attempt: 1, PackageRoot: root, RunnerTemp: rootTemp}, ArtifactID: 500, ArtifactDigest: "sha256:" + SHA256(archive), Checkpoint: filepath.Join(rootTemp, "checkpoint"), WorkflowSHA: intentValue.(Object)["workflow_sha"].(string)}
	return engine, reader, options, target
}

func TestFinalizerRequiresActualUploadedRawProofBeforeAdvancing(t *testing.T) {
	engine, reader, options, target := finalizerPublicationFixture(t)
	result, err := engine.Finalize(context.Background(), reader, options)
	if err != nil || result["outcome"] != "checkpoint" {
		t.Fatalf("qualified publication finalization failed: %v %v", result, err)
	}
	value, err := LoadJSON(result["checkpoint_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	chain := value.(Object)
	if len(chain["settlements"].([]any)) != 1 || !Equal(chain["target"], target) {
		t.Fatal("finalizer lost the exact settled attempt or host identity")
	}
	if _, err := engine.Finalize(context.Background(), reader, options); err == nil {
		t.Fatal("existing immutable checkpoint replaced")
	}
}

func TestFinalizerPreservesHoldOnUploadMismatchOrIncompleteContext(t *testing.T) {
	for _, mutation := range []string{"retained-bytes", "upload-receipt", "extra-uploaded-file", "phase", "missing-control-sha", "wrong-control-sha"} {
		t.Run(mutation, func(t *testing.T) {
			engine, reader, options, _ := finalizerPublicationFixture(t)
			switch mutation {
			case "missing-control-sha":
				options.WorkflowSHA = ""
			case "wrong-control-sha":
				options.WorkflowSHA = strings.Repeat("8", 40)
			case "retained-bytes":
				if err := os.WriteFile(filepath.Join(options.PackageRoot, "events", "trigger-event.json"), []byte(`{"different":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "upload-receipt":
				options.ArtifactDigest = "sha256:" + SHA256([]byte("foreign"))
			case "extra-uploaded-file":
				files, err := archiveFiles(reader.archives[500], false)
				if err != nil {
					t.Fatal(err)
				}
				entries := []recoveryZipEntry{{name: "recovery-needed.json", data: []byte(`{"status":"recovery_needed"}`)}}
				for name, data := range files {
					entries = append(entries, recoveryZipEntry{name: name, data: data})
				}
				payload := recoveryZip(t, entries...)
				reader.archives[500] = payload
				options.ArtifactDigest = "sha256:" + SHA256(payload)
				reader.reads["repos/sample/repo/actions/artifacts/500"]["digest"] = options.ArtifactDigest
			case "phase":
				value, err := LoadJSON(filepath.Join(options.PackageRoot, "run-context.json"))
				if err != nil {
					t.Fatal(err)
				}
				value.(Object)["phase"] = "dispatching"
				if err := persistPackageJSON(options.PackageRoot, "run-context.json", value); err != nil {
					t.Fatal(err)
				}
			}
			result, err := engine.Finalize(context.Background(), reader, options)
			if mutation == "phase" {
				if err != nil || result["outcome"] != "pending" {
					t.Fatalf("unfinished phase manufactured checkpoint: %v %v", result, err)
				}
			} else if err == nil {
				t.Fatal("changed uploaded or retained proof accepted")
			}
			if _, err := os.Stat(options.Checkpoint); !os.IsNotExist(err) {
				t.Fatal("rejected finalization wrote a checkpoint")
			}
		})
	}
}
