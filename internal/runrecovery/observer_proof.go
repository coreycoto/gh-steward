package runrecovery

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/workflow"
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
	sourceOriginRunID, sourceOriginAttempt, originErr := contextPlanOrigin(sourceContext, true)
	if originErr != nil {
		return nil, errors.New("source context does not preserve its exact native plan origin")
	}
	_, observerOriginRunPresent := observerContext["plan_origin_run_id"]
	_, observerOriginAttemptPresent := observerContext["plan_origin_attempt"]
	var observerOriginRunID, observerOriginAttempt int64
	if observerOriginRunPresent || observerOriginAttemptPresent {
		if observerOriginRunPresent != observerOriginAttemptPresent {
			return nil, errors.New("observer context has an incomplete native plan origin")
		}
		observerOriginRunID, observerOriginAttempt, originErr = contextPlanOrigin(observerContext, true)
		if originErr != nil {
			return nil, errors.New("observer context has an invalid native plan origin")
		}
	} else {
		if sourceContext["recovered_from_run_id"] != nil || sourceContext["recovered_from_attempt"] != nil {
			return nil, errors.New("multi-attempt observer omits its original native plan origin")
		}
		observerOriginRunID, observerOriginAttempt = sourceOriginRunID, sourceOriginAttempt
	}
	if observerOriginRunID != sourceOriginRunID || observerOriginAttempt != sourceOriginAttempt {
		return nil, errors.New("observer changed the exact native plan origin")
	}

	decoded := make([]Object, 0, len(source.plans))
	seenNames := make(map[string]bool, len(source.plans))
	persistedDispatches := 0
	for index, sourceRaw := range source.plans {
		original, err := exactWithOptional(sourceRaw, observerSourcePlanFields, []string{"apply_result_file"}, "source native plan proof")
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
		if sourceContextPlan["status"] == "prepared" {
			return nil, errors.New("recovered source contains an unstarted native plan")
		}
		zeroOperationCandidate := len(source.plans) == 2 && index == 1 && name == "branch-cleanup" &&
			original["command"] == workflow.BranchCleanupCommand && sourceContextPlan["status"] == "dispatching" &&
			sourceContextPlans[0].(map[string]any)["status"] == "completed"
		if original["journal_file"] == nil && sourceContextPlan["status"] != "prepared" && !zeroOperationCandidate {
			return nil, errors.New("a started source plan lost its durable native journal")
		}
		if sourceContextPlan["status"] == "completed" {
			if original["journal_file"] == nil || original["apply_result_file"] == nil {
				return nil, errors.New("completed source plan lost its exact terminal receipts")
			}
			if !Equal(original["journal_file"], completed["journal_file"]) ||
				!Equal(original["apply_result_file"], completed["apply_result_file"]) {
				return nil, errors.New("observer changed completed source parent receipt bytes")
			}
			if _, err := ValidateTerminalPlanProof(original); err != nil {
				return nil, fmt.Errorf("completed source parent receipt is invalid: %w", err)
			}
		} else if original["apply_result_file"] != nil {
			return nil, errors.New("dispatching source plan cannot claim a terminal apply result")
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
		if zeroOperationCandidate && len(plan["operations"].([]any)) == 0 {
			if err := validateRecoveredZeroOperationCleanup(source, sourceContext, index, original, completed, plan, decoded[index-1]); err != nil {
				return nil, fmt.Errorf("source is not the exact completed-merge all-absent cleanup continuation: %w", err)
			}
		} else if zeroOperationCandidate {
			if original["journal_file"] == nil {
				return nil, errors.New("nonzero cleanup source plan lost its durable native journal")
			}
			dispatches, err := observerSourceProgress(original, completed, plan)
			if err != nil {
				return nil, err
			}
			persistedDispatches += dispatches
		} else {
			dispatches, err := observerSourceProgress(original, completed, plan)
			if err != nil {
				return nil, err
			}
			persistedDispatches += dispatches
		}
		decoded = append(decoded, plan)
	}
	if persistedDispatches == 0 {
		return nil, errors.New("interrupted source lacks any persisted native dispatch identity")
	}
	return decoded, nil
}

func validateRecoveredZeroOperationCleanup(source observerAttemptRecord, sourceContext Object, childIndex int, childSourceProof, childObserverProof, childPlanValue, parentPlanValue Object) error {
	if childIndex != 1 || len(source.plans) != 2 || childSourceProof["name"] != "branch-cleanup" ||
		childSourceProof["command"] != workflow.BranchCleanupCommand || childSourceProof["apply_result_file"] != nil {
		return errors.New("cleanup source is not the sole dispatching child without a source result")
	}
	contextPlans, err := array(sourceContext["plans"], "zero-operation source plans")
	if err != nil || len(contextPlans) != 2 {
		return errors.New("cleanup source context does not contain exactly its completed parent and child")
	}
	parentContext, err := object(contextPlans[0], "zero-operation parent context")
	if err != nil || parentContext["name"] != "merge" || parentContext["command"] != workflow.MergeCommand || parentContext["status"] != "completed" {
		return errors.New("zero-operation cleanup source lacks its exact completed merge parent")
	}
	childContext, err := object(contextPlans[1], "zero-operation child context")
	if err != nil || childContext["status"] != "dispatching" || childContext["sha256"] != childSourceProof["plan_sha256"] ||
		childContext["journal_id"] != childSourceProof["journal_id"] {
		return errors.New("zero-operation cleanup source context differs from its exact plan proof")
	}
	parentProof, err := object(source.plans[0], "completed source merge proof")
	if err != nil || parentProof["name"] != "merge" || parentProof["command"] != workflow.MergeCommand {
		return errors.New("zero-operation cleanup source does not retain its exact merge proof")
	}
	parentPlan, err := contract.ParsePlan(parentPlanValue)
	if err != nil || parentPlan.Command != workflow.MergeCommand {
		return errors.New("zero-operation cleanup parent is not the exact native merge plan")
	}
	mergeAdapter := workflow.MergeAdapter{}
	mergeOperations, err := mergeAdapter.Operations(parentPlan)
	if err != nil || len(mergeOperations) != 1 || !Equal(operationObjects(mergeOperations), parentPlanValue["operations"]) {
		return errors.New("zero-operation cleanup parent does not contain its exact merge operation")
	}
	parentJournalValue, err := LoadFileProof(parentProof["journal_file"], "completed merge parent journal")
	if err != nil {
		return err
	}
	parentJournal, err := Exact(parentJournalValue, observerJournalFields, "completed merge parent journal")
	if err != nil {
		return err
	}
	parentSteps, err := array(parentJournal["steps"], "completed merge parent dispatches")
	if err != nil || len(parentSteps) != 1 {
		return errors.New("zero-operation cleanup parent lacks exactly one acknowledged merge")
	}
	parentStep, err := object(parentSteps[0], "completed merge parent receipt")
	if err != nil || parentStep["status"] != "completed" {
		return errors.New("zero-operation cleanup parent merge is not positively complete")
	}
	mergeReceipt, err := object(parentStep["result"], "completed merge provider receipt")
	if err != nil || mergeAdapter.ValidateReceipt(parentPlan, mergeOperations[0], mergeReceipt) != nil {
		return errors.New("zero-operation cleanup parent receipt does not prove its exact merge")
	}
	mergeACK, err := object(mergeReceipt["provider_ack"], "completed merge provider acknowledgement")
	if err != nil {
		return err
	}
	mergeCommit, err := contract.Nonempty(mergeACK, "sha")
	if err != nil {
		return errors.New("zero-operation cleanup parent receipt lacks its exact merge commit")
	}
	settings, err := recoveredParentRepositorySettings(source.policyFiles)
	if err != nil {
		return err
	}
	settingsRepositoryValue, err := object(settings["repository"], "completed merge repository settings identity")
	if err != nil {
		return err
	}
	settingsRepository, err := contract.ParseRepository(settingsRepositoryValue)
	if err != nil || settingsRepository != parentPlan.Repository || !strings.EqualFold(fmt.Sprint(settings["owner_login"]), settingsRepository.Owner) ||
		!strings.EqualFold(fmt.Sprint(settings["name"]), settingsRepository.Name) {
		return errors.New("zero-operation cleanup settings identify another repository")
	}
	parentInventory, err := contract.ObjectAt(parentPlan.Data, "inventory")
	if err != nil {
		return err
	}
	parentRepositoryInventory, err := contract.ObjectAt(parentInventory, "repository")
	if err != nil || parentRepositoryInventory["id"] != settings["repository_node_id"] || parentRepositoryInventory["defaultBranch"] != settings["default_branch"] {
		return errors.New("zero-operation cleanup parent repository settings differ from its merge inventory")
	}
	parentPR, err := contract.ObjectAt(parentInventory, "pull_request")
	if err != nil {
		return err
	}
	childPlan, err := contract.ParsePlan(childPlanValue)
	if err != nil || childPlan.Command != workflow.BranchCleanupCommand || childPlan.Repository != parentPlan.Repository || len(childPlan.Operations) != 0 {
		return errors.New("cleanup child is not an exact zero-operation plan for its merge repository")
	}
	cleanupOperations, err := (workflow.BranchCleanup{}).Operations(childPlan)
	if err != nil || len(cleanupOperations) != 0 || !Equal(operationObjects(cleanupOperations), childPlanValue["operations"]) {
		return errors.New("cleanup child does not recompute to an exact all-absent zero-operation plan")
	}
	cleanupInventory, err := contract.ObjectAt(childPlan.Data, "inventory")
	if err != nil || cleanupInventory["repository_node_id"] != settings["repository_node_id"] || cleanupInventory["default_branch"] != settings["default_branch"] {
		return errors.New("cleanup child repository incarnation or default branch differs from its merge parent")
	}
	branchEvidence, err := contract.ObjectAt(cleanupInventory, "branch_evidence")
	if err != nil || branchEvidence["delete_branch_on_merge"] != settings["delete_branch_on_merge"] {
		return errors.New("cleanup child retention evidence differs from its merge parent settings")
	}
	cleanupRows, err := contract.Objects(cleanupInventory, "branches")
	if err != nil || len(cleanupRows) != 1 {
		return errors.New("cleanup child does not retain exactly its merged pull request")
	}
	cleanupPR, err := contract.ObjectAt(cleanupRows[0], "pull_request")
	cleanupNumberText := fmt.Sprint(cleanupPR["number"])
	cleanupNumber, numberErr := strconv.ParseInt(cleanupNumberText, 10, 64)
	if err != nil || numberErr != nil || cleanupNumber < 1 || cleanupPR["id"] != parentPR["id"] || cleanupPR["id"] != mergeOperations[0].Target["node_id"] ||
		!nativeNonemptyValue(cleanupPR["headRefName"]) || cleanupPR["headRefName"] != parentPR["headRefName"] ||
		cleanupNumber != mergeOperations[0].Target["number"] || cleanupPR["headRefOid"] != mergeOperations[0].Target["head_sha"] ||
		cleanupPR["merge_commit_sha"] != mergeCommit {
		return errors.New("cleanup child pull request or merge commit differs from its acknowledged parent")
	}
	sourceTarget, err := object(sourceContext["attempt_target"], "completed merge source target")
	if err != nil || !Equal(sourceTarget, Object{"plan_sha256": parentPlan.SHA256}) {
		return errors.New("completed merge source attempt target differs from its exact parent plan")
	}
	if childSourceProof["journal_file"] != nil {
		sourceJournalValue, err := LoadFileProof(childSourceProof["journal_file"], "zero-operation source journal")
		if err != nil {
			return err
		}
		sourceJournal, err := Exact(sourceJournalValue, observerJournalFields, "zero-operation source journal")
		if err != nil {
			return err
		}
		childRepository, err := nativePlanRepository(childPlanValue)
		if err != nil {
			return err
		}
		journalIdentity := Object{"schema_version": int64(2), "repository": childRepository, "command": childPlan.Command, "plan_sha256": childPlan.SHA256}
		if !Equal(sourceJournal["identity"], journalIdentity) {
			return errors.New("zero-operation source journal differs from the exact cleanup plan")
		}
		steps, err := array(sourceJournal["steps"], "zero-operation source journal steps")
		if err != nil || len(steps) != 0 {
			return errors.New("zero-operation source journal contains an unreviewed dispatch")
		}
		observerJournalValue, err := LoadFileProof(childObserverProof["journal_file"], "zero-operation observer journal")
		if err != nil {
			return err
		}
		observerJournal, err := Exact(observerJournalValue, observerJournalFields, "zero-operation observer journal")
		if err != nil || !Equal(sourceJournal["identity"], observerJournal["identity"]) ||
			(sourceJournal["result"] != nil && !Equal(sourceJournal["result"], observerJournal["result"])) {
			return errors.New("zero-operation observer journal differs from the exact source identity or result")
		}
	}
	return nil
}

func recoveredParentRepositorySettings(raw any) (Object, error) {
	files, err := object(raw, "source policy files")
	if err != nil {
		return nil, err
	}
	fields := []string{"schema_version", "repository", "repository_node_id", "owner_login", "owner_type", "name", "default_branch", "delete_branch_on_merge"}
	var settings Object
	for _, rawProof := range files {
		value, err := LoadFileProof(rawProof, "source policy file")
		if err != nil {
			return nil, err
		}
		candidate, candidateErr := Exact(value, fields, "merge repository settings")
		if candidateErr != nil {
			continue
		}
		if settings != nil {
			return nil, errors.New("zero-operation cleanup source contains ambiguous repository settings")
		}
		settings = candidate
	}
	if settings == nil || !observerIntegerIs(settings["schema_version"], 1) || !nativeNonemptyValue(settings["repository_node_id"]) ||
		!nativeNonemptyValue(settings["default_branch"]) || !nativeNonemptyValue(settings["owner_login"]) ||
		(settings["owner_type"] != "User" && settings["owner_type"] != "Organization") {
		return nil, errors.New("zero-operation cleanup source lacks exact merge repository settings")
	}
	if _, err := contract.Bool(settings, "delete_branch_on_merge"); err != nil {
		return nil, err
	}
	return settings, nil
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
