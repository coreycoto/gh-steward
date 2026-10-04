package planning

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var quarterPattern = regexp.MustCompile(`^([0-9]{4}) Q([1-4])$`)

var quarterPriorities = map[string]bool{"Now": true, "Next": true, "Later": true}

// NormalizeQuarterPlan validates and normalizes structured quarter plans and
// the retained legacy items form. It performs no repository reads.
func NormalizeQuarterPlan(payload contract.Object) (contract.Object, error) {
	version, err := contract.Integer(payload["schema_version"])
	if err != nil || version != 1 {
		return nil, fmt.Errorf("quarter plan must declare schema_version: 1")
	}
	if _, legacy := payload["items"]; legacy {
		return normalizeLegacyQuarterPlan(payload)
	}
	allowed := map[string]bool{"schema_version": true, "quarter": true, "quarter_goals": true, "active_tracks": true, "commit_issue_numbers": true, "issue_rationale": true}
	for key := range payload {
		if !allowed[key] {
			return nil, fmt.Errorf("quarter plan contains unsupported field %q", key)
		}
	}
	quarter, err := contract.Nonempty(payload, "quarter")
	if err != nil || !quarterPattern.MatchString(strings.TrimSpace(quarter)) {
		return nil, fmt.Errorf("quarter plan quarter must match YYYY QN")
	}
	quarter = strings.TrimSpace(quarter)
	goals, err := quarterStringList(payload, "quarter_goals")
	if err != nil {
		return nil, err
	}
	tracks, err := quarterStringList(payload, "active_tracks")
	if err != nil {
		return nil, err
	}
	commitRaw, err := contract.Array(payload, "commit_issue_numbers")
	if err != nil {
		return nil, err
	}
	commits := map[int64]bool{}
	for _, raw := range commitRaw {
		number, err := contract.PositiveInteger(raw)
		if err != nil {
			return nil, fmt.Errorf("commit_issue_numbers must contain positive whole-number issue references")
		}
		commits[number] = true
	}
	commitNumbers := make([]int64, 0, len(commits))
	for number := range commits {
		commitNumbers = append(commitNumbers, number)
	}
	sort.Slice(commitNumbers, func(i, j int) bool { return commitNumbers[i] < commitNumbers[j] })
	rawRationale, ok := payload["issue_rationale"]
	if !ok || rawRationale == nil {
		return nil, fmt.Errorf("issue_rationale must be an object")
	}
	rationale, ok := rawRationale.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("issue_rationale must be an object")
	}
	keys := make([]string, 0, len(rationale))
	for key := range rationale {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	normalizedRationale := contract.Object{}
	for _, key := range keys {
		if key == "" {
			return nil, fmt.Errorf("issue_rationale key %q must be an issue number", key)
		}
		for _, ch := range key {
			if ch < '0' || ch > '9' {
				return nil, fmt.Errorf("issue_rationale key %q must be an issue number", key)
			}
		}
		number, err := strconv.ParseInt(key, 10, 64)
		if err != nil || number < 1 {
			return nil, fmt.Errorf("issue_rationale key %q must be a positive issue number", key)
		}
		text, ok := rationale[key].(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("issue_rationale for issue #%s must be a non-empty string", key)
		}
		canonicalKey := strconv.FormatInt(number, 10)
		if _, duplicate := normalizedRationale[canonicalKey]; duplicate {
			return nil, fmt.Errorf("issue_rationale contains duplicate issue number #%s", canonicalKey)
		}
		normalizedRationale[canonicalKey] = strings.TrimSpace(text)
	}
	return contract.Object{
		"schema_version":       int64(1),
		"quarter":              quarter,
		"quarter_goals":        stringSliceAsAny(goals),
		"active_tracks":        stringSliceAsAny(tracks),
		"commit_issue_numbers": intSliceAsAny(commitNumbers),
		"issue_rationale":      normalizedRationale,
	}, nil
}

func quarterStringList(payload contract.Object, key string) ([]string, error) {
	values, err := contract.Strings(payload[key])
	if err != nil {
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result, nil
}

func normalizeLegacyQuarterPlan(payload contract.Object) (contract.Object, error) {
	quarter, err := contract.Nonempty(payload, "quarter")
	if err != nil {
		return nil, err
	}
	quarter = strings.TrimSpace(quarter)
	if !quarterPattern.MatchString(quarter) {
		return nil, fmt.Errorf("quarter plan quarter must match YYYY QN")
	}
	items, err := contract.Objects(payload, "items")
	if err != nil || len(items) == 0 {
		return nil, fmt.Errorf("legacy quarter plan must contain a non-empty items list")
	}
	normalized := make([]any, 0, len(items))
	for i, item := range items {
		title, err := contract.Nonempty(item, "title")
		if err != nil {
			return nil, fmt.Errorf("quarter plan item %d must declare title", i+1)
		}
		title = strings.TrimSpace(title)
		epic, err := optionalQuarterString(item, "epic")
		if err != nil {
			return nil, fmt.Errorf("quarter plan item %d epic must be a string or null", i+1)
		}
		priority, err := optionalQuarterString(item, "priority")
		if err != nil {
			return nil, fmt.Errorf("quarter plan item %d priority must be a string or null", i+1)
		}
		var epicValue any
		if strings.TrimSpace(epic) != "" {
			epicValue = strings.TrimSpace(epic)
		}
		var priorityValue any
		if strings.TrimSpace(priority) != "" {
			priority = strings.TrimSpace(priority)
			if !quarterPriorities[priority] {
				return nil, fmt.Errorf("unsupported quarter plan priority: %s", priority)
			}
			priorityValue = priority
		}
		titleType := quarterTitleType(title)
		epicType := quarterTitleType(epic)
		if titleType == "Initiative" && epicValue != nil {
			return nil, fmt.Errorf("initiative quarter plan items must not declare parent references")
		}
		if titleType == "Epic" && epicType != "" && epicType != "Initiative" {
			return nil, fmt.Errorf("epic quarter plan items may only declare Initiative parents")
		}
		if titleType != "" && titleType != "Initiative" && titleType != "Epic" && epicType == "Initiative" {
			return nil, fmt.Errorf("leaf quarter plan items must not declare Initiative parents directly")
		}
		normalized = append(normalized, contract.Object{"title": title, "epic": epicValue, "priority": priorityValue})
	}
	return contract.Object{"schema_version": int64(1), "quarter": quarter, "items": normalized}, nil
}

func optionalQuarterString(object contract.Object, key string) (string, error) {
	raw, exists := object[key]
	if !exists || raw == nil {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string or null", key)
	}
	return value, nil
}

func quarterTitleType(title string) string {
	for _, issueType := range []string{"Initiative", "Epic", "Research", "Enhancement", "Bug", "Maintenance"} {
		if strings.HasPrefix(title, issueType+": ") {
			return issueType
		}
	}
	return ""
}

// BuildQuarterPlanBacklogDelta preserves the offline compatibility path for
// legacy {items:[...]} quarter plans. The returned value is a backlog plan,
// not an authorization envelope.
func BuildQuarterPlanBacklogDelta(payload contract.Object) (contract.Object, error) {
	plan, err := NormalizeQuarterPlan(payload)
	if err != nil {
		return nil, err
	}
	items, err := contract.Objects(plan, "items")
	if err != nil {
		return nil, err
	}
	issues := make([]any, 0, len(items))
	for _, item := range items {
		title, _ := contract.String(item, "title")
		typeName := quarterTitleType(title)
		if typeName == "" {
			typeName = "Enhancement"
		}
		normalizedTitle, err := reviewNormalizedIssueTitle(typeName, title)
		if err != nil {
			return nil, err
		}
		if err := reviewLegacyParent(typeName, item["epic"]); err != nil {
			return nil, err
		}
		issues = append(issues, contract.Object{
			"action": "update", "type": typeName, "title": normalizedTitle, "body": "",
			"epic": item["epic"], "labels": []any{}, "milestone": plan["quarter"],
			"priority": item["priority"], "status": nil,
		})
	}
	quarter, _ := contract.String(plan, "quarter")
	sort.Slice(issues, func(i, j int) bool {
		a := issues[i].(contract.Object)["title"].(string)
		b := issues[j].(contract.Object)["title"].(string)
		return a < b
	})
	return contract.Object{
		"schema_version": int64(1), "source": "quarter-plan", "issues": issues,
		"notes": []any{fmt.Sprintf("Apply quarter milestone assignments for %s.", quarter)},
	}, nil
}

// BuildQuarterPlanDelta builds a provider-free quarter assignment preview from
// an already captured issue graph. Optional source evidence and snapshots can
// produce exact review operations; they never grant approval.
func BuildQuarterPlanDelta(input contract.Object) (contract.Object, error) {
	planInput, err := contract.ObjectAt(input, "plan")
	if err != nil {
		return nil, err
	}
	plan, err := NormalizeQuarterPlan(planInput)
	if err != nil {
		return nil, err
	}
	if _, legacy := plan["items"]; legacy {
		return nil, fmt.Errorf("structured quarter plan required for quarter assignment delta")
	}
	graph, err := contract.ObjectAt(input, "issue_graph")
	if err != nil {
		return nil, err
	}
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	quarter, _ := contract.String(plan, "quarter")
	issueByNumber := map[int64]contract.Object{}
	assigned := map[int64]bool{}
	complete := true
	for _, issue := range issues {
		number, numberErr := contract.PositiveInteger(issue["number"])
		if numberErr != nil || issueByNumber[number] != nil {
			complete = false
			continue
		}
		issueByNumber[number] = issue
		if id, ok := issue["id"].(string); !ok || id == "" || issue["state"] != "OPEN" {
			complete = false
		}
		if _, ok := issue["title"].(string); !ok || issue["title"] == "" {
			complete = false
		}
		if _, ok := issue["url"].(string); !ok || issue["url"] == "" {
			complete = false
		}
		milestone, exists := issue["milestone"]
		if !exists || (milestone != nil && func() bool { _, ok := milestone.(string); return !ok }()) {
			complete = false
		}
		if milestone == quarter && issue["state"] == "OPEN" {
			assigned[number] = true
		}
	}
	if !complete {
		assigned = map[int64]bool{}
	}
	current := sortedIssueNumbers(assigned)
	auto, err := optionalQuarterBool(input, "auto_from_current_state")
	if err != nil {
		return nil, err
	}
	desired := []int64{}
	if auto {
		desired = append(desired, current...)
		plan["commit_issue_numbers"] = intSliceAsAny(desired)
	} else {
		raw, err := contract.Array(plan, "commit_issue_numbers")
		if err != nil {
			return nil, err
		}
		seen := map[int64]bool{}
		for _, value := range raw {
			n, err := contract.PositiveInteger(value)
			if err != nil || seen[n] {
				return nil, fmt.Errorf("commit_issue_numbers must contain unique positive integers")
			}
			seen[n] = true
			desired = append(desired, n)
		}
		sort.Slice(desired, func(i, j int) bool { return desired[i] < desired[j] })
	}
	desiredSet := map[int64]bool{}
	for _, number := range desired {
		desiredSet[number] = true
	}
	add, keep, remove := []int64{}, []int64{}, []int64{}
	for _, number := range desired {
		if assigned[number] {
			keep = append(keep, number)
		} else {
			add = append(add, number)
		}
	}
	for _, number := range current {
		if !desiredSet[number] {
			remove = append(remove, number)
		}
	}
	touchedSet := map[int64]bool{}
	for _, number := range add {
		touchedSet[number] = true
	}
	for _, number := range remove {
		touchedSet[number] = true
	}
	touched := sortedIssueNumbers(touchedSet)
	rationale, _ := contract.ObjectAt(plan, "issue_rationale")
	rationaleGaps := []int64{}
	for _, number := range touched {
		if value, ok := rationale[strconv.FormatInt(number, 10)]; !ok || strings.TrimSpace(fmt.Sprint(value)) == "" {
			rationaleGaps = append(rationaleGaps, number)
		}
	}
	description, dueOn, err := quarterDescription(quarter, plan["quarter_goals"], plan["active_tracks"])
	if err != nil {
		return nil, err
	}
	issuesOut := contract.Object{}
	for _, number := range sortedUnion(current, desired) {
		entry := contract.Object{"number": number, "title": nil, "current_milestone": nil}
		if issue := issueByNumber[number]; issue != nil {
			entry["title"] = issue["title"]
			entry["current_milestone"] = issue["milestone"]
		}
		issuesOut[strconv.FormatInt(number, 10)] = entry
	}
	var milestoneAuditSummary any
	if audit, ok := input["milestone_audit"].(map[string]any); ok {
		milestoneAuditSummary = audit["summary"]
	}
	repo := graph["repo"]
	if value, exists := input["repo"]; exists {
		if _, ok := value.(map[string]any); !ok {
			return nil, fmt.Errorf("repo must be an object")
		}
		repo = value
	}
	if repo == nil {
		return nil, fmt.Errorf("repo identity is required in issue_graph or input")
	}
	repoObject, ok := repo.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("repo must be an object")
	}
	repository, err := contract.ParseRepository(repoObject)
	if err != nil {
		return nil, err
	}
	if graphRepo, ok := graph["repo"].(map[string]any); ok {
		graphRepository, err := contract.ParseRepository(graphRepo)
		if err != nil || graphRepository != repository {
			return nil, fmt.Errorf("repo identity does not match the issue graph")
		}
	}
	delta := contract.Object{
		"schema_version": int64(1), "repo": repo, "quarter": quarter,
		"summary": contract.Object{
			"add_count": len(add), "keep_count": len(keep), "remove_count": len(remove),
			"rationale_gap_count":     len(rationaleGaps),
			"milestone_finding_count": quarterFindingCount(milestoneAuditSummary),
		},
		"target_milestone_description": description,
		"current_commit_issue_numbers": intSliceAsAny(current),
		"add_issue_numbers":            intSliceAsAny(add), "keep_issue_numbers": intSliceAsAny(keep),
		"remove_issue_numbers": intSliceAsAny(remove), "issue_rationale": rationale,
		"rationale_gaps": intSliceAsAny(rationaleGaps), "issues": issuesOut,
		"milestone_audit_summary": milestoneAuditSummary,
	}
	if operationData, ok, err := buildQuarterReviewOperations(input, plan, graph, issueByNumber, complete, touched, add, rationale, description, dueOn); err != nil {
		return nil, err
	} else if ok {
		delta["review_preparation"] = operationData
	}
	return contract.Object{"normalized_plan": plan, "delta": delta}, nil
}

func optionalQuarterBool(object contract.Object, key string) (bool, error) {
	value, exists := object[key]
	if !exists {
		return false, nil
	}
	flag, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return flag, nil
}

func quarterFindingCount(value any) int {
	if object, ok := value.(map[string]any); ok {
		if count, err := contract.Integer(object["finding_count"]); err == nil && count >= 0 {
			return int(count)
		}
	}
	return 0
}

func quarterDescription(title string, goalsValue, tracksValue any) (string, string, error) {
	match := quarterPattern.FindStringSubmatch(title)
	if match == nil {
		return "", "", fmt.Errorf("invalid quarter title")
	}
	year, _ := strconv.Atoi(match[1])
	quarter, _ := strconv.Atoi(match[2])
	if year < 1 || year > 9999 || (year == 9999 && quarter == 4) {
		return "", "", fmt.Errorf("quarter year is outside supported calendar range")
	}
	month := time.Month(1 + (quarter-1)*3)
	start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 3, -1)
	due := end.Format("2006-01-02") + "T23:59:59Z"
	goals, _ := contract.Strings(goalsValue)
	tracks, _ := contract.Strings(tracksValue)
	if len(goals) == 0 {
		goals = []string{"None recorded yet."}
	}
	if len(tracks) == 0 {
		tracks = []string{"None recorded yet."}
	}
	lines := []string{fmt.Sprintf("Quarter: %s", title), fmt.Sprintf("Date range: %s through %s", start.Format("2006-01-02"), end.Format("2006-01-02")), "Quarter goals:"}
	for _, goal := range goals {
		lines = append(lines, "- "+goal)
	}
	lines = append(lines, "Active initiatives:")
	for _, track := range tracks {
		lines = append(lines, "- "+track)
	}
	lines = append(lines, "Use only for issues explicitly committed to this quarter.", "Record quarter rationale in the issue body or a maintainer comment.")
	return strings.Join(lines, "\n"), due, nil
}

func sortedUnion(a, b []int64) []int64 {
	set := map[int64]bool{}
	for _, value := range a {
		set[value] = true
	}
	for _, value := range b {
		set[value] = true
	}
	return sortedIssueNumbers(set)
}

func buildQuarterReviewOperations(input, plan, graph contract.Object, issueByNumber map[int64]contract.Object, issueInventoryComplete bool, touched, add []int64, rationale contract.Object, description, dueOn string) (contract.Object, bool, error) {
	sources, ok := input["source_evidence"].(map[string]any)
	if !ok || !issueInventoryComplete {
		return nil, false, nil
	}
	issuesSource, okI := sources["issues"].(map[string]any)
	milestonesSource, okM := sources["milestones"].(map[string]any)
	commentsSource, okC := sources["rationale_comments"].(map[string]any)
	if !okI || !okM || !okC || !quarterSourceIsComplete(issuesSource) || !quarterSourceIsComplete(milestonesSource) || !quarterSourceIsComplete(commentsSource) {
		return nil, false, nil
	}
	repo, err := contract.ObjectAt(input, "repo")
	if err != nil {
		repo, err = contract.ObjectAt(graph, "repo")
	}
	if err != nil {
		return nil, false, nil
	}
	if _, err := contract.ParseRepository(repo); err != nil {
		return nil, false, nil
	}
	milestones, err := contract.Objects(input, "milestones")
	if err != nil {
		return nil, false, err
	}
	byTitle := map[string]contract.Object{}
	seenNumbers := map[int64]bool{}
	for _, milestone := range milestones {
		number, err := contract.PositiveInteger(milestone["number"])
		if err != nil {
			return nil, false, fmt.Errorf("milestone snapshot has invalid number")
		}
		title, err := contract.Nonempty(milestone, "title")
		if err != nil {
			return nil, false, err
		}
		state, err := contract.String(milestone, "state")
		if err != nil || (state != "open" && state != "closed") {
			return nil, false, fmt.Errorf("milestone snapshot has invalid state")
		}
		if _, exists := milestone["description"]; !exists {
			return nil, false, fmt.Errorf("milestone snapshot is missing description")
		}
		if milestone["description"] != nil {
			if _, ok := milestone["description"].(string); !ok {
				return nil, false, fmt.Errorf("milestone description must be string or null")
			}
		}
		if _, exists := milestone["due_on"]; !exists {
			return nil, false, fmt.Errorf("milestone snapshot is missing due_on")
		}
		if milestone["due_on"] != nil {
			if _, ok := milestone["due_on"].(string); !ok {
				return nil, false, fmt.Errorf("milestone due_on must be string or null")
			}
		}
		if seenNumbers[number] || byTitle[title] != nil {
			return nil, false, fmt.Errorf("milestone snapshot contains duplicate identity or title")
		}
		seenNumbers[number] = true
		byTitle[title] = milestone
	}
	quarter, _ := contract.String(plan, "quarter")
	existing := byTitle[quarter]
	action, varNumber := "create", any(nil)
	if existing != nil {
		state := existing["state"].(string)
		if state == "closed" {
			action = "closed-existing"
		} else {
			action = "update"
		}
		varNumber = existing["number"]
	}
	if action == "closed-existing" {
		return nil, false, nil
	}
	var state, oldDescription, due any
	exists := existing != nil
	if existing != nil {
		state, oldDescription, due = existing["state"], existing["description"], existing["due_on"]
	}
	operations := []any{contract.Object{
		"id":     "milestone:" + quarter,
		"target": contract.Object{"kind": "repository-milestone", "title": quarter, "action": action, "number": varNumber},
		"before": contract.Object{"exists": exists, "state": state, "description": oldDescription, "due_on": due},
		"after":  contract.Object{"exists": true, "state": "open", "description": description, "due_on": dueOn},
	}}
	addSet := map[int64]bool{}
	for _, number := range add {
		addSet[number] = true
	}
	comments, ok := input["rationale_comments_by_issue"].(map[string]any)
	if len(touched) > 0 && !ok {
		return nil, false, nil
	}
	marker := "<!-- quarter-rationale quarter=" + quarter + " -->"
	for _, number := range touched {
		issue := issueByNumber[number]
		if issue == nil || issue["state"] != "OPEN" {
			return nil, false, nil
		}
		text, ok := rationale[strconv.FormatInt(number, 10)].(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, false, nil
		}
		comment, ok := comments[strconv.FormatInt(number, 10)].(map[string]any)
		if !ok {
			return nil, false, nil
		}
		body, hasBody := comment["body"]
		if !hasBody || (body != nil && func() bool { _, ok := body.(string); return !ok }()) {
			return nil, false, fmt.Errorf("rationale comment body must be string or null")
		}
		ids, err := quarterPositiveIDs(comment, "ids")
		if err != nil {
			return nil, false, err
		}
		milestone := any(nil)
		if addSet[number] {
			milestone = quarter
		}
		newBody := marker + "\nQuarter commitment rationale for " + quarter + ":\n\n" + strings.TrimSpace(text) + "\n"
		operations = append(operations, contract.Object{
			"id": "quarter-assignment:" + strconv.FormatInt(number, 10),
			"target": contract.Object{"kind": "issue-quarter-assignment", "issue_number": number, "action": func() string {
				if addSet[number] {
					return "assign"
				}
				return "clear"
			}(), "marker": marker},
			"before": contract.Object{"milestone": issue["milestone"], "rationale_comment_body": body, "rationale_comment_ids": ids, "issue_state": issue["state"]},
			"after":  contract.Object{"milestone": milestone, "rationale_comment_body": newBody},
		})
	}
	return contract.Object{"operations": operations, "sources": contract.Object{"issues": issuesSource, "milestones": milestonesSource, "rationale_comments": commentsSource}}, true, nil
}

func quarterSourceIsComplete(source contract.Object) bool {
	return source["live"] == true && source["complete"] == true
}

func quarterPositiveIDs(object contract.Object, key string) ([]any, error) {
	values, err := contract.Array(object, key)
	if err != nil {
		return nil, err
	}
	result := make([]int64, 0, len(values))
	seen := map[int64]bool{}
	for _, value := range values {
		n, err := contract.PositiveInteger(value)
		if err != nil || seen[n] {
			return nil, fmt.Errorf("%s must contain unique positive integer ids", key)
		}
		seen[n] = true
		result = append(result, n)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return intSliceAsAny(result), nil
}

func stringSliceAsAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func intSliceAsAny(values []int64) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}
