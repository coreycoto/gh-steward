package workflow

import (
	"context"
	"errors"
	"fmt"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/planning"
	"sort"
	"strconv"
	"strings"
	"time"
)

func PrepareQuarterBacklog(ctx context.Context, provider BacklogProvider, repo contract.Repository, quarterPlan contract.Object, now time.Time) (contract.Plan, error) {
	if provider == nil {
		return contract.Plan{}, errors.New("quarter backlog preparation requires a provider")
	}
	normalized, err := planning.NormalizeQuarterPlan(quarterPlan)
	if err != nil {
		return contract.Plan{}, err
	}
	if _, legacy := normalized["items"]; legacy {
		return contract.Plan{}, errors.New("quarter backlog apply requires a structured quarter plan")
	}
	quarter, err := contract.Nonempty(normalized, "quarter")
	if err != nil {
		return contract.Plan{}, err
	}
	request := BacklogInventoryRequest{CommentMarkers: []string{quarterRationaleMarker(quarter)}}
	raw, err := provider.BacklogInventory(ctx, request)
	if err != nil {
		return contract.Plan{}, err
	}
	inventory, err := normalizeBacklogInventory(raw, repo, request)
	if err != nil {
		return contract.Plan{}, err
	}
	data := contract.Object{"quarter_plan": quarterPlan, "inventory": inventory, "inventory_request": request.Object()}
	preview, err := quarterDelta(data, inventory, repo)
	if err != nil {
		return contract.Plan{}, err
	}
	delta, _ := contract.ObjectAt(preview, "delta")
	summary, _ := contract.ObjectAt(delta, "summary")
	if summary["rationale_gap_count"] != int64(0) && summary["rationale_gap_count"] != 0 {
		return contract.Plan{}, errors.New("quarter backlog plan is missing rationale for touched issues")
	}
	if _, err = contract.ObjectAt(delta, "review_preparation"); err != nil {
		return contract.Plan{}, errors.New("quarter backlog requires complete issue, milestone and rationale-comment planning evidence")
	}
	sources := backlogPlanSources(inventory, request)
	ops, err := backlogOperations(contract.Plan{Command: QuarterBacklogCommand, Repository: repo, Data: data, Sources: sources})
	if err != nil {
		return contract.Plan{}, err
	}
	return contract.PreparePlan(QuarterBacklogCommand, repo, backlogPlanSources(inventory, request), data, ops, now)
}

func quarterRationaleMarker(quarter string) string {
	return "<!-- quarter-rationale quarter=" + quarter + " -->"
}

func quarterDelta(data, inventory contract.Object, repo contract.Repository) (contract.Object, error) {
	plan, err := contract.ObjectAt(data, "quarter_plan")
	if err != nil {
		return nil, err
	}
	normalized, err := planning.NormalizeQuarterPlan(plan)
	if err != nil {
		return nil, err
	}
	if _, legacy := normalized["items"]; legacy {
		return nil, errors.New("quarter apply requires a structured plan")
	}
	quarter, err := contract.Nonempty(normalized, "quarter")
	if err != nil {
		return nil, err
	}
	requestRaw, err := contract.ObjectAt(data, "inventory_request")
	if err != nil {
		return nil, err
	}
	request, err := parseInventoryRequest(requestRaw)
	if err != nil {
		return nil, err
	}
	if request.IncludeRelationships || len(request.Projects) != 0 || !same(request.CommentMarkers, []string{quarterRationaleMarker(quarter)}) {
		return nil, errors.New("quarter plan inventory request differs from its authored quarter")
	}
	graph, err := contract.ObjectAt(inventory, "issue_inventory")
	if err != nil {
		return nil, err
	}
	issues, err := contract.Objects(graph, "issues")
	if err != nil {
		return nil, err
	}
	open := []any{}
	for _, issue := range issues {
		if issue["state"] == "OPEN" {
			open = append(open, issue)
		}
	}
	openGraph := contract.Object{"repo": repo.Object(), "issues": open}
	milestones, err := contract.Objects(inventory, "milestones")
	if err != nil {
		return nil, err
	}
	milestoneRows := make([]any, 0, len(milestones))
	for _, m := range milestones {
		copy, _ := contract.Clone(m)
		copy["state"] = strings.ToLower(fmt.Sprint(copy["state"]))
		milestoneRows = append(milestoneRows, copy)
	}
	marker := quarterRationaleMarker(quarter)
	comments, err := contract.Objects(inventory, "comments")
	if err != nil {
		return nil, err
	}
	byIssue := contract.Object{}
	for _, comment := range comments {
		body, _ := comment["body"].(string)
		if !strings.Contains(body, marker) {
			continue
		}
		n, _ := contract.PositiveInteger(comment["issue_number"])
		key := strconv.FormatInt(n, 10)
		existing, ok := byIssue[key].(map[string]any)
		if !ok {
			existing = contract.Object{"body": body, "ids": []any{}}
			byIssue[key] = existing
		} else {
			return nil, errors.New("quarter rationale marker identifies multiple comments for an issue")
		}
		existing["ids"] = []any{comment["id"]}
	}
	for _, n := range allQuarterTouchedNumbers(normalized, open) {
		key := strconv.FormatInt(n, 10)
		if _, ok := byIssue[key]; !ok {
			byIssue[key] = contract.Object{"body": nil, "ids": []any{}}
		}
	}
	input := contract.Object{"plan": plan, "repo": repo.Object(), "issue_graph": openGraph, "milestones": milestoneRows, "rationale_comments_by_issue": byIssue, "source_evidence": contract.Object{"issues": contract.Object{"source": "github_api", "live": true, "complete": true}, "milestones": contract.Object{"source": "github_api", "live": true, "complete": true}, "rationale_comments": contract.Object{"source": "github_api", "live": true, "complete": true}}}
	return planning.BuildQuarterPlanDelta(input)
}

func allQuarterTouchedNumbers(plan contract.Object, open []any) []int64 {
	raw, _ := contract.Array(plan, "commit_issue_numbers")
	desired := map[int64]bool{}
	for _, v := range raw {
		n, e := contract.PositiveInteger(v)
		if e == nil {
			desired[n] = true
		}
	}
	current := map[int64]bool{}
	for _, v := range open {
		issue, ok := v.(map[string]any)
		if !ok {
			continue
		}
		n, e := contract.PositiveInteger(issue["number"])
		if e == nil && issue["milestone"] == plan["quarter"] {
			current[n] = true
		}
	}
	set := map[int64]bool{}
	for n := range desired {
		if !current[n] {
			set[n] = true
		}
	}
	for n := range current {
		if !desired[n] {
			set[n] = true
		}
	}
	out := make([]int64, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func quarterBacklogOperations(p contract.Plan, inventory contract.Object, request BacklogInventoryRequest) ([]contract.Operation, error) {
	data := p.Data
	plan, err := contract.ObjectAt(data, "quarter_plan")
	if err != nil {
		return nil, err
	}
	normalized, err := planning.NormalizeQuarterPlan(plan)
	if err != nil {
		return nil, err
	}
	quarter, err := contract.Nonempty(normalized, "quarter")
	if err != nil {
		return nil, err
	}
	if len(request.CommentMarkers) != 1 || request.CommentMarkers[0] != quarterRationaleMarker(quarter) {
		return nil, errors.New("quarter comment marker scope differs from authored plan")
	}
	preview, err := quarterDelta(data, inventory, p.Repository)
	if err != nil {
		return nil, err
	}
	delta, err := contract.ObjectAt(preview, "delta")
	if err != nil {
		return nil, err
	}
	summary, err := contract.ObjectAt(delta, "summary")
	if err != nil {
		return nil, err
	}
	gaps, err := contract.Integer(summary["rationale_gap_count"])
	if err != nil || gaps != 0 {
		return nil, errors.New("quarter plan has rationale gaps")
	}
	prep, err := contract.ObjectAt(delta, "review_preparation")
	if err != nil {
		return nil, errors.New("quarter plan lacks complete review preparation evidence")
	}
	reviewOps, err := contract.Objects(prep, "operations")
	if err != nil {
		return nil, err
	}
	marker := quarterRationaleMarker(quarter)
	issueRows := mustObjects(inventory["issue_inventory"].(map[string]any), "issues")
	issuesByNumber := map[int64]contract.Object{}
	for _, issue := range issueRows {
		n, _ := contract.PositiveInteger(issue["number"])
		issuesByNumber[n] = issue
	}
	milestones := mustObjects(inventory, "milestones")
	var milestone contract.Object
	for _, m := range milestones {
		if m["title"] == quarter {
			if milestone != nil {
				return nil, errors.New("quarter milestone title is ambiguous")
			}
			milestone = m
		}
	}
	ops := []contract.Operation{}
	for _, reviewOp := range reviewOps {
		target, _ := contract.ObjectAt(reviewOp, "target")
		kind, _ := target["kind"].(string)
		if kind == "repository-milestone" {
			before, _ := contract.ObjectAt(reviewOp, "before")
			after, _ := contract.ObjectAt(reviewOp, "after")
			title, _ := target["title"].(string)
			if title != quarter {
				return nil, errors.New("quarter milestone operation targets another title")
			}
			action, _ := target["action"].(string)
			if action == "closed-existing" {
				return nil, errors.New("closed quarter milestone requires a separate reviewed recovery")
			}
			oldDesc := any(nil)
			oldDue := any(nil)
			oldState := any(nil)
			exists := false
			var number any
			if milestone != nil {
				exists = true
				number = milestone["number"]
				oldDesc = milestone["description"]
				oldDue = milestone["due_on"]
				oldState = milestone["state"]
			}
			if before["exists"] != exists || before["state"] != oldState || before["description"] != oldDesc || before["due_on"] != oldDue {
				return nil, errors.New("quarter milestone before-state differs from complete inventory")
			}
			desc, err := contract.Nonempty(contract.Object{"description": after["description"]}, "description")
			if err != nil {
				return nil, err
			}
			due, err := contract.String(after, "due_on")
			if err != nil {
				return nil, err
			}
			if milestone != nil && milestone["state"] == "open" && milestone["description"] == desc && milestone["due_on"] == due {
				continue
			}
			var stableID, nodeID any
			if milestone != nil {
				stableID, nodeID = milestone["id"], milestone["node_id"]
			}
			ops = append(ops, contract.Operation{ID: "milestone:" + quarter, Kind: func() string {
				if milestone == nil {
					return "milestone-create"
				}
				return "milestone-update"
			}(), Target: contract.Object{"title": quarter, "number": number, "description": desc, "due_on": due}, Before: contract.Object{"exists": exists, "number": number, "id": stableID, "node_id": nodeID, "state": oldState, "description": oldDesc, "due_on": oldDue}, After: contract.Object{"exists": true, "number": number, "id": stableID, "node_id": nodeID, "state": "open", "description": desc, "due_on": due}})
			continue
		}
		if kind != "issue-quarter-assignment" {
			return nil, errors.New("quarter planning output contains an unsupported operation")
		}
		n, err := contract.PositiveInteger(target["issue_number"])
		if err != nil {
			return nil, err
		}
		issue := issuesByNumber[n]
		if issue == nil || issue["state"] != "OPEN" {
			return nil, fmt.Errorf("quarter target issue #%d is not open in complete inventory", n)
		}
		before, _ := contract.ObjectAt(reviewOp, "before")
		after, _ := contract.ObjectAt(reviewOp, "after")
		if before["issue_state"] != "OPEN" || issue["state"] != "OPEN" || before["milestone"] != issue["milestone"] {
			return nil, fmt.Errorf("quarter issue #%d before-state differs from complete inventory", n)
		}
		rationaleBody, err := contract.Nonempty(after, "rationale_comment_body")
		if err != nil {
			return nil, err
		}
		commentBefore, err := commentStateForMarker(inventory, n, marker)
		if err != nil {
			return nil, err
		}
		if commentBefore == nil || commentBefore["body"] != rationaleBody {
			var priorID any
			if commentBefore != nil {
				priorID = commentBefore["id"]
			}
			ops = append(ops, contract.Operation{ID: fmt.Sprintf("quarter-comment:%d", n), Kind: "issue-comment-upsert", Target: contract.Object{"issue_number": n, "issue_id": issue["id"], "marker": marker, "comment_id": priorID, "body": rationaleBody}, Before: contract.Object{"issue_number": n, "issue_id": issue["id"], "issue_state": "OPEN", "marker": marker, "comment_id": priorID, "body": func() any {
				if commentBefore != nil {
					return commentBefore["body"]
				}
				return nil
			}()}, After: contract.Object{"issue_number": n, "issue_id": issue["id"], "issue_state": "OPEN", "marker": marker, "body": rationaleBody}})
		}
		desiredMilestone := after["milestone"]
		if issue["milestone"] != desiredMilestone {
			ops = append(ops, contract.Operation{ID: fmt.Sprintf("quarter-assignment:%d", n), Kind: "issue-milestone-set", Target: contract.Object{"issue_number": n, "issue_id": issue["id"], "milestone_title": desiredMilestone}, Before: contract.Object{"issue_number": n, "issue_id": issue["id"], "issue_state": "OPEN", "milestone": issue["milestone"]}, After: contract.Object{"issue_number": n, "issue_id": issue["id"], "issue_state": "OPEN", "milestone": desiredMilestone}})
		}
	}
	return ops, nil
}
