package governance

import (
	"errors"
	"fmt"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func MilestoneCheck(policy, snapshot contract.Object) (contract.Object, error) {
	repo, err := object(snapshot, "repo")
	if err != nil {
		return nil, err
	}
	current, err := requiredString(policy, "current_milestone")
	if err != nil {
		return nil, err
	}
	next, err := requiredString(policy, "next_milestone")
	if err != nil {
		return nil, err
	}
	quarterMilestones, err := stringList(policy, "quarter_milestones", false)
	if err != nil {
		return nil, err
	}
	quarterSet := namesSet(quarterMilestones)
	if !quarterSet[current] || !quarterSet[next] {
		return nil, errors.New("quarter_milestones must include current_milestone and next_milestone")
	}
	expectedEntries, err := objects(policy, "expected_milestones")
	if err != nil {
		return nil, err
	}
	expected := map[string]contract.Object{}
	for _, item := range expectedEntries {
		title, err := requiredString(item, "title")
		if err != nil {
			return nil, err
		}
		if _, exists := expected[title]; exists {
			return nil, fmt.Errorf("milestone policy contains duplicate title %q", title)
		}
		dueOn, err := requiredString(item, "due_on")
		if err != nil || len(dueOn) < 10 {
			return nil, fmt.Errorf("expected milestone %q requires an ISO due_on date", title)
		}
		descriptionPrefixes, err := stringList(item, "description_prefixes", false)
		if err != nil {
			return nil, fmt.Errorf("expected milestone %q requires description_prefixes", title)
		}
		descriptionTokens, err := stringList(item, "description_tokens", false)
		if err != nil {
			return nil, fmt.Errorf("expected milestone %q requires description_tokens", title)
		}
		expected[title] = contract.Object{"title": title, "due_on": dueOn, "description_prefixes": asAny(descriptionPrefixes), "description_tokens": asAny(descriptionTokens)}
	}
	for _, title := range []string{current, next} {
		if _, ok := expected[title]; !ok {
			return nil, fmt.Errorf("expected_milestones must include %q", title)
		}
	}
	epicPrefixes, err := stringList(policy, "epic_prefixes", true)
	if err != nil {
		return nil, err
	}
	priorityField, err := requiredString(policy, "priority_field")
	if err != nil {
		return nil, err
	}
	laterPriority, err := requiredString(policy, "later_priority")
	if err != nil {
		return nil, err
	}
	rationaleMarkers, err := stringList(policy, "rationale_markers", false)
	if err != nil {
		return nil, err
	}
	rationaleRules, err := milestoneRationaleRules(policy)
	if err != nil {
		return nil, err
	}
	milestones, err := objects(snapshot, "milestones")
	if err != nil {
		return nil, err
	}
	issues, err := objects(snapshot, "issues")
	if err != nil {
		return nil, err
	}
	observed := map[string]contract.Object{}
	for _, item := range milestones {
		title, err := requiredString(item, "title")
		if err != nil {
			return nil, err
		}
		if _, exists := observed[title]; exists {
			return nil, fmt.Errorf("milestone snapshot contains duplicate title %q", title)
		}
		observed[title] = item
	}
	projectPresent := snapshot["project"] != nil
	if raw, ok := snapshot["project"]; ok && raw != nil {
		if _, ok := raw.(map[string]any); !ok {
			return nil, errors.New("project must be an object or null")
		}
	}

	findings := []any{}
	for _, title := range []string{current, next} {
		want := expected[title]
		got, exists := observed[title]
		if !exists {
			addFinding(&findings, "missing-quarter-milestone", "warning", fmt.Sprintf("Open milestone %q is missing.", title), true, nil, nil)
			continue
		}
		dueOn, _, err := optionalString(got, "due_on")
		if err != nil {
			return nil, err
		}
		if len(dueOn) < 10 || dueOn[:10] != want["due_on"].(string)[:10] {
			addFinding(&findings, "stale-quarter-due-date", "warning", fmt.Sprintf("Milestone %q has a stale due date.", title), true, nil, nil)
		}
		description, _, err := optionalString(got, "description")
		if err != nil {
			return nil, err
		}
		prefixes, _ := contract.Strings(want["description_prefixes"])
		tokens, _ := contract.Strings(want["description_tokens"])
		if milestoneDescriptionStale(description, prefixes, tokens) {
			addFinding(&findings, "stale-quarter-description", "warning", fmt.Sprintf("Milestone %q description does not match policy.", title), true, nil, nil)
		}
	}
	for _, item := range milestones {
		title := item["title"].(string)
		if !quarterSet[title] {
			continue
		}
		empty, err := noOpenIssues(item["open_issues"])
		if err != nil {
			return nil, fmt.Errorf("milestone %q open_issues: %w", title, err)
		}
		if empty {
			addFinding(&findings, "empty-quarter-milestone", "info", fmt.Sprintf("Quarter milestone %q is currently empty and may be a retirement candidate.", title), false, nil, nil)
		}
	}
	issueDetails, err := optionalObject(snapshot, "issue_details")
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	for _, issue := range issues {
		state, err := requiredString(issue, "state")
		if err != nil || state != "OPEN" {
			return nil, errors.New("milestone snapshot may contain only normalized open issues")
		}
		number, err := positiveInteger(issue, "number")
		if err != nil || seen[number] {
			return nil, errors.New("milestone issue identity is missing or duplicated")
		}
		seen[number] = true
		milestone, _, err := optionalString(issue, "milestone")
		if err != nil {
			return nil, err
		}
		if !quarterSet[milestone] {
			continue
		}
		title, err := requiredString(issue, "title")
		if err != nil {
			return nil, err
		}
		body, comments, err := issueText(issue, issueDetails, number)
		if err != nil {
			return nil, err
		}
		if !hasMilestoneRationale(strings.Join(append([]string{body}, comments...), "\n"), milestone, rationaleMarkers, rationaleRules) {
			addFinding(&findings, "missing-quarter-rationale", "warning", "Issue carries a quarter milestone without explicit quarter rationale in the body or comments.", false, issue, nil)
		}
		prefix := issuePrefix(title)
		if contains(epicPrefixes, prefix) {
			children := []any{}
			if raw, exists := issue["child_numbers"]; exists {
				children, err = contract.Array(contract.Object{"child_numbers": raw}, "child_numbers")
				if err != nil {
					return nil, err
				}
			}
			openChildren := []any{}
			for _, raw := range children {
				if _, err := contract.PositiveInteger(raw); err != nil {
					return nil, errors.New("child_numbers entries must be positive issue numbers")
				}
				openChildren = append(openChildren, raw)
			}
			if len(openChildren) > 0 {
				addFinding(&findings, "umbrella-holds-quarter-milestone", "warning", fmt.Sprintf("Umbrella issue carries a quarter milestone while open child issues exist: %s.", joinNumbers(openChildren)), false, issue, contract.Object{"child_numbers": openChildren})
			}
		}
		fields, err := optionalObject(issue, "field_values")
		if err != nil {
			return nil, err
		}
		if fields[priorityField] == laterPriority {
			addFinding(&findings, "later-issue-has-quarter-milestone", "warning", "Issue is in the caller's later priority band but still carries a quarter milestone.", false, issue, nil)
		}
	}

	summary := findingSummary(findings)
	summary["current_quarter"] = current
	summary["next_quarter"] = next
	summary["open_milestone_count"] = len(milestones)
	summary["quarter_issue_count"] = countQuarterIssues(issues, quarterSet)
	summary["project_audited"] = projectPresent
	payload := contract.Object{"tool": "milestone_check", "repo": repo, "summary": summary, "findings": findings}
	if projectPresent {
		payload["project"] = snapshot["project"]
	}
	if stamp, ok := snapshot["generated_at"].(string); ok {
		payload["generated_at"] = stamp
	}
	return payload, nil
}

func optionalObject(o contract.Object, key string) (contract.Object, error) {
	raw, ok := o[key]
	if !ok || raw == nil {
		return contract.Object{}, nil
	}
	value, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object or null", key)
	}
	return value, nil
}

func noOpenIssues(raw any) (bool, error) {
	switch value := raw.(type) {
	case nil:
		return true, nil
	case []any:
		return len(value) == 0, nil
	default:
		n, err := contract.Integer(raw)
		if err != nil || n < 0 {
			return false, errors.New("must be an array or nonnegative integer count")
		}
		return n == 0, nil
	}
}

func milestoneDescriptionStale(description string, prefixes, requiredTokens []string) bool {
	normalized := strings.TrimSpace(description)
	if normalized == "" {
		return true
	}
	for _, expected := range prefixes {
		if !strings.Contains(normalized, expected) {
			return true
		}
	}
	for _, token := range requiredTokens {
		if !strings.Contains(normalized, token) {
			return true
		}
	}
	return false
}

func issueText(issue contract.Object, detailsByNumber contract.Object, number int64) (string, []string, error) {
	details := contract.Object{}
	if raw, ok := detailsByNumber[fmt.Sprint(number)]; ok {
		var valid bool
		details, valid = raw.(map[string]any)
		if !valid {
			return "", nil, errors.New("issue_details entries must be objects")
		}
	}
	body, _, err := optionalString(details, "body")
	if err != nil {
		return "", nil, err
	}
	if body == "" {
		body, _, err = optionalString(issue, "body")
		if err != nil {
			return "", nil, err
		}
	}
	commentsRaw, exists := details["comments"]
	if !exists {
		commentsRaw = issue["comments"]
	}
	comments := []string{}
	if commentsRaw != nil {
		items, ok := commentsRaw.([]any)
		if !ok {
			return "", nil, errors.New("issue comments must be an array")
		}
		for _, raw := range items {
			comment, ok := raw.(map[string]any)
			if !ok {
				return "", nil, errors.New("issue comment entries must be objects")
			}
			value, _, err := optionalString(comment, "body")
			if err != nil {
				return "", nil, err
			}
			comments = append(comments, value)
		}
	}
	return body, comments, nil
}

type milestoneRationaleRule struct {
	markers          []string
	requireMilestone bool
}

func milestoneRationaleRules(policy contract.Object) ([]milestoneRationaleRule, error) {
	if _, exists := policy["rationale_rules"]; !exists {
		return nil, nil
	}
	rows, err := objects(policy, "rationale_rules")
	if err != nil {
		return nil, err
	}
	rules := make([]milestoneRationaleRule, 0, len(rows))
	for _, row := range rows {
		if len(row) != 2 {
			return nil, errors.New("rationale_rules entries require exactly markers and require_milestone")
		}
		markers, err := stringList(row, "markers", false)
		if err != nil {
			return nil, err
		}
		requireMilestone, err := contract.Bool(row, "require_milestone")
		if err != nil {
			return nil, err
		}
		rules = append(rules, milestoneRationaleRule{markers: markers, requireMilestone: requireMilestone})
	}
	return rules, nil
}

func hasMilestoneRationale(text, milestone string, markers []string, rules []milestoneRationaleRule) bool {
	normalized := strings.ToLower(text)
	for _, marker := range markers {
		if strings.Contains(normalized, strings.ToLower(marker)) {
			return true
		}
	}
	for _, rule := range rules {
		matches := !rule.requireMilestone || strings.Contains(normalized, strings.ToLower(milestone))
		for _, marker := range rule.markers {
			matches = matches && strings.Contains(normalized, strings.ToLower(marker))
		}
		if matches {
			return true
		}
	}
	return false
}

func contains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func joinNumbers(values []any) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = fmt.Sprint(value)
	}
	return strings.Join(parts, ", ")
}

func countQuarterIssues(issues []contract.Object, quarterSet map[string]bool) int {
	count := 0
	for _, issue := range issues {
		if milestone, ok := issue["milestone"].(string); ok && quarterSet[milestone] {
			count++
		}
	}
	return count
}
