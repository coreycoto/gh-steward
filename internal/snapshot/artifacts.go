// Package snapshot normalizes complete native reads. It never substitutes
// checked-in seeds for failed live reads or treats inaccessible data as empty.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// RunArtifactReader is the read-only native seam for one exact Actions run.
// The REST page method must return every page, not just the first page.
type RunArtifactReader interface {
	ReadRepository(context.Context) (contract.Object, error)
	ReadWorkflowRun(context.Context, int64) (contract.Object, error)
	RunArtifactPages(context.Context, int64) ([]any, error)
}

// RunArtifactInventory returns a normalized, complete inventory for the
// selected repository and run. No generated timestamp is included so the
// same complete source can be compared during recovery and terminal checks.
func RunArtifactInventory(ctx context.Context, reader RunArtifactReader, repository contract.Repository, runID int64) (contract.Object, error) {
	if reader == nil || runID < 1 {
		return nil, errors.New("artifact inventory requires a reader and positive run ID")
	}
	normalizedRepo, err := contract.ParseRepository(repository.Object())
	if err != nil || normalizedRepo != repository {
		return nil, errors.New("artifact inventory requires a normalized exact repository")
	}

	repoSource, err := reader.ReadRepository(ctx)
	if err != nil {
		return nil, err
	}
	actualRepo, err := contract.ParseRepository(repoSource)
	if err != nil || actualRepo != repository {
		return nil, errors.New("artifact repository read differs from the selected target")
	}
	repoNodeID, err := contract.Nonempty(repoSource, "id")
	if err != nil {
		return nil, errors.New("artifact repository immutable identity is unavailable")
	}

	runSource, err := reader.ReadWorkflowRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	actualRunID, err := contract.PositiveInteger(runSource["id"])
	if err != nil || actualRunID != runID {
		return nil, errors.New("workflow run identity differs from the selected run")
	}
	runRepo, err := contract.ObjectAt(runSource, "repository")
	if err != nil {
		return nil, errors.New("workflow run has no repository identity")
	}
	fullName, err := contract.Nonempty(runRepo, "full_name")
	if err != nil || !strings.EqualFold(fullName, repository.FullName()) {
		return nil, errors.New("workflow run belongs to another repository")
	}
	runRepoNodeID, err := contract.Nonempty(runRepo, "node_id")
	if err != nil || runRepoNodeID != repoNodeID {
		return nil, errors.New("workflow run immutable repository identity differs from the selected repository")
	}
	rawURL, err := contract.Nonempty(runRepo, "html_url")
	if err != nil {
		return nil, errors.New("workflow run repository URL is unavailable")
	}
	parsedRunRepo, err := contract.ParseRepository(contract.Object{"nameWithOwner": fullName, "url": rawURL})
	if err != nil || parsedRunRepo != repository {
		return nil, errors.New("workflow run repository URL differs from the selected target")
	}

	pages, err := reader.RunArtifactPages(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, errors.New("artifact listing returned no page; completeness is unproven")
	}

	var totalCount int64 = -1
	artifacts := []any{}
	seenIDs := map[int64]bool{}
	for _, rawPage := range pages {
		page, ok := rawPage.(map[string]any)
		if !ok {
			return nil, errors.New("artifact listing contains a malformed page")
		}
		count, err := contract.Integer(page["total_count"])
		if err != nil || count < 0 {
			return nil, errors.New("artifact page total_count must be a nonnegative integer")
		}
		if totalCount == -1 {
			totalCount = count
		} else if totalCount != count {
			return nil, errors.New("artifact listing changed while pages were being read")
		}
		pageArtifacts, err := contract.Array(page, "artifacts")
		if err != nil {
			return nil, errors.New("artifact page has no valid artifact collection")
		}
		for _, rawArtifact := range pageArtifacts {
			artifact, ok := rawArtifact.(map[string]any)
			if !ok {
				return nil, errors.New("artifact listing contains a malformed artifact")
			}
			normalized, err := normalizeRunArtifact(artifact, runID)
			if err != nil {
				return nil, err
			}
			id, _ := contract.PositiveInteger(normalized["id"])
			if seenIDs[id] {
				return nil, errors.New("artifact listing contains a duplicate ID")
			}
			seenIDs[id] = true
			artifacts = append(artifacts, normalized)
		}
	}
	if totalCount != int64(len(artifacts)) {
		return nil, fmt.Errorf("artifact listing is truncated: received %d of %d", len(artifacts), totalCount)
	}
	sort.Slice(artifacts, func(i, j int) bool {
		a, _ := contract.PositiveInteger(artifacts[i].(map[string]any)["id"])
		b, _ := contract.PositiveInteger(artifacts[j].(map[string]any)["id"])
		return a < b
	})

	return contract.Object{
		"repository":  contract.Object{"host": repository.Host, "owner": repository.Owner, "name": repository.Name, "url": repository.URL, "node_id": repoNodeID},
		"run":         contract.Object{"id": runID, "repository_node_id": repoNodeID},
		"total_count": totalCount,
		"artifacts":   artifacts,
		"provenance":  contract.Object{"source": "github_api", "live": true, "complete": true},
	}, nil
}

func normalizeRunArtifact(raw contract.Object, runID int64) (contract.Object, error) {
	id, err := contract.PositiveInteger(raw["id"])
	if err != nil {
		return nil, errors.New("artifact ID must be a positive integer")
	}
	name, err := contract.String(raw, "name")
	if err != nil {
		return nil, errors.New("artifact name must be a string")
	}
	out := contract.Object{"id": id, "name": name}
	if value, exists := raw["size_in_bytes"]; exists {
		size, err := contract.Integer(value)
		if err != nil || size < 0 {
			return nil, errors.New("artifact size_in_bytes must be a nonnegative integer")
		}
		out["size_in_bytes"] = size
	}
	if value, exists := raw["expired"]; exists {
		expired, ok := value.(bool)
		if !ok {
			return nil, errors.New("artifact expired must be a boolean")
		}
		out["expired"] = expired
	}
	if value, exists := raw["workflow_run"]; exists {
		workflowRun, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("artifact workflow_run must be an object")
		}
		actualRunID, err := contract.PositiveInteger(workflowRun["id"])
		if err != nil || actualRunID != runID {
			return nil, errors.New("artifact workflow_run differs from the selected run")
		}
		normalized := contract.Object{"id": actualRunID}
		for _, key := range []string{"repository_id", "head_repository_id"} {
			if rawID, exists := workflowRun[key]; exists {
				value, err := contract.PositiveInteger(rawID)
				if err != nil {
					return nil, fmt.Errorf("artifact workflow_run %s must be a positive integer", key)
				}
				normalized[key] = value
			}
		}
		out["workflow_run"] = normalized
	}
	return out, nil
}
