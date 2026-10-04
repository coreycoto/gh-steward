package governance

import (
	"errors"
	"fmt"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func GovernanceSummary(policy, snapshot contract.Object) (contract.Object, error) {
	mode := "post-fix"
	if raw, present := snapshot["mode"]; present {
		value, ok := raw.(string)
		if !ok || (value != "preview" && value != "post-fix") {
			return nil, errors.New("governance summary mode must be preview or post-fix")
		}
		mode = value
	}
	sections, err := objects(policy, "report_sections")
	if err != nil {
		return nil, err
	}
	if len(sections) == 0 {
		return nil, errors.New("governance summary policy requires report_sections")
	}
	keys, labels := map[string]bool{}, map[string]bool{}
	for _, section := range sections {
		key, err := requiredString(section, "key")
		if err != nil {
			return nil, err
		}
		label, err := requiredString(section, "label")
		if err != nil {
			return nil, err
		}
		if keys[key] || labels[label] {
			return nil, errors.New("governance summary section keys and labels must be unique")
		}
		keys[key], labels[label] = true, true
	}
	eventName, err := requiredString(snapshot, "event_name")
	if err != nil {
		return nil, err
	}
	projectAccess, err := boolValue(snapshot, "project_access")
	if err != nil {
		return nil, err
	}
	safeFixesEnabled, err := boolValue(snapshot, "safe_fixes_enabled")
	if err != nil {
		return nil, err
	}
	driftIssueEnabled, err := boolValue(snapshot, "drift_issue_enabled")
	if err != nil {
		return nil, err
	}
	quarterRolloverClean, err := boolValue(snapshot, "quarter_rollover_clean")
	if err != nil {
		return nil, err
	}
	tokenSource, err := requiredString(snapshot, "token_source")
	if err != nil {
		return nil, err
	}
	projectTitle, err := requiredString(snapshot, "project_title")
	if err != nil {
		return nil, err
	}
	projectNumber, err := requiredString(snapshot, "project_number")
	if err != nil {
		return nil, err
	}
	preReports, err := optionalObject(snapshot, "pre_reports")
	if err != nil {
		return nil, err
	}
	postReports, err := optionalObject(snapshot, "post_reports")
	if err != nil {
		return nil, err
	}
	fixResult, err := optionalObject(snapshot, "fix_result")
	if err != nil {
		return nil, err
	}
	if mode == "preview" && (len(postReports) > 0 || len(fixResult) > 0) {
		return nil, errors.New("preview summary cannot contain post-fix reports or applied-fix results")
	}
	preSummary, postSummary := contract.Object{}, contract.Object{}
	preFindings, postFindings, evaluatedFindings, evaluatedErrors := int64(0), int64(0), int64(0), int64(0)
	missingReports := []any{}
	postSectionLines := []string{}
	remainingDrift := []string{}
	reportHasFinding := map[string]bool{}
	for _, section := range sections {
		key := section["key"].(string)
		label := section["label"].(string)
		pre, ok, err := reportSummary(preReports, key)
		if err != nil {
			return nil, err
		}
		if ok {
			preSummary[key] = pre
			preFindings += summaryCount(pre, "finding_count")
		}
		post, ok, err := reportSummary(postReports, key)
		if err != nil {
			return nil, err
		}
		if ok {
			postSummary[key] = post
			postFindings += summaryCount(post, "finding_count")
		}
		evaluated, reports := post, postReports
		if mode == "preview" {
			evaluated, reports = pre, preReports
		}
		if evaluated == nil {
			postSectionLines = append(postSectionLines, "- "+label+": not run")
			missingReports = append(missingReports, key)
			continue
		}
		findings := summaryCount(evaluated, "finding_count")
		evaluatedFindings += findings
		evaluatedErrors += summaryCount(evaluated, "error_count")
		reportHasFinding[key] = findings > 0
		postSectionLines = append(postSectionLines, fmt.Sprintf("- %s: findings=%d, errors=%d, fixable=%d", label, findings, summaryCount(evaluated, "error_count"), summaryCount(evaluated, "fixable_count")))
		if findings > 0 {
			if report, exists := reports[key].(map[string]any); exists {
				items, _ := report["findings"].([]any)
				for _, raw := range items {
					finding, ok := raw.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("evaluated report %q findings must contain objects", key)
					}
					message, _ := finding["message"].(string)
					if message == "" {
						message, _ = finding["code"].(string)
					}
					if message == "" {
						return nil, fmt.Errorf("evaluated report %q finding requires message or code", key)
					}
					line := "  - " + message
					if number, exists := finding["number"]; exists && number != nil {
						line += " (#" + fmt.Sprint(number) + ")"
					}
					remainingDrift = append(remainingDrift, line)
				}
			}
		}
	}
	fixFailed := false
	if len(fixResult) > 0 {
		summary, ok := fixResult["summary"].(map[string]any)
		if !ok {
			return nil, errors.New("fix_result requires a summary object")
		}
		failedRaw, ok := summary["failed_count"]
		if !ok {
			failedRaw, ok = summary["failure_count"]
		}
		failedCount, err := contract.Integer(failedRaw)
		if !ok || err != nil || failedCount < 0 {
			return nil, errors.New("fix_result summary requires a nonnegative failed_count")
		}
		fixFailed = failedCount > 0
	}
	unresolved := !projectAccess || evaluatedFindings > 0 || evaluatedErrors > 0 || len(missingReports) > 0 || fixFailed
	fixPass, driftMaintenance, checksHeading := enabled(safeFixesEnabled), enabled(driftIssueEnabled), "## Post-fix checks"
	if mode == "preview" {
		fixPass, driftMaintenance, checksHeading = "preview only", "preview only", "## Preview checks"
	}
	markdown := []string{
		"# Governance Summary", "", "## Summary", "",
		"- trigger: " + eventName,
		"- safe-fix pass: " + fixPass,
		"- drift issue maintenance: " + driftMaintenance,
		"- project: " + projectTitle,
		"- project number: " + projectNumber,
		"- project token source: " + tokenSource,
		fmt.Sprintf("- project access via %s: %s", tokenSource, available(projectAccess)),
		"- milestone rollover: " + cleanState(quarterRolloverClean),
	}
	if !projectAccess {
		markdown = append(markdown, "- note: project-backed governance checks were skipped because access was unavailable")
	}
	if len(fixResult) > 0 {
		fixSummary, _ := fixResult["summary"].(map[string]any)
		markdown = append(markdown,
			fmt.Sprintf("- safe fixes applied: %d", summaryCount(fixSummary, "applied_count")),
			fmt.Sprintf("- safe fixes failed: %d", summaryCount(fixSummary, "failed_count")),
		)
	}
	markdown = append(markdown, fmt.Sprintf("- unresolved manual drift: %t", unresolved), "", checksHeading, "")
	markdown = append(markdown, postSectionLines...)
	markdown = append(markdown, "", "## Remaining drift", "")
	if !unresolved {
		if mode == "preview" {
			markdown = append(markdown, "- No unresolved manual drift was found in the complete preview checks.")
		} else {
			markdown = append(markdown, "- No unresolved manual drift remains after the reviewed fix pass.")
		}
	} else if len(remainingDrift) == 0 {
		markdown = append(markdown, "- No finding details were supplied; access, missing checks, report errors or fix failures remain unresolved.")
	} else {
		markdown = append(markdown, remainingDrift...)
	}
	if guidance, ok, err := summaryGuidance(policy, reportHasFinding); err != nil {
		return nil, err
	} else if ok {
		markdown = append(markdown, "", guidance["heading"].(string), "")
		if link, ok := guidance["workflow_link"].(string); ok && link != "" {
			label := guidance["workflow_label"].(string)
			markdown = append(markdown, fmt.Sprintf("- %s: [%s](%s)", guidance["preview_instruction"], label, link))
		}
		if link, ok := guidance["guide_link"].(string); ok && link != "" {
			label := guidance["guide_label"].(string)
			markdown = append(markdown, fmt.Sprintf("- operator guidance: [%s](%s)", label, link))
		}
		if instruction, ok := guidance["apply_instruction"].(string); ok && instruction != "" {
			markdown = append(markdown, "- "+instruction)
		}
	}

	status := contract.Object{
		"mode":       mode,
		"event_name": eventName, "project_access": projectAccess,
		"safe_fixes_enabled": safeFixesEnabled, "drift_issue_enabled": driftIssueEnabled,
		"token_source": tokenSource, "project_title": projectTitle,
		"project_number": projectNumber, "quarter_rollover_clean": quarterRolloverClean,
		"unresolved_manual_drift": unresolved, "post_findings": postFindings,
		"pre_findings": preFindings, "evaluated_findings": evaluatedFindings, "evaluated_errors": evaluatedErrors, "missing_reports": missingReports,
		"pre_reports": preSummary, "post_reports": postSummary,
		"fix_summary": nil,
	}
	if len(fixResult) > 0 {
		status["fix_summary"] = fixResult["summary"]
	}
	return contract.Object{"markdown": strings.Join(markdown, "\n") + "\n", "status": status}, nil
}

func reportSummary(reports contract.Object, key string) (contract.Object, bool, error) {
	raw, exists := reports[key]
	if !exists || raw == nil {
		return nil, false, nil
	}
	report, ok := raw.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("report %q must be an object or null", key)
	}
	summary, ok := report["summary"].(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("report %q requires a summary object", key)
	}
	for _, name := range []string{"finding_count", "error_count", "fixable_count"} {
		if n, err := contract.Integer(summary[name]); err != nil || n < 0 {
			return nil, false, fmt.Errorf("report %q summary.%s must be a nonnegative integer", key, name)
		}
	}
	return summary, true, nil
}

func summaryCount(summary contract.Object, key string) int64 {
	if summary == nil {
		return 0
	}
	value, err := contract.Integer(summary[key])
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func summaryGuidance(policy contract.Object, reports map[string]bool) (contract.Object, bool, error) {
	raw, exists := policy["guidance"]
	if !exists || raw == nil {
		return nil, false, nil
	}
	guidance, ok := raw.(map[string]any)
	if !ok {
		return nil, false, errors.New("guidance must be an object or null")
	}
	key, err := requiredString(guidance, "report_key")
	if err != nil {
		return nil, false, err
	}
	if !reports[key] {
		return nil, false, nil
	}
	for _, field := range []string{"heading", "preview_instruction", "apply_instruction"} {
		if _, err := requiredString(guidance, field); err != nil {
			return nil, false, err
		}
	}
	if link, exists := guidance["workflow_link"]; exists && link != nil && link != "" {
		if _, ok := link.(string); !ok {
			return nil, false, errors.New("guidance.workflow_link must be a string")
		}
		if _, err := requiredString(guidance, "workflow_label"); err != nil {
			return nil, false, err
		}
	}
	if link, exists := guidance["guide_link"]; exists && link != nil && link != "" {
		if _, ok := link.(string); !ok {
			return nil, false, errors.New("guidance.guide_link must be a string")
		}
		if _, err := requiredString(guidance, "guide_label"); err != nil {
			return nil, false, err
		}
	}
	return guidance, true, nil
}

func enabled(value bool) string {
	if value {
		return "enabled"
	}
	return "disabled"
}

func available(value bool) string {
	if value {
		return "available"
	}
	return "unavailable"
}

func cleanState(value bool) string {
	if value {
		return "clean"
	}
	return "needs manual attention"
}
