package cli

import (
	"context"
	"errors"
	"flag"
	"path/filepath"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
	"github.com/coreycoto/gh-steward/internal/runrecovery"
)

// Legacy commands have their own flag surface. Provider mutation, native plan
// registration and recovery dispatch arguments are deliberately unavailable.
func (r Runner) runLegacyRecovery(ctx context.Context, args []string) error {
	action := args[0]
	flags := flag.NewFlagSet("gh steward runs "+action, flag.ContinueOnError)
	flags.SetOutput(r.Err)
	root := flags.String("repo-root", ".", "trusted checkout root")
	repo := flags.String("repo", "", "explicit repository HTTPS URL")
	out := flags.String("out", "", "optional private full-result file; stdout is summary only")
	format := flags.String("format", "json", "machine output format")
	var inputs inputsFlag
	flags.Var(&inputs, "input", "one named raw JSON document")
	var policyPath, storePath, approvedSHA string
	if action != "legacy-review" {
		flags.StringVar(&policyPath, "policy", ".agents/gh-steward-recovery-policy.json", "trusted consumer recovery policy")
	}
	if action == "import-legacy" {
		flags.StringVar(&storePath, "store", "", "existing private append-only store relative to checkout")
		flags.StringVar(&approvedSHA, "approve-review-sha", "", "exact import review digest; not a grant of human approval")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *format != "json" {
		return errors.New("legacy runs commands accept flags only and JSON output")
	}
	checkout, repository, err := resolveCheckout(ctx, *root, *repo)
	if err != nil {
		return err
	}
	if *out != "" {
		if _, err := legacyOutputParts(*out); err != nil {
			return err
		}
		if err := validateLegacyOutputConflicts(checkout, *out, inputs, policyPath, storePath); err != nil {
			return err
		}
	}
	fullOutput, err := prepareLegacyResultOutput(checkout, *out)
	if err != nil {
		return err
	}
	defer fullOutput.Close()
	if action == "import-legacy" && fullOutput != nil && fullOutput.hasExisting {
		return errors.New("import-legacy requires a fresh --out path; omit --out or choose a new private file")
	}
	name := map[string]string{"legacy-review": "evidence", "legacy-import-preview": "candidate", "import-legacy": "import"}[action]
	value, err := r.recoveryDocument(checkout, inputs, name)
	if err != nil {
		return err
	}
	fields := map[string][]string{
		"legacy-review":         {"evidence", "review"},
		"legacy-import-preview": {"report", "checkpoint", "outcome", "dispositions"},
		"import-legacy":         {"report", "review", "checkpoint", "history"},
	}
	document, err := runrecovery.Exact(value, fields[action], "legacy command input")
	if err != nil {
		return err
	}
	var data contract.Object
	if action == "legacy-review" {
		evidence, err := contract.ObjectAt(document, "evidence")
		if err != nil {
			return err
		}
		review, err := contract.ObjectAt(document, "review")
		if err != nil {
			return err
		}
		data, err = runrecovery.VerifyLegacyEvidence(evidence, review)
		if err != nil {
			return err
		}
		target, _ := data["target"].(contract.Object)
		if target["repository"] != repository.FullName() || target["server_url"] != "https://"+repository.Host {
			return errors.New("legacy evidence belongs to another checkout repository")
		}
	} else {
		if filepath.IsAbs(policyPath) {
			return errors.New("legacy recovery policy must be relative to the trusted checkout")
		}
		path, err := runrecovery.PackageFile(checkout, policyPath)
		if err != nil {
			return err
		}
		if err := validateActionsControlCheckout(ctx, checkout, policyPath); err != nil {
			return err
		}
		policyValue, err := runrecovery.LoadJSON(path)
		if err != nil {
			return err
		}
		policy, ok := policyValue.(contract.Object)
		if !ok {
			return errors.New("legacy recovery policy must be an object")
		}
		engine, err := runrecovery.NewEngine(policy, repository)
		if err != nil {
			return err
		}
		report, err := contract.ObjectAt(document, "report")
		if err != nil {
			return err
		}
		checkpoint, err := contract.ObjectAt(document, "checkpoint")
		if err != nil {
			return err
		}
		var importReview, history contract.Object
		var store string
		if action == "import-legacy" {
			importReview, err = contract.ObjectAt(document, "review")
			if err != nil {
				return err
			}
			history, err = contract.ObjectAt(document, "history")
			if err != nil {
				return err
			}
			if !runrecovery.IsSHA256(approvedSHA) || importReview["sha256"] != approvedSHA {
				return errors.New("legacy import requires the exact --approve-review-sha; it does not authorize a provider write")
			}
			if storePath == "" || filepath.IsAbs(storePath) || filepath.Clean(storePath) != storePath || storePath == "." {
				return errors.New("legacy import store must be an exact checkout-relative directory")
			}
			store = filepath.Join(checkout, storePath)
			if rel, err := filepath.Rel(checkout, store); err != nil || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
				return errors.New("legacy import store escapes the trusted checkout")
			}
		}
		reader := r.Actions
		if reader == nil {
			transport, err := native.New(checkout, repository)
			if err != nil {
				return err
			}
			reader = runrecovery.NativeActionsReader{Transport: transport}
		}
		if action == "legacy-import-preview" {
			outcome, err := contract.String(document, "outcome")
			if err != nil {
				return err
			}
			dispositions, ok := document["dispositions"].([]any)
			if !ok {
				return errors.New("legacy preview requires an explicit disposition array")
			}
			data, err = engine.PreviewLegacyImport(ctx, reader, report, checkpoint, outcome, dispositions)
		} else {
			data, err = engine.ImportLegacy(ctx, reader, runrecovery.LegacyImportOptions{Report: report, Review: importReview, Checkpoint: checkpoint, History: history, StoreDirectory: store})
		}
		if err != nil {
			return err
		}
	}
	result := contract.Object{"schema_version": 2, "tool_version": Version, "command": "workflow-" + action, "repository": repository.Object(), "data": data}
	if fullOutput != nil {
		serialized, err := runrecovery.Canonical(result)
		if err != nil {
			return err
		}
		if err := fullOutput.Write(append(serialized, '\n')); err != nil {
			return err
		}
	}
	return r.write(legacyRecoverySummary(action, data, repository, fullOutput))
}

func legacyRecoverySummary(action string, data contract.Object, repository contract.Repository, output *legacyResultOutput) contract.Object {
	summary := contract.Object{}
	switch action {
	case "legacy-review":
		run, _ := data["run"].(contract.Object)
		summary = contract.Object{
			"target": data["target"], "run_id": run["id"], "attempt": data["attempt"],
			"classification": data["classification"], "intent_proven": data["intent_proven"],
			"reasons": data["reasons"], "report_sha256": data["sha256"],
		}
	case "legacy-import-preview":
		review, _ := data["review"].(contract.Object)
		checkpoint, _ := data["checkpoint"].(contract.Object)
		summary = contract.Object{
			"target": review["target"], "run_id": review["run_id"], "attempt": review["attempt"],
			"review_sha256": review["sha256"], "history_sha256": review["history_sha256"],
			"checkpoint_sha256": checkpoint["sha256"], "outcome": review["outcome"],
			"chain_version": review["chain_version"],
		}
	case "import-legacy":
		checkpoint, _ := data["checkpoint"].(contract.Object)
		summary = contract.Object{
			"outcome": data["outcome"], "review_sha256": data["review_sha256"],
			"checkpoint_sha256":          checkpoint["sha256"],
			"imported_checkpoint_sha256": data["imported_checkpoint_sha256"],
		}
	}
	if output != nil {
		summary["full_result_path"] = output.path
	}
	return contract.Object{
		"schema_version": 2, "tool_version": Version, "command": "workflow-" + action,
		"repository": repository.Object(), "data": summary,
	}
}
