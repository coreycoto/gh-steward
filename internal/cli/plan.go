package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// runPlan is an offline CLI boundary. It deliberately does not resolve a Git
// checkout or initialize the GitHub provider: callers supply the repository
// identity and a complete, already-produced result envelope.
func (r Runner) runPlan(args []string) error {
	if len(args) == 0 || args[0] != "extract" {
		return errors.New("plan supports only the extract action")
	}
	flags := flag.NewFlagSet("gh steward plan extract", flag.ContinueOnError)
	flags.SetOutput(r.Err)
	root := flags.String("repo-root", ".", "base directory for relative input and output paths")
	repoURL := flags.String("repo", "", "exact repository HTTPS URL")
	outerCommand := flags.String("outer-command", "", "expected CLI prepare envelope command")
	planCommand := flags.String("plan-command", "", "expected inner plan command")
	outPath := flags.String("out", "", "required canonical plan output file")
	var declared inputsFlag
	flags.Var(&declared, "input", "one named bounded JSON object: envelope=path; use envelope=- for stdin")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *repoURL == "" || *outerCommand == "" || *planCommand == "" || *outPath == "" {
		return errors.New("plan extract requires --repo, --outer-command, --plan-command and --out")
	}
	if strings.TrimSpace(*outerCommand) != *outerCommand || strings.TrimSpace(*planCommand) != *planCommand {
		return errors.New("expected commands must not have surrounding whitespace")
	}
	if len(declared) != 1 {
		return errors.New("plan extract requires exactly one --input envelope=FILE")
	}
	inputParts := strings.SplitN(declared[0], "=", 2)
	if len(inputParts) != 2 || inputParts[0] != "envelope" || inputParts[1] == "" {
		return errors.New("plan extract input must use envelope=path")
	}

	rootPath, err := resolvePlanRoot(*root)
	if err != nil {
		return err
	}
	repository, err := parsePlanRepositoryURL(*repoURL)
	if err != nil {
		return err
	}
	envelope, err := readPlanEnvelope(rootPath, inputParts[1], r.Input)
	if err != nil {
		return fmt.Errorf("envelope: %w", err)
	}
	plan, err := extractPlan(envelope, repository, *outerCommand, *planCommand)
	if err != nil {
		return err
	}
	if err := writeFile(rootPath, *outPath, plan.Object()); err != nil {
		return err
	}
	path := *outPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(rootPath, path)
	}
	return r.write(contract.Object{
		"schema_version": 2,
		"tool_version":   Version,
		"command":        "plan-extract",
		"repository":     repository.Object(),
		"data": contract.Object{
			"plan_sha256":  plan.SHA256,
			"plan_command": plan.Command,
			"path":         filepath.Clean(path),
		},
	})
}

func resolvePlanRoot(root string) (string, error) {
	path, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("repo-root must be a directory")
	}
	return path, nil
}

func parsePlanRepositoryURL(raw string) (contract.Repository, error) {
	trimmed := strings.TrimSuffix(raw, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 5 || parts[0] != "https:" || parts[1] != "" {
		return contract.Repository{}, errors.New("repository target must be an exact HTTPS host/owner/name URL")
	}
	return contract.ParseRepository(contract.Object{
		"nameWithOwner": parts[3] + "/" + parts[4],
		"url":           trimmed,
	})
}

func readPlanEnvelope(root, input string, stdin io.Reader) (contract.Object, error) {
	if input == "-" {
		if stdin == nil {
			return nil, errors.New("stdin is unavailable")
		}
		return contract.Decode(stdin)
	}
	return loadObject(root, input)
}

func extractPlan(envelope contract.Object, repository contract.Repository, expectedOuter, expectedPlan string) (contract.Plan, error) {
	if len(envelope) != 5 {
		return contract.Plan{}, errors.New("prepare result must use the exact five-field v2 envelope")
	}
	for _, key := range []string{"schema_version", "tool_version", "command", "repository", "data"} {
		if _, exists := envelope[key]; !exists {
			return contract.Plan{}, errors.New("prepare result must use the exact five-field v2 envelope")
		}
	}
	version, err := contract.Integer(envelope["schema_version"])
	if err != nil || version != contract.MachineSchemaVersion {
		return contract.Plan{}, errors.New("prepare result must use schema_version 2")
	}
	if _, err := contract.Nonempty(envelope, "tool_version"); err != nil {
		return contract.Plan{}, err
	}
	outer, err := contract.Nonempty(envelope, "command")
	if err != nil {
		return contract.Plan{}, err
	}
	if outer != expectedOuter {
		return contract.Plan{}, errors.New("prepare result has an unexpected outer command")
	}
	envelopeRepository, err := contract.ObjectAt(envelope, "repository")
	if err != nil {
		return contract.Plan{}, err
	}
	if len(envelopeRepository) != 5 {
		return contract.Plan{}, errors.New("prepare result repository must use the exact five-field identity")
	}
	for key, expected := range repository.Object() {
		actual, ok := envelopeRepository[key].(string)
		if !ok || actual != expected {
			return contract.Plan{}, errors.New("prepare result repository does not match the requested target")
		}
	}
	parsedRepository, err := contract.ParseRepository(envelopeRepository)
	if err != nil || parsedRepository != repository {
		return contract.Plan{}, errors.New("prepare result repository does not match the requested target")
	}
	planObject, err := contract.ObjectAt(envelope, "data")
	if err != nil {
		return contract.Plan{}, err
	}
	plan, err := contract.ParsePlan(planObject)
	if err != nil {
		return contract.Plan{}, err
	}
	if plan.Command != expectedPlan || plan.Repository != repository {
		return contract.Plan{}, errors.New("reviewed plan targets another command or repository")
	}
	return plan, nil
}
