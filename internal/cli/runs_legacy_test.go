package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/runrecovery"
)

const legacyCLIDomain = "gh-steward-legacy-evidence-v1\n"

type legacyCLIArchive struct {
	evidence contract.Object
	review   contract.Object
	payload  contract.Object
	private  ed25519.PrivateKey
}

func newLegacyCLIArchive(t *testing.T) *legacyCLIArchive {
	t.Helper()
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	target := contract.Object{
		"workflow_file": "task.yml", "repository": "example/widgets", "server_url": "https://github.com",
		"workflow_id": int64(9), "recovery_key": "workflow-history-v2",
	}
	run := contract.Object{
		"id": int64(100), "created_at": "2026-09-01T12:00:00Z", "display_title": "Apply reviewed task",
		"event": "workflow_dispatch", "workflow_id": int64(9), "head_branch": "main",
		"head_sha": strings.Repeat("a", 40), "run_attempt": int64(2), "status": "completed", "conclusion": "success",
	}
	attemptTarget := contract.Object{"recovery_key": "reviewed-task", "identity": contract.Object{"plan_sha256": strings.Repeat("b", 64)}}
	workflowSHA := strings.Repeat("c", 40)
	workflowBytes := []byte("name: Task\non:\n  workflow_dispatch:\njobs:\n  apply:\n    steps:\n      - name: Apply operation\n        run: gh steward apply\n  validate:\n    steps:\n      - run: echo valid\n")
	workflowProof := runrecovery.MakeFileProof(workflowBytes)
	sources := []any{contract.Object{"path": ".github/workflows/task.yml", "commit": workflowSHA, "file": workflowProof}}
	eventBytes, err := runrecovery.Canonical(contract.Object{
		"repository": contract.Object{"full_name": "example/widgets", "html_url": "https://github.com/example/widgets"},
		"inputs":     contract.Object{"change": "apply", "number": "17"},
	})
	if err != nil {
		t.Fatal(err)
	}
	archiveBytes := legacyCLIArchiveZIP(t)
	artifactSHA := "sha256:" + runrecovery.SHA256(archiveBytes)
	metadata := contract.Object{
		"id": int64(300), "name": "legacy-attempt-100-2", "digest": artifactSHA, "expired": false,
		"workflow_run": contract.Object{"id": int64(100), "run_id": int64(100), "run_attempt": int64(2), "head_sha": run["head_sha"]},
	}
	artifact := contract.Object{"metadata": metadata, "archive": runrecovery.MakeFileProof(archiveBytes), "origin": "actions_artifact"}
	mutatorJob := contract.Object{
		"id": int64(401), "run_id": int64(100), "run_attempt": int64(2), "name": "Apply",
		"status": "completed", "conclusion": "success",
		"steps": []any{contract.Object{"name": "Apply operation", "status": "completed", "conclusion": "skipped"}},
	}
	validationJob := contract.Object{
		"id": int64(402), "run_id": int64(100), "run_attempt": int64(2), "name": "Validate",
		"status": "completed", "conclusion": "success", "steps": []any{},
	}
	payload := contract.Object{
		"schema_version": int64(1), "target": target, "run": run, "attempt": int64(2), "attempt_target": attemptTarget,
		"run_packet": run, "jobs_packet": contract.Object{"run_id": int64(100), "attempt": int64(2), "pages": []any{contract.Object{"total_count": int64(2), "jobs": []any{mutatorJob, validationJob}}}},
		"workflow_sha": workflowSHA, "event_file": runrecovery.MakeFileProof(eventBytes),
		"inputs": contract.Object{"change": "apply", "number": "17"}, "sources": sources,
		"intended_effects": []any{contract.Object{"kind": "issue-comment", "issue": int64(17), "body": "Reviewed update"}},
		"receipts":         []any{}, "observations": []any{}, "artifacts": []any{artifact},
	}
	review := contract.Object{
		"schema_version": int64(1), "target": target, "run_id": int64(100), "attempt": int64(2),
		"evidence_sha256": strings.Repeat("0", 64), "archive_public_key": base64.StdEncoding.EncodeToString(private.Public().(ed25519.PublicKey)),
		"workflow_sha":     workflowSHA,
		"sources":          []any{contract.Object{"path": ".github/workflows/task.yml", "sha256": workflowProof["sha256"]}},
		"mutators":         []any{contract.Object{"job": "Apply", "steps": []any{"Apply operation"}}},
		"non_mutator_jobs": []any{"Validate"},
		"artifacts":        []any{contract.Object{"name": metadata["name"], "id": metadata["id"], "digest": artifactSHA}},
	}
	fixture := &legacyCLIArchive{review: review, payload: payload, private: private}
	fixture.reseal(t)
	return fixture
}

func (f *legacyCLIArchive) reseal(t *testing.T) {
	t.Helper()
	payload, err := runrecovery.Canonical(f.payload)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(f.private, append([]byte(legacyCLIDomain), payload...))
	f.evidence = contract.Object{"schema_version": int64(1), "payload": f.payload, "signature": base64.StdEncoding.EncodeToString(signature)}
	evidence, err := runrecovery.Canonical(f.evidence)
	if err != nil {
		t.Fatal(err)
	}
	f.review["evidence_sha256"] = runrecovery.SHA256(evidence)
}

func legacyCLIArchiveZIP(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	file, err := writer.Create("archive.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(`{"retained":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func writeLegacyCLIJSON(t *testing.T, root, name string, value any) string {
	t.Helper()
	data, err := runrecovery.Canonical(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func legacyCLIInput(fixture *legacyCLIArchive) contract.Object {
	return contract.Object{"evidence": fixture.evidence, "review": fixture.review}
}

func pathWithGitOnly(t *testing.T) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if _, err := exec.LookPath("gh"); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("provider executable unexpectedly available in isolated PATH: %v", err)
	}
}

type forbiddenLegacyCLIProvider struct{ t *testing.T }

func (p forbiddenLegacyCLIProvider) Read(context.Context, string) (contract.Object, error) {
	p.t.Fatal("legacy classification made a provider read")
	return nil, nil
}
func (p forbiddenLegacyCLIProvider) Pages(context.Context, string) ([]any, error) {
	p.t.Fatal("legacy classification paginated provider state")
	return nil, nil
}
func (p forbiddenLegacyCLIProvider) Archive(context.Context, int64) ([]byte, error) {
	p.t.Fatal("legacy classification fetched a provider artifact")
	return nil, nil
}

func TestLegacyReviewVerifiesExactSignedPacketOffline(t *testing.T) {
	root := checkout(t)
	fixture := newLegacyCLIArchive(t)
	writeLegacyCLIJSON(t, root, "evidence.json", legacyCLIInput(fixture))
	pathWithGitOnly(t)
	var stdout, stderr bytes.Buffer
	err := (Runner{Out: &stdout, Err: &stderr, Actions: forbiddenLegacyCLIProvider{t}}).Run(context.Background(), []string{
		"runs", "legacy-review", "--repo-root", root, "--repo", "https://github.com/example/widgets", "--input", "evidence=evidence.json",
	})
	if err != nil {
		t.Fatal(err, stderr.String())
	}
	result, err := contract.Decode(&stdout)
	if err != nil {
		t.Fatal(err)
	}
	data, err := contract.ObjectAt(result, "data")
	schemaVersion, schemaErr := contract.Integer(result["schema_version"])
	if err != nil || schemaErr != nil || result["command"] != "workflow-legacy-review" || schemaVersion != 2 {
		t.Fatalf("legacy review returned an unexpected machine envelope: result=%#v err=%v", result, err)
	}
	if data["classification"] != "no_dispatch_proven" || data["intent_proven"] != true || !runrecovery.IsSHA256(data["report_sha256"]) {
		t.Fatalf("complete exact-attempt signed proof was not classified deterministically: %#v", data)
	}
	for _, forbidden := range []string{"\"evidence\"", "\"review\"", "\"signature\"", "\"sources\"", "\"inputs\"", "\"event_file\"", "\"artifacts\"", "\"receipts\"", "\"observations\"", "\"base64\""} {
		if strings.Contains(stdout.String(), forbidden) {
			t.Fatalf("legacy review summary leaked full evidence field %s: %s", forbidden, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), fixture.evidence["signature"].(string)) {
		t.Fatal("legacy review summary leaked the signed archive signature")
	}
}

func TestLegacyReviewOutKeepsFullEnvelopePrivateAndIdempotent(t *testing.T) {
	root := checkout(t)
	fixture := newLegacyCLIArchive(t)
	writeLegacyCLIJSON(t, root, "evidence.json", legacyCLIInput(fixture))
	pathWithGitOnly(t)
	outPath := "private/legacy-report-envelope.json"
	args := []string{"runs", "legacy-review", "--repo-root", root, "--repo", "https://github.com/example/widgets", "--input", "evidence=evidence.json", "--out", outPath}
	run := func() contract.Object {
		var stdout, stderr bytes.Buffer
		if err := (Runner{Out: &stdout, Err: &stderr}).Run(context.Background(), args); err != nil {
			t.Fatal(err, stderr.String())
		}
		result, err := contract.Decode(&stdout)
		if err != nil {
			t.Fatal(err)
		}
		data, err := contract.ObjectAt(result, "data")
		if err != nil || data["full_result_path"] != outPath || data["classification"] != "no_dispatch_proven" {
			t.Fatalf("unexpected bounded stdout summary: %#v err=%v", result, err)
		}
		for _, forbidden := range []string{"\"evidence\"", "\"review\"", "\"signature\"", "\"sources\"", "\"inputs\"", "\"event_file\"", "\"artifacts\"", "\"receipts\"", "\"observations\"", "\"base64\""} {
			if strings.Contains(stdout.String(), forbidden) {
				t.Fatalf("stdout summary leaked full evidence field %s: %s", forbidden, stdout.String())
			}
		}
		if strings.Contains(stdout.String(), fixture.evidence["signature"].(string)) {
			t.Fatal("stdout summary leaked the signed archive signature")
		}
		return data
	}
	first := run()
	output := filepath.Join(root, outPath)
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("full legacy result is not a private mode-0600 file: info=%v err=%v", info, err)
	}
	parentInfo, err := os.Stat(filepath.Dir(output))
	if err != nil || parentInfo.Mode().Perm() != 0700 {
		t.Fatalf("legacy result parent was not created mode 0700: info=%v err=%v", parentInfo, err)
	}
	full, err := contract.Decode(bytes.NewReader(mustRead(t, output)))
	if err != nil {
		t.Fatal(err)
	}
	fullData, err := contract.ObjectAt(full, "data")
	if err != nil {
		t.Fatal(err)
	}
	verified, err := runrecovery.ValidateLegacyReport(fullData)
	if err != nil || verified["classification"] != "no_dispatch_proven" {
		t.Fatalf("private full result did not preserve the validated raw report: err=%v", err)
	}
	contents := mustRead(t, output)
	second := run()
	if first["report_sha256"] != second["report_sha256"] || !bytes.Equal(contents, mustRead(t, output)) {
		t.Fatal("byte-identical result retry replaced or changed the existing private output")
	}
}

func TestLegacyRecoverySummariesExcludeSensitiveFieldsForEveryAction(t *testing.T) {
	const sentinel = "PRIVATE_LEGACY_PACKET_SENTINEL"
	output := &legacyResultOutput{path: "private/result.json"}
	repository := contract.Repository{Host: "github.com", Owner: "example", Name: "widgets", URL: "https://github.com/example/widgets"}
	target := contract.Object{"repository": "example/widgets", "workflow_file": "task.yml"}
	cases := []struct {
		action  string
		data    contract.Object
		allowed []string
	}{
		{
			action: "legacy-review",
			data: contract.Object{
				"target": target, "run": contract.Object{"id": int64(100), "private_raw": sentinel}, "attempt": int64(2),
				"classification": "no_dispatch_proven", "intent_proven": true, "reasons": []any{"all mutation steps skipped"},
				"sha256": strings.Repeat("a", 64), "evidence": contract.Object{"signature": sentinel}, "sources": []any{sentinel},
				"inputs": contract.Object{"body": sentinel}, "event_file": sentinel, "artifacts": []any{sentinel}, "receipts": []any{sentinel},
			},
			allowed: []string{"target", "run_id", "attempt", "classification", "intent_proven", "reasons", "report_sha256", "full_result_path"},
		},
		{
			action: "legacy-import-preview",
			data: contract.Object{
				"review": contract.Object{
					"target": target, "run_id": int64(100), "attempt": int64(2), "sha256": strings.Repeat("b", 64),
					"history_sha256": strings.Repeat("c", 64), "outcome": "legacy_no_dispatch", "chain_version": int64(5), "raw_packet": sentinel,
				},
				"checkpoint": contract.Object{"sha256": strings.Repeat("d", 64), "raw_history": sentinel},
				"history":    contract.Object{"event_inputs": sentinel}, "report": contract.Object{"sources": sentinel},
				"dispositions": []any{contract.Object{"evidence": sentinel}},
			},
			allowed: []string{"target", "run_id", "attempt", "review_sha256", "history_sha256", "checkpoint_sha256", "outcome", "chain_version", "full_result_path"},
		},
		{
			action: "import-legacy",
			data: contract.Object{
				"outcome": "legacy_no_dispatch", "review_sha256": strings.Repeat("e", 64),
				"checkpoint":                 contract.Object{"sha256": strings.Repeat("f", 64), "raw_chain": sentinel},
				"imported_checkpoint_sha256": strings.Repeat("1", 64), "report": contract.Object{"evidence": sentinel},
				"history": contract.Object{"private_event": sentinel}, "review": contract.Object{"dispositions": sentinel},
			},
			allowed: []string{"outcome", "review_sha256", "checkpoint_sha256", "imported_checkpoint_sha256", "full_result_path"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			encoded, err := runrecovery.Canonical(legacyRecoverySummary(tc.action, tc.data, repository, output))
			if err != nil {
				t.Fatal(err)
			}
			var summary contract.Object
			if err := json.Unmarshal(encoded, &summary); err != nil {
				t.Fatal(err)
			}
			data, err := contract.ObjectAt(summary, "data")
			if err != nil {
				t.Fatal(err)
			}
			if len(data) != len(tc.allowed) {
				t.Fatalf("summary contains unexpected fields: %#v", data)
			}
			for _, key := range tc.allowed {
				if _, ok := data[key]; !ok {
					t.Errorf("summary omitted allowed field %q: %#v", key, data)
				}
			}
			if strings.Contains(string(encoded), sentinel) {
				t.Fatalf("%s summary exposed a private evidence sentinel: %s", tc.action, encoded)
			}
		})
	}
}

type countingLegacyCLIProvider struct{ calls int }

func (p *countingLegacyCLIProvider) Read(context.Context, string) (contract.Object, error) {
	p.calls++
	return nil, errors.New("unexpected provider read")
}
func (p *countingLegacyCLIProvider) Pages(context.Context, string) ([]any, error) {
	p.calls++
	return nil, errors.New("unexpected provider pagination")
}
func (p *countingLegacyCLIProvider) Archive(context.Context, int64) ([]byte, error) {
	p.calls++
	return nil, errors.New("unexpected provider archive")
}

func legacyImportCLIInput(t *testing.T, root string) (contract.Object, string) {
	t.Helper()
	fixture := newLegacyCLIArchive(t)
	report, err := runrecovery.VerifyLegacyEvidence(fixture.evidence, fixture.review)
	if err != nil {
		t.Fatal(err)
	}
	history := contract.Object{}
	historyBytes, err := runrecovery.Canonical(history)
	if err != nil {
		t.Fatal(err)
	}
	unsignedReview := contract.Object{
		"schema_version": int64(1), "chain_version": int64(5), "target": report["target"],
		"run_id": report["run"].(contract.Object)["id"], "attempt": report["attempt"],
		"previous_checkpoint_sha256":  strings.Repeat("a", 64),
		"previous_settlements_sha256": strings.Repeat("b", 64),
		"history_sha256":              runrecovery.SHA256(historyBytes), "report_sha256": report["sha256"],
		"outcome": "legacy_no_dispatch", "dispositions": []any{},
	}
	reviewBytes, err := runrecovery.Canonical(unsignedReview)
	if err != nil {
		t.Fatal(err)
	}
	reviewSHA := runrecovery.SHA256(reviewBytes)
	review := contract.Object{}
	for key, value := range unsignedReview {
		review[key] = value
	}
	review["sha256"] = reviewSHA
	policy := contract.Object{"schema_version": int64(1), "workflows": contract.Object{
		"task.yml": contract.Object{
			"mutator_step_alternatives": []any{[]any{"Apply operation"}}, "reviewed_source_shas": []any{},
			"allow_publication": false, "plans": contract.Object{},
			"legacy_import_reviews": []any{reviewSHA},
		},
	}}
	writeLegacyCLIJSON(t, root, "policy.json", policy)
	input := contract.Object{"report": report, "review": review, "checkpoint": contract.Object{}, "history": history}
	writeLegacyCLIJSON(t, root, "import.json", input)
	return input, reviewSHA
}

func TestLegacyOutputPreflightRejectsUnsafeDestinationsBeforeImportReads(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	pathWithGitOnly(t)
	root := checkout(t)
	_, reviewSHA := legacyImportCLIInput(t, root)
	private := filepath.Join(root, ".private")
	store := filepath.Join(private, "store")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store, 0700); err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(root, "real-parent")
	if err := os.Mkdir(realParent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realParent, filepath.Join(root, "linked-parent")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "private-output"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "private-output", "target.json")
	if err := os.WriteFile(target, []byte("kept"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "private-output", "symlink.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "public-output"), 0700); err != nil {
		t.Fatal(err)
	}
	publicFile := filepath.Join(root, "public-output", "report.json")
	if err := os.WriteFile(publicFile, []byte("public"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(publicFile, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "private-output", "existing.json"), []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := &countingLegacyCLIProvider{}
	absolute := filepath.Join(t.TempDir(), "outside.json")
	cases := []struct {
		name, out string
	}{
		{"traversal", "../outside.json"},
		{"absolute", absolute},
		{"checkout-root", "."},
		{"backslash", `private-output\\report.json`},
		{"symlink-parent", "linked-parent/report.json"},
		{"symlink-file", "private-output/symlink.json"},
		{"public-file", "public-output/report.json"},
		{"existing-private-import-output", "private-output/existing.json"},
		{"store-overlap", ".private/store/report.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := (Runner{Out: &stdout, Err: &stderr, Actions: provider}).Run(context.Background(), []string{
				"runs", "import-legacy", "--repo-root", root, "--repo", "https://github.com/example/widgets",
				"--policy", "policy.json", "--input", "import=import.json", "--store", ".private/store",
				"--approve-review-sha", reviewSHA, "--out", tc.out,
			})
			if err == nil || stdout.Len() != 0 {
				t.Fatalf("unsafe result output emitted success: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			if provider.calls != 0 {
				t.Fatalf("invalid result destination was discovered after %d provider reads", provider.calls)
			}
		})
	}
	entries, err := os.ReadDir(store)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid result output advanced the append-only import store: entries=%v err=%v", entries, err)
	}
}

func TestLegacyReviewDoesNotReplaceDifferentExistingPrivateOutput(t *testing.T) {
	root := checkout(t)
	fixture := newLegacyCLIArchive(t)
	writeLegacyCLIJSON(t, root, "evidence.json", legacyCLIInput(fixture))
	if err := os.Mkdir(filepath.Join(root, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(root, "private", "report.json")
	before := []byte("existing private output\n")
	if err := os.WriteFile(outPath, before, 0600); err != nil {
		t.Fatal(err)
	}
	pathWithGitOnly(t)
	var stdout, stderr bytes.Buffer
	err := (Runner{Out: &stdout, Err: &stderr}).Run(context.Background(), []string{
		"runs", "legacy-review", "--repo-root", root, "--repo", "https://github.com/example/widgets",
		"--input", "evidence=evidence.json", "--out", "private/report.json",
	})
	if err == nil || !strings.Contains(err.Error(), "already exists with different bytes") || stdout.Len() != 0 {
		t.Fatalf("conflicting output was overwritten or accepted: err=%v stdout=%q", err, stdout.String())
	}
	if !bytes.Equal(before, mustRead(t, outPath)) {
		t.Fatal("conflicting result replaced the existing private output")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLegacyReviewRejectsBadSchemaEnvelopeAndUnrelatedFlags(t *testing.T) {
	root := checkout(t)
	fixture := newLegacyCLIArchive(t)
	fixture.evidence["schema_version"] = int64(2)
	evidenceBytes, err := runrecovery.Canonical(fixture.evidence)
	if err != nil {
		t.Fatal(err)
	}
	fixture.review["evidence_sha256"] = runrecovery.SHA256(evidenceBytes)
	writeLegacyCLIJSON(t, root, "bad-schema.json", legacyCLIInput(fixture))
	fixture = newLegacyCLIArchive(t)
	input := legacyCLIInput(fixture)
	envelope := contract.Object{"schema_version": int64(2), "tool_version": "0.2.1", "command": "workflow-legacy-review", "repository": contract.Object{}, "data": input}
	writeLegacyCLIJSON(t, root, "envelope.json", envelope)
	pathWithGitOnly(t)
	base := []string{"runs", "legacy-review", "--repo-root", root, "--repo", "https://github.com/example/widgets"}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "bad schema", args: append(append([]string{}, base...), "--input", "evidence=bad-schema.json")},
		{name: "machine envelope", args: append(append([]string{}, base...), "--input", "evidence=envelope.json")},
		{name: "native plan approval", args: append(append([]string{}, base...), "--input", "evidence=envelope.json", "--approve-plan-sha", strings.Repeat("a", 64))},
		{name: "recovery dispatch selector", args: append(append([]string{}, base...), "--input", "evidence=envelope.json", "--workflow-sha", strings.Repeat("a", 40))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := (Runner{Out: &stdout, Err: &stderr}).Run(context.Background(), tc.args)
			if err == nil || stdout.Len() != 0 {
				t.Fatalf("invalid legacy CLI contract emitted success: err=%v stdout=%q", err, stdout.String())
			}
		})
	}
}

func TestLegacyReviewRejectsSignedEvidenceForAnotherCheckoutRepository(t *testing.T) {
	root := checkout(t)
	fixture := newLegacyCLIArchive(t)
	fixture.payload["target"].(contract.Object)["repository"] = "example/other"
	event, err := runrecovery.Canonical(contract.Object{
		"repository": contract.Object{"full_name": "example/other", "html_url": "https://github.com/example/other"},
		"inputs":     contract.Object{"change": "apply", "number": "17"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.payload["event_file"] = runrecovery.MakeFileProof(event)
	fixture.reseal(t)
	writeLegacyCLIJSON(t, root, "evidence.json", legacyCLIInput(fixture))
	pathWithGitOnly(t)
	var stdout, stderr bytes.Buffer
	err = (Runner{Out: &stdout, Err: &stderr}).Run(context.Background(), []string{
		"runs", "legacy-review", "--repo-root", root, "--repo", "https://github.com/example/widgets", "--input", "evidence=evidence.json",
	})
	if err == nil || !strings.Contains(err.Error(), "another checkout repository") || stdout.Len() != 0 {
		t.Fatalf("legacy evidence for a different repository was accepted: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

func TestLegacyImportChecksExactReviewAndCheckoutStoreBeforeProvider(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	root := checkout(t)
	policy := contract.Object{"schema_version": int64(1), "workflows": contract.Object{
		"task.yml": contract.Object{
			"mutator_step_alternatives": []any{[]any{"Apply operation"}}, "reviewed_source_shas": []any{},
			"allow_publication": false, "plans": contract.Object{},
		},
	}}
	writeLegacyCLIJSON(t, root, "policy.json", policy)
	wrongSHAInput := contract.Object{
		"report": contract.Object{}, "review": contract.Object{"sha256": strings.Repeat("a", 64)},
		"checkpoint": contract.Object{}, "history": contract.Object{},
	}
	writeLegacyCLIJSON(t, root, "wrong-sha.json", wrongSHAInput)
	storeEscapeInput := contract.Object{
		"report": contract.Object{}, "review": contract.Object{"sha256": strings.Repeat("b", 64)},
		"checkpoint": contract.Object{}, "history": contract.Object{},
	}
	writeLegacyCLIJSON(t, root, "store-escape.json", storeEscapeInput)
	pathWithGitOnly(t)
	provider := forbiddenLegacyCLIProvider{t}
	for _, tc := range []struct {
		name string
		file string
		sha  string
		want string
	}{
		{name: "wrong review digest", file: "wrong-sha.json", sha: strings.Repeat("b", 64), want: "exact --approve-review-sha"},
		{name: "checkout store traversal", file: "store-escape.json", sha: strings.Repeat("b", 64), want: "escapes the trusted checkout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := (Runner{Out: &stdout, Err: &stderr, Actions: provider}).Run(context.Background(), []string{
				"runs", "import-legacy", "--repo-root", root, "--repo", "https://github.com/example/widgets",
				"--policy", "policy.json", "--input", "import=" + tc.file, "--store", "../outside",
				"--approve-review-sha", tc.sha,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) || stdout.Len() != 0 {
				t.Fatalf("legacy import crossed a pre-provider boundary: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
		})
	}
}
