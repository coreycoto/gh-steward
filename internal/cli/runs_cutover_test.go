package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/runrecovery"
)

type cutoverCLIReader struct {
	objects map[string]contract.Object
	pages   map[string][]any
	reads   []string
}

func (r *cutoverCLIReader) Read(_ context.Context, endpoint string) (contract.Object, error) {
	r.reads = append(r.reads, endpoint)
	if value, exists := r.objects[endpoint]; exists {
		return value, nil
	}
	return nil, fmt.Errorf("unexpected read: %s", endpoint)
}
func (r *cutoverCLIReader) Pages(_ context.Context, endpoint string) ([]any, error) {
	r.reads = append(r.reads, endpoint)
	if value, exists := r.pages[endpoint]; exists {
		return value, nil
	}
	return nil, fmt.Errorf("unexpected pages: %s", endpoint)
}
func (r *cutoverCLIReader) Archive(context.Context, int64) ([]byte, error) {
	return nil, errors.New("cutover must not acquire archives or replay historical packages")
}

func emptyCutoverCLIReader() *cutoverCLIReader {
	return &cutoverCLIReader{
		objects: map[string]contract.Object{
			"repos/example/widgets/actions/workflows/task.yml": {"id": int64(9), "path": ".github/workflows/task.yml"},
		},
		pages: map[string][]any{
			"repos/example/widgets/actions/workflows/task.yml/runs?per_page=100": {contract.Object{"total_count": int64(0), "workflow_runs": []any{}}},
			"repos/example/widgets/actions/artifacts?per_page=100":               {contract.Object{"total_count": int64(0), "artifacts": []any{}}},
		},
	}
}

func cutoverCLIPolicy(review bool) contract.Object {
	workflow := contract.Object{"mutator_step_alternatives": []any{[]any{"Apply reviewed plan"}}, "reviewed_source_shas": []any{}, "allow_publication": false, "plans": contract.Object{}}
	if review {
		workflow["history_cutover_review_issue"] = contract.Object{"number": int64(17), "trusted_logins": []any{"maintainer"}}
	}
	return contract.Object{"schema_version": int64(1), "workflows": contract.Object{"task.yml": workflow}}
}

func cutoverCLIRepository(t *testing.T) contract.Repository {
	t.Helper()
	repository, err := contract.ParseRepository(contract.Object{"nameWithOwner": "example/widgets", "url": "https://github.com/example/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func cutoverCLIBaseline(t *testing.T) contract.Object {
	t.Helper()
	baseline, err := runrecovery.CaptureHistoryCutover(context.Background(), emptyCutoverCLIReader(), cutoverCLIRepository(t), "task.yml", nil)
	if err != nil {
		t.Fatal(err)
	}
	return baseline
}

func TestCutoverPreviewRetainsPrivateEvidenceAndPrintsOnlySummary(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	root := checkout(t)
	writeLegacyCLIJSON(t, root, "policy.json", cutoverCLIPolicy(false))
	reader := emptyCutoverCLIReader()
	reader.objects["repos/example/widgets/issues/17"] = contract.Object{"title": "sensitive retained review evidence"}
	var stdout, stderr bytes.Buffer
	err := (Runner{Out: &stdout, Err: &stderr, Actions: reader}).Run(context.Background(), []string{"runs", "cutover-preview", "--repo-root", root, "--workflow", "task.yml", "--policy", "policy.json", "--state-read", "repos/example/widgets/issues/17", "--out", "baseline.json"})
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	if strings.Contains(stdout.String(), "sensitive retained") || strings.Contains(stdout.String(), "state_reads") {
		t.Fatal("stdout leaked complete private provider evidence", stdout.String())
	}
	result, err := contract.Decode(bytes.NewReader(stdout.Bytes()))
	data, dataErr := contract.ObjectAt(result, "data")
	if err != nil || dataErr != nil || data["activation"] != "not-performed" || data["validation"] != "complete-live-read-only-capture" {
		t.Fatal("preview incorrectly claimed activation", result, err)
	}
	path := filepath.Join(root, "baseline.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("full evidence did not remain private", info, err)
	}
	value, err := runrecovery.LoadJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := unwrapHistoryCutover(value.(contract.Object), cutoverCLIRepository(t))
	if err != nil || baseline["sha256"] != data["baseline_sha256"] {
		t.Fatal("summary and retained baseline differ", err)
	}
	if len(reader.reads) != 4 {
		t.Fatal("preview made unexpected reads", reader.reads)
	}
}

func TestCutoverValidationIsOfflineAndRejectsTamperOrForeignEnvelope(t *testing.T) {
	root := checkout(t)
	baseline := cutoverCLIBaseline(t)
	pathWithGitOnly(t)
	for _, kind := range []string{"raw", "envelope", "tamper", "foreign", "wrong-command"} {
		t.Run(kind, func(t *testing.T) {
			object := contract.Object{}
			for key, value := range baseline {
				object[key] = value
			}
			if kind == "tamper" {
				object["scope"] = "apply"
			} else if kind != "raw" {
				object = contract.Object{"schema_version": int64(2), "tool_version": "0.4.0", "command": "workflow-cutover-preview", "repository": cutoverCLIRepository(t).Object(), "data": object}
				if kind == "foreign" {
					object["repository"] = contract.Object{"nameWithOwner": "other/widgets", "url": "https://github.com/other/widgets"}
				} else if kind == "wrong-command" {
					object["command"] = "workflow-import-legacy"
				}
			}
			writeLegacyCLIJSON(t, root, "input.json", object)
			var stdout, stderr bytes.Buffer
			err := (Runner{Out: &stdout, Err: &stderr, Actions: forbiddenLegacyCLIProvider{t}}).Run(context.Background(), []string{"runs", "cutover-validate", "--repo-root", root, "--input", "baseline=input.json"})
			valid := kind == "raw" || kind == "envelope"
			if (err == nil) != valid || (!valid && stdout.Len() != 0) {
				t.Fatalf("offline validation %s: %v; %s", kind, err, stdout.String())
			}
		})
	}
}

func TestCutoverPreviewRejectsUnreviewableOutputAndUntrustedActionsPolicyBeforeProviderReads(t *testing.T) {
	root := checkout(t)
	writeLegacyCLIJSON(t, root, "policy.json", cutoverCLIPolicy(false))
	for _, kind := range []string{"missing-out", "overlap", "symlink", "untrusted-actions-policy"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", "")
			args := []string{"runs", "cutover-preview", "--repo-root", root, "--workflow", "task.yml", "--policy", "policy.json"}
			switch kind {
			case "overlap":
				args = append(args, "--out", "policy.json")
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "outside")); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--out", "outside/evidence.json")
			case "untrusted-actions-policy":
				t.Setenv("GITHUB_ACTIONS", "true")
				t.Setenv("GITHUB_WORKFLOW_SHA", strings.Repeat("a", 40))
				args = append(args, "--out", "untrusted.json")
			}
			var stdout, stderr bytes.Buffer
			err := (Runner{Out: &stdout, Err: &stderr, Actions: forbiddenLegacyCLIProvider{t}}).Run(context.Background(), args)
			if err == nil || stdout.Len() != 0 {
				t.Fatalf("unsafe preview %s emitted success: %v", kind, err)
			}
		})
	}
}

func cutoverReviewComment(id int64, body, login, userType string) contract.Object {
	return contract.Object{"id": id, "body": body, "html_url": fmt.Sprintf("https://github.com/example/widgets/issues/17#issuecomment-%d", id), "created_at": "2026-10-05T12:00:00Z", "updated_at": "2026-10-05T12:00:00Z", "user": contract.Object{"login": login, "type": userType}}
}

func TestCutoverReviewsRequireCurrentCompleteExactMaintainerApproval(t *testing.T) {
	baseline := cutoverCLIBaseline(t)
	digest := fmt.Sprint(baseline["sha256"])
	approve := "APPROVE HISTORY CUTOVER example/widgets#17 task.yml " + digest
	revoke := "REVOKE HISTORY CUTOVER example/widgets#17 task.yml " + digest
	for _, kind := range []string{"approved", "revoked", "superseded", "deleted", "bot", "untrusted", "read-only", "foreign-permission", "duplicate", "incomplete", "foreign-comment", "malformed", "timestamp"} {
		t.Run(kind, func(t *testing.T) {
			repository := cutoverCLIRepository(t)
			engine, err := runrecovery.NewEngine(cutoverCLIPolicy(true), repository)
			if err != nil {
				t.Fatal(err)
			}
			rows := []any{cutoverReviewComment(100, approve, "maintainer", "User")}
			permission := contract.Object{"permission": "write", "user": contract.Object{"login": "maintainer", "type": "User"}}
			count := int64(1)
			switch kind {
			case "revoked":
				rows = append(rows, cutoverReviewComment(101, revoke, "maintainer", "User"))
				count = 2
			case "superseded":
				rows = append(rows, cutoverReviewComment(101, "APPROVE HISTORY CUTOVER example/widgets#17 task.yml "+strings.Repeat("a", 64), "maintainer", "User"))
				count = 2
			case "deleted":
				rows = []any{}
				count = 0
			case "bot":
				rows = []any{cutoverReviewComment(100, approve, "maintainer", "Bot")}
			case "untrusted":
				rows = []any{cutoverReviewComment(100, approve, "stranger", "User")}
			case "read-only":
				permission["permission"] = "read"
			case "foreign-permission":
				permission["user"] = contract.Object{"login": "other", "type": "User"}
			case "duplicate":
				rows = append(rows, rows[0])
				count = 2
			case "incomplete":
				count = 2
			case "foreign-comment":
				rows[0].(contract.Object)["html_url"] = "https://github.com/other/widgets/issues/17#issuecomment-100"
			case "malformed":
				rows[0].(contract.Object)["body"] = "APPROVE HISTORY CUTOVER example/widgets#17 task.yml invalid"
			case "timestamp":
				rows[0].(contract.Object)["updated_at"] = "2026-10-01T12:00:00Z"
			}
			reader := &cutoverCLIReader{objects: map[string]contract.Object{
				"repos/example/widgets/issues/17":                           {"id": int64(71), "number": int64(17), "comments": count, "html_url": "https://github.com/example/widgets/issues/17"},
				"repos/example/widgets/collaborators/maintainer/permission": permission,
			}, pages: map[string][]any{"repos/example/widgets/issues/17/comments?per_page=100": {rows}}}
			err = resolveHistoryCutoverReviews(context.Background(), reader, engine, repository, "task.yml")
			_, admittedErr := engine.ValidateReviewedHistoryCutover(baseline, baseline["target"])
			if kind == "approved" {
				if err != nil || admittedErr != nil {
					t.Fatalf("current exact approval rejected: %v %v", err, admittedErr)
				}
			} else if admittedErr == nil {
				t.Fatalf("%s admitted an unreviewed or revoked baseline", kind)
			}
		})
	}
}

func TestCutoverReviewPaginationAndLatestDecisionAreDeterministic(t *testing.T) {
	digest := strings.Repeat("a", 64)
	rows := make([]any, 100)
	for index := range rows {
		rows[index] = cutoverReviewComment(int64(index+1), "ordinary discussion", "maintainer", "User")
	}
	rows[0] = cutoverReviewComment(1, "APPROVE HISTORY CUTOVER example/widgets#17 task.yml "+digest, "maintainer", "User")
	pages := []any{rows, []any{cutoverReviewComment(101, "REVOKE HISTORY CUTOVER example/widgets#17 task.yml "+digest, "maintainer", "User")}}
	decisions, err := cutoverReviewDecisions(pages, 101, map[string]bool{"maintainer": true}, cutoverCLIRepository(t), 17, "task.yml")
	if err != nil || len(decisions) != 1 || decisions[0].approved {
		t.Fatal("later-page revocation did not supersede the earlier exact approval", decisions, err)
	}
}

func TestCutoverReviewReadFailureRetainsTypedHoldWithoutStartingRecovery(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	root := checkout(t)
	writeLegacyCLIJSON(t, root, "policy.json", cutoverCLIPolicy(true))
	writeLegacyCLIJSON(t, root, "baseline.json", cutoverCLIBaseline(t))
	temporary := t.TempDir()
	packageRoot := filepath.Join(temporary, "invocation")
	reader := &cutoverCLIReader{objects: map[string]contract.Object{}, pages: map[string][]any{}}
	var stdout, stderr bytes.Buffer
	err := (Runner{Out: &stdout, Err: &stderr, Actions: reader}).Run(context.Background(), []string{
		"runs", "recover", "--repo-root", root, "--workflow", "task.yml", "--policy", "policy.json",
		"--run-id", "101", "--attempt", "1", "--run-name", "Current", "--recovery-key", "current-target",
		"--runner-temp", temporary, "--package-root", packageRoot, "--input", "history-cutover=baseline.json",
	})
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	result, err := contract.Decode(bytes.NewReader(stdout.Bytes()))
	data, dataErr := contract.ObjectAt(result, "data")
	if err != nil || dataErr != nil || data["outcome"] != "recovery_needed" {
		t.Fatal("review failure did not retain a typed recovery hold", result, err)
	}
	value, err := runrecovery.LoadJSON(filepath.Join(packageRoot, "recovery-needed.json"))
	if err != nil || !strings.Contains(fmt.Sprint(value), "history cutover review could not be verified") {
		t.Fatal("review failure diagnostic disappeared", value, err)
	}
	for _, path := range []string{"settlement-chain.json", "run-context.json", "publication/qualification.json"} {
		if _, err := os.Lstat(filepath.Join(packageRoot, path)); !os.IsNotExist(err) {
			t.Fatal("review failure advanced recovery or publication", path, err)
		}
	}
	if len(reader.reads) != 1 || reader.reads[0] != "repos/example/widgets/issues/17" {
		t.Fatal("failed approval read started history or provider mutation", reader.reads)
	}
}

func TestCutoverReviewRouteKeepsLocalContextCommandsProviderFree(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	root := checkout(t)
	writeLegacyCLIJSON(t, root, "policy.json", cutoverCLIPolicy(true))
	temporary := t.TempDir()
	packageRoot := filepath.Join(temporary, "invocation")
	if err := os.Mkdir(packageRoot, 0700); err != nil {
		t.Fatal(err)
	}
	contextInput := contract.Object{
		"workflow_file": "task.yml", "repository": "example/widgets", "recovery_key": "current-target", "run_name": "Current",
		"run_id": int64(101), "attempt": int64(1), "attempt_target": contract.Object{"event": "exact"},
		"dispatch_steps": []any{"Apply reviewed plan"}, "trusted_source_sha": strings.Repeat("b", 40),
	}
	writeLegacyCLIJSON(t, root, "context.json", contextInput)
	pathWithGitOnly(t)
	var stdout, stderr bytes.Buffer
	err := (Runner{Out: &stdout, Err: &stderr, Actions: forbiddenLegacyCLIProvider{t}}).Run(context.Background(), []string{
		"runs", "context-start", "--repo-root", root, "--policy", "policy.json", "--workflow", "task.yml",
		"--runner-temp", temporary, "--package-root", packageRoot, "--input", "context=context.json",
	})
	if err != nil {
		t.Fatal("local context unexpectedly required a provider approval read", err, stderr.String())
	}
}
