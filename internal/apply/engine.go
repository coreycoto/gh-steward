// Package apply coordinates reviewed domain primitives. Domain adapters own
// full-inventory preconditions, receipt validation and positive observation;
// this layer guarantees durable intent before dispatch and never retries an
// ambiguous write. It does not grant authorization to a caller.
package apply

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/progress"
)

type Adapter interface {
	// Operations recomputes the exact primitive list from reviewed domain data.
	Operations(contract.Plan) ([]contract.Operation, error)
	// ValidateReceipt checks identity, intent and the typed provider completion.
	ValidateReceipt(contract.Plan, contract.Operation, contract.Object) error
	// Preflight compares all reviewed source inventories, projected through only
	// validated completed receipts, with fresh complete live reads.
	Preflight(context.Context, contract.Plan, []contract.Object) error
	Dispatch(context.Context, contract.Plan, contract.Operation, string, []contract.Object) (contract.Object, error)
	// Observe is positive target+operation identity evidence. Absence or a
	// matching title/value alone is not proof that a write completed.
	Observe(context.Context, contract.Plan, contract.Operation, string, []contract.Object) (contract.Object, contract.Object, error)
}

// AcknowledgingAdapter persists a typed successful native response before the
// independent after-state read. This is optional: an adapter without captured
// acknowledgement must positively reconcile ambiguous writes by other proof.
type AcknowledgingAdapter interface {
	ValidateAcknowledgement(contract.Plan, contract.Operation, contract.Object) error
	DispatchAcknowledged(context.Context, contract.Plan, contract.Operation, string, []contract.Object, func(contract.Object) error) (contract.Object, error)
}

type Engine struct {
	Root       string
	Repository contract.Repository
	Command    string
	Adapter    Adapter
}

func operationIntent(op contract.Operation) contract.Object {
	return contract.Object{"id": op.ID, "kind": op.Kind, "target": op.Target, "before": op.Before, "after": op.After}
}

func (e Engine) Apply(ctx context.Context, raw contract.Object) (contract.Object, error) {
	if e.Adapter == nil {
		return nil, errors.New("apply requires a domain adapter")
	}
	plan, err := contract.ParsePlan(raw)
	if err != nil {
		return nil, err
	}
	ops, err := e.Adapter.Operations(plan)
	if err != nil {
		return nil, err
	}
	if err = plan.VerifyTarget(e.Command, e.Repository, ops); err != nil {
		return nil, err
	}
	j, err := progress.Open(e.Root, e.Repository, e.Command, plan.SHA256)
	if err != nil {
		return nil, err
	}
	defer j.Close()
	byID := map[string]contract.Operation{}
	ids := []string{}
	for _, op := range ops {
		byID[op.ID] = op
		ids = append(ids, op.ID)
		if err = j.ValidateIntent(op.ID, operationIntent(op)); err != nil {
			return nil, err
		}
	}
	validate := func() ([]contract.Object, error) {
		receipts, err := j.Receipts()
		if err != nil {
			return nil, err
		}
		if len(receipts) > len(ops) {
			return nil, errors.New("journal exceeds reviewed primitive inventory")
		}
		for index, receipt := range receipts {
			id, err := contract.Nonempty(receipt, "id")
			if err != nil {
				return nil, err
			}
			op, exists := byID[id]
			if !exists || id != ops[index].ID {
				return nil, errors.New("journal has a primitive outside this reviewed domain plan")
			}
			if receipt["status"] == "completed" {
				result, err := contract.ObjectAt(receipt, "result")
				if err != nil {
					return nil, err
				}
				if result["operation_id"] != receipt["operation_id"] {
					return nil, errors.New("completion result does not match durable dispatch identity")
				}
				if err = e.Adapter.ValidateReceipt(plan, op, result); err != nil {
					return nil, err
				}
			}
			if raw, exists := receipt["acknowledgement"]; exists {
				adapter, ok := e.Adapter.(AcknowledgingAdapter)
				ack, typed := raw.(map[string]any)
				if !ok || !typed || ack["operation_id"] != receipt["operation_id"] {
					return nil, errors.New("journal acknowledgement is not valid for this domain adapter")
				}
				if err := adapter.ValidateAcknowledgement(plan, op, ack); err != nil {
					return nil, err
				}
			}
		}
		return receipts, nil
	}
	// Reconcile every unknown receipt before checking the projected inventory.
	// A later primitive may already have taken effect while earlier receipts
	// are complete; preflighting the earlier prefix would misclassify that
	// accepted write as external drift. Observation performs no write.
	for _, op := range ops {
		receipts, err := validate()
		if err != nil {
			return nil, err
		}
		if j.Status(op.ID) == "unknown" {
			result, evidence, err := e.Adapter.Observe(ctx, plan, op, j.OperationID(op.ID), receipts)
			if err != nil {
				return nil, err
			}
			if result["operation_id"] != j.OperationID(op.ID) {
				return nil, errors.New("observation result has another operation identity")
			}
			if err = e.Adapter.ValidateReceipt(plan, op, result); err != nil {
				return nil, err
			}
			if err = j.Observe(op.ID, operationIntent(op), result, evidence); err != nil {
				return nil, err
			}
			receipts, err = validate()
			if err != nil {
				return nil, err
			}
		}
	}
	for _, op := range ops {
		receipts, err := validate()
		if err != nil {
			return nil, err
		}
		// Completed writes still participate in full-inventory validation. They
		// never dispatch again, including a replay after terminal completion.
		if err = e.Adapter.Preflight(ctx, plan, receipts); err != nil {
			return nil, err
		}
		_, err = j.Execute(op.ID, operationIntent(op), func(operationID string) (contract.Object, error) {
			var result contract.Object
			var err error
			if adapter, ok := e.Adapter.(AcknowledgingAdapter); ok {
				result, err = adapter.DispatchAcknowledged(ctx, plan, op, operationID, receipts, func(ack contract.Object) error {
					if ack["operation_id"] != operationID {
						return errors.New("native acknowledgement has another operation identity")
					}
					if err := adapter.ValidateAcknowledgement(plan, op, ack); err != nil {
						return err
					}
					return j.Acknowledge(op.ID, operationIntent(op), ack)
				})
			} else {
				result, err = e.Adapter.Dispatch(ctx, plan, op, operationID, receipts)
			}
			if err != nil {
				return nil, err
			}
			if result["operation_id"] != operationID {
				return nil, errors.New("provider result has another operation identity")
			}
			if err = e.Adapter.ValidateReceipt(plan, op, result); err != nil {
				return nil, fmt.Errorf("provider result was not a valid completion: %w", err)
			}
			return result, nil
		})
		if err != nil {
			return nil, err
		}
	}
	receipts, err := validate()
	if err != nil {
		return nil, err
	}
	if err = e.Adapter.Preflight(ctx, plan, receipts); err != nil {
		return nil, err
	}
	result := contract.Object{"status": "completed", "command": e.Command, "repository": e.Repository.Object(), "plan_sha256": plan.SHA256, "receipts": objects(receipts)}
	if err = j.Finish(result, ids); err != nil {
		return nil, err
	}
	return result, nil
}

func objects(receipts []contract.Object) []any {
	out := make([]any, 0, len(receipts))
	for _, r := range receipts {
		out = append(out, r)
	}
	return out
}
