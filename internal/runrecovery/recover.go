package runrecovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func (e *Engine) recoveryHold(root, artifactName, reason string, invocation Invocation) (Object, error) {
	if len(reason) > 2048 || strings.ContainsAny(reason, "\r\n\x00") {
		reason = "required recovery evidence could not be verified"
	}
	if err := persistPackageJSON(root, "recovery-needed.json", Object{"schema_version": 1, "status": "recovery_needed", "workflow_file": invocation.Workflow, "repository": e.repository.FullName(), "recovery_key": invocation.RecoveryKey, "reason": reason}); err != nil {
		return nil, err
	}
	return Object{"outcome": "recovery_needed", "reason": reason, "artifact_name": artifactName}, nil
}

// Each historical archive is extracted in its own bounded lifetime. A long
// sequence of terminal predecessors cannot retain every expanded package.
func (e *Engine) inspectHistoricalPackage(ctx context.Context, reader ActionsReader, invocation Invocation, runs []Object, run, target, artifact Object, attempt int64, payload []byte, preparedFrontierOpen bool) (Object, error) {
	extracted, err := os.MkdirTemp(filepath.Dir(invocation.PackageRoot), "gh-steward-source-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(extracted)
	if err := ExtractPackageArchive(payload, extracted); err != nil {
		return nil, err
	}
	runID, err := positiveInteger(run["id"], "historical run ID")
	if err != nil {
		return nil, err
	}
	original := Invocation{Workflow: invocation.Workflow, RunID: runID, Attempt: attempt, RunName: run["display_title"].(string)}
	runContext, err := e.invocationContext(extracted, original)
	if err != nil {
		return nil, err
	}
	if runContext["phase"] == "completed" {
		if (runContext["recovered_from_run_id"] != nil || runContext["recovered_from_attempt"] != nil) && !preparedFrontierOpen {
			return nil, errors.New("historical observer requires its separate original source checkpoint")
		}
		record, err := e.terminalRecord(ctx, reader, original, extracted, runs, run, target, artifact, runContext)
		kind := "terminal"
		if preparedFrontierOpen {
			kind = "prepared-terminal"
		}
		return Object{"kind": kind, "record": record}, err
	}
	publication, hasPublication := runContext["publication"].(Object)
	qualificationPath := filepath.Join(extracted, filepath.FromSlash(preparedQualificationPath))
	_, qualificationStatErr := os.Lstat(qualificationPath)
	hasPreparedQualification := qualificationStatErr == nil
	if qualificationStatErr != nil && !errors.Is(qualificationStatErr, os.ErrNotExist) {
		return nil, errors.New("historical prepared qualification is unsafe or unreadable")
	}
	preparedPublication := runContext["phase"] == "prepared" && hasPublication && publication["stage"] == "pr-verify-pending"
	preparedNative := runContext["phase"] == "prepared" && runContext["publication"] == nil && hasPreparedQualification
	if runContext["phase"] != "dispatching" && !preparedPublication && !preparedNative {
		return nil, errors.New("prepared or no-plan attempt lacks source-qualified no-dispatch proof")
	}
	plans, err := terminalPlanInputs(extracted, runContext, false)
	if err != nil {
		return nil, err
	}
	if hasPreparedQualification {
		source, err := e.BuildPreparedSourceRecord(Object{
			"target": target, "run_id": runID, "attempt": attempt, "run": run, "artifact": artifact,
			"context_path": filepath.Join(extracted, "run-context.json"), "plans": plans, "qualification_path": qualificationPath,
		})
		return Object{"kind": "prepared", "source": source, "context": runContext}, err
	}
	input := Object{"target": target, "run_id": runID, "attempt": attempt, "run": run, "artifact": artifact, "context_path": filepath.Join(extracted, "run-context.json"), "plans": plans}
	if hasPublication {
		origin, err := publicationOrigin(runs, runContext)
		if err != nil {
			return nil, err
		}
		proof, err := BuildPublicationProof(extracted, origin, runContext, target, false)
		if err != nil {
			return nil, err
		}
		if err := VerifyPublicationAcquisition(ctx, reader, e.repository, extracted, proof, target, runContext, false, ""); err != nil {
			return nil, err
		}
		input["publication_origin_run"] = origin
	}
	source, err := e.BuildRecoverySourceRecord(input)
	return Object{"kind": "source", "source": source, "context": runContext}, err
}

func checkpointCandidates(ctx context.Context, reader ActionsReader, e *Engine, target Object, observed []Object, attempts map[int64]int64, all []Object, currentID, currentAttempt int64) ([]Object, error) {
	empty, err := EmptyChain(target)
	if err != nil {
		return nil, err
	}
	pending, err := PendingAttempts(empty, observed, attempts, currentID, currentAttempt)
	if err != nil {
		return nil, err
	}
	known := map[string]Object{}
	for _, item := range pending {
		run, err := object(item["run"], "historical attempt run")
		if err != nil {
			return nil, err
		}
		id, err := positiveInteger(run["id"], "historical run ID")
		if err != nil {
			return nil, err
		}
		attempt, err := positiveInteger(item["attempt"], "historical attempt")
		if err != nil {
			return nil, err
		}
		known[CheckpointArtifactName(target, id, attempt)] = Object{"run_id": id, "attempt": attempt}
	}
	candidates := []Object{}
	seen := map[string]bool{}
	selected := []Object{}
	var declaredBytes int64
	for _, metadata := range all {
		name, ok := metadata["name"].(string)
		if !ok || !strings.HasPrefix(name, recoveryArtifactPrefix(target)+"-run-") || !strings.HasSuffix(name, "-checkpoint-00") {
			continue
		}
		_, ok = known[name]
		if !ok || seen[name] {
			return nil, errors.New("recovery checkpoint is duplicated or is not one known historical attempt")
		}
		seen[name] = true
		if metadata["expired"] == true {
			continue
		}
		selected = append(selected, metadata)
		if len(selected) > MaxCheckpointArtifacts {
			return nil, errors.New("checkpoint acquisition exceeds the aggregate artifact-count budget")
		}
		if size, exists := metadata["size_in_bytes"]; exists {
			value, err := contract.Integer(size)
			if err != nil || value < 0 || value > MaxArtifactBytes {
				return nil, errors.New("checkpoint artifact declares an invalid size")
			}
			declaredBytes += value
			if declaredBytes > MaxHistoryAcquisitionBytes {
				return nil, errors.New("checkpoint acquisition exceeds the aggregate byte budget")
			}
		}
	}
	var downloadedBytes int64
	for _, metadata := range selected {
		name := metadata["name"].(string)
		identity := known[name]
		id, _ := positiveInteger(identity["run_id"], "checkpoint run ID")
		attempt, _ := positiveInteger(identity["attempt"], "checkpoint attempt")
		artifact, err := artifactIdentity(metadata, name, id)
		if err != nil {
			return nil, err
		}
		payload, err := downloadArtifact(ctx, reader, artifact)
		if err != nil {
			return nil, err
		}
		downloadedBytes += int64(len(payload))
		if downloadedBytes > MaxHistoryAcquisitionBytes {
			return nil, errors.New("checkpoint acquisition exceeds the aggregate downloaded-byte budget")
		}
		chain, err := CheckpointFromArchive(payload)
		if err != nil {
			return nil, err
		}
		checkpointMeta := Object{"name": name, "id": artifact["id"], "digest": artifact["digest"], "workflow_run_id": id, "workflow_run_attempt": attempt}
		owner, err := e.ValidateCheckpointArtifact(chain, target, checkpointMeta)
		if err != nil {
			return nil, err
		}
		metadataRun, err := object(metadata["workflow_run"], "checkpoint artifact workflow run")
		ownerRun, ownerErr := object(owner["run"], "checkpoint owner run")
		if err != nil || ownerErr != nil || metadataRun["head_sha"] != ownerRun["head_sha"] {
			return nil, errors.New("checkpoint artifact differs from its exact settled source")
		}
		candidates = append(candidates, chain)
	}
	return candidates, nil
}

func preparedFrontierKey(runID, attempt int64) string {
	return fmt.Sprintf("%d:%d", runID, attempt)
}

func (e *Engine) verifyPreparedFrontierPackages(ctx context.Context, reader ActionsReader, invocation Invocation, runs []Object, target Object, allArtifacts []Object, frontier []any) (map[string][]byte, int64, int64, error) {
	payloads := make(map[string][]byte, len(frontier))
	var bytesRead int64
	for _, raw := range frontier {
		entry, err := Exact(raw, []string{"kind", "source"}, "prepared frontier source")
		if err != nil {
			return nil, 0, 0, err
		}
		source, err := object(entry["source"], "prepared frontier source")
		if err != nil {
			return nil, 0, 0, err
		}
		runID, attempt, err := preparedSourceIdentity(source)
		if err != nil {
			return nil, 0, 0, err
		}
		sourceRun, err := object(source["run"], "prepared frontier source run")
		if err != nil {
			return nil, 0, 0, err
		}
		historyRun := observedRunFromRuns(runs, runID)
		if historyRun == nil {
			return nil, 0, 0, errors.New("prepared frontier source differs from its exact current workflow history")
		}
		historyIdentity, historyErr := NormalizeRun(historyRun)
		if historyErr != nil || !Equal(historyIdentity, source["run"]) {
			return nil, 0, 0, errors.New("prepared frontier source differs from its exact current workflow history")
		}
		status, err := reader.Read(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d", e.repository.FullName(), runID, attempt))
		identity, identityErr := NormalizeRun(status)
		if err != nil || identityErr != nil || !Equal(identity, source["run"]) || !exactInt(status["run_attempt"], attempt) || status["status"] != "completed" {
			return nil, 0, 0, errors.New("prepared frontier source attempt is active or its exact status cannot be verified")
		}
		name := RecoveryArtifactName(target, runID, attempt)
		var metadata Object
		for _, candidate := range allArtifacts {
			if candidate["name"] != name {
				continue
			}
			if metadata != nil {
				return nil, 0, 0, errors.New("prepared frontier source has duplicated recovery artifacts")
			}
			metadata = candidate
		}
		if metadata == nil {
			return nil, 0, 0, errors.New("prepared frontier source has no unique immutable recovery artifact")
		}
		artifact, err := artifactIdentity(metadata, name, runID)
		metadataRun, metadataErr := object(metadata["workflow_run"], "prepared frontier artifact run")
		storedArtifact, storedErr := object(source["artifact"], "prepared frontier stored artifact")
		if err != nil || metadataErr != nil || storedErr != nil || !Equal(artifact, storedArtifact) || metadataRun["head_sha"] != sourceRun["head_sha"] {
			return nil, 0, 0, errors.New("prepared frontier artifact differs from its exact immutable upload receipt")
		}
		payload, err := downloadArtifact(ctx, reader, artifact)
		if err != nil {
			return nil, 0, 0, err
		}
		bytesRead += int64(len(payload))
		if len(payloads)+1 > MaxCheckpointArtifacts || bytesRead > MaxHistoryAcquisitionBytes {
			return nil, 0, 0, errors.New("prepared frontier acquisition exceeds its aggregate history budget")
		}
		inspected, err := e.inspectHistoricalPackage(ctx, reader, invocation, runs, sourceRun, target, artifact, attempt, payload, false)
		if err != nil {
			return nil, 0, 0, err
		}
		kind := "journaled"
		if inspected["kind"] == "prepared" {
			kind = "prepared"
		} else if inspected["kind"] != "source" {
			return nil, 0, 0, errors.New("prepared frontier artifact no longer contains its qualified interrupted source")
		}
		if entry["kind"] != kind || !Equal(inspected["source"], source) {
			return nil, 0, 0, errors.New("prepared frontier artifact differs from its retained immutable source proof")
		}
		payloads[preparedFrontierKey(runID, attempt)] = payload
	}
	return payloads, int64(len(payloads)), bytesRead, nil
}

func observedRunFromRuns(runs []Object, runID int64) Object {
	for _, run := range runs {
		if exactInt(run["id"], runID) {
			return run
		}
	}
	return nil
}

func (e *Engine) restoreRecoveredSource(payload []byte, source, sourceContext, current Object, runID, attempt int64, invocation Invocation, target Object, chain Object, root string, artifactName string) (Object, error) {
	if err := restorePackageExceptFrontier(payload, root); err != nil {
		return nil, err
	}
	planOriginRunID, planOriginAttempt, err := contextPlanOrigin(sourceContext, true)
	if err != nil {
		return nil, errors.New("restored source context does not preserve its immutable plan origin")
	}
	if err := persistPackageJSON(root, "settlement-chain.json", chain); err != nil {
		return nil, err
	}
	if err := persistPackageJSON(root, "recovery-source.json", source); err != nil {
		return nil, err
	}
	sourceContext["workflow_run_id"] = invocation.RunID
	sourceContext["workflow_run_attempt"] = invocation.Attempt
	sourceContext["run_name"] = invocation.RunName
	sourceContext["recovered_from_run_id"] = runID
	sourceContext["recovered_from_attempt"] = attempt
	sourceContext["plan_origin_run_id"] = planOriginRunID
	sourceContext["plan_origin_attempt"] = planOriginAttempt
	if err := persistPackageJSON(root, "run-context.json", sourceContext); err != nil {
		return nil, err
	}
	if err := persistRecoveryObservation(root, invocation, current, target, chain, "resumed"); err != nil {
		return nil, err
	}
	return Object{"outcome": "resumed", "reason": "restored the exact original target, plans and durable journals", "artifact_name": artifactName}, nil
}

// Cumulative checkpoints repeat their predecessor prefix. Limit aggregate
// work explicitly until an authenticated checkpoint-compaction protocol exists.
const MaxCheckpointArtifacts = 1024
const MaxHistoryAcquisitionBytes = int64(256 << 20)

// Recover inspects complete workflow history and immutable artifact bytes.
// It restores one exact interrupted target, or leaves a typed hold. No provider
// mutation, new plan or unknown-write replay occurs in this boundary.
func (e *Engine) Recover(ctx context.Context, reader ActionsReader, invocation Invocation) (Object, error) {
	if !nonemptyString(invocation.RunName) || !nonemptyString(invocation.RecoveryKey) {
		return nil, errors.New("current workflow run title and semantic recovery key are required")
	}
	root, err := runnerDirectory(invocation.PackageRoot, invocation.RunnerTemp, true, true)
	if err != nil {
		return nil, err
	}
	artifactName := ""
	hold := func(err error) (Object, error) { return e.recoveryHold(root, artifactName, err.Error(), invocation) }
	runs, current, target, observed, attempts, err := e.invocationHistory(ctx, reader, invocation)
	if err != nil {
		return hold(err)
	}
	artifactName = RecoveryArtifactName(target, invocation.RunID, invocation.Attempt)
	allArtifacts, err := CompleteRepositoryArtifacts(ctx, reader, e.repository)
	if err != nil {
		return hold(err)
	}
	candidates, err := checkpointCandidates(ctx, reader, e, target, observed, attempts, allArtifacts, invocation.RunID, invocation.Attempt)
	if err != nil {
		return hold(err)
	}
	chain, err := e.SelectCheckpoint(candidates, target, observed, attempts)
	if err != nil {
		return hold(err)
	}
	if chain == nil {
		chain, err = EmptyChain(target)
		if err != nil {
			return hold(err)
		}
	}
	scratch, err := os.MkdirTemp(filepath.Dir(root), "gh-steward-history-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	frontier, err := array(chain["prepared_frontier"], "prepared source frontier")
	if err != nil {
		return hold(err)
	}
	frontierPayloads, historicalCount, historicalBytes, err := e.verifyPreparedFrontierPackages(ctx, reader, invocation, runs, target, allArtifacts, frontier)
	if err != nil {
		return hold(err)
	}
	for {
		pending, err := PendingAttempts(chain, observed, attempts, invocation.RunID, invocation.Attempt)
		if err != nil {
			return hold(err)
		}
		if len(pending) == 0 {
			if err := persistPackageJSON(root, "settlement-chain.json", chain); err != nil {
				return nil, err
			}
			outcome := "fresh"
			if invocation.Attempt > 1 {
				outcome = "terminal"
			}
			if err := persistRecoveryObservation(root, invocation, current, target, chain, outcome); err != nil {
				return nil, err
			}
			if invocation.Attempt > 1 {
				// An already settled rerun must never prepare another target. Its
				// positive predecessor checkpoint is retained without manufacturing
				// a second provider acknowledgement or a no-dispatch assertion.
				return Object{"outcome": "terminal", "reason": "the rerun predecessor is already positively settled; fresh work is prohibited", "artifact_name": artifactName}, nil
			}
			return Object{"outcome": "fresh", "reason": "all prior exact workflow attempts are positively settled", "artifact_name": artifactName}, nil
		}
		frontier, err = array(chain["prepared_frontier"], "prepared source frontier")
		if err != nil || len(pending) < len(frontier) {
			return hold(errors.New("prepared source frontier does not cover its exact unsettled history"))
		}
		if len(frontier) > 0 && len(pending) == len(frontier) {
			last, err := Exact(frontier[len(frontier)-1], []string{"kind", "source"}, "latest prepared frontier source")
			if err != nil {
				return hold(err)
			}
			source, err := object(last["source"], "latest prepared frontier source")
			if err != nil {
				return hold(err)
			}
			runID, attempt, err := preparedSourceIdentity(source)
			if err != nil {
				return hold(err)
			}
			payload := frontierPayloads[preparedFrontierKey(runID, attempt)]
			if payload == nil {
				return hold(errors.New("latest prepared frontier source was not reverified from its immutable artifact"))
			}
			sourceContext, err := preparedSourceContext(source)
			if err != nil {
				return hold(err)
			}
			result, err := e.restoreRecoveredSource(payload, source, sourceContext, current, runID, attempt, invocation, target, chain, root, artifactName)
			if err != nil {
				return nil, err
			}
			return result, nil
		}
		first := pending[len(frontier)]
		run, err := object(first["run"], "pending immutable run")
		if err != nil {
			return hold(err)
		}
		runID, _ := positiveInteger(run["id"], "pending run ID")
		attempt, _ := positiveInteger(first["attempt"], "pending attempt")
		// Missing plans, old SDK outputs and skipped job names cannot settle
		// history. Verify terminal status, then require a unique typed package.
		runPacket, runErr := reader.Read(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d", e.repository.FullName(), runID, attempt))
		packetIdentity, identityErr := NormalizeRun(runPacket)
		if runErr != nil || identityErr != nil || !Equal(packetIdentity, run) || !exactInt(runPacket["run_attempt"], attempt) || runPacket["status"] != "completed" {
			return hold(errors.New("earliest unsettled attempt is active or its exact status cannot be verified"))
		}
		name := RecoveryArtifactName(target, runID, attempt)
		var metadata Object
		for _, candidate := range allArtifacts {
			if candidate["name"] == name {
				if metadata != nil {
					return hold(errors.New("earliest unsettled attempt has duplicated recovery artifacts"))
				}
				metadata = candidate
			}
		}
		if metadata == nil {
			return hold(errors.New("earliest unsettled attempt has no unique exact recovery artifact"))
		}
		artifact, err := artifactIdentity(metadata, name, runID)
		if err != nil {
			return hold(err)
		}
		metadataRun, err := object(metadata["workflow_run"], "recovery artifact run")
		if err != nil || metadataRun["head_sha"] != run["head_sha"] {
			return hold(errors.New("recovery artifact differs from the exact historical source"))
		}
		payload, err := downloadArtifact(ctx, reader, artifact)
		if err != nil {
			return hold(err)
		}
		historicalCount++
		historicalBytes += int64(len(payload))
		if historicalCount > MaxCheckpointArtifacts || historicalBytes > MaxHistoryAcquisitionBytes {
			return hold(errors.New("normal recovery acquisition exceeds its aggregate history budget"))
		}
		inspected, err := e.inspectHistoricalPackage(ctx, reader, invocation, runs, run, target, artifact, attempt, payload, len(frontier) > 0)
		if err != nil {
			return hold(err)
		}
		if inspected["kind"] == "prepared-terminal" {
			chain, err = appendPreparedTerminal(chain, target, observed, attempts, inspected["record"].(Object), invocation.RunID, invocation.Attempt, e)
			if err != nil {
				return hold(err)
			}
			if err := persistPackageJSON(root, "settlement-chain.json", chain); err != nil {
				return nil, err
			}
			continue
		}
		if inspected["kind"] == "terminal" {
			if len(frontier) != 0 {
				return hold(errors.New("an open prepared frontier is followed by a terminal attempt without direct source lineage"))
			}
			chain, err = e.AppendSettlement(chain, target, observed, attempts, inspected["record"].(Object), invocation.RunID, invocation.Attempt)
			if err != nil {
				return hold(err)
			}
			if err := persistPackageJSON(root, "settlement-chain.json", chain); err != nil {
				return nil, err
			}
			continue
		}
		if inspected["kind"] == "prepared" {
			source := inspected["source"].(Object)
			chain, err = appendPreparedFrontier(chain, target, observed, attempts, preparedFrontierEntry("prepared", source), invocation.RunID, invocation.Attempt, e)
			if err != nil {
				return hold(err)
			}
			frontierPayloads[preparedFrontierKey(runID, attempt)] = payload
			if err := persistPackageJSON(root, "settlement-chain.json", chain); err != nil {
				return nil, err
			}
			continue
		}
		if inspected["kind"] != "source" {
			return hold(errors.New("historical attempt has no supported recovery proof"))
		}
		source := inspected["source"].(Object)
		runContext := inspected["context"].(Object)
		if source["publication"] == nil && len(frontier) > 0 {
			chain, err = appendPreparedFrontier(chain, target, observed, attempts, preparedFrontierEntry("journaled", source), invocation.RunID, invocation.Attempt, e)
			if err != nil {
				return hold(err)
			}
			frontierPayloads[preparedFrontierKey(runID, attempt)] = payload
			if err := persistPackageJSON(root, "settlement-chain.json", chain); err != nil {
				return nil, err
			}
			continue
		}
		if len(pending) != 1 || runContext["recovered_from_run_id"] != nil || runContext["recovered_from_attempt"] != nil {
			return hold(errors.New("an unqualified interrupted attempt must be the only unsettled predecessor before it can be restored"))
		}
		result, err := e.restoreRecoveredSource(payload, source, runContext, current, runID, attempt, invocation, target, chain, root, artifactName)
		if err != nil {
			return nil, err
		}
		return result, nil
	}
}

func restorePackageExceptFrontier(payload []byte, root string) error {
	existing, err := retainedPackageFiles(root)
	if err != nil {
		return err
	}
	if len(existing) > 1 || (len(existing) == 1 && existing[0] != "settlement-chain.json") {
		return errors.New("restore destination contains unbound execution evidence")
	}
	files, err := archiveFiles(payload, false)
	if err != nil {
		return err
	}
	for name, data := range files {
		if name == "settlement-chain.json" || name == "recovery-source.json" || name == "recovery-observation.json" ||
			name == preparedQualificationPath || name == "recovery-needed.json" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			return errors.New("restored source file already exists")
		}
		if err := persistPackageFile(root, name, data); err != nil {
			return err
		}
	}
	return nil
}
