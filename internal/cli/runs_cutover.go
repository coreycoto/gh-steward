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

// Cutover proposals only observe provider state. No plan, apply, import store,
// workflow dispatch or review acknowledgement is available on this flag surface.
func (r Runner) runHistoryCutover(ctx context.Context, args []string) error {
	action := args[0]
	flags := flag.NewFlagSet("gh steward runs "+action, flag.ContinueOnError)
	flags.SetOutput(r.Err)
	root := flags.String("repo-root", ".", "trusted checkout root")
	repo := flags.String("repo", "", "explicit repository HTTPS URL")
	out := flags.String("out", "", "private checkout-relative result file; stdout is summary only")
	format := flags.String("format", "json", "machine output format")
	var workflow, policy string
	var stateReads inputsFlag
	var inputs inputsFlag
	if action == "cutover-preview" {
		flags.StringVar(&workflow, "workflow", "", "exact workflow filename")
		flags.StringVar(&policy, "policy", ".agents/gh-steward-recovery-policy.json", "trusted recovery policy")
		flags.Var(&stateReads, "state-read", "same-repository issue or pull-request object endpoint; repeatable")
	} else {
		flags.Var(&inputs, "input", "one raw baseline or cutover-preview envelope: baseline=FILE")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *format != "json" {
		return errors.New("history cutover commands accept flags only and JSON output")
	}
	if action == "cutover-preview" && *out == "" {
		return errors.New("cutover-preview requires --out to retain its complete private review evidence")
	}
	checkout, repository, err := resolveCheckout(ctx, *root, *repo)
	if err != nil {
		return err
	}
	if err := validateLegacyOutputConflicts(checkout, *out, inputs, policy, ""); err != nil {
		return err
	}
	output, err := prepareLegacyResultOutput(checkout, *out)
	if err != nil {
		return err
	}
	defer output.Close()
	var baseline contract.Object
	if action == "cutover-preview" {
		if filepath.IsAbs(policy) {
			return errors.New("cutover policy must be relative to the trusted checkout")
		}
		path, err := runrecovery.PackageFile(checkout, policy)
		if err != nil {
			return err
		}
		if err := validateActionsControlCheckout(ctx, checkout, policy); err != nil {
			return err
		}
		value, err := runrecovery.LoadJSON(path)
		if err != nil {
			return err
		}
		policyObject, ok := value.(contract.Object)
		if !ok {
			return errors.New("cutover policy must be a JSON object")
		}
		engine, err := runrecovery.NewEngine(policyObject, repository)
		if err != nil {
			return err
		}
		if _, err := engine.MutatorStepAlternatives(workflow); err != nil {
			return err
		}
		reader := r.Actions
		if reader == nil {
			transport, err := native.New(checkout, repository)
			if err != nil {
				return err
			}
			reader = runrecovery.NativeActionsReader{Transport: transport}
		}
		baseline, err = runrecovery.CaptureHistoryCutover(ctx, reader, repository, workflow, []string(stateReads))
		if err != nil {
			return err
		}
	} else {
		value, err := r.recoveryDocument(checkout, inputs, "baseline")
		if err != nil {
			return err
		}
		object, ok := value.(contract.Object)
		if !ok {
			return errors.New("cutover baseline must be a JSON object")
		}
		baseline, err = unwrapHistoryCutover(object, repository)
		if err != nil {
			return err
		}
	}
	result := contract.Object{"schema_version": 2, "tool_version": Version, "command": "workflow-" + action, "repository": repository.Object(), "data": baseline}
	if output != nil {
		encoded, err := runrecovery.Canonical(result)
		if err != nil {
			return err
		}
		if len(encoded)+1 > runrecovery.MaxFileBytes {
			return errors.New("full cutover evidence envelope exceeds the 8 MiB file bound")
		}
		if err := output.Write(append(encoded, '\n')); err != nil {
			return err
		}
	}
	data := contract.Object{"baseline_sha256": baseline["sha256"], "scope": baseline["scope"], "target": baseline["target"], "activation": "not-performed", "validation": "shape-and-digest-only"}
	evidence, err := runrecovery.HistoryCutoverEvidence(baseline)
	if err != nil {
		return err
	}
	data["inventory_counts"] = contract.Object{"runs": len(evidence["run_inventory"].([]any)), "attempts": len(evidence["attempts"].([]any)), "artifacts": len(evidence["artifact_inventory"].([]any)), "state": len(evidence["state_reads"].([]any))}
	data["evidence_schema"] = baseline["schema_version"]
	if archive, ok := baseline["evidence"].(contract.Object); ok {
		data["archive"] = contract.Object{"encoding": archive["encoding"], "compressed_bytes": archive["compressed_bytes"], "uncompressed_bytes": archive["uncompressed_bytes"], "compressed_sha256": archive["compressed_sha256"], "uncompressed_sha256": archive["uncompressed_sha256"]}
	}
	if action == "cutover-preview" {
		data["validation"] = "complete-live-read-only-capture"
	}
	if output != nil {
		data["full_result_path"] = output.path
	}
	return r.write(contract.Object{"schema_version": 2, "tool_version": Version, "command": "workflow-" + action, "repository": repository.Object(), "data": data})
}

func unwrapHistoryCutover(value contract.Object, repository contract.Repository) (contract.Object, error) {
	if value["command"] != nil {
		envelope, err := runrecovery.Exact(value, []string{"schema_version", "tool_version", "command", "repository", "data"}, "cutover preview envelope")
		if err != nil {
			return nil, err
		}
		version, err := contract.Integer(envelope["schema_version"])
		toolVersion, versionErr := contract.Nonempty(envelope, "tool_version")
		if err != nil || versionErr != nil || toolVersion == "" || version != 2 || (envelope["command"] != "workflow-cutover-preview" && envelope["command"] != "workflow-cutover-validate") || !runrecovery.Equal(envelope["repository"], repository.Object()) {
			return nil, errors.New("cutover envelope belongs to another command or repository")
		}
		value, err = contract.ObjectAt(envelope, "data")
		if err != nil {
			return nil, err
		}
	}
	target, err := contract.ObjectAt(value, "target")
	if err != nil || target["repository"] != repository.FullName() || target["server_url"] != "https://"+repository.Host {
		return nil, errors.New("cutover baseline belongs to another repository")
	}
	return runrecovery.ValidateHistoryCutover(value)
}
