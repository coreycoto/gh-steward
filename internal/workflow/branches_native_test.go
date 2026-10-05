package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestNativeBranchCleanupRequiresCompleteCurrentRefsAndMergedPRs(t *testing.T) {
	f := &deliveryNativeFixture{merged: true}
	n := NativeBranchCleanup{Transport: bridgeTransport(f)}
	plan, err := PrepareBranchCleanup(context.Background(), n, testRepo(), branchTestSelection(1), time.Now())
	if err != nil || len(plan.Operations) != 1 || plan.Operations[0].Kind != "branch-delete" {
		t.Fatal("native merged source does not feed reviewed cleanup", err)
	}
	for name, fixture := range map[string]*deliveryNativeFixture{
		"failed branch collection":            {merged: true, failFacet: "/branches?"},
		"branch moved during capture":         {merged: true, branchDrift: true},
		"repository recreated during capture": {merged: true, recreateAfterRepo: 4},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (NativeBranchCleanup{Transport: bridgeTransport(fixture)}).BranchCleanupInventory(context.Background(), branchTestSelection(1)); err == nil {
				t.Fatal("incomplete native branch evidence accepted")
			}
		})
	}
	for name, fixture := range map[string]*deliveryNativeFixture{
		"PR still open": {}, "draft dependent PR": {merged: true, dependent: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PrepareBranchCleanup(context.Background(), NativeBranchCleanup{Transport: bridgeTransport(fixture)}, testRepo(), branchTestSelection(1), time.Now()); err == nil {
				t.Fatal("unqualified branch deletion prepared")
			}
		})
	}
	f = &deliveryNativeFixture{}
	if _, err := n.BranchCleanupInventory(context.Background(), contract.Object{"branches": []any{}, "unexpected": true}); err == nil {
		t.Fatal("extra caller selector accepted")
	}
	if _, err := (NativeBranchCleanup{Transport: bridgeTransport(f)}).BranchCleanupInventory(context.Background(), contract.Object{"branches": "all"}); err == nil || len(f.calls) != 0 {
		t.Fatal("unreviewed broad selection reached IO", err, f.calls)
	}
}

func TestNativeBranchCleanupProtectsReusedOpenHeadsBeforePreparationAndApply(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(fmt.Sprintf("draft_%t", draft), func(t *testing.T) {
			f := &deliveryNativeFixture{merged: true}
			n := NativeBranchCleanup{Transport: bridgeTransport(f)}
			plan, err := PrepareBranchCleanup(context.Background(), n, testRepo(), branchTestSelection(1), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			f.openHead, f.headIsDraft = true, draft
			if _, err := PrepareBranchCleanup(context.Background(), n, testRepo(), branchTestSelection(1), time.Now()); err == nil {
				t.Fatal("reused open head was prepared for deletion")
			}
			if err := (BranchCleanup{Provider: n}).Preflight(context.Background(), plan, nil); err == nil {
				t.Fatal("an open head introduced after review did not block apply")
			}
		})
	}
	f := &deliveryNativeFixture{merged: true, failHeadRead: true}
	if _, err := PrepareBranchCleanup(context.Background(), NativeBranchCleanup{Transport: bridgeTransport(f)}, testRepo(), branchTestSelection(1), time.Now()); err == nil {
		t.Fatal("failed head inventory was treated as no active PR")
	}
}
