package native

import (
	"context"
	"errors"
	"fmt"
)

const MaxActionsArchiveBytes = 32 * 1024 * 1024

// ActionsArtifactArchive is a bounded read of one immutable artifact in the
// selected repository. It exposes no arbitrary endpoint, method or output path.
func (t *Transport) ActionsArtifactArchive(ctx context.Context, id int64) ([]byte, error) {
	if id < 1 {
		return nil, errors.New("Actions artifact ID must be a positive integer")
	}
	endpoint := fmt.Sprintf("repos/%s/actions/artifacts/%d/zip", t.Repository.FullName(), id)
	if err := t.ValidateRESTEndpoint(endpoint); err != nil {
		return nil, err
	}
	reader := *t
	if _, nativeProcess := reader.Executor.(ProcessExecutor); nativeProcess {
		reader.Executor = ProcessExecutor{outputLimit: MaxActionsArchiveBytes}
	}
	result, err := reader.run(ctx, []string{"api", endpoint, "--hostname", t.Repository.Host, "--method", "GET"}, nil, false)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("Actions artifact download failed with exit code %d", result.ExitCode)
	}
	if len(result.Stdout) > MaxActionsArchiveBytes {
		return nil, errors.New("Actions artifact archive exceeds the supported size")
	}
	return result.Stdout, nil
}
