package workflow

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func branchAbsenceInventory(count int, autoDelete bool, absent ...int) contract.Object {
	inventory := branchTestInventory(count)
	rows, _ := contract.Objects(inventory, "branches")
	for _, index := range absent {
		rows[index]["branch"] = nil
	}
	refs := []any{}
	for _, row := range rows {
		if row["branch"] != nil {
			ref, _ := contract.ObjectAt(row, "branch")
			refs = append(refs, contract.Object{"name": ref["name"], "sha": ref["sha"]})
		}
	}
	refs = append(refs, contract.Object{"name": "main", "sha": strings.Repeat("b", 40)})
	inventory["branch_evidence"] = contract.Object{"inventory": contract.Object{"repo": testRepo().Object(), "branches": refs,
		"provenance": contract.Object{"live": true, "complete": true, "source": "github_api", "repository_node_id": "R_widgets"}}, "delete_branch_on_merge": autoDelete}
	return inventory
}

func TestBranchCleanupAbsentSelectionHasTruthfulZeroWriteTerminal(t *testing.T) {
	for _, autoDelete := range []bool{false, true} {
		t.Run(fmt.Sprintf("auto_delete_%t", autoDelete), func(t *testing.T) {
			f := &branchTestProvider{inventory: branchAbsenceInventory(1, autoDelete, 0)}
			plan := branchTestPlan(t, f, 1)
			absences, _ := contract.Objects(plan.Data, "already_absent")
			if len(plan.Operations) != 0 || len(absences) != 1 || absences[0]["kind"] != "already_absent" || absences[0]["delete_branch_on_merge"] != autoDelete || !same(absences[0]["selection"], branchTestSelection(1)["branches"].([]any)[0]) {
				t.Fatal("absence lost its exact typed outcome", plan.Data, plan.Operations)
			}
			root := t.TempDir()
			result, err := branchTestEngine(root, f).Apply(context.Background(), plan.Object())
			if err != nil || result["status"] != "completed" || !same(result["receipts"], []any{}) || f.writes != 0 {
				t.Fatal("already absent branch was not a zero-write completion", result, err, f.writes)
			}
			if _, err := branchTestEngine(root, f).Apply(context.Background(), plan.Object()); err != nil || f.writes != 0 {
				t.Fatal("completed absence was not safely revalidated", err, f.writes)
			}
		})
	}
}

func TestBranchCleanupMixedSelectionsProjectOnlyActualDeletions(t *testing.T) {
	for _, absent := range [][]int{nil, {0}, {1}, {0, 2}, {1, 2}, {0, 1, 2}} {
		t.Run(fmt.Sprint(absent), func(t *testing.T) {
			f := &branchTestProvider{inventory: branchAbsenceInventory(3, true, absent...)}
			plan := branchTestPlan(t, f, 3)
			want := 3 - len(absent)
			if len(plan.Operations) != want {
				t.Fatal("absent candidate gained a deletion primitive", plan.Operations)
			}
			root := t.TempDir()
			result, err := branchTestEngine(root, f).Apply(context.Background(), plan.Object())
			if err != nil || result["status"] != "completed" || f.writes != int64(want) {
				t.Fatal("mixed cleanup projected the wrong branch prefix", result, err, f.writes)
			}
			if _, err := branchTestEngine(root, f).Apply(context.Background(), plan.Object()); err != nil || f.writes != int64(want) {
				t.Fatal("mixed completion repeated a write", err, f.writes)
			}
		})
	}
}

func TestBranchCleanupAbsentEvidenceAndSelectedPRMustBeComplete(t *testing.T) {
	for _, change := range []string{"no_evidence", "missing_policy", "untyped_policy", "partial", "not_live", "foreign_repo", "foreign_incarnation", "missing_default", "repeated_ref", "invalid_ref", "ref_present", "wrong_pr_id_shape", "wrong_head", "open_pr", "draft_pr", "fork", "dependent"} {
		t.Run(change, func(t *testing.T) {
			inventory := branchAbsenceInventory(1, true, 0)
			evidence, _ := contract.ObjectAt(inventory, "branch_evidence")
			collection, _ := contract.ObjectAt(evidence, "inventory")
			prov, _ := contract.ObjectAt(collection, "provenance")
			rows, _ := contract.Objects(inventory, "branches")
			pr, _ := contract.ObjectAt(rows[0], "pull_request")
			switch change {
			case "no_evidence":
				delete(inventory, "branch_evidence")
			case "missing_policy":
				delete(evidence, "delete_branch_on_merge")
			case "untyped_policy":
				evidence["delete_branch_on_merge"] = "true"
			case "partial":
				prov["complete"] = false
			case "not_live":
				prov["live"] = false
			case "foreign_repo":
				collection["repo"] = contract.Object{"url": "https://github.com/foreign/widgets"}
			case "foreign_incarnation":
				prov["repository_node_id"] = "R_recreated"
			case "missing_default":
				collection["branches"] = []any{}
			case "repeated_ref":
				collection["branches"] = append(collection["branches"].([]any), collection["branches"].([]any)[0])
			case "invalid_ref":
				collection["branches"].([]any)[0].(contract.Object)["name"] = "bad..ref"
			case "ref_present":
				collection["branches"] = append(collection["branches"].([]any), contract.Object{"name": "codex/issue-17", "sha": strings.Repeat("c", 40)})
			case "wrong_pr_id_shape":
				pr["id"] = ""
			case "wrong_head":
				pr["headRefOid"] = strings.Repeat("c", 40)
			case "open_pr":
				pr["state"], pr["merged"] = "OPEN", false
			case "draft_pr":
				pr["isDraft"] = true
			case "fork":
				pr["head_repository"] = contract.Object{"url": "https://github.com/foreign/widgets"}
			case "dependent":
				rows[0]["base_dependents"].(contract.Object)["pull_requests"] = []any{contract.Object{"id": "PR_4", "number": int64(4), "url": testRepo().URL + "/pull/4", "baseRefName": "codex/issue-17", "headRefName": "codex/other", "state": "OPEN", "isDraft": true, "merged": false}}
			}
			f := &branchTestProvider{inventory: inventory}
			if _, err := PrepareBranchCleanup(context.Background(), f, testRepo(), branchTestSelection(1), time.Now()); err == nil || f.writes != 0 {
				t.Fatal("unqualified absence was accepted", err, f.writes)
			}
		})
	}
}

func TestBranchCleanupAbsentPlanCannotDropOrForgeTypedOutcome(t *testing.T) {
	for _, change := range []string{"missing", "empty", "digest", "pr_identity", "policy", "extra", "invented_operation"} {
		t.Run(change, func(t *testing.T) {
			f := &branchTestProvider{inventory: branchAbsenceInventory(1, true, 0)}
			plan := branchTestPlan(t, f, 1)
			outcomes, _ := contract.Objects(plan.Data, "already_absent")
			switch change {
			case "missing":
				delete(plan.Data, "already_absent")
			case "empty":
				plan.Data["already_absent"] = []any{}
			case "digest":
				outcomes[0]["branch_inventory_sha256"] = strings.Repeat("a", 64)
			case "pr_identity":
				outcomes[0]["pull_request_id"] = "PR_replaced"
			case "policy":
				outcomes[0]["delete_branch_on_merge"] = false
			case "extra":
				outcomes[0]["deleted_by_this_attempt"] = true
			case "invented_operation":
				plan.Operations = branchTestPlan(t, &branchTestProvider{inventory: branchTestInventory(1)}, 1).Operations
			}
			forged, err := contract.PreparePlan(plan.Command, plan.Repository, plan.Sources, plan.Data, plan.Operations, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := branchTestEngine(t.TempDir(), f).Apply(context.Background(), forged.Object()); err == nil || f.writes != 0 {
				t.Fatal("forged no-op plan reached completion", err, f.writes)
			}
		})
	}
}

func TestBranchCleanupAbsentPlanStillHoldsLiveDrift(t *testing.T) {
	for _, change := range []string{"retention", "pr_identity", "repository", "unrelated_branch", "reused_name"} {
		t.Run(change, func(t *testing.T) {
			f := &branchTestProvider{inventory: branchAbsenceInventory(1, true, 0)}
			plan := branchTestPlan(t, f, 1)
			evidence, _ := contract.ObjectAt(f.inventory, "branch_evidence")
			collection, _ := contract.ObjectAt(evidence, "inventory")
			rows, _ := contract.Objects(f.inventory, "branches")
			switch change {
			case "retention":
				evidence["delete_branch_on_merge"] = false
			case "pr_identity":
				rows[0]["pull_request"].(contract.Object)["id"] = "PR_replaced"
			case "repository":
				f.inventory["repository_node_id"] = "R_recreated"
			case "unrelated_branch":
				collection["branches"] = append(collection["branches"].([]any), contract.Object{"name": "other", "sha": strings.Repeat("c", 40)})
			case "reused_name":
				rows[0]["branch"] = contract.Object{"repo": testRepo().Object(), "repository_node_id": "R_widgets", "id": "REF_reused", "name": "codex/issue-17", "sha": strings.Repeat("c", 40)}
				collection["branches"] = append([]any{contract.Object{"name": "codex/issue-17", "sha": strings.Repeat("c", 40)}}, collection["branches"].([]any)...)
			}
			if _, err := branchTestEngine(t.TempDir(), f).Apply(context.Background(), plan.Object()); err == nil || f.writes != 0 {
				t.Fatal("no-op hid live inventory drift", err, f.writes)
			}
		})
	}
}

type interruptedAbsenceProvider struct {
	*branchTestProvider
}

func (f interruptedAbsenceProvider) BranchCleanupInventory(ctx context.Context, selection contract.Object) (contract.Object, error) {
	os.Exit(83) // Crash after the journal is opened but before terminal preflight.
	return nil, nil
}

func TestBranchCleanupAbsentFreshProcessHelper(t *testing.T) {
	root := os.Getenv("STEWARD_ABSENCE_TEST_ROOT")
	if root == "" {
		return
	}
	bytes, err := os.ReadFile(filepath.Join(root, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contract.Decode(strings.NewReader(string(bytes)))
	if err != nil {
		t.Fatal(err)
	}
	f := &branchTestProvider{inventory: branchAbsenceInventory(1, true, 0)}
	engine := branchTestEngine(root, f)
	engine.Adapter = BranchCleanup{Provider: interruptedAbsenceProvider{f}}
	if _, err := engine.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	t.Fatal("expected interruption")
}

func TestBranchCleanupAbsentFreshProcessResumesWithoutDeletionReceipt(t *testing.T) {
	f := &branchTestProvider{inventory: branchAbsenceInventory(1, true, 0)}
	plan := branchTestPlan(t, f, 1)
	root := t.TempDir()
	bytes, _ := contract.Canonical(plan.Object())
	if err := os.WriteFile(filepath.Join(root, "plan.json"), bytes, 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestBranchCleanupAbsentFreshProcessHelper$")
	child.Env = append(os.Environ(), "STEWARD_ABSENCE_TEST_ROOT="+root)
	if output, err := child.CombinedOutput(); err == nil {
		t.Fatal("child did not interrupt", string(output))
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 83 {
		t.Fatal("unexpected child failure", err, string(output))
	}
	result, err := branchTestEngine(root, f).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" || f.writes != 0 || !same(result["receipts"], []any{}) {
		t.Fatal("fresh process fabricated a deletion receipt", result, err, f.writes)
	}
}
