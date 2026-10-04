package snapshot

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

type linkPolicy struct {
	marker      string
	pattern     *regexp.Regexp
	numberIndex int
}

func parseLinkPolicy(raw contract.Object) (linkPolicy, error) {
	marker, err := contract.Nonempty(raw, "marker_prefix")
	if err != nil {
		return linkPolicy{}, err
	}
	pattern, err := contract.Nonempty(raw, "pr_number_pattern")
	if err != nil {
		return linkPolicy{}, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return linkPolicy{}, err
	}
	index := re.SubexpIndex("number")
	if index < 1 {
		return linkPolicy{}, errors.New("linked PR pattern requires a named number group")
	}
	return linkPolicy{marker: marker, pattern: re, numberIndex: index}, nil
}

func commentIdentity(raw contract.Object, repo contract.Repository) (contract.Object, error) {
	id, err := contract.PositiveInteger(raw["id"])
	if err != nil {
		return nil, err
	}
	body, err := contract.String(raw, "body")
	if err != nil {
		return nil, err
	}
	issue, err := contract.Nonempty(raw, "issue_url")
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(issue)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) < 2 {
		return nil, errors.New("comment issue identity missing")
	}
	number, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil || number < 1 {
		return nil, errors.New("comment issue number invalid")
	}
	if err := issueURL(issue, repo, number, true); err != nil {
		return nil, err
	}
	web, err := contract.Nonempty(raw, "html_url")
	if err != nil {
		return nil, err
	}
	w, err := url.Parse(web)
	if err != nil || w.Fragment != "issuecomment-"+strconv.FormatInt(id, 10) {
		return nil, errors.New("comment URL has another identity")
	}
	// Repository issue-comment listing also includes pull-request comments.
	// Both forms must stay in the selected repository and match the REST issue.
	w.Fragment = ""
	base := fmt.Sprintf("https://%s/%s", repo.Host, repo.FullName())
	if w.User != nil || w.RawQuery != "" || (w.String() != fmt.Sprintf("%s/issues/%d", base, number) && w.String() != fmt.Sprintf("%s/pull/%d", base, number)) {
		return nil, errors.New("comment URL targets another repository or number")
	}
	return contract.Object{"id": id, "issue_number": number, "body": body, "url": web}, nil
}

// NormalizePullRequest deliberately validates the base repository. A fork is
// an allowed head source, and a deleted head repository may be null.
func NormalizePullRequest(raw contract.Object, repo contract.Repository) (contract.Object, error) {
	number, err := contract.PositiveInteger(raw["number"])
	if err != nil {
		return nil, err
	}
	id, err := contract.Nonempty(raw, "node_id")
	if err != nil {
		return nil, err
	}
	web, err := contract.Nonempty(raw, "html_url")
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(web)
	if err != nil || u.User != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), repo.Host) || (u.Port() != "" && u.Port() != "443") || u.RawQuery != "" || u.Fragment != "" || !strings.EqualFold(u.EscapedPath(), fmt.Sprintf("/%s/pull/%d", repo.FullName(), number)) {
		return nil, errors.New("pull request URL has another repository or number")
	}
	title, err := contract.String(raw, "title")
	if err != nil {
		return nil, err
	}
	body, exists := raw["body"]
	if !exists {
		return nil, errors.New("pull request nullable body missing")
	}
	if body == nil {
		body = ""
	}
	if _, ok := body.(string); !ok {
		return nil, errors.New("pull request body must be string or null")
	}
	state, err := contract.Nonempty(raw, "state")
	if err != nil || (state != "open" && state != "closed") {
		return nil, errors.New("pull request state invalid")
	}
	draft, err := contract.Bool(raw, "draft")
	if err != nil {
		return nil, err
	}
	mergedAt, exists := raw["merged_at"]
	if !exists {
		return nil, errors.New("pull request nullable merge time missing")
	}
	isMerged := mergedAt != nil
	if isMerged {
		stamp, ok := mergedAt.(string)
		if !ok {
			return nil, errors.New("pull request merge time invalid")
		}
		if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil || state != "closed" {
			return nil, errors.New("pull request merge evidence conflicts with state")
		}
	}
	head, err := contract.ObjectAt(raw, "head")
	if err != nil {
		return nil, err
	}
	base, err := contract.ObjectAt(raw, "base")
	if err != nil {
		return nil, err
	}
	headBranch, err := contract.Nonempty(head, "ref")
	if err != nil {
		return nil, err
	}
	baseBranch, err := contract.Nonempty(base, "ref")
	if err != nil {
		return nil, err
	}
	sha, err := contract.Nonempty(head, "sha")
	if err != nil || !regexp.MustCompile(`^[a-fA-F0-9]{40}$`).MatchString(sha) {
		return nil, errors.New("pull request head revision invalid")
	}
	baseRepo, err := contract.ObjectAt(base, "repo")
	if err != nil {
		return nil, err
	}
	full, err := contract.Nonempty(baseRepo, "full_name")
	if err != nil || !strings.EqualFold(full, repo.FullName()) {
		return nil, errors.New("pull request base repository identity changed")
	}
	baseURL, err := contract.Nonempty(baseRepo, "html_url")
	if err != nil {
		return nil, err
	}
	parsed, err := contract.ParseRepository(contract.Object{"nameWithOwner": full, "url": baseURL})
	if err != nil || parsed != repo {
		return nil, errors.New("pull request base repository URL changed")
	}
	var headOwner any
	if headRepo, exists := head["repo"]; !exists {
		return nil, errors.New("pull request nullable head repository missing")
	} else if headRepo != nil {
		r, ok := headRepo.(map[string]any)
		if !ok {
			return nil, errors.New("pull request head repository invalid")
		}
		owner, err := contract.ObjectAt(r, "owner")
		if err != nil {
			return nil, err
		}
		headOwner, err = contract.Nonempty(owner, "login")
		if err != nil {
			return nil, err
		}
	}
	publicState := strings.ToUpper(state)
	if isMerged {
		publicState = "MERGED"
	}
	return contract.Object{"id": id, "number": number, "title": title, "url": web, "body": body, "state": publicState, "is_draft": draft, "is_merged": isMerged, "merged_at": mergedAt, "head_branch": headBranch, "base_branch": baseBranch, "head_sha": strings.ToLower(sha), "head_repository_owner": headOwner}, nil
}

// LinkedPullRequests uses a complete repository comment inventory and explicit
// consumer marker policy. It never treats a failed live read as an absent link.
// The retained comment and PR inventories are reviewed preconditions, so a
// later change to the linkage cannot silently authorize an archive.
func (s Service) LinkedPullRequests(ctx context.Context, issueNumbers []int64, policy contract.Object) (contract.Object, error) {
	p, err := parseLinkPolicy(policy)
	if err != nil {
		return nil, err
	}
	selected := map[int64]bool{}
	for _, n := range issueNumbers {
		if n < 1 || selected[n] {
			return nil, errors.New("linked PR selection must contain unique positive issue numbers")
		}
		selected[n] = true
	}
	commentPages, err := s.Reader.RESTPages(ctx, "repos/"+s.Repository.FullName()+"/issues/comments?per_page=100")
	if err != nil {
		return nil, err
	}
	rawComments, err := arrayObjects(commentPages)
	if err != nil {
		return nil, err
	}
	comments := []any{}
	seenIDs := map[int64]bool{}
	latest := map[int64]contract.Object{}
	prByIssue := map[int64]int64{}
	for _, raw := range rawComments {
		c, err := commentIdentity(raw, s.Repository)
		if err != nil {
			return nil, err
		}
		id, _ := contract.PositiveInteger(c["id"])
		if seenIDs[id] {
			return nil, errors.New("duplicate issue comment identity during pagination")
		}
		seenIDs[id] = true
		n, _ := contract.PositiveInteger(c["issue_number"])
		if !selected[n] {
			continue
		}
		comments = append(comments, c)
		body := c["body"].(string)
		if !strings.Contains(body, p.marker) {
			continue
		}
		matches := p.pattern.FindAllStringSubmatch(body, -1)
		if len(matches) == 0 {
			return nil, errors.New("linked PR marker has no qualified pull request number")
		}
		var prNumber int64
		for _, match := range matches {
			v, err := strconv.ParseInt(match[p.numberIndex], 10, 64)
			if err != nil || v < 1 {
				return nil, errors.New("linked PR marker number is invalid")
			}
			if prNumber != 0 && prNumber != v {
				return nil, errors.New("linked PR marker contains ambiguous pull request numbers")
			}
			prNumber = v
		}
		prior := int64(0)
		if existing := latest[n]; existing != nil {
			prior, _ = contract.PositiveInteger(existing["id"])
		}
		if id > prior {
			latest[n] = c
			prByIssue[n] = prNumber
		}
	}
	sort.Slice(comments, func(i, j int) bool {
		a, _ := contract.PositiveInteger(comments[i].(map[string]any)["id"])
		b, _ := contract.PositiveInteger(comments[j].(map[string]any)["id"])
		return a < b
	})
	prs := []any{}
	byNumber := map[int64]contract.Object{}
	prSource := "not_requested"
	if len(prByIssue) > 0 {
		prPages, err := s.Reader.RESTPages(ctx, "repos/"+s.Repository.FullName()+"/pulls?state=all&per_page=100")
		if err != nil {
			return nil, err
		}
		rawPRs, err := arrayObjects(prPages)
		if err != nil {
			return nil, err
		}
		prSource = "github_api"
		wanted := map[int64]bool{}
		for _, n := range prByIssue {
			wanted[n] = true
		}
		seen := map[int64]bool{}
		for _, raw := range rawPRs {
			pr, err := NormalizePullRequest(raw, s.Repository)
			if err != nil {
				return nil, err
			}
			n, _ := contract.PositiveInteger(pr["number"])
			if seen[n] {
				return nil, errors.New("duplicate pull request identity during pagination")
			}
			seen[n] = true
			if wanted[n] {
				byNumber[n] = pr
				prs = append(prs, pr)
			}
		}
		sort.Slice(prs, func(i, j int) bool {
			a, _ := contract.PositiveInteger(prs[i].(map[string]any)["number"])
			b, _ := contract.PositiveInteger(prs[j].(map[string]any)["number"])
			return a < b
		})
	}
	byIssue := contract.Object{}
	requested := []int64{}
	for issue := range selected {
		requested = append(requested, issue)
	}
	sort.Slice(requested, func(i, j int) bool { return requested[i] < requested[j] })
	issueNumbersJSON := []any{}
	for _, issue := range requested {
		byIssue[strconv.FormatInt(issue, 10)] = nil
		issueNumbersJSON = append(issueNumbersJSON, issue)
	}
	unresolved := []any{}
	for issue, pr := range prByIssue {
		if row, ok := byNumber[pr]; ok {
			byIssue[strconv.FormatInt(issue, 10)] = row
		} else {
			unresolved = append(unresolved, contract.Object{"issue_number": issue, "pull_request_number": pr})
		}
	}
	sort.Slice(unresolved, func(i, j int) bool {
		a, _ := contract.PositiveInteger(unresolved[i].(map[string]any)["issue_number"])
		b, _ := contract.PositiveInteger(unresolved[j].(map[string]any)["issue_number"])
		return a < b
	})
	return contract.Object{"repo": s.Repository.Object(), "issue_numbers": issueNumbersJSON, "by_issue": byIssue, "comments": comments, "pull_requests": prs, "unresolved_links": unresolved, "generated_at": s.stamp(), "provenance": contract.Object{"live": true, "complete": true, "comments_source": "github_api", "pull_requests_source": prSource}}, nil
}
