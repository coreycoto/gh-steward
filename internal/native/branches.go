package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// DeleteBranch leases the exact reviewed ref rather than using an unguarded
// REST delete. Git runs in disposable scratch with its configuration and hooks
// isolated from the checkout; credentials stay inside native gh and Git.
func (t *Transport) DeleteBranch(ctx context.Context, branch, expectedSHA, repositoryNodeID, operationID string) (contract.Object, error) {
	if err := ValidateBranchName(branch); err != nil {
		return nil, err
	}
	if _, err := CommitOID(expectedSHA); err != nil {
		return nil, err
	}
	if _, err := OperationMarker(operationID); err != nil {
		return nil, err
	}
	if repositoryNodeID == "" {
		return nil, errors.New("branch deletion requires the reviewed repository incarnation")
	}
	if err := CheckEnvironment(t.Repository, t.Environment); err != nil {
		return nil, err
	}
	if t.Executor == nil || !filepath.IsAbs(t.Executable) || t.Timeout <= 0 {
		return nil, errors.New("leased branch deletion requires a complete native execution context")
	}
	repository, err := t.ReadRepository(ctx)
	if err != nil {
		return nil, err
	}
	defaultRef, err := contract.ObjectAt(repository, "defaultBranchRef")
	if err != nil || defaultRef["name"] == branch || repository["id"] != repositoryNodeID {
		return nil, errors.New("branch deletion cannot target the default branch or another repository incarnation")
	}
	selected, err := t.ReadBranch(ctx, branch)
	if err != nil {
		return nil, err
	}
	if selected["repository_node_id"] != repositoryNodeID || selected["sha"] != expectedSHA {
		return nil, errors.New("reviewed branch changed before leased deletion")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, errors.New("leased branch deletion requires native Git")
	}
	git, err = filepath.Abs(git)
	if err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp("", "gh-steward-branch-delete-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	environment := isolatedGitEnvironment(t.Repository, t.Environment)
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	initialized, err := t.Executor.Execute(ctx, git, []string{"init", "--bare", "--quiet", "--template=", scratch}, nil, scratch, environment)
	if err != nil || initialized.ExitCode != 0 {
		return nil, errors.New("disposable branch-deletion Git context could not be initialized")
	}
	remoteURL := t.Repository.URL + ".git"
	args := leasedDeleteArguments(t.Repository.Host, t.Executable, remoteURL, branch, expectedSHA)
	result, err := t.Executor.Execute(ctx, git, args, nil, scratch, environment)
	if err != nil {
		return nil, fmt.Errorf("leased branch dispatch has an unknown outcome: %w", err)
	}
	ack := contract.Object{"exit_code": result.ExitCode, "stdout": string(result.Stdout), "stderr": string(result.Stderr),
		"remote_url": remoteURL, "ref": "refs/heads/" + branch, "expected_sha": expectedSHA}
	if err := ValidateBranchDeletion(ack, t.Repository, branch, expectedSHA); err != nil {
		return nil, err
	}
	return ack, nil
}

func isolatedGitEnvironment(repository contract.Repository, source []string) []string {
	environment := []string{}
	for _, entry := range source {
		key, _, _ := strings.Cut(entry, "=")
		// Git's environment can override -c settings, install a different remote
		// helper or expose tracing output. GH authentication variables remain.
		if strings.HasPrefix(key, "GIT_") || key == "SSH_ASKPASS" {
			continue
		}
		environment = append(environment, entry)
	}
	environment = scopedEnvironment(repository, environment)
	return append(environment, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

func leasedDeleteArguments(host, ghExecutable, remoteURL, branch, expectedSHA string) []string {
	// Git interprets !credential helpers through a shell. Quote the already
	// selected executable as data; no caller text is inserted into shell code.
	quoted := "'" + strings.ReplaceAll(ghExecutable, "'", "'\"'\"'") + "'"
	return []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never",
		"-c", "http.followRedirects=false", "-c", "credential.helper=", "-c", "credential.https://" + host + ".helper=!" + quoted + " auth git-credential",
		"push", "--porcelain", "--no-verify", "--no-recurse-submodules", "--force-with-lease=refs/heads/" + branch + ":" + expectedSHA,
		remoteURL, ":refs/heads/" + branch}
}

// ValidateBranchDeletion rechecks the native porcelain completion in retained
// journals. A missing, rejected, repeated, foreign or additional ref row is
// not proof that this one reviewed deletion completed.
func ValidateBranchDeletion(ack contract.Object, repository contract.Repository, branch, expectedSHA string) error {
	if err := ValidateBranchName(branch); err != nil {
		return err
	}
	if _, err := CommitOID(expectedSHA); err != nil {
		return err
	}
	if !exactObjectKeys(ack, "exit_code", "stdout", "stderr", "remote_url", "ref", "expected_sha") {
		return errors.New("branch deletion acknowledgement has unsupported or missing fields")
	}
	exit, err := contract.Integer(ack["exit_code"])
	if err != nil || exit != 0 || ack["remote_url"] != repository.URL+".git" || ack["ref"] != "refs/heads/"+branch || ack["expected_sha"] != expectedSHA {
		return errors.New("branch deletion was not positively acknowledged for the reviewed target and lease")
	}
	stdout, err := contract.String(ack, "stdout")
	if err != nil {
		return err
	}
	if _, err := contract.String(ack, "stderr"); err != nil {
		return err
	}
	rows, headers, completed := 0, 0, 0
	for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		switch {
		case line == "To "+repository.URL+".git":
			headers++
		case line == "Done":
			completed++
		case strings.Contains(line, "\t"):
			fields := strings.Split(line, "\t")
			if len(fields) != 3 || fields[0] != "-" || fields[1] != ":refs/heads/"+branch || fields[2] != "[deleted]" {
				return errors.New("native Git did not identify the one reviewed branch deletion")
			}
			rows++
		default:
			return errors.New("branch deletion output contains unrecognized completion data")
		}
	}
	if rows != 1 || headers != 1 || completed != 1 {
		return errors.New("branch deletion output has incomplete or repeated completion evidence")
	}
	return nil
}

func exactObjectKeys(object contract.Object, keys ...string) bool {
	if len(object) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, present := object[key]; !present {
			return false
		}
	}
	return true
}

// ReadOpenPullRequestsForBase proves whether a branch still supports another
// open PR. The complete connection includes draft and fork PRs as dependents;
// no head repository assumption can hide an active base dependency.
func (t *Transport) ReadOpenPullRequestsForBase(ctx context.Context, branch string) (contract.Object, error) {
	if err := ValidateBranchName(branch); err != nil {
		return nil, err
	}
	var cursor *string
	var repositoryNodeID string
	seenCursors, seenIDs, seenNumbers := map[string]bool{}, map[string]bool{}, map[int64]bool{}
	rows := []any{}
	for {
		variables := t.repoVariables()
		variables["base"], variables["cursor"] = branch, cursor
		data, err := t.graphQL(ctx, `query($owner:String!,$name:String!,$base:String!,$cursor:String){repository(owner:$owner,name:$name){`+repositoryFields+` pullRequests(first:100,after:$cursor,baseRefName:$base,states:[OPEN],orderBy:{field:CREATED_AT,direction:ASC}){nodes{id number url baseRefName headRefName state isDraft merged}pageInfo{hasNextPage endCursor}}}}`, variables)
		if err != nil {
			return nil, err
		}
		repository, err := t.repositoryResult(data)
		if err != nil {
			return nil, err
		}
		id, _ := contract.Nonempty(repository, "id")
		if repositoryNodeID != "" && id != repositoryNodeID {
			return nil, errors.New("repository incarnation changed during base PR pagination")
		}
		repositoryNodeID = id
		prs, page, err := Connection(repository, "pullRequests")
		if err != nil {
			return nil, err
		}
		for _, pr := range prs {
			number, err := contract.PositiveInteger(pr["number"])
			if err != nil || seenNumbers[number] {
				return nil, errors.New("base PR collection contains a malformed or repeated number")
			}
			nodeID, err := contract.Nonempty(pr, "id")
			if err != nil || seenIDs[nodeID] {
				return nil, errors.New("base PR collection contains a malformed or repeated immutable identity")
			}
			if err := t.ValidatePullRequestURL(pr["url"], number); err != nil {
				return nil, err
			}
			if pr["baseRefName"] != branch || pr["state"] != "OPEN" || pr["merged"] != false {
				return nil, errors.New("base PR has another branch or an inconsistent lifecycle state")
			}
			head, err := contract.Nonempty(pr, "headRefName")
			if err != nil {
				return nil, err
			}
			if err := ValidateBranchName(head); err != nil {
				return nil, err
			}
			if _, err := contract.Bool(pr, "isDraft"); err != nil {
				return nil, err
			}
			seenNumbers[number], seenIDs[nodeID] = true, true
			rows = append(rows, pr)
		}
		if len(rows) > 100000 {
			return nil, errors.New("base PR collection exceeds supported size")
		}
		if page["hasNextPage"] != true {
			break
		}
		next, err := contract.Nonempty(page, "endCursor")
		if err != nil || seenCursors[next] {
			return nil, errors.New("base PR pagination cursor did not advance")
		}
		seenCursors[next], cursor = true, &next
	}
	sort.Slice(rows, func(i, j int) bool {
		a, _ := contract.PositiveInteger(rows[i].(contract.Object)["number"])
		b, _ := contract.PositiveInteger(rows[j].(contract.Object)["number"])
		return a < b
	})
	return contract.Object{"repo": t.Repository.Object(), "repository_node_id": repositoryNodeID, "base_branch": branch, "pull_requests": rows, "provenance": contract.Object{"live": true, "complete": true, "source": "github_api"}}, nil
}
