package native

import (
	"context"
	"testing"
)

func TestGenericRepositoryRESTCannotBypassTypedMutations(t *testing.T) {
	f := &fakeExecutor{}
	tpt := transport(f)
	for _, method := range []string{"DELETE", "PATCH", "POST", "PUT", "get", "GET\nDELETE"} {
		if _, err := tpt.REST(context.Background(), method, "repos/example/widgets", nil); err == nil {
			t.Fatal("generic mutation allowed", method)
		}
	}
	if f.calls != 0 {
		t.Fatal("rejected write reached native gh")
	}
}
