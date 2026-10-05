package runrecovery

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/workflow"
)

func prepareAllAbsentCleanup(t *testing.T, engine *Engine, root string, provider *composedCleanupProvider, repository contract.Repository) Object {
	t.Helper()
	selection, err := workflow.ParseBranchCleanupSelection(Object{"branches": []any{Object{
		"name": "codex/change", "sha": strings.Repeat("a", 40), "pull_request_number": int64(3),
	}}}, repository)
	if err != nil {
		t.Fatal(err)
	}
	inventory := composedBranchInventory(repository, selection, provider.mergeCommit)
	rows, _ := contract.Objects(inventory, "branches")
	rows[0]["branch"] = nil
	inventory["branch_evidence"] = Object{
		"inventory": Object{
			"repo":       repository.Object(),
			"branches":   []any{Object{"name": "main", "sha": strings.Repeat("c", 40)}},
			"provenance": Object{"live": true, "complete": true, "source": "github_api", "repository_node_id": "R_widgets"},
		},
		"delete_branch_on_merge": true,
	}
	provider.cleanupInventory = inventory
	planValue, err := workflow.PrepareBranchCleanup(context.Background(), provider, repository, selection, time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("prepare positively evidenced all-absent cleanup: %v", err)
	}
	plan := planValue.Object()
	operations, err := (workflow.BranchCleanup{}).Operations(mustParsedPlan(t, plan))
	if err != nil || len(operations) != 0 || len(plan["operations"].([]any)) != 0 {
		t.Fatalf("all-absent cleanup did not produce an exact zero-operation plan: %#v %v", plan["operations"], err)
	}
	if _, err := engine.RecordContextPlan(root, Object{
		"relative": "plans/branch-cleanup.json", "name": "branch-cleanup", "command": workflow.BranchCleanupCommand,
		"repository": repository.FullName(), "plan": plan,
	}); err != nil {
		t.Fatalf("record exact all-absent cleanup child: %v", err)
	}
	return plan
}

func mustParsedPlan(t *testing.T, plan Object) contract.Plan {
	t.Helper()
	parsed, err := contract.ParsePlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func rebuildCleanupPlan(t *testing.T, base Object, mutate func(Object)) Object {
	t.Helper()
	parsed := mustParsedPlan(t, base)
	data, err := contract.Clone(parsed.Data)
	if err != nil {
		t.Fatal(err)
	}
	mutate(data)
	captured, err := time.Parse(time.RFC3339Nano, parsed.CapturedAt)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := contract.PreparePlan(parsed.Command, parsed.Repository, parsed.Sources, data, parsed.Operations, captured)
	if err != nil {
		t.Fatalf("rebuild tampered native plan: %v", err)
	}
	return rebuilt.Object()
}

func TestCompletedMergeZeroOpCleanupRecoveryResumesWithoutWrites(t *testing.T) {
	if inputPath := os.Getenv("GH_STEWARD_ZERO_OP_CLEANUP_INPUT"); inputPath != "" {
		value, err := LoadJSON(inputPath)
		if err != nil {
			t.Fatal(err)
		}
		input, err := object(value, "persisted zero-op cleanup fixture")
		if err != nil {
			t.Fatal(err)
		}
		report := runPersistedZeroOpCleanupLifecycle(t, input)
		encoded, err := Canonical(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(input["result_path"].(string), append(encoded, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}

	for _, journalState := range []string{"before-journal", "open-empty-journal", "completed-empty-journal"} {
		t.Run(journalState, func(t *testing.T) {
			repository := policyTestRepository(t)
			engine, err := NewEngine(completedMergePolicy(), repository)
			if err != nil {
				t.Fatal(err)
			}
			temp, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			sourceRoot := filepath.Join(temp, "source-package")
			if err := os.Mkdir(sourceRoot, 0700); err != nil {
				t.Fatal(err)
			}
			nativeRoot := filepath.Join(temp, "source-native")
			if err := os.Mkdir(nativeRoot, 0700); err != nil {
				t.Fatal(err)
			}
			_, mergeInventory := composedMergeSource(repository)
			provider := &composedCleanupProvider{repository: repository, mergeInventory: mergeInventory}
			_, _, parentPlanBytes, parentJournalBytes, parentResultBytes := prepareComposedParent(t, engine, sourceRoot, nativeRoot, provider)
			childPlan := prepareAllAbsentCleanup(t, engine, sourceRoot, provider, repository)
			if _, err := engine.MarkContextPlan(sourceRoot, "branch-cleanup", "dispatching"); err != nil {
				t.Fatal(err)
			}
			if journalState == "completed-empty-journal" {
				result, err := (apply.Engine{
					Root: nativeRoot, Repository: repository, Command: workflow.BranchCleanupCommand,
					Adapter: workflow.BranchCleanup{Provider: provider},
				}).Apply(context.Background(), childPlan)
				if err != nil || result["status"] != "completed" || provider.branchDeleteWrites != 0 {
					t.Fatalf("all-absent source apply did not complete without a branch write: %#v %v writes=%d", result, err, provider.branchDeleteWrites)
				}
				if _, err := engine.CaptureJournal(sourceRoot, filepath.Join(nativeRoot, ".artifacts", "gh-steward", "journals"), "branch-cleanup"); err != nil {
					t.Fatal(err)
				}
			} else if journalState == "open-empty-journal" {
				parsed, err := contract.ParsePlan(childPlan)
				if err != nil {
					t.Fatal(err)
				}
				journalID := mustPlanEntry(t, sourceRoot, "branch-cleanup")["journal_id"].(string)
				openJournal := Object{
					"identity": Object{"schema_version": int64(2), "repository": repository.Object(), "command": parsed.Command, "plan_sha256": parsed.SHA256},
					"steps":    []any{}, "result": nil,
				}
				if err := persistPackageJSON(sourceRoot, "journal/"+journalID+".json", openJournal); err != nil {
					t.Fatalf("persist exact interrupted empty cleanup journal: %v", err)
				}
			}
			sourcePayload := writePackageZip(t, sourceRoot)
			sourceArchivePath := filepath.Join(temp, "source.zip")
			if err := os.WriteFile(sourceArchivePath, sourcePayload, 0600); err != nil {
				t.Fatal(err)
			}
			sourceRun := recoveryHistoryRun(100, 1, "Merge On Green")
			currentRun := recoveryHistoryRun(101, 1, "Merge On Green Recovery")
			target, err := TargetObject("merge-on-green.yml", repository.URL, 9, "workflow-history-v2")
			if err != nil {
				t.Fatal(err)
			}
			sourceArtifactID := int64(77)
			sourceMetadata := Object{
				"id": sourceArtifactID, "name": RecoveryArtifactName(target, 100, 1), "expired": false,
				"digest": "sha256:" + SHA256(sourcePayload), "workflow_run": Object{"id": int64(100), "head_sha": sourceRun["head_sha"]},
			}
			observerTemp := filepath.Join(temp, "fresh-observer")
			if err := os.Mkdir(observerTemp, 0700); err != nil {
				t.Fatal(err)
			}
			input := Object{
				"repository": repository.Object(), "source_run": sourceRun, "current_run": currentRun,
				"source_artifact": sourceMetadata, "source_archive_path": sourceArchivePath,
				"scratch_root": observerTemp, "result_path": filepath.Join(temp, "lifecycle-result.json"),
			}
			inputBytes, err := Canonical(input)
			if err != nil {
				t.Fatal(err)
			}
			inputPath := filepath.Join(temp, "lifecycle-input.json")
			if err := os.WriteFile(inputPath, append(inputBytes, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			var report Object
			if journalState == "before-journal" {
				entries, err := os.ReadDir(observerTemp)
				if err != nil || len(entries) != 0 {
					t.Fatalf("fresh process scratch is not empty: entries=%v err=%v", entries, err)
				}
				command := exec.Command(os.Args[0], "-test.run=^TestCompletedMergeZeroOpCleanupRecoveryResumesWithoutWrites$")
				command.Env = append(os.Environ(), "GH_STEWARD_ZERO_OP_CLEANUP_INPUT="+inputPath)
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("fresh-process complete-history recovery failed: %v: %s", err, output)
				}
				reportValue, err := LoadJSON(input["result_path"].(string))
				if err != nil {
					t.Fatal(err)
				}
				report, err = object(reportValue, "complete-history zero-op lifecycle result")
				if err != nil {
					t.Fatal(err)
				}
			} else {
				report = runPersistedZeroOpCleanupLifecycle(t, input)
			}
			if report["recovered"].(Object)["outcome"] != "resumed" || report["finalized"].(Object)["outcome"] != "checkpoint" {
				t.Fatalf("complete-history recovery did not resume and finalize: %#v", report)
			}
			installOutcome := report["journal_install"].(Object)["outcome"]
			journalInstall := report["journal_install"].(Object)
			if journalState == "before-journal" && installOutcome != "no-journal-required" {
				t.Fatalf("recovery did not preserve the exact %s journal state: %#v", journalState, report["journal_install"])
			}
			if journalState != "before-journal" && (!nativeNonemptyValue(journalInstall["path"]) ||
				journalInstall["journal_id"] != mustPlanEntry(t, filepath.Join(observerTemp, "package"), "branch-cleanup")["journal_id"] ||
				!IsSHA256(fmt.Sprint(journalInstall["sha256"]))) {
				t.Fatalf("recovery did not install the exact %s journal: %#v", journalState, journalInstall)
			}
			observerRoot := filepath.Join(observerTemp, "package")
			if fmt.Sprint(report["merge_writes"]) != "0" || fmt.Sprint(report["branch_delete_writes"]) != "0" {
				t.Fatalf("recovered all-absent cleanup dispatched a provider write: %#v", report)
			}
			if journalState == "before-journal" {
				target, err := object(report["target"], "terminal proof target")
				if err != nil {
					t.Fatal(err)
				}
				proof, err := object(report["proof"], "terminal proof")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ValidateRecoveredTerminalProof(proof, target); err != nil {
					t.Fatalf("completed exact zero-op continuation lacks a recovered terminal proof: %v", err)
				}
				assertRecoveredZeroOpProofRejectsWeakContinuations(t, proof, target)
				assertRecoveredNonzeroCleanupWithoutJournalRejected(t, proof, target)
			}
			for relative, want := range map[string][]byte{
				"plans/merge.json": parentPlanBytes,
				"journal/" + mustPlanEntry(t, observerRoot, "merge")["journal_id"].(string) + ".json": parentJournalBytes,
				"apply-results/merge.json": parentResultBytes,
			} {
				got, err := ReadPackageFile(observerRoot, relative)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("recovery changed completed parent bytes at %s: %v", relative, err)
				}
			}
		})
	}
}

func assertRecoveredZeroOpProofRejectsWeakContinuations(t *testing.T, proof, target Object) {
	t.Helper()
	mutations := map[string]func(Object){
		"missing parent receipt": func(candidate Object) {
			source := candidate["source"].(Object)
			source["plans"].([]any)[0].(Object)["journal_file"] = nil
		},
		"source child proof reordered": func(candidate Object) {
			plans := candidate["source"].(Object)["plans"].([]any)
			plans[0], plans[1] = plans[1], plans[0]
		},
		"source child apply result injected": func(candidate Object) {
			sourcePlans := candidate["source"].(Object)["plans"].([]any)
			observerPlans := candidate["observer"].(Object)["plans"].([]any)
			sourcePlans[1].(Object)["apply_result_file"] = observerPlans[1].(Object)["apply_result_file"]
		},
		"unstarted child": func(candidate Object) {
			mutateRecoveredProofContext(t, candidate["source"].(Object), func(value Object) {
				value["plans"].([]any)[1].(Object)["status"] = "prepared"
			})
		},
		"recovery target drift": func(candidate Object) {
			wrongTarget := Object{"plan_sha256": strings.Repeat("f", 64)}
			for _, attemptName := range []string{"source", "observer"} {
				mutateRecoveredProofContext(t, candidate[attemptName].(Object), func(value Object) {
					value["attempt_target"] = wrongTarget
				})
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate, err := cloneObject(proof)
			if err != nil {
				t.Fatal(err)
			}
			mutate(candidate)
			if _, err := ValidateRecoveredTerminalProof(candidate, target); err == nil {
				t.Fatal("weak or drifted zero-operation recovery proof was accepted")
			}
		})
	}
}

func assertRecoveredNonzeroCleanupWithoutJournalRejected(t *testing.T, proof, target Object) {
	t.Helper()
	candidate, err := cloneObject(proof)
	if err != nil {
		t.Fatal(err)
	}
	source := candidate["source"].(Object)
	observer := candidate["observer"].(Object)
	sourcePlans := source["plans"].([]any)
	observerPlans := observer["plans"].([]any)
	basePlan, err := LoadFileProof(observerPlans[1].(Object)["plan_file"], "all-absent cleanup plan")
	if err != nil {
		t.Fatal(err)
	}
	plan := makeNonzeroCleanupPlan(t, basePlan.(Object))
	_, terminalProof := makeNativeTerminalProofFromPlan(t, plan, nativeAckAcknowledged)
	terminalProof["name"] = "branch-cleanup"

	childSourceProof := sourcePlans[1].(Object)
	for _, field := range []string{"plan_sha256", "journal_id", "plan_file"} {
		childSourceProof[field] = terminalProof[field]
	}
	childSourceProof["journal_file"] = nil
	delete(childSourceProof, "apply_result_file")
	observerPlans[1] = terminalProof
	for _, attempt := range []Object{source, observer} {
		mutateRecoveredProofContext(t, attempt, func(value Object) {
			childContext := value["plans"].([]any)[1].(Object)
			childContext["sha256"] = terminalProof["plan_sha256"]
			childContext["journal_id"] = terminalProof["journal_id"]
		})
	}
	if _, err := ValidateRecoveredTerminalProof(candidate, target); err == nil || !strings.Contains(err.Error(), "nonzero cleanup source plan lost its durable native journal") {
		t.Fatalf("exact completed merge plus nonzero cleanup child without its source journal was not held at the journal gate: %v", err)
	}
}

func makeNonzeroCleanupPlan(t *testing.T, base Object) contract.Plan {
	t.Helper()
	parsed := mustParsedPlan(t, base)
	data, err := contract.Clone(parsed.Data)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := contract.ObjectAt(data, "inventory")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := contract.Objects(inventory, "branches")
	if err != nil || len(rows) != 1 {
		t.Fatalf("all-absent base plan does not contain one selected branch: rows=%v err=%v", rows, err)
	}
	selection, err := contract.ObjectAt(rows[0], "selection")
	if err != nil {
		t.Fatal(err)
	}
	name, sha := selection["name"].(string), selection["sha"].(string)
	rows[0]["branch"] = Object{
		"repo": parsed.Repository.Object(), "repository_node_id": inventory["repository_node_id"],
		"id": "BR_recovered_nonzero", "name": name, "sha": sha,
	}
	evidence, err := contract.ObjectAt(inventory, "branch_evidence")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := contract.ObjectAt(evidence, "inventory")
	if err != nil {
		t.Fatal(err)
	}
	collection["branches"] = []any{
		Object{"name": name, "sha": sha},
		Object{"name": inventory["default_branch"], "sha": strings.Repeat("c", 40)},
	}
	delete(data, "already_absent")
	provisional := contract.Plan{
		SchemaVersion: contract.MachineSchemaVersion, Command: parsed.Command, Repository: parsed.Repository,
		CapturedAt: parsed.CapturedAt, Sources: parsed.Sources, Data: data,
	}
	operations, err := (workflow.BranchCleanup{}).Operations(provisional)
	if err != nil || len(operations) != 1 {
		t.Fatalf("recompute one exact present-branch cleanup operation: operations=%v err=%v", operations, err)
	}
	captured, err := time.Parse(time.RFC3339Nano, parsed.CapturedAt)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contract.PreparePlan(parsed.Command, parsed.Repository, parsed.Sources, data, operations, captured)
	if err != nil {
		t.Fatalf("prepare exact nonzero cleanup proof plan: %v", err)
	}
	return plan
}

func mutateRecoveredProofContext(t *testing.T, attempt Object, mutate func(Object)) {
	t.Helper()
	value, err := LoadFileProof(attempt["context_file"], "recovered proof context")
	if err != nil {
		t.Fatal(err)
	}
	context, err := object(value, "recovered proof context")
	if err != nil {
		t.Fatal(err)
	}
	mutate(context)
	encoded, err := Canonical(context)
	if err != nil {
		t.Fatal(err)
	}
	attempt["context_file"] = MakeFileProof(encoded)
}

func runPersistedZeroOpCleanupLifecycle(t *testing.T, input Object) Object {
	t.Helper()
	repositoryObject, err := object(input["repository"], "persisted repository")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := contract.ParseRepository(repositoryObject)
	if err != nil {
		t.Fatal(err)
	}
	sourceRun, err := object(input["source_run"], "persisted source run")
	if err != nil {
		t.Fatal(err)
	}
	currentRun, err := object(input["current_run"], "persisted current run")
	if err != nil {
		t.Fatal(err)
	}
	sourceArtifact, err := object(input["source_artifact"], "persisted source artifact")
	if err != nil {
		t.Fatal(err)
	}
	sourcePayload, err := os.ReadFile(input["source_archive_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	scratchRoot := input["scratch_root"].(string)
	packageRoot := filepath.Join(scratchRoot, "package")
	invocation := Invocation{
		Workflow: "merge-on-green.yml", RunID: mustPositive(currentRun["id"]), Attempt: mustPositive(currentRun["run_attempt"]),
		RunName: currentRun["display_title"].(string), RecoveryKey: "merge-ci-8-pr-3",
		PackageRoot: packageRoot, RunnerTemp: scratchRoot,
	}
	engine, err := NewEngine(completedMergePolicy(), repository)
	if err != nil {
		t.Fatal(err)
	}
	workflowRunsEndpoint := "repos/" + repository.FullName() + "/actions/workflows/merge-on-green.yml/runs?per_page=100"
	artifactsEndpoint := "repos/" + repository.FullName() + "/actions/artifacts?per_page=100"
	sourceID := mustPositive(sourceArtifact["id"])
	reader := &recoveryReaderFixture{
		reads: map[string]Object{
			"repos/" + repository.FullName() + "/actions/runs/" + fmt.Sprint(sourceRun["id"]) + "/attempts/" + fmt.Sprint(sourceRun["run_attempt"]): sourceRun,
		},
		pages: map[string][]any{
			workflowRunsEndpoint: {Object{"total_count": int64(2), "workflow_runs": []any{sourceRun, currentRun}}},
			artifactsEndpoint:    {Object{"total_count": int64(1), "artifacts": []any{sourceArtifact}}},
		},
		archives: map[int64][]byte{sourceID: sourcePayload},
	}
	recovered, err := engine.Recover(context.Background(), reader, invocation)
	if err != nil || recovered["outcome"] != "resumed" {
		t.Fatalf("complete-history recovery did not restore the exact zero-op cleanup: %#v %v", recovered, err)
	}
	observer, err := engine.ObserveSourceContext(packageRoot, Object{
		"current_run_id": invocation.RunID, "current_attempt": invocation.Attempt, "current_run_name": invocation.RunName,
		"workflow_file": invocation.Workflow, "repository": repository.FullName(), "trusted_source_sha": strings.Repeat("c", 40),
	})
	if err != nil || observer["phase"] != "dispatching" {
		t.Fatalf("fresh source observer rejected the exact zero-op continuation: %#v %v", observer, err)
	}
	workflowPolicy, err := engine.workflowPolicy(invocation.Workflow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.completedMergeParentOnlyContext(observer, "branch-cleanup", workflow.BranchCleanupCommand, workflowPolicy); err == nil {
		t.Fatal("recovered context was allowed to create a replacement cleanup child")
	}
	nativeRoot := filepath.Join(scratchRoot, "native")
	if err := os.Mkdir(nativeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	nativeJournalRoot := filepath.Join(nativeRoot, ".artifacts", "gh-steward", "journals")
	if err := os.MkdirAll(nativeJournalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	installed, err := engine.InstallRestoredJournal(packageRoot, nativeJournalRoot, "branch-cleanup")
	if err != nil {
		t.Fatalf("fresh source could not install its exact journal state: %#v %v", installed, err)
	}
	cleanupEntry, err := contextPlanByName(observer, "branch-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	cleanupPlan, err := engine.loadContextPlan(packageRoot, observer, cleanupEntry)
	if err != nil {
		t.Fatal(err)
	}
	cleanupData := cleanupPlan["data"].(Object)
	cleanupInventory, err := object(cleanupData["inventory"], "reviewed cleanup inventory")
	if err != nil {
		t.Fatal(err)
	}
	_, mergeInventory := composedMergeSource(repository)
	provider := &composedCleanupProvider{
		repository: repository, mergeInventory: mergeInventory,
		cleanupInventory: cleanupInventory,
	}
	applyResult, err := (apply.Engine{
		Root: nativeRoot, Repository: repository, Command: workflow.BranchCleanupCommand,
		Adapter: workflow.BranchCleanup{Provider: provider},
	}).Apply(context.Background(), cleanupPlan)
	if err != nil || applyResult["status"] != "completed" || provider.branchDeleteWrites != 0 {
		t.Fatalf("recovered all-absent cleanup did not complete without writes: %#v %v writes=%d", applyResult, err, provider.branchDeleteWrites)
	}
	if _, err := engine.CaptureJournal(packageRoot, nativeJournalRoot, "branch-cleanup"); err != nil {
		t.Fatal(err)
	}
	if err := persistNativeApplyResult(packageRoot, "branch-cleanup", cleanupPlan, applyResult); err != nil {
		t.Fatal(err)
	}
	completedContext, err := engine.MarkContextPlan(packageRoot, "branch-cleanup", "completed")
	if err != nil || completedContext["phase"] != "completed" {
		t.Fatalf("zero-op cleanup lacks an exact terminal context: %#v %v", completedContext, err)
	}
	target, err := TargetObject(invocation.Workflow, repository.URL, 9, "workflow-history-v2")
	if err != nil {
		t.Fatal(err)
	}
	terminalPayload := writePackageZip(t, packageRoot)
	terminalArtifactID := int64(78)
	terminalMetadata := Object{
		"id": terminalArtifactID, "name": RecoveryArtifactName(target, invocation.RunID, invocation.Attempt), "expired": false,
		"digest": "sha256:" + SHA256(terminalPayload), "workflow_run": Object{"id": invocation.RunID, "head_sha": currentRun["head_sha"]},
	}
	reader.reads["repos/"+repository.FullName()+"/actions/artifacts/78"] = terminalMetadata
	reader.archives[terminalArtifactID] = terminalPayload
	finalized, err := engine.Finalize(context.Background(), reader, FinalizeOptions{
		Invocation: invocation, ArtifactID: terminalArtifactID, ArtifactDigest: terminalMetadata["digest"].(string),
		Checkpoint: filepath.Join(scratchRoot, "checkpoint"),
	})
	if err != nil || finalized["outcome"] != "checkpoint" {
		t.Fatalf("completed recovered cleanup did not finalize an exact checkpoint: %#v %v", finalized, err)
	}
	checkpointValue, err := LoadJSON(finalized["checkpoint_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	settlements, err := array(checkpointValue.(Object)["settlements"], "final checkpoint settlements")
	if err != nil || len(settlements) != 2 || !exactInt(settlements[0].(Object)["run_id"], 100) || !exactInt(settlements[1].(Object)["run_id"], 101) {
		t.Fatalf("checkpoint lost source and recovered observer settlements: %#v %v", settlements, err)
	}
	recoveredRecord, err := object(settlements[0], "recovered source checkpoint record")
	if err != nil {
		t.Fatal(err)
	}
	recoveredSettlement, err := object(recoveredRecord["settlement"], "recovered source settlement")
	if err != nil {
		t.Fatal(err)
	}
	recoveredProof, err := object(recoveredSettlement["proof"], "recovered source terminal proof")
	if err != nil {
		t.Fatal(err)
	}
	if provider.mergeWrites != 0 || provider.branchDeleteWrites != 0 {
		t.Fatalf("recovery dispatched a provider write: merge=%d branch-delete=%d", provider.mergeWrites, provider.branchDeleteWrites)
	}
	return Object{
		"recovered": recovered, "observed": observer, "journal_install": installed, "apply": applyResult, "finalized": finalized,
		"merge_writes": int64(provider.mergeWrites), "branch_delete_writes": int64(provider.branchDeleteWrites),
		"proof": recoveredProof, "target": target,
	}
}

func TestCompletedAllAbsentCleanupHasTerminalProofWithoutMutation(t *testing.T) {
	repository := policyTestRepository(t)
	engine, err := NewEngine(completedMergePolicy(), repository)
	if err != nil {
		t.Fatal(err)
	}
	temp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(temp, "package")
	nativeRoot := filepath.Join(temp, "native")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(nativeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	_, mergeInventory := composedMergeSource(repository)
	provider := &composedCleanupProvider{repository: repository, mergeInventory: mergeInventory}
	_, _, parentPlanBytes, parentJournalBytes, parentResultBytes := prepareComposedParent(t, engine, root, nativeRoot, provider)
	plan := prepareAllAbsentCleanup(t, engine, root, provider, repository)
	if _, err := engine.MarkContextPlan(root, "branch-cleanup", "dispatching"); err != nil {
		t.Fatal(err)
	}
	result, err := (apply.Engine{
		Root: nativeRoot, Repository: repository, Command: workflow.BranchCleanupCommand,
		Adapter: workflow.BranchCleanup{Provider: provider},
	}).Apply(context.Background(), plan)
	if err != nil || result["status"] != "completed" || provider.branchDeleteWrites != 0 {
		t.Fatalf("all-absent cleanup did not complete without dispatching a branch write: %#v %v writes=%d", result, err, provider.branchDeleteWrites)
	}
	if _, err := engine.CaptureJournal(root, filepath.Join(nativeRoot, ".artifacts", "gh-steward", "journals"), "branch-cleanup"); err != nil {
		t.Fatal(err)
	}
	if err := persistNativeApplyResult(root, "branch-cleanup", plan, result); err != nil {
		t.Fatal(err)
	}
	completed, err := engine.MarkContextPlan(root, "branch-cleanup", "completed")
	if err != nil || completed["phase"] != "completed" {
		t.Fatalf("completed zero-op cleanup lacks terminal context proof: %#v %v", completed, err)
	}
	entry, err := contextPlanByName(completed, "branch-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.terminalContextPlanProof(root, entry, plan); err != nil {
		t.Fatalf("completed zero-op cleanup terminal proof failed: %v", err)
	}
	if provider.mergeWrites != 1 || provider.branchDeleteWrites != 0 {
		t.Fatalf("zero-op completion replayed a provider mutation: merge=%d branch-delete=%d", provider.mergeWrites, provider.branchDeleteWrites)
	}
	for relative, want := range map[string][]byte{
		"plans/merge.json": parentPlanBytes,
		"journal/" + mustPlanEntry(t, root, "merge")["journal_id"].(string) + ".json": parentJournalBytes,
		"apply-results/merge.json": parentResultBytes,
	} {
		got, err := ReadPackageFile(root, relative)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("zero-op completion changed completed parent bytes at %s: %v", relative, err)
		}
	}
}

func TestAllAbsentCleanupRequiresRecomputedParentBoundEvidence(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(Object)
	}{
		{name: "forged absence digest", mutate: func(data Object) {
			data["already_absent"].([]any)[0].(Object)["branch_inventory_sha256"] = strings.Repeat("f", 64)
		}},
		{name: "incomplete branch inventory", mutate: func(data Object) {
			data["inventory"].(Object)["branch_evidence"].(Object)["inventory"].(Object)["branches"] = []any{}
		}},
		{name: "different pull request id", mutate: func(data Object) {
			data["inventory"].(Object)["branches"].([]any)[0].(Object)["pull_request"].(Object)["id"] = "PR_other"
			data["already_absent"].([]any)[0].(Object)["pull_request_id"] = "PR_other"
		}},
		{name: "different acknowledged merge commit", mutate: func(data Object) {
			data["inventory"].(Object)["branches"].([]any)[0].(Object)["pull_request"].(Object)["merge_commit_sha"] = strings.Repeat("e", 40)
		}},
		{name: "different repository incarnation", mutate: func(data Object) {
			inventory := data["inventory"].(Object)
			inventory["repository_node_id"] = "R_other"
			inventory["provenance"].(Object)["repository_node_id"] = "R_other"
			inventory["branches"].([]any)[0].(Object)["base_dependents"].(Object)["repository_node_id"] = "R_other"
			data["already_absent"].([]any)[0].(Object)["repository_node_id"] = "R_other"
			evidence := inventory["branch_evidence"].(Object)
			collection := evidence["inventory"].(Object)
			collection["provenance"].(Object)["repository_node_id"] = "R_other"
			digest, err := contract.Digest(collection)
			if err != nil {
				t.Fatal(err)
			}
			data["already_absent"].([]any)[0].(Object)["branch_inventory_sha256"] = digest
		}},
		{name: "retention setting drift", mutate: func(data Object) {
			data["inventory"].(Object)["branch_evidence"].(Object)["delete_branch_on_merge"] = false
			data["already_absent"].([]any)[0].(Object)["delete_branch_on_merge"] = false
		}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			repository := policyTestRepository(t)
			engine, err := NewEngine(completedMergePolicy(), repository)
			if err != nil {
				t.Fatal(err)
			}
			temp, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(temp, "source-package")
			nativeRoot := filepath.Join(temp, "source-native")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(nativeRoot, 0700); err != nil {
				t.Fatal(err)
			}
			_, mergeInventory := composedMergeSource(repository)
			provider := &composedCleanupProvider{repository: repository, mergeInventory: mergeInventory}
			prepareComposedParent(t, engine, root, nativeRoot, provider)
			base := prepareAllAbsentCleanupUnrecorded(t, repository, provider)
			candidate := rebuildCleanupPlan(t, base, testCase.mutate)
			contextValue, err := engine.readRunContext(root)
			if err != nil {
				t.Fatal(err)
			}
			parentEntry := contextValue["plans"].([]any)[0].(Object)
			entry := Object{
				"name": "branch-cleanup", "path": "plans/branch-cleanup.json", "command": workflow.BranchCleanupCommand,
				"sha256": candidate["sha256"], "journal_id": journalIDForPlan(candidate), "status": "prepared",
			}
			contextValue["phase"] = "prepared"
			contextValue["plans"] = []any{parentEntry, entry}
			if err := engine.validateRunContext(contextValue); err != nil {
				t.Fatalf("test context is not structurally exact: %v", err)
			}
			reader := &policyReader{root: root, used: map[string]bool{}}
			if err := engine.validateRecoveryPlanWithReader(contextValue, entry, candidate, reader); err == nil {
				t.Fatal("forged or drifted zero-op child passed completed-merge recovery policy")
			}
		})
	}
}

func prepareAllAbsentCleanupUnrecorded(t *testing.T, repository contract.Repository, provider *composedCleanupProvider) Object {
	t.Helper()
	selection, err := workflow.ParseBranchCleanupSelection(Object{"branches": []any{Object{
		"name": "codex/change", "sha": strings.Repeat("a", 40), "pull_request_number": int64(3),
	}}}, repository)
	if err != nil {
		t.Fatal(err)
	}
	inventory := composedBranchInventory(repository, selection, provider.mergeCommit)
	rows, _ := contract.Objects(inventory, "branches")
	rows[0]["branch"] = nil
	inventory["branch_evidence"] = Object{
		"inventory": Object{
			"repo":       repository.Object(),
			"branches":   []any{Object{"name": "main", "sha": strings.Repeat("c", 40)}},
			"provenance": Object{"live": true, "complete": true, "source": "github_api", "repository_node_id": "R_widgets"},
		},
		"delete_branch_on_merge": true,
	}
	provider.cleanupInventory = inventory
	planValue, err := workflow.PrepareBranchCleanup(context.Background(), provider, repository, selection, time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("prepare all-absent test plan: %v", err)
	}
	return planValue.Object()
}
