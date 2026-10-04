package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/snapshot"
)

func graphQLWorkflowProjectFields() []contract.Object {
	return []contract.Object{
		{"id": "FIELD_STATUS", "name": "Status", "dataType": "SINGLE_SELECT", "options": []any{
			contract.Object{"id": "OPT_TODO", "name": "Todo"},
			contract.Object{"id": "OPT_PROGRESS", "name": "In Progress"},
			contract.Object{"id": "OPT_DONE", "name": "Done"},
		}},
		{"id": "FIELD_PRIORITY", "name": "Priority", "dataType": "SINGLE_SELECT", "options": []any{
			contract.Object{"id": "OPT_NOW", "name": "Now"},
			contract.Object{"id": "OPT_NEXT", "name": "Next"},
			contract.Object{"id": "OPT_LATER", "name": "Later"},
		}},
		{"id": "FIELD_ORDER", "name": "Queue Order", "dataType": "NUMBER", "options": []any{}},
	}
}

func normalizeWorkflowProjectFields(t *testing.T, raw []contract.Object) contract.Object {
	t.Helper()
	fields, err := snapshot.NormalizeFields(raw)
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestBacklogMutationPreparationConsumesSnapshotNormalizedProjectFields(t *testing.T) {
	provider := newBacklogMutationTestProvider()
	project := mustObjects(backlogStateInventory(provider.state), "projects")[0]
	project["fields_by_name"] = normalizeWorkflowProjectFields(t, graphQLWorkflowProjectFields())
	intent := contract.Object{"schema_version": int64(1), "issues": []any{
		contract.Object{"issue_number": int64(1), "project": contract.Object{
			"title": "Backlog", "status": "In Progress", "priority": "Next", "queue_order": 20,
		}},
	}}

	plan, err := PrepareBacklogMutations(context.Background(), provider, testRepo(), intent, []ProjectScope{backlogMutationProjectScope()}, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{}
	for _, op := range plan.Operations {
		if op.Kind == "project-field-set" {
			values[op.Target["field_name"].(string)] = op.After["value"]
		}
	}
	queueOrder, queueOrderErr := contract.Number(values["Queue Order"])
	if values["Status"] != "In Progress" || values["Priority"] != "Next" || queueOrderErr != nil || queueOrder != 20 {
		t.Fatalf("normalized Project field definitions did not prepare all typed values: %#v", values)
	}
}

func TestBacklogMutationPreparationRejectsInvalidNormalizedProjectFields(t *testing.T) {
	tests := []struct {
		name   string
		fields func(t *testing.T) contract.Object
		status string
	}{
		{
			name: "wrong field data type",
			fields: func(t *testing.T) contract.Object {
				raw := graphQLWorkflowProjectFields()
				raw[0]["dataType"] = "NUMBER"
				return normalizeWorkflowProjectFields(t, raw)
			},
			status: "In Progress",
		},
		{
			name: "unknown option",
			fields: func(t *testing.T) contract.Object {
				raw := graphQLWorkflowProjectFields()
				raw[0]["options"] = []any{
					contract.Object{"id": "OPT_TODO", "name": "Todo"},
					contract.Object{"id": "OPT_PROGRESS", "name": "In Progress"},
				}
				return normalizeWorkflowProjectFields(t, raw)
			},
			status: "Done",
		},
		{
			name: "missing normalized data type",
			fields: func(t *testing.T) contract.Object {
				fields := normalizeWorkflowProjectFields(t, graphQLWorkflowProjectFields())
				delete(fields["Status"].(map[string]any), "data_type")
				return fields
			},
			status: "In Progress",
		},
		{
			name: "wrong normalized data type shape",
			fields: func(t *testing.T) contract.Object {
				fields := normalizeWorkflowProjectFields(t, graphQLWorkflowProjectFields())
				fields["Status"].(map[string]any)["data_type"] = int64(1)
				return fields
			},
			status: "In Progress",
		},
		{
			name: "malformed unselected normalized option",
			fields: func(t *testing.T) contract.Object {
				fields := normalizeWorkflowProjectFields(t, graphQLWorkflowProjectFields())
				options := fields["Status"].(map[string]any)["options_by_name"].(map[string]any)
				options["Broken"] = contract.Object{"name": "Broken"}
				return fields
			},
			status: "In Progress",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := newBacklogMutationTestProvider()
			project := mustObjects(backlogStateInventory(provider.state), "projects")[0]
			project["fields_by_name"] = test.fields(t)
			intent := contract.Object{"schema_version": int64(1), "issues": []any{
				contract.Object{"issue_number": int64(1), "project": contract.Object{"title": "Backlog", "status": test.status}},
			}}
			if _, err := PrepareBacklogMutations(context.Background(), provider, testRepo(), intent, []ProjectScope{backlogMutationProjectScope()}, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)); err == nil {
				t.Fatal("malformed Project field inventory prepared a plan")
			}
		})
	}
}

func TestBacklogMutationSnapshotNormalizationRejectsMissingTypeAndMalformedOption(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]contract.Object)
	}{
		{
			name: "missing GraphQL dataType",
			mutate: func(fields []contract.Object) {
				delete(fields[0], "dataType")
			},
		},
		{
			name: "malformed GraphQL option identity",
			mutate: func(fields []contract.Object) {
				fields[0]["options"] = []any{contract.Object{"name": "Todo"}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := graphQLWorkflowProjectFields()
			test.mutate(raw)
			if _, err := snapshot.NormalizeFields(raw); err == nil {
				t.Fatal("malformed GraphQL Project fields were normalized")
			}
		})
	}
}

func TestExecutionPreparationConsumesAndValidatesSnapshotNormalizedProjectFields(t *testing.T) {
	selector := ExecutionSelector{IssueNumber: 17, Project: ptrProject(executionTestProject())}
	policy := executionTestPolicy()
	makeProvider := func(t *testing.T, fields contract.Object) *executionTestProvider {
		t.Helper()
		inventory := executionTestRawInventory(policy, selector, "OPEN", true, true)
		project := inventory["project_inventory"].(map[string]any)
		project["fields_by_name"] = fields
		return newExecutionTestProvider(inventory)
	}
	fields := normalizeWorkflowProjectFields(t, graphQLWorkflowProjectFields())
	provider := makeProvider(t, fields)
	plan, err := PrepareExecutionSync(context.Background(), provider, testRepo(), selector, policy, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	ops, err := (ExecutionSync{}).Operations(plan)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, op := range ops {
		if op.Kind == "project-field-set" && op.Target["field_name"] == "Status" && op.After["value"] == "In Progress" {
			found = true
		}
	}
	if !found {
		t.Fatalf("execution plan omitted the normalized Status update: %#v", ops)
	}

	for _, test := range []struct {
		name   string
		fields func(t *testing.T) contract.Object
		active string
	}{
		{
			name: "wrong normalized field type",
			fields: func(t *testing.T) contract.Object {
				out := normalizeWorkflowProjectFields(t, graphQLWorkflowProjectFields())
				out["Status"].(map[string]any)["data_type"] = "NUMBER"
				return out
			},
			active: "In Progress",
		},
		{
			name: "unknown normalized option",
			fields: func(t *testing.T) contract.Object {
				out := normalizeWorkflowProjectFields(t, graphQLWorkflowProjectFields())
				delete(out["Status"].(map[string]any)["options_by_name"].(map[string]any), "In Progress")
				return out
			},
			active: "In Progress",
		},
		{
			name: "missing normalized type",
			fields: func(t *testing.T) contract.Object {
				out := normalizeWorkflowProjectFields(t, graphQLWorkflowProjectFields())
				delete(out["Status"].(map[string]any), "data_type")
				return out
			},
			active: "In Progress",
		},
		{
			name: "malformed normalized option",
			fields: func(t *testing.T) contract.Object {
				out := normalizeWorkflowProjectFields(t, graphQLWorkflowProjectFields())
				out["Status"].(map[string]any)["options_by_name"].(map[string]any)["Broken"] = contract.Object{"name": "Broken"}
				return out
			},
			active: "In Progress",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			testPolicy := policy
			testPolicy.Statuses.Active = test.active
			if _, err := PrepareExecutionSync(context.Background(), makeProvider(t, test.fields(t)), testRepo(), selector, testPolicy, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)); err == nil {
				t.Fatal("invalid normalized Project field inventory prepared an execution plan")
			}
		})
	}
}
