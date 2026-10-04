package runrecovery

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

type Invocation struct {
	Workflow, RunName, RecoveryKey, PackageRoot, RunnerTemp string
	RunID, Attempt                                          int64
}

type FinalizeOptions struct {
	Invocation
	ArtifactID                       int64
	ArtifactDigest, Checkpoint       string
	RecoverySource, PublicationProof string
	WorkflowSHA                      string
}

func (e *Engine) invocationHistory(ctx context.Context, reader ActionsReader, invocation Invocation) ([]Object, Object, Object, []Object, map[int64]int64, error) {
	if invocation.RunID < 1 || invocation.Attempt < 1 || !workflowFilename.MatchString(invocation.Workflow) {
		return nil, nil, nil, nil, nil, errors.New("workflow, run and attempt identities are required")
	}
	if _, err := e.MutatorStepAlternatives(invocation.Workflow); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	runs, err := CompleteWorkflowRuns(ctx, reader, e.repository, invocation.Workflow)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	var current Object
	for _, run := range runs {
		if exactInt(run["id"], invocation.RunID) {
			if current != nil || !exactInt(run["run_attempt"], invocation.Attempt) ||
				(invocation.RunName != "" && run["display_title"] != invocation.RunName) {
				return nil, nil, nil, nil, nil, errors.New("current invocation changed or is ambiguous in complete history")
			}
			current = run
		}
	}
	if current == nil {
		return nil, nil, nil, nil, nil, errors.New("current invocation is missing from complete history")
	}
	workflowID, err := positiveInteger(current["workflow_id"], "workflow ID")
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	target, err := TargetObject(invocation.Workflow, e.repository.URL, workflowID, "workflow-history-v2")
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	identity, err := NormalizeRun(current)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	rows := make([]any, len(runs))
	for i := range runs {
		rows[i] = runs[i]
	}
	observed, attempts, err := NormalizeInventory(rows, target)
	return runs, identity, target, observed, attempts, err
}

func (e *Engine) invocationContext(root string, invocation Invocation) (Object, error) {
	data, err := ReadPackageFile(root, "run-context.json")
	if err != nil {
		return nil, err
	}
	value, err := DecodeValue(data)
	if err != nil {
		return nil, err
	}
	context, err := object(value, "invocation context")
	if err != nil || !exactInt(context["schema_version"], 1) || context["workflow_file"] != invocation.Workflow ||
		context["repository"] != e.repository.FullName() || !exactInt(context["workflow_run_id"], invocation.RunID) ||
		!exactInt(context["workflow_run_attempt"], invocation.Attempt) ||
		(invocation.RunName != "" && context["run_name"] != invocation.RunName) ||
		(invocation.RecoveryKey != "" && context["recovery_key"] != invocation.RecoveryKey) {
		return nil, errors.New("invocation context differs from the exact consumer and workflow attempt")
	}
	if _, err := array(context["plans"], "invocation context plans"); err != nil {
		return nil, err
	}
	return context, nil
}

func publicationOrigin(runs []Object, runContext Object) (Object, error) {
	publication, err := object(runContext["publication"], "publication context")
	if err != nil {
		return nil, err
	}
	stamp, err := positiveInteger(publication["origin_run_attempt"], "publication origin attempt")
	if err != nil {
		return nil, err
	}
	var origin Object
	for _, run := range runs {
		if Equal(run["id"], publication["origin_run_id"]) {
			latest, err := positiveInteger(run["run_attempt"], "origin latest attempt")
			if err != nil || origin != nil || latest < stamp {
				return nil, errors.New("publication origin is ambiguous or ahead of complete history")
			}
			origin, err = NormalizeRun(run)
			if err != nil {
				return nil, err
			}
		}
	}
	if origin == nil {
		return nil, errors.New("publication origin is missing from complete history")
	}
	return origin, nil
}

func terminalPlanInputs(root string, runContext Object, completed bool) ([]any, error) {
	entries, err := array(runContext["plans"], "invocation plan list")
	if err != nil {
		return nil, err
	}
	plans := make([]any, 0, len(entries))
	seen := map[string]bool{}
	for _, raw := range entries {
		entry, err := object(raw, "invocation plan entry")
		if err != nil {
			return nil, err
		}
		name, nameOK := entry["name"].(string)
		journalID, journalOK := entry["journal_id"].(string)
		if !nameOK || !policyPlanName.MatchString(name) || seen[name] || !journalOK || !IsSHA256(journalID) ||
			(completed && entry["status"] != "completed") {
			return nil, errors.New("invocation plan or journal identity is invalid, duplicated or incomplete")
		}
		seen[name] = true
		relative, ok := entry["path"].(string)
		if !ok {
			return nil, errors.New("invocation plan lacks its exact file path")
		}
		planPath, err := PackageFile(root, relative)
		if err != nil {
			return nil, err
		}
		plan := Object{"name": name, "command": entry["command"], "plan_sha256": entry["sha256"], "journal_id": journalID, "plan_path": planPath}
		journalRelative := "journal/" + journalID + ".json"
		if value, exists := entry["journal_path"]; exists {
			journalRelative, ok = value.(string)
			if !ok {
				return nil, errors.New("invocation journal path is invalid")
			}
		}
		journalPath, err := PackageFile(root, journalRelative)
		if err != nil {
			if completed || entry["status"] != "prepared" {
				return nil, err
			}
			// A prepared source plan is explicitly journal-free. A symbolic or
			// unreadable existing path cannot be treated as absence.
			if _, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(journalRelative))); !errors.Is(statErr, os.ErrNotExist) {
				return nil, err
			}
			plan["journal_path"] = nil
		} else {
			plan["journal_path"] = journalPath
		}
		if completed {
			resultRelative := "apply-results/" + name + ".json"
			if value, exists := entry["apply_result_path"]; exists {
				resultRelative, ok = value.(string)
				if !ok {
					return nil, errors.New("invocation apply-result path is invalid")
				}
			}
			resultPath, err := PackageFile(root, resultRelative)
			if err != nil {
				return nil, err
			}
			plan["apply_result_path"] = resultPath
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func retainedPackageFiles(root string) ([]string, error) {
	files := []string{}
	var total int64
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("invocation package contains a symbolic or unreadable entry")
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() > MaxFileBytes || len(files) >= MaxArtifactEntries {
			return errors.New("invocation package has a nonregular, oversized or excessive file inventory")
		}
		total += info.Size()
		if total > MaxArtifactBytes {
			return errors.New("invocation package exceeds its aggregate file size bound")
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	sort.Strings(files)
	return files, err
}

func (e *Engine) terminalRecord(ctx context.Context, reader ActionsReader, invocation Invocation, root string, runs []Object, run, target, artifact, runContext Object) (Object, error) {
	plans, err := terminalPlanInputs(root, runContext, true)
	if err != nil {
		return nil, err
	}
	input := Object{
		"target": target, "run_id": invocation.RunID, "attempt": invocation.Attempt, "run": run,
		"attempt_target": Object{"recovery_key": runContext["recovery_key"], "identity": runContext["attempt_target"]},
		"artifact":       artifact, "context_path": filepath.Join(root, "run-context.json"), "plans": plans, "publication": nil,
	}
	if runContext["publication"] != nil {
		origin, err := publicationOrigin(runs, runContext)
		if err != nil {
			return nil, err
		}
		proof, err := BuildPublicationProof(root, origin, runContext, target, true)
		if err != nil {
			return nil, err
		}
		if err := VerifyPublicationAcquisition(ctx, reader, e.repository, root, proof, target, runContext, true, ""); err != nil {
			return nil, err
		}
		input["publication"] = proof
	}
	return e.BuildTerminalRecord(input)
}

// Finalize authenticates the actual uploaded package before advancing a
// checkpoint. It never calls a provider write or replaces an existing sidecar.
func (e *Engine) Finalize(ctx context.Context, reader ActionsReader, options FinalizeOptions) (Object, error) {
	if options.ArtifactID < 1 || !settlementArtifactDigest.MatchString(options.ArtifactDigest) {
		return nil, errors.New("uploaded artifact ID and exact sha256 digest are required")
	}
	root, err := runnerDirectory(options.PackageRoot, options.RunnerTemp, false, false)
	if err != nil {
		return nil, err
	}
	destination, err := checkpointDestination(options.Checkpoint, options.RunnerTemp)
	if err != nil {
		return nil, err
	}
	runContext, err := e.invocationContext(root, options.Invocation)
	if err != nil {
		return nil, err
	}
	entries := runContext["plans"].([]any)
	if runContext["phase"] != "completed" || (len(entries) == 0 && runContext["publication"] == nil) {
		return Object{"outcome": "pending"}, nil
	}
	runs, run, target, observed, attempts, err := e.invocationHistory(ctx, reader, options.Invocation)
	if err != nil {
		return nil, err
	}
	if runContext["publication"] != nil {
		origin, err := publicationOrigin(runs, runContext)
		if err != nil {
			return nil, err
		}
		trusted := options.WorkflowSHA
		if !settlementSHA40.MatchString(trusted) {
			return nil, errors.New("publication finalization requires exact trusted runtime workflow source")
		}
		if !Equal(origin["id"], options.RunID) || !exactInt(runContext["publication"].(Object)["origin_run_attempt"], options.Attempt) {
			trusted = ""
		}
		proof, err := BuildPublicationProof(root, origin, runContext, target, true)
		if err != nil {
			return nil, err
		}
		if trusted != "" {
			if err := ValidatePublicationQualification(proof, target, runContext, trusted); err != nil {
				return nil, err
			}
		} else {
			// The observer's control source can differ from the original publisher.
			// Validate the retained, source-bound qualification and recovery identity.
			if _, err := ValidatePublicationProof(proof, target, runContext, true); err != nil {
				return nil, err
			}
		}
	}
	metadata, err := reader.Read(ctx, fmt.Sprintf("repos/%s/actions/artifacts/%d", e.repository.FullName(), options.ArtifactID))
	if err != nil {
		return nil, err
	}
	artifact, err := artifactIdentity(metadata, RecoveryArtifactName(target, options.RunID, options.Attempt), options.RunID)
	if err != nil || !exactInt(artifact["id"], options.ArtifactID) || artifact["digest"] != options.ArtifactDigest {
		return nil, errors.New("uploaded artifact differs from its exact upload receipt")
	}
	artifactRun, err := object(metadata["workflow_run"], "uploaded artifact workflow run")
	if err != nil || artifactRun["head_sha"] != run["head_sha"] {
		return nil, errors.New("uploaded artifact differs from the exact invocation source")
	}
	payload, err := downloadArtifact(ctx, reader, artifact)
	if err != nil {
		return nil, err
	}
	record, err := e.terminalRecord(ctx, reader, options.Invocation, root, runs, run, target, artifact, runContext)
	if err != nil {
		return nil, err
	}
	chain, err := EmptyChain(target)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(root, "settlement-chain.json")); err == nil {
		value, err := LoadJSON(filepath.Join(root, "settlement-chain.json"))
		if err != nil {
			return nil, err
		}
		chain, err = object(value, "invocation settlement chain")
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	settled := record["settlement"].(Object)
	if options.PublicationProof != "" {
		data, err := ReadPackageFile(root, options.PublicationProof)
		if err != nil {
			return nil, err
		}
		proof, err := DecodeValue(data)
		if err != nil || !Equal(proof, settled["publication"]) {
			return nil, errors.New("supplied publication proof differs from actual retained files")
		}
	}
	if options.RecoverySource == "" {
		if _, err := os.Lstat(filepath.Join(root, "recovery-source.json")); err == nil {
			options.RecoverySource = "recovery-source.json"
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if options.RecoverySource != "" {
		data, err := ReadPackageFile(root, options.RecoverySource)
		if err != nil {
			return nil, err
		}
		value, err := DecodeValue(data)
		if err != nil {
			return nil, err
		}
		source, err := object(value, "original recovery source proof")
		if err != nil {
			return nil, err
		}
		sourceContextValue, err := LoadFileProof(source["context_file"], "original recovery source context")
		if err != nil {
			return nil, err
		}
		sourceContext, err := object(sourceContextValue, "original recovery source context")
		if err != nil {
			return nil, err
		}
		observer := Object{"run_id": record["run_id"], "attempt": record["attempt"], "run": record["run"], "artifact": record["artifact"], "context_file": record["context_file"], "plans": settled["plans"], "policy_files": settled["policy_files"]}
		if settled["publication"] != nil {
			observer["publication"] = settled["publication"]
		}
		recovered := Object{
			"run_id": source["run_id"], "attempt": source["attempt"], "run": source["run"], "artifact": source["artifact"], "context_file": source["context_file"],
			"attempt_target": Object{"recovery_key": sourceContext["recovery_key"], "identity": sourceContext["attempt_target"]},
			"settlement":     Object{"kind": "recovered_terminal", "phase": "completed", "policy_files": settled["policy_files"], "proof": Object{"source": source, "observer": observer}},
		}
		chain, err = e.AppendSettlement(chain, target, observed, attempts, recovered, options.RunID, options.Attempt)
		if err != nil {
			return nil, err
		}
	} else if runContext["recovered_from_run_id"] != nil || runContext["recovered_from_attempt"] != nil {
		return nil, errors.New("recovered terminal context lost its separate original source proof")
	}
	advanced, err := e.AppendSettlement(chain, target, observed, attempts, record, options.RunID, options.Attempt)
	if err != nil {
		return nil, err
	}
	retained, err := retainedPackageFiles(root)
	if err != nil {
		return nil, err
	}
	if err := VerifyUploadedFiles(payload, root, retained); err != nil {
		return nil, err
	}
	encoded, err := Canonical(advanced)
	if err != nil || len(encoded)+1 > MaxCheckpointBytes {
		return nil, errors.New("advanced checkpoint exceeds its size bound")
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return nil, err
	}
	if err := persistPackageFile(destination, "settlement-chain.json", append(encoded, '\n')); err != nil {
		return nil, err
	}
	return Object{"outcome": "checkpoint", "checkpoint_name": CheckpointArtifactName(target, options.RunID, options.Attempt), "checkpoint_path": filepath.Join(destination, "settlement-chain.json")}, nil
}
