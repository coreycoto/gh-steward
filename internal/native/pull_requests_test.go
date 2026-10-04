package native

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestRequiredCheckReadDistinguishesProviderFailureFromPendingEvidence(t *testing.T) {
	for _, bucket := range []struct {
		code  int
		value string
	}{{0, "pass"}, {1, "fail"}, {8, "pending"}} {
		s := &sequenceExecutor{responses: []Result{{ExitCode: bucket.code, Stdout: []byte(fmt.Sprintf(`[{"name":"test","bucket":%q,"state":"COMPLETED","link":"https://github.com/example/widgets/actions/runs/8","workflow":"CI"}]`, bucket.value))}}}
		checks, err := sequence(s).RequiredChecks(context.Background(), 3)
		if err != nil || len(checks) != 1 || checks[0]["bucket"] != bucket.value {
			t.Fatal(checks, err)
		}
		args := strings.Join(s.calls[0], " ")
		if !strings.Contains(args, "--required") || !strings.Contains(args, "--repo github.com/example/widgets") {
			t.Fatal(args)
		}
	}
	for _, bad := range []Result{{ExitCode: 1, Stdout: []byte(`[]`)}, {ExitCode: 2, Stdout: []byte(`[]`)}, {ExitCode: 1, Stdout: []byte(`provider failed`)}, response(`[{"name":"test","bucket":"unexpected","state":"COMPLETED","link":"","workflow":"CI"}]`), response(`[{"name":"test","bucket":"pass","state":"COMPLETED","link":"","workflow":"CI"},{"name":"test","bucket":"pass","state":"COMPLETED","link":"","workflow":"CI"}]`)} {
		s := &sequenceExecutor{responses: []Result{bad}}
		if _, err := sequence(s).RequiredChecks(context.Background(), 3); err == nil {
			t.Fatal("incomplete or failed check read accepted", bad)
		}
	}
}

func TestWorkflowRunReadUsesRESTRepositoryIdentity(t *testing.T) {
	body := `{"id":8,"name":"CI","event":"pull_request","status":"completed","conclusion":"success","head_branch":"codex/change","head_sha":"` + strings.Repeat("a", 40) + `","repository":{"full_name":"example/widgets","html_url":"https://github.com/example/widgets","node_id":"R_widgets"},"pull_requests":[]}`
	s := &sequenceExecutor{responses: []Result{response(body)}}
	if _, err := sequence(s).ReadWorkflowRun(context.Background(), 8); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(body, `"id":8`, `"id":9`, 1), strings.Replace(body, "example/widgets", "other/widgets", -1), strings.Replace(body, "github.com/example/widgets", "ghe.invalid/example/widgets", 1), strings.Replace(body, `"node_id":"R_widgets"`, `"node_id":""`, 1)} {
		s = &sequenceExecutor{responses: []Result{response(bad)}}
		if _, err := sequence(s).ReadWorkflowRun(context.Background(), 8); err == nil {
			t.Fatal("foreign run accepted")
		}
	}
}

func TestNativeMergeBindsExactHeadAndRequiresPositiveCommitAcknowledgement(t *testing.T) {
	head, merge := strings.Repeat("a", 40), strings.Repeat("b", 40)
	s := &sequenceExecutor{responses: []Result{response(`{"merged":true,"sha":"` + merge + `","message":"Merged"}`)}}
	ack, err := sequence(s).MergePullRequest(context.Background(), 3, head, "squash")
	if err != nil || ack["sha"] != merge || s.inputs[0]["sha"] != head || s.inputs[0]["merge_method"] != "squash" || s.calls[0][1] != "repos/example/widgets/pulls/3/merge" {
		t.Fatal(ack, err, s.calls, s.inputs)
	}
	for _, result := range []Result{response(`{"merged":false,"sha":"` + merge + `"}`), response(`{"merged":true,"sha":null}`), {ExitCode: 1}} {
		s = &sequenceExecutor{responses: []Result{result}}
		if _, err := sequence(s).MergePullRequest(context.Background(), 3, head, "squash"); err == nil {
			t.Fatal("unacknowledged merge accepted")
		}
		if len(s.calls) != 1 {
			t.Fatal("merge retried")
		}
	}
	s = &sequenceExecutor{}
	for _, invalid := range []contract.Object{{"number": int64(0), "sha": head, "method": "squash"}, {"number": int64(3), "sha": "bad", "method": "squash"}, {"number": int64(3), "sha": head, "method": "auto"}} {
		n, _ := contract.Integer(invalid["number"])
		if _, err := sequence(s).MergePullRequest(context.Background(), n, invalid["sha"].(string), invalid["method"].(string)); err == nil {
			t.Fatal("invalid merge intent accepted")
		}
	}
	if len(s.calls) != 0 {
		t.Fatal("invalid merge intent reached provider")
	}
}
