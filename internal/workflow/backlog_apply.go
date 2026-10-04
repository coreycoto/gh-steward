package workflow

import (
	"context"
	"errors"
	"fmt"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"math"
)

func sourceFacet(source contract.Object, keys ...string) contract.Object {
	out := contract.Object{}
	for _, key := range keys {
		out[key] = source[key]
	}
	return out
}

func backlogPlanSources(inventory contract.Object, request BacklogInventoryRequest) contract.Object {
	return contract.Object{
		"issues":     contract.Object{"source": "github_api", "live": true, "complete": true, "state": "all"},
		"milestones": contract.Object{"source": "github_api", "live": true, "complete": true},
		"labels":     contract.Object{"source": "github_api", "live": true, "complete": true},
		"comments": contract.Object{"source": func() string {
			if len(request.CommentMarkers) > 0 {
				return "github_api"
			}
			return "not_requested"
		}(), "live": true, "complete": true, "markers": stringSliceAny(request.CommentMarkers)},
		"relationships": contract.Object{"source": func() string {
			if request.IncludeRelationships {
				return "github_api"
			}
			return "not_requested"
		}(), "live": true, "complete": true},
		"projects": contract.Object{"source": "github_project_api", "live": true, "complete": true, "scopes": projectScopesAny(request.Projects)},
	}
}

// validation helpers for provider-produced typed values.
func finiteNumber(v any) (float64, error) {
	n, err := contract.Number(v)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, errors.New("value must be finite numeric data")
	}
	return n, nil
}

// Backlog implements reviewed backlog workflows. Operations is always
// regenerated from the authored source payload, explicit policy and captured
// complete inventory; the serialized operation array is only compared by the
// apply engine against this regenerated list.
type Backlog struct{ Provider BacklogProvider }

func (a Backlog) Operations(p contract.Plan) ([]contract.Operation, error) {
	return backlogOperations(p)
}

func backlogOperations(p contract.Plan) ([]contract.Operation, error) {
	if p.Command != ReviewBacklogCommand && p.Command != QuarterBacklogCommand && p.Command != BacklogMutationsCommand {
		return nil, errors.New("backlog adapter does not accept this command")
	}
	data := p.Data
	allowed := map[string]bool{"inventory": true, "inventory_request": true}
	if p.Command == BacklogMutationsCommand {
		allowed["intent"] = true
	} else if p.Command == ReviewBacklogCommand {
		allowed["findings"], allowed["policy"] = true, true
	} else {
		allowed["quarter_plan"] = true
	}
	for key := range data {
		if !allowed[key] {
			return nil, fmt.Errorf("backlog plan contains unsupported data field %q", key)
		}
	}
	requestRaw, err := contract.ObjectAt(data, "inventory_request")
	if err != nil {
		return nil, err
	}
	request, err := parseInventoryRequest(requestRaw)
	if err != nil {
		return nil, err
	}
	inventoryRaw, err := contract.ObjectAt(data, "inventory")
	if err != nil {
		return nil, err
	}
	inventory, err := validateStoredBacklogInventory(inventoryRaw, p.Repository, request)
	if err != nil {
		return nil, err
	}
	if !same(p.Sources, backlogPlanSources(inventory, request)) {
		return nil, errors.New("backlog plan source declarations differ from captured inventory")
	}
	if p.Command == BacklogMutationsCommand {
		return backlogMutationOperations(p, inventory, request)
	}
	if p.Command == ReviewBacklogCommand {
		return reviewBacklogOperations(p, inventory, request)
	}
	return quarterBacklogOperations(p, inventory, request)
}

func (a Backlog) ValidateReceipt(p contract.Plan, op contract.Operation, result contract.Object) error {
	if err := a.ValidateAcknowledgement(p, op, result); err != nil {
		return err
	}
	if result["after_verified"] != true {
		return errors.New("backlog receipt lacks complete verified after-state")
	}
	return nil
}

func (a Backlog) ValidateAcknowledgement(p contract.Plan, op contract.Operation, ack contract.Object) error {
	if p.Command != ReviewBacklogCommand && p.Command != QuarterBacklogCommand && p.Command != BacklogMutationsCommand {
		return errors.New("backlog acknowledgement targets an unsupported command")
	}
	ops, err := backlogOperations(p)
	if err != nil {
		return err
	}
	found := false
	for _, candidate := range ops {
		if candidate.ID == op.ID && same(candidate, op) {
			found = true
			break
		}
	}
	if !found {
		return errors.New("backlog acknowledgement primitive is outside derived plan")
	}
	allowed := map[string]bool{"kind": true, "primitive_id": true, "repository": true, "operation_id": true, "target": true, "before": true, "after": true, "acknowledged": true, "provider_result": true, "after_verified": true}
	for key := range ack {
		if !allowed[key] {
			return fmt.Errorf("backlog acknowledgement contains unsupported field %q", key)
		}
	}
	if verified, exists := ack["after_verified"]; exists && verified != true {
		return errors.New("backlog acknowledgement has an invalid after-state verification flag")
	}
	if ack["kind"] != op.Kind || ack["primitive_id"] != op.ID || ack["acknowledged"] != true || ack["operation_id"] == nil || !same(ack["target"], op.Target) || !same(ack["before"], op.Before) || !same(ack["after"], op.After) {
		return errors.New("backlog acknowledgement does not match reviewed primitive")
	}
	repository, err := contract.ObjectAt(ack, "repository")
	if err != nil {
		return err
	}
	repo, err := contract.ParseRepository(repository)
	if err != nil || repo != p.Repository {
		return errors.New("backlog acknowledgement belongs to another repository")
	}
	nonce, err := contract.Nonempty(ack, "operation_id")
	if err != nil {
		return err
	}
	if _, err = native.OperationMarker(nonce); err != nil {
		return err
	}
	result, err := contract.ObjectAt(ack, "provider_result")
	if err != nil {
		return err
	}
	return validateBacklogProviderResult(p, op, nonce, result)
}

func (a Backlog) Preflight(ctx context.Context, p contract.Plan, receipts []contract.Object) error {
	if a.Provider == nil {
		return errors.New("backlog adapter requires a provider")
	}
	request, err := backlogRequestFromPlan(p)
	if err != nil {
		return err
	}
	expected, err := backlogExpectedInventory(p, receipts)
	if err != nil {
		return err
	}
	raw, err := a.Provider.BacklogInventory(ctx, request)
	if err != nil {
		return err
	}
	actual, err := normalizeBacklogInventory(raw, p.Repository, request)
	if err != nil {
		return err
	}
	if !same(expected, actual) {
		return errors.New("complete live backlog inventory drifted from reviewed state")
	}
	return nil
}

func (a Backlog) Dispatch(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, error) {
	return a.DispatchAcknowledged(ctx, p, op, nonce, receipts, func(contract.Object) error { return nil })
}

func (a Backlog) DispatchAcknowledged(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object, persist func(contract.Object) error) (contract.Object, error) {
	if a.Provider == nil {
		return nil, errors.New("backlog adapter requires a provider")
	}
	if _, err := native.OperationMarker(nonce); err != nil {
		return nil, err
	}
	request, err := backlogRequestFromPlan(p)
	if err != nil {
		return nil, err
	}
	prior, err := backlogExpectedInventory(p, receipts)
	if err != nil {
		return nil, err
	}
	providerResult, err := a.dispatchProvider(ctx, p, op, nonce, receipts)
	if err != nil {
		return nil, err
	}
	ack := contract.Object{"kind": op.Kind, "primitive_id": op.ID, "repository": p.Repository.Object(), "operation_id": nonce, "target": op.Target, "before": op.Before, "after": op.After, "acknowledged": true, "provider_result": providerResult}
	if err = a.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, err
	}
	if persist == nil {
		return nil, errors.New("backlog acknowledgement persistence callback is required")
	}
	if err = persist(ack); err != nil {
		return nil, err
	}
	projected, err := backlogProjectAcknowledgement(prior, p, op, ack, receipts)
	if err != nil {
		return nil, fmt.Errorf("captured backlog acknowledgement could not be projected: %w", err)
	}
	raw, err := a.Provider.BacklogInventory(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("backlog write was acknowledged but after-state read failed: %w", err)
	}
	actual, err := normalizeBacklogInventory(raw, p.Repository, request)
	if err != nil {
		return nil, fmt.Errorf("backlog write was acknowledged but after-state is invalid: %w", err)
	}
	if !same(projected, actual) {
		return nil, errors.New("backlog write was acknowledged but complete after-state differs from primitive")
	}
	ack["after_verified"] = true
	return ack, nil
}

func (a Backlog) Observe(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, contract.Object, error) {
	if a.Provider == nil {
		return nil, nil, errors.New("backlog adapter requires a provider")
	}
	var ack contract.Object
	for _, receipt := range receipts {
		if receipt["id"] == op.ID && receipt["status"] == "unknown" && receipt["operation_id"] == nonce {
			if raw, ok := receipt["acknowledgement"].(map[string]any); ok {
				ack = raw
			}
		}
	}
	if ack == nil || ack["operation_id"] != nonce {
		return nil, nil, errors.New("ambiguous backlog dispatch has no durable native acknowledgement; no write was retried")
	}
	if err := a.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, nil, err
	}
	prior, err := backlogExpectedInventory(p, receipts)
	if err != nil {
		return nil, nil, err
	}
	projected, err := backlogProjectAcknowledgement(prior, p, op, ack, backlogCompletedPrefix(receipts, op.ID))
	if err != nil {
		return nil, nil, err
	}
	request, err := backlogRequestFromPlan(p)
	if err != nil {
		return nil, nil, err
	}
	raw, err := a.Provider.BacklogInventory(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	actual, err := normalizeBacklogInventory(raw, p.Repository, request)
	if err != nil {
		return nil, nil, err
	}
	if !same(projected, actual) {
		return nil, nil, errors.New("acknowledged backlog operation does not match complete live after-state")
	}
	result, err := contract.Clone(ack)
	if err != nil {
		return nil, nil, err
	}
	result["after_verified"] = true
	digest, err := contract.Digest(ack)
	if err != nil {
		return nil, nil, err
	}
	return result, contract.Object{"positive_identity": true, "after_state_verified": true, "operation_id": nonce, "reference": "journal:native-acknowledgement:" + digest}, nil
}

func backlogRequestFromPlan(p contract.Plan) (BacklogInventoryRequest, error) {
	requestRaw, err := contract.ObjectAt(p.Data, "inventory_request")
	if err != nil {
		return BacklogInventoryRequest{}, err
	}
	return parseInventoryRequest(requestRaw)
}

func backlogExpectedInventory(p contract.Plan, receipts []contract.Object) (contract.Object, error) {
	request, err := backlogRequestFromPlan(p)
	if err != nil {
		return nil, err
	}
	raw, err := contract.ObjectAt(p.Data, "inventory")
	if err != nil {
		return nil, err
	}
	expected, err := validateStoredBacklogInventory(raw, p.Repository, request)
	if err != nil {
		return nil, err
	}
	ops, err := backlogOperations(p)
	if err != nil {
		return nil, err
	}
	byID := map[string]contract.Operation{}
	for _, op := range ops {
		byID[op.ID] = op
	}
	prefix := []contract.Object{}
	for _, receipt := range receipts {
		if receipt["status"] != "completed" {
			continue
		}
		id, err := contract.Nonempty(receipt, "id")
		if err != nil {
			return nil, err
		}
		op, ok := byID[id]
		if !ok {
			return nil, errors.New("backlog journal receipt is outside derived primitive plan")
		}
		result, err := contract.ObjectAt(receipt, "result")
		if err != nil {
			return nil, err
		}
		if err = (&Backlog{}).ValidateReceipt(p, op, result); err != nil {
			return nil, err
		}
		if receipt["operation_id"] != result["operation_id"] {
			return nil, errors.New("backlog completion does not match its durable dispatch identity")
		}
		expected, err = backlogProjectAcknowledgement(expected, p, op, result, prefix)
		if err != nil {
			return nil, err
		}
		prefix = append(prefix, receipt)
	}
	return expected, nil
}
