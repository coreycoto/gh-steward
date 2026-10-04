package runrecovery

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestTerminalPlanProofAcceptsZeroOperationV2Result(t *testing.T) {
	plan, proof := makeNativeTerminalProof(t, nil, nativeAckAcknowledged)
	decoded, err := ValidateTerminalPlanProof(proof)
	if err != nil {
		t.Fatalf("valid zero-operation terminal proof rejected: %v", err)
	}
	if !Equal(decoded, plan.Object()) {
		t.Fatalf("decoded plan differs from reviewed plan: %#v", decoded)
	}
}

func TestTerminalPlanProofPreservesAcknowledgementAndObservationEvidence(t *testing.T) {
	op := nativeTestOperation("first", "branch-delete")
	for _, mode := range []nativeAckMode{nativeAckAcknowledged, nativeAckBranchDeletion, nativeAckObservation} {
		t.Run(string(mode), func(t *testing.T) {
			_, proof := makeNativeTerminalProof(t, []contract.Operation{op}, mode)
			if _, err := ValidateTerminalPlanProof(proof); err != nil {
				t.Fatalf("valid %s terminal receipt rejected: %v", mode, err)
			}
		})
	}
}

func TestTerminalPlanProofRejectsDuplicateDispatchAndChangedIntent(t *testing.T) {
	first := nativeTestOperation("first", "branch-delete")
	second := nativeTestOperation("second", "branch-delete")
	_, proof := makeNativeTerminalProof(t, []contract.Operation{first, second}, nativeAckAcknowledged)
	journalValue, err := LoadFileProof(proof["journal_file"], "journal")
	if err != nil {
		t.Fatal(err)
	}
	journal := journalValue.(Object)
	steps := journal["steps"].([]any)
	steps[1].(Object)["operation_id"] = steps[0].(Object)["operation_id"]
	steps[1].(Object)["result"].(Object)["operation_id"] = steps[0].(Object)["operation_id"]
	steps[1].(Object)["acknowledgement"].(Object)["operation_id"] = steps[0].(Object)["operation_id"]
	proof["journal_file"] = nativeJSONFileProof(t, journal)
	if _, err := ValidateTerminalPlanProof(proof); err == nil {
		t.Fatal("duplicate durable dispatch identity was accepted")
	}

	_, changed := makeNativeTerminalProof(t, []contract.Operation{first}, nativeAckAcknowledged)
	planBytes, err := decodePublicationRawProof(changed["plan_file"], "plan")
	if err != nil {
		t.Fatal(err)
	}
	planValue, err := LoadFileProof(changed["plan_file"], "plan")
	if err != nil {
		t.Fatal(err)
	}
	planObject := planValue.(Object)
	planObject["data"].(Object)["scope"] = "changed"
	changed["plan_file"] = nativeJSONFileProof(t, planObject)
	_ = planBytes
	if _, err := ValidateTerminalPlanProof(changed); err == nil {
		t.Fatal("plan whose semantic digest no longer matches was accepted")
	}
}

func TestTerminalPlanProofRejectsDuplicateJSONLexicalIntegerAndBadProofHash(t *testing.T) {
	_, baseline := makeNativeTerminalProof(t, nil, nativeAckAcknowledged)
	raw, err := decodePublicationRawProof(baseline["plan_file"], "plan")
	if err != nil {
		t.Fatal(err)
	}
	duplicate := strings.Replace(string(raw), `"schema_version":2`, `"schema_version":2,"schema_version":2`, 1)
	proof := cloneNativeProof(baseline)
	proof["plan_file"] = MakeFileProof([]byte(duplicate))
	if _, err := ValidateTerminalPlanProof(proof); err == nil {
		t.Fatal("duplicate plan key was accepted")
	}

	lexical := strings.Replace(string(raw), `"schema_version":2`, `"schema_version":2.0`, 1)
	proof = cloneNativeProof(baseline)
	proof["plan_file"] = MakeFileProof([]byte(lexical))
	if _, err := ValidateTerminalPlanProof(proof); err == nil {
		t.Fatal("fractional spelling of schema version was accepted")
	}

	proof = cloneNativeProof(baseline)
	proof["plan_sha256"] = strings.Repeat("0", 64)
	if _, err := ValidateTerminalPlanProof(proof); err == nil {
		t.Fatal("proof with a changed exact plan digest was accepted")
	}

	_, withOperation := makeNativeTerminalProof(t, []contract.Operation{nativeTestOperation("first", "branch-delete")}, nativeAckAcknowledged)
	operationRaw, err := decodePublicationRawProof(withOperation["plan_file"], "plan")
	if err != nil {
		t.Fatal(err)
	}
	numericID := strings.Replace(string(operationRaw), `"id":"first"`, `"id":1.0`, 1)
	withOperation["plan_file"] = MakeFileProof([]byte(numericID))
	if _, err := ValidateTerminalPlanProof(withOperation); err == nil {
		t.Fatal("numeric JSON token was accepted as a native string operation identity")
	}
}

func TestTerminalPlanProofRetainsReviewedNumericLexemes(t *testing.T) {
	repository, err := contract.ParseRepository(Object{"owner": "sample", "name": "repo", "url": "https://github.com/sample/repo"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contract.PreparePlan(
		"branch-delete", repository,
		Object{"github": Object{"live": true, "complete": true}},
		Object{"queue_order": json.Number("2.50")}, nil,
		time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, proof := makeNativeTerminalProofFromPlan(t, plan, nativeAckAcknowledged)
	decoded, err := ValidateTerminalPlanProof(proof)
	if err != nil {
		t.Fatalf("valid numeric data plan rejected: %v", err)
	}
	if got := decoded["data"].(Object)["queue_order"]; got != json.Number("2.50") {
		t.Fatalf("numeric lexeme changed: got %#v", got)
	}
}

type nativeAckMode string

const (
	nativeAckAcknowledged   nativeAckMode = "provider-result"
	nativeAckBranchDeletion nativeAckMode = "branch-provider-ack"
	nativeAckObservation    nativeAckMode = "positive-observation"
)

func makeNativeTerminalProof(t *testing.T, operations []contract.Operation, mode nativeAckMode) (contract.Plan, Object) {
	t.Helper()
	repository, err := contract.ParseRepository(Object{"owner": "sample", "name": "repo", "url": "https://github.com/sample/repo"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contract.PreparePlan(
		"branch-delete", repository,
		Object{"github": Object{"live": true, "complete": true}},
		Object{"scope": "test"}, operations, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	return makeNativeTerminalProofFromPlan(t, plan, mode)
}

func makeNativeTerminalProofFromPlan(t *testing.T, plan contract.Plan, mode nativeAckMode) (contract.Plan, Object) {
	t.Helper()
	operations := plan.Operations
	planObject := plan.Object()
	repoObject := plan.Repository.Object()
	steps := make([]any, 0, len(operations))
	for index, op := range operations {
		operation := Object{"id": op.ID, "kind": op.Kind, "target": op.Target, "before": op.Before, "after": op.After}
		operationID := strings.Repeat(string(rune('a'+index)), 32)
		if mode == nativeAckBranchDeletion {
			operationID = strings.Repeat(string(rune('a'+index)), 32)
		}
		step := Object{
			"id": op.ID, "intent": operation,
			"intent_sha256": nativeDigestOf(t, operation), "operation_id": operationID,
			"status": "completed", "started_at": "2026-10-04T12:01:00Z", "completed_at": "2026-10-04T12:02:00Z",
		}
		ack := Object{
			"kind": op.Kind, "primitive_id": op.ID, "repository": repoObject,
			"operation_id": operationID, "target": op.Target,
		}
		var result Object
		switch mode {
		case nativeAckAcknowledged:
			ack["before"], ack["after"] = op.Before, op.After
			ack["acknowledged"], ack["provider_result"] = true, Object{}
			result = cloneNativeObject(ack)
			result["after_verified"] = true
			step["acknowledgement"] = ack
		case nativeAckBranchDeletion:
			branch := op.Target["name"].(string)
			sha := op.Target["sha"].(string)
			ack["provider_ack"] = Object{
				"exit_code": 0, "stdout": "To https://github.com/sample/repo.git\n-\t:refs/heads/" + branch + "\t[deleted]\nDone\n",
				"stderr": "", "remote_url": "https://github.com/sample/repo.git", "ref": "refs/heads/" + branch, "expected_sha": sha,
			}
			result = cloneNativeObject(ack)
			result["observed_inventory_sha256"] = strings.Repeat("b", 64)
			step["acknowledgement"] = ack
		case nativeAckObservation:
			result = Object{"operation_id": operationID, "after_verified": true}
			step["observation"] = Object{
				"positive_identity": true, "after_state_verified": true,
				"operation_id": operationID, "reference": "refs/heads/reviewed",
			}
		default:
			t.Fatalf("unknown test receipt mode %q", mode)
		}
		step["result"] = result
		steps = append(steps, step)
	}
	terminal := Object{
		"status": "completed", "command": plan.Command, "repository": repoObject,
		"plan_sha256": plan.SHA256, "receipts": steps,
	}
	identity := Object{"schema_version": 2, "repository": repoObject, "command": plan.Command, "plan_sha256": plan.SHA256}
	identityDigest := nativeDigestOf(t, identity)
	journal := Object{"identity": identity, "steps": steps, "result": terminal}
	applyResult := Object{"schema_version": 2, "tool_version": "0.1.0", "command": plan.Command, "repository": repoObject, "data": terminal}
	proof := Object{
		"name": "branch-deletion", "command": plan.Command, "plan_sha256": plan.SHA256, "journal_id": identityDigest,
		"plan_file": nativeJSONFileProof(t, planObject), "journal_file": nativeJSONFileProof(t, journal),
		"apply_result_file": nativeJSONFileProof(t, applyResult),
	}
	return plan, proof
}

func nativeTestOperation(id, kind string) contract.Operation {
	sha := strings.Repeat("c", 40)
	target := Object{"name": "reviewed-branch", "sha": sha}
	before := Object{"present": true}
	after := Object{"present": false}
	return contract.Operation{ID: id, Kind: kind, Target: target, Before: before, After: after}
}

func nativeJSONFileProof(t *testing.T, value any) Object {
	t.Helper()
	data, err := Canonical(value)
	if err != nil {
		t.Fatal(err)
	}
	return MakeFileProof(data)
}

func nativeDigestOf(t *testing.T, value any) string {
	t.Helper()
	data, err := Canonical(value)
	if err != nil {
		t.Fatal(err)
	}
	return SHA256(data)
}

func cloneNativeObject(value Object) Object {
	result := make(Object, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func cloneNativeProof(value Object) Object { return cloneNativeObject(value) }
