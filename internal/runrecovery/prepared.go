package runrecovery

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

const preparedQualificationPath = "prepared-qualification.json"

const MaxPreparedFrontierAttempts = 8

var preparedQualificationFields = []string{
	"schema_version", "workflow_file", "repository", "run_id", "attempt", "run", "recovery_key",
	"attempt_target", "workflow_sha", "workflow_source_sha256", "plans", "workflow_source", "jobs",
}

var preparedSourceFields = []string{
	"run_id", "attempt", "run", "artifact", "context_file", "plans", "policy_files", "prepared_qualification",
}

var preparedPlanFields = []string{"name", "command", "plan_sha256", "journal_id", "plan_file", "journal_file", "apply_result_file"}

type PreparedQualificationOptions struct {
	Invocation
	WorkflowSHA string
}

// QualifyPrepared records positive source and workflow-step evidence for the
// current attempt only. It creates no plan, journal, result, or settlement.
func (e *Engine) QualifyPrepared(ctx context.Context, reader ActionsReader, options PreparedQualificationOptions) (Object, error) {
	if reader == nil || !nonemptyString(options.RunName) || !nonemptyString(options.RecoveryKey) || !settlementSHA40.MatchString(options.WorkflowSHA) {
		return nil, errors.New("prepared qualification requires exact current run, target and workflow source identities")
	}
	root, err := runnerDirectory(options.PackageRoot, options.RunnerTemp, false, false)
	if err != nil {
		return nil, err
	}
	contextValue, err := e.invocationContext(root, options.Invocation)
	if err != nil {
		return nil, err
	}
	cutover, err := e.previewOnlyCutoverAtRoot(root, options.Workflow)
	if err != nil {
		return nil, err
	}
	if cutover {
		return nil, errors.New("preview-only history cutover cannot qualify executable plans for recovery")
	}
	if err := e.validateRunContext(contextValue); err != nil {
		return nil, err
	}
	if contextValue["trusted_source_sha"] != options.WorkflowSHA {
		return nil, errors.New("prepared qualification differs from the exact trusted context workflow source")
	}
	if contextValue["publication"] != nil {
		return Object{"outcome": "not-needed", "reason": "publication recovery is qualified by its separate source proof"}, nil
	}
	entries, err := array(contextValue["plans"], "prepared qualification context plans")
	if err != nil || len(entries) == 0 {
		return Object{"outcome": "not-needed", "reason": "the current package has no native plan to qualify"}, nil
	}
	progress, err := e.detectPreparedNativeProgress(root, contextValue)
	if err != nil {
		return nil, err
	}
	if progress {
		return Object{"outcome": "not-needed", "reason": "positive native journal progress is handled by exact journal recovery"}, nil
	}
	pending, completed, err := e.preparedPlanProofs(root, contextValue)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return Object{"outcome": "not-needed", "reason": "all native plans already have terminal receipts"}, nil
	}
	if len(pending) > MaxArtifactEntries {
		return nil, errors.New("prepared native plan inventory exceeds its explicit bound")
	}
	workflowPolicy, err := e.workflowPolicy(options.Workflow)
	if err != nil {
		return nil, err
	}
	var sourceDigest string
	mutators := map[string]map[string]bool{}
	for _, plan := range pending {
		name := fmt.Sprint(plan["name"])
		policy, ok := workflowPolicy.plans[name]
		if !ok || policy.preparedRecovery == nil {
			return nil, fmt.Errorf("pending native plan %s has no trusted prepared-recovery policy", name)
		}
		if sourceDigest == "" {
			sourceDigest = policy.preparedRecovery.sourceSHA256
		} else if sourceDigest != policy.preparedRecovery.sourceSHA256 {
			return nil, errors.New("pending native plans do not agree on the exact workflow source digest")
		}
		for _, mutator := range policy.preparedRecovery.mutators {
			job := mutator["job"].(string)
			if mutators[job] == nil {
				mutators[job] = map[string]bool{}
			}
			for _, step := range mutator["steps"].([]string) {
				mutators[job][step] = true
			}
		}
	}
	if sourceDigest == "" {
		return nil, errors.New("prepared native plans have no qualified workflow source")
	}
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(preparedQualificationPath))); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("prepared qualification already exists or its path is unsafe")
	}
	runs, run, target, observed, attempts, err := e.invocationHistory(ctx, reader, options.Invocation)
	if err != nil {
		return nil, err
	}
	active := false
	for _, historyRun := range runs {
		if exactInt(historyRun["id"], options.RunID) && historyRun["status"] == "in_progress" {
			active = true
		}
	}
	if !active {
		return nil, errors.New("prepared qualification applies only to the currently executing workflow attempt")
	}
	chainValue, err := LoadJSON(filepath.Join(root, "settlement-chain.json"))
	if err != nil {
		return nil, err
	}
	chain, err := e.ValidateChain(chainValue, target, observed, attempts)
	if err != nil {
		return nil, err
	}
	if err := e.validatePreparedPredecessorFrontier(chain, observed, attempts, options.RunID, options.Attempt, contextValue); err != nil {
		return nil, err
	}
	observationValue, err := LoadJSON(filepath.Join(root, "recovery-observation.json"))
	if err != nil {
		return nil, err
	}
	if err := validatePreparedObservation(observationValue, options.Invocation, target, run, chain); err != nil {
		return nil, err
	}
	workflowSource, err := reader.Read(ctx, fmt.Sprintf("repos/%s/contents/.github/workflows/%s?ref=%s", e.repository.FullName(), options.Workflow, options.WorkflowSHA))
	if err != nil {
		return nil, err
	}
	workflowBytes, err := validatePreparedWorkflowSource(workflowSource, options.Workflow, sourceDigest)
	if err != nil {
		return nil, err
	}
	_ = workflowBytes
	jobPages, err := reader.Pages(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", e.repository.FullName(), options.RunID, options.Attempt))
	if err != nil {
		return nil, err
	}
	jobs := Object{"run_id": options.RunID, "attempt": options.Attempt, "pages": jobPages}
	if err := validatePreparedMutationJobs(jobs, mutators, options.RunID, options.Attempt); err != nil {
		return nil, err
	}
	qualificationPlans := make([]any, 0, len(pending))
	for index, item := range pending {
		identity := Object{"name": item["name"], "command": item["command"], "plan_sha256": item["plan_sha256"], "journal_id": item["journal_id"]}
		pending[index] = identity
		qualificationPlans = append(qualificationPlans, identity)
	}
	qualification := Object{
		"schema_version": int64(1), "workflow_file": options.Workflow, "repository": e.repository.FullName(),
		"run_id": options.RunID, "attempt": options.Attempt, "run": run, "recovery_key": options.RecoveryKey,
		"attempt_target": contextValue["attempt_target"], "workflow_sha": options.WorkflowSHA,
		"workflow_source_sha256": sourceDigest, "plans": qualificationPlans,
		"workflow_source": workflowSource, "jobs": jobs,
	}
	if err := validatePreparedQualification(qualification, target, contextValue, run, pending, workflowPolicy, false); err != nil {
		return nil, err
	}
	_ = completed
	if err := persistPackageJSON(root, preparedQualificationPath, qualification); err != nil {
		return nil, err
	}
	return Object{"outcome": "qualified", "plan_count": int64(len(pending)), "workflow_source_sha256": sourceDigest}, nil
}

func (e *Engine) detectPreparedNativeProgress(root string, context Object) (bool, error) {
	entries, err := array(context["plans"], "prepared recovery plans")
	if err != nil {
		return false, err
	}
	progressCount, pendingCount := 0, 0
	for _, raw := range entries {
		entry, err := object(raw, "prepared recovery plan entry")
		if err != nil {
			return false, err
		}
		if entry["status"] == "completed" {
			continue
		}
		pendingCount++
		name, command := fmt.Sprint(entry["name"]), fmt.Sprint(entry["command"])
		journalRelative := "journal/" + fmt.Sprint(entry["journal_id"]) + ".json"
		if value, exists := entry["journal_path"]; exists {
			journalRelative = fmt.Sprint(value)
		}
		journalPath := filepath.Join(root, filepath.FromSlash(journalRelative))
		if _, err := os.Lstat(journalPath); errors.Is(err, os.ErrNotExist) {
			resultRelative := "apply-results/" + name + ".json"
			if _, resultErr := os.Lstat(filepath.Join(root, filepath.FromSlash(resultRelative))); !errors.Is(resultErr, os.ErrNotExist) {
				return false, fmt.Errorf("unfinished plan %s has an apply result without a native journal", name)
			}
			continue
		} else if err != nil {
			return false, fmt.Errorf("unfinished plan %s has an unsafe journal path", name)
		}
		journalBytes, err := readRegularFile(journalPath)
		if err != nil {
			return false, fmt.Errorf("unfinished plan %s has an unreadable native journal", name)
		}
		journalValue, err := DecodeValue(journalBytes)
		if err != nil {
			return false, err
		}
		journal, err := object(journalValue, "prepared native journal")
		if err != nil {
			return false, err
		}
		planPath, err := PackageFile(root, fmt.Sprint(entry["path"]))
		if err != nil {
			return false, err
		}
		planBytes, err := readRegularFile(planPath)
		if err != nil {
			return false, err
		}
		planValue, err := DecodeValue(planBytes)
		if err != nil {
			return false, err
		}
		plan, err := object(planValue, "prepared native plan")
		if err != nil {
			return false, err
		}
		if err := e.ValidateRecoveryPlan(context, entry, plan, root); err != nil {
			return false, err
		}
		parsed, err := contract.ParsePlan(plan)
		if err != nil || parsed.Command != command {
			return false, errors.New("prepared native journal plan differs from its exact manifest")
		}
		count, err := validatePartialJournal(journal, parsed, fmt.Sprint(entry["journal_id"]))
		if err != nil || count == 0 {
			return false, errors.New("unfinished plan journal lacks a positive persisted dispatch receipt")
		}
		resultRelative := "apply-results/" + name + ".json"
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(resultRelative))); !errors.Is(err, os.ErrNotExist) {
			return false, errors.New("unfinished plan has an unbound apply result")
		}
		progressCount++
	}
	if progressCount != 0 && progressCount != pendingCount {
		return false, errors.New("mixed prepared and journaled progress is ambiguous without per-plan terminal proof")
	}
	return progressCount > 0, nil
}

func (e *Engine) preparedPlanProofs(root string, context Object) ([]Object, []Object, error) {
	entries, err := array(context["plans"], "prepared recovery plans")
	if err != nil {
		return nil, nil, err
	}
	pending, completed := []Object{}, []Object{}
	knownJournals, knownResults := map[string]bool{}, map[string]bool{}
	for _, raw := range entries {
		entry, err := object(raw, "prepared recovery plan entry")
		if err != nil {
			return nil, nil, err
		}
		planBytes, err := ReadPackageFile(root, fmt.Sprint(entry["path"]))
		if err != nil {
			return nil, nil, fmt.Errorf("prepared recovery plan %s is missing: %w", entry["name"], err)
		}
		planValue, err := DecodeValue(planBytes)
		if err != nil {
			return nil, nil, err
		}
		plan, err := object(planValue, "prepared recovery native plan")
		if err != nil || plan["name"] == "workflow-noop" || plan["command"] == "workflow-noop" {
			return nil, nil, errors.New("workflow no-op is outside prepared native recovery")
		}
		if err := e.ValidateRecoveryPlan(context, entry, plan, root); err != nil {
			return nil, nil, err
		}
		proof := Object{"name": entry["name"], "command": entry["command"], "plan_sha256": entry["sha256"], "journal_id": entry["journal_id"], "plan_file": MakeFileProof(planBytes), "journal_file": nil, "apply_result_file": nil}
		switch entry["status"] {
		case "prepared", "dispatching":
			if path, exists := entry["journal_path"]; exists && path != nil {
				return nil, nil, errors.New("undispatched plan manifest contains a journal path")
			}
			if path, exists := entry["apply_result_path"]; exists && path != nil {
				return nil, nil, errors.New("undispatched plan manifest contains an apply-result path")
			}
			journalRelative := "journal/" + fmt.Sprint(entry["journal_id"]) + ".json"
			resultRelative := "apply-results/" + fmt.Sprint(entry["name"]) + ".json"
			if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(journalRelative))); !errors.Is(err, os.ErrNotExist) {
				return nil, nil, errors.New("prepared plan has a journal, an unsafe journal path, or unknown apply progress")
			}
			if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(resultRelative))); !errors.Is(err, os.ErrNotExist) {
				return nil, nil, errors.New("prepared plan has an apply result, an unsafe result path, or unknown apply progress")
			}
			pending = append(pending, proof)
		case "completed":
			terminal, err := e.terminalContextPlanProof(root, entry, plan)
			if err != nil {
				return nil, nil, err
			}
			journalRelative, resultRelative := "journal/"+fmt.Sprint(entry["journal_id"])+".json", "apply-results/"+fmt.Sprint(entry["name"])+".json"
			if entry["journal_path"] != nil {
				journalRelative = fmt.Sprint(entry["journal_path"])
			}
			if entry["apply_result_path"] != nil {
				resultRelative = fmt.Sprint(entry["apply_result_path"])
			}
			knownJournals[journalRelative], knownResults[resultRelative] = true, true
			completed = append(completed, terminal)
		default:
			return nil, nil, errors.New("prepared qualification found an unsupported native plan status")
		}
	}
	files, err := retainedPackageFiles(root)
	if err != nil {
		return nil, nil, err
	}
	for _, file := range files {
		if strings.HasPrefix(file, "journal/") && !knownJournals[file] {
			return nil, nil, errors.New("prepared package contains unbound journal evidence")
		}
		if strings.HasPrefix(file, "apply-results/") && !knownResults[file] {
			return nil, nil, errors.New("prepared package contains unbound apply-result evidence")
		}
	}
	return pending, completed, nil
}

// BuildPreparedSourceRecord binds one uploaded historical package to its
// persisted no-dispatch qualification. The artifact identity is supplied only
// after Actions has returned the immutable upload metadata.
func (e *Engine) BuildPreparedSourceRecord(input Object) (Object, error) {
	fields, err := exactWithOptional(input,
		[]string{"target", "run_id", "attempt", "run", "artifact", "context_path", "plans", "qualification_path"},
		[]string{}, "prepared recovery source input")
	if err != nil {
		return nil, err
	}
	target, err := e.validateTarget(fields["target"])
	if err != nil {
		return nil, err
	}
	runID, err := positiveInteger(fields["run_id"], "prepared recovery source run ID")
	if err != nil {
		return nil, err
	}
	attempt, err := positiveInteger(fields["attempt"], "prepared recovery source attempt")
	if err != nil {
		return nil, err
	}
	run, err := Exact(fields["run"], immutableRunFields, "prepared recovery source run")
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
	contextPath, err := requiredAbsolutePath(fields["context_path"], "prepared recovery source context")
	if err != nil {
		return nil, err
	}
	contextFile, err := readPathProof(contextPath, "prepared recovery source context")
	if err != nil {
		return nil, err
	}
	contextValue, err := LoadFileProof(contextFile, "prepared recovery source context")
	if err != nil {
		return nil, err
	}
	sourceContext, err := object(contextValue, "prepared recovery source context")
	if err != nil {
		return nil, err
	}
	if err := e.validateRunContext(sourceContext); err != nil {
		return nil, err
	}
	if err := validateSourceContext(sourceContext, target, runID, attempt, run); err != nil {
		return nil, err
	}
	if sourceContext["publication"] != nil || (sourceContext["phase"] != "prepared" && sourceContext["phase"] != "dispatching") {
		return nil, errors.New("prepared recovery source has a publication or unsupported phase")
	}
	rawPlans, err := array(fields["plans"], "prepared recovery source plan inputs")
	if err != nil {
		return nil, err
	}
	contextPlans, err := array(sourceContext["plans"], "prepared recovery source context plans")
	if err != nil || len(rawPlans) == 0 || len(rawPlans) != len(contextPlans) {
		return nil, errors.New("prepared recovery source plan inventory differs from its context")
	}
	planProofs := make([]Object, 0, len(rawPlans))
	pendingIdentities := []Object{}
	knownJournals, knownResults := map[string]bool{}, map[string]bool{}
	retainedPolicyReader := &policyReader{root: filepath.Dir(contextPath), used: map[string]bool{}}
	for index, raw := range rawPlans {
		planInput, err := exactWithOptional(raw,
			[]string{"name", "command", "plan_sha256", "journal_id", "plan_path", "journal_path"},
			[]string{"apply_result_path"}, "prepared recovery source plan input")
		if err != nil {
			return nil, err
		}
		entry, err := exactWithOptional(contextPlans[index], runContextPlanRequiredFields, runContextPlanOptionalFields, "prepared recovery source plan entry")
		if err != nil {
			return nil, err
		}
		for field, key := range map[string]string{"name": "name", "command": "command", "plan_sha256": "sha256", "journal_id": "journal_id"} {
			if !Equal(planInput[field], entry[key]) {
				return nil, errors.New("prepared recovery source plan input differs from exact context")
			}
		}
		planPath, err := requiredAbsolutePath(planInput["plan_path"], "prepared recovery source plan")
		if err != nil {
			return nil, err
		}
		planFile, err := readPathProof(planPath, "prepared recovery source plan")
		if err != nil {
			return nil, err
		}
		planValue, err := LoadFileProof(planFile, "prepared recovery source plan")
		if err != nil {
			return nil, err
		}
		plan, err := object(planValue, "prepared recovery source plan")
		if err != nil {
			return nil, err
		}
		proof := Object{"name": entry["name"], "command": entry["command"], "plan_sha256": entry["sha256"], "journal_id": entry["journal_id"], "plan_file": planFile, "journal_file": nil, "apply_result_file": nil}
		switch entry["status"] {
		case "prepared", "dispatching":
			if planInput["journal_path"] != nil || planInput["apply_result_path"] != nil {
				return nil, errors.New("prepared source input names an unbound journal or result")
			}
			journalRelative := "journal/" + fmt.Sprint(entry["journal_id"]) + ".json"
			resultRelative := "apply-results/" + fmt.Sprint(entry["name"]) + ".json"
			if _, err := os.Lstat(filepath.Join(filepath.Dir(contextPath), filepath.FromSlash(journalRelative))); !errors.Is(err, os.ErrNotExist) {
				return nil, errors.New("prepared source contains a journal or unbound journal path")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(contextPath), filepath.FromSlash(resultRelative))); !errors.Is(err, os.ErrNotExist) {
				return nil, errors.New("prepared source contains an apply result or unbound result path")
			}
			pendingIdentities = append(pendingIdentities, Object{"name": entry["name"], "command": entry["command"], "plan_sha256": entry["sha256"], "journal_id": entry["journal_id"]})
		case "completed":
			journalPath, err := requiredAbsolutePath(planInput["journal_path"], "completed parent journal")
			if err != nil {
				return nil, err
			}
			resultPath, err := requiredAbsolutePath(planInput["apply_result_path"], "completed parent apply result")
			if err != nil {
				return nil, err
			}
			journal, err := readPathProof(journalPath, "completed parent journal")
			if err != nil {
				return nil, err
			}
			result, err := readPathProof(resultPath, "completed parent apply result")
			if err != nil {
				return nil, err
			}
			proof["journal_file"], proof["apply_result_file"] = journal, result
			if _, err := ValidateTerminalPlanProof(proof); err != nil {
				return nil, fmt.Errorf("completed parent plan lacks exact terminal evidence: %w", err)
			}
			journalRelative, resultRelative := "journal/"+fmt.Sprint(entry["journal_id"])+".json", "apply-results/"+fmt.Sprint(entry["name"])+".json"
			if entry["journal_path"] != nil {
				journalRelative = fmt.Sprint(entry["journal_path"])
			}
			if entry["apply_result_path"] != nil {
				resultRelative = fmt.Sprint(entry["apply_result_path"])
			}
			knownJournals[journalRelative], knownResults[resultRelative] = true, true
		default:
			return nil, errors.New("prepared recovery source contains an unsupported plan status")
		}
		if err := e.validateRecoveryPlanWithReader(sourceContext, entry, plan, retainedPolicyReader); err != nil {
			return nil, fmt.Errorf("prepared recovery plan is outside its exact policy: %w", err)
		}
		planProofs = append(planProofs, proof)
	}
	retainedFiles, err := retainedPackageFiles(filepath.Dir(contextPath))
	if err != nil {
		return nil, err
	}
	for _, relative := range retainedFiles {
		if strings.HasPrefix(relative, "journal/") && !knownJournals[relative] {
			return nil, errors.New("prepared source contains an unbound native journal")
		}
		if strings.HasPrefix(relative, "apply-results/") && !knownResults[relative] {
			return nil, errors.New("prepared source contains an unbound native apply result")
		}
	}
	if len(pendingIdentities) == 0 {
		return nil, errors.New("prepared recovery source has no pending native plan")
	}
	policyFiles, err := retainedPolicyFiles(filepath.Dir(contextPath), retainedPolicyReader)
	if err != nil {
		return nil, err
	}
	qualificationPath, err := requiredAbsolutePath(fields["qualification_path"], "prepared recovery qualification")
	if err != nil {
		return nil, err
	}
	qualificationBytes, err := readRegularFile(qualificationPath)
	if err != nil {
		return nil, errors.New("prepared recovery qualification is missing or unsafe")
	}
	qualificationValue, err := DecodeValue(qualificationBytes)
	if err != nil {
		return nil, err
	}
	workflowPolicy, err := e.workflowPolicy(fmt.Sprint(sourceContext["workflow_file"]))
	if err != nil {
		return nil, err
	}
	if err := validatePreparedQualification(qualificationValue, target, sourceContext, run, pendingIdentities, workflowPolicy, true); err != nil {
		return nil, err
	}
	planProofValues := make([]any, len(planProofs))
	for index, proof := range planProofs {
		planProofValues[index] = proof
	}
	return Object{
		"run_id": runID, "attempt": attempt, "run": run, "artifact": artifact,
		"context_file": contextFile, "plans": planProofValues, "policy_files": policyFiles,
		"prepared_qualification": MakeFileProof(qualificationBytes),
	}, nil
}

func validatePreparedWorkflowSource(packet Object, workflow, expectedDigest string) ([]byte, error) {
	if packet["type"] != "file" || packet["path"] != ".github/workflows/"+workflow || packet["encoding"] != "base64" {
		return nil, errors.New("prepared workflow source is not the exact trusted file")
	}
	content, ok := packet["content"].(string)
	if !ok || len(content) > MaxFileBytes*2 {
		return nil, errors.New("prepared workflow source is missing or oversized")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(strings.ReplaceAll(content, "\n", ""))
	if err != nil || len(raw) > MaxFileBytes || SHA256(raw) != expectedDigest {
		return nil, errors.New("prepared workflow source differs from the exact reviewed raw digest")
	}
	gitBlob := sha1.New()
	fmt.Fprintf(gitBlob, "blob %d%c", len(raw), 0)
	_, _ = gitBlob.Write(raw)
	if packet["sha"] != hex.EncodeToString(gitBlob.Sum(nil)) {
		return nil, errors.New("prepared workflow Git blob identity differs from its raw bytes")
	}
	return raw, nil
}

func validatePreparedMutationJobs(packet Object, required map[string]map[string]bool, runID, attempt int64) error {
	fields, err := Exact(packet, []string{"run_id", "attempt", "pages"}, "prepared current jobs proof")
	if err != nil || !exactInt(fields["run_id"], runID) || !exactInt(fields["attempt"], attempt) {
		return errors.New("prepared current jobs proof belongs to another exact attempt")
	}
	pages, err := array(fields["pages"], "prepared complete current jobs pages")
	if err != nil {
		return err
	}
	jobs, err := completePages(pages, "jobs")
	if err != nil {
		return err
	}
	for jobName, requiredSteps := range required {
		var matched Object
		for _, job := range jobs {
			if job["name"] != jobName {
				continue
			}
			if matched != nil || !exactInt(job["run_id"], runID) || !exactInt(job["run_attempt"], attempt) {
				return errors.New("prepared mutation job is duplicated or belongs to another exact attempt")
			}
			matched = job
		}
		if matched == nil || !contains([]string{"in_progress", "completed"}, fmt.Sprint(matched["status"])) {
			return errors.New("prepared mutation job is missing, queued or in an unknown state")
		}
		steps, err := array(matched["steps"], "prepared mutation job steps")
		if err != nil {
			return err
		}
		for stepName := range requiredSteps {
			count := 0
			for _, raw := range steps {
				step, err := object(raw, "prepared mutation step")
				if err != nil {
					return err
				}
				if step["name"] != stepName {
					continue
				}
				count++
				if step["status"] != "completed" || step["conclusion"] != "skipped" {
					return errors.New("prepared mutation-capable step was started or is not positively skipped")
				}
			}
			if count != 1 {
				return errors.New("prepared mutation step is missing or duplicated")
			}
		}
	}
	return nil
}

func validatePreparedQualification(value any, target, sourceContext, sourceRun Object, expectedPlans []Object, workflowPolicy workflowPolicy, allowPrevious bool) error {
	qualification, err := Exact(value, preparedQualificationFields, "prepared qualification")
	if err != nil {
		return err
	}
	runID, err := positiveInteger(qualification["run_id"], "prepared qualification run ID")
	if err != nil {
		return err
	}
	attempt, err := positiveInteger(qualification["attempt"], "prepared qualification attempt")
	if err != nil {
		return err
	}
	workflowSHA, ok := qualification["workflow_sha"].(string)
	if !ok || !settlementSHA40.MatchString(workflowSHA) || !exactInt(sourceContext["workflow_run_id"], runID) ||
		!exactInt(sourceContext["workflow_run_attempt"], attempt) || qualification["workflow_file"] != target["workflow_file"] ||
		!strings.EqualFold(fmt.Sprint(qualification["repository"]), fmt.Sprint(target["repository"])) ||
		qualification["recovery_key"] != sourceContext["recovery_key"] || !Equal(qualification["attempt_target"], sourceContext["attempt_target"]) ||
		workflowSHA != sourceContext["trusted_source_sha"] {
		return errors.New("prepared qualification changes its exact run, target or trusted source")
	}
	if !Equal(qualification["run"], sourceRun) {
		return errors.New("prepared qualification differs from its immutable source run")
	}
	rawPlans, err := array(qualification["plans"], "prepared qualification plan identities")
	if err != nil || len(rawPlans) != len(expectedPlans) {
		return errors.New("prepared qualification plan inventory differs from the exact pending plans")
	}
	for index, raw := range rawPlans {
		want := expectedPlans[index]
		got, err := Exact(raw, []string{"name", "command", "plan_sha256", "journal_id"}, "prepared qualification plan identity")
		if err != nil || !Equal(got["name"], want["name"]) || !Equal(got["command"], want["command"]) ||
			!Equal(got["plan_sha256"], want["plan_sha256"]) || !Equal(got["journal_id"], want["journal_id"]) {
			return errors.New("prepared qualification plan identity differs from its exact source plan")
		}
	}
	digest, ok := qualification["workflow_source_sha256"].(string)
	if !ok || !IsSHA256(digest) {
		return errors.New("prepared qualification workflow source digest is invalid")
	}
	mutators := map[string]map[string]bool{}
	for _, raw := range expectedPlans {
		name := fmt.Sprint(raw["name"])
		policy, exists := workflowPolicy.plans[name]
		if !exists || policy.preparedRecovery == nil {
			return fmt.Errorf("source plan %s is outside the exact prepared recovery policy", name)
		}
		inventory, allowed := policy.preparedRecovery.mutators, policy.preparedRecovery.sourceSHA256 == digest
		if !allowed && allowPrevious {
			inventory, allowed = policy.preparedRecovery.previous[digest]
		}
		if !allowed {
			return errors.New("prepared qualification source is neither the current source nor an explicitly retained prior proof")
		}
		for _, mutator := range inventory {
			job := mutator["job"].(string)
			if mutators[job] == nil {
				mutators[job] = map[string]bool{}
			}
			for _, step := range mutator["steps"].([]string) {
				mutators[job][step] = true
			}
		}
	}
	if _, err := validatePreparedWorkflowSource(mustObject(qualification["workflow_source"]), fmt.Sprint(target["workflow_file"]), digest); err != nil {
		return err
	}
	jobs, err := object(qualification["jobs"], "prepared complete jobs witness")
	if err != nil {
		return err
	}
	if err := validatePreparedMutationJobs(jobs, mutators, runID, attempt); err != nil {
		return err
	}
	return nil
}

func (e *Engine) validatePreparedSourceRecord(value any, target Object) (Object, error) {
	source, err := Exact(value, preparedSourceFields, "prepared recovery source record")
	if err != nil {
		return nil, err
	}
	runID, err := positiveInteger(source["run_id"], "prepared source run ID")
	if err != nil {
		return nil, err
	}
	attempt, err := positiveInteger(source["attempt"], "prepared source attempt")
	if err != nil {
		return nil, err
	}
	run, err := Exact(source["run"], immutableRunFields, "prepared source run")
	if err != nil {
		return nil, err
	}
	if _, err := validateRunIdentity(run, runID, attempt, target); err != nil {
		return nil, err
	}
	artifact, err := validateSourceArtifact(source["artifact"], target, runID, attempt)
	if err != nil {
		return nil, err
	}
	contextValue, err := LoadFileProof(source["context_file"], "prepared source run context")
	if err != nil {
		return nil, err
	}
	sourceContext, err := object(contextValue, "prepared source run context")
	if err != nil {
		return nil, err
	}
	if err := e.validateRunContext(sourceContext); err != nil {
		return nil, err
	}
	if err := validateSourceContext(sourceContext, target, runID, attempt, run); err != nil {
		return nil, err
	}
	if sourceContext["publication"] != nil || (sourceContext["phase"] != "prepared" && sourceContext["phase"] != "dispatching") {
		return nil, errors.New("prepared source context has a publication or unsupported phase")
	}
	entries, err := array(sourceContext["plans"], "prepared source context plans")
	if err != nil {
		return nil, err
	}
	proofs, err := array(source["plans"], "prepared source plan proofs")
	if err != nil || len(entries) == 0 || len(proofs) != len(entries) {
		return nil, errors.New("prepared source plan proof inventory differs from its context")
	}
	pending := []Object{}
	retained := make([]Object, 0, len(proofs))
	for index, raw := range proofs {
		proof, err := Exact(raw, preparedPlanFields, "prepared source native plan proof")
		if err != nil {
			return nil, err
		}
		entry, err := exactWithOptional(entries[index], runContextPlanRequiredFields, runContextPlanOptionalFields, "prepared source context plan")
		if err != nil {
			return nil, err
		}
		for field, contextField := range map[string]string{"name": "name", "command": "command", "plan_sha256": "sha256", "journal_id": "journal_id"} {
			if !Equal(proof[field], entry[contextField]) {
				return nil, errors.New("prepared source plan differs from its exact context entry")
			}
		}
		planValue, err := LoadFileProof(proof["plan_file"], "prepared source native plan")
		if err != nil {
			return nil, err
		}
		if _, err := object(planValue, "prepared source native plan"); err != nil {
			return nil, err
		}
		switch entry["status"] {
		case "prepared", "dispatching":
			if proof["journal_file"] != nil || proof["apply_result_file"] != nil {
				return nil, errors.New("prepared source plan proof contains a journal or apply result")
			}
			pending = append(pending, Object{"name": entry["name"], "command": entry["command"], "plan_sha256": entry["sha256"], "journal_id": entry["journal_id"]})
		case "completed":
			if proof["journal_file"] == nil || proof["apply_result_file"] == nil {
				return nil, errors.New("completed parent source lacks its exact journal or apply result")
			}
			if _, err := ValidateTerminalPlanProof(proof); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("prepared source context contains an unsupported plan status")
		}
		retained = append(retained, proof)
	}
	if len(pending) == 0 {
		return nil, errors.New("prepared source has no pending native plan")
	}
	policyFiles, err := object(source["policy_files"], "prepared source policy files")
	if err != nil {
		return nil, err
	}
	if err := e.validateRetainedPlanPolicyFiles(sourceContext, retained, policyFiles); err != nil {
		return nil, fmt.Errorf("prepared source plan policy evidence is invalid: %w", err)
	}
	qualification, err := LoadFileProof(source["prepared_qualification"], "prepared source qualification")
	if err != nil {
		return nil, err
	}
	workflowPolicy, err := e.workflowPolicy(fmt.Sprint(target["workflow_file"]))
	if err != nil {
		return nil, err
	}
	if err := validatePreparedQualification(qualification, target, sourceContext, run, pending, workflowPolicy, true); err != nil {
		return nil, err
	}
	return Object{
		"run_id": runID, "attempt": attempt, "run": run, "artifact": artifact,
		"context_file": source["context_file"], "plans": retained, "policy_files": policyFiles,
		"prepared_qualification": source["prepared_qualification"],
	}, nil
}

func validatePreparedObservation(value any, invocation Invocation, target, run, chain Object) error {
	fields := []string{"schema_version", "outcome", "target", "run", "run_id", "attempt", "recovery_key", "chain_sha256", "prepared_frontier_sha256"}
	observation, err := exactWithOptional(value, fields, []string{noopHistoryCutoverDigestField}, "prepared recovery observation")
	if err != nil {
		return err
	}
	var cutoverDigest any
	if isHistoryCutoverChain(chain) {
		baseline, err := historyCutoverFromChain(chain)
		if err != nil {
			return err
		}
		cutoverDigest = baseline["sha256"]
	}
	if !Equal(observation[noopHistoryCutoverDigestField], cutoverDigest) {
		return errors.New("prepared recovery observation lost its quarantined history identity")
	}
	settled, err := settlementPrefixDigest(chain["settlements"])
	if err != nil {
		return err
	}
	frontierDigest, err := Canonical(chain["prepared_frontier"])
	if err != nil {
		return err
	}
	frontierSHA := SHA256(frontierDigest)
	if !exactInt(observation["schema_version"], 1) || !contains([]string{"fresh", "resumed"}, fmt.Sprint(observation["outcome"])) ||
		!Equal(observation["target"], target) || !Equal(observation["run"], run) || !exactInt(observation["run_id"], invocation.RunID) ||
		!exactInt(observation["attempt"], invocation.Attempt) || observation["recovery_key"] != invocation.RecoveryKey ||
		observation["chain_sha256"] != settled || observation["prepared_frontier_sha256"] != frontierSHA {
		return errors.New("prepared recovery observation does not bind the current exact settlement and prepared frontiers")
	}
	return nil
}

func preparedFrontierDigest(frontier any) (string, error) {
	encoded, err := Canonical(frontier)
	if err != nil {
		return "", err
	}
	return SHA256(encoded), nil
}

func preparedRecoveryPlanInputs(root string, context Object) ([]Object, error) {
	entries, err := array(context["plans"], "prepared recovery context plans")
	if err != nil || len(entries) == 0 {
		return nil, errors.New("prepared recovery source has no exact plan inventory")
	}
	result := make([]Object, 0, len(entries))
	for _, raw := range entries {
		entry, err := object(raw, "prepared recovery context plan")
		if err != nil {
			return nil, err
		}
		name, nameOK := entry["name"].(string)
		journalID, journalOK := entry["journal_id"].(string)
		if !nameOK || !policyPlanName.MatchString(name) || !journalOK || !IsSHA256(journalID) {
			return nil, errors.New("prepared recovery context plan identity is invalid")
		}
		planPath, err := PackageFile(root, fmt.Sprint(entry["path"]))
		if err != nil {
			return nil, err
		}
		row := Object{"name": name, "command": entry["command"], "plan_sha256": entry["sha256"], "journal_id": journalID, "plan_path": planPath}
		if entry["status"] == "completed" {
			journalRelative := "journal/" + journalID + ".json"
			if value, exists := entry["journal_path"]; exists {
				journalRelative = fmt.Sprint(value)
			}
			resultRelative := "apply-results/" + name + ".json"
			if value, exists := entry["apply_result_path"]; exists {
				resultRelative = fmt.Sprint(value)
			}
			row["journal_path"], err = PackageFile(root, journalRelative)
			if err != nil {
				return nil, err
			}
			row["apply_result_path"], err = PackageFile(root, resultRelative)
			if err != nil {
				return nil, err
			}
		} else if entry["status"] == "prepared" || entry["status"] == "dispatching" {
			if _, exists := entry["journal_path"]; exists || entry["apply_result_path"] != nil {
				return nil, errors.New("pending prepared recovery plan has a journal or result path")
			}
			row["journal_path"], row["apply_result_path"] = nil, nil
		} else {
			return nil, errors.New("prepared recovery plan has an unsupported status")
		}
		result = append(result, row)
	}
	return result, nil
}

func mustObject(value any) Object {
	obj, _ := value.(map[string]any)
	return obj
}

func preparedFrontierEntry(kind string, source Object) Object {
	return Object{"kind": kind, "source": source}
}

func preparedSourceIdentity(source Object) (int64, int64, error) {
	runID, err := positiveInteger(source["run_id"], "prepared frontier source run ID")
	if err != nil {
		return 0, 0, err
	}
	attempt, err := positiveInteger(source["attempt"], "prepared frontier source attempt")
	return runID, attempt, err
}

func preparedSourceContext(source Object) (Object, error) {
	value, err := LoadFileProof(source["context_file"], "prepared frontier source context")
	if err != nil {
		return nil, err
	}
	return object(value, "prepared frontier source context")
}

func preparedSourcePlans(source Object) ([]Object, error) {
	rows, err := array(source["plans"], "prepared frontier source plans")
	if err != nil || len(rows) == 0 {
		return nil, errors.New("prepared frontier source has no plan inventory")
	}
	result := make([]Object, 0, len(rows))
	for _, raw := range rows {
		plan, err := object(raw, "prepared frontier source plan")
		if err != nil {
			return nil, err
		}
		result = append(result, plan)
	}
	return result, nil
}

func preparedPlanIdentities(plans []Object) []any {
	identities := make([]any, 0, len(plans))
	for _, plan := range plans {
		identities = append(identities, Object{"name": plan["name"], "command": plan["command"], "plan_sha256": plan["plan_sha256"], "journal_id": plan["journal_id"]})
	}
	return identities
}

func validatePreparedObserverPlanRetention(source Object, observerPlans []Object) error {
	sourcePlans, err := preparedSourcePlans(source)
	if err != nil {
		return err
	}
	context, err := preparedSourceContext(source)
	if err != nil {
		return err
	}
	entries, err := array(context["plans"], "prepared source plan contexts")
	if err != nil || len(entries) != len(sourcePlans) {
		return errors.New("prepared source plan contexts differ from retained proof inventory")
	}
	byName := make(map[string]Object, len(observerPlans))
	for _, proof := range observerPlans {
		name := fmt.Sprint(proof["name"])
		if !policyPlanName.MatchString(name) || byName[name] != nil {
			return errors.New("terminal observer repeats or malforms a retained plan identity")
		}
		byName[name] = proof
	}
	for index, old := range sourcePlans {
		entry, err := object(entries[index], "prepared source plan context")
		if err != nil {
			return err
		}
		current := byName[fmt.Sprint(old["name"])]
		if current == nil || !Equal(preparedPlanIdentities([]Object{old}), preparedPlanIdentities([]Object{current})) ||
			!Equal(old["plan_file"], current["plan_file"]) {
			return errors.New("terminal observer changed the exact source plan bytes or identity")
		}
		if entry["status"] == "completed" {
			if !Equal(old["journal_file"], current["journal_file"]) || !Equal(old["apply_result_file"], current["apply_result_file"]) {
				return errors.New("terminal observer changed completed parent journal or apply-result bytes")
			}
			continue
		}
		if old["journal_file"] == nil {
			continue
		}
		if current["journal_file"] == nil {
			return errors.New("terminal observer lost a positively persisted native journal")
		}
		oldBytes, err := LoadRawFileProof(old["journal_file"], "prepared source partial journal")
		if err != nil {
			return err
		}
		currentBytes, err := LoadRawFileProof(current["journal_file"], "terminal observer journal")
		if err != nil {
			return err
		}
		oldValue, err := DecodeValue(oldBytes)
		if err != nil {
			return err
		}
		currentValue, err := DecodeValue(currentBytes)
		if err != nil {
			return err
		}
		oldJournal, err := object(oldValue, "prepared source partial journal")
		if err != nil {
			return err
		}
		currentJournal, err := object(currentValue, "terminal observer journal")
		if err != nil || !Equal(oldJournal["identity"], currentJournal["identity"]) || oldJournal["result"] != nil {
			return errors.New("terminal observer changed or completed an earlier partial-journal identity")
		}
		oldSteps, err := array(oldJournal["steps"], "prepared source partial journal steps")
		currentSteps, currentErr := array(currentJournal["steps"], "terminal observer journal steps")
		if err != nil || currentErr != nil || len(oldSteps) > len(currentSteps) {
			return errors.New("terminal observer journal does not preserve the exact source progress prefix")
		}
		for stepIndex := range oldSteps {
			oldStep, err := object(oldSteps[stepIndex], "prepared source journal step")
			if err != nil {
				return err
			}
			currentStep, err := object(currentSteps[stepIndex], "terminal observer journal step")
			if err != nil {
				return err
			}
			if Equal(oldStep, currentStep) {
				continue
			}
			// A prior positive acknowledgement can be followed by an actual
			// terminal observer's independently verified after-state receipt. Keep
			// the original dispatch identity and acknowledgement byte-for-byte;
			// only the exact unknown step may advance monotonically to completed.
			if oldStep["status"] != "unknown" || currentStep["status"] != "completed" ||
				oldStep["result"] != nil || oldStep["completed_at"] != nil || oldStep["observation"] != nil || currentStep["observation"] == nil {
				return errors.New("terminal observer changed or replayed an earlier native operation receipt")
			}
			for _, field := range []string{"id", "intent", "intent_sha256", "operation_id", "started_at"} {
				if !Equal(oldStep[field], currentStep[field]) {
					return errors.New("terminal observer changed an earlier native dispatch identity")
				}
			}
			if !Equal(oldStep["acknowledgement"], currentStep["acknowledgement"]) {
				return errors.New("terminal observer changed or fabricated an earlier native acknowledgement")
			}
			observation, err := object(currentStep["observation"], "terminal observer positive reconciliation")
			if err != nil || observation["positive_identity"] != true || observation["after_state_verified"] != true ||
				observation["operation_id"] != oldStep["operation_id"] || !nonemptyString(observation["reference"]) {
				return errors.New("terminal observer did not positively reconcile the exact earlier dispatch")
			}
		}
	}
	return nil
}

// validatePreparedSourceLineage checks the raw attempt contexts as retained;
// it never synthesizes or rewrites recovered_from fields.
func validatePreparedSourceLineage(frontier []any) ([]Object, error) {
	validated := make([]Object, 0, len(frontier))
	var previous Object
	var priorPlans []Object
	var priorContext Object
	var planOriginRunID, planOriginAttempt int64
	for _, raw := range frontier {
		entry, err := Exact(raw, []string{"kind", "source"}, "prepared frontier entry")
		if err != nil || !contains([]string{"prepared", "journaled"}, fmt.Sprint(entry["kind"])) {
			return nil, errors.New("prepared frontier has an unsupported or malformed source kind")
		}
		source, err := object(entry["source"], "prepared frontier source")
		if err != nil {
			return nil, err
		}
		if entry["kind"] == "prepared" {
			if _, err := exactWithOptional(source, preparedSourceFields, nil, "prepared frontier qualified source"); err != nil {
				return nil, err
			}
		} else if _, err := exactWithOptional(source,
			[]string{"run_id", "attempt", "run", "artifact", "context_file", "plans", "policy_files"}, nil,
			"prepared frontier journaled source"); err != nil {
			return nil, err
		}
		runID, attempt, err := preparedSourceIdentity(source)
		if err != nil {
			return nil, err
		}
		context, err := preparedSourceContext(source)
		if err != nil || context["publication"] != nil || !exactInt(context["workflow_run_id"], runID) || !exactInt(context["workflow_run_attempt"], attempt) {
			return nil, errors.New("prepared frontier context changes its exact run or contains publication")
		}
		originRunID, originAttempt, err := contextPlanOrigin(context, true)
		if err != nil {
			return nil, errors.New("prepared frontier context lacks its exact immutable plan origin")
		}
		if previous == nil {
			if context["recovered_from_run_id"] != nil || context["recovered_from_attempt"] != nil {
				return nil, errors.New("prepared frontier begins with an observer whose original source is absent")
			}
			if originRunID != runID || originAttempt != attempt {
				return nil, errors.New("prepared frontier origin differs from its original source attempt")
			}
			planOriginRunID, planOriginAttempt = originRunID, originAttempt
		} else {
			prevID, prevAttempt, err := preparedSourceIdentity(previous)
			if err != nil || !exactInt(context["recovered_from_run_id"], prevID) || !exactInt(context["recovered_from_attempt"], prevAttempt) {
				return nil, errors.New("prepared frontier observer does not directly retain its exact prior attempt lineage")
			}
			if originRunID != planOriginRunID || originAttempt != planOriginAttempt {
				return nil, errors.New("prepared frontier observer changed the exact immutable plan origin")
			}
			previousRun, _ := object(previous["run"], "prior prepared source run")
			currentRun, _ := object(source["run"], "current prepared source run")
			previousTime, currentTime := fmt.Sprint(previousRun["created_at"]), fmt.Sprint(currentRun["created_at"])
			ordered := currentTime > previousTime || (currentTime == previousTime &&
				(runID > prevID || (runID == prevID && attempt > prevAttempt)))
			if !ordered {
				return nil, errors.New("prepared frontier sources are not in strict workflow chronology")
			}
		}
		plans, err := preparedSourcePlans(source)
		if err != nil {
			return nil, err
		}
		if priorPlans != nil {
			if !Equal(preparedPlanIdentities(priorPlans), preparedPlanIdentities(plans)) {
				return nil, errors.New("prepared frontier changes its exact native plan inventory")
			}
			oldEntries, _ := array(priorContext["plans"], "prepared previous context plans")
			newEntries, _ := array(context["plans"], "prepared current context plans")
			for index := range plans {
				if !Equal(priorPlans[index]["plan_file"], plans[index]["plan_file"]) {
					return nil, errors.New("prepared frontier changed exact native plan bytes")
				}
				oldEntry, _ := object(oldEntries[index], "prepared previous context plan")
				newEntry, _ := object(newEntries[index], "prepared current context plan")
				if oldEntry["status"] == "completed" {
					if newEntry["status"] != "completed" || !Equal(priorPlans[index]["journal_file"], plans[index]["journal_file"]) ||
						!Equal(priorPlans[index]["apply_result_file"], plans[index]["apply_result_file"]) {
						return nil, errors.New("prepared frontier lost completed parent journal or apply-result bytes")
					}
				}
			}
		}
		validated = append(validated, source)
		previous, priorPlans, priorContext = source, plans, context
	}
	return validated, nil
}

func (e *Engine) validateJournaledPreparedSource(source Object, target Object) error {
	fields, err := Exact(source, []string{"run_id", "attempt", "run", "artifact", "context_file", "plans", "policy_files"}, "journaled prepared source")
	if err != nil {
		return err
	}
	runID, err := positiveInteger(fields["run_id"], "journaled source run ID")
	if err != nil {
		return err
	}
	attempt, err := positiveInteger(fields["attempt"], "journaled source attempt")
	if err != nil {
		return err
	}
	run, err := validateRunIdentity(fields["run"], runID, attempt, target)
	if err != nil {
		return err
	}
	if _, err := validateSourceArtifact(fields["artifact"], target, runID, attempt); err != nil {
		return err
	}
	contextValue, err := LoadFileProof(fields["context_file"], "journaled source context")
	if err != nil {
		return err
	}
	context, err := object(contextValue, "journaled source context")
	if err != nil || e.validateRunContext(context) != nil || validateSourceContext(context, target, runID, attempt, run) != nil || context["publication"] != nil || context["phase"] != "dispatching" {
		return errors.New("journaled source context is not an exact interrupted native attempt")
	}
	entries, err := array(context["plans"], "journaled source context plans")
	proofs, proofErr := array(fields["plans"], "journaled source plan proofs")
	if err != nil || proofErr != nil || len(entries) == 0 || len(entries) != len(proofs) {
		return errors.New("journaled source proofs differ from the exact context plan inventory")
	}
	policyFiles, err := object(fields["policy_files"], "journaled source policy files")
	if err != nil {
		return err
	}
	retained := make([]Object, 0, len(proofs))
	persisted := 0
	for index, raw := range proofs {
		proof, err := exactWithOptional(raw,
			[]string{"name", "command", "plan_sha256", "journal_id", "plan_file", "journal_file"},
			[]string{"apply_result_file"}, "journaled source plan proof")
		if err != nil {
			return err
		}
		entry, err := exactWithOptional(entries[index], runContextPlanRequiredFields, runContextPlanOptionalFields, "journaled source context plan")
		if err != nil {
			return err
		}
		for field, key := range map[string]string{"name": "name", "command": "command", "plan_sha256": "sha256", "journal_id": "journal_id"} {
			if !Equal(proof[field], entry[key]) {
				return errors.New("journaled source plan identity differs from its exact context")
			}
		}
		planValue, err := LoadFileProof(proof["plan_file"], "journaled source plan")
		if err != nil {
			return err
		}
		plan, err := object(planValue, "journaled source plan")
		if err != nil {
			return err
		}
		policyReader := &policyReader{proofs: policyFiles, used: map[string]bool{}}
		if err := e.validateRecoveryPlanWithReader(context, entry, plan, policyReader); err != nil {
			return err
		}
		if entry["status"] == "completed" {
			if proof["apply_result_file"] == nil {
				return errors.New("completed parent lost its exact apply-result bytes")
			}
			if _, err := ValidateTerminalPlanProof(proof); err != nil {
				return err
			}
		} else {
			if proof["apply_result_file"] != nil || proof["journal_file"] == nil {
				return errors.New("journaled source lacks a durable journal or carries an unbound result")
			}
			journalBytes, err := LoadRawFileProof(proof["journal_file"], "journaled source journal")
			if err != nil {
				return err
			}
			journalValue, err := DecodeValue(journalBytes)
			if err != nil {
				return err
			}
			journal, err := object(journalValue, "journaled source journal")
			if err != nil {
				return err
			}
			parsed, err := contract.ParsePlan(plan)
			if err != nil {
				return err
			}
			dispatches, err := validatePartialJournal(journal, parsed, fmt.Sprint(entry["journal_id"]))
			if err != nil {
				return err
			}
			persisted += dispatches
		}
		retained = append(retained, proof)
	}
	if persisted == 0 {
		return errors.New("journaled source contains no positive native dispatch receipt")
	}
	return e.validateRetainedPlanPolicyFiles(context, retained, policyFiles)
}

func allUnsettledAttempts(observed []Object, latest map[int64]int64, covered map[int64]Object, quarantined map[int64]int64) []Object {
	rows := []Object{}
	for _, run := range observed {
		id := mustPositive(run["id"])
		first := quarantined[id] + 1
		if item := covered[id]; item != nil {
			first = mustPositive(item["settled_attempt"]) + 1
		}
		for attempt := first; attempt <= latest[id]; attempt++ {
			rows = append(rows, Object{"run_id": id, "attempt": attempt, "run": run})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		runA, runB := a["run"].(map[string]any), b["run"].(map[string]any)
		if runA["created_at"] != runB["created_at"] {
			return fmt.Sprint(runA["created_at"]) < fmt.Sprint(runB["created_at"])
		}
		if mustPositive(a["run_id"]) != mustPositive(b["run_id"]) {
			return mustPositive(a["run_id"]) < mustPositive(b["run_id"])
		}
		return mustPositive(a["attempt"]) < mustPositive(b["attempt"])
	})
	return rows
}

func (e *Engine) validatePreparedFrontier(chain Object, target Object, observed []Object, latest map[int64]int64, covered map[int64]Object, recordsByRun map[int64][]int64, quarantined map[int64]int64) error {
	frontier, _ := array(chain["prepared_frontier"], "prepared source frontier")
	proofs, _ := array(chain["prepared_terminal_proofs"], "prepared terminal proof inventory")
	ordered, err := validatePreparedSourceLineage(frontier)
	if err != nil {
		return err
	}
	for index, raw := range frontier {
		entry := raw.(map[string]any)
		source := entry["source"].(map[string]any)
		switch entry["kind"] {
		case "prepared":
			if _, err := e.validatePreparedSourceRecord(source, target); err != nil {
				return fmt.Errorf("prepared frontier source is not positively qualified: %w", err)
			}
		case "journaled":
			if err := e.validateJournaledPreparedSource(source, target); err != nil {
				return fmt.Errorf("prepared frontier journaled observer is not positively proved: %w", err)
			}
		}
		runID, attempt, err := preparedSourceIdentity(source)
		if err != nil {
			return err
		}
		all := allUnsettledAttempts(observed, latest, covered, quarantined)
		if index >= len(all) || !exactInt(all[index]["run_id"], runID) || !exactInt(all[index]["attempt"], attempt) {
			return errors.New("prepared frontier skips or changes an earlier unsettled attempt")
		}
		if int64(len(recordsByRun[runID]))+quarantined[runID] >= attempt {
			return errors.New("prepared frontier repeats an already settled attempt")
		}
	}
	if err := e.validatePreparedTerminalTable(proofs, chain["settlements"].([]any), target, observed); err != nil {
		return err
	}
	_ = ordered
	return nil
}

func (e *Engine) validatePreparedPredecessorFrontier(chain Object, observed []Object, latest map[int64]int64, currentID, currentAttempt int64, context Object) error {
	frontier, err := array(chain["prepared_frontier"], "prepared predecessor frontier")
	if err != nil {
		return err
	}
	pending, err := PendingAttempts(chain, observed, latest, currentID, currentAttempt)
	if err != nil || len(pending) != len(frontier) {
		return errors.New("current prepared qualification cannot skip any unsettled predecessor")
	}
	if len(frontier) == 0 {
		if context["recovered_from_run_id"] != nil || context["recovered_from_attempt"] != nil {
			return errors.New("recovered attempt has no durable prepared frontier for its retained source")
		}
		return nil
	}
	last, err := Exact(frontier[len(frontier)-1], []string{"kind", "source"}, "latest prepared predecessor")
	if err != nil {
		return err
	}
	source, err := object(last["source"], "latest prepared source")
	if err != nil {
		return err
	}
	runID, attempt, err := preparedSourceIdentity(source)
	if err != nil || !exactInt(context["recovered_from_run_id"], runID) || !exactInt(context["recovered_from_attempt"], attempt) {
		return errors.New("current observer context differs from the exact latest prepared source")
	}
	sources, err := validatePreparedSourceLineage(frontier)
	if err != nil {
		return err
	}
	firstContext, err := preparedSourceContext(sources[0])
	if err != nil {
		return err
	}
	wantOriginRunID, wantOriginAttempt, err := contextPlanOrigin(firstContext, true)
	if err != nil {
		return err
	}
	gotOriginRunID, gotOriginAttempt, err := contextPlanOrigin(context, true)
	if err != nil || gotOriginRunID != wantOriginRunID || gotOriginAttempt != wantOriginAttempt {
		return errors.New("current observer context changed the exact immutable plan origin")
	}
	return nil
}

func (e *Engine) validatePreparedTerminalTable(proofs, settlements []any, target Object, observed []Object) error {
	byAttempt := map[string]Object{}
	settlementIndex := map[string]int{}
	for index, raw := range settlements {
		row, err := object(raw, "prepared settlement row")
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%d:%d", mustPositive(row["run_id"]), mustPositive(row["attempt"]))
		byAttempt[key], settlementIndex[key] = row, index
	}
	used := map[string]map[int64]bool{}
	proofDigests := map[string]bool{}
	for _, raw := range proofs {
		proof, err := Exact(raw, []string{"schema_version", "sources", "observer_run_id", "observer_attempt", "plan_origin_run_id", "plan_origin_attempt", "plan_identities", "sha256"}, "prepared terminal proof bundle")
		if err != nil || !exactInt(proof["schema_version"], 1) {
			return errors.New("prepared terminal proof bundle has an unsupported schema")
		}
		unsigned := Object{}
		for _, field := range []string{"schema_version", "sources", "observer_run_id", "observer_attempt", "plan_origin_run_id", "plan_origin_attempt", "plan_identities"} {
			unsigned[field] = proof[field]
		}
		encoded, err := Canonical(unsigned)
		digest, ok := proof["sha256"].(string)
		if err != nil || !ok || !IsSHA256(digest) || SHA256(encoded) != digest || proofDigests[digest] {
			return errors.New("prepared terminal proof digest is invalid or duplicated")
		}
		proofDigests[digest] = true
		sources, err := array(proof["sources"], "prepared terminal source entries")
		if err != nil || len(sources) == 0 || len(sources) > MaxPreparedFrontierAttempts {
			return errors.New("prepared terminal source inventory is empty or exceeds its explicit bound")
		}
		validatedSources, err := validatePreparedSourceLineage(sources)
		if err != nil {
			return err
		}
		observerID, err := positiveInteger(proof["observer_run_id"], "prepared terminal observer run ID")
		if err != nil {
			return err
		}
		observerAttempt, err := positiveInteger(proof["observer_attempt"], "prepared terminal observer attempt")
		if err != nil {
			return err
		}
		planOriginRunID, err := positiveInteger(proof["plan_origin_run_id"], "prepared terminal plan origin run ID")
		if err != nil {
			return err
		}
		planOriginAttempt, err := positiveInteger(proof["plan_origin_attempt"], "prepared terminal plan origin attempt")
		if err != nil {
			return err
		}
		firstContext, err := preparedSourceContext(validatedSources[0])
		firstOriginRunID, firstOriginAttempt, originErr := contextPlanOrigin(firstContext, true)
		if err != nil || originErr != nil || firstOriginRunID != planOriginRunID || firstOriginAttempt != planOriginAttempt {
			return errors.New("prepared terminal proof differs from its exact immutable source plan origin")
		}
		observerKey := fmt.Sprintf("%d:%d", observerID, observerAttempt)
		observerRecord := byAttempt[observerKey]
		if observerRecord == nil || !Equal(observerRecord["run"], observedRun(observed, observerID)) {
			return errors.New("prepared terminal proof has no exact observer record in complete history")
		}
		observerSettlement, err := object(observerRecord["settlement"], "prepared terminal observer settlement")
		if err != nil || observerSettlement["kind"] != "terminal" || observerSettlement["publication"] != nil {
			return errors.New("prepared terminal proof is not closed by an actual native terminal observer receipt")
		}
		observerPlansRaw, err := array(observerSettlement["plans"], "prepared terminal observer plans")
		if err != nil || len(observerPlansRaw) == 0 {
			return errors.New("prepared terminal observer lacks actual native plan receipts")
		}
		observerPlans := make([]Object, 0, len(observerPlansRaw))
		for _, rawPlan := range observerPlansRaw {
			plan, err := object(rawPlan, "prepared terminal observer plan")
			if err != nil {
				return err
			}
			observerPlans = append(observerPlans, plan)
		}
		identities := preparedPlanIdentities(observerPlans)
		if !Equal(identities, proof["plan_identities"]) {
			return errors.New("prepared terminal proof plan inventory differs from actual observer receipts")
		}
		var previous Object
		for index, rawSourceEntry := range sources {
			sourceEntry, err := Exact(rawSourceEntry, []string{"kind", "source"}, "prepared terminal source entry")
			if err != nil || !contains([]string{"prepared", "journaled"}, fmt.Sprint(sourceEntry["kind"])) {
				return errors.New("prepared terminal source kind is invalid")
			}
			source, err := object(sourceEntry["source"], "prepared terminal source")
			if err != nil {
				return err
			}
			if sourceEntry["kind"] == "prepared" {
				if _, err := e.validatePreparedSourceRecord(source, target); err != nil {
					return fmt.Errorf("prepared terminal source qualification is invalid: %w", err)
				}
			} else if err := e.validateJournaledPreparedSource(source, target); err != nil {
				return err
			}
			plans, err := preparedSourcePlans(source)
			if err != nil || !Equal(preparedPlanIdentities(plans), identities) {
				return errors.New("prepared terminal source changes the exact native plan set")
			}
			if err := validatePreparedObserverPlanRetention(source, observerPlans); err != nil {
				return err
			}
			if !Equal(source["policy_files"], observerSettlement["policy_files"]) {
				return errors.New("prepared terminal observer changed exact approval or review policy files")
			}
			sourceID, sourceAttempt, err := preparedSourceIdentity(source)
			if err != nil {
				return err
			}
			rowKey := fmt.Sprintf("%d:%d", sourceID, sourceAttempt)
			row := byAttempt[rowKey]
			if row == nil || settlementIndex[rowKey] >= settlementIndex[observerKey] {
				return errors.New("prepared terminal proof source does not precede its terminal observer")
			}
			rowSettlement, err := object(row["settlement"], "prepared source settlement")
			if err != nil || rowSettlement["kind"] != "prepared_terminal" || rowSettlement["proof_sha256"] != digest || !exactInt(rowSettlement["source_index"], int64(index)) {
				return errors.New("prepared source settlement does not reference its exact multi-attempt proof")
			}
			if !Equal(row["run"], source["run"]) || !Equal(row["artifact"], source["artifact"]) || !Equal(row["context_file"], source["context_file"]) {
				return errors.New("prepared source settlement changes its exact artifact or raw context")
			}
			context, err := preparedSourceContext(source)
			if err != nil {
				return err
			}
			attemptTarget, err := object(row["attempt_target"], "prepared source settlement target")
			if err != nil || attemptTarget["recovery_key"] != context["recovery_key"] || !Equal(attemptTarget["identity"], context["attempt_target"]) {
				return errors.New("prepared source settlement changes its original attempt target")
			}
			if previous != nil {
				prevID, prevAttempt, _ := preparedSourceIdentity(previous)
				if !exactInt(context["recovered_from_run_id"], prevID) || !exactInt(context["recovered_from_attempt"], prevAttempt) {
					return errors.New("prepared terminal proof loses an intermediate observer lineage")
				}
			}
			if used[digest] == nil {
				used[digest] = map[int64]bool{}
			}
			if used[digest][int64(index)] {
				return errors.New("prepared terminal source attempt is settled more than once")
			}
			used[digest][int64(index)] = true
			previous = source
		}
		observerContextValue, err := LoadFileProof(observerRecord["context_file"], "prepared terminal observer context")
		observerContext, contextErr := object(observerContextValue, "prepared terminal observer context")
		if err != nil || contextErr != nil || previous == nil {
			return errors.New("prepared terminal observer context is missing")
		}
		observerOriginRunID, observerOriginAttempt, originErr := contextPlanOrigin(observerContext, true)
		if originErr != nil || observerOriginRunID != planOriginRunID || observerOriginAttempt != planOriginAttempt {
			return errors.New("prepared terminal observer changed the exact immutable plan origin")
		}
		prevID, prevAttempt, _ := preparedSourceIdentity(previous)
		if !exactInt(observerContext["recovered_from_run_id"], prevID) || !exactInt(observerContext["recovered_from_attempt"], prevAttempt) {
			return errors.New("terminal observer context does not bind the latest exact source attempt")
		}
	}
	for _, raw := range settlements {
		row, _ := object(raw, "settlement row")
		settlement, _ := object(row["settlement"], "settlement evidence")
		if settlement["kind"] != "prepared_terminal" {
			continue
		}
		digest := fmt.Sprint(settlement["proof_sha256"])
		index := mustNonnegative(settlement["source_index"])
		if !proofDigests[digest] || !used[digest][index] {
			return errors.New("prepared-terminal settlement references no exact shared terminal proof")
		}
	}
	for digest := range proofDigests {
		for index := int64(0); index < int64(len(used[digest])); index++ {
			if !used[digest][index] {
				return errors.New("prepared terminal proof bundle has an unsettled source attempt")
			}
		}
	}
	return nil
}

func observedRun(observed []Object, runID int64) Object {
	for _, run := range observed {
		if exactInt(run["id"], runID) {
			return run
		}
	}
	return nil
}

func appendPreparedFrontier(chainValue Object, target Object, observed []Object, latest map[int64]int64, sourceEntry Object, currentID, currentAttempt int64, engine *Engine) (Object, error) {
	chain, err := engine.ValidateChain(chainValue, target, observed, latest)
	if err != nil {
		return nil, err
	}
	frontier := chain["prepared_frontier"].([]any)
	if len(frontier) >= MaxPreparedFrontierAttempts {
		return nil, errors.New("prepared source frontier reached its explicit eight-attempt bound")
	}
	pending, err := PendingAttempts(chain, observed, latest, currentID, currentAttempt)
	if err != nil || len(pending) <= len(frontier) {
		return nil, errors.New("prepared frontier has no next exact unsettled historical attempt")
	}
	entry, err := Exact(sourceEntry, []string{"kind", "source"}, "prepared frontier source input")
	if err != nil || !contains([]string{"prepared", "journaled"}, fmt.Sprint(entry["kind"])) {
		return nil, errors.New("prepared frontier source kind is invalid")
	}
	source, err := object(entry["source"], "prepared frontier source input")
	if err != nil {
		return nil, err
	}
	runID, attempt, err := preparedSourceIdentity(source)
	if err != nil {
		return nil, err
	}
	wantRun, err := object(pending[len(frontier)]["run"], "next prepared workflow run")
	if err != nil || !exactInt(wantRun["id"], runID) || !exactInt(pending[len(frontier)]["attempt"], attempt) {
		return nil, errors.New("prepared source differs from the exact next unsettled workflow attempt")
	}
	if entry["kind"] == "prepared" {
		if _, err := engine.validatePreparedSourceRecord(source, target); err != nil {
			return nil, err
		}
	} else if err := engine.validateJournaledPreparedSource(source, target); err != nil {
		return nil, err
	}
	candidateLineage := append(append([]any{}, frontier...), entry)
	if _, err := validatePreparedSourceLineage(candidateLineage); err != nil {
		return nil, err
	}
	updated, err := cloneObject(chain)
	if err != nil {
		return nil, err
	}
	updated["prepared_frontier"] = append(updated["prepared_frontier"].([]any), entry)
	updated, err = resealPreparedChain(updated)
	if err != nil {
		return nil, err
	}
	return engine.ValidateChain(updated, target, observed, latest)
}

func appendPreparedCurrentFrontier(chainValue Object, target Object, observed []Object, latest map[int64]int64, sourceEntry Object, currentID, currentAttempt int64, engine *Engine) (Object, error) {
	chain, err := engine.ValidateChain(chainValue, target, observed, latest)
	if err != nil {
		return nil, err
	}
	frontier, err := array(chain["prepared_frontier"], "prepared source frontier")
	if err != nil || len(frontier) >= MaxPreparedFrontierAttempts {
		return nil, errors.New("prepared source frontier reached its explicit eight-attempt bound")
	}
	pending, err := PendingAttempts(chain, observed, latest, currentID, currentAttempt)
	if err != nil || len(pending) != len(frontier) {
		return nil, errors.New("current prepared source cannot skip an unqualified historical predecessor")
	}
	entry, err := Exact(sourceEntry, []string{"kind", "source"}, "current prepared frontier source")
	if err != nil || !contains([]string{"prepared", "journaled"}, fmt.Sprint(entry["kind"])) {
		return nil, errors.New("current prepared frontier source kind is invalid")
	}
	source, err := object(entry["source"], "current prepared frontier source")
	if err != nil {
		return nil, err
	}
	if !exactInt(source["run_id"], currentID) || !exactInt(source["attempt"], currentAttempt) || !Equal(source["run"], observedRun(observed, currentID)) {
		return nil, errors.New("prepared frontier source differs from the exact current workflow attempt")
	}
	if entry["kind"] == "prepared" {
		if _, err := engine.validatePreparedSourceRecord(source, target); err != nil {
			return nil, err
		}
	} else if err := engine.validateJournaledPreparedSource(source, target); err != nil {
		return nil, err
	}
	candidateLineage := append(append([]any{}, frontier...), entry)
	if _, err := validatePreparedSourceLineage(candidateLineage); err != nil {
		return nil, err
	}
	updated, err := cloneObject(chain)
	if err != nil {
		return nil, err
	}
	updated["prepared_frontier"] = append(updated["prepared_frontier"].([]any), entry)
	updated, err = resealPreparedChain(updated)
	if err != nil {
		return nil, err
	}
	return engine.ValidateChain(updated, target, observed, latest)
}

func resealPreparedChain(chain Object) (Object, error) {
	delete(chain, "sha256")
	unsigned, err := Canonical(chain)
	if err != nil || len(unsigned) > MaxCheckpointBytes {
		return nil, errors.New("prepared settlement checkpoint exceeds its explicit byte bound")
	}
	chain["sha256"] = SHA256(unsigned)
	return chain, nil
}

func appendPreparedTerminal(chainValue Object, target Object, observed []Object, latest map[int64]int64, terminal Object, currentID, currentAttempt int64, engine *Engine) (Object, error) {
	chain, err := engine.ValidateChain(chainValue, target, observed, latest)
	if err != nil {
		return nil, err
	}
	frontier := chain["prepared_frontier"].([]any)
	if len(frontier) == 0 {
		return nil, errors.New("prepared terminal completion has no open qualified source frontier")
	}
	terminal, err = engine.ValidateSettlementRecord(terminal, target)
	if err != nil {
		return nil, err
	}
	terminalSettlement, err := object(terminal["settlement"], "prepared terminal observer settlement")
	if err != nil || terminalSettlement["kind"] != "terminal" || terminalSettlement["publication"] != nil {
		return nil, errors.New("prepared recovery can close only with an actual terminal native observer receipt")
	}
	terminalID, terminalAttempt := mustPositive(terminal["run_id"]), mustPositive(terminal["attempt"])
	if !Equal(terminal["run"], observedRun(observed, terminalID)) {
		return nil, errors.New("prepared terminal observer differs from complete workflow history")
	}
	pending, err := PendingAttempts(chain, observed, latest, currentID, currentAttempt)
	isCurrent := terminalID == currentID && terminalAttempt == currentAttempt
	if err != nil || (isCurrent && len(pending) != len(frontier)) ||
		(!isCurrent && (len(pending) < len(frontier)+1 || !exactInt(pending[len(frontier)]["run_id"], terminalID) || !exactInt(pending[len(frontier)]["attempt"], terminalAttempt))) {
		return nil, errors.New("prepared terminal observer cannot skip an unqualified or unknown predecessor")
	}
	for index, raw := range frontier {
		entry, _ := object(raw, "prepared terminal source entry")
		source, _ := object(entry["source"], "prepared terminal source")
		runID, attempt, err := preparedSourceIdentity(source)
		pendingRun, pendingErr := object(pending[index]["run"], "pending prepared source run")
		if pendingErr != nil || err != nil || !exactInt(pendingRun["id"], runID) || !exactInt(pending[index]["attempt"], attempt) {
			return nil, fmt.Errorf("prepared terminal source inventory at index %d differs from exact unsettled chronology", index)
		}
	}
	sources, err := validatePreparedSourceLineage(frontier)
	if err != nil {
		return nil, err
	}
	latestSource := sources[len(sources)-1]
	observerContextValue, err := LoadFileProof(terminal["context_file"], "prepared terminal observer context")
	observerContext, contextErr := object(observerContextValue, "prepared terminal observer context")
	lastID, lastAttempt, identityErr := preparedSourceIdentity(latestSource)
	if err != nil || contextErr != nil || identityErr != nil || !exactInt(observerContext["recovered_from_run_id"], lastID) ||
		!exactInt(observerContext["recovered_from_attempt"], lastAttempt) {
		return nil, errors.New("prepared terminal observer does not preserve the latest direct source lineage")
	}
	firstSourceContext, err := preparedSourceContext(sources[0])
	firstOriginRunID, firstOriginAttempt, originErr := contextPlanOrigin(firstSourceContext, true)
	observerOriginRunID, observerOriginAttempt, observerOriginErr := contextPlanOrigin(observerContext, true)
	if err != nil || originErr != nil || observerOriginErr != nil || observerOriginRunID != firstOriginRunID || observerOriginAttempt != firstOriginAttempt {
		return nil, errors.New("prepared terminal observer changed the exact immutable source plan origin")
	}
	observerPlansRaw, err := array(terminalSettlement["plans"], "prepared terminal observer plans")
	if err != nil || len(observerPlansRaw) == 0 {
		return nil, errors.New("prepared terminal observer lacks actual native plan receipts")
	}
	observerPlans := make([]Object, 0, len(observerPlansRaw))
	for _, raw := range observerPlansRaw {
		plan, err := object(raw, "prepared terminal observer plan")
		if err != nil {
			return nil, err
		}
		observerPlans = append(observerPlans, plan)
	}
	identities := preparedPlanIdentities(observerPlans)
	for _, source := range sources {
		plans, err := preparedSourcePlans(source)
		if err != nil || !Equal(preparedPlanIdentities(plans), identities) {
			return nil, errors.New("prepared terminal observer changed the exact source plan set")
		}
		if !Equal(source["policy_files"], terminalSettlement["policy_files"]) {
			return nil, errors.New("prepared terminal observer changed the exact approval or review policy files")
		}
		if err := validatePreparedObserverPlanRetention(source, observerPlans); err != nil {
			return nil, err
		}
	}
	proof := Object{
		"schema_version": int64(1), "sources": frontier, "observer_run_id": terminalID, "observer_attempt": terminalAttempt,
		"plan_origin_run_id": firstOriginRunID, "plan_origin_attempt": firstOriginAttempt, "plan_identities": identities,
	}
	proofBytes, err := Canonical(proof)
	if err != nil {
		return nil, err
	}
	proofSHA := SHA256(proofBytes)
	proof["sha256"] = proofSHA
	updated, err := cloneObject(chain)
	if err != nil {
		return nil, err
	}
	updated["prepared_terminal_proofs"] = append(updated["prepared_terminal_proofs"].([]any), proof)
	settlements := updated["settlements"].([]any)
	inventory := updated["inventory"].([]any)
	for index, entryRaw := range frontier {
		entry, _ := object(entryRaw, "prepared terminal frontier entry")
		source, _ := object(entry["source"], "prepared terminal source")
		runID, attempt, _ := preparedSourceIdentity(source)
		run, _ := object(source["run"], "prepared terminal source run")
		context, _ := preparedSourceContext(source)
		settlements = append(settlements, Object{
			"run_id": runID, "attempt": attempt, "run": run,
			"attempt_target": Object{"recovery_key": context["recovery_key"], "identity": context["attempt_target"]},
			"artifact":       source["artifact"], "context_file": source["context_file"],
			"settlement": Object{"kind": "prepared_terminal", "phase": "completed", "proof_sha256": proofSHA, "source_index": int64(index)},
		})
		item := findPreparedInventory(inventory, runID)
		if item == nil {
			item = Object{"run": run, "settled_attempt": int64(0)}
			inventory = append(inventory, item)
		}
		if !Equal(item["run"], run) || mustNonnegative(item["settled_attempt"])+1 != attempt {
			return nil, errors.New("prepared source settlements are not the exact next workflow attempts")
		}
		item["settled_attempt"] = attempt
	}
	updated["prepared_frontier"], updated["settlements"], updated["inventory"] = []any{}, settlements, inventory
	prior, err := PendingAttempts(updated, observed, latest, currentID, currentAttempt)
	if err != nil || (isCurrent && len(prior) != 0) ||
		(!isCurrent && (len(prior) == 0 || !exactInt(prior[0]["run_id"], terminalID) || !exactInt(prior[0]["attempt"], terminalAttempt))) {
		return nil, errors.New("prepared terminal observer cannot bypass another unsettled predecessor")
	}
	terminalInventory := findPreparedInventory(inventory, terminalID)
	if terminalInventory != nil && mustNonnegative(terminalInventory["settled_attempt"])+1 != terminalAttempt {
		return nil, errors.New("prepared terminal observer does not follow its exact prior attempt")
	}
	if terminalInventory == nil {
		if terminalAttempt != 1 {
			return nil, errors.New("prepared terminal observer lacks its exact prior run-attempt inventory")
		}
		terminalInventory = Object{"run": terminal["run"], "settled_attempt": int64(0)}
		inventory = append(inventory, terminalInventory)
	}
	settlements = append(settlements, terminal)
	terminalInventory["settled_attempt"] = terminalAttempt
	sort.Slice(inventory, func(i, j int) bool {
		return mustPositive(inventory[i].(map[string]any)["run"].(map[string]any)["id"]) < mustPositive(inventory[j].(map[string]any)["run"].(map[string]any)["id"])
	})
	updated["inventory"], updated["settlements"] = inventory, settlements
	updated, err = resealPreparedChain(updated)
	if err != nil {
		return nil, err
	}
	return engine.ValidateChain(updated, target, observed, latest)
}

func findPreparedInventory(inventory []any, runID int64) Object {
	for _, raw := range inventory {
		item, _ := object(raw, "prepared chain inventory entry")
		run, _ := object(item["run"], "prepared chain inventory run")
		if exactInt(run["id"], runID) {
			return item
		}
	}
	return nil
}
