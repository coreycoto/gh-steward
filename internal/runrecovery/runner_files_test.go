package runrecovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunnerPackageAndAtomicOutputsRejectSymbolicOrExistingEvidence(t *testing.T) {
	temp := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(temp, "link")); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{filepath.Join(temp, "link"), filepath.Join(outside, "package"), temp + "/../bad", temp} {
		if _, err := runnerDirectory(directory, temp, true, true); err == nil {
			t.Fatalf("unsafe runner directory accepted: %s", directory)
		}
	}
	root, err := runnerDirectory(filepath.Join(temp, "package"), temp, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistPackageJSON(root, "plans/change.json", Object{"status": "prepared"}); err != nil {
		t.Fatal(err)
	}
	if err := persistPackageJSON(root, "plans/change.json", Object{"status": "completed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runnerDirectory(root, temp, true, true); err == nil {
		t.Fatal("occupied package accepted for a fresh recovery")
	}
	if _, err := checkpointDestination(root, temp); err == nil {
		t.Fatal("existing invocation allowed as checkpoint destination")
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := persistPackageJSON(root, "escape/proof.json", Object{}); err == nil {
		t.Fatal("atomic proof output followed symbolic parent")
	}
}

func TestActionsOutputRejectsInjectedLinesAndSymbolicDestination(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "output")
	if err := os.WriteFile(output, []byte("prior=retained\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteActionsOutput(output, Object{"outcome": "resumed", "reason": "original target restored"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, fields := range []Object{{"outcome": "resumed\ninjected=1"}, {"bad\nkey": "value"}, {"outcome": int64(1)}} {
		if err := WriteActionsOutput(output, fields); err == nil {
			t.Fatal("unsafe Actions output accepted")
		}
	}
	after, err := os.ReadFile(output)
	if err != nil || string(before) != string(after) {
		t.Fatal("rejected output altered existing values")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(output, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteActionsOutput(link, Object{"outcome": "fresh"}); err == nil {
		t.Fatal("Actions output followed a symlink")
	}
}
