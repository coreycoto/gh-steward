package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func TestPlanExtractPreservesCanonicalNumbersAndUnicodeWithoutCheckout(t *testing.T) {
	root := t.TempDir() // Deliberately not a Git checkout: extraction is offline.
	repository, plan := planExtractionFixture(t, "example", "widgets", "execution-sync")
	plan.Data["numbers"] = []any{
		json.Number("1.2300"),
		json.Number("1e-7"),
		json.Number("-0"),
		json.Number("900719925474099312345678901234567890"),
	}
	plan.Data["unicode"] = "snowman ☃, nonbreaking space, <literal>"
	plan, err := contract.PreparePlan(plan.Command, repository, plan.Sources, plan.Data, nil, time.Date(2026, 10, 4, 12, 30, 0, 123, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	envelope := planExtractionEnvelope(plan, "execution-sync-prepare")
	input, err := contract.Canonical(envelope)
	if err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(root, "prepare.json")
	if err := os.WriteFile(inputPath, input, 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(root, "scratch", "reviewed-plan.json")
	var stdout, stderr bytes.Buffer
	err = (Runner{Out: &stdout, Err: &stderr}).Run(context.Background(), []string{
		"plan", "extract", "--repo-root", root, "--repo", repository.URL,
		"--input", "envelope=prepare.json", "--outer-command", "execution-sync-prepare",
		"--plan-command", "execution-sync", "--out", "scratch/reviewed-plan.json",
	})
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := contract.Canonical(plan.Object())
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if !bytes.Equal(got, want) {
		t.Fatalf("extracted plan changed its canonical bytes\n got: %s\nwant: %s", got, want)
	}
	for _, lexeme := range []string{"1.2300", "1e-7", "-0", "900719925474099312345678901234567890", "snowman ☃", "<literal>"} {
		if !bytes.Contains(got, []byte(lexeme)) {
			t.Errorf("extracted plan lost %q: %s", lexeme, got)
		}
	}
	result, err := contract.Decode(&stdout)
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	wantResultPath := filepath.Join(resolvedRoot, "scratch", "reviewed-plan.json")
	repo, repoErr := contract.ObjectAt(result, "repository")
	if repoErr != nil || result["command"] != "plan-extract" || repo["nameWithOwner"] != repository.FullName() {
		t.Fatalf("unexpected extraction result envelope: %#v", result)
	}
	data, err := contract.ObjectAt(result, "data")
	if err != nil || data["plan_sha256"] != plan.SHA256 || data["plan_command"] != plan.Command || data["path"] != wantResultPath {
		t.Fatalf("unexpected extraction result data: %#v (%v)", data, err)
	}
}

func TestPlanExtractReportsResolvedPathForSymlinkRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	rootAlias := filepath.Join(parent, "root-alias")
	if err := os.Symlink(root, rootAlias); err != nil {
		t.Fatal(err)
	}
	repository, plan := planExtractionFixture(t, "example", "widgets", "execution-sync")
	input, err := contract.Canonical(planExtractionEnvelope(plan, "execution-sync-prepare"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "prepare.json"), input, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = (Runner{Out: &stdout, Err: &stderr}).Run(context.Background(), []string{
		"plan", "extract", "--repo-root", rootAlias, "--repo", repository.URL,
		"--input", "envelope=prepare.json", "--outer-command", "execution-sync-prepare",
		"--plan-command", "execution-sync", "--out", "reviewed-plan.json",
	})
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
	resolvedRoot, err := filepath.EvalSymlinks(rootAlias)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(resolvedRoot, "reviewed-plan.json")
	if data["path"] != wantPath {
		t.Fatalf("reported path should identify the resolved root: got %#v, want %q", data["path"], wantPath)
	}
	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	want := append(mustCanonical(t, plan.Object()), '\n')
	if !bytes.Equal(got, want) {
		t.Fatalf("symlink-root extraction changed its canonical bytes\n got: %s\nwant: %s", got, want)
	}
}

func TestPlanExtractReadsOneNamedStdinEnvelope(t *testing.T) {
	root := t.TempDir()
	repository, plan := planExtractionFixture(t, "example", "widgets", "quarter-backlog")
	input, err := contract.Canonical(planExtractionEnvelope(plan, "quarter-backlog-prepare"))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = (Runner{Out: &stdout, Err: &stderr, Input: bytes.NewReader(input)}).Run(context.Background(), []string{
		"plan", "extract", "--repo-root", root, "--repo", repository.URL,
		"--input", "envelope=-", "--outer-command", "quarter-backlog-prepare",
		"--plan-command", "quarter-backlog", "--out", "extracted.json",
	})
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	result, err := contract.Decode(bytes.NewReader(mustReadFile(t, filepath.Join(root, "extracted.json"))))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := contract.ParsePlan(result)
	if err != nil || parsed.Command != plan.Command || parsed.SHA256 != plan.SHA256 {
		t.Fatalf("stdin extraction did not preserve the reviewed plan: %#v (%v)", parsed, err)
	}
}

func TestPlanExtractRejectsInvalidEnvelopesAndPreservesOutput(t *testing.T) {
	root := t.TempDir()
	repository, plan := planExtractionFixture(t, "example", "widgets", "execution-sync")
	otherRepository, otherPlan := planExtractionFixture(t, "another", "widgets", "execution-sync")
	planRepositoryMismatch := planExtractionEnvelope(otherPlan, "execution-sync-prepare")
	planRepositoryMismatch["repository"] = repository.Object()
	baseEnvelope := planExtractionEnvelope(plan, "execution-sync-prepare")
	planWithDifferentCommand := planExtractionEnvelope(mustPreparePlan(t, repository, "other-plan-command"), "execution-sync-prepare")
	tests := []struct {
		name      string
		input     []byte
		repo      string
		outer     string
		plan      string
		inputArgs []string
	}{
		{name: "extra envelope field", input: mustCanonical(t, withField(t, baseEnvelope, "extra", true))},
		{name: "missing envelope field", input: mustCanonical(t, removeField(t, baseEnvelope, "tool_version"))},
		{name: "wrong schema version", input: mustCanonical(t, withField(t, baseEnvelope, "schema_version", int64(1)))},
		{name: "malformed repository identity", input: mustCanonical(t, withField(t, baseEnvelope, "repository", contract.Object{"host": "github.com", "owner": "example", "name": "widgets", "url": repository.URL, "nameWithOwner": "example/widgets", "extra": "value"}))},
		{name: "envelope repository mismatch", input: mustCanonical(t, withField(t, baseEnvelope, "repository", otherRepository.Object()))},
		{name: "outer command mismatch", input: mustCanonical(t, baseEnvelope), outer: "merge-prepare"},
		{name: "inner command mismatch", input: mustCanonical(t, planWithDifferentCommand)},
		{name: "plan repository mismatch", input: mustCanonical(t, planRepositoryMismatch)},
		{name: "explicit repository mismatch", input: mustCanonical(t, baseEnvelope), repo: otherRepository.URL},
		{name: "unsupported plan field", input: mustCanonical(t, withPlanField(t, baseEnvelope, "extra", true))},
		{name: "incomplete source evidence", input: mustCanonical(t, withPlanSource(t, baseEnvelope, "complete", false))},
		{name: "invalid capture date", input: mustCanonical(t, withPlanField(t, baseEnvelope, "captured_at", "2026-10-04T12:00:00"))},
		{name: "stale plan hash", input: mustCanonical(t, withPlanField(t, baseEnvelope, "sha256", strings.Repeat("b", 64)))},
		{name: "tampered signed data", input: mustCanonical(t, tamperPlanData(t, baseEnvelope))},
		{name: "duplicate json key", input: []byte(`{"schema_version":2,"schema_version":2}`)},
		{name: "multiple json documents", input: append(mustCanonical(t, baseEnvelope), []byte(` {}`)...)},
		{name: "wrong named input", input: mustCanonical(t, baseEnvelope), inputArgs: []string{"--input", "plan=prepare.json"}},
		{name: "duplicate named inputs", input: mustCanonical(t, baseEnvelope), inputArgs: []string{"--input", "envelope=prepare.json", "--input", "envelope=prepare.json"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inputPath := filepath.Join(root, "prepare.json")
			if err := os.WriteFile(inputPath, tc.input, 0600); err != nil {
				t.Fatal(err)
			}
			outPath := filepath.Join(root, "existing-plan.json")
			before := []byte("preserve exact output bytes\n")
			if err := os.WriteFile(outPath, before, 0600); err != nil {
				t.Fatal(err)
			}
			outer := tc.outer
			if outer == "" {
				outer = "execution-sync-prepare"
			}
			inputArgs := tc.inputArgs
			if inputArgs == nil {
				inputArgs = []string{"--input", "envelope=prepare.json"}
			}
			repoURL := tc.repo
			if repoURL == "" {
				repoURL = repository.URL
			}
			planCommand := tc.plan
			if planCommand == "" {
				planCommand = "execution-sync"
			}
			args := []string{"plan", "extract", "--repo-root", root, "--repo", repoURL}
			args = append(args, inputArgs...)
			args = append(args, "--outer-command", outer, "--plan-command", planCommand, "--out", outPath)
			var stdout, stderr bytes.Buffer
			err := (Runner{Out: &stdout, Err: &stderr}).Run(context.Background(), args)
			if err == nil || stdout.Len() != 0 {
				t.Fatalf("invalid extraction succeeded: err=%v stdout=%q", err, stdout.String())
			}
			got, readErr := os.ReadFile(outPath)
			if readErr != nil || !bytes.Equal(got, before) {
				t.Fatalf("rejected input changed existing output: bytes=%q err=%v", got, readErr)
			}
		})
	}
}

func TestPlanValidatePreservesSignedJSONAndNeverWritesFiles(t *testing.T) {
	root := t.TempDir() // Deliberately not a Git checkout or provider workspace.
	repository, plan := planExtractionFixture(t, "example", "widgets", "execution-sync")
	plan.Data["numbers"] = []any{json.Number("1e-7"), json.Number("-0"), json.Number("123456789012345678901234567890")}
	plan, err := contract.PreparePlan(plan.Command, repository, plan.Sources, plan.Data, nil, time.Date(2026, 10, 4, 12, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	planBytes := mustCanonical(t, plan.Object())
	planPath := filepath.Join(root, "standalone-plan.json")
	contextPath := filepath.Join(root, "context.json")
	contextBefore := []byte("context remains unchanged\n")
	if err := os.WriteFile(planPath, planBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contextPath, contextBefore, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = (Runner{Out: &stdout, Err: &stderr}).Run(context.Background(), []string{
		"plan", "validate", "--repo-root", root, "--repo", repository.URL,
		"--input", "plan=standalone-plan.json", "--plan-command", "execution-sync",
	})
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	if got := mustReadFile(t, planPath); !bytes.Equal(got, planBytes) {
		t.Fatalf("plan validate rewrote its input: got %s, want %s", got, planBytes)
	}
	if got := mustReadFile(t, contextPath); !bytes.Equal(got, contextBefore) {
		t.Fatalf("plan validate changed context: got %s", got)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("validation created an unexpected file: entries=%v err=%v", entries, err)
	}
	result, err := contract.Decode(&stdout)
	if err != nil {
		t.Fatal(err)
	}
	data, err := contract.ObjectAt(result, "data")
	if err != nil || result["command"] != "plan-validate" || data["plan_sha256"] != plan.SHA256 || data["plan_command"] != plan.Command || data["operation_count"] != json.Number("0") {
		t.Fatalf("unexpected plan validation result: %#v (%v)", result, err)
	}
}

func TestPlanValidateRejectsInvalidPlansAndPreservesAllFiles(t *testing.T) {
	root := t.TempDir()
	repository, plan := planExtractionFixture(t, "example", "widgets", "execution-sync")
	_, otherPlan := planExtractionFixture(t, "another", "widgets", "execution-sync")
	tests := []struct {
		name      string
		input     []byte
		repo      string
		command   string
		inputArgs []string
	}{
		{name: "unsupported field", input: mustCanonical(t, withField(t, plan.Object(), "extra", true))},
		{name: "wrong schema", input: mustCanonical(t, withPlanField(t, plan.Object(), "schema_version", int64(1)))},
		{name: "incomplete source", input: mustCanonical(t, withPlanSource(t, plan.Object(), "complete", false))},
		{name: "invalid capture time", input: mustCanonical(t, withPlanField(t, plan.Object(), "captured_at", "2026-10-04T12:00:00"))},
		{name: "stale digest", input: mustCanonical(t, withPlanField(t, plan.Object(), "sha256", strings.Repeat("b", 64)))},
		{name: "tampered signed data", input: mustCanonical(t, tamperPlanObject(t, plan.Object()))},
		{name: "invalid explicit repository URL", input: mustCanonical(t, plan.Object()), repo: "http://github.com/example/widgets"},
		{name: "expected command mismatch", input: mustCanonical(t, plan.Object()), command: "other-command"},
		{name: "valid plan from another repository", input: mustCanonical(t, otherPlan.Object())},
		{name: "duplicate json key", input: []byte(`{"schema_version":2,"schema_version":2}`)},
		{name: "multiple json documents", input: append(mustCanonical(t, plan.Object()), []byte(` {}`)...)},
		{name: "wrong named input", input: mustCanonical(t, plan.Object()), inputArgs: []string{"--input", "envelope=plan.json"}},
		{name: "mixed named inputs", input: mustCanonical(t, plan.Object()), inputArgs: []string{"--input", "plan=plan.json", "--input", "payload=extra.json"}},
		{name: "unexpected output flag", input: mustCanonical(t, plan.Object()), inputArgs: []string{"--input", "plan=plan.json", "--out", "unexpected.json"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			planPath := filepath.Join(root, "plan.json")
			contextPath := filepath.Join(root, "context.json")
			contextBefore := []byte("preserve context bytes\n")
			if err := os.WriteFile(planPath, tc.input, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(contextPath, contextBefore, 0600); err != nil {
				t.Fatal(err)
			}
			planCommand := tc.command
			if planCommand == "" {
				planCommand = "execution-sync"
			}
			inputArgs := tc.inputArgs
			if inputArgs == nil {
				inputArgs = []string{"--input", "plan=plan.json"}
			}
			repoURL := tc.repo
			if repoURL == "" {
				repoURL = repository.URL
			}
			args := []string{"plan", "validate", "--repo-root", root, "--repo", repoURL}
			args = append(args, inputArgs...)
			args = append(args, "--plan-command", planCommand)
			var stdout, stderr bytes.Buffer
			err := (Runner{Out: &stdout, Err: &stderr}).Run(context.Background(), args)
			if err == nil || stdout.Len() != 0 {
				t.Fatalf("invalid plan succeeded: err=%v stdout=%q", err, stdout.String())
			}
			if got := mustReadFile(t, planPath); !bytes.Equal(got, tc.input) {
				t.Fatalf("rejected plan input changed: got %s, want %s", got, tc.input)
			}
			if got := mustReadFile(t, contextPath); !bytes.Equal(got, contextBefore) {
				t.Fatalf("rejected plan changed context: got %s", got)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 2 {
				t.Fatalf("rejected plan created an unexpected file: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestPlanCommandsAreDocumentedInCLIHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := (Runner{Out: &stdout, Err: &stderr}).Run(context.Background(), []string{"help"}); err != nil {
		t.Fatal(err, stderr.String())
	}
	for _, command := range []string{
		"gh steward plan extract --repo-root PATH --repo HTTPS_URL --input envelope=FILE --outer-command COMMAND --plan-command COMMAND --out FILE",
		"gh steward plan validate --repo-root PATH --repo HTTPS_URL --input plan=FILE --plan-command COMMAND",
	} {
		if !strings.Contains(stdout.String(), command) {
			t.Errorf("CLI help omits %q", command)
		}
	}
}

func planExtractionFixture(t *testing.T, owner, name, command string) (contract.Repository, contract.Plan) {
	t.Helper()
	repository, err := contract.ParseRepository(contract.Object{
		"nameWithOwner": owner + "/" + name,
		"url":           "https://github.com/" + owner + "/" + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contract.PreparePlan(command, repository, contract.Object{
		"source": contract.Object{"live": true, "complete": true, "sha256": strings.Repeat("a", 64)},
	}, contract.Object{"intent": "reviewed fixture"}, nil, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return repository, plan
}

func planExtractionEnvelope(plan contract.Plan, command string) contract.Object {
	return contract.Object{
		"schema_version": int64(2),
		"tool_version":   "0.2.1-dev",
		"command":        command,
		"repository":     plan.Repository.Object(),
		"data":           plan.Object(),
	}
}

func mustPreparePlan(t *testing.T, repository contract.Repository, command string) contract.Plan {
	t.Helper()
	plan, err := contract.PreparePlan(command, repository, contract.Object{
		"source": contract.Object{"live": true, "complete": true},
	}, contract.Object{"intent": "reviewed fixture"}, nil, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func mustCanonical(t *testing.T, value contract.Object) []byte {
	t.Helper()
	encoded, err := contract.Canonical(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func withField(t *testing.T, value contract.Object, key string, field any) contract.Object {
	t.Helper()
	copy, err := contract.Clone(value)
	if err != nil {
		t.Fatal(err)
	}
	copy[key] = field
	return copy
}

func removeField(t *testing.T, value contract.Object, key string) contract.Object {
	t.Helper()
	copy, err := contract.Clone(value)
	if err != nil {
		t.Fatal(err)
	}
	delete(copy, key)
	return copy
}

func tamperPlanData(t *testing.T, value contract.Object) contract.Object {
	t.Helper()
	copy, err := contract.Clone(value)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contract.ObjectAt(copy, "data")
	if err != nil {
		t.Fatal(err)
	}
	planData, err := contract.ObjectAt(plan, "data")
	if err != nil {
		t.Fatal(err)
	}
	planData["intent"] = "tampered"
	return copy
}

func tamperPlanObject(t *testing.T, value contract.Object) contract.Object {
	t.Helper()
	copy, err := contract.Clone(value)
	if err != nil {
		t.Fatal(err)
	}
	data, err := contract.ObjectAt(copy, "data")
	if err != nil {
		t.Fatal(err)
	}
	data["intent"] = "tampered"
	return copy
}

func withPlanField(t *testing.T, value contract.Object, key string, field any) contract.Object {
	t.Helper()
	copy, err := contract.Clone(value)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := contract.ObjectAt(copy, "data")
	if err != nil {
		t.Fatal(err)
	}
	plan[key] = field
	return copy
}

func withPlanSource(t *testing.T, value contract.Object, key string, field any) contract.Object {
	t.Helper()
	copy, err := contract.Clone(value)
	if err != nil {
		t.Fatal(err)
	}
	plan := copy
	if _, exists := plan["sources"]; !exists {
		plan, err = contract.ObjectAt(copy, "data")
		if err != nil {
			t.Fatal(err)
		}
	}
	sources, err := contract.ObjectAt(plan, "sources")
	if err != nil {
		t.Fatal(err)
	}
	source, err := contract.ObjectAt(sources, "source")
	if err != nil {
		t.Fatal(err)
	}
	source[key] = field
	return copy
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
