package governance

import (
	"errors"
	"fmt"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func GovernanceCheck(policy, snapshot contract.Object) (contract.Object, error) {
	repo, err := object(snapshot, "repo")
	if err != nil {
		return nil, err
	}
	issues, err := objects(snapshot, "issues")
	if err != nil {
		return nil, err
	}
	prefixLabels, err := object(policy, "prefix_labels")
	if err != nil || len(prefixLabels) == 0 {
		return nil, errors.New("governance policy requires prefix_labels")
	}
	governanceLabels := map[string]bool{}
	for prefix, raw := range prefixLabels {
		if strings.TrimSpace(prefix) == "" {
			return nil, errors.New("governance prefix policy contains an empty prefix")
		}
		label, ok := raw.(string)
		if !ok || strings.TrimSpace(label) == "" {
			return nil, fmt.Errorf("governance prefix %q must map to a nonempty label", prefix)
		}
		governanceLabels[label] = true
	}
	retiredLabels, err := stringList(policy, "retired_labels", true)
	if err != nil {
		return nil, err
	}
	retired := namesSet(retiredLabels)
	statusField, err := requiredString(policy, "status_field")
	if err != nil {
		return nil, err
	}
	validStatuses, err := stringList(policy, "valid_statuses", false)
	if err != nil {
		return nil, err
	}
	priorityField, err := requiredString(policy, "priority_field")
	if err != nil {
		return nil, err
	}
	validPriorities, err := stringList(policy, "valid_priorities", false)
	if err != nil {
		return nil, err
	}
	statusSet, prioritySet := namesSet(validStatuses), namesSet(validPriorities)
	_, projectPresent := snapshot["project"]
	projectPresent = projectPresent && snapshot["project"] != nil
	if projectPresent {
		if _, ok := snapshot["project"].(map[string]any); !ok {
			return nil, errors.New("project must be an object or null")
		}
	}

	findings := []any{}
	seenNumbers := map[int64]bool{}
	for _, issue := range issues {
		state, err := requiredString(issue, "state")
		if err != nil || state != "OPEN" {
			return nil, errors.New("governance snapshot may contain only normalized open issues")
		}
		number, err := positiveInteger(issue, "number")
		if err != nil || seenNumbers[number] {
			return nil, errors.New("governance issue identity is missing or duplicated")
		}
		seenNumbers[number] = true
		title, err := requiredString(issue, "title")
		if err != nil {
			return nil, err
		}
		labels, err := labelNames(issue)
		if err != nil {
			return nil, err
		}
		prefix := issuePrefix(title)
		if prefix == "" {
			addFinding(&findings, "invalid-prefix", "warning", "Open issue title does not use a supported governance prefix.", false, issue, nil)
			removed := intersect(labels, retired)
			if len(removed) > 0 {
				addFinding(&findings, "retired-labels", "warning", "Open issue still uses retired labels: "+strings.Join(removed, ", ")+".", true, issue, contract.Object{"retired_labels": asAny(removed)})
			}
			continue
		}
		expected, hasExpected := prefixLabels[prefix].(string)
		if hasExpected && !labels[expected] {
			addFinding(&findings, "missing-governance-label", "warning", fmt.Sprintf("Open issue is missing the governance label %q implied by its title prefix.", expected), true, issue, contract.Object{"expected_label": expected})
		}
		conflicts := map[string]bool{}
		for label := range labels {
			if governanceLabels[label] && (!hasExpected || label != expected) {
				conflicts[label] = true
			}
		}
		conflicting := sortedStrings(conflicts)
		if len(conflicting) > 0 {
			addFinding(&findings, "conflicting-governance-labels", "warning", "Open issue has conflicting governance labels: "+strings.Join(conflicting, ", ")+".", true, issue, contract.Object{"conflicting_labels": asAny(conflicting)})
		}
		removed := intersect(labels, retired)
		if len(removed) > 0 {
			addFinding(&findings, "retired-labels", "warning", "Open issue still uses retired labels: "+strings.Join(removed, ", ")+".", true, issue, contract.Object{"retired_labels": asAny(removed)})
		}
		if projectPresent {
			inProject, err := boolValue(issue, "in_project")
			if err != nil {
				return nil, fmt.Errorf("issue %d in_project must be boolean when project data is supplied", number)
			}
			if !inProject {
				addFinding(&findings, "missing-backlog-membership", "warning", "Open typed issue is missing from the configured backlog project.", false, issue, nil)
			}
		}
	}

	if projectPresent {
		for _, issue := range issues {
			inProject, err := boolValue(issue, "in_project")
			if err != nil {
				return nil, errors.New("issue in_project must be boolean when project data is supplied")
			}
			if !inProject {
				continue
			}
			fields, err := object(issue, "field_values")
			if err != nil {
				return nil, err
			}
			status := fields[statusField]
			statusName, ok := status.(string)
			if !ok || !statusSet[statusName] {
				addFinding(&findings, "invalid-project-status", "error", fmt.Sprintf("Project item uses invalid %s %q.", statusField, printable(status)), false, issue, nil)
			}
			priority := fields[priorityField]
			priorityName, ok := priority.(string)
			if !ok || !prioritySet[priorityName] {
				addFinding(&findings, "invalid-project-priority", "error", fmt.Sprintf("Project item uses invalid %s %q.", priorityField, printable(priority)), false, issue, nil)
			}
		}
	}

	summary := findingSummary(findings)
	summary["open_issue_count"] = len(issues)
	summary["project_audited"] = projectPresent
	payload := contract.Object{"tool": "governance_check", "repo": repo, "summary": summary, "findings": findings}
	if projectPresent {
		payload["project"] = snapshot["project"]
	}
	if stamp, ok := snapshot["generated_at"].(string); ok {
		payload["generated_at"] = stamp
	}
	return payload, nil
}

func issuePrefix(title string) string {
	index := strings.IndexByte(title, ':')
	if index < 0 {
		return ""
	}
	return strings.TrimSpace(title[:index])
}

func intersect(left, right map[string]bool) []string {
	result := map[string]bool{}
	for name := range left {
		if right[name] {
			result[name] = true
		}
	}
	return sortedStrings(result)
}

func asAny(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

func printable(value any) string {
	if value == nil {
		return "None"
	}
	return fmt.Sprint(value)
}
