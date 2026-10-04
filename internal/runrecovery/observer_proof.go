package runrecovery

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var (
	observerDispatchID = regexp.MustCompile(`^[0-9a-f]{32}$`)
	observerSHA40      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	observerArtifactID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

var (
	observerRunFields        = []string{"id", "created_at", "display_title", "event", "workflow_id", "head_branch", "head_sha"}
	observerAttemptFields    = []string{"run_id", "attempt", "run", "artifact", "context_file", "plans", "policy_files"}
	observerArtifactFields   = []string{"name", "id", "digest"}
	observerJournalFields    = []string{"identity", "steps", "result"}
	observerSourcePlanFields = []string{"name", "command", "plan_sha256", "journal_id", "plan_file", "journal_file"}
)

// ValidateRecoveredTerminalProof proves that a distinct observer attempt
// monotonically completed a source attempt's exact native plans. The proof
// cannot authorize replay of a primitive whose source result was uncertain.
func ValidateRecoveredTerminalProof(proof any, target Object) ([]Object, error) {
	root, err := Exact(proof, []string{"source", "observer"}, "recovered terminal proof")
	if err != nil {
		return nil, err
	}
	target, err = ValidateTarget(target)
	if err != nil {
		return nil, err
	}
	workflowFile := target["workflow_file"].(string)
	repositoryName := target["repository"].(string)

	source, err := observerAttempt(root["source"], target, "source")
	if err != nil {
		return nil, err
	}
	observer, err := observerAttempt(root["observer"], target, "observer")
	if err != nil {
		return nil, err
	}
	if source.runID == observer.runID && source.attempt == observer.attempt {
		return nil, errors.New("source and observer are the same workflow attempt")
	}
	if source.runID == observer.runID && source.attempt >= observer.attempt {
		return nil, errors.New("observer does not follow the original rerun attempt")
	}
	if source.runID != observer.runID && source.createdAt > observer.createdAt {
		return nil, errors.New("observer precedes the original workflow run")
	}
	if source.artifactID == observer.artifactID {
		return nil, errors.New("source and observer incorrectly share a workflow artifact")
	}
	if !Equal(source.policyFiles, observer.policyFiles) {
		return nil, errors.New("observer changed the exact approval or event policy files")
	}

	sourceContext, err := observerContext(source, workflowFile, repositoryName, "source")
	if err != nil {
		return nil, err
	}
	observerContext, err := observerContext(observer, workflowFile, repositoryName, "observer")
	if err != nil {
		return nil, err
	}
	if sourceContext["phase"] != "dispatching" || observerContext["phase"] != "completed" {
		return nil, errors.New("recovery does not bind an interrupted source and terminal observer")
	}
	if observerContext["recovery_key"] != sourceContext["recovery_key"] ||
		!Equal(observerContext["attempt_target"], sourceContext["attempt_target"]) ||
		!observerIntegerIs(observerContext["recovered_from_run_id"], source.runID) ||
		!observerIntegerIs(observerContext["recovered_from_attempt"], source.attempt) ||
		len(source.plans) != len(observer.plans) {
		return nil, errors.New("observer lost the exact recovery origin, target or plan inventory")
	}

	decoded := make([]Object, 0, len(source.plans))
	seenNames := make(map[string]bool, len(source.plans))
	persistedDispatches := 0
	for index, sourceRaw := range source.plans {
		original, err := Exact(sourceRaw, observerSourcePlanFields, "source native plan proof")
		if err != nil {
			return nil, err
		}
		completed, ok := observer.plans[index].(map[string]any)
		if !ok {
			return nil, errors.New("observer native plan proof is not an object")
		}
		for _, field := range []string{"name", "command", "plan_sha256", "journal_id", "plan_file"} {
			if !Equal(original[field], completed[field]) {
				return nil, errors.New("observer plan differs from the original exact plan bytes or identity")
			}
		}
		name, nameOK := original["name"].(string)
		if !nameOK || !nativeNonempty(name) || seenNames[name] {
			return nil, errors.New("recovered plan names are invalid or duplicated")
		}
		seenNames[name] = true
		sourceContextPlans := sourceContext["plans"].([]any)
		sourceContextPlan := sourceContextPlans[index].(map[string]any)
		if original["journal_file"] == nil && sourceContextPlan["status"] != "prepared" {
			return nil, errors.New("a started source plan lost its durable native journal")
		}
		plan, err := ValidateTerminalPlanProof(completed)
		if err != nil {
			return nil, fmt.Errorf("observer lacks a valid exact terminal native plan: %w", err)
		}
		planRepository, ok := plan["repository"].(map[string]any)
		if !ok || !strings.EqualFold(fmt.Sprint(planRepository["owner"])+"/"+fmt.Sprint(planRepository["name"]), repositoryName) {
			return nil, errors.New("recovered native plan identifies another repository")
		}
		identity := Object{
			"schema_version": 2,
			"repository":     nil,
			"command":        plan["command"],
			"plan_sha256":    plan["sha256"],
		}
		identity["repository"], err = nativePlanRepository(plan)
		if err != nil {
			return nil, err
		}
		identityBytes, err := Canonical(identity)
		if err != nil || SHA256(identityBytes) != original["journal_id"] {
			return nil, errors.New("original native journal identity differs from its exact plan")
		}
		dispatches, err := observerSourceProgress(original, completed, plan)
		if err != nil {
			return nil, err
		}
		persistedDispatches += dispatches
		decoded = append(decoded, plan)
	}
	if persistedDispatches == 0 {
		return nil, errors.New("interrupted source lacks any persisted native dispatch identity")
	}
	return decoded, nil
}

type observerAttemptRecord struct {
	runID, attempt int64
	createdAt      string
	artifactID     int64
	run            Object
	plans          []any
	policyFiles    Object
	contextFile    any
}

func observerAttempt(value any, target Object, name string) (observerAttemptRecord, error) {
	attempt, err := Exact(value, observerAttemptFields, name+" attempt")
	if err != nil {
		return observerAttemptRecord{}, err
	}
	workflowID, _ := proofPositiveInteger(target["workflow_id"])
	runID, err := proofPositiveInteger(attempt["run_id"])
	if err != nil {
		return observerAttemptRecord{}, fmt.Errorf("%s run ID is invalid", name)
	}
	attemptNumber, err := proofPositiveInteger(attempt["attempt"])
	if err != nil {
		return observerAttemptRecord{}, fmt.Errorf("%s attempt number is invalid", name)
	}
	run, err := Exact(attempt["run"], observerRunFields, name+" run identity")
	if err != nil {
		return observerAttemptRecord{}, err
	}
	actualRunID, err := proofPositiveInteger(run["id"])
	if err != nil || actualRunID != runID {
		return observerAttemptRecord{}, fmt.Errorf("%s run identity differs from its exact workflow attempt", name)
	}
	actualWorkflowID, err := proofPositiveInteger(run["workflow_id"])
	if err != nil || actualWorkflowID != workflowID {
		return observerAttemptRecord{}, fmt.Errorf("%s run identity differs from its exact workflow attempt", name)
	}
	_, branchOK := run["head_branch"].(string)
	headSHA, shaOK := run["head_sha"].(string)
	createdAt, createdOK := run["created_at"].(string)
	if !branchOK || !shaOK || !observerSHA40.MatchString(headSHA) || !createdOK ||
		!nativeNonempty(createdAt) || !nativeNonemptyValue(run["display_title"]) || !nativeNonemptyValue(run["event"]) {
		return observerAttemptRecord{}, fmt.Errorf("%s run identity differs from its exact workflow attempt", name)
	}
	artifact, err := Exact(attempt["artifact"], observerArtifactFields, name+" artifact")
	if err != nil {
		return observerAttemptRecord{}, err
	}
	artifactID, err := proofPositiveInteger(artifact["id"])
	if err != nil {
		return observerAttemptRecord{}, fmt.Errorf("%s artifact differs from its actual workflow attempt", name)
	}
	artifactName, nameOK := artifact["name"].(string)
	artifactDigest, digestOK := artifact["digest"].(string)
	expectedName := RecoveryArtifactName(target, runID, attemptNumber)
	if !nameOK || artifactName != expectedName || !digestOK || !observerArtifactID.MatchString(artifactDigest) {
		return observerAttemptRecord{}, fmt.Errorf("%s artifact differs from its actual workflow attempt", name)
	}
	plans, ok := attempt["plans"].([]any)
	if !ok || len(plans) == 0 {
		return observerAttemptRecord{}, fmt.Errorf("%s attempt lacks an exact native plan inventory", name)
	}
	policyFiles, err := observerPolicyFiles(attempt["policy_files"], name)
	if err != nil {
		return observerAttemptRecord{}, err
	}
	return observerAttemptRecord{
		runID: runID, attempt: attemptNumber, createdAt: createdAt, artifactID: artifactID,
		run: run, plans: plans, policyFiles: policyFiles, contextFile: attempt["context_file"],
	}, nil
}

func observerPolicyFiles(value any, name string) (Object, error) {
	files, ok := value.(map[string]any)
	if !ok || len(files) > MaxArtifactEntries {
		return nil, fmt.Errorf("%s policy files must be an object", name)
	}
	for relative, rawProof := range files {
		if !observerSafeRelativePath(relative) {
			return nil, fmt.Errorf("%s policy files contain an unsafe relative path", name)
		}
		if _, err := LoadFileProof(rawProof, name+" policy file"); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func observerSafeRelativePath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || path.Clean(value) != value {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func observerContext(attempt observerAttemptRecord, workflowFile, repository, name string) (Object, error) {
	value, err := LoadFileProof(attempt.contextFile, name+" context")
	if err != nil {
		return nil, err
	}
	context, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s context is not an object", name)
	}
	runName, _ := attempt.run["display_title"].(string)
	plans, plansOK := context["plans"].([]any)
	runID, _ := proofPositiveInteger(attempt.runID)
	if !observerIntegerIs(context["schema_version"], 1) || context["workflow_file"] != workflowFile ||
		context["repository"] != repository || !observerIntegerIs(context["workflow_run_id"], runID) ||
		!observerIntegerIs(context["workflow_run_attempt"], attempt.attempt) || context["run_name"] != runName ||
		!nativeNonemptyValue(context["recovery_key"]) || !observerNonemptyObject(context["attempt_target"]) ||
		!plansOK || len(plans) != len(attempt.plans) {
		return nil, fmt.Errorf("%s context differs from its exact attempt or target", name)
	}
	for index, rawPlan := range plans {
		contextPlan, ok := rawPlan.(map[string]any)
		retained, retainedOK := attempt.plans[index].(map[string]any)
		if !ok || !retainedOK {
			return nil, fmt.Errorf("%s context plan differs from its retained exact proof", name)
		}
		for field, retainedField := range map[string]string{
			"name": "name", "command": "command", "sha256": "plan_sha256", "journal_id": "journal_id",
		} {
			if !Equal(contextPlan[field], retained[retainedField]) {
				return nil, fmt.Errorf("%s context plan differs from its retained exact proof", name)
			}
		}
		status, statusOK := contextPlan["status"].(string)
		if !statusOK || (name == "observer" && status != "completed") ||
			(name == "source" && status != "prepared" && status != "dispatching" && status != "completed") {
			return nil, fmt.Errorf("%s context contains an unsupported plan status", name)
		}
	}
	return context, nil
}

func observerSourceProgress(sourceProof, observerProof Object, plan Object) (int, error) {
	sourceFile := sourceProof["journal_file"]
	if sourceFile == nil {
		return 0, nil
	}
	sourceValue, err := LoadFileProof(sourceFile, "source journal")
	if err != nil {
		return 0, err
	}
	source, err := Exact(sourceValue, observerJournalFields, "source journal")
	if err != nil {
		return 0, err
	}
	observerValue, err := LoadFileProof(observerProof["journal_file"], "observer journal")
	if err != nil {
		return 0, err
	}
	observer, err := Exact(observerValue, observerJournalFields, "observer journal")
	if err != nil {
		return 0, err
	}
	repository, err := nativePlanRepository(plan)
	if err != nil {
		return 0, err
	}
	identity := Object{"schema_version": 2, "repository": repository, "command": plan["command"], "plan_sha256": plan["sha256"]}
	if !observerIntegerIsFromObject(source["identity"], "schema_version", 2) || !Equal(source["identity"], identity) {
		return 0, errors.New("source journal differs from the exact original native plan")
	}
	steps, ok := source["steps"].([]any)
	planOperations, okPlan := plan["operations"].([]any)
	observerSteps, okObserver := observer["steps"].([]any)
	if !ok || !okPlan || !okObserver || len(steps) == 0 || len(steps) > len(planOperations) || len(observerSteps) != len(planOperations) {
		return 0, errors.New("started source journal lacks its exact ordered dispatch prefix")
	}
	required := []string{"id", "intent", "intent_sha256", "operation_id", "status", "result", "started_at"}
	optional := []string{"completed_at", "acknowledgement", "observation"}
	seenDispatches := make(map[string]bool, len(steps))
	for index, rawOriginal := range steps {
		original, ok := rawOriginal.(map[string]any)
		if !ok || !nativeAllowedFields(original, required, optional) {
			return 0, errors.New("source journal primitive has an unsupported shape")
		}
		operation, okOperation := planOperations[index].(map[string]any)
		completed, okCompleted := observerSteps[index].(map[string]any)
		if !okOperation || !okCompleted {
			return 0, errors.New("source or observer journal primitive is not an object")
		}
		intent := Object{"id": operation["id"], "kind": operation["kind"], "target": operation["target"], "before": operation["before"], "after": operation["after"]}
		intentBytes, err := Canonical(intent)
		if err != nil {
			return 0, err
		}
		dispatchID, idOK := original["operation_id"].(string)
		status, statusOK := original["status"].(string)
		if original["id"] != operation["id"] || !Equal(original["intent"], intent) ||
			original["intent_sha256"] != SHA256(intentBytes) || !idOK || !observerDispatchID.MatchString(dispatchID) ||
			seenDispatches[dispatchID] || !nativeNonemptyValue(original["started_at"]) || !statusOK ||
			(status != "dispatching" && status != "unknown" && status != "completed") {
			return 0, errors.New("source primitive differs from its original intent or dispatch identity")
		}
		seenDispatches[dispatchID] = true
		if status == "completed" {
			if !Equal(original, completed) {
				return 0, errors.New("completed source primitive was altered or replayed by the observer")
			}
			continue
		}
		if original["result"] != nil {
			return 0, errors.New("unfinished source primitive already claims a result")
		}
		for field, value := range original {
			if field == "status" || field == "result" || field == "completed_at" {
				continue
			}
			if !Equal(completed[field], value) {
				return 0, errors.New("observer changed a persisted source dispatch or acknowledgement")
			}
		}
	}
	if source["result"] != nil && !Equal(source["result"], observer["result"]) {
		return 0, errors.New("observer altered an already terminal source result")
	}
	return len(steps), nil
}

func proofPositiveInteger(value any) (int64, error) {
	number, err := contract.PositiveInteger(value)
	if err != nil {
		return 0, err
	}
	return number, nil
}

func observerIntegerIs(value any, expected int64) bool {
	number, err := contract.Integer(value)
	return err == nil && number == expected
}

func observerIntegerIsFromObject(value any, key string, expected int64) bool {
	object, ok := value.(map[string]any)
	return ok && observerIntegerIs(object[key], expected)
}

func observerNonemptyObject(value any) bool {
	object, ok := value.(map[string]any)
	return ok && len(object) > 0
}
