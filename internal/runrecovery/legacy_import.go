package runrecovery

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

var legacyImportReviewFields = []string{"schema_version", "chain_version", "target", "run_id", "attempt", "previous_checkpoint_sha256", "previous_settlements_sha256", "history_sha256", "report_sha256", "outcome", "dispositions", "sha256"}

// LegacyImportOptions describes one locally reviewed, append-only history
// change. Neither this contract nor its digest grants provider write authority.
type LegacyImportOptions struct {
	Report, Review, Checkpoint, History Object
	StoreDirectory                      string
}

func isLegacyRecord(record Object) bool {
	settlement, ok := record["settlement"].(Object)
	kind, _ := settlement["kind"].(string)
	return ok && strings.HasPrefix(kind, "legacy_")
}

func legacyDigest(value any) (string, error) {
	data, err := Canonical(value)
	if err != nil || len(data) > MaxCheckpointBytes {
		return "", recoveryError("legacy evidence exceeds the bounded checkpoint size")
	}
	return SHA256(data), nil
}

func rehashLegacyObject(value Object) error {
	delete(value, "sha256")
	digest, err := legacyDigest(value)
	if err != nil {
		return err
	}
	value["sha256"] = digest
	return nil
}

func validateLegacyImportReview(value any, report Object) (Object, error) {
	review, err := Exact(value, legacyImportReviewFields, "legacy import review")
	if err != nil {
		return nil, err
	}
	if !exactInt(review["schema_version"], 1) || !exactInt(review["chain_version"], legacySettlementSchemaVersion) ||
		!Equal(review["target"], report["target"]) || !Equal(review["run_id"], report["run"].(Object)["id"]) ||
		!Equal(review["attempt"], report["attempt"]) || review["report_sha256"] != report["sha256"] {
		return nil, recoveryError("legacy import review differs from its exact report, target, attempt or chain version")
	}
	for _, field := range []string{"previous_checkpoint_sha256", "previous_settlements_sha256", "history_sha256", "report_sha256", "sha256"} {
		if !IsSHA256(review[field]) {
			return nil, recoveryError("legacy import review lacks an exact %s", field)
		}
	}
	unsigned, err := cloneObject(review)
	if err != nil {
		return nil, err
	}
	if err := rehashLegacyObject(unsigned); err != nil || unsigned["sha256"] != review["sha256"] {
		return nil, recoveryError("legacy import review digest is invalid")
	}
	if err := validateLegacyDisposition(report, review["outcome"], review["dispositions"]); err != nil {
		return nil, err
	}
	return review, nil
}

func validateLegacyDisposition(report Object, outcome any, value any) error {
	rows, err := array(value, "legacy operation dispositions")
	if err != nil || len(rows) > 256 {
		return recoveryError("legacy operation dispositions must be a bounded array")
	}
	switch outcome {
	case "legacy_no_dispatch":
		if report["classification"] != "no_dispatch_proven" || len(rows) != 0 {
			return recoveryError("legacy no-dispatch import requires exhaustive skipped-mutator proof and no disposition")
		}
	case "legacy_terminal_receipt":
		if report["classification"] != "terminal_receipt_proven" || len(rows) != 0 {
			return recoveryError("legacy terminal import requires receipt-complete evidence and no disposition")
		}
	case "legacy_operation_disposition":
		if report["intent_proven"] != true || (report["classification"] != "effect_observed" && report["classification"] != "unresolved") {
			return recoveryError("legacy disposition requires authenticated complete original intent; observations alone cannot supply it")
		}
		effects, err := LegacyIntendedEffects(report)
		if err != nil || len(effects) == 0 || len(rows) != len(effects) {
			return recoveryError("legacy disposition must cover every original intended effect exactly once")
		}
		wanted := map[string]bool{}
		for _, effect := range effects {
			digest, err := legacyDigest(effect)
			if err != nil || wanted[digest] {
				return recoveryError("original legacy effect inventory is invalid or duplicated")
			}
			wanted[digest] = true
		}
		for _, raw := range rows {
			row, err := Exact(raw, []string{"effect_sha256", "decision", "evidence"}, "legacy operation disposition")
			if err != nil {
				return err
			}
			digest, ok := row["effect_sha256"].(string)
			if !ok || !wanted[digest] {
				return recoveryError("legacy disposition repeats or substitutes an original intended effect")
			}
			delete(wanted, digest)
			switch row["decision"] {
			case "accepted_observed_effect", "no_effect_proven", "accepted_unobservable_effect":
			default:
				return recoveryError("legacy disposition requires an explicit operation-specific decision")
			}
			proofs, err := array(row["evidence"], "operation-specific disposition evidence")
			if err != nil || len(proofs) == 0 || len(proofs) > 32 {
				return recoveryError("legacy disposition lacks bounded operation-specific evidence")
			}
			seen := map[string]bool{}
			for _, proof := range proofs {
				bytes, err := LoadRawFileProof(proof, "disposition")
				if err != nil || len(bytes) == 0 || seen[SHA256(bytes)] {
					return recoveryError("legacy disposition contains missing, duplicate or mismatched evidence")
				}
				seen[SHA256(bytes)] = true
			}
		}
	default:
		return recoveryError("legacy import outcome is unsupported")
	}
	return nil
}

func (e *Engine) validateLegacySettlement(record, target Object) error {
	settlement, err := Exact(record["settlement"], []string{"kind", "phase", "report", "import_review", "history_file"}, "legacy settlement")
	if err != nil {
		return err
	}
	if record["artifact"] != nil || record["context_file"] != nil || settlement["phase"] != "completed" {
		return recoveryError("legacy settlement must preserve legacy provenance without fabricating a native artifact or context")
	}
	report, err := ValidateLegacyReport(settlement["report"])
	if err != nil {
		return err
	}
	if !Equal(report["target"], target) || !Equal(report["run"], record["run"]) ||
		!Equal(report["attempt"], record["attempt"]) || !Equal(report["attempt_target"], record["attempt_target"]) {
		return recoveryError("legacy settlement differs from its original report identity")
	}
	review, err := validateLegacyImportReview(settlement["import_review"], report)
	if err != nil {
		return err
	}
	if settlement["kind"] != review["outcome"] || !e.workflows[target["workflow_file"].(string)].legacyImportReviews[review["sha256"].(string)] {
		return recoveryError("legacy import lacks this exact separately reviewed consumer policy digest")
	}
	history, err := LoadFileProof(settlement["history_file"], "legacy import history")
	if err != nil {
		return err
	}
	digest, err := legacyDigest(history)
	if err != nil || digest != review["history_sha256"] {
		return recoveryError("legacy import history differs from its reviewed complete inventory")
	}
	_, _, err = legacyHistoryInventory(history, target)
	return err
}

func legacyHistoryInventory(value any, target Object) ([]Object, map[int64]int64, error) {
	history, err := Exact(value, []string{"schema_version", "target", "runs"}, "legacy complete history")
	if err != nil || !exactInt(history["schema_version"], 1) || !Equal(history["target"], target) {
		return nil, nil, recoveryError("legacy complete history differs from its target or schema")
	}
	rows, err := array(history["runs"], "legacy complete runs")
	if err != nil || len(rows) == 0 || len(rows) > maxHistoryRuns {
		return nil, nil, recoveryError("legacy complete history is empty or oversized")
	}
	apiRows := make([]any, 0, len(rows))
	for _, raw := range rows {
		row, err := Exact(raw, []string{"run", "latest_attempt"}, "legacy history row")
		if err != nil {
			return nil, nil, err
		}
		run, err := Exact(row["run"], immutableRunFields, "legacy history run")
		if err != nil {
			return nil, nil, err
		}
		full, err := cloneObject(run)
		if err != nil {
			return nil, nil, err
		}
		full["run_attempt"] = row["latest_attempt"]
		apiRows = append(apiRows, full)
	}
	return NormalizeInventory(apiRows, target)
}

func legacyEarliest(prefix []any, runs []Object, latest map[int64]int64) (Object, error) {
	covered := map[int64]int64{}
	for _, raw := range prefix {
		record, err := object(raw, "settled predecessor")
		if err != nil {
			return nil, err
		}
		id, attempt := mustPositive(record["run_id"]), mustPositive(record["attempt"])
		if id <= 0 || attempt != covered[id]+1 {
			return nil, recoveryError("settled predecessor attempts are not contiguous")
		}
		covered[id] = attempt
	}
	var first Object
	for _, run := range runs {
		id := mustPositive(run["id"])
		if covered[id] >= latest[id] {
			continue
		}
		candidate := Object{"run": run, "attempt": covered[id] + 1}
		if first == nil || run["created_at"].(string) < first["run"].(Object)["created_at"].(string) ||
			(run["created_at"] == first["run"].(Object)["created_at"] && id < mustPositive(first["run"].(Object)["id"])) {
			first = candidate
		}
	}
	return first, nil
}

func validateLegacyFrontier(record Object, prefix []any) error {
	settlement := record["settlement"].(Object)
	review := settlement["import_review"].(Object)
	if err := validateLegacyArtifactContinuity(settlement["report"].(Object), prefix); err != nil {
		return err
	}
	digest, err := legacyDigest(prefix)
	if err != nil || digest != review["previous_settlements_sha256"] {
		return recoveryError("legacy import is not attached to its exact reviewed settlement prefix")
	}
	history, err := LoadFileProof(settlement["history_file"], "legacy import history")
	if err != nil {
		return err
	}
	runs, latest, err := legacyHistoryInventory(history, review["target"].(Object))
	if err != nil {
		return err
	}
	for _, raw := range prefix {
		prior := raw.(Object)
		id, attempt := mustPositive(prior["run_id"]), mustPositive(prior["attempt"])
		found := false
		for _, run := range runs {
			if mustPositive(run["id"]) == id && Equal(run, prior["run"]) && latest[id] >= attempt {
				found = true
				break
			}
		}
		if !found {
			return recoveryError("legacy import's historical inventory omits a settled predecessor")
		}
	}
	first, err := legacyEarliest(prefix, runs, latest)
	if err != nil || first == nil || !Equal(first["run"], record["run"]) || !Equal(first["attempt"], record["attempt"]) {
		return recoveryError("legacy import does not settle the earliest uncovered attempt in its reviewed full history")
	}
	return nil
}

func validateLegacyArtifactContinuity(report Object, prefix []any) error {
	used := map[int64]bool{}
	for _, raw := range prefix {
		record := raw.(Object)
		if artifact, ok := record["artifact"].(Object); ok {
			used[mustPositive(artifact["id"])] = true
		}
		if isLegacyRecord(record) {
			prior := record["settlement"].(Object)["report"].(Object)["evidence"].(Object)["payload"].(Object)
			for _, artifact := range prior["artifacts"].([]any) {
				used[mustPositive(artifact.(Object)["metadata"].(Object)["id"])] = true
			}
		}
	}
	payload := report["evidence"].(Object)["payload"].(Object)
	for _, raw := range payload["artifacts"].([]any) {
		if used[mustPositive(raw.(Object)["metadata"].(Object)["id"])] {
			return recoveryError("legacy artifact ID is already bound to a different settled attempt")
		}
	}
	return nil
}

// An explicit imported checkpoint may carry only the reviewed append, not an
// unauthenticated native suffix. Reconstructing its predecessor also prevents
// changing inventories or shared proof fields around an unchanged legacy row.
func validateExplicitLegacyCheckpoint(chain Object) error {
	rows, err := array(chain["settlements"], "explicit legacy settlements")
	if err != nil || len(rows) == 0 || !exactInt(chain["schema_version"], legacySettlementSchemaVersion) {
		return recoveryError("explicit legacy checkpoint must end at its reviewed imported legacy record")
	}
	last, err := object(rows[len(rows)-1], "last imported settlement")
	if err != nil || !isLegacyRecord(last) {
		return recoveryError("explicit legacy checkpoint must end at its reviewed imported legacy record")
	}
	settlement, err := object(last["settlement"], "last legacy settlement")
	if err != nil {
		return err
	}
	review, err := object(settlement["import_review"], "last legacy import review")
	if err != nil {
		return err
	}
	if frontier, ok := chain["prepared_frontier"].([]any); !ok || len(frontier) != 0 {
		return recoveryError("explicit imported checkpoint cannot carry an unreviewed prepared frontier")
	}
	previous, err := cloneObject(chain)
	if err != nil {
		return err
	}
	previous["settlements"] = previous["settlements"].([]any)[:len(rows)-1]
	inventory, err := array(previous["inventory"], "explicit imported inventory")
	if err != nil {
		return err
	}
	id, attempt := mustPositive(last["run_id"]), mustPositive(last["attempt"])
	found := false
	before := []any{}
	for _, raw := range inventory {
		item, err := object(raw, "explicit imported inventory row")
		if err != nil {
			return err
		}
		run, err := object(item["run"], "explicit imported inventory identity")
		if err != nil {
			return err
		}
		if exactInt(run["id"], id) {
			if found || attempt <= 0 || !exactInt(item["settled_attempt"], attempt) {
				return recoveryError("explicit imported checkpoint has a changed settlement frontier")
			}
			found = true
			if attempt == 1 {
				continue
			}
			item["settled_attempt"] = attempt - 1
		}
		before = append(before, item)
	}
	if !found {
		return recoveryError("explicit imported checkpoint lost its original source run")
	}
	previous["inventory"] = before
	for _, version := range []int64{settlementSchemaVersion, legacySettlementSchemaVersion} {
		previous["schema_version"] = version
		if err := rehashLegacyObject(previous); err != nil {
			return err
		}
		if previous["sha256"] == review["previous_checkpoint_sha256"] {
			return nil
		}
	}
	return recoveryError("explicit imported checkpoint does not reconstruct its exact reviewed predecessor")
}

// ObserveLegacyHistory performs complete, typed GETs. It never obtains the
// executed workflow source by guessing from the trigger's head SHA.
func (e *Engine) ObserveLegacyHistory(ctx context.Context, reader ActionsReader, targetValue any) (Object, error) {
	target, err := e.validateTarget(targetValue)
	if err != nil || reader == nil {
		return nil, recoveryError("legacy history needs an exact target and read-only Actions transport")
	}
	workflow := target["workflow_file"].(string)
	metadata, err := reader.Read(ctx, fmt.Sprintf("repos/%s/actions/workflows/%s", e.repository.FullName(), workflow))
	if err != nil {
		return nil, err
	}
	if !Equal(metadata["id"], target["workflow_id"]) || metadata["path"] != ".github/workflows/"+workflow {
		return nil, recoveryError("live workflow file and ID differ from the legacy target")
	}
	full, err := CompleteWorkflowRuns(ctx, reader, e.repository, workflow)
	if err != nil {
		return nil, err
	}
	values := make([]any, 0, len(full))
	for _, run := range full {
		values = append(values, run)
	}
	runs, latest, err := NormalizeInventory(values, target)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(runs))
	for _, run := range runs {
		rows = append(rows, Object{"run": run, "latest_attempt": latest[mustPositive(run["id"])]})
	}
	history := Object{"schema_version": int64(1), "target": target, "runs": rows}
	if _, err := legacyDigest(history); err != nil {
		return nil, err
	}
	return history, nil
}

func (e *Engine) verifyLegacyLiveAttempt(ctx context.Context, reader ActionsReader, report Object) error {
	id, attempt := mustPositive(report["run"].(Object)["id"]), mustPositive(report["attempt"])
	payload := report["evidence"].(Object)["payload"].(Object)
	packet, err := reader.Read(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d", e.repository.FullName(), id, attempt))
	if err != nil {
		return err
	}
	identity, err := NormalizeRun(packet)
	if err != nil || !Equal(identity, report["run"]) || !exactInt(packet["run_attempt"], attempt) || packet["status"] != "completed" ||
		!Equal(packet, payload["run_packet"]) {
		return recoveryError("live exact-attempt packet differs from the retained completed legacy packet")
	}
	pages, err := reader.Pages(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", e.repository.FullName(), id, attempt))
	if err != nil {
		return err
	}
	jobs, err := completePages(pages, "jobs")
	if err != nil {
		return err
	}
	signedPages, err := array(payload["jobs_packet"].(Object)["pages"], "signed jobs pages")
	if err != nil {
		return err
	}
	signedJobs, err := completePages(signedPages, "jobs")
	if err != nil {
		return err
	}
	sort.Slice(jobs, func(i, j int) bool { return mustPositive(jobs[i]["id"]) < mustPositive(jobs[j]["id"]) })
	sort.Slice(signedJobs, func(i, j int) bool { return mustPositive(signedJobs[i]["id"]) < mustPositive(signedJobs[j]["id"]) })
	if !Equal(jobs, signedJobs) {
		return recoveryError("live complete jobs differ from the signed exact-attempt packet")
	}
	artifacts := payload["artifacts"].([]any)
	reviewedIDs := map[int64]bool{}
	for _, raw := range artifacts {
		metadata := raw.(Object)["metadata"].(Object)
		reviewedIDs[mustPositive(metadata["id"])] = true
	}
	live, err := CompleteRepositoryArtifacts(ctx, reader, e.repository)
	if err != nil {
		return err
	}
	byID, byName := map[int64]Object{}, map[string]int{}
	for _, item := range live {
		byID[mustPositive(item["id"])] = item
		workflowRun, _ := item["workflow_run"].(Object)
		liveRunID := mustPositive(workflowRun["id"])
		if liveRunID <= 0 {
			return recoveryError("live legacy artifact inventory lacks a positive source-run binding")
		}
		if runID, exposed := workflowRun["run_id"]; exposed && !exactInt(runID, liveRunID) {
			return recoveryError("live legacy artifact inventory has conflicting source-run bindings")
		}
		if liveRunID != id {
			continue
		}
		liveAttempt := int64(0)
		for _, packet := range []Object{item, workflowRun} {
			if value, exposed := packet["run_attempt"]; exposed {
				boundAttempt := mustPositive(value)
				if boundAttempt <= 0 || (liveAttempt != 0 && liveAttempt != boundAttempt) {
					return recoveryError("live legacy artifact inventory has invalid or conflicting attempt bindings")
				}
				liveAttempt = boundAttempt
			}
		}
		// A positive different-attempt witness may exclude another retry's
		// artifact. Missing attempt metadata cannot justify excluding evidence.
		if liveAttempt != 0 && liveAttempt != attempt {
			continue
		}
		if !reviewedIDs[mustPositive(item["id"])] {
			return recoveryError("live legacy artifact inventory contains an unreviewed or ambiguously bound artifact")
		}
		name, _ := item["name"].(string)
		byName[name]++
	}
	var downloaded int
	for _, raw := range artifacts {
		artifact := raw.(Object)
		original := artifact["metadata"].(Object)
		metadata := byID[mustPositive(original["id"])]
		if byName[fmt.Sprint(original["name"])] > 1 {
			return recoveryError("live legacy artifact name is duplicated for the source run")
		}
		if metadata == nil || metadata["expired"] != false {
			if artifact["origin"] != "authoritative_archive" {
				return recoveryError("legacy artifact is missing or expired without an authoritative archive")
			}
		}
		if metadata != nil {
			for _, field := range []string{"id", "name", "digest"} {
				if !Equal(metadata[field], original[field]) {
					return recoveryError("live legacy artifact identity changed")
				}
			}
			liveRun, _ := metadata["workflow_run"].(Object)
			oldRun, _ := original["workflow_run"].(Object)
			if !Equal(liveRun["id"], oldRun["id"]) || !Equal(liveRun["head_sha"], oldRun["head_sha"]) {
				return recoveryError("live legacy artifact belongs to another source")
			}
			for _, packet := range []Object{metadata, liveRun} {
				if value, exposed := packet["run_attempt"]; exposed && !exactInt(value, attempt) {
					return recoveryError("live legacy artifact belongs to another exact attempt")
				}
			}
		}
		if metadata != nil && metadata["expired"] == false {
			bytes, err := reader.Archive(ctx, mustPositive(metadata["id"]))
			if err != nil {
				return err
			}
			downloaded += len(bytes)
			if downloaded > MaxArtifactBytes || "sha256:"+SHA256(bytes) != metadata["digest"] {
				return recoveryError("live legacy artifact bytes are oversized or differ from the retained digest")
			}
			retained, err := LoadRawFileProof(artifact["archive"], "legacy archive")
			if err != nil || SHA256(retained) != SHA256(bytes) {
				return recoveryError("live legacy archive differs from its exact retained bytes")
			}
		}
	}
	return nil
}

// PreviewLegacyImport creates an inert, deterministic review artifact. It does
// not enroll its digest in consumer policy or persist a settlement checkpoint.
func (e *Engine) PreviewLegacyImport(ctx context.Context, reader ActionsReader, reportValue, checkpointValue Object, outcome string, dispositions []any) (Object, error) {
	report, err := ValidateLegacyReport(reportValue)
	if err != nil {
		return nil, err
	}
	if err := validateLegacyDisposition(report, outcome, dispositions); err != nil {
		return nil, err
	}
	history, err := e.ObserveLegacyHistory(ctx, reader, report["target"])
	if err != nil {
		return nil, err
	}
	runs, latest, err := legacyHistoryInventory(history, report["target"].(Object))
	if err != nil {
		return nil, err
	}
	checkpoint, err := e.ValidateChain(checkpointValue, report["target"], runs, latest)
	if err != nil {
		return nil, err
	}
	if len(checkpoint["prepared_frontier"].([]any)) != 0 {
		return nil, recoveryError("legacy import cannot close an open prepared recovery frontier")
	}
	if err := validateLegacyArtifactContinuity(report, checkpoint["settlements"].([]any)); err != nil {
		return nil, err
	}
	first, err := legacyEarliest(checkpoint["settlements"].([]any), runs, latest)
	if err != nil || first == nil || !Equal(first["run"], report["run"]) || !Equal(first["attempt"], report["attempt"]) {
		return nil, recoveryError("legacy review must address the earliest uncovered attempt")
	}
	if err := e.verifyLegacyLiveAttempt(ctx, reader, report); err != nil {
		return nil, err
	}
	prefixSHA, _ := legacyDigest(checkpoint["settlements"])
	historySHA, _ := legacyDigest(history)
	review := Object{"schema_version": int64(1), "chain_version": legacySettlementSchemaVersion, "target": report["target"],
		"run_id": report["run"].(Object)["id"], "attempt": report["attempt"], "previous_checkpoint_sha256": checkpoint["sha256"],
		"previous_settlements_sha256": prefixSHA, "history_sha256": historySHA, "report_sha256": report["sha256"],
		"outcome": outcome, "dispositions": dispositions}
	if err := rehashLegacyObject(review); err != nil {
		return nil, err
	}
	return Object{"review": review, "history": history, "checkpoint": checkpoint}, nil
}

func (e *Engine) appendLegacyImport(checkpoint, report, review, history Object, runs []Object, latest map[int64]int64) (Object, error) {
	checked, err := e.ValidateChain(checkpoint, report["target"], runs, latest)
	if err != nil {
		return nil, err
	}
	if checked["sha256"] != review["previous_checkpoint_sha256"] {
		return nil, recoveryError("legacy import predecessor changed after review")
	}
	historySHA, err := legacyDigest(history)
	if err != nil || historySHA != review["history_sha256"] {
		return nil, recoveryError("complete live legacy history changed after review")
	}
	historyBytes, err := Canonical(history)
	if err != nil {
		return nil, err
	}
	record := Object{"run_id": review["run_id"], "attempt": review["attempt"], "run": report["run"], "attempt_target": report["attempt_target"],
		"artifact": nil, "context_file": nil, "settlement": Object{"kind": review["outcome"], "phase": "completed", "report": report,
			"import_review": review, "history_file": MakeFileProof(historyBytes)}}
	if _, err := e.ValidateSettlementRecord(record, report["target"]); err != nil {
		return nil, err
	}
	if err := validateLegacyFrontier(record, checked["settlements"].([]any)); err != nil {
		return nil, err
	}
	upgraded, err := cloneObject(checked)
	if err != nil {
		return nil, err
	}
	upgraded["schema_version"] = legacySettlementSchemaVersion
	if err := rehashLegacyObject(upgraded); err != nil {
		return nil, err
	}
	// AppendSettlement excludes the current invocation from its predecessor
	// scan. Selecting the newest run preserves the same earliest-attempt order;
	// validateLegacyFrontier separately checks the entire observed inventory.
	newest := runs[0]
	for _, run := range runs[1:] {
		if run["created_at"].(string) > newest["created_at"].(string) || (run["created_at"] == newest["created_at"] && mustPositive(run["id"]) > mustPositive(newest["id"])) {
			newest = run
		}
	}
	id := mustPositive(newest["id"])
	return e.AppendSettlement(upgraded, report["target"], runs, latest, record, id, latest[id])
}

// ImportLegacy advances only the exact reviewed earliest attempt in a private,
// durable local store. Its ActionsReader interface has no provider mutation.
func (e *Engine) ImportLegacy(ctx context.Context, reader ActionsReader, options LegacyImportOptions) (Object, error) {
	report, err := ValidateLegacyReport(options.Report)
	if err != nil {
		return nil, err
	}
	review, err := validateLegacyImportReview(options.Review, report)
	if err != nil {
		return nil, err
	}
	target, err := e.validateTarget(report["target"])
	if err != nil || !e.workflows[target["workflow_file"].(string)].legacyImportReviews[review["sha256"].(string)] {
		return nil, recoveryError("legacy import digest is not separately reviewed in the trusted consumer policy")
	}
	retainedSHA, err := legacyDigest(options.History)
	if err != nil || retainedSHA != review["history_sha256"] {
		return nil, recoveryError("legacy import requires its exact retained history preview")
	}
	fresh, err := e.ObserveLegacyHistory(ctx, reader, target)
	if err != nil {
		return nil, err
	}
	runs, latest, err := legacyHistoryInventory(fresh, target)
	if err != nil {
		return nil, err
	}
	return e.persistLegacyImport(options.StoreDirectory, options.Checkpoint, target, runs, latest, review, func(base Object) (Object, error) {
		if err := e.verifyLegacyLiveAttempt(ctx, reader, report); err != nil {
			return nil, err
		}
		return e.appendLegacyImport(base, report, review, fresh, runs, latest)
	})
}
