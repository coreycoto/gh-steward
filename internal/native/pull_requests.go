package native

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

var commitOID = regexp.MustCompile(`^[0-9a-f]{40}$`)

func CommitOID(value any) (string, error) {
	text, ok := value.(string)
	if !ok || !commitOID.MatchString(text) {
		return "", errors.New("commit identity requires a lowercase 40-character SHA")
	}
	return text, nil
}

func (t *Transport) ValidatePullRequestURL(raw any, number int64) error {
	text, ok := raw.(string)
	if !ok || number < 1 {
		return errors.New("pull request requires an exact qualified URL and number")
	}
	u, err := url.Parse(text)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || !strings.EqualFold(u.Hostname(), t.Repository.Host) || !strings.EqualFold(u.EscapedPath(), fmt.Sprintf("/%s/pull/%d", t.Repository.FullName(), number)) {
		return errors.New("pull request URL differs from the selected repository and number")
	}
	return nil
}

// ReadPullRequest retains the immutable repository/node identity and exact
// head alongside all values used by the deterministic merge gate. An
// incomplete nested label connection fails rather than authorizing from a
// truncated list.
func (t *Transport) ReadPullRequest(ctx context.Context, number int64) (contract.Object, error) {
	if number < 1 {
		return nil, errors.New("pull request number must be positive")
	}
	variables := t.repoVariables()
	variables["number"] = number
	data, err := t.graphQL(ctx, `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){`+repositoryFields+` pullRequest(number:$number){id number url title body state isDraft baseRefName headRefName headRefOid mergeStateStatus reviewDecision merged mergedAt mergeCommit{oid} author{login __typename} headRepository{id nameWithOwner url} labels(first:100){nodes{name}pageInfo{hasNextPage}}}}}`, variables)
	if err != nil {
		return nil, err
	}
	r, err := t.repositoryResult(data)
	if err != nil {
		return nil, err
	}
	pr, err := contract.ObjectAt(r, "pullRequest")
	if err != nil {
		return nil, errors.New("selected pull request is unavailable")
	}
	n, err := contract.PositiveInteger(pr["number"])
	if err != nil || n != number {
		return nil, errors.New("pull request number differs from the selected candidate")
	}
	if err := t.ValidatePullRequestURL(pr["url"], number); err != nil {
		return nil, err
	}
	if _, err := contract.Nonempty(pr, "id"); err != nil {
		return nil, err
	}
	return contract.Object{"repository": r, "pull_request": pr}, nil
}

// RequiredChecks accepts gh's documented pending status and its failed-check
// exit only when a complete, valid JSON collection is present. Transport and
// malformed-source failures are never an empty green check set.
func (t *Transport) RequiredChecks(ctx context.Context, number int64) ([]contract.Object, error) {
	if number < 1 {
		return nil, errors.New("pull request number must be positive")
	}
	result, err := t.run(ctx, []string{"pr", "checks", fmt.Sprint(number), "--required", "--json", "name,bucket,state,link,workflow"}, nil, false)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 && result.ExitCode != 1 && result.ExitCode != 8 {
		return nil, fmt.Errorf("required-check read failed with exit code %d", result.ExitCode)
	}
	raw, err := contract.Decode(bytes.NewReader(append(append([]byte(`{"checks":`), result.Stdout...), '}')))
	if err != nil {
		return nil, errors.New("required-check read has no complete JSON collection")
	}
	checks, err := contract.Objects(raw, "checks")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, check := range checks {
		for _, key := range []string{"name", "bucket", "state"} {
			if _, err := contract.Nonempty(check, key); err != nil {
				return nil, err
			}
		}
		for _, key := range []string{"link", "workflow"} {
			if _, err := contract.String(check, key); err != nil {
				return nil, err
			}
		}
		switch check["bucket"] {
		case "pass", "fail", "pending", "skipping", "cancel":
		default:
			return nil, errors.New("required check has an unknown bucket")
		}
		identity, _ := contract.Digest(contract.Object{"name": check["name"], "workflow": check["workflow"], "link": check["link"]})
		if seen[identity] {
			return nil, errors.New("required-check inventory has duplicate identity")
		}
		seen[identity] = true
	}
	if result.ExitCode != 0 && len(checks) == 0 {
		return nil, errors.New("failed or pending required-check read has no evidence")
	}
	nonpass, pending := false, false
	for _, check := range checks {
		nonpass = nonpass || check["bucket"] != "pass"
		pending = pending || check["bucket"] == "pending"
	}
	if (result.ExitCode == 1 && !nonpass) || (result.ExitCode == 8 && !pending) {
		return nil, errors.New("required-check JSON disagrees with native command status")
	}
	sort.Slice(checks, func(i, j int) bool {
		a, _ := contract.Canonical(checks[i])
		b, _ := contract.Canonical(checks[j])
		return bytes.Compare(a, b) < 0
	})
	return checks, nil
}

func (t *Transport) ReadWorkflowRun(ctx context.Context, runID int64) (contract.Object, error) {
	if runID < 1 {
		return nil, errors.New("workflow run identity must be positive")
	}
	run, err := t.REST(ctx, "GET", fmt.Sprintf("repos/%s/actions/runs/%d", t.Repository.FullName(), runID), nil)
	if err != nil {
		return nil, err
	}
	actual, err := contract.PositiveInteger(run["id"])
	if err != nil || actual != runID {
		return nil, errors.New("workflow run identity differs from the selected trigger")
	}
	r, err := contract.ObjectAt(run, "repository")
	if err != nil {
		return nil, err
	}
	parsed, err := contract.ParseRepository(contract.Object{"nameWithOwner": r["full_name"], "url": r["html_url"]})
	if err != nil || parsed != t.Repository {
		return nil, errors.New("workflow run belongs to another repository")
	}
	if _, err := contract.Nonempty(r, "node_id"); err != nil {
		return nil, errors.New("workflow run repository immutable identity is unavailable")
	}
	for _, key := range []string{"name", "event", "status", "head_branch"} {
		if _, err := contract.Nonempty(run, key); err != nil {
			return nil, err
		}
	}
	if _, err := CommitOID(run["head_sha"]); err != nil {
		return nil, err
	}
	if _, err := contract.Objects(run, "pull_requests"); err != nil {
		return nil, err
	}
	return run, nil
}

// MergePullRequest is the only native merge dispatch. GitHub atomically checks
// the reviewed head SHA; the response must positively identify a merge commit.
// There is no retry, auto-merge enqueue or asynchronous acceptance fallback.
func (t *Transport) MergePullRequest(ctx context.Context, number int64, headSHA, method string) (contract.Object, error) {
	if number < 1 {
		return nil, errors.New("pull request number must be positive")
	}
	if _, err := CommitOID(headSHA); err != nil {
		return nil, err
	}
	if method != "merge" && method != "squash" && method != "rebase" {
		return nil, errors.New("merge method must be merge, squash or rebase")
	}
	ack, err := t.rest(ctx, "PUT", fmt.Sprintf("repos/%s/pulls/%d/merge", t.Repository.FullName(), number), contract.Object{"sha": headSHA, "merge_method": method})
	if err != nil {
		return nil, err
	}
	if ack["merged"] != true {
		return nil, errors.New("native merge did not acknowledge a completed merge")
	}
	if _, err := CommitOID(ack["sha"]); err != nil {
		return nil, err
	}
	return ack, nil
}
