package governance

import (
	"fmt"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// Run evaluates one pure governance operation over caller-supplied policy and
// snapshots. Native reads, filesystem access, and mutations belong to callers.
func Run(command string, inputs map[string]contract.Object) (contract.Object, error) {
	if inputs == nil {
		inputs = map[string]contract.Object{}
	}
	switch command {
	case "backlog-audit":
		policy, err := required(inputs, "policy")
		if err != nil {
			return nil, err
		}
		graph, err := required(inputs, "issue_graph")
		if err != nil {
			return nil, err
		}
		return BacklogAudit(policy, graph, inputs["queue_snapshot"])
	case "label-plan":
		policy, err := required(inputs, "policy")
		if err != nil {
			return nil, err
		}
		snapshot, err := required(inputs, "snapshot")
		if err != nil {
			return nil, err
		}
		return LabelPaletteDiff(policy, snapshot)
	case "governance-check":
		policy, err := required(inputs, "policy")
		if err != nil {
			return nil, err
		}
		snapshot, err := required(inputs, "snapshot")
		if err != nil {
			return nil, err
		}
		return GovernanceCheck(policy, snapshot)
	case "governance-summary":
		policy, err := required(inputs, "policy")
		if err != nil {
			return nil, err
		}
		snapshot, err := required(inputs, "snapshot")
		if err != nil {
			return nil, err
		}
		return GovernanceSummary(policy, snapshot)
	case "milestone-check":
		policy, err := required(inputs, "policy")
		if err != nil {
			return nil, err
		}
		snapshot, err := required(inputs, "snapshot")
		if err != nil {
			return nil, err
		}
		return MilestoneCheck(policy, snapshot)
	case "execution-preflight":
		policy, err := required(inputs, "policy")
		if err != nil {
			return nil, err
		}
		snapshot, err := required(inputs, "snapshot")
		if err != nil {
			return nil, err
		}
		return ExecutionPreflight(policy, snapshot)
	case "execution-transition":
		policy, err := required(inputs, "policy")
		if err != nil {
			return nil, err
		}
		snapshot, err := required(inputs, "snapshot")
		if err != nil {
			return nil, err
		}
		return ExecutionTransition(policy, snapshot)
	case "execution-link-facts":
		policy, err := required(inputs, "policy")
		if err != nil {
			return nil, err
		}
		snapshot, err := required(inputs, "snapshot")
		if err != nil {
			return nil, err
		}
		return ExecutionLinkFacts(policy, snapshot)
	case "execution-recover":
		policy, err := required(inputs, "policy")
		if err != nil {
			return nil, err
		}
		snapshot, err := required(inputs, "snapshot")
		if err != nil {
			return nil, err
		}
		return VerifyRecoveryAttestation(policy, snapshot)
	case "merge-eligibility":
		policy, err := required(inputs, "policy")
		if err != nil {
			return nil, err
		}
		snapshot, err := required(inputs, "snapshot")
		if err != nil {
			return nil, err
		}
		return MergeEligibility(policy, snapshot)
	default:
		return nil, fmt.Errorf("unsupported governance command %q", command)
	}
}
