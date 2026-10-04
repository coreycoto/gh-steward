package governance

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func MergeEligibility(policy, snapshot contract.Object) (contract.Object, error) {
	workflowName, err := requiredString(policy, "workflow_name")
	if err != nil {
		return nil, err
	}
	workflowEvent, err := requiredString(policy, "workflow_event")
	if err != nil {
		return nil, err
	}
	requiredLabel, err := requiredString(policy, "required_label")
	if err != nil {
		return nil, err
	}
	passBucket, err := requiredString(policy, "pass_bucket")
	if err != nil {
		return nil, err
	}
	mergeMethod, err := requiredString(policy, "merge_method")
	if err != nil {
		return nil, err
	}
	branchPatterns, err := stringList(policy, "branch_patterns", true)
	if err != nil {
		return nil, err
	}
	trustedLogins, err := stringList(policy, "trusted_logins", true)
	if err != nil {
		return nil, err
	}
	if len(branchPatterns) == 0 && len(trustedLogins) == 0 {
		return nil, errors.New("merge policy requires a branch pattern or trusted login")
	}
	repo, err := object(snapshot, "repository")
	if err != nil {
		return nil, err
	}
	repositoryName, err := requiredString(repo, "nameWithOwner")
	if err != nil || !repositoryNamePattern.MatchString(repositoryName) {
		return nil, errors.New("merge snapshot repository identity is invalid")
	}
	defaultBranch, err := requiredString(repo, "defaultBranch")
	if err != nil {
		return nil, errors.New("repository default branch is required for merge eligibility")
	}
	event, err := object(snapshot, "event")
	if err != nil {
		return nil, err
	}
	workflowRaw, hasWorkflow := event["workflow_run"]
	if !hasWorkflow || workflowRaw == nil {
		return mergeResult("noop", nil, "", contract.Object{}, nil, nil, nil), nil
	}
	workflow, ok := workflowRaw.(map[string]any)
	if !ok {
		return nil, errors.New("event.workflow_run must be an object")
	}
	rawPRs, ok := workflow["pull_requests"].([]any)
	if !ok {
		if workflow["pull_requests"] == nil {
			rawPRs = []any{}
		} else {
			return nil, errors.New("event.workflow_run.pull_requests must be an array")
		}
	}
	if len(rawPRs) == 0 {
		return mergeResult("noop", nil, "", triggerSummary(workflow, "", ""), nil, nil, nil), nil
	}
	blocking := []string{}
	verified := []string{}
	var prNumber int64
	if len(rawPRs) != 1 {
		blocking = append(blocking, fmt.Sprintf("The triggering CI workflow must identify exactly one pull request; it identified %d.", len(rawPRs)))
	} else {
		association, ok := rawPRs[0].(map[string]any)
		if !ok {
			return nil, errors.New("workflow pull request association must be an object")
		}
		prNumber, err = positiveInteger(association, "number")
		if err != nil {
			blocking = append(blocking, "The triggering CI workflow attached a pull request without a valid number.")
			prNumber = 0
		}
	}
	if workflow["name"] != workflowName {
		blocking = append(blocking, fmt.Sprintf("The triggering workflow must be %q; received %q.", workflowName, printable(workflow["name"])))
	}
	if workflow["event"] != workflowEvent {
		blocking = append(blocking, fmt.Sprintf("The triggering workflow event must be %q; received %q.", workflowEvent, printable(workflow["event"])))
	}
	if workflow["status"] != "completed" {
		blocking = append(blocking, fmt.Sprintf("The triggering CI workflow is not complete: %q.", printable(workflow["status"])))
	}
	conclusion, _ := workflow["conclusion"].(string)
	if !strings.EqualFold(conclusion, "success") {
		blocking = append(blocking, fmt.Sprintf("The triggering CI workflow did not conclude successfully: %q.", printable(workflow["conclusion"])))
	}
	eventRepository := ""
	if raw, ok := workflow["repository"].(map[string]any); ok {
		eventRepository, _ = raw["full_name"].(string)
	}
	if !repositoryNamePattern.MatchString(eventRepository) || !repositoryNamePattern.MatchString(repositoryName) {
		blocking = append(blocking, "The triggering workflow and checkout must identify an exact repository.")
	} else if !strings.EqualFold(eventRepository, repositoryName) {
		blocking = append(blocking, fmt.Sprintf("The triggering CI workflow belongs to a different repository: %q.", eventRepository))
	}
	triggerSHA, validTriggerSHA := normalizedSHA(workflow["head_sha"])
	if !validTriggerSHA {
		blocking = append(blocking, "The triggering CI workflow did not provide a valid 40-character head SHA.")
	}
	triggerBranch, branchOK := workflow["head_branch"].(string)
	triggerBranch = strings.TrimSpace(triggerBranch)
	if !branchOK || triggerBranch == "" {
		blocking = append(blocking, "The triggering CI workflow did not provide a head branch.")
		triggerBranch = ""
	}
	if len(rawPRs) == 1 {
		association := rawPRs[0].(map[string]any)
		associatedHead, ok := association["head"].(map[string]any)
		if !ok {
			blocking = append(blocking, "The pull request association head is unavailable.")
		} else {
			associatedSHA, valid := normalizedSHA(associatedHead["sha"])
			if !valid || !validTriggerSHA || associatedSHA != triggerSHA {
				blocking = append(blocking, "The pull request association head does not match the triggering CI head SHA.")
			}
			associatedBranch, valid := associatedHead["ref"].(string)
			if !valid || strings.TrimSpace(associatedBranch) != triggerBranch {
				blocking = append(blocking, "The pull request association branch does not match the triggering CI head branch.")
			}
		}
	}
	trigger := triggerSummary(workflow, eventRepository, triggerBranch)
	trigger["head_sha"] = triggerSHA
	if !validTriggerSHA {
		trigger["head_sha"] = nil
	}
	if len(blocking) > 0 {
		return mergeResult("blocked", ptrNumber(prNumber), "", trigger, nil, blocking, nil), nil
	}

	pr, err := object(snapshot, "pull_request")
	if err != nil {
		return nil, fmt.Errorf("eligible trigger requires a normalized pull_request snapshot: %w", err)
	}
	prIdentity, err := positiveInteger(pr, "number")
	if err != nil || prIdentity != prNumber {
		return nil, errors.New("pull request snapshot number does not match the triggering candidate")
	}
	prRepository, err := requiredString(pr, "repository")
	if err != nil || !repositoryNamePattern.MatchString(prRepository) || !strings.EqualFold(prRepository, repositoryName) {
		return nil, errors.New("pull request snapshot repository does not match the triggering candidate")
	}
	checks, err := objects(snapshot, "required_checks")
	if err != nil {
		return nil, err
	}
	verified, blocking = checkMergeCandidate(requiredLabel, passBucket, branchPatterns, trustedLogins, defaultBranch, triggerBranch, triggerSHA, pr, checks)
	status := "eligible"
	if len(blocking) > 0 {
		status = "blocked"
	}
	return mergeResult(status, ptrNumber(prNumber), mergeMethod, trigger, pr, blocking, verified), nil
}

func checkMergeCandidate(requiredLabel, passBucket string, branchPatterns, trustedLogins []string, defaultBranch, triggerBranch, triggerSHA string, pr contract.Object, checks []contract.Object) ([]string, []string) {
	verified, blocking := []string{}, []string{}
	state, _ := pr["state"].(string)
	if !strings.EqualFold(state, "OPEN") {
		blocking = append(blocking, fmt.Sprintf("PR state is %q, not OPEN.", printable(pr["state"])))
	} else {
		verified = append(verified, "PR is open")
	}
	draft, draftOK := pr["isDraft"].(bool)
	if !draftOK {
		blocking = append(blocking, "PR draft state is unavailable.")
	} else if draft {
		blocking = append(blocking, "PR is still a draft.")
	} else {
		verified = append(verified, "PR is not a draft")
	}
	if pr["baseRefName"] != defaultBranch {
		blocking = append(blocking, fmt.Sprintf("PR base branch %q is not the repository default branch %q.", printable(pr["baseRefName"]), defaultBranch))
	} else {
		verified = append(verified, "PR targets the default branch "+defaultBranch)
	}
	headBranch, branchOK := pr["headRefName"].(string)
	if !branchOK || headBranch == "" {
		blocking = append(blocking, "PR head branch is unavailable.")
	} else if headBranch != triggerBranch {
		blocking = append(blocking, fmt.Sprintf("PR head branch %q does not match the triggering CI branch %q.", headBranch, triggerBranch))
	} else {
		verified = append(verified, "PR head branch matches the triggering CI branch")
	}
	headSHA, validSHA := normalizedSHA(pr["headRefOid"])
	if !validSHA || triggerSHA == "" {
		blocking = append(blocking, "PR or triggering CI head SHA is unavailable or invalid.")
	} else if headSHA != triggerSHA {
		blocking = append(blocking, "PR head SHA does not match the triggering CI head SHA.")
	} else {
		verified = append(verified, "PR head SHA matches the triggering CI head SHA")
	}
	mergeState, _ := pr["mergeStateStatus"].(string)
	if !strings.EqualFold(mergeState, "CLEAN") {
		blocking = append(blocking, fmt.Sprintf("PR merge state is %q, not CLEAN.", printable(pr["mergeStateStatus"])))
	} else {
		verified = append(verified, "PR merge state is CLEAN")
	}
	reviewDecision, _ := pr["reviewDecision"].(string)
	reviewDecision = strings.ToUpper(reviewDecision)
	if reviewDecision != "" && reviewDecision != "APPROVED" {
		blocking = append(blocking, fmt.Sprintf("PR review state is %q, not merge-safe.", printable(pr["reviewDecision"])))
	} else if reviewDecision == "APPROVED" {
		verified = append(verified, "PR review state is approved")
	} else {
		verified = append(verified, "PR has no outstanding review restriction")
	}
	labels, err := contract.Objects(contract.Object{"labels": pr["labels"]}, "labels")
	labelSet := map[string]bool{}
	if err == nil {
		for _, item := range labels {
			if label, ok := item["name"].(string); ok {
				labelSet[strings.ToLower(strings.TrimSpace(label))] = true
			}
		}
	}
	if !labelSet[strings.ToLower(requiredLabel)] {
		blocking = append(blocking, fmt.Sprintf("PR is missing the required %q label.", requiredLabel))
	} else {
		verified = append(verified, fmt.Sprintf("PR has the %q label", requiredLabel))
	}
	if branchMatchesAny(branchPatterns, headBranch) {
		verified = append(verified, fmt.Sprintf("head branch %q matches the caller's trusted automation policy", headBranch))
	} else {
		author, _ := pr["author"].(map[string]any)
		login, _ := author["login"].(string)
		isBot, isBotKnown := author["is_bot"].(bool)
		trusted := false
		for _, allowed := range trustedLogins {
			if strings.EqualFold(strings.TrimSpace(login), strings.TrimSpace(allowed)) && isBotKnown && isBot {
				trusted = true
				break
			}
		}
		if trusted {
			verified = append(verified, fmt.Sprintf("author %q is a trusted automation bot", login))
		} else {
			blocking = append(blocking, "PR author and head branch do not match the caller's trusted automation policy")
		}
	}
	if len(checks) == 0 {
		blocking = append(blocking, "No required checks were reported for the PR.")
	} else {
		failed := []string{}
		for _, check := range checks {
			name, ok := check["name"].(string)
			if !ok || strings.TrimSpace(name) == "" {
				name = "unnamed check"
			}
			bucket, _ := check["bucket"].(string)
			if strings.EqualFold(bucket, passBucket) {
				verified = append(verified, "required check passed: "+name)
			} else {
				state, _ := check["state"].(string)
				if state == "" {
					state = bucket
				}
				if state == "" {
					state = "unknown"
				}
				failed = append(failed, fmt.Sprintf("%s (%s)", name, state))
			}
		}
		if len(failed) > 0 {
			blocking = append(blocking, "Required checks are not all green: "+strings.Join(failed, ", ")+".")
		} else {
			verified = append(verified, "all required checks passed")
		}
	}
	return verified, blocking
}

func triggerSummary(workflow contract.Object, repository, branch string) contract.Object {
	trigger := contract.Object{
		"workflow_name": workflow["name"], "workflow_event": workflow["event"],
		"workflow_status": workflow["status"], "workflow_conclusion": workflow["conclusion"],
		"workflow_run_id": workflow["id"], "repository": repository, "head_branch": branch,
	}
	if sha, ok := normalizedSHA(workflow["head_sha"]); ok {
		trigger["head_sha"] = sha
	} else {
		trigger["head_sha"] = nil
	}
	return trigger
}

func mergeResult(status string, prNumber *int64, mergeMethod string, trigger, pr contract.Object, reasons, verified []string) contract.Object {
	if trigger == nil {
		trigger = contract.Object{}
	}
	result := contract.Object{
		"schema_version": 1, "status": status, "summary": "",
		"pr_number": nil, "merge_method": nil, "verified_checks": contractStrings(verified),
		"blocking_reasons": contractStrings(reasons), "pull_request": contract.Object{},
		"triggering_workflow": trigger,
	}
	switch status {
	case "noop":
		result["summary"] = "No pull request is attached to the triggering workflow run."
	case "eligible":
		result["summary"] = fmt.Sprintf("PR #%d passed the deterministic merge gate.", *prNumber)
		result["merge_method"] = mergeMethod
		result["expected_head_sha"] = trigger["head_sha"]
	case "blocked":
		if prNumber != nil && *prNumber > 0 {
			result["summary"] = fmt.Sprintf("PR #%d is not eligible for automatic merge.", *prNumber)
		} else {
			result["summary"] = "The triggering workflow event is not eligible for automatic merge."
		}
	default:
		result["summary"] = "Merge eligibility evaluation did not complete."
	}
	if prNumber != nil && *prNumber > 0 {
		result["pr_number"] = *prNumber
	}
	if pr != nil {
		result["pull_request"] = pr
	}
	return result
}

func ptrNumber(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

func contractStrings(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}

func branchMatchesAny(patterns []string, branch string) bool {
	for _, pattern := range patterns {
		if globMatch(pattern, branch) {
			return true
		}
	}
	return false
}

func globMatch(pattern, value string) bool {
	var builder strings.Builder
	builder.WriteByte('^')
	for _, char := range pattern {
		switch char {
		case '*':
			builder.WriteString(".*")
		case '?':
			builder.WriteByte('.')
		default:
			builder.WriteString(regexp.QuoteMeta(string(char)))
		}
	}
	builder.WriteByte('$')
	compiled, err := regexp.Compile(builder.String())
	return err == nil && compiled.MatchString(value)
}
