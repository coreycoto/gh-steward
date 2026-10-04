package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var target = contract.Repository{Host: "github.com", Owner: "example", Name: "widgets", URL: "https://github.com/example/widgets"}

func operation(id string, n int, before, after string) contract.Operation {
	return contract.Operation{ID: id, Kind: "issue-update", Target: contract.Object{"number": n}, Before: contract.Object{"title": before}, After: contract.Object{"title": after}}
}
func operations() []contract.Operation {
	return []contract.Operation{operation("issue:1", 1, "Old one", "New one"), operation("issue:2", 2, "Old two", "New two")}
}
func reviewed(t *testing.T) contract.Plan {
	t.Helper()
	plan, err := contract.PreparePlan("backlog-apply", target, contract.Object{"issues": contract.Object{"live": true, "complete": true}}, contract.Object{"inventory": contract.Object{"1": "Old one", "2": "Old two", "3": "Unrelated"}}, operations(), time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

type fakeAdapter struct {
	root         string
	calls        int
	driftAfter   int
	ambiguous    bool
	weak         bool
	wrongReceipt bool
	crashAt      string
	crashAfter   bool
}

func (f *fakeAdapter) path() string { return filepath.Join(f.root, "remote.json") }
func (f *fakeAdapter) read() (contract.Object, error) {
	b, err := os.ReadFile(f.path())
	if err != nil {
		return nil, err
	}
	return contract.Decode(strings.NewReader(string(b)))
}
func (f *fakeAdapter) write(o contract.Object) error {
	b, err := contract.Canonical(o)
	if err != nil {
		return err
	}
	return os.WriteFile(f.path(), b, 0600)
}
func fixture(t *testing.T, root string) *fakeAdapter {
	t.Helper()
	f := &fakeAdapter{root: root}
	err := f.write(contract.Object{"inventory": contract.Object{"1": "Old one", "2": "Old two", "3": "Unrelated"}, "effects": contract.Object{}, "calls": 0})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fakeAdapter) Operations(contract.Plan) ([]contract.Operation, error) {
	return operations(), nil
}
func (f *fakeAdapter) ValidateReceipt(_ contract.Plan, op contract.Operation, r contract.Object) error {
	key := fmt.Sprint(op.Target["number"])
	if r["key"] != key || r["title"] != op.After["title"] {
		return errors.New("receipt has foreign target or after-state")
	}
	return nil
}
func (f *fakeAdapter) Preflight(_ context.Context, p contract.Plan, receipts []contract.Object) error {
	r, err := f.read()
	if err != nil {
		return err
	}
	want, err := contract.Clone(p.Data["inventory"].(map[string]any))
	if err != nil {
		return err
	}
	for _, receipt := range receipts {
		if receipt["status"] == "completed" {
			after := receipt["result"].(map[string]any)
			want[after["key"].(string)] = after["title"]
		}
	}
	a, _ := contract.Digest(want)
	b, _ := contract.Digest(r["inventory"])
	if a != b {
		return errors.New("complete reviewed inventory drifted")
	}
	return nil
}
func (f *fakeAdapter) Dispatch(_ context.Context, _ contract.Plan, op contract.Operation, operationID string, _ []contract.Object) (contract.Object, error) {
	if f.crashAt == op.ID && !f.crashAfter {
		os.Exit(73)
	}
	r, err := f.read()
	if err != nil {
		return nil, err
	}
	key := fmt.Sprint(op.Target["number"])
	result := contract.Object{"operation_id": operationID, "key": key, "title": op.After["title"]}
	r["inventory"].(map[string]any)[key] = op.After["title"]
	r["effects"].(map[string]any)[operationID] = result
	calls, _ := contract.Integer(r["calls"])
	r["calls"] = calls + 1
	f.calls++
	if f.calls == f.driftAfter {
		r["inventory"].(map[string]any)["3"] = "External drift"
	}
	if err = f.write(r); err != nil {
		return nil, err
	}
	if f.crashAt == op.ID && f.crashAfter {
		os.Exit(73)
	}
	if f.ambiguous {
		return nil, errors.New("accepted then timed out")
	}
	if f.wrongReceipt {
		result["operation_id"] = "foreign"
	}
	return result, nil
}
func (f *fakeAdapter) Observe(_ context.Context, _ contract.Plan, _ contract.Operation, id string, _ []contract.Object) (contract.Object, contract.Object, error) {
	r, err := f.read()
	if err != nil {
		return nil, nil, err
	}
	result, exists := r["effects"].(map[string]any)[id].(map[string]any)
	if !exists {
		return nil, nil, errors.New("no positive operation identity found; no retry")
	}
	return result, contract.Object{"positive_identity": !f.weak, "after_state_verified": true, "operation_id": id, "reference": "https://github.com/example/widgets/issues/1#effect-" + id}, nil
}
func engine(root string, f *fakeAdapter) Engine {
	return Engine{Root: root, Repository: target, Command: "backlog-apply", Adapter: f}
}

func TestApplyChecksFullInventoryAndTerminalDrift(t *testing.T) {
	for _, drift := range []int{1, 2} {
		t.Run(fmt.Sprint(drift), func(t *testing.T) {
			root := t.TempDir()
			f := fixture(t, root)
			f.driftAfter = drift
			if _, err := engine(root, f).Apply(context.Background(), reviewed(t).Object()); err == nil {
				t.Fatal("inventory drift reported success")
			}
			if f.calls != drift {
				t.Fatal("write dispatched after unrelated inventory drift")
			}
		})
	}
}
func TestApplyValidatesDomainIntentBeforeAnyDispatch(t *testing.T) {
	root := t.TempDir()
	f := fixture(t, root)
	p := reviewed(t)
	p.Operations[0].After["title"] = "Unreviewed"
	p.SHA256, _ = contract.Digest(p.Unsigned())
	if _, err := engine(root, f).Apply(context.Background(), p.Object()); err == nil || f.calls != 0 {
		t.Fatal("valid hash bypassed domain intent recomputation")
	}
	p = reviewed(t)
	other := engine(root, f)
	other.Repository = contract.Repository{Host: "github.com", Owner: "other", Name: "widgets", URL: "https://github.com/other/widgets"}
	if _, err := other.Apply(context.Background(), p.Object()); err == nil || f.calls != 0 {
		t.Fatal("foreign repository dispatched")
	}
}
func TestSuccessReplayAndAcceptedUnknownResumeDoNotRepeatWrites(t *testing.T) {
	root := t.TempDir()
	f := fixture(t, root)
	p := reviewed(t)
	f.ambiguous = true
	if _, err := engine(root, f).Apply(context.Background(), p.Object()); err == nil {
		t.Fatal("ambiguous write reported success")
	}
	f.ambiguous = false
	f.weak = true
	if _, err := engine(root, f).Apply(context.Background(), p.Object()); err == nil || f.calls != 1 {
		t.Fatal("weak observation advanced or repeated write")
	}
	f.weak = false
	r, err := engine(root, f).Apply(context.Background(), p.Object())
	if err != nil || r["status"] != "completed" || f.calls != 2 {
		t.Fatal("positive recovery failed", r, err)
	}
	if _, err := engine(root, f).Apply(context.Background(), p.Object()); err != nil || f.calls != 2 {
		t.Fatal("success replay repeated completed primitives", err)
	}
	raw, _ := f.read()
	raw["inventory"].(map[string]any)["3"] = "External"
	_ = f.write(raw)
	if _, err := engine(root, f).Apply(context.Background(), p.Object()); err == nil || f.calls != 2 {
		t.Fatal("terminal replay hid new drift")
	}
}
func TestInvalidCompletionRemainsUnknown(t *testing.T) {
	root := t.TempDir()
	f := fixture(t, root)
	f.wrongReceipt = true
	if _, err := engine(root, f).Apply(context.Background(), reviewed(t).Object()); err == nil || f.calls != 1 {
		t.Fatal("wrong operation receipt accepted")
	}
}
func TestFreshProcessCrashAtSecondPrimitive(t *testing.T) {
	if root := os.Getenv("STEWARD_APPLY_TEST_ROOT"); root != "" {
		p := reviewed(t)
		f := &fakeAdapter{root: root, crashAt: "issue:2", crashAfter: os.Getenv("STEWARD_APPLY_TEST_AFTER") == "1"}
		_, _ = engine(root, f).Apply(context.Background(), p.Object())
		os.Exit(92)
	}
	for _, after := range []string{"0", "1"} {
		t.Run(after, func(t *testing.T) {
			root := t.TempDir()
			f := fixture(t, root)
			p := reviewed(t)
			child := exec.Command(os.Args[0], "-test.run=^TestFreshProcessCrashAtSecondPrimitive$")
			child.Env = append(os.Environ(), "STEWARD_APPLY_TEST_ROOT="+root, "STEWARD_APPLY_TEST_AFTER="+after)
			if err := child.Run(); err == nil {
				t.Fatal("child did not interrupt")
			}
			_, err := engine(root, f).Apply(context.Background(), p.Object())
			remote, _ := f.read()
			calls, _ := contract.Integer(remote["calls"])
			if after == "0" {
				if err == nil || calls != 1 {
					t.Fatal("absence authorized retry")
				}
			} else {
				if err != nil || calls != 2 {
					t.Fatal("fresh process repeated completed effect", err, calls)
				}
			}
		})
	}
}
