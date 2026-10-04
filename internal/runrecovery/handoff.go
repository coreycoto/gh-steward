package runrecovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// AcquireHandoff transfers a package between isolated workflow jobs by exact
// immutable upload identity. Its name is derived from the host/workflow/run,
// never supplied by untrusted job output or an artifact-name search.
func (e *Engine) AcquireHandoff(ctx context.Context, reader ActionsReader, invocation Invocation, artifactID int64, digest string) (Object, error) {
	return e.AcquireHandoffFor(ctx, reader, invocation, artifactID, digest, "apply")
}

// Transport transfers frontier or no-op data without granting permission to
// apply. Apply additionally validates every scoped plan and exact predecessor.
func (e *Engine) AcquireHandoffFor(ctx context.Context, reader ActionsReader, invocation Invocation, artifactID int64, digest, purpose string) (Object, error) {
	if purpose != "apply" && purpose != "transport" {
		return nil, errors.New("handoff purpose must be apply or transport")
	}
	if !nonemptyString(invocation.RunName) || !nonemptyString(invocation.RecoveryKey) {
		return nil, errors.New("handoff requires exact current title and semantic target")
	}
	if artifactID < 1 || !settlementArtifactDigest.MatchString(digest) {
		return nil, errors.New("handoff upload ID and exact SHA-256 are required")
	}
	root, err := runnerDirectory(invocation.PackageRoot, invocation.RunnerTemp, true, true)
	if err != nil {
		return nil, err
	}
	runs, run, target, observed, attempts, err := e.invocationHistory(ctx, reader, invocation)
	if err != nil {
		return nil, err
	}
	name := RecoveryArtifactName(target, invocation.RunID, invocation.Attempt) + "-handoff-00"
	metadata, err := reader.Read(ctx, fmt.Sprintf("repos/%s/actions/artifacts/%d", e.repository.FullName(), artifactID))
	if err != nil {
		return nil, err
	}
	artifact, err := artifactIdentity(metadata, name, invocation.RunID)
	if err != nil || !exactInt(artifact["id"], artifactID) || artifact["digest"] != digest {
		return nil, errors.New("recovery handoff differs from its exact upload receipt")
	}
	metadataRun, err := object(metadata["workflow_run"], "handoff workflow run")
	if err != nil || metadataRun["head_sha"] != run["head_sha"] {
		return nil, errors.New("handoff belongs to another invocation source")
	}
	payload, err := downloadArtifact(ctx, reader, artifact)
	if err != nil {
		return nil, err
	}
	if err := ExtractPackageArchive(payload, root); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(root, "recovery-needed.json")); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("a held recovery package cannot authorize publication")
	}
	chainPath, err := PackageFile(root, "settlement-chain.json")
	if err != nil {
		return nil, err
	}
	chain, err := LoadJSON(chainPath)
	if err != nil {
		return nil, err
	}
	validatedChain, err := e.ValidateChain(chain, target, observed, attempts)
	if err != nil {
		return nil, err
	}
	pending, err := PendingAttempts(validatedChain, observed, attempts, invocation.RunID, invocation.Attempt)
	if err != nil {
		return nil, err
	}
	var runContext Object
	if _, err := os.Lstat(filepath.Join(root, "run-context.json")); err == nil {
		runContext, err = e.invocationContext(root, invocation)
		if err != nil {
			return nil, err
		}
		if err := e.validateRunContext(runContext); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(pending) > 0 {
		if len(pending) != 1 || runContext == nil {
			return nil, errors.New("handoff cannot bypass an unsettled predecessor")
		}
		if err := e.validateRestoredHandoff(ctx, reader, invocation, root, runs, target, pending[0], runContext); err != nil {
			return nil, err
		}
	} else if runContext != nil && (runContext["recovered_from_run_id"] != nil || runContext["recovered_from_attempt"] != nil) {
		return nil, errors.New("restored handoff lost its exact pending source")
	}
	if purpose == "apply" {
		if runContext == nil || runContext["publication"] != nil {
			return nil, errors.New("apply handoff requires an exact native invocation context")
		}
		plans, _ := array(runContext["plans"], "apply handoff plans")
		if len(plans) == 0 {
			return nil, errors.New("apply handoff cannot authorize a plan-free transfer")
		}
		if len(pending) == 0 && runContext["phase"] != "prepared" {
			return nil, errors.New("fresh native handoff must contain prepared plans")
		}
		reader := &policyReader{root: root, used: map[string]bool{}}
		retainedPlans := []Object{}
		for _, raw := range plans {
			entry, err := object(raw, "apply handoff manifest")
			if err != nil {
				return nil, err
			}
			if entry["command"] == "workflow-noop" {
				return nil, errors.New("no-op transfer does not grant native apply authority")
			}
			planBytes, err := ReadPackageFile(root, fmt.Sprint(entry["path"]))
			if err != nil {
				return nil, err
			}
			retained := Object{"plan_file": MakeFileProof(planBytes)}
			if entry["status"] == "completed" {
				value, err := DecodeValue(planBytes)
				if err != nil {
					return nil, err
				}
				retained, err = e.terminalContextPlanProof(root, entry, value.(Object))
				if err != nil {
					return nil, err
				}
				if _, err := ValidateTerminalPlanProof(retained); err != nil {
					return nil, err
				}
			}
			retainedPlans = append(retainedPlans, retained)
			if len(pending) == 0 {
				journalRelative := "journal/" + fmt.Sprint(entry["journal_id"]) + ".json"
				if entry["status"] == "prepared" {
					if _, err := os.Lstat(filepath.Join(root, journalRelative)); !errors.Is(err, os.ErrNotExist) {
						return nil, errors.New("prepared handoff already has unbound dispatch evidence")
					}
				} else if entry["status"] != "completed" {
					return nil, errors.New("fresh handoff contains a started native dispatch")
				}
			}
		}
		if err := e.validatePlanPolicyFiles(runContext, retainedPlans, reader); err != nil {
			return nil, err
		}
	}
	return Object{"outcome": "acquired", "purpose": purpose, "artifact_name": name, "package_root": root}, nil
}

func (e *Engine) validateRestoredHandoff(ctx context.Context, reader ActionsReader, inv Invocation, root string, runs []Object, target, pending, runContext Object) error {
	source, err := readRecoverySource(root)
	if err != nil {
		return err
	}
	sourceRun, err := object(pending["run"], "earliest pending handoff source")
	if err != nil {
		return err
	}
	if !Equal(source["run_id"], sourceRun["id"]) || !Equal(source["attempt"], pending["attempt"]) || !Equal(source["run"], sourceRun) || !Equal(runContext["recovered_from_run_id"], source["run_id"]) || !Equal(runContext["recovered_from_attempt"], source["attempt"]) {
		return errors.New("handoff recovery is not the exact earliest pending source")
	}
	sourceArtifact, err := object(source["artifact"], "original handoff artifact")
	if err != nil {
		return err
	}
	id, err := positiveInteger(sourceArtifact["id"], "source artifact ID")
	if err != nil {
		return err
	}
	runID, _ := positiveInteger(source["run_id"], "source run ID")
	attempt, _ := positiveInteger(source["attempt"], "source attempt")
	metadata, err := reader.Read(ctx, fmt.Sprintf("repos/%s/actions/artifacts/%d", e.repository.FullName(), id))
	if err != nil {
		return err
	}
	artifact, err := artifactIdentity(metadata, RecoveryArtifactName(target, runID, attempt), runID)
	if err != nil || !Equal(artifact, sourceArtifact) {
		return errors.New("handoff source artifact differs from its original upload")
	}
	metadataRun, err := object(metadata["workflow_run"], "handoff source workflow run")
	if err != nil || metadataRun["head_sha"] != sourceRun["head_sha"] {
		return errors.New("handoff source artifact belongs to another exact source")
	}
	payload, err := downloadArtifact(ctx, reader, artifact)
	if err != nil {
		return err
	}
	inspected, err := e.inspectHistoricalPackage(ctx, reader, inv, runs, sourceRun, target, artifact, attempt, payload, false)
	if err != nil {
		return err
	}
	if inspected["kind"] != "source" || !Equal(inspected["source"], source) {
		return errors.New("handoff recovery source differs from its actual original package")
	}
	original := inspected["context"].(Object)
	for _, field := range []string{"workflow_file", "repository", "recovery_key", "attempt_target", "dispatch_steps", "plans", "phase", "publication", "parent_merge", "branch_cleanup_outcome"} {
		if !Equal(runContext[field], original[field]) {
			return errors.New("restored handoff changed an original plan, target or execution phase")
		}
	}
	files, err := archiveFiles(payload, false)
	if err != nil {
		return err
	}
	for relative, raw := range files {
		if relative == "run-context.json" || relative == "settlement-chain.json" || relative == "recovery-source.json" {
			continue
		}
		current, err := ReadPackageFile(root, relative)
		if err != nil || !Equal(current, raw) {
			return errors.New("handoff lost the exact original evidence bytes")
		}
	}
	return nil
}
