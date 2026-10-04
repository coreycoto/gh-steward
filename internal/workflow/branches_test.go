package workflow

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

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
)

func branchTestSelection(count int) contract.Object {
	rows := []any{}
	for index := 0; index < count; index++ {
		rows = append(rows, contract.Object{"name": fmt.Sprintf("codex/issue-%d", 17+index), "sha": strings.Repeat("a", 40), "pull_request_number": int64(3 + index)})
	}
	return contract.Object{"branches": rows}
}

func branchTestInventory(count int) contract.Object {
	selection := branchTestSelection(count)
	requested, _ := contract.Objects(selection, "branches")
	rows := []any{}
	for index, row := range requested {
		pr, _ := contract.ObjectAt(mergeSource(), "pull_request")
		pr["id"], pr["number"], pr["url"] = fmt.Sprintf("PR_%d", 3+index), int64(3+index), fmt.Sprintf("%s/pull/%d", testRepo().URL, 3+index)
		pr["headRefName"], pr["state"], pr["merged"], pr["mergeStateStatus"], pr["merge_commit_sha"] = row["name"], "MERGED", true, "MERGED", strings.Repeat("b", 40)
		branch := contract.Object{"repo": testRepo().Object(), "repository_node_id": "R_widgets", "id": fmt.Sprintf("REF_%d", index), "name": row["name"], "sha": row["sha"]}
		dependents := contract.Object{"repo": testRepo().Object(), "repository_node_id": "R_widgets", "base_branch": row["name"], "pull_requests": []any{}, "provenance": contract.Object{"live": true, "complete": true, "source": "github_api"}}
		rows = append(rows, contract.Object{"selection": row, "branch": branch, "pull_request": pr, "base_dependents": dependents})
	}
	return contract.Object{"repo": testRepo().Object(), "repository_node_id": "R_widgets", "default_branch": "main", "branches": rows,
		"provenance": contract.Object{"live": true, "complete": true, "source": "github_api", "repository_node_id": "R_widgets", "selection": selection}}
}

type branchTestProvider struct {
	inventory contract.Object
	writes    int64
	path      string
	crashMode string
	badACK    bool
}

func (f *branchTestProvider) load() error {
	if f.path == "" {
		return nil
	}
	bytes, err := os.ReadFile(f.path)
	if err != nil {
		return err
	}
	state, err := contract.Decode(strings.NewReader(string(bytes)))
	if err != nil {
		return err
	}
	f.inventory, err = contract.ObjectAt(state, "inventory")
	if err != nil {
		return err
	}
	f.writes, err = contract.Integer(state["writes"])
	return err
}
func (f *branchTestProvider) save() error {
	if f.path == "" {
		return nil
	}
	bytes, err := contract.Canonical(contract.Object{"inventory": f.inventory, "writes": f.writes})
	if err != nil {
		return err
	}
	return os.WriteFile(f.path, bytes, 0600)
}
func (f *branchTestProvider) BranchCleanupInventory(_ context.Context, selection contract.Object) (contract.Object, error) {
	if err := f.load(); err != nil {
		return nil, err
	}
	if f.writes > 0 && f.crashMode == "after-ack" {
		os.Exit(82)
	}
	if !same(selection, f.inventory["provenance"].(contract.Object)["selection"]) {
		return nil, errors.New("fixture selection changed")
	}
	return contract.Clone(f.inventory)
}
func (f *branchTestProvider) DeleteBranch(_ context.Context, nonce, branch, sha, nodeID string) (contract.Object, error) {
	if err := f.load(); err != nil {
		return nil, err
	}
	if nodeID != "R_widgets" || nonce == "" {
		return nil, errors.New("foreign fixture deletion")
	}
	rows, _ := contract.Objects(f.inventory, "branches")
	found := false
	for _, row := range rows {
		selection, _ := contract.ObjectAt(row, "selection")
		if selection["name"] == branch && selection["sha"] == sha && row["branch"] != nil {
			row["branch"] = nil
			found = true
		}
	}
	if !found {
		return nil, errors.New("duplicate or foreign fixture deletion")
	}
	f.writes++
	if err := f.save(); err != nil {
		return nil, err
	}
	if f.crashMode == "before-ack" {
		os.Exit(81)
	}
	stdout := fmt.Sprintf("To %s.git\n-\t:refs/heads/%s\t[deleted]\nDone\n", testRepo().URL, branch)
	if f.badACK {
		stdout = ""
	}
	return contract.Object{"exit_code": int64(0), "stdout": stdout, "stderr": "", "remote_url": testRepo().URL + ".git", "ref": "refs/heads/" + branch, "expected_sha": sha}, nil
}

func branchTestPlan(t *testing.T, f *branchTestProvider, count int) contract.Plan {
	t.Helper()
	plan, err := PrepareBranchCleanup(context.Background(), f, testRepo(), branchTestSelection(count), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
func branchTestEngine(root string, f *branchTestProvider) apply.Engine {
	return apply.Engine{Root: root, Repository: testRepo(), Command: BranchCleanupCommand, Adapter: BranchCleanup{Provider: f}}
}

func TestBranchCleanupDeletesExactlyReviewedMergedHeadsAndReplaysReadOnly(t *testing.T) {
	f := &branchTestProvider{inventory: branchTestInventory(2)}
	plan := branchTestPlan(t, f, 2)
	if len(plan.Operations) != 2 || plan.Operations[0].Kind != "branch-delete" {
		t.Fatal("wrong branch primitives", plan.Operations)
	}
	root := t.TempDir()
	result, err := branchTestEngine(root, f).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" || f.writes != 2 {
		t.Fatal("reviewed deletions did not complete exactly", result, err, f.writes)
	}
	if _, err := branchTestEngine(root, f).Apply(context.Background(), plan.Object()); err != nil || f.writes != 2 {
		t.Fatal("completed branch deletion replayed", err, f.writes)
	}
	rows, _ := contract.Objects(f.inventory, "branches")
	for _, row := range rows {
		if row["branch"] != nil {
			t.Fatal("reviewed branch remains")
		}
	}
}

func TestBranchCleanupKeepsEmptySelectionAsVerifiedNoop(t *testing.T) {
	f := &branchTestProvider{inventory: branchTestInventory(0)}
	plan := branchTestPlan(t, f, 0)
	result, err := branchTestEngine(t.TempDir(), f).Apply(context.Background(), plan.Object())
	if err != nil || result["status"] != "completed" || f.writes != 0 || len(plan.Operations) != 0 {
		t.Fatal("empty cleanup is not a read-only no-op", result, err)
	}
}

func TestBranchCleanupRejectsIncompleteAndUnrelatedCandidatesBeforeWrites(t *testing.T) {
	for _, change := range []string{"default", "missing", "head", "ref", "fork", "closed", "draft", "pr_number", "merge_commit", "base_dependent", "partial", "seed", "repository", "missing_provenance", "unrequested", "extra_data"} {
		t.Run(change, func(t *testing.T) {
			f := &branchTestProvider{inventory: branchTestInventory(1)}
			rows, _ := contract.Objects(f.inventory, "branches")
			row := rows[0]
			pr, _ := contract.ObjectAt(row, "pull_request")
			ref, _ := contract.ObjectAt(row, "branch")
			prov, _ := contract.ObjectAt(f.inventory, "provenance")
			switch change {
			case "default":
				f.inventory["default_branch"] = "codex/issue-17"
			case "missing":
				row["branch"] = nil
			case "head":
				pr["headRefOid"] = strings.Repeat("c", 40)
			case "ref":
				ref["name"] = "other"
			case "fork":
				pr["head_repository"] = contract.Object{"host": "github.com", "owner": "other", "name": "widgets", "nameWithOwner": "other/widgets", "url": "https://github.com/other/widgets"}
			case "closed":
				pr["state"], pr["merged"], pr["merge_commit_sha"] = "CLOSED", false, nil
			case "draft":
				pr["isDraft"] = true
			case "pr_number":
				pr["number"] = int64(4)
			case "merge_commit":
				pr["merge_commit_sha"] = nil
			case "base_dependent":
				d, _ := contract.ObjectAt(row, "base_dependents")
				d["pull_requests"] = []any{contract.Object{"id": "PR_19", "number": int64(19), "url": testRepo().URL + "/pull/19", "baseRefName": "codex/issue-17", "headRefName": "codex/issue-19", "state": "OPEN", "isDraft": true, "merged": false}}
			case "partial":
				prov["complete"] = false
			case "seed":
				prov["source"] = "seed"
			case "repository":
				ref["repository_node_id"] = "R_recreated"
			case "missing_provenance":
				delete(prov, "selection")
			case "unrequested":
				f.inventory["branches"] = append(f.inventory["branches"].([]any), row)
			case "extra_data":
				pr["invented_field"] = true
			}
			if _, err := PrepareBranchCleanup(context.Background(), f, testRepo(), branchTestSelection(1), time.Now()); err == nil {
				t.Fatal("unqualified cleanup candidate accepted", change)
			}
			if f.writes != 0 {
				t.Fatal("preparation wrote a branch")
			}
		})
	}
}

func TestBranchCleanupRejectsExternalDriftAndUnqualifiedNativeACK(t *testing.T) {
	for _, badACK := range []bool{false, true} {
		f := &branchTestProvider{inventory: branchTestInventory(1)}
		plan := branchTestPlan(t, f, 1)
		f.badACK = badACK
		if !badACK {
			rows, _ := contract.Objects(f.inventory, "branches")
			ref, _ := contract.ObjectAt(rows[0], "branch")
			ref["sha"] = strings.Repeat("c", 40)
		}
		if _, err := branchTestEngine(t.TempDir(), f).Apply(context.Background(), plan.Object()); err == nil {
			t.Fatal("drift/bad acknowledgement reported completion")
		}
		want := int64(0)
		if badACK {
			want = 1
		}
		if f.writes != want {
			t.Fatal("write repeated or drift dispatched", f.writes, want)
		}
	}
}

func TestBranchCleanupFreshProcessHelper(t *testing.T) {
	root := os.Getenv("STEWARD_BRANCH_TEST_ROOT")
	if root == "" {
		return
	}
	bytes, err := os.ReadFile(filepath.Join(root, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contract.Decode(strings.NewReader(string(bytes)))
	if err != nil {
		t.Fatal(err)
	}
	f := &branchTestProvider{path: filepath.Join(root, "provider.json"), crashMode: os.Getenv("STEWARD_BRANCH_TEST_CRASH")}
	if _, err := branchTestEngine(root, f).Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
}

func TestBranchCleanupFreshProcessNeverReplaysUnknownOrCompletedWrites(t *testing.T) {
	for _, mode := range []string{"before-ack", "after-ack"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			f := &branchTestProvider{inventory: branchTestInventory(2), path: filepath.Join(root, "provider.json")}
			if err := f.save(); err != nil {
				t.Fatal(err)
			}
			plan := branchTestPlan(t, f, 2)
			bytes, err := contract.Canonical(plan.Object())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "plan.json"), bytes, 0600); err != nil {
				t.Fatal(err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestBranchCleanupFreshProcessHelper$")
			child.Env = append(os.Environ(), "STEWARD_BRANCH_TEST_ROOT="+root, "STEWARD_BRANCH_TEST_CRASH="+mode)
			out, err := child.CombinedOutput()
			var exit *exec.ExitError
			wantCode := 81
			if mode == "after-ack" {
				wantCode = 82
			}
			if !errors.As(err, &exit) || exit.ExitCode() != wantCode {
				t.Fatalf("child did not stop at native boundary: %v %s", err, out)
			}
			fresh := &branchTestProvider{path: f.path}
			result, err := branchTestEngine(root, fresh).Apply(context.Background(), plan.Object())
			if loadErr := fresh.load(); loadErr != nil {
				t.Fatal(loadErr)
			}
			if mode == "before-ack" {
				if err == nil || fresh.writes != 1 {
					t.Fatal("unknown deletion replayed", result, err, fresh.writes)
				}
			} else if err != nil || result["status"] != "completed" || fresh.writes != 2 {
				t.Fatal("durable deletion did not resume remaining operation once", result, err, fresh.writes)
			}
		})
	}
}

func TestBranchCleanupSelectionRejectsAmbiguityAndKeepsCanonicalOrder(t *testing.T) {
	for _, change := range []string{"duplicate", "number", "sha", "name", "extra", "missing"} {
		selection := branchTestSelection(1)
		rows, _ := contract.Objects(selection, "branches")
		switch change {
		case "duplicate":
			selection["branches"] = append(selection["branches"].([]any), rows[0])
		case "number":
			rows[0]["pull_request_number"] = true
		case "sha":
			rows[0]["sha"] = "HEAD"
		case "name":
			rows[0]["name"] = "--force"
		case "extra":
			rows[0]["yes"] = true
		case "missing":
			delete(rows[0], "sha")
		}
		if _, err := ParseBranchCleanupSelection(selection, testRepo()); err == nil {
			t.Fatal("ambiguous selection accepted", change)
		}
	}
}
