// Package native executes the user's GitHub CLI in one explicit target scope.
// It does not implement authentication or retry ambiguous requests.
package native

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gh "github.com/cli/go-gh/v2"
	"github.com/coreycoto/gh-steward/internal/contract"
)

type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type Executor interface {
	Execute(context.Context, string, []string, []byte, string, []string) (Result, error)
}

type ProcessExecutor struct{ outputLimit int }

type limitedBuffer struct {
	bytes.Buffer
	overflow bool
	limit    int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	limit := b.limit
	if limit == 0 {
		limit = contract.MaxJSONBytes
	}
	if b.Len()+len(p) > limit {
		b.overflow = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func (executor ProcessExecutor) Execute(ctx context.Context, executable string, args []string, input []byte, cwd string, env []string) (Result, error) {
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir, command.Env, command.Stdin = cwd, env, bytes.NewReader(input)
	var stdout, stderr limitedBuffer
	stdout.limit = executor.outputLimit
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if stdout.overflow || stderr.overflow {
		return result, errors.New("native output exceeds the supported size")
	}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}

type Transport struct {
	Root        string
	Repository  contract.Repository
	Executable  string
	Executor    Executor
	Environment []string
	Timeout     time.Duration
}

func New(root string, repository contract.Repository) (*Transport, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		return nil, errors.New("checkout root must be a directory")
	}
	repository, err = contract.ParseRepository(repository.Object())
	if err != nil {
		return nil, err
	}
	env := os.Environ()
	if err := CheckEnvironment(repository, env); err != nil {
		return nil, err
	}
	executable, err := gh.Path()
	if err != nil {
		return nil, errors.New("GitHub CLI is unavailable; install and authenticate native gh")
	}
	return &Transport{Root: absolute, Repository: repository, Executable: executable, Executor: ProcessExecutor{}, Environment: env, Timeout: 45 * time.Second}, nil
}

func envValue(env []string, key string) string {
	value := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			value = strings.TrimPrefix(entry, key+"=")
		}
	}
	return value
}

func CheckEnvironment(repository contract.Repository, env []string) error {
	if host := envValue(env, "GH_HOST"); host != "" && !strings.EqualFold(host, repository.Host) {
		return errors.New("ambient GH_HOST conflicts with the explicit repository")
	}
	if repo := envValue(env, "GH_REPO"); repo != "" {
		allowed := []string{repository.FullName(), repository.Host + "/" + repository.FullName(), repository.URL}
		matched := false
		for _, candidate := range allowed {
			if strings.EqualFold(repo, candidate) {
				matched = true
			}
		}
		if !matched {
			return errors.New("ambient GH_REPO conflicts with the explicit repository")
		}
	}
	return nil
}

func scopedEnvironment(repository contract.Repository, env []string) []string {
	result := []string{}
	for _, entry := range env {
		if !strings.HasPrefix(entry, "GH_HOST=") && !strings.HasPrefix(entry, "GH_REPO=") {
			result = append(result, entry)
		}
	}
	return append(result, "GH_HOST="+repository.Host, "GH_REPO="+repository.Host+"/"+repository.FullName(), "GH_PROMPT_DISABLED=1")
}

func (t *Transport) run(ctx context.Context, args []string, input []byte, staticGraphQL bool) (Result, error) {
	if err := CheckEnvironment(t.Repository, t.Environment); err != nil {
		return Result{}, err
	}
	if t.Executor == nil || t.Executable == "" || t.Timeout <= 0 || len(args) == 0 {
		return Result{}, errors.New("native execution context is incomplete")
	}
	args, err := t.scopedArgs(args, staticGraphQL)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	return t.Executor.Execute(ctx, t.Executable, args, input, t.Root, scopedEnvironment(t.Repository, t.Environment))
}

func (t *Transport) scopedArgs(raw []string, staticGraphQL bool) ([]string, error) {
	args := append([]string{}, raw...)
	if len(args) < 2 {
		return nil, errors.New("native command is incomplete")
	}
	hasHost, hasRepo := false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return nil, errors.New("native scope cannot follow an argument delimiter")
		}
		key, value := "", ""
		switch {
		case arg == "--hostname" || arg == "--repo" || arg == "-R":
			key = arg
			if i+1 >= len(args) {
				return nil, errors.New("native target argument is missing")
			}
			i++
			value = args[i]
		case strings.HasPrefix(arg, "--hostname="):
			key = "--hostname"
			value = strings.TrimPrefix(arg, "--hostname=")
		case strings.HasPrefix(arg, "--repo="):
			key = "--repo"
			value = strings.TrimPrefix(arg, "--repo=")
		case strings.HasPrefix(arg, "-R") && len(arg) > 2:
			key = "-R"
			value = arg[2:]
		}
		if key == "--hostname" {
			if !strings.EqualFold(value, t.Repository.Host) {
				return nil, errors.New("explicit native hostname conflicts with the reviewed repository")
			}
			hasHost = true
		}
		if key == "--repo" || key == "-R" {
			if err := CheckEnvironment(t.Repository, []string{"GH_REPO=" + value}); err != nil || value == "" {
				return nil, errors.New("explicit native repository conflicts with the reviewed target")
			}
			hasRepo = true
		}
	}
	switch args[0] {
	case "api":
		if args[1] == "graphql" && !staticGraphQL {
			return nil, errors.New("GraphQL is available only through a typed target adapter")
		}
		if args[1] != "graphql" {
			if err := t.ValidateRESTEndpoint(args[1]); err != nil {
				return nil, err
			}
		}
		if hasRepo {
			return nil, errors.New("native API target must use its qualified endpoint")
		}
		if !hasHost {
			args = append(args, "--hostname", t.Repository.Host)
		}
	case "issue", "pr":
		if hasHost {
			return nil, errors.New("native issue/PR commands use the qualified repository argument")
		}
		if !hasRepo {
			args = append(args, "--repo", t.Repository.Host+"/"+t.Repository.FullName())
		}
	default:
		return nil, errors.New("this command requires its purpose-specific native adapter")
	}
	return args, nil
}

func resultObject(result Result, err error) (contract.Object, error) {
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("native gh command failed with exit code %d", result.ExitCode)
	}
	return contract.Decode(bytes.NewReader(result.Stdout))
}

func (t *Transport) commandJSON(ctx context.Context, args []string, input []byte) (contract.Object, error) {
	return resultObject(t.run(ctx, args, input, false))
}

func (t *Transport) graphQL(ctx context.Context, query string, variables contract.Object) (contract.Object, error) {
	input, err := contract.Canonical(contract.Object{"query": query, "variables": variables})
	if err != nil {
		return nil, err
	}
	result, err := resultObject(t.run(ctx, []string{"api", "graphql", "--hostname", t.Repository.Host, "--input", "-"}, input, true))
	if err != nil {
		return nil, err
	}
	if raw, exists := result["errors"]; exists && raw != nil {
		a, ok := raw.([]any)
		if !ok || len(a) > 0 {
			return nil, errors.New("GraphQL result contains errors; partial data is not a complete source")
		}
	}
	data, err := contract.ObjectAt(result, "data")
	if err != nil {
		return nil, errors.New("GraphQL result has no complete data object")
	}
	return data, nil
}

func (t *Transport) ValidateRESTEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || strings.ContainsAny(u.Path, "\\") || strings.Contains(u.RawPath, "%") {
		return errors.New("REST endpoint must remain inside the selected repository")
	}
	path := strings.TrimPrefix(u.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) < 3 || parts[0] != "repos" || !strings.EqualFold(parts[1], t.Repository.Owner) || !strings.EqualFold(parts[2], t.Repository.Name) {
		return errors.New("REST endpoint targets a different repository")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return errors.New("REST endpoint contains an escaping path component")
		}
	}
	return nil
}

func (t *Transport) REST(ctx context.Context, method, endpoint string, input contract.Object) (contract.Object, error) {
	if method != "GET" || input != nil {
		return nil, errors.New("generic REST is read-only; use a typed native mutation")
	}
	return t.rest(ctx, method, endpoint, input)
}

func (t *Transport) rest(ctx context.Context, method, endpoint string, input contract.Object) (contract.Object, error) {
	if err := t.ValidateRESTEndpoint(endpoint); err != nil {
		return nil, err
	}
	args := []string{"api", endpoint, "--hostname", t.Repository.Host, "--method", method}
	var body []byte
	if input != nil {
		var err error
		body, err = contract.Canonical(input)
		if err != nil {
			return nil, err
		}
		args = append(args, "--input", "-")
	}
	return t.commandJSON(ctx, args, body)
}

// RESTPages uses native pagination, while rejecting malformed or incomplete
// page envelopes at the caller's declared list boundary.
func (t *Transport) RESTPages(ctx context.Context, endpoint string) ([]any, error) {
	if err := t.ValidateRESTEndpoint(endpoint); err != nil {
		return nil, err
	}
	result, err := t.run(ctx, []string{"api", endpoint, "--hostname", t.Repository.Host, "--method", "GET", "--paginate", "--slurp"}, nil, false)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("native paginated read failed with exit code %d", result.ExitCode)
	}
	wrapped := append([]byte(`{"pages":`), result.Stdout...)
	wrapped = append(wrapped, '}')
	o, err := contract.Decode(bytes.NewReader(wrapped))
	if err != nil {
		return nil, err
	}
	return contract.Array(o, "pages")
}

func Connection(o contract.Object, key string) ([]contract.Object, contract.Object, error) {
	connection, err := contract.ObjectAt(o, key)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := contract.Objects(connection, "nodes")
	if err != nil {
		return nil, nil, err
	}
	page, err := contract.ObjectAt(connection, "pageInfo")
	if err != nil {
		return nil, nil, err
	}
	hasNext, err := contract.Bool(page, "hasNextPage")
	if err != nil {
		return nil, nil, err
	}
	if hasNext {
		cursor, err := contract.Nonempty(page, "endCursor")
		if err != nil || cursor == "" {
			return nil, nil, errors.New("paginated connection has no valid next cursor")
		}
	}
	return nodes, page, nil
}

func CompleteConnection(o contract.Object, key string) ([]contract.Object, error) {
	nodes, page, err := Connection(o, key)
	if err != nil {
		return nil, err
	}
	if page["hasNextPage"] == true {
		return nil, fmt.Errorf("nested %s connection is incomplete", key)
	}
	return nodes, nil
}
