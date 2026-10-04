package runrecovery

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func publicationAcquisitionPolicy() Object {
	return Object{"schema_version": 1, "workflows": Object{"automation.yml": Object{
		"mutator_step_alternatives": []any{[]any{"Publish candidate"}}, "reviewed_source_shas": []any{},
		"allow_publication": true, "plans": Object{},
	}}}
}

func publicationAcquisitionFixture(t *testing.T) (*Engine, *recoveryReaderFixture, Invocation, Object, Object) {
	t.Helper()
	proof, _, runContext := makePublicationFixture(t, "push-pending")
	repository := contract.Repository{Host: "github.com", Owner: "sample", Name: "repo", URL: "https://github.com/sample/repo"}
	engine, err := NewEngine(publicationAcquisitionPolicy(), repository)
	if err != nil {
		t.Fatal(err)
	}
	reader := publicationLiveReader(t, proof)
	run := cloneNativeObject(proof["origin_run"].(Object))
	run["run_attempt"], run["status"] = int64(1), "in_progress"
	reader.pages["repos/sample/repo/actions/workflows/automation.yml/runs?per_page=100"] = []any{Object{"total_count": 1, "workflow_runs": []any{run}}}
	reader.pages["repos/sample/repo/actions/artifacts?per_page=100"] = []any{Object{"total_count": 0, "artifacts": []any{}}}
	temp := t.TempDir()
	inv := Invocation{Workflow: "automation.yml", RunName: run["display_title"].(string), RecoveryKey: "workflow-history-v2", RunID: 100, Attempt: 1, PackageRoot: filepath.Join(temp, "package"), RunnerTemp: temp}
	result, err := engine.Recover(context.Background(), reader, inv)
	if err != nil || result["outcome"] != "fresh" {
		t.Fatalf("fresh frontier fixture failed: %v %v", result, err)
	}
	return engine, reader, inv, proof, runContext
}

func TestPublicationAcquisitionRetainsExactVerifierPacketBeforeIntentAndQualification(t *testing.T) {
	e, r, inv, proof, runContext := publicationAcquisitionFixture(t)
	metadata := r.reads["repos/sample/repo/actions/artifacts/300"]
	result, err := e.AcquirePublicationCandidate(context.Background(), r, inv, 300, metadata["digest"].(string), strings.Repeat("9", 40))
	if err != nil || result["outcome"] != "acquired" {
		t.Fatalf("exact publication acquisition failed: %v %v", result, err)
	}
	for _, key := range []string{"candidate_file", "patch_file", "result_file", "event_file", "candidate_archive_file"} {
		want, err := LoadRawFileProof(proof[key], key)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ReadPackageFile(inv.PackageRoot, publicationPaths[key])
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("acquisition changed %s: %v", key, err)
		}
	}
	for _, key := range []string{"intent_file", "push_ack_file", "pr_ack_file", "qualification_file"} {
		if _, err := os.Lstat(filepath.Join(inv.PackageRoot, publicationPaths[key])); !os.IsNotExist(err) {
			t.Fatalf("acquisition fabricated %s", key)
		}
	}
	if _, err := e.AcquirePublicationCandidate(context.Background(), r, inv, 300, metadata["digest"].(string), strings.Repeat("9", 40)); err == nil {
		t.Fatal("acquisition replaced an already retained packet")
	}
	// A separate publisher preparation produces the reviewed exact intent. A
	// new engine revalidates the acquired raw packet and creates qualification.
	intentBytes, err := LoadRawFileProof(proof["intent_file"], "intent")
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPackageFile(inv.PackageRoot, publicationPaths["intent_file"], intentBytes); err != nil {
		t.Fatal(err)
	}
	runContext["schema_version"] = 1
	if err := persistPackageJSON(inv.PackageRoot, "run-context.json", runContext); err != nil {
		t.Fatal(err)
	}
	inv.RecoveryKey = runContext["recovery_key"].(string)
	freshEngine, err := NewEngine(publicationAcquisitionPolicy(), e.repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := freshEngine.VerifyPublication(context.Background(), r, inv, strings.Repeat("9", 40)); err != nil {
		t.Fatalf("separate publisher could not qualify acquired bytes: %v", err)
	}
}

func TestPublicationAcquisitionRejectsForeignIncompleteAndChangedEvidenceWithoutWriting(t *testing.T) {
	for _, mutation := range []string{"control", "name", "head", "digest", "failed-job", "duplicate-job", "upstream", "candidate-source", "candidate-event", "extra-file", "prior-context", "observation", "pending"} {
		t.Run(mutation, func(t *testing.T) {
			e, r, inv, _, _ := publicationAcquisitionFixture(t)
			metadata := r.reads["repos/sample/repo/actions/artifacts/300"]
			digest := metadata["digest"].(string)
			workflowSHA := strings.Repeat("9", 40)
			switch mutation {
			case "control":
				workflowSHA = strings.Repeat("8", 40)
			case "name":
				metadata["name"] = "gh-steward-candidate-100-2"
			case "head":
				metadata["workflow_run"].(Object)["head_sha"] = strings.Repeat("a", 40)
			case "digest":
				digest = "sha256:" + strings.Repeat("0", 64)
			case "failed-job", "duplicate-job":
				page := r.pages["repos/sample/repo/actions/runs/100/attempts/1/jobs?per_page=100"][0].(Object)
				jobs := page["jobs"].([]any)
				if mutation == "failed-job" {
					jobs[0].(Object)["conclusion"] = "failure"
				} else {
					duplicate := cloneNativeObject(jobs[0].(Object))
					duplicate["id"] = int64(999)
					page["jobs"], page["total_count"] = append(jobs, duplicate), len(jobs)+1
				}
			case "upstream":
				r.reads["repos/sample/repo/actions/artifacts/301"]["expired"] = true
			case "candidate-source", "candidate-event", "extra-file":
				files, err := archiveFiles(r.archives[300], false)
				if err != nil {
					t.Fatal(err)
				}
				if mutation == "extra-file" {
					files["other.txt"] = []byte("unbound")
				} else {
					value, err := DecodeValue(files["publication/candidate.json"])
					if err != nil {
						t.Fatal(err)
					}
					candidate := value.(Object)
					if mutation == "candidate-source" {
						candidate["source_repository"] = "foreign/repo"
					} else {
						candidate["trigger_event_sha256"] = strings.Repeat("0", 64)
					}
					files["publication/candidate.json"], err = Canonical(candidate)
					if err != nil {
						t.Fatal(err)
					}
				}
				entries := []recoveryZipEntry{}
				for path, raw := range files {
					entries = append(entries, recoveryZipEntry{name: path, data: raw})
				}
				r.archives[300] = recoveryZip(t, entries...)
				metadata["digest"] = "sha256:" + SHA256(r.archives[300])
				digest = metadata["digest"].(string)
			case "prior-context":
				if err := persistPackageJSON(inv.PackageRoot, "run-context.json", Object{"phase": "dispatching"}); err != nil {
					t.Fatal(err)
				}
			case "observation":
				value, err := LoadJSON(filepath.Join(inv.PackageRoot, "recovery-observation.json"))
				if err != nil {
					t.Fatal(err)
				}
				value.(Object)["outcome"] = "terminal"
				if err := persistPackageJSON(inv.PackageRoot, "recovery-observation.json", value); err != nil {
					t.Fatal(err)
				}
			case "pending":
				previous := cloneNativeObject(r.pages["repos/sample/repo/actions/workflows/automation.yml/runs?per_page=100"][0].(Object)["workflow_runs"].([]any)[0].(Object))
				previous["id"], previous["status"], previous["created_at"] = int64(99), "completed", "2026-10-04T11:00:00Z"
				page := r.pages["repos/sample/repo/actions/workflows/automation.yml/runs?per_page=100"][0].(Object)
				page["workflow_runs"] = append(page["workflow_runs"].([]any), previous)
				page["total_count"] = 2
			}
			if _, err := e.AcquirePublicationCandidate(context.Background(), r, inv, 300, digest, workflowSHA); err == nil {
				t.Fatalf("acquisition accepted %s", mutation)
			}
			for _, path := range []string{"publication/candidate.json", "publication/candidate-artifact.json", "publication/qualification.json"} {
				if _, err := os.Lstat(filepath.Join(inv.PackageRoot, path)); !os.IsNotExist(err) {
					t.Fatalf("failed acquisition left %s", path)
				}
			}
		})
	}
}

func TestPublicationAcquisitionRejectsUnparsedActionOutputIDs(t *testing.T) {
	e, r, inv, _, _ := publicationAcquisitionFixture(t)
	files, err := archiveFiles(r.archives[300], false)
	if err != nil {
		t.Fatal(err)
	}
	value, err := DecodeValue(files["publication/candidate.json"])
	if err != nil {
		t.Fatal(err)
	}
	candidate := value.(Object)
	upstream := candidate["upstream_artifacts"].(Object)
	upstream["capture"].(Object)["id"] = "301"
	upstream["proposal"].(Object)["id"] = "302"
	raw, err := Canonical(candidate)
	if err != nil {
		t.Fatal(err)
	}
	files["publication/candidate.json"] = raw
	entries := []recoveryZipEntry{}
	for path, data := range files {
		entries = append(entries, recoveryZipEntry{name: path, data: data})
	}
	r.archives[300] = recoveryZip(t, entries...)
	digest := "sha256:" + SHA256(r.archives[300])
	r.reads["repos/sample/repo/actions/artifacts/300"]["digest"] = digest
	if _, err := e.AcquirePublicationCandidate(context.Background(), r, inv, 300, digest, strings.Repeat("9", 40)); err == nil {
		t.Fatal("unparsed string Action IDs were accepted as native integer artifact identities")
	}
	if _, err := os.Lstat(filepath.Join(inv.PackageRoot, "publication/candidate.json")); !os.IsNotExist(err) {
		t.Fatal("invalid string IDs left a partial candidate packet")
	}
}
