package runrecovery

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

const settlementSchemaVersion = int64(4)
const legacySettlementSchemaVersion = int64(5)
const maxHistoryRuns = 100_000
const maxPendingAttempts = 100_000

var (
	settlementSHA40          = regexp.MustCompile(`^[0-9a-f]{40}$`)
	settlementArtifactDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	settlementWorkflowFile   = regexp.MustCompile(`^[A-Za-z0-9._-]+\.ya?ml$`)
	settlementRepository     = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

var (
	immutableRunFields  = []string{"id", "created_at", "display_title", "event", "workflow_id", "head_branch", "head_sha"}
	targetFields        = []string{"workflow_file", "repository", "server_url", "workflow_id", "recovery_key"}
	chainFields         = []string{"schema_version", "target", "inventory", "settlements", "prepared_frontier", "prepared_terminal_proofs", "sha256"}
	inventoryFields     = []string{"run", "settled_attempt"}
	recordFields        = []string{"run_id", "attempt", "run", "attempt_target", "artifact", "context_file", "settlement"}
	artifactFields      = []string{"name", "id", "digest"}
	attemptTargetFields = []string{"recovery_key", "identity"}
)

// RecoveryError marks malformed, incomplete, or conflicting durable workflow evidence.
type RecoveryError struct{ Reason string }

func (e *RecoveryError) Error() string { return e.Reason }

func recoveryError(format string, args ...any) error {
	return &RecoveryError{Reason: fmt.Sprintf(format, args...)}
}

func positiveInteger(value any, name string) (int64, error) {
	number, err := contract.PositiveInteger(value)
	if err != nil {
		return 0, recoveryError("%s must be a positive integer", name)
	}
	return number, nil
}

func exactInt(value any, expected int64) bool {
	n, err := contract.Integer(value)
	return err == nil && n == expected
}

func nonemptyString(value any) bool {
	text, ok := value.(string)
	return ok && text != "" && !strings.ContainsAny(text, "\r\n")
}

func object(value any, name string) (Object, error) {
	result, ok := value.(map[string]any)
	if !ok {
		return nil, recoveryError("%s must be an object", name)
	}
	return result, nil
}

func array(value any, name string) ([]any, error) {
	result, ok := value.([]any)
	if !ok {
		return nil, recoveryError("%s must be an array", name)
	}
	return result, nil
}

func stringsArray(value any, name string, nonempty bool) ([]string, error) {
	values, ok := value.([]any)
	if !ok {
		if typed, yes := value.([]string); yes {
			out := append([]string{}, typed...)
			if nonempty && len(out) == 0 {
				return nil, recoveryError("%s must not be empty", name)
			}
			for _, item := range out {
				if !nonemptyString(item) {
					return nil, recoveryError("%s contains an invalid string", name)
				}
			}
			return out, nil
		}
		return nil, recoveryError("%s must be an array of strings", name)
	}
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		text, ok := value.(string)
		if !ok || !nonemptyString(text) || seen[text] {
			return nil, recoveryError("%s contains an invalid or duplicate string", name)
		}
		seen[text] = true
		out = append(out, text)
	}
	if nonempty && len(out) == 0 {
		return nil, recoveryError("%s must not be empty", name)
	}
	return out, nil
}

func cloneObject(value Object) (Object, error) {
	copyValue, err := contract.Clone(value)
	if err != nil {
		return nil, recoveryError("recovery object cannot be copied: %v", err)
	}
	return copyValue, nil
}

func ValidateTarget(value any) (Object, error) {
	target, err := Exact(value, targetFields, "settlement target")
	if err != nil {
		return nil, err
	}
	workflow, ok := target["workflow_file"].(string)
	if !ok || !settlementWorkflowFile.MatchString(workflow) {
		return nil, recoveryError("workflow filename is invalid")
	}
	repository, ok := target["repository"].(string)
	if !ok || !settlementRepository.MatchString(repository) {
		return nil, recoveryError("repository identity is invalid")
	}
	serverURL, ok := target["server_url"].(string)
	if !ok || serverURL == "" {
		return nil, recoveryError("repository server URL is missing")
	}
	parsedOrigin, err := url.Parse(serverURL)
	if err != nil || parsedOrigin.Scheme != "https" || parsedOrigin.Host == "" || parsedOrigin.User != nil ||
		parsedOrigin.Path != "" || parsedOrigin.RawQuery != "" || parsedOrigin.Fragment != "" ||
		parsedOrigin.Host != strings.ToLower(parsedOrigin.Host) || strings.Contains(parsedOrigin.Host, ":") {
		return nil, recoveryError("repository server URL must be its canonical HTTPS origin")
	}
	parsedRepository, err := contract.ParseRepository(Object{"nameWithOwner": repository, "url": serverURL + "/" + repository})
	if err != nil || parsedRepository.Owner+"/"+parsedRepository.Name != repository ||
		parsedRepository.Host != strings.TrimPrefix(serverURL, "https://") {
		return nil, recoveryError("repository server URL does not identify its exact canonical host and owner/name")
	}
	if _, err := positiveInteger(target["workflow_id"], "workflow ID"); err != nil {
		return nil, err
	}
	if target["recovery_key"] != "workflow-history-v2" {
		return nil, recoveryError("settlement target lacks its stable workflow identity")
	}
	return target, nil
}

// TargetObject makes the stable exact identity from a canonical full HTTPS repository URL.
func TargetObject(workflowFile, repositoryURL string, workflowID int64, recoveryKey string) (Object, error) {
	parsed, err := url.Parse(repositoryURL)
	if err != nil || parsed == nil {
		return nil, recoveryError("repository URL is invalid")
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) != 2 {
		return nil, recoveryError("repository URL must identify exactly one owner and repository")
	}
	nameWithOwner := parts[0] + "/" + parts[1]
	repository, err := contract.ParseRepository(Object{"nameWithOwner": nameWithOwner, "url": repositoryURL})
	if err != nil || repository.URL != repositoryURL {
		return nil, recoveryError("repository URL is not canonical")
	}
	target := Object{"workflow_file": workflowFile, "repository": repository.Owner + "/" + repository.Name, "server_url": "https://" + repository.Host, "workflow_id": workflowID, "recovery_key": recoveryKey}
	return ValidateTarget(target)
}

// NormalizeRun projects one full-history API result to its immutable identity.
func NormalizeRun(value any) (Object, error) {
	run, err := object(value, "workflow history run")
	if err != nil {
		return nil, err
	}
	identity := Object{}
	for _, field := range immutableRunFields {
		entry, exists := run[field]
		if !exists {
			return nil, recoveryError("workflow history lacks immutable run field %s", field)
		}
		identity[field] = entry
	}
	if _, err := positiveInteger(identity["id"], "workflow history run ID"); err != nil {
		return nil, err
	}
	if !nonemptyString(identity["created_at"]) || !nonemptyString(identity["event"]) || !nonemptyString(identity["display_title"]) {
		return nil, recoveryError("workflow history contains an incomplete immutable run identity")
	}
	if _, err := positiveInteger(identity["workflow_id"], "workflow history workflow ID"); err != nil {
		return nil, err
	}
	if _, ok := identity["head_branch"].(string); !ok {
		return nil, recoveryError("workflow history contains an invalid immutable branch identity")
	}
	headSHA, ok := identity["head_sha"].(string)
	if !ok || !settlementSHA40.MatchString(headSHA) {
		return nil, recoveryError("workflow history contains an invalid immutable source SHA")
	}
	if _, err := positiveInteger(run["run_attempt"], "workflow history latest attempt"); err != nil {
		return nil, err
	}
	return identity, nil
}

// NormalizeInventory validates every full-history row and returns a sorted immutable inventory.
func NormalizeInventory(rows []any, targetValue any) ([]Object, map[int64]int64, error) {
	target, err := ValidateTarget(targetValue)
	if err != nil {
		return nil, nil, err
	}
	wantedWorkflowID, _ := positiveInteger(target["workflow_id"], "target workflow ID")
	identities := make(map[int64]Object, len(rows))
	latest := make(map[int64]int64, len(rows))
	for _, row := range rows {
		identity, err := NormalizeRun(row)
		if err != nil {
			return nil, nil, err
		}
		workflowID, _ := positiveInteger(identity["workflow_id"], "workflow ID")
		if workflowID != wantedWorkflowID {
			return nil, nil, recoveryError("complete workflow history contains a foreign workflow identity")
		}
		runID, _ := positiveInteger(identity["id"], "run ID")
		if _, exists := identities[runID]; exists {
			return nil, nil, recoveryError("complete workflow history has duplicate run IDs")
		}
		run, _ := object(row, "workflow history run")
		attempt, _ := positiveInteger(run["run_attempt"], "workflow history latest attempt")
		identities[runID], latest[runID] = identity, attempt
	}
	ids := make([]int64, 0, len(identities))
	for id := range identities {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	inventory := make([]Object, 0, len(ids))
	for _, id := range ids {
		inventory = append(inventory, identities[id])
	}
	return inventory, latest, nil
}

// EmptyChain creates the canonical empty checkpoint for one workflow target.
func EmptyChain(targetValue any) (Object, error) {
	target, err := ValidateTarget(targetValue)
	if err != nil {
		return nil, err
	}
	chain := Object{"schema_version": settlementSchemaVersion, "target": target, "inventory": []any{}, "settlements": []any{}, "prepared_frontier": []any{}, "prepared_terminal_proofs": []any{}}
	unsigned, err := Canonical(chain)
	if err != nil {
		return nil, err
	}
	chain["sha256"] = SHA256(unsigned)
	return chain, nil
}

func validateAttemptTarget(value any, allowUnknown bool) (Object, error) {
	target, err := Exact(value, attemptTargetFields, "attempt target")
	if err != nil {
		return nil, err
	}
	recoveryKey, identity := target["recovery_key"], target["identity"]
	if recoveryKey != nil && !nonemptyString(recoveryKey) {
		return nil, recoveryError("attempt target recovery key is invalid")
	}
	if identity != nil {
		identityObject, ok := identity.(map[string]any)
		if !ok || len(identityObject) == 0 {
			return nil, recoveryError("attempt target identity must be a nonempty object or null")
		}
	}
	if (recoveryKey == nil) != (identity == nil) {
		return nil, recoveryError("attempt target recovery key and identity must be bound together")
	}
	if !allowUnknown && recoveryKey == nil {
		return nil, recoveryError("terminal operation lacks an exact target recovery key or identity")
	}
	return target, nil
}

func validateArtifact(value any, target Object, runID, attempt int64) (Object, error) {
	artifact, err := Exact(value, artifactFields, "settlement artifact identity")
	if err != nil {
		return nil, err
	}
	name, ok := artifact["name"].(string)
	if !ok || name == "" {
		return nil, recoveryError("settlement record has an invalid recovery artifact name")
	}
	if _, err := positiveInteger(artifact["id"], "settlement artifact ID"); err != nil {
		return nil, err
	}
	digest, ok := artifact["digest"].(string)
	if !ok || !settlementArtifactDigest.MatchString(digest) {
		return nil, recoveryError("settlement artifact digest is invalid")
	}
	if name != RecoveryArtifactName(target, runID, attempt) {
		return nil, recoveryError("settlement artifact name differs from its exact workflow attempt")
	}
	return artifact, nil
}

func recoveryArtifactPrefix(target Object) string {
	workflow, _ := target["workflow_file"].(string)
	repository, _ := target["repository"].(string)
	workflowID, _ := positiveInteger(target["workflow_id"], "workflow ID")
	key, _ := target["recovery_key"].(string)
	serverURL, _ := target["server_url"].(string)
	return "gh-steward-recovery-" + SHA256([]byte(fmt.Sprintf("%s|%s|%d|%s|%s", workflow, repository, workflowID, key, serverURL)))
}

// RecoveryArtifactName identifies exactly one workflow run attempt.
func RecoveryArtifactName(targetValue any, runID, attempt int64) string {
	target, err := ValidateTarget(targetValue)
	if err != nil || runID < 1 || attempt < 1 {
		return ""
	}
	return fmt.Sprintf("%s-run-%d-attempt-%d", recoveryArtifactPrefix(target), runID, attempt)
}

// CheckpointArtifactName identifies the sidecar containing one settled attempt.
func CheckpointArtifactName(targetValue any, runID, attempt int64) string {
	name := RecoveryArtifactName(targetValue, runID, attempt)
	if name == "" {
		return ""
	}
	return name + "-checkpoint-00"
}

func validateContextFile(proof any, target Object, runID, attempt int64, run, attemptTarget Object, plans []Object) (Object, error) {
	value, err := LoadFileProof(proof, "run context")
	if err != nil {
		return nil, err
	}
	context, err := object(value, "run context")
	if err != nil {
		return nil, err
	}
	if !exactInt(context["schema_version"], 1) || context["phase"] != "completed" ||
		context["workflow_file"] != target["workflow_file"] || context["repository"] != target["repository"] ||
		!exactInt(context["workflow_run_id"], runID) || !exactInt(context["workflow_run_attempt"], attempt) ||
		context["run_name"] != run["display_title"] || context["recovery_key"] != attemptTarget["recovery_key"] ||
		!Equal(context["attempt_target"], attemptTarget["identity"]) || !nonemptyString(context["recovery_key"]) {
		return nil, recoveryError("terminal run context differs from its exact repository, attempt or target")
	}
	contextPlans, err := array(context["plans"], "terminal context plans")
	if err != nil || len(contextPlans) != len(plans) {
		return nil, recoveryError("terminal run context does not cover its exact completed plan list")
	}
	for index, raw := range contextPlans {
		entry, err := object(raw, "terminal context plan")
		if err != nil {
			return nil, err
		}
		plan := plans[index]
		for field, want := range map[string]any{
			"name": plan["name"], "command": plan["command"], "sha256": plan["plan_sha256"],
			"journal_id": plan["journal_id"], "status": "completed",
		} {
			if !Equal(entry[field], want) {
				return nil, recoveryError("terminal run context plan identity differs from its retained proof")
			}
		}
	}
	return context, nil
}

func validateRunIdentity(value any, runID, attempt int64, target Object) (Object, error) {
	run, err := Exact(value, immutableRunFields, "settlement run identity")
	if err != nil {
		return nil, err
	}
	actualID, err := positiveInteger(run["id"], "settlement run ID")
	if err != nil || actualID != runID || !nonemptyString(run["created_at"]) || !nonemptyString(run["event"]) ||
		!nonemptyString(run["display_title"]) || !nonemptyString(run["head_branch"]) {
		return nil, recoveryError("settlement record does not bind its exact immutable workflow run")
	}
	workflowID, err := positiveInteger(run["workflow_id"], "settlement workflow ID")
	targetWorkflowID, _ := positiveInteger(target["workflow_id"], "target workflow ID")
	if err != nil || workflowID != targetWorkflowID {
		return nil, recoveryError("settlement record belongs to another workflow identity")
	}
	headSHA, ok := run["head_sha"].(string)
	if !ok || !settlementSHA40.MatchString(headSHA) {
		return nil, recoveryError("settlement record has malformed immutable source identity")
	}
	_ = attempt
	return run, nil
}

func mustPositive(value any) int64 {
	n, _ := contract.PositiveInteger(value)
	return n
}

func validatePlanProofs(raw any, target Object, allowEmpty bool) ([]Object, error) {
	entries, err := array(raw, "terminal plan proofs")
	if err != nil {
		return nil, err
	}
	if !allowEmpty && len(entries) == 0 {
		return nil, recoveryError("terminal settlement lacks a typed completed operation")
	}
	plans := make([]Object, 0, len(entries))
	seenNames := map[string]bool{}
	for _, rawPlan := range entries {
		proof, err := object(rawPlan, "terminal plan proof")
		if err != nil {
			return nil, err
		}
		name, nameOK := proof["name"].(string)
		command, commandOK := proof["command"].(string)
		if !nameOK || !nonemptyString(name) || seenNames[name] || !commandOK || !nativeCommandPattern.MatchString(command) {
			return nil, recoveryError("terminal plan proof has invalid or duplicate identity")
		}
		seenNames[name] = true
		decoded, err := ValidateTerminalPlanProof(proof)
		if err != nil {
			return nil, recoveryError("terminal native plan proof is invalid: %v", err)
		}
		repository, ok := decoded["repository"].(map[string]any)
		if !ok || !strings.EqualFold(fmt.Sprint(repository["owner"])+"/"+fmt.Sprint(repository["name"]), fmt.Sprint(target["repository"])) ||
			!strings.EqualFold(fmt.Sprint(repository["host"]), strings.TrimPrefix(fmt.Sprint(target["server_url"]), "https://")) {
			return nil, recoveryError("terminal native plan identifies another repository host or owner/name")
		}
		plans = append(plans, proof)
	}
	return plans, nil
}

func (e *Engine) validateRecoveredTerminal(value Object, target, record, policyFiles Object) error {
	evidence, err := Exact(value, []string{"kind", "phase", "proof", "policy_files"}, "recovered-terminal settlement")
	if err != nil {
		return err
	}
	if evidence["kind"] != "recovered_terminal" || evidence["phase"] != "completed" {
		return recoveryError("recovered-terminal settlement has an unsupported kind or phase")
	}
	proof, err := Exact(evidence["proof"], []string{"source", "observer"}, "recovered terminal proof")
	if err != nil {
		return err
	}
	source, err := object(proof["source"], "recovered terminal source")
	if err != nil {
		return err
	}
	if _, publication := source["publication"]; publication {
		if err := ValidateRecoveredPublicationProof(proof, target); err != nil {
			return recoveryError("recovered publication proof is invalid: %v", err)
		}
	} else {
		if _, err := ValidateRecoveredTerminalProof(proof, target); err != nil {
			return recoveryError("recovered terminal proof is invalid: %v", err)
		}
	}
	observer, err := object(proof["observer"], "recovered terminal observer")
	if err != nil {
		return err
	}
	observerPolicyFiles, err := object(observer["policy_files"], "observer policy files")
	if err != nil || !Equal(policyFiles, observerPolicyFiles) {
		return recoveryError("settlement policy files differ from the exact terminal observer")
	}
	for _, field := range []string{"run_id", "attempt", "run", "artifact", "context_file"} {
		if !Equal(source[field], record[field]) {
			return recoveryError("recovered-terminal record differs from its immutable source attempt")
		}
	}
	context, err := LoadFileProof(record["context_file"], "recovered source run context")
	if err != nil {
		return err
	}
	contextObject, err := object(context, "recovered source run context")
	if err != nil {
		return err
	}
	attemptTarget, err := validateAttemptTarget(record["attempt_target"], false)
	if err != nil {
		return err
	}
	if contextObject["recovery_key"] != attemptTarget["recovery_key"] || !Equal(contextObject["attempt_target"], attemptTarget["identity"]) {
		return recoveryError("recovered-terminal record differs from its original exact target")
	}
	if _, publication := source["publication"]; !publication {
		plans, err := array(observer["plans"], "recovered terminal observer plans")
		if err != nil {
			return err
		}
		retainedPlans := make([]Object, 0, len(plans))
		for _, raw := range plans {
			plan, err := object(raw, "recovered terminal observer plan")
			if err != nil {
				return err
			}
			retainedPlans = append(retainedPlans, plan)
		}
		observerContextValue, err := LoadFileProof(observer["context_file"], "recovered observer run context")
		if err != nil {
			return err
		}
		observerContext, err := object(observerContextValue, "recovered observer run context")
		if err != nil {
			return err
		}
		if err := e.validateRetainedPlanPolicyFiles(observerContext, retainedPlans, policyFiles); err != nil {
			return recoveryError("recovered observer policy evidence is invalid: %v", err)
		}
	}
	return nil
}

// ValidateSettlementRecord validates a typed settlement and all retained source proofs.
func (e *Engine) ValidateSettlementRecord(value any, targetValue any) (Object, error) {
	target, err := e.validateTarget(targetValue)
	if err != nil {
		return nil, err
	}
	record, err := Exact(value, recordFields, "settlement record")
	if err != nil {
		return nil, err
	}
	runID, err := positiveInteger(record["run_id"], "settlement run ID")
	if err != nil {
		return nil, err
	}
	attempt, err := positiveInteger(record["attempt"], "settlement attempt")
	if err != nil {
		return nil, err
	}
	run, err := validateRunIdentity(record["run"], runID, attempt, target)
	if err != nil {
		return nil, err
	}
	attemptTarget, err := validateAttemptTarget(record["attempt_target"], true)
	if err != nil {
		return nil, err
	}
	var artifact Object
	if record["artifact"] != nil {
		artifact, err = validateArtifact(record["artifact"], target, runID, attempt)
		if err != nil {
			return nil, err
		}
	}
	if record["context_file"] != nil {
		if _, err := LoadFileProof(record["context_file"], "run context"); err != nil {
			return nil, err
		}
	}
	settlement, err := object(record["settlement"], "settlement evidence")
	if err != nil {
		return nil, err
	}
	kind, ok := settlement["kind"].(string)
	if !ok {
		return nil, recoveryError("settlement evidence lacks a typed kind")
	}
	switch kind {
	case "legacy_no_dispatch", "legacy_terminal_receipt", "legacy_operation_disposition":
		if err := e.validateLegacySettlement(record, target); err != nil {
			return nil, err
		}
	case "terminal":
		if artifact == nil || record["context_file"] == nil || attemptTarget["recovery_key"] == nil {
			return nil, recoveryError("terminal settlement needs its exact artifact, context and attempt target")
		}
		if artifact["name"] != RecoveryArtifactName(target, runID, attempt) {
			return nil, recoveryError("terminal artifact name differs from the stable workflow/run/attempt identity")
		}
		settlement, err = Exact(settlement, []string{"kind", "phase", "plans", "publication", "policy_files"}, "terminal settlement")
		if err != nil {
			return nil, err
		}
		if settlement["phase"] != "completed" {
			return nil, recoveryError("terminal settlement must record a completed phase")
		}
		plans, err := validatePlanProofs(settlement["plans"], target, true)
		if err != nil {
			return nil, err
		}
		publication := settlement["publication"]
		if len(plans) == 0 && publication == nil {
			return nil, recoveryError("terminal settlement lacks a typed completed operation")
		}
		context, err := validateContextFile(record["context_file"], target, runID, attempt, run, attemptTarget, plans)
		if err != nil {
			return nil, err
		}
		if publication != nil {
			policyFiles, err := object(settlement["policy_files"], "terminal policy files")
			if err != nil || len(policyFiles) != 0 || len(plans) != 0 || !e.workflowAllowsPublication(target["workflow_file"].(string)) {
				return nil, recoveryError("workflow policy does not allow publication-only settlement")
			}
			if _, err := ValidatePublicationProof(publication, target, context, true); err != nil {
				return nil, recoveryError("publication proof is invalid: %v", err)
			}
		} else if context["publication"] != nil {
			return nil, recoveryError("terminal context lost its raw publication proof")
		} else {
			policyFiles, err := object(settlement["policy_files"], "terminal policy files")
			if err != nil {
				return nil, err
			}
			if err := e.validateRetainedPlanPolicyFiles(context, plans, policyFiles); err != nil {
				return nil, recoveryError("terminal policy evidence is invalid: %v", err)
			}
		}
	case "recovered_terminal":
		if artifact == nil || record["context_file"] == nil || attemptTarget["recovery_key"] == nil {
			return nil, recoveryError("recovered-terminal settlement needs its original artifact, context and attempt target")
		}
		if artifact["name"] != RecoveryArtifactName(target, runID, attempt) {
			return nil, recoveryError("recovered-terminal artifact name differs from its exact source attempt")
		}
		settlement, err = Exact(settlement, []string{"kind", "phase", "proof", "policy_files"}, "recovered-terminal settlement")
		if err != nil {
			return nil, err
		}
		policyFiles, err := object(settlement["policy_files"], "recovered-terminal policy files")
		if err != nil {
			return nil, err
		}
		if err := e.validateRecoveredTerminal(settlement, target, record, policyFiles); err != nil {
			return nil, err
		}
	case "prepared_terminal":
		if artifact == nil || record["context_file"] == nil || attemptTarget["recovery_key"] == nil {
			return nil, recoveryError("prepared-terminal settlement needs its exact artifact, context and attempt target")
		}
		if artifact["name"] != RecoveryArtifactName(target, runID, attempt) {
			return nil, recoveryError("prepared-terminal artifact name differs from its exact source attempt")
		}
		settlement, err = Exact(settlement, []string{"kind", "phase", "proof_sha256", "source_index"}, "prepared-terminal settlement")
		if err != nil {
			return nil, err
		}
		index, indexErr := contract.Integer(settlement["source_index"])
		if settlement["phase"] != "completed" || !IsSHA256(settlement["proof_sha256"]) || indexErr != nil || index < 0 || index >= MaxPreparedFrontierAttempts {
			return nil, recoveryError("prepared-terminal settlement has an invalid shared proof reference")
		}
		if _, err := LoadFileProof(record["context_file"], "prepared recovery source context"); err != nil {
			return nil, err
		}
	default:
		return nil, recoveryError("settlement kind is unsupported")
	}
	return record, nil
}

// ValidateChain validates a complete checkpoint against freshly observed full workflow history.
func (e *Engine) ValidateChain(value any, targetValue any, observed []Object, observedAttempts map[int64]int64) (Object, error) {
	target, err := e.validateTarget(targetValue)
	if err != nil {
		return nil, err
	}
	chain, err := Exact(value, chainFields, "settlement checkpoint")
	if err != nil {
		return nil, err
	}
	if (!exactInt(chain["schema_version"], settlementSchemaVersion) && !exactInt(chain["schema_version"], legacySettlementSchemaVersion)) || !Equal(chain["target"], target) {
		return nil, recoveryError("settlement checkpoint is for another repository or workflow target")
	}
	inventoryRows, err := array(chain["inventory"], "checkpoint inventory")
	if err != nil {
		return nil, err
	}
	settlements, err := array(chain["settlements"], "checkpoint settlements")
	if err != nil {
		return nil, err
	}
	preparedFrontier, err := array(chain["prepared_frontier"], "prepared source frontier")
	if err != nil || len(preparedFrontier) > MaxPreparedFrontierAttempts {
		return nil, recoveryError("prepared source frontier is malformed or exceeds its explicit bound")
	}
	preparedProofs, err := array(chain["prepared_terminal_proofs"], "prepared terminal proof inventory")
	if err != nil || len(preparedProofs) > maxPendingAttempts {
		return nil, recoveryError("prepared terminal proof inventory is malformed or exceeds its explicit bound")
	}
	unsigned := Object{"schema_version": chain["schema_version"], "target": chain["target"], "inventory": chain["inventory"], "settlements": chain["settlements"], "prepared_frontier": chain["prepared_frontier"], "prepared_terminal_proofs": chain["prepared_terminal_proofs"]}
	canonical, err := Canonical(unsigned)
	if err != nil || !IsSHA256(chain["sha256"]) || SHA256(canonical) != chain["sha256"] {
		return nil, recoveryError("settlement checkpoint digest is invalid")
	}
	fullBytes, err := Canonical(chain)
	if err != nil || len(fullBytes) > MaxCheckpointBytes {
		return nil, recoveryError("settlement checkpoint exceeds the explicit safety bound")
	}
	if len(observed) > maxHistoryRuns || len(observedAttempts) != len(observed) {
		return nil, recoveryError("complete workflow history exceeds its bound or has an inconsistent attempt inventory")
	}
	currentByID := make(map[int64]Object, len(observed))
	for _, run := range observed {
		id, err := positiveInteger(run["id"], "observed run ID")
		if err != nil || currentByID[id] != nil {
			return nil, recoveryError("observed workflow history has invalid or duplicate run IDs")
		}
		checkedRun, err := validateRunIdentity(run, id, 1, target)
		if err != nil {
			return nil, err
		}
		if _, err := positiveInteger(observedAttempts[id], "observed latest attempt"); err != nil {
			return nil, recoveryError("complete workflow history lacks an exact latest attempt")
		}
		currentByID[id] = checkedRun
	}
	coveredByID := make(map[int64]Object, len(inventoryRows))
	previousID := int64(0)
	for _, raw := range inventoryRows {
		item, err := Exact(raw, inventoryFields, "covered run inventory entry")
		if err != nil {
			return nil, err
		}
		run, err := Exact(item["run"], immutableRunFields, "covered run identity")
		if err != nil {
			return nil, err
		}
		runID, err := positiveInteger(run["id"], "covered run ID")
		if err != nil || runID <= previousID || coveredByID[runID] != nil {
			return nil, recoveryError("covered run inventory has duplicate, invalid or unordered identities")
		}
		previousID = runID
		if _, err := positiveInteger(item["settled_attempt"], "settled attempt frontier"); err != nil {
			return nil, err
		}
		if !Equal(currentByID[runID], run) {
			return nil, recoveryError("complete current history disagrees with immutable checkpoint run identity")
		}
		coveredByID[runID] = item
	}
	recordsByRun := map[int64][]int64{}
	settledPrefix := make([]any, 0, len(settlements))
	for _, raw := range settlements {
		record, err := e.ValidateSettlementRecord(raw, target)
		if err != nil {
			return nil, err
		}
		runID, _ := positiveInteger(record["run_id"], "settlement run ID")
		attempt, _ := positiveInteger(record["attempt"], "settlement attempt")
		item := coveredByID[runID]
		if item == nil || !Equal(item["run"], record["run"]) {
			return nil, recoveryError("typed settlement does not bind a covered immutable run")
		}
		priorAttempts := recordsByRun[runID]
		if int64(len(priorAttempts))+1 != attempt {
			return nil, recoveryError("settlement checkpoint has a repeated, missing or out-of-order attempt record")
		}
		if err := validateNoopFrontier(record, settledPrefix); err != nil {
			return nil, err
		}
		if isLegacyRecord(record) {
			if !exactInt(chain["schema_version"], legacySettlementSchemaVersion) {
				return nil, recoveryError("legacy settlement requires explicitly reviewed chain version 5")
			}
			if err := validateLegacyFrontier(record, settledPrefix); err != nil {
				return nil, err
			}
		}
		recordsByRun[runID] = append(priorAttempts, attempt)
		settledPrefix = append(settledPrefix, record)
	}
	for runID, item := range coveredByID {
		attempts := recordsByRun[runID]
		frontier := mustPositive(item["settled_attempt"])
		if int64(len(attempts)) != frontier {
			return nil, recoveryError("settlement checkpoint has a missing or out-of-order attempt record")
		}
		latest, exists := observedAttempts[runID]
		if !exists || frontier > latest {
			return nil, recoveryError("settlement checkpoint is ahead of complete workflow history")
		}
	}
	for runID := range observedAttempts {
		if currentByID[runID] == nil {
			return nil, recoveryError("latest attempt inventory contains a run absent from complete history")
		}
	}
	if err := e.validatePreparedFrontier(chain, target, observed, observedAttempts, coveredByID, recordsByRun); err != nil {
		return nil, err
	}
	return chain, nil
}

// ValidateCheckpointArtifact binds sidecar metadata to the exact attempt it settles.
func (e *Engine) ValidateCheckpointArtifact(chainValue, targetValue, metadataValue any) (Object, error) {
	target, err := e.validateTarget(targetValue)
	if err != nil {
		return nil, err
	}
	chain, err := object(chainValue, "settlement checkpoint")
	if err != nil {
		return nil, err
	}
	metadata, err := Exact(metadataValue, []string{"name", "id", "digest", "workflow_run_id", "workflow_run_attempt"}, "settlement checkpoint artifact metadata")
	if err != nil {
		return nil, err
	}
	runID, err := positiveInteger(metadata["workflow_run_id"], "checkpoint artifact run ID")
	if err != nil {
		return nil, err
	}
	attempt, err := positiveInteger(metadata["workflow_run_attempt"], "checkpoint artifact attempt")
	if err != nil {
		return nil, err
	}
	if metadata["name"] != CheckpointArtifactName(target, runID, attempt) || !settlementArtifactDigest.MatchString(fmt.Sprint(metadata["digest"])) {
		return nil, recoveryError("checkpoint artifact name or digest differs from its exact attempt")
	}
	if _, err := positiveInteger(metadata["id"], "checkpoint artifact ID"); err != nil {
		return nil, err
	}
	settlements, err := array(chain["settlements"], "checkpoint settlements")
	if err != nil {
		return nil, err
	}
	var match Object
	for _, raw := range settlements {
		record, err := object(raw, "checkpoint settlement")
		if err != nil {
			return nil, err
		}
		if exactInt(record["run_id"], runID) && exactInt(record["attempt"], attempt) {
			if match != nil {
				return nil, recoveryError("checkpoint repeats its own settled attempt")
			}
			match = record
		}
	}
	if match != nil {
		return match, nil
	}
	// A pending-work checkpoint is allowed to advance only its own exact
	// qualified source into the open frontier. It is not a terminal settlement:
	// the source artifact and its immutable receipt remain independently
	// verifiable, and a later actual native terminal record must close it.
	frontier, err := array(chain["prepared_frontier"], "checkpoint prepared frontier")
	if err != nil || len(frontier) == 0 || len(frontier) > MaxPreparedFrontierAttempts {
		return nil, recoveryError("checkpoint has no bounded exact prepared frontier owner")
	}
	last, err := Exact(frontier[len(frontier)-1], []string{"kind", "source"}, "pending checkpoint owner")
	if err != nil || !contains([]string{"prepared", "journaled"}, fmt.Sprint(last["kind"])) {
		return nil, recoveryError("checkpoint frontier owner has an unsupported source kind")
	}
	source, err := object(last["source"], "pending checkpoint source")
	if err != nil || !exactInt(source["run_id"], runID) || !exactInt(source["attempt"], attempt) {
		return nil, recoveryError("pending checkpoint does not name its own exact frontier attempt")
	}
	var checked Object
	if last["kind"] == "prepared" {
		checked, err = e.validatePreparedSourceRecord(source, target)
	} else {
		err = e.validateJournaledPreparedSource(source, target)
		checked = source
	}
	if err != nil {
		return nil, recoveryError("pending checkpoint owner is not positively source-qualified: %v", err)
	}
	artifact, err := Exact(checked["artifact"], artifactFields, "pending checkpoint source artifact")
	if err != nil || artifact["name"] != RecoveryArtifactName(target, runID, attempt) {
		return nil, recoveryError("pending checkpoint source artifact differs from its exact attempt")
	}
	return Object{"run": checked["run"], "pending": true}, nil
}

// SelectCheckpoint returns the unique longest valid checkpoint and rejects forks.
func (e *Engine) SelectCheckpoint(candidates []Object, target any, observed []Object, attempts map[int64]int64) (Object, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	valid := make([]Object, 0, len(candidates))
	for _, candidate := range candidates {
		checked, err := e.ValidateChain(candidate, target, observed, attempts)
		if err != nil {
			return nil, err
		}
		valid = append(valid, checked)
	}
	longest := valid[0]
	longestSettlements := longest["settlements"].([]any)
	for _, chain := range valid[1:] {
		rows := chain["settlements"].([]any)
		frontier := chain["prepared_frontier"].([]any)
		longestFrontier := longest["prepared_frontier"].([]any)
		if len(rows) > len(longestSettlements) || (len(rows) == len(longestSettlements) && len(frontier) > len(longestFrontier)) {
			longest, longestSettlements = chain, rows
		}
	}
	for _, chain := range valid {
		rows := chain["settlements"].([]any)
		frontier := chain["prepared_frontier"].([]any)
		longestFrontier := longest["prepared_frontier"].([]any)
		if len(rows) > len(longestSettlements) {
			return nil, recoveryError("checkpoint selection invariant failed")
		}
		for index := range rows {
			if !Equal(rows[index], longestSettlements[index]) {
				return nil, recoveryError("available settlement checkpoints form incompatible histories")
			}
		}
		matchesOpenPrefix := len(frontier) <= len(longestFrontier)
		if matchesOpenPrefix {
			for index := range frontier {
				if !Equal(frontier[index], longestFrontier[index]) {
					matchesOpenPrefix = false
					break
				}
			}
		}
		if !matchesOpenPrefix && !preparedFrontierRetainedByTerminalProof(frontier, longest["prepared_terminal_proofs"].([]any)) {
			return nil, recoveryError("available prepared frontiers form incompatible histories")
		}
	}
	return longest, nil
}

func preparedFrontierRetainedByTerminalProof(frontier, proofs []any) bool {
	if len(frontier) == 0 {
		return true
	}
	for _, rawProof := range proofs {
		proof, ok := rawProof.(map[string]any)
		if !ok {
			continue
		}
		sources, ok := proof["sources"].([]any)
		if !ok || len(sources) < len(frontier) {
			continue
		}
		matched := true
		for index := range frontier {
			if !Equal(frontier[index], sources[index]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// PendingAttempts returns every uncovered historical attempt in deterministic order.
func PendingAttempts(chainValue Object, observed []Object, latest map[int64]int64, currentID, currentAttempt int64) ([]Object, error) {
	if currentID < 1 || currentAttempt < 1 {
		return nil, recoveryError("current workflow attempt identity is invalid")
	}
	if len(observed) > maxHistoryRuns || len(latest) != len(observed) {
		return nil, recoveryError("complete workflow history exceeds its bound or has an inconsistent attempt inventory")
	}
	covered := map[int64]int64{}
	items, err := array(chainValue["inventory"], "checkpoint inventory")
	if err != nil {
		return nil, err
	}
	for _, raw := range items {
		item, err := object(raw, "checkpoint inventory entry")
		if err != nil {
			return nil, err
		}
		run, err := object(item["run"], "checkpoint inventory run")
		if err != nil {
			return nil, err
		}
		id, err := positiveInteger(run["id"], "checkpoint inventory run ID")
		if err != nil {
			return nil, err
		}
		frontier, err := positiveInteger(item["settled_attempt"], "checkpoint attempt frontier")
		if err != nil {
			return nil, err
		}
		covered[id] = frontier
	}
	seen := map[int64]bool{}
	result := []Object{}
	currentSeen := false
	for _, run := range observed {
		id, err := positiveInteger(run["id"], "observed run ID")
		if err != nil || seen[id] {
			return nil, recoveryError("observed workflow history has invalid or duplicate run IDs")
		}
		seen[id] = true
		if _, err := Exact(run, immutableRunFields, "observed workflow run"); err != nil {
			return nil, err
		}
		highest, ok := latest[id]
		if !ok || highest < 1 {
			return nil, recoveryError("observed history lacks an exact latest attempt")
		}
		limit := highest
		if id == currentID {
			currentSeen = true
			if highest != currentAttempt {
				return nil, recoveryError("current workflow attempt differs from complete history")
			}
			limit = currentAttempt - 1
		}
		frontier := covered[id]
		if frontier < limit {
			first := frontier + 1
			count := limit - frontier
			if count > int64(maxPendingAttempts-len(result)) {
				return nil, recoveryError("pending historical attempts exceed the explicit bound")
			}
			for number := first; ; number++ {
				result = append(result, Object{"run": run, "attempt": number})
				if number == limit {
					break
				}
			}
		}
	}
	if len(seen) != len(latest) {
		return nil, recoveryError("latest attempt inventory differs from complete workflow history")
	}
	if !currentSeen {
		return nil, recoveryError("current workflow run is absent from complete history")
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		runA, runB := a["run"].(map[string]any), b["run"].(map[string]any)
		createdA, createdB := runA["created_at"].(string), runB["created_at"].(string)
		if createdA != createdB {
			return createdA < createdB
		}
		idA, idB := mustPositive(runA["id"]), mustPositive(runB["id"])
		if idA != idB {
			return idA < idB
		}
		return mustPositive(a["attempt"]) < mustPositive(b["attempt"])
	})
	return result, nil
}

// AppendSettlement advances only the earliest unsettled prior attempt or the current terminal attempt.
func (e *Engine) AppendSettlement(chainValue Object, target any, observed []Object, latest map[int64]int64, recordValue Object, currentID, currentAttempt int64) (Object, error) {
	chain, err := e.ValidateChain(chainValue, target, observed, latest)
	if err != nil {
		return nil, err
	}
	frontier, err := array(chain["prepared_frontier"], "prepared source frontier")
	if err != nil || len(frontier) != 0 {
		return nil, recoveryError("an open prepared source frontier can close only with its actual multi-attempt terminal proof")
	}
	record, err := e.ValidateSettlementRecord(recordValue, target)
	if err != nil {
		return nil, err
	}
	runID, _ := positiveInteger(record["run_id"], "settlement run ID")
	attempt, _ := positiveInteger(record["attempt"], "settlement attempt")
	var currentRun Object
	for _, run := range observed {
		if exactInt(run["id"], runID) {
			if currentRun != nil {
				return nil, recoveryError("complete workflow history repeats a source run")
			}
			currentRun = run
		}
	}
	if currentRun == nil {
		return nil, recoveryError("settlement record source is not in complete workflow history")
	}
	if !Equal(currentRun, record["run"]) {
		return nil, recoveryError("settlement record source differs from complete workflow history")
	}
	if record["settlement"].(map[string]any)["kind"] == "recovered_terminal" {
		proof := record["settlement"].(map[string]any)["proof"].(map[string]any)
		observer := proof["observer"].(map[string]any)
		var currentInvocationRun Object
		for _, run := range observed {
			if exactInt(run["id"], currentID) {
				currentInvocationRun = run
				break
			}
		}
		if !exactInt(observer["run_id"], currentID) || !exactInt(observer["attempt"], currentAttempt) ||
			currentInvocationRun == nil || !Equal(observer["run"], currentInvocationRun) || latest[currentID] != currentAttempt {
			return nil, recoveryError("recovered-terminal observer differs from the exact current workflow attempt")
		}
	}
	pending, err := PendingAttempts(chain, observed, latest, currentID, currentAttempt)
	if err != nil {
		return nil, err
	}
	settlesCurrent := runID == currentID && attempt == currentAttempt && len(pending) == 0 && latest[currentID] == currentAttempt
	settlesEarliest := len(pending) > 0 && mustPositive(pending[0]["run"].(map[string]any)["id"]) == runID && mustPositive(pending[0]["attempt"]) == attempt
	if !settlesCurrent && !settlesEarliest {
		return nil, recoveryError("only the earliest unsettled prior attempt or the fully-preceded current attempt may advance the checkpoint")
	}
	if err := validateNoopFrontier(record, chain["settlements"].([]any)); err != nil {
		return nil, err
	}
	if isLegacyRecord(record) {
		if !exactInt(chain["schema_version"], legacySettlementSchemaVersion) {
			return nil, recoveryError("legacy settlement requires explicitly reviewed chain version 5")
		}
		if err := validateLegacyFrontier(record, chain["settlements"].([]any)); err != nil {
			return nil, err
		}
	}
	result, err := cloneObject(chain)
	if err != nil {
		return nil, err
	}
	settlements := result["settlements"].([]any)
	for _, existingRaw := range settlements {
		existing := existingRaw.(map[string]any)
		if exactInt(existing["run_id"], runID) && exactInt(existing["attempt"], attempt) {
			return nil, recoveryError("workflow run attempt is already settled and cannot be replaced")
		}
	}
	settlements = append(settlements, record)
	inventory := result["inventory"].([]any)
	var item Object
	for _, raw := range inventory {
		candidate := raw.(map[string]any)
		if Equal(candidate["run"], record["run"]) {
			item = candidate
			break
		}
	}
	if item == nil {
		item = Object{"run": record["run"], "settled_attempt": int64(0)}
		inventory = append(inventory, item)
	}
	if !Equal(item["run"], record["run"]) || mustPositive(record["attempt"]) != mustNonnegative(item["settled_attempt"])+1 {
		return nil, recoveryError("settlement record does not advance the exact next attempt")
	}
	item["settled_attempt"] = attempt
	sort.Slice(inventory, func(i, j int) bool {
		return mustPositive(inventory[i].(map[string]any)["run"].(map[string]any)["id"]) < mustPositive(inventory[j].(map[string]any)["run"].(map[string]any)["id"])
	})
	result["inventory"], result["settlements"] = inventory, settlements
	delete(result, "sha256")
	unsigned, err := Canonical(result)
	if err != nil || len(unsigned) > MaxCheckpointBytes {
		return nil, recoveryError("settlement checkpoint exceeds the explicit safety bound")
	}
	result["sha256"] = SHA256(unsigned)
	return e.ValidateChain(result, target, observed, latest)
}

func mustNonnegative(value any) int64 {
	n, err := contract.Integer(value)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

func readPathProof(value any, name string) (Object, error) {
	path, ok := value.(string)
	if !ok || !filepath.IsAbs(path) {
		return nil, recoveryError("%s path must be absolute", name)
	}
	data, err := readRegularFile(path)
	if err != nil {
		return nil, recoveryError("%s is missing, unsafe, unreadable or oversized: %v", name, err)
	}
	return MakeFileProof(data), nil
}

func validateSourceArtifact(value any, target Object, runID, attempt int64) (Object, error) {
	return validateArtifact(value, target, runID, attempt)
}

func validateSourceContext(context Object, target Object, runID, attempt int64, run Object) error {
	if !exactInt(context["schema_version"], 1) || context["workflow_file"] != target["workflow_file"] ||
		context["repository"] != target["repository"] || !exactInt(context["workflow_run_id"], runID) ||
		!exactInt(context["workflow_run_attempt"], attempt) || context["run_name"] != run["display_title"] ||
		(context["phase"] != "dispatching" && context["phase"] != "prepared") || !nonemptyString(context["recovery_key"]) {
		return recoveryError("recovery-source context differs from its exact dispatching attempt")
	}
	return nil
}

func fileProofPath(root, relative string, name string) (Object, error) {
	data, err := ReadPackageFile(root, relative)
	if err != nil {
		return nil, recoveryError("%s proof file is missing or unsafe: %v", name, err)
	}
	return MakeFileProof(data), nil
}

func requiredAbsolutePath(value any, name string) (string, error) {
	path, ok := value.(string)
	if !ok || path == "" || !filepath.IsAbs(path) {
		return "", recoveryError("%s path must be absolute", name)
	}
	return path, nil
}

func exactWithOptional(value any, required, optional []string, name string) (Object, error) {
	obj, err := object(value, name)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, field := range required {
		allowed[field] = true
		if _, ok := obj[field]; !ok {
			return nil, recoveryError("%s lacks %s", name, field)
		}
	}
	for _, field := range optional {
		allowed[field] = true
	}
	for field := range obj {
		if !allowed[field] {
			return nil, recoveryError("%s contains unsupported field %s", name, field)
		}
	}
	return obj, nil
}

func retainedPolicyFiles(root string, reader *policyReader) (Object, error) {
	files := Object{}
	for relative := range reader.used {
		proof, err := fileProofPath(root, relative, "policy sidecar")
		if err != nil {
			return nil, err
		}
		files[relative] = proof
	}
	return files, nil
}

func (e *Engine) BuildTerminalRecord(input Object) (Object, error) {
	fields, err := exactWithOptional(input,
		[]string{"target", "run_id", "attempt", "run", "attempt_target", "artifact", "context_path", "plans", "publication"},
		[]string{"publication_root", "publication_origin_run"}, "terminal settlement record input")
	if err != nil {
		return nil, err
	}
	_, hasRoot := fields["publication_root"]
	_, hasOrigin := fields["publication_origin_run"]
	if hasRoot != hasOrigin {
		return nil, recoveryError("publication root and origin run must be supplied together")
	}
	target, err := e.validateTarget(fields["target"])
	if err != nil {
		return nil, err
	}
	runID, err := positiveInteger(fields["run_id"], "terminal run ID")
	if err != nil {
		return nil, err
	}
	attempt, err := positiveInteger(fields["attempt"], "terminal attempt")
	if err != nil {
		return nil, err
	}
	run, err := Exact(fields["run"], immutableRunFields, "terminal run identity")
	if err != nil {
		return nil, err
	}
	if _, err := validateRunIdentity(run, runID, attempt, target); err != nil {
		return nil, err
	}
	attemptTarget, err := validateAttemptTarget(fields["attempt_target"], false)
	if err != nil {
		return nil, err
	}
	contextPath, err := requiredAbsolutePath(fields["context_path"], "terminal run context")
	if err != nil {
		return nil, err
	}
	contextFile, err := readPathProof(contextPath, "terminal run context")
	if err != nil {
		return nil, err
	}
	contextValue, err := LoadFileProof(contextFile, "terminal run context")
	if err != nil {
		return nil, err
	}
	context, err := object(contextValue, "terminal run context")
	if err != nil {
		return nil, err
	}
	rawPlans, err := array(fields["plans"], "terminal plan proof inputs")
	if err != nil {
		return nil, err
	}
	plans := make([]any, 0, len(rawPlans))
	for _, raw := range rawPlans {
		planInput, err := Exact(raw, []string{
			"name", "command", "plan_sha256", "journal_id", "plan_path", "journal_path", "apply_result_path",
		}, "terminal plan proof input")
		if err != nil {
			return nil, err
		}
		planPath, err := readPathProof(planInput["plan_path"], "terminal plan")
		if err != nil {
			return nil, err
		}
		journalPath, err := readPathProof(planInput["journal_path"], "terminal journal")
		if err != nil {
			return nil, err
		}
		applyPath, err := readPathProof(planInput["apply_result_path"], "terminal apply result")
		if err != nil {
			return nil, err
		}
		plans = append(plans, Object{
			"name": planInput["name"], "command": planInput["command"], "plan_sha256": planInput["plan_sha256"],
			"journal_id": planInput["journal_id"], "plan_file": planPath, "journal_file": journalPath,
			"apply_result_file": applyPath,
		})
	}
	validatedPlans, err := validatePlanProofs(plans, target, true)
	if err != nil {
		return nil, err
	}
	if _, err := validateContextFile(contextFile, target, runID, attempt, run, attemptTarget, validatedPlans); err != nil {
		return nil, err
	}
	publication := fields["publication"]
	if hasRoot {
		if publication != nil || len(plans) != 0 || !e.workflowAllowsPublication(target["workflow_file"].(string)) {
			return nil, recoveryError("workflow policy does not permit publication-only terminal proof construction")
		}
		publicationRoot, err := requiredAbsolutePath(fields["publication_root"], "publication source root")
		if err != nil {
			return nil, err
		}
		origin, err := object(fields["publication_origin_run"], "publication origin run")
		if err != nil {
			return nil, err
		}
		publication, err = BuildPublicationProof(publicationRoot, origin, context, target, true)
		if err != nil {
			return nil, recoveryError("publication proof construction failed: %v", err)
		}
	}
	policyFiles := Object{}
	if publication == nil {
		reader := &policyReader{root: filepath.Dir(contextPath), used: map[string]bool{}}
		if err := e.validatePlanPolicyFiles(context, validatedPlans, reader); err != nil {
			return nil, recoveryError("terminal plan policy evidence is invalid: %v", err)
		}
		policyFiles, err = retainedPolicyFiles(reader.root, reader)
		if err != nil {
			return nil, err
		}
	}
	record := Object{
		"run_id": runID, "attempt": attempt, "run": run, "attempt_target": attemptTarget,
		"artifact": fields["artifact"], "context_file": contextFile,
		"settlement": Object{"kind": "terminal", "phase": "completed", "plans": plans, "publication": publication, "policy_files": policyFiles},
	}
	if _, err := e.ValidateSettlementRecord(record, target); err != nil {
		return nil, err
	}
	return record, nil
}

// BuildRecoverySourceRecord retains exact immutable source bytes for a later
// observer settlement. It validates the full plan prefix and every saved step.
func (e *Engine) BuildRecoverySourceRecord(input Object) (Object, error) {
	fields, err := exactWithOptional(input,
		[]string{"target", "run_id", "attempt", "run", "artifact", "context_path", "plans"},
		[]string{"publication_origin_run"}, "recovery-source record input")
	if err != nil {
		return nil, err
	}
	target, err := e.validateTarget(fields["target"])
	if err != nil {
		return nil, err
	}
	runID, err := positiveInteger(fields["run_id"], "recovery-source run ID")
	if err != nil {
		return nil, err
	}
	attempt, err := positiveInteger(fields["attempt"], "recovery-source attempt")
	if err != nil {
		return nil, err
	}
	run, err := Exact(fields["run"], immutableRunFields, "recovery-source run identity")
	if err != nil {
		return nil, err
	}
	if _, err := validateRunIdentity(run, runID, attempt, target); err != nil {
		return nil, err
	}
	artifact, err := validateSourceArtifact(fields["artifact"], target, runID, attempt)
	if err != nil {
		return nil, err
	}
	contextPath, err := requiredAbsolutePath(fields["context_path"], "recovery-source run context")
	if err != nil {
		return nil, err
	}
	contextFile, err := readPathProof(contextPath, "recovery-source run context")
	if err != nil {
		return nil, err
	}
	contextValue, err := LoadFileProof(contextFile, "recovery-source run context")
	if err != nil {
		return nil, err
	}
	context, err := object(contextValue, "recovery-source run context")
	if err != nil {
		return nil, err
	}
	if err := validateSourceContext(context, target, runID, attempt, run); err != nil {
		return nil, err
	}
	rawPlans, err := array(fields["plans"], "recovery-source plan inputs")
	if err != nil {
		return nil, err
	}
	contextPlans, err := array(context["plans"], "recovery-source context plans")
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(contextPath)
	if context["publication"] != nil {
		if len(rawPlans) != 0 || len(contextPlans) != 0 || fields["publication_origin_run"] == nil ||
			!e.workflowAllowsPublication(target["workflow_file"].(string)) || context["phase"] != "prepared" ||
			!Equal(context["dispatch_steps"], []any{}) {
			return nil, recoveryError("publication source lacks its exact read-only verification phase and positive policy")
		}
		origin, err := object(fields["publication_origin_run"], "publication origin run")
		if err != nil {
			return nil, err
		}
		publication, err := BuildPublicationProof(root, origin, context, target, false)
		if err != nil {
			return nil, recoveryError("publication recovery source proof failed: %v", err)
		}
		return Object{
			"run_id": runID, "attempt": attempt, "run": run, "artifact": artifact,
			"context_file": contextFile, "plans": []any{}, "publication": publication, "policy_files": Object{},
		}, nil
	}
	if context["phase"] != "dispatching" || fields["publication_origin_run"] != nil || len(rawPlans) == 0 || len(rawPlans) != len(contextPlans) {
		return nil, recoveryError("native recovery source lacks its exact interrupted plan inventory")
	}
	planProofs := make([]Object, 0, len(rawPlans))
	completedDispatches := 0
	unfinishedDispatches := 0
	unfinishedWithoutDispatch := 0
	zeroOperationContinuation := false
	reader := &policyReader{root: root, used: map[string]bool{}}
	for index, raw := range rawPlans {
		planInput, err := exactWithOptional(raw, []string{"name", "command", "plan_sha256", "journal_id", "plan_path", "journal_path"}, []string{"apply_result_path"}, "recovery-source plan input")
		if err != nil {
			return nil, err
		}
		entry, err := object(contextPlans[index], "recovery-source context plan")
		if err != nil {
			return nil, err
		}
		name, nameOK := planInput["name"].(string)
		command, commandOK := planInput["command"].(string)
		planDigest, digestOK := planInput["plan_sha256"].(string)
		journalID, journalOK := planInput["journal_id"].(string)
		status, statusOK := entry["status"].(string)
		if !nameOK || !policyPlanName.MatchString(name) || !commandOK || !nativeCommandPattern.MatchString(command) ||
			!digestOK || !IsSHA256(planDigest) || !journalOK || !IsSHA256(journalID) || !statusOK ||
			!contains([]string{"prepared", "dispatching", "completed"}, status) ||
			entry["name"] != name || entry["command"] != command || entry["sha256"] != planDigest || entry["journal_id"] != journalID {
			return nil, recoveryError("recovery-source plan differs from its exact saved manifest")
		}
		planFile, err := readPathProof(planInput["plan_path"], "recovery-source plan")
		if err != nil {
			return nil, err
		}
		planValue, err := LoadFileProof(planFile, "recovery-source plan")
		if err != nil {
			return nil, err
		}
		rawPlan, err := object(planValue, "recovery-source plan")
		if err != nil {
			return nil, err
		}
		parsedPlan, err := contract.ParsePlan(rawPlan)
		if err != nil || parsedPlan.Command != command || parsedPlan.SHA256 != planDigest ||
			parsedPlan.Repository.FullName() != strings.ToLower(fmt.Sprint(target["repository"])) ||
			parsedPlan.Repository.Host != strings.TrimPrefix(target["server_url"].(string), "https://") {
			return nil, recoveryError("recovery-source plan file differs from its exact v2 identity or repository host")
		}
		if err := e.validateRecoveryPlanWithReader(context, entry, rawPlan, reader); err != nil {
			return nil, recoveryError("recovery-source policy evidence is invalid: %v", err)
		}
		zeroOperationContinuation = zeroOperationContinuation || e.completedMergeZeroOperationCleanup(context, entry, rawPlan)
		var journalFile, applyResultFile Object
		if planInput["journal_path"] != nil {
			journalPath, err := requiredAbsolutePath(planInput["journal_path"], "recovery-source journal")
			if err != nil {
				return nil, err
			}
			if _, err := os.Lstat(journalPath); err == nil {
				journalFile, err = readPathProof(journalPath, "recovery-source journal")
				if err != nil {
					return nil, err
				}
				journalValue, err := LoadFileProof(journalFile, "recovery-source journal")
				if err != nil {
					return nil, err
				}
				journal, err := object(journalValue, "recovery-source journal")
				if err != nil {
					return nil, err
				}
				dispatches, err := validatePartialJournal(journal, parsedPlan, journalID)
				if err != nil {
					return nil, err
				}
				if status == "completed" {
					completedDispatches += dispatches
					if planInput["apply_result_path"] == nil {
						return nil, recoveryError("completed parent source lost its exact apply-result path")
					}
					applyResultPath, err := requiredAbsolutePath(planInput["apply_result_path"], "completed parent apply result")
					if err != nil {
						return nil, err
					}
					applyResultFile, err = readPathProof(applyResultPath, "completed parent apply result")
					if err != nil {
						return nil, err
					}
					parentProof := Object{"name": name, "command": command, "plan_sha256": planDigest, "journal_id": journalID,
						"plan_file": planFile, "journal_file": journalFile, "apply_result_file": applyResultFile}
					if _, err := ValidateTerminalPlanProof(parentProof); err != nil {
						return nil, recoveryError("completed parent source has invalid terminal receipt bytes: %v", err)
					}
				} else if dispatches > 0 {
					unfinishedDispatches += dispatches
				} else if !e.completedMergeZeroOperationCleanup(context, entry, rawPlan) {
					return nil, recoveryError("unfinished source journal has no positive native dispatch receipt")
				}
			} else if !os.IsNotExist(err) {
				return nil, recoveryError("recovery-source journal is unsafe or unreadable: %v", err)
			} else if status != "prepared" && !e.completedMergeZeroOperationCleanup(context, entry, rawPlan) {
				return nil, recoveryError("started recovery-source plan lost its durable journal")
			} else {
				if status == "prepared" {
					unfinishedWithoutDispatch++
				}
			}
		} else if status != "prepared" && !e.completedMergeZeroOperationCleanup(context, entry, rawPlan) {
			return nil, recoveryError("started recovery-source plan lacks its durable journal path")
		} else {
			if status == "prepared" {
				unfinishedWithoutDispatch++
			}
		}
		proof := Object{
			"name": name, "command": command, "plan_sha256": planDigest, "journal_id": journalID,
			"plan_file": planFile, "journal_file": journalFile,
		}
		if applyResultFile != nil {
			proof["apply_result_file"] = applyResultFile
		}
		planProofs = append(planProofs, proof)
	}
	if unfinishedDispatches == 0 {
		if !zeroOperationContinuation || completedDispatches == 0 || unfinishedWithoutDispatch != 0 {
			if completedDispatches > 0 {
				return nil, recoveryError("completed parent receipts cannot qualify an unstarted continuation")
			}
			return nil, recoveryError("recovery-source has no durable native dispatch identity; no-dispatch remains unqualified")
		}
	}
	if unfinishedWithoutDispatch != 0 {
		return nil, recoveryError("recovery-source mixes journaled progress with an unstarted native plan")
	}
	policyFiles, err := retainedPolicyFiles(root, reader)
	if err != nil {
		return nil, err
	}
	planProofValues := make([]any, len(planProofs))
	for index, proof := range planProofs {
		planProofValues[index] = proof
	}
	return Object{
		"run_id": runID, "attempt": attempt, "run": run, "artifact": artifact,
		"context_file": contextFile, "plans": planProofValues, "policy_files": policyFiles,
	}, nil
}

func validatePartialJournal(journal Object, plan contract.Plan, journalID string) (int, error) {
	retained, err := Exact(journal, nativeJournalFields, "recovery-source journal")
	if err != nil {
		return 0, err
	}
	repository := plan.Repository.Object()
	identity, err := Exact(retained["identity"], nativeJournalIdentityFields, "recovery-source journal identity")
	if err != nil {
		return 0, err
	}
	wantIdentity := Object{"schema_version": int64(2), "repository": repository, "command": plan.Command, "plan_sha256": plan.SHA256}
	identityBytes, err := Canonical(wantIdentity)
	if err != nil || !Equal(identity, wantIdentity) || SHA256(identityBytes) != journalID {
		return 0, recoveryError("recovery-source journal differs from its exact v2 plan identity")
	}
	steps, err := array(retained["steps"], "recovery-source journal steps")
	if err != nil || len(steps) > len(plan.Operations) {
		return 0, recoveryError("recovery-source journal has an invalid primitive prefix")
	}
	dispatchIDs := map[string]bool{}
	completed := 0
	unfinished := false
	for index, raw := range steps {
		step, err := exactWithOptional(raw,
			[]string{"id", "intent", "intent_sha256", "operation_id", "status", "result", "started_at"},
			[]string{"completed_at", "acknowledgement", "observation"}, "recovery-source journal step")
		if err != nil {
			return 0, err
		}
		op := plan.Operations[index]
		intent := Object{"id": op.ID, "kind": op.Kind, "target": op.Target, "before": op.Before, "after": op.After}
		intentBytes, err := Canonical(intent)
		operationID, idOK := step["operation_id"].(string)
		if err != nil || step["id"] != op.ID || !Equal(step["intent"], intent) || step["intent_sha256"] != SHA256(intentBytes) ||
			!idOK || !nativeOperationID.MatchString(operationID) || dispatchIDs[operationID] || !nonemptyString(step["started_at"]) {
			return 0, recoveryError("recovery-source journal primitive differs from its exact ordered plan intent")
		}
		dispatchIDs[operationID] = true
		if err := nativeTimestamp(step["started_at"], "recovery-source primitive start time"); err != nil {
			return 0, err
		}
		hasAck := step["acknowledgement"] != nil
		hasObservation := step["observation"] != nil
		if hasAck && hasObservation && step["status"] != "completed" {
			return 0, recoveryError("recovery-source step has both acknowledgement and observation evidence")
		}
		switch step["status"] {
		case "completed":
			result, err := object(step["result"], "recovery-source completed primitive result")
			if err != nil || !nonemptyString(step["completed_at"]) || (!hasAck && !hasObservation) {
				return 0, recoveryError("completed recovery-source step lacks its exact result, time or receipt")
			}
			if err := nativeTimestamp(step["completed_at"], "recovery-source primitive completion time"); err != nil {
				return 0, err
			}
			if hasAck {
				if err := validateNativeAcknowledgement(step["acknowledgement"], result, intent, repository, operationID, plan.Command); err != nil {
					return 0, recoveryError("recovery-source acknowledgement is invalid: %v", err)
				}
			}
			if hasObservation {
				if err := validateNativeObservation(step["observation"], result, operationID); err != nil {
					return 0, recoveryError("recovery-source positive observation is invalid: %v", err)
				}
			}
			completed++
		case "dispatching", "unknown":
			if unfinished || step["result"] != nil || step["completed_at"] != nil || hasObservation {
				return 0, recoveryError("unfinished recovery-source step has completion or observation evidence")
			}
			unfinished = true
			if hasAck {
				if err := validatePartialAcknowledgement(step["acknowledgement"], intent, repository, operationID, plan.Command); err != nil {
					return 0, err
				}
			}
		default:
			return 0, recoveryError("recovery-source journal has an unsupported primitive status")
		}
		if unfinished && index != len(steps)-1 {
			return 0, recoveryError("recovery-source journal dispatched after an unfinished primitive")
		}
	}
	if retained["result"] != nil {
		terminal, err := Exact(retained["result"], nativeTerminalFields, "recovery-source terminal journal result")
		if err != nil || len(steps) != len(plan.Operations) || unfinished || completed != len(plan.Operations) ||
			terminal["status"] != "completed" || terminal["command"] != plan.Command || !Equal(terminal["repository"], repository) ||
			terminal["plan_sha256"] != plan.SHA256 || !Equal(terminal["receipts"], steps) {
			return 0, recoveryError("recovery-source terminal journal result differs from its exact completed plan")
		}
	}
	return len(dispatchIDs), nil
}

func validatePartialAcknowledgement(raw any, operation, repository Object, operationID, command string) error {
	ack, err := object(raw, "partial provider acknowledgement")
	if err != nil {
		return err
	}
	if ack["operation_id"] != operationID || !Equal(ack["repository"], repository) || !Equal(ack["target"], operation["target"]) {
		return recoveryError("partial acknowledgement differs from its exact durable dispatch")
	}
	result := Object{}
	for field, value := range ack {
		result[field] = value
	}
	if _, standard := ack["acknowledged"]; standard {
		result["after_verified"] = true
	} else if _, providerAck := ack["provider_ack"]; providerAck {
		result["observed_inventory_sha256"] = strings.Repeat("0", 64)
	} else {
		return recoveryError("partial acknowledgement has an unsupported native shape")
	}
	if err := validateNativeAcknowledgement(ack, result, operation, repository, operationID, command); err != nil {
		return recoveryError("partial acknowledgement is malformed: %v", err)
	}
	return nil
}
