package runrecovery

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

type legacyEvidenceFixture struct {
	evidence Object
	review   Object
	payload  Object
	key      ed25519.PrivateKey
}

func newLegacyEvidenceFixture(t *testing.T) *legacyEvidenceFixture {
	t.Helper()
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	target := Object{
		"workflow_file": "task.yml", "repository": "example/widgets", "server_url": "https://github.com",
		"workflow_id": int64(9), "recovery_key": "workflow-history-v2",
	}
	run := Object{
		"id": int64(100), "created_at": "2026-09-01T12:00:00Z", "display_title": "Apply reviewed task",
		"event": "workflow_dispatch", "workflow_id": int64(9), "head_branch": "main",
		"head_sha": strings.Repeat("a", 40), "run_attempt": int64(2), "status": "completed", "conclusion": "success",
	}
	attemptTarget := Object{"recovery_key": "reviewed-task", "identity": Object{"plan_sha256": strings.Repeat("b", 64)}}
	workflowSHA := strings.Repeat("c", 40)
	workflowBytes := []byte("name: Task\non:\n  workflow_dispatch:\njobs:\n  apply:\n    name: Apply\n    steps:\n      - name: Apply operation\n        run: gh steward apply\n  validate:\n    name: Validate\n    steps:\n      - run: echo valid\n")
	workflowProof := MakeFileProof(workflowBytes)
	sources := []any{Object{"path": ".github/workflows/task.yml", "commit": workflowSHA, "file": workflowProof}}
	eventBytes, err := Canonical(Object{
		"repository": Object{"full_name": "example/widgets", "html_url": "https://github.com/example/widgets"},
		"inputs":     Object{"change": "apply", "number": "17"},
	})
	if err != nil {
		t.Fatal(err)
	}
	effects := []any{Object{"kind": "issue-comment", "issue": int64(17), "body": "Reviewed update"}}
	effectBytes, _ := Canonical(effects[0])
	effectSHA := SHA256(effectBytes)
	archiveBytes := legacyTestArchive(t)
	artifactSHA := "sha256:" + SHA256(archiveBytes)
	metadata := Object{
		"id": int64(300), "node_id": "MDg6QXJ0aWZhY3QzMDA=", "name": "legacy-attempt-100-2",
		"size_in_bytes": int64(len(archiveBytes)), "url": "https://api.github.com/repos/example/widgets/actions/artifacts/300",
		"archive_download_url": "https://api.github.com/repos/example/widgets/actions/artifacts/300/zip",
		"expired":              false, "created_at": "2026-09-01T12:01:00Z", "expires_at": "2026-10-01T12:01:00Z",
		"updated_at": "2026-09-01T12:01:00Z", "digest": artifactSHA,
		"workflow_run": Object{
			"id": int64(100), "run_id": int64(100), "run_attempt": int64(2), "repository_id": int64(101),
			"head_repository_id": int64(101), "head_branch": "main", "head_sha": run["head_sha"],
		},
	}
	artifact := Object{"metadata": metadata, "archive": MakeFileProof(archiveBytes), "origin": "actions_artifact"}
	receiptBytes, err := Canonical(Object{
		"schema_version": int64(1), "target": target, "run_id": int64(100), "attempt": int64(2),
		"attempt_target": attemptTarget, "effect_sha256": effectSHA, "outcome": "completed", "provider_receipt_id": "receipt-abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	mutatorJob := Object{
		"id": int64(401), "run_id": int64(100), "run_attempt": int64(2), "name": "Apply",
		"status": "completed", "conclusion": "success",
		"steps": []any{Object{"name": "Apply operation", "status": "completed", "conclusion": "completed"}},
	}
	validationJob := Object{
		"id": int64(402), "run_id": int64(100), "run_attempt": int64(2), "name": "Validate",
		"status": "completed", "conclusion": "success", "steps": []any{},
	}
	payload := Object{
		"schema_version": int64(1), "target": target, "run": run, "attempt": int64(2), "attempt_target": attemptTarget,
		"run_packet": run, "jobs_packet": Object{"run_id": int64(100), "attempt": int64(2), "pages": []any{Object{"total_count": int64(2), "jobs": []any{mutatorJob, validationJob}}}},
		"workflow_sha": workflowSHA, "event_file": MakeFileProof(eventBytes),
		"inputs": Object{"change": "apply", "number": "17"}, "sources": sources, "intended_effects": effects,
		"receipts":     []any{Object{"effect_sha256": effectSHA, "kind": "provider_receipt", "receipt": MakeFileProof(receiptBytes)}},
		"observations": []any{}, "artifacts": []any{artifact},
	}
	review := Object{
		"schema_version": int64(1), "target": target, "run_id": int64(100), "attempt": int64(2),
		"evidence_sha256": strings.Repeat("0", 64), "archive_public_key": base64.StdEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey)),
		"workflow_sha":     workflowSHA,
		"sources":          []any{Object{"path": ".github/workflows/task.yml", "sha256": workflowProof["sha256"]}},
		"mutators":         []any{Object{"job": "Apply", "steps": []any{"Apply operation"}}},
		"non_mutator_jobs": []any{"Validate"},
		"artifacts":        []any{Object{"name": metadata["name"], "id": metadata["id"], "digest": artifactSHA}},
	}
	fixture := &legacyEvidenceFixture{review: review, payload: payload, key: privateKey}
	fixture.reseal(t)
	_ = effectSHA
	return fixture
}

func (f *legacyEvidenceFixture) reseal(t *testing.T) {
	t.Helper()
	payloadBytes, err := Canonical(f.payload)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(f.key, append([]byte(legacyEvidenceDomain), payloadBytes...))
	f.evidence = Object{"schema_version": int64(1), "payload": f.payload, "signature": base64.StdEncoding.EncodeToString(signature)}
	f.bindReview(t)
}

func (f *legacyEvidenceFixture) bindReview(t *testing.T) {
	t.Helper()
	evidenceBytes, err := Canonical(f.evidence)
	if err != nil {
		t.Fatal(err)
	}
	f.review["evidence_sha256"] = SHA256(evidenceBytes)
}

func legacyTestArchive(t *testing.T) []byte {
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

func TestVerifyLegacyEvidenceProducesDeterministicTerminalReport(t *testing.T) {
	fixture := newLegacyEvidenceFixture(t)
	first, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
	if err != nil {
		t.Fatal(err)
	}
	if first["classification"] != "terminal_receipt_proven" || first["intent_proven"] != true {
		t.Fatalf("unexpected terminal classification: %#v", first)
	}
	second, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
	if err != nil || !Equal(first, second) {
		t.Fatalf("report is not deterministic: err=%v", err)
	}
	verified, err := ValidateLegacyReport(first)
	if err != nil || !Equal(first, verified) {
		t.Fatalf("canonical report did not validate: err=%v", err)
	}
	effects, err := LegacyIntendedEffects(first)
	if err != nil || len(effects) != 1 || effects[0]["kind"] != "issue-comment" {
		t.Fatalf("authenticated effects were not recovered: effects=%#v err=%v", effects, err)
	}

	changed := Object{}
	for key, value := range first {
		changed[key] = value
	}
	changed["classification"] = "effect_observed"
	if _, err := ValidateLegacyReport(changed); err == nil {
		t.Fatal("edited classification passed report recomputation")
	}
}

func TestVerifyLegacyEvidenceAcceptsDocumentedArtifactMetadata(t *testing.T) {
	fixture := newLegacyEvidenceFixture(t)
	report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
	if err != nil || report["classification"] != "terminal_receipt_proven" {
		t.Fatalf("documented artifact metadata with explicit attempt binding was rejected: report=%#v err=%v", report, err)
	}

	t.Run("head repository ID must be positive", func(t *testing.T) {
		invalid := newLegacyEvidenceFixture(t)
		metadata := invalid.payload["artifacts"].([]any)[0].(Object)["metadata"].(Object)
		metadata["workflow_run"].(Object)["head_repository_id"] = int64(0)
		invalid.reseal(t)
		if _, err := VerifyLegacyEvidence(invalid.evidence, invalid.review); err == nil {
			t.Fatal("nonpositive head repository ID passed artifact metadata validation")
		}
	})
	t.Run("unknown workflow run fields remain rejected", func(t *testing.T) {
		invalid := newLegacyEvidenceFixture(t)
		metadata := invalid.payload["artifacts"].([]any)[0].(Object)["metadata"].(Object)
		metadata["workflow_run"].(Object)["unreviewed_field"] = "unexpected"
		invalid.reseal(t)
		if _, err := VerifyLegacyEvidence(invalid.evidence, invalid.review); err == nil {
			t.Fatal("unknown workflow run metadata field passed strict validation")
		}
	})
}

func TestVerifyLegacyEvidenceClassifiesSkippedStartedAndPartialReceipts(t *testing.T) {
	t.Run("all mutators skipped", func(t *testing.T) {
		fixture := newLegacyEvidenceFixture(t)
		fixture.payload["receipts"] = []any{}
		jobs := fixture.payload["jobs_packet"].(Object)["pages"].([]any)[0].(Object)["jobs"].([]any)
		jobs[0].(Object)["steps"].([]any)[0].(Object)["status"] = "completed"
		jobs[0].(Object)["steps"].([]any)[0].(Object)["conclusion"] = "skipped"
		fixture.reseal(t)
		report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
		if err != nil || report["classification"] != "no_dispatch_proven" {
			t.Fatalf("skipped mutator was not proven: report=%#v err=%v", report, err)
		}
	})
	t.Run("started mutator with observation", func(t *testing.T) {
		fixture := newLegacyEvidenceFixture(t)
		fixture.payload["receipts"] = []any{}
		jobs := fixture.payload["jobs_packet"].(Object)["pages"].([]any)[0].(Object)["jobs"].([]any)
		step := jobs[0].(Object)["steps"].([]any)[0].(Object)
		step["status"], step["conclusion"] = "in_progress", nil
		operation := fixture.payload["intended_effects"].([]any)[0]
		operationBytes, _ := Canonical(operation)
		fixture.payload["observations"] = []any{Object{
			"effect_sha256": SHA256(operationBytes), "evidence": MakeFileProof([]byte(`{"state":"present"}`)),
		}}
		fixture.reseal(t)
		report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
		if err != nil || report["classification"] != "effect_observed" || report["intent_proven"] != true {
			t.Fatalf("started mutator was misclassified: report=%#v err=%v", report, err)
		}
	})
	t.Run("partial receipts", func(t *testing.T) {
		fixture := newLegacyEvidenceFixture(t)
		fixture.payload["receipts"] = []any{}
		fixture.reseal(t)
		report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
		if err != nil || report["classification"] != "unresolved" || report["intent_proven"] != true {
			t.Fatalf("missing terminal receipt was not held: report=%#v err=%v", report, err)
		}
	})
	t.Run("SDK observation is not terminal", func(t *testing.T) {
		fixture := newLegacyEvidenceFixture(t)
		operationBytes, _ := Canonical(fixture.payload["intended_effects"].([]any)[0])
		fixture.payload["receipts"] = []any{Object{
			"effect_sha256": SHA256(operationBytes), "kind": "sdk_observation",
			"receipt": MakeFileProof([]byte(`{"sdk":"observed"}`)),
		}}
		fixture.reseal(t)
		report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
		if err != nil || report["classification"] != "effect_observed" {
			t.Fatalf("SDK observation established an unexpected result: report=%#v err=%v", report, err)
		}
	})
}

func TestVerifyLegacyEvidenceDistinguishesEmptyAndMissingEffects(t *testing.T) {
	t.Run("explicit empty effects with skipped mutator", func(t *testing.T) {
		fixture := newLegacyEvidenceFixture(t)
		fixture.payload["intended_effects"] = []any{}
		fixture.payload["receipts"] = []any{}
		jobs := fixture.payload["jobs_packet"].(Object)["pages"].([]any)[0].(Object)["jobs"].([]any)
		step := jobs[0].(Object)["steps"].([]any)[0].(Object)
		step["status"], step["conclusion"] = "completed", "skipped"
		fixture.reseal(t)

		report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
		if err != nil || report["classification"] != "no_dispatch_proven" || report["intent_proven"] != true {
			t.Fatalf("explicit empty effects did not support a skipped-mutator proof: report=%#v err=%v", report, err)
		}
	})
	t.Run("explicit empty effects with started mutator", func(t *testing.T) {
		fixture := newLegacyEvidenceFixture(t)
		fixture.payload["intended_effects"] = []any{}
		fixture.payload["receipts"] = []any{}
		jobs := fixture.payload["jobs_packet"].(Object)["pages"].([]any)[0].(Object)["jobs"].([]any)
		step := jobs[0].(Object)["steps"].([]any)[0].(Object)
		step["status"], step["conclusion"] = "in_progress", nil
		fixture.reseal(t)

		report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
		if err != nil || report["classification"] != "unresolved" || report["intent_proven"] != true {
			t.Fatalf("empty effects vacuously established a terminal result: report=%#v err=%v", report, err)
		}
	})
	t.Run("missing effects with skipped mutator", func(t *testing.T) {
		fixture := newLegacyEvidenceFixture(t)
		fixture.payload["intended_effects"] = nil
		fixture.payload["receipts"] = []any{}
		jobs := fixture.payload["jobs_packet"].(Object)["pages"].([]any)[0].(Object)["jobs"].([]any)
		step := jobs[0].(Object)["steps"].([]any)[0].(Object)
		step["status"], step["conclusion"] = "completed", "skipped"
		fixture.reseal(t)

		report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
		if err != nil || report["classification"] != "unresolved" || report["intent_proven"] != false {
			t.Fatalf("missing effects were treated as an explicit empty set: report=%#v err=%v", report, err)
		}
	})
}

func TestVerifyLegacyEvidenceAllowsEmptyReviewedArtifactInventory(t *testing.T) {
	t.Run("terminal receipts with no Actions artifacts", func(t *testing.T) {
		fixture := newLegacyEvidenceFixture(t)
		fixture.payload["artifacts"] = []any{}
		fixture.review["artifacts"] = []any{}
		fixture.reseal(t)

		report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
		if err != nil || report["classification"] != "terminal_receipt_proven" || report["intent_proven"] != true {
			t.Fatalf("empty reviewed artifact inventory was not complete: report=%#v err=%v", report, err)
		}
	})
	t.Run("no dispatch with no Actions artifacts", func(t *testing.T) {
		fixture := newLegacyEvidenceFixture(t)
		fixture.payload["artifacts"] = []any{}
		fixture.review["artifacts"] = []any{}
		fixture.payload["receipts"] = []any{}
		jobs := fixture.payload["jobs_packet"].(Object)["pages"].([]any)[0].(Object)["jobs"].([]any)
		step := jobs[0].(Object)["steps"].([]any)[0].(Object)
		step["status"], step["conclusion"] = "completed", "skipped"
		fixture.reseal(t)

		report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
		if err != nil || report["classification"] != "no_dispatch_proven" || report["intent_proven"] != true {
			t.Fatalf("empty reviewed artifact inventory blocked no-dispatch proof: report=%#v err=%v", report, err)
		}
	})
	t.Run("review expecting an artifact cannot omit its witness", func(t *testing.T) {
		fixture := newLegacyEvidenceFixture(t)
		fixture.payload["artifacts"] = []any{}
		fixture.reseal(t)

		if _, err := VerifyLegacyEvidence(fixture.evidence, fixture.review); err == nil {
			t.Fatal("artifact inventory mismatch passed legacy evidence verification")
		}
	})
}

func TestVerifyLegacyEvidenceAcceptsRepositoryDispatchAndExpiredArchivedBytes(t *testing.T) {
	t.Run("repository dispatch client payload", func(t *testing.T) {
		fixture := newLegacyEvidenceFixture(t)
		fixture.payload["run"].(Object)["event"] = "repository_dispatch"
		fixture.payload["run_packet"].(Object)["event"] = "repository_dispatch"
		event, err := Canonical(Object{
			"repository":     Object{"full_name": "example/widgets", "html_url": "https://github.com/example/widgets"},
			"client_payload": Object{"change": "apply", "number": "17"},
		})
		if err != nil {
			t.Fatal(err)
		}
		fixture.payload["event_file"] = MakeFileProof(event)
		fixture.reseal(t)
		report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
		if err != nil || report["classification"] != "terminal_receipt_proven" {
			t.Fatalf("repository-dispatch payload was not preserved: report=%#v err=%v", report, err)
		}
	})
	t.Run("expired artifact retained by authoritative archive", func(t *testing.T) {
		fixture := newLegacyEvidenceFixture(t)
		artifact := fixture.payload["artifacts"].([]any)[0].(Object)
		artifact["metadata"].(Object)["expired"] = true
		artifact["origin"] = "authoritative_archive"
		fixture.reseal(t)
		report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
		if err != nil || report["intent_proven"] != true {
			t.Fatalf("complete archived bytes were rejected: report=%#v err=%v", report, err)
		}
	})
}

func TestVerifyLegacyEvidenceRejectsEffectsContradictingSkippedMutators(t *testing.T) {
	for _, withProviderReceipt := range []bool{true, false} {
		name := "observation"
		if withProviderReceipt {
			name = "provider receipt"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newLegacyEvidenceFixture(t)
			jobs := fixture.payload["jobs_packet"].(Object)["pages"].([]any)[0].(Object)["jobs"].([]any)
			jobs[0].(Object)["steps"].([]any)[0].(Object)["status"] = "completed"
			jobs[0].(Object)["steps"].([]any)[0].(Object)["conclusion"] = "skipped"
			operationBytes, _ := Canonical(fixture.payload["intended_effects"].([]any)[0])
			if withProviderReceipt {
				fixture.payload["observations"] = []any{}
			} else {
				fixture.payload["receipts"] = []any{}
				fixture.payload["observations"] = []any{Object{
					"effect_sha256": SHA256(operationBytes), "evidence": MakeFileProof([]byte(`{"state":"present"}`)),
				}}
			}
			fixture.reseal(t)
			if _, err := VerifyLegacyEvidence(fixture.evidence, fixture.review); err == nil {
				t.Fatal("effect evidence did not conflict with the exhaustive skipped-mutator proof")
			}
		})
	}
}

func TestVerifyLegacyEvidenceRejectsDuplicateInventories(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*legacyEvidenceFixture)
	}{
		{"duplicate source", func(f *legacyEvidenceFixture) {
			f.payload["sources"] = append(f.payload["sources"].([]any), f.payload["sources"].([]any)[0])
			f.review["sources"] = append(f.review["sources"].([]any), f.review["sources"].([]any)[0])
		}},
		{"duplicate effect", func(f *legacyEvidenceFixture) {
			f.payload["intended_effects"] = append(f.payload["intended_effects"].([]any), f.payload["intended_effects"].([]any)[0])
		}},
		{"duplicate job ID", func(f *legacyEvidenceFixture) {
			jobs := f.payload["jobs_packet"].(Object)["pages"].([]any)[0].(Object)["jobs"].([]any)
			jobs[1].(Object)["id"] = jobs[0].(Object)["id"]
		}},
		{"duplicate effect receipt", func(f *legacyEvidenceFixture) {
			receipts := f.payload["receipts"].([]any)
			f.payload["receipts"] = append(receipts, receipts[0])
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newLegacyEvidenceFixture(t)
			tc.mutate(fixture)
			if tc.name != "duplicate effect receipt" {
				fixture.payload["receipts"] = []any{}
				jobs := fixture.payload["jobs_packet"].(Object)["pages"].([]any)[0].(Object)["jobs"].([]any)
				jobs[0].(Object)["steps"].([]any)[0].(Object)["status"] = "completed"
				jobs[0].(Object)["steps"].([]any)[0].(Object)["conclusion"] = "skipped"
			}
			fixture.reseal(t)
			if _, err := VerifyLegacyEvidence(fixture.evidence, fixture.review); err == nil {
				t.Fatal("duplicate legacy evidence inventory was accepted")
			}
		})
	}
}

func TestVerifyLegacyEvidenceHoldsMissingOriginalEvidence(t *testing.T) {
	cases := []struct {
		name   string
		intent bool
		mutate func(*legacyEvidenceFixture)
	}{
		{"source closure", false, func(f *legacyEvidenceFixture) {
			f.payload["sources"], f.review["sources"] = []any{}, []any{}
		}},
		{"event", false, func(f *legacyEvidenceFixture) { f.payload["event_file"] = nil }},
		{"event repository URL missing", false, func(f *legacyEvidenceFixture) {
			event, err := LoadFileProof(f.payload["event_file"], "fixture event")
			if err != nil {
				t.Fatal(err)
			}
			delete(event.(Object)["repository"].(Object), "html_url")
			eventBytes, err := Canonical(event)
			if err != nil {
				t.Fatal(err)
			}
			f.payload["event_file"] = MakeFileProof(eventBytes)
		}},
		{"event repository URL has another host", false, func(f *legacyEvidenceFixture) {
			event, err := LoadFileProof(f.payload["event_file"], "fixture event")
			if err != nil {
				t.Fatal(err)
			}
			event.(Object)["repository"].(Object)["html_url"] = "https://ghe.example/example/widgets"
			eventBytes, err := Canonical(event)
			if err != nil {
				t.Fatal(err)
			}
			f.payload["event_file"] = MakeFileProof(eventBytes)
		}},
		{"inputs", false, func(f *legacyEvidenceFixture) { f.payload["inputs"] = nil }},
		{"intended effects", false, func(f *legacyEvidenceFixture) { f.payload["intended_effects"] = nil }},
		{"receipts", true, func(f *legacyEvidenceFixture) { f.payload["receipts"] = []any{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newLegacyEvidenceFixture(t)
			tc.mutate(fixture)
			fixture.reseal(t)
			report, err := VerifyLegacyEvidence(fixture.evidence, fixture.review)
			if err != nil {
				t.Fatalf("missing archival evidence should remain held: %v", err)
			}
			if report["classification"] != "unresolved" || report["intent_proven"] != tc.intent {
				t.Fatalf("missing original evidence was trusted: %#v", report)
			}
		})
	}
}

func TestVerifyLegacyEvidenceRejectsInvalidSignaturesAndBindings(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*legacyEvidenceFixture)
	}{
		{"wrong signature", func(f *legacyEvidenceFixture) {
			f.evidence["signature"] = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
		}},
		{"unknown archival key", func(f *legacyEvidenceFixture) {
			other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, ed25519.SeedSize))
			f.review["archive_public_key"] = base64.StdEncoding.EncodeToString(other.Public().(ed25519.PublicKey))
		}},
		{"wrong target", func(f *legacyEvidenceFixture) {
			f.payload["target"].(Object)["repository"] = "another/widgets"
		}},
		{"wrong attempt", func(f *legacyEvidenceFixture) { f.payload["attempt"] = int64(3) }},
		{"wrong root source commit", func(f *legacyEvidenceFixture) {
			f.payload["sources"].([]any)[0].(Object)["commit"] = strings.Repeat("d", 40)
		}},
		{"wrong event repository", func(f *legacyEvidenceFixture) {
			event, _ := Canonical(Object{"repository": Object{"full_name": "another/widgets", "html_url": "https://github.com/another/widgets"}, "inputs": Object{"change": "apply", "number": "17"}})
			f.payload["event_file"] = MakeFileProof(event)
		}},
		{"wrong artifact attempt", func(f *legacyEvidenceFixture) {
			f.payload["artifacts"].([]any)[0].(Object)["metadata"].(Object)["workflow_run"].(Object)["run_attempt"] = int64(1)
		}},
		{"expired artifact without archive authority", func(f *legacyEvidenceFixture) {
			f.payload["artifacts"].([]any)[0].(Object)["metadata"].(Object)["expired"] = true
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newLegacyEvidenceFixture(t)
			tc.mutate(fixture)
			if tc.name == "wrong signature" {
				fixture.bindReview(t)
			} else if tc.name != "unknown archival key" {
				fixture.reseal(t)
			}
			if _, err := VerifyLegacyEvidence(fixture.evidence, fixture.review); err == nil {
				t.Fatal("invalid signed evidence binding was accepted")
			}
		})
	}
}
