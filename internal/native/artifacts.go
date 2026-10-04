package native

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// RunArtifactPages returns every REST page for one run in the selected
// repository. Snapshot normalization validates the page envelopes and the
// complete artifact inventory.
func (t *Transport) RunArtifactPages(ctx context.Context, runID int64) ([]any, error) {
	if runID < 1 {
		return nil, errors.New("workflow run identity must be positive")
	}
	endpoint := fmt.Sprintf("repos/%s/actions/runs/%d/artifacts?per_page=100", t.Repository.FullName(), runID)
	return t.RESTPages(ctx, endpoint)
}

// ReadRunArtifact binds a repository-scoped artifact ID to its immutable run
// identity before a mutation. The caller still validates the complete run
// inventory and reviewed name before dispatch.
func (t *Transport) ReadRunArtifact(ctx context.Context, artifactID int64) (contract.Object, error) {
	if artifactID < 1 {
		return nil, errors.New("artifact identity must be positive")
	}
	artifact, err := t.REST(ctx, "GET", fmt.Sprintf("repos/%s/actions/artifacts/%d", t.Repository.FullName(), artifactID), nil)
	if err != nil {
		return nil, err
	}
	actualID, err := contract.PositiveInteger(artifact["id"])
	if err != nil || actualID != artifactID {
		return nil, errors.New("artifact read identity differs from the selected artifact")
	}
	if _, err := contract.String(artifact, "name"); err != nil {
		return nil, errors.New("artifact read has no valid name")
	}
	workflowRun, err := contract.ObjectAt(artifact, "workflow_run")
	if err != nil {
		return nil, errors.New("artifact read has no workflow run identity")
	}
	runID, err := contract.PositiveInteger(workflowRun["id"])
	if err != nil {
		return nil, errors.New("artifact read has an invalid workflow run identity")
	}
	return contract.Object{"id": actualID, "name": artifact["name"], "run_id": runID}, nil
}

// DeleteRunArtifact deletes one artifact already selected from a reviewed
// run-scoped inventory. The API identifies the write by artifact ID, so the
// workflow adapter rechecks the run inventory immediately before dispatch.
// The native acknowledgement is accepted only for GitHub's documented 204
// empty response; gh's zero exit code alone is insufficient evidence.
func (t *Transport) DeleteRunArtifact(ctx context.Context, runID, artifactID int64) (contract.Object, error) {
	if runID < 1 || artifactID < 1 {
		return nil, errors.New("artifact deletion requires positive run and artifact identities")
	}
	artifact, err := t.ReadRunArtifact(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	actualRunID, _ := contract.PositiveInteger(artifact["run_id"])
	if actualRunID != runID {
		return nil, errors.New("artifact ID belongs to another workflow run")
	}
	endpoint := fmt.Sprintf("repos/%s/actions/artifacts/%d", t.Repository.FullName(), artifactID)
	if err := t.ValidateRESTEndpoint(endpoint); err != nil {
		return nil, err
	}
	result, err := t.run(ctx, []string{"api", endpoint, "--hostname", t.Repository.Host, "--method", "DELETE", "--include"}, nil, false)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("native artifact deletion failed with exit code %d", result.ExitCode)
	}
	if err := validateEmpty204(result.Stdout); err != nil {
		return nil, err
	}
	return contract.Object{"run_id": runID, "artifact_id": artifactID, "status_code": int64(204), "no_content": true}, nil
}

func validateEmpty204(response []byte) error {
	separator := []byte("\r\n\r\n")
	index := strings.Index(string(response), string(separator))
	separatorSize := len(separator)
	if index < 0 {
		separator = []byte("\n\n")
		index = strings.Index(string(response), string(separator))
		separatorSize = len(separator)
	}
	if index < 0 {
		return errors.New("artifact deletion did not return an HTTP status and header block")
	}
	if index+separatorSize != len(response) {
		return errors.New("artifact deletion returned a body; the 204 acknowledgement is not empty")
	}
	lines := strings.Split(strings.ReplaceAll(string(response[:index]), "\r\n", "\n"), "\n")
	if len(lines) < 2 {
		return errors.New("artifact deletion response headers are incomplete")
	}
	status := strings.Fields(lines[0])
	if len(status) < 3 || !strings.HasPrefix(status[0], "HTTP/") {
		return errors.New("artifact deletion response status line is malformed")
	}
	code, err := strconv.Atoi(status[1])
	if err != nil || code != 204 || strings.Join(status[2:], " ") != "No Content" {
		return errors.New("artifact deletion did not positively acknowledge HTTP 204 No Content")
	}
	for _, header := range lines[1:] {
		if strings.TrimSpace(header) == "" || !strings.Contains(header, ":") {
			return errors.New("artifact deletion response contains a malformed header")
		}
	}
	return nil
}
