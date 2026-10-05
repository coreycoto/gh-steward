package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/snapshot"
)

type BranchCleanupProvider interface {
	BranchCleanupInventory(context.Context, contract.Object) (contract.Object, error)
	DeleteBranch(context.Context, string, string, string, string) (contract.Object, error)
}

type BranchCleanup struct{ Provider BranchCleanupProvider }

func PrepareBranchCleanup(ctx context.Context, provider BranchCleanupProvider, repo contract.Repository, rawSelection contract.Object, now time.Time) (contract.Plan, error) {
	if provider == nil {
		return contract.Plan{}, errors.New("branch cleanup preparation requires a provider")
	}
	selection, err := ParseBranchCleanupSelection(rawSelection, repo)
	if err != nil {
		return contract.Plan{}, err
	}
	raw, err := provider.BranchCleanupInventory(ctx, selection)
	if err != nil {
		return contract.Plan{}, err
	}
	inventory, err := normalizeBranchCleanupInventory(raw, repo, selection)
	if err != nil {
		return contract.Plan{}, err
	}
	sources := branchCleanupInventorySources(inventory)
	data := contract.Object{"selection": selection, "inventory": inventory}
	absences, err := branchCleanupAbsences(inventory)
	if err != nil {
		return contract.Plan{}, err
	}
	if len(absences) > 0 {
		data["already_absent"] = absences
	}
	plan := contract.Plan{Command: BranchCleanupCommand, Repository: repo, Data: data, Sources: sources}
	ops, err := branchCleanupOperations(plan)
	if err != nil {
		return contract.Plan{}, err
	}
	return contract.PreparePlan(BranchCleanupCommand, repo, sources, data, ops, now)
}

func branchCleanupSources() contract.Object {
	return contract.Object{"branches": contract.Object{"source": "github_api", "live": true, "complete": true},
		"pull_requests":   contract.Object{"source": "github_api", "live": true, "complete": true},
		"base_dependents": contract.Object{"source": "github_api", "live": true, "complete": true}}
}

func branchCleanupInventorySources(inventory contract.Object) contract.Object {
	sources := branchCleanupSources()
	if _, present := inventory["branch_evidence"]; present {
		sources["repository_settings"] = contract.Object{"source": "github_api", "live": true, "complete": true}
	}
	return sources
}

func normalizeBranchCleanupInventory(raw contract.Object, repo contract.Repository, selection contract.Object) (contract.Object, error) {
	keys := []string{"repo", "repository_node_id", "default_branch", "branches", "provenance"}
	if _, present := raw["branch_evidence"]; present {
		keys = append(keys, "branch_evidence")
	}
	if !executionKeys(raw, keys...) {
		return nil, errors.New("branch cleanup inventory has unsupported or missing facets")
	}
	rawRepo, err := contract.ObjectAt(raw, "repo")
	if err != nil {
		return nil, err
	}
	identity, err := contract.ParseRepository(rawRepo)
	if err != nil || identity != repo {
		return nil, errors.New("branch cleanup inventory belongs to another repository")
	}
	nodeID, err := contract.Nonempty(raw, "repository_node_id")
	if err != nil {
		return nil, err
	}
	defaultBranch, err := contract.Nonempty(raw, "default_branch")
	if err != nil {
		return nil, err
	}
	if err := native.ValidateBranchName(defaultBranch); err != nil {
		return nil, err
	}
	var evidence contract.Object
	var refs map[string]string
	if _, present := raw["branch_evidence"]; present {
		evidence, refs, err = normalizeBranchCleanupEvidence(raw["branch_evidence"], repo, nodeID, defaultBranch)
		if err != nil {
			return nil, err
		}
	}
	provenance, err := contract.ObjectAt(raw, "provenance")
	if err != nil || !same(provenance, contract.Object{"live": true, "complete": true, "source": "github_api", "repository_node_id": nodeID, "selection": selection}) {
		return nil, errors.New("branch cleanup inventory lacks complete live evidence for the selected repository and branches")
	}
	requested, err := contract.Objects(selection, "branches")
	if err != nil {
		return nil, err
	}
	rows, err := contract.Objects(raw, "branches")
	if err != nil || len(rows) != len(requested) {
		return nil, errors.New("branch cleanup inventory does not cover the exact selection")
	}
	canonical := make([]any, 0, len(rows))
	for index, row := range rows {
		if !executionKeys(row, "selection", "branch", "pull_request", "base_dependents") || !same(row["selection"], requested[index]) {
			return nil, errors.New("branch cleanup candidate has another selection or incomplete facets")
		}
		name := requested[index]["name"].(string)
		var branch any
		if row["branch"] != nil {
			ref, err := contract.ObjectAt(row, "branch")
			if err != nil || !executionKeys(ref, "repo", "repository_node_id", "id", "name", "sha") || !same(ref["repo"], repo.Object()) || ref["repository_node_id"] != nodeID || ref["name"] != name {
				return nil, errors.New("cleanup branch ref is outside the selected immutable scope")
			}
			if _, err := contract.Nonempty(ref, "id"); err != nil {
				return nil, err
			}
			if _, err := native.CommitOID(ref["sha"]); err != nil {
				return nil, err
			}
			branch = ref
		}
		if evidence != nil {
			sha, exists := refs[name]
			if (branch != nil) != exists || (exists && branch.(contract.Object)["sha"] != sha) {
				return nil, errors.New("cleanup ref differs from the complete branch inventory")
			}
		}
		pr, err := contract.ObjectAt(row, "pull_request")
		if err != nil {
			return nil, err
		}
		// The stored representation is the same closed normalized shape as the
		// exact-head merge adapter. Rebuild a provider envelope for validation.
		provider, err := branchCleanupProviderPR(pr)
		if err != nil {
			return nil, err
		}
		number, _ := contract.PositiveInteger(requested[index]["pull_request_number"])
		normalized, err := snapshot.NormalizeMergePullRequest(provider, repo, number)
		if err != nil || !same(normalized, pr) {
			return nil, errors.New("cleanup PR is not complete canonical evidence for the selected candidate")
		}
		dependents, err := contract.ObjectAt(row, "base_dependents")
		if err != nil {
			return nil, err
		}
		if err := validateBranchBaseDependents(dependents, repo, nodeID, name); err != nil {
			return nil, err
		}
		canonical = append(canonical, contract.Object{"selection": requested[index], "branch": branch, "pull_request": normalized, "base_dependents": dependents})
	}
	result := contract.Object{"repo": repo.Object(), "repository_node_id": nodeID, "default_branch": defaultBranch, "branches": canonical, "provenance": provenance}
	if evidence != nil {
		result["branch_evidence"] = evidence
	}
	return result, nil
}

func branchCleanupProviderPR(pr contract.Object) (contract.Object, error) {
	if !executionKeys(pr, "number", "repository", "id", "url", "baseRefName", "headRefName", "state", "mergeStateStatus", "title", "body", "isDraft", "merged", "headRefOid", "reviewDecision", "merge_commit_sha", "labels", "author", "head_repository") {
		return nil, errors.New("cleanup PR has unsupported or missing normalized fields")
	}
	provider, err := contract.Clone(pr)
	if err != nil {
		return nil, err
	}
	provider["headRepository"] = pr["head_repository"]
	if pr["author"] != nil {
		author, err := contract.ObjectAt(pr, "author")
		if err != nil || !executionKeys(author, "login", "is_bot") {
			return nil, errors.New("cleanup PR author evidence is incomplete")
		}
		login, err := contract.Nonempty(author, "login")
		if err != nil {
			return nil, err
		}
		bot, err := contract.Bool(author, "is_bot")
		if err != nil {
			return nil, err
		}
		typename := "User"
		if bot {
			typename = "Bot"
		}
		provider["author"] = contract.Object{"login": login, "__typename": typename}
	}
	provider["mergeCommit"] = nil
	if pr["merge_commit_sha"] != nil {
		provider["mergeCommit"] = contract.Object{"oid": pr["merge_commit_sha"]}
	}
	labels, err := contract.Array(pr, "labels")
	if err != nil {
		return nil, err
	}
	provider["labels"] = contract.Object{"nodes": labels, "pageInfo": contract.Object{"hasNextPage": false}}
	return provider, nil
}

func validateBranchBaseDependents(raw contract.Object, repo contract.Repository, nodeID, branch string) error {
	if !executionKeys(raw, "repo", "repository_node_id", "base_branch", "pull_requests", "provenance") || !same(raw["repo"], repo.Object()) || raw["repository_node_id"] != nodeID || raw["base_branch"] != branch {
		return errors.New("base dependency collection has another repository incarnation or branch")
	}
	if !same(raw["provenance"], contract.Object{"live": true, "complete": true, "source": "github_api"}) {
		return errors.New("base dependency collection is not complete live evidence")
	}
	rows, err := contract.Objects(raw, "pull_requests")
	if err != nil {
		return err
	}
	seenIDs, seenNumbers := map[string]bool{}, map[int64]bool{}
	var previous int64
	for _, row := range rows {
		if !executionKeys(row, "id", "number", "url", "baseRefName", "headRefName", "state", "isDraft", "merged") || row["baseRefName"] != branch || row["state"] != "OPEN" || row["merged"] != false {
			return errors.New("base dependency contains incomplete or inconsistent PR state")
		}
		id, err := contract.Nonempty(row, "id")
		if err != nil || seenIDs[id] {
			return errors.New("base dependency repeats or lacks an immutable PR identity")
		}
		number, err := contract.PositiveInteger(row["number"])
		if err != nil || seenNumbers[number] || number <= previous {
			return errors.New("base dependency numbers are malformed, repeated or noncanonical")
		}
		if err := (&native.Transport{Repository: repo}).ValidatePullRequestURL(row["url"], number); err != nil {
			return err
		}
		head, err := contract.Nonempty(row, "headRefName")
		if err != nil {
			return err
		}
		if err := native.ValidateBranchName(head); err != nil {
			return err
		}
		if _, err := contract.Bool(row, "isDraft"); err != nil {
			return err
		}
		seenIDs[id], seenNumbers[number], previous = true, true, number
	}
	return nil
}

func branchCleanupPlan(p contract.Plan) (contract.Object, contract.Object, error) {
	keys := []string{"selection", "inventory"}
	if _, present := p.Data["already_absent"]; present {
		keys = append(keys, "already_absent")
	}
	if p.Command != BranchCleanupCommand || !executionKeys(p.Data, keys...) {
		return nil, nil, errors.New("branch cleanup adapter rejects this command or authored source scope")
	}
	rawSelection, err := contract.ObjectAt(p.Data, "selection")
	if err != nil {
		return nil, nil, err
	}
	selection, err := ParseBranchCleanupSelection(rawSelection, p.Repository)
	if err != nil || !same(selection, rawSelection) {
		return nil, nil, errors.New("branch cleanup selection is not canonical")
	}
	raw, err := contract.ObjectAt(p.Data, "inventory")
	if err != nil {
		return nil, nil, err
	}
	inventory, err := normalizeBranchCleanupInventory(raw, p.Repository, selection)
	if err != nil || !same(inventory, raw) {
		return nil, nil, errors.New("stored branch cleanup inventory is incomplete or noncanonical")
	}
	if !same(p.Sources, branchCleanupInventorySources(inventory)) {
		return nil, nil, errors.New("branch cleanup source scope differs from the reviewed inventory")
	}
	absences, err := branchCleanupAbsences(inventory)
	if err != nil {
		return nil, nil, err
	}
	_, recorded := p.Data["already_absent"]
	if (len(absences) > 0) != recorded || (recorded && !same(p.Data["already_absent"], absences)) {
		return nil, nil, errors.New("branch cleanup no-op evidence differs from the exact reviewed absent candidates")
	}
	return selection, inventory, nil
}

func branchCleanupOperations(p contract.Plan) ([]contract.Operation, error) {
	_, inventory, err := branchCleanupPlan(p)
	if err != nil {
		return nil, err
	}
	rows, _ := contract.Objects(inventory, "branches")
	ops := []contract.Operation{}
	for index, row := range rows {
		selection, _ := contract.ObjectAt(row, "selection")
		name := selection["name"].(string)
		if name == inventory["default_branch"] {
			return nil, errors.New("cleanup requires an exact reviewed non-default branch")
		}
		pr, _ := contract.ObjectAt(row, "pull_request")
		headRepo, err := contract.ObjectAt(pr, "head_repository")
		if err != nil {
			return nil, errors.New("cleanup PR head repository is unavailable")
		}
		identity, err := contract.ParseRepository(headRepo)
		if err != nil || identity != p.Repository || pr["repository"] != p.Repository.FullName() || pr["state"] != "MERGED" || pr["merged"] != true || pr["isDraft"] != false || pr["headRefName"] != name || pr["headRefOid"] != selection["sha"] {
			return nil, errors.New("branch cleanup candidate is not the selected merged same-repository PR head")
		}
		dependents, _ := contract.ObjectAt(row, "base_dependents")
		prs, _ := contract.Objects(dependents, "pull_requests")
		if len(prs) > 0 {
			return nil, errors.New("branch cleanup candidate is still a base of an open PR")
		}
		// Positive reviewed absence is a zero-write outcome. It is not a
		// deletion primitive or proof that an earlier uncertain write succeeded.
		if row["branch"] == nil {
			continue
		}
		branch, err := contract.ObjectAt(row, "branch")
		if err != nil || branch["sha"] != selection["sha"] {
			return nil, errors.New("cleanup requires the present exact reviewed branch SHA")
		}
		number, _ := contract.PositiveInteger(selection["pull_request_number"])
		ops = append(ops, contract.Operation{ID: fmt.Sprintf("branch-cleanup-%06d", index+1), Kind: "branch-delete", Target: contract.Object{"name": name, "sha": selection["sha"], "pull_request_number": number, "repository_node_id": inventory["repository_node_id"], "ref_id": branch["id"]}, Before: contract.Object{"exists": true, "ref": branch}, After: contract.Object{"exists": false, "ref": nil}})
	}
	return ops, nil
}

func (b BranchCleanup) Operations(p contract.Plan) ([]contract.Operation, error) {
	return branchCleanupOperations(p)
}

func branchCleanupPrefixInventory(p contract.Plan, last int) (contract.Object, error) {
	_, inventory, err := branchCleanupPlan(p)
	if err != nil {
		return nil, err
	}
	projected, err := contract.Clone(inventory)
	if err != nil {
		return nil, err
	}
	ops, err := branchCleanupOperations(p)
	if err != nil {
		return nil, err
	}
	if last >= len(ops) {
		return nil, errors.New("branch cleanup projection exceeds reviewed candidates")
	}
	deleted := map[string]bool{}
	for index := 0; index <= last; index++ {
		deleted[ops[index].Target["name"].(string)] = true
	}
	rows, _ := contract.Objects(projected, "branches")
	for _, row := range rows {
		selection, _ := contract.ObjectAt(row, "selection")
		if deleted[selection["name"].(string)] {
			row["branch"] = nil
		}
	}
	if evidence, present := projected["branch_evidence"].(contract.Object); present {
		collection, _ := contract.ObjectAt(evidence, "inventory")
		branches, _ := contract.Objects(collection, "branches")
		remaining := []any{}
		for _, branch := range branches {
			if !deleted[branch["name"].(string)] {
				remaining = append(remaining, branch)
			}
		}
		collection["branches"] = remaining
	}
	return projected, nil
}

func branchCleanupOperationIndex(p contract.Plan, op contract.Operation) (int, error) {
	ops, err := branchCleanupOperations(p)
	if err != nil {
		return 0, err
	}
	for index, candidate := range ops {
		if same(candidate, op) {
			return index, nil
		}
	}
	return 0, errors.New("branch deletion primitive is outside the reviewed plan")
}

func (b BranchCleanup) ValidateAcknowledgement(p contract.Plan, op contract.Operation, ack contract.Object) error {
	if _, err := branchCleanupOperationIndex(p, op); err != nil {
		return err
	}
	allowed := keySet("kind", "primitive_id", "repository", "operation_id", "target", "provider_ack")
	if _, exists := ack["observed_inventory_sha256"]; exists {
		allowed["observed_inventory_sha256"] = true
	}
	if !sameKeys(ack, allowed) || ack["kind"] != op.Kind || ack["primitive_id"] != op.ID || !same(ack["repository"], p.Repository.Object()) || !same(ack["target"], op.Target) {
		return errors.New("branch deletion acknowledgement has another intent or scope")
	}
	nonce, err := contract.Nonempty(ack, "operation_id")
	if err != nil {
		return err
	}
	if _, err := native.OperationMarker(nonce); err != nil {
		return err
	}
	providerACK, err := contract.ObjectAt(ack, "provider_ack")
	if err != nil {
		return err
	}
	return native.ValidateBranchDeletion(providerACK, p.Repository, op.Target["name"].(string), op.Target["sha"].(string))
}

func (b BranchCleanup) ValidateReceipt(p contract.Plan, op contract.Operation, result contract.Object) error {
	if err := b.ValidateAcknowledgement(p, op, result); err != nil {
		return err
	}
	index, err := branchCleanupOperationIndex(p, op)
	if err != nil {
		return err
	}
	expected, err := branchCleanupPrefixInventory(p, index)
	if err != nil {
		return err
	}
	digest, err := contract.Digest(expected)
	if err != nil || result["observed_inventory_sha256"] != digest {
		return errors.New("branch deletion receipt does not prove the complete reviewed prefix after-state")
	}
	return nil
}

func (b BranchCleanup) Preflight(ctx context.Context, p contract.Plan, receipts []contract.Object) error {
	if b.Provider == nil {
		return errors.New("branch cleanup requires a provider")
	}
	ops, err := branchCleanupOperations(p)
	if err != nil {
		return err
	}
	completed := 0
	for index, receipt := range receipts {
		if receipt["status"] != "completed" {
			return errors.New("branch cleanup cannot preflight an unresolved dispatch")
		}
		if index >= len(ops) || receipt["id"] != ops[index].ID {
			return errors.New("branch cleanup receipts are outside the reviewed prefix")
		}
		result, err := contract.ObjectAt(receipt, "result")
		if err != nil {
			return err
		}
		if err := b.ValidateReceipt(p, ops[index], result); err != nil {
			return err
		}
		if result["operation_id"] != receipt["operation_id"] {
			return errors.New("branch cleanup receipt has another dispatch identity")
		}
		completed++
	}
	expected, err := branchCleanupPrefixInventory(p, completed-1)
	if err != nil {
		return err
	}
	selection, _ := contract.ObjectAt(p.Data, "selection")
	raw, err := b.Provider.BranchCleanupInventory(ctx, selection)
	if err != nil {
		return err
	}
	actual, err := branchCleanupObservedInventory(raw, p, selection)
	if err != nil {
		return err
	}
	if !same(expected, actual) {
		return errors.New("complete live branch cleanup inventory drifted from reviewed state")
	}
	return nil
}

// New captures carry additive complete-collection/retention evidence. Validate
// it before projecting a live read into an older present-only plan's schema;
// historical receipt digests and exact deletion intents must remain unchanged.
func branchCleanupObservedInventory(raw contract.Object, p contract.Plan, selection contract.Object) (contract.Object, error) {
	actual, err := normalizeBranchCleanupInventory(raw, p.Repository, selection)
	if err != nil {
		return nil, err
	}
	reviewed, _ := contract.ObjectAt(p.Data, "inventory")
	if _, extended := reviewed["branch_evidence"]; !extended {
		delete(actual, "branch_evidence")
	}
	return actual, nil
}

func (b BranchCleanup) Dispatch(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, error) {
	return b.DispatchAcknowledged(ctx, p, op, nonce, receipts, func(contract.Object) error { return nil })
}

func (b BranchCleanup) DispatchAcknowledged(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object, persist func(contract.Object) error) (contract.Object, error) {
	if b.Provider == nil || persist == nil {
		return nil, errors.New("branch cleanup requires a provider and durable acknowledgement callback")
	}
	if _, err := native.OperationMarker(nonce); err != nil {
		return nil, err
	}
	index, err := branchCleanupOperationIndex(p, op)
	if err != nil {
		return nil, err
	}
	if len(receipts) != index {
		return nil, errors.New("branch deletion has an incomplete preceding receipt prefix")
	}
	if err := b.Preflight(ctx, p, receipts); err != nil {
		return nil, err
	}
	providerACK, err := b.Provider.DeleteBranch(ctx, nonce, op.Target["name"].(string), op.Target["sha"].(string), op.Target["repository_node_id"].(string))
	if err != nil {
		return nil, err
	}
	ack := contract.Object{"kind": op.Kind, "primitive_id": op.ID, "repository": p.Repository.Object(), "operation_id": nonce, "target": op.Target, "provider_ack": providerACK}
	if err := b.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, err
	}
	if err := persist(ack); err != nil {
		return nil, err
	}
	return b.observeAfter(ctx, p, op, ack)
}

func (b BranchCleanup) observeAfter(ctx context.Context, p contract.Plan, op contract.Operation, ack contract.Object) (contract.Object, error) {
	if err := b.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, err
	}
	index, _ := branchCleanupOperationIndex(p, op)
	expected, err := branchCleanupPrefixInventory(p, index)
	if err != nil {
		return nil, err
	}
	selection, _ := contract.ObjectAt(p.Data, "selection")
	raw, err := b.Provider.BranchCleanupInventory(ctx, selection)
	if err != nil {
		return nil, err
	}
	actual, err := branchCleanupObservedInventory(raw, p, selection)
	if err != nil {
		return nil, err
	}
	if !same(expected, actual) {
		return nil, errors.New("acknowledged branch deletion differs from complete independent after-state")
	}
	result, err := contract.Clone(ack)
	if err != nil {
		return nil, err
	}
	result["observed_inventory_sha256"], err = contract.Digest(actual)
	return result, err
}

func (b BranchCleanup) Observe(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, contract.Object, error) {
	if b.Provider == nil {
		return nil, nil, errors.New("branch cleanup observation requires a provider")
	}
	var ack contract.Object
	for _, receipt := range receipts {
		if receipt["id"] == op.ID && receipt["status"] == "unknown" && receipt["operation_id"] == nonce {
			ack, _ = receipt["acknowledgement"].(map[string]any)
		}
	}
	if ack == nil || ack["operation_id"] != nonce {
		return nil, nil, errors.New("ambiguous branch deletion has no durable native acknowledgement; no write was retried")
	}
	result, err := b.observeAfter(ctx, p, op, ack)
	if err != nil {
		return nil, nil, err
	}
	digest, err := contract.Digest(ack)
	if err != nil {
		return nil, nil, err
	}
	return result, contract.Object{"positive_identity": true, "after_state_verified": true, "operation_id": nonce, "reference": "journal:native-acknowledgement:" + digest}, nil
}
