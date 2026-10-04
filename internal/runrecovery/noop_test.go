package runrecovery

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"github.com/coreycoto/gh-steward/internal/contract"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const noopWorkflowSource = "name: Task\non: workflow_dispatch\njobs:\n  apply:\n    name: Mutation job\n    runs-on: ubuntu-latest\n    steps:\n      - name: Apply reviewed change\n        if: false\n        run: gh steward apply\n"

func noopSourcePacket() Object {
	raw := []byte(noopWorkflowSource)
	blob := sha1.Sum(append([]byte(fmt.Sprintf("blob %d%c", len(raw), 0)), raw...))
	return Object{"type": "file", "path": ".github/workflows/task.yml", "encoding": "base64", "content": base64.StdEncoding.EncodeToString(raw), "sha": hex.EncodeToString(blob[:])}
}

func noopEngine(t *testing.T) *Engine {
	t.Helper()
	policy := Object{"schema_version": 1, "workflows": Object{"task.yml": Object{
		"mutator_step_alternatives": []any{[]any{"Apply reviewed change"}},
		"reviewed_source_shas":      []any{}, "allow_publication": false,
		"plans": Object{"workflow-noop": Object{"command": "workflow-noop", "domain_profile": "workflow-noop", "allowed_operation_kinds": []any{}, "attempt_target": "workflow_noop", "approval": Object{"kind": "local-noop", "workflow_source_sha256": SHA256([]byte(noopWorkflowSource)), "mutators": []any{Object{"job": "Mutation job", "steps": []any{"Apply reviewed change"}}}}, "event": Object{"kind": "workflow_noop", "path": "events/trigger-event.json"}, "parent_merge": nil}},
	}}}
	repository := contract.Repository{Host: "github.com", Owner: "example", Name: "widgets", URL: "https://github.com/example/widgets"}
	e, err := NewEngine(policy, repository)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func startNoopFixture(t *testing.T, e *Engine, r *recoveryReaderFixture, rootTemp string, id, attempt int64, decision string) NoopOptions {
	t.Helper()
	invocation := Invocation{Workflow: "task.yml", RunID: id, Attempt: attempt, RunName: "Current", RecoveryKey: "task", PackageRoot: filepath.Join(rootTemp, "invocation"), RunnerTemp: rootTemp}
	recovered, err := e.Recover(context.Background(), r, invocation)
	if err != nil || (recovered["outcome"] != "fresh" && recovered["outcome"] != "terminal") {
		t.Fatalf("new protocol observation: %v %v", recovered, err)
	}
	workflowSHA := strings.Repeat("b", 40)
	event := Object{"repository": Object{"full_name": "example/widgets", "html_url": "https://github.com/example/widgets"}, "inputs": Object{}}
	if err := persistPackageJSON(invocation.PackageRoot, "events/trigger-event.json", event); err != nil {
		t.Fatal(err)
	}
	eventBytes, err := ReadPackageFile(invocation.PackageRoot, "events/trigger-event.json")
	if err != nil {
		t.Fatal(err)
	}
	target := Object{"event_sha256": SHA256(eventBytes)}
	_, err = e.InitializeContext(invocation.PackageRoot, Object{"workflow_file": "task.yml", "repository": "example/widgets", "recovery_key": "task", "run_name": "Current", "run_id": id, "attempt": attempt, "attempt_target": target, "dispatch_steps": []any{}, "trusted_source_sha": workflowSHA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetContextPhase(invocation.PackageRoot, "prepared", ""); err != nil {
		t.Fatal(err)
	}
	statement := Object{"schema_version": 1, "decision": decision, "repository": "example/widgets", "server_url": "https://github.com", "workflow_file": "task.yml", "run_id": id, "attempt": attempt, "recovery_key": "task", "attempt_target": target, "workflow_sha": workflowSHA, "event_name": "workflow_dispatch", "event_sha256": SHA256(eventBytes)}
	if err := persistPackageJSON(invocation.PackageRoot, noopDecisionPath, statement); err != nil {
		t.Fatal(err)
	}
	return NoopOptions{Invocation: invocation, WorkflowSHA: workflowSHA, ToolVersion: "test"}
}

func readerWithCurrentNoop(id, attempt int64) *recoveryReaderFixture {
	run := recoveryHistoryRun(id, attempt, "Current")
	run["status"], run["conclusion"] = "in_progress", nil
	return &recoveryReaderFixture{
		reads: map[string]Object{"repos/example/widgets/contents/.github/workflows/task.yml?ref=" + strings.Repeat("b", 40): noopSourcePacket()}, archives: map[int64][]byte{},
		pages: map[string][]any{
			"repos/example/widgets/actions/workflows/task.yml/runs?per_page=100":                                     {Object{"total_count": 1, "workflow_runs": []any{run}}},
			"repos/example/widgets/actions/artifacts?per_page=100":                                                   {Object{"total_count": 0, "artifacts": []any{}}},
			"repos/example/widgets/actions/runs/" + fmtID(id) + "/attempts/" + fmtID(attempt) + "/jobs?per_page=100": {Object{"total_count": 1, "jobs": []any{Object{"id": id*100 + attempt, "run_id": id, "run_attempt": attempt, "name": "Mutation job", "status": "in_progress", "steps": []any{Object{"name": "Apply reviewed change", "status": "completed", "conclusion": "skipped"}}}}}},
		},
	}
}

func uploadFixturePackage(t *testing.T, r *recoveryReaderFixture, e *Engine, inv Invocation, artifactID int64) (Object, []byte) {
	t.Helper()
	files, err := retainedPackageFiles(inv.PackageRoot)
	if err != nil {
		t.Fatal(err)
	}
	entries := []recoveryZipEntry{}
	for _, name := range files {
		data, err := ReadPackageFile(inv.PackageRoot, name)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, recoveryZipEntry{name: name, data: data})
	}
	payload := recoveryZip(t, entries...)
	target, err := TargetObject(inv.Workflow, e.repository.URL, 9, "workflow-history-v2")
	if err != nil {
		t.Fatal(err)
	}
	metadata := Object{"id": artifactID, "name": RecoveryArtifactName(target, inv.RunID, inv.Attempt), "expired": false, "digest": "sha256:" + SHA256(payload), "workflow_run": Object{"id": inv.RunID, "head_sha": strings.Repeat("a", 40)}}
	r.reads["repos/example/widgets/actions/artifacts/"+fmtID(artifactID)] = metadata
	r.archives[artifactID] = payload
	return metadata, payload
}
func fmtID(id int64) string { return fmt.Sprint(id) }

func TestNegativeReviewKeepsInertCandidateAndProvesApplyWasSkipped(t *testing.T) {
	for _, status := range []string{"noop", "blocked", "approved", "missing"} {
		t.Run(status, func(t *testing.T) {
			e := noopEngine(t)
			r := readerWithCurrentNoop(10, 1)
			options := startNoopFixture(t, e, r, t.TempDir(), 10, 1, "review-declined")
			for name, value := range map[string]Object{"candidate": Object{"schema_version": 2, "operations": []any{Object{"kind": "branch-delete"}}}, "review": Object{"status": status}} {
				if name == "review" && status == "missing" {
					continue
				}
				if err := persistPackageJSON(options.PackageRoot, "proposals/"+name+".json", value); err != nil {
					t.Fatal(err)
				}
			}
			candidate, err := ReadPackageFile(options.PackageRoot, "proposals/candidate.json")
			if err != nil {
				t.Fatal(err)
			}
			var reviewDigest any
			if status != "missing" {
				review, err := ReadPackageFile(options.PackageRoot, "proposals/review.json")
				if err != nil {
					t.Fatal(err)
				}
				reviewDigest = SHA256(review)
			}
			value, err := LoadJSON(filepath.Join(options.PackageRoot, noopDecisionPath))
			if err != nil {
				t.Fatal(err)
			}
			value.(Object)["proposal"] = Object{"candidate_sha256": SHA256(candidate), "review_sha256": reviewDigest}
			if err := persistPackageJSON(options.PackageRoot, noopDecisionPath, value); err != nil {
				t.Fatal(err)
			}
			result, err := e.FinishNoop(context.Background(), r, options)
			if status == "approved" || status == "missing" {
				if err == nil {
					t.Fatal("positive review relabeled as a declined candidate")
				}
				return
			}
			if err != nil || result["outcome"] != "completed" {
				t.Fatalf("positively skipped negative review was not settled: %v %v", result, err)
			}
			original, err := ReadPackageFile(options.PackageRoot, "proposals/candidate.json")
			if err != nil || !Equal(original, candidate) {
				t.Fatal("inert candidate changed")
			}
			planValue, err := LoadJSON(filepath.Join(options.PackageRoot, "plans/workflow-noop.json"))
			if err != nil {
				t.Fatal(err)
			}
			if len(planValue.(Object)["operations"].([]any)) != 0 {
				t.Fatal("candidate operations became executable")
			}
		})
	}
}

func TestEmptyCandidateNoopRetainsNativePlanWithoutInventingReview(t *testing.T) {
	for _, mutation := range []string{"none", "nonempty", "foreign-repository", "invented-review"} {
		t.Run(mutation, func(t *testing.T) {
			e := noopEngine(t)
			r := readerWithCurrentNoop(10, 1)
			options := startNoopFixture(t, e, r, t.TempDir(), 10, 1, "no-change")
			repository := e.repository
			if mutation == "foreign-repository" {
				repository.Owner, repository.URL = "other", "https://github.com/other/widgets"
			}
			operations := []contract.Operation{}
			if mutation == "nonempty" {
				operations = append(operations, nativeTestOperation("remove-branch", "branch-delete"))
			}
			candidate, err := contract.PreparePlan("governance-apply", repository, Object{"github": Object{"live": true, "complete": true}}, Object{"labels": []any{}}, operations, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err := persistPackageJSON(options.PackageRoot, "proposals/candidate.json", candidate.Object()); err != nil {
				t.Fatal(err)
			}
			raw, err := ReadPackageFile(options.PackageRoot, "proposals/candidate.json")
			if err != nil {
				t.Fatal(err)
			}
			decisionValue, err := LoadJSON(filepath.Join(options.PackageRoot, noopDecisionPath))
			if err != nil {
				t.Fatal(err)
			}
			proposal := Object{"candidate_sha256": SHA256(raw), "review_sha256": nil}
			if mutation == "invented-review" {
				if err := persistPackageJSON(options.PackageRoot, "proposals/review.json", Object{"status": "approved"}); err != nil {
					t.Fatal(err)
				}
				review, err := ReadPackageFile(options.PackageRoot, "proposals/review.json")
				if err != nil {
					t.Fatal(err)
				}
				proposal["review_sha256"] = SHA256(review)
			}
			decisionValue.(Object)["proposal"] = proposal
			if err := persistPackageJSON(options.PackageRoot, noopDecisionPath, decisionValue); err != nil {
				t.Fatal(err)
			}
			_, err = e.FinishNoop(context.Background(), r, options)
			if mutation != "none" {
				if err == nil {
					t.Fatal("unqualified empty-candidate decision accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			retained, err := ReadPackageFile(options.PackageRoot, "proposals/candidate.json")
			if err != nil || !Equal(raw, retained) {
				t.Fatal("empty candidate bytes changed")
			}
			if _, err := os.Lstat(filepath.Join(options.PackageRoot, "proposals/review.json")); !os.IsNotExist(err) {
				t.Fatal("invented candidate review")
			}
			if _, err := os.Lstat(filepath.Join(options.PackageRoot, "apply-results/governance.json")); !os.IsNotExist(err) {
				t.Fatal("candidate treated as applied")
			}
		})
	}
}

func TestPreviewOnlyRetainsEveryRawPlanningDocumentWithoutReviewOrExecution(t *testing.T) {
	for _, mutation := range []string{"none", "missing-inventory", "missing-file", "duplicate-path", "unsafe-path", "too-many", "empty-object", "invalid-json", "foreign-native-plan", "malformed-native-plan", "missing-native-command", "missing-native-operations", "null-native-operations", "changed-hash", "invented-proposal", "wrong-decision", "unreferenced-file", "mutation-started"} {
		t.Run(mutation, func(t *testing.T) {
			e := noopEngine(t)
			r := readerWithCurrentNoop(10, 1)
			options := startNoopFixture(t, e, r, t.TempDir(), 10, 1, "preview-only")
			refs := []any{}
			originals := map[string][]byte{}
			for index, relative := range []string{"previews/governance-fix.json", "previews/governance-drift.json", "previews/quarter-delta.json"} {
				value := Object{"schema_version": 1, "normalized": Object{}, "delta": Object{"proposed": 1}}
				if index < 2 {
					repository := e.repository
					if index == 0 && mutation == "foreign-native-plan" {
						repository.Owner, repository.URL = "other", "https://github.com/other/widgets"
					}
					candidate, err := contract.PreparePlan("branch-cleanup-apply", repository, Object{"github": Object{"live": true, "complete": true}}, Object{"selection": index}, []contract.Operation{nativeTestOperation("remove-branch", "branch-delete")}, time.Now())
					if err != nil {
						t.Fatal(err)
					}
					value = candidate.Object()
					if index == 0 && mutation == "malformed-native-plan" {
						value["sha256"] = strings.Repeat("e", 64)
					}
					if index == 0 && mutation == "missing-native-command" {
						delete(value, "command")
					}
					if index == 0 && mutation == "null-native-operations" {
						value["operations"] = nil
					}
					if index == 0 && mutation == "missing-native-operations" {
						delete(value, "operations")
					}
				}
				if index == 0 && mutation == "empty-object" {
					value = Object{}
				}
				if err := persistPackageJSON(options.PackageRoot, relative, value); err != nil {
					t.Fatal(err)
				}
				if index == 0 && mutation == "invalid-json" {
					if err := persistPackageFile(options.PackageRoot, relative, []byte("{invalid JSON")); err != nil {
						t.Fatal(err)
					}
				}
				raw, err := ReadPackageFile(options.PackageRoot, relative)
				if err != nil {
					t.Fatal(err)
				}
				originals[relative] = raw
				refs = append(refs, Object{"path": relative, "sha256": SHA256(raw)})
			}
			decision, err := LoadJSON(filepath.Join(options.PackageRoot, noopDecisionPath))
			if err != nil {
				t.Fatal(err)
			}
			value := decision.(Object)
			value["previews"] = refs
			switch mutation {
			case "missing-inventory":
				delete(value, "previews")
			case "missing-file":
				if err := os.Remove(filepath.Join(options.PackageRoot, "previews/governance-fix.json")); err != nil {
					t.Fatal(err)
				}
			case "duplicate-path":
				value["previews"] = append(refs, refs[0])
			case "unsafe-path":
				refs[0].(Object)["path"] = "previews/../plans/candidate.json"
			case "too-many":
				value["previews"] = make([]any, MaxNoopPreviewFiles+1)
			case "changed-hash":
				refs[0].(Object)["sha256"] = strings.Repeat("d", 64)
			case "invented-proposal":
				value["proposal"] = Object{"candidate_sha256": strings.Repeat("d", 64), "review_sha256": nil}
			case "wrong-decision":
				value["decision"] = "no-change"
			case "unreferenced-file":
				if err := persistPackageJSON(options.PackageRoot, "previews/extra.json", Object{"extra": true}); err != nil {
					t.Fatal(err)
				}
			case "mutation-started":
				jobs := r.pages["repos/example/widgets/actions/runs/10/attempts/1/jobs?per_page=100"][0].(Object)["jobs"].([]any)
				jobs[0].(Object)["steps"].([]any)[0].(Object)["conclusion"] = "success"
			}
			if err := persistPackageJSON(options.PackageRoot, noopDecisionPath, value); err != nil {
				t.Fatal(err)
			}
			result, err := e.FinishNoop(context.Background(), r, options)
			if mutation != "none" {
				if err == nil {
					t.Fatal("unqualified preview-only decision was settled")
				}
				if _, err := os.Stat(filepath.Join(options.PackageRoot, "apply-results/workflow-noop.json")); !os.IsNotExist(err) {
					t.Fatal("failed preview produced terminal evidence")
				}
				return
			}
			if err != nil || result["outcome"] != "completed" {
				t.Fatalf("qualified preview was not settled: %#v %v", result, err)
			}
			planValue, err := LoadJSON(filepath.Join(options.PackageRoot, "plans/workflow-noop.json"))
			if err != nil || len(planValue.(Object)["operations"].([]any)) != 0 || planValue.(Object)["data"].(Object)["decision"] != "preview-only" {
				t.Fatal("preview documents became executable", planValue, err)
			}
			for relative, original := range originals {
				raw, err := ReadPackageFile(options.PackageRoot, relative)
				if err != nil || !Equal(raw, original) {
					t.Fatal("preview bytes changed", relative, err)
				}
			}
			if _, err := os.Stat(filepath.Join(options.PackageRoot, "proposals/review.json")); !os.IsNotExist(err) {
				t.Fatal("preview invented a review")
			}
			metadata, _ := uploadFixturePackage(t, r, e, options.Invocation, 501)
			if _, err := e.Finalize(context.Background(), r, FinalizeOptions{Invocation: options.Invocation, ArtifactID: 501,
				ArtifactDigest: metadata["digest"].(string), Checkpoint: filepath.Join(options.RunnerTemp, "checkpoint")}); err != nil {
				t.Fatal("qualified preview lost planning documents in terminal settlement", err)
			}
		})
	}
}

func TestQualifiedNoopSurvivesReviewedWorkflowInventoryUpdate(t *testing.T) {
	e := noopEngine(t)
	r := readerWithCurrentNoop(1, 1)
	inv := startNoopFixture(t, e, r, t.TempDir(), 1, 1, "no-change")
	if _, err := e.FinishNoop(context.Background(), r, inv); err != nil {
		t.Fatal(err)
	}
	metadata, _ := uploadFixturePackage(t, r, e, inv.Invocation, 501)
	result, err := e.Finalize(context.Background(), r, FinalizeOptions{Invocation: inv.Invocation, ArtifactID: 501, ArtifactDigest: metadata["digest"].(string), Checkpoint: filepath.Join(inv.RunnerTemp, "checkpoint")})
	if err != nil {
		t.Fatal(err)
	}
	chain, err := LoadJSON(result["checkpoint_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	_, _, target, observed, attempts, err := e.invocationHistory(context.Background(), r, inv.Invocation)
	if err != nil {
		t.Fatal(err)
	}
	prior := Object{"workflow_source_sha256": SHA256([]byte(noopWorkflowSource)), "mutators": []any{Object{"job": "Mutation job", "steps": []any{"Apply reviewed change"}}}}
	for _, keep := range []bool{true, false} {
		approval := Object{"kind": "local-noop", "workflow_source_sha256": SHA256([]byte("updated workflow")), "mutators": []any{Object{"job": "New mutation job", "steps": []any{"New write step"}}}}
		if keep {
			approval["previous_sources"] = []any{prior}
		}
		parsed, err := parseNoopPolicy(approval)
		if err != nil {
			t.Fatal(err)
		}
		wf := e.workflows["task.yml"]
		pp := wf.plans["workflow-noop"]
		pp.approval.noop = parsed
		wf.plans["workflow-noop"] = pp
		e.workflows["task.yml"] = wf
		_, err = e.ValidateChain(chain, target, observed, attempts)
		if keep && err != nil {
			t.Fatalf("qualified prior proof lost after policy update: %v", err)
		}
		if !keep && err == nil {
			t.Fatal("unreviewed prior workflow source accepted")
		}
	}
}

func TestCurrentNoopCreatesNativeTerminalAndClosesTwoPredecessorsInOrder(t *testing.T) {
	e := noopEngine(t)
	r := readerWithCurrentNoop(1, 1)
	one := startNoopFixture(t, e, r, t.TempDir(), 1, 1, "no-change")
	if result, err := e.FinishNoop(context.Background(), r, one); err != nil || result["outcome"] != "completed" {
		t.Fatalf("no-op finish: %v %v", result, err)
	}
	artifact1, _ := uploadFixturePackage(t, r, e, one.Invocation, 501)
	result, err := e.Finalize(context.Background(), r, FinalizeOptions{Invocation: one.Invocation, ArtifactID: 501, ArtifactDigest: artifact1["digest"].(string), Checkpoint: filepath.Join(one.RunnerTemp, "checkpoint")})
	if err != nil || result["outcome"] != "checkpoint" {
		t.Fatalf("real no-op checkpoint: %v %v", result, err)
	}
	if _, err := e.FinishNoop(context.Background(), r, one); err == nil {
		t.Fatal("completed no-op evidence replaced")
	}

	old1 := recoveryHistoryRun(1, 1, "Current")
	current2 := recoveryHistoryRun(2, 1, "Current")
	current2["status"], current2["conclusion"] = "in_progress", nil
	r.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"] = []any{Object{"total_count": 2, "workflow_runs": []any{old1, current2}}}
	r.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": 1, "artifacts": []any{artifact1}}}
	r.reads["repos/example/widgets/actions/runs/1/attempts/1"] = old1
	r.pages["repos/example/widgets/actions/runs/2/attempts/1/jobs?per_page=100"] = readerWithCurrentNoop(2, 1).pages["repos/example/widgets/actions/runs/2/attempts/1/jobs?per_page=100"]
	two := startNoopFixture(t, e, r, t.TempDir(), 2, 1, "prerequisite-unavailable")
	if _, err := e.FinishNoop(context.Background(), r, two); err != nil {
		t.Fatal(err)
	}
	artifact2, _ := uploadFixturePackage(t, r, e, two.Invocation, 502)

	old2 := recoveryHistoryRun(2, 1, "Current")
	current3 := recoveryHistoryRun(3, 1, "Current")
	current3["status"], current3["conclusion"] = "in_progress", nil
	r.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"] = []any{Object{"total_count": 3, "workflow_runs": []any{old1, old2, current3}}}
	r.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": 2, "artifacts": []any{artifact1, artifact2}}}
	r.reads["repos/example/widgets/actions/runs/2/attempts/1"] = old2
	// A later missing attempt must not discard a positively settled prefix.
	r.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": 1, "artifacts": []any{artifact1}}}
	heldTemp := t.TempDir()
	held := Invocation{Workflow: "task.yml", RunID: 3, Attempt: 1, RunName: "Current", RecoveryKey: "task", RunnerTemp: heldTemp, PackageRoot: filepath.Join(heldTemp, "invocation")}
	if result, err := e.Recover(context.Background(), r, held); err != nil || result["outcome"] != "recovery_needed" {
		t.Fatalf("missing later attempt did not stay held: %v %v", result, err)
	}
	heldChain, err := LoadJSON(filepath.Join(held.PackageRoot, "settlement-chain.json"))
	if err != nil {
		t.Fatal(err)
	}
	settledPrefix := heldChain.(Object)["settlements"].([]any)
	if len(settledPrefix) != 1 || !exactInt(settledPrefix[0].(Object)["run_id"], 1) {
		t.Fatal("later hold discarded or expanded the verified predecessor prefix")
	}
	r.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": 2, "artifacts": []any{artifact1, artifact2}}}
	temp := t.TempDir()
	three := Invocation{Workflow: "task.yml", RunID: 3, Attempt: 1, RunName: "Current", RecoveryKey: "task", RunnerTemp: temp, PackageRoot: filepath.Join(temp, "invocation")}
	recovered, err := e.Recover(context.Background(), r, three)
	if err != nil || recovered["outcome"] != "fresh" {
		t.Fatalf("multiple valid terminal predecessors were blocked: %v %v", recovered, err)
	}
	value, err := LoadJSON(filepath.Join(three.PackageRoot, "settlement-chain.json"))
	if err != nil {
		t.Fatal(err)
	}
	settlements := value.(Object)["settlements"].([]any)
	if len(settlements) != 2 || !exactInt(settlements[0].(Object)["run_id"], 1) || !exactInt(settlements[1].(Object)["run_id"], 2) {
		t.Fatal("terminal predecessors advanced out of order")
	}
	changed, err := cloneObject(value.(Object))
	if err != nil {
		t.Fatal(err)
	}
	changed["settlements"].([]any)[0].(Object)["artifact"].(Object)["id"] = int64(999)
	delete(changed, "sha256")
	unsigned, err := Canonical(changed)
	if err != nil {
		t.Fatal(err)
	}
	changed["sha256"] = SHA256(unsigned)
	_, _, target, observed, attempts, err := e.invocationHistory(context.Background(), r, three)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ValidateChain(changed, target, observed, attempts); err == nil || !strings.Contains(err.Error(), "predecessor prefix") {
		t.Fatalf("recomputed checkpoint changed an immutable no-op predecessor: %v", err)
	}
	entries, err := os.ReadDir(temp)
	if err != nil || len(entries) != 1 {
		t.Fatalf("historical extracted packages retained after recovery: %v %v", entries, err)
	}
}

func TestNoopRefusesPriorExecutionHistoricalOrChangedDecision(t *testing.T) {
	for _, mutation := range []string{"plan", "journal", "publication", "recovered", "target", "event", "observation", "decision", "control-source", "historical", "outcome", "mutation-started", "missing-mutator", "workflow-content"} {
		t.Run(mutation, func(t *testing.T) {
			e := noopEngine(t)
			r := readerWithCurrentNoop(10, 1)
			options := startNoopFixture(t, e, r, t.TempDir(), 10, 1, "no-change")
			contextValue, err := LoadJSON(filepath.Join(options.PackageRoot, "run-context.json"))
			if err != nil {
				t.Fatal(err)
			}
			runContext := contextValue.(Object)
			switch mutation {
			case "plan", "journal":
				if err := persistPackageJSON(options.PackageRoot, mutation+"/previous.json", Object{"status": "unknown"}); err != nil {
					t.Fatal(err)
				}
			case "publication":
				runContext["publication"] = Object{"stage": "push-pending"}
			case "recovered":
				runContext["recovered_from_run_id"], runContext["recovered_from_attempt"] = int64(1), int64(1)
			case "target":
				runContext["attempt_target"] = Object{"event_sha256": strings.Repeat("f", 64)}
			case "event":
				if err := persistPackageJSON(options.PackageRoot, "events/trigger-event.json", Object{"repository": Object{"full_name": "other/repo"}}); err != nil {
					t.Fatal(err)
				}
			case "observation":
				if err := persistPackageJSON(options.PackageRoot, "recovery-observation.json", Object{}); err != nil {
					t.Fatal(err)
				}
			case "decision":
				if err := persistPackageJSON(options.PackageRoot, noopDecisionPath, Object{"status": "noop"}); err != nil {
					t.Fatal(err)
				}
			case "control-source":
				options.WorkflowSHA = strings.Repeat("c", 40)
			case "historical":
				r.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)["workflow_runs"].([]any)[0].(Object)["status"] = "completed"
			case "outcome":
				value, err := LoadJSON(filepath.Join(options.PackageRoot, "recovery-observation.json"))
				if err != nil {
					t.Fatal(err)
				}
				value.(Object)["outcome"] = "resumed"
				if err := persistPackageJSON(options.PackageRoot, "recovery-observation.json", value); err != nil {
					t.Fatal(err)
				}
			case "mutation-started":
				r.pages["repos/example/widgets/actions/runs/10/attempts/1/jobs?per_page=100"][0].(Object)["jobs"].([]any)[0].(Object)["steps"].([]any)[0].(Object)["conclusion"] = "success"
			case "missing-mutator":
				r.pages["repos/example/widgets/actions/runs/10/attempts/1/jobs?per_page=100"] = []any{Object{"total_count": 0, "jobs": []any{}}}
			case "workflow-content":
				r.reads["repos/example/widgets/contents/.github/workflows/task.yml?ref="+options.WorkflowSHA]["content"] = base64.StdEncoding.EncodeToString([]byte("different source"))
			}
			if err := persistPackageJSON(options.PackageRoot, "run-context.json", runContext); err != nil {
				t.Fatal(err)
			}
			if _, err := e.FinishNoop(context.Background(), r, options); err == nil {
				t.Fatal("unsafe prior execution or unbound no-op evidence accepted")
			}
			if _, err := os.Lstat(filepath.Join(options.PackageRoot, "plans", "workflow-noop.json")); !os.IsNotExist(err) {
				t.Fatal("rejected no-op wrote a plan")
			}
		})
	}
}

func TestSettledRerunGetsItsOwnTypedNoopWithoutRepeatingOriginalWork(t *testing.T) {
	e := noopEngine(t)
	r := readerWithCurrentNoop(1, 1)
	one := startNoopFixture(t, e, r, t.TempDir(), 1, 1, "no-change")
	if _, err := e.FinishNoop(context.Background(), r, one); err != nil {
		t.Fatal(err)
	}
	artifact, _ := uploadFixturePackage(t, r, e, one.Invocation, 501)
	current := recoveryHistoryRun(1, 2, "Current")
	current["status"], current["conclusion"] = "in_progress", nil
	r.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"] = []any{Object{"total_count": 1, "workflow_runs": []any{current}}}
	r.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": 1, "artifacts": []any{artifact}}}
	old := recoveryHistoryRun(1, 1, "Current")
	r.reads["repos/example/widgets/actions/runs/1/attempts/1"] = old
	r.pages["repos/example/widgets/actions/runs/1/attempts/2/jobs?per_page=100"] = readerWithCurrentNoop(1, 2).pages["repos/example/widgets/actions/runs/1/attempts/2/jobs?per_page=100"]
	two := startNoopFixture(t, e, r, t.TempDir(), 1, 2, "already-settled")
	if _, err := e.FinishNoop(context.Background(), r, two); err != nil {
		t.Fatal(err)
	}
	artifact2, _ := uploadFixturePackage(t, r, e, two.Invocation, 502)
	result, err := e.Finalize(context.Background(), r, FinalizeOptions{Invocation: two.Invocation, ArtifactID: 502, ArtifactDigest: artifact2["digest"].(string), Checkpoint: filepath.Join(two.RunnerTemp, "checkpoint")})
	if err != nil || result["outcome"] != "checkpoint" {
		t.Fatalf("settled observer got no exact current terminal: %v %v", result, err)
	}
	chainValue, err := LoadJSON(result["checkpoint_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	settlements := chainValue.(Object)["settlements"].([]any)
	if len(settlements) != 2 || !exactInt(settlements[1].(Object)["attempt"], 2) {
		t.Fatal("rerun terminal lost its own attempt")
	}
}
