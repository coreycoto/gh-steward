// Package cli is the versioned machine boundary used by native gh extensions,
// CI and consumer adapters. Domain modules do not import it.
package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/governance"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/planning"
	"github.com/coreycoto/gh-steward/internal/runrecovery"
	"github.com/coreycoto/gh-steward/internal/snapshot"
	"github.com/coreycoto/gh-steward/internal/workflow"
)

var Version = "0.2.0-dev"
var SourceRevision = "unknown"
var SourceDirty = "unknown"

type inputsFlag []string

func (i *inputsFlag) String() string         { return strings.Join(*i, ",") }
func (i *inputsFlag) Set(value string) error { *i = append(*i, value); return nil }

type Runner struct {
	Out, Err io.Writer
	Input    io.Reader
	Actions  runrecovery.ActionsReader
}

func (r Runner) Run(ctx context.Context, args []string) error {
	if r.Out == nil || r.Err == nil {
		return errors.New("CLI output streams are required")
	}
	if len(args) == 0 {
		return errors.New("choose version, snapshot, relationships, backlog, backlog-mutations, quarter, review, execution, governance, closeout, delivery, branches or merge; use help for input contracts")
	}
	if args[0] == "help" || args[0] == "--help" {
		_, err := fmt.Fprint(r.Out, help)
		return err
	}
	if args[0] == "version" || args[0] == "--version" || args[0] == "--source-revision" {
		if args[0] == "--source-revision" {
			_, err := fmt.Fprintln(r.Out, SourceRevision)
			return err
		}
		if len(args) > 2 || (len(args) == 2 && args[1] != "--json") {
			return errors.New("version accepts only --json")
		}
		var dirty any
		switch SourceDirty {
		case "true":
			dirty = true
		case "false":
			dirty = false
		case "unknown":
			dirty = nil
		default:
			return errors.New("binary has invalid source provenance")
		}
		return r.write(contract.Object{"schema_version": 2, "tool": "gh-steward", "tool_version": Version, "source_revision": SourceRevision, "source_dirty": dirty, "target": runtime.GOOS + "/" + runtime.GOARCH, "go_version": runtime.Version()})
	}
	if len(args) < 2 {
		return errors.New("command family requires an action")
	}
	if args[0] == "runs" {
		return r.runRecovery(ctx, args[1:])
	}
	command, err := commandID(args[0], args[1])
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("gh steward "+args[0]+" "+args[1], flag.ContinueOnError)
	flags.SetOutput(r.Err)
	root := flags.String("repo-root", ".", "explicit checkout root")
	repoURL := flags.String("repo", "", "repository HTTPS URL; otherwise resolve checkout origin")
	state := flags.String("state", "open", "issue snapshot state: open, closed or all")
	projectOwner := flags.String("project-owner", "", "explicit Project user/organization owner")
	projectType := flags.String("project-owner-type", "", "User or Organization")
	projectNumber := flags.Int64("project-number", 0, "explicit Project number")
	projectID := flags.String("project-id", "", "expected Project node ID when known")
	joinProject := flags.Bool("join-project", false, "include explicit live Project in an issue graph")
	policyPath := flags.String("policy", "", "consumer policy JSON file")
	outPath := flags.String("out", "", "optional result file; stdout still emits the complete result")
	format := flags.String("format", "json", "machine format; currently json")
	approvedPlan := flags.String("approve-plan-sha", "", "exact reviewed plan SHA-256; required only for an explicitly authorized apply")
	var declared inputsFlag
	flags.Var(&declared, "input", "named bounded JSON object: name=path; repeatable, path=- reads stdin once")
	if err = flags.Parse(args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *format != "json" {
		return errors.New("machine output format must be json")
	}
	checkout, repository, err := resolveCheckout(ctx, *root, *repoURL)
	if err != nil {
		return err
	}
	inputs, err := r.loadInputs(checkout, declared)
	if err != nil {
		return err
	}
	if *policyPath != "" {
		if _, exists := inputs["policy"]; exists {
			return errors.New("policy supplied twice")
		}
		p, err := loadObject(checkout, *policyPath)
		if err != nil {
			return err
		}
		inputs["policy"] = p
	}
	if err = validateInputTargets(repository, inputs); err != nil {
		return err
	}
	if *approvedPlan != "" && args[1] != "apply" {
		return errors.New("plan approval is valid only for an apply action")
	}
	var data contract.Object
	if workflowCommand := nativeWorkflowCommand(args[0]); workflowCommand != "" && (args[1] == "prepare" || args[1] == "apply") {
		var rawPlan contract.Object
		var reviewedPlan contract.Plan
		if args[1] == "prepare" {
			if err := validatePrepareArguments(args[0], flags, inputs); err != nil {
				return err
			}
		}
		if args[1] == "apply" {
			if err := validateApplyArguments(flags, inputs); err != nil {
				return err
			}
			rawPlan = inputs["plan"]
			plan, err := contract.ParsePlan(rawPlan)
			if err != nil {
				return err
			}
			if *approvedPlan == "" || *approvedPlan != plan.SHA256 {
				return errors.New("apply requires the exact separately authorized --approve-plan-sha value")
			}
			if plan.Command != workflowCommand || plan.Repository != repository {
				return errors.New("reviewed apply targets another command or repository")
			}
			reviewedPlan = plan
		}
		tpt, err := native.New(checkout, repository)
		if err != nil {
			return err
		}
		if args[0] == "relationships" && args[1] == "prepare" {
			payload, exists := inputs["payload"]
			if !exists {
				return errors.New("payload input is required")
			}
			plan, err := workflow.PrepareRelationships(ctx, workflow.NativeRelationships{Transport: tpt}, repository, payload, inputs["hierarchy_policy"], time.Now())
			if err != nil {
				return err
			}
			data = plan.Object()
		} else if args[0] == "backlog" && args[1] == "prepare" {
			policy, err := snapshot.ParseQueuePolicy(inputs["policy"])
			if err != nil {
				return err
			}
			project := native.ProjectScope{Host: repository.Host, Owner: *projectOwner, OwnerType: *projectType, Number: *projectNumber, ID: *projectID}
			plan, err := workflow.PrepareRebalance(ctx, workflow.NativeRebalanceProvider{Transport: tpt}, repository, project, policy, inputs["payload"], inputs["options"], inputs["backlog_audit"], time.Now())
			if err != nil {
				return err
			}
			data = plan.Object()
		} else if args[0] == "review" && args[1] == "prepare" {
			policy, err := parseReviewBacklogPolicy(inputs["policy"])
			if err != nil {
				return err
			}
			if inputs["payload"] == nil {
				return errors.New("review preparation requires authored findings as payload")
			}
			plan, err := workflow.PrepareReviewBacklog(ctx, workflow.NativeBacklog{Transport: tpt, Projects: []workflow.ProjectScope{policy.Project}}, repository, inputs["payload"], policy, time.Now())
			if err != nil {
				return err
			}
			data = plan.Object()
		} else if args[0] == "backlog-mutations" && args[1] == "prepare" {
			if inputs["payload"] == nil {
				return errors.New("backlog mutation preparation requires the authored v1 payload")
			}
			projects, err := parseBacklogMutationProjects(inputs["projects"])
			if err != nil {
				return err
			}
			plan, err := workflow.PrepareBacklogMutations(ctx, workflow.NativeBacklog{Transport: tpt, Projects: projects}, repository, inputs["payload"], projects, time.Now())
			if err != nil {
				return err
			}
			data = plan.Object()
		} else if args[0] == "quarter" && args[1] == "prepare" {
			if inputs["payload"] == nil {
				return errors.New("quarter preparation requires an authored structured plan as payload")
			}
			plan, err := workflow.PrepareQuarterBacklog(ctx, workflow.NativeBacklog{Transport: tpt}, repository, inputs["payload"], time.Now())
			if err != nil {
				return err
			}
			data = plan.Object()
		} else if args[0] == "closeout" && args[1] == "prepare" {
			policy, err := workflow.ParseCloseoutPolicy(inputs["policy"], repository)
			if err != nil {
				return err
			}
			if inputs["summary"] == nil {
				return errors.New("closeout preparation requires the reviewed audit summary")
			}
			reviewRaw, err := contract.ObjectAt(policy.Object(), "review_backlog")
			if err != nil {
				return err
			}
			reviewPolicy, err := parseReviewBacklogPolicy(reviewRaw)
			if err != nil {
				return err
			}
			plan, err := workflow.PrepareReviewCloseout(ctx, workflow.NativeBacklog{Transport: tpt, Projects: []workflow.ProjectScope{reviewPolicy.Project}}, repository, inputs["summary"], policy, time.Now())
			if err != nil {
				return err
			}
			data = plan.Object()
		} else if args[0] == "merge" && args[1] == "prepare" {
			plan, err := workflow.PrepareMerge(ctx, workflow.NativeMerge{Transport: tpt}, repository, inputs["event"], inputs["policy"], time.Now())
			if err != nil {
				return err
			}
			data = plan.Object()
		} else if args[0] == "execution" && args[1] == "prepare" {
			selector, err := parseExecutionSelector(inputs["selector"])
			if err != nil {
				return err
			}
			policy, err := parseExecutionPolicy(inputs["policy"])
			if err != nil {
				return err
			}
			projects := []workflow.ProjectScope{}
			if selector.Project != nil {
				projects = append(projects, *selector.Project)
			}
			plan, err := workflow.PrepareExecutionSync(ctx, workflow.NativeExecution{NativeBacklog: workflow.NativeBacklog{Transport: tpt, Projects: projects}}, repository, selector, policy, time.Now())
			if err != nil {
				return err
			}
			data = plan.Object()
		} else if args[0] == "governance" && args[1] == "prepare" {
			payload := inputs["payload"]
			if len(payload) != 2 {
				return errors.New("governance preparation requires exactly kind and policy")
			}
			kind, err := contract.Nonempty(payload, "kind")
			if err != nil {
				return err
			}
			policy, err := contract.ObjectAt(payload, "policy")
			if err != nil {
				return err
			}
			projects := []workflow.ProjectScope{}
			if project, exists := policy["project"]; exists && project != nil {
				object, ok := project.(map[string]any)
				if !ok {
					return errors.New("governance Project selector must be an explicit scope object")
				}
				parsed, err := parseBacklogProject(object)
				if err != nil {
					return err
				}
				projects = append(projects, parsed)
			}
			plan, err := workflow.PrepareGovernance(ctx, workflow.NativeGovernance{NativeBacklog: workflow.NativeBacklog{Transport: tpt, Projects: projects}}, repository, kind, policy, time.Now())
			if err != nil {
				return err
			}
			data = plan.Object()
		} else if args[0] == "branches" && args[1] == "prepare" {
			selection, err := workflow.ParseBranchCleanupSelection(inputs["payload"], repository)
			if err != nil {
				return err
			}
			plan, err := workflow.PrepareBranchCleanup(ctx, workflow.NativeBranchCleanup{Transport: tpt}, repository, selection, time.Now())
			if err != nil {
				return err
			}
			data = plan.Object()
		} else if args[0] == "delivery" && args[1] == "prepare" {
			payload := inputs["payload"]
			if len(payload) != 2 {
				return errors.New("delivery preparation requires exactly kind and policy")
			}
			kind, err := contract.Nonempty(payload, "kind")
			if err != nil {
				return err
			}
			rawPolicy, err := contract.ObjectAt(payload, "policy")
			if err != nil {
				return err
			}
			policy, err := workflow.ParseDeliveryPolicy(kind, rawPolicy, repository)
			if err != nil {
				return err
			}
			projects, err := deliveryPolicyProjects(policy)
			if err != nil {
				return err
			}
			provider := workflow.NativeDelivery{NativeGovernance: workflow.NativeGovernance{NativeBacklog: workflow.NativeBacklog{Transport: tpt, Projects: projects}}}
			plan, err := workflow.PrepareDelivery(ctx, provider, repository, kind, policy, time.Now())
			if err != nil {
				return err
			}
			data = plan.Object()
		} else if args[0] == "artifacts" && args[1] == "prepare" {
			payload := inputs["payload"]
			if len(payload) != 3 {
				return errors.New("artifact preparation requires run_id, names and ignore_missing")
			}
			runID, err := contract.PositiveInteger(payload["run_id"])
			if err != nil {
				return err
			}
			names, err := contract.Strings(payload["names"])
			if err != nil {
				return err
			}
			ignore, err := contract.Bool(payload, "ignore_missing")
			if err != nil {
				return err
			}
			plan, err := workflow.PrepareRunArtifactDeletion(ctx, workflow.NativeRunArtifactProvider{Transport: tpt}, repository, runID, workflow.RunArtifactSelection{Names: names, IgnoreMissing: ignore}, time.Now())
			if err != nil {
				return err
			}
			data = plan.Object()
		} else {
			var adapter apply.Adapter = workflow.Relationships{Provider: workflow.NativeRelationships{Transport: tpt}}
			if args[0] == "backlog" {
				adapter = workflow.RebalanceAdapter{Provider: workflow.NativeRebalanceProvider{Transport: tpt}}
			}
			if args[0] == "review" || args[0] == "quarter" {
				projects, err := backlogPlanProjects(reviewedPlan)
				if err != nil {
					return err
				}
				adapter = workflow.Backlog{Provider: workflow.NativeBacklog{Transport: tpt, Projects: projects}}
			}
			if args[0] == "backlog-mutations" {
				projects, err := backlogPlanProjects(reviewedPlan)
				if err != nil {
					return err
				}
				adapter = workflow.BacklogMutations{Provider: workflow.NativeBacklog{Transport: tpt, Projects: projects}}
			}
			if args[0] == "merge" {
				adapter = workflow.MergeAdapter{Provider: workflow.NativeMerge{Transport: tpt}}
			}
			if args[0] == "execution" {
				request, err := workflow.ExecutionRequestFromPlan(reviewedPlan)
				if err != nil {
					return err
				}
				projects := []workflow.ProjectScope{}
				if request.Selector.Project != nil {
					projects = append(projects, *request.Selector.Project)
				}
				adapter = workflow.ExecutionSync{Provider: workflow.NativeExecution{NativeBacklog: workflow.NativeBacklog{Transport: tpt, Projects: projects}}}
			}
			if args[0] == "artifacts" {
				adapter = workflow.RunArtifactDeletionAdapter{Provider: workflow.NativeRunArtifactProvider{Transport: tpt}}
			}
			if args[0] == "governance" {
				projects, err := backlogPlanProjects(reviewedPlan)
				if err != nil {
					return err
				}
				adapter = workflow.Governance{Provider: workflow.NativeGovernance{NativeBacklog: workflow.NativeBacklog{Transport: tpt, Projects: projects}}}
			}
			if args[0] == "closeout" {
				projects, err := backlogPlanProjects(reviewedPlan)
				if err != nil {
					return err
				}
				adapter = workflow.Closeout{Provider: workflow.NativeBacklog{Transport: tpt, Projects: projects}}
			}
			if args[0] == "delivery" {
				request, err := workflow.DeliveryRequestFromPlan(reviewedPlan)
				if err != nil {
					return err
				}
				projects, err := deliveryPolicyProjects(request.Policy)
				if err != nil {
					return err
				}
				adapter = workflow.Delivery{Provider: workflow.NativeDelivery{NativeGovernance: workflow.NativeGovernance{NativeBacklog: workflow.NativeBacklog{Transport: tpt, Projects: projects}}}}
			}
			if args[0] == "branches" {
				adapter = workflow.BranchCleanup{Provider: workflow.NativeBranchCleanup{Transport: tpt}}
			}
			data, err = (apply.Engine{Root: checkout, Repository: repository, Command: workflowCommand, Adapter: adapter}).Apply(ctx, rawPlan)
			if err != nil {
				return err
			}
			if args[0] == "artifacts" {
				missing, err := workflow.MissingRunArtifactNames(reviewedPlan)
				if err != nil {
					return err
				}
				selection, err := contract.ObjectAt(reviewedPlan.Data, "selection")
				if err != nil {
					return err
				}
				ignore, err := contract.Bool(selection, "ignore_missing")
				if err != nil {
					return err
				}
				data["missing_names"] = missing
				if len(missing) > 0 && !ignore {
					data["status"] = "completed_with_missing_names"
				}
			}
		}
	} else if args[0] == "snapshot" {
		tpt, err := native.New(checkout, repository)
		if err != nil {
			return err
		}
		s := snapshot.Service{Reader: tpt, Repository: repository}
		project := native.ProjectScope{Host: repository.Host, Owner: *projectOwner, OwnerType: *projectType, Number: *projectNumber, ID: *projectID}
		switch args[1] {
		case "execution":
			if err := validateExecutionSnapshotArguments(flags, inputs); err != nil {
				return err
			}
			selector, err := parseExecutionSelector(inputs["selector"])
			if err != nil {
				return err
			}
			policy, err := parseExecutionPolicy(inputs["policy"])
			if err != nil {
				return err
			}
			projects := []workflow.ProjectScope{}
			if selector.Project != nil {
				projects = append(projects, *selector.Project)
			}
			data, err = workflow.SnapshotExecution(ctx, workflow.NativeExecution{NativeBacklog: workflow.NativeBacklog{Transport: tpt, Projects: projects}}, repository, selector, policy)
			if err != nil {
				return err
			}
		case "repo":
			data, err = s.CurrentRepository(ctx)
		case "project":
			data, err = s.Project(ctx, project)
		case "projects":
			if err = validateProjectDiscoveryArguments(flags, inputs); err != nil {
				return err
			}
			data, err = s.OwnerProjects(ctx, native.ProjectOwnerScope{Host: repository.Host, Owner: *projectOwner, OwnerType: *projectType})
		case "merge":
			data, err = snapshot.MergeInventory(ctx, tpt, repository, inputs["event"])
		case "artifacts":
			payload := inputs["payload"]
			if len(payload) != 1 {
				return errors.New("artifact snapshot accepts only payload.run_id")
			}
			runID, err := contract.PositiveInteger(payload["run_id"])
			if err != nil {
				return err
			}
			data, err = snapshot.RunArtifactInventory(ctx, tpt, repository, runID)
			if err != nil {
				return err
			}
		case "linked-prs":
			payload, exists := inputs["payload"]
			if !exists {
				return errors.New("linked-PR snapshot requires payload.issue_numbers")
			}
			values, err := contract.Array(payload, "issue_numbers")
			if err != nil {
				return err
			}
			numbers := []int64{}
			for _, value := range values {
				number, err := contract.PositiveInteger(value)
				if err != nil {
					return err
				}
				numbers = append(numbers, number)
			}
			data, err = s.LinkedPullRequests(ctx, numbers, inputs["policy"])
			if err != nil {
				return err
			}
		case "backlog":
			options := inputs["options"]
			markers := []string{}
			includeRelationships := false
			caseInsensitive := false
			if options != nil {
				for key := range options {
					if key != "comment_markers" && key != "include_relationships" && key != "comment_case_insensitive" {
						return errors.New("unsupported backlog inventory option")
					}
				}
				if raw, exists := options["comment_markers"]; exists {
					markers, err = contract.Strings(raw)
					if err != nil {
						return err
					}
				}
				if _, exists := options["include_relationships"]; exists {
					includeRelationships, err = contract.Bool(options, "include_relationships")
					if err != nil {
						return err
					}
				}
				if _, exists := options["comment_case_insensitive"]; exists {
					caseInsensitive, err = contract.Bool(options, "comment_case_insensitive")
					if err != nil {
						return err
					}
				}
			}
			projects := []native.ProjectScope{}
			if *joinProject {
				projects = append(projects, project)
			}
			if caseInsensitive {
				data, err = s.BacklogInventoryCaseInsensitiveComments(ctx, projects, markers, includeRelationships)
			} else {
				data, err = s.BacklogInventory(ctx, projects, markers, includeRelationships)
			}
		case "issues", "queue":
			var p contract.Object
			if *joinProject || args[1] == "queue" {
				p, err = s.Project(ctx, project)
				if err != nil {
					return err
				}
			}
			data, err = s.IssueGraph(ctx, *state, p)
			if err == nil && args[1] == "queue" {
				policy, err := snapshot.ParseQueuePolicy(inputs["policy"])
				if err != nil {
					return err
				}
				data, err = s.Queue(data, policy)
				if err != nil {
					return err
				}
			}
		}
		if err != nil {
			return err
		}
	} else if args[0] == "governance" || args[0] == "execution" || args[0] == "merge" || (args[0] == "backlog" && args[1] == "audit") {
		data, err = governance.Run(command, inputs)
		if err != nil {
			return err
		}
	} else {
		data, err = planning.Run(command, inputs)
		if err != nil {
			return err
		}
	}
	result := contract.Object{"schema_version": 2, "tool_version": Version, "command": command, "repository": repository.Object(), "data": data}
	if *outPath != "" {
		if err = writeFile(checkout, *outPath, result); err != nil {
			return err
		}
	}
	if err := r.write(result); err != nil {
		return err
	}
	if data["status"] == "completed_with_missing_names" {
		return errors.New("selected artifacts were deleted; required requested names were missing in the reviewed inventory")
	}
	return nil
}

func commandID(family, action string) (string, error) {
	commands := map[string]map[string]string{
		"snapshot":          {"repo": "repository-snapshot", "project": "project-snapshot", "projects": "owner-project-inventory", "issues": "issue-graph", "queue": "queue-snapshot", "backlog": "backlog-inventory", "merge": "merge-inventory", "linked-prs": "linked-pr-inventory", "artifacts": "run-artifact-inventory", "execution": "execution-inventory"},
		"relationships":     {"audit": "relationship-audit", "validate": "relationship-validate", "plan": "relationship-delta", "prepare": "relationship-prepare", "apply": "relationship-apply"},
		"backlog":           {"audit": "backlog-audit", "next": "next-item", "rank": "rebalance-rank", "validate": "rebalance-validate", "plan": "rebalance-plan", "prepare": "rebalance-prepare", "apply": "rebalance-apply"},
		"backlog-mutations": {"prepare": "backlog-mutations-prepare", "apply": workflow.BacklogMutationsCommand},
		"quarter":           {"validate": "quarter-plan-validate", "backlog-plan": "quarter-plan-backlog-delta", "plan": "quarter-plan-delta", "prepare": "quarter-backlog-prepare", "apply": workflow.QuarterBacklogCommand},
		"review":            {"validate": "review-findings-validate", "plan": "review-backlog-delta", "legacy-plan": "review-backlog-legacy-delta", "closeout-validate": "review-closeout-findings-validate", "closeout-audit": "review-closeout-audit", "prepare": "review-backlog-prepare", "apply": workflow.ReviewBacklogCommand},
		"governance":        {"labels-plan": "label-plan", "check": "governance-check", "summary": "governance-summary", "milestones-check": "milestone-check", "prepare": "governance-prepare", "apply": workflow.GovernanceCommand},
		"execution":         {"preflight": "execution-preflight", "transition": "execution-transition", "links": "execution-link-facts", "recover": "execution-recover", "prepare": "execution-sync-prepare", "apply": workflow.ExecutionSyncCommand},
		"merge":             {"eligibility": "merge-eligibility", "prepare": "merge-prepare", "apply": workflow.MergeCommand},
		"artifacts":         {"prepare": "run-artifact-delete-prepare", "apply": workflow.RunArtifactDeleteCommand},
		"closeout":          {"prepare": "review-closeout-prepare", "apply": workflow.ReviewCloseoutCommand},
		"delivery":          {"prepare": "delivery-prepare", "apply": workflow.DeliveryCommand},
		"branches":          {"prepare": "branch-cleanup-prepare", "apply": workflow.BranchCleanupCommand},
	}
	if command, exists := commands[family][action]; exists {
		return command, nil
	}
	return "", fmt.Errorf("unsupported command %s %s", family, action)
}

func resolveCheckout(ctx context.Context, root, explicit string) (string, contract.Repository, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", contract.Repository{}, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", contract.Repository{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	command.Dir = abs
	output, err := command.Output()
	if err != nil {
		return "", contract.Repository{}, errors.New("repo-root must resolve a Git checkout")
	}
	checkout := strings.TrimSpace(string(output))
	checkout, err = filepath.EvalSymlinks(checkout)
	if err != nil {
		return "", contract.Repository{}, err
	}
	if explicit == "" {
		command = exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
		command.Dir = checkout
		output, err = command.Output()
		if err != nil {
			return "", contract.Repository{}, errors.New("checkout origin is unavailable; provide --repo HTTPS URL")
		}
		explicit = strings.TrimSpace(string(output))
		if strings.HasPrefix(explicit, "git@") {
			split := strings.SplitN(strings.TrimPrefix(explicit, "git@"), ":", 2)
			if len(split) == 2 {
				explicit = "https://" + split[0] + "/" + split[1]
			}
		}
		explicit = strings.TrimSuffix(explicit, ".git")
	}
	// The URL owns the host/owner/name. Construct the corresponding identity,
	// then let the shared strict parser reject credentials and escaping paths.
	parts := strings.Split(strings.TrimSuffix(explicit, "/"), "/")
	if len(parts) != 5 {
		return "", contract.Repository{}, errors.New("repository target must be an exact HTTPS host/owner/name URL")
	}
	repository, err := contract.ParseRepository(contract.Object{"nameWithOwner": parts[3] + "/" + parts[4], "url": explicit})
	if err != nil {
		return "", contract.Repository{}, err
	}
	if err = native.CheckEnvironment(repository, os.Environ()); err != nil {
		return "", contract.Repository{}, err
	}
	return checkout, repository, nil
}

func loadObject(root, path string) (contract.Object, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("input must be a bounded regular JSON file")
	}
	return contract.Decode(f)
}
func (r Runner) loadInputs(root string, declared inputsFlag) (map[string]contract.Object, error) {
	out := map[string]contract.Object{}
	stdinUsed := false
	for _, entry := range declared {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, errors.New("input must use name=path")
		}
		if _, exists := out[parts[0]]; exists {
			return nil, errors.New("named input was supplied twice")
		}
		var value contract.Object
		var err error
		if parts[1] == "-" {
			if stdinUsed || r.Input == nil {
				return nil, errors.New("stdin can supply only one named object")
			}
			stdinUsed = true
			value, err = contract.Decode(r.Input)
		} else {
			value, err = loadObject(root, parts[1])
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", parts[0], err)
		}
		// A previous public result is accepted only in its explicit v2 shape.
		if raw, exists := value["tool_version"]; exists && raw != nil {
			v, err := contract.Integer(value["schema_version"])
			if err != nil || v != 2 {
				return nil, errors.New("unsupported machine result envelope")
			}
			data, err := contract.ObjectAt(value, "data")
			if err != nil {
				return nil, err
			}
			data, err = contract.Clone(data)
			if err != nil {
				return nil, err
			}
			if repo, exists := value["repository"]; exists {
				data["_machine_repository"] = repo
			}
			value = data
		}
		out[parts[0]] = value
	}
	return out, nil
}
func validateInputTargets(repository contract.Repository, inputs map[string]contract.Object) error {
	for _, input := range inputs {
		for _, key := range []string{"repo", "repository", "_machine_repository"} {
			if raw, exists := input[key]; exists {
				obj, ok := raw.(map[string]any)
				if !ok {
					return errors.New("input repository identity must be object")
				}
				if _, rest := obj["full_name"]; rest {
					obj = contract.Object{"nameWithOwner": obj["full_name"], "url": obj["html_url"]}
				}
				r, err := contract.ParseRepository(obj)
				if err != nil || r != repository {
					return errors.New("named input belongs to another repository or host")
				}
			}
		}
		delete(input, "_machine_repository")
	}
	return nil
}
func (r Runner) write(v contract.Object) error {
	data, err := contract.Canonical(v)
	if err != nil {
		return err
	}
	if len(data) > contract.MaxJSONBytes {
		return errors.New("machine result exceeds supported size")
	}
	data = append(data, '\n')
	_, err = r.Out.Write(data)
	return err
}
func writeFile(root, path string, value contract.Object) error {
	data, err := contract.Canonical(value)
	if err != nil {
		return err
	}
	if len(data) > contract.MaxJSONBytes {
		return errors.New("machine result exceeds supported size")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("output must not replace a symlink or special file")
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".steward-output-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = io.Copy(f, bytes.NewReader(append(data, '\n'))); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

const help = `gh steward version --json
gh steward snapshot repo|project|issues|queue|backlog [--repo-root PATH] [--repo HTTPS_URL]
gh steward snapshot execution --input selector=FILE --policy FILE
gh steward snapshot projects --project-owner LOGIN --project-owner-type User|Organization
  Project discovery returns complete accessible Projects for that explicit owner.
  Project reads require --project-owner LOGIN --project-owner-type User|Organization
  --project-number N [--project-id NODE_ID]. Issues omit Projects unless --join-project.
  Queue requires --policy FILE with explicit field/semantic option mappings.
gh steward snapshot linked-prs --input payload=FILE --policy FILE
gh steward snapshot merge --input event=FILE
gh steward snapshot artifacts --input payload=FILE
gh steward relationships audit|validate|plan --input issue_graph=FILE [--input payload=FILE]
gh steward relationships prepare --input payload=FILE [--input hierarchy_policy=FILE] --out reviewed-plan.json
gh steward relationships apply --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward backlog next|rank|validate|plan --input NAME=FILE ...
gh steward backlog audit --policy FILE --input issue_graph=FILE [--input queue_snapshot=FILE]
gh steward backlog prepare --policy FILE --input payload=FILE [--input options=FILE] [--input backlog_audit=FILE]
  Backlog preparation requires the explicit Project selectors above.
gh steward quarter validate|backlog-plan|plan --input NAME=FILE ...
gh steward review validate|plan|legacy-plan|closeout-validate|closeout-audit --input NAME=FILE ...
gh steward governance labels-plan|check|summary|milestones-check --policy FILE --input snapshot=FILE
gh steward execution preflight|transition|links|recover --policy FILE --input snapshot=FILE
gh steward execution prepare --input selector=FILE --policy FILE
gh steward artifacts prepare --input payload=FILE
gh steward governance prepare --input payload=FILE
gh steward closeout prepare --input summary=FILE --policy FILE
gh steward delivery prepare --input payload=FILE
gh steward branches prepare --input payload=FILE
  Artifact payload: run_id, exact names, and explicit ignore_missing.
gh steward merge eligibility --policy FILE --input snapshot=FILE
gh steward review prepare --policy FILE --input payload=FILE
gh steward backlog-mutations prepare --input payload=FILE --input projects=FILE
  Payload is authored v1 issue intent; projects is {projects:[exact six-field scopes]}.
gh steward quarter prepare --input payload=FILE
gh steward merge prepare --policy FILE --input event=FILE
gh steward backlog|backlog-mutations|review|quarter|merge|execution|artifacts|governance|closeout|delivery|branches apply --input plan=FILE --approve-plan-sha EXACT_SHA256
gh steward runs digest --input document=FILE
gh steward runs recover --workflow FILE --run-id ID --attempt N --run-name TITLE --recovery-key KEY --package-root PATH
gh steward runs qualify-prepared --workflow FILE --run-id ID --attempt N --run-name TITLE --recovery-key KEY --package-root PATH --workflow-sha CONTROL_SHA
gh steward runs acquire-handoff --workflow FILE --run-id ID --attempt N --run-name TITLE --recovery-key KEY --package-root PATH --artifact-id ID --artifact-digest sha256:DIGEST [--purpose apply|transport]
gh steward runs verify-publication --workflow FILE --run-id ID --attempt N --run-name TITLE --recovery-key KEY --package-root PATH --workflow-sha CONTROL_SHA
gh steward runs acquire-publication-candidate --workflow FILE --run-id ID --attempt N --run-name TITLE --recovery-key KEY --package-root PATH --workflow-sha CONTROL_SHA --artifact-id ID --artifact-digest sha256:DIGEST
gh steward runs finish-noop --workflow FILE --run-id ID --attempt N --run-name TITLE --recovery-key KEY --package-root PATH --workflow-sha CONTROL_SHA
gh steward runs finalize --workflow FILE --run-id ID --attempt N --package-root PATH --artifact-id ID --artifact-digest sha256:DIGEST --checkpoint PATH [--workflow-sha CONTROL_SHA]
gh steward runs context-start|context-observe|context-observe-source --package-root PATH --input context=FILE
gh steward runs context-record-plan --package-root PATH --input plan=FILE
gh steward runs context-phase --package-root PATH --phase PHASE [--reason REASON]
gh steward runs context-mark-plan --package-root PATH --name NAME --status STATUS
gh steward runs context-capture-journal|context-install-journal --package-root PATH --name NAME --journal-root PATH
  Runs uses --policy FILE from the trusted checkout (default .agents/gh-steward-recovery-policy.json).
  Packages must be real direct children of --runner-temp PATH (default RUNNER_TEMP).
  --github-output PATH appends bounded outputs to an existing regular Actions output file.
  Recovery outcomes are fresh, resumed, terminal, or recovery_needed. These commands
  read provider state and persist local proofs; they never dispatch provider mutations.
  Publication finalization requires --workflow-sha outside Actions; in Actions it
  is bound to GITHUB_WORKFLOW_SHA. Context commands are provider-free.
  Preparation captures live complete state. Apply accepts only the exact reviewed plan;
  selector/policy overrides are rejected and unknown writes are never replayed.
  Pure planning consumes named complete snapshots, authored payloads and consumer policy.
  Repeat --input for each named JSON object; one may read stdin with name=-.
  Use --out FILE to retain a result. All results use the schema_version 2 envelope.
  No authentication changes, plugin management or automatic Python fallback are performed.
`
