package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
	"github.com/coreycoto/gh-steward/internal/runrecovery"
)

const maxCutoverReviewComments = 10000

type cutoverReviewDecision struct {
	workflow, digest, login string
	approved                bool
	id                      int64
}

// The review channel is independent of source commits: recording an approval
// must not create another workflow attempt that the baseline silently absorbs.
// Authentication establishes authorship, not human authorization. Agents must
// obtain approval for the exact baseline before recording this statement.
func resolveHistoryCutoverReviews(ctx context.Context, reader runrecovery.ActionsReader, engine *runrecovery.Engine, repository contract.Repository, workflow string) error {
	return resolveHistoryReview(ctx, reader, engine, repository, workflow, "CUTOVER")
}

func resolveHistoryPromotionReviews(ctx context.Context, reader runrecovery.ActionsReader, engine *runrecovery.Engine, repository contract.Repository, workflow string) error {
	return resolveHistoryReview(ctx, reader, engine, repository, workflow, "PROMOTION")
}

func resolveHistoryReview(ctx context.Context, reader runrecovery.ActionsReader, engine *runrecovery.Engine, repository contract.Repository, workflow, kind string) error {
	route := engine.HistoryCutoverReviewIssues()[workflow]
	if kind == "PROMOTION" {
		route = engine.HistoryPromotionReviewIssues()[workflow]
	}
	if route == nil {
		return nil
	}
	number, err := contract.PositiveInteger(route["number"])
	if err != nil {
		return err
	}
	engine.ResetHistoryReviews(workflow, kind)
	trusted := map[string]bool{}
	logins, ok := route["trusted_logins"].([]any)
	if !ok {
		return errors.New("history cutover review route has no trusted logins")
	}
	for _, raw := range logins {
		login, ok := raw.(string)
		if !ok {
			return errors.New("history cutover review login is invalid")
		}
		trusted[login] = true
	}
	issueRoute := fmt.Sprintf("repos/%s/issues/%d", repository.FullName(), number)
	issue, err := reader.Read(ctx, issueRoute)
	if err != nil {
		return fmt.Errorf("read history cutover review issue: %w", err)
	}
	count, err := validateCutoverReviewIssue(issue, repository, number)
	if err != nil {
		return err
	}
	pages, err := reader.Pages(ctx, issueRoute+"/comments?per_page=100")
	if err != nil {
		return fmt.Errorf("read complete history cutover review comments: %w", err)
	}
	decisions, err := cutoverReviewDecisions(pages, count, trusted, repository, number, workflow, kind)
	if err != nil {
		return err
	}
	currentIssue, err := reader.Read(ctx, issueRoute)
	if err != nil {
		return err
	}
	currentCount, err := validateCutoverReviewIssue(currentIssue, repository, number)
	if err != nil || count != currentCount {
		return errors.New("history cutover review issue changed during complete inventory acquisition")
	}
	for _, decision := range decisions {
		if !decision.approved {
			continue
		}
		permission, err := reader.Read(ctx, fmt.Sprintf("repos/%s/collaborators/%s/permission", repository.FullName(), decision.login))
		if err != nil {
			return fmt.Errorf("read current history cutover reviewer permission: %w", err)
		}
		user, err := contract.ObjectAt(permission, "user")
		if err != nil || strings.ToLower(fmt.Sprint(user["login"])) != decision.login || user["type"] != "User" {
			return errors.New("history cutover reviewer permission identifies another principal")
		}
		switch permission["permission"] {
		case "write", "maintain", "admin":
			var admitErr error
			if kind == "PROMOTION" {
				admitErr = engine.AdmitHistoryPromotionReview(workflow, decision.digest)
			} else {
				admitErr = engine.AdmitHistoryCutoverReview(workflow, decision.digest)
			}
			if admitErr != nil {
				return admitErr
			}
		case "read", "none", "triage":
			// A prior statement loses authority when its author loses write access.
		default:
			return errors.New("history cutover reviewer permission is unsupported")
		}
	}
	return nil
}

func validateCutoverReviewIssue(issue contract.Object, repository contract.Repository, number int64) (int64, error) {
	id, err := contract.PositiveInteger(issue["id"])
	actual, numberErr := contract.PositiveInteger(issue["number"])
	count, countErr := contract.Integer(issue["comments"])
	url := fmt.Sprintf("%s/issues/%d", repository.URL, number)
	if err != nil || id == 0 || numberErr != nil || actual != number || countErr != nil || count < 0 || count > maxCutoverReviewComments || issue["html_url"] != url || issue["pull_request"] != nil {
		return 0, errors.New("history cutover review issue identity or comment count is invalid")
	}
	return count, nil
}

func cutoverReviewDecisions(pages []any, expected int64, trusted map[string]bool, repository contract.Repository, issue int64, workflow string, kinds ...string) ([]cutoverReviewDecision, error) {
	kind := "CUTOVER"
	if len(kinds) > 1 {
		return nil, errors.New("one exact history review kind is required")
	}
	if len(kinds) == 1 {
		kind = kinds[0]
	}
	if kind != "CUTOVER" && kind != "PROMOTION" {
		return nil, errors.New("unsupported history review kind")
	}
	if len(pages) == 0 || len(pages) > maxCutoverReviewComments/100+1 {
		return nil, errors.New("history cutover review comments require a complete bounded page inventory")
	}
	seen := map[int64]bool{}
	latest := map[string]cutoverReviewDecision{}
	for pageIndex, page := range pages {
		rows, ok := page.([]any)
		if !ok || len(rows) > 100 || (pageIndex < len(pages)-1 && len(rows) != 100) {
			return nil, errors.New("history cutover review comment pages are malformed or incomplete")
		}
		for _, raw := range rows {
			comment, ok := raw.(contract.Object)
			if !ok {
				return nil, errors.New("history cutover review comment is not an object")
			}
			id, err := contract.PositiveInteger(comment["id"])
			if err != nil || seen[id] || len(seen) >= maxCutoverReviewComments {
				return nil, errors.New("history cutover review comment IDs are invalid or repeated")
			}
			seen[id] = true
			user, err := contract.ObjectAt(comment, "user")
			body, bodyErr := contract.String(comment, "body")
			if err != nil || bodyErr != nil {
				return nil, errors.New("history cutover review comment has no author or body")
			}
			login := strings.ToLower(fmt.Sprint(user["login"]))
			if !trusted[login] || user["type"] != "User" {
				continue
			}
			line := strings.TrimSpace(strings.SplitN(body, "\n", 2)[0])
			if !strings.HasPrefix(line, "APPROVE HISTORY "+kind) && !strings.HasPrefix(line, "REVOKE HISTORY "+kind) {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) != 6 || parts[1] != "HISTORY" || parts[2] != kind || (parts[0] != "APPROVE" && parts[0] != "REVOKE") || parts[3] != fmt.Sprintf("%s#%d", repository.FullName(), issue) || !runrecovery.IsSHA256(parts[5]) {
				return nil, errors.New("trusted history cutover review statement is malformed or targets another repository or issue")
			}
			if parts[4] != workflow {
				continue
			}
			if comment["html_url"] != fmt.Sprintf("%s/issues/%d#issuecomment-%d", repository.URL, issue, id) {
				return nil, errors.New("history cutover review comment belongs to another issue")
			}
			created, createdErr := time.Parse(time.RFC3339, fmt.Sprint(comment["created_at"]))
			updated, updatedErr := time.Parse(time.RFC3339, fmt.Sprint(comment["updated_at"]))
			if createdErr != nil || updatedErr != nil || updated.Before(created) {
				return nil, errors.New("history cutover review comment timestamps are invalid")
			}
			if previous, exists := latest[login]; !exists || id > previous.id {
				latest[login] = cutoverReviewDecision{workflow: workflow, digest: parts[5], login: login, approved: parts[0] == "APPROVE", id: id}
			}
		}
	}
	if int64(len(seen)) != expected {
		return nil, errors.New("history cutover review comments do not match the complete issue inventory")
	}
	result := make([]cutoverReviewDecision, 0, len(latest))
	for _, decision := range latest {
		result = append(result, decision)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].login < result[j].login })
	return result, nil
}
