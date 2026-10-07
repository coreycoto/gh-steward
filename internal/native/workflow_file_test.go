package native

import (
	"context"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func workflowFileFixture() contract.Object {
	return contract.Object{"id": "WFR_7", "databaseId": int64(7), "event": "workflow_run", "runAttempt": int64(1), "url": "https://github.com/example/widgets/actions/runs/7",
		"workflow": contract.Object{"id": "W_9", "databaseId": int64(9), "url": "https://github.com/example/widgets/actions/workflows/task.yml"},
		"file":     contract.Object{"id": "WF_7", "path": ".github/workflows/task.yml", "repositoryName": "example/widgets", "repositoryFileUrl": "https://github.com/example/widgets/blob/" + strings.Repeat("b", 40) + "/.github/workflows/task.yml", "url": "https://github.com/example/widgets/actions/runs/7/workflow", "run": contract.Object{"id": "WFR_7", "databaseId": int64(7)}}}
}

func TestExecutedWorkflowQueryUsesTypedRunAndQualifiedFile(t *testing.T) {
	data, err := contract.Canonical(contract.Object{"data": contract.Object{"node": workflowFileFixture()}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeExecutor{result: Result{Stdout: data}}
	tpt := transport(f)
	value, err := tpt.ReadWorkflowRunFile(context.Background(), "task.yml", 7, "WFR_7")
	if err != nil || value["id"] != "WFR_7" {
		t.Fatal(value, err)
	}
	request, err := contract.Decode(strings.NewReader(string(f.input)))
	variables, _ := contract.ObjectAt(request, "variables")
	query, _ := contract.String(request, "query")
	if err != nil || variables["id"] != "WFR_7" || !strings.Contains(query, "repositoryFileUrl") || f.root != "/synthetic/checkout" || !strings.Contains(strings.Join(f.args, " "), "--hostname github.com") {
		t.Fatal("query lost its exact source or target", request, f.args, err)
	}
	for _, bad := range []string{"../task.yml", "task.yml?ref=other"} {
		before := f.calls
		if _, err := tpt.ReadWorkflowRunFile(context.Background(), bad, 7, "WFR_7"); err == nil || f.calls != before {
			t.Fatal("invalid workflow reached provider")
		}
	}
}

func TestExecutedWorkflowWitnessRejectsMissingForeignAndMovingSources(t *testing.T) {
	for _, name := range []string{"missing", "run", "node", "parent", "repository", "path", "branch", "host", "query", "fragment", "workflow"} {
		t.Run(name, func(t *testing.T) {
			node := workflowFileFixture()
			file := node["file"].(contract.Object)
			switch name {
			case "missing":
				node["file"] = nil
			case "run":
				node["databaseId"] = int64(8)
			case "node":
				node["id"] = "WFR_8"
			case "parent":
				file["run"].(contract.Object)["id"] = "WFR_8"
			case "repository":
				file["repositoryName"] = "other/widgets"
			case "path":
				file["path"] = ".github/workflows/other.yml"
			case "branch":
				file["repositoryFileUrl"] = "https://github.com/example/widgets/blob/main/.github/workflows/task.yml"
			case "host":
				file["repositoryFileUrl"] = strings.Replace(file["repositoryFileUrl"].(string), "github.com", "evil.example", 1)
			case "query":
				file["repositoryFileUrl"] = file["repositoryFileUrl"].(string) + "?raw=1"
			case "fragment":
				file["repositoryFileUrl"] = file["repositoryFileUrl"].(string) + "#L1"
			case "workflow":
				node["workflow"].(contract.Object)["url"] = "https://github.com/example/widgets/actions/workflows/other.yml"
			}
			if _, err := WorkflowRunFileCommit(target(), "task.yml", 7, "WFR_7", node); err == nil {
				t.Fatal("unqualified executed source was accepted")
			}
		})
	}
}
