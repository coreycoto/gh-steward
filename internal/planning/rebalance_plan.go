package planning

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var decisionBands = []string{"now_issue_numbers", "next_issue_numbers", "later_issue_numbers"}
var allowedDecisionFields = map[string]bool{"schema_version": true, "project_title": true, "now_issue_numbers": true, "next_issue_numbers": true, "later_issue_numbers": true, "archive_issue_numbers": true, "notes": true}

// NormalizeRebalanceDecisions validates an authored decision payload against
// the supplied project membership and open issue graph.
func NormalizeRebalanceDecisions(payload, issueGraph, projectSnapshot contract.Object, excludedPrefixes []string) (contract.Object, error) {
	for key := range payload {
		if !allowedDecisionFields[key] {
			return nil, fmt.Errorf("rebalance decisions contain unsupported property %q", key)
		}
	}
	version, err := contract.Integer(payload["schema_version"])
	if err != nil || version != 1 {
		return nil, fmt.Errorf("rebalance decisions schema_version must be the integer 1")
	}
	title, err := contract.Nonempty(payload, "project_title")
	if err != nil {
		return nil, err
	}
	title = strings.TrimSpace(title)
	project := objectOrEmpty(projectSnapshot, "project")
	if snapshotTitle, ok := project["title"].(string); !ok || strings.TrimSpace(snapshotTitle) != title {
		return nil, fmt.Errorf("decision project_title does not match the supplied project snapshot")
	}
	projectItems, err := contract.Objects(projectSnapshot, "items")
	if err != nil {
		return nil, err
	}
	members := map[int64]bool{}
	prefixes := map[int64]string{}
	for i, item := range projectItems {
		number, present, err := optionalIssueNumber(item, fmt.Sprintf("project items[%d]", i))
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		if members[number] {
			return nil, fmt.Errorf("project snapshot has an invalid or duplicate issue number")
		}
		members[number] = true
		prefixes[number] = prefixFor(item)
	}
	issues, err := contract.Objects(issueGraph, "issues")
	if err != nil {
		return nil, err
	}
	open := map[int64]bool{}
	for i, issue := range issues {
		number, err := issueNumber(issue, fmt.Sprintf("issues[%d]", i))
		if err != nil {
			return nil, err
		}
		if open[number] {
			return nil, fmt.Errorf("issue graph contains duplicate issue #%d", number)
		}
		state := strings.ToUpper(text(issue["state"]))
		if state == "" || state == "OPEN" {
			open[number] = true
		}
		if prefixes[number] == "" {
			prefixes[number] = prefixFor(issue)
		}
	}
	excluded := stringSet(excludedPrefixes)
	normalized := contract.Object{"schema_version": int64(1), "project_title": title}
	seen := map[int64]string{}
	allBands := append(append([]string{}, decisionBands...), "archive_issue_numbers")
	for _, band := range allBands {
		raw, exists := payload[band]
		if !exists {
			return nil, fmt.Errorf("rebalance decisions are missing %s", band)
		}
		values, ok := raw.([]any)
		if !ok {
			if typed, ok := raw.([]int64); ok {
				values = make([]any, len(typed))
				for i, v := range typed {
					values[i] = v
				}
			} else if typed, ok := raw.([]int); ok {
				values = make([]any, len(typed))
				for i, v := range typed {
					values[i] = v
				}
			} else {
				return nil, fmt.Errorf("%s must be an array", band)
			}
		}
		numbers := []int64{}
		for _, rawNumber := range values {
			number, err := contract.PositiveInteger(rawNumber)
			if err != nil {
				return nil, fmt.Errorf("%s entries must be positive integers", band)
			}
			if excluded[prefixes[number]] {
				continue
			}
			if prior, ok := seen[number]; ok {
				return nil, fmt.Errorf("issue #%d appears in both %s and %s", number, prior, band)
			}
			seen[number] = band
			if !members[number] {
				return nil, fmt.Errorf("issue #%d is not present in project %q", number, title)
			}
			if band != "archive_issue_numbers" && !open[number] {
				return nil, fmt.Errorf("issue #%d is not open and cannot be assigned to an active band", number)
			}
			numbers = append(numbers, number)
		}
		normalized[band] = numbers
	}
	if note, exists := payload["notes"]; exists {
		value, ok := note.(string)
		if !ok {
			return nil, fmt.Errorf("notes must be a string")
		}
		normalized["notes"] = strings.TrimSpace(value)
	} else {
		normalized["notes"] = ""
	}
	return normalized, nil
}

func currentBandState(snapshot contract.Object) (map[string][]int64, map[int64]queueItem, error) {
	items, err := queueItems(snapshot, "items")
	if err != nil {
		return nil, nil, err
	}
	bands := map[string][]int64{"now_issue_numbers": {}, "next_issue_numbers": {}, "later_issue_numbers": {}}
	byNumber := map[int64]queueItem{}
	for _, item := range items {
		byNumber[item.number] = item
		for _, band := range decisionBands {
			if bandPriority(band) == item.priority {
				bands[band] = append(bands[band], item.number)
			}
		}
	}
	return bands, byNumber, nil
}
func bandPriority(band string) string {
	switch band {
	case "now_issue_numbers":
		return "Now"
	case "next_issue_numbers":
		return "Next"
	case "later_issue_numbers":
		return "Later"
	}
	return ""
}

func optionalIssueNumber(item contract.Object, where string) (int64, bool, error) {
	raw, exists := item["number"]
	if !exists || raw == nil {
		return 0, false, nil
	}
	number, err := contract.PositiveInteger(raw)
	if err != nil {
		return 0, true, fmt.Errorf("%s.number must be a positive integer when present", where)
	}
	return number, true, nil
}
func normalizeQueueOrder(value any) (float64, bool) { return normalizedQueueOrder(value) }
func integerRank(value any) (int64, bool) {
	n, ok := normalizedQueueOrder(value)
	if !ok || math.Trunc(n) != n || n > math.MaxInt64 || n < math.MinInt64 {
		return 0, false
	}
	return int64(n), true
}
func fractionalRank(value any) bool { return isFractionalQueueOrder(value) }
func lcs(left, right []int64) []int64 {
	if len(left) == 0 || len(right) == 0 {
		return []int64{}
	}
	dp := make([][]int, len(left)+1)
	for i := range dp {
		dp[i] = make([]int, len(right)+1)
	}
	for i := 1; i <= len(left); i++ {
		for j := 1; j <= len(right); j++ {
			if left[i-1] == right[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	result := []int64{}
	i, j := len(left), len(right)
	for i > 0 && j > 0 {
		if left[i-1] == right[j-1] {
			result = append(result, left[i-1])
			i--
			j--
		} else if dp[i-1][j] >= dp[i][j-1] {
			i--
		} else {
			j--
		}
	}
	for x, y := 0, len(result)-1; x < y; x, y = x+1, y-1 {
		result[x], result[y] = result[y], result[x]
	}
	return result
}
func checkedRank(step int64, index int64) (int64, error) {
	if index != 0 && step > math.MaxInt64/index {
		return 0, fmt.Errorf("queue rank exceeds integer range")
	}
	return step * index, nil
}
func denseOrders(numbers []int64, step int64) (map[int64]int64, error) {
	result := map[int64]int64{}
	for i, n := range numbers {
		rank, err := checkedRank(step, int64(i+1))
		if err != nil {
			return nil, err
		}
		result[n] = rank
	}
	return result, nil
}
func assignSegment(numbers []int64, left, right *int64, step int64) (map[int64]int64, bool, error) {
	result := map[int64]int64{}
	if len(numbers) == 0 {
		return result, true, nil
	}
	if left == nil && right == nil {
		r, err := denseOrders(numbers, step)
		return r, err == nil, err
	}
	if left != nil && right != nil {
		if *right <= *left {
			return nil, false, fmt.Errorf("minimal queue-mode anchors must be strictly increasing")
		}
		gap := *right - *left
		if gap < 0 {
			return nil, false, fmt.Errorf("queue anchor gap overflows")
		}
		rankStep := gap / int64(len(numbers)+1)
		if rankStep < 1 {
			return nil, false, nil
		}
		for i, n := range numbers {
			result[n] = *left + rankStep*int64(i+1)
		}
		return result, true, nil
	}
	if left != nil {
		for i, n := range numbers {
			offset, err := checkedRank(step, int64(i+1))
			if err != nil || *left > math.MaxInt64-offset {
				return nil, false, fmt.Errorf("queue rank exceeds integer range")
			}
			result[n] = *left + offset
		}
		return result, true, nil
	}
	startOffset, err := checkedRank(step, int64(len(numbers)))
	if err != nil {
		return nil, false, err
	}
	if *right < math.MinInt64+startOffset {
		return nil, false, nil
	}
	start := *right - startOffset
	if start < step {
		return nil, false, nil
	}
	for i, n := range numbers {
		offset, err := checkedRank(step, int64(i))
		if err != nil {
			return nil, false, err
		}
		result[n] = start + offset
	}
	return result, true, nil
}

func targetQueueOrders(normalized, snapshot contract.Object, mode string, rankStep int64) (map[int64]int64, []string, error) {
	if mode != "minimal" && mode != "normalize" {
		return nil, nil, fmt.Errorf("queue_mode must be minimal or normalize")
	}
	if rankStep <= 0 {
		return nil, nil, fmt.Errorf("rank_step must be a positive integer")
	}
	if mode == "normalize" {
		result := map[int64]int64{}
		for _, band := range decisionBands {
			numbers, _ := normalized[band].([]int64)
			r, err := denseOrders(numbers, rankStep)
			if err != nil {
				return nil, nil, err
			}
			for n, v := range r {
				result[n] = v
			}
		}
		return result, []string{}, nil
	}
	currentBands, currentItems, err := currentBandState(snapshot)
	if err != nil {
		return nil, nil, err
	}
	currentBandByNumber := map[int64]string{}
	for band, numbers := range currentBands {
		for _, number := range numbers {
			currentBandByNumber[number] = band
		}
	}
	desiredBandByNumber := map[int64]string{}
	for _, band := range decisionBands {
		numbers, _ := normalized[band].([]int64)
		for _, number := range numbers {
			desiredBandByNumber[number] = band
		}
	}
	result := map[int64]int64{}
	compacted := []string{}
	for _, band := range decisionBands {
		desired, _ := normalized[band].([]int64)
		if len(desired) == 0 {
			continue
		}
		current := currentBands[band]
		currentCandidates := []int64{}
		for _, number := range current {
			rank, ok := integerRank(currentItems[number].fields["Queue Order"])
			if desiredBandByNumber[number] == band && ok {
				_ = rank
				currentCandidates = append(currentCandidates, number)
			}
		}
		desiredCandidates := []int64{}
		for _, number := range desired {
			_, ok := integerRank(currentItems[number].fields["Queue Order"])
			if currentBandByNumber[number] == band && ok {
				desiredCandidates = append(desiredCandidates, number)
			}
		}
		anchors := lcs(currentCandidates, desiredCandidates)
		anchorRanks := make([]int64, len(anchors))
		usable := len(anchors) > 0
		for i, n := range anchors {
			rank, ok := integerRank(currentItems[n].fields["Queue Order"])
			if !ok {
				usable = false
				break
			}
			anchorRanks[i] = rank
		}
		for i := 1; i < len(anchorRanks); i++ {
			if anchorRanks[i-1] >= anchorRanks[i] {
				usable = false
			}
		}
		if !usable {
			dense, err := denseOrders(desired, rankStep)
			if err != nil {
				return nil, nil, err
			}
			mergeRanks(result, dense)
			compacted = append(compacted, bandPriority(band))
			continue
		}
		index := map[int64]int{}
		for i, n := range desired {
			index[n] = i
		}
		anchorByNumber := map[int64]int64{}
		for i, n := range anchors {
			anchorByNumber[n] = anchorRanks[i]
		}
		previousIndex := -1
		var previous *int64
		bandCompacted := false
		for _, anchor := range anchors {
			at := index[anchor]
			segment := desired[previousIndex+1 : at]
			if len(segment) > 0 {
				right := anchorByNumber[anchor]
				assigned, ok, err := assignSegment(segment, previous, &right, rankStep)
				if err != nil {
					return nil, nil, err
				}
				if !ok {
					bandCompacted = true
					break
				}
				mergeRanks(result, assigned)
			}
			right := anchorByNumber[anchor]
			result[anchor] = right
			previousIndex = at
			previous = &right
		}
		if bandCompacted {
			dense, err := denseOrders(desired, rankStep)
			if err != nil {
				return nil, nil, err
			}
			mergeRanks(result, dense)
			compacted = append(compacted, bandPriority(band))
			continue
		}
		trailing := desired[previousIndex+1:]
		if len(trailing) > 0 {
			assigned, ok, err := assignSegment(trailing, previous, nil, rankStep)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				dense, err := denseOrders(desired, rankStep)
				if err != nil {
					return nil, nil, err
				}
				mergeRanks(result, dense)
				compacted = append(compacted, bandPriority(band))
				continue
			}
			mergeRanks(result, assigned)
		}
	}
	return result, compacted, nil
}
func mergeRanks(target, source map[int64]int64) {
	for n, v := range source {
		target[n] = v
	}
}
func queueField(item queueItem, name string) any {
	value, ok := item.fields[name]
	if !ok {
		return nil
	}
	return value
}
func projectItemID(item contract.Object) any {
	if id, ok := item["item_id"]; ok {
		return id
	}
	if projectItem, ok := item["project_item"].(map[string]any); ok {
		return projectItem["item_id"]
	}
	return nil
}
func minimumGaps(normalized contract.Object, orders map[int64]int64) contract.Object {
	summary := contract.Object{}
	for _, band := range decisionBands {
		numbers, _ := normalized[band].([]int64)
		priority := bandPriority(band)
		values := []int64{}
		for _, number := range numbers {
			if value, ok := orders[number]; ok {
				values = append(values, value)
			}
		}
		if len(values) < 2 {
			summary[priority] = nil
			continue
		}
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		gap := values[1] - values[0]
		for i := 2; i < len(values); i++ {
			if diff := values[i] - values[i-1]; diff < gap {
				gap = diff
			}
		}
		summary[priority] = gap
	}
	return summary
}

// BuildRebalancePlan creates a provider-free project queue delta from validated
// decisions and explicit snapshots. Ranking weights, project policy, and any
// linked pull-request states come from the caller.
func BuildRebalancePlan(payload, issueGraph, queueSnapshot, projectSnapshot, options, backlogAudit contract.Object, excludedPrefixes []string) (contract.Object, error) {
	normalized, err := NormalizeRebalanceDecisions(payload, issueGraph, projectSnapshot, excludedPrefixes)
	if err != nil {
		return nil, err
	}
	if err := validateRepositoryPair(issueGraph, projectSnapshot); err != nil {
		return nil, err
	}
	if err := validateRepositoryPair(queueSnapshot, projectSnapshot); err != nil {
		return nil, err
	}
	if err := validateProjectPair(issueGraph, projectSnapshot); err != nil {
		return nil, err
	}
	if err := validateProjectPair(queueSnapshot, projectSnapshot); err != nil {
		return nil, err
	}
	mode, ok := options["queue_mode"].(string)
	if !ok || mode == "" {
		return nil, fmt.Errorf("options.queue_mode is required")
	}
	step, err := contract.PositiveInteger(options["rank_step"])
	if err != nil {
		return nil, fmt.Errorf("options.rank_step must be a positive integer")
	}
	targets, compacted, err := targetQueueOrders(normalized, queueSnapshot, mode, step)
	if err != nil {
		return nil, err
	}
	activeItems, err := queueItems(queueSnapshot, "items")
	if err != nil {
		return nil, err
	}
	activeByNumber := map[int64]queueItem{}
	for _, item := range activeItems {
		activeByNumber[item.number] = item
	}
	projectRows, err := contract.Objects(projectSnapshot, "items")
	if err != nil {
		return nil, err
	}
	projectByNumber := map[int64]contract.Object{}
	for i, item := range projectRows {
		n, present, err := optionalIssueNumber(item, fmt.Sprintf("project items[%d]", i))
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		projectByNumber[n] = item
	}
	graphIssues, err := contract.Objects(issueGraph, "issues")
	if err != nil {
		return nil, err
	}
	issuesByNumber := map[int64]contract.Object{}
	for _, issue := range graphIssues {
		n, err := issueNumber(issue, "issue graph item")
		if err != nil {
			return nil, err
		}
		issuesByNumber[n] = issue
	}
	linked := objectOrEmpty(options, "linked_prs_by_issue")
	if err := validateLinkedPRs(linked, issuesByNumber); err != nil {
		return nil, err
	}
	changes, unchanged := []any{}, []any{}
	for _, band := range decisionBands {
		priority := bandPriority(band)
		numbers, _ := normalized[band].([]int64)
		for _, number := range numbers {
			item, ok := activeByNumber[number]
			if !ok {
				return nil, fmt.Errorf("active issue #%d is missing from queue snapshot", number)
			}
			target, ok := targets[number]
			if !ok {
				return nil, fmt.Errorf("active issue #%d has no target queue rank", number)
			}
			statusTarget := any(nil)
			status := item.fields["Status"]
			if status != "Todo" && status != "In Progress" && status != "Done" {
				statusTarget = "Todo"
			}
			blockers, err := issueReferenceArray(issuesByNumber[number], "blocked_by_numbers")
			if err != nil {
				return nil, err
			}
			entry := contract.Object{"issue_number": number, "project_item_id": projectItemID(item.raw), "title": item.raw["title"], "current_priority": queueField(item, "Priority"), "target_priority": priority, "current_queue_order": queueField(item, "Queue Order"), "target_queue_order": target, "current_status": status, "target_status": statusTarget, "blocked_by_numbers": blockers}
			currentOrder, currentOK := normalizeQueueOrder(queueField(item, "Queue Order"))
			isSame := queueField(item, "Priority") == priority && currentOK && currentOrder == float64(target) && statusTarget == nil
			if isSame {
				unchanged = append(unchanged, entry)
			} else {
				changes = append(changes, entry)
			}
		}
	}
	archiveNumbers, _ := normalized["archive_issue_numbers"].([]int64)
	archiveActions := []any{}
	for _, number := range archiveNumbers {
		item, ok := projectByNumber[number]
		if !ok {
			return nil, fmt.Errorf("archive issue #%d is missing from project snapshot", number)
		}
		state := strings.ToUpper(text(item["state"]))
		status := objectOrEmpty(item, "field_values")["Status"]
		signals := []string{}
		if state == "CLOSED" {
			signals = append(signals, "issue-closed")
		}
		if status == "Done" {
			signals = append(signals, "project-status-done")
		}
		if linkedPRState(linked[strconv.FormatInt(number, 10)]) == "merged" {
			signals = append(signals, "linked-pr-merged")
		}
		canArchive := len(signals) > 0
		var setStatus any = nil
		if canArchive {
			setStatus = "Done"
		}
		var closeIssue any = nil
		if canArchive && state == "OPEN" {
			closeIssue = true
		} else {
			closeIssue = false
		}
		archiveActions = append(archiveActions, contract.Object{"issue_number": number, "project_item_id": projectItemID(item), "title": item["title"], "current_state": item["state"], "current_status": status, "can_archive": canArchive, "finished_signals": signals, "close_issue": closeIssue, "set_status": setStatus})
	}
	activeCount := 0
	for _, band := range decisionBands {
		numbers, _ := normalized[band].([]int64)
		activeCount += len(numbers)
	}
	legacyFractional := 0
	for _, item := range activeItems {
		if fractionalRank(queueField(item, "Queue Order")) {
			legacyFractional++
		}
	}
	queueChanges := 0
	for _, raw := range changes {
		row := raw.(contract.Object)
		current, currentOK := normalizeQueueOrder(row["current_queue_order"])
		target, _ := contract.Number(row["target_queue_order"])
		if !currentOK || current != target {
			queueChanges++
		}
	}
	ratio := float64(0)
	if activeCount > 0 {
		ratio = math.RoundToEven(float64(queueChanges)/float64(activeCount)*1000) / 1000
	}
	project := projectIdentity(issueGraph, projectSnapshot)
	repo := firstPresent(issueGraph["repo"], projectSnapshot["repo"])
	queueItemsTotal := queueSnapshot["item_count"]
	if queueItemsTotal == nil {
		queueItemsTotal = len(activeItems)
	}
	projectItemCount := queueSnapshot["project_item_count"]
	if projectItemCount == nil {
		projectItemCount = activeCount
	}
	excludedCount := queueSnapshot["excluded_count"]
	if excludedCount == nil {
		if rows, ok := queueSnapshot["excluded_items"].([]any); ok {
			excludedCount = len(rows)
		} else {
			excludedCount = 0
		}
	}
	invalidArchiveCount := 0
	for _, raw := range archiveActions {
		if raw.(contract.Object)["can_archive"] != true {
			invalidArchiveCount++
		}
	}
	summary := contract.Object{"queue_mode": mode, "project_item_count": projectItemCount, "queue_item_count": queueItemsTotal, "excluded_count": excludedCount, "change_count": len(changes), "queue_change_count": queueChanges, "queue_change_ratio": ratio, "unchanged_count": len(unchanged), "legacy_fractional_queue_count": legacyFractional, "local_compaction_band_count": len(compacted), "local_compaction_bands": compacted, "minimum_gap_by_priority": minimumGaps(normalized, targets), "archive_count": len(archiveActions), "invalid_archive_count": invalidArchiveCount, "audit_finding_count": 0}
	if auditSummary, ok := backlogAudit["summary"].(map[string]any); ok {
		summary["audit_finding_count"] = auditSummary["finding_count"]
	}
	if compactedWarning(mode, compacted, queueChanges, activeCount) != "" {
		summary["churn_warning"] = compactedWarning(mode, compacted, queueChanges, activeCount)
		summary["compaction_recommended"] = true
	}
	plan := contract.Object{"schema_version": normalized["schema_version"], "queue_mode": mode, "repo": repo, "project": project, "summary": summary, "changes": changes, "unchanged": unchanged, "archive_actions": archiveActions}
	if value, ok := backlogAudit["summary"]; ok {
		plan["audit_summary"] = value
	}
	if value, ok := backlogAudit["findings"]; ok {
		plan["audit_findings"] = value
	}
	if evidence, ok := queueInventory(queueSnapshot, objectOrEmpty(projectSnapshot, "project")); ok {
		plan["queue_inventory"] = evidence
	}
	return contract.Object{"normalized": normalized, "delta": plan}, nil
}

func compactedWarning(mode string, bands []string, changes, count int) string {
	if mode == "minimal" && len(bands) > 0 {
		return "Minimal queue mode exhausted sparse gaps in " + strings.Join(bands, ", ") + ", so those bands were compacted to fresh sparse ranks."
	}
	if mode == "minimal" && changes >= 3 && count > 0 && float64(changes)/float64(count) >= 0.5 {
		return fmt.Sprintf("Minimal queue mode would rewrite %d/%d queue orders; consider queue_mode=normalize for deliberate compaction.", changes, count)
	}
	return ""
}
func projectIdentity(issueGraph, projectSnapshot contract.Object) contract.Object {
	project := objectOrEmpty(issueGraph, "project")
	if len(project) == 0 {
		project = objectOrEmpty(projectSnapshot, "project")
	}
	result := contract.Object{}
	for _, key := range []string{"id", "number", "title", "owner_login", "url"} {
		if value, ok := project[key]; ok {
			result[key] = value
		}
	}
	if len(result) == 0 {
		return contract.Object{"title": objectOrEmpty(projectSnapshot, "project")["title"]}
	}
	return result
}
func queueInventory(snapshot, project contract.Object) (contract.Object, bool) {
	rows, err := contract.Objects(snapshot, "items")
	if err != nil {
		return nil, false
	}
	projectID, ok := project["id"].(string)
	if !ok || projectID == "" {
		return nil, false
	}
	result := []any{}
	seen := map[int64]bool{}
	for _, item := range rows {
		number, err := issueNumber(item, "queue item")
		if err != nil || seen[number] || item["state"] != "OPEN" {
			return nil, false
		}
		seen[number] = true
		title, ok := item["title"].(string)
		if !ok || title == "" {
			return nil, false
		}
		itemID, ok := projectItemID(item).(string)
		if !ok || itemID == "" {
			return nil, false
		}
		fields, ok := item["field_values"].(map[string]any)
		if !ok {
			return nil, false
		}
		rowFields := contract.Object{}
		for _, name := range []string{"Priority", "Queue Order", "Status"} {
			value, present := fields[name]
			if present && name != "Queue Order" {
				if _, ok := value.(string); !ok {
					return nil, false
				}
			}
			if present && name == "Queue Order" {
				if _, err := contract.Number(value); err != nil {
					return nil, false
				}
			}
			rowFields[name] = contract.Object{"present": present, "value": value}
		}
		result = append(result, contract.Object{"issue_number": number, "project_item_id": itemID, "title": title, "state": "OPEN", "field_values": rowFields})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].(contract.Object)["issue_number"].(int64) < result[j].(contract.Object)["issue_number"].(int64)
	})
	return contract.Object{"schema_version": int64(1), "project_id": projectID, "items": result}, true
}
