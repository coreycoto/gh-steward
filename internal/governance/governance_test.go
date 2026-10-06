package governance

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func baseline(t *testing.T) contract.Object {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "governance", "baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := contract.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func nested(t *testing.T, root contract.Object, key string) contract.Object {
	t.Helper()
	value, err := contract.ObjectAt(root, key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func clone(t *testing.T, source contract.Object) contract.Object {
	t.Helper()
	value, err := contract.Clone(source)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func resultSummary(t *testing.T, result contract.Object) contract.Object {
	t.Helper()
	return nested(t, result, "summary")
}

func findingExists(t *testing.T, result contract.Object, code string, number int64) bool {
	t.Helper()
	findings, err := contract.Array(result, "findings")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range findings {
		finding, ok := raw.(map[string]any)
		if !ok || finding["code"] != code {
			continue
		}
		if number == 0 && finding["number"] == nil {
			return true
		}
		if actual, err := contract.Integer(finding["number"]); err == nil && actual == number {
			return true
		}
	}
	return false
}

func TestLabelPlanClassifiesManagedAndDefaultDrift(t *testing.T) {
	fixture := baseline(t)
	result, err := Run("label-plan", map[string]contract.Object{
		"policy":   nested(t, fixture, "label_policy"),
		"snapshot": nested(t, fixture, "label_snapshot"),
	})
	if err != nil {
		t.Fatal(err)
	}
	summary := resultSummary(t, result)
	for key, want := range map[string]int{"preferred_label_count": 5, "repo_managed_count": 3, "create_count": 1, "update_count": 1, "noop_count": 1, "managed_drift_count": 2, "default_drift_count": 1, "missing_default_count": 1} {
		if got := summary[key]; got != want {
			t.Errorf("summary.%s = %v, want %d", key, got, want)
		}
	}
	labels, _ := contract.Objects(result, "labels")
	if labels[0]["action"] != "update" || labels[1]["action"] != "noop" || labels[2]["action"] != "create" || labels[3]["action"] != "default-drift" || labels[4]["action"] != "missing-default" {
		t.Fatalf("unexpected ordered label actions: %#v", labels)
	}
	if _, err := LabelPaletteDiff(nested(t, fixture, "label_policy"), contract.Object{"labels": []any{contract.Object{"name": "task", "color": "bad", "description": "", "default": false}}}); err == nil {
		t.Fatal("malformed live color was silently accepted")
	}
}

func TestGovernanceCheckAppliesCallerVocabularyAndReportsDrift(t *testing.T) {
	fixture := baseline(t)
	result, err := Run("governance-check", map[string]contract.Object{
		"policy":   nested(t, fixture, "governance_policy"),
		"snapshot": nested(t, fixture, "governance_snapshot"),
	})
	if err != nil {
		t.Fatal(err)
	}
	summary := resultSummary(t, result)
	if summary["open_issue_count"] != 3 || summary["project_audited"] != true || summary["finding_count"] != 8 || summary["error_count"] != 1 {
		t.Fatalf("unexpected governance summary: %#v", summary)
	}
	for _, expected := range []struct {
		code   string
		number int64
	}{{"missing-governance-label", 1}, {"conflicting-governance-labels", 1}, {"retired-labels", 1}, {"invalid-project-status", 1}, {"invalid-prefix", 2}, {"retired-labels", 2}, {"missing-governance-label", 3}, {"missing-backlog-membership", 3}} {
		if !findingExists(t, result, expected.code, expected.number) {
			t.Errorf("expected finding %s for issue %d", expected.code, expected.number)
		}
	}
	bad := clone(t, nested(t, fixture, "governance_snapshot"))
	issues := bad["issues"].([]any)
	issues[1].(map[string]any)["number"] = issues[0].(map[string]any)["number"]
	if _, err := GovernanceCheck(nested(t, fixture, "governance_policy"), bad); err == nil {
		t.Fatal("duplicate issue identity was accepted")
	}
}

func TestMilestoneCheckUsesSuppliedDatesDescriptionsAndRationaleRules(t *testing.T) {
	fixture := baseline(t)
	result, err := Run("milestone-check", map[string]contract.Object{
		"policy":   nested(t, fixture, "milestone_policy"),
		"snapshot": nested(t, fixture, "milestone_snapshot"),
	})
	if err != nil {
		t.Fatal(err)
	}
	summary := resultSummary(t, result)
	if summary["quarter_issue_count"] != 2 || summary["open_milestone_count"] != 2 || summary["project_audited"] != true || summary["finding_count"] != 7 {
		t.Fatalf("unexpected milestone summary: %#v", summary)
	}
	for _, expected := range []struct {
		code   string
		number int64
	}{{"missing-quarter-milestone", 0}, {"stale-quarter-due-date", 0}, {"stale-quarter-description", 0}, {"empty-quarter-milestone", 0}, {"missing-quarter-rationale", 41}, {"umbrella-holds-quarter-milestone", 41}, {"later-issue-has-quarter-milestone", 41}} {
		if !findingExists(t, result, expected.code, expected.number) {
			t.Errorf("expected finding %s for issue %d", expected.code, expected.number)
		}
	}
}

func TestMilestoneCheckAcceptsDynamicDescriptionSectionsWhenPolicyMarkersRemain(t *testing.T) {
	fixture := baseline(t)
	snapshot := clone(t, nested(t, fixture, "milestone_snapshot"))
	milestones := snapshot["milestones"].([]any)
	milestones[1].(map[string]any)["description"] = strings.Join([]string{
		"Quarter: 2026 Q4",
		"Date range: 2026-10-01 through 2026-12-31",
		"Quarter goals:",
		"Finish the active product work.",
		"Active initiatives:",
		"Reviewed initiative A",
		"Use only for issues explicitly committed to this quarter.",
		"Record quarter rationale in the issue body or a maintainer comment.",
	}, "\n")
	result, err := Run("milestone-check", map[string]contract.Object{
		"policy":   nested(t, fixture, "milestone_policy"),
		"snapshot": snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if findingExists(t, result, "stale-quarter-description", 0) {
		t.Fatal("description with all caller policy markers and dynamic sections was marked stale")
	}
}

func TestGovernanceSummaryKeepsReportLabelsAndLinksCallerSupplied(t *testing.T) {
	fixture := baseline(t)
	result, err := Run("governance-summary", map[string]contract.Object{
		"policy":   nested(t, fixture, "summary_policy"),
		"snapshot": nested(t, fixture, "summary_snapshot"),
	})
	if err != nil {
		t.Fatal(err)
	}
	status := nested(t, result, "status")
	if status["unresolved_manual_drift"] != true || status["post_findings"] != int64(1) {
		t.Fatalf("unexpected summary status: %#v", status)
	}
	markdown, _ := result["markdown"].(string)
	for _, expected := range []string{"Backlog audit: findings=1", "Managed label needs an update", "https://example.invalid/workflow", "https://example.invalid/guide"} {
		if !strings.Contains(markdown, expected) {
			t.Errorf("summary markdown is missing %q", expected)
		}
	}
}

func TestExecutionPreflightDistinguishesTrackedChangesAndStackedFlow(t *testing.T) {
	fixture := baseline(t)
	result, err := Run("execution-preflight", map[string]contract.Object{
		"policy":   nested(t, fixture, "preflight_policy"),
		"snapshot": nested(t, fixture, "preflight_snapshot"),
	})
	if err != nil {
		t.Fatal(err)
	}
	branch := nested(t, result, "branch_context")
	guardrails := nested(t, result, "guardrails")
	plan := nested(t, result, "planned_action")
	if branch["is_stacked_branch"] != true || branch["is_detached_head"] != false || guardrails["dirty_worktree_blocked"] != false || plan["merge_method"] != "merge" {
		t.Fatalf("unexpected preflight result: %#v", result)
	}
	git := nested(t, nested(t, fixture, "preflight_snapshot"), "git")
	if nested(t, nested(t, result, "git"), "worktree")["has_tracked_changes"] != false || git["worktree_entries"] == nil {
		t.Fatal("untracked-only worktree should not block the finish preflight")
	}
	policy := contract.Object{"intent": "open-pr"}
	snapshot := clone(t, nested(t, fixture, "preflight_snapshot"))
	delete(snapshot, "pull_request")
	git = nested(t, snapshot, "git")
	git["current_branch"] = "main"
	git["worktree_entries"] = []any{" M tracked.go", "?? notes.txt"}
	blocked, err := ExecutionPreflight(policy, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	blockedGuardrails := nested(t, blocked, "guardrails")
	if blockedGuardrails["default_branch_blocked"] != true || blockedGuardrails["dirty_worktree_blocked"] != true || len(blockedGuardrails["blocking_reasons"].([]any)) != 2 {
		t.Fatalf("default branch and tracked changes should both block open-pr: %#v", blockedGuardrails)
	}
}

func TestExecutionTransitionRequiresBranchEvidenceAndUsesConfiguredStatuses(t *testing.T) {
	fixture := baseline(t)
	policy := nested(t, fixture, "transition_policy")
	snapshot := clone(t, nested(t, fixture, "transition_snapshot"))
	result, err := ExecutionTransition(policy, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if result["sync_state"] != "closed-unmerged-branch-live" || result["final_status"] != "Active" {
		t.Fatalf("closed PR with a live head must remain active: %#v", result)
	}
	snapshot["branch_exists"] = false
	result, err = ExecutionTransition(policy, snapshot)
	if err != nil || result["sync_state"] != "closed-unmerged-branch-deleted" || result["final_status"] != "Backlog" {
		t.Fatalf("deleted PR head should return to caller's todo state: %#v, %v", result, err)
	}
	delete(snapshot, "branch_exists")
	if _, err := ExecutionTransition(policy, snapshot); err == nil {
		t.Fatal("closed unmerged PR without branch evidence was accepted")
	}
	pr := nested(t, snapshot, "pull_request")
	pr["is_merged"] = true
	issue, _ := contract.ObjectAt(snapshot, "issue")
	issue["state"] = "CLOSED"
	result, err = ExecutionTransition(policy, snapshot)
	if err != nil || result["sync_state"] != "merged" || result["final_status"] != "Complete" || len(result["actions"].([]any)) != 0 {
		t.Fatalf("authoritatively closed issue may remain done without a second close: %#v, %v", result, err)
	}

	issue["state"] = "OPEN"
	snapshot["default_branch"] = "main"
	pr["base_branch"] = "main"
	pr["closing_issue_numbers"] = []any{int64(41)}
	result, err = ExecutionTransition(policy, snapshot)
	if err != nil || result["sync_state"] != "merged" || result["final_status"] != "Complete" || !reflect.DeepEqual(result["actions"], []any{"close-issue"}) {
		t.Fatalf("default-base closing reference should complete an open issue: %#v, %v", result, err)
	}

	for _, test := range []struct {
		name          string
		baseBranch    string
		closingIssues any
		defaultBranch any
	}{
		{name: "reference-only", baseBranch: "main", closingIssues: []any{}, defaultBranch: "main"},
		{name: "non-default base", baseBranch: "release", closingIssues: []any{int64(41)}, defaultBranch: "main"},
		{name: "missing completion evidence", baseBranch: "main", closingIssues: nil, defaultBranch: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := clone(t, snapshot)
			candidateIssue, _ := contract.ObjectAt(candidate, "issue")
			candidateIssue["state"] = "OPEN"
			candidatePR, _ := contract.ObjectAt(candidate, "pull_request")
			candidatePR["base_branch"] = test.baseBranch
			if test.closingIssues != nil {
				candidatePR["closing_issue_numbers"] = test.closingIssues
			} else {
				delete(candidatePR, "closing_issue_numbers")
			}
			if test.defaultBranch != nil {
				candidate["default_branch"] = test.defaultBranch
			} else {
				delete(candidate, "default_branch")
			}
			got, err := ExecutionTransition(policy, candidate)
			if err != nil || got["sync_state"] != "merged" || got["final_status"] != nil || len(got["actions"].([]any)) != 0 {
				t.Fatalf("unverified merged PR proposed completion: %#v, %v", got, err)
			}
		})
	}
}

func TestExecutionLinkFactsVerifyExactIssueAndClosingIdentity(t *testing.T) {
	fixture := baseline(t)
	result, err := ExecutionLinkFacts(nested(t, fixture, "link_policy"), nested(t, fixture, "link_snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	if result["parsed_issue_number"] != int64(41) || result["closing_keyword_verified"] != true || result["body_has_execution_marker"] != true || result["link_verification_state"] != "auto-link-verified" {
		t.Fatalf("link facts do not verify the exact issue: %#v", result)
	}
	changed := clone(t, nested(t, fixture, "link_snapshot"))
	pr := nested(t, changed, "pull_request")
	pr["closing_issue_numbers"] = []any{int64(42)}
	result, err = ExecutionLinkFacts(nested(t, fixture, "link_policy"), changed)
	if err != nil || result["closing_keyword_verified"] != false || result["link_verification_state"] != "auto-link-unverified" {
		t.Fatalf("different issue closure must not verify this link: %#v, %v", result, err)
	}
}

func TestExecutionRecoveryRequiresTypedExactEvidence(t *testing.T) {
	fixture := baseline(t)
	policy, snapshot := nested(t, fixture, "recovery_policy"), nested(t, fixture, "recovery_snapshot")
	verified, err := VerifyRecoveryAttestation(policy, snapshot)
	if err != nil || verified["verified"] != true || verified["state"] != "no_project_writes" {
		t.Fatalf("reviewed no-write evidence did not verify: %#v, %v", verified, err)
	}
	tests := []struct {
		name   string
		mutate func(contract.Object)
	}{
		{"run identity", func(s contract.Object) { nested(t, s, "run")["id"] = int64(101) }},
		{"PR identity", func(s contract.Object) {
			nested(t, s, "run")["pull_requests"].([]any)[0].(map[string]any)["number"] = int64(43)
		}},
		{"artifact identity", func(s contract.Object) {
			nested(t, s, "artifact_listing")["artifacts"].([]any)[0].(map[string]any)["id"] = int64(301)
		}},
		{"ambiguous inventory", func(s contract.Object) { nested(t, s, "artifact_files")["entries"] = []any{} }},
		{"symlink evidence", func(s contract.Object) {
			nested(t, s, "artifact_files")["entries"].([]any)[0].(map[string]any)["symlink"] = true
		}},
		{"untyped symlink evidence", func(s contract.Object) {
			delete(nested(t, s, "artifact_files")["entries"].([]any)[0].(map[string]any), "symlink")
		}},
		{"sentinel state", func(s contract.Object) { nested(t, s, "sentinel")["state"] = "no_project_writes" }},
		{"log digest", func(s contract.Object) {
			s["log_sha256"] = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		}},
		{"log symlink", func(s contract.Object) { s["log_is_symlink"] = true }},
		{"run phase", func(s contract.Object) { nested(t, s, "attestation")["phase"] = "after_project_sync" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := clone(t, snapshot)
			test.mutate(changed)
			if _, err := VerifyRecoveryAttestation(policy, changed); err == nil {
				t.Fatal("drifted recovery evidence was accepted")
			}
		})
	}
}

func TestMergeEligibilityRequiresRepositoryAndExactHead(t *testing.T) {
	fixture := baseline(t)
	policy := nested(t, fixture, "merge_policy")
	snapshot := nested(t, fixture, "merge_snapshot")
	result, err := MergeEligibility(policy, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "eligible" || result["expected_head_sha"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || result["merge_method"] != "squash" || len(result["verified_checks"].([]any)) == 0 {
		t.Fatalf("all merge evidence passed but candidate is not eligible: %#v", result)
	}
	mutations := []struct {
		name   string
		mutate func(contract.Object)
	}{
		{"workflow repository", func(s contract.Object) {
			nested(t, nested(t, s, "event"), "workflow_run")["repository"].(map[string]any)["full_name"] = "other/project"
		}},
		{"associated head", func(s contract.Object) {
			nested(t, nested(t, s, "event"), "workflow_run")["pull_requests"].([]any)[0].(map[string]any)["head"].(map[string]any)["sha"] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
		{"PR head drift", func(s contract.Object) {
			nested(t, s, "pull_request")["headRefOid"] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := clone(t, snapshot)
			mutation.mutate(changed)
			got, err := MergeEligibility(policy, changed)
			if err != nil || got["status"] != "blocked" {
				t.Fatalf("identity/head drift did not block: %#v, %v", got, err)
			}
		})
	}
	changed := clone(t, snapshot)
	changed["required_checks"] = []any{contract.Object{"bucket": "pending", "name": "integration", "state": "pending"}}
	got, err := MergeEligibility(policy, changed)
	if err != nil || got["status"] != "blocked" {
		t.Fatalf("pending check did not block: %#v, %v", got, err)
	}
	changed = clone(t, snapshot)
	nested(t, changed, "pull_request")["repository"] = "foreign/project"
	if _, err := MergeEligibility(policy, changed); err == nil {
		t.Fatal("cross-repository PR snapshot identity was accepted")
	}
	changed = clone(t, snapshot)
	nested(t, changed, "pull_request")["number"] = int64(99)
	if _, err := MergeEligibility(policy, changed); err == nil {
		t.Fatal("different pull-request number was accepted")
	}
	changed = clone(t, snapshot)
	workflow := nested(t, nested(t, changed, "event"), "workflow_run")
	workflow["head_branch"] = "service/branch"
	workflow["pull_requests"].([]any)[0].(map[string]any)["head"].(map[string]any)["ref"] = "service/branch"
	pr := nested(t, changed, "pull_request")
	pr["headRefName"] = "service/branch"
	pr["author"] = contract.Object{"login": "service[bot]", "is_bot": true}
	got, err = MergeEligibility(policy, changed)
	if err != nil || got["status"] != "eligible" {
		t.Fatalf("verified trusted bot should satisfy caller actor policy: %#v, %v", got, err)
	}
	pr["author"] = contract.Object{"login": "service[bot]", "is_bot": false}
	got, err = MergeEligibility(policy, changed)
	if err != nil || got["status"] != "blocked" {
		t.Fatalf("non-bot author must fail the trusted actor policy: %#v, %v", got, err)
	}
}

func TestRunRejectsUnknownCommandsAndMissingPolicy(t *testing.T) {
	if _, err := Run("does-not-exist", nil); err == nil {
		t.Fatal("unknown command was accepted")
	}
	if _, err := Run("label-plan", map[string]contract.Object{"snapshot": {"labels": []any{}}}); err == nil {
		t.Fatal("missing policy was accepted")
	}
}
