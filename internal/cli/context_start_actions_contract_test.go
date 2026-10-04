package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestActionsContextStartRejectsControlCheckoutDriftBeforeWriting(t *testing.T) {
	for _, drift := range []string{"checkout-head", "committed-policy"} {
		t.Run(drift, func(t *testing.T) {
			root := checkout(t)
			policy := `{"schema_version":1,"workflows":{"task.yml":{"mutator_step_alternatives":[["Apply"]],"reviewed_source_shas":[],"allow_publication":false,"plans":{}}}}`
			policyPath := filepath.Join(root, "policy.json")
			if err := os.WriteFile(policyPath, []byte(policy), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{
				{"add", "policy.json"},
				{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "Reviewed policy"},
			} {
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

			expectedError := "trusted runtime commit blob"
			if drift == "checkout-head" {
				t.Setenv("GITHUB_WORKFLOW_SHA", strings.Repeat("b", 40))
				expectedError = "checkout HEAD"
			} else if err := os.WriteFile(policyPath, []byte(strings.Replace(policy, "Apply", "Different step", 1)), 0600); err != nil {
				t.Fatal(err)
			}

			temp := t.TempDir()
			pkg := filepath.Join(temp, "invocation")
			if err := os.Mkdir(pkg, 0700); err != nil {
				t.Fatal(err)
			}
			// The claimed source matches the runtime in both cases. Only the
			// actual checkout/policy guard can reject the changed authority.
			input := `{"workflow_file":"task.yml","repository":"example/widgets","recovery_key":"task","run_name":"Current","run_id":1,"attempt":1,"attempt_target":{"event":"exact"},"dispatch_steps":["Apply"],"trusted_source_sha":"` + os.Getenv("GITHUB_WORKFLOW_SHA") + `"}`
			var out, stderr bytes.Buffer
			runner := Runner{Out: &out, Err: &stderr, Input: strings.NewReader(input), Actions: noRecoveryProviderReads{t}}
			err = runner.Run(context.Background(), []string{
				"runs", "context-start", "--repo-root", root, "--policy", "policy.json",
				"--runner-temp", temp, "--package-root", pkg, "--input", "context=-",
			})
			if err == nil || !strings.Contains(err.Error(), expectedError) || out.Len() != 0 {
				t.Fatalf("drifted control source reached context initialization: %v; output=%s", err, out.String())
			}
			if _, err := os.Lstat(filepath.Join(pkg, "run-context.json")); !os.IsNotExist(err) {
				t.Fatal("rejected control source changed execution evidence", err)
			}
		})
	}
}
