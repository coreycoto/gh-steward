package workflow

import (
	"context"
	"errors"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// NativeGovernance adds explicit label and issue-state primitives to the
// same complete backlog inventory and Project boundary used by reviewed plans.
type NativeGovernance struct {
	NativeBacklog
}

func (n NativeGovernance) CreateLabel(ctx context.Context, nonce string, draft LabelDraft) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	return n.Transport.CreateLabel(ctx, contract.Object{"name": draft.Name, "color": draft.Color, "description": draft.Description}, nonce)
}

func (n NativeGovernance) UpdateLabel(ctx context.Context, nonce, nodeID, name, color, description string) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	return n.Transport.UpdateLabel(ctx, nodeID, contract.Object{"name": name, "color": color, "description": description}, nonce)
}

func (n NativeGovernance) SetIssueState(ctx context.Context, nonce string, number int64, state string) (contract.Object, error) {
	if err := n.validateMutation(nonce); err != nil {
		return nil, err
	}
	switch state {
	case "OPEN":
		return n.Transport.ReopenExecutionIssue(ctx, nonce, number)
	case "CLOSED":
		return n.Transport.CloseExecutionIssue(ctx, nonce, number)
	default:
		return nil, errors.New("governance issue state must be exactly OPEN or CLOSED")
	}
}
