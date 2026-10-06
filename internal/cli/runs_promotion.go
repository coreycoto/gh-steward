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

func (r Runner) runHistoryPromotion(ctx context.Context, args []string) error {
	action := args[0]
	flags := flag.NewFlagSet("gh steward runs "+action, flag.ContinueOnError)
	flags.SetOutput(r.Err)
	root := flags.String("repo-root", ".", "trusted checkout root")
	repo := flags.String("repo", "", "explicit repository HTTPS URL")
	out := flags.String("out", "", "private checkout-relative evidence file")
	format := flags.String("format", "json", "machine output format")
	var workflow, policy string
	var inputs, plans, stateReads inputsFlag
	var artifactID int64
	var artifactDigest string
	if action == "promotion-preview" {
		flags.StringVar(&workflow, "workflow", "", "exact workflow filename")
		flags.StringVar(&policy, "policy", ".agents/gh-steward-recovery-policy.json", "trusted recovery policy")
		flags.Var(&inputs, "input", "one reviewed baseline: baseline=FILE")
		flags.Var(&plans, "plan", "exact future native plan name; repeatable")
		flags.Var(&stateReads, "state-read", "explicit same-repository reconciliation endpoint; repeatable")
		flags.Int64Var(&artifactID, "checkpoint-artifact-id", 0, "optional exact hosted preview checkpoint upload ID")
		flags.StringVar(&artifactDigest, "checkpoint-artifact-digest", "", "exact hosted preview checkpoint digest")
	} else {
		flags.Var(&inputs, "input", "one promotion document: promotion=FILE")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *format != "json" {
		return errors.New("promotion commands accept flags only and JSON output")
	}
	if action == "promotion-preview" && (*out == "" || len(plans) == 0 || len(stateReads) == 0 || (artifactID == 0) != (artifactDigest == "") || artifactID < 0) {
		return errors.New("promotion-preview requires --out, explicit plans and state reads; a checkpoint upload needs both exact ID and digest")
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
	var promotion contract.Object
	if action == "promotion-preview" {
		if filepath.IsAbs(policy) {
			return errors.New("promotion policy must be trusted-checkout relative")
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
			return errors.New("promotion policy must be an object")
		}
		engine, err := runrecovery.NewEngine(policyObject, repository)
		if err != nil {
			return err
		}
		value, err = r.recoveryDocument(checkout, inputs, "baseline")
		if err != nil {
			return err
		}
		baselineValue, ok := value.(contract.Object)
		if !ok {
			return errors.New("promotion baseline must be an object")
		}
		baseline, err := unwrapHistoryCutover(baselineValue, repository)
		if err != nil {
			return err
		}
		if baseline["target"].(contract.Object)["workflow_file"] != workflow {
			return errors.New("promotion baseline identifies another workflow")
		}
		reader := r.Actions
		if reader == nil {
			transport, err := native.New(checkout, repository)
			if err != nil {
				return err
			}
			reader = runrecovery.NativeActionsReader{Transport: transport}
		}
		if err := resolveHistoryCutoverReviews(ctx, reader, engine, repository, workflow); err != nil {
			return err
		}
		var checkpoint contract.Object
		if artifactID != 0 {
			checkpoint, err = engine.AcquirePromotionPreviewCheckpoint(ctx, reader, baseline, artifactID, artifactDigest)
			if err != nil {
				return err
			}
		}
		promotion, err = engine.PreviewHistoryPromotion(ctx, reader, baseline, checkpoint, []string(plans), []string(stateReads))
		if err != nil {
			return err
		}
	} else {
		value, err := r.recoveryDocument(checkout, inputs, "promotion")
		if err != nil {
			return err
		}
		object, ok := value.(contract.Object)
		if !ok {
			return errors.New("promotion input must be an object")
		}
		promotion, err = unwrapHistoryPromotion(object, repository)
		if err != nil {
			return err
		}
	}
	envelope := contract.Object{"schema_version": 2, "tool_version": Version, "command": "workflow-" + action, "repository": repository.Object(), "data": promotion}
	if output != nil {
		bytes, err := runrecovery.Canonical(envelope)
		if err != nil {
			return err
		}
		if err := output.Write(append(bytes, '\n')); err != nil {
			return err
		}
	}
	data := contract.Object{"promotion_sha256": promotion["sha256"], "scope": promotion["scope"], "target": promotion["target"], "policy_sha256": promotion["policy_sha256"], "activation": "not-performed", "validation": "shape-and-digest-only"}
	if action == "promotion-preview" {
		data["validation"] = "complete-live-read-only-promotion-capture"
	}
	if output != nil {
		data["full_result_path"] = output.path
	}
	return r.write(contract.Object{"schema_version": 2, "tool_version": Version, "command": "workflow-" + action, "repository": repository.Object(), "data": data})
}

func unwrapHistoryPromotion(value contract.Object, repository contract.Repository) (contract.Object, error) {
	if value["command"] != nil {
		envelope, err := runrecovery.Exact(value, []string{"schema_version", "tool_version", "command", "repository", "data"}, "promotion envelope")
		if err != nil {
			return nil, err
		}
		version, versionErr := contract.Integer(envelope["schema_version"])
		_, toolErr := contract.Nonempty(envelope, "tool_version")
		if versionErr != nil || version != 2 || toolErr != nil || !runrecovery.Equal(envelope["repository"], repository.Object()) || (envelope["command"] != "workflow-promotion-preview" && envelope["command"] != "workflow-promotion-validate") {
			return nil, errors.New("promotion envelope identifies another repository or command")
		}
		value, err = contract.ObjectAt(envelope, "data")
		if err != nil {
			return nil, err
		}
	}
	p, err := runrecovery.ValidateHistoryPromotion(value)
	if err != nil {
		return nil, err
	}
	target := p["target"].(contract.Object)
	if target["repository"] != repository.FullName() || target["server_url"] != "https://"+repository.Host {
		return nil, errors.New("promotion identifies another repository")
	}
	return p, nil
}
