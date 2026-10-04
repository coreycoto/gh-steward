package runrecovery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var (
	runContextBaseFields = []string{
		"schema_version", "workflow_file", "repository", "recovery_key", "run_name",
		"workflow_run_id", "workflow_run_attempt", "attempt_target", "phase",
		"dispatch_steps", "plans",
	}
	runContextOptionalFields = []string{
		"trusted_source_sha", "recovered_from_run_id", "recovered_from_attempt",
		"plan_origin_run_id", "plan_origin_attempt",
		"blocked_reason", "branch_cleanup_outcome", "parent_merge", "publication",
	}
	runContextPlanRequiredFields = []string{"name", "path", "command", "sha256", "journal_id", "status"}
	runContextPlanOptionalFields = []string{"review_path", "review_sha256", "journal_path", "apply_result_path"}
)

// InitializeContext writes the first durable context for a run. The workflow
// identity and mutation-step inventory are caller supplied and policy scoped.
func (e *Engine) InitializeContext(root string, input Object) (Object, error) {
	fields, err := exactWithOptional(input,
		[]string{"workflow_file", "repository", "recovery_key", "run_name", "run_id", "attempt", "attempt_target", "dispatch_steps"},
		[]string{"trusted_source_sha", "blocked_reason", "parent_merge", "publication"}, "context start input")
	if err != nil {
		return nil, err
	}
	workflow, repository, recoveryKey, runName := fields["workflow_file"], fields["repository"], fields["recovery_key"], fields["run_name"]
	if _, err := e.workflowPolicy(fmt.Sprint(workflow)); err != nil {
		return nil, err
	}
	if !strings.EqualFold(fmt.Sprint(repository), e.repository.Owner+"/"+e.repository.Name) {
		return nil, recoveryError("new run context belongs to another repository")
	}
	if !contextKeyPattern.MatchString(fmt.Sprint(recoveryKey)) || !nonemptyString(runName) ||
		!settlementWorkflowFile.MatchString(fmt.Sprint(workflow)) {
		return nil, recoveryError("new run context has an invalid workflow, recovery key or run name")
	}
	runID, err := positiveInteger(fields["run_id"], "context workflow run ID")
	if err != nil {
		return nil, err
	}
	attempt, err := positiveInteger(fields["attempt"], "context workflow attempt")
	if err != nil {
		return nil, err
	}
	attemptTarget, err := object(fields["attempt_target"], "context attempt target")
	if err != nil || len(attemptTarget) == 0 {
		return nil, recoveryError("new run context requires a nonempty exact target identity")
	}
	dispatchSteps, err := stringsArray(fields["dispatch_steps"], "context mutation-step inventory", false)
	if err != nil {
		return nil, err
	}
	context := Object{
		"schema_version": int64(1), "workflow_file": workflow, "repository": repository,
		"recovery_key": recoveryKey, "run_name": runName, "workflow_run_id": runID,
		"workflow_run_attempt": attempt, "attempt_target": attemptTarget,
		"phase": "started", "dispatch_steps": dispatchSteps, "plans": []any{},
	}
	for _, field := range []string{"trusted_source_sha", "blocked_reason", "parent_merge", "publication"} {
		if value, exists := fields[field]; exists {
			context[field] = value
		}
	}
	if err := e.validateRunContext(context); err != nil {
		return nil, err
	}
	if _, statErr := os.Lstat(filepath.Join(root, "run-context.json")); statErr == nil {
		return nil, recoveryError("run context already exists and cannot be replaced by a new attempt")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, recoveryError("existing run context path is unsafe or unreadable: %v", statErr)
	}
	if err := persistPackageJSON(root, "run-context.json", context); err != nil {
		return nil, recoveryError("new run context could not be persisted: %v", err)
	}
	return context, nil
}

// ObserveContext creates a fresh observer context while keeping the source run,
// plan, journal and policy evidence immutable in recovery-source.json.
func (e *Engine) ObserveContext(root string, input Object) (Object, error) {
	fields, err := exactWithOptional(input,
		[]string{"current_run_id", "current_attempt", "current_run_name", "workflow_file", "repository", "recovery_key", "attempt_target", "dispatch_steps"},
		[]string{"trusted_source_sha"}, "context observer input")
	if err != nil {
		return nil, err
	}
	return e.observeContext(root, fields)
}

// ObserveSourceContext derives its target and mutation-step inventory only
// from the retained source context, so callers cannot select a new target.
func (e *Engine) ObserveSourceContext(root string, input Object) (Object, error) {
	fields, err := exactWithOptional(input,
		[]string{"current_run_id", "current_attempt", "current_run_name", "workflow_file", "repository"},
		[]string{"trusted_source_sha"}, "source observer input")
	if err != nil {
		return nil, err
	}
	source, err := readRecoverySource(root)
	if err != nil {
		return nil, err
	}
	original, err := contextFromProof(source["context_file"], "recovery source context")
	if err != nil {
		return nil, err
	}
	if original["workflow_file"] != fields["workflow_file"] ||
		!strings.EqualFold(fmt.Sprint(original["repository"]), fmt.Sprint(fields["repository"])) {
		return nil, recoveryError("source observer cannot change its original workflow or repository")
	}
	derived := Object{
		"current_run_id": fields["current_run_id"], "current_attempt": fields["current_attempt"],
		"current_run_name": fields["current_run_name"], "workflow_file": original["workflow_file"],
		"repository": original["repository"], "recovery_key": original["recovery_key"],
		"attempt_target": original["attempt_target"], "dispatch_steps": original["dispatch_steps"],
	}
	if trusted, exists := fields["trusted_source_sha"]; exists {
		derived["trusted_source_sha"] = trusted
	}
	return e.observeContext(root, derived)
}

func (e *Engine) observeContext(root string, fields Object) (Object, error) {
	currentRunID, err := positiveInteger(fields["current_run_id"], "observer workflow run ID")
	if err != nil {
		return nil, err
	}
	currentAttempt, err := positiveInteger(fields["current_attempt"], "observer workflow attempt")
	if err != nil {
		return nil, err
	}
	currentRunName, ok := fields["current_run_name"].(string)
	if !ok || !nonemptyString(currentRunName) {
		return nil, recoveryError("observer run name is invalid")
	}
	source, err := readRecoverySource(root)
	if err != nil {
		return nil, err
	}
	sourceRunID, err := positiveInteger(source["run_id"], "source workflow run ID")
	if err != nil {
		return nil, err
	}
	sourceAttempt, err := positiveInteger(source["attempt"], "source workflow attempt")
	if err != nil {
		return nil, err
	}
	sourceRun, err := Exact(source["run"], immutableRunFields, "source immutable workflow run")
	if err != nil {
		return nil, err
	}
	if _, err := validateRunIdentity(sourceRun, sourceRunID, sourceAttempt, Object{"workflow_id": sourceRun["workflow_id"]}); err != nil {
		return nil, err
	}
	if sourceRunID == currentRunID && sourceAttempt >= currentAttempt {
		return nil, recoveryError("observer does not follow its exact original source attempt")
	}
	sourceContext, err := contextFromProof(source["context_file"], "recovery source context")
	if err != nil {
		return nil, err
	}
	preparedSource := source["prepared_qualification"] != nil
	if !preparedSource && sourceContext["phase"] != "dispatching" && sourceContext["publication"] == nil {
		return nil, recoveryError("observer source is not an interrupted native attempt")
	}
	if err := e.validateRunContext(sourceContext); err != nil {
		return nil, err
	}
	workflow := fmt.Sprint(sourceContext["workflow_file"])
	repository := fmt.Sprint(sourceContext["repository"])
	if fields["workflow_file"] != sourceContext["workflow_file"] ||
		!strings.EqualFold(fmt.Sprint(fields["repository"]), repository) ||
		fields["recovery_key"] != sourceContext["recovery_key"] ||
		!Equal(fields["attempt_target"], sourceContext["attempt_target"]) ||
		!Equal(fields["dispatch_steps"], sourceContext["dispatch_steps"]) ||
		!exactInt(sourceContext["workflow_run_id"], sourceRunID) ||
		!exactInt(sourceContext["workflow_run_attempt"], sourceAttempt) ||
		sourceContext["run_name"] != sourceRun["display_title"] {
		return nil, recoveryError("observer input changes the exact workflow, repository or source target")
	}
	if err := e.validateTargetForContext(workflow, repository, sourceRun); err != nil {
		return nil, err
	}
	_, err = validateArtifact(source["artifact"], Object{
		"workflow_file": workflow, "repository": e.repository.Owner + "/" + e.repository.Name,
		"server_url": "https://" + e.repository.Host, "workflow_id": sourceRun["workflow_id"], "recovery_key": "workflow-history-v2",
	}, sourceRunID, sourceAttempt)
	if err != nil {
		return nil, err
	}
	plans, err := array(source["plans"], "recovery source plans")
	if err != nil {
		return nil, err
	}
	contextPlans, err := array(sourceContext["plans"], "source context plans")
	if err != nil || len(plans) != len(contextPlans) {
		return nil, recoveryError("observer source plan inventory differs from its immutable context")
	}
	policyFiles, err := object(source["policy_files"], "recovery source policy files")
	if err != nil {
		return nil, err
	}
	if sourceContext["publication"] != nil {
		if len(plans) != 0 || len(contextPlans) != 0 || len(policyFiles) != 0 ||
			sourceContext["phase"] != "prepared" || sourceContext["publication"] == nil {
			return nil, recoveryError("publication observer source has a mixed or unsupported plan state")
		}
		if _, err := ValidatePublicationProof(source["publication"], Object{
			"workflow_file": workflow, "repository": e.repository.Owner + "/" + e.repository.Name,
			"server_url": "https://" + e.repository.Host, "workflow_id": sourceRun["workflow_id"], "recovery_key": "workflow-history-v2",
		}, sourceContext, false); err != nil {
			return nil, recoveryError("publication source proof is invalid: %v", err)
		}
	} else if preparedSource {
		if sourceContext["publication"] != nil || (sourceContext["phase"] != "prepared" && sourceContext["phase"] != "dispatching") {
			return nil, recoveryError("qualified prepared source has an unsupported observer phase")
		}
		target, err := TargetObject(workflow, e.repository.URL, mustPositive(sourceRun["workflow_id"]), "workflow-history-v2")
		if err != nil {
			return nil, err
		}
		if _, err := e.validatePreparedSourceRecord(source, target); err != nil {
			return nil, recoveryError("prepared observer source proof is invalid: %v", err)
		}
		retainedPlans := make([]Object, 0, len(plans))
		for _, rawPlan := range plans {
			proof, err := Exact(rawPlan, preparedPlanFields, "prepared source native plan proof")
			if err != nil {
				return nil, err
			}
			retainedPlans = append(retainedPlans, proof)
		}
		if err := e.validateRetainedPlanPolicyFiles(sourceContext, retainedPlans, policyFiles); err != nil {
			return nil, recoveryError("prepared observer source policy evidence is invalid: %v", err)
		}
	} else {
		if len(plans) == 0 || sourceContext["phase"] != "dispatching" {
			return nil, recoveryError("native observer source lacks its exact interrupted plan inventory")
		}
		retainedPlans := make([]Object, 0, len(plans))
		for _, rawPlan := range plans {
			proof, err := exactWithOptional(rawPlan, []string{"name", "command", "plan_sha256", "journal_id", "plan_file", "journal_file"}, []string{"apply_result_file"}, "source native plan proof")
			if err != nil {
				return nil, err
			}
			retainedPlans = append(retainedPlans, proof)
		}
		policyReader := &policyReader{root: root, proofs: policyFiles, used: map[string]bool{}}
		if err := e.validatePlanPolicyFiles(sourceContext, retainedPlans, policyReader); err != nil {
			return nil, recoveryError("source native plans violate their exact recovery policy: %v", err)
		}
		if err := validatePolicyFilesUsed(policyFiles, policyReader); err != nil {
			return nil, err
		}
		persistedDispatches := 0
		unstartedPlans := 0
		for index, rawPlan := range plans {
			proof, err := exactWithOptional(rawPlan, []string{"name", "command", "plan_sha256", "journal_id", "plan_file", "journal_file"}, []string{"apply_result_file"}, "source native plan proof")
			if err != nil {
				return nil, err
			}
			entry, err := exactWithOptional(contextPlans[index], runContextPlanRequiredFields, runContextPlanOptionalFields, "source context plan")
			if err != nil {
				return nil, err
			}
			if proof["name"] != entry["name"] || proof["command"] != entry["command"] ||
				proof["plan_sha256"] != entry["sha256"] || proof["journal_id"] != entry["journal_id"] {
				return nil, recoveryError("source native plan differs from its immutable context entry")
			}
			planValue, err := LoadFileProof(proof["plan_file"], "source native plan")
			if err != nil {
				return nil, err
			}
			plan, err := object(planValue, "source native plan")
			if err != nil {
				return nil, err
			}
			parsedPlan, err := contract.ParsePlan(plan)
			if err != nil || parsedPlan.Command != entry["command"] || parsedPlan.SHA256 != entry["sha256"] ||
				parsedPlan.Repository.FullName() != strings.ToLower(e.repository.Owner+"/"+e.repository.Name) || parsedPlan.Repository.Host != e.repository.Host {
				return nil, recoveryError("source native plan differs from its exact hash, command or repository")
			}
			planPath, ok := entry["path"].(string)
			if !ok || !safePolicyRelativePath(planPath) {
				return nil, recoveryError("source native plan has an unsafe package path")
			}
			planBytes, err := ReadPackageFile(root, planPath)
			if err != nil || !Equal(MakeFileProof(planBytes), proof["plan_file"]) {
				return nil, recoveryError("restored native plan differs from its immutable source proof")
			}
			journalProof := proof["journal_file"]
			if entry["status"] == "completed" {
				if journalProof == nil || proof["apply_result_file"] == nil {
					return nil, recoveryError("completed parent source lost its exact terminal receipt")
				}
				if _, err := ValidateTerminalPlanProof(proof); err != nil {
					return nil, recoveryError("completed parent source receipt is invalid: %v", err)
				}
			} else if journalProof == nil {
				if proof["apply_result_file"] != nil {
					return nil, recoveryError("unstarted source plan contains an unbound apply result")
				}
				if entry["status"] != "prepared" {
					return nil, recoveryError("started source plan lost its exact persisted native journal")
				}
				journalPath := "journal/" + fmt.Sprint(entry["journal_id"]) + ".json"
				if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(journalPath))); !errors.Is(err, os.ErrNotExist) {
					return nil, recoveryError("journal-free prepared source contains an unbound journal path")
				}
				unstartedPlans++
			} else {
				journalBytes, err := LoadRawFileProof(journalProof, "source native journal")
				if err != nil {
					return nil, err
				}
				journalPath := "journal/" + fmt.Sprint(entry["journal_id"]) + ".json"
				restored, err := ReadPackageFile(root, journalPath)
				if err != nil || !Equal(restored, journalBytes) {
					return nil, recoveryError("restored journal bytes differ from their immutable source proof")
				}
				journalValue, err := DecodeValue(journalBytes)
				if err != nil {
					return nil, err
				}
				journal, err := object(journalValue, "source native journal")
				if err != nil {
					return nil, err
				}
				dispatches, err := validatePartialJournal(journal, parsedPlan, fmt.Sprint(entry["journal_id"]))
				if err != nil {
					return nil, err
				}
				if dispatches == 0 {
					return nil, recoveryError("unfinished source journal has no positive native dispatch receipt")
				}
				persistedDispatches += dispatches
			}
		}
		if persistedDispatches == 0 {
			return nil, recoveryError("source has no persisted native dispatch identity and cannot be resumed")
		}
		if unstartedPlans != 0 {
			return nil, recoveryError("source mixes positive journal progress with an unstarted native plan")
		}
	}
	planOriginRunID, planOriginAttempt, err := contextPlanOrigin(sourceContext, true)
	if err != nil {
		return nil, recoveryError("observer source does not preserve its immutable plan origin: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "run-context.json")); statErr == nil {
		currentContext, err := e.readRunContext(root)
		if err != nil {
			return nil, recoveryError("existing observer context is malformed: %v", err)
		}
		if currentContext["workflow_file"] != workflow || !strings.EqualFold(fmt.Sprint(currentContext["repository"]), repository) ||
			currentContext["recovery_key"] != sourceContext["recovery_key"] || !Equal(currentContext["attempt_target"], sourceContext["attempt_target"]) ||
			!Equal(currentContext["dispatch_steps"], sourceContext["dispatch_steps"]) {
			return nil, recoveryError("existing observer context changes the immutable source target")
		}
		isSourceContext := exactInt(currentContext["workflow_run_id"], sourceRunID) && exactInt(currentContext["workflow_run_attempt"], sourceAttempt)
		isObserverContext := exactInt(currentContext["workflow_run_id"], currentRunID) && exactInt(currentContext["workflow_run_attempt"], currentAttempt) &&
			exactInt(currentContext["recovered_from_run_id"], sourceRunID) && exactInt(currentContext["recovered_from_attempt"], sourceAttempt)
		if isObserverContext {
			currentOriginRunID, currentOriginAttempt, originErr := contextPlanOrigin(currentContext, true)
			isObserverContext = originErr == nil && currentOriginRunID == planOriginRunID && currentOriginAttempt == planOriginAttempt
		}
		if !isSourceContext && !isObserverContext {
			return nil, recoveryError("existing context is neither the exact source nor its current observer")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, recoveryError("existing observer context path is unsafe or unreadable: %v", statErr)
	}
	observerPhase := "dispatching"
	if sourceContext["publication"] != nil || (preparedSource && sourceContext["phase"] == "prepared") {
		observerPhase = "prepared"
	}
	observerContext := Object{
		"schema_version": int64(1), "workflow_file": workflow, "repository": repository,
		"recovery_key": sourceContext["recovery_key"], "run_name": currentRunName,
		"workflow_run_id": currentRunID, "workflow_run_attempt": currentAttempt,
		"attempt_target": sourceContext["attempt_target"], "phase": observerPhase,
		"dispatch_steps": sourceContext["dispatch_steps"], "plans": sourceContext["plans"],
		"recovered_from_run_id": sourceRunID, "recovered_from_attempt": sourceAttempt,
		"plan_origin_run_id": planOriginRunID, "plan_origin_attempt": planOriginAttempt,
	}
	for _, field := range []string{"parent_merge", "branch_cleanup_outcome", "publication"} {
		if value, exists := sourceContext[field]; exists {
			observerContext[field] = value
		}
	}
	if trusted, exists := fields["trusted_source_sha"]; exists {
		observerContext["trusted_source_sha"] = trusted
	}
	if err := e.validateRunContext(observerContext); err != nil {
		return nil, err
	}
	if err := persistPackageJSON(root, "run-context.json", observerContext); err != nil {
		return nil, recoveryError("observer run context could not be persisted: %v", err)
	}
	return observerContext, nil
}

// RecordContextPlan binds one exact v2 plan before any native operation can be dispatched.
func (e *Engine) RecordContextPlan(root string, input Object) (Object, error) {
	fields, err := exactWithOptional(input,
		[]string{"relative", "name", "command", "repository", "plan"},
		[]string{"review_path", "review_sha256"}, "context plan input")
	if err != nil {
		return nil, err
	}
	name, ok := fields["name"].(string)
	if !ok || !policyPlanName.MatchString(name) {
		return nil, recoveryError("context plan name is invalid")
	}
	relative, ok := fields["relative"].(string)
	if !ok || relative != "plans/"+name+".json" {
		return nil, recoveryError("context plan path must be its exact plans/<name>.json identity")
	}
	command, ok := fields["command"].(string)
	if !ok || !nativeCommandPattern.MatchString(command) {
		return nil, recoveryError("context plan command is invalid")
	}
	if !strings.EqualFold(fmt.Sprint(fields["repository"]), e.repository.Owner+"/"+e.repository.Name) {
		return nil, recoveryError("context plan belongs to another repository")
	}
	context, err := e.readRunContext(root)
	if err != nil {
		return nil, err
	}
	dispatchSteps, err := stringsArray(context["dispatch_steps"], "context mutation-step inventory", false)
	if err != nil {
		return nil, err
	}
	if name == "workflow-noop" || command == "workflow-noop" || len(dispatchSteps) == 0 {
		return nil, recoveryError("workflow no-op plans are completed only by the dedicated no-op finalizer")
	}
	workflowPolicy, err := e.workflowPolicy(fmt.Sprint(context["workflow_file"]))
	if err != nil {
		return nil, err
	}
	composedMerge := false
	if context["phase"] == "completed" {
		if _, err := e.completedMergeParentOnlyContext(context, name, command, workflowPolicy); err != nil {
			return nil, recoveryError("a completed run accepts only its exact reviewed branch-cleanup continuation: %v", err)
		}
		composedMerge = true
	}
	if context["phase"] != "started" && context["phase"] != "prepared" && !composedMerge {
		return nil, recoveryError("a new plan cannot be recorded after dispatch has begun or completed")
	}
	plan, err := object(fields["plan"], "context native plan")
	if err != nil {
		return nil, err
	}
	parsedPlan, err := contract.ParsePlan(plan)
	if err != nil || parsedPlan.Command != command || !strings.EqualFold(parsedPlan.Repository.FullName(), fmt.Sprint(fields["repository"])) || parsedPlan.Repository.Host != e.repository.Host {
		return nil, recoveryError("context native plan differs from its exact command or repository")
	}
	for _, row := range context["plans"].([]any) {
		entry, _ := object(row, "context plan entry")
		if entry["name"] == name || entry["sha256"] == parsedPlan.SHA256 || entry["journal_id"] == journalIDForPlan(plan) {
			return nil, recoveryError("run context already records this exact plan name, hash or journal")
		}
	}
	entry := Object{
		"name": name, "path": relative, "command": command, "sha256": parsedPlan.SHA256,
		"journal_id": journalIDForPlan(plan), "status": "prepared",
	}
	if _, hasPath := fields["review_path"]; hasPath {
		entry["review_path"] = fields["review_path"]
	}
	if _, hasDigest := fields["review_sha256"]; hasDigest {
		entry["review_sha256"] = fields["review_sha256"]
	}
	context["plans"] = append(context["plans"].([]any), entry)
	context["phase"] = "prepared"
	if err := e.ValidateRecoveryPlan(context, entry, plan, root); err != nil {
		return nil, recoveryError("context plan is outside the exact reviewed workflow policy: %v", err)
	}
	planBytes, err := Canonical(plan)
	if err != nil {
		return nil, err
	}
	if existing, err := ReadPackageFile(root, relative); err == nil {
		if !Equal(existing, append(planBytes, '\n')) {
			return nil, recoveryError("existing context plan file differs from the exact proposed plan")
		}
	} else if _, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(relative))); errors.Is(statErr, os.ErrNotExist) {
		if err := persistPackageFile(root, relative, append(planBytes, '\n')); err != nil {
			return nil, recoveryError("exact native plan could not be persisted: %v", err)
		}
	} else {
		return nil, recoveryError("existing context plan path is unsafe or unreadable: %v", err)
	}
	if err := persistPackageJSON(root, "run-context.json", context); err != nil {
		return nil, recoveryError("updated run context could not be persisted: %v", err)
	}
	return context, nil
}

// SetContextPhase labels plan-free runs as prepared or recovery-needed. Modern
// no-op-enabled workflows must finish through the exact workflow-noop plan.
func (e *Engine) SetContextPhase(root, phase, reason string) (Object, error) {
	if phase != "prepared" && phase != "noop" && phase != "recovery_needed" {
		return nil, recoveryError("only prepared or recovery-needed plan-free phases may be assigned directly")
	}
	context, err := e.readRunContext(root)
	if err != nil {
		return nil, err
	}
	workflowPolicy, err := e.workflowPolicy(fmt.Sprint(context["workflow_file"]))
	if err != nil {
		return nil, err
	}
	if len(context["plans"].([]any)) != 0 || context["publication"] != nil {
		return nil, recoveryError("a run with a plan or publication cannot be relabeled as plan-free")
	}
	if phase == "prepared" {
		dispatchSteps, err := stringsArray(context["dispatch_steps"], "context mutation-step inventory", false)
		if err != nil {
			return nil, err
		}
		if len(dispatchSteps) != 0 || workflowPolicy.plans["workflow-noop"].profile != "workflow-noop" ||
			(context["phase"] != "started" && context["phase"] != "prepared") {
			return nil, recoveryError("prepared plan-free state is reserved for a no-op-enabled workflow before dispatch")
		}
	}
	if phase == "noop" && workflowPolicy.plans["workflow-noop"].profile == "workflow-noop" {
		return nil, recoveryError("workflow no-op must finish through its exact completed workflow-noop plan")
	}
	if reason != "" {
		if !nonemptyString(reason) {
			return nil, recoveryError("context blocked reason must be a nonempty single-line string")
		}
		context["blocked_reason"] = reason
	}
	context["phase"] = phase
	if err := e.validateRunContext(context); err != nil {
		return nil, err
	}
	if err := persistPackageJSON(root, "run-context.json", context); err != nil {
		return nil, recoveryError("context phase could not be persisted: %v", err)
	}
	return context, nil
}

// MarkContextPlan advances one plan monotonically and records completion only
// after its full native v2 plan, journal and apply result validate.
func (e *Engine) MarkContextPlan(root, name, status string) (Object, error) {
	if status != "dispatching" && status != "completed" {
		return nil, recoveryError("context plan transition is unsupported")
	}
	context, err := e.readRunContext(root)
	if err != nil {
		return nil, err
	}
	entry, err := contextPlanByName(context, name)
	if err != nil {
		return nil, err
	}
	if name == "workflow-noop" || entry["command"] == "workflow-noop" {
		return nil, recoveryError("workflow no-op plans are finalized only by the dedicated no-op finalizer")
	}
	plan, err := e.loadContextPlan(root, context, entry)
	if err != nil {
		return nil, err
	}
	prior := fmt.Sprint(entry["status"])
	if status == "dispatching" && prior == "completed" {
		if _, err := e.terminalContextPlanProof(root, entry, plan); err != nil {
			return nil, err
		}
		return context, nil
	}
	if status == "dispatching" {
		if prior != "prepared" && prior != "dispatching" {
			return nil, recoveryError("only an exact prepared plan may enter dispatch")
		}
		if err := e.ValidateRecoveryPlan(context, entry, plan, root); err != nil {
			return nil, err
		}
		entry["status"], context["phase"] = "dispatching", "dispatching"
	} else {
		if prior != "dispatching" && prior != "completed" {
			return nil, recoveryError("only a dispatched plan may become completed")
		}
		if _, err := e.terminalContextPlanProof(root, entry, plan); err != nil {
			return nil, err
		}
		if err := e.ValidateRecoveryPlan(context, entry, plan, root); err != nil {
			return nil, err
		}
		entry["status"] = "completed"
		context["phase"] = "dispatching"
		allCompleted := true
		for _, raw := range context["plans"].([]any) {
			row, _ := object(raw, "context plan entry")
			allCompleted = allCompleted && row["status"] == "completed"
		}
		if allCompleted {
			context["phase"] = "completed"
		}
	}
	if err := e.validateRunContext(context); err != nil {
		return nil, err
	}
	if err := persistPackageJSON(root, "run-context.json", context); err != nil {
		return nil, recoveryError("plan transition could not be persisted: %v", err)
	}
	return context, nil
}

// CaptureJournal copies the exact bounded native journal from the execution
// engine into the workflow package after validating its ordered intent prefix.
func (e *Engine) CaptureJournal(root, journalRoot, name string) (Object, error) {
	context, err := e.readRunContext(root)
	if err != nil {
		return nil, err
	}
	entry, err := contextPlanByName(context, name)
	if err != nil {
		return nil, err
	}
	plan, err := e.loadContextPlan(root, context, entry)
	if err != nil {
		return nil, err
	}
	journalPath, err := confinedRootFile(journalRoot, fmt.Sprint(entry["journal_id"])+".json")
	if err != nil {
		return nil, err
	}
	journalBytes, err := readRegularFile(journalPath)
	if err != nil {
		return nil, recoveryError("native engine journal is missing or unsafe: %v", err)
	}
	journalValue, err := DecodeValue(journalBytes)
	if err != nil {
		return nil, err
	}
	journal, err := object(journalValue, "native engine journal")
	if err != nil {
		return nil, err
	}
	parsed, err := contract.ParsePlan(plan)
	if err != nil {
		return nil, err
	}
	if _, err := validatePartialJournal(journal, parsed, fmt.Sprint(entry["journal_id"])); err != nil {
		return nil, err
	}
	relative := "journal/" + fmt.Sprint(entry["journal_id"]) + ".json"
	if existing, err := ReadPackageFile(root, relative); err == nil {
		if !Equal(existing, journalBytes) {
			value, err := DecodeValue(existing)
			if err != nil {
				return nil, err
			}
			previous, err := object(value, "previous package journal")
			if err != nil {
				return nil, err
			}
			if err := validateJournalAdvance(previous, journal, parsed, fmt.Sprint(entry["journal_id"])); err != nil {
				return nil, err
			}
			if err := persistPackageFile(root, relative, journalBytes); err != nil {
				return nil, err
			}
		}
	} else if _, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(relative))); errors.Is(statErr, os.ErrNotExist) {
		if err := persistPackageFile(root, relative, journalBytes); err != nil {
			return nil, recoveryError("native engine journal could not be captured: %v", err)
		}
	} else {
		return nil, recoveryError("workflow package journal path is unsafe or unreadable: %v", err)
	}
	return Object{"path": relative, "sha256": SHA256(journalBytes), "journal_id": entry["journal_id"]}, nil
}

// Capturing progress may advance the live package journal while the original
// source proof remains immutable. Existing completions and dispatch identities
// cannot be removed, changed or replayed.
func validateJournalAdvance(previous, current Object, plan contract.Plan, journalID string) error {
	if _, err := validatePartialJournal(previous, plan, journalID); err != nil {
		return err
	}
	if _, err := validatePartialJournal(current, plan, journalID); err != nil {
		return err
	}
	oldSteps, _ := array(previous["steps"], "previous journal steps")
	newSteps, _ := array(current["steps"], "current journal steps")
	if len(newSteps) < len(oldSteps) {
		return recoveryError("journal capture discarded a durable dispatch")
	}
	for i, raw := range oldSteps {
		old := raw.(Object)
		next := newSteps[i].(Object)
		if old["status"] == "completed" {
			if !Equal(old, next) {
				return recoveryError("journal capture altered a completed primitive")
			}
			continue
		}
		if old["status"] == "unknown" && next["status"] == "dispatching" {
			return recoveryError("journal capture reopened an unknown dispatch")
		}
		for field, value := range old {
			if field == "status" || field == "result" || field == "completed_at" {
				continue
			}
			if !Equal(next[field], value) {
				return recoveryError("journal capture changed a persisted dispatch or acknowledgement")
			}
		}
	}
	if previous["result"] != nil && !Equal(previous["result"], current["result"]) {
		return recoveryError("journal capture changed a terminal result")
	}
	return nil
}

// InstallRestoredJournal installs only the exact partial journal retained with
// the immutable recovery source. An existing different journal is a hard hold.
func (e *Engine) InstallRestoredJournal(root, journalRoot, name string) (Object, error) {
	context, err := e.readRunContext(root)
	if err != nil {
		return nil, err
	}
	entry, err := contextPlanByName(context, name)
	if err != nil {
		return nil, err
	}
	if !exactInt(context["recovered_from_run_id"], mustPositive(context["recovered_from_run_id"])) ||
		!positiveInt(context["recovered_from_attempt"]) {
		return nil, recoveryError("journal installation requires an exact observer source attempt")
	}
	source, err := readRecoverySource(root)
	if err != nil {
		return nil, err
	}
	if !exactInt(source["run_id"], mustPositive(context["recovered_from_run_id"])) ||
		!exactInt(source["attempt"], mustPositive(context["recovered_from_attempt"])) {
		return nil, recoveryError("recovery source differs from the exact observer origin")
	}
	sourceContext, err := contextFromProof(source["context_file"], "recovery source context")
	if err != nil {
		return nil, err
	}
	if sourceContext["workflow_file"] != context["workflow_file"] || sourceContext["repository"] != context["repository"] ||
		sourceContext["recovery_key"] != context["recovery_key"] || !Equal(sourceContext["attempt_target"], context["attempt_target"]) {
		return nil, recoveryError("observer changed the immutable source workflow target")
	}
	var sourcePlan Object
	for _, raw := range source["plans"].([]any) {
		row, _ := object(raw, "recovery source plan")
		if row["name"] == name {
			if sourcePlan != nil {
				return nil, recoveryError("recovery source repeats an exact plan identity")
			}
			sourcePlan = row
		}
	}
	if sourcePlan == nil || sourcePlan["name"] != entry["name"] || sourcePlan["command"] != entry["command"] ||
		sourcePlan["plan_sha256"] != entry["sha256"] || sourcePlan["journal_id"] != entry["journal_id"] {
		return nil, recoveryError("observer plan has no exact persisted source journal proof")
	}
	if source["prepared_qualification"] != nil && sourcePlan["journal_file"] == nil && sourcePlan["apply_result_file"] == nil {
		if entry["status"] != "prepared" && entry["status"] != "dispatching" {
			return nil, recoveryError("journal-free prepared source has an unsupported manifest status")
		}
		sourceRun, err := Exact(source["run"], immutableRunFields, "journal-free source run")
		if err != nil {
			return nil, err
		}
		target, err := TargetObject(fmt.Sprint(context["workflow_file"]), e.repository.URL, mustPositive(sourceRun["workflow_id"]), "workflow-history-v2")
		if err != nil {
			return nil, err
		}
		if _, err := e.validatePreparedSourceRecord(source, target); err != nil {
			return nil, recoveryError("journal-free source proof is invalid: %v", err)
		}
		packageJournal := filepath.Join(root, "journal", fmt.Sprint(entry["journal_id"])+".json")
		if _, err := os.Lstat(packageJournal); !errors.Is(err, os.ErrNotExist) {
			return nil, recoveryError("journal-free source package contains unbound native progress")
		}
		enginePath, err := confinedRootFile(journalRoot, fmt.Sprint(entry["journal_id"])+".json")
		if err != nil {
			return nil, err
		}
		if _, err := os.Lstat(enginePath); !errors.Is(err, os.ErrNotExist) {
			return nil, recoveryError("native engine already has progress absent from the exact source proof")
		}
		return Object{"outcome": "no-journal-required", "journal_id": entry["journal_id"]}, nil
	}
	if sourcePlan["journal_file"] == nil {
		return nil, recoveryError("observer plan has no exact persisted source journal proof")
	}
	plan, err := e.loadContextPlan(root, context, entry)
	if err != nil {
		return nil, err
	}
	planProof, err := LoadRawFileProof(sourcePlan["plan_file"], "source plan")
	if err != nil {
		return nil, err
	}
	planBytes, err := ReadPackageFile(root, fmt.Sprint(entry["path"]))
	if err != nil || !Equal(planBytes, planProof) {
		return nil, recoveryError("restored source plan bytes differ from their exact proof")
	}
	sourcePlans, err := array(sourceContext["plans"], "recovery source context plans")
	if err != nil {
		return nil, err
	}
	var sourceEntry Object
	for _, raw := range sourcePlans {
		row, _ := object(raw, "recovery source context plan")
		if row["name"] == name {
			sourceEntry = row
			break
		}
	}
	if sourceEntry == nil {
		return nil, recoveryError("source context has no exact plan manifest entry")
	}
	policyFiles, err := object(source["policy_files"], "recovery source policy files")
	if err != nil {
		return nil, err
	}
	sourcePlanProofs, err := array(source["plans"], "recovery source plans")
	if err != nil {
		return nil, err
	}
	retainedPlans := make([]Object, 0, len(sourcePlanProofs))
	for _, raw := range sourcePlanProofs {
		proof, err := exactWithOptional(raw, []string{"name", "command", "plan_sha256", "journal_id", "plan_file", "journal_file"}, []string{"apply_result_file"}, "source native plan proof")
		if err != nil {
			return nil, err
		}
		retainedPlans = append(retainedPlans, proof)
	}
	policyReader := &policyReader{root: root, proofs: policyFiles, used: map[string]bool{}}
	if err := e.validatePlanPolicyFiles(sourceContext, retainedPlans, policyReader); err != nil {
		return nil, recoveryError("restored source plan violates its exact policy: %v", err)
	}
	if err := validatePolicyFilesUsed(policyFiles, policyReader); err != nil {
		return nil, err
	}
	journalBytes, err := LoadRawFileProof(sourcePlan["journal_file"], "source journal")
	if err != nil {
		return nil, err
	}
	parsed, err := contract.ParsePlan(plan)
	if err != nil {
		return nil, err
	}
	journalValue, err := DecodeValue(journalBytes)
	if err != nil {
		return nil, err
	}
	journal, err := object(journalValue, "source native journal")
	if err != nil {
		return nil, err
	}
	if _, err := validatePartialJournal(journal, parsed, fmt.Sprint(entry["journal_id"])); err != nil {
		return nil, err
	}
	packageJournalPath := "journal/" + fmt.Sprint(entry["journal_id"]) + ".json"
	packageJournal, err := ReadPackageFile(root, packageJournalPath)
	if err != nil || !Equal(packageJournal, journalBytes) {
		return nil, recoveryError("restored package journal differs from the exact source proof")
	}
	enginePath, err := confinedRootFile(journalRoot, fmt.Sprint(entry["journal_id"])+".json")
	if err != nil {
		return nil, err
	}
	if existing, err := readRegularFile(enginePath); err == nil {
		if !Equal(existing, journalBytes) {
			return nil, recoveryError("existing native engine journal differs from the exact source journal")
		}
	} else if _, statErr := os.Lstat(enginePath); errors.Is(statErr, os.ErrNotExist) {
		if err := persistPackageFile(journalRoot, filepath.Base(enginePath), journalBytes); err != nil {
			return nil, recoveryError("exact source journal could not be installed: %v", err)
		}
	} else {
		return nil, recoveryError("native engine journal destination is unsafe or unreadable: %v", err)
	}
	return Object{"path": enginePath, "sha256": SHA256(journalBytes), "journal_id": entry["journal_id"]}, nil
}

func (e *Engine) readRunContext(root string) (Object, error) {
	data, err := ReadPackageFile(root, "run-context.json")
	if err != nil {
		return nil, recoveryError("run context is missing or unsafe: %v", err)
	}
	value, err := DecodeValue(data)
	if err != nil {
		return nil, recoveryError("run context is malformed: %v", err)
	}
	context, err := object(value, "run context")
	if err != nil {
		return nil, err
	}
	if err := e.validateRunContext(context); err != nil {
		return nil, err
	}
	return context, nil
}

func (e *Engine) validateRunContext(context Object) error {
	context, err := exactWithOptional(context, runContextBaseFields, runContextOptionalFields, "run context")
	if err != nil {
		return err
	}
	if !exactInt(context["schema_version"], 1) || !settlementWorkflowFile.MatchString(fmt.Sprint(context["workflow_file"])) {
		return recoveryError("run context schema or workflow identity is invalid")
	}
	workflowPolicy, err := e.workflowPolicy(fmt.Sprint(context["workflow_file"]))
	if err != nil {
		return err
	}
	if !strings.EqualFold(fmt.Sprint(context["repository"]), e.repository.Owner+"/"+e.repository.Name) ||
		!contextKeyPattern.MatchString(fmt.Sprint(context["recovery_key"])) || !nonemptyString(context["run_name"]) {
		return recoveryError("run context belongs to another repository or has an invalid exact run identity")
	}
	if _, err := positiveInteger(context["workflow_run_id"], "context workflow run ID"); err != nil {
		return err
	}
	if _, err := positiveInteger(context["workflow_run_attempt"], "context workflow attempt"); err != nil {
		return err
	}
	if _, err := object(context["attempt_target"], "context attempt target"); err != nil {
		return err
	} else if len(context["attempt_target"].(Object)) == 0 {
		return recoveryError("run context target identity is empty")
	}
	phase, ok := context["phase"].(string)
	if !ok || !contains([]string{"started", "prepared", "dispatching", "completed", "noop", "recovery_needed"}, phase) {
		return recoveryError("run context phase is unsupported")
	}
	dispatchSteps, err := stringsArray(context["dispatch_steps"], "context mutation-step inventory", false)
	if err != nil {
		return err
	}
	plans, err := array(context["plans"], "context plans")
	if err != nil || len(plans) > MaxArtifactEntries {
		return recoveryError("run context plan inventory is malformed or oversized")
	}
	seenNames, seenSHA, seenJournal := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, raw := range plans {
		entry, err := exactWithOptional(raw, runContextPlanRequiredFields, runContextPlanOptionalFields, "context plan entry")
		if err != nil {
			return err
		}
		name, nameOK := entry["name"].(string)
		path, pathOK := entry["path"].(string)
		command, commandOK := entry["command"].(string)
		sha, shaOK := entry["sha256"].(string)
		journalID, journalOK := entry["journal_id"].(string)
		status, statusOK := entry["status"].(string)
		if !nameOK || !policyPlanName.MatchString(name) || seenNames[name] || !pathOK || path != "plans/"+name+".json" ||
			!commandOK || !nativeCommandPattern.MatchString(command) || !shaOK || !IsSHA256(sha) || seenSHA[sha] ||
			!journalOK || !IsSHA256(journalID) || seenJournal[journalID] || !statusOK || !contains([]string{"prepared", "dispatching", "completed"}, status) {
			return recoveryError("run context contains an invalid, duplicate or incomplete plan identity")
		}
		seenNames[name], seenSHA[sha], seenJournal[journalID] = true, true, true
		if _, pathSet := entry["review_path"]; pathSet {
			if entry["review_path"] != "reviews/"+name+".json" || !IsSHA256(entry["review_sha256"]) {
				return recoveryError("context plan review path or digest differs from its exact plan name")
			}
		} else if _, digestSet := entry["review_sha256"]; digestSet {
			return recoveryError("context plan review path and digest must be bound together")
		}
		for _, optionalPath := range []string{"journal_path", "apply_result_path"} {
			if value, exists := entry[optionalPath]; exists {
				if relative, ok := value.(string); !ok || !safePolicyRelativePath(relative) {
					return recoveryError("context plan contains an unsafe optional evidence path")
				}
			}
		}
	}
	publicationStage := ""
	if context["publication"] != nil {
		publication, err := object(context["publication"], "context publication")
		if err != nil {
			return err
		}
		publicationStage, _ = publication["stage"].(string)
		if !contains([]string{"push-pending", "pr-pending", "pr-verify-pending", "completed"}, publicationStage) || len(plans) != 0 {
			return recoveryError("run context publication stage or plan combination is unsupported")
		}
	}
	closeoutName, closeoutPolicy, hasCompletedMergeCloseout := completedMergeCloseout(workflowPolicy)
	composedMerge := false
	if hasCompletedMergeCloseout {
		for _, raw := range plans {
			entry, _ := object(raw, "context plan entry")
			if entry["name"] == closeoutName || entry["command"] == closeoutPolicy.command {
				composedMerge = true
				break
			}
		}
	}
	if composedMerge {
		if _, _, err := e.completedMergeContextEntries(context, closeoutPolicy.parentMerge); err != nil {
			return recoveryError("run context contains an invalid composed merge cleanup: %v", err)
		}
	}
	noopPlanPolicy, supportsWorkflowNoop := workflowPolicy.plans["workflow-noop"]
	noOpContext := supportsWorkflowNoop && noopPlanPolicy.profile == "workflow-noop" && len(dispatchSteps) == 0 && publicationStage == "" &&
		((len(plans) == 0 && (phase == "started" || phase == "prepared")) ||
			(len(plans) == 1 && phase == "completed" && contextPlanIsCompletedWorkflowNoop(plans[0])))
	hasWorkflowNoopPlan := false
	for _, raw := range plans {
		entry, _ := object(raw, "context plan entry")
		if entry["name"] == "workflow-noop" || entry["command"] == "workflow-noop" {
			hasWorkflowNoopPlan = true
		}
	}
	if hasWorkflowNoopPlan && !noOpContext {
		return recoveryError("workflow-noop plan is outside its exact completed no-op context")
	}
	if len(dispatchSteps) == 0 && publicationStage != "pr-verify-pending" && publicationStage != "completed" && !noOpContext {
		return recoveryError("run context mutation-step inventory is empty outside the exact workflow no-op lifecycle")
	}
	switch phase {
	case "started":
		if len(plans) != 0 || publicationStage != "" {
			return recoveryError("started run context unexpectedly contains a plan or publication")
		}
	case "prepared":
		if publicationStage != "" {
			if !contains([]string{"push-pending", "pr-pending", "pr-verify-pending"}, publicationStage) {
				return recoveryError("prepared publication has no pending exact stage")
			}
		} else {
			for index, raw := range plans {
				entry, _ := object(raw, "prepared context plan")
				if composedMerge && index == 0 && entry["status"] == "completed" {
					continue
				}
				if entry["status"] != "prepared" {
					return recoveryError("prepared run context contains a dispatched or completed plan")
				}
			}
		}
	case "completed":
		if publicationStage == "completed" {
			break
		}
		if publicationStage != "" || len(plans) == 0 {
			return recoveryError("completed run context lacks its exact terminal plan or publication")
		}
		for _, raw := range plans {
			entry, _ := object(raw, "completed context plan")
			if entry["status"] != "completed" {
				return recoveryError("completed run context has an unfinished plan")
			}
		}
	case "noop", "recovery_needed":
		if len(plans) != 0 || publicationStage != "" {
			return recoveryError("plan-free terminal context unexpectedly contains a plan or publication")
		}
		if phase == "noop" && supportsWorkflowNoop {
			return recoveryError("workflow no-op terminal state must contain its exact completed workflow-noop plan")
		}
	}
	if trusted, exists := context["trusted_source_sha"]; exists && !settlementSHA40.MatchString(fmt.Sprint(trusted)) {
		return recoveryError("run context trusted source SHA is invalid")
	}
	if context["recovered_from_run_id"] != nil || context["recovered_from_attempt"] != nil {
		if !positiveInt(context["recovered_from_run_id"]) || !positiveInt(context["recovered_from_attempt"]) {
			return recoveryError("run context has an incomplete recovery source identity")
		}
	}
	_, originRunPresent := context["plan_origin_run_id"]
	_, originAttemptPresent := context["plan_origin_attempt"]
	if originRunPresent || originAttemptPresent {
		if !positiveInt(context["recovered_from_run_id"]) || !positiveInt(context["recovered_from_attempt"]) ||
			!positiveInt(context["plan_origin_run_id"]) || !positiveInt(context["plan_origin_attempt"]) {
			return recoveryError("run context has an incomplete or non-recovered plan origin identity")
		}
		if exactInt(context["plan_origin_run_id"], mustPositive(context["workflow_run_id"])) &&
			mustPositive(context["plan_origin_attempt"]) >= mustPositive(context["workflow_run_attempt"]) {
			return recoveryError("run context plan origin is not an earlier exact attempt")
		}
		if exactInt(context["plan_origin_run_id"], mustPositive(context["recovered_from_run_id"])) &&
			mustPositive(context["plan_origin_attempt"]) > mustPositive(context["recovered_from_attempt"]) {
			return recoveryError("run context plan origin is ahead of its immediate source")
		}
	}
	if context["blocked_reason"] != nil && !nonemptyString(context["blocked_reason"]) {
		return recoveryError("run context blocked reason is malformed")
	}
	if context["branch_cleanup_outcome"] != nil {
		outcome, err := Exact(context["branch_cleanup_outcome"], []string{"path", "sha256"}, "branch cleanup outcome reference")
		if err != nil || outcome["path"] != "branch-cleanup-outcome.json" || !IsSHA256(outcome["sha256"]) {
			return recoveryError("run context branch cleanup outcome reference is invalid")
		}
	}
	if context["parent_merge"] != nil {
		if _, err := Exact(context["parent_merge"], []string{
			"run_id", "attempt", "artifact", "pull_request_number", "plan_sha256", "acknowledgement_sha256",
			"merge_commit_sha", "proof_path", "proof_sha256",
		}, "run context parent merge descriptor"); err != nil {
			return err
		}
	}
	return nil
}

// contextPlanOrigin returns the immutable attempt that owns the native plan
// and review. Direct recovery lineage remains in recovered_from_* and may
// advance independently on each interrupted observer.
func contextPlanOrigin(context Object, requireKnown bool) (int64, int64, error) {
	originRunValue, originRunPresent := context["plan_origin_run_id"]
	originAttemptValue, originAttemptPresent := context["plan_origin_attempt"]
	if originRunPresent != originAttemptPresent {
		return 0, 0, recoveryError("plan origin identity is incomplete")
	}
	if originRunPresent {
		runID, runErr := positiveInteger(originRunValue, "plan origin run ID")
		attempt, attemptErr := positiveInteger(originAttemptValue, "plan origin attempt")
		if runErr != nil || attemptErr != nil || context["recovered_from_run_id"] == nil || context["recovered_from_attempt"] == nil {
			return 0, 0, recoveryError("plan origin is not bound to a recovered context")
		}
		return runID, attempt, nil
	}
	if context["recovered_from_run_id"] != nil || context["recovered_from_attempt"] != nil {
		if requireKnown {
			return 0, 0, recoveryError("recovered context lacks its immutable plan origin")
		}
		runID, runErr := positiveInteger(context["recovered_from_run_id"], "recovery source run ID")
		attempt, attemptErr := positiveInteger(context["recovered_from_attempt"], "recovery source attempt")
		if runErr != nil || attemptErr != nil {
			return 0, 0, recoveryError("single-source plan origin cannot be derived from its exact recovery source")
		}
		return runID, attempt, nil
	}
	runID, runErr := positiveInteger(context["workflow_run_id"], "original plan run ID")
	attempt, attemptErr := positiveInteger(context["workflow_run_attempt"], "original plan attempt")
	if runErr != nil || attemptErr != nil {
		return 0, 0, recoveryError("original plan origin cannot be derived from its exact workflow context")
	}
	return runID, attempt, nil
}

func contextPlanIsCompletedWorkflowNoop(value any) bool {
	entry, err := Exact(value, runContextPlanRequiredFields, "workflow no-op context plan")
	return err == nil && entry["name"] == "workflow-noop" && entry["command"] == "workflow-noop" && entry["status"] == "completed"
}

func (e *Engine) validateTargetForContext(workflow, repository string, run Object) error {
	workflowID, err := positiveInteger(run["workflow_id"], "source workflow ID")
	if err != nil {
		return err
	}
	target, err := TargetObject(workflow, e.repository.URL, workflowID, "workflow-history-v2")
	if err != nil {
		return err
	}
	if !strings.EqualFold(fmt.Sprint(target["repository"]), repository) {
		return recoveryError("source context belongs to another repository")
	}
	_, err = e.validateTarget(target)
	return err
}

func readRecoverySource(root string) (Object, error) {
	data, err := ReadPackageFile(root, "recovery-source.json")
	if err != nil {
		return nil, recoveryError("immutable recovery source proof is missing or unsafe: %v", err)
	}
	value, err := DecodeValue(data)
	if err != nil {
		return nil, recoveryError("immutable recovery source proof is malformed: %v", err)
	}
	source, err := object(value, "immutable recovery source proof")
	if err != nil {
		return nil, err
	}
	fields := []string{"run_id", "attempt", "run", "artifact", "context_file", "plans", "policy_files"}
	optional := []string{"publication", "prepared_qualification"}
	source, err = exactWithOptional(source, fields, optional, "immutable recovery source proof")
	if err != nil {
		return nil, err
	}
	return source, nil
}

func contextFromProof(proof any, name string) (Object, error) {
	value, err := LoadFileProof(proof, name)
	if err != nil {
		return nil, err
	}
	context, err := object(value, name)
	if err != nil {
		return nil, err
	}
	return context, nil
}

func (e *Engine) loadContextPlan(root string, context, entry Object) (Object, error) {
	if err := e.validateRunContext(context); err != nil {
		return nil, err
	}
	name := fmt.Sprint(entry["name"])
	if _, err := exactWithOptional(entry, runContextPlanRequiredFields, runContextPlanOptionalFields, "context plan entry"); err != nil {
		return nil, err
	}
	path, err := PackageFile(root, fmt.Sprint(entry["path"]))
	if err != nil {
		return nil, err
	}
	value, err := LoadJSON(path)
	if err != nil {
		return nil, err
	}
	plan, err := object(value, "context native plan")
	if err != nil {
		return nil, err
	}
	parsed, err := contract.ParsePlan(plan)
	if err != nil || parsed.Command != entry["command"] || parsed.SHA256 != entry["sha256"] || journalIDForPlan(plan) != entry["journal_id"] || name == "" ||
		parsed.Repository.Host != e.repository.Host || !strings.EqualFold(parsed.Repository.FullName(), e.repository.Owner+"/"+e.repository.Name) {
		return nil, recoveryError("context plan bytes differ from their immutable exact command, digest or repository")
	}
	return plan, nil
}

func (e *Engine) terminalContextPlanProof(root string, entry, plan Object) (Object, error) {
	planBytes, err := ReadPackageFile(root, fmt.Sprint(entry["path"]))
	if err != nil {
		return nil, err
	}
	journalPath := "journal/" + fmt.Sprint(entry["journal_id"]) + ".json"
	if path, exists := entry["journal_path"]; exists {
		journalPath = fmt.Sprint(path)
	}
	journalBytes, err := ReadPackageFile(root, journalPath)
	if err != nil {
		return nil, recoveryError("terminal context plan journal is missing or unsafe: %v", err)
	}
	applyResultPath := "apply-results/" + fmt.Sprint(entry["name"]) + ".json"
	if path, exists := entry["apply_result_path"]; exists {
		applyResultPath = fmt.Sprint(path)
	}
	resultBytes, err := ReadPackageFile(root, applyResultPath)
	if err != nil {
		return nil, recoveryError("terminal context plan result is missing or unsafe: %v", err)
	}
	proof := Object{
		"name": entry["name"], "command": entry["command"], "plan_sha256": entry["sha256"], "journal_id": entry["journal_id"],
		"plan_file": MakeFileProof(planBytes), "journal_file": MakeFileProof(journalBytes), "apply_result_file": MakeFileProof(resultBytes),
	}
	decoded, err := ValidateTerminalPlanProof(proof)
	if err != nil || !Equal(decoded, plan) {
		return nil, recoveryError("plan completion lacks its exact terminal native evidence: %v", err)
	}
	return proof, nil
}

func contextPlanByName(context Object, name string) (Object, error) {
	if !policyPlanName.MatchString(name) {
		return nil, recoveryError("context plan name is invalid")
	}
	plans, err := array(context["plans"], "context plans")
	if err != nil {
		return nil, err
	}
	var match Object
	for _, raw := range plans {
		entry, err := object(raw, "context plan entry")
		if err != nil {
			return nil, err
		}
		if entry["name"] == name {
			if match != nil {
				return nil, recoveryError("context repeats the exact plan name")
			}
			match = entry
		}
	}
	if match == nil {
		return nil, recoveryError("context plan selection does not identify exactly one recorded plan")
	}
	return match, nil
}

func journalIDForPlan(plan Object) string {
	repository, err := nativePlanRepository(plan)
	if err != nil {
		return ""
	}
	identity := Object{
		"schema_version": int64(2), "repository": repository,
		"command": plan["command"], "plan_sha256": plan["sha256"],
	}
	data, err := Canonical(identity)
	if err != nil {
		return ""
	}
	return SHA256(data)
}

func confinedRootFile(root, relative string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !safePolicyRelativePath(relative) {
		return "", recoveryError("native journal root or relative file identity is invalid")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return "", recoveryError("native journal root must be an existing real directory")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", recoveryError("native journal root is missing or unsafe")
	}
	path, err := PackageFile(root, relative)
	if err == nil {
		return path, nil
	}
	if _, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(relative))); errors.Is(statErr, os.ErrNotExist) {
		return filepath.Join(root, filepath.FromSlash(relative)), nil
	}
	return "", recoveryError("native journal path is unsafe or unreadable: %v", err)
}

var contextKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
