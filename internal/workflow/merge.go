package workflow

import (
	"context"
	"errors"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/governance"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/snapshot"
)

const MergeCommand = "merge-apply"

type MergeProvider interface {
	MergeInventory(context.Context, contract.Object) (contract.Object, error)
	MergePullRequest(context.Context, int64, string, string) (contract.Object, error)
}

type NativeMerge struct{ Transport *native.Transport }

func (n NativeMerge) MergeInventory(ctx context.Context, event contract.Object) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("native merge requires a transport")
	}
	return snapshot.MergeInventory(ctx, n.Transport, n.Transport.Repository, event)
}
func (n NativeMerge) MergePullRequest(ctx context.Context, number int64, sha, method string) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("native merge requires a transport")
	}
	return n.Transport.MergePullRequest(ctx, number, sha, method)
}

func PrepareMerge(ctx context.Context, provider MergeProvider, repo contract.Repository, event, policy contract.Object, now time.Time) (contract.Plan, error) {
	if provider == nil {
		return contract.Plan{}, errors.New("merge preparation requires a provider")
	}
	trigger, err := snapshot.NormalizeMergeTrigger(event, repo)
	if err != nil {
		return contract.Plan{}, err
	}
	inventory, err := provider.MergeInventory(ctx, trigger)
	if err != nil {
		return contract.Plan{}, err
	}
	data := contract.Object{"event": trigger, "policy": policy, "inventory": inventory}
	ops, err := mergeOperations(contract.Plan{Command: MergeCommand, Repository: repo, Data: data})
	if err != nil {
		return contract.Plan{}, err
	}
	return contract.PreparePlan(MergeCommand, repo, contract.Object{"candidate": contract.Object{"source": "github_api", "live": true, "complete": true}}, data, ops, now)
}

func mergeOperations(p contract.Plan) ([]contract.Operation, error) {
	if p.Command != MergeCommand {
		return nil, errors.New("merge adapter rejects this command")
	}
	inventory, err := contract.ObjectAt(p.Data, "inventory")
	if err != nil {
		return nil, err
	}
	provenance, err := contract.ObjectAt(inventory, "provenance")
	if err != nil || provenance["live"] != true || provenance["complete"] != true || provenance["source"] != "github_api" || provenance["host"] != p.Repository.Host {
		return nil, errors.New("merge requires complete live evidence for the selected host")
	}
	r, err := contract.ObjectAt(inventory, "repository")
	if err != nil || r["nameWithOwner"] != p.Repository.FullName() {
		return nil, errors.New("merge inventory repository differs from the reviewed target")
	}
	if _, err := contract.Nonempty(r, "id"); err != nil {
		return nil, err
	}
	event, err := contract.ObjectAt(p.Data, "event")
	if err != nil || !same(event, inventory["event"]) {
		return nil, errors.New("merge inventory trigger differs from the reviewed event")
	}
	if _, err := snapshot.NormalizeMergeTrigger(event, p.Repository); err != nil {
		return nil, err
	}
	policy, err := contract.ObjectAt(p.Data, "policy")
	if err != nil {
		return nil, err
	}
	gate, err := governance.MergeEligibility(policy, inventory)
	if err != nil {
		return nil, err
	}
	if gate["status"] != "eligible" {
		return nil, errors.New("current candidate did not pass the deterministic merge gate")
	}
	number, err := contract.PositiveInteger(gate["pr_number"])
	if err != nil {
		return nil, err
	}
	pr, err := contract.ObjectAt(inventory, "pull_request")
	if err != nil {
		return nil, err
	}
	if pr["merged"] != false || pr["merge_commit_sha"] != nil {
		return nil, errors.New("merge preparation requires an unmerged candidate")
	}
	nodeID, err := contract.Nonempty(pr, "id")
	if err != nil {
		return nil, err
	}
	if err := (&native.Transport{Repository: p.Repository}).ValidatePullRequestURL(pr["url"], number); err != nil {
		return nil, err
	}
	headSHA, err := native.CommitOID(gate["expected_head_sha"])
	if err != nil {
		return nil, err
	}
	method, err := contract.Nonempty(gate, "merge_method")
	if err != nil || (method != "merge" && method != "squash" && method != "rebase") {
		return nil, errors.New("reviewed merge method is invalid")
	}
	return []contract.Operation{{ID: "merge-000001", Kind: "pull-request-merge", Target: contract.Object{"number": number, "node_id": nodeID, "url": pr["url"], "head_sha": headSHA, "merge_method": method}, Before: contract.Object{"merged": false}, After: contract.Object{"merged": true}}}, nil
}

type MergeAdapter struct{ Provider MergeProvider }

func (m MergeAdapter) Operations(p contract.Plan) ([]contract.Operation, error) {
	return mergeOperations(p)
}

func mergeExpected(p contract.Plan, providerACK contract.Object) (contract.Object, error) {
	baseline, err := contract.ObjectAt(p.Data, "inventory")
	if err != nil {
		return nil, err
	}
	projected, err := contract.Clone(baseline)
	if err != nil {
		return nil, err
	}
	if providerACK != nil {
		pr, err := contract.ObjectAt(projected, "pull_request")
		if err != nil {
			return nil, err
		}
		pr["state"], pr["merged"], pr["mergeStateStatus"], pr["merge_commit_sha"] = "MERGED", true, "MERGED", providerACK["sha"]
	}
	return projected, nil
}

func (m MergeAdapter) ValidateAcknowledgement(p contract.Plan, op contract.Operation, ack contract.Object) error {
	ops, err := mergeOperations(p)
	if err != nil || len(ops) != 1 || !same(ops[0], op) {
		return errors.New("merge acknowledgement is outside the reviewed primitive")
	}
	nonce, err := contract.Nonempty(ack, "operation_id")
	if err != nil {
		return err
	}
	if _, err := native.OperationMarker(nonce); err != nil {
		return err
	}
	if ack["command"] != MergeCommand || !same(ack["repository"], p.Repository.Object()) || !same(ack["target"], op.Target) {
		return errors.New("merge acknowledgement has another repository, head or dispatch target")
	}
	providerACK, err := contract.ObjectAt(ack, "provider_ack")
	if err != nil || providerACK["merged"] != true {
		return errors.New("merge acknowledgement did not positively identify completion")
	}
	_, err = native.CommitOID(providerACK["sha"])
	return err
}

func (m MergeAdapter) ValidateReceipt(p contract.Plan, op contract.Operation, result contract.Object) error {
	if err := m.ValidateAcknowledgement(p, op, result); err != nil {
		return err
	}
	ack, _ := contract.ObjectAt(result, "provider_ack")
	expected, err := mergeExpected(p, ack)
	if err != nil {
		return err
	}
	digest, err := contract.Digest(expected)
	if err != nil || result["observed_inventory_sha256"] != digest {
		return errors.New("merge receipt does not prove the complete projected after-state")
	}
	return nil
}

func mergeCompletedACK(receipts []contract.Object) (contract.Object, error) {
	if len(receipts) > 1 {
		return nil, errors.New("merge journal contains multiple operations")
	}
	if len(receipts) == 0 || receipts[0]["status"] != "completed" {
		return nil, nil
	}
	result, err := contract.ObjectAt(receipts[0], "result")
	if err != nil {
		return nil, err
	}
	return contract.ObjectAt(result, "provider_ack")
}

func (m MergeAdapter) observeAfter(ctx context.Context, p contract.Plan, op contract.Operation, ack contract.Object) (contract.Object, error) {
	if m.Provider == nil {
		return nil, errors.New("merge apply requires a provider")
	}
	if err := m.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, err
	}
	providerACK, _ := contract.ObjectAt(ack, "provider_ack")
	expected, err := mergeExpected(p, providerACK)
	if err != nil {
		return nil, err
	}
	event, _ := contract.ObjectAt(p.Data, "event")
	observed, err := m.Provider.MergeInventory(ctx, event)
	if err != nil {
		return nil, err
	}
	if !same(expected, observed) {
		return nil, errors.New("merged candidate's independent after-state differs from the reviewed projection")
	}
	result, err := contract.Clone(ack)
	if err != nil {
		return nil, err
	}
	result["observed_inventory_sha256"], err = contract.Digest(observed)
	return result, err
}

func (m MergeAdapter) Preflight(ctx context.Context, p contract.Plan, receipts []contract.Object) error {
	if m.Provider == nil {
		return errors.New("merge apply requires a provider")
	}
	providerACK, err := mergeCompletedACK(receipts)
	if err != nil {
		return err
	}
	expected, err := mergeExpected(p, providerACK)
	if err != nil {
		return err
	}
	event, _ := contract.ObjectAt(p.Data, "event")
	observed, err := m.Provider.MergeInventory(ctx, event)
	if err != nil {
		return err
	}
	if !same(expected, observed) {
		return errors.New("complete merge source changed; prepare and review a fresh candidate")
	}
	return nil
}

func (m MergeAdapter) Dispatch(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, error) {
	return m.DispatchAcknowledged(ctx, p, op, nonce, receipts, func(contract.Object) error { return nil })
}

func (m MergeAdapter) DispatchAcknowledged(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object, acknowledge func(contract.Object) error) (contract.Object, error) {
	if m.Provider == nil || acknowledge == nil {
		return nil, errors.New("merge dispatch requires provider and durable acknowledgement")
	}
	ops, err := mergeOperations(p)
	if err != nil || len(ops) != 1 || !same(ops[0], op) {
		return nil, errors.New("merge dispatch is outside the reviewed primitive")
	}
	if _, err := native.OperationMarker(nonce); err != nil {
		return nil, err
	}
	if len(receipts) != 1 || receipts[0]["status"] != "dispatching" || receipts[0]["id"] != op.ID || receipts[0]["operation_id"] != nonce {
		// Engine's snapshot is captured before it appends durable dispatch
		// intent. An empty prefix is also the valid first operation context.
		if len(receipts) != 0 {
			return nil, errors.New("merge dispatch cannot replay an existing receipt")
		}
	}
	if err := m.Preflight(ctx, p, receipts); err != nil {
		return nil, err
	}
	number, err := contract.PositiveInteger(op.Target["number"])
	if err != nil {
		return nil, err
	}
	sha, _ := contract.Nonempty(op.Target, "head_sha")
	method, _ := contract.Nonempty(op.Target, "merge_method")
	providerACK, err := m.Provider.MergePullRequest(ctx, number, sha, method)
	if err != nil {
		return nil, err
	}
	ack := contract.Object{"operation_id": nonce, "command": MergeCommand, "repository": p.Repository.Object(), "target": op.Target, "provider_ack": providerACK}
	if err := m.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, err
	}
	if err := acknowledge(ack); err != nil {
		return nil, err
	}
	return m.observeAfter(ctx, p, op, ack)
}

func (m MergeAdapter) Observe(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, contract.Object, error) {
	for _, receipt := range receipts {
		if receipt["id"] != op.ID || receipt["operation_id"] != nonce {
			continue
		}
		ack, err := contract.ObjectAt(receipt, "acknowledgement")
		if err != nil {
			return nil, nil, errors.New("unknown merge has no retained provider acknowledgement; it will not be retried")
		}
		result, err := m.observeAfter(ctx, p, op, ack)
		if err != nil {
			return nil, nil, err
		}
		providerACK, _ := contract.ObjectAt(result, "provider_ack")
		return result, contract.Object{"positive_identity": true, "after_state_verified": true, "live": true, "complete": true, "source": "github_api", "operation_id": nonce, "reference": p.Repository.URL + "/commit/" + providerACK["sha"].(string), "inventory_sha256": result["observed_inventory_sha256"]}, nil
	}
	return nil, nil, errors.New("unknown merge has no durable dispatch receipt")
}
