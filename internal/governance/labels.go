package governance

import (
	"errors"
	"fmt"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func normalizeColor(raw any, field, label string) (string, error) {
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("label %q %s must be a six-digit hex color", label, field)
	}
	value = strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(value), "#"))
	if len(value) != 6 {
		return "", fmt.Errorf("label %q %s must be a six-digit hex color", label, field)
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'A' && char <= 'F')) {
			return "", fmt.Errorf("label %q %s must be a six-digit hex color", label, field)
		}
	}
	return value, nil
}

func LabelPaletteDiff(policy, snapshot contract.Object) (contract.Object, error) {
	entries, err := objects(policy, "labels")
	if err != nil || len(entries) == 0 {
		return nil, errors.New("label policy requires an ordered labels array")
	}
	liveEntries, err := objects(snapshot, "labels")
	if err != nil {
		return nil, err
	}
	liveByName := make(map[string]contract.Object, len(liveEntries))
	for _, raw := range liveEntries {
		name, err := requiredString(raw, "name")
		if err != nil {
			return nil, err
		}
		if _, exists := liveByName[name]; exists {
			return nil, fmt.Errorf("live label snapshot contains duplicate label %q", name)
		}
		color, err := normalizeColor(raw["color"], "color", name)
		if err != nil {
			return nil, err
		}
		description, present, err := optionalString(raw, "description")
		if err != nil {
			return nil, err
		}
		if !present {
			description = ""
		}
		isDefault := false
		if value, exists := raw["default"]; exists {
			isDefault, err = contract.Bool(contract.Object{"default": value}, "default")
			if err != nil {
				return nil, fmt.Errorf("live label %q default flag must be boolean", name)
			}
		}
		liveByName[name] = contract.Object{"name": name, "color": color, "description": strings.TrimSpace(description), "default": isDefault}
	}

	labels := make([]any, 0, len(entries))
	seen := map[string]bool{}
	summary := contract.Object{
		"preferred_label_count": len(entries), "repo_managed_count": 0,
		"governance_controlled_count": 0, "create_count": 0,
		"update_count": 0, "noop_count": 0, "managed_drift_count": 0,
		"default_drift_count": 0, "missing_default_count": 0,
	}
	for _, entry := range entries {
		name, err := requiredString(entry, "name")
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("label policy contains duplicate label %q", name)
		}
		seen[name] = true
		role, err := requiredString(entry, "semantic_role")
		if err != nil {
			return nil, err
		}
		owner, err := requiredString(entry, "platform_owner")
		if err != nil || (owner != "repo-managed" && owner != "github-default") {
			return nil, fmt.Errorf("label %q platform_owner must be repo-managed or github-default", name)
		}
		governanceControlled, err := boolValue(entry, "governance_controlled")
		if err != nil {
			return nil, fmt.Errorf("label %q governance_controlled must be boolean", name)
		}
		targetColor, err := normalizeColor(entry["target_color"], "target_color", name)
		if err != nil {
			return nil, err
		}
		referenceColor, err := normalizeColor(entry["current_reference_color"], "current_reference_color", name)
		if err != nil {
			return nil, err
		}
		targetDescription, err := requiredString(entry, "description")
		if err != nil {
			return nil, err
		}
		notes, _, err := optionalString(entry, "notes")
		if err != nil {
			return nil, err
		}
		live, found := liveByName[name]
		managed := owner == "repo-managed"
		if managed {
			summary["repo_managed_count"] = summary["repo_managed_count"].(int) + 1
		}
		if governanceControlled {
			summary["governance_controlled_count"] = summary["governance_controlled_count"].(int) + 1
		}
		colorChanged := found && live["color"] != targetColor
		descriptionChanged := found && live["description"] != targetDescription
		action := "reference-only"
		switch {
		case managed && !found:
			action = "create"
			summary["create_count"] = summary["create_count"].(int) + 1
			summary["managed_drift_count"] = summary["managed_drift_count"].(int) + 1
		case managed && (colorChanged || descriptionChanged):
			action = "update"
			summary["update_count"] = summary["update_count"].(int) + 1
			summary["managed_drift_count"] = summary["managed_drift_count"].(int) + 1
		case managed:
			action = "noop"
			summary["noop_count"] = summary["noop_count"].(int) + 1
		case !found:
			action = "missing-default"
			summary["missing_default_count"] = summary["missing_default_count"].(int) + 1
		case colorChanged || descriptionChanged:
			action = "default-drift"
			summary["default_drift_count"] = summary["default_drift_count"].(int) + 1
		}
		var currentColor, currentDescription any
		defaultLabel := owner == "github-default"
		if found {
			currentColor = live["color"]
			currentDescription = live["description"]
			defaultLabel = live["default"] == true
		}
		labels = append(labels, contract.Object{
			"name": name, "semantic_role": role, "platform_owner": owner,
			"governance_controlled": governanceControlled, "managed": managed,
			"action": action, "current_reference_color": referenceColor,
			"current_live_color": currentColor, "target_color": targetColor,
			"current_live_description": currentDescription,
			"target_description":       targetDescription, "default_label": defaultLabel,
			"notes": notes,
		})
	}
	return contract.Object{"tool": "label_palette_diff", "schema_version": 2, "summary": summary, "labels": labels}, nil
}
