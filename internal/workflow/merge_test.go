package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
)

func mergeSource() contract.Object {
	head := strings.Repeat("a", 40)
	event := contract.Object{"workflow_run": contract.Object{"id": int64(8), "run_attempt": int64(1), "workflow_id": int64(88), "name": "CI", "event": "pull_request", "status": "completed", "conclusion": "success", "head_sha": head, "head_branch": "codex/change", "repository": contract.Object{"full_name": "example/widgets"}, "pull_requests": []any{contract.Object{"number": int64(3), "head": contract.Object{"sha": head, "ref": "codex/change"}}}}}
	pr := contract.Object{"number": int64(3), "id": "PR_3", "url": "https://github.com/example/widgets/pull/3", "repository": "example/widgets", "title": "Reviewed change", "body": "Closes #1", "state": "OPEN", "isDraft": false, "merged": false, "baseRefName": "main", "headRefName": "codex/change", "headRefOid": head, "mergeStateStatus": "CLEAN", "reviewDecision": "APPROVED", "merge_commit_sha": nil, "head_repository": testRepo().Object(), "author": contract.Object{"login": "automation[bot]", "is_bot": true}, "labels": []any{contract.Object{"name": "automerge"}}}
	return contract.Object{"repository": contract.Object{"id": "R_widgets", "nameWithOwner": "example/widgets", "defaultBranch": "main"}, "event": event, "pull_request": pr, "required_checks": []any{contract.Object{"name": "tests", "bucket": "pass", "state": "SUCCESS", "workflow": "CI", "link": "https://github.com/example/widgets/actions/runs/8"}}, "provenance": contract.Object{"live": true, "complete": true, "source": "github_api", "host": "github.com"}}
}

func mergePolicy() contract.Object {
	return contract.Object{"workflow_name": "CI", "workflow_event": "pull_request", "required_label": "automerge", "pass_bucket": "pass", "merge_method": "squash", "branch_patterns": []any{"codex/*"}, "trusted_logins": []any{}}
}

type mergeFixture struct {
	source                                        contract.Object
	path                                          string
	writes, reads, failRead, driftRead, crashRead int
	unknown, crashWrite                           bool
}

func (f *mergeFixture) load() error {
	if f.path == "" {
		return nil
	}
	body, err := os.ReadFile(f.path)
	if err != nil {
		return err
	}
	f.source, err = contract.Decode(strings.NewReader(string(body)))
	return err
}
func (f *mergeFixture) save() error {
	if f.path == "" {
		return nil
	}
	body, err := contract.Canonical(f.source)
	if err != nil {
		return err
	}
	return os.WriteFile(f.path, body, 0600)
}
func (f *mergeFixture) MergeInventory(context.Context, contract.Object) (contract.Object, error) {
	if err := f.load(); err != nil {
		return nil, err
	}
	f.reads++
	if f.crashRead == f.reads {
		os.Exit(82)
	}
	if f.failRead == f.reads {
		return nil, errors.New("independent after-state unavailable")
	}
	if f.driftRead == f.reads {
		repo, _ := contract.ObjectAt(f.source, "repository")
		repo["defaultBranch"] = "changed"
	}
	return contract.Clone(f.source)
}
func (f *mergeFixture) MergePullRequest(_ context.Context, number int64, sha, method string) (contract.Object, error) {
	if number != 3 || sha != strings.Repeat("a", 40) || method != "squash" {
		return nil, errors.New("foreign merge intent")
	}
	f.writes++
	pr, _ := contract.ObjectAt(f.source, "pull_request")
	pr["state"], pr["merged"], pr["mergeStateStatus"], pr["merge_commit_sha"] = "MERGED", true, "MERGED", strings.Repeat("b", 40)
	if err := f.save(); err != nil {
		return nil, err
	}
	if f.crashWrite {
		os.Exit(81)
	}
	if f.unknown {
		return nil, errors.New("ambiguous provider write")
	}
	return contract.Object{"merged": true, "sha": strings.Repeat("b", 40), "message": "Merged"}, nil
}
func mergeEngine(root string, f *mergeFixture) apply.Engine {
	return apply.Engine{Root: root, Repository: testRepo(), Command: MergeCommand, Adapter: MergeAdapter{Provider: f}}
}
func prepareMergeFixture(t *testing.T, f *mergeFixture) contract.Plan {
	t.Helper()
	event, _ := contract.ObjectAt(f.source, "event")
	plan, err := PrepareMerge(context.Background(), f, testRepo(), event, mergePolicy(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestMergeApplyProjectsExactCommitAndSkipsCompletedDispatch(t *testing.T) {
	f := &mergeFixture{source: mergeSource()}
	p := prepareMergeFixture(t, f)
	e := mergeEngine(t.TempDir(), f)
	for i := 0; i < 2; i++ {
		result, err := e.Apply(context.Background(), p.Object())
		if err != nil || result["status"] != "completed" {
			t.Fatal(result, err)
		}
	}
	if f.writes != 1 {
		t.Fatal("completed merge replayed", f.writes)
	}
}
func TestMergeCompleteSourceDriftBlocksBeforeOrAfterMerge(t *testing.T) {
	for _, facet := range []string{"head", "checks", "labels", "repository", "trigger"} {
		t.Run(facet, func(t *testing.T) {
			f := &mergeFixture{source: mergeSource()}
			p := prepareMergeFixture(t, f)
			switch facet {
			case "head":
				pr, _ := contract.ObjectAt(f.source, "pull_request")
				pr["headRefOid"] = strings.Repeat("c", 40)
			case "checks":
				f.source["required_checks"] = []any{}
			case "labels":
				pr, _ := contract.ObjectAt(f.source, "pull_request")
				pr["labels"] = []any{}
			case "repository":
				repo, _ := contract.ObjectAt(f.source, "repository")
				repo["id"] = "R_recreated"
			case "trigger":
				event, _ := contract.ObjectAt(f.source, "event")
				run, _ := contract.ObjectAt(event, "workflow_run")
				run["run_attempt"] = int64(2)
			}
			if _, err := mergeEngine(t.TempDir(), f).Apply(context.Background(), p.Object()); err == nil || f.writes != 0 {
				t.Fatal("drift caused merge", err, f.writes)
			}
		})
	}
	f := &mergeFixture{source: mergeSource()}
	p := prepareMergeFixture(t, f)
	f.driftRead = 5
	if _, err := mergeEngine(t.TempDir(), f).Apply(context.Background(), p.Object()); err == nil || f.writes != 1 {
		t.Fatal("terminal drift accepted", err, f.writes)
	}
}
func TestMergeUnknownRecoveryRequiresRetainedAcknowledgement(t *testing.T) {
	for _, ack := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-ack", true: "after-ack"}[ack], func(t *testing.T) {
			f := &mergeFixture{source: mergeSource()}
			p := prepareMergeFixture(t, f)
			e := mergeEngine(t.TempDir(), f)
			if ack {
				f.failRead = 4
			} else {
				f.unknown = true
			}
			if _, err := e.Apply(context.Background(), p.Object()); err == nil {
				t.Fatal("unknown merge claimed completed")
			}
			f.failRead, f.unknown = 0, false
			result, err := e.Apply(context.Background(), p.Object())
			if ack {
				if err != nil || result["status"] != "completed" {
					t.Fatal(result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "no retained provider acknowledgement") {
				t.Fatal("unknown merge replay permitted", err)
			}
			if f.writes != 1 {
				t.Fatal("accepted merge replayed", f.writes)
			}
		})
	}
}
func TestMergeCannotAcceptAlteredDomainPrimitive(t *testing.T) {
	f := &mergeFixture{source: mergeSource()}
	p := prepareMergeFixture(t, f)
	p.Operations[0].Target["head_sha"] = strings.Repeat("d", 40)
	if _, err := mergeEngine(t.TempDir(), f).Apply(context.Background(), p.Object()); err == nil || f.writes != 0 {
		t.Fatal("altered operation dispatched", err)
	}
}
func TestMergeFreshProcessCrashKeepsProgressWithoutRetry(t *testing.T) {
	if mode := os.Getenv("STEWARD_MERGE_CRASH"); mode != "" {
		root := os.Getenv("STEWARD_MERGE_ROOT")
		file, err := os.Open(filepath.Join(root, "plan.json"))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		p, err := contract.Decode(file)
		if err != nil {
			t.Fatal(err)
		}
		f := &mergeFixture{path: filepath.Join(root, "provider.json"), crashWrite: mode == "before-ack"}
		if mode == "after-ack" {
			f.crashRead = 3
		}
		_, err = mergeEngine(root, f).Apply(context.Background(), p)
		t.Fatal("crash fixture did not exit", err)
		return
	}
	for _, mode := range []string{"before-ack", "after-ack"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			f := &mergeFixture{source: mergeSource(), path: filepath.Join(root, "provider.json")}
			if err := f.save(); err != nil {
				t.Fatal(err)
			}
			p := prepareMergeFixture(t, f)
			body, _ := contract.Canonical(p.Object())
			if err := os.WriteFile(filepath.Join(root, "plan.json"), body, 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(os.Args[0], "-test.run=^TestMergeFreshProcessCrashKeepsProgressWithoutRetry$")
			command.Env = append(os.Environ(), "STEWARD_MERGE_CRASH="+mode, "STEWARD_MERGE_ROOT="+root)
			err := command.Run()
			exit, ok := err.(*exec.ExitError)
			want := 81
			if mode == "after-ack" {
				want = 82
			}
			if !ok || exit.ExitCode() != want {
				t.Fatal("crash fixture failed", err)
			}
			fresh := &mergeFixture{path: f.path}
			result, err := mergeEngine(root, fresh).Apply(context.Background(), p.Object())
			if mode == "after-ack" {
				if err != nil || result["status"] != "completed" {
					t.Fatal(result, err)
				}
			} else if err == nil {
				t.Fatal("unknown merge advanced")
			}
			if fresh.writes != 0 {
				t.Fatal("fresh process replayed merge")
			}
		})
	}
}
