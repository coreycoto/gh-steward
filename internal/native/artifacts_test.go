package native

import (
	"context"
	"strings"
	"testing"
)

func TestRunArtifactPagesUsesCompletePaginatedRunEndpoint(t *testing.T) {
	executor := &fakeExecutor{result: Result{Stdout: []byte(`[{"total_count":1,"artifacts":[{"id":9,"name":"build"}]},{"total_count":1,"artifacts":[]}]`)}}
	pages, err := transport(executor).RunArtifactPages(context.Background(), 123)
	if err != nil || len(pages) != 2 {
		t.Fatal(pages, err)
	}
	if len(executor.args) < 7 || executor.args[0] != "api" || executor.args[1] != "repos/example/widgets/actions/runs/123/artifacts?per_page=100" || !containsArgs(executor.args, "--paginate") || !containsArgs(executor.args, "--slurp") {
		t.Fatal("run artifact read was not fully paginated and repository scoped", executor.args)
	}
	if _, err := transport(&fakeExecutor{}).RunArtifactPages(context.Background(), 0); err == nil {
		t.Fatal("invalid run ID reached native gh")
	}
}

func TestDeleteRunArtifactRequiresExactEmptyHTTP204Acknowledgement(t *testing.T) {
	response204 := Result{Stdout: []byte("HTTP/2.0 204 No Content\r\nDate: Sun, 04 Oct 2026 00:00:00 GMT\r\n\r\n")}
	artifact := response(`{"id":9,"name":"target","workflow_run":{"id":123}}`)
	executor := &sequenceExecutor{responses: []Result{artifact, response204}}
	ack, err := sequence(executor).DeleteRunArtifact(context.Background(), 123, 9)
	if err != nil || ack["run_id"] != int64(123) || ack["artifact_id"] != int64(9) || ack["status_code"] != int64(204) || ack["no_content"] != true {
		t.Fatal(ack, err)
	}
	if len(executor.calls) != 2 || executor.calls[0][1] != "repos/example/widgets/actions/artifacts/9" || !strings.Contains(strings.Join(executor.calls[0], " "), "--method GET") {
		t.Fatal("artifact-to-run identity was not read before deletion", executor.calls)
	}
	args := strings.Join(executor.calls[1], " ")
	if !strings.Contains(args, "repos/example/widgets/actions/artifacts/9") || !strings.Contains(args, "--method DELETE") || !strings.Contains(args, "--include") || !strings.Contains(args, "--hostname github.com") {
		t.Fatal("delete was not an explicit, target-scoped HTTP request", args)
	}

	for _, bad := range []Result{
		{Stdout: []byte("204 No Content\r\n\r\n")},
		{Stdout: []byte("HTTP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n")},
		{Stdout: []byte("HTTP/2.0 204 No Content\r\nContent-Length: 1\r\n\r\n{}")},
		{Stdout: []byte("HTTP/2.0 204 No Content\r\n\r\n\n")},
		{Stdout: []byte("HTTP/2.0 404 Not Found\r\nContent-Length: 0\r\n\r\n")},
		{Stdout: []byte("HTTP/2.0 204 No Content\r\nMalformed\r\n\r\n")},
		{ExitCode: 1},
	} {
		executor = &sequenceExecutor{responses: []Result{artifact, bad}}
		if _, err := sequence(executor).DeleteRunArtifact(context.Background(), 123, 9); err == nil {
			t.Fatal("non-204 or ambiguous response accepted", bad)
		}
		if len(executor.calls) != 2 {
			t.Fatal("failed acknowledgement was retried")
		}
	}

	executor = &sequenceExecutor{responses: []Result{response(`{"id":9,"name":"target","workflow_run":{"id":124}}`), response204}}
	if _, err := sequence(executor).DeleteRunArtifact(context.Background(), 123, 9); err == nil || len(executor.calls) != 1 {
		t.Fatal("artifact from another run reached DELETE", err, executor.calls)
	}
}

func TestDeleteRunArtifactRejectsInvalidIdentityOrHostBeforeDispatch(t *testing.T) {
	executor := &sequenceExecutor{}
	tr := sequence(executor)
	for _, pair := range [][2]int64{{0, 9}, {123, 0}} {
		if _, err := tr.DeleteRunArtifact(context.Background(), pair[0], pair[1]); err == nil {
			t.Fatal("invalid artifact identity accepted", pair)
		}
	}
	for _, malformed := range []string{`{"id":10,"name":"target","workflow_run":{"id":123}}`, `{"id":9,"name":"target"}`, `{"id":9,"name":"target","workflow_run":{"id":true}}`} {
		executor = &sequenceExecutor{responses: []Result{response(malformed)}}
		tr = sequence(executor)
		if _, err := tr.DeleteRunArtifact(context.Background(), 123, 9); err == nil || len(executor.calls) != 1 {
			t.Fatal("malformed or mismatched artifact identity reached DELETE", err, executor.calls)
		}
	}
	executor = &sequenceExecutor{}
	tr = sequence(executor)
	tr.Environment = append(tr.Environment, "GH_HOST=other.example")
	if _, err := tr.DeleteRunArtifact(context.Background(), 123, 9); err == nil {
		t.Fatal("ambient host conflict accepted")
	}
	if len(executor.calls) != 0 {
		t.Fatal("invalid target reached native gh", executor.calls)
	}
}

func containsArgs(args []string, wanted string) bool {
	for _, arg := range args {
		if arg == wanted {
			return true
		}
	}
	return false
}
