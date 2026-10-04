// Package workflow composes pure reviewed intent with qualified native reads
// and durable primitive execution. It has no policy defaults or raw API escape.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/planning"
	"github.com/coreycoto/gh-steward/internal/snapshot"
)

const RelationshipCommand = "relationship-apply"

type RelationshipProvider interface {
	IssueGraph(context.Context) (contract.Object, error)
	AddRelationship(context.Context, int64, int64, string) (contract.Object, error)
	RemoveRelationship(context.Context, int64, int64, string) (contract.Object, error)
}

type NativeRelationships struct{ Transport *native.Transport }

func (n NativeRelationships) IssueGraph(ctx context.Context) (contract.Object, error) {
	return (snapshot.Service{Reader: n.Transport, Repository: n.Transport.Repository}).IssueGraph(ctx, "all", nil)
}
func (n NativeRelationships) AddRelationship(ctx context.Context, a, b int64, kind string) (contract.Object, error) {
	return n.Transport.AddRelationship(ctx, a, b, kind)
}
func (n NativeRelationships) RemoveRelationship(ctx context.Context, a, b int64, kind string) (contract.Object, error) {
	return n.Transport.RemoveRelationship(ctx, a, b, kind)
}

func same(a, b any) bool {
	x, e := contract.Digest(a)
	if e != nil {
		return false
	}
	y, e := contract.Digest(b)
	return e == nil && x == y
}

// relationshipInventory covers all repository issues, including closed issue
// identities and both directions of hierarchy. Reads cannot silently omit an
// issue that a reviewed dependency refers to.
func relationshipInventory(graph contract.Object, repo contract.Repository) (contract.Object, error) {
	raw, err := contract.ObjectAt(graph, "repo")
	if err != nil {
		return nil, err
	}
	r, err := contract.ParseRepository(raw)
	if err != nil || r != repo {
		return nil, errors.New("relationship source targets another repository")
	}
	prov, err := contract.ObjectAt(graph, "provenance")
	if err != nil || prov["live"] != true || prov["complete"] != true {
		return nil, errors.New("relationship preparation requires complete live issue evidence")
	}
	if prov["issue_state"] != "all" {
		return nil, errors.New("relationship source must explicitly include all issue states")
	}
	repositoryNodeID, err := contract.Nonempty(prov, "repository_node_id")
	if err != nil {
		return nil, err
	}
	if _, err := planning.RelationshipTopology(graph); err != nil {
		return nil, err
	}
	rows, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	items := []any{}
	for _, row := range rows {
		i := contract.Object{}
		for _, key := range []string{"id", "number", "title", "body", "state", "url", "labels", "milestone", "blocked_by_numbers", "parent_number", "child_numbers"} {
			v, exists := row[key]
			if !exists {
				return nil, fmt.Errorf("complete relationship inventory is missing %s", key)
			}
			i[key] = v
		}
		for _, key := range []string{"id", "title", "url"} {
			if _, err := contract.Nonempty(i, key); err != nil {
				return nil, err
			}
		}
		if _, err := contract.String(i, "body"); err != nil {
			return nil, err
		}
		if i["state"] != "OPEN" && i["state"] != "CLOSED" {
			return nil, errors.New("complete issue inventory has an invalid state")
		}
		n, _ := contract.PositiveInteger(i["number"])
		validator := &native.Transport{Repository: repo}
		if err := validator.ValidateIssueURL(i["url"], n); err != nil {
			return nil, err
		}
		if _, err := contract.Strings(i["labels"]); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	sort.Slice(items, func(a, b int) bool {
		x, _ := contract.PositiveInteger(items[a].(map[string]any)["number"])
		y, _ := contract.PositiveInteger(items[b].(map[string]any)["number"])
		return x < y
	})
	return contract.Clone(contract.Object{"repo": repo.Object(), "issues": items, "provenance": contract.Object{"live": true, "complete": true, "issue_state": "all", "repository_node_id": repositoryNodeID}})
}

func PrepareRelationships(ctx context.Context, provider RelationshipProvider, repo contract.Repository, payload, hierarchy contract.Object, now time.Time) (contract.Plan, error) {
	if provider == nil {
		return contract.Plan{}, errors.New("relationship preparation requires a provider")
	}
	graph, err := provider.IssueGraph(ctx)
	if err != nil {
		return contract.Plan{}, err
	}
	inv, err := relationshipInventory(graph, repo)
	if err != nil {
		return contract.Plan{}, err
	}
	data := contract.Object{"payload": payload, "issue_inventory": inv}
	if hierarchy != nil {
		data["hierarchy_policy"] = hierarchy
	}
	p := contract.Plan{Command: RelationshipCommand, Repository: repo, Data: data}
	ops, err := relationshipOperations(p)
	if err != nil {
		return contract.Plan{}, err
	}
	return contract.PreparePlan(RelationshipCommand, repo, contract.Object{"issues": contract.Object{"source": "github_api", "live": true, "complete": true, "state": "all"}}, data, ops, now)
}

func relationshipOperations(p contract.Plan) ([]contract.Operation, error) {
	if p.Command != RelationshipCommand {
		return nil, errors.New("relationship adapter does not accept this command")
	}
	graph, err := contract.ObjectAt(p.Data, "issue_inventory")
	if err != nil {
		return nil, err
	}
	graph, err = relationshipInventory(graph, p.Repository)
	if err != nil {
		return nil, err
	}
	payload, err := contract.ObjectAt(p.Data, "payload")
	if err != nil {
		return nil, err
	}
	var hierarchy contract.Object
	if raw, exists := p.Data["hierarchy_policy"]; exists {
		var ok bool
		hierarchy, ok = raw.(map[string]any)
		if !ok {
			return nil, errors.New("hierarchy policy must be an object")
		}
	}
	normalized, err := planning.ValidateRelationshipPayload(payload, graph, hierarchy)
	if err != nil {
		return nil, err
	}
	delta, err := planning.BuildRelationshipDelta(normalized, graph)
	if err != nil {
		return nil, err
	}
	changes, err := contract.Objects(delta, "changes")
	if err != nil {
		return nil, err
	}
	ops := []contract.Operation{}
	appendOp := func(kind string, issue, related int64, add bool) {
		ops = append(ops, contract.Operation{ID: fmt.Sprintf("relationship-%06d", len(ops)+1), Kind: "issue-relationship", Target: contract.Object{"issue_number": issue, "related_issue_number": related, "relation": kind}, Before: contract.Object{"present": !add}, After: contract.Object{"present": add}})
	}
	for _, change := range changes {
		n, err := contract.PositiveInteger(change["issue_number"])
		if err != nil {
			return nil, err
		}
		switch change["type"] {
		case "remove_blocked_by", "add_blocked_by":
			other, err := contract.PositiveInteger(change["blocked_by_issue_number"])
			if err != nil {
				return nil, err
			}
			appendOp("blocked-by", n, other, change["type"] == "add_blocked_by")
		case "clear_parent", "reparent":
			parent, err := contract.PositiveInteger(change["current_parent_issue_number"])
			if err != nil {
				return nil, err
			}
			appendOp("child", parent, n, false)
			if change["type"] == "reparent" {
				next, err := contract.PositiveInteger(change["target_parent_issue_number"])
				if err != nil {
					return nil, err
				}
				appendOp("child", next, n, true)
			}
		case "set_parent":
			parent, err := contract.PositiveInteger(change["target_parent_issue_number"])
			if err != nil {
				return nil, err
			}
			appendOp("child", parent, n, true)
		default:
			return nil, errors.New("pure relationship delta contains an unsupported change")
		}
	}
	// Remove the old edges before adding any reviewed replacements. A final
	// acyclic reparenting can otherwise introduce a temporary provider-rejected
	// cycle when an old ancestor is still attached during the first addition.
	sort.SliceStable(ops, func(i, j int) bool { return ops[i].After["present"] == false && ops[j].After["present"] == true })
	for index := range ops {
		ops[index].ID = fmt.Sprintf("relationship-%06d", index+1)
	}
	return ops, nil
}

type Relationships struct{ Provider RelationshipProvider }

func (a Relationships) Operations(p contract.Plan) ([]contract.Operation, error) {
	return relationshipOperations(p)
}
func (a Relationships) ValidateReceipt(p contract.Plan, op contract.Operation, result contract.Object) error {
	if err := a.ValidateAcknowledgement(p, op, result); err != nil {
		return err
	}
	if result["after_verified"] != true {
		return errors.New("relationship completion does not match the reviewed primitive")
	}
	return nil
}
func (a Relationships) ValidateAcknowledgement(p contract.Plan, op contract.Operation, result contract.Object) error {
	if result["kind"] != op.Kind || result["primitive_id"] != op.ID || !same(result["target"], op.Target) || !same(result["before"], op.Before) || !same(result["after"], op.After) || result["acknowledged"] != true {
		return errors.New("native relationship acknowledgement does not match reviewed intent")
	}
	if _, err := contract.ObjectAt(result, "provider_result"); err != nil {
		return err
	}
	raw, err := contract.ObjectAt(result, "repository")
	if err != nil {
		return err
	}
	repo, err := contract.ParseRepository(raw)
	if err != nil || repo != p.Repository {
		return errors.New("relationship receipt belongs to another repository")
	}
	nonce, err := contract.Nonempty(result, "operation_id")
	if err != nil {
		return err
	}
	_, err = native.OperationMarker(nonce)
	return err
}

func relationshipTarget(op contract.Operation) (int64, int64, string, bool, error) {
	a, e := contract.PositiveInteger(op.Target["issue_number"])
	if e != nil {
		return 0, 0, "", false, e
	}
	b, e := contract.PositiveInteger(op.Target["related_issue_number"])
	if e != nil || a == b {
		return 0, 0, "", false, errors.New("relationship targets must be distinct issues")
	}
	k, e := contract.Nonempty(op.Target, "relation")
	if e != nil || (k != "child" && k != "blocked-by") {
		return 0, 0, "", false, errors.New("unsupported relationship relation")
	}
	add, e := contract.Bool(op.After, "present")
	if e != nil {
		return 0, 0, "", false, e
	}
	return a, b, k, add, nil
}

func projectEdge(inv contract.Object, op contract.Operation) error {
	a, b, kind, add, err := relationshipTarget(op)
	if err != nil {
		return err
	}
	rows, err := contract.Objects(inv, "issues")
	if err != nil {
		return err
	}
	byNumber := map[int64]contract.Object{}
	for _, r := range rows {
		n, _ := contract.PositiveInteger(r["number"])
		byNumber[n] = r
	}
	if byNumber[a] == nil || byNumber[b] == nil {
		return errors.New("relationship projection references an unknown issue")
	}
	key := "blocked_by_numbers"
	if kind == "child" {
		key = "child_numbers"
	}
	list, err := contract.Array(byNumber[a], key)
	if err != nil {
		return err
	}
	values := []int64{}
	found := false
	for _, raw := range list {
		n, err := contract.PositiveInteger(raw)
		if err != nil {
			return err
		}
		if n == b {
			found = true
			if !add {
				continue
			}
		}
		values = append(values, n)
	}
	if found == add {
		return errors.New("primitive reviewed before-state differs from projected inventory")
	}
	if add {
		values = append(values, b)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	out := []any{}
	for _, n := range values {
		out = append(out, n)
	}
	byNumber[a][key] = out
	if kind == "child" {
		if add {
			if byNumber[b]["parent_number"] != nil {
				return errors.New("child already has a projected parent")
			}
			byNumber[b]["parent_number"] = a
		} else {
			parent, err := contract.PositiveInteger(byNumber[b]["parent_number"])
			if err != nil || parent != a {
				return errors.New("projected child has another parent")
			}
			byNumber[b]["parent_number"] = nil
		}
	}
	return nil
}

func (a Relationships) expected(p contract.Plan, receipts []contract.Object) (contract.Object, error) {
	inv, err := contract.ObjectAt(p.Data, "issue_inventory")
	if err != nil {
		return nil, err
	}
	inv, err = contract.Clone(inv)
	if err != nil {
		return nil, err
	}
	ops, err := a.Operations(p)
	if err != nil {
		return nil, err
	}
	byID := map[string]contract.Operation{}
	for _, op := range ops {
		byID[op.ID] = op
	}
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
			return nil, errors.New("receipt outside relationship plan")
		}
		result, err := contract.ObjectAt(receipt, "result")
		if err != nil {
			return nil, err
		}
		if err = a.ValidateReceipt(p, op, result); err != nil {
			return nil, err
		}
		if err = projectEdge(inv, op); err != nil {
			return nil, err
		}
	}
	return inv, nil
}
func (a Relationships) compare(ctx context.Context, p contract.Plan, expected contract.Object) error {
	if a.Provider == nil {
		return errors.New("relationship adapter requires a provider")
	}
	graph, err := a.Provider.IssueGraph(ctx)
	if err != nil {
		return err
	}
	actual, err := relationshipInventory(graph, p.Repository)
	if err != nil {
		return err
	}
	if !same(expected, actual) {
		return errors.New("complete relationship inventory drifted from reviewed state")
	}
	return nil
}
func (a Relationships) Preflight(ctx context.Context, p contract.Plan, receipts []contract.Object) error {
	expected, err := a.expected(p, receipts)
	if err != nil {
		return err
	}
	return a.compare(ctx, p, expected)
}
func (a Relationships) Dispatch(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, error) {
	return a.DispatchAcknowledged(ctx, p, op, nonce, receipts, func(contract.Object) error { return nil })
}
func (a Relationships) DispatchAcknowledged(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object, persist func(contract.Object) error) (contract.Object, error) {
	if _, err := native.OperationMarker(nonce); err != nil {
		return nil, err
	}
	expected, err := a.expected(p, receipts)
	if err != nil {
		return nil, err
	}
	if err = projectEdge(expected, op); err != nil {
		return nil, err
	}
	issue, related, kind, add, err := relationshipTarget(op)
	if err != nil {
		return nil, err
	}
	var nativeResult contract.Object
	if add {
		nativeResult, err = a.Provider.AddRelationship(ctx, issue, related, kind)
	} else {
		nativeResult, err = a.Provider.RemoveRelationship(ctx, issue, related, kind)
	}
	if err != nil {
		return nil, err
	}
	ack := contract.Object{"kind": op.Kind, "primitive_id": op.ID, "repository": p.Repository.Object(), "operation_id": nonce, "target": op.Target, "before": op.Before, "after": op.After, "acknowledged": true, "provider_result": nativeResult}
	if err := a.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, err
	}
	if persist == nil {
		return nil, errors.New("native acknowledgement persistence callback is required")
	}
	if err := persist(ack); err != nil {
		return nil, err
	}
	if err = a.compare(ctx, p, expected); err != nil {
		return nil, fmt.Errorf("relationship was acknowledged but completion could not be verified: %w", err)
	}
	ack["after_verified"] = true
	return ack, nil
}
func (a Relationships) Observe(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, contract.Object, error) {
	var ack contract.Object
	for _, receipt := range receipts {
		if receipt["id"] == op.ID && receipt["status"] == "unknown" && receipt["operation_id"] == nonce {
			if raw, exists := receipt["acknowledgement"]; exists {
				ack, _ = raw.(map[string]any)
			}
		}
	}
	// Matching live edges without the response captured during this dispatch
	// cannot establish that this specific write completed.
	if ack == nil || ack["operation_id"] != nonce {
		return nil, nil, errors.New("ambiguous relationship dispatch requires reviewed positive provider evidence; no write was retried")
	}
	if err := a.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, nil, err
	}
	expected, err := a.expected(p, receipts)
	if err != nil {
		return nil, nil, err
	}
	if err = projectEdge(expected, op); err != nil {
		return nil, nil, err
	}
	if err = a.compare(ctx, p, expected); err != nil {
		return nil, nil, err
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
