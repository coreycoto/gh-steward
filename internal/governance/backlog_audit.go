package governance

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var backlogPriorityBands = []string{"Now", "Next", "Later"}

type backlogAuditPolicy struct {
	repository       contract.Repository
	project          contract.Object
	statusField      string
	validStatuses    map[string]bool
	doneStatuses     map[string]bool
	priorityField    string
	priorityBands    map[string]string
	excludedPrefixes map[string]bool
	typedPrefixes    map[string]string
	orderField       string
}

type backlogAuditItem struct {
	issue    contract.Object
	number   int64
	title    string
	prefix   string
	fields   contract.Object
	status   string
	statusOK bool
	priority string
	band     string
	bandOK   bool
	order    float64
	orderOK  bool
	archived bool
	blockers []int64
	children []int64
}

// BacklogAudit reports queue and project-discipline findings from a normalized
// issue graph. queueSnapshot is optional corroborating evidence; audit values
// are always read from the issue graph so invalid consumer fields remain
// reportable instead of preventing the audit.
func BacklogAudit(policy, issueGraph, queueSnapshot contract.Object) (contract.Object, error) {
	parsed, err := parseBacklogAuditPolicy(policy)
	if err != nil {
		return nil, err
	}
	repo, err := contract.ObjectAt(issueGraph, "repo")
	if err != nil {
		return nil, err
	}
	graphRepo, err := contract.ParseRepository(repo)
	if err != nil {
		return nil, err
	}
	if graphRepo != parsed.repository {
		return nil, errors.New("backlog audit repository does not match the explicit policy scope")
	}
	project, err := contract.ObjectAt(issueGraph, "project")
	if err != nil {
		return nil, errors.New("backlog audit requires an issue graph joined to the selected Project")
	}
	if !sameBacklogProject(project, parsed.project) {
		return nil, errors.New("backlog audit Project does not match the explicit policy scope")
	}
	if queueSnapshot != nil {
		if err := validateAuditQueueSnapshot(queueSnapshot, repo, project); err != nil {
			return nil, err
		}
	}

	issues, err := contract.Objects(issueGraph, "issues")
	if err != nil {
		return nil, err
	}
	items, byNumber, openIssueCount, err := buildBacklogAuditItems(issues, parsed)
	if err != nil {
		return nil, err
	}
	activeProject := make([]*backlogAuditItem, 0, len(items))
	queueItems := make([]*backlogAuditItem, 0, len(items))
	excludedCounts := contract.Object{}
	for _, item := range items {
		if item.issue["state"] != "OPEN" || !item.issue["in_project"].(bool) || item.archived {
			continue
		}
		activeProject = append(activeProject, item)
		if parsed.excludedPrefixes[item.prefix] {
			count, _ := contract.Integer(excludedCounts[item.prefix])
			excludedCounts[item.prefix] = count + 1
			continue
		}
		queueItems = append(queueItems, item)
	}
	sort.SliceStable(activeProject, func(i, j int) bool { return backlogAuditLess(activeProject[i], activeProject[j]) })
	sort.SliceStable(queueItems, func(i, j int) bool { return backlogAuditLess(queueItems[i], queueItems[j]) })
	if queueSnapshot != nil {
		if err := validateAuditQueueMembership(queueSnapshot, queueItems, activeProject, parsed); err != nil {
			return nil, err
		}
	}

	findings := []any{}
	queueOrdersByBand := map[string]map[float64][]*backlogAuditItem{}
	ordersByBand := map[string][]float64{}
	for _, item := range activeProject {
		if !item.statusOK {
			addFinding(&findings, "invalid-status", "error", fmt.Sprintf("Project item uses invalid %s '%s'.", parsed.statusField, printable(item.fields[parsed.statusField])), false, item.issue, nil)
		}
		if !item.bandOK {
			addFinding(&findings, "invalid-priority", "error", fmt.Sprintf("Project item uses invalid %s '%s'.", parsed.priorityField, printable(item.fields[parsed.priorityField])), false, item.issue, nil)
		}
		if item.statusOK && parsed.doneStatuses[item.status] {
			if item.issue["state"] == "OPEN" {
				addFinding(&findings, "done-open-issue", "warning", "Project item is marked Done while the underlying issue is still open.", false, item.issue, nil)
			}
			continue
		}
		if parsed.excludedPrefixes[item.prefix] {
			continue
		}
		if !item.orderOK {
			if raw, exists := item.fields[parsed.orderField]; !exists || raw == nil {
				addFinding(&findings, "missing-queue-order", "warning", fmt.Sprintf("Project item is missing a numeric %s.", parsed.orderField), false, item.issue, nil)
			} else {
				addFinding(&findings, "invalid-queue-order", "error", fmt.Sprintf("Project item has a non-numeric %s value.", parsed.orderField), false, item.issue, contract.Object{"value": raw})
			}
			continue
		}
		if fractional(item.order) {
			addFinding(&findings, "legacy-fractional-queue-order", "warning", fmt.Sprintf("Project item still uses a fractional %s.", parsed.orderField), false, item.issue, nil)
		}
		if !item.bandOK {
			continue
		}
		if queueOrdersByBand[item.band] == nil {
			queueOrdersByBand[item.band] = map[float64][]*backlogAuditItem{}
		}
		queueOrdersByBand[item.band][item.order] = append(queueOrdersByBand[item.band][item.order], item)
		ordersByBand[item.band] = append(ordersByBand[item.band], item.order)
	}
	for _, band := range backlogPriorityBands {
		orders := queueOrdersByBand[band]
		orderValues := make([]float64, 0, len(orders))
		for order := range orders {
			orderValues = append(orderValues, order)
		}
		sort.Float64s(orderValues)
		for _, order := range orderValues {
			collisions := orders[order]
			if len(collisions) < 2 {
				continue
			}
			sort.Slice(collisions, func(i, j int) bool { return collisions[i].number < collisions[j].number })
			for _, item := range collisions {
				priority := item.priority
				addFinding(&findings, "duplicate-queue-order", "warning", fmt.Sprintf("%s %s is duplicated inside %s '%s'.", parsed.orderField, strconv.FormatFloat(order, 'g', -1, 64), parsed.priorityField, priority), false, item.issue, nil)
			}
		}
	}

	for _, item := range queueItems {
		if item.issue["state"] != "OPEN" {
			continue
		}
		if item.bandOK && item.band == "Now" && !(item.statusOK && parsed.doneStatuses[item.status]) {
			blockers := openBlockers(item, byNumber)
			if len(blockers) > 0 {
				blockerStrings := make([]any, len(blockers))
				for i, blocker := range blockers {
					blockerStrings[i] = strconv.FormatInt(blocker, 10)
				}
				addFinding(&findings, "blocked-now-item", "warning", fmt.Sprintf("`Now` item is blocked by open dependencies: %s.", strings.Join(int64Strings(blockers), ", ")), false, item.issue, contract.Object{"blocked_by": blockerStrings})
			}
		}
		if item.statusOK && parsed.doneStatuses[item.status] {
			continue
		}
		actionableChildren := actionableBacklogChildren(item, queueItems, byNumber, parsed)
		orderedChildren := []int64{}
		for _, childNumber := range actionableChildren {
			child := byNumber[childNumber]
			if child != nil && backlogAuditLess(item, child) {
				orderedChildren = append(orderedChildren, childNumber)
			}
		}
		if len(orderedChildren) > 0 {
			details := make([]any, len(orderedChildren))
			for i, number := range orderedChildren {
				details[i] = number
			}
			addFinding(&findings, "parent-above-actionable-child", "warning", "Parent issue is still ordered ahead of actionable child issues: "+strings.Join(int64Strings(orderedChildren), ", ")+".", false, item.issue, contract.Object{"actionable_children": details})
		}
	}

	for _, issue := range issues {
		if issue["state"] != "OPEN" || issue["in_project"] == true {
			continue
		}
		prefix := titlePrefix(auditString(issue["title"]))
		if _, typed := parsed.typedPrefixes[prefix]; typed {
			addFinding(&findings, "missing-project-membership", "warning", "Open typed issue is missing from the configured backlog project.", false, issue, nil)
		}
	}

	minimumGaps := contract.Object{}
	for _, band := range backlogPriorityBands {
		gap := any(nil)
		values := ordersByBand[band]
		if len(values) >= 2 {
			sort.Float64s(values)
			minGap := math.Inf(1)
			for i := 1; i < len(values); i++ {
				if difference := values[i] - values[i-1]; difference < minGap {
					minGap = difference
				}
			}
			gap = minGap
		}
		for option, mappedBand := range parsed.priorityBands {
			if mappedBand == band {
				minimumGaps[option] = gap
				break
			}
		}
	}
	summary := findingSummary(findings)
	summary["project_item_count"] = len(activeProject)
	summary["queue_item_count"] = len(queueItems)
	summary["excluded_item_count"] = len(activeProject) - len(queueItems)
	summary["excluded_item_count_by_prefix"] = excludedCounts
	summary["open_issue_count"] = openIssueCount
	summary["fractional_queue_order_count"] = fractionalCountFor(queueItems)
	summary["minimum_gap_by_priority"] = minimumGaps
	payload := contract.Object{"tool": "backlog_audit", "repo": graphRepo.Object(), "project": project, "summary": summary, "findings": findings}
	if generatedAt, ok := issueGraph["generated_at"].(string); ok {
		payload["generated_at"] = generatedAt
	}
	return payload, nil
}

func parseBacklogAuditPolicy(policy contract.Object) (backlogAuditPolicy, error) {
	parsed := backlogAuditPolicy{}
	version, err := contract.Integer(policy["schema_version"])
	if err != nil || version != 1 {
		return parsed, errors.New("backlog audit policy schema_version must be the integer 1")
	}
	for key := range policy {
		if !map[string]bool{"schema_version": true, "scope": true, "taxonomy": true, "excluded_prefixes": true, "status_field": true, "valid_statuses": true, "done_statuses": true, "priority_field": true, "priorities": true, "order_field": true}[key] {
			return parsed, fmt.Errorf("backlog audit policy has unsupported property %q", key)
		}
	}
	scope, err := contract.ObjectAt(policy, "scope")
	if err != nil {
		return parsed, err
	}
	repo, err := contract.ObjectAt(scope, "repo")
	if err != nil {
		return parsed, err
	}
	parsed.repository, err = contract.ParseRepository(repo)
	if err != nil {
		return parsed, err
	}
	parsed.project, err = contract.ObjectAt(scope, "project")
	if err != nil {
		return parsed, err
	}
	if _, err := requiredString(parsed.project, "id"); err != nil {
		return parsed, err
	}
	if _, err := requiredString(parsed.project, "title"); err != nil {
		return parsed, err
	}
	if _, err := positiveInteger(parsed.project, "number"); err != nil {
		return parsed, err
	}
	taxonomy, err := contract.ObjectAt(policy, "taxonomy")
	if err != nil {
		return parsed, err
	}
	for key := range taxonomy {
		if key != "issue_type_labels" {
			return parsed, fmt.Errorf("backlog audit taxonomy has unsupported property %q", key)
		}
	}
	labels, err := contract.ObjectAt(taxonomy, "issue_type_labels")
	if err != nil {
		return parsed, err
	}
	parsed.typedPrefixes = map[string]string{}
	for prefix, rawLabel := range labels {
		label, ok := rawLabel.(string)
		if strings.TrimSpace(prefix) == "" || strings.TrimSpace(prefix) != prefix || !ok || strings.TrimSpace(label) == "" {
			return parsed, errors.New("taxonomy issue_type_labels must map exact nonempty prefixes to labels")
		}
		parsed.typedPrefixes[prefix] = label
	}
	parsed.excludedPrefixes = map[string]bool{}
	if raw, exists := policy["excluded_prefixes"]; !exists {
		return parsed, errors.New("backlog audit policy requires explicit excluded_prefixes")
	} else {
		prefixes, err := contract.Strings(raw)
		if err != nil {
			return parsed, fmt.Errorf("excluded_prefixes: %w", err)
		}
		for _, prefix := range prefixes {
			if strings.TrimSpace(prefix) == "" || strings.TrimSpace(prefix) != prefix || parsed.excludedPrefixes[prefix] {
				return parsed, errors.New("excluded_prefixes must contain unique exact nonempty prefixes")
			}
			parsed.excludedPrefixes[prefix] = true
		}
	}
	if parsed.statusField, err = requiredString(policy, "status_field"); err != nil {
		return parsed, err
	}
	validStatuses, err := uniquePolicyStrings(policy, "valid_statuses", false)
	if err != nil {
		return parsed, err
	}
	doneStatuses, err := uniquePolicyStrings(policy, "done_statuses", false)
	if err != nil {
		return parsed, err
	}
	parsed.validStatuses, parsed.doneStatuses = namesSet(validStatuses), namesSet(doneStatuses)
	for status := range parsed.doneStatuses {
		if !parsed.validStatuses[status] {
			return parsed, errors.New("done_statuses must be a subset of valid_statuses")
		}
	}
	if parsed.priorityField, err = requiredString(policy, "priority_field"); err != nil {
		return parsed, err
	}
	if parsed.orderField, err = requiredString(policy, "order_field"); err != nil {
		return parsed, err
	}
	if parsed.statusField == parsed.priorityField || parsed.statusField == parsed.orderField || parsed.priorityField == parsed.orderField {
		return parsed, errors.New("status_field, priority_field, and order_field must identify distinct Project fields")
	}
	priorityOptions, err := contract.ObjectAt(policy, "priorities")
	if err != nil {
		return parsed, err
	}
	parsed.priorityBands = map[string]string{}
	usedBands := map[string]bool{}
	for option, rawBand := range priorityOptions {
		band, ok := rawBand.(string)
		if strings.TrimSpace(option) == "" || strings.TrimSpace(option) != option || !ok || !contains(backlogPriorityBands, band) || usedBands[band] {
			return parsed, errors.New("priorities must map distinct nonempty options to Now, Next, and Later exactly once")
		}
		parsed.priorityBands[option] = band
		usedBands[band] = true
	}
	if len(parsed.priorityBands) != len(backlogPriorityBands) {
		return parsed, errors.New("priorities must map distinct nonempty options to Now, Next, and Later exactly once")
	}
	return parsed, nil
}

func uniquePolicyStrings(policy contract.Object, key string, allowEmpty bool) ([]string, error) {
	values, err := contract.Strings(policy[key])
	if err != nil {
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
	seen := map[string]bool{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value || seen[value] {
			return nil, fmt.Errorf("%s entries must be unique exact nonempty strings", key)
		}
		seen[value] = true
	}
	if !allowEmpty && len(values) == 0 {
		return nil, fmt.Errorf("%s must not be empty", key)
	}
	return values, nil
}

func sameBacklogProject(left, right contract.Object) bool {
	leftID, lok := left["id"].(string)
	rightID, rok := right["id"].(string)
	leftTitle, ltok := left["title"].(string)
	rightTitle, rtok := right["title"].(string)
	leftNumber, lne := contract.PositiveInteger(left["number"])
	rightNumber, rne := contract.PositiveInteger(right["number"])
	return lok && rok && leftID != "" && leftID == rightID && ltok && rtok && leftTitle != "" && leftTitle == rightTitle && lne == nil && rne == nil && leftNumber == rightNumber
}

func validateAuditQueueSnapshot(queueSnapshot, graphRepo, graphProject contract.Object) error {
	repo, err := contract.ObjectAt(queueSnapshot, "repo")
	if err != nil {
		return err
	}
	a, err := contract.ParseRepository(graphRepo)
	if err != nil {
		return err
	}
	b, err := contract.ParseRepository(repo)
	if err != nil || a != b {
		return errors.New("queue snapshot repository does not match the issue graph")
	}
	project, err := contract.ObjectAt(queueSnapshot, "project")
	if err != nil || !sameBacklogProject(project, graphProject) {
		return errors.New("queue snapshot Project does not match the issue graph")
	}
	return nil
}

func validateAuditQueueMembership(queueSnapshot contract.Object, queueItems, activeProject []*backlogAuditItem, policy backlogAuditPolicy) error {
	queueRows, err := contract.Objects(queueSnapshot, "items")
	if err != nil {
		return err
	}
	excludedRows, err := contract.Objects(queueSnapshot, "excluded_items")
	if err != nil {
		return err
	}
	wantQueue, wantExcluded := map[int64]bool{}, map[int64]bool{}
	for _, item := range queueItems {
		wantQueue[item.number] = true
	}
	for _, item := range activeProject {
		if policy.excludedPrefixes[item.prefix] {
			wantExcluded[item.number] = true
		}
	}
	if err := exactAuditNumberSet(queueRows, wantQueue, "queue snapshot items"); err != nil {
		return err
	}
	if err := exactAuditNumberSet(excludedRows, wantExcluded, "queue snapshot excluded_items"); err != nil {
		return err
	}
	for key, count := range map[string]int{"item_count": len(queueRows), "excluded_item_count": len(excludedRows)} {
		if raw, exists := queueSnapshot[key]; exists {
			actual, err := contract.Integer(raw)
			if err != nil || actual != int64(count) {
				return fmt.Errorf("queue snapshot %s does not match its item inventory", key)
			}
		}
	}
	return nil
}

func exactAuditNumberSet(rows []contract.Object, expected map[int64]bool, where string) error {
	actual := map[int64]bool{}
	for i, row := range rows {
		number, err := contract.PositiveInteger(row["number"])
		if err != nil || actual[number] || !expected[number] {
			return fmt.Errorf("%s contains an invalid, duplicate, or out-of-scope issue at index %d", where, i)
		}
		actual[number] = true
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("%s does not cover the complete expected issue inventory", where)
	}
	return nil
}

func buildBacklogAuditItems(issues []contract.Object, policy backlogAuditPolicy) ([]*backlogAuditItem, map[int64]*backlogAuditItem, int, error) {
	items := make([]*backlogAuditItem, 0, len(issues))
	byNumber := make(map[int64]*backlogAuditItem, len(issues))
	openCount := 0
	for index, issue := range issues {
		number, err := contract.PositiveInteger(issue["number"])
		if err != nil || byNumber[number] != nil {
			return nil, nil, 0, fmt.Errorf("issue graph contains a missing or duplicate number at index %d", index)
		}
		title, err := contract.String(issue, "title")
		if err != nil {
			return nil, nil, 0, fmt.Errorf("issue graph issue #%d must have a title", number)
		}
		state, err := contract.String(issue, "state")
		if err != nil || (state != "OPEN" && state != "CLOSED") {
			return nil, nil, 0, fmt.Errorf("issue graph issue #%d has an invalid state", number)
		}
		if state == "OPEN" {
			openCount++
		}
		inProject, err := contract.Bool(issue, "in_project")
		if err != nil {
			return nil, nil, 0, fmt.Errorf("issue graph issue #%d in_project must be boolean", number)
		}
		fields, err := contract.ObjectAt(issue, "field_values")
		if err != nil {
			return nil, nil, 0, fmt.Errorf("issue graph issue #%d field_values must be an object", number)
		}
		item := &backlogAuditItem{issue: issue, number: number, title: title, prefix: titlePrefix(title), fields: fields}
		if raw, ok := fields[policy.statusField].(string); ok {
			item.status = raw
			item.statusOK = policy.validStatuses[raw]
		}
		if raw, ok := fields[policy.priorityField].(string); ok {
			item.priority = raw
			item.band, item.bandOK = policy.priorityBands[raw]
		}
		if raw, ok := fields[policy.orderField]; ok && raw != nil {
			item.order, err = contract.Number(raw)
			item.orderOK = err == nil
		}
		item.blockers, err = auditIssueReferences(issue, "blocked_by_numbers", number)
		if err != nil {
			return nil, nil, 0, err
		}
		item.children, err = auditIssueReferences(issue, "child_numbers", number)
		if err != nil {
			return nil, nil, 0, err
		}
		if inProject {
			projectItem, err := contract.ObjectAt(issue, "project_item")
			if err != nil {
				return nil, nil, 0, fmt.Errorf("issue #%d claims Project membership without item evidence", number)
			}
			if _, err := requiredString(projectItem, "item_id"); err != nil {
				return nil, nil, 0, fmt.Errorf("issue #%d Project item is missing its immutable item id", number)
			}
			item.archived, err = contract.Bool(projectItem, "archived")
			if err != nil {
				return nil, nil, 0, fmt.Errorf("issue #%d Project archive state must be boolean", number)
			}
			projectFields, err := contract.ObjectAt(projectItem, "field_values")
			if err != nil || !sameAuditObject(projectFields, fields) {
				return nil, nil, 0, fmt.Errorf("issue #%d Project field evidence conflicts with the issue graph", number)
			}
		} else if raw, exists := issue["project_item"]; exists && raw != nil {
			return nil, nil, 0, fmt.Errorf("issue #%d has Project item evidence but in_project is false", number)
		}
		byNumber[number] = item
		items = append(items, item)
	}
	return items, byNumber, openCount, nil
}

func auditIssueReferences(issue contract.Object, key string, number int64) ([]int64, error) {
	values, ok := issue[key].([]any)
	if !ok {
		if numbers, typed := issue[key].([]int64); typed {
			values = make([]any, len(numbers))
			for i, value := range numbers {
				values[i] = value
			}
		} else {
			return nil, fmt.Errorf("issue #%d %s must be an array", number, key)
		}
	}
	result := make([]int64, 0, len(values))
	seen := map[int64]bool{}
	for _, raw := range values {
		value, err := contract.PositiveInteger(raw)
		if err != nil || value == number || seen[value] {
			return nil, fmt.Errorf("issue #%d %s contains an invalid, self, or duplicate reference", number, key)
		}
		seen[value] = true
		result = append(result, value)
	}
	return result, nil
}

func sameAuditObject(a, b contract.Object) bool {
	aBytes, errA := contract.Canonical(a)
	bBytes, errB := contract.Canonical(b)
	return errA == nil && errB == nil && string(aBytes) == string(bBytes)
}

func titlePrefix(title string) string {
	index := strings.IndexByte(title, ':')
	if index < 0 {
		return ""
	}
	return strings.TrimSpace(title[:index])
}

func backlogAuditLess(a, b *backlogAuditItem) bool {
	ra, rb := backlogBandIndex(a), backlogBandIndex(b)
	if ra != rb {
		return ra < rb
	}
	ao, bo := a.order, b.order
	if !a.orderOK {
		ao = math.Inf(1)
	}
	if !b.orderOK {
		bo = math.Inf(1)
	}
	if ao != bo {
		return ao < bo
	}
	return a.number < b.number
}

func backlogBandIndex(item *backlogAuditItem) int {
	if !item.bandOK {
		return len(backlogPriorityBands)
	}
	for i, band := range backlogPriorityBands {
		if band == item.band {
			return i
		}
	}
	return len(backlogPriorityBands)
}

func fractional(value float64) bool { return math.Trunc(value) != value }

func fractionalCountFor(items []*backlogAuditItem) int {
	count := 0
	for _, item := range items {
		if item.orderOK && fractional(item.order) {
			count++
		}
	}
	return count
}

func openBlockers(item *backlogAuditItem, byNumber map[int64]*backlogAuditItem) []int64 {
	result := []int64{}
	for _, number := range item.blockers {
		if blocker := byNumber[number]; blocker == nil || blocker.issue["state"] != "CLOSED" {
			result = append(result, number)
		}
	}
	return result
}

func actionableBacklogChildren(item *backlogAuditItem, queueItems []*backlogAuditItem, byNumber map[int64]*backlogAuditItem, policy backlogAuditPolicy) []int64 {
	eligible := map[int64]bool{}
	for _, candidate := range queueItems {
		eligible[candidate.number] = true
	}
	result := []int64{}
	for _, number := range item.children {
		child := byNumber[number]
		if child == nil || !eligible[number] || child.issue["state"] != "OPEN" || (child.statusOK && policy.doneStatuses[child.status]) || len(openBlockers(child, byNumber)) > 0 {
			continue
		}
		result = append(result, number)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func int64Strings(values []int64) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = strconv.FormatInt(value, 10)
	}
	return result
}

func auditString(value any) string {
	result, _ := value.(string)
	return result
}
