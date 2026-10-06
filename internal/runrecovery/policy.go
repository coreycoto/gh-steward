package runrecovery

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/workflow"
)

var policyPlanName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var workflowInputName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,80}$`)
var historyCutoverLogin = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`)

type Engine struct {
	repository contract.Repository
	workflows  map[string]workflowPolicy
}

type workflowPolicy struct {
	mutatorAlternatives       [][]string
	allowPublication          bool
	plans                     map[string]planPolicy
	legacyImportReviews       map[string]bool
	historyCutoverReviews     map[string]bool
	historyCutoverReviewIssue *Object
}

type planPolicy struct {
	command, profile, attemptTarget string
	approval                        approvalPolicy
	allowedOps                      map[string]bool
	event                           *eventPolicy
	parentMerge                     *parentMergePolicy
	preparedRecovery                *preparedRecoveryPolicy
}

type preparedRecoveryPolicy struct {
	sourceSHA256 string
	mutators     []Object
	previous     map[string][]Object
}

type eventPolicy struct{ kind, path string }
type approvalPolicy struct {
	kind, approvalInput, reviewedRunInput string
	requiredInputs                        Object
	noop                                  *noopPolicy
}
type parentMergePolicy struct{ kind, planName, command, settingsPath string }

var profileNames = map[string]bool{"merge": true, "execution": true, "governance": true, "quarter": true, "closeout": true, "workflow-noop": true}
var attemptTargetKinds = map[string]bool{"single_plan_sha256": true, "plan_set": true, "execution": true, "parent_merge": true, "workflow_noop": true}
var approvalKinds = map[string]bool{
	"git-slop-governance": true, "git-slop-merge": true, "git-slop-execution": true,
	"reviewed-dispatch": true, "parent-merge": true, "local-noop": true,
}

// NewEngine parses a strict declarative consumer policy. Policy data cannot execute code.
func NewEngine(policy Object, repository contract.Repository) (*Engine, error) {
	if policy == nil {
		return nil, recoveryError("recovery policy must be an object")
	}
	if _, err := contract.ParseRepository(repository.Object()); err != nil {
		return nil, recoveryError("recovery repository identity is invalid: %v", err)
	}
	root, err := Exact(policy, []string{"schema_version", "workflows"}, "recovery policy")
	if err != nil {
		return nil, err
	}
	if !exactInt(root["schema_version"], 1) {
		return nil, recoveryError("recovery policy schema version is unsupported")
	}
	rawWorkflows, err := object(root["workflows"], "recovery workflow policy map")
	if err != nil || len(rawWorkflows) == 0 {
		return nil, recoveryError("recovery policy must contain at least one workflow")
	}
	engine := &Engine{repository: repository, workflows: make(map[string]workflowPolicy, len(rawWorkflows))}
	for workflow, raw := range rawWorkflows {
		if !settlementWorkflowFile.MatchString(workflow) {
			return nil, recoveryError("recovery policy has an invalid workflow filename")
		}
		parsed, err := parseWorkflowPolicy(raw, workflow)
		if err != nil {
			return nil, err
		}
		engine.workflows[workflow] = parsed
	}
	return engine, nil
}

func parseWorkflowPolicy(value any, workflow string) (workflowPolicy, error) {
	raw, err := exactWithOptional(value, []string{"mutator_step_alternatives", "reviewed_source_shas", "allow_publication", "plans"}, []string{"legacy_import_reviews", "history_cutover_reviews", "history_cutover_review_issue"}, "workflow policy")
	if err != nil {
		return workflowPolicy{}, err
	}
	rawAlternatives, err := array(raw["mutator_step_alternatives"], "mutator-step alternatives")
	if err != nil || len(rawAlternatives) == 0 {
		return workflowPolicy{}, recoveryError("workflow %s requires exact mutator-step alternatives", workflow)
	}
	alternatives := make([][]string, 0, len(rawAlternatives))
	seenAlternatives := map[string]bool{}
	for _, rawAlternative := range rawAlternatives {
		alternative, err := stringsArray(rawAlternative, "mutator-step alternative", true)
		if err != nil {
			return workflowPolicy{}, err
		}
		canonical, err := Canonical(alternative)
		if err != nil || seenAlternatives[string(canonical)] {
			return workflowPolicy{}, recoveryError("workflow %s has a duplicate mutator-step alternative", workflow)
		}
		seenAlternatives[string(canonical)] = true
		alternatives = append(alternatives, alternative)
	}
	sourceSHAs, err := stringsArray(raw["reviewed_source_shas"], "reviewed source SHA allowlist", false)
	if err != nil {
		return workflowPolicy{}, err
	}
	for _, sha := range sourceSHAs {
		if !settlementSHA40.MatchString(sha) {
			return workflowPolicy{}, recoveryError("reviewed workflow source must be a 40-hex commit SHA")
		}
	}
	if len(sourceSHAs) != 0 {
		return workflowPolicy{}, recoveryError("reviewed source SHA allowlist must remain empty until an authenticated executed-workflow source witness is supported")
	}
	allowPublication, ok := raw["allow_publication"].(bool)
	if !ok {
		return workflowPolicy{}, recoveryError("workflow publication policy must be an explicit boolean")
	}
	rawPlans, err := object(raw["plans"], "workflow plan policy map")
	if err != nil {
		return workflowPolicy{}, err
	}
	plans := make(map[string]planPolicy, len(rawPlans))
	for name, rawPlan := range rawPlans {
		if !policyPlanName.MatchString(name) {
			return workflowPolicy{}, recoveryError("workflow policy contains an invalid plan name")
		}
		parsed, err := parsePlanPolicy(rawPlan, workflow, name)
		if err != nil {
			return workflowPolicy{}, err
		}
		plans[name] = parsed
	}
	completedMergeParents := map[string]bool{}
	completedMergeCloseouts := 0
	for name, plan := range plans {
		if plan.parentMerge == nil || plan.parentMerge.kind != "completed_merge" {
			continue
		}
		completedMergeCloseouts++
		parent, exists := plans[plan.parentMerge.planName]
		if !exists || parent.profile != "merge" || parent.command != plan.parentMerge.command || plan.parentMerge.command != "merge-apply" {
			return workflowPolicy{}, recoveryError("plan %s does not name an exact merge plan in the same workflow policy", name)
		}
		if completedMergeParents[plan.parentMerge.planName] {
			return workflowPolicy{}, recoveryError("workflow policy reuses one completed merge parent for multiple closeout plans")
		}
		completedMergeParents[plan.parentMerge.planName] = true
	}
	if completedMergeCloseouts > 1 {
		return workflowPolicy{}, recoveryError("workflow policy may configure only one completed-merge closeout")
	}
	legacyReviews := map[string]bool{}
	if value, exists := raw["legacy_import_reviews"]; exists {
		rows, err := stringsArray(value, "exact legacy import reviews", false)
		if err != nil || len(rows) > maxPendingAttempts {
			return workflowPolicy{}, recoveryError("legacy import reviews must be a bounded exact digest inventory")
		}
		for _, digest := range rows {
			if !IsSHA256(digest) || legacyReviews[digest] {
				return workflowPolicy{}, recoveryError("legacy import review digests are invalid or repeated")
			}
			legacyReviews[digest] = true
		}
	}
	historyCutoverReviews := map[string]bool{}
	if value, exists := raw["history_cutover_reviews"]; exists {
		rows, err := stringsArray(value, "exact history cutover reviews", false)
		if err != nil || len(rows) > maxPendingAttempts {
			return workflowPolicy{}, recoveryError("history cutover reviews must be a bounded exact digest inventory")
		}
		for _, digest := range rows {
			if !IsSHA256(digest) || historyCutoverReviews[digest] {
				return workflowPolicy{}, recoveryError("history cutover review digests are invalid or repeated")
			}
			historyCutoverReviews[digest] = true
		}
	}
	var historyCutoverReviewIssue *Object
	if value, exists := raw["history_cutover_review_issue"]; exists {
		issue, err := Exact(value, []string{"number", "trusted_logins"}, "history cutover review issue")
		if err != nil {
			return workflowPolicy{}, err
		}
		number, err := positiveInteger(issue["number"], "history cutover review issue number")
		logins, loginErr := stringsArray(issue["trusted_logins"], "history cutover trusted logins", true)
		if err != nil || loginErr != nil || len(logins) > 128 {
			return workflowPolicy{}, recoveryError("history cutover review issue requires a positive issue number and bounded trusted login list")
		}
		seen := map[string]bool{}
		for _, login := range logins {
			if !historyCutoverLogin.MatchString(login) || seen[login] {
				return workflowPolicy{}, recoveryError("history cutover trusted logins must be unique canonical GitHub logins")
			}
			seen[login] = true
		}
		copyIssue := Object{"number": number, "trusted_logins": logins}
		historyCutoverReviewIssue = &copyIssue
	}
	if len(historyCutoverReviews) > 0 && historyCutoverReviewIssue != nil {
		return workflowPolicy{}, recoveryError("workflow policy cannot combine static and issue-based history cutover review channels")
	}
	return workflowPolicy{mutatorAlternatives: alternatives, allowPublication: allowPublication, plans: plans, legacyImportReviews: legacyReviews, historyCutoverReviews: historyCutoverReviews, historyCutoverReviewIssue: historyCutoverReviewIssue}, nil
}

// ValidateReviewedHistoryCutover checks the explicitly listed baseline digest
// and exact target. The allowlist records which artifact was reviewed; it is
// neither human approval nor permission to perform provider mutations.
func (e *Engine) ValidateReviewedHistoryCutover(value any, targetValue any) (Object, error) {
	baseline, err := ValidateHistoryCutover(value)
	if err != nil {
		return nil, err
	}
	target, err := e.validateTarget(targetValue)
	if err != nil {
		return nil, err
	}
	if !Equal(baseline["target"], target) {
		return nil, recoveryError("history cutover baseline is for another repository or workflow target")
	}
	policy, err := e.workflowPolicy(fmt.Sprint(target["workflow_file"]))
	if err != nil {
		return nil, err
	}
	if !policy.historyCutoverReviews[fmt.Sprint(baseline["sha256"])] {
		return nil, recoveryError("history cutover digest is not explicitly listed in trusted workflow policy")
	}
	return baseline, nil
}

// HistoryCutoverReviewIssues returns the configured issue used as an
// independent review channel, keyed by workflow filename. The returned
// entries are copies and do not prove that a qualifying comment exists.
func (e *Engine) HistoryCutoverReviewIssues() map[string]Object {
	result := map[string]Object{}
	for workflow, policy := range e.workflows {
		if policy.historyCutoverReviewIssue == nil {
			continue
		}
		copyValue, err := cloneObject(*policy.historyCutoverReviewIssue)
		if err == nil {
			result[workflow] = copyValue
		}
	}
	return result
}

// AdmitHistoryCutoverReview registers one exact digest in this Engine's
// in-memory review allowlist after the caller has verified the complete
// issue-comment inventory, exact statement and live author permission. It
// verifies that the workflow has an independent review route configured; it
// does not establish human approval by itself.
func (e *Engine) AdmitHistoryCutoverReview(workflow, digest string) error {
	if !IsSHA256(digest) {
		return recoveryError("history cutover review requires an exact baseline digest")
	}
	policy, err := e.workflowPolicy(workflow)
	if err != nil {
		return err
	}
	if policy.historyCutoverReviewIssue == nil {
		return recoveryError("workflow policy has no independent history cutover review issue")
	}
	policy.historyCutoverReviews[digest] = true
	e.workflows[workflow] = policy
	return nil
}

func parsePlanPolicy(value any, workflow, name string) (planPolicy, error) {
	raw, err := exactWithOptional(value,
		[]string{"command", "domain_profile", "allowed_operation_kinds", "attempt_target", "approval", "event", "parent_merge"},
		[]string{"prepared_recovery"}, "workflow plan policy")
	if err != nil {
		return planPolicy{}, err
	}
	command, ok := raw["command"].(string)
	if !ok || !nativeCommandPattern.MatchString(command) {
		return planPolicy{}, recoveryError("plan %s has an invalid command", name)
	}
	profile, ok := raw["domain_profile"].(string)
	if !ok || !profileNames[profile] {
		return planPolicy{}, recoveryError("plan %s has an unsupported domain profile", name)
	}
	operations, err := stringsArray(raw["allowed_operation_kinds"], "allowed operation kinds", false)
	if err != nil {
		return planPolicy{}, err
	}
	allowed := make(map[string]bool, len(operations))
	for _, operation := range operations {
		if !nativeCommandPattern.MatchString(operation) {
			return planPolicy{}, recoveryError("plan %s has an invalid allowed operation kind", name)
		}
		allowed[operation] = true
	}
	attempt, ok := raw["attempt_target"].(string)
	if !ok || !attemptTargetKinds[attempt] {
		return planPolicy{}, recoveryError("plan %s has an unsupported attempt-target contract", name)
	}
	approval, err := parseApprovalPolicy(raw["approval"], name)
	if err != nil {
		return planPolicy{}, err
	}
	var event *eventPolicy
	if raw["event"] != nil {
		eventObject, err := Exact(raw["event"], []string{"kind", "path"}, "workflow event contract")
		if err != nil {
			return planPolicy{}, err
		}
		kind, kindOK := eventObject["kind"].(string)
		path, pathOK := eventObject["path"].(string)
		if !kindOK || !pathOK || !nonemptyString(path) ||
			(kind == "workflow_run" && path != "events/workflow-run-event.json") ||
			((kind == "workflow_dispatch" || kind == "execution_event") && path != "events/dispatch-event.json") ||
			(kind == "workflow_noop" && path != "events/trigger-event.json") ||
			(kind != "workflow_run" && kind != "workflow_dispatch" && kind != "execution_event" && kind != "workflow_noop") {
			return planPolicy{}, recoveryError("plan %s has an unsupported event kind or path", name)
		}
		event = &eventPolicy{kind: kind, path: path}
	}
	var parent *parentMergePolicy
	if raw["parent_merge"] != nil {
		parentObject, err := object(raw["parent_merge"], "parent-merge contract")
		if err != nil {
			return planPolicy{}, err
		}
		kind, ok := parentObject["kind"].(string)
		if !ok {
			return planPolicy{}, recoveryError("plan %s has an invalid parent-merge contract", name)
		}
		switch kind {
		case "completed_merge":
			parentRaw, err := Exact(parentObject, []string{"kind", "plan_name", "command", "settings_path"}, "completed parent-merge contract")
			if err != nil {
				return planPolicy{}, err
			}
			planName, planNameOK := parentRaw["plan_name"].(string)
			parentCommand, commandOK := parentRaw["command"].(string)
			settingsPath, settingsOK := parentRaw["settings_path"].(string)
			if !planNameOK || !policyPlanName.MatchString(planName) || !commandOK || parentCommand != "merge-apply" ||
				!settingsOK || !safePolicyRelativePath(settingsPath) {
				return planPolicy{}, recoveryError("plan %s has an unsupported completed parent-merge contract", name)
			}
			parent = &parentMergePolicy{kind: kind, planName: planName, command: parentCommand, settingsPath: settingsPath}
		default:
			return planPolicy{}, recoveryError("plan %s has an unsupported parent-merge continuation", name)
		}
	}
	var prepared *preparedRecoveryPolicy
	if raw["prepared_recovery"] != nil {
		prepared, err = parsePreparedRecoveryPolicy(raw["prepared_recovery"], name)
		if err != nil {
			return planPolicy{}, err
		}
	}
	policy := planPolicy{command: command, profile: profile, attemptTarget: attempt, approval: approval,
		allowedOps: allowed, event: event, parentMerge: parent, preparedRecovery: prepared}
	if err := validatePlanPolicyCombination(policy, workflow, name); err != nil {
		return planPolicy{}, err
	}
	return policy, nil
}

func parsePreparedRecoveryPolicy(value any, planName string) (*preparedRecoveryPolicy, error) {
	fields, err := exactWithOptional(value,
		[]string{"workflow_source_sha256", "mutators"}, []string{"previous_sources"}, "prepared recovery policy")
	if err != nil {
		return nil, err
	}
	digest, ok := fields["workflow_source_sha256"].(string)
	if !ok || !IsSHA256(digest) {
		return nil, recoveryError("plan %s prepared recovery requires an exact workflow source digest", planName)
	}
	mutators, err := parsePreparedMutators(fields["mutators"], planName)
	if err != nil {
		return nil, err
	}
	previous := map[string][]Object{}
	if fields["previous_sources"] != nil {
		rows, err := array(fields["previous_sources"], "prepared recovery prior sources")
		if err != nil || len(rows) > 64 {
			return nil, recoveryError("plan %s has a malformed or oversized prepared recovery source history", planName)
		}
		for _, row := range rows {
			prior, err := Exact(row, []string{"workflow_source_sha256", "mutators"}, "prepared recovery prior source")
			if err != nil {
				return nil, err
			}
			priorDigest, ok := prior["workflow_source_sha256"].(string)
			if !ok || !IsSHA256(priorDigest) || priorDigest == digest || previous[priorDigest] != nil {
				return nil, recoveryError("plan %s has a duplicated or invalid prior prepared workflow source", planName)
			}
			priorMutators, err := parsePreparedMutators(prior["mutators"], planName)
			if err != nil {
				return nil, err
			}
			previous[priorDigest] = priorMutators
		}
	}
	return &preparedRecoveryPolicy{sourceSHA256: digest, mutators: mutators, previous: previous}, nil
}

func parsePreparedMutators(value any, planName string) ([]Object, error) {
	rows, err := array(value, "prepared recovery mutation jobs")
	if err != nil || len(rows) == 0 || len(rows) > 128 {
		return nil, recoveryError("plan %s requires a bounded nonempty prepared recovery mutation-job inventory", planName)
	}
	seenJobs := map[string]bool{}
	result := make([]Object, 0, len(rows))
	totalSteps := 0
	for _, row := range rows {
		entry, err := Exact(row, []string{"job", "steps"}, "prepared recovery mutation job")
		if err != nil {
			return nil, err
		}
		job, ok := entry["job"].(string)
		steps, stepsErr := stringsArray(entry["steps"], "prepared recovery mutation steps", true)
		if !ok || !nonemptyString(job) || seenJobs[job] || stepsErr != nil || len(steps) > 512 {
			return nil, recoveryError("plan %s has an invalid or duplicated prepared recovery mutation job", planName)
		}
		seenJobs[job] = true
		totalSteps += len(steps)
		if totalSteps > 512 {
			return nil, recoveryError("plan %s prepared recovery mutation inventory exceeds its step bound", planName)
		}
		result = append(result, Object{"job": job, "steps": steps})
	}
	return result, nil
}

func parseApprovalPolicy(value any, name string) (approvalPolicy, error) {
	objectValue, ok := value.(map[string]any)
	if !ok {
		return approvalPolicy{}, recoveryError("plan %s has an unsupported approval contract", name)
	}
	kind, ok := objectValue["kind"].(string)
	if !ok || !approvalKinds[kind] {
		return approvalPolicy{}, recoveryError("plan %s has an unsupported approval contract", name)
	}
	if kind == "reviewed-dispatch" {
		fields, err := Exact(objectValue, []string{"kind", "approval_input", "reviewed_run_input", "required_inputs"}, "reviewed dispatch approval contract")
		if err != nil {
			return approvalPolicy{}, err
		}
		approvalInput, aOK := fields["approval_input"].(string)
		runInput, rOK := fields["reviewed_run_input"].(string)
		if !aOK || !workflowInputName.MatchString(approvalInput) || !rOK || !workflowInputName.MatchString(runInput) || approvalInput == runInput {
			return approvalPolicy{}, recoveryError("plan %s has invalid reviewed dispatch input names", name)
		}
		required, err := object(fields["required_inputs"], "required workflow-dispatch input contract")
		if err != nil {
			return approvalPolicy{}, err
		}
		if len(required) == 0 || len(required) > 8 {
			return approvalPolicy{}, recoveryError("plan %s has an unsupported required dispatch input count", name)
		}
		for input, expected := range required {
			if !workflowInputName.MatchString(input) || !(expected == true || nonemptyString(expected)) {
				return approvalPolicy{}, recoveryError("plan %s has an invalid exact dispatch input requirement", name)
			}
		}
		return approvalPolicy{kind: kind, approvalInput: approvalInput, reviewedRunInput: runInput, requiredInputs: required}, nil
	}
	if kind == "local-noop" {
		noop, err := parseNoopPolicy(objectValue)
		if err != nil {
			return approvalPolicy{}, recoveryError("plan %s has an invalid workflow no-op approval contract: %v", name, err)
		}
		return approvalPolicy{kind: kind, noop: noop}, nil
	}
	fields, err := Exact(objectValue, []string{"kind"}, "approval contract")
	if err != nil {
		return approvalPolicy{}, err
	}
	return approvalPolicy{kind: fields["kind"].(string)}, nil
}

func validatePlanPolicyCombination(policy planPolicy, workflow, name string) error {
	invalid := func() error {
		return recoveryError("plan %s has inconsistent profile, approval, event or target contracts", name)
	}
	if policy.profile != "workflow-noop" && len(policy.allowedOps) == 0 {
		return invalid()
	}
	switch policy.profile {
	case "governance":
		if policy.attemptTarget != "single_plan_sha256" || policy.approval.kind != "git-slop-governance" || policy.event != nil || policy.parentMerge != nil {
			return invalid()
		}
	case "merge":
		if policy.attemptTarget != "single_plan_sha256" || policy.approval.kind != "git-slop-merge" || policy.event == nil || policy.event.kind != "workflow_run" || policy.parentMerge != nil {
			return invalid()
		}
	case "execution":
		if policy.parentMerge != nil {
			return invalid()
		}
		if policy.attemptTarget == "execution" {
			if policy.approval.kind != "git-slop-execution" || policy.event == nil || policy.event.kind != "execution_event" {
				return invalid()
			}
		} else if policy.attemptTarget == "plan_set" {
			if policy.approval.kind != "reviewed-dispatch" || policy.event == nil || policy.event.kind != "workflow_dispatch" || policy.approval.approvalInput == "" || policy.approval.reviewedRunInput == "" {
				return invalid()
			}
		} else {
			return invalid()
		}
	case "quarter":
		if policy.attemptTarget != "plan_set" || policy.approval.kind != "reviewed-dispatch" || policy.event == nil || policy.event.kind != "workflow_dispatch" || policy.parentMerge != nil || policy.approval.approvalInput == "" || policy.approval.reviewedRunInput == "" {
			return invalid()
		}
	case "closeout":
		if policy.attemptTarget != "parent_merge" || policy.approval.kind != "parent-merge" || policy.event != nil || policy.parentMerge == nil || policy.command != "branch-cleanup-apply" {
			return invalid()
		}
		if policy.parentMerge.kind != "completed_merge" || policy.parentMerge.settingsPath == "" {
			return invalid()
		}
	case "workflow-noop":
		if name != "workflow-noop" || policy.command != "workflow-noop" || policy.attemptTarget != "workflow_noop" ||
			policy.approval.kind != "local-noop" || policy.approval.noop == nil || policy.event == nil || policy.event.kind != "workflow_noop" ||
			policy.event.path != "events/trigger-event.json" || policy.parentMerge != nil || len(policy.allowedOps) != 0 {
			return invalid()
		}
	}
	if workflow == "" || name == "" {
		return invalid()
	}
	return nil
}

func (e *Engine) workflowPolicy(workflow string) (workflowPolicy, error) {
	policy, ok := e.workflows[workflow]
	if !ok {
		return workflowPolicy{}, recoveryError("workflow has no trusted recovery policy")
	}
	return policy, nil
}

func (e *Engine) validateTarget(value any) (Object, error) {
	target, err := ValidateTarget(value)
	if err != nil {
		return nil, err
	}
	if _, err := e.workflowPolicy(fmt.Sprint(target["workflow_file"])); err != nil {
		return nil, err
	}
	if !strings.EqualFold(fmt.Sprint(target["repository"]), e.repository.Owner+"/"+e.repository.Name) ||
		!strings.EqualFold(fmt.Sprint(target["server_url"]), "https://"+e.repository.Host) {
		return nil, recoveryError("settlement target belongs to another repository host or owner/name")
	}
	return target, nil
}

// MutatorStepAlternatives returns a copy of the exact historical step sequences.
func (e *Engine) MutatorStepAlternatives(workflow string) ([][]string, error) {
	policy, err := e.workflowPolicy(workflow)
	if err != nil {
		return nil, err
	}
	result := make([][]string, len(policy.mutatorAlternatives))
	for i, alternative := range policy.mutatorAlternatives {
		result[i] = append([]string{}, alternative...)
	}
	return result, nil
}

func (e *Engine) workflowAllowsPublication(workflow string) bool {
	policy, ok := e.workflows[workflow]
	return ok && policy.allowPublication
}

type policyReader struct {
	root        string
	proofs      Object
	used        map[string]bool
	nativePlans map[string]Object
}

func (e *Engine) ValidateRecoveryPlan(context, entry, plan Object, root string) error {
	workflow := ""
	if context != nil {
		workflow = fmt.Sprint(context["workflow_file"])
	}
	cutover, err := e.previewOnlyCutoverAtRoot(root, workflow)
	if err != nil {
		return err
	}
	if cutover {
		if entry == nil || plan == nil || context == nil {
			return recoveryError("preview-only workflow-noop qualification requires exact context, plan and entry")
		}
		parsed, err := contract.ParsePlan(plan)
		if err != nil || entry["name"] != "workflow-noop" || entry["command"] != "workflow-noop" || parsed.Command != "workflow-noop" || len(parsed.Operations) != 0 || context["publication"] != nil {
			return recoveryError("reviewed history cutover permits only the exact zero-operation workflow-noop plan")
		}
	}
	return e.validateRecoveryPlanWithReader(context, entry, plan, &policyReader{root: root, used: map[string]bool{}})
}

func (e *Engine) previewOnlyCutoverAtRoot(root, workflow string) (bool, error) {
	if root == "" {
		return false, nil
	}
	path := filepath.Join(root, "settlement-chain.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > MaxCheckpointBytes {
		return false, recoveryError("settlement chain is unsafe or unreadable while checking preview-only mode")
	}
	value, err := LoadJSON(path)
	if err != nil {
		return false, err
	}
	chain, err := object(value, "settlement chain")
	if err != nil {
		return false, err
	}
	if !exactInt(chain["schema_version"], historyCutoverSettlementSchemaVersion) {
		return false, nil
	}
	chain, err = Exact(chain, cutoverChainFields, "schema-6 settlement chain")
	if err != nil {
		return false, err
	}
	target, err := e.validateTarget(chain["target"])
	if err != nil || target["workflow_file"] != workflow {
		return false, recoveryError("schema-6 settlement chain belongs to another workflow")
	}
	if _, err := e.ValidateReviewedHistoryCutover(chain["history_cutover"], target); err != nil {
		return false, err
	}
	unsigned := Object{}
	for _, field := range cutoverChainFields[:len(cutoverChainFields)-1] {
		unsigned[field] = chain[field]
	}
	canonical, err := Canonical(unsigned)
	if err != nil || !IsSHA256(chain["sha256"]) || SHA256(canonical) != chain["sha256"] {
		return false, recoveryError("schema-6 settlement chain digest is invalid")
	}
	return true, nil
}

// ValidateRecoveryPlanFiles revalidates a retained plan using only its exact sidecar file proofs.
func (e *Engine) ValidateRecoveryPlanFiles(context, entry, plan, policyFiles Object) error {
	reader := &policyReader{proofs: policyFiles, used: map[string]bool{}}
	if err := e.validateRecoveryPlanWithReader(context, entry, plan, reader); err != nil {
		return err
	}
	return validatePolicyFilesUsed(policyFiles, reader)
}

func validatePolicyFilesUsed(policyFiles Object, reader *policyReader) error {
	if policyFiles == nil || len(reader.used) != len(policyFiles) {
		return recoveryError("retained policy files contain an unreferenced or unsupported sidecar")
	}
	for path, proof := range policyFiles {
		if !safePolicyRelativePath(path) || !reader.used[path] {
			return recoveryError("retained policy files contain an unreferenced or unsafe sidecar")
		}
		if _, err := LoadFileProof(proof, "retained policy sidecar"); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) validatePlanPolicyFiles(context Object, retainedPlans []Object, reader *policyReader) error {
	entries, err := array(context["plans"], "recovery plan manifest")
	if err != nil || len(entries) != len(retainedPlans) {
		return recoveryError("recovery policy evidence does not cover the exact plan inventory")
	}
	reader.nativePlans = make(map[string]Object, len(retainedPlans))
	for index, rawEntry := range entries {
		entry, err := object(rawEntry, "recovery plan manifest entry")
		if err != nil {
			return err
		}
		proof, err := object(retainedPlans[index], "retained native plan proof")
		if err != nil {
			return err
		}
		name, ok := entry["name"].(string)
		if !ok || !policyPlanName.MatchString(name) {
			return recoveryError("recovery plan manifest contains an invalid exact name")
		}
		reader.nativePlans[name] = proof
		planValue, err := LoadFileProof(proof["plan_file"], "recovery plan")
		if err != nil {
			return err
		}
		plan, err := object(planValue, "recovery plan")
		if err != nil {
			return err
		}
		if err := e.validateRecoveryPlanWithReader(context, entry, plan, reader); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) validateRetainedPlanPolicyFiles(context Object, retainedPlans []Object, policyFiles Object) error {
	reader := &policyReader{proofs: policyFiles, used: map[string]bool{}}
	if err := e.validatePlanPolicyFiles(context, retainedPlans, reader); err != nil {
		return err
	}
	return validatePolicyFilesUsed(policyFiles, reader)
}

func (e *Engine) validateRecoveryPlanWithReader(context, entry, plan Object, reader *policyReader) error {
	if context == nil || entry == nil || plan == nil {
		return recoveryError("recovery context, manifest entry and plan must be objects")
	}
	workflow, ok := context["workflow_file"].(string)
	if !ok || !settlementWorkflowFile.MatchString(workflow) {
		return recoveryError("recovery context has no exact workflow filename")
	}
	wfPolicy, err := e.workflowPolicy(workflow)
	if err != nil {
		return err
	}
	name, ok := entry["name"].(string)
	if !ok || !policyPlanName.MatchString(name) {
		return recoveryError("recovery plan entry has no exact name")
	}
	policy, ok := wfPolicy.plans[name]
	if !ok {
		return recoveryError("recovery plan name is outside the trusted workflow policy")
	}
	repository, ok := context["repository"].(string)
	if !ok || !strings.EqualFold(repository, e.repository.Owner+"/"+e.repository.Name) {
		return recoveryError("recovery context identifies another repository")
	}
	digest, ok := entry["sha256"].(string)
	if !ok || !IsSHA256(digest) || entry["command"] != policy.command || plan["command"] != policy.command || plan["sha256"] != digest {
		return recoveryError("recovery plan differs from its trusted command or immutable digest")
	}
	parsed, err := contract.ParsePlan(plan)
	if err != nil {
		return recoveryError("recovery plan is not a valid native v2 plan: %v", err)
	}
	if !strings.EqualFold(parsed.Repository.Owner+"/"+parsed.Repository.Name, repository) || parsed.Repository.Host != e.repository.Host || parsed.Command != policy.command {
		return recoveryError("recovery plan identifies another repository, host or command")
	}
	for _, operation := range parsed.Operations {
		if !policy.allowedOps[operation.Kind] {
			return recoveryError("recovery plan contains an operation outside its declared policy")
		}
	}
	if err := e.validateAttemptTarget(context, entry, plan, policy); err != nil {
		return err
	}
	phase, phaseOK := context["phase"].(string)
	if !phaseOK || !contains([]string{"prepared", "dispatching", "completed"}, phase) {
		return recoveryError("recovery context has an unsupported phase")
	}
	switch policy.profile {
	case "governance":
		return e.validateGovernance(context, entry, plan, reader)
	case "merge":
		return e.validateMerge(context, entry, plan, reader, policy)
	case "execution":
		if policy.attemptTarget == "plan_set" {
			return e.validatePlanSetDispatch(context, entry, plan, reader, policy)
		}
		return e.validateExecution(context, entry, plan, reader, policy)
	case "quarter":
		return e.validatePlanSetDispatch(context, entry, plan, reader, policy)
	case "closeout":
		return e.validateParentMergeContinuation(context, entry, plan, reader, policy)
	case "workflow-noop":
		return e.validateWorkflowNoop(context, entry, plan, reader)
	default:
		return recoveryError("recovery plan has no trusted domain profile")
	}
}

func (e *Engine) validateAttemptTarget(context, entry, plan Object, policy planPolicy) error {
	digest, _ := entry["sha256"].(string)
	switch policy.attemptTarget {
	case "single_plan_sha256":
		if !Equal(context["attempt_target"], Object{"plan_sha256": digest}) {
			return recoveryError("run context attempt target does not bind the exact reviewed plan")
		}
	case "execution":
		attempt, err := Exact(context["attempt_target"], []string{"plan_sha256", "target", "event_sha256"}, "execution attempt target")
		if err != nil || attempt["plan_sha256"] != digest || !IsSHA256(attempt["event_sha256"]) {
			return recoveryError("execution recovery does not bind one exact sync plan and source event")
		}
		target, err := Exact(attempt["target"], []string{"kind", "number"}, "execution target")
		if err != nil || (target["kind"] != "issue" && target["kind"] != "pull-request") {
			return recoveryError("execution recovery target has an unsupported shape")
		}
		if _, err := positiveInteger(target["number"], "execution target number"); err != nil {
			return err
		}
	case "plan_set":
		return validatePlanSetContext(context)
	case "parent_merge":
		if !nonemptyString(context["recovery_key"]) || context["attempt_target"] == nil {
			return recoveryError("manual cleanup context lacks its exact parent-derived target")
		}
		if policy.parentMerge != nil && policy.parentMerge.kind == "completed_merge" {
			parentEntry, _, err := e.completedMergeContextEntries(context, policy.parentMerge)
			if err != nil || !Equal(context["attempt_target"], Object{"plan_sha256": parentEntry["sha256"]}) {
				return recoveryError("composed cleanup target does not preserve the exact completed merge plan")
			}
		}
	case "workflow_noop":
		data, err := object(plan["data"], "workflow no-op plan data")
		if err != nil || !Equal(data["attempt_target"], context["attempt_target"]) {
			return recoveryError("workflow no-op plan does not preserve its original exact attempt target")
		}
	default:
		return recoveryError("recovery policy has an unsupported attempt-target contract")
	}
	_ = plan
	return nil
}

func validatePlanSetContext(context Object) error {
	manifest, err := array(context["plans"], "ordered plan-set manifest")
	if err != nil || len(manifest) == 0 {
		return recoveryError("recovery context lacks its ordered plan inventory")
	}
	identities := make([]any, 0, len(manifest))
	seen := map[string]bool{}
	for _, raw := range manifest {
		entry, err := object(raw, "plan-set manifest entry")
		if err != nil {
			return err
		}
		name, nameOK := entry["name"].(string)
		command, commandOK := entry["command"].(string)
		digest, digestOK := entry["sha256"].(string)
		if !nameOK || !nonemptyString(name) || !commandOK || !nativeCommandPattern.MatchString(command) || !digestOK || !IsSHA256(digest) || seen[name] {
			return recoveryError("recovery context contains an invalid or duplicate plan identity")
		}
		seen[name] = true
		identities = append(identities, Object{"name": name, "command": command, "sha256": digest})
	}
	expected := Object{"plans": identities}
	if !Equal(context["attempt_target"], expected) {
		return recoveryError("recovery context target does not bind its ordered exact plan inventory")
	}
	canonical, err := Canonical(expected)
	if err != nil || context["recovery_key"] != "plan-set-"+SHA256(canonical) {
		return recoveryError("recovery context has no exact plan-set key")
	}
	return nil
}

func (reader *policyReader) read(relative, name string) (Object, []byte, error) {
	if !safePolicyRelativePath(relative) {
		return nil, nil, recoveryError("saved %s path is unsafe", name)
	}
	var data []byte
	var err error
	if reader.proofs != nil {
		proof, ok := reader.proofs[relative]
		if !ok {
			return nil, nil, recoveryError("saved %s sidecar proof is missing", name)
		}
		if _, err := LoadFileProof(proof, name); err != nil {
			return nil, nil, err
		}
		proofObject, _ := object(proof, name+" file proof")
		encoded, _ := proofObject["base64"].(string)
		data, err = base64.StdEncoding.Strict().DecodeString(encoded)
		if err == nil && reader.root != "" {
			packageBytes, packageErr := ReadPackageFile(reader.root, relative)
			if packageErr != nil || !Equal(packageBytes, data) {
				return nil, nil, recoveryError("saved %s differs from its exact retained package proof", name)
			}
		}
	} else if reader.root != "" {
		data, err = ReadPackageFile(reader.root, relative)
	} else {
		return nil, nil, recoveryError("saved %s has no trusted package or retained file proof", name)
	}
	if err != nil {
		return nil, nil, recoveryError("saved %s is missing or unsafe: %v", name, err)
	}
	reader.used[relative] = true
	value, err := DecodeValue(data)
	if err != nil {
		return nil, nil, recoveryError("saved %s is malformed: %v", name, err)
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, nil, recoveryError("saved %s is not an object", name)
	}
	return result, data, nil
}

func safePolicyRelativePath(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\\r\n\x00") || strings.HasPrefix(value, "/") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func readReview(reader *policyReader, entry Object) (Object, error) {
	name, _ := entry["name"].(string)
	relative, _ := entry["review_path"].(string)
	digest, _ := entry["review_sha256"].(string)
	if !policyPlanName.MatchString(name) || relative != "reviews/"+name+".json" || !IsSHA256(digest) {
		return nil, recoveryError("reviewed plan lacks its exact saved approval reference")
	}
	review, data, err := reader.read(relative, "plan review")
	if err != nil {
		return nil, err
	}
	if SHA256(data) != digest {
		return nil, recoveryError("saved plan review bytes differ from their durable identity")
	}
	return review, nil
}

func (e *Engine) validateGovernance(_ Object, entry, plan Object, reader *policyReader) error {
	data, err := object(plan["data"], "governance plan data")
	if err != nil || !nonemptyString(data["kind"]) {
		return recoveryError("governance recovery lacks typed plan data")
	}
	operations, err := array(plan["operations"], "governance plan operations")
	if err != nil {
		return err
	}
	ids := make([]any, 0, len(operations))
	for _, raw := range operations {
		operation, err := Exact(raw, []string{"id", "kind", "target", "before", "after"}, "governance plan operation")
		if err != nil {
			return err
		}
		ids = append(ids, operation["id"])
	}
	review, err := readReview(reader, entry)
	if err != nil {
		return err
	}
	approved, err := Exact(review, []string{"status", "approved_plans"}, "governance approval")
	if err != nil || approved["status"] != "approved" || !Equal(approved["approved_plans"], []any{Object{
		"name": entry["name"], "plan_sha256": entry["sha256"], "operation_ids": ids,
	}}) {
		return recoveryError("saved governance review does not bind the exact plan and operation order")
	}
	return nil
}

func (e *Engine) validateMerge(context, entry, plan Object, reader *policyReader, policy planPolicy) error {
	if policy.event == nil || policy.event.kind != "workflow_run" {
		return recoveryError("merge policy lacks its exact workflow event")
	}
	_, err := e.validateWorkflowRunEvent(context, plan, reader, policy)
	if err != nil {
		return err
	}
	operations, err := array(plan["operations"], "merge plan operations")
	if err != nil || len(operations) != 1 {
		return recoveryError("merge recovery must contain exactly one reviewed candidate")
	}
	operation, err := object(operations[0], "merge operation")
	if err != nil || !policy.allowedOps[fmt.Sprint(operation["kind"])] {
		return recoveryError("merge recovery operation is outside its declared policy")
	}
	target, err := object(operation["target"], "merge operation target")
	if err != nil {
		return err
	}
	prNumber, err := positiveInteger(target["number"], "merge pull request number")
	if err != nil {
		return err
	}
	planData, _ := object(plan["data"], "merge plan data")
	normalized, _ := object(planData["event"], "normalized merge event")
	run, _ := object(normalized["workflow_run"], "normalized workflow-run event")
	runID, err := positiveInteger(run["id"], "source CI run ID")
	if err != nil || context["recovery_key"] != fmt.Sprintf("merge-ci-%d-pr-%d", runID, prNumber) {
		return recoveryError("merge recovery key does not bind the exact CI run and pull request")
	}
	prs, err := array(run["pull_requests"], "workflow-run pull request associations")
	if err != nil || len(prs) != 1 {
		return recoveryError("merge plan must bind exactly one pull-request association")
	}
	pr, _ := object(prs[0], "workflow-run pull request association")
	head, _ := object(pr["head"], "workflow-run pull request head")
	if !exactInt(pr["number"], prNumber) || target["head_sha"] != head["sha"] {
		return recoveryError("merge plan target differs from its exact triggering workflow association")
	}
	review, err := readReview(reader, entry)
	if err != nil {
		return err
	}
	review, err = Exact(review, []string{"status", "approved_plan_sha", "pr_number", "merge_method", "blocking_reasons"}, "merge approval")
	if err != nil || review["status"] != "approved" || review["approved_plan_sha"] != entry["sha256"] ||
		!exactInt(review["pr_number"], prNumber) || review["merge_method"] != target["merge_method"] || !Equal(review["blocking_reasons"], []any{}) {
		return recoveryError("saved approval does not bind the exact merge plan and pull request")
	}
	if context["branch_cleanup_outcome"] != nil {
		return recoveryError("branch-cleanup observation is outside the exact merge-plan contract")
	}
	return nil
}

func (e *Engine) validateWorkflowRunEvent(context, plan Object, reader *policyReader, policy planPolicy) (Object, error) {
	event, _, err := reader.read(policy.event.path, "merge workflow_run event")
	if err != nil {
		return nil, err
	}
	rawRun, err := object(event["workflow_run"], "saved merge workflow_run event")
	if err != nil {
		return nil, err
	}
	rawRepo, err := object(rawRun["repository"], "source CI repository")
	if err != nil || !stringEqualFold(rawRepo["full_name"], context["repository"]) {
		return nil, recoveryError("saved merge event belongs to another repository")
	}
	expected := Object{}
	for _, field := range []string{"name", "event", "status", "head_branch"} {
		if !nonemptyString(rawRun[field]) {
			return nil, recoveryError("saved merge event has an invalid workflow-run field")
		}
		expected[field] = rawRun[field]
	}
	for _, field := range []string{"id", "run_attempt", "workflow_id"} {
		if _, err := positiveInteger(rawRun[field], "merge event run identity"); err != nil {
			return nil, err
		}
		expected[field] = rawRun[field]
	}
	headSHA, ok := rawRun["head_sha"].(string)
	if !ok || !settlementSHA40.MatchString(headSHA) {
		return nil, recoveryError("saved merge event has an invalid head SHA")
	}
	expected["head_sha"] = headSHA
	conclusion := rawRun["conclusion"]
	if conclusion != nil && !nonemptyString(conclusion) {
		return nil, recoveryError("saved merge event has an invalid conclusion")
	}
	expected["conclusion"] = conclusion
	associations, err := array(rawRun["pull_requests"], "merge event pull requests")
	if err != nil {
		return nil, err
	}
	prs := make([]Object, 0, len(associations))
	seen := map[int64]bool{}
	for _, raw := range associations {
		association, err := object(raw, "merge event pull request association")
		if err != nil {
			return nil, err
		}
		number, err := positiveInteger(association["number"], "associated pull request number")
		if err != nil || seen[number] {
			return nil, recoveryError("saved merge event has duplicate or invalid pull requests")
		}
		seen[number] = true
		head, err := object(association["head"], "associated pull request head")
		if err != nil {
			return nil, err
		}
		sha, shaOK := head["sha"].(string)
		ref, refOK := head["ref"].(string)
		if !shaOK || !settlementSHA40.MatchString(sha) || !refOK || !nonemptyString(ref) {
			return nil, recoveryError("saved merge event has an invalid associated pull-request head")
		}
		prs = append(prs, Object{"number": rawNumber(number, association["number"]), "head": Object{"sha": sha, "ref": ref}})
	}
	// API association ordering is not stable; the signed normalized plan sorts by number.
	for i := 1; i < len(prs); i++ {
		for j := i; j > 0 && mustPositive(prs[j]["number"]) < mustPositive(prs[j-1]["number"]); j-- {
			prs[j], prs[j-1] = prs[j-1], prs[j]
		}
	}
	data, err := object(plan["data"], "merge plan data")
	if err != nil {
		return nil, err
	}
	normalized, err := object(data["event"], "normalized plan workflow event")
	if err != nil {
		return nil, err
	}
	normalizedRun, err := object(normalized["workflow_run"], "normalized plan workflow_run")
	if err != nil || !stringEqualFold(nestedString(normalizedRun, "repository", "full_name"), context["repository"]) {
		return nil, recoveryError("merge plan trigger repository differs from the workflow repository")
	}
	expected["repository"] = Object{"full_name": nestedString(normalizedRun, "repository", "full_name")}
	expected["pull_requests"] = prs
	if !Equal(normalized, Object{"workflow_run": expected}) {
		return nil, recoveryError("saved triggering event differs from the event captured in the reviewed merge plan")
	}
	return rawRun, nil
}

func (e *Engine) validateExecution(context, entry, plan Object, reader *policyReader, policy planPolicy) error {
	if policy.event == nil || policy.event.kind != "execution_event" {
		return recoveryError("execution recovery has no exact source-event contract")
	}
	attemptTarget, _ := object(context["attempt_target"], "execution attempt target")
	target, _ := object(attemptTarget["target"], "execution target selector")
	kind := fmt.Sprint(target["kind"])
	number, err := positiveInteger(target["number"], "execution target number")
	if err != nil {
		return err
	}
	event, eventBytes, err := reader.read(policy.event.path, "execution source event")
	if err != nil || SHA256(eventBytes) != attemptTarget["event_sha256"] {
		return recoveryError("execution source event differs from its bound digest")
	}
	if !stringEqualFold(nestedString(event, "repository", "full_name"), context["repository"]) {
		return recoveryError("execution source event belongs to another repository")
	}
	data, _ := object(plan["data"], "execution plan data")
	selector, _ := object(data["selector"], "execution plan selector")
	issue, issueErr := contract.Integer(selector["issue_number"])
	if issueErr != nil {
		issue = 0
	}
	pr, prErr := contract.Integer(selector["pull_request_number"])
	if prErr != nil {
		pr = 0
	}
	if (kind == "issue" && (issue != number || pr != 0)) || (kind == "pull-request" && (pr != number || issue != 0)) {
		return recoveryError("execution plan selector differs from the approved issue or pull request target")
	}
	if event["action"] != nil {
		pullRequest, err := object(event["pull_request"], "execution pull request event")
		if err != nil || kind != "pull-request" || !exactInt(event["number"], number) || !exactInt(pullRequest["number"], number) {
			return recoveryError("pull request event differs from the exact execution target")
		}
	} else {
		inputs, err := object(event["inputs"], "manual execution event inputs")
		if err != nil {
			return err
		}
		want := fmt.Sprint(number)
		issueInput, prInput := inputs["issue_number"], inputs["pr_number"]
		if (kind == "issue" && (issueInput != want || (prInput != nil && prInput != ""))) ||
			(kind == "pull-request" && (prInput != want || (issueInput != nil && issueInput != ""))) {
			return recoveryError("manual event inputs differ from the selected execution target")
		}
	}
	runID, attempt := contextRunOrigin(context)
	if runID < 1 || attempt < 1 {
		return recoveryError("execution recovery context lacks its exact source attempt")
	}
	review, err := readReview(reader, entry)
	if err != nil {
		return err
	}
	wantReview := Object{
		"schema_version": 1, "workflow_file": context["workflow_file"], "repository": context["repository"],
		"workflow_run_id": runID, "workflow_run_attempt": attempt, "name": entry["name"], "command": entry["command"],
		"plan_sha256": entry["sha256"], "target": Object{"kind": kind, "number": rawNumber(number, target["number"])},
		"event_path": policy.event.path, "event_sha256": attemptTarget["event_sha256"],
	}
	if !Equal(review, wantReview) {
		return recoveryError("saved execution review does not bind the exact event, target and plan")
	}
	return nil
}

func (e *Engine) validatePlanSetDispatch(context, entry, plan Object, reader *policyReader, policy planPolicy) error {
	status, statusOK := entry["status"].(string)
	phase, phaseOK := context["phase"].(string)
	if !statusOK || !phaseOK || !contains([]string{"prepared", "dispatching", "completed"}, status) || !contains([]string{"prepared", "dispatching", "completed"}, phase) {
		return recoveryError("plan-set workflow or context has an unsupported phase")
	}
	if (status == "completed" && phase != "completed") || (status == "prepared" && phase != "prepared") {
		return recoveryError("plan status differs from the enclosing workflow context")
	}
	_, hasPath := entry["review_path"]
	_, hasDigest := entry["review_sha256"]
	if !hasPath && !hasDigest {
		if status == "prepared" && phase == "prepared" {
			return nil
		}
		return recoveryError("dispatched or completed recovery plan lacks its exact approval event")
	}
	if !hasPath || !hasDigest {
		return recoveryError("recovery approval path and digest must be bound together")
	}
	review, err := readReview(reader, entry)
	if err != nil {
		return err
	}
	runID, attempt := contextRunOrigin(context)
	if len(review) != 13 || !exactInt(review["schema_version"], 1) || review["workflow_file"] != context["workflow_file"] ||
		review["repository"] != context["repository"] || review["plan_name"] != entry["name"] || review["command"] != policy.command ||
		review["plan_sha256"] != entry["sha256"] || review["approval_input"] != policy.approval.approvalInput || review["workflow_event"] != "workflow_dispatch" ||
		!exactInt(review["workflow_run_id"], runID) || !exactInt(review["workflow_run_attempt"], attempt) ||
		!positiveInt(review["reviewed_plan_run_id"]) || review["event_path"] != policy.event.path || !IsSHA256(review["event_sha256"]) {
		return recoveryError("saved recovery approval does not bind the exact workflow plan")
	}
	event, bytes, err := reader.read(policy.event.path, "dispatch event")
	if err != nil || SHA256(bytes) != review["event_sha256"] {
		return recoveryError("saved dispatch event bytes differ from their exact approval receipt")
	}
	if !stringEqualFold(nestedString(event, "repository", "full_name"), context["repository"]) {
		return recoveryError("dispatch event belongs to another repository")
	}
	inputs, err := object(event["inputs"], "workflow dispatch inputs")
	if err != nil {
		return err
	}
	if !deepEventInputMatches(inputs[policy.approval.approvalInput], fmt.Sprint(entry["sha256"])) ||
		!deepEventInputMatches(inputs[policy.approval.reviewedRunInput], fmt.Sprint(review["reviewed_plan_run_id"])) {
		return recoveryError("dispatch event does not prove approval of the exact retained plan")
	}
	for inputName, expected := range policy.approval.requiredInputs {
		if !Equal(inputs[inputName], expected) {
			return recoveryError("dispatch event lacks an exact required approval input")
		}
	}
	_ = plan
	return nil
}

func (e *Engine) validateParentMergeContinuation(context, entry, plan Object, reader *policyReader, policy planPolicy) error {
	if policy.parentMerge == nil || policy.parentMerge.kind != "completed_merge" {
		return recoveryError("closeout policy lacks its exact completed parent merge contract")
	}
	return e.validateCompletedMergeContinuation(context, entry, plan, reader, policy, *policy.parentMerge)
}

func completedMergeCloseout(workflowPolicy workflowPolicy) (string, planPolicy, bool) {
	var name string
	var closeout planPolicy
	for candidate, plan := range workflowPolicy.plans {
		if plan.profile != "closeout" || plan.parentMerge == nil || plan.parentMerge.kind != "completed_merge" {
			continue
		}
		if name != "" {
			return "", planPolicy{}, false
		}
		name, closeout = candidate, plan
	}
	return name, closeout, name != ""
}

func (e *Engine) completedMergeParentOnlyContext(context Object, childName, childCommand string, workflowPolicy workflowPolicy) (Object, error) {
	configuredName, closeout, ok := completedMergeCloseout(workflowPolicy)
	if !ok || configuredName != childName || closeout.command != childCommand || closeout.parentMerge == nil {
		return nil, recoveryError("workflow has no exact completed-merge closeout for this plan")
	}
	if context["phase"] != "completed" || context["parent_merge"] != nil || context["recovered_from_run_id"] != nil || context["recovered_from_attempt"] != nil {
		return nil, recoveryError("composed cleanup requires a same-run, unrecovered completed merge context")
	}
	plans, err := array(context["plans"], "completed merge context plans")
	if err != nil || len(plans) != 1 {
		return nil, recoveryError("composed cleanup requires exactly one completed merge parent")
	}
	parent, err := exactWithOptional(plans[0], runContextPlanRequiredFields, runContextPlanOptionalFields, "completed merge parent entry")
	if err != nil || parent["name"] != closeout.parentMerge.planName || parent["command"] != closeout.parentMerge.command || parent["status"] != "completed" {
		return nil, recoveryError("composed cleanup parent is not the exact configured completed merge plan")
	}
	if !Equal(context["attempt_target"], Object{"plan_sha256": parent["sha256"]}) {
		return nil, recoveryError("composed cleanup source target differs from its exact completed merge plan")
	}
	return parent, nil
}

func (e *Engine) completedMergeContextEntries(context Object, parentPolicy *parentMergePolicy) (Object, Object, error) {
	if parentPolicy == nil || parentPolicy.kind != "completed_merge" || context["parent_merge"] != nil {
		return nil, nil, recoveryError("composed cleanup context lacks its closed completed-merge contract")
	}
	wfPolicy, err := e.workflowPolicy(fmt.Sprint(context["workflow_file"]))
	if err != nil {
		return nil, nil, err
	}
	childName, closeout, ok := completedMergeCloseout(wfPolicy)
	if !ok || closeout.parentMerge == nil || *closeout.parentMerge != *parentPolicy {
		return nil, nil, recoveryError("workflow policy does not declare one exact completed-merge closeout")
	}
	plans, err := array(context["plans"], "composed cleanup plan inventory")
	if err != nil || len(plans) != 2 {
		return nil, nil, recoveryError("composed cleanup must preserve exactly one parent and one child plan")
	}
	parent, err := exactWithOptional(plans[0], runContextPlanRequiredFields, runContextPlanOptionalFields, "composed cleanup parent entry")
	if err != nil || parent["name"] != parentPolicy.planName || parent["command"] != parentPolicy.command || parent["status"] != "completed" {
		return nil, nil, recoveryError("composed cleanup lost its exact completed parent merge entry")
	}
	child, err := exactWithOptional(plans[1], runContextPlanRequiredFields, runContextPlanOptionalFields, "composed cleanup child entry")
	if err != nil || child["name"] != childName || child["command"] != closeout.command {
		return nil, nil, recoveryError("composed cleanup does not contain the configured closeout child")
	}
	if !Equal(context["attempt_target"], Object{"plan_sha256": parent["sha256"]}) {
		return nil, nil, recoveryError("composed cleanup target does not preserve the exact completed merge plan")
	}
	status := fmt.Sprint(child["status"])
	phase := fmt.Sprint(context["phase"])
	allowed := map[string]bool{"prepared": status == "prepared", "dispatching": status == "dispatching", "completed": status == "completed"}
	if !allowed[phase] {
		return nil, nil, recoveryError("composed cleanup phase differs from its exact child plan status")
	}
	return parent, child, nil
}

func (e *Engine) validateCompletedMergeContinuation(context, childEntry, childPlan Object, reader *policyReader, childPolicy planPolicy, parentPolicy parentMergePolicy) error {
	parentEntry, recordedChild, err := e.completedMergeContextEntries(context, &parentPolicy)
	if err != nil || !Equal(childEntry, recordedChild) {
		return recoveryError("composed cleanup plan is not the exact child of its completed merge context")
	}
	if context["recovered_from_run_id"] != nil && (!positiveInt(context["recovered_from_run_id"]) || !positiveInt(context["recovered_from_attempt"])) {
		return recoveryError("composed cleanup observer has an incomplete source attempt identity")
	}
	parentValue, parentBytes, err := reader.read(fmt.Sprint(parentEntry["path"]), "completed parent merge plan")
	if err != nil {
		return err
	}
	parentPlan, err := object(parentValue, "completed parent merge plan")
	if err != nil {
		return err
	}
	journalPath := "journal/" + fmt.Sprint(parentEntry["journal_id"]) + ".json"
	if value, exists := parentEntry["journal_path"]; exists {
		journalPath = fmt.Sprint(value)
	}
	journalValue, journalBytes, err := reader.read(journalPath, "completed parent merge journal")
	if err != nil {
		return err
	}
	journal, err := object(journalValue, "completed parent merge journal")
	if err != nil {
		return err
	}
	resultPath := "apply-results/" + fmt.Sprint(parentEntry["name"]) + ".json"
	if value, exists := parentEntry["apply_result_path"]; exists {
		resultPath = fmt.Sprint(value)
	}
	resultValue, resultBytes, err := reader.read(resultPath, "completed parent merge apply result")
	if err != nil {
		return err
	}
	if _, err := object(resultValue, "completed parent merge apply result"); err != nil {
		return err
	}
	parentProof := Object{
		"name": parentEntry["name"], "command": parentEntry["command"], "plan_sha256": parentEntry["sha256"],
		"journal_id": parentEntry["journal_id"], "plan_file": MakeFileProof(parentBytes),
		"journal_file": MakeFileProof(journalBytes), "apply_result_file": MakeFileProof(resultBytes),
	}
	if retained, ok := reader.nativePlans[fmt.Sprint(parentEntry["name"])]; ok {
		for field, expected := range map[string]any{"plan_file": parentProof["plan_file"], "journal_file": parentProof["journal_file"]} {
			if !Equal(retained[field], expected) {
				return recoveryError("completed merge %s differs from its immutable source proof", field)
			}
		}
		if retainedResult, exists := retained["apply_result_file"]; exists && !Equal(retainedResult, parentProof["apply_result_file"]) {
			return recoveryError("completed merge apply result differs from its immutable source proof")
		}
	}
	decodedParent, err := ValidateTerminalPlanProof(parentProof)
	if err != nil || !Equal(decodedParent, parentPlan) {
		return recoveryError("completed merge lacks its exact positive native terminal proof: %v", err)
	}
	parentWorkflowPolicy, err := e.workflowPolicy(fmt.Sprint(context["workflow_file"]))
	if err != nil {
		return err
	}
	mergePolicy, ok := parentWorkflowPolicy.plans[parentPolicy.planName]
	if !ok || mergePolicy.profile != "merge" || mergePolicy.command != parentPolicy.command {
		return recoveryError("completed merge parent no longer matches its exact domain policy")
	}
	originRunID, originAttempt := contextRunOrigin(context)
	if originRunID < 1 || originAttempt < 1 {
		return recoveryError("completed merge context lacks its exact original run and attempt")
	}
	parentContext, err := cloneObject(context)
	if err != nil {
		return err
	}
	parentContext["phase"] = "completed"
	parentContext["workflow_run_id"], parentContext["workflow_run_attempt"] = originRunID, originAttempt
	parentContext["attempt_target"] = Object{"plan_sha256": parentEntry["sha256"]}
	parentContext["plans"] = []any{parentEntry}
	delete(parentContext, "recovered_from_run_id")
	delete(parentContext, "recovered_from_attempt")
	if err := e.validateRecoveryPlanWithReader(parentContext, parentEntry, parentPlan, reader); err != nil {
		return recoveryError("completed merge approval or event is invalid: %v", err)
	}
	parsedParent, err := contract.ParsePlan(parentPlan)
	if err != nil {
		return err
	}
	runtimeRepository := parsedParent.Repository.Object()
	mergeAdapter := workflow.MergeAdapter{}
	operations, err := mergeAdapter.Operations(parsedParent)
	if err != nil || len(operations) != 1 || !Equal(operationObjects(operations), parentPlan["operations"]) {
		return recoveryError("completed merge plan no longer passes its exact embedded business policy")
	}
	steps, err := array(journal["steps"], "completed merge journal steps")
	if err != nil || len(steps) != 1 {
		return recoveryError("completed merge must have exactly one positive native dispatch")
	}
	step, err := object(steps[0], "completed merge journal step")
	if err != nil || step["status"] != "completed" || step["id"] != operations[0].ID {
		return recoveryError("completed merge journal does not bind its exact reviewed operation")
	}
	receipt, err := object(step["result"], "completed merge receipt")
	if err != nil {
		return err
	}
	if err := mergeAdapter.ValidateReceipt(parsedParent, operations[0], receipt); err != nil {
		return recoveryError("completed merge lacks a positive exact provider acknowledgement")
	}
	mergeACK, err := contract.ObjectAt(receipt, "provider_ack")
	if err != nil {
		return recoveryError("completed merge receipt lacks its exact provider acknowledgement")
	}
	mergeCommitSHA, err := contract.Nonempty(mergeACK, "sha")
	if err != nil {
		return recoveryError("completed merge receipt lacks its exact merge commit")
	}
	settingsValue, _, err := reader.read(parentPolicy.settingsPath, "completed merge repository settings")
	if err != nil {
		return err
	}
	settings, err := Exact(settingsValue, []string{
		"schema_version", "repository", "repository_node_id", "owner_login", "owner_type", "name", "default_branch", "delete_branch_on_merge",
	}, "completed merge repository settings")
	if err != nil {
		return err
	}
	data, err := object(parentPlan["data"], "completed merge plan data")
	if err != nil {
		return err
	}
	inventory, err := object(data["inventory"], "completed merge inventory")
	if err != nil {
		return err
	}
	repositoryInventory, err := object(inventory["repository"], "completed merge repository inventory")
	if err != nil {
		return err
	}
	ownerLogin, ownerOK := settings["owner_login"].(string)
	repositoryName, nameOK := settings["name"].(string)
	_, deleteOK := settings["delete_branch_on_merge"].(bool)
	if !exactInt(settings["schema_version"], 1) || !Equal(settings["repository"], runtimeRepository) ||
		!Equal(runtimeRepository, e.repository.Object()) || !nonemptyString(settings["repository_node_id"]) || !ownerOK || !strings.EqualFold(ownerLogin, e.repository.Owner) ||
		(settings["owner_type"] != "User" && settings["owner_type"] != "Organization") || !nameOK || !strings.EqualFold(repositoryName, e.repository.Name) ||
		!nonemptyString(settings["default_branch"]) || strings.ContainsAny(fmt.Sprint(settings["default_branch"]), "\r\n\x00") || !deleteOK ||
		repositoryInventory["id"] != settings["repository_node_id"] || repositoryInventory["defaultBranch"] != settings["default_branch"] {
		return recoveryError("completed merge repository settings differ from its exact reviewed inventory")
	}
	pr, err := object(inventory["pull_request"], "completed merge pull request inventory")
	if err != nil {
		return err
	}
	branch, branchOK := pr["headRefName"].(string)
	headSHA, shaOK := pr["headRefOid"].(string)
	number, err := positiveInteger(pr["number"], "completed merge pull request number")
	if err != nil || !branchOK || !nonemptyString(branch) || branch == settings["default_branch"] || !shaOK || !settlementSHA40.MatchString(headSHA) ||
		!exactInt(operations[0].Target["number"], number) || operations[0].Target["head_sha"] != headSHA {
		return recoveryError("completed merge does not identify one safe exact non-default branch")
	}
	wantSelection := Object{"branches": []any{Object{"name": branch, "sha": headSHA, "pull_request_number": number}}}
	childData, err := object(childPlan["data"], "composed cleanup plan data")
	if err != nil {
		return err
	}
	actualSelection, err := object(childData["selection"], "composed cleanup selection")
	if err != nil {
		return err
	}
	selection, err := workflow.ParseBranchCleanupSelection(actualSelection, e.repository)
	if err != nil || !Equal(selection, wantSelection) {
		return recoveryError("branch cleanup selection differs from the exact positively acknowledged merge head")
	}
	if childPolicy.parentMerge == nil || *childPolicy.parentMerge != parentPolicy {
		return recoveryError("composed cleanup parent policy changed during validation")
	}
	parsedChild, err := contract.ParsePlan(childPlan)
	if err != nil {
		return recoveryError("composed cleanup child is not an exact native plan")
	}
	cleanupOperations, err := (workflow.BranchCleanup{}).Operations(parsedChild)
	if err != nil || !Equal(operationObjects(cleanupOperations), childPlan["operations"]) {
		return recoveryError("composed cleanup child does not pass its exact branch-cleanup policy")
	}
	if len(cleanupOperations) == 0 && !isAllAbsentBranchCleanup(parsedChild) {
		return recoveryError("zero-operation cleanup lacks exact positive absent-branch evidence")
	}
	cleanupInventory, err := object(childData["inventory"], "composed cleanup inventory")
	if err != nil || cleanupInventory["repository_node_id"] != settings["repository_node_id"] {
		return recoveryError("branch cleanup repository incarnation differs from the completed merge")
	}
	cleanupRows, err := contract.Objects(cleanupInventory, "branches")
	if err != nil || len(cleanupRows) != 1 {
		return recoveryError("composed cleanup must retain its exact merged pull request row")
	}
	cleanupRow, err := object(cleanupRows[0], "composed cleanup branch row")
	if err != nil {
		return err
	}
	cleanupPR, err := object(cleanupRow["pull_request"], "composed cleanup pull request")
	if err != nil || !nonemptyString(pr["id"]) || cleanupPR["id"] != pr["id"] || cleanupPR["merge_commit_sha"] != mergeCommitSHA {
		return recoveryError("branch cleanup pull request identity differs from the completed merge")
	}
	if rawEvidence, exists := cleanupInventory["branch_evidence"]; exists {
		evidence, err := object(rawEvidence, "composed cleanup branch evidence")
		if err != nil || evidence["delete_branch_on_merge"] != settings["delete_branch_on_merge"] {
			return recoveryError("branch cleanup retention setting differs from the completed merge inventory")
		}
	}
	return nil
}

// isAllAbsentBranchCleanup recognizes only a nonempty, complete no-op cleanup
// child. The caller must first validate its completed-merge continuation and
// retained parent receipt through validateCompletedMergeContinuation.
func isAllAbsentBranchCleanup(plan contract.Plan) bool {
	if plan.Command != workflow.BranchCleanupCommand || len(plan.Operations) != 0 {
		return false
	}
	data := plan.Data
	selection, err := contract.ObjectAt(data, "selection")
	if err != nil {
		return false
	}
	selected, err := contract.Objects(selection, "branches")
	if err != nil || len(selected) == 0 {
		return false
	}
	absent, err := contract.Objects(data, "already_absent")
	if err != nil || len(absent) != len(selected) {
		return false
	}
	inventory, err := contract.ObjectAt(data, "inventory")
	if err != nil {
		return false
	}
	if _, ok := inventory["branch_evidence"]; !ok {
		return false
	}
	operations, err := (workflow.BranchCleanup{}).Operations(plan)
	return err == nil && len(operations) == 0
}

// completedMergeZeroOperationCleanup identifies the sole journal-free native
// continuation that can be observed safely: an existing dispatching cleanup
// child whose exact merged parent and all-absent child evidence were validated
// by the caller's recovery-policy pass.
func (e *Engine) completedMergeZeroOperationCleanup(context, entry, plan Object) bool {
	name, ok := entry["name"].(string)
	if !ok {
		return false
	}
	wfPolicy, err := e.workflowPolicy(fmt.Sprint(context["workflow_file"]))
	if err != nil {
		return false
	}
	childName, closeout, ok := completedMergeCloseout(wfPolicy)
	if !ok || name != childName || closeout.parentMerge == nil || entry["status"] != "dispatching" {
		return false
	}
	parent, child, err := e.completedMergeContextEntries(context, closeout.parentMerge)
	if err != nil || parent["status"] != "completed" || !Equal(child, entry) {
		return false
	}
	parsed, err := contract.ParsePlan(plan)
	return err == nil && parsed.SHA256 == entry["sha256"] && isAllAbsentBranchCleanup(parsed)
}

func (e *Engine) completedMergeParentEntry(context, entry Object) bool {
	wfPolicy, err := e.workflowPolicy(fmt.Sprint(context["workflow_file"]))
	if err != nil {
		return false
	}
	_, closeout, ok := completedMergeCloseout(wfPolicy)
	if !ok || closeout.parentMerge == nil || entry["name"] != closeout.parentMerge.planName ||
		entry["command"] != closeout.parentMerge.command || entry["status"] != "completed" {
		return false
	}
	parent, _, err := e.completedMergeContextEntries(context, closeout.parentMerge)
	return err == nil && Equal(parent, entry)
}

func operationObjects(operations []contract.Operation) []any {
	result := make([]any, 0, len(operations))
	for _, operation := range operations {
		result = append(result, Object{
			"id": operation.ID, "kind": operation.Kind, "target": operation.Target,
			"before": operation.Before, "after": operation.After,
		})
	}
	return result
}

func contextRunOrigin(context Object) (int64, int64) {
	runValue, attemptValue := context["workflow_run_id"], context["workflow_run_attempt"]
	if context["plan_origin_run_id"] != nil || context["plan_origin_attempt"] != nil {
		if context["plan_origin_run_id"] == nil || context["plan_origin_attempt"] == nil {
			return 0, 0
		}
		runValue, attemptValue = context["plan_origin_run_id"], context["plan_origin_attempt"]
	} else if context["recovered_from_run_id"] != nil || context["recovered_from_attempt"] != nil {
		if context["recovered_from_run_id"] == nil || context["recovered_from_attempt"] == nil {
			return 0, 0
		}
		runValue, attemptValue = context["recovered_from_run_id"], context["recovered_from_attempt"]
	}
	runID, runErr := contract.PositiveInteger(runValue)
	attempt, attemptErr := contract.PositiveInteger(attemptValue)
	if runErr != nil || attemptErr != nil {
		return 0, 0
	}
	return runID, attempt
}

func positiveInt(value any) bool { _, err := contract.PositiveInteger(value); return err == nil }
func rawNumber(number int64, original any) any {
	if _, err := contract.Integer(original); err == nil {
		return original
	}
	return number
}
func stringEqualFold(left, right any) bool {
	a, aOK := left.(string)
	b, bOK := right.(string)
	return aOK && bOK && strings.EqualFold(a, b)
}
func nestedString(value Object, first, second string) string {
	child, ok := value[first].(map[string]any)
	if !ok {
		return ""
	}
	text, _ := child[second].(string)
	return text
}
func deepEventInputMatches(value any, expected string) bool {
	if text, ok := value.(string); ok {
		return text == expected
	}
	number, err := contract.Integer(value)
	return err == nil && fmt.Sprint(number) == expected
}
func deepTruthy(value any) bool { return value == true || value == "true" }
func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
