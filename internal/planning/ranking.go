package planning

import (
	"container/heap"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

type bodyPattern struct {
	name, source string
	weight       int64
	re           *regexp.Regexp
}
type rankedCandidate struct {
	report             contract.Object
	score              int64
	priority           string
	order              float64
	number             int64
	eligible           bool
	blockers, children []int64
}

func validatePolicy(policy contract.Object) (contract.Object, []bodyPattern, error) {
	version, err := contract.Integer(policy["schema_version"])
	if err != nil || version != 1 {
		return nil, nil, fmt.Errorf("ranking policy schema_version must be the integer 1")
	}
	targets, err := contract.ObjectAt(policy, "band_targets")
	if err != nil {
		return nil, nil, err
	}
	now, err := contract.Integer(targets["now"])
	if err != nil || now < 0 {
		return nil, nil, fmt.Errorf("policy band_targets.now must be a non-negative integer")
	}
	next, err := contract.Integer(targets["next"])
	if err != nil || next < 0 {
		return nil, nil, fmt.Errorf("policy band_targets.next must be a non-negative integer")
	}
	step, err := contract.PositiveInteger(policy["sparse_rank_step"])
	if err != nil {
		return nil, nil, fmt.Errorf("policy sparse_rank_step must be a positive integer")
	}
	weights, err := contract.ObjectAt(policy, "weights")
	if err != nil {
		return nil, nil, fmt.Errorf("ranking policy must define weights")
	}
	patterns, err := contract.Array(policy, "body_patterns")
	if err != nil {
		return nil, nil, fmt.Errorf("ranking policy must define body_patterns")
	}
	normalized := contract.Object{"schema_version": int64(1), "band_targets": contract.Object{"now": now, "next": next}, "sparse_rank_step": step, "weights": weights}
	compiled := make([]bodyPattern, 0, len(patterns))
	for i, raw := range patterns {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("body_patterns[%d] must be an object", i)
		}
		name, ok := item["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, nil, fmt.Errorf("body_patterns[%d].name must be nonempty", i)
		}
		source, ok := item["pattern"].(string)
		if !ok || strings.TrimSpace(source) == "" {
			return nil, nil, fmt.Errorf("body pattern %q must define a regex", name)
		}
		weight, err := contract.Integer(item["weight"])
		if err != nil {
			return nil, nil, fmt.Errorf("body pattern %q weight must be an integer", name)
		}
		re, err := regexp.Compile(source)
		if err != nil {
			return nil, nil, fmt.Errorf("body pattern %q has invalid regex: %w", name, err)
		}
		compiled = append(compiled, bodyPattern{name: strings.TrimSpace(name), source: source, weight: weight, re: re})
	}
	normalized["body_patterns"] = patterns
	return normalized, compiled, nil
}

func weight(weights contract.Object, category, key string) (int64, error) {
	sub := objectOrEmpty(weights, category)
	if raw, ok := sub[key]; ok {
		return contract.Integer(raw)
	}
	if raw, ok := sub["unknown"]; ok {
		return contract.Integer(raw)
	}
	return 0, nil
}
func addScore(components *[]any, source string, weight int64, detail string) int64 {
	if weight != 0 {
		*components = append(*components, contract.Object{"source": source, "weight": weight, "detail": detail})
	}
	return weight
}
func normalizedQueueOrder(value any) (float64, bool) {
	number, err := contract.Number(value)
	if err != nil {
		return 0, false
	}
	rounded := math.RoundToEven(number*1e6) / 1e6
	if rounded == 0 {
		rounded = 0
	}
	return rounded, true
}
func isFractionalQueueOrder(value any) bool {
	n, ok := normalizedQueueOrder(value)
	return ok && math.Trunc(n) != n
}
func linkedPRState(value any) string {
	pr, ok := value.(map[string]any)
	if !ok {
		return "missing"
	}
	if pr["is_merged"] == true {
		return "merged"
	}
	if pr["state"] == "OPEN" {
		if pr["is_draft"] == true {
			return "open_draft"
		}
		return "open_ready"
	}
	return "closed_unmerged"
}

func validateLinkedPRs(linked contract.Object, issues map[int64]contract.Object) error {
	for rawNumber, rawPR := range linked {
		number, err := strconv.ParseInt(rawNumber, 10, 64)
		if err != nil || number <= 0 || strconv.FormatInt(number, 10) != rawNumber || issues[number] == nil {
			return fmt.Errorf("linked_prs_by_issue contains an invalid or unknown issue key %q", rawNumber)
		}
		if rawPR == nil {
			continue
		}
		pr, ok := rawPR.(map[string]any)
		if !ok {
			return fmt.Errorf("linked pull request for issue #%d must be an object or null", number)
		}
		if state, exists := pr["state"]; exists {
			if _, ok := state.(string); !ok {
				return fmt.Errorf("linked pull request state for issue #%d must be a string", number)
			}
		}
		for _, key := range []string{"is_merged", "is_draft"} {
			if value, exists := pr[key]; exists {
				if _, ok := value.(bool); !ok {
					return fmt.Errorf("linked pull request %s for issue #%d must be a boolean", key, number)
				}
			}
		}
	}
	return nil
}

func linkedPRCount(linked contract.Object, candidates map[int64]rankedCandidate) int {
	count := 0
	for number := range candidates {
		if value, exists := linked[strconv.FormatInt(number, 10)]; exists && value != nil {
			count++
		}
	}
	return count
}
func issueReferenceArray(issue contract.Object, key string) ([]int64, error) {
	return arrayPositiveIntegers(defaultArray(issue[key]), "issue "+key)
}
func prefixFor(issue contract.Object) string {
	if prefix := text(issue["prefix"]); prefix != "" {
		return prefix
	}
	return prefixOf(issue)
}
func scoreSortLess(a, b rankedCandidate) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	if priorityIndex(a.priority) != priorityIndex(b.priority) {
		return priorityIndex(a.priority) < priorityIndex(b.priority)
	}
	if a.order != b.order {
		return a.order < b.order
	}
	return a.number < b.number
}

// BuildRankedRebalanceSeed ranks active queue rows using a caller-supplied
// policy and snapshots. No repository configuration or pull-request lookup is
// performed; linked PR state must be supplied in linkedPRsByIssue.
func BuildRankedRebalanceSeed(issueGraph, queueSnapshot, projectSnapshot, policy, linkedPRsByIssue contract.Object, excludedPrefixes []string) (contract.Object, error) {
	normalizedPolicy, patterns, err := validatePolicy(policy)
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
	if _, err := contract.Objects(projectSnapshot, "items"); err != nil {
		return nil, err
	}
	graphIssues, err := contract.Objects(issueGraph, "issues")
	if err != nil {
		return nil, err
	}
	issuesByNumber := map[int64]contract.Object{}
	for i, issue := range graphIssues {
		number, err := issueNumber(issue, fmt.Sprintf("issues[%d]", i))
		if err != nil || issuesByNumber[number] != nil {
			return nil, fmt.Errorf("issue graph contains invalid or duplicate issue number")
		}
		issuesByNumber[number] = issue
	}
	items, err := queueItems(queueSnapshot, "items")
	if err != nil {
		return nil, err
	}
	excludedSet := stringSet(excludedPrefixes)
	archiveSet := intSet(archiveCandidates(projectSnapshot, excludedSet))
	active := []queueItem{}
	for _, item := range items {
		issue := issuesByNumber[item.number]
		if issue == nil {
			return nil, fmt.Errorf("queue issue #%d is missing from issue graph", item.number)
		}
		if archiveSet[item.number] || excludedSet[prefixFor(issue)] || excludedSet[prefixFor(item.raw)] {
			continue
		}
		active = append(active, item)
	}
	if linkedPRsByIssue == nil {
		linkedPRsByIssue = contract.Object{}
	}
	if err := validateLinkedPRs(linkedPRsByIssue, issuesByNumber); err != nil {
		return nil, err
	}
	candidates := map[int64]rankedCandidate{}
	weights := objectOrEmpty(normalizedPolicy, "weights")
	for _, item := range active {
		issue := issuesByNumber[item.number]
		if state := strings.ToUpper(text(issue["state"])); state != "" && state != "OPEN" {
			return nil, fmt.Errorf("queue issue #%d is not open in the issue graph", item.number)
		}
		candidate, err := buildCandidate(item, issue, linkedPRsByIssue, weights, patterns)
		if err != nil {
			return nil, err
		}
		candidates[item.number] = candidate
	}
	ranked, cycles, reasons := stableRankedOrder(candidates)
	eligible := []int64{}
	for _, number := range ranked {
		if candidates[number].eligible {
			eligible = append(eligible, number)
		}
	}
	targets := objectOrEmpty(normalizedPolicy, "band_targets")
	nowTarget, _ := contract.Integer(targets["now"])
	nextTarget, _ := contract.Integer(targets["next"])
	if nowTarget > int64(len(ranked)) {
		nowTarget = int64(len(ranked))
	}
	if nextTarget > int64(len(ranked))-nowTarget {
		nextTarget = int64(len(ranked)) - nowTarget
	}
	now := []int64{}
	if nowTarget == 0 {
		// An explicit zero target means the consumer has disabled this band.
	} else if int64(len(eligible)) >= nowTarget {
		for _, number := range ranked {
			if candidates[number].eligible {
				now = append(now, number)
				if int64(len(now)) >= nowTarget {
					break
				}
			}
		}
	} else {
		now = append(now, ranked[:int(nowTarget)]...)
	}
	nowSet := intSet(now)
	remaining := []int64{}
	for _, number := range ranked {
		if !nowSet[number] {
			remaining = append(remaining, number)
		}
	}
	next := append([]int64{}, remaining[:int(nextTarget)]...)
	nextSet := intSet(next)
	later := []int64{}
	for _, number := range remaining {
		if !nextSet[number] {
			later = append(later, number)
		}
	}
	assigned := map[int64]string{}
	for _, n := range now {
		assigned[n] = "Now"
	}
	for _, n := range next {
		assigned[n] = "Next"
	}
	for _, n := range later {
		assigned[n] = "Later"
	}
	reportIssues := []any{}
	for index, number := range ranked {
		candidate := candidates[number]
		row := contract.Object{"rank": index + 1, "issue_number": number, "title": candidate.report["title"], "assigned_band": assigned[number], "score": candidate.score, "current_priority": candidate.priority, "current_queue_order": candidate.report["current_queue_order"], "current_status": candidate.report["current_status"], "milestone": candidate.report["milestone"], "blocked_by_numbers": candidate.blockers, "actionable_child_numbers": candidate.children, "child_numbers": candidate.report["child_numbers"], "parent_number": candidate.report["parent_number"], "eligible_for_now": candidate.eligible, "now_ineligibility_reasons": candidate.report["now_ineligibility_reasons"], "linked_pr_state": candidate.report["linked_pr_state"], "score_breakdown": candidate.report["score_breakdown"], "body_pattern_matches": candidate.report["body_pattern_matches"], "precedence_reasons": reasons[number], "legacy_fractional_queue_order": candidate.report["legacy_fractional_queue_order"]}
		reportIssues = append(reportIssues, row)
	}
	project := projectSnapshot["project"]
	decisions := contract.Object{"schema_version": int64(1), "project_title": objectOrEmpty(projectSnapshot, "project")["title"], "now_issue_numbers": now, "next_issue_numbers": next, "later_issue_numbers": later, "archive_issue_numbers": sortedIssueNumbers(archiveSet)}
	decisions["notes"] = "Auto-generated by the deterministic rebalance ranking engine. Review and edit before apply."
	archiveNumbers := decisions["archive_issue_numbers"].([]int64)
	bandCounts := contract.Object{"Now": nowTarget, "Next": nextTarget, "Later": int64(len(ranked)) - nowTarget - nextTarget}
	fracCount := int64(0)
	for _, candidate := range candidates {
		if candidate.report["legacy_fractional_queue_order"] == true {
			fracCount++
		}
	}
	excludedCount := len(items) - len(active) + snapshotExcludedCount(queueSnapshot)
	report := contract.Object{"tool": "build_ranked_rebalance_seed", "repo": firstPresent(issueGraph["repo"], projectSnapshot["repo"]), "project": project, "policy": contract.Object{"band_targets": normalizedPolicy["band_targets"], "sparse_rank_step": normalizedPolicy["sparse_rank_step"]}, "summary": contract.Object{"active_issue_count": len(ranked), "archive_candidate_count": len(archiveNumbers), "eligible_now_count": len(eligible), "cycle_count": len(cycles), "linked_pr_count": linkedPRCount(linkedPRsByIssue, candidates), "legacy_fractional_queue_order_count": fracCount, "excluded_count": excludedCount, "band_targets": bandCounts}, "cycles": cycles, "decisions": decisions, "issues": reportIssues}
	return contract.Object{"decisions": decisions, "report": report}, nil
}

func buildCandidate(item queueItem, issue contract.Object, linked map[string]any, weights contract.Object, patterns []bodyPattern) (rankedCandidate, error) {
	components := []any{}
	score := int64(0)
	priority := item.priority
	status := text(item.fields["Status"])
	queue, queueOK := normalizedQueueOrder(item.fields["Queue Order"])
	appendWeight := func(source, category, key, detail string) error {
		weight, err := weight(weights, category, key)
		if err != nil {
			return fmt.Errorf("weight %s.%s must be an integer", category, key)
		}
		updated, err := checkedScoreAdd(score, weight)
		if err != nil {
			return err
		}
		score = updated
		addScore(&components, source, weight, detail)
		return nil
	}
	if err := appendWeight("current-priority", "current_priority", priority, fmt.Sprintf("current priority %s", fallback(priority, "unknown"))); err != nil {
		return rankedCandidate{}, err
	}
	if err := appendWeight("status", "status", status, fmt.Sprintf("status %s", fallback(status, "unknown"))); err != nil {
		return rankedCandidate{}, err
	}
	milestone := issue["milestone"]
	if milestone != nil && milestone != "" {
		if err := appendWeight("milestone", "milestone", "present", fmt.Sprintf("milestone %v", milestone)); err != nil {
			return rankedCandidate{}, err
		}
	}
	prefix := prefixFor(issue)
	if err := appendWeight("prefix", "prefix", prefix, fmt.Sprintf("prefix %s", fallback(prefix, "unknown"))); err != nil {
		return rankedCandidate{}, err
	}
	blockers, err := issueReferenceArray(issue, "blocked_by_numbers")
	if err != nil {
		return rankedCandidate{}, err
	}
	blocking, err := issueReferenceArray(issue, "blocking_numbers")
	if err != nil {
		return rankedCandidate{}, err
	}
	children, err := arrayPositiveIntegers(defaultArray(item.raw["actionable_child_numbers"]), "actionable children")
	if err != nil {
		return rankedCandidate{}, err
	}
	allChildren, err := arrayPositiveIntegers(defaultArray(item.raw["child_numbers"]), "children")
	if err != nil {
		return rankedCandidate{}, err
	}
	if len(blockers) > 0 {
		if err := appendWeight("dependency", "dependency", "blocked", fmt.Sprintf("blocked by %v", blockers)); err != nil {
			return rankedCandidate{}, err
		}
	}
	if len(blocking) > 0 {
		base, err := weight(weights, "dependency", "blocking")
		if err != nil {
			return rankedCandidate{}, err
		}
		if base != 0 && int64(len(blocking)) > math.MaxInt64/absInt64(base) {
			return rankedCandidate{}, fmt.Errorf("ranking score exceeds supported integer range")
		}
		combined := base * int64(len(blocking))
		updated, err := checkedScoreAdd(score, combined)
		if err != nil {
			return rankedCandidate{}, err
		}
		score = updated
		addScore(&components, "dependency", combined, fmt.Sprintf("blocking %v", blocking))
	}
	if len(children) > 0 {
		if err := appendWeight("hierarchy", "hierarchy", "has_actionable_child", fmt.Sprintf("actionable children %v", children)); err != nil {
			return rankedCandidate{}, err
		}
	} else if len(allChildren) > 0 {
		if err := appendWeight("hierarchy", "hierarchy", "has_children", fmt.Sprintf("children %v", allChildren)); err != nil {
			return rankedCandidate{}, err
		}
	}
	if priorityIndex(priority) == len(priorityBands) {
		if err := appendWeight("field-health", "field_health", "missing_priority", "missing current priority"); err != nil {
			return rankedCandidate{}, err
		}
	}
	if !queueOK {
		if err := appendWeight("field-health", "field_health", "missing_queue_order", "missing current queue order"); err != nil {
			return rankedCandidate{}, err
		}
	}
	body := text(issue["body"])
	matches := []any{}
	for _, pattern := range patterns {
		if pattern.re.MatchString(body) {
			matches = append(matches, contract.Object{"name": pattern.name, "weight": pattern.weight})
			updated, err := checkedScoreAdd(score, pattern.weight)
			if err != nil {
				return rankedCandidate{}, err
			}
			score = updated
			addScore(&components, "body-pattern", pattern.weight, pattern.name)
		}
	}
	prValue := linked[strconv.FormatInt(item.number, 10)]
	prState := linkedPRState(prValue)
	if err := appendWeight("linked-pr", "pr_state", prState, "linked PR "+prState); err != nil {
		return rankedCandidate{}, err
	}
	if !queueOK {
		queue = math.Inf(1)
	}
	eligible := len(blockers) == 0 && len(children) == 0
	ineligible := []any{}
	if len(blockers) > 0 {
		ineligible = append(ineligible, "blocked")
	}
	if len(children) > 0 {
		ineligible = append(ineligible, "parent-with-actionable-child")
	}
	childValue := any(nil)
	if issue["parent_number"] != nil {
		childValue = issue["parent_number"]
	}
	report := contract.Object{"title": issue["title"], "prefix": prefix, "body": body, "current_queue_order": func() any {
		if queueOK {
			return queue
		}
		return nil
	}(), "current_status": item.fields["Status"], "milestone": milestone, "child_numbers": allChildren, "parent_number": childValue, "linked_pr_state": prState, "score_breakdown": components, "body_pattern_matches": matches, "now_ineligibility_reasons": ineligible, "legacy_fractional_queue_order": isFractionalQueueOrder(item.fields["Queue Order"])}
	return rankedCandidate{report: report, score: score, priority: priority, order: queue, number: item.number, eligible: eligible, blockers: blockers, children: children}, nil
}

func checkedScoreAdd(current, delta int64) (int64, error) {
	if (delta > 0 && current > math.MaxInt64-delta) || (delta < 0 && current < math.MinInt64-delta) {
		return 0, fmt.Errorf("ranking score exceeds supported integer range")
	}
	return current + delta, nil
}

func absInt64(value int64) int64 {
	if value == math.MinInt64 {
		return math.MaxInt64
	}
	if value < 0 {
		return -value
	}
	return value
}

func fallback(value, otherwise string) string {
	if value == "" {
		return otherwise
	}
	return value
}
func stringSet(values []string) map[string]bool {
	result := map[string]bool{}
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			result[v] = true
		}
	}
	return result
}
func intSet(values []int64) map[int64]bool {
	result := map[int64]bool{}
	for _, v := range values {
		result[v] = true
	}
	return result
}
func firstPresent(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func stableRankedOrder(candidates map[int64]rankedCandidate) ([]int64, []int64, map[int64][]string) {
	successors := map[int64]map[int64]bool{}
	indegree := map[int64]int{}
	reasons := map[int64][]string{}
	for number := range candidates {
		successors[number] = map[int64]bool{}
		indegree[number] = 0
		reasons[number] = []string{}
	}
	add := func(from, to int64, reason string) {
		if _, ok := candidates[from]; !ok {
			return
		}
		if _, ok := candidates[to]; !ok {
			return
		}
		if successors[from][to] {
			return
		}
		successors[from][to] = true
		indegree[to]++
		reasons[to] = append(reasons[to], reason)
	}
	for number, candidate := range candidates {
		for _, blocker := range candidate.blockers {
			add(blocker, number, fmt.Sprintf("blocked-by #%d", blocker))
		}
		for _, child := range candidate.children {
			add(child, number, fmt.Sprintf("actionable-child #%d", child))
		}
	}
	available := rankedCandidateHeap{}
	for number, degree := range indegree {
		if degree == 0 {
			heap.Push(&available, candidates[number])
		}
	}
	heap.Init(&available)
	ordered := []int64{}
	for available.Len() > 0 {
		candidate := heap.Pop(&available).(rankedCandidate)
		ordered = append(ordered, candidate.number)
		nexts := make([]int64, 0, len(successors[candidate.number]))
		for n := range successors[candidate.number] {
			nexts = append(nexts, n)
		}
		sort.Slice(nexts, func(i, j int) bool { return nexts[i] < nexts[j] })
		for _, next := range nexts {
			indegree[next]--
			if indegree[next] == 0 {
				heap.Push(&available, candidates[next])
			}
		}
	}
	cycles := []int64{}
	for number, degree := range indegree {
		if degree > 0 {
			cycles = append(cycles, number)
		}
	}
	sort.Slice(cycles, func(i, j int) bool { return scoreSortLess(candidates[cycles[i]], candidates[cycles[j]]) })
	ordered = append(ordered, cycles...)
	for n := range reasons {
		sort.Strings(reasons[n])
	}
	return ordered, cycles, reasons
}

type rankedCandidateHeap []rankedCandidate

func (h rankedCandidateHeap) Len() int           { return len(h) }
func (h rankedCandidateHeap) Less(i, j int) bool { return scoreSortLess(h[i], h[j]) }
func (h rankedCandidateHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *rankedCandidateHeap) Push(value any)    { *h = append(*h, value.(rankedCandidate)) }
func (h *rankedCandidateHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func archiveCandidates(projectSnapshot contract.Object, excluded map[string]bool) []int64 {
	rows, err := contract.Objects(projectSnapshot, "items")
	if err != nil {
		return nil
	}
	set := map[int64]bool{}
	for _, item := range rows {
		number, err := issueNumber(item, "project item")
		if err != nil || excluded[prefixFor(item)] {
			continue
		}
		state := strings.ToUpper(text(item["state"]))
		fields := objectOrEmpty(item, "field_values")
		if state == "CLOSED" || fields["Status"] == "Done" {
			set[number] = true
		}
	}
	result := sortedIssueNumbers(set)
	return result
}

func snapshotExcludedCount(snapshot contract.Object) int {
	if count, err := contract.Integer(snapshot["excluded_count"]); err == nil && count >= 0 {
		return int(count)
	}
	if items, ok := snapshot["excluded_items"].([]any); ok {
		return len(items)
	}
	return 0
}

func validateRepositoryPair(left, right contract.Object) error {
	a, okA := left["repo"].(map[string]any)
	b, okB := right["repo"].(map[string]any)
	if !okA || !okB {
		return fmt.Errorf("issue graph and project snapshot must both identify a repository")
	}
	ra, err := contract.ParseRepository(a)
	if err != nil {
		return err
	}
	rb, err := contract.ParseRepository(b)
	if err != nil {
		return err
	}
	if ra != rb {
		return fmt.Errorf("issue graph and project snapshot repository/host identities differ")
	}
	return nil
}
func validateProjectPair(left, right contract.Object) error {
	a, okA := left["project"].(map[string]any)
	b, okB := right["project"].(map[string]any)
	if !okA || !okB {
		return fmt.Errorf("queue and project snapshots must both identify a project")
	}
	leftID, leftIDOK := a["id"].(string)
	rightID, rightIDOK := b["id"].(string)
	leftTitle, leftTitleOK := a["title"].(string)
	rightTitle, rightTitleOK := b["title"].(string)
	if !leftIDOK || !rightIDOK || leftID == "" || leftID != rightID {
		return fmt.Errorf("queue and project snapshot project id identities differ")
	}
	if !leftTitleOK || !rightTitleOK || strings.TrimSpace(leftTitle) == "" || leftTitle != rightTitle {
		return fmt.Errorf("queue and project snapshot project title identities differ")
	}
	leftNumber, leftErr := contract.PositiveInteger(a["number"])
	rightNumber, rightErr := contract.PositiveInteger(b["number"])
	if leftErr != nil || rightErr != nil || leftNumber != rightNumber {
		return fmt.Errorf("queue and project snapshot project number identities differ")
	}
	return nil
}
