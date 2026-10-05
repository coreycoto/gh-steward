package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestContextRecordPlanDirectInputPreservesNumericLexemesWithoutProvider(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	repository := testContextRepository("example", "widgets")
	data := executionPlanData(17)
	data["numeric_evidence"] = []any{
		json.Number("1.2300"),
		json.Number("1e+04"),
		json.Number("900719925474099312345678901234567890"),
		json.Number("-0"),
	}
	fixture := newExecutionContextFixture(t, repository, "execution-sync", data)
	preparePath := filepath.Join(fixture.root, "prepare-result.json")
	writeCanonicalFile(t, preparePath, planExtractionEnvelope(fixture.plan, "execution-sync-prepare"))
	planPath := filepath.Join(fixture.root, "native-plan.json")
	var prepareOut, prepareErr bytes.Buffer
	if err := (Runner{Out: &prepareOut, Err: &prepareErr}).Run(context.Background(), []string{
		"plan", "extract", "--repo-root", fixture.root, "--repo", fixture.repository.URL,
		"--input", "envelope=" + preparePath, "--outer-command", "execution-sync-prepare",
		"--plan-command", "execution-sync", "--out", "native-plan.json",
	}); err != nil {
		t.Fatal("producer plan extraction failed:", err, prepareErr.String())
	}
	planBytes, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}

	result := runContextRecordPlan(t, fixture, []string{
		"--input", "native-plan=" + planPath,
		"--name", "execution",
		"--relative", "plans/execution.json",
		"--plan-command", "execution-sync",
		"--review-path", "reviews/execution.json",
		"--review-sha256", fixture.reviewSHA256,
	})
	if result == nil {
		t.Fatal("direct context plan registration returned no result")
	}

	got, err := os.ReadFile(filepath.Join(fixture.packageRoot, "plans/execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), planBytes...)
	if !bytes.Equal(got, want) {
		t.Fatalf("persisted native plan changed canonical input bytes\n got: %s\nwant: %s", got, want)
	}
	for _, lexeme := range []string{"1.2300", "1e+04", "900719925474099312345678901234567890", "-0"} {
		if !bytes.Contains(got, []byte(lexeme)) {
			t.Errorf("persisted plan lost numeric token %q: %s", lexeme, got)
		}
	}
}

func TestContextRecordPlanDirectInputAcceptsNoReviewPair(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	root := checkout(t)
	repository := testContextRepository("example", "widgets")
	plan, err := contract.PreparePlan("quarter-backlog", repository,
		contract.Object{"github": contract.Object{"live": true, "complete": true}},
		contract.Object{"quarter": "2026-Q4"},
		[]contract.Operation{{ID: "quarter:update", Kind: "issue-update", Target: contract.Object{"issue_number": int64(17)}, Before: contract.Object{"title": "Old"}, After: contract.Object{"title": "New"}}},
		time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	policy := contract.Object{"schema_version": int64(1), "workflows": contract.Object{"task.yml": contract.Object{
		"mutator_step_alternatives": [][]string{{"Apply reviewed plan"}}, "reviewed_source_shas": []string{}, "allow_publication": false,
		"plans": contract.Object{"quarter": contract.Object{
			"command": "quarter-backlog", "domain_profile": "quarter", "allowed_operation_kinds": []string{"issue-update"}, "attempt_target": "plan_set",
			"approval": contract.Object{"kind": "reviewed-dispatch", "approval_input": "approved_plan_sha", "reviewed_run_input": "reviewed_plan_run_id", "required_inputs": contract.Object{"scope": true}},
			"event":    contract.Object{"kind": "workflow_dispatch", "path": "events/dispatch-event.json"}, "parent_merge": nil,
		}},
	}}}
	writeCanonicalFile(t, filepath.Join(root, "policy.json"), policy)
	temp := t.TempDir()
	pkg := filepath.Join(temp, "invocation")
	if err := os.Mkdir(pkg, 0700); err != nil {
		t.Fatal(err)
	}
	identities := []any{contract.Object{"name": "quarter", "command": plan.Command, "sha256": plan.SHA256}}
	attemptTarget := contract.Object{"plans": identities}
	planSetBytes, err := contract.Canonical(attemptTarget)
	if err != nil {
		t.Fatal(err)
	}
	contextInput := contract.Object{
		"workflow_file": "task.yml", "repository": repository.FullName(), "recovery_key": "plan-set-" + digestBytes(planSetBytes),
		"run_name": "Quarter backlog", "run_id": int64(100), "attempt": int64(1), "attempt_target": attemptTarget,
		"dispatch_steps": []string{"Apply reviewed plan"},
	}
	var out, stderr bytes.Buffer
	inputBytes, err := contract.Canonical(contextInput)
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Out: &out, Err: &stderr, Input: bytes.NewReader(inputBytes), Actions: noRecoveryProviderReads{t}}
	if err := runner.Run(context.Background(), []string{"runs", "context-start", "--repo-root", root, "--policy", "policy.json", "--runner-temp", temp, "--package-root", pkg, "--input", "context=-"}); err != nil {
		t.Fatal("could not initialize provider-free plan-set context:", err, stderr.String())
	}
	planPath := filepath.Join(root, "quarter-plan.json")
	writeCanonicalFile(t, planPath, plan.Object())
	out.Reset()
	stderr.Reset()
	runner.Input = strings.NewReader("")
	if err := runner.Run(context.Background(), []string{
		"runs", "context-record-plan", "--repo-root", root, "--policy", "policy.json", "--runner-temp", temp, "--package-root", pkg,
		"--input", "native-plan=" + planPath, "--name", "quarter", "--relative", "plans/quarter.json", "--plan-command", "quarter-backlog",
	}); err != nil {
		t.Fatal("direct native registration without a review pair failed:", err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(pkg, "plans/quarter.json")); err != nil {
		t.Fatal("native plan was not persisted:", err)
	}
}

func TestContextRecordPlanRetainsLegacyWrapperMode(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	fixture := newExecutionContextFixture(t, testContextRepository("example", "widgets"), "execution-sync", executionPlanData(17))
	wrapper := contract.Object{
		"relative": "plans/execution.json", "name": "execution", "command": fixture.plan.Command,
		"repository": fixture.repository.FullName(), "plan": fixture.plan.Object(),
		"review_path": "reviews/execution.json", "review_sha256": fixture.reviewSHA256,
	}
	wrapperPath := filepath.Join(fixture.root, "plan-wrapper.json")
	writeCanonicalFile(t, wrapperPath, wrapper)
	if result := runContextRecordPlan(t, fixture, []string{"--input", "plan=" + wrapperPath}); result == nil {
		t.Fatal("legacy plan wrapper was not accepted")
	}
	if _, err := os.Stat(filepath.Join(fixture.packageRoot, "plans/execution.json")); err != nil {
		t.Fatal("legacy wrapper did not persist the plan:", err)
	}
}

func TestContextRecordPlanRejectsAmbiguousDirectInputsWithoutChangingContext(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	repository := testContextRepository("example", "widgets")
	tests := []struct {
		name      string
		planRepo  contract.Repository
		command   string
		arguments func(planPath string, plan contract.Plan, reviewSHA string) []string
		tamper    bool
	}{
		{name: "wrong repository", planRepo: testContextRepository("elsewhere", "widgets"), command: "execution-sync", arguments: directContextPlanArguments},
		{name: "wrong expected command", planRepo: repository, command: "different-command", arguments: func(path string, _ contract.Plan, review string) []string {
			return directContextPlanArguments(path, contract.Plan{}, review)
		}},
		{name: "tampered digest", planRepo: repository, command: "execution-sync", arguments: directContextPlanArguments, tamper: true},
		{name: "mixed direct and wrapper inputs", planRepo: repository, command: "execution-sync", arguments: func(path string, _ contract.Plan, review string) []string {
			args := directContextPlanArguments(path, contract.Plan{}, review)
			return append(args, "--input", "plan="+path)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newExecutionContextFixture(t, tc.planRepo, tc.command, executionPlanData(17))
			planPath := filepath.Join(fixture.root, "candidate-plan.json")
			planBytes, err := contract.Canonical(fixture.plan.Object())
			if err != nil {
				t.Fatal(err)
			}
			if tc.tamper {
				planBytes = bytes.Replace(planBytes, []byte(`"issue_number":17`), []byte(`"issue_number":18`), 1)
				if bytes.Equal(planBytes, mustCanonicalForTest(t, fixture.plan.Object())) {
					t.Fatal("tamper fixture did not change the plan bytes")
				}
			}
			if err := os.WriteFile(planPath, planBytes, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(fixture.packageRoot, "run-context.json"))
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"runs", "context-record-plan", "--repo-root", fixture.root, "--policy", "policy.json", "--runner-temp", fixture.runnerTemp, "--package-root", fixture.packageRoot}
			args = append(args, tc.arguments(planPath, fixture.plan, fixture.reviewSHA256)...)
			var out, stderr bytes.Buffer
			err = (Runner{Out: &out, Err: &stderr, Actions: noRecoveryProviderReads{t}}).Run(context.Background(), args)
			if err == nil || out.Len() != 0 {
				t.Fatalf("invalid direct plan was accepted: err=%v stdout=%q stderr=%q", err, out.String(), stderr.String())
			}
			after, err := os.ReadFile(filepath.Join(fixture.packageRoot, "run-context.json"))
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("rejected plan changed the run context: err=%v\nbefore=%s\nafter=%s", err, before, after)
			}
			if _, err := os.Lstat(filepath.Join(fixture.packageRoot, "plans/execution.json")); !os.IsNotExist(err) {
				t.Fatalf("rejected plan left execution evidence behind: %v", err)
			}
		})
	}
}

func TestContextRecordPlanRejectsMetadataOutsideDirectMode(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	root := checkout(t)
	for _, args := range [][]string{
		{"runs", "context-start", "--repo-root", root, "--relative", "plans/execution.json"},
		{"runs", "context-record-plan", "--repo-root", root, "--input", "plan=wrapper.json", "--name", "execution"},
		{"runs", "context-record-plan", "--repo-root", root, "--input", "native-plan=plan.json", "--name", "execution", "--relative", "plans/execution.json", "--plan-command", "execution-sync", "--review-path", "reviews/execution.json"},
		{"runs", "context-record-plan", "--repo-root", root, "--input", "native-plan=plan.json", "--name", "execution", "--relative", "plans/execution.json"},
		{"runs", "context-record-plan", "--repo-root", root, "--input", "native-plan=-", "--name", "execution", "--relative", "plans/execution.json", "--plan-command", "execution-sync"},
	} {
		var out, stderr bytes.Buffer
		err := (Runner{Out: &out, Err: &stderr}).Run(context.Background(), args)
		if err == nil || out.Len() != 0 {
			t.Fatalf("invalid context-plan mode was accepted: args=%v err=%v stdout=%q", args, err, out.String())
		}
	}
}

type contextRecordFixture struct {
	root, runnerTemp, packageRoot string
	repository                    contract.Repository
	plan                          contract.Plan
	reviewSHA256                  string
}

func newExecutionContextFixture(t *testing.T, planRepository contract.Repository, command string, data contract.Object) contextRecordFixture {
	t.Helper()
	root := checkout(t)
	repository := testContextRepository("example", "widgets")
	plan, err := contract.PreparePlan(command, planRepository,
		contract.Object{"github": contract.Object{"live": true, "complete": true}}, data,
		[]contract.Operation{{ID: "execution:link-comment", Kind: "issue-comment-upsert", Target: contract.Object{"issue_number": int64(17), "marker": "<!-- work -->"}, Before: contract.Object{"body": nil}, After: contract.Object{"body": "Reviewed work state"}}},
		time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	policy := contract.Object{"schema_version": int64(1), "workflows": contract.Object{"task.yml": contract.Object{
		"mutator_step_alternatives": [][]string{{"Apply execution"}}, "reviewed_source_shas": []string{}, "allow_publication": false,
		"plans": contract.Object{"execution": contract.Object{
			"command": "execution-sync", "domain_profile": "execution", "allowed_operation_kinds": []string{"issue-comment-upsert"}, "attempt_target": "execution",
			"approval": contract.Object{"kind": "git-slop-execution"}, "event": contract.Object{"kind": "execution_event", "path": "events/dispatch-event.json"}, "parent_merge": nil,
		}},
	}}}
	writeCanonicalFile(t, filepath.Join(root, "policy.json"), policy)
	temp := t.TempDir()
	pkg := filepath.Join(temp, "invocation")
	if err := os.Mkdir(pkg, 0700); err != nil {
		t.Fatal(err)
	}
	target := contract.Object{"kind": "issue", "number": int64(17)}
	event := contract.Object{"repository": contract.Object{"full_name": repository.FullName()}, "inputs": contract.Object{"issue_number": "17", "pr_number": ""}}
	eventBytes, err := contract.Canonical(event)
	if err != nil {
		t.Fatal(err)
	}
	writeContextPackageFile(t, pkg, "events/dispatch-event.json", eventBytes)
	contextInput := contract.Object{
		"workflow_file": "task.yml", "repository": repository.FullName(), "recovery_key": "execution-17", "run_name": "Execution",
		"run_id": int64(100), "attempt": int64(1), "attempt_target": contract.Object{"plan_sha256": plan.SHA256, "target": target, "event_sha256": digestBytes(eventBytes)},
		"dispatch_steps": []string{"Apply execution"},
	}
	contextBytes, err := contract.Canonical(contextInput)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	runner := Runner{Out: &out, Err: &stderr, Input: bytes.NewReader(contextBytes), Actions: noRecoveryProviderReads{t}}
	if err := runner.Run(context.Background(), []string{"runs", "context-start", "--repo-root", root, "--policy", "policy.json", "--runner-temp", temp, "--package-root", pkg, "--input", "context=-"}); err != nil {
		t.Fatal("could not initialize provider-free execution context:", err, stderr.String())
	}
	review := contract.Object{
		"schema_version": int64(1), "workflow_file": "task.yml", "repository": repository.FullName(), "workflow_run_id": int64(100), "workflow_run_attempt": int64(1),
		"name": "execution", "command": plan.Command, "plan_sha256": plan.SHA256, "target": target,
		"event_path": "events/dispatch-event.json", "event_sha256": digestBytes(eventBytes),
	}
	reviewBytes, err := contract.Canonical(review)
	if err != nil {
		t.Fatal(err)
	}
	writeContextPackageFile(t, pkg, "reviews/execution.json", reviewBytes)
	return contextRecordFixture{root: root, runnerTemp: temp, packageRoot: pkg, repository: repository, plan: plan, reviewSHA256: digestBytes(reviewBytes)}
}

func runContextRecordPlan(t *testing.T, fixture contextRecordFixture, extra []string) contract.Object {
	t.Helper()
	planPath := filepath.Join(fixture.root, "native-plan.json")
	if _, err := os.Stat(planPath); os.IsNotExist(err) {
		writeCanonicalFile(t, planPath, fixture.plan.Object())
	}
	args := []string{"runs", "context-record-plan", "--repo-root", fixture.root, "--policy", "policy.json", "--runner-temp", fixture.runnerTemp, "--package-root", fixture.packageRoot}
	args = append(args, extra...)
	var out, stderr bytes.Buffer
	err := (Runner{Out: &out, Err: &stderr, Actions: noRecoveryProviderReads{t}}).Run(context.Background(), args)
	if err != nil {
		t.Fatalf("context plan registration failed: %v\nstderr: %s", err, stderr.String())
	}
	result, err := contract.Decode(&out)
	if err != nil {
		t.Fatal("context plan registration returned invalid output:", err)
	}
	return result
}

func directContextPlanArguments(path string, _ contract.Plan, reviewSHA string) []string {
	return []string{"--input", "native-plan=" + path, "--name", "execution", "--relative", "plans/execution.json", "--plan-command", "execution-sync", "--review-path", "reviews/execution.json", "--review-sha256", reviewSHA}
}

func executionPlanData(number int64) contract.Object {
	selector := contract.Object{"issue_number": number, "pull_request_number": int64(0), "skip_project_sync": true, "project": nil}
	return contract.Object{"selector": selector}
}

func testContextRepository(owner, name string) contract.Repository {
	repository, _ := contract.ParseRepository(contract.Object{"nameWithOwner": owner + "/" + name, "url": "https://github.com/" + owner + "/" + name})
	return repository
}

func writeCanonicalFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := contract.Canonical(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func writeContextPackageFile(t *testing.T, root, relative string, data []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func mustCanonicalForTest(t *testing.T, value any) []byte {
	t.Helper()
	data, err := contract.Canonical(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
