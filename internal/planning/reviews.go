package planning

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var reviewIssueTypes = []string{"Initiative", "Epic", "Research", "Enhancement", "Bug", "Maintenance"}
var reviewDestinations = map[string]string{
	"agents-note": "agents-note", "agent-note": "agents-note", "agents_note": "agents-note",
	"engineering-doc": "engineering-doc", "engineering_doc": "engineering-doc",
	"skill-update": "skill-update", "skill_update": "skill-update",
	"shared-reference": "shared-reference", "shared_reference": "shared-reference",
	"backlog": "backlog", "none": "none",
}
var reviewTypeSet = stringSet(reviewIssueTypes)
var reviewSeverityRank = map[string]int{"critical": 0, "high": 1, "now": 2, "medium": 3, "next": 4, "low": 5, "later": 6}
var reviewPriorityRank = map[string]int{"Now": 0, "Next": 1, "Later": 2}

// NormalizeReviewFindings validates and normalizes structured review findings.
func NormalizeReviewFindings(payload contract.Object) (contract.Object, error) {
	version, err := contract.Integer(payload["schema_version"])
	if err != nil || version != 1 {
		return nil, fmt.Errorf("structured review findings must declare schema_version: 1")
	}
	rawFindings, err := contract.Objects(payload, "findings")
	if err != nil || len(rawFindings) == 0 {
		return nil, fmt.Errorf("structured review findings require a non-empty findings list")
	}
	scope := "repo"
	if raw, exists := payload["scope"]; exists && raw != nil {
		if value, ok := raw.(string); ok && strings.TrimSpace(value) != "" {
			scope = strings.TrimSpace(value)
		}
	}
	normalized := make([]any, 0, len(rawFindings))
	groups := map[string]contract.Object{}
	for i, raw := range rawFindings {
		title, err := contract.Nonempty(raw, "title")
		if err != nil {
			return nil, fmt.Errorf("finding #%d is missing a non-empty title", i+1)
		}
		summary, err := contract.Nonempty(raw, "summary")
		if err != nil {
			return nil, fmt.Errorf("finding #%d is missing a non-empty summary", i+1)
		}
		issueType, err := reviewType(raw, "issue_type", i)
		if err != nil {
			return nil, err
		}
		severity, err := reviewSeverity(raw, "severity", i)
		if err != nil {
			return nil, err
		}
		groupKey, err := reviewOptionalTrimmedString(raw, "group_key")
		if err != nil {
			return nil, fmt.Errorf("finding #%d group_key must be a string or null", i+1)
		}
		backlogTitle, err := reviewOptionalTrimmedString(raw, "backlog_title")
		if err != nil {
			return nil, fmt.Errorf("finding #%d backlog_title must be a string or null", i+1)
		}
		if (groupKey == nil) != (backlogTitle == nil) {
			return nil, fmt.Errorf("finding #%d: group_key and backlog_title must either both be set or both be omitted", i+1)
		}
		body, err := reviewOptionalText(raw, "body")
		if err != nil {
			return nil, fmt.Errorf("finding #%d body must be a string", i+1)
		}
		notes, err := reviewOptionalText(raw, "notes")
		if err != nil {
			return nil, fmt.Errorf("finding #%d notes must be a string", i+1)
		}
		files, err := reviewFiles(raw["files"])
		if err != nil {
			return nil, fmt.Errorf("finding #%d files must be an array", i+1)
		}
		evidence, err := reviewEvidence(raw["evidence"])
		if err != nil {
			return nil, fmt.Errorf("finding #%d: %w", i+1, err)
		}
		existingIssue, err := reviewOptionalPositive(raw, "existing_issue", i)
		if err != nil {
			return nil, err
		}
		parentIssue, err := reviewOptionalPositive(raw, "parent_issue_number", i)
		if err != nil {
			return nil, err
		}
		blockers, err := reviewPositiveNumbers(raw, "blocked_by_issue_numbers", i)
		if err != nil {
			return nil, err
		}
		id := fmt.Sprintf("finding-%d", i+1)
		if rawID, exists := raw["id"]; exists && rawID != nil {
			value, ok := rawID.(string)
			if !ok {
				return nil, fmt.Errorf("finding #%d id must be a string", i+1)
			}
			if strings.TrimSpace(value) != "" {
				id = strings.TrimSpace(value)
			}
		}
		canonicalTitle := reviewTitleWithPrefix(issueType, strings.TrimSpace(title))
		var groupKeyValue, backlogValue, canonicalBacklog any
		if groupKey != nil {
			groupKeyValue = *groupKey
		}
		if backlogTitle != nil {
			backlogValue, canonicalBacklog = *backlogTitle, reviewTitleWithPrefix(issueType, *backlogTitle)
		}
		finding := contract.Object{
			"id": id, "title": strings.TrimSpace(title), "canonical_title": canonicalTitle,
			"issue_type": issueType, "severity": severity, "summary": strings.TrimSpace(summary),
			"group_key": groupKeyValue, "backlog_title": backlogValue, "canonical_backlog_title": canonicalBacklog,
			"body": body, "files": stringSliceAsAny(files), "evidence": evidence,
			"existing_issue": optionalIntAny(existingIssue), "notes": notes,
			"blocked_by_issue_numbers": intSliceAsAny(blockers), "parent_issue_number": optionalIntAny(parentIssue),
		}
		normalized = append(normalized, finding)
		if groupKey == nil {
			continue
		}
		group := groups[*groupKey]
		if group == nil {
			group = contract.Object{"issue_type": issueType, "canonical_backlog_title": canonicalBacklog, "existing_issue": optionalIntAny(existingIssue), "parent_issue_number": optionalIntAny(parentIssue)}
			groups[*groupKey] = group
		}
		if group["issue_type"] != issueType {
			return nil, fmt.Errorf("group_key %q mixes issue types", *groupKey)
		}
		if group["canonical_backlog_title"] != canonicalBacklog {
			return nil, fmt.Errorf("group_key %q mixes backlog titles", *groupKey)
		}
		if old, _ := contract.Integer(group["existing_issue"]); old > 0 && existingIssue != nil && old != *existingIssue {
			return nil, fmt.Errorf("group_key %q mixes existing_issue values", *groupKey)
		}
		if old, _ := contract.Integer(group["parent_issue_number"]); old > 0 && parentIssue != nil && old != *parentIssue {
			return nil, fmt.Errorf("group_key %q mixes parent_issue_number values", *groupKey)
		}
		if group["existing_issue"] == nil && existingIssue != nil {
			group["existing_issue"] = *existingIssue
		}
		if group["parent_issue_number"] == nil && parentIssue != nil {
			group["parent_issue_number"] = *parentIssue
		}
	}
	return contract.Object{"schema_version": int64(1), "scope": scope, "findings": normalized}, nil
}

func reviewType(raw contract.Object, key string, index int) (string, error) {
	value, err := contract.String(raw, key)
	if err != nil {
		return "", fmt.Errorf("finding #%d: unsupported issue_type", index+1)
	}
	value = strings.ToLower(strings.TrimRight(strings.TrimSpace(value), ":"))
	for _, issueType := range reviewIssueTypes {
		if strings.ToLower(issueType) == value {
			return issueType, nil
		}
	}
	return "", fmt.Errorf("finding #%d: unsupported issue_type %q", index+1, value)
}

func reviewSeverity(raw contract.Object, key string, index int) (string, error) {
	value, err := contract.String(raw, key)
	if err != nil {
		return "", fmt.Errorf("finding #%d: unsupported severity", index+1)
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if _, ok := reviewSeverityRank[value]; !ok {
		return "", fmt.Errorf("finding #%d: unsupported severity %q", index+1, value)
	}
	return value, nil
}

func reviewTitleWithPrefix(issueType, title string) string {
	title = strings.TrimSpace(title)
	if strings.HasPrefix(title, issueType+":") {
		return title
	}
	return issueType + ": " + title
}

func reviewOptionalTrimmedString(raw contract.Object, key string) (*string, error) {
	value, exists := raw[key]
	if !exists || value == nil {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be a string or null", key)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	return &text, nil
}

func reviewOptionalText(raw contract.Object, key string) (string, error) {
	value, exists := raw[key]
	if !exists || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string or null", key)
	}
	return strings.TrimSpace(text), nil
}

func reviewOptionalPositive(raw contract.Object, key string, index int) (*int64, error) {
	value, exists := raw[key]
	if !exists || value == nil {
		return nil, nil
	}
	number, err := contract.PositiveInteger(value)
	if err != nil {
		return nil, fmt.Errorf("finding #%d: %s must be a positive whole-number issue reference or null", index+1, key)
	}
	return &number, nil
}

func reviewPositiveNumbers(raw contract.Object, key string, index int) ([]int64, error) {
	value, exists := raw[key]
	if !exists || value == nil {
		return []int64{}, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("finding #%d: %s must be a list of issue references or null", index+1, key)
	}
	set := map[int64]bool{}
	for _, item := range values {
		number, err := contract.PositiveInteger(item)
		if err != nil {
			return nil, fmt.Errorf("finding #%d: %s must contain only positive whole-number issue references", index+1, key)
		}
		set[number] = true
	}
	result := make([]int64, 0, len(set))
	for number := range set {
		result = append(result, number)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func reviewFiles(raw any) ([]string, error) {
	if raw == nil {
		return []string{}, nil
	}
	values, ok := raw.([]any)
	if !ok {
		if typed, ok := raw.([]string); ok {
			values = make([]any, len(typed))
			for i, value := range typed {
				values[i] = value
			}
		} else {
			return nil, fmt.Errorf("value must be an array")
		}
	}
	set := map[string]bool{}
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			set[strings.TrimSpace(text)] = true
		}
	}
	result := make([]string, 0, len(set))
	for text := range set {
		result = append(result, text)
	}
	sort.Strings(result)
	return result, nil
}

func reviewEvidence(raw any) ([]any, error) {
	values, ok := raw.([]any)
	if !ok {
		if raw == nil {
			return []any{}, nil
		}
		return nil, fmt.Errorf("evidence must be an array")
	}
	result := []contract.Object{}
	for _, value := range values {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		path, ok := entry["path"].(string)
		if !ok {
			if entry["path"] == nil || entry["path"] == "" {
				continue
			}
			return nil, fmt.Errorf("evidence path must be a string")
		}
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		item := contract.Object{"path": path}
		if number, err := contract.PositiveInteger(entry["line"]); err == nil {
			item["line"] = number
		}
		if detail, ok := entry["detail"].(string); ok && strings.TrimSpace(detail) != "" {
			item["detail"] = strings.TrimSpace(detail)
		} else if summary, ok := entry["summary"].(string); ok && strings.TrimSpace(summary) != "" {
			item["summary"] = strings.TrimSpace(summary)
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a["path"] != b["path"] {
			return a["path"].(string) < b["path"].(string)
		}
		al, _ := contract.Integer(a["line"])
		bl, _ := contract.Integer(b["line"])
		if al != bl {
			return al < bl
		}
		ad, _ := a["detail"].(string)
		bd, _ := b["detail"].(string)
		if ad == "" {
			ad, _ = a["summary"].(string)
		}
		if bd == "" {
			bd, _ = b["summary"].(string)
		}
		return ad < bd
	})
	out := make([]any, len(result))
	for i, value := range result {
		out[i] = value
	}
	return out, nil
}

func optionalIntAny(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// BuildLegacyReviewBacklogDelta translates the retained {kind,severity}
// compatibility format. All consumer type/label vocabulary is explicit.
func BuildLegacyReviewBacklogDelta(payload, policy contract.Object) (contract.Object, error) {
	version, err := contract.Integer(payload["schema_version"])
	if err != nil || version != 1 {
		return nil, fmt.Errorf("review findings payload must declare schema_version: 1")
	}
	findings, err := contract.Objects(payload, "findings")
	if err != nil || len(findings) == 0 {
		return nil, fmt.Errorf("review findings payload requires a non-empty findings list")
	}
	kindTypes, err := contract.ObjectAt(policy, "legacy_kind_to_type")
	if err != nil {
		return nil, err
	}
	kindLabels, err := contract.ObjectAt(policy, "legacy_kind_to_label")
	if err != nil {
		return nil, err
	}
	priorities, err := contract.ObjectAt(policy, "legacy_severity_to_priority")
	if err != nil {
		return nil, err
	}
	issues := []any{}
	for i, finding := range findings {
		title, err := contract.Nonempty(finding, "title")
		if err != nil {
			return nil, fmt.Errorf("finding #%d must declare title", i+1)
		}
		summary, err := contract.Nonempty(finding, "summary")
		if err != nil {
			return nil, fmt.Errorf("finding #%d must declare summary", i+1)
		}
		kind, err := contract.Nonempty(finding, "kind")
		if err != nil {
			return nil, fmt.Errorf("finding #%d must declare kind", i+1)
		}
		kind = strings.ToLower(strings.TrimSpace(kind))
		severity := "medium"
		if raw, ok := finding["severity"]; ok && raw != nil {
			value, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("finding #%d severity must be a string", i+1)
			}
			if strings.TrimSpace(value) != "" {
				severity = strings.ToLower(strings.TrimSpace(value))
			}
		}
		typeName, ok := kindTypes[kind].(string)
		if !ok || !reviewTypeSet[typeName] {
			return nil, fmt.Errorf("unsupported review finding kind: %s", kind)
		}
		label, ok := kindLabels[kind].(string)
		if !ok || strings.TrimSpace(label) == "" {
			return nil, fmt.Errorf("legacy policy has no label for kind %q", kind)
		}
		priority, ok := priorities[severity].(string)
		if !ok || !reviewPriorityRankValid(priority) {
			return nil, fmt.Errorf("legacy policy has no valid priority for severity %q", severity)
		}
		epic, err := reviewOptionalText(finding, "epic")
		if err != nil {
			return nil, err
		}
		var parent any
		if strings.TrimSpace(epic) != "" {
			parent = strings.TrimSpace(epic)
		}
		title, err = reviewNormalizedIssueTitle(typeName, title)
		if err != nil {
			return nil, err
		}
		if err := reviewLegacyParent(typeName, parent); err != nil {
			return nil, err
		}
		issues = append(issues, contract.Object{"action": "create", "type": typeName, "title": title, "body": summary, "epic": parent, "labels": []any{label}, "milestone": nil, "priority": priority, "status": "Todo"})
	}
	sort.Slice(issues, func(i, j int) bool {
		return issues[i].(contract.Object)["title"].(string) < issues[j].(contract.Object)["title"].(string)
	})
	return contract.Object{"schema_version": int64(1), "source": "review-findings", "issues": issues, "notes": []any{"Generated from deterministic repo review findings."}}, nil
}

func reviewPriorityRankValid(priority string) bool { _, ok := reviewPriorityRank[priority]; return ok }

// BuildReviewBacklogDelta converts normalized findings plus an explicit issue
// catalog and caller-owned taxonomy/priority policy into a provider-free
// preview. Catalog mode is evidence only; the result is not an apply grant.
func BuildReviewBacklogDelta(input contract.Object) (contract.Object, error) {
	findingsPayload, err := contract.ObjectAt(input, "findings")
	if err != nil {
		return nil, err
	}
	normalized, err := NormalizeReviewFindings(findingsPayload)
	if err != nil {
		return nil, err
	}
	repo, err := contract.ObjectAt(input, "repo")
	if err != nil {
		return nil, err
	}
	if _, err := contract.ParseRepository(repo); err != nil {
		return nil, err
	}
	projectTitle, err := contract.Nonempty(input, "project_title")
	if err != nil {
		return nil, err
	}
	mode, err := contract.String(input, "issue_catalog_mode")
	if err != nil || (mode != "live" && mode != "empty_snapshot" && mode != "unknown_snapshot") {
		return nil, fmt.Errorf("issue_catalog_mode must be live, empty_snapshot, or unknown_snapshot")
	}
	issues, err := contract.Objects(input, "issue_catalog")
	if err != nil {
		return nil, err
	}
	issueByNumber, bySlug, err := reviewIssueCatalog(issues)
	if err != nil {
		return nil, err
	}
	policy, err := contract.ObjectAt(input, "policy")
	if err != nil {
		return nil, err
	}
	priorityBySeverity, err := contract.ObjectAt(policy, "severity_to_priority")
	if err != nil {
		return nil, err
	}
	labelsByType, err := contract.ObjectAt(policy, "issue_type_labels")
	if err != nil {
		return nil, err
	}
	for severity := range reviewSeverityRank {
		value, ok := priorityBySeverity[severity].(string)
		if !ok || !reviewPriorityRankValid(value) {
			return nil, fmt.Errorf("policy severity_to_priority must map %s to Now, Next, or Later", severity)
		}
	}
	normalizedFindings, _ := contract.Objects(normalized, "findings")
	sort.Slice(normalizedFindings, func(i, j int) bool {
		pi := priorityBySeverity[normalizedFindings[i]["severity"].(string)].(string)
		pj := priorityBySeverity[normalizedFindings[j]["severity"].(string)].(string)
		if reviewPriorityRank[pi] != reviewPriorityRank[pj] {
			return reviewPriorityRank[pi] < reviewPriorityRank[pj]
		}
		ti := reviewProposalTitle(normalizedFindings[i])
		tj := reviewProposalTitle(normalizedFindings[j])
		if ti != tj {
			return ti < tj
		}
		return normalizedFindings[i]["id"].(string) < normalizedFindings[j]["id"].(string)
	})
	groups := []contract.Object{}
	groupByKey := map[string]contract.Object{}
	for _, finding := range normalizedFindings {
		issueType := finding["issue_type"].(string)
		label, ok := labelsByType[issueType].(string)
		if !ok || strings.TrimSpace(label) == "" {
			return nil, fmt.Errorf("policy issue_type_labels has no label for %q", issueType)
		}
		key := "title:" + reviewSlug(reviewProposalTitle(finding))
		if group, ok := finding["group_key"].(string); ok {
			key = "group:" + group
		}
		group := groupByKey[key]
		if group == nil {
			priority := priorityBySeverity[finding["severity"].(string)].(string)
			group = contract.Object{"group_key": finding["group_key"], "title": reviewProposalTitle(finding), "issue_type": issueType, "priority": priority, "findings": []any{}, "blocked_by_issue_numbers": []int64{}, "parent_issue_number": finding["parent_issue_number"], "existing_issue": finding["existing_issue"], "labels": []any{label}}
			groupByKey[key] = group
			groups = append(groups, group)
		}
		if group["issue_type"] != issueType {
			return nil, fmt.Errorf("review group %q mixes issue types", key)
		}
		currentPriority := group["priority"].(string)
		nextPriority := priorityBySeverity[finding["severity"].(string)].(string)
		if reviewPriorityRank[nextPriority] < reviewPriorityRank[currentPriority] {
			group["priority"] = nextPriority
		}
		group["findings"] = append(group["findings"].([]any), finding)
		blockerSet := intMap(group["blocked_by_issue_numbers"])
		blockers, _ := contract.Array(finding, "blocked_by_issue_numbers")
		for _, raw := range blockers {
			n, _ := contract.PositiveInteger(raw)
			blockerSet[n] = true
		}
		group["blocked_by_issue_numbers"] = sortedIntMap(blockerSet)
		for _, name := range []string{"parent_issue_number", "existing_issue"} {
			if group[name] == nil && finding[name] != nil {
				group[name] = finding[name]
			}
		}
	}
	sort.Slice(groups, func(i, j int) bool {
		pi := groups[i]["priority"].(string)
		pj := groups[j]["priority"].(string)
		if reviewPriorityRank[pi] != reviewPriorityRank[pj] {
			return reviewPriorityRank[pi] < reviewPriorityRank[pj]
		}
		return groups[i]["title"].(string) < groups[j]["title"].(string)
	})
	proposals := []any{}
	for _, group := range groups {
		title := group["title"].(string)
		catalogExact := bySlug[reviewSlug(title)]
		explicitIssue, _ := contract.PositiveInteger(group["existing_issue"])
		var existingOpen contract.Object
		relatedClosed := []int64{}
		if explicitIssue > 0 && issueByNumber[explicitIssue] != nil {
			issue := issueByNumber[explicitIssue]
			if issue["state"] == "OPEN" {
				existingOpen = issue
			} else {
				relatedClosed = []int64{explicitIssue}
			}
		} else {
			openMatches, closedMatches := []contract.Object{}, []int64{}
			for _, issue := range catalogExact {
				if issue["state"] == "OPEN" {
					openMatches = append(openMatches, issue)
				} else {
					number, _ := contract.PositiveInteger(issue["number"])
					closedMatches = append(closedMatches, number)
				}
			}
			sort.Slice(openMatches, func(i, j int) bool {
				a, _ := contract.PositiveInteger(openMatches[i]["number"])
				b, _ := contract.PositiveInteger(openMatches[j]["number"])
				return a < b
			})
			sort.Slice(closedMatches, func(i, j int) bool { return closedMatches[i] < closedMatches[j] })
			if len(openMatches) > 0 {
				existingOpen = openMatches[0]
			} else {
				relatedClosed = closedMatches
			}
		}
		groupFindings, _ := contract.Objects(contract.Object{"items": group["findings"]}, "items")
		summaries, ids := []string{}, []any{}
		bodyLines, evidence, noteLines := []string{}, []string{}, []string{}
		for _, finding := range groupFindings {
			summaries = append(summaries, finding["summary"].(string))
			ids = append(ids, finding["id"])
			for _, line := range reviewEvidenceLines(finding) {
				evidence = append(evidence, line)
			}
			if finding["body"] != "" {
				noteLines = append(noteLines, finding["body"].(string))
			}
			if finding["notes"] != "" {
				noteLines = append(noteLines, finding["notes"].(string))
			}
			bodyLines = append(bodyLines, finding["summary"].(string))
		}
		goal := strings.Join(summaries, "; ")
		proposalTitle := title
		var issueNumber any
		action := "create"
		candidateMatches := []any{}
		if existingOpen != nil {
			proposalTitle = existingOpen["title"].(string)
			issueNumber = existingOpen["number"]
			action = "reuse-open"
		} else if len(relatedClosed) > 0 {
			action = "create-follow-up"
			candidateMatches = reviewCandidateMatches(title, issues)
		} else {
			candidateMatches = reviewCandidateMatches(title, issues)
		}
		labelValues, _ := contract.Array(group, "labels")
		body := reviewProposalBody(goal, bodyLines, evidence, noteLines)
		proposals = append(proposals, contract.Object{
			"action": action, "issue_number": issueNumber, "title": proposalTitle, "issue_type": group["issue_type"],
			"priority": group["priority"], "labels": labelValues, "project_title": projectTitle, "project_status": "Todo",
			"findings": group["findings"], "candidate_matches": candidateMatches,
			"related_closed_issues": intSliceAsAny(relatedClosed), "goal": goal, "finding_ids": ids,
			"group_key": group["group_key"], "blocked_by_issue_numbers": group["blocked_by_issue_numbers"],
			"parent_issue_number": group["parent_issue_number"], "body": body,
		})
	}
	sort.Slice(proposals, func(i, j int) bool {
		a, b := proposals[i].(contract.Object), proposals[j].(contract.Object)
		pa, pb := a["priority"].(string), b["priority"].(string)
		if reviewPriorityRank[pa] != reviewPriorityRank[pb] {
			return reviewPriorityRank[pa] < reviewPriorityRank[pb]
		}
		return a["title"].(string) < b["title"].(string)
	})
	scope := normalized["scope"]
	findingsOutput := contract.Object{"repo": repo, "schema_version": int64(1), "scope": scope, "issue_catalog_mode": mode, "finding_count": len(normalizedFindings), "findings": objectSliceAsAny(normalizedFindings)}
	delta := contract.Object{"repo": repo, "schema_version": int64(1), "scope": scope, "issue_catalog_mode": mode, "proposal_count": len(proposals), "proposals": proposals}
	return contract.Object{"findings_payload": findingsOutput, "delta": delta}, nil
}

func reviewProposalTitle(finding contract.Object) string {
	if title, ok := finding["canonical_backlog_title"].(string); ok && title != "" {
		return title
	}
	return finding["canonical_title"].(string)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func reviewSlug(text string) string {
	return strings.TrimSpace(nonSlug.ReplaceAllString(strings.ToLower(text), " "))
}

func reviewIssueCatalog(issues []contract.Object) (map[int64]contract.Object, map[string][]contract.Object, error) {
	byNumber, bySlug := map[int64]contract.Object{}, map[string][]contract.Object{}
	for _, issue := range issues {
		number, err := contract.PositiveInteger(issue["number"])
		if err != nil {
			return nil, nil, fmt.Errorf("issue catalog contains an invalid issue number")
		}
		title, err := contract.Nonempty(issue, "title")
		if err != nil {
			return nil, nil, err
		}
		state, err := contract.String(issue, "state")
		if err != nil || (state != "OPEN" && state != "CLOSED") {
			return nil, nil, fmt.Errorf("issue catalog state must be OPEN or CLOSED")
		}
		if byNumber[number] != nil {
			return nil, nil, fmt.Errorf("issue catalog contains duplicate issue #%d", number)
		}
		copy, _ := contract.Clone(issue)
		copy["number"], copy["title"], copy["state"] = number, strings.TrimSpace(title), state
		byNumber[number] = copy
		bySlug[reviewSlug(title)] = append(bySlug[reviewSlug(title)], copy)
	}
	return byNumber, bySlug, nil
}

func reviewEvidenceLines(finding contract.Object) []string {
	lines, seen := []string{}, map[string]bool{}
	if values, ok := finding["evidence"].([]any); ok {
		for _, raw := range values {
			evidence, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			path, _ := evidence["path"].(string)
			if path == "" {
				continue
			}
			ref := path
			if line, err := contract.PositiveInteger(evidence["line"]); err == nil {
				ref += ":" + strconv.FormatInt(line, 10)
			}
			detail, _ := evidence["detail"].(string)
			if detail == "" {
				detail, _ = evidence["summary"].(string)
			}
			if detail != "" {
				ref += " - " + detail
			}
			if !seen[ref] {
				seen[ref] = true
				lines = append(lines, ref)
			}
		}
	}
	if values, ok := finding["files"].([]any); ok {
		for _, raw := range values {
			if path, ok := raw.(string); ok && !seen[path] {
				seen[path] = true
				lines = append(lines, path)
			}
		}
	}
	sort.Strings(lines)
	return lines
}

func reviewCandidateMatches(title string, issues []contract.Object) []any {
	tokens := reviewTokenSet(title)
	if len(tokens) == 0 {
		return []any{}
	}
	type candidate struct {
		item   contract.Object
		shared []string
		number int64
	}
	candidates := []candidate{}
	for _, issue := range issues {
		other := reviewTokenSet(issue["title"].(string))
		shared := []string{}
		for token := range tokens {
			if other[token] {
				shared = append(shared, token)
			}
		}
		sort.Strings(shared)
		if len(shared) < 2 {
			continue
		}
		number, _ := contract.PositiveInteger(issue["number"])
		candidates = append(candidates, candidate{issue, shared, number})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i].shared) != len(candidates[j].shared) {
			return len(candidates[i].shared) > len(candidates[j].shared)
		}
		return candidates[i].number < candidates[j].number
	})
	if len(candidates) > 3 {
		candidates = candidates[:3]
	}
	result := make([]any, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, contract.Object{"number": candidate.number, "title": candidate.item["title"], "state": candidate.item["state"], "shared_tokens": stringSliceAsAny(candidate.shared)})
	}
	return result
}

func reviewTokenSet(title string) map[string]bool {
	tokens := strings.Fields(reviewSlug(title))
	result := map[string]bool{}
	for _, token := range tokens {
		if len(token) > 2 {
			result[token] = true
		}
	}
	return result
}

func reviewProposalBody(goal string, summaries, evidence, notes []string) string {
	lines := []string{"## Goal", "", goal, "", "## Findings", ""}
	for _, summary := range summaries {
		lines = append(lines, "- "+summary)
	}
	if len(evidence) > 0 {
		sort.Strings(evidence)
		evidence = uniqueStrings(evidence)
		lines = append(lines, "", "## Evidence", "")
		for _, line := range evidence {
			lines = append(lines, "- "+line)
		}
	}
	if len(notes) > 0 {
		lines = append(lines, "", "## Maintainer Notes", "")
		for _, line := range notes {
			lines = append(lines, "- "+line)
		}
	}
	lines = append(lines, "", "## Out Of Scope", "", "- Additional work beyond the findings captured here.")
	return strings.TrimSpace(strings.Join(lines, "\n")) + "\n"
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func intMap(raw any) map[int64]bool {
	result := map[int64]bool{}
	if values, ok := raw.([]any); ok {
		for _, value := range values {
			if n, err := contract.PositiveInteger(value); err == nil {
				result[n] = true
			}
		}
	} else if values, ok := raw.([]int64); ok {
		for _, value := range values {
			if value > 0 {
				result[value] = true
			}
		}
	}
	return result
}

func reviewNormalizedIssueTitle(issueType, title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", fmt.Errorf("issue title cannot be empty")
	}
	if strings.HasPrefix(title, issueType+": ") {
		return title, nil
	}
	for _, otherType := range reviewIssueTypes {
		if strings.HasPrefix(title, otherType+": ") {
			return "", fmt.Errorf("issue title %q does not match declared type %q", title, issueType)
		}
	}
	return issueType + ": " + title, nil
}

func reviewLegacyParent(issueType string, parent any) error {
	text, ok := parent.(string)
	if !ok || text == "" {
		return nil
	}
	parentType := ""
	for _, candidate := range reviewIssueTypes {
		if strings.HasPrefix(text, candidate+": ") {
			parentType = candidate
			break
		}
	}
	if issueType == "Initiative" && text != "" {
		return fmt.Errorf("initiatives must not declare a parent issue reference")
	}
	if issueType == "Epic" && parentType != "" && parentType != "Initiative" {
		return fmt.Errorf("Epics may only declare Initiative parents")
	}
	if issueType != "Initiative" && issueType != "Epic" && parentType == "Initiative" {
		return fmt.Errorf("leaf issues must not declare Initiative parents directly")
	}
	return nil
}
func sortedIntMap(values map[int64]bool) []int64 {
	out := make([]int64, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
func objectSliceAsAny(values []contract.Object) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

// NormalizeReviewCloseoutFindings validates the structured review/closeout
// findings format and canonicalizes its documented aliases.
func NormalizeReviewCloseoutFindings(payload contract.Object) (contract.Object, error) {
	version, err := contract.Integer(payload["schema_version"])
	if err != nil || version != 1 {
		return nil, fmt.Errorf("structured review-closeout findings must declare schema_version: 1")
	}
	rawFindings, err := contract.Objects(payload, "findings")
	if err != nil {
		return nil, fmt.Errorf("structured review-closeout findings must contain a findings list")
	}
	modeRaw, _ := payload["mode"].(string)
	mode := strings.ToLower(strings.TrimSpace(modeRaw))
	switch mode {
	case "review":
		mode = "review"
	case "epic-closeout", "epic closeout", "epic_closeout":
		mode = "epic-closeout"
	default:
		return nil, fmt.Errorf("unsupported mode %q; expected review or epic-closeout", modeRaw)
	}
	var epicNumber *int64
	if value, exists := payload["epic_issue_number"]; exists && value != nil {
		n, err := contract.PositiveInteger(value)
		if err != nil {
			return nil, fmt.Errorf("epic_issue_number must be a positive integer when provided")
		}
		epicNumber = &n
	}
	if mode == "epic-closeout" && epicNumber == nil {
		return nil, fmt.Errorf("epic-closeout findings must declare epic_issue_number")
	}
	scope := "repo"
	if value, ok := payload["scope"].(string); ok {
		if value == "" {
			scope = "repo"
		} else if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("scope must be nonempty")
		} else {
			scope = strings.TrimSpace(value)
		}
	}
	findings := make([]any, 0, len(rawFindings))
	for i, raw := range rawFindings {
		for _, key := range []string{"existing_issue", "parent_issue_number"} {
			if value, exists := raw[key]; exists && value != nil {
				if _, err := contract.PositiveInteger(value); err != nil {
					return nil, fmt.Errorf("finding #%d: %s must be a positive integer when provided", i+1, key)
				}
			}
		}
		blockers, err := reviewPositiveNumbers(raw, "blocked_by_issue_numbers", i)
		if err != nil {
			return nil, fmt.Errorf("issue references must be positive integer numbers: %w", err)
		}
		title, err := contract.Nonempty(raw, "title")
		if err != nil {
			return nil, fmt.Errorf("finding #%d is missing a non-empty title", i+1)
		}
		summary, err := contract.Nonempty(raw, "summary")
		if err != nil {
			return nil, fmt.Errorf("finding #%d is missing a non-empty summary", i+1)
		}
		blocking, err := contract.Bool(raw, "blocking")
		if err != nil {
			return nil, fmt.Errorf("finding #%d must declare boolean blocking", i+1)
		}
		destinationRaw, _ := raw["destination"].(string)
		destination, ok := reviewDestinations[strings.ToLower(strings.TrimSpace(destinationRaw))]
		if !ok {
			return nil, fmt.Errorf("finding #%d: unsupported destination %q", i+1, destinationRaw)
		}
		targetPaths, err := reviewFiles(raw["target_paths"])
		if err != nil {
			return nil, fmt.Errorf("finding #%d target_paths must be an array", i+1)
		}
		placement := destination == "agents-note" || destination == "engineering-doc" || destination == "skill-update" || destination == "shared-reference"
		if placement && len(targetPaths) == 0 {
			return nil, fmt.Errorf("finding #%d: destination %q requires target_paths", i+1, destination)
		}
		if (destination == "backlog" || destination == "none") && len(targetPaths) > 0 {
			return nil, fmt.Errorf("finding #%d: destination %q must not declare target_paths", i+1, destination)
		}
		if destination == "none" && blocking {
			return nil, fmt.Errorf("finding #%d: destination none cannot be blocking", i+1)
		}
		backlogMetadata := false
		for _, key := range []string{"issue_type", "group_key", "backlog_title", "existing_issue", "blocked_by_issue_numbers", "parent_issue_number"} {
			if v, ok := raw[key]; ok && v != nil && v != "" {
				if a, ok := v.([]any); !ok || len(a) > 0 {
					backlogMetadata = true
				}
			}
		}
		var issueType, canonicalBacklog any
		groupKey, err := reviewOptionalTrimmedString(raw, "group_key")
		if err != nil {
			return nil, err
		}
		backlogTitle, err := reviewOptionalTrimmedString(raw, "backlog_title")
		if err != nil {
			return nil, err
		}
		canonicalTitle := strings.TrimSpace(title)
		if destination == "backlog" {
			typeName, err := reviewType(raw, "issue_type", i)
			if err != nil {
				return nil, err
			}
			issueType = typeName
			if (groupKey == nil) != (backlogTitle == nil) {
				return nil, fmt.Errorf("finding #%d: group_key and backlog_title must either both be set or both be omitted", i+1)
			}
			canonicalTitle = reviewTitleWithPrefix(typeName, canonicalTitle)
			if backlogTitle != nil {
				canonicalBacklog = reviewTitleWithPrefix(typeName, *backlogTitle)
			}
		} else if backlogMetadata {
			return nil, fmt.Errorf("finding #%d: backlog issue metadata is only valid for backlog destination", i+1)
		}
		severity, err := reviewSeverity(raw, "severity", i)
		if err != nil {
			return nil, err
		}
		id := fmt.Sprintf("finding-%d", i+1)
		if value, ok := raw["id"]; ok && value != nil {
			str, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("finding #%d id must be a string", i+1)
			}
			if strings.TrimSpace(str) != "" {
				id = strings.TrimSpace(str)
			}
		}
		body, err := reviewOptionalText(raw, "body")
		if err != nil {
			return nil, err
		}
		notes, err := reviewOptionalText(raw, "notes")
		if err != nil {
			return nil, err
		}
		files, err := reviewFiles(raw["files"])
		if err != nil {
			return nil, fmt.Errorf("finding #%d files must be an array", i+1)
		}
		evidence, err := reviewEvidence(raw["evidence"])
		if err != nil {
			return nil, err
		}
		existing, _ := contract.PositiveInteger(raw["existing_issue"])
		parent, _ := contract.PositiveInteger(raw["parent_issue_number"])
		var group, backlog any
		if groupKey != nil {
			group = *groupKey
		}
		if backlogTitle != nil {
			backlog = *backlogTitle
		}
		findings = append(findings, contract.Object{"id": id, "title": strings.TrimSpace(title), "canonical_title": canonicalTitle, "severity": severity, "summary": strings.TrimSpace(summary), "blocking": blocking, "destination": destination, "target_paths": stringSliceAsAny(targetPaths), "body": body, "files": stringSliceAsAny(files), "evidence": evidence, "notes": notes, "issue_type": issueType, "group_key": group, "backlog_title": backlog, "canonical_backlog_title": canonicalBacklog, "existing_issue": optionalIntFromValue(existing, raw, "existing_issue"), "blocked_by_issue_numbers": intSliceAsAny(blockers), "parent_issue_number": optionalIntFromValue(parent, raw, "parent_issue_number")})
	}
	result := contract.Object{"schema_version": int64(1), "mode": mode, "scope": scope, "findings": findings}
	if epicNumber != nil {
		result["epic_issue_number"] = *epicNumber
	}
	return result, nil
}

func optionalIntFromValue(parsed int64, object contract.Object, key string) any {
	if _, exists := object[key]; !exists || object[key] == nil || parsed < 1 {
		return nil
	}
	return parsed
}

// SortReviewCloseoutFindings returns deterministic finding order: blocking,
// severity, destination, title, then id.
func SortReviewCloseoutFindings(findings []contract.Object) []contract.Object {
	result := append([]contract.Object{}, findings...)
	sort.SliceStable(result, func(i, j int) bool {
		a, b := result[i], result[j]
		ab, _ := a["blocking"].(bool)
		bb, _ := b["blocking"].(bool)
		if ab != bb {
			return ab
		}
		as, _ := a["severity"].(string)
		bs, _ := b["severity"].(string)
		if reviewSeverityRank[as] != reviewSeverityRank[bs] {
			return reviewSeverityRank[as] < reviewSeverityRank[bs]
		}
		for _, key := range []string{"destination", "title", "id"} {
			av, _ := a[key].(string)
			bv, _ := b[key].(string)
			if av != bv {
				return av < bv
			}
		}
		return false
	})
	return result
}

// DeriveReviewCloseoutBacklogFindings retains only findings routed to backlog.
func DeriveReviewCloseoutBacklogFindings(findings []contract.Object, scope string) contract.Object {
	out := []any{}
	for _, finding := range findings {
		if finding["destination"] != "backlog" {
			continue
		}
		out = append(out, contract.Object{"id": finding["id"], "title": finding["title"], "issue_type": finding["issue_type"], "severity": finding["severity"], "summary": finding["summary"], "group_key": finding["group_key"], "backlog_title": finding["backlog_title"], "body": finding["body"], "files": finding["files"], "evidence": finding["evidence"], "existing_issue": finding["existing_issue"], "notes": finding["notes"], "blocked_by_issue_numbers": finding["blocked_by_issue_numbers"], "parent_issue_number": finding["parent_issue_number"]})
	}
	return contract.Object{"schema_version": int64(1), "scope": scope, "findings": out}
}

// EpicIssueState derives open child and blocker sets from a complete local graph.
func EpicIssueState(issueGraph contract.Object, epicIssueNumber int64) (contract.Object, error) {
	issues, err := contract.Objects(issueGraph, "issues")
	if err != nil {
		return nil, err
	}
	byNumber := map[int64]contract.Object{}
	for _, issue := range issues {
		number, err := contract.PositiveInteger(issue["number"])
		if err != nil {
			return nil, fmt.Errorf("issue graph contains invalid issue number")
		}
		if byNumber[number] != nil {
			return nil, fmt.Errorf("issue graph has duplicate issue number #%d", number)
		}
		byNumber[number] = issue
	}
	epic := byNumber[epicIssueNumber]
	if epic == nil {
		return nil, fmt.Errorf("epic issue #%d is not present in the open issue graph", epicIssueNumber)
	}
	children, err := reviewPositiveNumbers(epic, "child_numbers", 0)
	if err != nil && epic["child_numbers"] != nil {
		return nil, err
	}
	blockers, err := reviewPositiveNumbers(epic, "blocked_by_numbers", 0)
	if err != nil && epic["blocked_by_numbers"] != nil {
		return nil, err
	}
	openChildren, openBlockers := []int64{}, []int64{}
	for _, number := range children {
		if issue := byNumber[number]; issue != nil && issue["state"] == "OPEN" {
			openChildren = append(openChildren, number)
		}
	}
	for _, number := range blockers {
		if issue := byNumber[number]; issue != nil && issue["state"] == "OPEN" {
			openBlockers = append(openBlockers, number)
		}
	}
	title := epic["title"]
	return contract.Object{"issue_number": epicIssueNumber, "title": title, "open_child_numbers": intSliceAsAny(openChildren), "blocked_by_numbers": intSliceAsAny(openBlockers)}, nil
}

// MatchingGovernanceFindings filters a governance report by exact positive
// issue number and annotates each matching finding with its source label.
func MatchingGovernanceFindings(payload contract.Object, issueNumber int64, source string) ([]any, error) {
	findings, ok := payload["findings"]
	if !ok || findings == nil {
		return []any{}, nil
	}
	items, ok := findings.([]any)
	if !ok {
		return nil, fmt.Errorf("governance findings must be an array")
	}
	result := []any{}
	for _, raw := range items {
		finding, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		number, err := contract.PositiveInteger(finding["number"])
		if err != nil || number != issueNumber {
			continue
		}
		result = append(result, contract.Object{"source": source, "code": finding["code"], "severity": finding["severity"], "message": finding["message"]})
	}
	return result, nil
}

// BuildReviewCloseoutAudit constructs findings, summary and verdict domain
// payloads from normalized findings and already prepared audit inputs.
func BuildReviewCloseoutAudit(input contract.Object) (contract.Object, error) {
	repo, err := contract.ObjectAt(input, "repo")
	if err != nil {
		return nil, err
	}
	if _, err := contract.ParseRepository(repo); err != nil {
		return nil, err
	}
	normalized, err := contract.ObjectAt(input, "normalized_input")
	if err != nil {
		return nil, err
	}
	version, err := contract.Integer(normalized["schema_version"])
	if err != nil || version != 1 {
		return nil, fmt.Errorf("normalized review-closeout input must declare schema_version: 1")
	}
	findings, err := contract.Objects(normalized, "findings")
	if err != nil {
		return nil, err
	}
	findings = SortReviewCloseoutFindings(findings)
	mode, err := contract.String(normalized, "mode")
	if err != nil || (mode != "review" && mode != "epic-closeout") {
		return nil, fmt.Errorf("normalized review-closeout mode is invalid")
	}
	scope, err := contract.Nonempty(normalized, "scope")
	if err != nil {
		return nil, err
	}
	generatedAt, err := contract.String(input, "generated_at")
	if err != nil {
		return nil, err
	}
	delta, err := contract.ObjectAt(input, "backlog_delta")
	if err != nil {
		return nil, err
	}
	proposalCount, err := contract.Integer(delta["proposal_count"])
	if err != nil || proposalCount < 0 {
		return nil, fmt.Errorf("backlog delta proposal_count must be a nonnegative integer")
	}
	backlogFindingCount, err := contract.Integer(input["backlog_finding_count"])
	if err != nil || backlogFindingCount < 0 {
		return nil, fmt.Errorf("backlog_finding_count must be a nonnegative integer")
	}
	actualBacklogFindingCount := int64(0)
	for _, finding := range findings {
		if finding["destination"] == "backlog" {
			actualBacklogFindingCount++
		}
	}
	if backlogFindingCount != actualBacklogFindingCount {
		return nil, fmt.Errorf("backlog_finding_count does not match normalized findings")
	}
	if rawProposals, exists := delta["proposals"]; exists {
		proposals, ok := rawProposals.([]any)
		if !ok || int64(len(proposals)) != proposalCount {
			return nil, fmt.Errorf("backlog delta proposal_count does not match its proposal list")
		}
	}
	reasons, err := contract.Strings(input["blocking_reasons"])
	if err != nil {
		return nil, err
	}
	var epic any
	if value, exists := normalized["epic_issue_number"]; exists {
		epic = value
	}
	findingsPayload := contract.Object{"repo": repo, "schema_version": int64(1), "mode": mode, "scope": scope, "epic_issue_number": epic, "finding_count": len(findings), "findings": objectSliceAsAny(findings)}
	blockingCount := 0
	backlogFindings := 0
	for _, finding := range findings {
		if finding["blocking"] == true {
			blockingCount++
		}
		if finding["destination"] == "backlog" {
			backlogFindings++
		}
	}
	blockingReasons := stringSliceAsAny(reasons)
	summary := contract.Object{"repo": repo, "schema_version": int64(1), "mode": mode, "scope": scope, "generated_at": generatedAt, "epic_issue_number": epic, "finding_count": len(findings), "blocking_finding_count": blockingCount, "backlog": contract.Object{"finding_count": backlogFindingCount, "proposal_count": proposalCount, "delta": delta}, "blocking_reasons": blockingReasons, "findings": objectSliceAsAny(findings)}
	verdict := contract.Object{"repo": repo, "schema_version": int64(1), "mode": mode, "scope": scope, "generated_at": generatedAt, "epic_issue_number": epic, "finding_count": len(findings), "blocking_reasons": blockingReasons}
	if mode == "review" {
		status := "clean"
		if len(findings) > 0 {
			status = "follow-up-required"
		}
		summary["status"], verdict["status"] = status, status
	} else {
		number, err := contract.PositiveInteger(normalized["epic_issue_number"])
		if err != nil {
			return nil, fmt.Errorf("epic-closeout mode requires a positive epic_issue_number")
		}
		epicState, err := contract.ObjectAt(input, "epic_state")
		if err != nil {
			return nil, err
		}
		stateNumber, err := contract.PositiveInteger(epicState["issue_number"])
		if err != nil || stateNumber != number {
			return nil, fmt.Errorf("epic state does not match normalized epic issue number")
		}
		ready := len(reasons) == 0
		// Keep the input epic state as prepared by the caller; this builder only
		// assembles the deterministic audit result.
		summary["epic_state"], summary["closeout_ready"] = epicState, ready
		verdict["closeout_ready"] = ready
	}
	return contract.Object{"findings": findingsPayload, "summary": summary, "verdict": verdict}, nil
}
