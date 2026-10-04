package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// These checks exercise the public named-input boundary, not another copy of
// the domain implementation. The fixtures are authored domain examples.
func TestPortablePlanningAndGovernanceCommandsAtPublicBoundary(t *testing.T) {
	root := checkout(t)
	governance, err := loadObject("../..", "testdata/governance/baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ family, action, policy, snapshot, command string }{
		{"governance", "labels-plan", "label_policy", "label_snapshot", "label-plan"},
		{"governance", "check", "governance_policy", "governance_snapshot", "governance-check"},
		{"governance", "summary", "summary_policy", "summary_snapshot", "governance-summary"},
		{"governance", "milestones-check", "milestone_policy", "milestone_snapshot", "milestone-check"},
		{"execution", "preflight", "preflight_policy", "preflight_snapshot", "execution-preflight"},
		{"execution", "transition", "transition_policy", "transition_snapshot", "execution-transition"},
		{"execution", "links", "link_policy", "link_snapshot", "execution-link-facts"},
		{"execution", "recover", "recovery_policy", "recovery_snapshot", "execution-recover"},
		{"merge", "eligibility", "merge_policy", "merge_snapshot", "merge-eligibility"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			policy, _ := contract.ObjectAt(governance, tc.policy)
			snapshot, _ := contract.ObjectAt(governance, tc.snapshot)
			// A fixture's optional repository display identity is normalized to
			// this caller's target; identity rejection is tested separately.
			for _, key := range []string{"repo", "repository"} {
				if raw, ok := snapshot[key]; ok {
					identity, ok := raw.(map[string]any)
					if !ok {
						t.Fatal("fixture identity must be object")
					}
					identity["nameWithOwner"] = "example/widgets"
					identity["url"] = "https://github.com/example/widgets"
				}
			}
			if tc.command == "execution-preflight" {
				snapshot["repo"] = contract.Object{"nameWithOwner": "example/widgets", "url": "https://github.com/example/widgets", "defaultBranch": "main"}
			}
			if tc.command == "merge-eligibility" {
				snapshot["repository"] = contract.Object{"nameWithOwner": "example/widgets", "url": "https://github.com/example/widgets", "defaultBranch": "main"}
			}
			for name, object := range map[string]contract.Object{"policy": policy, "snapshot": snapshot} {
				if err := writeFile(root, name+".json", object); err != nil {
					t.Fatal(err)
				}
			}
			var out, stderr bytes.Buffer
			args := []string{tc.family, tc.action, "--repo-root", root, "--policy", "policy.json", "--input", "snapshot=snapshot.json"}
			if err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), args); err != nil {
				t.Fatal(err, stderr.String())
			}
			result, err := contract.Decode(&out)
			if err != nil || result["command"] != tc.command {
				t.Fatal(result, err)
			}
			if _, err := contract.ObjectAt(result, "data"); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, tc := range []struct{ family, action, file, command string }{
		{"quarter", "validate", "quarter-plan.json", "quarter-plan-validate"},
		{"review", "validate", "review-findings.json", "review-findings-validate"},
		{"review", "closeout-validate", "review-closeout-findings.json", "review-closeout-findings-validate"},
	} {
		body, err := os.ReadFile(filepath.Join("../..", "testdata/planning", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "payload.json"), body, 0600); err != nil {
			t.Fatal(err)
		}
		var out, stderr bytes.Buffer
		if err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), []string{tc.family, tc.action, "--repo-root", root, "--input", "payload=payload.json"}); err != nil {
			t.Fatal(tc.command, err)
		}
		result, err := contract.Decode(&out)
		if err != nil || result["command"] != tc.command {
			t.Fatal(tc.command, result, err)
		}
	}
}

func TestSourceDirtyIsTypedProvenance(t *testing.T) {
	original := SourceDirty
	t.Cleanup(func() { SourceDirty = original })
	for _, tc := range []struct {
		raw  string
		want any
	}{{"true", true}, {"false", false}, {"unknown", nil}} {
		SourceDirty = tc.raw
		var out, stderr bytes.Buffer
		if err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), []string{"version", "--json"}); err != nil {
			t.Fatal(err)
		}
		info, err := contract.Decode(&out)
		if err != nil || info["source_dirty"] != tc.want {
			t.Fatal(info, err)
		}
	}
	SourceDirty = "release-clean"
	var out, stderr bytes.Buffer
	if err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), []string{"version"}); err == nil || out.Len() != 0 {
		t.Fatal("invalid build provenance emitted success")
	}
}
