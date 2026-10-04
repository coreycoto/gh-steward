package snapshot

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// QueuePolicy maps consumer vocabulary to the portable queue's semantic bands.
// There is no inferred project, taxonomy or status/priority default.
type QueuePolicy struct {
	StatusField      string
	PriorityField    string
	OrderField       string
	DoneStatuses     []string
	Priorities       map[string]string // Consumer option -> Now, Next or Later.
	ExcludedPrefixes []string
}

func ParseQueuePolicy(raw contract.Object) (QueuePolicy, error) {
	p := QueuePolicy{}
	var err error
	if p.StatusField, err = contract.Nonempty(raw, "status_field"); err != nil {
		return p, err
	}
	if p.PriorityField, err = contract.Nonempty(raw, "priority_field"); err != nil {
		return p, err
	}
	if p.OrderField, err = contract.Nonempty(raw, "order_field"); err != nil {
		return p, err
	}
	if p.DoneStatuses, err = contract.Strings(raw["done_statuses"]); err != nil || len(p.DoneStatuses) == 0 {
		return p, errors.New("queue policy requires explicit done statuses")
	}
	priorities, err := contract.ObjectAt(raw, "priorities")
	if err != nil {
		return p, err
	}
	p.Priorities = map[string]string{}
	seenBands := map[string]bool{}
	for key, value := range priorities {
		band, ok := value.(string)
		if !ok || key == "" || (band != "Now" && band != "Next" && band != "Later") || seenBands[band] {
			return p, errors.New("queue priority policy must map one distinct option to each semantic band")
		}
		seenBands[band] = true
		p.Priorities[key] = band
	}
	if len(seenBands) != 3 {
		return p, errors.New("queue policy requires Now, Next and Later semantic mappings")
	}
	if _, exists := raw["excluded_prefixes"]; exists {
		p.ExcludedPrefixes, err = contract.Strings(raw["excluded_prefixes"])
		if err != nil {
			return p, err
		}
	}
	return p, nil
}

func (s Service) Queue(graph contract.Object, policy QueuePolicy) (contract.Object, error) {
	if policy.StatusField == "" || policy.PriorityField == "" || policy.OrderField == "" || len(policy.DoneStatuses) == 0 || len(policy.Priorities) != 3 {
		return nil, errors.New("queue policy is incomplete")
	}
	repo, err := contract.ObjectAt(graph, "repo")
	if err != nil {
		return nil, err
	}
	if err = s.validateRepository(repo); err != nil {
		return nil, err
	}
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	exclude := map[string]bool{}
	for _, prefix := range policy.ExcludedPrefixes {
		exclude[prefix] = true
	}
	done := map[string]bool{}
	for _, status := range policy.DoneStatuses {
		done[status] = true
	}
	all := []contract.Object{}
	eligible := map[int64]contract.Object{}
	seen := map[int64]bool{}
	for _, issue := range issues {
		n, err := contract.PositiveInteger(issue["number"])
		if err != nil || seen[n] {
			return nil, errors.New("queue issue identity missing or duplicated")
		}
		seen[n] = true
		inProject, err := contract.Bool(issue, "in_project")
		if err != nil {
			return nil, err
		}
		if !inProject {
			continue
		}
		if projectItem, ok := issue["project_item"].(map[string]any); ok {
			if projectItem["archived"] == true {
				continue
			}
		}
		item, err := contract.Clone(issue)
		if err != nil {
			return nil, err
		}
		fields, err := contract.ObjectAt(item, "field_values")
		if err != nil {
			return nil, err
		}
		canonical := contract.Object{}
		if status, exists := fields[policy.StatusField]; exists && status != nil {
			str, ok := status.(string)
			if !ok {
				return nil, errors.New("queue status must be string or unset")
			}
			if done[str] {
				canonical["Status"] = "Done"
			} else {
				canonical["Status"] = str
			}
		}
		if priority, exists := fields[policy.PriorityField]; exists && priority != nil {
			str, ok := priority.(string)
			if !ok {
				return nil, errors.New("queue priority must be string or unset")
			}
			semantic, exists := policy.Priorities[str]
			if !exists && str != "" {
				return nil, fmt.Errorf("live queue priority %q is outside the explicit consumer mapping", str)
			}
			canonical["Priority"] = semantic
		}
		if order, exists := fields[policy.OrderField]; exists && order != nil {
			if _, err := contract.Number(order); err != nil {
				return nil, err
			}
			canonical["Queue Order"] = order
		}
		item["source_field_values"] = fields
		item["field_values"] = canonical
		if _, exists := item["prefix"]; !exists {
			item["prefix"] = nil
			title, err := contract.String(item, "title")
			if err != nil {
				return nil, err
			}
			if i := strings.Index(title, ":"); i > 0 {
				item["prefix"] = strings.TrimSpace(title[:i])
			}
		}
		all = append(all, item)
		if !exclude[fmt.Sprint(item["prefix"])] {
			eligible[n] = item
		}
	}
	sort.Slice(all, func(i, j int) bool {
		order := func(o contract.Object) float64 {
			f := o["field_values"].(map[string]any)
			n, err := contract.Number(f["Queue Order"])
			if err != nil {
				return math.Inf(1)
			}
			return n
		}
		a, b := order(all[i]), order(all[j])
		if a != b {
			return a < b
		}
		na, _ := contract.PositiveInteger(all[i]["number"])
		nb, _ := contract.PositiveInteger(all[j]["number"])
		return na < nb
	})
	items, excluded := []any{}, []any{}
	for _, item := range all {
		children, err := contract.Array(item, "child_numbers")
		if err != nil {
			return nil, err
		}
		blockers, err := contract.Array(item, "blocked_by_numbers")
		if err != nil {
			return nil, err
		}
		for _, raw := range blockers {
			if _, err := contract.PositiveInteger(raw); err != nil {
				return nil, err
			}
		}
		queueChildren, actionableChildren := []any{}, []any{}
		for _, raw := range children {
			n, err := contract.PositiveInteger(raw)
			if err != nil {
				return nil, err
			}
			if child, exists := eligible[n]; exists {
				queueChildren = append(queueChildren, n)
				f := child["field_values"].(map[string]any)
				bs, err := contract.Array(child, "blocked_by_numbers")
				if err != nil {
					return nil, err
				}
				if child["state"] == "OPEN" && f["Status"] != "Done" && len(bs) == 0 {
					actionableChildren = append(actionableChildren, n)
				}
			}
		}
		isDone := item["field_values"].(map[string]any)["Status"] == "Done"
		if item["state"] != "OPEN" && item["state"] != "CLOSED" {
			return nil, errors.New("queue issue state must be OPEN or CLOSED")
		}
		item["child_numbers"] = queueChildren
		item["actionable_child_numbers"] = actionableChildren
		item["is_done"] = isDone
		item["is_blocked"] = len(blockers) > 0
		item["has_actionable_child"] = len(actionableChildren) > 0
		item["is_actionable"] = item["state"] == "OPEN" && !isDone && len(blockers) == 0 && len(actionableChildren) == 0
		if exclude[fmt.Sprint(item["prefix"])] {
			item["exclusion_reason"] = "excluded_by_consumer_policy"
			excluded = append(excluded, item)
		} else {
			items = append(items, item)
		}
	}
	return contract.Object{"repo": repo, "project": graph["project"], "provenance": graph["provenance"], "generated_at": s.stamp(), "items": items, "excluded_items": excluded, "item_count": len(items), "project_item_count": len(all), "excluded_item_count": len(excluded)}, nil
}
