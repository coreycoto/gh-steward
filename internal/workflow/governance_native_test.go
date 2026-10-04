package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestNativeGovernanceStatePrimitiveKeepsCallerStateAndProviderReceipt(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	for _, state := range []string{"OPEN", "CLOSED"} {
		after := bridgeIssue("reviewed")
		after["state"] = strings.ToLower(state)
		f := &nativeBridgeExecutor{responses: []contract.Object{bridgeIssue("reviewed"), after}}
		n := NativeGovernance{NativeBacklog: NativeBacklog{Transport: bridgeTransport(f)}}
		ack, err := n.SetIssueState(context.Background(), nonce, 17, state)
		if err != nil || ack["provider_extra"] != "retain" || ack["state"] != strings.ToLower(state) {
			t.Fatal("typed state or raw acknowledgement changed", ack, err)
		}
		reason := "completed"
		if state == "OPEN" {
			reason = "reopened"
		}
		if len(f.inputs) != 1 || len(f.inputs[0]) != 2 || f.inputs[0]["state"] != strings.ToLower(state) || f.inputs[0]["state_reason"] != reason {
			t.Fatal("governance primitive broadened the issue patch", f.inputs)
		}
	}
}

func TestNativeGovernanceLabelBridgeKeepsImmutableScopeAndMutationCorrelation(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	for _, update := range []bool{false, true} {
		repository := contract.Object{"id": "R_1", "nameWithOwner": "example/widgets", "url": "https://github.com/example/widgets"}
		label := contract.Object{"id": "LA_1", "name": "bug", "color": "a1b2c3", "description": "Defect", "repository": repository}
		prior := contract.Object{"id": "R_1", "nameWithOwner": "example/widgets", "url": "https://github.com/example/widgets"}
		key := "createLabel"
		if update {
			prior["label"], key = label, "updateLabel"
		}
		payload := contract.Object{"label": label, "clientMutationId": nonce, "provider_extra": "retain"}
		f := &nativeBridgeExecutor{responses: []contract.Object{
			{"data": contract.Object{"repository": prior}}, {"data": contract.Object{key: payload}},
		}}
		n := NativeGovernance{NativeBacklog: NativeBacklog{Transport: bridgeTransport(f)}}
		var ack contract.Object
		var err error
		if update {
			ack, err = n.UpdateLabel(context.Background(), nonce, "LA_1", "bug", "a1b2c3", "Defect")
		} else {
			ack, err = n.CreateLabel(context.Background(), nonce, LabelDraft{Name: "bug", Color: "a1b2c3", Description: "Defect"})
		}
		if err != nil || ack["clientMutationId"] != nonce || ack["provider_extra"] != "retain" {
			t.Fatal("native label proof changed at the workflow seam", ack, err)
		}
	}
}

func TestNativeGovernanceRejectsMissingTransportCorrelationAndUnknownStateBeforeIO(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	for _, absent := range []bool{false, true} {
		f := &nativeBridgeExecutor{}
		n := NativeGovernance{NativeBacklog: NativeBacklog{Transport: bridgeTransport(f)}}
		usedNonce := "invalid"
		if absent {
			n.Transport, usedNonce = nil, nonce
		}
		for _, call := range []func() (contract.Object, error){
			func() (contract.Object, error) { return n.CreateLabel(context.Background(), usedNonce, LabelDraft{}) },
			func() (contract.Object, error) {
				return n.UpdateLabel(context.Background(), usedNonce, "LA_1", "bug", "a1b2c3", "Defect")
			},
			func() (contract.Object, error) { return n.SetIssueState(context.Background(), usedNonce, 17, "OPEN") },
		} {
			if _, err := call(); err == nil {
				t.Fatal("missing native scope/correlation accepted")
			}
		}
		if len(f.args) != 0 {
			t.Fatal("unqualified mutation reached native gh", f.args)
		}
	}
	f := &nativeBridgeExecutor{}
	n := NativeGovernance{NativeBacklog: NativeBacklog{Transport: bridgeTransport(f)}}
	for _, state := range []string{"open", "closed", "ARCHIVED", "", "CLOSED\nOPEN"} {
		if _, err := n.SetIssueState(context.Background(), nonce, 17, state); err == nil {
			t.Fatal("unsupported state dispatched", state)
		}
	}
	if len(f.args) != 0 {
		t.Fatal("invalid state reached native gh", f.args)
	}
}
