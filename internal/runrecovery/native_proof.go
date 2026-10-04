package runrecovery

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var (
	nativeCommandPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,79}$`)
	nativeOperationID    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	nativeCommitOID      = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

var (
	nativeJournalIdentityFields = []string{"schema_version", "repository", "command", "plan_sha256"}
	nativeJournalFields         = []string{"identity", "steps", "result"}
	nativeTerminalFields        = []string{"status", "command", "repository", "plan_sha256", "receipts"}
	nativeApplyFields           = []string{"schema_version", "tool_version", "command", "repository", "data"}
	nativeStepRequiredFields    = []string{"id", "intent", "intent_sha256", "operation_id", "status", "result", "started_at", "completed_at"}
	nativeStepOptionalFields    = []string{"acknowledgement", "observation"}
)

// ValidateTerminalPlanProof verifies one exact native v2 plan, its durable
// journal and its apply-result envelope. It only validates evidence; it never
// resumes or dispatches an operation.
func ValidateTerminalPlanProof(proof any) (Object, error) {
	retained, err := Exact(proof, []string{"name", "command", "plan_sha256", "journal_id", "plan_file", "journal_file", "apply_result_file"}, "terminal plan proof")
	if err != nil {
		return nil, err
	}
	name, ok := retained["name"].(string)
	if !ok || !nativeNonempty(name) {
		return nil, errors.New("terminal plan proof has no canonical name")
	}
	command, ok := retained["command"].(string)
	if !ok || !nativeCommandPattern.MatchString(command) {
		return nil, errors.New("terminal plan proof has no canonical command")
	}
	planDigest, err := nativeDigest(retained["plan_sha256"], "terminal plan proof digest")
	if err != nil {
		return nil, err
	}
	journalDigest, err := nativeDigest(retained["journal_id"], "terminal journal identity")
	if err != nil {
		return nil, err
	}

	planValue, err := LoadFileProof(retained["plan_file"], "plan")
	if err != nil {
		return nil, err
	}
	rawPlan, ok := planValue.(map[string]any)
	if !ok {
		return nil, errors.New("terminal plan is not an object")
	}
	plan, err := contract.ParsePlan(rawPlan)
	if err != nil {
		return nil, fmt.Errorf("terminal Go v2 plan is invalid: %w", err)
	}
	if plan.Command != command || plan.SHA256 != planDigest {
		return nil, errors.New("terminal plan command or digest differs from its retained proof")
	}
	for sourceName := range plan.Sources {
		if !nativeNonempty(sourceName) {
			return nil, errors.New("terminal plan source provenance has an invalid identity")
		}
	}
	planObject := plan.Object()
	repository := plan.Repository.Object()
	operations := make([]Object, 0, len(plan.Operations))
	for _, operation := range plan.Operations {
		if !nativeNonempty(operation.ID) {
			return nil, errors.New("terminal plan primitive identity is invalid")
		}
		operations = append(operations, Object{
			"id": operation.ID, "kind": operation.Kind, "target": operation.Target,
			"before": operation.Before, "after": operation.After,
		})
	}

	journalValue, err := LoadFileProof(retained["journal_file"], "journal")
	if err != nil {
		return nil, err
	}
	journal, err := Exact(journalValue, nativeJournalFields, "terminal journal")
	if err != nil {
		return nil, err
	}
	identity, err := Exact(journal["identity"], nativeJournalIdentityFields, "terminal journal identity")
	if err != nil {
		return nil, err
	}
	expectedIdentity := Object{
		"schema_version": 2, "repository": repository, "command": command, "plan_sha256": planDigest,
	}
	if !nativeIntegerIs(identity["schema_version"], 2) || !Equal(identity, expectedIdentity) {
		return nil, errors.New("terminal journal does not bind the exact repository, command and plan")
	}
	identityBytes, err := Canonical(expectedIdentity)
	if err != nil || SHA256(identityBytes) != journalDigest {
		return nil, errors.New("terminal proof journal ID differs from the exact Go plan identity")
	}

	steps, ok := journal["steps"].([]any)
	if !ok || len(steps) != len(operations) {
		return nil, errors.New("terminal journal does not cover every planned primitive in order")
	}
	dispatches := make(map[string]bool, len(steps))
	for index, rawStep := range steps {
		step, ok := rawStep.(map[string]any)
		if !ok || !nativeAllowedFields(step, nativeStepRequiredFields, nativeStepOptionalFields) {
			return nil, errors.New("terminal journal primitive has an unsupported or incomplete shape")
		}
		operation := operations[index]
		if step["id"] != operation["id"] {
			return nil, errors.New("terminal journal primitive order differs from the plan")
		}
		intent := Object{
			"id": operation["id"], "kind": operation["kind"], "target": operation["target"],
			"before": operation["before"], "after": operation["after"],
		}
		if !Equal(step["intent"], intent) {
			return nil, errors.New("terminal journal primitive intent differs from the plan")
		}
		intentBytes, err := Canonical(intent)
		if err != nil || step["intent_sha256"] != SHA256(intentBytes) {
			return nil, errors.New("terminal journal primitive intent digest is invalid")
		}
		operationID, ok := step["operation_id"].(string)
		if !ok || !nativeOperationID.MatchString(operationID) {
			return nil, errors.New("terminal journal primitive lacks its exact 32-hex dispatch identity")
		}
		if dispatches[operationID] {
			return nil, errors.New("terminal journal reuses a primitive dispatch identity")
		}
		dispatches[operationID] = true
		if step["status"] != "completed" {
			return nil, errors.New("terminal journal contains an unfinished primitive")
		}
		result, ok := step["result"].(map[string]any)
		if !ok {
			return nil, errors.New("completed journal primitive result must be an object")
		}
		if err := nativeTimestamp(step["started_at"], "journal dispatch start time"); err != nil {
			return nil, err
		}
		if err := nativeTimestamp(step["completed_at"], "journal completion time"); err != nil {
			return nil, err
		}
		if result["operation_id"] != operationID {
			return nil, errors.New("terminal primitive result differs from its exact dispatch identity")
		}
		ackRaw, hasAck := step["acknowledgement"]
		observationRaw, hasObservation := step["observation"]
		if !hasAck && !hasObservation {
			return nil, errors.New("terminal primitive has neither a durable acknowledgement nor positive observation")
		}
		if hasAck {
			if err := validateNativeAcknowledgement(ackRaw, result, operation, repository, operationID, command); err != nil {
				return nil, err
			}
		}
		if hasObservation {
			if err := validateNativeObservation(observationRaw, result, operationID); err != nil {
				return nil, err
			}
		}
	}

	terminal, err := Exact(journal["result"], nativeTerminalFields, "terminal journal result")
	if err != nil {
		return nil, err
	}
	if terminal["status"] != "completed" || terminal["command"] != command ||
		!Equal(terminal["repository"], repository) || terminal["plan_sha256"] != planDigest ||
		!Equal(terminal["receipts"], steps) {
		return nil, errors.New("terminal journal result does not match the exact complete primitive receipt list")
	}
	if _, ok := terminal["receipts"].([]any); !ok {
		return nil, errors.New("terminal journal receipts must be an ordered list")
	}

	applyValue, err := LoadFileProof(retained["apply_result_file"], "apply result")
	if err != nil {
		return nil, err
	}
	apply, err := Exact(applyValue, nativeApplyFields, "terminal apply result")
	if err != nil {
		return nil, err
	}
	if !nativeIntegerIs(apply["schema_version"], 2) || !nativeNonemptyValue(apply["tool_version"]) ||
		apply["command"] != command || !Equal(apply["repository"], repository) || !Equal(apply["data"], terminal) {
		return nil, errors.New("terminal apply artifact differs from its exact command, repository or journal result")
	}
	return planObject, nil
}

// Native runtime envelopes use Repository.Object(), including nameWithOwner.
// The unsigned reviewed plan deliberately uses the four-field repository shape.
func nativePlanRepository(plan Object) (Object, error) {
	raw, err := object(plan["repository"], "native plan repository")
	if err != nil {
		return nil, err
	}
	repository, err := contract.ParseRepository(raw)
	if err != nil {
		return nil, err
	}
	return repository.Object(), nil
}

func nativeAllowedFields(value Object, required, optional []string) bool {
	if len(value) < len(required) || len(value) > len(required)+len(optional) {
		return false
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, field := range required {
		allowed[field] = true
		if _, exists := value[field]; !exists {
			return false
		}
	}
	for _, field := range optional {
		allowed[field] = true
	}
	for field := range value {
		if !allowed[field] {
			return false
		}
	}
	return true
}

func nativeNonempty(value string) bool {
	return value != "" && !strings.ContainsAny(value, "\r\n")
}

func nativeNonemptyValue(value any) bool {
	text, ok := value.(string)
	return ok && nativeNonempty(text)
}

func nativeDigest(value any, name string) (string, error) {
	digest, ok := value.(string)
	if !ok || !IsSHA256(digest) {
		return "", fmt.Errorf("%s is not a canonical SHA-256 digest", name)
	}
	return digest, nil
}

func nativeIntegerIs(value any, expected int64) bool {
	number, err := contract.Integer(value)
	return err == nil && number == expected
}

func nativeTimestamp(value any, name string) error {
	raw, ok := value.(string)
	if !ok {
		return fmt.Errorf("%s is not an explicit RFC3339 timestamp", name)
	}
	stamp, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || stamp.Year() < 1 || stamp.UTC().Year() < 1 || stamp.UTC().IsZero() {
		return fmt.Errorf("%s is not a valid nonzero RFC3339 timestamp", name)
	}
	return nil
}

func validateNativeAcknowledgement(raw any, result Object, operation Object, repository Object, operationID, command string) error {
	ack, ok := raw.(map[string]any)
	if !ok {
		return errors.New("terminal native acknowledgement is malformed")
	}
	common := Object{"operation_id": operationID, "repository": repository, "target": operation["target"]}
	var expectedResultFields map[string]bool
	if _, exists := ack["acknowledged"]; exists {
		fields := []string{"kind", "primitive_id", "repository", "operation_id", "target", "before", "after", "acknowledged", "provider_result"}
		if _, err := Exact(ack, fields, "terminal provider acknowledgement"); err != nil {
			return err
		}
		_, isObject := ack["provider_result"].(map[string]any)
		if ack["kind"] != operation["kind"] || ack["primitive_id"] != operation["id"] ||
			!Equal(ack["before"], operation["before"]) || !Equal(ack["after"], operation["after"]) ||
			ack["acknowledged"] != true || !isObject {
			return errors.New("terminal provider acknowledgement differs from its reviewed primitive")
		}
		expectedResultFields = nativeFieldSet(fields, "after_verified")
		if !nativeHasExactSet(result, expectedResultFields) || result["after_verified"] != true {
			return errors.New("terminal provider result lacks its independent after-state verification")
		}
	} else if _, exists := ack["provider_ack"]; exists {
		branchFields := []string{"kind", "primitive_id", "repository", "operation_id", "target", "provider_ack"}
		branchInventoryFields := append(append([]string{}, branchFields...), "observed_inventory_sha256")
		mergeFields := []string{"command", "repository", "operation_id", "target", "provider_ack"}
		switch {
		case nativeHasExactSet(ack, nativeSliceSet(branchFields)) || nativeHasExactSet(ack, nativeSliceSet(branchInventoryFields)):
			if ack["kind"] != operation["kind"] || ack["primitive_id"] != operation["id"] {
				return errors.New("terminal native acknowledgement differs from its reviewed primitive")
			}
			expectedResultFields = nativeFieldSetFromObject(ack, "observed_inventory_sha256")
			if err := validateNativeBranchAck(ack["provider_ack"], operation, repository); err != nil {
				return err
			}
		case nativeHasExactSet(ack, nativeSliceSet(mergeFields)):
			if ack["command"] != command {
				return errors.New("terminal merge acknowledgement differs from its plan command")
			}
			expectedResultFields = nativeFieldSet(mergeFields, "observed_inventory_sha256")
			if err := validateNativeMergeAck(ack["provider_ack"]); err != nil {
				return err
			}
		default:
			return errors.New("terminal native provider acknowledgement has an unsupported shape")
		}
		if _, err := nativeDigest(result["observed_inventory_sha256"], "verified after-state inventory"); err != nil {
			return err
		}
		if !nativeHasExactSet(result, expectedResultFields) {
			return errors.New("terminal native result has an unsupported after-state shape")
		}
	} else {
		return errors.New("terminal primitive has no supported native acknowledgement")
	}
	for field, expected := range common {
		if !Equal(ack[field], expected) {
			return errors.New("terminal native acknowledgement differs from its exact dispatch identity")
		}
	}
	for field, value := range ack {
		if !Equal(result[field], value) {
			return errors.New("terminal primitive result differs from its durable native acknowledgement")
		}
	}
	return nil
}

func validateNativeBranchAck(raw any, operation Object, repository Object) error {
	ack, err := Exact(raw, []string{"exit_code", "stdout", "stderr", "remote_url", "ref", "expected_sha"}, "leased branch-deletion acknowledgement")
	if err != nil {
		return err
	}
	target, ok := operation["target"].(map[string]any)
	branch, branchOK := target["name"].(string)
	expectedSHA, shaOK := target["sha"].(string)
	repoURL, urlOK := repository["url"].(string)
	stdout, stdoutOK := ack["stdout"].(string)
	_, stderrOK := ack["stderr"].(string)
	if !ok || !branchOK || !nativeNonempty(branch) || !shaOK || !nativeCommitOID.MatchString(expectedSHA) ||
		!urlOK || !stdoutOK || !stderrOK || !nativeIntegerIs(ack["exit_code"], 0) ||
		ack["remote_url"] != repoURL+".git" || ack["ref"] != "refs/heads/"+branch || ack["expected_sha"] != expectedSHA {
		return errors.New("terminal branch-deletion acknowledgement differs from its reviewed lease")
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	header, deletion, completed := 0, 0, 0
	for _, line := range lines {
		switch line {
		case "To " + repoURL + ".git":
			header++
		case "Done":
			completed++
		default:
			if !strings.Contains(line, "\t") {
				return errors.New("terminal Git porcelain output contains unrecognized completion data")
			}
			if line != "-\t:refs/heads/"+branch+"\t[deleted]" {
				return errors.New("terminal Git porcelain output does not identify the reviewed branch deletion")
			}
			deletion++
		}
	}
	if header != 1 || deletion != 1 || completed != 1 {
		return errors.New("terminal Git porcelain output has incomplete or repeated deletion evidence")
	}
	return nil
}

func validateNativeMergeAck(raw any) error {
	ack, ok := raw.(map[string]any)
	if !ok || ack["merged"] != true {
		return errors.New("terminal merge acknowledgement does not positively identify completion")
	}
	commit, ok := ack["sha"].(string)
	if !ok || !nativeCommitOID.MatchString(commit) {
		return errors.New("terminal merge acknowledgement lacks its exact commit identity")
	}
	return nil
}

func validateNativeObservation(raw any, result Object, operationID string) error {
	base := []string{"positive_identity", "after_state_verified", "operation_id", "reference"}
	complete := append(append([]string{}, base...), "live", "complete", "source", "inventory_sha256")
	observation, ok := raw.(map[string]any)
	if !ok || !(nativeHasExactSet(observation, nativeSliceSet(base)) || nativeHasExactSet(observation, nativeSliceSet(complete))) {
		return errors.New("terminal positive observation has an unsupported shape")
	}
	if observation["positive_identity"] != true || observation["after_state_verified"] != true ||
		observation["operation_id"] != operationID || !nativeNonemptyValue(observation["reference"]) {
		return errors.New("terminal positive observation differs from its exact primitive")
	}
	if nativeHasExactSet(observation, nativeSliceSet(complete)) {
		if observation["live"] != true || observation["complete"] != true || observation["source"] != "github_api" {
			return errors.New("terminal observation lacks complete live native provenance")
		}
		inventory, err := nativeDigest(observation["inventory_sha256"], "observed inventory")
		if err != nil || result["observed_inventory_sha256"] != inventory {
			return errors.New("terminal result differs from its exact observed inventory digest")
		}
	}
	if result["operation_id"] != operationID {
		return errors.New("terminal observed result differs from its exact dispatch identity")
	}
	if result["after_verified"] == true {
		return nil
	}
	if _, err := nativeDigest(result["observed_inventory_sha256"], "observed result inventory"); err != nil {
		return errors.New("terminal observed result lacks independently verified after-state")
	}
	return nil
}

func nativeSliceSet(fields []string) map[string]bool {
	result := make(map[string]bool, len(fields))
	for _, field := range fields {
		result[field] = true
	}
	return result
}

func nativeHasExactSet(value Object, expected map[string]bool) bool {
	if len(value) != len(expected) {
		return false
	}
	for field := range value {
		if !expected[field] {
			return false
		}
	}
	return true
}

func nativeFieldSet(fields []string, extra string) map[string]bool {
	result := nativeSliceSet(fields)
	result[extra] = true
	return result
}

func nativeFieldSetFromObject(object Object, extra string) map[string]bool {
	result := make(map[string]bool, len(object)+1)
	for field := range object {
		result[field] = true
	}
	result[extra] = true
	return result
}
