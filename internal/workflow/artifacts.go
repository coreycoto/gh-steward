// Package workflow composes exact run-artifact selection with complete live
// inventories and durable per-artifact deletion acknowledgements.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/snapshot"
)

const RunArtifactDeleteCommand = "artifacts-delete-apply"

// RunArtifactSelection contains all consumer-specific selection decisions.
// Names match the historical utility's exact, case-sensitive name lookup.
type RunArtifactSelection struct {
	Names         []string
	IgnoreMissing bool
}

func (s RunArtifactSelection) Object() contract.Object {
	names := make([]any, 0, len(s.Names))
	for _, name := range s.Names {
		names = append(names, name)
	}
	return contract.Object{"names": names, "ignore_missing": s.IgnoreMissing}
}

// RunArtifactDeletionProvider is the only workflow I/O seam. Inventory is a
// normalized complete read for the selected run; each delete returns a typed
// HTTP 204 acknowledgement, with no generic successful-response fallback.
type RunArtifactDeletionProvider interface {
	RunArtifactInventory(context.Context, int64) (contract.Object, error)
	DeleteRunArtifact(context.Context, int64, int64) (contract.Object, error)
}

// NativeRunArtifactProvider binds reads and writes to the native transport's
// exact repository and host. It does not select artifact names or authorize
// deletion.
type NativeRunArtifactProvider struct{ Transport *native.Transport }

func (n NativeRunArtifactProvider) RunArtifactInventory(ctx context.Context, runID int64) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("native artifact inventory requires a transport")
	}
	return snapshot.RunArtifactInventory(ctx, n.Transport, n.Transport.Repository, runID)
}

func (n NativeRunArtifactProvider) DeleteRunArtifact(ctx context.Context, runID, artifactID int64) (contract.Object, error) {
	if n.Transport == nil {
		return nil, errors.New("native artifact deletion requires a transport")
	}
	return n.Transport.DeleteRunArtifact(ctx, runID, artifactID)
}

func validateRunArtifactSelection(selection RunArtifactSelection) error {
	if len(selection.Names) == 0 {
		return errors.New("run artifact selection requires at least one exact name")
	}
	seen := map[string]bool{}
	for _, name := range selection.Names {
		if strings.TrimSpace(name) == "" || seen[name] {
			return errors.New("run artifact names must be nonempty and unique")
		}
		seen[name] = true
	}
	return nil
}

// PrepareRunArtifactDeletion captures the complete repository/run artifact
// inventory and the explicit names/missing policy in a v2 apply plan. A
// missing name does not suppress other selected deletions: like the previous
// utility, it remains reportable while matching artifacts proceed. The CLI
// can use MissingRunArtifactNames to preserve its nonzero outcome when the
// policy does not ignore missing names.
func PrepareRunArtifactDeletion(ctx context.Context, provider RunArtifactDeletionProvider, repo contract.Repository, runID int64, selection RunArtifactSelection, now time.Time) (contract.Plan, error) {
	if provider == nil || runID < 1 {
		return contract.Plan{}, errors.New("artifact preparation requires a provider and positive run ID")
	}
	normalizedRepo, err := contract.ParseRepository(repo.Object())
	if err != nil || normalizedRepo != repo {
		return contract.Plan{}, errors.New("artifact preparation requires a normalized exact repository")
	}
	if err := validateRunArtifactSelection(selection); err != nil {
		return contract.Plan{}, err
	}
	inventory, err := provider.RunArtifactInventory(ctx, runID)
	if err != nil {
		return contract.Plan{}, err
	}
	if err := validateRunArtifactInventory(inventory, repo, runID); err != nil {
		return contract.Plan{}, err
	}
	selectionObject := selection.Object()
	missing, ops, err := selectedArtifactOperations(inventory, selectionObject, runID)
	if err != nil {
		return contract.Plan{}, err
	}
	data := contract.Object{
		"selection":     selectionObject,
		"inventory":     inventory,
		"missing_names": stringValues(missing),
	}
	digest, err := contract.Digest(inventory)
	if err != nil {
		return contract.Plan{}, err
	}
	provenance, _ := contract.ObjectAt(inventory, "provenance")
	repoIdentity, _ := contract.ObjectAt(inventory, "repository")
	repoNodeID, _ := contract.Nonempty(repoIdentity, "node_id")
	sources := contract.Object{"run_artifacts": contract.Object{
		"source": "github_api", "live": provenance["live"], "complete": provenance["complete"],
		"run_id": runID, "repository_node_id": repoNodeID, "inventory_sha256": digest,
	}}
	return contract.PreparePlan(RunArtifactDeleteCommand, repo, sources, data, ops, now)
}

// MissingRunArtifactNames returns the exact prepare-time missing selection in
// request order. The CLI uses this with selection.ignore_missing to preserve
// the standalone utility's final status behavior.
func MissingRunArtifactNames(p contract.Plan) ([]string, error) {
	if p.Command != RunArtifactDeleteCommand {
		return nil, errors.New("missing artifact report belongs to another command")
	}
	data, err := artifactPlanData(p)
	if err != nil {
		return nil, err
	}
	return contract.Strings(data["missing_names"])
}

func stringValues(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func parseRunArtifactSelection(raw contract.Object) (RunArtifactSelection, error) {
	if len(raw) != 2 {
		return RunArtifactSelection{}, errors.New("artifact selection has unsupported or missing fields")
	}
	names, err := contract.Strings(raw["names"])
	if err != nil {
		return RunArtifactSelection{}, err
	}
	ignoreMissing, err := contract.Bool(raw, "ignore_missing")
	if err != nil {
		return RunArtifactSelection{}, err
	}
	selection := RunArtifactSelection{Names: names, IgnoreMissing: ignoreMissing}
	return selection, validateRunArtifactSelection(selection)
}

func artifactPlanData(p contract.Plan) (contract.Object, error) {
	if p.Command != RunArtifactDeleteCommand || len(p.Data) != 3 {
		return nil, errors.New("artifact plan has an unsupported command or data shape")
	}
	selectionRaw, err := contract.ObjectAt(p.Data, "selection")
	if err != nil {
		return nil, err
	}
	_, err = parseRunArtifactSelection(selectionRaw)
	if err != nil {
		return nil, err
	}
	inventory, err := contract.ObjectAt(p.Data, "inventory")
	if err != nil {
		return nil, err
	}
	if err := validateRunArtifactInventory(inventory, p.Repository, 0); err != nil {
		return nil, err
	}
	missing, _, err := selectedArtifactOperations(inventory, selectionRaw, 0)
	if err != nil {
		return nil, err
	}
	actualMissing, err := contract.Strings(p.Data["missing_names"])
	if err != nil || !same(actualMissing, missing) {
		return nil, errors.New("artifact plan missing-name evidence differs from its complete inventory")
	}
	sources, err := contract.ObjectAt(p.Sources, "run_artifacts")
	if err != nil || len(p.Sources) != 1 || sources["source"] != "github_api" || sources["live"] != true || sources["complete"] != true {
		return nil, errors.New("artifact plan requires complete live run-artifact source evidence")
	}
	run, _ := contract.ObjectAt(inventory, "run")
	runID, _ := contract.PositiveInteger(run["id"])
	if sourceRunID, err := contract.PositiveInteger(sources["run_id"]); err != nil || sourceRunID != runID {
		return nil, errors.New("artifact source run identity differs from its captured inventory")
	}
	repoIdentity, _ := contract.ObjectAt(inventory, "repository")
	repoNodeID, _ := contract.Nonempty(repoIdentity, "node_id")
	if sources["repository_node_id"] != repoNodeID {
		return nil, errors.New("artifact source repository incarnation differs from its captured inventory")
	}
	digest, err := contract.Digest(inventory)
	if err != nil || sources["inventory_sha256"] != digest {
		return nil, errors.New("artifact source digest differs from its captured inventory")
	}
	return p.Data, nil
}

func validateRunArtifactInventory(inventory contract.Object, repo contract.Repository, expectedRunID int64) error {
	if len(inventory) != 5 {
		return errors.New("run artifact inventory has an unsupported schema")
	}
	repoObject, err := contract.ObjectAt(inventory, "repository")
	if err != nil || len(repoObject) != 5 {
		return errors.New("run artifact inventory repository identity is incomplete")
	}
	parsedRepo, err := contract.ParseRepository(repoObject)
	if err != nil || parsedRepo != repo {
		return errors.New("run artifact inventory belongs to another repository")
	}
	repoNodeID, err := contract.Nonempty(repoObject, "node_id")
	if err != nil {
		return errors.New("run artifact inventory lacks an immutable repository identity")
	}
	run, err := contract.ObjectAt(inventory, "run")
	if err != nil || len(run) != 2 {
		return errors.New("run artifact inventory run identity is incomplete")
	}
	runID, err := contract.PositiveInteger(run["id"])
	if err != nil || (expectedRunID > 0 && runID != expectedRunID) || run["repository_node_id"] != repoNodeID {
		return errors.New("run artifact inventory has another run or repository incarnation")
	}
	provenance, err := contract.ObjectAt(inventory, "provenance")
	if err != nil || len(provenance) != 3 || provenance["source"] != "github_api" || provenance["live"] != true || provenance["complete"] != true {
		return errors.New("run artifact inventory is not a complete live GitHub read")
	}
	total, err := contract.Integer(inventory["total_count"])
	if err != nil || total < 0 {
		return errors.New("run artifact inventory total_count is invalid")
	}
	artifacts, err := contract.Objects(inventory, "artifacts")
	if err != nil || int64(len(artifacts)) != total {
		return errors.New("run artifact inventory is truncated or malformed")
	}
	seen := map[int64]bool{}
	lastID := int64(0)
	for _, artifact := range artifacts {
		id, err := contract.PositiveInteger(artifact["id"])
		if err != nil || seen[id] || id <= lastID {
			return errors.New("run artifact inventory has duplicate or noncanonical IDs")
		}
		seen[id], lastID = true, id
		if _, err := contract.String(artifact, "name"); err != nil {
			return errors.New("run artifact inventory has a malformed name")
		}
		for key := range artifact {
			switch key {
			case "id", "name", "size_in_bytes", "expired", "workflow_run":
			default:
				return errors.New("run artifact inventory contains an unsupported artifact field")
			}
		}
		if size, exists := artifact["size_in_bytes"]; exists {
			value, err := contract.Integer(size)
			if err != nil || value < 0 {
				return errors.New("run artifact inventory size is invalid")
			}
		}
		if expired, exists := artifact["expired"]; exists {
			if _, ok := expired.(bool); !ok {
				return errors.New("run artifact inventory expired state is invalid")
			}
		}
		if raw, exists := artifact["workflow_run"]; exists {
			workflowRun, ok := raw.(map[string]any)
			actualRunID, err := contract.PositiveInteger(workflowRun["id"])
			if !ok || err != nil || actualRunID != runID {
				return errors.New("run artifact inventory contains a foreign nested run")
			}
			for key, value := range workflowRun {
				if key != "id" && key != "repository_id" && key != "head_repository_id" {
					return errors.New("run artifact inventory contains unsupported nested run data")
				}
				if key != "id" {
					if _, err := contract.PositiveInteger(value); err != nil {
						return errors.New("run artifact inventory has invalid nested repository IDs")
					}
				}
			}
		}
	}
	return nil
}

func selectedArtifactOperations(inventory, selection contract.Object, expectedRunID int64) ([]string, []contract.Operation, error) {
	parsed, err := parseRunArtifactSelection(selection)
	if err != nil {
		return nil, nil, err
	}
	run, err := contract.ObjectAt(inventory, "run")
	if err != nil {
		return nil, nil, err
	}
	runID, err := contract.PositiveInteger(run["id"])
	if err != nil || (expectedRunID > 0 && runID != expectedRunID) {
		return nil, nil, errors.New("artifact selection targets another workflow run")
	}
	artifacts, err := contract.Objects(inventory, "artifacts")
	if err != nil {
		return nil, nil, err
	}
	byName := map[string][]int64{}
	for _, artifact := range artifacts {
		name, _ := contract.String(artifact, "name")
		id, _ := contract.PositiveInteger(artifact["id"])
		byName[name] = append(byName[name], id)
	}
	for name := range byName {
		sort.Slice(byName[name], func(i, j int) bool { return byName[name][i] < byName[name][j] })
	}
	missing := []string{}
	ops := []contract.Operation{}
	for _, name := range parsed.Names {
		matches := byName[name]
		if len(matches) == 0 {
			missing = append(missing, name)
			continue
		}
		for _, id := range matches {
			ops = append(ops, contract.Operation{
				ID:     fmt.Sprintf("artifact-%06d", len(ops)+1),
				Kind:   "run-artifact-delete",
				Target: contract.Object{"run_id": runID, "artifact_id": id, "name": name},
				Before: contract.Object{"present": true},
				After:  contract.Object{"present": false},
			})
		}
	}
	return missing, ops, nil
}

func runArtifactOperations(p contract.Plan) ([]contract.Operation, error) {
	data, err := artifactPlanData(p)
	if err != nil {
		return nil, err
	}
	inventory, _ := contract.ObjectAt(data, "inventory")
	selection, _ := contract.ObjectAt(data, "selection")
	_, ops, err := selectedArtifactOperations(inventory, selection, 0)
	return ops, err
}

type RunArtifactDeletionAdapter struct{ Provider RunArtifactDeletionProvider }

func (a RunArtifactDeletionAdapter) Operations(p contract.Plan) ([]contract.Operation, error) {
	return runArtifactOperations(p)
}

func artifactOperationIndex(ops []contract.Operation, operationID string) int {
	for i, op := range ops {
		if op.ID == operationID {
			return i
		}
	}
	return -1
}

func (a RunArtifactDeletionAdapter) ValidateAcknowledgement(p contract.Plan, op contract.Operation, ack contract.Object) error {
	ops, err := runArtifactOperations(p)
	if err != nil || artifactOperationIndex(ops, op.ID) < 0 || !same(ops[artifactOperationIndex(ops, op.ID)], op) {
		return errors.New("artifact acknowledgement is outside the reviewed primitive list")
	}
	if len(ack) != 5 || ack["command"] != RunArtifactDeleteCommand {
		return errors.New("artifact acknowledgement has an unsupported shape or command")
	}
	if !same(ack["repository"], p.Repository.Object()) {
		return errors.New("artifact acknowledgement belongs to another repository")
	}
	if !same(ack["target"], op.Target) {
		return errors.New("artifact acknowledgement has another run or artifact target")
	}
	operationID, err := contract.Nonempty(ack, "operation_id")
	if err != nil {
		return err
	}
	if _, err := native.OperationMarker(operationID); err != nil {
		return err
	}
	providerACK, err := contract.ObjectAt(ack, "provider_ack")
	if err != nil || len(providerACK) != 4 {
		return errors.New("artifact deletion acknowledgement is not typed")
	}
	runID, _ := contract.PositiveInteger(op.Target["run_id"])
	artifactID, _ := contract.PositiveInteger(op.Target["artifact_id"])
	actualRunID, runErr := contract.PositiveInteger(providerACK["run_id"])
	actualArtifactID, artifactErr := contract.PositiveInteger(providerACK["artifact_id"])
	status, statusErr := contract.Integer(providerACK["status_code"])
	if runErr != nil || artifactErr != nil || statusErr != nil || actualRunID != runID || actualArtifactID != artifactID || status != 204 || providerACK["no_content"] != true {
		return errors.New("artifact deletion acknowledgement does not prove exact HTTP 204 completion")
	}
	return nil
}

func artifactExpectedAfter(p contract.Plan, op contract.Operation) (contract.Object, error) {
	ops, err := runArtifactOperations(p)
	if err != nil {
		return nil, err
	}
	index := artifactOperationIndex(ops, op.ID)
	if index < 0 || !same(ops[index], op) {
		return nil, errors.New("artifact primitive differs from recomputed reviewed intent")
	}
	data, _ := artifactPlanData(p)
	inventory, _ := contract.ObjectAt(data, "inventory")
	return projectArtifactInventory(inventory, ops[:index+1])
}

func projectArtifactInventory(inventory contract.Object, operations []contract.Operation) (contract.Object, error) {
	projected, err := contract.Clone(inventory)
	if err != nil {
		return nil, err
	}
	removed := map[int64]bool{}
	for _, op := range operations {
		if op.Kind != "run-artifact-delete" {
			return nil, errors.New("artifact projection contains an unsupported primitive")
		}
		id, err := contract.PositiveInteger(op.Target["artifact_id"])
		if err != nil || removed[id] {
			return nil, errors.New("artifact projection has invalid or duplicate deletion identity")
		}
		removed[id] = true
	}
	rows, _ := contract.Objects(projected, "artifacts")
	remaining := make([]any, 0, len(rows))
	for _, row := range rows {
		id, _ := contract.PositiveInteger(row["id"])
		if !removed[id] {
			remaining = append(remaining, row)
		}
	}
	if len(rows)-len(remaining) != len(removed) {
		return nil, errors.New("artifact projection tried to remove an artifact outside the reviewed baseline")
	}
	projected["artifacts"] = remaining
	projected["total_count"] = int64(len(remaining))
	return projected, nil
}

func completedArtifactPrefix(p contract.Plan, ops []contract.Operation, receipts []contract.Object) (int, error) {
	if len(receipts) > len(ops) {
		return 0, errors.New("artifact journal exceeds the reviewed primitive list")
	}
	for i, receipt := range receipts {
		if receipt["id"] != ops[i].ID || receipt["status"] != "completed" {
			return 0, errors.New("artifact journal is not a completed operation prefix")
		}
		result, err := contract.ObjectAt(receipt, "result")
		if err != nil || aValidateArtifactReceipt(p, ops[i], result) != nil {
			return 0, errors.New("artifact journal contains an invalid completed receipt")
		}
	}
	return len(receipts), nil
}

func aValidateArtifactReceipt(p contract.Plan, op contract.Operation, result contract.Object) error {
	return (RunArtifactDeletionAdapter{}).ValidateReceipt(p, op, result)
}

func (a RunArtifactDeletionAdapter) ValidateReceipt(p contract.Plan, op contract.Operation, result contract.Object) error {
	if len(result) != 6 {
		return errors.New("artifact receipt has unsupported fields")
	}
	ack, err := contract.Clone(result)
	if err != nil {
		return err
	}
	delete(ack, "observed_inventory_sha256")
	if err := a.ValidateAcknowledgement(p, op, ack); err != nil {
		return err
	}
	expected, err := artifactExpectedAfter(p, op)
	if err != nil {
		return err
	}
	digest, err := contract.Digest(expected)
	if err != nil || result["observed_inventory_sha256"] != digest {
		return errors.New("artifact receipt lacks independent complete after-state evidence")
	}
	return nil
}

func (a RunArtifactDeletionAdapter) Preflight(ctx context.Context, p contract.Plan, receipts []contract.Object) error {
	if a.Provider == nil {
		return errors.New("artifact apply requires a provider")
	}
	ops, err := runArtifactOperations(p)
	if err != nil {
		return err
	}
	completed, err := completedArtifactPrefix(p, ops, receipts)
	if err != nil {
		return err
	}
	data, _ := artifactPlanData(p)
	baseline, _ := contract.ObjectAt(data, "inventory")
	expected, err := projectArtifactInventory(baseline, ops[:completed])
	if err != nil {
		return err
	}
	run, _ := contract.ObjectAt(baseline, "run")
	runID, _ := contract.PositiveInteger(run["id"])
	observed, err := a.Provider.RunArtifactInventory(ctx, runID)
	if err != nil {
		return err
	}
	if err := validateRunArtifactInventory(observed, p.Repository, runID); err != nil {
		return err
	}
	if !same(expected, observed) {
		return errors.New("complete run artifact inventory changed; prepare and review a fresh plan")
	}
	return nil
}

func makeArtifactAcknowledgement(p contract.Plan, op contract.Operation, nonce string, providerACK contract.Object) contract.Object {
	return contract.Object{"operation_id": nonce, "command": RunArtifactDeleteCommand, "repository": p.Repository.Object(), "target": op.Target, "provider_ack": providerACK}
}

func (a RunArtifactDeletionAdapter) observeAfter(ctx context.Context, p contract.Plan, op contract.Operation, ack contract.Object) (contract.Object, error) {
	if a.Provider == nil {
		return nil, errors.New("artifact apply requires a provider")
	}
	if err := a.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, err
	}
	expected, err := artifactExpectedAfter(p, op)
	if err != nil {
		return nil, err
	}
	runID, _ := contract.PositiveInteger(op.Target["run_id"])
	observed, err := a.Provider.RunArtifactInventory(ctx, runID)
	if err != nil {
		return nil, err
	}
	if err := validateRunArtifactInventory(observed, p.Repository, runID); err != nil {
		return nil, err
	}
	if !same(expected, observed) {
		return nil, errors.New("artifact delete acknowledged but independent complete after-state differs from the reviewed projection")
	}
	result, err := contract.Clone(ack)
	if err != nil {
		return nil, err
	}
	result["observed_inventory_sha256"], err = contract.Digest(observed)
	return result, err
}

func (a RunArtifactDeletionAdapter) Dispatch(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, error) {
	return a.DispatchAcknowledged(ctx, p, op, nonce, receipts, func(contract.Object) error { return nil })
}

func (a RunArtifactDeletionAdapter) DispatchAcknowledged(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object, persist func(contract.Object) error) (contract.Object, error) {
	if a.Provider == nil || persist == nil {
		return nil, errors.New("artifact dispatch requires a provider and durable acknowledgement")
	}
	ops, err := runArtifactOperations(p)
	if err != nil {
		return nil, err
	}
	index := artifactOperationIndex(ops, op.ID)
	if index < 0 || !same(ops[index], op) {
		return nil, errors.New("artifact dispatch is outside the reviewed primitive list")
	}
	completed, err := completedArtifactPrefix(p, ops, receipts)
	if err != nil || completed != index {
		return nil, errors.New("artifact dispatch cannot skip or replay an earlier primitive")
	}
	if _, err := native.OperationMarker(nonce); err != nil {
		return nil, err
	}
	if err := a.Preflight(ctx, p, receipts); err != nil {
		return nil, err
	}
	runID, err := contract.PositiveInteger(op.Target["run_id"])
	if err != nil {
		return nil, err
	}
	artifactID, err := contract.PositiveInteger(op.Target["artifact_id"])
	if err != nil {
		return nil, err
	}
	providerACK, err := a.Provider.DeleteRunArtifact(ctx, runID, artifactID)
	if err != nil {
		return nil, err
	}
	if err := validateArtifactProviderACK(providerACK, runID, artifactID); err != nil {
		return nil, err
	}
	ack := makeArtifactAcknowledgement(p, op, nonce, providerACK)
	if err := a.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, err
	}
	if err := persist(ack); err != nil {
		return nil, err
	}
	return a.observeAfter(ctx, p, op, ack)
}

func validateArtifactProviderACK(ack contract.Object, runID, artifactID int64) error {
	if len(ack) != 4 {
		return errors.New("native artifact deletion acknowledgement has unsupported fields")
	}
	actualRunID, runErr := contract.PositiveInteger(ack["run_id"])
	actualArtifactID, artifactErr := contract.PositiveInteger(ack["artifact_id"])
	status, statusErr := contract.Integer(ack["status_code"])
	if runErr != nil || artifactErr != nil || statusErr != nil || actualRunID != runID || actualArtifactID != artifactID || status != 204 || ack["no_content"] != true {
		return errors.New("native artifact deletion did not acknowledge exact HTTP 204 No Content")
	}
	return nil
}

func (a RunArtifactDeletionAdapter) Observe(ctx context.Context, p contract.Plan, op contract.Operation, nonce string, receipts []contract.Object) (contract.Object, contract.Object, error) {
	ops, err := runArtifactOperations(p)
	if err != nil {
		return nil, nil, err
	}
	index := artifactOperationIndex(ops, op.ID)
	if index < 0 || !same(ops[index], op) || len(receipts) != index+1 {
		return nil, nil, errors.New("unknown artifact receipt is outside the expected operation prefix")
	}
	for i := 0; i < index; i++ {
		if receipts[i]["id"] != ops[i].ID || receipts[i]["status"] != "completed" {
			return nil, nil, errors.New("unknown artifact receipt has incomplete earlier operations")
		}
		result, err := contract.ObjectAt(receipts[i], "result")
		if err != nil || a.ValidateReceipt(p, ops[i], result) != nil {
			return nil, nil, errors.New("unknown artifact receipt has an invalid completed prefix")
		}
	}
	receipt := receipts[index]
	if receipt["id"] != op.ID || receipt["operation_id"] != nonce || receipt["status"] != "unknown" {
		return nil, nil, errors.New("unknown artifact journal has another operation identity")
	}
	ack, err := contract.ObjectAt(receipt, "acknowledgement")
	if err != nil {
		return nil, nil, errors.New("unknown artifact delete has no retained 204 acknowledgement; it will not be replayed")
	}
	if err := a.ValidateAcknowledgement(p, op, ack); err != nil {
		return nil, nil, err
	}
	result, err := a.observeAfter(ctx, p, op, ack)
	if err != nil {
		return nil, nil, err
	}
	digest, _ := result["observed_inventory_sha256"].(string)
	runID, _ := contract.PositiveInteger(op.Target["run_id"])
	evidence := contract.Object{"positive_identity": true, "after_state_verified": true, "live": true, "complete": true, "source": "github_api", "operation_id": nonce, "reference": fmt.Sprintf("%s/actions/runs/%d", p.Repository.URL, runID), "inventory_sha256": digest}
	return result, evidence, nil
}
