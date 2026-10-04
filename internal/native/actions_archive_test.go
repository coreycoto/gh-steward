package native

import (
	"bytes"
	"context"
	"reflect"
	"testing"
)

func TestActionsArchiveHasOneFixedReadScopeAndBoundedBytes(t *testing.T) {
	data := []byte{'P', 'K', 0, 0xff, '\n'}
	f := &fakeExecutor{result: Result{Stdout: data}}
	tpt := transport(f)
	actual, err := tpt.ActionsArtifactArchive(context.Background(), 37)
	if err != nil || !bytes.Equal(data, actual) || !reflect.DeepEqual(f.args, []string{"api", "repos/example/widgets/actions/artifacts/37/zip", "--hostname", "github.com", "--method", "GET"}) {
		t.Fatalf("raw archive or repository scope lost: %v", err)
	}
	for _, id := range []int64{0, -1} {
		if _, err := tpt.ActionsArtifactArchive(context.Background(), id); err == nil {
			t.Fatal("nonpositive artifact ID dispatched")
		}
	}
	if f.calls != 1 {
		t.Fatal("invalid artifact IDs reached provider")
	}
	tpt.Environment = append(tpt.Environment, "GH_HOST=foreign.example")
	if _, err := tpt.ActionsArtifactArchive(context.Background(), 37); err == nil || f.calls != 1 {
		t.Fatal("ambient host crossed selected artifact scope")
	}
	tpt.Environment = nil
	f.result = Result{ExitCode: 1}
	if _, err := tpt.ActionsArtifactArchive(context.Background(), 37); err == nil {
		t.Fatal("failed download became empty evidence")
	}
	f.result = Result{Stdout: make([]byte, MaxActionsArchiveBytes+1)}
	if _, err := tpt.ActionsArtifactArchive(context.Background(), 37); err == nil {
		t.Fatal("oversized archive accepted")
	}
}
