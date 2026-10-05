package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/runrecovery"
)

func (r Runner) runRecovery(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("runs requires an action")
	}
	action := args[0]
	if action == "legacy-review" || action == "legacy-import-preview" || action == "import-legacy" {
		return r.runLegacyRecovery(ctx, args)
	}
	flags := flag.NewFlagSet("gh steward runs "+action, flag.ContinueOnError)
	flags.SetOutput(r.Err)
	root := flags.String("repo-root", ".", "trusted checkout root")
	repo := flags.String("repo", "", "explicit repository HTTPS URL")
	out := flags.String("out", "", "optional machine result file")
	format := flags.String("format", "json", "machine output format")
	workflow := flags.String("workflow", "", "exact workflow filename")
	runID := flags.Int64("run-id", 0, "exact current workflow run ID")
	attempt := flags.Int64("attempt", 0, "exact current workflow attempt")
	runName := flags.String("run-name", "", "immutable current workflow display title")
	recoveryKey := flags.String("recovery-key", "", "semantic target recovery key")
	packageRoot := flags.String("package-root", "", "invocation package below RUNNER_TEMP")
	runnerTemp := flags.String("runner-temp", os.Getenv("RUNNER_TEMP"), "runner scratch directory")
	policyPath := flags.String("policy", ".agents/gh-steward-recovery-policy.json", "trusted consumer recovery policy")
	githubOutput := flags.String("github-output", "", "optional existing GitHub Actions output file")
	artifactID := flags.Int64("artifact-id", 0, "immutable uploaded recovery artifact ID")
	artifactDigest := flags.String("artifact-digest", "", "exact uploaded artifact sha256 digest")
	checkpoint := flags.String("checkpoint", "", "new direct-child checkpoint destination")
	source := flags.String("recovery-source", "", "package-relative original recovery source proof")
	publicationProof := flags.String("publication-proof", "", "optional package-relative publication proof")
	workflowSHA := flags.String("workflow-sha", "", "trusted control workflow commit SHA")
	phase := flags.String("phase", "", "exact plan-free context phase")
	reason := flags.String("reason", "", "bounded context reason")
	var name singleRecoveryValue
	name.flagName = "name"
	flags.Var(&name, "name", "exact native plan name")
	var relative, planCommand, reviewPath, reviewSHA256 singleRecoveryValue
	relative.flagName = "relative"
	planCommand.flagName = "plan-command"
	reviewPath.flagName = "review-path"
	reviewSHA256.flagName = "review-sha256"
	flags.Var(&relative, "relative", "exact package-relative native plan path for direct context registration")
	flags.Var(&planCommand, "plan-command", "expected native plan command for direct context registration")
	flags.Var(&reviewPath, "review-path", "optional package-relative native plan review path")
	flags.Var(&reviewSHA256, "review-sha256", "optional exact native plan review SHA-256")
	journalRoot := flags.String("journal-root", "", "native apply engine journal directory")
	status := flags.String("status", "", "exact native plan status")
	purpose := flags.String("purpose", "apply", "handoff use: apply or transport")
	var inputs inputsFlag
	flags.Var(&inputs, "input", "named raw JSON file; repeatable")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *format != "json" {
		return errors.New("runs accepts flags only and JSON output")
	}
	directNativePlan, err := contextRecordPlanInputMode(action, inputs, name, relative, planCommand, reviewPath, reviewSHA256)
	if err != nil {
		return err
	}
	if (action == "finalize" || action == "qualify-prepared") && *workflowSHA == "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		*workflowSHA = os.Getenv("GITHUB_WORKFLOW_SHA")
	}
	if action != "context-start" {
		if err := validateActionsWorkflowSource(action, *workflowSHA); err != nil {
			return err
		}
	}
	checkout, repository, err := resolveCheckout(ctx, *root, *repo)
	if err != nil {
		return err
	}
	var data contract.Object
	switch action {
	case "digest":
		value, err := r.recoveryDocument(checkout, inputs, "document")
		if err != nil {
			return err
		}
		encoded, err := runrecovery.Canonical(value)
		if err != nil {
			return err
		}
		data = contract.Object{"sha256": runrecovery.SHA256(encoded)}
	case "recover", "finalize", "qualify-prepared", "verify-publication", "acquire-publication-candidate", "acquire-handoff", "finish-noop", "context-start", "context-observe", "context-observe-source", "context-record-plan", "context-phase", "context-mark-plan", "context-capture-journal", "context-install-journal":
		if filepath.IsAbs(*policyPath) {
			return errors.New("recovery policy must be relative to the trusted checkout")
		}
		policyFile, err := runrecovery.PackageFile(checkout, *policyPath)
		if err != nil {
			return err
		}
		if err := validateActionsControlCheckout(ctx, checkout, *policyPath); err != nil {
			return err
		}
		value, err := runrecovery.LoadJSON(policyFile)
		if err != nil {
			return err
		}
		policy, ok := value.(contract.Object)
		if !ok {
			return errors.New("recovery policy must be an object")
		}
		engine, err := runrecovery.NewEngine(policy, repository)
		if err != nil {
			return err
		}
		if strings.HasPrefix(action, "context-") {
			confined, err := runrecovery.ContextPackageDirectory(*packageRoot, *runnerTemp)
			if err != nil {
				return err
			}
			*packageRoot = confined
		}
		needsProvider := action == "recover" || action == "finalize" || action == "qualify-prepared" || action == "verify-publication" || action == "acquire-publication-candidate" || action == "acquire-handoff" || action == "finish-noop"
		reader := r.Actions
		if needsProvider && reader == nil {
			transport, err := native.New(checkout, repository)
			if err != nil {
				return err
			}
			reader = runrecovery.NativeActionsReader{Transport: transport}
		}
		invocation := runrecovery.Invocation{Workflow: *workflow, RunID: *runID, Attempt: *attempt, RunName: *runName, RecoveryKey: *recoveryKey, PackageRoot: *packageRoot, RunnerTemp: *runnerTemp}
		var input contract.Object
		inputName := ""
		switch action {
		case "context-start", "context-observe", "context-observe-source":
			inputName = "context"
		case "context-record-plan":
			if !directNativePlan {
				inputName = "plan"
			}
		}
		if action == "context-record-plan" && directNativePlan {
			value, inputErr := r.recoveryDocument(checkout, inputs, "native-plan")
			if inputErr != nil {
				return inputErr
			}
			planObject, ok := value.(contract.Object)
			if !ok {
				return errors.New("context native plan input must be a raw JSON object")
			}
			plan, parseErr := contract.ParsePlan(planObject)
			if parseErr != nil {
				return errors.New("context native plan input is not a valid canonical reviewed plan: " + parseErr.Error())
			}
			if plan.Repository != repository {
				return errors.New("context native plan belongs to another repository")
			}
			if plan.Command != planCommand.value {
				return errors.New("context native plan command differs from --plan-command")
			}
			input = contract.Object{
				"relative": relative.value, "name": name.value, "command": plan.Command,
				"repository": repository.FullName(), "plan": plan.Object(),
			}
			if reviewPath.provided {
				input["review_path"] = reviewPath.value
				input["review_sha256"] = reviewSHA256.value
			}
		} else if inputName != "" {
			value, inputErr := r.recoveryDocument(checkout, inputs, inputName)
			if inputErr != nil {
				return inputErr
			}
			var ok bool
			input, ok = value.(contract.Object)
			if !ok {
				return errors.New("context input must be a raw JSON object")
			}
			if action == "context-start" && os.Getenv("GITHUB_ACTIONS") == "true" {
				trusted, ok := input["trusted_source_sha"].(string)
				if !ok {
					return errors.New("Actions context requires its runtime trusted_source_sha")
				}
				if err := validateActionsWorkflowSource("context-start", trusted); err != nil {
					return err
				}
			}
		} else if action == "recover" && len(inputs) != 0 {
			value, inputErr := r.recoveryDocument(checkout, inputs, "legacy-checkpoint")
			if inputErr != nil {
				return inputErr
			}
			var ok bool
			input, ok = value.(contract.Object)
			if !ok {
				return errors.New("legacy checkpoint must be a raw settlement chain object")
			}
		} else if len(inputs) != 0 {
			return errors.New("this runs action does not accept named inputs")
		}
		switch action {
		case "recover":
			if input == nil {
				data, err = engine.Recover(ctx, reader, invocation)
			} else {
				data, err = engine.RecoverWithLegacyCheckpoint(ctx, reader, invocation, input)
			}
		case "finalize":
			data, err = engine.Finalize(ctx, reader, runrecovery.FinalizeOptions{Invocation: invocation, ArtifactID: *artifactID, ArtifactDigest: *artifactDigest, Checkpoint: *checkpoint, RecoverySource: *source, PublicationProof: *publicationProof, WorkflowSHA: *workflowSHA})
		case "qualify-prepared":
			data, err = engine.QualifyPrepared(ctx, reader, runrecovery.PreparedQualificationOptions{Invocation: invocation, WorkflowSHA: *workflowSHA})
		case "verify-publication":
			data, err = engine.VerifyPublication(ctx, reader, invocation, *workflowSHA)
		case "acquire-publication-candidate":
			data, err = engine.AcquirePublicationCandidate(ctx, reader, invocation, *artifactID, *artifactDigest, *workflowSHA)
		case "acquire-handoff":
			data, err = engine.AcquireHandoffFor(ctx, reader, invocation, *artifactID, *artifactDigest, *purpose)
		case "finish-noop":
			data, err = engine.FinishNoop(ctx, reader, runrecovery.NoopOptions{Invocation: invocation, WorkflowSHA: *workflowSHA, ToolVersion: Version})
		case "context-start":
			data, err = engine.InitializeContext(*packageRoot, input)
		case "context-observe":
			data, err = engine.ObserveContext(*packageRoot, input)
		case "context-observe-source":
			data, err = engine.ObserveSourceContext(*packageRoot, input)
		case "context-record-plan":
			data, err = engine.RecordContextPlan(*packageRoot, input)
		case "context-phase":
			data, err = engine.SetContextPhase(*packageRoot, *phase, *reason)
		case "context-mark-plan":
			data, err = engine.MarkContextPlan(*packageRoot, name.value, *status)
		case "context-capture-journal":
			data, err = engine.CaptureJournal(*packageRoot, *journalRoot, name.value)
		case "context-install-journal":
			data, err = engine.InstallRestoredJournal(*packageRoot, *journalRoot, name.value)
		}
		if err != nil {
			return err
		}
		outputs := data
		if strings.HasPrefix(action, "context-") {
			outputs = contract.Object{"outcome": "context"}
			if phase, ok := data["phase"].(string); ok {
				outputs["phase"] = phase
			}
		}
		if err := runrecovery.WriteActionsOutput(*githubOutput, outputs); err != nil {
			return err
		}
	default:
		return errors.New("unsupported runs action")
	}
	result := contract.Object{"schema_version": 2, "tool_version": Version, "command": "workflow-" + action, "repository": repository.Object(), "data": data}
	if *out != "" {
		if err := writeFile(checkout, *out, result); err != nil {
			return err
		}
	}
	return r.write(result)
}

type singleRecoveryValue struct {
	flagName string
	value    string
	provided bool
}

func (v *singleRecoveryValue) String() string { return v.value }

func (v *singleRecoveryValue) Set(value string) error {
	if v.provided {
		return errors.New("--" + v.flagName + " may be specified only once")
	}
	v.value = value
	v.provided = true
	return nil
}

func contextRecordPlanInputMode(action string, inputs inputsFlag, name, relative, planCommand, reviewPath, reviewSHA256 singleRecoveryValue) (bool, error) {
	hasNativePlan := false
	for _, input := range inputs {
		key, _, ok := strings.Cut(input, "=")
		if ok && key == "native-plan" {
			hasNativePlan = true
		}
	}
	metadataProvided := relative.provided || planCommand.provided || reviewPath.provided || reviewSHA256.provided

	if action != "context-record-plan" {
		if hasNativePlan || metadataProvided {
			return false, errors.New("direct native plan inputs are supported only by runs context-record-plan")
		}
		return false, nil
	}
	if !hasNativePlan {
		if metadataProvided || name.provided {
			return false, errors.New("direct context plan metadata requires exactly one --input native-plan=FILE")
		}
		return false, nil
	}
	if len(inputs) != 1 {
		return false, errors.New("direct context plan registration requires exactly one --input native-plan=FILE")
	}
	parts := strings.SplitN(inputs[0], "=", 2)
	if len(parts) != 2 || parts[0] != "native-plan" || parts[1] == "" || parts[1] == "-" {
		return false, errors.New("direct context plan input must use native-plan=FILE")
	}
	if !name.provided || strings.TrimSpace(name.value) == "" || !relative.provided || strings.TrimSpace(relative.value) == "" ||
		!planCommand.provided || strings.TrimSpace(planCommand.value) == "" {
		return false, errors.New("direct context plan registration requires --name, --relative and --plan-command")
	}
	if reviewPath.provided != reviewSHA256.provided || (reviewPath.provided && (strings.TrimSpace(reviewPath.value) == "" || strings.TrimSpace(reviewSHA256.value) == "")) {
		return false, errors.New("--review-path and --review-sha256 must be supplied together")
	}
	return true, nil
}

var actionsWorkflowSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// GitHub supplies the executed workflow's source separately from the trigger's
// commit. A workflow_run caller must never qualify the trigger SHA as control.
func validateActionsWorkflowSource(action, supplied string) error {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return nil
	}
	switch action {
	case "context-start", "finalize", "qualify-prepared", "verify-publication", "acquire-publication-candidate", "finish-noop":
		runtimeSHA := os.Getenv("GITHUB_WORKFLOW_SHA")
		if !actionsWorkflowSHA.MatchString(runtimeSHA) || supplied != runtimeSHA {
			return errors.New("trusted workflow source must equal the actual GITHUB_WORKFLOW_SHA runtime value")
		}
	}
	return nil
}

func validateActionsControlCheckout(ctx context.Context, checkout, policyPath string) error {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return nil
	}
	runtimeSHA := os.Getenv("GITHUB_WORKFLOW_SHA")
	if !actionsWorkflowSHA.MatchString(runtimeSHA) {
		return errors.New("Actions recovery requires the actual GITHUB_WORKFLOW_SHA runtime value")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "HEAD")
	command.Dir = checkout
	head, err := command.Output()
	if err != nil || strings.TrimSpace(string(head)) != runtimeSHA {
		return errors.New("trusted control checkout HEAD must equal GITHUB_WORKFLOW_SHA")
	}
	working, err := runrecovery.ReadPackageFile(checkout, policyPath)
	if err != nil {
		return err
	}
	command = exec.CommandContext(ctx, "git", "show", runtimeSHA+":"+filepath.ToSlash(policyPath))
	command.Dir = checkout
	pipe, err := command.StdoutPipe()
	if err != nil {
		return errors.New("trusted workflow policy blob is unavailable")
	}
	if err := command.Start(); err != nil {
		return errors.New("trusted workflow policy blob is unavailable")
	}
	committed, readErr := io.ReadAll(io.LimitReader(pipe, runrecovery.MaxFileBytes+1))
	if readErr != nil || len(committed) > runrecovery.MaxFileBytes {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if readErr != nil || waitErr != nil || len(committed) > runrecovery.MaxFileBytes || !bytes.Equal(committed, working) {
		return errors.New("recovery policy must equal the exact trusted runtime commit blob")
	}
	return nil
}

// Recovery proofs hash the complete raw JSON document. Unlike planner inputs,
// this boundary must not unwrap a previous machine result or discard fields.
func (r Runner) recoveryDocument(root string, inputs inputsFlag, name string) (any, error) {
	if len(inputs) != 1 {
		return nil, errors.New("runs action requires exactly one named input")
	}
	parts := strings.SplitN(inputs[0], "=", 2)
	if len(parts) != 2 || parts[0] != name || parts[1] == "" {
		return nil, errors.New("unexpected runs input name or missing file")
	}
	if parts[1] == "-" {
		if r.Input == nil {
			return nil, errors.New("named stdin input is unavailable")
		}
		data, err := io.ReadAll(io.LimitReader(r.Input, runrecovery.MaxFileBytes+1))
		if err != nil {
			return nil, err
		}
		return runrecovery.DecodeValue(data)
	}
	path := parts[1]
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	return runrecovery.LoadJSON(path)
}
