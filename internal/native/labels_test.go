package native

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func labelRepository() contract.Object {
	return contract.Object{"id": "R_1", "nameWithOwner": "example/widgets", "url": "https://github.com/example/widgets"}
}

func labelObject() contract.Object {
	return contract.Object{"id": "LA_1", "name": "bug", "color": "A1B2C3", "description": "Defect", "repository": labelRepository()}
}

func labelResult(value contract.Object) Result {
	body, err := contract.Canonical(contract.Object{"data": value})
	if err != nil {
		panic(err)
	}
	return Result{Stdout: body}
}

func labelRead() Result {
	repo := labelRepository()
	repo["label"] = labelObject()
	return labelResult(contract.Object{"repository": repo})
}

func TestLabelPrimitivesRetainNativeProofAndTypedInput(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	draft := contract.Object{"name": "bug", "color": "a1b2c3", "description": "Defect"}
	for _, update := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "update"}[update], func(t *testing.T) {
			key, queryType := "createLabel", "CreateLabelInput!"
			prior := labelResult(contract.Object{"repository": labelRepository()})
			if update {
				key, queryType, prior = "updateLabel", "UpdateLabelInput!", labelRead()
			}
			payload := contract.Object{"clientMutationId": nonce, "label": labelObject(), "provider_extra": "keep"}
			exec := &sequenceExecutor{responses: []Result{prior, labelResult(contract.Object{key: payload})}}
			var ack contract.Object
			var err error
			if update {
				ack, err = sequence(exec).UpdateLabel(context.Background(), "LA_1", draft, nonce)
			} else {
				ack, err = sequence(exec).CreateLabel(context.Background(), draft, nonce)
			}
			if err != nil || ack["provider_extra"] != "keep" || ack["clientMutationId"] != nonce {
				t.Fatal("native mutation proof was not retained", ack, err)
			}
			if len(exec.calls) != 2 || !strings.Contains(exec.inputs[1]["query"].(string), queryType) {
				t.Fatal("label primitive changed its typed native dispatch", exec.inputs)
			}
			input := exec.inputs[1]["variables"].(map[string]any)["input"].(map[string]any)
			if input["clientMutationId"] != nonce || input["name"] != "bug" || input["color"] != "a1b2c3" || input["description"] != "Defect" {
				t.Fatal("reviewed draft or correlation changed", input)
			}
			if update && (input["id"] != "LA_1" || len(input) != 5) {
				t.Fatal("update lost the immutable label identity", input)
			}
			if !update && (input["repositoryId"] != "R_1" || len(input) != 5) {
				t.Fatal("create lost the immutable repository identity", input)
			}
			// GitHub Labels expose global node IDs, not database IDs.
			if strings.Contains(exec.inputs[1]["query"].(string), "databaseId") {
				t.Fatal("unsupported Label field requested")
			}
		})
	}
}

func TestLabelAcknowledgementRejectsUncorrelatedOrChangedResults(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	draft := contract.Object{"name": "bug", "color": "a1b2c3", "description": "Defect"}
	for _, kind := range []string{"nonce", "missing_nonce", "name", "color", "description", "missing_description", "node", "repository_node", "foreign_repository", "missing_label", "partial_graphql"} {
		t.Run(kind, func(t *testing.T) {
			label := labelObject()
			payload := contract.Object{"clientMutationId": nonce, "label": label}
			switch kind {
			case "nonce":
				payload["clientMutationId"] = strings.Repeat("b", 32)
			case "missing_nonce":
				delete(payload, "clientMutationId")
			case "name":
				label["name"] = "feature"
			case "color":
				label["color"] = "ffffff"
			case "description":
				label["description"] = "Another purpose"
			case "missing_description":
				delete(label, "description")
			case "node":
				label["id"] = "LA_other"
			case "repository_node":
				label["repository"].(contract.Object)["id"] = "R_other"
			case "foreign_repository":
				label["repository"] = contract.Object{"id": "R_1", "nameWithOwner": "other/widgets", "url": "https://github.com/other/widgets"}
			case "missing_label":
				delete(payload, "label")
			}
			mutation := labelResult(contract.Object{"updateLabel": payload})
			if kind == "partial_graphql" {
				body, _ := json.Marshal(contract.Object{"data": contract.Object{"updateLabel": payload}, "errors": []any{contract.Object{"message": "permission denied"}}})
				mutation = Result{Stdout: body}
			}
			exec := &sequenceExecutor{responses: []Result{labelRead(), mutation}}
			if _, err := sequence(exec).UpdateLabel(context.Background(), "LA_1", draft, nonce); err == nil {
				t.Fatal("unqualified acknowledgement accepted")
			}
			if len(exec.calls) != 2 {
				t.Fatal("failed mutation was retried", exec.calls)
			}
		})
	}
}

func TestLabelNullDescriptionIsExplicitAndOnlyMatchesEmptyDraft(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	for _, description := range []string{"", "Defect"} {
		label := labelObject()
		label["description"] = nil
		exec := &sequenceExecutor{responses: []Result{labelResult(contract.Object{"repository": labelRepository()}), labelResult(contract.Object{"createLabel": contract.Object{"clientMutationId": nonce, "label": label}})}}
		_, err := sequence(exec).CreateLabel(context.Background(), contract.Object{"name": "bug", "color": "a1b2c3", "description": description}, nonce)
		if (err == nil) != (description == "") {
			t.Fatal("nullable description changed reviewed semantics", description, err)
		}
	}
}

func TestLabelDraftAndScopeFailuresStopBeforeMutation(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	valid := contract.Object{"name": "bug", "color": "a1b2c3", "description": "Defect"}
	invalid := []contract.Object{
		{"name": " bug", "color": "a1b2c3", "description": ""},
		{"name": "bug\nfeature", "color": "a1b2c3", "description": ""},
		{"name": "bug", "color": "#a1b2c3", "description": ""},
		{"name": "bug", "color": "a1b2c3", "description": nil},
		{"name": "bug", "color": "a1b2c3", "description": "", "state": "closed"},
	}
	exec := &sequenceExecutor{}
	for _, draft := range invalid {
		if _, err := sequence(exec).CreateLabel(context.Background(), draft, nonce); err == nil {
			t.Fatal("invalid label draft dispatched", draft)
		}
	}
	if _, err := sequence(exec).CreateLabel(context.Background(), valid, "invalid"); err == nil {
		t.Fatal("invalid correlation dispatched")
	}
	if _, err := sequence(exec).UpdateLabel(context.Background(), "", valid, nonce); err == nil {
		t.Fatal("missing identity dispatched")
	}
	if len(exec.calls) != 0 {
		t.Fatal("invalid input reached GitHub", exec.calls)
	}
	for _, prior := range []Result{labelRead(), labelResult(contract.Object{"repository": contract.Object{"id": "R_1", "nameWithOwner": "other/widgets", "url": "https://github.com/other/widgets", "label": labelObject()}})} {
		exec = &sequenceExecutor{responses: []Result{prior}}
		if _, err := sequence(exec).UpdateLabel(context.Background(), "LA_other", valid, nonce); err == nil {
			t.Fatal("foreign prior label accepted")
		}
		if len(exec.calls) != 1 {
			t.Fatal("unqualified prior scope dispatched a mutation", exec.calls)
		}
	}
}
