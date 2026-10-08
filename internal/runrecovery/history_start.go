package runrecovery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

const scopedSettlementSchemaVersion = int64(8)

var scopedChainFields = []string{"schema_version", "target", "history_start", "inventory", "settlements", "prepared_frontier", "prepared_terminal_proofs", "sha256"}

// history_start is the immutable identity of the last excluded workflow run.
// A reviewed policy defines the scope; it makes no assertion about old writes.
func parseHistoryStart(value any) (Object, error) {
	start, err := Exact(value, immutableRunFields, "history start")
	if err != nil {
		return nil, err
	}
	run, err := cloneObject(start)
	if err != nil {
		return nil, err
	}
	run["run_attempt"] = int64(1)
	start, err = NormalizeRun(run)
	if err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.RFC3339Nano, fmt.Sprint(start["created_at"])); err != nil {
		return nil, recoveryError("history start requires an exact RFC3339 creation time")
	}
	return start, nil
}

func (e *Engine) emptyChain(target Object) (Object, error) {
	chain, err := EmptyChain(target)
	if err != nil {
		return nil, err
	}
	start := e.workflows[fmt.Sprint(target["workflow_file"])].historyStart
	if start == nil {
		return chain, nil
	}
	chain["schema_version"], chain["history_start"] = scopedSettlementSchemaVersion, start
	delete(chain, "sha256")
	encoded, err := Canonical(chain)
	if err != nil {
		return nil, err
	}
	chain["sha256"] = SHA256(encoded)
	return chain, nil
}

// scopeHistory still checks the complete inventory and exact excluded anchor.
// Excluded attempts never become native settlements or recovery candidates.
func (e *Engine) scopeHistory(workflow string, currentID int64, runs []Object) ([]Object, error) {
	start := e.workflows[workflow].historyStart
	if start == nil {
		return runs, nil
	}
	anchorID := mustPositive(start["id"])
	if currentID <= anchorID {
		return nil, recoveryError("excluded historical runs cannot be replayed")
	}
	anchorTime, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(start["created_at"]))
	found := false
	result := make([]Object, 0, len(runs))
	for _, run := range runs {
		identity, err := NormalizeRun(run)
		if err != nil {
			return nil, err
		}
		if !Equal(identity["workflow_id"], start["workflow_id"]) {
			return nil, recoveryError("history start identifies another workflow")
		}
		id := mustPositive(identity["id"])
		created, err := time.Parse(time.RFC3339Nano, fmt.Sprint(identity["created_at"]))
		if err != nil {
			return nil, recoveryError("workflow creation time is invalid")
		}
		if id == anchorID {
			if found || !Equal(identity, start) {
				return nil, recoveryError("history start changed or is ambiguous")
			}
			found = true
		}
		if id <= anchorID {
			if created.After(anchorTime) || run["status"] != "completed" {
				return nil, recoveryError("excluded history contains a changed or active run")
			}
			continue
		}
		if created.Before(anchorTime) {
			return nil, recoveryError("workflow ordering conflicts with the history start")
		}
		result = append(result, run)
	}
	if !found {
		return nil, recoveryError("history start is missing from complete workflow history")
	}
	return result, nil
}

func (e *Engine) scopeArtifacts(ctx context.Context, reader ActionsReader, target Object, artifacts []Object) ([]Object, error) {
	start := e.workflows[fmt.Sprint(target["workflow_file"])].historyStart
	if start == nil {
		return artifacts, nil
	}
	result := make([]Object, 0, len(artifacts))
	excluded := []Object{}
	var declaredBytes int64
	for _, artifact := range artifacts {
		if strings.HasPrefix(fmt.Sprint(artifact["name"]), recoveryArtifactPrefix(target)+"-run-") {
			owner, err := object(artifact["workflow_run"], "recovery artifact owner")
			if err != nil {
				return nil, err
			}
			id, err := positiveInteger(owner["id"], "recovery artifact owner run ID")
			if err != nil {
				return nil, err
			}
			if id <= mustPositive(start["id"]) {
				// A later policy cannot move the boundary past native recovery that
				// was already started under this protocol, even if interrupted.
				if strings.HasSuffix(fmt.Sprint(artifact["name"]), "-checkpoint-00") {
					excluded = append(excluded, artifact)
					if len(excluded) > MaxCheckpointArtifacts {
						return nil, recoveryError("excluded checkpoint acquisition exceeds its count bound")
					}
					if size, exists := artifact["size_in_bytes"]; exists {
						value, err := contract.Integer(size)
						if err != nil || value < 0 || value > MaxArtifactBytes {
							return nil, recoveryError("excluded checkpoint declares an invalid size")
						}
						declaredBytes += value
						if declaredBytes > MaxHistoryAcquisitionBytes {
							return nil, recoveryError("excluded checkpoint acquisition exceeds its byte bound")
						}
					}
				}
				continue
			}
		}
		result = append(result, artifact)
	}
	var downloadedBytes int64
	for _, artifact := range excluded {
		owner := artifact["workflow_run"].(Object)
		identity, err := artifactIdentity(artifact, fmt.Sprint(artifact["name"]), mustPositive(owner["id"]))
		if err != nil {
			return nil, err
		}
		payload, err := downloadArtifact(ctx, reader, identity)
		if err != nil {
			return nil, err
		}
		downloadedBytes += int64(len(payload))
		if downloadedBytes > MaxHistoryAcquisitionBytes {
			return nil, recoveryError("excluded checkpoint acquisition exceeds its byte bound")
		}
		chain, err := CheckpointFromArchive(payload)
		if err != nil {
			return nil, err
		}
		if exactInt(chain["schema_version"], settlementSchemaVersion) || exactInt(chain["schema_version"], scopedSettlementSchemaVersion) {
			return nil, recoveryError("history start cannot advance past existing native recovery")
		}
	}
	return result, nil
}
