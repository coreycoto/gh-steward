package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/native"
)

type MergeReader interface {
	ReadRepository(context.Context) (contract.Object, error)
	ReadPullRequest(context.Context, int64) (contract.Object, error)
	RequiredChecks(context.Context, int64) ([]contract.Object, error)
	ReadWorkflowRun(context.Context, int64) (contract.Object, error)
}

// NormalizeMergeTrigger records every trigger value consumed by the merge
// gate. The current run is independently read and compared with this submitted
// event before it can qualify a candidate.
func NormalizeMergeTrigger(raw contract.Object, repo contract.Repository) (contract.Object, error) {
	run, err := contract.ObjectAt(raw, "workflow_run")
	if err != nil {
		return nil, errors.New("merge preparation requires a workflow_run event")
	}
	id, err := contract.PositiveInteger(run["id"])
	if err != nil {
		return nil, err
	}
	r, err := contract.ObjectAt(run, "repository")
	if err != nil || !strings.EqualFold(fmt.Sprint(r["full_name"]), repo.FullName()) {
		return nil, errors.New("merge event repository differs from the selected repository")
	}
	out := contract.Object{"id": id, "repository": contract.Object{"full_name": repo.FullName()}}
	for _, key := range []string{"name", "event", "status", "head_branch"} {
		out[key], err = contract.Nonempty(run, key)
		if err != nil {
			return nil, err
		}
	}
	out["head_sha"], err = native.CommitOID(run["head_sha"])
	if err != nil {
		return nil, err
	}
	out["conclusion"] = run["conclusion"]
	if run["conclusion"] != nil {
		if _, err := contract.Nonempty(run, "conclusion"); err != nil {
			return nil, err
		}
	}
	for _, key := range []string{"run_attempt", "workflow_id"} {
		out[key], err = contract.PositiveInteger(run[key])
		if err != nil {
			return nil, fmt.Errorf("merge event %s is unavailable: %w", key, err)
		}
	}
	associations, err := contract.Objects(run, "pull_requests")
	if err != nil {
		return nil, err
	}
	prs := []any{}
	seen := map[int64]bool{}
	for _, association := range associations {
		n, err := contract.PositiveInteger(association["number"])
		if err != nil || seen[n] {
			return nil, errors.New("merge trigger has invalid or duplicate pull request association")
		}
		seen[n] = true
		head, err := contract.ObjectAt(association, "head")
		if err != nil {
			return nil, err
		}
		sha, err := native.CommitOID(head["sha"])
		if err != nil {
			return nil, err
		}
		branch, err := contract.Nonempty(head, "ref")
		if err != nil {
			return nil, err
		}
		prs = append(prs, contract.Object{"number": n, "head": contract.Object{"sha": sha, "ref": branch}})
	}
	sort.Slice(prs, func(i, j int) bool {
		a, _ := contract.PositiveInteger(prs[i].(map[string]any)["number"])
		b, _ := contract.PositiveInteger(prs[j].(map[string]any)["number"])
		return a < b
	})
	out["pull_requests"] = prs
	return contract.Object{"workflow_run": out}, nil
}

func NormalizeMergePullRequest(raw contract.Object, repo contract.Repository, number int64) (contract.Object, error) {
	actual, err := contract.PositiveInteger(raw["number"])
	if err != nil || actual != number {
		return nil, errors.New("merge pull request number differs from the selected candidate")
	}
	if err := (&native.Transport{Repository: repo}).ValidatePullRequestURL(raw["url"], number); err != nil {
		return nil, err
	}
	out := contract.Object{"number": number, "repository": repo.FullName()}
	for _, key := range []string{"id", "url", "baseRefName", "headRefName", "state", "mergeStateStatus"} {
		out[key], err = contract.Nonempty(raw, key)
		if err != nil {
			return nil, err
		}
	}
	for _, key := range []string{"title", "body"} {
		out[key], err = contract.String(raw, key)
		if err != nil {
			return nil, err
		}
	}
	for _, key := range []string{"isDraft", "merged"} {
		out[key], err = contract.Bool(raw, key)
		if err != nil {
			return nil, err
		}
	}
	if raw["state"] != "OPEN" && raw["state"] != "CLOSED" && raw["state"] != "MERGED" {
		return nil, errors.New("merge pull request has an unknown state")
	}
	if (raw["state"] == "MERGED") != (raw["merged"] == true) {
		return nil, errors.New("merge pull request state and merged fact disagree")
	}
	out["headRefOid"], err = native.CommitOID(raw["headRefOid"])
	if err != nil {
		return nil, err
	}
	out["reviewDecision"] = ""
	if raw["reviewDecision"] != nil {
		out["reviewDecision"], err = contract.String(raw, "reviewDecision")
		if err != nil {
			return nil, err
		}
	}
	out["merge_commit_sha"] = nil
	if raw["merged"] == true {
		commit, err := contract.ObjectAt(raw, "mergeCommit")
		if err != nil {
			return nil, errors.New("merged pull request has no immutable merge commit")
		}
		out["merge_commit_sha"], err = native.CommitOID(commit["oid"])
		if err != nil {
			return nil, err
		}
		// GitHub's mergeability projection is transient after a merge. The
		// independently returned terminal state and merge commit are exact.
		out["mergeStateStatus"] = "MERGED"
	}
	labels, err := native.CompleteConnection(raw, "labels")
	if err != nil {
		return nil, err
	}
	names, seen := []string{}, map[string]bool{}
	for _, label := range labels {
		name, err := contract.Nonempty(label, "name")
		if err != nil || seen[name] {
			return nil, errors.New("merge label inventory has invalid or duplicate names")
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	values := []any{}
	for _, name := range names {
		values = append(values, contract.Object{"name": name})
	}
	out["labels"] = values
	out["author"] = nil
	if raw["author"] != nil {
		author, err := contract.ObjectAt(raw, "author")
		if err != nil {
			return nil, err
		}
		login, err := contract.Nonempty(author, "login")
		if err != nil {
			return nil, err
		}
		typename, err := contract.Nonempty(author, "__typename")
		if err != nil || (typename != "User" && typename != "Bot" && typename != "Mannequin") {
			return nil, errors.New("pull request author type is unavailable")
		}
		out["author"] = contract.Object{"login": login, "is_bot": typename == "Bot"}
	}
	out["head_repository"] = nil
	if raw["headRepository"] != nil {
		headRepo, err := contract.ObjectAt(raw, "headRepository")
		if err != nil {
			return nil, err
		}
		parsed, err := contract.ParseRepository(headRepo)
		if err != nil || parsed.Host != repo.Host {
			return nil, errors.New("pull request head repository identity is invalid")
		}
		out["head_repository"] = parsed.Object()
	}
	return out, nil
}

func MergeInventory(ctx context.Context, reader MergeReader, repo contract.Repository, event contract.Object) (contract.Object, error) {
	if reader == nil {
		return nil, errors.New("merge inventory requires a native reader")
	}
	trigger, err := NormalizeMergeTrigger(event, repo)
	if err != nil {
		return nil, err
	}
	submitted, _ := contract.ObjectAt(trigger, "workflow_run")
	runID, _ := contract.PositiveInteger(submitted["id"])
	run, err := reader.ReadWorkflowRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	observed, err := NormalizeMergeTrigger(contract.Object{"workflow_run": run}, repo)
	if err != nil {
		return nil, err
	}
	a, _ := contract.Digest(trigger)
	b, _ := contract.Digest(observed)
	if a != b {
		return nil, errors.New("current workflow run differs from the submitted merge trigger")
	}
	r, err := reader.ReadRepository(ctx)
	if err != nil {
		return nil, err
	}
	parsed, err := contract.ParseRepository(r)
	if err != nil || parsed != repo {
		return nil, errors.New("merge repository read targets another repository")
	}
	id, err := contract.Nonempty(r, "id")
	if err != nil {
		return nil, err
	}
	runRepository, err := contract.ObjectAt(run, "repository")
	if err != nil || runRepository["node_id"] != id {
		return nil, errors.New("workflow run immutable repository identity differs from the checkout")
	}
	branch, err := contract.ObjectAt(r, "defaultBranchRef")
	if err != nil {
		return nil, err
	}
	defaultBranch, err := contract.Nonempty(branch, "name")
	if err != nil {
		return nil, err
	}
	repository := repo.Object()
	repository["id"], repository["defaultBranch"] = id, defaultBranch
	result := contract.Object{"repository": repository, "event": trigger, "required_checks": []any{}, "pull_request": contract.Object{}, "provenance": contract.Object{"live": true, "complete": true, "source": "github_api", "host": repo.Host}}
	prs, _ := contract.Objects(submitted, "pull_requests")
	if len(prs) != 1 {
		return result, nil
	}
	number, _ := contract.PositiveInteger(prs[0]["number"])
	prSource, err := reader.ReadPullRequest(ctx, number)
	if err != nil {
		return nil, err
	}
	prRepository, err := contract.ObjectAt(prSource, "repository")
	if err != nil || prRepository["id"] != id {
		return nil, errors.New("pull request immutable repository identity changed")
	}
	prParsed, err := contract.ParseRepository(prRepository)
	if err != nil || prParsed != repo {
		return nil, errors.New("pull request source targets another repository")
	}
	pr, err := contract.ObjectAt(prSource, "pull_request")
	if err != nil {
		return nil, err
	}
	result["pull_request"], err = NormalizeMergePullRequest(pr, repo, number)
	if err != nil {
		return nil, err
	}
	checks, err := reader.RequiredChecks(ctx, number)
	if err != nil {
		return nil, err
	}
	checkValues := []any{}
	for _, check := range checks {
		checkValues = append(checkValues, check)
	}
	result["required_checks"] = checkValues
	final, err := reader.ReadRepository(ctx)
	if err != nil {
		return nil, err
	}
	finalRepo, err := contract.ParseRepository(final)
	if err != nil || finalRepo != repo || final["id"] != id {
		return nil, errors.New("immutable repository identity changed across merge inventory reads")
	}
	finalBranch, err := contract.ObjectAt(final, "defaultBranchRef")
	if err != nil || finalBranch["name"] != defaultBranch {
		return nil, errors.New("repository default branch changed across merge inventory reads")
	}
	return contract.Clone(result)
}
