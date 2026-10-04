package governance

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

func ExecutionPreflight(policy, snapshot contract.Object) (contract.Object, error) {
	intent, err := requiredString(policy, "intent")
	if err != nil {
		return nil, err
	}
	if !contains([]string{"push", "open-pr", "finish", "delete-branch"}, intent) {
		return nil, fmt.Errorf("unsupported execution preflight intent %q", intent)
	}
	repo, err := object(snapshot, "repo")
	if err != nil {
		return nil, err
	}
	defaultBranch, err := requiredString(repo, "defaultBranch")
	if err != nil {
		return nil, err
	}
	git, err := object(snapshot, "git")
	if err != nil {
		return nil, err
	}
	currentBranch, err := requiredString(git, "current_branch")
	if err != nil {
		return nil, err
	}
	headCommit, err := requiredString(git, "head_commit")
	if err != nil {
		return nil, err
	}
	statusEntries, err := contract.Strings(git["worktree_entries"])
	if err != nil {
		return nil, errors.New("git.worktree_entries must be an array of status entries")
	}
	worktree := worktreeSummary(statusEntries)
	branches, err := stringList(snapshot, "active_issue_branches", true)
	if err != nil {
		return nil, err
	}
	pr, err := optionalObject(snapshot, "pull_request")
	if err != nil {
		return nil, err
	}
	issue, err := optionalObject(snapshot, "issue")
	if err != nil {
		return nil, err
	}
	baseBranch, exists, err := optionalString(policy, "base_branch")
	if err != nil {
		return nil, err
	}
	if !exists || baseBranch == "" {
		baseBranch, exists, err = optionalString(pr, "base_branch")
		if err != nil {
			return nil, err
		}
	}
	if !exists || baseBranch == "" {
		baseBranch = defaultBranch
	}
	headBranch, exists, err := optionalString(pr, "head_branch")
	if err != nil {
		return nil, err
	}
	if !exists || headBranch == "" {
		headBranch = currentBranch
	}
	stacked := baseBranch != defaultBranch
	branchContext := contract.Object{
		"default_branch":           defaultBranch,
		"base_branch":              baseBranch,
		"head_branch":              headBranch,
		"is_default_branch_action": baseBranch == defaultBranch,
		"is_stacked_branch":        stacked,
		"is_detached_head":         currentBranch == "HEAD",
	}
	defaultBranchBlocked := intent == "open-pr" && headBranch == defaultBranch
	dirtyWorktreeBlocked := (intent == "open-pr" || intent == "finish") && worktree["has_tracked_changes"] == true
	blockingReasons := []any{}
	if defaultBranchBlocked {
		blockingReasons = append(blockingReasons, "execution-state open-pr refuses to operate on the default branch; create or switch to a feature branch first.")
	}
	if dirtyWorktreeBlocked {
		blockingReasons = append(blockingReasons, "Tracked worktree changes must be committed, reverted, or moved into a separate worktree before this action.")
	}
	guardrails := contract.Object{"default_branch_blocked": defaultBranchBlocked, "dirty_worktree_blocked": dirtyWorktreeBlocked, "blocking_reasons": blockingReasons}
	worktreeReasons := []any{}
	if len(branches) > 1 {
		worktreeReasons = append(worktreeReasons, "More than one active issue branch exists.")
	}
	if stacked {
		worktreeReasons = append(worktreeReasons, "This action targets a stacked non-default base branch.")
	}
	if currentBranch == "HEAD" {
		worktreeReasons = append(worktreeReasons, "Detached HEAD is for throwaway exploration only. Create or switch to a named branch before the first commit, push, or GitHub mutation.")
	}
	mergeMethod := ""
	if intent == "finish" {
		mergeMethods, err := object(policy, "merge_methods")
		if err != nil {
			return nil, errors.New("finish preflight requires explicit merge_methods policy")
		}
		defaultBaseMethod, err := requiredString(mergeMethods, "default_base")
		if err != nil {
			return nil, err
		}
		stackedMethod, err := requiredString(mergeMethods, "stacked")
		if err != nil {
			return nil, err
		}
		mergeMethod, _, err = optionalString(policy, "merge_method")
		if err != nil {
			return nil, err
		}
		if mergeMethod == "" {
			if stacked {
				mergeMethod = stackedMethod
			} else {
				mergeMethod = defaultBaseMethod
			}
		}
		if !contains([]string{"merge", "rebase", "squash"}, mergeMethod) {
			return nil, errors.New("merge_method must be merge, rebase, or squash")
		}
	}
	planned := contract.Object{"intent": intent}
	if contains([]string{"push", "open-pr", "finish", "delete-branch"}, intent) {
		planned["head_branch"] = headBranch
	}
	if intent == "open-pr" || intent == "finish" {
		planned["base_branch"] = baseBranch
	}
	if intent == "finish" {
		planned["merge_method"] = mergeMethod
	}
	result := contract.Object{
		"command": "preflight", "intent": intent, "repo": repo,
		"git":   contract.Object{"current_branch": currentBranch, "head_commit": headCommit, "worktree": worktree},
		"issue": nil, "pull_request": nil, "branch_context": branchContext,
		"guardrails": guardrails, "active_issue_branch_count": len(branches),
		"active_issue_branches":   asAny(branches),
		"worktree_recommendation": contract.Object{"recommended": len(worktreeReasons) > 0, "reasons": worktreeReasons},
		"planned_action":          planned,
	}
	if len(issue) > 0 {
		result["issue"] = issue
	}
	if len(pr) > 0 {
		result["pull_request"] = pr
	}
	if stamp, ok := snapshot["generated_at"].(string); ok {
		result["generated_at"] = stamp
	}
	return result, nil
}

func worktreeSummary(entries []string) contract.Object {
	tracked, untracked := []any{}, []any{}
	for _, entry := range entries {
		if strings.HasPrefix(entry, "?? ") {
			untracked = append(untracked, entry)
		} else {
			tracked = append(tracked, entry)
		}
	}
	return contract.Object{
		"is_clean": len(entries) == 0, "changed_file_count": len(entries),
		"entries": asAny(entries), "has_tracked_changes": len(tracked) > 0,
		"tracked_changed_file_count": len(tracked), "tracked_entries": tracked,
		"untracked_file_count": len(untracked), "untracked_entries": untracked,
	}
}

func ExecutionTransition(policy, snapshot contract.Object) (contract.Object, error) {
	issue, err := object(snapshot, "issue")
	if err != nil {
		return nil, err
	}
	pr, hasPR, err := optionalObjectPresent(snapshot, "pull_request")
	if err != nil {
		return nil, err
	}
	if !hasPR {
		return contract.Object{"command": "sync", "issue": issue, "pull_request": nil, "sync_state": "no-linked-pr", "actions": []any{}, "final_status": nil}, nil
	}
	statuses, err := object(policy, "statuses")
	if err != nil {
		return nil, err
	}
	done, err := requiredString(statuses, "done")
	if err != nil {
		return nil, err
	}
	active, err := requiredString(statuses, "active")
	if err != nil {
		return nil, err
	}
	todo, err := requiredString(statuses, "todo")
	if err != nil {
		return nil, err
	}
	if done == active || done == todo || active == todo {
		return nil, errors.New("execution status policy values must be distinct")
	}
	merged, err := contract.Bool(pr, "is_merged")
	if err != nil {
		return nil, err
	}
	draft, err := contract.Bool(pr, "is_draft")
	if err != nil {
		return nil, err
	}
	state, err := requiredString(pr, "state")
	if err != nil {
		return nil, err
	}
	result := contract.Object{"command": "sync", "issue": issue, "pull_request": pr, "actions": []any{"reopen-issue"}}
	switch {
	case merged:
		result["sync_state"] = "merged"
		result["actions"] = []any{"close-issue"}
		result["final_status"] = done
	case state == "OPEN" || draft:
		result["sync_state"] = "draft-or-open"
		result["final_status"] = active
	default:
		branchExists, err := contract.Bool(snapshot, "branch_exists")
		if err != nil {
			return nil, errors.New("branch_exists must be supplied for a closed unmerged pull request")
		}
		if branchExists {
			result["sync_state"] = "closed-unmerged-branch-live"
			result["final_status"] = active
		} else {
			result["sync_state"] = "closed-unmerged-branch-deleted"
			result["final_status"] = todo
		}
	}
	if stamp, ok := snapshot["generated_at"].(string); ok {
		result["generated_at"] = stamp
	}
	return result, nil
}

func ExecutionLinkFacts(policy, snapshot contract.Object) (contract.Object, error) {
	issueNumber, err := positiveInteger(snapshot, "issue_number")
	if err != nil {
		return nil, err
	}
	defaultBranch, err := requiredString(snapshot, "default_branch")
	if err != nil {
		return nil, err
	}
	pr, err := object(snapshot, "pull_request")
	if err != nil {
		return nil, err
	}
	linkedPrefix, err := requiredString(policy, "linked_issue_marker_prefix")
	if err != nil {
		return nil, err
	}
	linkStatePrefix, err := requiredString(policy, "link_state_marker_prefix")
	if err != nil {
		return nil, err
	}
	body, _, err := optionalString(pr, "body")
	if err != nil {
		return nil, err
	}
	baseBranch, _, err := optionalString(pr, "base_branch")
	if err != nil {
		return nil, err
	}
	closingRaw, ok := pr["closing_issue_numbers"]
	if !ok {
		return nil, errors.New("pull_request.closing_issue_numbers is required")
	}
	closing, ok := closingRaw.([]any)
	if !ok {
		return nil, errors.New("pull_request.closing_issue_numbers must be an array")
	}
	closingSet := map[int64]bool{}
	for _, raw := range closing {
		number, err := contract.PositiveInteger(raw)
		if err != nil {
			return nil, errors.New("closing_issue_numbers must contain positive issue numbers")
		}
		closingSet[number] = true
	}
	closingNumbers := make([]int64, 0, len(closingSet))
	for n := range closingSet {
		closingNumbers = append(closingNumbers, n)
	}
	sortInt64(closingNumbers)
	isDefault := baseBranch == defaultBranch
	expectedLinkState, verificationState := "manual-link-required", "manual-link-unverified"
	if isDefault {
		expectedLinkState = "auto-link-expected"
		if closingSet[issueNumber] {
			verificationState = "auto-link-verified"
		} else {
			verificationState = "auto-link-unverified"
		}
	}
	linkedMarker := fmt.Sprintf("%s%d -->", linkedPrefix, issueNumber)
	linkStateMarker := fmt.Sprintf("%s%s -->", linkStatePrefix, expectedLinkState)
	usesCloses := issueReference(body, "Closes", issueNumber)
	usesRefs := issueReference(body, "Refs", issueNumber)
	result := contract.Object{
		"linked_issue_number":        issueNumber,
		"body_mentions_issue":        strings.Contains(body, fmt.Sprintf("Issue: #%d", issueNumber)),
		"body_uses_closes":           usesCloses,
		"body_uses_refs":             usesRefs,
		"body_has_execution_marker":  strings.Contains(body, linkedMarker),
		"expected_body_link_state":   expectedLinkState,
		"body_declares_link_state":   strings.Contains(body, "Link state: "+expectedLinkState),
		"body_has_link_state_marker": strings.Contains(body, linkStateMarker),
		"closing_issue_numbers":      intsAsAny(closingNumbers),
		"closing_keyword_verified":   closingSet[issueNumber],
		"link_verification_state":    verificationState,
		"parsed_issue_number":        parseIssueNumberFromPRBody(body, linkedPrefix),
	}
	return result, nil
}

func issueReference(body, keyword string, number int64) bool {
	pattern := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(keyword) + `\s+#` + fmt.Sprint(number) + `\b`)
	return pattern.MatchString(body)
}

func parseIssueNumberFromPRBody(body, markerPrefix string) any {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(regexp.QuoteMeta(markerPrefix) + `([0-9]+)\s*-->`),
		regexp.MustCompile(`(?i)\bIssue:\s*#([0-9]+)\b`),
		regexp.MustCompile(`(?i)\b(?:Closes|Refs)\s+#([0-9]+)\b`),
	}
	for _, pattern := range patterns {
		match := pattern.FindStringSubmatch(body)
		if len(match) == 2 {
			var number int64
			if _, err := fmt.Sscan(match[1], &number); err == nil && number > 0 {
				return number
			}
		}
	}
	return nil
}

func sortInt64(values []int64) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func intsAsAny(values []int64) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

func optionalObjectPresent(o contract.Object, key string) (contract.Object, bool, error) {
	raw, exists := o[key]
	if !exists || raw == nil {
		return nil, false, nil
	}
	value, ok := raw.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("%s must be an object or null", key)
	}
	return value, true, nil
}
