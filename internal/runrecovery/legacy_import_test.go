package runrecovery

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func legacyFixtureAttempt(t *testing.T, attempt int64) *legacyEvidenceFixture {
	t.Helper()
	f := newLegacyEvidenceFixture(t)
	f.payload["attempt"], f.review["attempt"] = attempt, attempt
	f.payload["run"].(Object)["run_attempt"] = attempt
	f.payload["run_packet"].(Object)["run_attempt"] = attempt
	jobs := f.payload["jobs_packet"].(Object)
	jobs["attempt"] = attempt
	for _, raw := range jobs["pages"].([]any)[0].(Object)["jobs"].([]any) {
		raw.(Object)["run_attempt"] = attempt
	}
	artifact := f.payload["artifacts"].([]any)[0].(Object)
	metadata := artifact["metadata"].(Object)
	metadata["id"], metadata["name"] = int64(300)+attempt, fmt.Sprintf("legacy-attempt-100-%d", attempt)
	metadata["workflow_run"].(Object)["run_attempt"] = attempt
	f.review["artifacts"].([]any)[0].(Object)["id"] = metadata["id"]
	f.review["artifacts"].([]any)[0].(Object)["name"] = metadata["name"]
	for _, raw := range f.payload["receipts"].([]any) {
		row := raw.(Object)
		value, err := LoadFileProof(row["receipt"], "fixture receipt")
		if err != nil {
			t.Fatal(err)
		}
		receipt := value.(Object)
		receipt["attempt"] = attempt
		bytes, _ := Canonical(receipt)
		row["receipt"] = MakeFileProof(bytes)
	}
	f.reseal(t)
	return f
}

func legacyImportReader(t *testing.T, report Object, latest int64) *recoveryReaderFixture {
	t.Helper()
	payload := report["evidence"].(Object)["payload"].(Object)
	packet := payload["run_packet"].(Object)
	full, err := cloneObject(packet)
	if err != nil {
		t.Fatal(err)
	}
	full["run_attempt"] = latest
	reader := &recoveryReaderFixture{reads: map[string]Object{}, pages: map[string][]any{}, archives: map[int64][]byte{}}
	reader.reads["repos/example/widgets/actions/workflows/task.yml"] = Object{"id": int64(9), "path": ".github/workflows/task.yml"}
	reader.reads[fmt.Sprintf("repos/example/widgets/actions/runs/100/attempts/%d", mustPositive(report["attempt"]))] = packet
	reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"] = []any{Object{"total_count": int64(1), "workflow_runs": []any{full}}}
	reader.pages[fmt.Sprintf("repos/example/widgets/actions/runs/100/attempts/%d/jobs?per_page=100", mustPositive(report["attempt"]))] = payload["jobs_packet"].(Object)["pages"].([]any)
	artifacts := []any{}
	for _, raw := range payload["artifacts"].([]any) {
		artifact := raw.(Object)
		metadata := artifact["metadata"].(Object)
		artifacts = append(artifacts, metadata)
		bytes, err := LoadRawFileProof(artifact["archive"], "fixture archive")
		if err != nil {
			t.Fatal(err)
		}
		reader.archives[mustPositive(metadata["id"])] = bytes
	}
	reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(len(artifacts)), "artifacts": artifacts}}
	return reader
}

func legacyImportSetup(t *testing.T, kind string) (*Engine, *recoveryReaderFixture, LegacyImportOptions) {
	t.Helper()
	f := legacyFixtureAttempt(t, 1)
	switch kind {
	case "legacy_no_dispatch":
		f.payload["receipts"] = []any{}
		f.payload["jobs_packet"].(Object)["pages"].([]any)[0].(Object)["jobs"].([]any)[0].(Object)["conclusion"] = "skipped"
	case "legacy_operation_disposition":
		f.payload["receipts"] = []any{}
	}
	f.reseal(t)
	report, err := VerifyLegacyEvidence(f.evidence, f.review)
	if err != nil {
		t.Fatal(err)
	}
	engine := testRecoveryEngine(t, false)
	reader := legacyImportReader(t, report, 2)
	checkpoint, err := EmptyChain(report["target"])
	if err != nil {
		t.Fatal(err)
	}
	dispositions := []any{}
	if kind == "legacy_operation_disposition" {
		effects, err := LegacyIntendedEffects(report)
		if err != nil {
			t.Fatal(err)
		}
		for _, effect := range effects {
			digest, _ := legacyDigest(effect)
			dispositions = append(dispositions, Object{"effect_sha256": digest, "decision": "accepted_unobservable_effect", "evidence": []any{MakeFileProof([]byte("Exact operation reviewed; original receipt was never retained."))}})
		}
	}
	preview, err := engine.PreviewLegacyImport(context.Background(), reader, report, checkpoint, kind, dispositions)
	if err != nil {
		t.Fatal(err)
	}
	review := preview["review"].(Object)
	engine.workflows["task.yml"].legacyImportReviews[review["sha256"].(string)] = true
	store, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store, 0700); err != nil {
		t.Fatal(err)
	}
	return engine, reader, LegacyImportOptions{Report: report, Review: review, Checkpoint: checkpoint, History: preview["history"].(Object), StoreDirectory: store}
}

func legacyAssertReadOnly(t *testing.T, reader *recoveryReaderFixture) {
	t.Helper()
	for _, call := range reader.calls {
		if !strings.HasPrefix(call, "GET repos/example/widgets/actions/") && !strings.HasPrefix(call, "PAGES repos/example/widgets/actions/") && !strings.HasPrefix(call, "ZIP ") {
			t.Fatalf("legacy workflow crossed the exact read-only boundary: %s", call)
		}
	}
}

func TestLegacyImportPreservesProvenanceAndOnlyAdvancesOneEarliestAttempt(t *testing.T) {
	for _, kind := range []string{"legacy_no_dispatch", "legacy_terminal_receipt", "legacy_operation_disposition"} {
		t.Run(kind, func(t *testing.T) {
			engine, reader, options := legacyImportSetup(t, kind)
			result, err := engine.ImportLegacy(context.Background(), reader, options)
			if err != nil || result["outcome"] != "imported" {
				t.Fatalf("reviewed import failed: %v %v", result, err)
			}
			chain := result["checkpoint"].(Object)
			record := chain["settlements"].([]any)[0].(Object)
			settlement := record["settlement"].(Object)
			if !exactInt(chain["schema_version"], 5) || len(chain["settlements"].([]any)) != 1 || record["artifact"] != nil || record["context_file"] != nil ||
				settlement["kind"] != kind || !Equal(settlement["report"], options.Report) {
				t.Fatal("legacy import fabricated native evidence or lost original provenance")
			}
			runs, latest, _ := legacyHistoryInventory(options.History, options.Report["target"].(Object))
			first, err := legacyEarliest(chain["settlements"].([]any), runs, latest)
			if err != nil || first == nil || !exactInt(first["attempt"], 2) {
				t.Fatal("import cleared more than the one reviewed attempt", first, err)
			}
			before, err := os.ReadDir(options.StoreDirectory)
			if err != nil || len(before) != 3 {
				t.Fatal("immutable base/import/lock receipts were not retained", before, err)
			}
			again, err := engine.ImportLegacy(context.Background(), reader, options)
			if err != nil || again["outcome"] != "already_imported" || !Equal(again["checkpoint"], chain) {
				t.Fatal("same reviewed import did not recover its durable receipt", again, err)
			}
			legacyAssertReadOnly(t, reader)
		})
	}
}

func TestLegacyClassificationAndPreviewNeverAdvanceSettlement(t *testing.T) {
	engine, reader, options := legacyImportSetup(t, "legacy_terminal_receipt")
	delete(engine.workflows["task.yml"].legacyImportReviews, options.Review["sha256"].(string))
	if _, err := engine.ImportLegacy(context.Background(), reader, options); err == nil {
		t.Fatal("inert classification/preview admitted an unreviewed import")
	}
	entries, _ := os.ReadDir(options.StoreDirectory)
	if len(entries) != 0 || len(options.Checkpoint["settlements"].([]any)) != 0 || !exactInt(options.Checkpoint["schema_version"], 4) {
		t.Fatal("classification or preview persisted settlement history")
	}
}

func TestLegacyImportHoldsChangedOrIncompleteLiveEvidence(t *testing.T) {
	for _, mutation := range []string{"history-attempt", "workflow-id", "workflow-path", "run-packet", "jobs", "artifact-missing", "artifact-expired", "artifact-digest", "artifact-attempt", "artifact-top-level-attempt", "artifact-bytes", "artifact-duplicate", "predecessor", "review-hash", "retained-history"} {
		t.Run(mutation, func(t *testing.T) {
			engine, reader, options := legacyImportSetup(t, "legacy_terminal_receipt")
			// Clone live fixtures so a provider drift cannot edit signed evidence by alias.
			for key, packet := range reader.reads {
				reader.reads[key], _ = cloneObject(packet)
			}
			for key, pages := range reader.pages {
				encoded, _ := Canonical(pages)
				value, _ := DecodeValue(encoded)
				reader.pages[key] = value.([]any)
			}
			switch mutation {
			case "history-attempt":
				reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)["workflow_runs"].([]any)[0].(Object)["run_attempt"] = int64(3)
			case "workflow-id":
				reader.reads["repos/example/widgets/actions/workflows/task.yml"]["id"] = int64(10)
			case "workflow-path":
				reader.reads["repos/example/widgets/actions/workflows/task.yml"]["path"] = ".github/workflows/other.yml"
			case "run-packet":
				reader.reads["repos/example/widgets/actions/runs/100/attempts/1"]["conclusion"] = "failure"
			case "jobs":
				reader.pages["repos/example/widgets/actions/runs/100/attempts/1/jobs?per_page=100"][0].(Object)["jobs"].([]any)[0].(Object)["name"] = "Changed job"
			case "artifact-missing":
				reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(0), "artifacts": []any{}}}
			case "artifact-expired":
				reader.pages["repos/example/widgets/actions/artifacts?per_page=100"][0].(Object)["artifacts"].([]any)[0].(Object)["expired"] = true
			case "artifact-digest":
				reader.pages["repos/example/widgets/actions/artifacts?per_page=100"][0].(Object)["artifacts"].([]any)[0].(Object)["digest"] = "sha256:" + strings.Repeat("d", 64)
			case "artifact-attempt":
				reader.pages["repos/example/widgets/actions/artifacts?per_page=100"][0].(Object)["artifacts"].([]any)[0].(Object)["workflow_run"].(Object)["run_attempt"] = int64(2)
			case "artifact-top-level-attempt":
				reader.pages["repos/example/widgets/actions/artifacts?per_page=100"][0].(Object)["artifacts"].([]any)[0].(Object)["run_attempt"] = int64(2)
			case "artifact-bytes":
				reader.archives[301] = []byte("different raw archive")
			case "artifact-duplicate":
				page := reader.pages["repos/example/widgets/actions/artifacts?per_page=100"][0].(Object)
				other, _ := cloneObject(page["artifacts"].([]any)[0].(Object))
				other["id"] = int64(999)
				page["artifacts"], page["total_count"] = append(page["artifacts"].([]any), other), int64(2)
			case "predecessor":
				options.Checkpoint, _ = cloneObject(options.Checkpoint)
				options.Checkpoint["schema_version"] = int64(5)
				_ = rehashLegacyObject(options.Checkpoint)
			case "review-hash":
				options.Review, _ = cloneObject(options.Review)
				options.Review["sha256"] = strings.Repeat("f", 64)
			case "retained-history":
				options.History, _ = cloneObject(options.History)
				options.History["runs"].([]any)[0].(Object)["latest_attempt"] = int64(3)
			}
			if _, err := engine.ImportLegacy(context.Background(), reader, options); err == nil {
				t.Fatal("changed or incomplete exact evidence admitted an import")
			}
			entries, _ := os.ReadDir(options.StoreDirectory)
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".json") {
					t.Fatal("failed import advanced a durable checkpoint")
				}
			}
			legacyAssertReadOnly(t, reader)
		})
	}
}

func TestLegacyImportRequiresCompleteLiveArtifactWitnesses(t *testing.T) {
	for _, emptyArchive := range []bool{false, true} {
		for _, binding := range []string{"same-attempt", "unknown-attempt", "conflicting-attempt", "missing-run", "other-attempt", "other-run"} {
			t.Run(fmt.Sprintf("empty-archive-%t/%s", emptyArchive, binding), func(t *testing.T) {
				f := legacyFixtureAttempt(t, 1)
				extra, _ := cloneObject(f.payload["artifacts"].([]any)[0].(Object)["metadata"].(Object))
				extra["id"], extra["name"] = int64(999), "additional-witness"
				if emptyArchive {
					f.payload["artifacts"], f.review["artifacts"] = []any{}, []any{}
					f.reseal(t)
				}
				report, err := VerifyLegacyEvidence(f.evidence, f.review)
				if err != nil {
					t.Fatal(err)
				}
				engine := testRecoveryEngine(t, false)
				reader := legacyImportReader(t, report, 2)
				checkpoint, _ := EmptyChain(report["target"])
				preview, err := engine.PreviewLegacyImport(context.Background(), reader, report, checkpoint, "legacy_terminal_receipt", []any{})
				if err != nil {
					t.Fatal("complete original inventory failed preview", err)
				}
				review := preview["review"].(Object)
				engine.workflows["task.yml"].legacyImportReviews[review["sha256"].(string)] = true
				store, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(store, 0700); err != nil {
					t.Fatal(err)
				}
				switch binding {
				case "unknown-attempt":
					delete(extra["workflow_run"].(Object), "run_attempt")
				case "conflicting-attempt":
					extra["run_attempt"] = int64(2)
				case "missing-run":
					delete(extra, "workflow_run")
				case "other-attempt":
					extra["workflow_run"].(Object)["run_attempt"] = int64(2)
				case "other-run":
					extra["workflow_run"].(Object)["id"] = int64(200)
					extra["workflow_run"].(Object)["run_id"] = int64(200)
				}
				page := reader.pages["repos/example/widgets/actions/artifacts?per_page=100"][0].(Object)
				page["artifacts"] = append(page["artifacts"].([]any), extra)
				page["total_count"] = int64(len(page["artifacts"].([]any)))
				accepted := binding == "other-attempt" || binding == "other-run"
				_, previewErr := engine.PreviewLegacyImport(context.Background(), reader, report, checkpoint, "legacy_terminal_receipt", []any{})
				if accepted != (previewErr == nil) {
					t.Fatalf("live artifact membership was misclassified: accepted=%t err=%v", accepted, previewErr)
				}
				result, importErr := engine.ImportLegacy(context.Background(), reader, LegacyImportOptions{
					Report: report, Review: review, Checkpoint: checkpoint, History: preview["history"].(Object), StoreDirectory: store,
				})
				if accepted {
					if importErr != nil || result["outcome"] != "imported" {
						t.Fatal("positive unrelated artifact witness blocked the exact attempt", result, importErr)
					}
				} else {
					if importErr == nil {
						t.Fatal("incomplete live artifact inventory admitted a reviewed import")
					}
					entries, _ := os.ReadDir(store)
					for _, entry := range entries {
						if strings.HasSuffix(entry.Name(), ".json") {
							t.Fatal("incomplete live artifact inventory advanced durable history")
						}
					}
				}
				for _, call := range reader.calls {
					if call == "ZIP 999" {
						t.Fatal("unreviewed artifact bytes were acquired before exact membership review")
					}
				}
				legacyAssertReadOnly(t, reader)
			})
		}
	}
}

func TestLegacyDispositionRejectsBlanketPartialAndInventedEffectDecisions(t *testing.T) {
	for _, mutation := range []string{"partial", "duplicate", "different-effect", "blanket", "no-evidence", "missing-inputs"} {
		t.Run(mutation, func(t *testing.T) {
			engine, reader, options := legacyImportSetup(t, "legacy_operation_disposition")
			rows, _ := cloneObject(Object{"rows": options.Review["dispositions"]})
			dispositions := rows["rows"].([]any)
			switch mutation {
			case "partial":
				dispositions = []any{}
			case "duplicate":
				dispositions = append(dispositions, dispositions[0])
			case "different-effect":
				dispositions[0].(Object)["effect_sha256"] = strings.Repeat("b", 64)
			case "blanket":
				dispositions[0].(Object)["decision"] = "acknowledge-all-old-runs"
			case "no-evidence":
				dispositions[0].(Object)["evidence"] = []any{}
			case "missing-inputs":
				f := legacyFixtureAttempt(t, 1)
				f.payload["inputs"], f.payload["receipts"] = nil, []any{}
				f.reseal(t)
				var err error
				options.Report, err = VerifyLegacyEvidence(f.evidence, f.review)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := engine.PreviewLegacyImport(context.Background(), reader, options.Report, options.Checkpoint, "legacy_operation_disposition", dispositions); err == nil {
				t.Fatal("incomplete or blanket disposition admitted history")
			}
		})
	}
}

func TestLegacyCheckpointRemainsHeldAtNextUncoveredAttempt(t *testing.T) {
	engine, reader, options := legacyImportSetup(t, "legacy_terminal_receipt")
	result, err := engine.ImportLegacy(context.Background(), reader, options)
	if err != nil {
		t.Fatal(err)
	}
	chain := result["checkpoint"].(Object)
	page := reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)
	current := recoveryHistoryRun(200, 1, "Current")
	current["created_at"] = "2026-09-02T12:00:00Z"
	page["workflow_runs"], page["total_count"] = append(page["workflow_runs"].([]any), current), int64(2)
	reader.reads["repos/example/widgets/actions/runs/100/attempts/2"] = reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)["workflow_runs"].([]any)[0].(Object)
	temp := t.TempDir()
	root := filepath.Join(temp, "invocation")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	invocation := Invocation{Workflow: "task.yml", RunID: 200, Attempt: 1, RunName: "Current", RecoveryKey: "new-task", PackageRoot: root, RunnerTemp: temp}
	held, err := engine.RecoverWithLegacyCheckpoint(context.Background(), reader, invocation, chain)
	if err != nil || held["outcome"] != "recovery_needed" {
		t.Fatal("next uncovered attempt did not remain held", held, err)
	}
	found := false
	for _, call := range reader.calls {
		found = found || call == "GET repos/example/widgets/actions/runs/100/attempts/2"
	}
	if !found {
		t.Fatal("native recovery did not observe the exact next historical attempt")
	}
	if _, err := os.Stat(filepath.Join(root, "run-context.json")); !os.IsNotExist(err) {
		t.Fatal("held recovery fabricated a native context")
	}
	legacyAssertReadOnly(t, reader)
}

func TestLegacyImportFreshProcessRecoversAfterReceiptBeforeAcknowledgement(t *testing.T) {
	if os.Getenv("GH_STEWARD_LEGACY_CHILD") != "" {
		path := os.Getenv("GH_STEWARD_LEGACY_INPUT")
		value, err := LoadJSON(path)
		if err != nil {
			t.Fatal(err)
		}
		input := value.(Object)
		report, review := input["report"].(Object), input["review"].(Object)
		engine := testRecoveryEngine(t, false)
		engine.workflows["task.yml"].legacyImportReviews[review["sha256"].(string)] = true
		reader := legacyImportReader(t, report, 2)
		options := LegacyImportOptions{Report: report, Review: review, Checkpoint: input["checkpoint"].(Object), History: input["history"].(Object), StoreDirectory: input["store"].(string)}
		result, err := engine.ImportLegacy(context.Background(), reader, options)
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv("GH_STEWARD_LEGACY_CHILD") == "interrupt" {
			if result["outcome"] != "imported" {
				t.Fatal("child did not import")
			}
			os.Exit(27) // Receipt persisted; caller never received an acknowledgement.
		}
		if result["outcome"] != "already_imported" {
			t.Fatal("fresh process re-applied the historical disposition", result)
		}
		return
	}
	_, _, options := legacyImportSetup(t, "legacy_terminal_receipt")
	input := Object{"report": options.Report, "review": options.Review, "checkpoint": options.Checkpoint, "history": options.History, "store": options.StoreDirectory}
	bytes, _ := Canonical(input)
	path := filepath.Join(t.TempDir(), "import.json")
	if err := os.WriteFile(path, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"interrupt", "recover"} {
		command := exec.Command(os.Args[0], "-test.run=^TestLegacyImportFreshProcessRecoversAfterReceiptBeforeAcknowledgement$")
		command.Env = append(os.Environ(), "GH_STEWARD_LEGACY_CHILD="+stage, "GH_STEWARD_LEGACY_INPUT="+path)
		out, err := command.CombinedOutput()
		if stage == "interrupt" {
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 27 {
				t.Fatalf("expected interrupted acknowledgement, got %v: %s", err, out)
			}
		} else if err != nil {
			t.Fatalf("fresh-process recovery failed: %v: %s", err, out)
		}
	}
	entries, _ := os.ReadDir(options.StoreDirectory)
	if len(entries) != 3 {
		t.Fatal("fresh-process recovery changed append-only history", entries)
	}
}

func TestExplicitLegacyCheckpointRejectsUnreviewedNativeSuffixAndChangedFields(t *testing.T) {
	engine, reader, options := legacyImportSetup(t, "legacy_terminal_receipt")
	result, err := engine.ImportLegacy(context.Background(), reader, options)
	if err != nil {
		t.Fatal(err)
	}
	chain := result["checkpoint"].(Object)
	if err := validateExplicitLegacyCheckpoint(chain); err != nil {
		t.Fatal("legitimate imported prefix rejected", err)
	}
	for _, mutation := range []string{"native-suffix", "prepared-frontier", "prepared-proofs", "inventory"} {
		t.Run(mutation, func(t *testing.T) {
			changed, _ := cloneObject(chain)
			switch mutation {
			case "native-suffix":
				changed["settlements"] = append(changed["settlements"].([]any), Object{"settlement": Object{"kind": "terminal"}})
			case "prepared-frontier":
				changed["prepared_frontier"] = []any{Object{"kind": "prepared"}}
			case "prepared-proofs":
				changed["prepared_terminal_proofs"] = []any{Object{"unreviewed": true}}
			case "inventory":
				changed["inventory"].([]any)[0].(Object)["settled_attempt"] = int64(2)
			}
			_ = rehashLegacyObject(changed)
			if err := validateExplicitLegacyCheckpoint(changed); err == nil {
				t.Fatal("unreviewed fields were admitted through an authentic legacy prefix")
			}
		})
	}
}

func TestLegacyImportsContinueFromVersionFiveWithoutReplacingPriorProvenance(t *testing.T) {
	engine, reader, options := legacyImportSetup(t, "legacy_terminal_receipt")
	first, err := engine.ImportLegacy(context.Background(), reader, options)
	if err != nil {
		t.Fatal(err)
	}
	chain := first["checkpoint"].(Object)
	f := legacyFixtureAttempt(t, 2)
	report, err := VerifyLegacyEvidence(f.evidence, f.review)
	if err != nil {
		t.Fatal(err)
	}
	secondReader := legacyImportReader(t, report, 2)
	preview, err := engine.PreviewLegacyImport(context.Background(), secondReader, report, chain, "legacy_terminal_receipt", []any{})
	if err != nil {
		t.Fatal(err)
	}
	review := preview["review"].(Object)
	engine.workflows["task.yml"].legacyImportReviews[review["sha256"].(string)] = true
	second, err := engine.ImportLegacy(context.Background(), secondReader, LegacyImportOptions{Report: report, Review: review, Checkpoint: chain, History: preview["history"].(Object), StoreDirectory: options.StoreDirectory})
	if err != nil || second["outcome"] != "imported" {
		t.Fatal("second exact import failed", second, err)
	}
	last := second["checkpoint"].(Object)
	if len(last["settlements"].([]any)) != 2 || !Equal(last["settlements"].([]any)[0], chain["settlements"].([]any)[0]) {
		t.Fatal("second import replaced original provenance")
	}
	if err := validateExplicitLegacyCheckpoint(last); err != nil {
		t.Fatal("version-five import cannot be recovered", err)
	}
	// Both imports settled exact attempts; a fresh current invocation may now
	// retain that prefix, with no fabricated native receipt for either source.
	page := secondReader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)
	current := recoveryHistoryRun(200, 1, "Current")
	current["created_at"] = "2026-09-02T12:00:00Z"
	page["workflow_runs"], page["total_count"] = append(page["workflow_runs"].([]any), current), int64(2)
	temp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(temp, "invocation")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	result, err := engine.RecoverWithLegacyCheckpoint(context.Background(), secondReader, Invocation{Workflow: "task.yml", RunID: 200, Attempt: 1, RunName: "Current", RecoveryKey: "new-task", PackageRoot: root, RunnerTemp: temp}, last)
	if err != nil || result["outcome"] != "fresh" {
		t.Fatal("fully qualified prefix remained unavailable", result, err)
	}
	retained, err := LoadJSON(filepath.Join(root, "settlement-chain.json"))
	if err != nil || !Equal(retained, last) {
		t.Fatal("recovery lost imported prefix", err)
	}
	legacyAssertReadOnly(t, secondReader)
}

func TestLegacyReviewRejectsArtifactReusedAcrossAttempts(t *testing.T) {
	engine, reader, options := legacyImportSetup(t, "legacy_terminal_receipt")
	first, err := engine.ImportLegacy(context.Background(), reader, options)
	if err != nil {
		t.Fatal(err)
	}
	f := legacyFixtureAttempt(t, 2)
	f.payload["artifacts"].([]any)[0].(Object)["metadata"].(Object)["id"] = int64(301)
	f.review["artifacts"].([]any)[0].(Object)["id"] = int64(301)
	f.reseal(t)
	report, err := VerifyLegacyEvidence(f.evidence, f.review)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.PreviewLegacyImport(context.Background(), legacyImportReader(t, report, 2), report, first["checkpoint"].(Object), "legacy_terminal_receipt", []any{}); err == nil {
		t.Fatal("immutable artifact ID was reassigned to another attempt")
	}
}
