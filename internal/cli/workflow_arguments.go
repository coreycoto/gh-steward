package cli

import (
	"errors"
	"flag"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/workflow"
)

func nativeWorkflowCommand(family string) string {
	return map[string]string{"relationships": workflow.RelationshipCommand, "backlog": workflow.RebalanceCommand, "backlog-mutations": workflow.BacklogMutationsCommand, "review": workflow.ReviewBacklogCommand, "quarter": workflow.QuarterBacklogCommand, "merge": workflow.MergeCommand, "execution": workflow.ExecutionSyncCommand, "artifacts": workflow.RunArtifactDeleteCommand, "governance": workflow.GovernanceCommand, "closeout": workflow.ReviewCloseoutCommand, "delivery": workflow.DeliveryCommand, "branches": workflow.BranchCleanupCommand}[family]
}

func validatePrepareArguments(family string, flags *flag.FlagSet, inputs map[string]contract.Object) error {
	allowedInputs := map[string]map[string]bool{
		"relationships":     {"payload": true, "hierarchy_policy": true},
		"backlog":           {"payload": true, "options": true, "policy": true, "backlog_audit": true},
		"backlog-mutations": {"payload": true, "projects": true},
		"review":            {"payload": true, "policy": true},
		"quarter":           {"payload": true},
		"merge":             {"event": true, "policy": true},
		"execution":         {"selector": true, "policy": true},
		"artifacts":         {"payload": true},
		"governance":        {"payload": true},
		"closeout":          {"summary": true, "policy": true},
		"delivery":          {"payload": true},
		"branches":          {"payload": true},
	}
	for key := range inputs {
		if !allowedInputs[family][key] {
			return errors.New("preparation has an unsupported named input")
		}
	}
	allowedFlags := map[string]bool{"repo-root": true, "repo": true, "out": true, "format": true, "input": true}
	if family == "backlog" || family == "review" || family == "merge" || family == "execution" || family == "closeout" {
		allowedFlags["policy"] = true
	}
	if family == "backlog" {
		for _, key := range []string{"project-owner", "project-owner-type", "project-number", "project-id"} {
			allowedFlags[key] = true
		}
	}
	var err error
	flags.Visit(func(f *flag.Flag) {
		if !allowedFlags[f.Name] {
			err = errors.New("preparation has a selector that is not used by this workflow")
		}
	})
	return err
}

func deliveryPolicyProjects(policy contract.Object) ([]workflow.ProjectScope, error) {
	state, err := contract.ObjectAt(policy, "execution_state")
	if err != nil {
		return nil, err
	}
	project, exists := state["project"]
	if !exists {
		return nil, errors.New("delivery requires an explicit nullable execution Project scope")
	}
	if project == nil {
		return []workflow.ProjectScope{}, nil
	}
	object, ok := project.(map[string]any)
	if !ok {
		return nil, errors.New("delivery Project scope must be an object")
	}
	parsed, err := parseBacklogProject(object)
	if err != nil {
		return nil, err
	}
	return []workflow.ProjectScope{parsed}, nil
}

func validateExecutionSnapshotArguments(flags *flag.FlagSet, inputs map[string]contract.Object) error {
	if len(inputs) != 2 || inputs["selector"] == nil || inputs["policy"] == nil {
		return errors.New("execution snapshot requires only explicit selector and policy inputs")
	}
	allowed := map[string]bool{"repo-root": true, "repo": true, "format": true, "out": true, "input": true, "policy": true}
	var err error
	flags.Visit(func(f *flag.Flag) {
		if !allowed[f.Name] {
			err = errors.New("execution snapshot selectors must come from its exact selector input")
		}
	})
	return err
}

func validateProjectDiscoveryArguments(flags *flag.FlagSet, inputs map[string]contract.Object) error {
	if len(inputs) != 0 {
		return errors.New("Project discovery accepts only explicit owner and owner-type selectors")
	}
	allowed := map[string]bool{"repo-root": true, "repo": true, "format": true, "out": true, "project-owner": true, "project-owner-type": true}
	var err error
	flags.Visit(func(f *flag.Flag) {
		if !allowed[f.Name] {
			err = errors.New("Project discovery has an unused or ambiguous selector")
		}
	})
	return err
}

func parseExecutionSelector(raw contract.Object) (workflow.ExecutionSelector, error) {
	var selector workflow.ExecutionSelector
	if len(raw) != 4 {
		return selector, errors.New("execution selector requires exactly issue_number, pull_request_number, skip_project_sync and project")
	}
	for _, choice := range []struct {
		key  string
		dest *int64
	}{{"issue_number", &selector.IssueNumber}, {"pull_request_number", &selector.PullRequestNumber}} {
		number, err := contract.Integer(raw[choice.key])
		if err != nil || number < 0 {
			return selector, errors.New("execution selector numbers must be nonnegative integers")
		}
		*choice.dest = number
	}
	var err error
	selector.SkipProjectSync, err = contract.Bool(raw, "skip_project_sync")
	if err != nil {
		return selector, err
	}
	project, exists := raw["project"]
	if !exists {
		return selector, errors.New("execution selector must include explicit nullable project")
	}
	if project != nil {
		object, ok := project.(map[string]any)
		if !ok {
			return selector, errors.New("execution Project selector must be an object")
		}
		parsed, err := parseBacklogProject(object)
		if err != nil {
			return selector, err
		}
		selector.Project = &parsed
	}
	if (selector.IssueNumber > 0) == (selector.PullRequestNumber > 0) || selector.SkipProjectSync != (selector.Project == nil) {
		return selector, errors.New("execution selector requires exactly one issue/PR and explicit Project sync choice")
	}
	return selector, nil
}

func parseExecutionPolicy(raw contract.Object) (workflow.ExecutionPolicy, error) {
	var policy workflow.ExecutionPolicy
	if len(raw) != 6 {
		return policy, errors.New("execution policy must provide the exact status and link marker fields")
	}
	statuses, err := contract.ObjectAt(raw, "statuses")
	if err != nil {
		return policy, err
	}
	if len(statuses) != 3 {
		return policy, errors.New("execution statuses require done, active and todo")
	}
	for _, choice := range []struct {
		key  string
		dest *string
	}{{"done", &policy.Statuses.Done}, {"active", &policy.Statuses.Active}, {"todo", &policy.Statuses.Todo}} {
		value, err := contract.Nonempty(statuses, choice.key)
		if err != nil {
			return policy, err
		}
		*choice.dest = value
	}
	for _, choice := range []struct {
		key  string
		dest *string
	}{{"status_field", &policy.StatusField}, {"pr_link_marker_prefix", &policy.PRLinkMarkerPrefix}, {"pr_link_number_pattern", &policy.PRLinkNumberPattern}, {"linked_issue_marker_prefix", &policy.LinkedIssueMarkerPrefix}, {"link_state_marker_prefix", &policy.LinkStateMarkerPrefix}} {
		value, err := contract.Nonempty(raw, choice.key)
		if err != nil {
			return policy, err
		}
		*choice.dest = value
	}
	return policy, nil
}

func parseBacklogProject(raw contract.Object) (workflow.ProjectScope, error) {
	var p workflow.ProjectScope
	if len(raw) != 6 {
		return p, errors.New("Project scope requires exactly host, owner, owner_type, number, id and title")
	}
	values := []struct {
		key  string
		dest *string
	}{{"host", &p.Host}, {"owner", &p.Owner}, {"owner_type", &p.OwnerType}, {"id", &p.ID}, {"title", &p.Title}}
	for _, value := range values {
		text, err := contract.Nonempty(raw, value.key)
		if err != nil {
			return p, err
		}
		*value.dest = text
	}
	number, err := contract.PositiveInteger(raw["number"])
	if err != nil {
		return p, err
	}
	p.Number = number
	return p, nil
}

func parseBacklogMutationProjects(raw contract.Object) ([]workflow.ProjectScope, error) {
	if len(raw) != 1 {
		return nil, errors.New("backlog mutation Projects require exactly a projects array")
	}
	objects, err := contract.Objects(raw, "projects")
	if err != nil {
		return nil, err
	}
	projects := make([]workflow.ProjectScope, 0, len(objects))
	for _, object := range objects {
		project, err := parseBacklogProject(object)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, nil
}

func parseReviewBacklogPolicy(raw contract.Object) (workflow.ReviewBacklogPolicy, error) {
	var policy workflow.ReviewBacklogPolicy
	project, err := contract.ObjectAt(raw, "project")
	if err != nil {
		return policy, err
	}
	policy.Project, err = parseBacklogProject(project)
	if err != nil {
		return policy, err
	}
	for _, choice := range []struct {
		key  string
		dest *map[string]string
	}{{"severity_to_priority", &policy.SeverityToPriority}, {"issue_type_labels", &policy.IssueTypeLabels}} {
		values, err := contract.ObjectAt(raw, choice.key)
		if err != nil {
			return policy, err
		}
		mapping := map[string]string{}
		for key := range values {
			value, err := contract.Nonempty(values, key)
			if err != nil {
				return policy, err
			}
			mapping[key] = value
		}
		*choice.dest = mapping
	}
	for key := range raw {
		if key != "project" && key != "severity_to_priority" && key != "issue_type_labels" {
			return policy, errors.New("review backlog policy has an unsupported field")
		}
	}
	return policy, nil
}

func backlogPlanProjects(plan contract.Plan) ([]workflow.ProjectScope, error) {
	request, err := contract.ObjectAt(plan.Data, "inventory_request")
	if err != nil {
		return nil, err
	}
	rows, err := contract.Objects(request, "projects")
	if err != nil {
		return nil, err
	}
	projects := make([]workflow.ProjectScope, 0, len(rows))
	for _, row := range rows {
		project, err := parseBacklogProject(row)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, nil
}

// Apply gets its domain scope and inputs from the reviewed plan. Extra inputs
// or selector flags cannot silently suggest a different target to the caller.
func validateApplyArguments(flags *flag.FlagSet, inputs map[string]contract.Object) error {
	if len(inputs) != 1 || inputs["plan"] == nil {
		return errors.New("apply accepts only the reviewed plan input")
	}
	allowed := map[string]bool{"repo-root": true, "repo": true, "out": true, "format": true, "approve-plan-sha": true, "input": true}
	var err error
	flags.Visit(func(f *flag.Flag) {
		if !allowed[f.Name] {
			err = errors.New("apply selectors and policy must come from the exact reviewed plan")
		}
	})
	return err
}
