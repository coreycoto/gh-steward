package runrecovery

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

// ActionsReader has read-only operations; recovery never dispatches a write.
type ActionsReader interface {
	Read(context.Context, string) (Object, error)
	Pages(context.Context, string) ([]any, error)
	Archive(context.Context, int64) ([]byte, error)
}

type NativeActionsReader struct{ Transport *native.Transport }

// ExecutedWorkflowReader is optional for ordinary recovery. Diagnostic hold
// qualification requires this authenticated, typed GraphQL source witness.
type ExecutedWorkflowReader interface {
	WorkflowRunFile(context.Context, string, int64, string) (Object, error)
}

func (r NativeActionsReader) WorkflowRunFile(ctx context.Context, workflow string, id int64, nodeID string) (Object, error) {
	return r.Transport.ReadWorkflowRunFile(ctx, workflow, id, nodeID)
}

func (r NativeActionsReader) Read(ctx context.Context, endpoint string) (Object, error) {
	return r.Transport.REST(ctx, "GET", endpoint, nil)
}
func (r NativeActionsReader) Pages(ctx context.Context, endpoint string) ([]any, error) {
	return r.Transport.RESTPages(ctx, endpoint)
}
func (r NativeActionsReader) Archive(ctx context.Context, id int64) ([]byte, error) {
	return r.Transport.ActionsArtifactArchive(ctx, id)
}

var workflowFilename = regexp.MustCompile(`^[A-Za-z0-9._-]+\.(?:yml|yaml)$`)

func completePages(pages []any, key string) ([]Object, error) {
	if len(pages) == 0 {
		return nil, errors.New("complete Actions inventory requires at least one page")
	}
	var total int64 = -1
	rows, seen := []Object{}, map[int64]bool{}
	for _, raw := range pages {
		page, ok := raw.(Object)
		if !ok {
			return nil, errors.New("Actions inventory page is not an object")
		}
		count, err := contract.Integer(page["total_count"])
		if err != nil || count < 0 || (total >= 0 && count != total) {
			return nil, errors.New("Actions inventory has inconsistent totals")
		}
		total = count
		items, err := contract.Objects(page, key)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			id, err := contract.PositiveInteger(item["id"])
			if err != nil || seen[id] {
				return nil, errors.New("Actions inventory contains an invalid or duplicate ID")
			}
			seen[id] = true
			rows = append(rows, item)
		}
	}
	if int64(len(rows)) != total {
		return nil, errors.New("Actions inventory is incomplete")
	}
	return rows, nil
}

func CompleteWorkflowRuns(ctx context.Context, reader ActionsReader, repository contract.Repository, workflow string) ([]Object, error) {
	if !workflowFilename.MatchString(workflow) {
		return nil, errors.New("workflow filename is invalid")
	}
	pages, err := reader.Pages(ctx, fmt.Sprintf("repos/%s/actions/workflows/%s/runs?per_page=100", repository.FullName(), workflow))
	if err != nil {
		return nil, err
	}
	runs, err := completePages(pages, "workflow_runs")
	if err != nil {
		return nil, err
	}
	for _, run := range runs {
		if _, err := NormalizeRun(run); err != nil {
			return nil, err
		}
		if _, err := contract.PositiveInteger(run["run_attempt"]); err != nil {
			return nil, err
		}
		if _, err := contract.Nonempty(run, "status"); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

func CompleteRepositoryArtifacts(ctx context.Context, reader ActionsReader, repository contract.Repository) ([]Object, error) {
	pages, err := reader.Pages(ctx, "repos/"+repository.FullName()+"/actions/artifacts?per_page=100")
	if err != nil {
		return nil, err
	}
	return completePages(pages, "artifacts")
}

func artifactIdentity(metadata Object, expectedName string, runID int64) (Object, error) {
	id, err := contract.PositiveInteger(metadata["id"])
	if err != nil {
		return nil, err
	}
	workflowRun, err := contract.ObjectAt(metadata, "workflow_run")
	if err != nil {
		return nil, err
	}
	actualRun, err := contract.PositiveInteger(workflowRun["id"])
	if err != nil || actualRun != runID || metadata["name"] != expectedName || metadata["expired"] != false {
		return nil, errors.New("artifact is expired or belongs to another exact attempt")
	}
	digest, err := contract.String(metadata, "digest")
	if err != nil || len(digest) != 71 || digest[:7] != "sha256:" || !IsSHA256(digest[7:]) {
		return nil, errors.New("artifact has no qualified SHA-256 identity")
	}
	return Object{"name": expectedName, "id": id, "digest": digest}, nil
}

func downloadArtifact(ctx context.Context, reader ActionsReader, artifact Object) ([]byte, error) {
	id, err := contract.PositiveInteger(artifact["id"])
	if err != nil {
		return nil, err
	}
	data, err := reader.Archive(ctx, id)
	if err != nil {
		return nil, err
	}
	digest, err := contract.String(artifact, "digest")
	if err != nil || digest != "sha256:"+SHA256(data) {
		return nil, errors.New("downloaded artifact bytes differ from GitHub digest")
	}
	return data, nil
}

func objectRows(rows []Object) []any {
	result := make([]any, len(rows))
	for index := range rows {
		result[index] = rows[index]
	}
	return result
}

func objectArray(value any, name string) ([]Object, error) {
	rows, err := array(value, name)
	if err != nil {
		return nil, err
	}
	objects := make([]Object, len(rows))
	for index, raw := range rows {
		objects[index], err = object(raw, name+" row")
		if err != nil {
			return nil, err
		}
	}
	return objects, nil
}
