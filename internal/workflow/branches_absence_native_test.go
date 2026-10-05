package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

type absenceNativeFixture struct {
	deliveryNativeFixture
	failQuery       string
	incompletePages bool
	reappear        bool
	branchReads     int
	changedPR       bool
}

func (f *absenceNativeFixture) Execute(ctx context.Context, executable string, args []string, input []byte, root string, env []string) (native.Result, error) {
	if len(args) > 1 && args[0] == "api" {
		if args[1] == "graphql" && f.failQuery != "" && strings.Contains(string(input), f.failQuery) {
			return native.Result{ExitCode: 1}, nil
		}
		if strings.Contains(args[1], "/branches?") {
			f.branchReads++
			if f.incompletePages {
				return native.Result{Stdout: []byte("[]")}, nil
			}
			if f.reappear && f.branchReads > 1 {
				f.deleted = false
			}
		}
	}
	result, err := f.deliveryNativeFixture.Execute(ctx, executable, args, input, root, env)
	if err == nil && f.changedPR && strings.Contains(string(input), "pullRequest(number") {
		value, err := contract.Decode(strings.NewReader(string(result.Stdout)))
		if err != nil {
			return native.Result{}, err
		}
		value["data"].(contract.Object)["repository"].(contract.Object)["pullRequest"].(contract.Object)["id"] = "PR_replaced"
		result.Stdout, err = contract.Canonical(value)
		return result, err
	}
	return result, err
}

func TestNativeBranchCleanupAutoDeletedBranchPreparesAppliesAndRevalidatesNoop(t *testing.T) {
	f := &absenceNativeFixture{deliveryNativeFixture: deliveryNativeFixture{merged: true, deleted: true, autoDelete: true}}
	n := NativeBranchCleanup{Transport: bridgeTransport(f)}
	plan, err := PrepareBranchCleanup(context.Background(), n, testRepo(), branchTestSelection(1), time.Now())
	if err != nil {
		t.Fatal("server auto-deletion could not prepare", err)
	}
	absences, _ := contract.Objects(plan.Data, "already_absent")
	if len(plan.Operations) != 0 || len(absences) != 1 || absences[0]["delete_branch_on_merge"] != true || f.branchReads != 2 {
		t.Fatal("no-op lacks positive native evidence", plan.Data, f.branchReads)
	}
	engine := apply.Engine{Root: t.TempDir(), Repository: testRepo(), Command: BranchCleanupCommand, Adapter: BranchCleanup{Provider: n}}
	result, err := engine.Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" || !same(result["receipts"], []any{}) {
		t.Fatal("native no-op did not complete without writes", result, err)
	}
	if f.branchReads != 4 {
		t.Fatal("empty apply skipped complete live preflight", f.branchReads)
	}
	f.deleted = false
	if _, err := engine.Apply(context.Background(), plan.Object()); err == nil {
		t.Fatal("reused branch name silently completed its old absence plan")
	}
}

func TestNativeBranchCleanupAbsenceHoldsMissingEvidenceAndCaptureDrift(t *testing.T) {
	for name, mutate := range map[string]func(*absenceNativeFixture){
		"unreadable branches":             func(f *absenceNativeFixture) { f.failFacet = "/branches?" },
		"no complete branch pages":        func(f *absenceNativeFixture) { f.incompletePages = true },
		"unreadable retention":            func(f *absenceNativeFixture) { f.failQuery = "deleteBranchOnMerge" },
		"unreadable PR":                   func(f *absenceNativeFixture) { f.failQuery = "pullRequest(number" },
		"unreadable head inventory":       func(f *absenceNativeFixture) { f.failHeadRead = true },
		"unreadable dependent inventory":  func(f *absenceNativeFixture) { f.failQuery = "baseRefName:$base" },
		"retention drift":                 func(f *absenceNativeFixture) { f.retentionDrift = true },
		"repository recreated":            func(f *absenceNativeFixture) { f.recreateAfterRepo = 2 },
		"head repository recreated":       func(f *absenceNativeFixture) { f.headIdentityDrift = true },
		"head repository unavailable":     func(f *absenceNativeFixture) { f.omitHeadID = true },
		"branch recreated during capture": func(f *absenceNativeFixture) { f.reappear = true },
		"draft dependent":                 func(f *absenceNativeFixture) { f.dependent = true },
		"draft reused head":               func(f *absenceNativeFixture) { f.openHead, f.headIsDraft = true, true },
	} {
		t.Run(name, func(t *testing.T) {
			f := &absenceNativeFixture{deliveryNativeFixture: deliveryNativeFixture{merged: true, deleted: true, autoDelete: true}}
			mutate(f)
			if _, err := PrepareBranchCleanup(context.Background(), NativeBranchCleanup{Transport: bridgeTransport(f)}, testRepo(), branchTestSelection(1), time.Now()); err == nil {
				t.Fatal("unqualified native absence became a no-op")
			}
		})
	}
}

func TestNativeBranchCleanupAbsenceRechecksPolicyPRAndDraftProtectionBeforeApply(t *testing.T) {
	for name, mutate := range map[string]func(*absenceNativeFixture){
		"retention policy":      func(f *absenceNativeFixture) { f.autoDelete = false },
		"immutable PR identity": func(f *absenceNativeFixture) { f.changedPR = true },
		"draft reused head":     func(f *absenceNativeFixture) { f.openHead, f.headIsDraft = true, true },
		"draft dependent":       func(f *absenceNativeFixture) { f.dependent = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := &absenceNativeFixture{deliveryNativeFixture: deliveryNativeFixture{merged: true, deleted: true, autoDelete: true}}
			n := NativeBranchCleanup{Transport: bridgeTransport(f)}
			plan, err := PrepareBranchCleanup(context.Background(), n, testRepo(), branchTestSelection(1), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			mutate(f)
			engine := apply.Engine{Root: t.TempDir(), Repository: testRepo(), Command: BranchCleanupCommand, Adapter: BranchCleanup{Provider: n}}
			if _, err := engine.Apply(context.Background(), plan.Object()); err == nil {
				t.Fatal("changed live source qualified an old no-op")
			}
		})
	}
}

func TestNativeBranchCleanupPreservesHistoricalPresentPlanAndReceiptDigest(t *testing.T) {
	f := &absenceNativeFixture{deliveryNativeFixture: deliveryNativeFixture{merged: true}}
	n := NativeBranchCleanup{Transport: bridgeTransport(f)}
	inventory, err := n.BranchCleanupInventory(context.Background(), branchTestSelection(1))
	if err != nil {
		t.Fatal(err)
	}
	delete(inventory, "branch_evidence")
	plan := branchTestPlan(t, &branchTestProvider{inventory: inventory}, 1)
	adapter := BranchCleanup{Provider: n}
	if err := adapter.Preflight(context.Background(), plan, nil); err != nil {
		t.Fatal("new native inventory blocked a historical exact present plan", err)
	}
	op := plan.Operations[0]
	ack := contract.Object{"kind": op.Kind, "primitive_id": op.ID, "repository": plan.Repository.Object(),
		"operation_id": strings.Repeat("f", 32), "target": op.Target,
		"provider_ack": contract.Object{"exit_code": int64(0), "stdout": "To " + testRepo().URL + ".git\n-\t:refs/heads/codex/issue-17\t[deleted]\nDone\n", "stderr": "", "remote_url": testRepo().URL + ".git", "ref": "refs/heads/codex/issue-17", "expected_sha": strings.Repeat("a", 40)}}
	f.deleted = true
	result, err := adapter.observeAfter(context.Background(), plan, op, ack)
	if err != nil || adapter.ValidateReceipt(plan, op, result) != nil {
		t.Fatal("historical acknowledged deletion lost its exact receipt schema", result, err)
	}
	expected, err := branchCleanupPrefixInventory(plan, 0)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := contract.Digest(expected)
	if result["observed_inventory_sha256"] != digest {
		t.Fatal("new native evidence rewrote a historical receipt digest")
	}
	// The same missing ref without that original acknowledgement still holds.
	if _, _, err := adapter.Observe(context.Background(), plan, op, strings.Repeat("f", 32), nil); err == nil {
		t.Fatal("historical absence resolved an unacknowledged deletion")
	}
}
