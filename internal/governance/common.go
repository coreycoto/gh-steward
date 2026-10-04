package governance

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func required(inputs map[string]contract.Object, key string) (contract.Object, error) {
	value, ok := inputs[key]
	if !ok || value == nil {
		return nil, fmt.Errorf("%s input is required", key)
	}
	return value, nil
}

func requiredString(o contract.Object, key string) (string, error) {
	return contract.Nonempty(o, key)
}

func optionalString(o contract.Object, key string) (string, bool, error) {
	raw, exists := o[key]
	if !exists || raw == nil {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", false, fmt.Errorf("%s must be a string or null", key)
	}
	return value, true, nil
}

func objects(o contract.Object, key string) ([]contract.Object, error) {
	values, err := contract.Objects(o, key)
	if err != nil {
		return nil, err
	}
	return values, nil
}

func object(o contract.Object, key string) (contract.Object, error) {
	return contract.ObjectAt(o, key)
}

func stringList(o contract.Object, key string, allowEmpty bool) ([]string, error) {
	values, err := contract.Strings(o[key])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	seen := map[string]bool{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || seen[value] {
			return nil, fmt.Errorf("%s entries must be nonempty and unique", key)
		}
		seen[value] = true
	}
	if !allowEmpty && len(values) == 0 {
		return nil, fmt.Errorf("%s must not be empty", key)
	}
	return values, nil
}

func positiveInteger(o contract.Object, key string) (int64, error) {
	n, err := contract.PositiveInteger(o[key])
	if err != nil {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}

func boolValue(o contract.Object, key string) (bool, error) {
	return contract.Bool(o, key)
}

func addFinding(findings *[]any, code, severity, message string, fixable bool, issue contract.Object, details contract.Object) {
	finding := contract.Object{
		"code": code, "severity": severity, "message": message, "fixable": fixable,
	}
	if issue != nil {
		for _, key := range []string{"number", "title", "url"} {
			if value, ok := issue[key]; ok && value != nil {
				finding[key] = value
			}
		}
	}
	if len(details) > 0 {
		finding["details"] = details
	}
	*findings = append(*findings, finding)
}

func findingSummary(findings []any) contract.Object {
	summary := contract.Object{"finding_count": len(findings), "error_count": 0, "warning_count": 0, "info_count": 0, "fixable_count": 0}
	for _, raw := range findings {
		finding := raw.(contract.Object)
		severity := finding["severity"].(string)
		key := severity + "_count"
		if value, ok := summary[key].(int); ok {
			summary[key] = value + 1
		}
		if finding["fixable"] == true {
			summary["fixable_count"] = summary["fixable_count"].(int) + 1
		}
	}
	return summary
}

func namesSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func sortedStrings(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func labelNames(issue contract.Object) (map[string]bool, error) {
	labels, err := contract.Strings(issue["labels"])
	if err != nil {
		return nil, fmt.Errorf("issue labels must be an array of names: %w", err)
	}
	set := map[string]bool{}
	for _, label := range labels {
		if strings.TrimSpace(label) == "" || set[label] {
			return nil, errors.New("issue labels must be nonempty and unique")
		}
		set[label] = true
	}
	return set, nil
}

var sha1Pattern = regexp.MustCompile(`(?i)^[0-9a-f]{40}$`)
var sha256Pattern = regexp.MustCompile(`(?i)^[0-9a-f]{64}$`)
var sha256TaggedPattern = regexp.MustCompile(`(?i)^sha256:[0-9a-f]{64}$`)
var repositoryNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func normalizedSHA(raw any) (string, bool) {
	value, ok := raw.(string)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	if !sha1Pattern.MatchString(value) {
		return "", false
	}
	return strings.ToLower(value), true
}

func sha256Hex(raw any, tagged bool) (string, bool) {
	value, ok := raw.(string)
	if !ok {
		return "", false
	}
	if tagged {
		return value, sha256TaggedPattern.MatchString(value)
	}
	return value, sha256Pattern.MatchString(value)
}
