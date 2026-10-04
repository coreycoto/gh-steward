package planning

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var relationshipChangeOrder = map[string]int{
	"clear_parent": 0, "remove_blocked_by": 1, "add_blocked_by": 2,
	"set_parent": 3, "reparent": 4,
}

type relationshipState struct {
	blockers map[int64][]int64
	parents  map[int64]*int64
	issues   map[int64]contract.Object
}

func issueNumber(issue contract.Object, where string) (int64, error) {
	n, err := contract.PositiveInteger(issue["number"])
	if err != nil {
		return 0, fmt.Errorf("%s.number must be a positive integer", where)
	}
	return n, nil
}

func relationshipGraph(graph contract.Object) (relationshipState, error) {
	state := relationshipState{
		blockers: map[int64][]int64{}, parents: map[int64]*int64{}, issues: map[int64]contract.Object{},
	}
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return state, fmt.Errorf("relationship graph: %w", err)
	}
	for i, issue := range issues {
		number, err := issueNumber(issue, fmt.Sprintf("issues[%d]", i))
		if err != nil || state.issues[number] != nil {
			return state, fmt.Errorf("relationship graph contains an invalid or duplicate issue number")
		}
		blockerValues, ok := issue["blocked_by_numbers"]
		if !ok {
			return state, fmt.Errorf("live blocker state for issue #%d is missing", number)
		}
		blockers, err := positiveNumberSet(blockerValues, fmt.Sprintf("issue #%d blockers", number))
		if err != nil {
			return state, err
		}
		parentValue, ok := issue["parent_number"]
		if !ok {
			return state, fmt.Errorf("live parent state for issue #%d is missing", number)
		}
		var parent *int64
		if parentValue != nil {
			value, err := contract.PositiveInteger(parentValue)
			if err != nil {
				return state, fmt.Errorf("live parent state for issue #%d is malformed", number)
			}
			parent = &value
		}
		childrenValue, ok := issue["child_numbers"]
		if !ok {
			return state, fmt.Errorf("live child state for issue #%d is missing", number)
		}
		children, err := positiveNumberSet(childrenValue, fmt.Sprintf("issue #%d children", number))
		if err != nil {
			return state, err
		}
		state.blockers[number], state.parents[number], state.issues[number] = blockers, parent, issue
		_ = children // retained below for graph consistency validation
	}

	known := state.issues
	parentCandidates := map[int64]map[int64]bool{}
	for number, issue := range state.issues {
		for _, blocker := range state.blockers[number] {
			if known[blocker] == nil {
				return state, fmt.Errorf("live blocker state for issue #%d references an issue outside the complete graph", number)
			}
		}
		if parent := state.parents[number]; parent != nil && known[*parent] == nil {
			return state, fmt.Errorf("live parent state for issue #%d references an issue outside the complete graph", number)
		}
		children, err := positiveNumberSet(issue["child_numbers"], fmt.Sprintf("issue #%d children", number))
		if err != nil {
			return state, err
		}
		for _, child := range children {
			if known[child] == nil {
				return state, fmt.Errorf("live child state for issue #%d references an issue outside the complete graph", number)
			}
			if parentCandidates[child] == nil {
				parentCandidates[child] = map[int64]bool{}
			}
			parentCandidates[child][number] = true
		}
	}
	for child := range state.issues {
		candidates := parentCandidates[child]
		if len(candidates) > 1 {
			return state, fmt.Errorf("live relationship graph reports multiple parents for issue #%d", child)
		}
		parent := state.parents[child]
		if parent == nil && len(candidates) != 0 || parent != nil && (len(candidates) != 1 || !candidates[*parent]) {
			return state, fmt.Errorf("live parent and child state disagree for issue #%d", child)
		}
	}
	return state, nil
}

// relationshipStateForPreview mirrors the offline preview path: absent edge
// fields mean no known blockers or parent, while any supplied malformed edge
// value still fails closed. Apply-oriented complete topology uses
// RelationshipTopology above.
func relationshipStateForPreview(graph contract.Object) (relationshipState, error) {
	state := relationshipState{blockers: map[int64][]int64{}, parents: map[int64]*int64{}, issues: map[int64]contract.Object{}}
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return state, err
	}
	for i, issue := range issues {
		number, err := issueNumber(issue, fmt.Sprintf("issues[%d]", i))
		if err != nil || state.issues[number] != nil {
			return state, fmt.Errorf("issue graph contains an invalid or duplicate issue number")
		}
		blockers := []int64{}
		if raw, exists := issue["blocked_by_numbers"]; exists && raw != nil {
			blockers, err = positiveNumberSet(raw, fmt.Sprintf("issue #%d blockers", number))
			if err != nil {
				return state, err
			}
		}
		var parent *int64
		if raw, exists := issue["parent_number"]; exists && raw != nil {
			value, err := contract.PositiveInteger(raw)
			if err != nil {
				return state, fmt.Errorf("issue #%d parent_number must be a positive integer or null", number)
			}
			parent = &value
		}
		state.blockers[number], state.parents[number], state.issues[number] = blockers, parent, issue
	}
	return state, nil
}

// RelationshipTopology validates a complete local issue graph and returns its
// canonical blocker and parent maps, keyed by decimal issue number.
func RelationshipTopology(issueGraph contract.Object) (contract.Object, error) {
	state, err := relationshipGraph(issueGraph)
	if err != nil {
		return nil, err
	}
	blockers := contract.Object{}
	parents := contract.Object{}
	numbers := sortedIssueNumbers(state.issues)
	for _, number := range numbers {
		key := strconv.FormatInt(number, 10)
		blockers[key] = state.blockers[number]
		if parent := state.parents[number]; parent == nil {
			parents[key] = nil
		} else {
			parents[key] = *parent
		}
	}
	return contract.Object{"blocked_by": blockers, "parent": parents}, nil
}

func positiveNumberSet(raw any, where string) ([]int64, error) {
	values, ok := raw.([]any)
	if !ok {
		if typed, ok := raw.([]int64); ok {
			values = make([]any, len(typed))
			for i, value := range typed {
				values[i] = value
			}
		} else if typed, ok := raw.([]int); ok {
			values = make([]any, len(typed))
			for i, value := range typed {
				values[i] = value
			}
		} else {
			return nil, fmt.Errorf("%s must be an array of positive integers", where)
		}
	}
	set := map[int64]bool{}
	for _, rawNumber := range values {
		number, err := contract.PositiveInteger(rawNumber)
		if err != nil {
			return nil, fmt.Errorf("%s contains a non-positive or non-integer reference", where)
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

func sortedIssueNumbers[T any](items map[int64]T) []int64 {
	numbers := make([]int64, 0, len(items))
	for number := range items {
		numbers = append(numbers, number)
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	return numbers
}

func topologyObject(state relationshipState) contract.Object {
	blockers, parents := contract.Object{}, contract.Object{}
	for _, number := range sortedIssueNumbers(state.issues) {
		key := strconv.FormatInt(number, 10)
		blockers[key] = append([]int64{}, state.blockers[number]...)
		if parent := state.parents[number]; parent == nil {
			parents[key] = nil
		} else {
			parents[key] = *parent
		}
	}
	return contract.Object{"blocked_by": blockers, "parent": parents}
}

func adjacencyFromBlockers(blockers map[int64][]int64) map[int64][]int64 {
	result := make(map[int64][]int64, len(blockers))
	for issue, values := range blockers {
		result[issue] = append([]int64{}, values...)
	}
	return result
}

func adjacencyFromParents(parents map[int64]*int64) map[int64][]int64 {
	result := make(map[int64][]int64, len(parents))
	for issue, parent := range parents {
		if parent != nil {
			result[issue] = []int64{*parent}
		} else {
			result[issue] = []int64{}
		}
	}
	return result
}

// FindDirectedCycles returns each simple directed cycle once, normalized by
// its smallest rotation. Input keys and references are positive issue numbers.
func FindDirectedCycles(adjacency contract.Object) ([][]int64, error) {
	graph := map[int64][]int64{}
	for key, raw := range adjacency {
		number, err := strconv.ParseInt(key, 10, 64)
		if err != nil || number <= 0 {
			return nil, fmt.Errorf("cycle graph contains invalid node %q", key)
		}
		neighbors, err := positiveNumberSet(raw, "cycle adjacency")
		if err != nil {
			return nil, err
		}
		graph[number] = neighbors
	}
	return findCycles(graph), nil
}

func findCycles(graph map[int64][]int64) [][]int64 {
	keys := sortedIssueNumbers(graph)
	cycles := map[string][]int64{}
	if len(keys) == 0 {
		return [][]int64{}
	}
	lowerBound := keys[0]
	for {
		components := stronglyConnectedComponents(graph, keys, lowerBound)
		var selected []int64
		for _, component := range components {
			if len(component) > 1 || hasSelfLoop(graph, component[0]) {
				if selected == nil || component[0] < selected[0] {
					selected = component
				}
			}
		}
		if selected == nil {
			break
		}
		start := selected[0]
		inside := intSet(selected)
		blocked := map[int64]bool{}
		blockedBy := map[int64]map[int64]bool{}
		stack := []int64{}
		var unblock func(int64)
		unblock = func(node int64) {
			blocked[node] = false
			for member := range blockedBy[node] {
				delete(blockedBy[node], member)
				if blocked[member] {
					unblock(member)
				}
			}
		}
		var circuit func(int64) bool
		circuit = func(node int64) bool {
			found := false
			stack = append(stack, node)
			blocked[node] = true
			for _, next := range graph[node] {
				if !inside[next] {
					continue
				}
				if next == start {
					cycle := normalizeCycle(append([]int64{}, stack...))
					cycles[numberKey(cycle)] = cycle
					found = true
				} else if !blocked[next] && circuit(next) {
					found = true
				}
			}
			if found {
				unblock(node)
			} else {
				for _, next := range graph[node] {
					if inside[next] {
						if blockedBy[next] == nil {
							blockedBy[next] = map[int64]bool{}
						}
						blockedBy[next][node] = true
					}
				}
			}
			stack = stack[:len(stack)-1]
			return found
		}
		circuit(start)
		index := sort.Search(len(keys), func(i int) bool { return keys[i] > start })
		if index >= len(keys) {
			break
		}
		lowerBound = keys[index]
	}
	keys2 := make([]string, 0, len(cycles))
	for key := range cycles {
		keys2 = append(keys2, key)
	}
	sort.Strings(keys2)
	result := make([][]int64, 0, len(keys2))
	for _, key := range keys2 {
		result = append(result, cycles[key])
	}
	sort.Slice(result, func(i, j int) bool { return compareNumbers(result[i], result[j]) < 0 })
	return result
}

func hasSelfLoop(graph map[int64][]int64, node int64) bool {
	for _, next := range graph[node] {
		if next == node {
			return true
		}
	}
	return false
}

func stronglyConnectedComponents(graph map[int64][]int64, vertices []int64, lowerBound int64) [][]int64 {
	index, low := map[int64]int{}, map[int64]int{}
	onStack := map[int64]bool{}
	stack := []int64{}
	nextIndex := 0
	components := [][]int64{}
	var visit func(int64)
	visit = func(node int64) {
		nextIndex++
		index[node], low[node] = nextIndex, nextIndex
		stack = append(stack, node)
		onStack[node] = true
		for _, next := range graph[node] {
			if next < lowerBound {
				continue
			}
			if _, exists := graph[next]; !exists {
				continue
			}
			if index[next] == 0 {
				visit(next)
				if low[next] < low[node] {
					low[node] = low[next]
				}
			} else if onStack[next] && index[next] < low[node] {
				low[node] = index[next]
			}
		}
		if low[node] == index[node] {
			component := []int64{}
			for {
				last := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[last] = false
				component = append(component, last)
				if last == node {
					break
				}
			}
			sort.Slice(component, func(i, j int) bool { return component[i] < component[j] })
			components = append(components, component)
		}
	}
	for _, node := range vertices {
		if node >= lowerBound && index[node] == 0 {
			visit(node)
		}
	}
	return components
}

func normalizeCycle(values []int64) []int64 {
	if len(values) == 0 {
		return nil
	}
	best := append([]int64{}, values...)
	for i := 1; i < len(values); i++ {
		rotated := append(append([]int64{}, values[i:]...), values[:i]...)
		if compareNumbers(rotated, best) < 0 {
			best = rotated
		}
	}
	return best
}
func numberKey(values []int64) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.FormatInt(value, 10)
	}
	return strings.Join(parts, ",")
}
func compareNumbers(left, right []int64) int {
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] < right[i] {
			return -1
		}
		if left[i] > right[i] {
			return 1
		}
	}
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return 0
}

func introducedCycles(before, after map[int64][]int64) [][]int64 {
	current := map[string]bool{}
	for _, cycle := range findCycles(before) {
		current[numberKey(cycle)] = true
	}
	result := [][]int64{}
	for _, cycle := range findCycles(after) {
		if !current[numberKey(cycle)] {
			result = append(result, cycle)
		}
	}
	return result
}

// AuditRelationships returns local dependency and hierarchy findings. Unlike
// RelationshipTopology, audit tolerates inconsistent parent edges so it can
// report multiple-parent defects rather than hiding them behind validation.
func AuditRelationships(graph contract.Object) (contract.Object, error) {
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	issueByNumber := map[int64]contract.Object{}
	blockers := map[int64][]int64{}
	parents := map[int64]*int64{}
	parentSets := map[int64]map[int64]bool{}
	for i, issue := range issues {
		number, err := issueNumber(issue, fmt.Sprintf("issues[%d]", i))
		if err != nil {
			return nil, err
		}
		if issueByNumber[number] != nil {
			return nil, fmt.Errorf("duplicate issue #%d", number)
		}
		issueByNumber[number] = issue
		blockerList, err := positiveNumberSet(defaultArray(issue["blocked_by_numbers"]), "blocked_by_numbers")
		if err != nil {
			return nil, err
		}
		blockers[number] = blockerList
		var parent *int64
		if issue["parent_number"] != nil {
			p, err := contract.PositiveInteger(issue["parent_number"])
			if err != nil {
				return nil, fmt.Errorf("issue #%d parent_number is invalid", number)
			}
			parent = &p
		}
		parents[number] = parent
		childList, err := positiveNumberSet(defaultArray(issue["child_numbers"]), "child_numbers")
		if err != nil {
			return nil, err
		}
		for _, child := range childList {
			if parentSets[child] == nil {
				parentSets[child] = map[int64]bool{}
			}
			parentSets[child][number] = true
		}
	}
	findings := []contract.Object{}
	for _, child := range sortedIssueNumbers(parentSets) {
		if len(parentSets[child]) <= 1 {
			continue
		}
		p := sortedIssueNumbers(parentSets[child])
		parentStrings := make([]int64, len(p))
		copy(parentStrings, p)
		finding := contract.Object{"code": "multiple-parents", "severity": "warning", "message": fmt.Sprintf("Issue has multiple parents: %s.", issueRefs(p)), "fixable": false, "number": child, "details": contract.Object{"parent_issue_numbers": parentStrings}}
		addIssueDetails(finding, issueByNumber[child])
		findings = append(findings, finding)
	}
	for _, cycle := range findCycles(adjacencyFromBlockers(blockers)) {
		findings = append(findings, contract.Object{"code": "dependency-cycle", "severity": "warning", "message": "Dependency cycle detected: " + cyclePath(cycle), "fixable": false, "details": contract.Object{"cycle_issue_numbers": cycle}})
	}
	for _, cycle := range findCycles(adjacencyFromParents(parents)) {
		findings = append(findings, contract.Object{"code": "hierarchy-cycle", "severity": "warning", "message": "Hierarchy cycle detected: " + cyclePath(cycle), "fixable": false, "details": contract.Object{"cycle_issue_numbers": cycle}})
	}
	blockerCount, parentCount := 0, 0
	for _, values := range blockers {
		blockerCount += len(values)
	}
	for _, parent := range parents {
		if parent != nil {
			parentCount++
		}
	}
	summary := contract.Object{"finding_count": len(findings), "error_count": 0, "warning_count": len(findings), "info_count": 0, "fixable_count": 0, "issue_count": len(issues), "blocker_edge_count": blockerCount, "parent_link_count": parentCount}
	repo, _ := graph["repo"]
	return contract.Object{"tool": "relationship_audit", "repo": repo, "summary": summary, "findings": findings}, nil
}

func defaultArray(value any) any {
	if value == nil {
		return []any{}
	}
	return value
}
func issueRefs(numbers []int64) string {
	parts := make([]string, len(numbers))
	for i, n := range numbers {
		parts[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(parts, ", ")
}
func cyclePath(cycle []int64) string {
	values := append(append([]int64{}, cycle...), cycle[0])
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = fmt.Sprintf("#%d", value)
	}
	return strings.Join(parts, " -> ")
}
func addIssueDetails(finding, issue contract.Object) {
	if issue == nil {
		return
	}
	if v, ok := issue["title"].(string); ok {
		finding["title"] = v
	}
	if v, ok := issue["url"].(string); ok {
		finding["url"] = v
	}
}

// ValidateRelationshipPayload strictly normalizes a schema-v1 relationship
// payload against a complete issue graph. hierarchyPolicy is optional and,
// when provided, maps parent prefixes to allowed child-prefix arrays.
func ValidateRelationshipPayload(payload, issueGraph contract.Object, hierarchyPolicy contract.Object) (contract.Object, error) {
	for key := range payload {
		if key != "schema_version" && key != "issues" && key != "notes" {
			return nil, fmt.Errorf("relationship payload has unsupported property %q", key)
		}
	}
	version, err := contract.Integer(payload["schema_version"])
	if err != nil || version != 1 {
		return nil, fmt.Errorf("relationship payload schema_version must be the integer 1")
	}
	rawIssues, err := contract.Objects(payload, "issues")
	if err != nil || len(rawIssues) == 0 {
		return nil, fmt.Errorf("relationship payload must contain a non-empty issues array")
	}
	if err := rejectForeignReferences(payload); err != nil {
		return nil, err
	}
	graph, err := relationshipStateForPreview(issueGraph)
	if err != nil {
		return nil, err
	}
	open := map[int64]bool{}
	for number, issue := range graph.issues {
		state, _ := issue["state"].(string)
		if state == "" || strings.EqualFold(state, "OPEN") {
			open[number] = true
		}
	}
	seen := map[int64]bool{}
	normalized := make([]any, 0, len(rawIssues))
	for i, raw := range rawIssues {
		for key := range raw {
			if key != "issue_number" && key != "desired_blocked_by_issue_numbers" && key != "desired_parent_issue_number" {
				return nil, fmt.Errorf("issues[%d] has unsupported property %q", i, key)
			}
		}
		n, err := contract.PositiveInteger(raw["issue_number"])
		if err != nil {
			return nil, fmt.Errorf("issues[%d].issue_number must be a positive integer", i)
		}
		if seen[n] {
			return nil, fmt.Errorf("relationship input contains duplicate issue #%d", n)
		}
		seen[n] = true
		if !open[n] {
			return nil, fmt.Errorf("relationship issue #%d must be open in the current graph", n)
		}
		touchesBlockers := false
		blockers := []int64{}
		if value, exists := raw["desired_blocked_by_issue_numbers"]; exists {
			touchesBlockers = true
			blockers, err = positiveNumberSet(defaultArray(value), fmt.Sprintf("issue #%d desired blockers", n))
			if err != nil {
				return nil, err
			}
			for _, b := range blockers {
				if b == n {
					return nil, fmt.Errorf("issue #%d cannot block itself", n)
				}
				if !open[b] {
					return nil, fmt.Errorf("issue #%d references closed or unknown blocker #%d", n, b)
				}
			}
		}
		touchesParent := false
		var parent any
		if value, exists := raw["desired_parent_issue_number"]; exists {
			touchesParent = true
			parent = value
			if value != nil {
				p, err := contract.PositiveInteger(value)
				if err != nil {
					return nil, fmt.Errorf("issue #%d desired parent must be a positive integer or null", n)
				}
				if p == n {
					return nil, fmt.Errorf("issue #%d cannot parent itself", n)
				}
				if !open[p] {
					return nil, fmt.Errorf("issue #%d references closed or unknown parent #%d", n, p)
				}
				parent = p
			}
		}
		if touchesParent && parent != nil && hierarchyPolicy != nil {
			if err := validateHierarchyPair(graph.issues[n], graph.issues[parent.(int64)], hierarchyPolicy); err != nil {
				return nil, err
			}
		}
		normalized = append(normalized, contract.Object{"issue_number": n, "touches_blockers": touchesBlockers, "desired_blocked_by_issue_numbers": blockers, "touches_parent": touchesParent, "desired_parent_issue_number": parent})
	}
	result := contract.Object{"schema_version": int64(1), "issues": normalized}
	if rawNote, exists := payload["notes"]; exists {
		note, ok := rawNote.(string)
		if !ok {
			return nil, fmt.Errorf("relationship notes must be a string")
		}
		if strings.TrimSpace(note) != "" {
			result["notes"] = strings.TrimSpace(note)
		}
	}
	before := graph
	after := proposedRelationshipState(graph, result)
	if cycles := introducedCycles(adjacencyFromBlockers(before.blockers), adjacencyFromBlockers(after.blockers)); len(cycles) > 0 {
		return nil, fmt.Errorf("relationship payload introduces dependency cycle: %s", cyclePath(cycles[0]))
	}
	if cycles := introducedCycles(adjacencyFromParents(before.parents), adjacencyFromParents(after.parents)); len(cycles) > 0 {
		return nil, fmt.Errorf("relationship payload introduces hierarchy cycle: %s", cyclePath(cycles[0]))
	}
	return result, nil
}

func rejectForeignReferences(payload contract.Object) error {
	for key, value := range payload {
		if containsScopeToken(key) {
			return fmt.Errorf("cross-repository relationships are unsupported; use local issue numbers only")
		}
		if key == "issues" {
			rows, ok := value.([]any)
			if !ok {
				continue
			}
			for _, row := range rows {
				item, ok := row.(map[string]any)
				if !ok {
					continue
				}
				for k, v := range item {
					if containsScopeToken(k) {
						return fmt.Errorf("cross-repository relationships are unsupported; use local issue numbers only")
					}
					if k == "issue_number" || k == "desired_parent_issue_number" || k == "desired_blocked_by_issue_numbers" {
						if repositoryQualified(v) {
							return fmt.Errorf("cross-repository relationships are unsupported; use local issue numbers only")
						}
					}
				}
			}
		}
	}
	return nil
}
func containsScopeToken(key string) bool {
	lower := strings.ToLower(key)
	return strings.Contains(lower, "repo") || strings.Contains(lower, "owner") || strings.Contains(lower, "host")
}
func repositoryQualified(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, "://") || (strings.Contains(v, "/") && strings.Contains(v, "#"))
	case []any:
		for _, item := range v {
			if repositoryQualified(item) {
				return true
			}
		}
	case map[string]any:
		for k, item := range v {
			if containsScopeToken(k) || repositoryQualified(item) {
				return true
			}
		}
	}
	return false
}
func prefixOf(issue contract.Object) string {
	if v, ok := issue["prefix"].(string); ok && v != "" {
		return v
	}
	title, _ := issue["title"].(string)
	if idx := strings.Index(title, ":"); idx >= 0 {
		return strings.TrimSpace(title[:idx])
	}
	return ""
}
func validateHierarchyPair(child, parent contract.Object, policy contract.Object) error {
	parentPrefix, childPrefix := prefixOf(parent), prefixOf(child)
	allowedRaw, exists := policy[parentPrefix]
	if !exists {
		return fmt.Errorf("hierarchy policy does not allow parent prefix %q", parentPrefix)
	}
	allowed, err := contract.StringSet(allowedRaw)
	if err != nil {
		return fmt.Errorf("hierarchy policy for %q must be a unique string array", parentPrefix)
	}
	for _, candidate := range allowed {
		if candidate == childPrefix {
			return nil
		}
	}
	return fmt.Errorf("hierarchy policy rejects parent prefix %q and child prefix %q", parentPrefix, childPrefix)
}

func proposedRelationshipState(graph relationshipState, payload contract.Object) relationshipState {
	next := relationshipState{blockers: map[int64][]int64{}, parents: map[int64]*int64{}, issues: graph.issues}
	for n, items := range graph.blockers {
		next.blockers[n] = append([]int64{}, items...)
	}
	for n, p := range graph.parents {
		if p == nil {
			next.parents[n] = nil
		} else {
			v := *p
			next.parents[n] = &v
		}
	}
	rows, _ := contract.Objects(payload, "issues")
	for _, row := range rows {
		n, _ := contract.PositiveInteger(row["issue_number"])
		if row["touches_blockers"] == true {
			values, _ := positiveNumberSet(row["desired_blocked_by_issue_numbers"], "desired blockers")
			next.blockers[n] = values
		}
		if row["touches_parent"] == true {
			if row["desired_parent_issue_number"] == nil {
				next.parents[n] = nil
			} else {
				p, _ := contract.PositiveInteger(row["desired_parent_issue_number"])
				next.parents[n] = &p
			}
		}
	}
	return next
}

// BuildRelationshipDelta compares normalized desired state with current state.
func BuildRelationshipDelta(normalized, issueGraph contract.Object) (contract.Object, error) {
	authored, err := authoredRelationshipPayload(normalized)
	if err != nil {
		return nil, err
	}
	normalized, err = ValidateRelationshipPayload(authored, issueGraph, nil)
	if err != nil {
		return nil, err
	}
	graph, err := relationshipStateForPreview(issueGraph)
	if err != nil {
		return nil, err
	}
	rows, err := contract.Objects(normalized, "issues")
	if err != nil {
		return nil, err
	}
	changes := []any{}
	unchanged := []int64{}
	for _, row := range rows {
		n, err := contract.PositiveInteger(row["issue_number"])
		if err != nil {
			return nil, err
		}
		changed := false
		if row["touches_blockers"] == true {
			desired, err := positiveNumberSet(row["desired_blocked_by_issue_numbers"], "desired blockers")
			if err != nil {
				return nil, err
			}
			current := numberSet(graph.blockers[n])
			for _, b := range graph.blockers[n] {
				if !numberSet(desired)[b] {
					changes = append(changes, contract.Object{"type": "remove_blocked_by", "issue_number": n, "blocked_by_issue_number": b})
					changed = true
				}
			}
			for _, b := range desired {
				if !current[b] {
					changes = append(changes, contract.Object{"type": "add_blocked_by", "issue_number": n, "blocked_by_issue_number": b})
					changed = true
				}
			}
		}
		if row["touches_parent"] == true {
			current := graph.parents[n]
			desiredValue := row["desired_parent_issue_number"]
			if current == nil && desiredValue != nil {
				p, _ := contract.PositiveInteger(desiredValue)
				changes = append(changes, contract.Object{"type": "set_parent", "issue_number": n, "target_parent_issue_number": p})
				changed = true
			} else if current != nil && desiredValue == nil {
				changes = append(changes, contract.Object{"type": "clear_parent", "issue_number": n, "current_parent_issue_number": *current})
				changed = true
			} else if current != nil && desiredValue != nil {
				p, _ := contract.PositiveInteger(desiredValue)
				if *current != p {
					changes = append(changes, contract.Object{"type": "reparent", "issue_number": n, "current_parent_issue_number": *current, "target_parent_issue_number": p})
					changed = true
				}
			}
		}
		if !changed {
			unchanged = append(unchanged, n)
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		a := changes[i].(contract.Object)
		b := changes[j].(contract.Object)
		ta, _ := a["type"].(string)
		tb, _ := b["type"].(string)
		if relationshipChangeOrder[ta] != relationshipChangeOrder[tb] {
			return relationshipChangeOrder[ta] < relationshipChangeOrder[tb]
		}
		na, _ := contract.Integer(a["issue_number"])
		nb, _ := contract.Integer(b["issue_number"])
		if na != nb {
			return na < nb
		}
		ra := firstNonzero(a, "blocked_by_issue_number", "current_parent_issue_number", "target_parent_issue_number")
		rb := firstNonzero(b, "blocked_by_issue_number", "current_parent_issue_number", "target_parent_issue_number")
		if ra != rb {
			return ra < rb
		}
		return false
	})
	before := topologyObject(graph)
	afterState := proposedRelationshipState(graph, normalized)
	after := topologyObject(afterState)
	return contract.Object{"changes": changes, "unchanged_issue_numbers": unchanged, "summary": relationshipSummary(len(rows), changes, unchanged), "relationships_before": before, "relationships_after": after}, nil
}

func authoredRelationshipPayload(normalized contract.Object) (contract.Object, error) {
	version, err := contract.Integer(normalized["schema_version"])
	if err != nil || version != 1 {
		return nil, fmt.Errorf("normalized relationship schema_version must be the integer 1")
	}
	rows, err := contract.Objects(normalized, "issues")
	if err != nil {
		return nil, err
	}
	authored := make([]any, 0, len(rows))
	for i, row := range rows {
		number, err := contract.PositiveInteger(row["issue_number"])
		if err != nil {
			return nil, fmt.Errorf("normalized issues[%d].issue_number must be positive", i)
		}
		touchesBlockers, ok := row["touches_blockers"].(bool)
		if !ok {
			return nil, fmt.Errorf("normalized issues[%d].touches_blockers must be boolean", i)
		}
		touchesParent, ok := row["touches_parent"].(bool)
		if !ok {
			return nil, fmt.Errorf("normalized issues[%d].touches_parent must be boolean", i)
		}
		item := contract.Object{"issue_number": number}
		if touchesBlockers {
			item["desired_blocked_by_issue_numbers"] = row["desired_blocked_by_issue_numbers"]
		}
		if touchesParent {
			item["desired_parent_issue_number"] = row["desired_parent_issue_number"]
		}
		authored = append(authored, item)
	}
	result := contract.Object{"schema_version": int64(1), "issues": authored}
	if note, exists := normalized["notes"]; exists {
		value, ok := note.(string)
		if !ok {
			return nil, fmt.Errorf("normalized relationship notes must be a string")
		}
		result["notes"] = value
	}
	return result, nil
}
func numberSet(numbers []int64) map[int64]bool {
	result := map[int64]bool{}
	for _, n := range numbers {
		result[n] = true
	}
	return result
}
func firstNonzero(o contract.Object, keys ...string) int64 {
	for _, k := range keys {
		if n, err := contract.Integer(o[k]); err == nil && n != 0 {
			return n
		}
	}
	return 0
}
func relationshipSummary(touched int, changes []any, unchanged []int64) contract.Object {
	s := contract.Object{"touched_issue_count": touched, "change_count": len(changes), "unchanged_issue_count": len(unchanged), "add_blocked_by_count": 0, "remove_blocked_by_count": 0, "set_parent_count": 0, "clear_parent_count": 0, "reparent_count": 0}
	for _, item := range changes {
		typ := item.(contract.Object)["type"].(string)
		key := typ + "_count"
		s[key] = s[key].(int) + 1
	}
	return s
}
