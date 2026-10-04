package governance

import (
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func previewSummaryFixture(t *testing.T) (contract.Object, contract.Object) {
	t.Helper()
	fixture := baseline(t)
	policy := clone(t, nested(t, fixture, "summary_policy"))
	snapshot := clone(t, nested(t, fixture, "summary_snapshot"))
	snapshot["mode"], snapshot["post_reports"], snapshot["fix_result"] = "preview", contract.Object{}, contract.Object{}
	snapshot["pre_reports"] = contract.Object{"audit": contract.Object{"summary": contract.Object{"finding_count": int64(1), "error_count": int64(0), "fixable_count": int64(1)}, "findings": []any{contract.Object{"code": "needs-review", "message": "An issue still needs review", "number": int64(19)}}}}
	return policy, snapshot
}

func TestGovernancePreviewUsesActualPreReportsAndDoesNotClaimAppliedFixes(t *testing.T) {
	policy, snapshot := previewSummaryFixture(t)
	result, err := GovernanceSummary(policy, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	status := nested(t, result, "status")
	if status["unresolved_manual_drift"] != true || status["pre_findings"] != int64(1) || status["post_findings"] != int64(0) || status["evaluated_findings"] != int64(1) || len(nested(t, status, "post_reports")) != 0 {
		t.Fatal("preview findings were replaced by missing post-fix evidence", status)
	}
	markdown := result["markdown"].(string)
	for _, expected := range []string{"## Preview checks", "An issue still needs review", "Backlog audit: findings=1", "safe-fix pass: preview only", "https://example.invalid/workflow"} {
		if !strings.Contains(markdown, expected) {
			t.Fatal("preview summary lost current finding evidence or guidance", expected, markdown)
		}
	}
	for _, invalid := range []string{"## Post-fix checks", "safe fixes applied", "after the reviewed fix pass"} {
		if strings.Contains(markdown, invalid) {
			t.Fatal("preview claimed applied qualification", invalid)
		}
	}
}

func TestGovernanceSummaryRequiresCompleteChecksBeforeReportingClean(t *testing.T) {
	for _, mode := range []string{"preview", "post-fix"} {
		for _, evidence := range []string{"complete_clean", "missing", "report_error", "access_failure"} {
			t.Run(mode+"/"+evidence, func(t *testing.T) {
				policy, snapshot := previewSummaryFixture(t)
				snapshot["mode"] = mode
				reports := contract.Object{"audit": contract.Object{"summary": contract.Object{"finding_count": int64(0), "error_count": int64(0), "fixable_count": int64(0)}}}
				if evidence == "missing" {
					delete(reports, "audit")
				}
				if evidence == "report_error" {
					nested(t, nested(t, reports, "audit"), "summary")["error_count"] = int64(1)
				}
				if evidence == "access_failure" {
					snapshot["project_access"] = false
				}
				if mode == "preview" {
					snapshot["pre_reports"] = reports
				} else {
					snapshot["post_reports"] = reports
				}
				result, err := GovernanceSummary(policy, snapshot)
				if err != nil {
					t.Fatal(err)
				}
				status := nested(t, result, "status")
				if status["unresolved_manual_drift"] != (evidence != "complete_clean") {
					t.Fatal("incomplete qualification reported clean", status)
				}
			})
		}
	}
}

func TestGovernancePreviewRejectsStalePostResultsAndUnknownModes(t *testing.T) {
	for _, kind := range []string{"post", "fix", "unknown_mode", "nontext_mode"} {
		policy, snapshot := previewSummaryFixture(t)
		switch kind {
		case "post":
			snapshot["post_reports"] = contract.Object{"audit": contract.Object{}}
		case "fix":
			snapshot["fix_result"] = contract.Object{"summary": contract.Object{"applied_count": int64(0), "failed_count": int64(0)}}
		case "unknown_mode":
			snapshot["mode"] = "automatic"
		case "nontext_mode":
			snapshot["mode"] = true
		}
		if _, err := GovernanceSummary(policy, snapshot); err == nil {
			t.Fatal("ambiguous preview qualification accepted", kind)
		}
	}
}
