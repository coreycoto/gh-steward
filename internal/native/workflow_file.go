package native

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var executedWorkflowName = regexp.MustCompile(`^[A-Za-z0-9._-]+\.(?:yml|yaml)$`)
var executedWorkflowCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

// WorkflowRun.file is GitHub's executed-file witness. In particular, this does
// not infer the workflow source from a trigger payload or a run's head SHA.
func (t *Transport) ReadWorkflowRunFile(ctx context.Context, workflow string, runID int64, nodeID string) (contract.Object, error) {
	if !executedWorkflowName.MatchString(workflow) || runID < 1 || nodeID == "" || len(nodeID) > 256 {
		return nil, errors.New("executed workflow requires an exact workflow and run identity")
	}
	data, err := t.graphQL(ctx, `query($id:ID!){node(id:$id){... on WorkflowRun{id databaseId event runAttempt url workflow{id databaseId url} file{id path repositoryName repositoryFileUrl url run{id databaseId}}}}}`, contract.Object{"id": nodeID})
	if err != nil {
		return nil, err
	}
	node, err := contract.ObjectAt(data, "node")
	if err != nil {
		return nil, errors.New("executed workflow witness is unavailable")
	}
	if _, err := WorkflowRunFileCommit(t.Repository, workflow, runID, nodeID, node); err != nil {
		return nil, err
	}
	return node, nil
}

// WorkflowRunFileCommit validates both live and retained typed witnesses. A
// moving branch URL, missing file, foreign repository or unbound node holds.
func WorkflowRunFileCommit(repository contract.Repository, workflow string, runID int64, nodeID string, node contract.Object) (string, error) {
	base := "https://" + repository.Host + "/" + repository.FullName()
	actualID, err := contract.PositiveInteger(node["databaseId"])
	if !executedWorkflowName.MatchString(workflow) || nodeID == "" || err != nil || actualID != runID || node["id"] != nodeID || node["url"] != fmt.Sprintf("%s/actions/runs/%d", base, runID) {
		return "", errors.New("executed workflow witness belongs to another run")
	}
	definition, err := contract.ObjectAt(node, "workflow")
	if err != nil || definition["url"] != base+"/actions/workflows/"+workflow {
		return "", errors.New("executed workflow definition differs from the target")
	}
	file, err := contract.ObjectAt(node, "file")
	if err != nil || file["repositoryName"] != repository.FullName() || file["path"] != ".github/workflows/"+workflow || file["url"] != fmt.Sprintf("%s/actions/runs/%d/workflow", base, runID) {
		return "", errors.New("executed workflow file is missing or foreign")
	}
	parent, err := contract.ObjectAt(file, "run")
	parentID, idErr := contract.PositiveInteger(parent["databaseId"])
	if err != nil || idErr != nil || parentID != runID || parent["id"] != nodeID {
		return "", errors.New("executed workflow file is not bound to its parent run")
	}
	link, ok := file["repositoryFileUrl"].(string)
	prefix, suffix := base+"/blob/", "/.github/workflows/"+workflow
	if !ok || !strings.HasPrefix(link, prefix) || !strings.HasSuffix(link, suffix) {
		return "", errors.New("executed workflow file has no immutable source URL")
	}
	sha := strings.TrimSuffix(strings.TrimPrefix(link, prefix), suffix)
	if !executedWorkflowCommit.MatchString(sha) {
		return "", errors.New("executed workflow file URL is not a full commit identity")
	}
	return sha, nil
}
