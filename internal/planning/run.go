package planning

import (
	"fmt"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// Run dispatches a pure domain operation. The CLI layer is responsible for
// adding the public envelope and for loading any snapshots or policy files.
func Run(command string, inputs map[string]contract.Object) (contract.Object, error) {
	get := func(key string) (contract.Object, error) {
		value, ok := inputs[key]
		if !ok || value == nil {
			return nil, fmt.Errorf("%s input is required", key)
		}
		return value, nil
	}
	optionalPrefixes := func() ([]string, error) {
		value, ok := inputs["excluded_prefixes"]
		if !ok {
			return []string{}, nil
		}
		if value == nil {
			return nil, fmt.Errorf("excluded_prefixes input must be an object")
		}
		values, ok := value["values"]
		if !ok {
			return nil, fmt.Errorf("excluded_prefixes input must contain values")
		}
		return contract.Strings(values)
	}
	switch command {
	case "quarter-plan-validate":
		payload, err := get("payload")
		if err != nil {
			return nil, err
		}
		return NormalizeQuarterPlan(payload)
	case "quarter-plan-backlog-delta":
		payload, err := get("payload")
		if err != nil {
			return nil, err
		}
		return BuildQuarterPlanBacklogDelta(payload)
	case "quarter-plan-delta":
		input, err := get("input")
		if err != nil {
			return nil, err
		}
		return BuildQuarterPlanDelta(input)
	case "review-findings-validate":
		payload, err := get("payload")
		if err != nil {
			return nil, err
		}
		return NormalizeReviewFindings(payload)
	case "review-backlog-delta":
		input, err := get("input")
		if err != nil {
			return nil, err
		}
		return BuildReviewBacklogDelta(input)
	case "review-backlog-legacy-delta":
		payload, err := get("payload")
		if err != nil {
			return nil, err
		}
		policy, err := get("policy")
		if err != nil {
			return nil, err
		}
		return BuildLegacyReviewBacklogDelta(payload, policy)
	case "review-closeout-findings-validate":
		payload, err := get("payload")
		if err != nil {
			return nil, err
		}
		return NormalizeReviewCloseoutFindings(payload)
	case "review-closeout-audit":
		input, err := get("input")
		if err != nil {
			return nil, err
		}
		return BuildReviewCloseoutAudit(input)
	case "relationship-audit":
		graph, err := get("issue_graph")
		if err != nil {
			return nil, err
		}
		return AuditRelationships(graph)
	case "relationship-validate", "relationship-delta":
		payload, err := get("payload")
		if err != nil {
			return nil, err
		}
		graph, err := get("issue_graph")
		if err != nil {
			return nil, err
		}
		var hierarchy contract.Object
		if raw, ok := inputs["hierarchy_policy"]; ok {
			hierarchy = raw
		}
		normalized, err := ValidateRelationshipPayload(payload, graph, hierarchy)
		if err != nil {
			return nil, err
		}
		if command == "relationship-validate" {
			return normalized, nil
		}
		delta, err := BuildRelationshipDelta(normalized, graph)
		if err != nil {
			return nil, err
		}
		for key, value := range normalized {
			if key == "schema_version" || key == "notes" {
				delta[key] = value
			}
		}
		delta["touched_issues"] = normalized["issues"]
		return delta, nil
	case "next-item":
		snapshot, err := get("queue_snapshot")
		if err != nil {
			return nil, err
		}
		var candidate *int64
		if raw, ok := inputs["candidate"]; ok {
			value, err := contract.PositiveInteger(raw["issue_number"])
			if err != nil {
				return nil, fmt.Errorf("candidate.issue_number must be a positive integer")
			}
			candidate = &value
		}
		return SelectNextItem(snapshot, candidate)
	case "rebalance-rank":
		graph, err := get("issue_graph")
		if err != nil {
			return nil, err
		}
		queue, err := get("queue_snapshot")
		if err != nil {
			return nil, err
		}
		project, err := get("project_snapshot")
		if err != nil {
			return nil, err
		}
		policy, err := get("policy")
		if err != nil {
			return nil, err
		}
		linked := contract.Object{}
		if value, ok := inputs["linked_prs_by_issue"]; ok {
			linked = value
		}
		prefixes, err := optionalPrefixes()
		if err != nil {
			return nil, err
		}
		return BuildRankedRebalanceSeed(graph, queue, project, policy, linked, prefixes)
	case "rebalance-validate":
		payload, err := get("payload")
		if err != nil {
			return nil, err
		}
		graph, err := get("issue_graph")
		if err != nil {
			return nil, err
		}
		project, err := get("project_snapshot")
		if err != nil {
			return nil, err
		}
		prefixes, err := optionalPrefixes()
		if err != nil {
			return nil, err
		}
		return NormalizeRebalanceDecisions(payload, graph, project, prefixes)
	case "rebalance-plan":
		payload, err := get("payload")
		if err != nil {
			return nil, err
		}
		graph, err := get("issue_graph")
		if err != nil {
			return nil, err
		}
		queue, err := get("queue_snapshot")
		if err != nil {
			return nil, err
		}
		project, err := get("project_snapshot")
		if err != nil {
			return nil, err
		}
		options, err := get("options")
		if err != nil {
			return nil, err
		}
		prefixes, err := optionalPrefixes()
		if err != nil {
			return nil, err
		}
		audit := contract.Object{}
		if value, ok := inputs["backlog_audit"]; ok {
			audit = value
		}
		return BuildRebalancePlan(payload, graph, queue, project, options, audit, prefixes)
	default:
		return nil, fmt.Errorf("unsupported planning command %q", command)
	}
}
