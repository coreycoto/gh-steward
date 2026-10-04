package progress

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var repository = contract.Repository{Host: "github.com", Owner: "example", Name: "widgets", URL: "https://github.com/example/widgets"}
var intent = contract.Object{"kind": "issue-comment", "target": contract.Object{"number": 17}, "body": "Reviewed follow-up"}

func open(t *testing.T, root string) *Journal {
	t.Helper()
	j, err := Open(root, repository, "review-apply", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestCompletedPrimitivesAreNotRepeatedAndIntentCannotChange(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)
	count := 0
	result, err := j.Execute("comment:17", intent, func(string) (contract.Object, error) {
		count++
		return contract.Object{"id": 31, "body": "Reviewed follow-up"}, nil
	})
	if err != nil || result["id"] == nil {
		t.Fatal(err)
	}
	path := j.Path()
	j.Close()
	j = open(t, root)
	defer j.Close()
	if j.Path() != path {
		t.Fatal("journal identity changed")
	}
	if _, err := j.Execute("comment:17", intent, func(string) (contract.Object, error) { count++; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("completed primitive repeated")
	}
	if _, err := j.Execute("comment:17", contract.Object{"body": "Unreviewed body"}, func(string) (contract.Object, error) { t.Fatal("changed intent dispatched"); return nil, nil }); err == nil {
		t.Fatal("changed intent accepted")
	}
	if err := j.Finish(contract.Object{"status": "success"}, []string{"comment:17"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Finish(contract.Object{"status": "changed"}, []string{"comment:17"}); err == nil {
		t.Fatal("terminal result changed")
	}
}

func TestCaseVariantsUseTheSameJournalAndLock(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)
	path := j.Path()
	variant := contract.Repository{Host: "GitHub.com", Owner: "Example", Name: "Widgets", URL: "https://GitHub.com/Example/Widgets"}
	if other, err := Open(root, variant, "review-apply", strings.Repeat("a", 64)); err == nil {
		other.Close()
		t.Fatal("case variant bypassed existing plan lock")
	}
	j.Close()
	other, err := Open(root, variant, "review-apply", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.Path() != path {
		t.Fatal("case variant created a different durable journal")
	}
}

func TestUnknownOutcomeNeedsPositiveExactObservation(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)
	if _, err := j.Execute("comment:17", intent, func(string) (contract.Object, error) { return nil, errors.New("ambiguous timeout") }); err == nil {
		t.Fatal("ambiguous outcome accepted")
	}
	operation := j.OperationID("comment:17")
	j.Close()
	j = open(t, root)
	defer j.Close()
	if _, err := j.Execute("comment:17", intent, func(string) (contract.Object, error) { t.Fatal("unknown write repeated"); return nil, nil }); err == nil {
		t.Fatal("unknown write repeated")
	}
	for _, evidence := range []contract.Object{{"positive_identity": false, "after_state_verified": true, "operation_id": operation, "reference": "https://github.com/example/widgets/issues/17#issuecomment-31"}, {"positive_identity": true, "after_state_verified": true, "operation_id": "other", "reference": "https://github.com/example/widgets/issues/17#issuecomment-31"}} {
		if err := j.Observe("comment:17", intent, contract.Object{"id": 31}, evidence); err == nil {
			t.Fatal("weak observation accepted")
		}
	}
	if err := j.Observe("comment:17", intent, contract.Object{"id": 31}, contract.Object{"positive_identity": true, "after_state_verified": true, "operation_id": operation, "reference": "https://github.com/example/widgets/issues/17#issuecomment-31"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Finish(contract.Object{"status": "success"}, nil); err == nil {
		t.Fatal("incomplete terminal receipt accepted")
	}
}

func TestFreshProcessCrashPreservesIntentBeforeAndAfterEffect(t *testing.T) {
	if root := os.Getenv("STEWARD_TEST_CRASH_ROOT"); root != "" {
		j, err := Open(root, repository, "review-apply", strings.Repeat("a", 64))
		if err != nil {
			os.Exit(90)
		}
		_, _ = j.Execute("comment:17", intent, func(operation string) (contract.Object, error) {
			if os.Getenv("STEWARD_TEST_CRASH_AFTER") == "1" {
				if err := os.WriteFile(filepath.Join(root, "effect"), []byte(operation), 0600); err != nil {
					os.Exit(91)
				}
			}
			os.Exit(73)
			return nil, nil
		})
		os.Exit(92)
	}
	for _, after := range []string{"0", "1"} {
		t.Run(after, func(t *testing.T) {
			root := t.TempDir()
			child := exec.Command(os.Args[0], "-test.run=^TestFreshProcessCrashPreservesIntentBeforeAndAfterEffect$")
			child.Env = append(os.Environ(), "STEWARD_TEST_CRASH_ROOT="+root, "STEWARD_TEST_CRASH_AFTER="+after)
			if err := child.Run(); err == nil {
				t.Fatal("child did not stop")
			}
			j := open(t, root)
			defer j.Close()
			if j.Status("comment:17") != "unknown" {
				t.Fatal("process death lost unknown receipt")
			}
			if _, err := j.Execute("comment:17", intent, func(string) (contract.Object, error) { t.Fatal("crashed primitive replayed"); return nil, nil }); err == nil {
				t.Fatal("unknown accepted")
			}
			if after == "1" {
				effect, err := os.ReadFile(filepath.Join(root, "effect"))
				if err != nil || string(effect) != j.OperationID("comment:17") {
					t.Fatal("durable effect identity lost")
				}
			}
		})
	}
}

func TestContentionAndUnsafeJournalPathsStopBeforeDispatch(t *testing.T) {
	root := t.TempDir()
	j := open(t, root)
	defer j.Close()
	if other, err := Open(root, repository, "review-apply", strings.Repeat("a", 64)); err == nil {
		other.Close()
		t.Fatal("concurrent plan acquired lock")
	}
	escape := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(escape, ".artifacts")); err != nil {
		t.Fatal(err)
	}
	if journal, err := Open(escape, repository, "review-apply", strings.Repeat("a", 64)); err == nil {
		journal.Close()
		t.Fatal("symlinked artifact root accepted")
	}
}

func TestReopenRejectsPermissiveReceiptsAndOwnedDirectories(t *testing.T) {
	for _, which := range []string{"state", "lock", "directory"} {
		t.Run(which, func(t *testing.T) {
			root := t.TempDir()
			j := open(t, root)
			path := j.Path()
			j.Close()
			target := path
			if which == "lock" {
				target += ".lock"
			}
			if which == "directory" {
				target = filepath.Dir(path)
			}
			if err := os.Chmod(target, 0777); err != nil {
				t.Fatal(err)
			}
			if reopened, err := Open(root, repository, "review-apply", strings.Repeat("a", 64)); err == nil {
				reopened.Close()
				t.Fatal("permissive journal state was trusted")
			}
		})
	}
}
