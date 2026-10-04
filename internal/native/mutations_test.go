package native

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

type sequenceExecutor struct {
	responses []Result
	calls     [][]string
	inputs    []contract.Object
	err       error
}

func (s *sequenceExecutor) Execute(_ context.Context, _ string, args []string, input []byte, _ string, _ []string) (Result, error) {
	s.calls = append(s.calls, args)
	if len(input) > 0 {
		value, err := contract.Decode(strings.NewReader(string(input)))
		if err != nil {
			return Result{}, err
		}
		s.inputs = append(s.inputs, value)
	}
	if s.err != nil {
		return Result{}, s.err
	}
	if len(s.responses) == 0 {
		return Result{}, errors.New("unexpected provider call")
	}
	r := s.responses[0]
	s.responses = s.responses[1:]
	return r, nil
}
func response(body string) Result { return Result{Stdout: []byte(body)} }
func sequence(s *sequenceExecutor) *Transport {
	t := transport(&fakeExecutor{})
	t.Executor = s
	return t
}
func restIssue(number, id string) Result {
	return response(`{"number":` + number + `,"id":` + id + `,"node_id":"I_` + number + `","html_url":"https://github.com/example/widgets/issues/` + number + `"}`)
}

func TestRelationshipPrimitivesUseDatabaseIDsAndSingularRemovalEndpoint(t *testing.T) {
	s := &sequenceExecutor{responses: []Result{restIssue("17", "10017"), restIssue("23", "10023"), response(`{"id":10023}`)}}
	if _, err := sequence(s).RemoveRelationship(context.Background(), 17, 23, "child"); err != nil {
		t.Fatal(err)
	}
	if s.calls[2][1] != "repos/example/widgets/issues/17/sub_issue" || s.inputs[0]["sub_issue_id"] != json.Number("10023") {
		t.Fatal("sub-issue removal used issue number/plural endpoint", s.calls, s.inputs)
	}
	s = &sequenceExecutor{responses: []Result{restIssue("17", "10017"), restIssue("23", "10023"), {}}}
	if _, err := sequence(s).RemoveRelationship(context.Background(), 17, 23, "blocked-by"); err != nil {
		t.Fatal(err)
	}
	if s.calls[2][1] != "repos/example/widgets/issues/17/dependencies/blocked_by/10023" {
		t.Fatal("dependency removal lost database identity")
	}
}

func TestCorrelatedCreateAndCommentRejectForeignOrUncorrelatedResults(t *testing.T) {
	id := strings.Repeat("a", 32)
	marker, _ := OperationMarker(id)
	draft := contract.Object{"title": "Fix edge case", "body": "Reviewed description", "labels": []any{"bug"}}
	body := "Reviewed description\n\n" + marker
	r := response(`{"number":17,"id":10017,"node_id":"I_17","html_url":"https://github.com/example/widgets/issues/17","title":"Fix edge case","body":` + strconvJSON(body) + `}`)
	s := &sequenceExecutor{responses: []Result{r}}
	if _, err := sequence(s).CreateIssue(context.Background(), draft, id); err != nil {
		t.Fatal(err)
	}
	if draft["body"] != "Reviewed description" || s.inputs[0]["body"] != body {
		t.Fatal("caller intent mutated or correlation missing")
	}
	for _, bad := range []string{strings.Replace(string(r.Stdout), "example/widgets", "other/widgets", 1), strings.Replace(string(r.Stdout), "Reviewed description", "Other description", 1)} {
		s = &sequenceExecutor{responses: []Result{response(bad)}}
		if _, err := sequence(s).CreateIssue(context.Background(), draft, id); err == nil {
			t.Fatal("foreign/uncorrelated issue result accepted")
		}
	}
	comment := response(`{"id":31,"html_url":"https://github.com/example/widgets/issues/17#issuecomment-31","body":` + strconvJSON(body) + `}`)
	s = &sequenceExecutor{responses: []Result{comment}}
	if _, err := sequence(s).CreateIssueComment(context.Background(), 17, "Reviewed description", id); err != nil {
		t.Fatal(err)
	}
	comment.Stdout = []byte(strings.Replace(string(comment.Stdout), "issues/17", "issues/23", 1))
	s = &sequenceExecutor{responses: []Result{comment}}
	if _, err := sequence(s).CreateIssueComment(context.Background(), 17, "Reviewed description", id); err == nil {
		t.Fatal("same-repo wrong-issue comment accepted")
	}
}
func strconvJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestPrimitiveInputFailuresStopBeforeDispatch(t *testing.T) {
	s := &sequenceExecutor{}
	tpt := sequence(s)
	for _, draft := range []contract.Object{{"title": "", "body": ""}, {"title": "Work", "body": true}, {"title": "Work", "body": "", "state": "closed"}, {"title": "Work", "body": "", "milestone": true}, {"title": "Work", "body": "", "labels": []any{true}}, {"title": "Work", "body": "", "url": "foreign"}} {
		if _, err := tpt.CreateIssue(context.Background(), draft, strings.Repeat("a", 32)); err == nil {
			t.Fatal("malformed issue draft dispatched", draft)
		}
	}
	if _, err := tpt.CreateIssueComment(context.Background(), 17, "text", "not-a-durable-nonce"); err == nil {
		t.Fatal("uncorrelated write dispatched")
	}
	if _, err := tpt.AddRelationship(context.Background(), 17, 17, "child"); err == nil {
		t.Fatal("self relationship dispatched")
	}
	if len(s.calls) != 0 {
		t.Fatal("invalid inputs reached provider")
	}
}

func TestProjectFieldValuesUseTypedNumbersAndExactDefinitions(t *testing.T) {
	for _, n := range []any{json.Number("0"), json.Number("2.5"), int64(25)} {
		value, err := fieldInput(contract.Object{"dataType": "NUMBER"}, n)
		if err != nil || value["number"] != n {
			t.Fatal("number coerced", n, value, err)
		}
	}
	for _, n := range []any{true, "2", json.Number("NaN")} {
		if _, err := fieldInput(contract.Object{"dataType": "NUMBER"}, n); err == nil {
			t.Fatal("malformed number accepted", n)
		}
	}
	field := contract.Object{"dataType": "SINGLE_SELECT", "options": []any{contract.Object{"id": "O_1", "name": "Now"}}}
	if value, err := fieldInput(field, "Now"); err != nil || !reflect.DeepEqual(value, contract.Object{"singleSelectOptionId": "O_1"}) {
		t.Fatal(value, err)
	}
	if _, err := fieldInput(field, "Missing"); err == nil {
		t.Fatal("unqualified option accepted")
	}
	if _, err := fieldInput(contract.Object{"dataType": "DATE"}, "2026-02-31"); err == nil {
		t.Fatal("invalid date accepted")
	}
}

func TestProjectMutationRequiresQualifiedReturnedOwnerBeforeDispatch(t *testing.T) {
	id := strings.Repeat("a", 32)
	scope := ProjectScope{Host: "github.com", Owner: "planner", OwnerType: "User", Number: 14, ID: "P_14"}
	s := &sequenceExecutor{responses: []Result{response(`{"data":{"repositoryOwner":{"__typename":"User","login":"other","projectV2":{"id":"P_14","number":14,"url":"https://github.com/users/planner/projects/14","title":"Queue","closed":false,"public":false}}}}`)}}
	if _, err := sequence(s).AddProjectIssue(context.Background(), scope, 17, id); err == nil {
		t.Fatal("foreign Project owner accepted")
	}
	if len(s.calls) != 1 || strings.Contains(string(s.inputs[0]["query"].(string)), "mutation") {
		t.Fatal("foreign Project caused a mutation")
	}
}

func TestProjectMembershipRetainsNativeMutationAcknowledgement(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	scope := ProjectScope{Host: "github.com", Owner: "planner", OwnerType: "User", Number: 14, ID: "P_14"}
	project := response(`{"data":{"repositoryOwner":{"__typename":"User","login":"planner","projectV2":{"id":"P_14","number":14,"url":"https://github.com/users/planner/projects/14","title":"Queue","closed":false,"public":false}}}}`)
	issue := response(`{"data":{"repository":{"id":"R_1","nameWithOwner":"example/widgets","url":"https://github.com/example/widgets","issue":{"id":"I_17","number":17,"url":"https://github.com/example/widgets/issues/17"}}}}`)
	for _, returnedNonce := range []string{nonce, strings.Repeat("b", 32), ""} {
		payload := contract.Object{"clientMutationId": returnedNonce, "provider_extra": "retain", "item": contract.Object{
			"id": "M_17", "isArchived": false,
			"content":     contract.Object{"id": "I_17", "number": int64(17), "url": "https://github.com/example/widgets/issues/17"},
			"fieldValues": contract.Object{"nodes": []any{}, "pageInfo": contract.Object{"hasNextPage": false}},
		}}
		body, _ := contract.Canonical(contract.Object{"data": contract.Object{"addProjectV2ItemById": payload}})
		s := &sequenceExecutor{responses: []Result{project, issue, {Stdout: body}}}
		ack, err := sequence(s).AddProjectIssue(context.Background(), scope, 17, nonce)
		if returnedNonce != nonce {
			if err == nil {
				t.Fatal("uncorrelated native mutation accepted", ack)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, rawErr := contract.ObjectAt(ack, "native_payload")
		if rawErr != nil || ack["id"] != "M_17" || ack["clientMutationId"] != nonce || raw["clientMutationId"] != nonce || raw["provider_extra"] != "retain" {
			t.Fatal("native membership proof was discarded", ack, rawErr)
		}
		if s.inputs[2]["variables"].(map[string]any)["operation"] != nonce {
			t.Fatal("reviewed mutation correlation was not sent", s.inputs)
		}
	}
}
