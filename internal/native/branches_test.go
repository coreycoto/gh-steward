package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

type branchExecutor struct {
	responses []Result
	err       error
	calls     [][]string
	roots     []string
	envs      [][]string
}

func (e *branchExecutor) Execute(_ context.Context, _ string, args []string, _ []byte, root string, env []string) (Result, error) {
	e.calls = append(e.calls, append([]string{}, args...))
	e.roots = append(e.roots, root)
	e.envs = append(e.envs, append([]string{}, env...))
	if len(e.responses) == 0 {
		return Result{}, e.err
	}
	r := e.responses[0]
	e.responses = e.responses[1:]
	return r, nil
}

func branchDeleteACK() contract.Object {
	return contract.Object{"exit_code": int64(0), "stdout": "To https://github.com/example/widgets.git\n-\t:refs/heads/codex/issue-17\t[deleted]\nDone\n", "stderr": "", "remote_url": target().URL + ".git", "ref": "refs/heads/codex/issue-17", "expected_sha": strings.Repeat("a", 40)}
}

func TestLeasedBranchDeleteIsolatesConfigAndRetainsNativePorcelain(t *testing.T) {
	repo := target().Object()
	repo["id"] = "R_1"
	repo["defaultBranchRef"] = contract.Object{"name": "main"}
	exec := &branchExecutor{responses: []Result{labelResult(contract.Object{"repository": repo}), deliveryBranch("codex/issue-17", strings.Repeat("a", 40)), {}, {Stdout: []byte(branchDeleteACK()["stdout"].(string)), Stderr: []byte("native provider detail")}}}
	tpt := &Transport{Repository: target(), Root: t.TempDir(), Executor: exec, Executable: "/test/native gh'quoted", Timeout: time.Second,
		Environment: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GH_TOKEN=fixture", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=url.evil.insteadOf", "GIT_CONFIG_VALUE_0=https://github.com/", "GIT_TRACE=1", "GIT_DIR=/wrong", "SSH_ASKPASS=/wrong"}}
	ack, err := tpt.DeleteBranch(context.Background(), "codex/issue-17", strings.Repeat("a", 40), "R_1", strings.Repeat("d", 32))
	if err != nil || ack["stderr"] != "native provider detail" {
		t.Fatal("native acknowledgement not retained", ack, err)
	}
	if len(exec.calls) != 4 {
		t.Fatal("unexpected native dispatch count", exec.calls)
	}
	args := strings.Join(exec.calls[3], "\n")
	for _, expected := range []string{"--force-with-lease=refs/heads/codex/issue-17:" + strings.Repeat("a", 40), "https://github.com/example/widgets.git", ":refs/heads/codex/issue-17", "--no-verify", "core.hooksPath=" + os.DevNull, "http.followRedirects=false", "credential.https://github.com.helper=!'/test/native gh'\"'\"'quoted' auth git-credential"} {
		if !strings.Contains(args, expected) {
			t.Fatal("missing scoped deletion argument", expected, args)
		}
	}
	if exec.roots[3] == tpt.Root || exec.roots[2] != exec.roots[3] {
		t.Fatal("Git did not use isolated disposable scratch", exec.roots)
	}
	if _, err := os.Stat(exec.roots[3]); !os.IsNotExist(err) {
		t.Fatal("temporary Git context retained", err)
	}
	for _, bad := range []string{"GIT_TRACE", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_DIR", "SSH_ASKPASS"} {
		if envValue(exec.envs[3], bad) != "" {
			t.Fatal("ambient Git override retained", bad)
		}
	}
	if envValue(exec.envs[3], "GH_TOKEN") != "fixture" || envValue(exec.envs[3], "GIT_CONFIG_GLOBAL") != os.DevNull || envValue(exec.envs[3], "GIT_TERMINAL_PROMPT") != "0" {
		t.Fatal("native auth or isolation context lost")
	}
}

func TestBranchDeleteRejectsDriftAndUnknownOutcomeWithoutRetry(t *testing.T) {
	for _, change := range []string{"default", "repository", "head", "unknown", "rejected"} {
		t.Run(change, func(t *testing.T) {
			repo := target().Object()
			repo["id"] = "R_1"
			repo["defaultBranchRef"] = contract.Object{"name": "main"}
			sha := strings.Repeat("a", 40)
			if change == "default" {
				repo["defaultBranchRef"] = contract.Object{"name": "codex/issue-17"}
			}
			if change == "repository" {
				repo["id"] = "R_recreated"
			}
			if change == "head" {
				sha = strings.Repeat("b", 40)
			}
			exec := &branchExecutor{responses: []Result{labelResult(contract.Object{"repository": repo}), deliveryBranch("codex/issue-17", sha), {}}, err: errors.New("timeout after dispatch")}
			if change == "rejected" {
				exec.responses = append(exec.responses, Result{ExitCode: 1, Stdout: []byte("To https://github.com/example/widgets.git\n!\t:refs/heads/codex/issue-17\t[rejected] (stale info)\nDone\n")})
			}
			tpt := &Transport{Repository: target(), Root: t.TempDir(), Executor: exec, Executable: "/test/gh", Timeout: time.Second, Environment: os.Environ()}
			if _, err := tpt.DeleteBranch(context.Background(), "codex/issue-17", strings.Repeat("a", 40), "R_1", strings.Repeat("d", 32)); err == nil {
				t.Fatal("unqualified deletion accepted")
			}
			limit := 4
			if change == "default" || change == "repository" {
				limit = 1
			}
			if change == "head" {
				limit = 2
			}
			if len(exec.calls) != limit {
				t.Fatal("write retried or drift reached Git", exec.calls)
			}
		})
	}
}

func TestBranchDeletionRetainedReceiptRequiresOneExactPositiveRef(t *testing.T) {
	for _, change := range []string{"empty", "foreign", "extra", "repeat", "wrong-ref", "up-to-date", "rejected", "exit", "wrong-sha", "missing", "extra-key"} {
		t.Run(change, func(t *testing.T) {
			ack := branchDeleteACK()
			out := ack["stdout"].(string)
			switch change {
			case "empty":
				out = ""
			case "foreign":
				out = strings.ReplaceAll(out, "example/widgets", "other/widgets")
			case "extra":
				out = strings.Replace(out, "Done", "-\t:refs/heads/other\t[deleted]\nDone", 1)
			case "repeat":
				out += out
			case "wrong-ref":
				out = strings.ReplaceAll(out, "codex/issue-17", "codex/issue-18")
			case "up-to-date":
				out = strings.Replace(out, "-\t", "=\t", 1)
			case "rejected":
				out = strings.Replace(out, "-\t", "!\t", 1)
			case "exit":
				ack["exit_code"] = int64(1)
			case "wrong-sha":
				ack["expected_sha"] = strings.Repeat("b", 40)
			case "missing":
				delete(ack, "stderr")
			case "extra-key":
				ack["completed"] = true
			}
			ack["stdout"] = out
			if err := ValidateBranchDeletion(ack, target(), "codex/issue-17", strings.Repeat("a", 40)); err == nil {
				t.Fatal("bad native completion accepted", change)
			}
		})
	}
}

func TestNativeGitExplicitLeaseProtectsAChangedBranchInFreshScratch(t *testing.T) {
	ctx := context.Background()
	git := "/usr/bin/git"
	executor := ProcessExecutor{}
	root := t.TempDir()
	env := isolatedGitEnvironment(target(), os.Environ())
	run := func(args ...string) Result {
		t.Helper()
		r, e := executor.Execute(ctx, git, args, nil, root, env)
		if e != nil || r.ExitCode != 0 {
			t.Fatal("scratch Git setup failed", args, e, string(r.Stderr))
		}
		return r
	}
	remote := filepath.Join(root, "remote.git")
	scratch := filepath.Join(root, "scratch.git")
	run("init", "--bare", "--quiet", "--template=", remote)
	run("init", "--bare", "--quiet", "--template=", scratch)
	tree := strings.TrimSpace(string(run("--git-dir="+remote, "mktree").Stdout))
	commit := func(message string) string {
		return strings.TrimSpace(string(run("--git-dir="+remote, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit-tree", tree, "-m", message).Stdout))
	}
	old, newSHA := commit("old"), commit("new")
	ref := "refs/heads/codex/issue-17"
	run("--git-dir="+remote, "update-ref", ref, newSHA)
	// Local file transport is enabled only in this scratch fixture. Production
	// requires the explicit HTTPS target and forbids file/ext transports.
	args := []string{"--git-dir=" + scratch, "-c", "protocol.file.allow=always", "-c", "core.hooksPath=" + os.DevNull, "push", "--porcelain", "--no-verify", "--force-with-lease=" + ref + ":" + old, remote, ":" + ref}
	r, e := executor.Execute(ctx, git, args, nil, root, env)
	if e != nil || r.ExitCode == 0 || !strings.Contains(string(r.Stdout), "[rejected] (stale info)") {
		t.Fatal("changed branch was deleted", r, e)
	}
	if got := strings.TrimSpace(string(run("--git-dir="+remote, "rev-parse", ref).Stdout)); got != newSHA {
		t.Fatal("lease rejection changed ref", got)
	}
	args[len(args)-3] = "--force-with-lease=" + ref + ":" + newSHA
	r, e = executor.Execute(ctx, git, args, nil, root, env)
	if e != nil || r.ExitCode != 0 || !strings.Contains(string(r.Stdout), "-\t:"+ref+"\t[deleted]") {
		t.Fatal("exact native lease did not delete fixture", r, e)
	}
	absent, e := executor.Execute(ctx, git, []string{"--git-dir=" + remote, "show-ref", "--verify", ref}, nil, root, env)
	if e != nil || absent.ExitCode == 0 {
		t.Fatal("exact lease did not remove ref", absent, e)
	}
}

func basePRPage(rows []any, next bool, cursor any) Result {
	repo := target().Object()
	repo["id"] = "R_1"
	repo["pullRequests"] = contract.Object{"nodes": rows, "pageInfo": contract.Object{"hasNextPage": next, "endCursor": cursor}}
	return labelResult(contract.Object{"repository": repo})
}

func basePR(number int64, draft bool) contract.Object {
	return contract.Object{"id": fmt.Sprintf("PR_%d", number), "number": number, "url": fmt.Sprintf("%s/pull/%d", target().URL, number), "baseRefName": "codex/issue-17", "headRefName": fmt.Sprintf("codex/issue-%d", number), "state": "OPEN", "isDraft": draft, "merged": false}
}

func TestOpenBasePRInventoryIncludesDraftsAndPaginatesExactly(t *testing.T) {
	exec := &sequenceExecutor{responses: []Result{basePRPage([]any{basePR(23, true)}, true, "next"), basePRPage([]any{basePR(18, false)}, false, nil)}}
	inventory, err := sequence(exec).ReadOpenPullRequestsForBase(context.Background(), "codex/issue-17")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := contract.Objects(inventory, "pull_requests")
	if len(rows) != 2 || rows[0]["number"] != json.Number("18") || rows[1]["isDraft"] != true {
		t.Fatal("incomplete ordered dependency collection", rows)
	}
	if inventory["repository_node_id"] != "R_1" || inventory["base_branch"] != "codex/issue-17" {
		t.Fatal("dependency scope missing", inventory)
	}
	vars, _ := contract.ObjectAt(exec.inputs[1], "variables")
	if vars["base"] != "codex/issue-17" || vars["cursor"] != "next" || !strings.Contains(exec.inputs[1]["query"].(string), "states:[OPEN]") {
		t.Fatal("base pagination scope changed", vars)
	}
}

func TestOpenBasePRInventoryRejectsPartialForeignAndRepeatedEvidence(t *testing.T) {
	for _, change := range []string{"wrong_base", "url", "state", "merged", "draft", "head", "number", "repeat_number", "repeat_id", "repository", "cursor", "no_cursor", "no_page"} {
		t.Run(change, func(t *testing.T) {
			pr := basePR(23, true)
			rows := []any{pr}
			responses := []Result{}
			switch change {
			case "wrong_base":
				pr["baseRefName"] = "other"
			case "url":
				pr["url"] = "https://github.com/other/widgets/pull/23"
			case "state":
				pr["state"] = "CLOSED"
			case "merged":
				pr["merged"] = true
			case "draft":
				pr["isDraft"] = "false"
			case "head":
				pr["headRefName"] = "--evil"
			case "number":
				pr["number"] = true
			case "repeat_number":
				rows = append(rows, basePR(23, false))
			case "repeat_id":
				other := basePR(24, false)
				other["id"] = pr["id"]
				rows = append(rows, other)
			case "repository":
				responses = append(responses, basePRPage(rows, true, "next"))
				rows = []any{}
				second := basePRPage(rows, false, nil)
				second.Stdout = []byte(strings.ReplaceAll(string(second.Stdout), `"id":"R_1"`, `"id":"R_recreated"`))
				responses = append(responses, second)
			case "cursor":
				responses = []Result{basePRPage(rows, true, "repeat"), basePRPage([]any{}, true, "repeat")}
			case "no_cursor":
				responses = []Result{basePRPage(rows, true, nil)}
			case "no_page":
				repo := target().Object()
				repo["id"] = "R_1"
				repo["pullRequests"] = contract.Object{"nodes": rows}
				responses = []Result{labelResult(contract.Object{"repository": repo})}
			}
			if len(responses) == 0 {
				responses = []Result{basePRPage(rows, false, nil)}
			}
			exec := &sequenceExecutor{responses: responses}
			if _, err := sequence(exec).ReadOpenPullRequestsForBase(context.Background(), "codex/issue-17"); err == nil {
				t.Fatal("invalid dependency collection accepted", change)
			}
		})
	}
}
