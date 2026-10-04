package planning

import (
	"fmt"
	"math"
	"sort"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var priorityBands = []string{"Now", "Next", "Later"}

type queueItem struct {
	raw      contract.Object
	number   int64
	fields   contract.Object
	priority string
	order    float64
	orderOK  bool
}

func objectOrEmpty(parent contract.Object, key string) contract.Object {
	if value, ok := parent[key].(map[string]any); ok {
		return value
	}
	return contract.Object{}
}

func queueItems(snapshot contract.Object, key string) ([]queueItem, error) {
	rows, err := contract.Objects(snapshot, key)
	if err != nil {
		return nil, err
	}
	items := make([]queueItem, 0, len(rows))
	seen := map[int64]bool{}
	for index, row := range rows {
		number, err := issueNumber(row, fmt.Sprintf("%s[%d]", key, index))
		if err != nil {
			return nil, err
		}
		if seen[number] {
			return nil, fmt.Errorf("%s contains duplicate issue #%d", key, number)
		}
		seen[number] = true
		fields := objectOrEmpty(row, "field_values")
		priority, _ := fields["Priority"].(string)
		order, orderErr := contract.Number(fields["Queue Order"])
		items = append(items, queueItem{raw: row, number: number, fields: fields, priority: priority, order: order, orderOK: orderErr == nil})
	}
	return items, nil
}

func itemOrder(item queueItem) float64 {
	if item.orderOK {
		return item.order
	}
	return math.Inf(1)
}
func priorityIndex(priority string) int {
	for i, band := range priorityBands {
		if band == priority {
			return i
		}
	}
	return len(priorityBands)
}
func sortQueueItems(items []queueItem, priority string) []queueItem {
	result := []queueItem{}
	for _, item := range items {
		if item.priority == priority {
			result = append(result, item)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, b := itemOrder(result[i]), itemOrder(result[j])
		if a != b {
			return a < b
		}
		return result[i].number < result[j].number
	})
	return result
}

func arrayPositiveIntegers(value any, where string) ([]int64, error) {
	return positiveNumberSet(defaultArray(value), where)
}
func text(value any) string  { result, _ := value.(string); return result }
func nullable(value any) any { return value }
func queueReason(item queueItem) string {
	if reason := text(item.raw["exclusion_reason"]); reason != "" {
		return reason
	}
	if item.fields["Status"] == "Done" {
		return "done"
	}
	blockers, _ := arrayPositiveIntegers(item.raw["blocked_by_numbers"], "blocked_by_numbers")
	if len(blockers) > 0 {
		return "blocked"
	}
	children, _ := arrayPositiveIntegers(item.raw["actionable_child_numbers"], "actionable_child_numbers")
	if len(children) > 0 {
		return "parent-with-actionable-child"
	}
	return "actionable"
}

// SelectNextItem selects the earliest actionable issue from Now, then Next.
// It deliberately stops before Later when neither current band is actionable.
func SelectNextItem(snapshot contract.Object, candidateNumber *int64) (contract.Object, error) {
	items, err := queueItems(snapshot, "items")
	if err != nil {
		return nil, err
	}
	excluded, err := queueItems(snapshot, "excluded_items")
	if err != nil && snapshot["excluded_items"] != nil {
		return nil, err
	}
	byNumber := map[int64]queueItem{}
	excludedByNumber := map[int64]queueItem{}
	for _, item := range items {
		byNumber[item.number] = item
	}
	for _, item := range excluded {
		excludedByNumber[item.number] = item
	}
	decisions := []any{}
	var selected *queueItem
	selectedBand := any(nil)
	actionableNow := false
	for _, band := range priorityBands {
		if band == "Later" {
			break
		}
		bandItems := sortQueueItems(items, band)
		actionable := []queueItem{}
		for _, item := range bandItems {
			blockers, _ := arrayPositiveIntegers(item.raw["blocked_by_numbers"], "blocked_by_numbers")
			children, _ := arrayPositiveIntegers(item.raw["actionable_child_numbers"], "actionable_child_numbers")
			decisions = append(decisions, contract.Object{"number": item.number, "title": item.raw["title"], "priority": band, "queue_order": item.fields["Queue Order"], "reason": queueReason(item), "blocked_by_numbers": blockers, "actionable_child_numbers": children})
			if queueReason(item) == "actionable" {
				actionable = append(actionable, item)
			}
		}
		if band == "Now" {
			actionableNow = len(actionable) > 0
		}
		if len(actionable) > 0 {
			choice := actionable[0]
			selected = &choice
			selectedBand = band
			break
		}
	}
	result := contract.Object{"repo": snapshot["repo"], "project": snapshot["project"], "generated_at": snapshot["generated_at"], "all_now_blocked": !actionableNow, "selected_band": selectedBand, "selected": nil, "candidate": nil, "candidate_is_selected": nil, "decisions": decisions}
	if selected != nil {
		blockers, _ := arrayPositiveIntegers(selected.raw["blocked_by_numbers"], "blocked_by_numbers")
		children, _ := arrayPositiveIntegers(selected.raw["actionable_child_numbers"], "actionable_child_numbers")
		result["selected"] = contract.Object{"number": selected.number, "title": selected.raw["title"], "priority": selected.priority, "queue_order": selected.fields["Queue Order"], "blocked_by_numbers": blockers, "actionable_child_numbers": children}
	}
	if candidateNumber != nil {
		candidate, ok := byNumber[*candidateNumber]
		if !ok {
			candidate, ok = excludedByNumber[*candidateNumber]
		}
		if ok {
			children, _ := arrayPositiveIntegers(candidate.raw["actionable_child_numbers"], "actionable_child_numbers")
			record := contract.Object{"number": candidate.number, "title": candidate.raw["title"], "prefix": candidate.raw["prefix"], "priority": candidate.priority, "queue_order": candidate.fields["Queue Order"], "reason": queueReason(candidate), "recommended_issue": nil}
			childCandidates := []queueItem{}
			for _, number := range children {
				if child, ok := byNumber[number]; ok {
					childCandidates = append(childCandidates, child)
				}
			}
			sort.SliceStable(childCandidates, func(i, j int) bool {
				a, b := childCandidates[i], childCandidates[j]
				if priorityIndex(a.priority) != priorityIndex(b.priority) {
					return priorityIndex(a.priority) < priorityIndex(b.priority)
				}
				if itemOrder(a) != itemOrder(b) {
					return itemOrder(a) < itemOrder(b)
				}
				return a.number < b.number
			})
			if len(childCandidates) > 0 {
				child := childCandidates[0]
				record["recommended_issue"] = contract.Object{"number": child.number, "title": child.raw["title"], "priority": child.priority, "queue_order": child.fields["Queue Order"]}
			}
			result["candidate"] = record
			result["candidate_is_selected"] = selected != nil && selected.number == candidate.number
		}
	}
	return result, nil
}
