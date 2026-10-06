package runrecovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func largeHistoryArchiveFixture(t *testing.T) (Object, *recoveryReaderFixture, []Object) {
	t.Helper()
	runs := make([]Object, 587)
	reads := map[string]Object{}
	payload := strings.Repeat("complete raw provider evidence; ", 600)
	for index := range runs {
		run := recoveryHistoryRun(int64(index+1), 1, fmt.Sprintf("Historical run %d", index+1))
		run["raw_provider_payload"] = payload
		runs[index] = run
		reads[historyCutoverAttemptEndpoint(int64(index+1), 1)] = historyCutoverAttemptResponse(run, 1, run["created_at"].(string))
	}
	artifact := Object{"id": int64(71), "name": "historical-report", "expired": true, "raw_artifact_metadata": Object{"retained": "complete", "nested": []any{"one", "two"}}}
	reader := historyCutoverFixture(runs, []Object{artifact}, reads, map[string]Object{"repos/example/widgets/issues/17": {"number": int64(17), "state": "open", "body": "complete point-in-time state"}})
	pages := []any{}
	for start := 0; start < len(runs); start += 100 {
		end := start + 100
		if end > len(runs) {
			end = len(runs)
		}
		pages = append(pages, Object{"total_count": int64(len(runs)), "workflow_runs": objectRows(runs[start:end])})
	}
	reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"] = pages
	b, err := CaptureHistoryCutover(context.Background(), reader, historyCutoverTestRepository, "task.yml", []string{"repos/example/widgets/issues/17"})
	if err != nil {
		t.Fatal(err)
	}
	return b, reader, runs
}

func TestLargeHistoryArchiveRetainsAll587RawRunsAttemptsArtifactsAndState(t *testing.T) {
	document, reader, runs := largeHistoryArchiveFixture(t)
	if !exactInt(document["schema_version"], 2) || document["scope"] != "preview-only" {
		t.Fatal("large complete history was truncated or did not use the archival schema")
	}
	encoded, err := Canonical(document)
	if err != nil || len(encoded) > MaxCheckpointBytes {
		t.Fatal("large history raised the checkpoint/file limit", len(encoded), err)
	}
	evidence := document["evidence"].(Object)
	if mustPositive(evidence["uncompressed_bytes"]) <= MaxCheckpointBytes {
		t.Fatal("fixture did not exceed the old 8 MiB bound")
	}
	raw, err := HistoryCutoverEvidence(document)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw["run_inventory"].([]any)) != 587 || len(raw["attempts"].([]any)) != 587 || len(raw["artifact_inventory"].([]any)) != 1 || len(raw["state_reads"].([]any)) != 1 {
		t.Fatal("archive lost complete capture inventories")
	}
	for index, run := range runs {
		if !Equal(raw["run_inventory"].([]any)[index], run) {
			t.Fatal("archive normalized away a raw workflow response", index)
		}
		attempt := raw["attempts"].([]any)[index].(Object)
		if !Equal(attempt["response"], reader.reads[historyCutoverAttemptEndpoint(int64(index+1), 1)]) || attempt["outcome"] != "unknown" || attempt["handling"] != "quarantined-never-replay" {
			t.Fatal("archive lost or settled an exact raw attempt", index)
		}
	}
	if !Equal(raw["state_reads"].([]any)[0].(Object)["object"], reader.reads["repos/example/widgets/issues/17"]) {
		t.Fatal("archive lost current state evidence")
	}
	second := captureHistoryCutoverFixture(t, reader, "repos/example/widgets/issues/17")
	if !Equal(document, second) {
		t.Fatal("same complete evidence did not produce a deterministic review identity")
	}
	t.Logf("587 runs/attempts: %d decompressed bytes, %d compressed bytes, %d checkpoint-document bytes", mustPositive(evidence["uncompressed_bytes"]), mustPositive(evidence["compressed_bytes"]), len(encoded))
}

func TestLargeArchiveReviewAndPromotionKeepExactIdentityAndUncapturedAttemptsPending(t *testing.T) {
	document, _, runs := largeHistoryArchiveFixture(t)
	f, _, _, _ := promotionFixture(t)
	policyValue, _ := DecodeValue(f.policyBytes)
	wf := policyValue.(Object)["workflows"].(Object)["task.yml"].(Object)
	wf["history_cutover_reviews"] = []any{document["sha256"]}
	engine, err := NewEngine(policyValue.(Object), f.repository)
	if err != nil {
		t.Fatal(err)
	}
	target := document["target"].(Object)
	chain, err := HistoryCutoverChain(target, document)
	if err != nil {
		t.Fatal(err)
	}
	observed, latest, err := promotionLiveHistory(runs, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testRecoveryEngine(t, false).ValidateChain(chain, target, observed, latest); err == nil {
		t.Fatal("compressed capture approved itself")
	}
	if _, err := engine.ValidateChain(chain, target, observed, latest); err != nil {
		t.Fatal(err)
	}
	// New runs and reruns remain outside the sealed complete legacy boundary.
	later := recoveryHistoryRun(600, 1, "Later")
	observedLater, _ := NormalizeRun(later)
	observed = append(observed, observedLater)
	latest[600] = 1
	latest[1] = 2
	current := recoveryHistoryRun(601, 1, "Current")
	currentRow, _ := NormalizeRun(current)
	observed = append(observed, currentRow)
	latest[601] = 1
	pending, err := PendingAttempts(chain, observed, latest, 601, 1)
	if err != nil || len(pending) != 2 || !exactInt(pending[0]["attempt"], 2) || !exactInt(pending[0]["run"].(Object)["id"], 1) || !exactInt(pending[1]["run"].(Object)["id"], 600) {
		t.Fatal("archive silently absorbed later history", pending, err)
	}
	reader := historyCutoverFixture(runs, nil, nil, map[string]Object{"repos/example/widgets/issues/17": {"number": int64(17), "state": "open"}})
	p, err := engine.PreviewHistoryPromotion(context.Background(), reader, document, nil, []string{"execution"}, []string{"repos/example/widgets/issues/17"})
	if err != nil {
		t.Fatal(err)
	}
	admitPromotionFixture(t, engine, p)
	promoted, err := historyPromotionChain(p)
	if err != nil {
		t.Fatal(err)
	}
	observed, latest, err = promotionLiveHistory(runs, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ValidateChain(promoted, target, observed, latest); err != nil {
		t.Fatal("promotion rejected complete compressed lineage", err)
	}
	retained, err := historyCutoverFromChain(promoted)
	if err != nil || !Equal(retained, document) || retained["sha256"] != document["sha256"] {
		t.Fatal("promotion changed the reviewed archive identity", err)
	}
}

func smallHistoryArchiveFixture(t *testing.T) Object {
	t.Helper()
	run := recoveryHistoryRun(101, 2, "Historical rerun")
	raw := captureHistoryCutoverFixture(t, historyCutoverFixture([]Object{run}, nil, map[string]Object{
		historyCutoverAttemptEndpoint(101, 1): historyCutoverAttemptResponse(run, 1, "2026-01-01T00:00:00Z"), historyCutoverAttemptEndpoint(101, 2): historyCutoverAttemptResponse(run, 2, "2026-01-02T00:00:00Z")}, nil))
	document, err := encodeHistoryArchive(raw)
	if err != nil {
		t.Fatal(err)
	}
	return document
}
func archiveRawBytes(t *testing.T, document Object) []byte {
	t.Helper()
	packed, err := base64.StdEncoding.DecodeString(document["evidence"].(Object)["data"].(string))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func replaceArchivePacked(t *testing.T, document Object, packed, raw []byte) Object {
	t.Helper()
	e := document["evidence"].(Object)
	e["data"] = base64.StdEncoding.EncodeToString(packed)
	e["compressed_sha256"] = SHA256(packed)
	e["compressed_bytes"] = int64(len(packed))
	if raw != nil {
		e["uncompressed_bytes"] = int64(len(raw))
		e["uncompressed_sha256"] = SHA256(raw)
	}
	return resealPromotion(t, document)
}
func replaceArchiveRaw(t *testing.T, document Object, raw []byte) Object {
	t.Helper()
	var packed bytes.Buffer
	writer := gzip.NewWriter(&packed)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return replaceArchivePacked(t, document, packed.Bytes(), raw)
}

func TestHistoryArchiveRejectsSealedTruncationDuplicateForeignAndTamperedEvidence(t *testing.T) {
	for _, kind := range []string{"outer-digest", "compressed-digest", "raw-digest", "missing", "duplicate", "reordered", "foreign", "unsettled", "later-rerun", "duplicate-json-key", "count", "header", "truncated-gzip", "trailing-member", "trailing-bytes", "missing-newline", "noncanonical-base64", "declared-compressed-bound", "declared-raw-bound", "declared-count-bound"} {
		t.Run(kind, func(t *testing.T) {
			document := smallHistoryArchiveFixture(t)
			raw := archiveRawBytes(t, document)
			evidence := document["evidence"].(Object)
			switch kind {
			case "outer-digest":
				document["sha256"] = strings.Repeat("a", 64)
			case "compressed-digest":
				evidence["compressed_sha256"] = strings.Repeat("a", 64)
				resealPromotion(t, document)
			case "raw-digest":
				evidence["uncompressed_sha256"] = strings.Repeat("a", 64)
				resealPromotion(t, document)
			case "missing", "duplicate", "reordered", "foreign", "unsettled", "later-rerun", "duplicate-json-key", "header", "count":
				lines := bytes.Split(raw, []byte{'\n'})
				switch kind {
				case "missing":
					lines = append(lines[:2], lines[3:]...)
				case "duplicate":
					lines[3] = append([]byte{}, lines[2]...)
				case "reordered":
					lines[2], lines[3] = lines[3], lines[2]
				case "foreign":
					lines[1] = bytes.ReplaceAll(lines[1], []byte(`"workflow_id":9`), []byte(`"workflow_id":10`))
				case "unsettled":
					lines[2] = bytes.ReplaceAll(lines[2], []byte(`"outcome":"unknown"`), []byte(`"outcome":"completed"`))
				case "later-rerun":
					lines[1] = bytes.ReplaceAll(lines[1], []byte(`"run_attempt":2`), []byte(`"run_attempt":3`))
				case "duplicate-json-key":
					lines[1] = bytes.Replace(lines[1], []byte(`"index":0`), []byte(`"index":0,"index":0`), 1)
				case "header":
					lines[0] = bytes.ReplaceAll(lines[0], []byte(`"scope":"preview-only"`), []byte(`"scope":"native"`))
				case "count":
					lines[0] = bytes.ReplaceAll(lines[0], []byte(`"attempt":2`), []byte(`"attempt":3`))
				}
				document = replaceArchiveRaw(t, document, bytes.Join(lines, []byte{'\n'}))
			case "truncated-gzip", "trailing-member", "trailing-bytes":
				packed, _ := base64.StdEncoding.DecodeString(evidence["data"].(string))
				switch kind {
				case "truncated-gzip":
					packed = packed[:len(packed)-3]
				case "trailing-member":
					packed = append(packed, packed...)
				case "trailing-bytes":
					packed = append(packed, 'x')
				}
				document = replaceArchivePacked(t, document, packed, nil)
			case "missing-newline":
				document = replaceArchiveRaw(t, document, raw[:len(raw)-1])
			case "noncanonical-base64":
				evidence["data"] = evidence["data"].(string) + "\n"
				resealPromotion(t, document)
			case "declared-compressed-bound":
				evidence["compressed_bytes"] = int64(MaxHistoryArchiveBytes + 1)
				resealPromotion(t, document)
			case "declared-raw-bound":
				evidence["uncompressed_bytes"] = int64(MaxHistoryEvidenceBytes + 1)
				resealPromotion(t, document)
			case "declared-count-bound":
				evidence["record_count"] = int64(maxHistoryRuns + 2*maxPendingAttempts + 130)
				resealPromotion(t, document)
			}
			if _, err := ValidateHistoryCutover(document); err == nil {
				t.Fatal("unqualified compressed history accepted", kind)
			}
		})
	}
}

func TestHistoryArchivePreservesStrictCompleteRawValidationWhenResealed(t *testing.T) {
	for _, kind := range []string{"duplicate-run", "duplicate-attempt", "foreign-attempt", "foreign-workflow", "new-highwater", "nonterminal", "old-native-lineage", "foreign-state", "unordered-state"} {
		t.Run(kind, func(t *testing.T) {
			document := smallHistoryArchiveFixture(t)
			raw, err := HistoryCutoverEvidence(document)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "duplicate-run":
				raw["run_inventory"] = append(raw["run_inventory"].([]any), raw["run_inventory"].([]any)[0])
			case "duplicate-attempt":
				raw["attempts"].([]any)[1] = raw["attempts"].([]any)[0]
			case "foreign-attempt":
				raw["attempts"].([]any)[0].(Object)["response"].(Object)["id"] = int64(102)
			case "foreign-workflow":
				raw["run_inventory"].([]any)[0].(Object)["workflow_id"] = int64(10)
			case "new-highwater":
				raw["run_inventory"].([]any)[0].(Object)["run_attempt"] = int64(3)
			case "nonterminal":
				raw["attempts"].([]any)[0].(Object)["response"].(Object)["status"] = "in_progress"
			case "old-native-lineage":
				raw["artifact_inventory"] = []any{Object{"id": int64(7), "name": RecoveryArtifactName(raw["target"], 101, 1)}}
			case "foreign-state":
				raw["state_reads"] = []any{Object{"endpoint": "repos/other/widgets/issues/17", "object": Object{"state": "open"}}}
			case "unordered-state":
				raw["state_reads"] = []any{Object{"endpoint": "repos/example/widgets/issues/18", "object": Object{}}, Object{"endpoint": "repos/example/widgets/issues/17", "object": Object{}}}
			}
			resealHistoryCutoverFixture(t, raw)
			changed, err := encodeHistoryArchive(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateHistoryCutover(changed); err == nil {
				t.Fatal("compression bypassed complete raw validation", kind)
			}
		})
	}
}

func TestHistoryCaptureBoundsSingleObjectsAggregateBytesAndNodesBeforeRetention(t *testing.T) {
	t.Run("single record", func(t *testing.T) {
		reader := historyCutoverFixture(nil, nil, nil, map[string]Object{"repos/example/widgets/issues/17": {"body": strings.Repeat("x", MaxHistoryRecordBytes)}})
		if _, err := CaptureHistoryCutover(context.Background(), reader, historyCutoverTestRepository, "task.yml", []string{"repos/example/widgets/issues/17"}); err == nil || !strings.Contains(err.Error(), "1 MiB") {
			t.Fatal("capture hid an oversized provider object", err)
		}
	})
	t.Run("aggregate bytes", func(t *testing.T) {
		runs := make([]Object, 70)
		reads := map[string]Object{}
		payload := strings.Repeat("x", 500<<10)
		for index := range runs {
			run := recoveryHistoryRun(int64(index+1), 1, "Historical")
			run["payload"] = payload
			runs[index] = run
			reads[historyCutoverAttemptEndpoint(int64(index+1), 1)] = historyCutoverAttemptResponse(run, 1, run["created_at"].(string))
		}
		reader := historyCutoverFixture(runs, nil, reads, nil)
		if _, err := CaptureHistoryCutover(context.Background(), reader, historyCutoverTestRepository, "task.yml", nil); err == nil || !strings.Contains(err.Error(), "64 MiB") {
			t.Fatal("capture exceeded aggregate evidence budget", err)
		}
		exactReads := 0
		for _, call := range reader.calls {
			if strings.Contains(call, "/attempts/") {
				exactReads++
			}
		}
		if exactReads >= len(runs) {
			t.Fatal("capture retained the entire over-budget inventory before rejection")
		}
	})
	t.Run("node memory", func(t *testing.T) {
		large := make([]any, MaxHistoryRecordNodes)
		if err := (&historyEvidenceBudget{}).takeNodes(Object{"values": large}); err == nil {
			t.Fatal("single record exceeded node memory bound")
		}
		budget := &historyEvidenceBudget{}
		row := Object{"values": make([]any, 10000)}
		failed := false
		for index := 0; index < 201; index++ {
			if err := budget.takeNodes(row); err != nil {
				failed = true
				break
			}
		}
		if !failed {
			t.Fatal("aggregate node memory budget was unbounded")
		}
	})
	t.Run("compressed storage", func(t *testing.T) {
		buffer := &historyArchiveBuffer{}
		if _, err := buffer.Write(make([]byte, MaxHistoryArchiveBytes)); err != nil {
			t.Fatal(err)
		}
		if _, err := buffer.Write([]byte{'x'}); err == nil || buffer.Len() != MaxHistoryArchiveBytes {
			t.Fatal("compressed writer allocated beyond its storage budget", err)
		}
	})
}

func TestHistoryArchiveStopsDishonestDecompressionAtByteBound(t *testing.T) {
	document := smallHistoryArchiveFixture(t)
	target := document["target"]
	header := Object{"kind": "header", "data": Object{"schema_version": int64(1), "scope": historyCutoverScope, "target": target, "workflow": Object{"id": int64(9), "path": ".github/workflows/task.yml"}, "sha256": strings.Repeat("a", 64), "counts": Object{"run": int64(130), "attempt": int64(0), "artifact": int64(0), "state": int64(0)}}}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	hash := sha256.New()
	total := 0
	write := func(row Object) {
		data, err := Canonical(row)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, '\n')
		total += len(data)
		_, _ = hash.Write(data)
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	write(header)
	payload := strings.Repeat("x", 520<<10)
	for index := 0; index < 130; index++ {
		write(Object{"kind": "run", "index": int64(index), "data": Object{"payload": payload}})
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if total <= MaxHistoryEvidenceBytes {
		t.Fatal("fixture did not exceed decompression bound")
	}
	evidence := document["evidence"].(Object)
	evidence["record_count"] = int64(131)
	evidence["uncompressed_bytes"] = int64(MaxHistoryEvidenceBytes)
	evidence["uncompressed_sha256"] = hex.EncodeToString(hash.Sum(nil))
	replaceArchivePacked(t, document, compressed.Bytes(), nil)
	if _, err := ValidateHistoryCutover(document); err == nil || !strings.Contains(err.Error(), "record stream could not be completed") {
		t.Fatal("dishonest decompression did not stop at its bound", err)
	}
}

func TestLargeHistoryArchiveSurvivesNativeNoopCheckpointAndFreshProcess(t *testing.T) {
	document, _, runs := largeHistoryArchiveFixture(t)
	engine := noopEngine(t)
	addHistoryCutoverReview(t, engine, document)
	reader := readerWithCurrentNoop(600, 1)
	packet := reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)
	current := packet["workflow_runs"].([]any)[0].(Object)
	packet["total_count"] = int64(len(runs) + 1)
	packet["workflow_runs"] = append(objectRows(runs), current)
	temp := t.TempDir()
	options := historyCutoverNoopOptions(t, engine, reader, document, temp, 600, 1)
	if result, err := engine.FinishNoop(context.Background(), reader, options); err != nil || result["outcome"] != "completed" {
		t.Fatal("compressed baseline broke native no-op completion", result, err)
	}
	current["status"], current["conclusion"] = "completed", "success"
	artifact, _ := uploadFixturePackage(t, reader, engine, options.Invocation, 701)
	reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(1), "artifacts": []any{artifact}}}
	result, err := engine.Finalize(context.Background(), reader, FinalizeOptions{Invocation: options.Invocation, ArtifactID: 701, ArtifactDigest: artifact["digest"].(string), Checkpoint: filepath.Join(temp, "checkpoint")})
	if err != nil {
		t.Fatal("compressed baseline could not finalize", err)
	}
	data, err := os.ReadFile(result["checkpoint_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > MaxCheckpointBytes {
		t.Fatal("large baseline raised checkpoint limit")
	}
	checkpoint, err := DecodeValue(data)
	if err != nil || !Equal(checkpoint.(Object)["history_cutover"], document) || len(checkpoint.(Object)["settlements"].([]any)) != 1 {
		t.Fatal("native checkpoint stripped the archive or fabricated legacy settlements", err)
	}
	payload := recoveryZip(t, recoveryZipEntry{name: "settlement-chain.json", data: data})
	meta := Object{"id": int64(702), "name": CheckpointArtifactName(document["target"], 600, 1), "expired": false, "digest": "sha256:" + SHA256(payload), "workflow_run": Object{"id": int64(600), "head_sha": current["head_sha"]}}
	reader.archives[702] = payload
	reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(2), "artifacts": []any{artifact, meta}}}
	next := recoveryHistoryRun(601, 1, "Current")
	next["status"], next["conclusion"] = "in_progress", nil
	packet["total_count"] = int64(len(runs) + 2)
	packet["workflow_runs"] = append(append(objectRows(runs), current), next)
	restarted := noopEngine(t)
	addHistoryCutoverReview(t, restarted, document)
	nextTemp := t.TempDir()
	invocation := Invocation{Workflow: "task.yml", RunID: 601, Attempt: 1, RunName: "Current", RecoveryKey: "task", RunnerTemp: nextTemp, PackageRoot: filepath.Join(nextTemp, "package")}
	if result, err := restarted.Recover(context.Background(), reader, invocation); err != nil || result["outcome"] != "fresh" || result["mode"] != "preview-only" {
		t.Fatal("fresh process lost hosted compressed preview lineage", result, err)
	}
	retained, err := LoadJSON(filepath.Join(invocation.PackageRoot, "settlement-chain.json"))
	if err != nil || !Equal(retained.(Object)["history_cutover"], document) {
		t.Fatal("fresh process changed exact archive identity", err)
	}
}
