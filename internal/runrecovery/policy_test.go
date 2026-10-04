package runrecovery

import (
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func reviewedDispatchPolicy(sourceSHAs []any) Object {
	return Object{
		"schema_version": 1,
		"workflows": Object{
			"task.yml": Object{
				"mutator_step_alternatives": []any{[]any{"Prepare reviewed plan", "Apply reviewed plan"}},
				"reviewed_source_shas":      sourceSHAs,
				"allow_publication":         false,
				"plans": Object{
					"sync": Object{
						"command": "batch-apply", "domain_profile": "quarter",
						"allowed_operation_kinds": []any{"branch-delete"}, "attempt_target": "plan_set",
						"approval": Object{
							"kind": "reviewed-dispatch", "approval_input": "approved-plan-sha",
							"reviewed_run_input": "reviewed-plan-run-id",
							"required_inputs":    Object{"continue": true, "release-channel": "stable"},
						},
						"event":        Object{"kind": "workflow_dispatch", "path": "events/dispatch-event.json"},
						"parent_merge": nil,
					},
				},
			},
		},
	}
}

func workflowNoopPolicy() Object {
	return Object{
		"schema_version": 1,
		"workflows": Object{
			"task.yml": Object{
				"mutator_step_alternatives": []any{[]any{"Prepare reviewed plan", "Apply reviewed plan"}},
				"reviewed_source_shas":      []any{},
				"allow_publication":         false,
				"plans": Object{
					"workflow-noop": Object{
						"command": "workflow-noop", "domain_profile": "workflow-noop",
						"allowed_operation_kinds": []any{}, "attempt_target": "workflow_noop",
						"approval":     workflowNoopApproval(),
						"event":        Object{"kind": "workflow_noop", "path": "events/trigger-event.json"},
						"parent_merge": nil,
					},
				},
			},
		},
	}
}

func workflowNoopApproval() Object {
	return Object{
		"kind": "local-noop", "workflow_source_sha256": strings.Repeat("c", 64),
		"mutators": []any{Object{"job": "Mutation job", "steps": []any{"Apply reviewed plan"}}},
	}
}

func completedMergePolicy() Object {
	return Object{
		"schema_version": 1,
		"workflows": Object{
			"merge-on-green.yml": Object{
				"mutator_step_alternatives": []any{[]any{"Prepare merge", "Apply merge"}},
				"reviewed_source_shas":      []any{},
				"allow_publication":         false,
				"plans": Object{
					"merge": Object{
						"command": "merge-apply", "domain_profile": "merge",
						"allowed_operation_kinds": []any{"pull-request-merge"}, "attempt_target": "single_plan_sha256",
						"approval": Object{"kind": "git-slop-merge"},
						"event":    Object{"kind": "workflow_run", "path": "events/workflow-run-event.json"}, "parent_merge": nil,
					},
					"branch-cleanup": Object{
						"command": "branch-cleanup-apply", "domain_profile": "closeout",
						"allowed_operation_kinds": []any{"branch-delete"}, "attempt_target": "parent_merge",
						"approval": Object{"kind": "parent-merge"}, "event": nil,
						"parent_merge": Object{
							"kind": "completed_merge", "plan_name": "merge", "command": "merge-apply",
							"settings_path": "previews/repository-settings-before.json",
						},
					},
				},
			},
		},
	}
}

func policyTestRepository(t *testing.T) contract.Repository {
	t.Helper()
	repository, err := contract.ParseRepository(Object{
		"owner": "example", "name": "widgets", "url": "https://github.com/example/widgets",
	})
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func TestRecoveryPolicyIsStrictAndKeepsWorkflowStateLocal(t *testing.T) {
	engine, err := NewEngine(reviewedDispatchPolicy([]any{}), policyTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	steps, err := engine.MutatorStepAlternatives("task.yml")
	if err != nil || !Equal(steps, []any{[]any{"Prepare reviewed plan", "Apply reviewed plan"}}) {
		t.Fatalf("exact mutator sequence was lost: %#v %v", steps, err)
	}
	steps[0][0] = "mutated caller copy"
	stepsAgain, err := engine.MutatorStepAlternatives("task.yml")
	if err != nil || stepsAgain[0][0] != "Prepare reviewed plan" {
		t.Fatal("caller mutation changed per-engine recovery policy")
	}
	if engine.workflowAllowsPublication("task.yml") {
		t.Fatal("explicitly disabled publication policy became enabled")
	}

	for name, mutate := range map[string]func(Object){
		"unknown workflow field": func(policy Object) {
			policy["workflows"].(Object)["task.yml"].(Object)["exec"] = true
		},
		"unknown plan field": func(policy Object) {
			policy["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)["sync"].(Object)["selector"] = "all"
		},
		"missing explicit publication choice": func(policy Object) {
			delete(policy["workflows"].(Object)["task.yml"].(Object), "allow_publication")
		},
		"unrecognized domain profile": func(policy Object) {
			policy["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)["sync"].(Object)["domain_profile"] = "custom"
		},
		"malformed reviewed source": func(policy Object) {
			policy["workflows"].(Object)["task.yml"].(Object)["reviewed_source_shas"] = []any{"not-a-commit"}
		},
		"valid SHA without an authenticated workflow witness": func(policy Object) {
			policy["workflows"].(Object)["task.yml"].(Object)["reviewed_source_shas"] = []any{strings.Repeat("c", 40)}
		},
		"empty required input": func(policy Object) {
			policy["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)["sync"].(Object)["approval"].(Object)["required_inputs"] = Object{"continue": ""}
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate, err := contract.Clone(reviewedDispatchPolicy([]any{}))
			if err != nil {
				t.Fatal(err)
			}
			mutate(candidate)
			if _, err := NewEngine(candidate, policyTestRepository(t)); err == nil {
				t.Fatal("invalid declarative recovery policy was accepted")
			}
		})
	}
	if _, err := NewEngine(reviewedDispatchPolicy([]any{strings.Repeat("d", 40)}), policyTestRepository(t)); err == nil ||
		!strings.Contains(err.Error(), "authenticated executed-workflow source witness") {
		t.Fatalf("nonempty reviewed source allowlist was not held with an explicit reason: %v", err)
	}
}

func TestRecoveryPolicySupportsClosedCompletedMergeContract(t *testing.T) {
	engine, err := NewEngine(completedMergePolicy(), policyTestRepository(t))
	if err != nil {
		t.Fatalf("valid completed-merge policy rejected: %v", err)
	}
	workflowPolicy, err := engine.workflowPolicy("merge-on-green.yml")
	if err != nil {
		t.Fatal(err)
	}
	name, closeout, ok := completedMergeCloseout(workflowPolicy)
	if !ok || name != "branch-cleanup" || closeout.parentMerge == nil || closeout.parentMerge.kind != "completed_merge" {
		t.Fatalf("completed merge closeout was not retained as a strict policy: %#v", closeout)
	}

	for name, mutate := range map[string]func(Object){
		"unsupported settled merge continuation": func(candidate Object) {
			candidate["workflows"].(Object)["merge-on-green.yml"].(Object)["plans"].(Object)["branch-cleanup"].(Object)["parent_merge"] = Object{
				"kind": "settled_merge", "workflow_file": "merge-on-green.yml", "plan_name": "merge", "command": "merge-apply",
				"proof_path": "previews/manual-cleanup-parent.json", "chain_path": "settlement-chain.json",
			}
		},
		"settings path traversal": func(candidate Object) {
			candidate["workflows"].(Object)["merge-on-green.yml"].(Object)["plans"].(Object)["branch-cleanup"].(Object)["parent_merge"].(Object)["settings_path"] = "../settings.json"
		},
		"wrong parent command": func(candidate Object) {
			candidate["workflows"].(Object)["merge-on-green.yml"].(Object)["plans"].(Object)["branch-cleanup"].(Object)["parent_merge"].(Object)["command"] = "batch-apply"
		},
		"unknown parent field": func(candidate Object) {
			candidate["workflows"].(Object)["merge-on-green.yml"].(Object)["plans"].(Object)["branch-cleanup"].(Object)["parent_merge"].(Object)["fallback"] = true
		},
		"parent is not a configured merge plan": func(candidate Object) {
			candidate["workflows"].(Object)["merge-on-green.yml"].(Object)["plans"].(Object)["branch-cleanup"].(Object)["parent_merge"].(Object)["plan_name"] = "missing"
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate, err := cloneObject(completedMergePolicy())
			if err != nil {
				t.Fatal(err)
			}
			mutate(candidate)
			if _, err := NewEngine(candidate, policyTestRepository(t)); err == nil {
				t.Fatal("invalid completed-merge policy was accepted")
			}
		})
	}
}

func TestWorkflowNoopPolicyRequiresExactOperationFreeContract(t *testing.T) {
	engine, err := NewEngine(workflowNoopPolicy(), policyTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	policy := engine.workflows["task.yml"].plans["workflow-noop"]
	if policy.command != "workflow-noop" || policy.profile != "workflow-noop" || policy.attemptTarget != "workflow_noop" ||
		policy.approval.kind != "local-noop" || policy.approval.noop == nil || policy.approval.noop.sourceSHA256 != strings.Repeat("c", 64) ||
		policy.event == nil || policy.event.kind != "workflow_noop" ||
		policy.event.path != "events/trigger-event.json" || policy.parentMerge != nil || len(policy.allowedOps) != 0 {
		t.Fatalf("workflow no-op contract was not parsed exactly: %#v", policy)
	}

	for name, mutate := range map[string]func(Object){
		"operation allowed": func(policy Object) {
			policy["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)["workflow-noop"].(Object)["allowed_operation_kinds"] = []any{"branch-push"}
		},
		"different attempt target": func(policy Object) {
			policy["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)["workflow-noop"].(Object)["attempt_target"] = "single_plan_sha256"
		},
		"different approval": func(policy Object) {
			policy["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)["workflow-noop"].(Object)["approval"] = Object{"kind": "git-slop-merge"}
		},
		"malformed workflow source hash": func(policy Object) {
			policy["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)["workflow-noop"].(Object)["approval"].(Object)["workflow_source_sha256"] = "not-a-hash"
		},
		"empty mutator steps": func(policy Object) {
			policy["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)["workflow-noop"].(Object)["approval"].(Object)["mutators"] = []any{Object{"job": "Mutation job", "steps": []any{}}}
		},
		"different event path": func(policy Object) {
			policy["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)["workflow-noop"].(Object)["event"].(Object)["path"] = "events/dispatch-event.json"
		},
		"wrong plan name": func(policy Object) {
			plans := policy["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)
			plans["other"] = plans["workflow-noop"]
			delete(plans, "workflow-noop")
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate, err := contract.Clone(workflowNoopPolicy())
			if err != nil {
				t.Fatal(err)
			}
			mutate(candidate)
			if _, err := NewEngine(candidate, policyTestRepository(t)); err == nil {
				t.Fatal("inconsistent workflow no-op policy was accepted")
			}
		})
	}
}

func TestReviewedDispatchRequiresExactEventReviewAndAllRequiredInputs(t *testing.T) {
	engine, err := NewEngine(reviewedDispatchPolicy([]any{}), policyTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	planPolicy := engine.workflows["task.yml"].plans["sync"]
	planDigest := strings.Repeat("b", 64)
	reviewedRun := int64(77)
	event := Object{
		"repository": Object{"full_name": "example/widgets"},
		"inputs": Object{
			"approved-plan-sha": planDigest, "reviewed-plan-run-id": "77",
			"continue": true, "release-channel": "stable",
		},
	}
	eventBytes, err := Canonical(event)
	if err != nil {
		t.Fatal(err)
	}
	workflowEventSHA := SHA256(eventBytes)
	review := Object{
		"schema_version": 1, "workflow_file": "task.yml", "repository": "example/widgets",
		"plan_name": "sync", "command": "batch-apply", "plan_sha256": planDigest,
		"approval_input": "approved-plan-sha", "workflow_event": "workflow_dispatch",
		"workflow_run_id": int64(90), "workflow_run_attempt": int64(1), "reviewed_plan_run_id": reviewedRun,
		"event_path": "events/dispatch-event.json", "event_sha256": workflowEventSHA,
	}
	reviewBytes, err := Canonical(review)
	if err != nil {
		t.Fatal(err)
	}
	entry := Object{
		"name": "sync", "command": "batch-apply", "sha256": planDigest,
		"status": "completed", "review_path": "reviews/sync.json", "review_sha256": SHA256(reviewBytes),
	}
	context := Object{
		"workflow_file": "task.yml", "repository": "example/widgets", "phase": "completed",
		"workflow_run_id": int64(90), "workflow_run_attempt": int64(1),
	}
	reader := &policyReader{
		proofs: Object{
			"reviews/sync.json":          MakeFileProof(reviewBytes),
			"events/dispatch-event.json": MakeFileProof(eventBytes),
		},
		used: map[string]bool{},
	}
	if err := engine.validatePlanSetDispatch(context, entry, Object{}, reader, planPolicy); err != nil {
		t.Fatalf("valid exact reviewed dispatch rejected: %v", err)
	}
	mutatedEvent := cloneNativeObject(event)
	mutatedEvent["inputs"].(Object)["release-channel"] = "preview"
	mutatedBytes, err := Canonical(mutatedEvent)
	if err != nil {
		t.Fatal(err)
	}
	reader = &policyReader{
		proofs: Object{
			"reviews/sync.json":          MakeFileProof(reviewBytes),
			"events/dispatch-event.json": MakeFileProof(mutatedBytes),
		},
		used: map[string]bool{},
	}
	if err := engine.validatePlanSetDispatch(context, entry, Object{}, reader, planPolicy); err == nil {
		t.Fatal("dispatch event with changed required input was accepted")
	}
	entry["review_sha256"] = strings.Repeat("0", 64)
	reader = &policyReader{proofs: Object{"reviews/sync.json": MakeFileProof(reviewBytes)}, used: map[string]bool{}}
	if err := engine.validatePlanSetDispatch(context, entry, Object{}, reader, planPolicy); err == nil {
		t.Fatal("review whose bytes differ from the manifest digest was accepted")
	}
}
