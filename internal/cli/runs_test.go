package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestWorkflowDigestRetainsEnvelopeUnicodeAndNumericTokens(t *testing.T) {
	root := checkout(t)
	var stdout, stderr bytes.Buffer
	input := `{"tool_version":"0.1.0","schema_version":2,"data":{"z":"<x>\u2028☃","n":1.00}}`
	err := (Runner{Out: &stdout, Err: &stderr, Input: strings.NewReader(input)}).Run(context.Background(), []string{"runs", "digest", "--repo-root", root, "--input", "document=-"})
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	result, err := contract.Decode(&stdout)
	if err != nil {
		t.Fatal(err)
	}
	data, err := contract.ObjectAt(result, "data")
	if err != nil {
		t.Fatal(err)
	}
	// Independent fixture hashes the complete sorted native JSON, including 1.00,
	// the existing envelope, raw UTF-8 and Go's explicit U+2028 escape.
	if result["command"] != "workflow-digest" || data["sha256"] != "13ff20bc77f7c1f9bed86b1da13a151c0a2398cacf34d6a8bce6093f58e3535f" {
		t.Fatalf("digest dropped or normalized retained evidence: %v", result)
	}
}

func TestContextCLIStaysProviderFreeAndConfinedToRunnerPackage(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	root := checkout(t)
	policy := `{"schema_version":1,"workflows":{"task.yml":{"mutator_step_alternatives":[["Apply reviewed plan"]],"reviewed_source_shas":[],"allow_publication":false,"plans":{"workflow-noop":{"command":"workflow-noop","domain_profile":"workflow-noop","allowed_operation_kinds":[],"attempt_target":"workflow_noop","approval":{"kind":"local-noop","workflow_source_sha256":"` + strings.Repeat("a", 64) + `","mutators":[{"job":"Mutation job","steps":["Apply reviewed plan"]}]},"event":{"kind":"workflow_noop","path":"events/trigger-event.json"},"parent_merge":null}}}}}`
	if err := os.WriteFile(filepath.Join(root, "policy.json"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	input := `{"workflow_file":"task.yml","repository":"example/widgets","recovery_key":"task","run_name":"Current","run_id":1,"attempt":1,"attempt_target":{"event":"exact"},"dispatch_steps":[],"trusted_source_sha":"` + strings.Repeat("b", 40) + `"}`
	// No gh binary is available: local context operations must not authenticate
	// or initialize provider transport.
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, kind := range []string{"direct", "outside", "nested", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			temp := t.TempDir()
			pkg := filepath.Join(temp, "invocation")
			switch kind {
			case "outside":
				pkg = t.TempDir()
			case "nested":
				if err := os.Mkdir(filepath.Join(temp, "parent"), 0700); err != nil {
					t.Fatal(err)
				}
				pkg = filepath.Join(temp, "parent", "invocation")
			case "symlink":
				if err := os.Symlink(t.TempDir(), pkg); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "outside" && kind != "symlink" {
				if err := os.Mkdir(pkg, 0700); err != nil {
					t.Fatal(err)
				}
			}
			var out, stderr bytes.Buffer
			err := (Runner{Out: &out, Err: &stderr, Input: strings.NewReader(input)}).Run(context.Background(), []string{"runs", "context-start", "--repo-root", root, "--policy", "policy.json", "--runner-temp", temp, "--package-root", pkg, "--input", "context=-"})
			if kind == "direct" {
				if err != nil {
					t.Fatal(err, stderr.String())
				}
				result, err := contract.Decode(&out)
				if err != nil {
					t.Fatal(err)
				}
				data, err := contract.ObjectAt(result, "data")
				if err != nil || data["phase"] != "started" {
					t.Fatal("local context did not initialize", result, err)
				}
			} else {
				if err == nil || out.Len() != 0 {
					t.Fatal("unconfined context emitted success")
				}
				if _, err := os.Lstat(filepath.Join(pkg, "run-context.json")); !os.IsNotExist(err) {
					t.Fatal("unconfined context changed execution evidence")
				}
			}
		})
	}
}

func TestActionsRecoveryRejectsTriggerSHAWithoutCallingProvider(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	for _, action := range []string{"finalize", "verify-publication", "acquire-publication-candidate", "finish-noop"} {
		for _, runtimeSHA := range []string{"", "malformed", strings.Repeat("a", 40)} {
			t.Run(action+"/"+runtimeSHA, func(t *testing.T) {
				t.Setenv("GITHUB_WORKFLOW_SHA", runtimeSHA)
				var out, stderr bytes.Buffer
				err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), []string{
					"runs", action, "--repo-root", filepath.Join(t.TempDir(), "missing-checkout"), "--workflow-sha", strings.Repeat("b", 40),
				})
				if err == nil || !strings.Contains(err.Error(), "GITHUB_WORKFLOW_SHA") || out.Len() != 0 {
					t.Fatalf("untrusted runtime source reached checkout/provider or emitted success: %v", err)
				}
			})
		}
	}
	t.Setenv("GITHUB_WORKFLOW_SHA", strings.Repeat("a", 40))
	if err := validateActionsWorkflowSource("finish-noop", strings.Repeat("a", 40)); err != nil {
		t.Fatal("actual runtime workflow source was rejected", err)
	}
	if err := validateActionsWorkflowSource("context-start", strings.Repeat("b", 40)); err == nil {
		t.Fatal("context accepted its trigger source as trusted control")
	}
	t.Setenv("GITHUB_ACTIONS", "")
	if err := validateActionsWorkflowSource("finish-noop", strings.Repeat("b", 40)); err != nil {
		t.Fatal("explicit offline source qualification was rejected", err)
	}
}

func TestActionsRecoveryRequiresExactControlCheckoutAndCommittedPolicy(t *testing.T) {
	root := checkout(t)
	policyPath := "policy.json"
	if err := os.WriteFile(filepath.Join(root, policyPath), []byte("{\"schema_version\":1}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "policy.json"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Reviewed policy"}} {
		command := exec.Command("git", args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
	}
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = root
	head, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_WORKFLOW_SHA", strings.TrimSpace(string(head)))
	if err := validateActionsControlCheckout(context.Background(), root, policyPath); err != nil {
		t.Fatal("exact committed control policy was rejected", err)
	}
	// Untracked run scratch is allowed; changing the declarative authority is not.
	if err := os.WriteFile(filepath.Join(root, "scratch.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateActionsControlCheckout(context.Background(), root, policyPath); err != nil {
		t.Fatal("unrelated run scratch changed control trust", err)
	}
	if err := os.WriteFile(filepath.Join(root, policyPath), []byte("{\"schema_version\":2}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateActionsControlCheckout(context.Background(), root, policyPath); err == nil {
		t.Fatal("modified policy was accepted at the same commit")
	}
	t.Setenv("GITHUB_WORKFLOW_SHA", strings.Repeat("b", 40))
	if err := validateActionsControlCheckout(context.Background(), root, policyPath); err == nil {
		t.Fatal("source-branch checkout was accepted as runtime control")
	}
}

type noRecoveryProviderReads struct{ t *testing.T }

func TestRetiredHistoryCommandsRejectWithoutProviderReads(t *testing.T) {
	for _, action := range []string{"legacy-review", "legacy-import-preview", "import-legacy", "cutover-preview", "cutover-validate", "promotion-preview", "promotion-validate"} {
		t.Run(action, func(t *testing.T) {
			var out, stderr bytes.Buffer
			err := (Runner{Out: &out, Err: &stderr, Actions: noRecoveryProviderReads{t: t}}).Run(context.Background(), []string{"runs", action, "--repo-root", checkout(t)})
			if err == nil || out.Len() != 0 {
				t.Fatal("retired migration command returned success", err, out.String())
			}
		})
	}
}

func (r noRecoveryProviderReads) Read(context.Context, string) (contract.Object, error) {
	r.t.Fatal("local or pending recovery command made a provider read")
	return nil, nil
}
func (r noRecoveryProviderReads) Pages(context.Context, string) ([]any, error) {
	r.t.Fatal("local or pending recovery command paginated provider state")
	return nil, nil
}
func (r noRecoveryProviderReads) Archive(context.Context, int64) ([]byte, error) {
	r.t.Fatal("local or pending recovery command downloaded an artifact")
	return nil, nil
}

func TestActionsContextAndOrdinaryFinalizePreserveDocumentedOmittedFlagContract(t *testing.T) {
	root := checkout(t)
	policy := `{"schema_version":1,"workflows":{"task.yml":{"mutator_step_alternatives":[["Apply"]],"reviewed_source_shas":[],"allow_publication":false,"plans":{}}}}`
	if err := os.WriteFile(filepath.Join(root, "policy.json"), []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "policy.json"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Reviewed policy"}} {
		command := exec.Command("git", args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
	}
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = root
	headBytes, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(headBytes))
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_WORKFLOW_SHA", head)
	temp := t.TempDir()
	pkg := filepath.Join(temp, "invocation")
	if err := os.Mkdir(pkg, 0700); err != nil {
		t.Fatal(err)
	}
	input := `{"workflow_file":"task.yml","repository":"example/widgets","recovery_key":"task","run_name":"Current","run_id":1,"attempt":1,"attempt_target":{"event":"exact"},"dispatch_steps":["Apply"],"trusted_source_sha":"` + head + `"}`
	var out, stderr bytes.Buffer
	runner := Runner{Out: &out, Err: &stderr, Input: strings.NewReader(input), Actions: noRecoveryProviderReads{t}}
	if err := runner.Run(context.Background(), []string{"runs", "context-start", "--repo-root", root, "--policy", "policy.json", "--runner-temp", temp, "--package-root", pkg, "--input", "context=-"}); err != nil {
		t.Fatal("documented context-start without a SHA flag failed", err)
	}
	out.Reset()
	if err := runner.Run(context.Background(), []string{"runs", "finalize", "--repo-root", root, "--policy", "policy.json", "--runner-temp", temp, "--package-root", pkg,
		"--workflow", "task.yml", "--run-id", "1", "--attempt", "1", "--artifact-id", "1", "--artifact-digest", "sha256:" + strings.Repeat("a", 64), "--checkpoint", filepath.Join(temp, "checkpoint")}); err != nil {
		t.Fatal("ordinary finalization without a SHA flag failed", err)
	}
	result, err := contract.Decode(&out)
	if err != nil {
		t.Fatal(err)
	}
	if result["data"].(contract.Object)["outcome"] != "pending" {
		t.Fatal("nonterminal context was settled", result)
	}
	if _, err := os.Stat(filepath.Join(temp, "checkpoint")); !os.IsNotExist(err) {
		t.Fatal("nonterminal finalization created a checkpoint")
	}
	out.Reset()
	runner.Input = strings.NewReader(strings.Replace(input, head, strings.Repeat("b", 40), 1))
	if err := runner.Run(context.Background(), []string{"runs", "context-start", "--repo-root", root, "--policy", "policy.json", "--runner-temp", temp, "--package-root", pkg, "--input", "context=-"}); err == nil || !strings.Contains(err.Error(), "GITHUB_WORKFLOW_SHA") || out.Len() != 0 {
		t.Fatal("context input accepted a trigger SHA as trusted workflow source", err)
	}
}

func TestWorkflowDigestRejectsDuplicateFieldsAndAmbiguousInputs(t *testing.T) {
	root := checkout(t)
	for _, args := range [][]string{
		{"--input", "document=-"},
		{"--input", "wrong=-"},
		{"--input", "document=-", "--input", "document=-"},
		{"--format", "text", "--input", "document=-"},
	} {
		var stdout, stderr bytes.Buffer
		err := (Runner{Out: &stdout, Err: &stderr, Input: strings.NewReader(`{"run":1,"run":2}`)}).Run(context.Background(), append([]string{"runs", "digest", "--repo-root", root}, args...))
		if err == nil || stdout.Len() != 0 {
			t.Fatal("invalid digest input emitted success", args, err)
		}
	}
}
