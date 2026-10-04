package runrecovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/governance"
	"github.com/coreycoto/gh-steward/internal/workflow"
)

func contextNoopEngine(t *testing.T) *Engine {
	t.Helper()
	engine, err := NewEngine(workflowNoopPolicy(), policyTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func startWorkflowNoopContext(t *testing.T, engine *Engine, root string, dispatchSteps []any) Object {
	t.Helper()
	context, err := engine.InitializeContext(root, workflowNoopContextInput(dispatchSteps))
	if err != nil {
		t.Fatal(err)
	}
	return context
}

func workflowNoopContextInput(dispatchSteps []any) Object {
	return Object{
		"workflow_file": "task.yml", "repository": "example/widgets", "recovery_key": "trigger-123",
		"run_name": "Task workflow", "run_id": int64(123), "attempt": int64(1),
		"attempt_target": Object{"trigger": "reviewed"}, "dispatch_steps": dispatchSteps,
	}
}

func TestWorkflowNoopAllowsOnlyPlanFreeEmptyDispatchSetup(t *testing.T) {
	engine := contextNoopEngine(t)
	root := t.TempDir()
	context := startWorkflowNoopContext(t, engine, root, []any{})
	if context["phase"] != "started" || !Equal(context["dispatch_steps"], []string{}) {
		t.Fatalf("initial no-op context has unexpected state: %#v", context)
	}

	prepared, err := engine.SetContextPhase(root, "prepared", "")
	if err != nil || prepared["phase"] != "prepared" {
		t.Fatalf("could not record the no-op finalizer precondition: %#v %v", prepared, err)
	}
	if _, err := engine.SetContextPhase(root, "noop", "finished without a plan"); err == nil || !strings.Contains(err.Error(), "exact completed workflow-noop plan") {
		t.Fatalf("phase-only no-op receipt was not held: %v", err)
	}
	if _, err := engine.RecordContextPlan(root, Object{
		"relative": "plans/workflow-noop.json", "name": "workflow-noop", "command": "workflow-noop",
		"repository": "example/widgets", "plan": Object{},
	}); err == nil || !strings.Contains(err.Error(), "dedicated no-op finalizer") {
		t.Fatalf("generic plan recorder accepted the no-op plan: %v", err)
	}

	ordinaryEngine, err := NewEngine(reviewedDispatchPolicy([]any{}), policyTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ordinaryEngine.InitializeContext(t.TempDir(), workflowNoopContextInput([]any{})); err == nil || !strings.Contains(err.Error(), "mutation-step inventory is empty") {
		t.Fatalf("ordinary workflow accepted an empty mutation-step inventory: %v", err)
	}
	withDispatchRoot := t.TempDir()
	if _, err := engine.InitializeContext(withDispatchRoot, workflowNoopContextInput([]any{"Apply reviewed plan"})); err != nil {
		t.Fatalf("no-op-enabled workflow rejected its ordinary mutation-step inventory: %v", err)
	}
	if _, err := engine.SetContextPhase(withDispatchRoot, "prepared", ""); err == nil || !strings.Contains(err.Error(), "prepared plan-free state") {
		t.Fatalf("no-op preparation accepted an ordinary mutation-step inventory: %v", err)
	}
}

func TestCompletedWorkflowNoopContextBindsOneExactPlanAndNoDispatchSteps(t *testing.T) {
	engine := contextNoopEngine(t)
	root := t.TempDir()
	context := startWorkflowNoopContext(t, engine, root, []any{})
	context["phase"] = "completed"
	context["plans"] = []any{Object{
		"name": "workflow-noop", "path": "plans/workflow-noop.json", "command": "workflow-noop",
		"sha256": strings.Repeat("a", 64), "journal_id": strings.Repeat("b", 64), "status": "completed",
	}}
	if err := engine.validateRunContext(context); err != nil {
		t.Fatalf("exact completed workflow-noop context rejected: %v", err)
	}

	for name, mutate := range map[string]func(Object){
		"mutation steps present": func(candidate Object) { candidate["dispatch_steps"] = []any{"Apply reviewed plan"} },
		"plan not completed":     func(candidate Object) { candidate["plans"].([]any)[0].(Object)["status"] = "prepared" },
		"plan renamed":           func(candidate Object) { candidate["plans"].([]any)[0].(Object)["name"] = "other" },
		"extra plan": func(candidate Object) {
			candidate["plans"] = append(candidate["plans"].([]any), Object{
				"name": "other", "path": "plans/other.json", "command": "batch-apply",
				"sha256": strings.Repeat("c", 64), "journal_id": strings.Repeat("d", 64), "status": "completed",
			})
		},
		"legacy noop phase": func(candidate Object) { candidate["phase"] = "noop" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate, err := cloneObject(context)
			if err != nil {
				t.Fatal(err)
			}
			mutate(candidate)
			if err := engine.validateRunContext(candidate); err == nil {
				t.Fatal("inexact workflow no-op context was accepted")
			}
		})
	}
}

func TestPublicationObserverRemainsPreparedAndReadOnly(t *testing.T) {
	proof, target, sourceContext := makePublicationFixture(t, "pr-verify-pending")
	if _, err := ValidatePublicationProof(proof, target, sourceContext, false); err != nil {
		t.Fatalf("publication source fixture is invalid: %v", err)
	}
	sourceRun := cloneNativeObject(proof["origin_run"].(Object))
	sourceContext["schema_version"] = int64(1)
	if sourceContext["phase"] != "prepared" || sourceContext["publication"].(Object)["stage"] != "pr-verify-pending" {
		t.Fatal("fixture does not represent a read-only publication observer source")
	}
	contextBytes, err := Canonical(sourceContext)
	if err != nil {
		t.Fatal(err)
	}
	artifact := Object{
		"name": RecoveryArtifactName(target, 100, 1), "id": int64(900), "digest": "sha256:" + strings.Repeat("f", 64),
	}
	root := t.TempDir()
	source := Object{
		"run_id": int64(100), "attempt": int64(1), "run": sourceRun, "artifact": artifact,
		"context_file": MakeFileProof(contextBytes), "plans": []any{}, "policy_files": Object{}, "publication": proof,
	}
	if err := persistPackageJSON(root, "recovery-source.json", source); err != nil {
		t.Fatal(err)
	}
	policy := Object{"schema_version": 1, "workflows": Object{"automation.yml": Object{
		"mutator_step_alternatives": []any{[]any{"Publish candidate"}}, "reviewed_source_shas": []any{},
		"allow_publication": true, "plans": Object{},
	}}}
	repository, err := contract.ParseRepository(Object{
		"owner": "sample", "name": "repo", "url": "https://github.com/sample/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(policy, repository)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := engine.ObserveContext(root, Object{
		"current_run_id": int64(201), "current_attempt": int64(1), "current_run_name": "publication recovery observer",
		"workflow_file": sourceContext["workflow_file"], "repository": sourceContext["repository"],
		"recovery_key": sourceContext["recovery_key"], "attempt_target": sourceContext["attempt_target"],
		"dispatch_steps": sourceContext["dispatch_steps"],
	})
	if err != nil {
		t.Fatalf("read-only publication observer failed: %v", err)
	}
	if observed["phase"] != "prepared" || observed["publication"].(Object)["stage"] != "pr-verify-pending" || len(observed["plans"].([]any)) != 0 {
		t.Fatalf("publication observer acquired dispatch state: %#v", observed)
	}
	if err := engine.validateRunContext(observed); err != nil {
		t.Fatalf("prepared publication observer context is invalid: %v", err)
	}
}

func TestCompletedMergeContextAllowsOnlyExactPreservedCleanupChild(t *testing.T) {
	engine, err := NewEngine(completedMergePolicy(), policyTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	parentSHA, childSHA := strings.Repeat("a", 64), strings.Repeat("d", 64)
	parent := Object{
		"name": "merge", "path": "plans/merge.json", "command": "merge-apply",
		"sha256": parentSHA, "journal_id": strings.Repeat("b", 64), "status": "completed",
		"review_path": "reviews/merge.json", "review_sha256": strings.Repeat("c", 64),
		"journal_path": "journal/merge.json", "apply_result_path": "apply-results/merge.json",
	}
	base := Object{
		"schema_version": int64(1), "workflow_file": "merge-on-green.yml", "repository": "example/widgets",
		"recovery_key": "merge-ci-8-pr-3", "run_name": "Merge On Green", "workflow_run_id": int64(99),
		"workflow_run_attempt": int64(1), "attempt_target": Object{"plan_sha256": parentSHA},
		"phase": "completed", "dispatch_steps": []any{"Apply exact merge", "Apply exact branch cleanup"},
		"plans": []any{parent},
	}
	workflowPolicy, _ := engine.workflowPolicy("merge-on-green.yml")
	if _, err := engine.completedMergeParentOnlyContext(base, "branch-cleanup", "branch-cleanup-apply", workflowPolicy); err != nil {
		t.Fatalf("exact completed merge parent was not eligible for its configured child: %v", err)
	}
	child := Object{
		"name": "branch-cleanup", "path": "plans/branch-cleanup.json", "command": "branch-cleanup-apply",
		"sha256": childSHA, "journal_id": strings.Repeat("e", 64), "status": "prepared",
		"review_path": "reviews/branch-cleanup.json", "review_sha256": strings.Repeat("f", 64),
	}
	composed, err := cloneObject(base)
	if err != nil {
		t.Fatal(err)
	}
	composed["phase"] = "prepared"
	composed["plans"] = []any{parent, child}
	if err := engine.validateRunContext(composed); err != nil {
		t.Fatalf("exact completed-merge cleanup continuation rejected: %v", err)
	}

	for name, mutate := range map[string]func(Object){
		"parent entry changed": func(candidate Object) {
			candidate["plans"].([]any)[0].(Object)["sha256"] = strings.Repeat("9", 64)
		},
		"parent no longer completed": func(candidate Object) {
			candidate["plans"].([]any)[0].(Object)["status"] = "dispatching"
		},
		"child wrong status for phase": func(candidate Object) {
			candidate["plans"].([]any)[1].(Object)["status"] = "completed"
		},
		"attempt target drifted": func(candidate Object) {
			candidate["attempt_target"] = Object{"plan_sha256": strings.Repeat("9", 64)}
		},
		"extra plan": func(candidate Object) {
			candidate["plans"] = append(candidate["plans"].([]any), Object{
				"name": "extra", "path": "plans/extra.json", "command": "batch-apply",
				"sha256": strings.Repeat("8", 64), "journal_id": strings.Repeat("7", 64), "status": "prepared",
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate, err := cloneObject(composed)
			if err != nil {
				t.Fatal(err)
			}
			mutate(candidate)
			if err := engine.validateRunContext(candidate); err == nil {
				t.Fatal("inexact composed cleanup context was accepted")
			}
		})
	}
	recovered, err := cloneObject(base)
	if err != nil {
		t.Fatal(err)
	}
	recovered["recovered_from_run_id"], recovered["recovered_from_attempt"] = int64(98), int64(1)
	if _, err := engine.completedMergeParentOnlyContext(recovered, "branch-cleanup", "branch-cleanup-apply", workflowPolicy); err == nil {
		t.Fatal("observer context was accepted as a new same-run cleanup parent")
	}
}

func TestContextPlanManifestPreservesOptionalEvidencePaths(t *testing.T) {
	entry := Object{
		"name": "merge", "path": "plans/merge.json", "command": "merge-apply",
		"sha256": strings.Repeat("a", 64), "journal_id": strings.Repeat("b", 64), "status": "completed",
		"review_path": "reviews/merge.json", "review_sha256": strings.Repeat("c", 64),
		"journal_path": "journal/merge.json", "apply_result_path": "apply-results/merge.json",
	}
	if _, err := exactWithOptional(entry, runContextPlanRequiredFields, runContextPlanOptionalFields, "context plan entry"); err != nil {
		t.Fatalf("optional recovery evidence fields were rejected: %v", err)
	}
}

func TestComposedCleanupRejectsMissingParentTerminalFilesBeforeWritingChild(t *testing.T) {
	engine, err := NewEngine(completedMergePolicy(), policyTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	parentSHA := strings.Repeat("a", 64)
	context := Object{
		"schema_version": int64(1), "workflow_file": "merge-on-green.yml", "repository": "example/widgets",
		"recovery_key": "merge-ci-8-pr-3", "run_name": "Merge On Green", "workflow_run_id": int64(99),
		"workflow_run_attempt": int64(1), "attempt_target": Object{"plan_sha256": parentSHA},
		"phase": "completed", "dispatch_steps": []any{"Apply exact merge", "Apply exact branch cleanup"},
		"plans": []any{Object{
			"name": "merge", "path": "plans/merge.json", "command": "merge-apply", "sha256": parentSHA,
			"journal_id": strings.Repeat("b", 64), "status": "completed",
			"review_path": "reviews/merge.json", "review_sha256": strings.Repeat("c", 64),
		}},
	}
	if err := persistPackageJSON(root, "run-context.json", context); err != nil {
		t.Fatal(err)
	}
	selection := Object{"branches": []any{Object{
		"name": "codex/change", "sha": strings.Repeat("a", 40), "pull_request_number": int64(3),
	}}}
	planValue, err := contract.PreparePlan("branch-cleanup-apply", policyTestRepository(t),
		Object{"source": Object{"source": "github_api", "live": true, "complete": true}},
		Object{"selection": selection}, []contract.Operation{}, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.RecordContextPlan(root, Object{
		"relative": "plans/branch-cleanup.json", "name": "branch-cleanup", "command": "branch-cleanup-apply",
		"repository": "example/widgets", "plan": planValue.Object(),
	})
	if err == nil {
		t.Fatal("cleanup continuation without parent merge proof was accepted")
	}
	if _, statErr := os.Lstat(filepath.Join(root, "plans", "branch-cleanup.json")); !os.IsNotExist(statErr) {
		t.Fatalf("failed parent proof left a child plan file behind: %v", statErr)
	}
}

type composedCleanupProvider struct {
	repository           contract.Repository
	mergeInventory       Object
	cleanupInventory     Object
	branchDeleteWrites   int
	mergeWrites          int
	unknownWithoutAck    bool
	failAfterDelete      bool
	failedPostDeleteRead bool
	mergeCommit          string
}

func composedMergeSource(repository contract.Repository) (Object, Object) {
	head := strings.Repeat("a", 40)
	event := Object{"workflow_run": Object{
		"id": int64(8), "run_attempt": int64(1), "workflow_id": int64(88), "name": "CI", "event": "pull_request",
		"status": "completed", "conclusion": "success", "head_sha": head, "head_branch": "codex/change",
		"repository":    Object{"full_name": repository.FullName()},
		"pull_requests": []any{Object{"number": int64(3), "head": Object{"sha": head, "ref": "codex/change"}}},
	}}
	pullRequest := Object{
		"number": int64(3), "id": "PR_3", "url": repository.URL + "/pull/3", "repository": repository.FullName(),
		"title": "Reviewed change", "body": "Closes #1", "state": "OPEN", "isDraft": false, "merged": false,
		"baseRefName": "main", "headRefName": "codex/change", "headRefOid": head, "mergeStateStatus": "CLEAN",
		"reviewDecision": "APPROVED", "merge_commit_sha": nil, "head_repository": repository.Object(),
		"author": Object{"login": "automation[bot]", "is_bot": true}, "labels": []any{Object{"name": "automerge"}},
	}
	inventory := Object{
		"repository": Object{"id": "R_widgets", "nameWithOwner": repository.FullName(), "defaultBranch": "main"},
		"event":      event, "pull_request": pullRequest,
		"required_checks": []any{Object{"name": "tests", "bucket": "pass", "state": "SUCCESS", "workflow": "CI", "link": repository.URL + "/actions/runs/8"}},
		"provenance":      Object{"live": true, "complete": true, "source": "github_api", "host": repository.Host},
	}
	return event, inventory
}

func composedMergePolicyData() Object {
	return Object{
		"workflow_name": "CI", "workflow_event": "pull_request", "required_label": "automerge",
		"pass_bucket": "pass", "merge_method": "squash", "branch_patterns": []any{"codex/*"}, "trusted_logins": []any{},
	}
}

func (p *composedCleanupProvider) MergeInventory(_ context.Context, _ contract.Object) (contract.Object, error) {
	return contract.Clone(p.mergeInventory)
}

func (p *composedCleanupProvider) MergePullRequest(_ context.Context, number int64, headSHA, method string) (contract.Object, error) {
	if number != 3 || headSHA != strings.Repeat("a", 40) || method != "squash" {
		return nil, errors.New("fixture merge differs from its exact reviewed target")
	}
	p.mergeWrites++
	pr, err := contract.ObjectAt(p.mergeInventory, "pull_request")
	if err != nil {
		return nil, err
	}
	p.mergeCommit = strings.Repeat("b", 40)
	pr["state"], pr["merged"], pr["mergeStateStatus"], pr["merge_commit_sha"] = "MERGED", true, "MERGED", p.mergeCommit
	return Object{"merged": true, "sha": p.mergeCommit, "message": "Merged"}, nil
}

func (p *composedCleanupProvider) BranchCleanupInventory(_ context.Context, _ contract.Object) (contract.Object, error) {
	if p.branchDeleteWrites > 0 && p.failAfterDelete && !p.failedPostDeleteRead {
		p.failedPostDeleteRead = true
		return nil, errors.New("independent branch after-state read unavailable")
	}
	return contract.Clone(p.cleanupInventory)
}

func (p *composedCleanupProvider) DeleteBranch(_ context.Context, nonce, name, sha, repositoryNodeID string) (contract.Object, error) {
	if nonce == "" || name != "codex/change" || sha != strings.Repeat("a", 40) || repositoryNodeID != "R_widgets" {
		return nil, errors.New("fixture deletion differs from its exact reviewed target")
	}
	rows, err := contract.Objects(p.cleanupInventory, "branches")
	if err != nil || len(rows) != 1 {
		return nil, errors.New("fixture has no exact cleanup branch")
	}
	branchRow, err := contract.ObjectAt(rows[0], "branch")
	if err != nil || branchRow["name"] != name || branchRow["sha"] != sha {
		return nil, errors.New("fixture branch changed before deletion")
	}
	rows[0]["branch"] = nil
	p.branchDeleteWrites++
	if p.unknownWithoutAck {
		return nil, errors.New("ambiguous deletion response before acknowledgement")
	}
	stdout := fmt.Sprintf("To %s.git\n-\t:refs/heads/%s\t[deleted]\nDone\n", p.repository.URL, name)
	return Object{
		"exit_code": int64(0), "stdout": stdout, "stderr": "", "remote_url": p.repository.URL + ".git",
		"ref": "refs/heads/" + name, "expected_sha": sha,
	}, nil
}

func composedBranchInventory(repository contract.Repository, selection Object, mergeCommit string) Object {
	selected := selection["branches"].([]any)[0].(Object)
	pullRequest := Object{
		"number": selected["pull_request_number"], "id": "PR_3", "url": repository.URL + "/pull/3",
		"repository": repository.FullName(), "title": "Reviewed change", "body": "Closes #1", "state": "MERGED",
		"isDraft": false, "merged": true, "baseRefName": "main", "headRefName": selected["name"],
		"headRefOid": selected["sha"], "mergeStateStatus": "MERGED", "reviewDecision": "APPROVED",
		"merge_commit_sha": mergeCommit, "head_repository": repository.Object(),
		"author": Object{"login": "automation[bot]", "is_bot": true}, "labels": []any{Object{"name": "automerge"}},
	}
	return Object{
		"repo": repository.Object(), "repository_node_id": "R_widgets", "default_branch": "main",
		"branches": []any{Object{
			"selection":    selected,
			"branch":       Object{"repo": repository.Object(), "repository_node_id": "R_widgets", "id": "REF_codex_change", "name": selected["name"], "sha": selected["sha"]},
			"pull_request": pullRequest,
			"base_dependents": Object{
				"repo": repository.Object(), "repository_node_id": "R_widgets", "base_branch": selected["name"],
				"pull_requests": []any{}, "provenance": Object{"live": true, "complete": true, "source": "github_api"},
			},
		}},
		"provenance": Object{"live": true, "complete": true, "source": "github_api", "repository_node_id": "R_widgets", "selection": selection},
	}
}

func prepareComposedParent(t *testing.T, engine *Engine, root, nativeRoot string, provider *composedCleanupProvider) (Object, Object, []byte, []byte, []byte) {
	t.Helper()
	repository := policyTestRepository(t)
	event := provider.mergeInventory["event"].(Object)
	gate, err := governance.MergeEligibility(composedMergePolicyData(), provider.mergeInventory)
	if err != nil || gate["status"] != "eligible" || !exactInt(gate["pr_number"], 3) {
		t.Fatalf("fixture merge did not pass the actual deterministic eligibility gate: %#v %v", gate, err)
	}
	planValue, err := workflow.PrepareMerge(context.Background(), provider, repository, event, composedMergePolicyData(), time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("prepare actual merge plan: %v", err)
	}
	plan := planValue.Object()
	contextValue, err := engine.InitializeContext(root, Object{
		"workflow_file": "merge-on-green.yml", "repository": repository.FullName(), "recovery_key": "merge-ci-8-pr-3",
		"run_name": "Merge On Green", "run_id": int64(100), "attempt": int64(1),
		"attempt_target": Object{"plan_sha256": plan["sha256"]}, "dispatch_steps": []any{"Prepare merge", "Apply merge"},
	})
	if err != nil {
		t.Fatalf("initialize parent merge context: %v", err)
	}
	_ = contextValue
	planEvent, err := object(plan["data"].(Object)["event"], "normalized merge event")
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPackageJSON(root, "events/workflow-run-event.json", planEvent); err != nil {
		t.Fatalf("save exact merge workflow event: %v", err)
	}
	review := Object{
		"status": "approved", "approved_plan_sha": plan["sha256"], "pr_number": int64(3),
		"merge_method": "squash", "blocking_reasons": []any{},
	}
	reviewBytes, err := Canonical(review)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPackageFile(root, "reviews/merge.json", reviewBytes); err != nil {
		t.Fatalf("save exact merge approval: %v", err)
	}
	settings := Object{
		"schema_version": int64(1), "repository": repository.Object(), "repository_node_id": "R_widgets",
		"owner_login": repository.Owner, "owner_type": "User", "name": repository.Name,
		"default_branch": "main", "delete_branch_on_merge": true,
	}
	if err := persistPackageJSON(root, "previews/repository-settings-before.json", settings); err != nil {
		t.Fatalf("save immutable repository settings: %v", err)
	}
	_, err = engine.RecordContextPlan(root, Object{
		"relative": "plans/merge.json", "name": "merge", "command": workflow.MergeCommand,
		"repository": repository.FullName(), "plan": plan, "review_path": "reviews/merge.json", "review_sha256": SHA256(reviewBytes),
	})
	if err != nil {
		t.Fatalf("real parent merge plan failed workflow policy validation: %v", err)
	}
	if _, err := engine.MarkContextPlan(root, "merge", "dispatching"); err != nil {
		t.Fatalf("mark parent merge dispatching: %v", err)
	}
	result, err := (apply.Engine{
		Root: nativeRoot, Repository: repository, Command: workflow.MergeCommand,
		Adapter: workflow.MergeAdapter{Provider: provider},
	}).Apply(context.Background(), plan)
	if err != nil || result["status"] != "completed" {
		t.Fatalf("real parent merge did not complete: %#v %v", result, err)
	}
	parentJournalDir := filepath.Join(nativeRoot, ".artifacts", "gh-steward", "journals")
	if _, err := engine.CaptureJournal(root, parentJournalDir, "merge"); err != nil {
		t.Fatalf("parent merge journal was not captured: %v", err)
	}
	if err := persistNativeApplyResult(root, "merge", plan, result); err != nil {
		t.Fatalf("save native parent apply result: %v", err)
	}
	completedContext, err := engine.MarkContextPlan(root, "merge", "completed")
	if err != nil {
		t.Fatalf("actual native merge terminal proof was not accepted: %v", err)
	}
	parentEntry, err := contextPlanByName(completedContext, "merge")
	if err != nil {
		t.Fatal(err)
	}
	planBytes, err := ReadPackageFile(root, "plans/merge.json")
	if err != nil {
		t.Fatalf("read retained parent plan: %v", err)
	}
	journalBytes, err := ReadPackageFile(root, fmt.Sprintf("journal/%v.json", parentEntry["journal_id"]))
	if err != nil {
		t.Fatalf("read retained parent journal: %v", err)
	}
	resultBytes, err := ReadPackageFile(root, "apply-results/merge.json")
	if err != nil {
		t.Fatalf("read retained parent apply result: %v", err)
	}
	return plan, parentEntry, planBytes, journalBytes, resultBytes
}

func persistNativeApplyResult(root, name string, plan, result Object) error {
	parsed, err := contract.ParsePlan(plan)
	if err != nil {
		return err
	}
	return persistPackageJSON(root, "apply-results/"+name+".json", Object{
		"schema_version": int64(2), "tool_version": "0.1.0", "command": parsed.Command,
		"repository": parsed.Repository.Object(), "data": result,
	})
}

func prepareComposedCleanup(t *testing.T, engine *Engine, root string, provider *composedCleanupProvider, repository contract.Repository) Object {
	t.Helper()
	selectionInput := Object{"branches": []any{Object{
		"name": "codex/change", "sha": strings.Repeat("a", 40), "pull_request_number": int64(3),
	}}}
	selection, err := workflow.ParseBranchCleanupSelection(selectionInput, repository)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(selection, Object{"branches": []any{Object{"name": "codex/change", "sha": strings.Repeat("a", 40), "pull_request_number": int64(3)}}}) {
		t.Fatalf("real branch-cleanup selection parser changed the exact selected head: %#v", selection)
	}
	provider.cleanupInventory = composedBranchInventory(repository, selection, provider.mergeCommit)
	planValue, err := workflow.PrepareBranchCleanup(context.Background(), provider, repository, selection, time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	plan := planValue.Object()
	if _, err := engine.RecordContextPlan(root, Object{
		"relative": "plans/branch-cleanup.json", "name": "branch-cleanup", "command": workflow.BranchCleanupCommand,
		"repository": repository.FullName(), "plan": plan,
	}); err != nil {
		t.Fatalf("real branch-cleanup plan was not appended to its exact completed merge: %v", err)
	}
	return plan
}

func writePackageZip(t *testing.T, root string) []byte {
	t.Helper()
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
	return recoveryZip(t, entries...)
}

func TestComposedCleanupRecoveryRetainsCompletedMergeAndNeverReplaysUnknownDelete(t *testing.T) {
	for _, unknownWithoutAck := range []bool{false, true} {
		name := "acknowledged-delete-is-observed"
		if unknownWithoutAck {
			name = "unacknowledged-delete-remains-held"
		}
		t.Run(name, func(t *testing.T) {
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
			parentJournalRoot := filepath.Join(temp, "parent-journal")
			if err := os.Mkdir(parentJournalRoot, 0700); err != nil {
				t.Fatal(err)
			}
			_, mergeInventory := composedMergeSource(repository)
			provider := &composedCleanupProvider{repository: repository, mergeInventory: mergeInventory}
			parentPlan, parentEntry, parentPlanBytes, parentJournalBytes, parentResultBytes := prepareComposedParent(t, engine, sourceRoot, parentJournalRoot, provider)
			cleanupPlan := prepareComposedCleanup(t, engine, sourceRoot, provider, repository)
			if !Equal(cleanupPlan["data"].(Object)["selection"], Object{"branches": []any{Object{
				"name": "codex/change", "sha": strings.Repeat("a", 40), "pull_request_number": int64(3),
			}}}) {
				t.Fatalf("cleanup plan lost its exact branch selection: %#v", cleanupPlan["data"])
			}
			if _, err := engine.MarkContextPlan(sourceRoot, "branch-cleanup", "dispatching"); err != nil {
				t.Fatal(err)
			}
			provider.unknownWithoutAck = unknownWithoutAck
			provider.failAfterDelete = !unknownWithoutAck
			cleanupApplyRoot := filepath.Join(temp, "cleanup-apply")
			if err := os.Mkdir(cleanupApplyRoot, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := (apply.Engine{
				Root: cleanupApplyRoot, Repository: repository, Command: workflow.BranchCleanupCommand,
				Adapter: workflow.BranchCleanup{Provider: provider},
			}).Apply(context.Background(), cleanupPlan); err == nil {
				t.Fatal("the intentionally interrupted cleanup was reported complete")
			}
			if provider.branchDeleteWrites != 1 {
				t.Fatalf("cleanup did not make exactly one original write: %d", provider.branchDeleteWrites)
			}
			cleanupJournalRoot := filepath.Join(cleanupApplyRoot, ".artifacts", "gh-steward", "journals")
			if _, err := engine.CaptureJournal(sourceRoot, cleanupJournalRoot, "branch-cleanup"); err != nil {
				t.Fatalf("partial cleanup journal could not be retained: %v", err)
			}
			sourceContext, err := engine.readRunContext(sourceRoot)
			if err != nil {
				t.Fatal(err)
			}
			if sourceContext["phase"] != "dispatching" || sourceContext["plans"].([]any)[0].(Object)["status"] != "completed" || sourceContext["plans"].([]any)[1].(Object)["status"] != "dispatching" {
				t.Fatalf("interrupted composed run has unexpected statuses: %#v", sourceContext)
			}
			if !Equal(parentPlan["sha256"], parentEntry["sha256"]) {
				t.Fatal("completed parent plan identity changed while preparing cleanup")
			}

			target, err := TargetObject("merge-on-green.yml", repository.URL, 9, "workflow-history-v2")
			if err != nil {
				t.Fatal(err)
			}
			chain, err := EmptyChain(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := persistPackageJSON(sourceRoot, "settlement-chain.json", chain); err != nil {
				t.Fatal(err)
			}
			sourceRun := recoveryHistoryRun(100, 1, "Merge On Green")
			currentRun := recoveryHistoryRun(101, 1, "Merge On Green Recovery")
			artifactID := int64(55)
			sourceArtifact := Object{"name": RecoveryArtifactName(target, 100, 1), "id": artifactID}
			plans, err := terminalPlanInputs(sourceRoot, sourceContext, false)
			if err != nil {
				t.Fatal(err)
			}
			builtSource, err := engine.BuildRecoverySourceRecord(Object{
				"target": target, "run_id": int64(100), "attempt": int64(1), "run": mustNormalizeRun(t, sourceRun),
				"artifact":     Object{"name": sourceArtifact["name"], "id": artifactID, "digest": "sha256:" + strings.Repeat("1", 64)},
				"context_path": filepath.Join(sourceRoot, "run-context.json"), "plans": plans,
			})
			if err != nil {
				t.Fatalf("real merge plus partial cleanup did not validate as recovery source: %v", err)
			}
			policyFiles, err := object(builtSource["policy_files"], "retained source policy files")
			if err != nil || policyFiles["apply-results/merge.json"] == nil {
				t.Fatalf("completed merge result was not retained before recovery: %#v %v", policyFiles, err)
			}
			sourcePayload := writePackageZip(t, sourceRoot)
			sourceMetadata := Object{
				"id": artifactID, "name": sourceArtifact["name"], "expired": false,
				"digest": "sha256:" + SHA256(sourcePayload), "workflow_run": Object{"id": int64(100), "head_sha": sourceRun["head_sha"]},
			}
			runsEndpoint := "repos/example/widgets/actions/workflows/merge-on-green.yml/runs?per_page=100"
			artifactsEndpoint := "repos/example/widgets/actions/artifacts?per_page=100"
			reader := &recoveryReaderFixture{
				reads: map[string]Object{"repos/example/widgets/actions/runs/100/attempts/1": sourceRun},
				pages: map[string][]any{
					runsEndpoint:      {Object{"total_count": int64(2), "workflow_runs": []any{sourceRun, currentRun}}},
					artifactsEndpoint: {Object{"total_count": int64(1), "artifacts": []any{sourceMetadata}}},
				},
				archives: map[int64][]byte{artifactID: sourcePayload},
			}
			observerTemp := filepath.Join(temp, "observer-temp")
			if err := os.Mkdir(observerTemp, 0700); err != nil {
				t.Fatal(err)
			}
			observerRoot := filepath.Join(observerTemp, "observer-package")
			invocation := Invocation{
				Workflow: "merge-on-green.yml", RunID: 101, Attempt: 1, RunName: "Merge On Green Recovery",
				RecoveryKey: "merge-ci-8-pr-3", PackageRoot: observerRoot, RunnerTemp: observerTemp,
			}
			recovered, err := engine.Recover(context.Background(), reader, invocation)
			if err != nil || recovered["outcome"] != "resumed" {
				t.Fatalf("complete-history recovery did not restore the exact interrupted cleanup: %#v %v", recovered, err)
			}
			for relative, want := range map[string][]byte{
				"plans/merge.json": parentPlanBytes, fmt.Sprintf("journal/%s.json", parentEntry["journal_id"]): parentJournalBytes,
				"apply-results/merge.json": parentResultBytes,
			} {
				got, err := ReadPackageFile(observerRoot, relative)
				if err != nil || !Equal(got, want) {
					t.Fatalf("recovery changed the completed parent evidence at %s: %v", relative, err)
				}
			}

			handoffTemp := filepath.Join(temp, "handoff-temp")
			if err := os.Mkdir(handoffTemp, 0700); err != nil {
				t.Fatal(err)
			}
			handoffRoot := filepath.Join(handoffTemp, "handoff-package")
			handoffPayload := writePackageZip(t, observerRoot)
			handoffArtifactID := int64(56)
			handoffName := RecoveryArtifactName(target, invocation.RunID, invocation.Attempt) + "-handoff-00"
			handoffMetadata := Object{
				"id": handoffArtifactID, "name": handoffName, "expired": false,
				"digest": "sha256:" + SHA256(handoffPayload), "workflow_run": Object{"id": int64(101), "head_sha": currentRun["head_sha"]},
			}
			reader.pages[artifactsEndpoint] = []any{Object{"total_count": int64(2), "artifacts": []any{sourceMetadata, handoffMetadata}}}
			reader.reads["repos/example/widgets/actions/artifacts/55"] = sourceMetadata
			reader.reads["repos/example/widgets/actions/artifacts/56"] = handoffMetadata
			reader.archives[handoffArtifactID] = handoffPayload
			acquired, err := engine.AcquireHandoff(context.Background(), reader, Invocation{
				Workflow: invocation.Workflow, RunID: invocation.RunID, Attempt: invocation.Attempt, RunName: invocation.RunName,
				RecoveryKey: invocation.RecoveryKey, PackageRoot: handoffRoot, RunnerTemp: handoffTemp,
			}, handoffArtifactID, handoffMetadata["digest"].(string))
			if err != nil || acquired["purpose"] != "apply" {
				t.Fatalf("fresh-process apply handoff rejected its exact uploaded recovery package: %#v %v", acquired, err)
			}
			immutableSource, err := ReadPackageFile(handoffRoot, "recovery-source.json")
			if err != nil {
				t.Fatal(err)
			}
			observerInput := Object{"current_run_id": invocation.RunID, "current_attempt": invocation.Attempt, "current_run_name": invocation.RunName,
				"workflow_file": invocation.Workflow, "repository": repository.FullName(), "trusted_source_sha": strings.Repeat("c", 40)}
			observedContext, err := engine.ObserveSourceContext(handoffRoot, observerInput)
			if err != nil || observedContext["recovery_key"] != invocation.RecoveryKey || !Equal(observedContext["attempt_target"], sourceContext["attempt_target"]) {
				t.Fatalf("source-derived observer changed the retained target: %#v %v", observedContext, err)
			}
			observerInput["workflow_file"] = "other.yml"
			if _, err := engine.ObserveSourceContext(handoffRoot, observerInput); err == nil {
				t.Fatal("source observer accepted a different workflow")
			}
			retainedSource, err := ReadPackageFile(handoffRoot, "recovery-source.json")
			if err != nil || !Equal(retainedSource, immutableSource) {
				t.Fatal("source observer changed its immutable proof", err)
			}
			restoredApplyRoot := filepath.Join(temp, "restored-apply")
			if err := os.Mkdir(restoredApplyRoot, 0700); err != nil {
				t.Fatal(err)
			}
			restoredJournalRoot := filepath.Join(restoredApplyRoot, ".artifacts", "gh-steward", "journals")
			if err := os.MkdirAll(restoredJournalRoot, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.InstallRestoredJournal(handoffRoot, restoredJournalRoot, "branch-cleanup"); err != nil {
				t.Fatalf("fresh process did not install the exact immutable cleanup journal: %v", err)
			}
			provider.unknownWithoutAck, provider.failAfterDelete = false, false
			resumed, err := (apply.Engine{
				Root: restoredApplyRoot, Repository: repository, Command: workflow.BranchCleanupCommand,
				Adapter: workflow.BranchCleanup{Provider: provider},
			}).Apply(context.Background(), cleanupPlan)
			if unknownWithoutAck {
				if err == nil || provider.branchDeleteWrites != 1 {
					t.Fatalf("cleanup without durable provider acknowledgement was replayed: %#v %v writes=%d", resumed, err, provider.branchDeleteWrites)
				}
			} else if err != nil || resumed["status"] != "completed" || provider.branchDeleteWrites != 1 {
				t.Fatalf("acknowledged cleanup was not positively observed without replay: %#v %v writes=%d", resumed, err, provider.branchDeleteWrites)
			}
			if !unknownWithoutAck {
				if _, err := engine.CaptureJournal(handoffRoot, restoredJournalRoot, "branch-cleanup"); err != nil {
					t.Fatalf("monotonic cleanup journal advance was not captured: %v", err)
				}
				if err := persistNativeApplyResult(handoffRoot, "branch-cleanup", cleanupPlan, resumed); err != nil {
					t.Fatal(err)
				}
				if _, err := engine.MarkContextPlan(handoffRoot, "branch-cleanup", "completed"); err != nil {
					t.Fatalf("recovered cleanup did not produce its exact terminal native proof: %v", err)
				}
				terminalPayload := writePackageZip(t, handoffRoot)
				terminalArtifactID := int64(57)
				terminalMetadata := Object{
					"id": terminalArtifactID, "name": RecoveryArtifactName(target, invocation.RunID, invocation.Attempt), "expired": false,
					"digest": "sha256:" + SHA256(terminalPayload), "workflow_run": Object{"id": invocation.RunID, "head_sha": currentRun["head_sha"]},
				}
				reader.reads["repos/example/widgets/actions/artifacts/57"] = terminalMetadata
				reader.archives[terminalArtifactID] = terminalPayload
				finalized, err := engine.Finalize(context.Background(), reader, FinalizeOptions{
					Invocation: Invocation{Workflow: invocation.Workflow, RunID: invocation.RunID, Attempt: invocation.Attempt,
						RunName: invocation.RunName, RecoveryKey: invocation.RecoveryKey, PackageRoot: handoffRoot, RunnerTemp: handoffTemp},
					ArtifactID: terminalArtifactID, ArtifactDigest: terminalMetadata["digest"].(string), Checkpoint: filepath.Join(handoffTemp, "checkpoint"),
				})
				if err != nil || finalized["outcome"] != "checkpoint" {
					t.Fatalf("actual uploaded recovered cleanup did not finalize: %#v %v", finalized, err)
				}
				checkpoint, err := LoadJSON(finalized["checkpoint_path"].(string))
				if err != nil {
					t.Fatal(err)
				}
				settlements := checkpoint.(Object)["settlements"].([]any)
				if len(settlements) != 2 || !exactInt(settlements[0].(Object)["run_id"], 100) || !exactInt(settlements[1].(Object)["run_id"], 101) {
					t.Fatalf("checkpoint lost original and observer settlement order: %#v", settlements)
				}
				if provider.mergeWrites != 1 || provider.branchDeleteWrites != 1 {
					t.Fatalf("finalization replayed a provider write: merge=%d delete=%d", provider.mergeWrites, provider.branchDeleteWrites)
				}
				checkpointPayload := writePackageZip(t, filepath.Dir(finalized["checkpoint_path"].(string)))
				checkpointMetadata := Object{
					"id": int64(58), "name": finalized["checkpoint_name"], "expired": false,
					"digest": "sha256:" + SHA256(checkpointPayload), "workflow_run": Object{"id": invocation.RunID, "head_sha": currentRun["head_sha"]},
				}
				nextRun := recoveryHistoryRun(102, 1, "Next invocation")
				reader.pages[runsEndpoint] = []any{Object{"total_count": int64(3), "workflow_runs": []any{sourceRun, currentRun, nextRun}}}
				reader.archives[58] = checkpointPayload
				for _, changedOwner := range []bool{false, true} {
					t.Run(map[bool]string{false: "checkpoint-reused-in-fresh-process", true: "checkpoint-source-mismatch-held"}[changedOwner], func(t *testing.T) {
						metadata, err := cloneObject(checkpointMetadata)
						if err != nil {
							t.Fatal(err)
						}
						if changedOwner {
							metadata["workflow_run"].(Object)["head_sha"] = strings.Repeat("d", 40)
						}
						reader.pages[artifactsEndpoint] = []any{Object{"total_count": int64(1), "artifacts": []any{metadata}}}
						reader.calls = nil
						nextTemp := t.TempDir()
						next, err := engine.Recover(context.Background(), reader, Invocation{
							Workflow: invocation.Workflow, RunID: 102, Attempt: 1, RunName: "Next invocation", RecoveryKey: invocation.RecoveryKey,
							PackageRoot: filepath.Join(nextTemp, "package"), RunnerTemp: nextTemp,
						})
						want := "fresh"
						if changedOwner {
							want = "recovery_needed"
						}
						if err != nil || next["outcome"] != want {
							t.Fatalf("checkpoint acquisition returned %#v %v, want %s", next, err, want)
						}
						for _, call := range reader.calls {
							if strings.Contains(call, "/attempts/") {
								t.Fatal("checkpoint reuse reprocessed a covered attempt", call)
							}
						}
					})
				}
				for relative, want := range map[string][]byte{
					"plans/merge.json": parentPlanBytes, fmt.Sprintf("journal/%s.json", parentEntry["journal_id"]): parentJournalBytes,
					"apply-results/merge.json": parentResultBytes,
				} {
					got, err := ReadPackageFile(handoffRoot, relative)
					if err != nil || !Equal(got, want) {
						t.Fatalf("cleanup recovery changed completed parent evidence at %s: %v", relative, err)
					}
				}
			}
		})
	}
}

func mustNormalizeRun(t *testing.T, value Object) Object {
	t.Helper()
	run, err := NormalizeRun(value)
	if err != nil {
		t.Fatal(err)
	}
	return run
}
