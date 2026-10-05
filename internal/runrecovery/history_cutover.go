package runrecovery

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

const historyCutoverSchemaVersion = int64(1)
const historyCutoverScope = "preview-only"

var (
	historyCutoverFields        = []string{"schema_version", "scope", "target", "workflow", "run_inventory", "attempts", "artifact_inventory", "state_reads", "sha256"}
	historyCutoverAttemptFields = []string{"run_id", "attempt", "response", "outcome", "handling"}
	historyCutoverStateFields   = []string{"endpoint", "object"}
	historyCutoverStateRoute    = regexp.MustCompile(`^repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/(?:issues|pulls)/[1-9][0-9]*$`)
)

// CaptureHistoryCutover takes a read-only, hash-sealed snapshot for an
// explicitly reviewed preview-only transition. Every historical attempt is
// retained as unknown and quarantined; none is converted to a native receipt.
func CaptureHistoryCutover(ctx context.Context, reader ActionsReader, repository contract.Repository, workflow string, stateReads []string) (Object, error) {
	if reader == nil || !workflowFilename.MatchString(workflow) {
		return nil, errors.New("history cutover requires a reader and exact workflow filename")
	}
	if _, err := contract.ParseRepository(repository.Object()); err != nil {
		return nil, fmt.Errorf("history cutover repository is invalid: %w", err)
	}
	workflowEndpoint := fmt.Sprintf("repos/%s/actions/workflows/%s", repository.FullName(), workflow)
	workflowObject, err := reader.Read(ctx, workflowEndpoint)
	if err != nil {
		return nil, fmt.Errorf("read exact workflow identity: %w", err)
	}
	workflowID, err := contract.PositiveInteger(workflowObject["id"])
	if err != nil || workflowObject["path"] != ".github/workflows/"+workflow {
		return nil, errors.New("workflow read does not identify the exact requested workflow")
	}
	target, err := TargetObject(workflow, repository.URL, workflowID, "workflow-history-v2")
	if err != nil {
		return nil, err
	}
	runRows, err := CompleteWorkflowRuns(ctx, reader, repository, workflow)
	if err != nil {
		return nil, fmt.Errorf("read complete workflow history: %w", err)
	}
	if len(runRows) > maxHistoryRuns {
		return nil, recoveryError("history cutover workflow run inventory exceeds its explicit bound")
	}
	runRows, attempts, err := historyCutoverRuns(ctx, reader, repository, runRows, target)
	if err != nil {
		return nil, err
	}
	artifactRows, err := CompleteRepositoryArtifacts(ctx, reader, repository)
	if err != nil {
		return nil, fmt.Errorf("read complete repository artifact inventory: %w", err)
	}
	for _, artifact := range artifactRows {
		name, _ := artifact["name"].(string)
		if strings.HasPrefix(name, recoveryArtifactPrefix(target)) {
			return nil, errors.New("history cutover cannot replace existing native recovery packages or checkpoints")
		}
	}
	artifactRows = sortArtifactRows(artifactRows)
	state, err := captureHistoryCutoverState(ctx, reader, repository, stateReads)
	if err != nil {
		return nil, err
	}
	baseline := Object{
		"schema_version":     historyCutoverSchemaVersion,
		"scope":              historyCutoverScope,
		"target":             target,
		"workflow":           workflowObject,
		"run_inventory":      objectRows(runRows),
		"attempts":           objectRows(attempts),
		"artifact_inventory": objectRows(artifactRows),
		"state_reads":        objectRows(state),
	}
	sealed, err := sealHistoryCutover(baseline)
	if err != nil {
		return nil, err
	}
	return sealed, nil
}

// ValidateHistoryCutover validates a raw cutover baseline without provider
// access. Its digest is an artifact identity, not user authorization.
func ValidateHistoryCutover(value any) (Object, error) {
	baseline, err := Exact(value, historyCutoverFields, "history cutover baseline")
	if err != nil {
		return nil, err
	}
	if !exactInt(baseline["schema_version"], historyCutoverSchemaVersion) || baseline["scope"] != historyCutoverScope {
		return nil, recoveryError("history cutover schema or scope is unsupported")
	}
	target, err := ValidateTarget(baseline["target"])
	if err != nil {
		return nil, err
	}
	workflow, err := object(baseline["workflow"], "history cutover workflow identity")
	if err != nil {
		return nil, err
	}
	workflowID, err := positiveInteger(workflow["id"], "history cutover workflow ID")
	if err != nil || workflowID != mustPositive(target["workflow_id"]) || workflow["path"] != ".github/workflows/"+fmt.Sprint(target["workflow_file"]) {
		return nil, recoveryError("history cutover workflow identity differs from its target")
	}
	runRows, err := objectArray(baseline["run_inventory"], "history cutover run inventory")
	if err != nil || len(runRows) > maxHistoryRuns {
		return nil, recoveryError("history cutover run inventory is malformed or exceeds its bound")
	}
	if err := validateCutoverRuns(runRows, baseline["attempts"], target); err != nil {
		return nil, err
	}
	artifactRows, err := objectArray(baseline["artifact_inventory"], "history cutover artifact inventory")
	if err != nil || len(artifactRows) > maxPendingAttempts {
		return nil, recoveryError("history cutover artifact inventory is malformed or exceeds its bound")
	}
	previousArtifactID := int64(0)
	for _, artifact := range artifactRows {
		id, err := positiveInteger(artifact["id"], "history cutover artifact ID")
		name, _ := artifact["name"].(string)
		if err != nil || id <= previousArtifactID || !nonemptyString(name) {
			return nil, recoveryError("history cutover artifact identities are invalid, duplicated or unordered")
		}
		previousArtifactID = id
		if strings.HasPrefix(name, recoveryArtifactPrefix(target)) {
			return nil, recoveryError("history cutover baseline contains native recovery lineage")
		}
	}
	state, err := objectArray(baseline["state_reads"], "history cutover state reads")
	if err != nil || len(state) > 128 {
		return nil, recoveryError("history cutover state-read inventory is malformed or exceeds its bound")
	}
	previousEndpoint := ""
	for _, read := range state {
		entry, err := Exact(read, historyCutoverStateFields, "history cutover state read")
		endpoint, ok := entry["endpoint"].(string)
		if err != nil || !ok || !historyCutoverStateRoute.MatchString(endpoint) || !strings.HasPrefix(endpoint, "repos/"+fmt.Sprint(target["repository"])+"/") || endpoint <= previousEndpoint {
			return nil, recoveryError("history cutover state reads must be sorted, unique, bounded same-repository issue or pull-request endpoints")
		}
		if _, err := object(entry["object"], "history cutover read object"); err != nil {
			return nil, err
		}
		previousEndpoint = endpoint
	}
	unsigned := Object{}
	for _, field := range historyCutoverFields[:len(historyCutoverFields)-1] {
		unsigned[field] = baseline[field]
	}
	canonical, err := Canonical(unsigned)
	if err != nil || !IsSHA256(baseline["sha256"]) || SHA256(canonical) != baseline["sha256"] {
		return nil, recoveryError("history cutover digest is invalid")
	}
	full, err := Canonical(baseline)
	if err != nil || len(full) > MaxCheckpointBytes {
		return nil, recoveryError("history cutover baseline exceeds the 8 MiB safety bound")
	}
	return baseline, nil
}

func HistoryCutoverDigest(value any) (string, error) {
	baseline, err := ValidateHistoryCutover(value)
	if err != nil {
		return "", err
	}
	return fmt.Sprint(baseline["sha256"]), nil
}

func validateHistoryCutoverPrefix(baseline Object, observed []Object, latest map[int64]int64) (map[int64]int64, error) {
	if len(observed) > maxHistoryRuns || len(latest) != len(observed) {
		return nil, recoveryError("complete current history is incomplete for the reviewed history cutover")
	}
	live := make(map[int64]Object, len(observed))
	for _, row := range observed {
		identity, err := Exact(row, immutableRunFields, "observed immutable workflow identity")
		if err != nil {
			return nil, err
		}
		id, idErr := positiveInteger(identity["id"], "observed workflow run ID")
		baselineTarget, targetErr := object(baseline["target"], "history cutover target")
		if idErr != nil || targetErr != nil || live[id] != nil || !exactInt(identity["workflow_id"], mustPositive(baselineTarget["workflow_id"])) {
			return nil, recoveryError("current workflow history contains duplicate or foreign run identities")
		}
		if _, err := validateRunIdentity(identity, id, 1, baselineTarget); err != nil {
			return nil, err
		}
		if _, err := positiveInteger(latest[id], "current workflow attempt"); err != nil {
			return nil, recoveryError("current workflow history has an invalid attempt high-water")
		}
		live[id] = identity
	}
	rows, err := objectArray(baseline["run_inventory"], "history cutover run inventory")
	if err != nil {
		return nil, err
	}
	highwaters := map[int64]int64{}
	for _, row := range rows {
		identity, err := NormalizeRun(row)
		if err != nil {
			return nil, err
		}
		id := mustPositive(identity["id"])
		highwater := mustPositive(row["run_attempt"])
		if current := live[id]; current == nil || !Equal(current, identity) || latest[id] < highwater {
			return nil, recoveryError("complete current history no longer contains the exact reviewed immutable prefix and attempt high-water")
		}
		highwaters[id] = highwater
	}
	return highwaters, nil
}

func sealHistoryCutover(value Object) (Object, error) {
	unsigned := Object{}
	for _, field := range historyCutoverFields[:len(historyCutoverFields)-1] {
		entry, exists := value[field]
		if !exists {
			return nil, recoveryError("history cutover lacks %s", field)
		}
		unsigned[field] = entry
	}
	canonical, err := Canonical(unsigned)
	if err != nil || len(canonical) > MaxCheckpointBytes {
		return nil, recoveryError("history cutover baseline exceeds the 8 MiB safety bound")
	}
	sealed := Object{}
	for key, entry := range unsigned {
		sealed[key] = entry
	}
	sealed["sha256"] = SHA256(canonical)
	return ValidateHistoryCutover(sealed)
}

func historyCutoverRuns(ctx context.Context, reader ActionsReader, repository contract.Repository, rows []Object, target Object) ([]Object, []Object, error) {
	copyRows := make([]Object, len(rows))
	for index, row := range rows {
		identity, err := NormalizeRun(row)
		if err != nil || !Equal(identity["workflow_id"], target["workflow_id"]) || row["status"] != "completed" || !nonemptyString(row["conclusion"]) {
			return nil, nil, recoveryError("history cutover requires complete terminal workflow runs with exact nonempty conclusions")
		}
		cloned, err := cloneObject(row)
		if err != nil {
			return nil, nil, err
		}
		copyRows[index] = cloned
	}
	copyRows = sortRunRows(copyRows)
	attemptCount := int64(0)
	for _, row := range copyRows {
		highest, err := positiveInteger(row["run_attempt"], "history cutover run attempt high-water")
		if err != nil || highest > int64(maxPendingAttempts)-attemptCount {
			return nil, nil, recoveryError("history cutover attempt inventory exceeds its explicit bound")
		}
		attemptCount += highest
	}
	attempts := make([]Object, 0, attemptCount)
	for _, row := range copyRows {
		runID := mustPositive(row["id"])
		identity, _ := NormalizeRun(row)
		highest := mustPositive(row["run_attempt"])
		var previousAttemptCreated time.Time
		for attempt := int64(1); attempt <= highest; attempt++ {
			endpoint := fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d", repository.FullName(), runID, attempt)
			response, err := reader.Read(ctx, endpoint)
			if err != nil {
				return nil, nil, fmt.Errorf("read exact workflow attempt %d/%d: %w", runID, attempt, err)
			}
			created, err := validateHistoryCutoverAttemptRead(response, identity, runID, attempt, previousAttemptCreated)
			if err != nil {
				return nil, nil, err
			}
			previousAttemptCreated = created
			cloned, err := cloneObject(response)
			if err != nil {
				return nil, nil, err
			}
			attempts = append(attempts, Object{
				"run_id": runID, "attempt": attempt, "response": cloned,
				"outcome": "unknown", "handling": "quarantined-never-replay",
			})
		}
	}
	return copyRows, attempts, nil
}

func captureHistoryCutoverState(ctx context.Context, reader ActionsReader, repository contract.Repository, endpoints []string) ([]Object, error) {
	if len(endpoints) > 128 {
		return nil, recoveryError("history cutover accepts at most 128 state reads")
	}
	ordered := append([]string{}, endpoints...)
	sort.Strings(ordered)
	rows := make([]Object, 0, len(ordered))
	previous := ""
	for _, endpoint := range ordered {
		if !historyCutoverStateRoute.MatchString(endpoint) || !strings.HasPrefix(endpoint, "repos/"+repository.FullName()+"/") || endpoint == previous {
			return nil, recoveryError("history cutover state reads must be unique same-repository issue or pull-request endpoints")
		}
		value, err := reader.Read(ctx, endpoint)
		if err != nil {
			return nil, fmt.Errorf("read requested issue or pull request %s: %w", endpoint, err)
		}
		cloned, err := cloneObject(value)
		if err != nil {
			return nil, err
		}
		rows = append(rows, Object{"endpoint": endpoint, "object": cloned})
		previous = endpoint
	}
	return rows, nil
}

func validateCutoverRuns(rows []Object, rawAttempts any, target Object) error {
	attempts, err := objectArray(rawAttempts, "history cutover attempt inventory")
	if err != nil || len(attempts) > maxPendingAttempts {
		return recoveryError("history cutover attempt inventory is malformed or exceeds its bound")
	}
	highwaters := map[int64]int64{}
	runs := map[int64]Object{}
	previousID := int64(0)
	var expectedAttempts int64
	for _, row := range rows {
		identity, err := NormalizeRun(row)
		if err != nil {
			return recoveryError("history cutover run inventory contains an invalid immutable identity")
		}
		id := mustPositive(identity["id"])
		highest, highErr := positiveInteger(row["run_attempt"], "history cutover attempt high-water")
		if highErr != nil || id <= previousID || !Equal(identity["workflow_id"], target["workflow_id"]) || row["status"] != "completed" || !nonemptyString(row["conclusion"]) {
			return recoveryError("history cutover run inventory is nonterminal, incomplete or unordered")
		}
		previousID = id
		if highest > int64(maxPendingAttempts)-expectedAttempts {
			return recoveryError("history cutover attempt inventory exceeds its explicit bound")
		}
		expectedAttempts += highest
		runs[id], highwaters[id] = identity, highest
	}
	if int64(len(attempts)) != expectedAttempts {
		return recoveryError("history cutover does not contain one exact read for every workflow attempt")
	}
	index := 0
	for _, row := range rows {
		id := mustPositive(row["id"])
		identity := runs[id]
		var previousAttemptCreated time.Time
		for attempt := int64(1); attempt <= highwaters[id]; attempt++ {
			entry, err := Exact(attempts[index], historyCutoverAttemptFields, "history cutover attempt")
			if err != nil || !exactInt(entry["run_id"], id) || !exactInt(entry["attempt"], attempt) || entry["outcome"] != "unknown" || entry["handling"] != "quarantined-never-replay" {
				return recoveryError("history cutover attempt is not explicitly quarantined in exact run and attempt order")
			}
			response, err := object(entry["response"], "history cutover exact attempt read")
			created, err := validateHistoryCutoverAttemptRead(response, identity, id, attempt, previousAttemptCreated)
			if err != nil {
				return err
			}
			previousAttemptCreated = created
			index++
		}
	}
	return nil
}

func validateHistoryCutoverAttemptRead(response, listedIdentity Object, runID, attempt int64, previousAttemptCreated time.Time) (time.Time, error) {
	responseIdentity, err := NormalizeRun(response)
	if err != nil {
		return time.Time{}, recoveryError("history cutover attempt %d/%d response lacks a complete immutable identity: %v", runID, attempt, err)
	}
	for _, field := range immutableRunFields {
		if field == "created_at" {
			continue
		}
		if !Equal(responseIdentity[field], listedIdentity[field]) {
			return time.Time{}, recoveryError("history cutover attempt %d/%d immutable field %s differs from the complete run inventory", runID, attempt, field)
		}
	}
	if !exactInt(response["run_attempt"], attempt) {
		return time.Time{}, recoveryError("history cutover attempt %d/%d exact response has a different attempt number", runID, attempt)
	}
	if response["status"] != "completed" {
		return time.Time{}, recoveryError("history cutover attempt %d/%d is active or lacks terminal status", runID, attempt)
	}
	if !nonemptyString(response["conclusion"]) {
		return time.Time{}, recoveryError("history cutover attempt %d/%d lacks a terminal conclusion", runID, attempt)
	}
	listedCreated, err := time.Parse(time.RFC3339Nano, fmt.Sprint(listedIdentity["created_at"]))
	if err != nil {
		return time.Time{}, recoveryError("history cutover run %d has a malformed original creation timestamp", runID)
	}
	attemptCreated, err := time.Parse(time.RFC3339Nano, fmt.Sprint(responseIdentity["created_at"]))
	if err != nil {
		return time.Time{}, recoveryError("history cutover attempt %d/%d has a malformed creation timestamp", runID, attempt)
	}
	if attemptCreated.Before(listedCreated) || (!previousAttemptCreated.IsZero() && attemptCreated.Before(previousAttemptCreated)) {
		return time.Time{}, recoveryError("history cutover attempt %d/%d creation timestamp precedes the run or prior attempt", runID, attempt)
	}
	return attemptCreated, nil
}

func sortRunRows(rows []Object) []Object {
	sort.Slice(rows, func(i, j int) bool { return mustPositive(rows[i]["id"]) < mustPositive(rows[j]["id"]) })
	return rows
}

func sortArtifactRows(rows []Object) []Object {
	sort.Slice(rows, func(i, j int) bool { return mustPositive(rows[i]["id"]) < mustPositive(rows[j]["id"]) })
	return rows
}

func rejectExistingTargetRecoveryArtifacts(target Object, artifacts []Object) error {
	prefix := recoveryArtifactPrefix(target)
	for _, artifact := range artifacts {
		name, _ := artifact["name"].(string)
		if strings.HasPrefix(name, prefix) {
			return recoveryError("history cutover cannot replace existing native recovery packages or checkpoints")
		}
	}
	return nil
}

// validateHistoryCutoverNativeArtifacts permits only artifacts created after
// the exact baseline high-water. The artifact inventory inside the baseline is
// point-in-time evidence and is deliberately not re-required to remain equal.
func validateHistoryCutoverNativeArtifacts(target, baseline Object, observed []Object, latest map[int64]int64, artifacts []Object) error {
	highwaters, err := validateHistoryCutoverPrefix(baseline, observed, latest)
	if err != nil {
		return err
	}
	prefix := recoveryArtifactPrefix(target)
	namePattern := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `-run-([1-9][0-9]*)-attempt-([1-9][0-9]*)(?:-checkpoint-00|-handoff-00)?$`)
	for _, artifact := range artifacts {
		name, _ := artifact["name"].(string)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		match := namePattern.FindStringSubmatch(name)
		if match == nil {
			return recoveryError("history cutover found an unsupported native recovery artifact")
		}
		runID, runErr := strconv.ParseInt(match[1], 10, 64)
		attempt, attemptErr := strconv.ParseInt(match[2], 10, 64)
		if runErr != nil || attemptErr != nil || latest[runID] < attempt || attempt <= highwaters[runID] {
			return recoveryError("history cutover found native recovery evidence at or below its quarantined attempt boundary")
		}
	}
	return nil
}

func objectRows(rows []Object) []any {
	result := make([]any, len(rows))
	for index := range rows {
		result[index] = rows[index]
	}
	return result
}

func objectArray(value any, name string) ([]Object, error) {
	rows, err := array(value, name)
	if err != nil {
		return nil, err
	}
	objects := make([]Object, len(rows))
	for index, raw := range rows {
		objects[index], err = object(raw, name+" row")
		if err != nil {
			return nil, err
		}
	}
	return objects, nil
}
