package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

type nativeBridgeExecutor struct {
	responses []contract.Object
	inputs    []contract.Object
	args      [][]string
}

func (f *nativeBridgeExecutor) Execute(_ context.Context, _ string, args []string, input []byte, _ string, _ []string) (native.Result, error) {
	f.args = append(f.args, args)
	if len(input) > 0 {
		o, err := contract.Decode(strings.NewReader(string(input)))
		if err != nil {
			return native.Result{}, err
		}
		f.inputs = append(f.inputs, o)
	}
	if len(f.responses) == 0 {
		return native.Result{}, errors.New("unexpected native bridge call")
	}
	o := f.responses[0]
	f.responses = f.responses[1:]
	body, err := contract.Canonical(o)
	return native.Result{Stdout: body}, err
}

func bridgeTransport(f native.Executor) *native.Transport {
	return &native.Transport{Root: ".", Repository: testRepo(), Executable: "injected-gh", Executor: f, Environment: []string{}, Timeout: time.Second}
}

func bridgeIssue(body string) contract.Object {
	return contract.Object{"number": int64(17), "id": int64(10017), "node_id": "I_17", "html_url": "https://github.com/example/widgets/issues/17", "title": "Fix case", "body": body, "state": "open", "labels": []any{}, "milestone": nil, "provider_extra": "retain"}
}

func TestNativeBacklogBridgeRetainsProviderACKAndUsesTypedPatch(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	marker, _ := native.OperationMarker(nonce)
	f := &nativeBridgeExecutor{responses: []contract.Object{bridgeIssue("Reviewed\n\n" + marker)}}
	n := NativeBacklog{Transport: bridgeTransport(f)}
	ack, err := n.CreateIssue(context.Background(), nonce, IssueDraft{Title: "Fix case", Body: "Reviewed", Labels: []string{"bug"}})
	if err != nil || ack["provider_extra"] != "retain" || f.inputs[0]["body"] != "Reviewed\n\n"+marker {
		t.Fatal(ack, err, f.inputs)
	}
	labels := []string{"task", "reviewed"}
	title := "Updated case"
	f = &nativeBridgeExecutor{responses: []contract.Object{bridgeIssue("old"), bridgeIssue("new")}}
	n.Transport = bridgeTransport(f)
	ack, err = n.UpdateIssue(context.Background(), nonce, 17, IssuePatch{Title: &title, Labels: &labels, ClearMilestone: true})
	if err != nil || ack["provider_extra"] != "retain" || len(f.inputs) != 1 {
		t.Fatal(ack, err)
	}
	patch := f.inputs[0]
	if patch["title"] != title || patch["milestone"] != nil || len(patch) != 3 || len(patch["labels"].([]any)) != 2 {
		t.Fatal(patch)
	}
	if f.args[1][1] != "repos/example/widgets/issues/17" {
		t.Fatal(f.args)
	}
	if _, err = n.UpdateIssue(context.Background(), nonce, 17, IssuePatch{ClearMilestone: true, MilestoneNumber: new(int64)}); err == nil || len(f.args) != 2 {
		t.Fatal("conflicting patch reached transport", err)
	}
}

func TestNativeBacklogRejectsMissingTransportOrUnreviewedProjectBeforeIO(t *testing.T) {
	n := NativeBacklog{}
	nonce := strings.Repeat("a", 32)
	for _, call := range []func() (contract.Object, error){
		func() (contract.Object, error) { return n.CreateIssue(context.Background(), nonce, IssueDraft{}) },
		func() (contract.Object, error) { return n.UpdateIssue(context.Background(), nonce, 17, IssuePatch{}) },
		func() (contract.Object, error) {
			return n.CreateMilestone(context.Background(), nonce, MilestoneDraft{})
		},
		func() (contract.Object, error) {
			return n.UpdateMilestone(context.Background(), nonce, 1, MilestonePatch{})
		},
		func() (contract.Object, error) { return n.CreateIssueComment(context.Background(), nonce, 17, "body") },
		func() (contract.Object, error) {
			return n.UpdateIssueComment(context.Background(), nonce, 17, 1, "body")
		},
		func() (contract.Object, error) { return n.AddProjectIssue(context.Background(), nonce, "P_1", "I_17") },
		func() (contract.Object, error) {
			return n.SetProjectField(context.Background(), nonce, "P_1", "M_17", ProjectStatusField, ProjectFieldValue{Text: stringPtr("Active")})
		},
		func() (contract.Object, error) { return n.AddRelationship(context.Background(), nonce, 17, 23, Child) },
		func() (contract.Object, error) {
			return n.RemoveRelationship(context.Background(), nonce, 17, 23, BlockedBy)
		},
	} {
		if _, err := call(); err == nil {
			t.Fatal("missing transport accepted")
		}
	}
	f := &nativeBridgeExecutor{}
	n.Transport = bridgeTransport(f)
	if _, err := n.AddProjectIssue(context.Background(), nonce, "P_unreviewed", "I_17"); err == nil || len(f.args) != 0 {
		t.Fatal("unreviewed Project reached provider", err)
	}
	if _, err := n.CreateIssueComment(context.Background(), "bad", 17, "body"); err == nil || len(f.args) != 0 {
		t.Fatal("uncorrelated comment reached provider", err)
	}
}
