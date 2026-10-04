package native

import (
	"context"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func milestoneResponse(body string) Result {
	return response(`{"number":3,"id":10003,"node_id":"M_3","html_url":"https://github.com/example/widgets/milestone/3","title":"2026 Q4","description":` + strconvJSON(body) + `,"state":"open","due_on":null}`)
}

func TestMilestoneCreationIsCorrelatedAndRepositoryScoped(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	marker, _ := OperationMarker(nonce)
	draft := contract.Object{"title": "2026 Q4", "description": "Reviewed quarter", "due_on": nil}
	s := &sequenceExecutor{responses: []Result{milestoneResponse("Reviewed quarter\n\n" + marker)}}
	if _, err := sequence(s).CreateMilestone(context.Background(), draft, nonce); err != nil {
		t.Fatal(err)
	}
	if draft["description"] != "Reviewed quarter" || s.inputs[0]["description"] != "Reviewed quarter\n\n"+marker || s.calls[0][1] != "repos/example/widgets/milestones" {
		t.Fatal("creation lost immutable reviewed intent", s.inputs, s.calls)
	}
	for _, bad := range []Result{milestoneResponse("Other"), response(strings.Replace(string(milestoneResponse("Reviewed quarter\n\n"+marker).Stdout), "example/widgets", "foreign/widgets", 1))} {
		s = &sequenceExecutor{responses: []Result{bad}}
		if _, err := sequence(s).CreateMilestone(context.Background(), draft, nonce); err == nil {
			t.Fatal("foreign or uncorrelated response accepted")
		}
	}
}

func TestMilestoneUpdateChecksImmutableIdentityBeforeAndAfterWrite(t *testing.T) {
	s := &sequenceExecutor{responses: []Result{milestoneResponse("Old"), milestoneResponse("New")}}
	if _, err := sequence(s).UpdateMilestone(context.Background(), 3, contract.Object{"description": "New"}); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 2 || s.calls[1][1] != "repos/example/widgets/milestones/3" {
		t.Fatal(s.calls)
	}
	for _, index := range []int{0, 1} {
		responses := []Result{milestoneResponse("Old"), milestoneResponse("New")}
		responses[index].Stdout = []byte(strings.Replace(string(responses[index].Stdout), `"node_id":"M_3"`, `"node_id":"M_other"`, 1))
		if index == 0 {
			responses[index].Stdout = []byte(strings.Replace(string(responses[index].Stdout), "milestone/3", "milestone/4", 1))
		}
		s = &sequenceExecutor{responses: responses}
		if _, err := sequence(s).UpdateMilestone(context.Background(), 3, contract.Object{"description": "New"}); err == nil {
			t.Fatal("foreign milestone accepted", index)
		}
		if index == 0 && len(s.calls) != 1 {
			t.Fatal("unqualified prior identity caused a write")
		}
	}
}

func TestInvalidMilestoneIntentStopsBeforeDispatch(t *testing.T) {
	s := &sequenceExecutor{}
	for _, draft := range []contract.Object{{"title": " ", "description": ""}, {"title": "Q", "description": true}, {"title": "Q", "description": "", "due_on": "2026-02-31"}, {"title": "Q", "description": "", "due_on": true}, {"title": "Q", "description": "", "number": 3}, {"title": "Q", "description": "", "state": "OPEN"}} {
		if _, err := sequence(s).CreateMilestone(context.Background(), draft, strings.Repeat("a", 32)); err == nil {
			t.Fatal("invalid milestone intent accepted", draft)
		}
	}
	if len(s.calls) != 0 {
		t.Fatal("invalid intent reached provider")
	}
}
