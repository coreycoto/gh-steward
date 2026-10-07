package runrecovery

import (
	"fmt"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

// Comparison semantics are sealed into each promotion. Never give an existing
// review new exclusions; its exact raw evidence and contract remain unchanged.
const (
	promotionClockStateContract = "github-rest-pr-repository-clock-v1"
	promotionStateContract      = "github-rest-pr-repository-bookkeeping-v2"
)

func supportedPromotionStateContract(value any) bool {
	name, ok := value.(string)
	return ok && (name == promotionClockStateContract || name == promotionStateContract)
}

func equalPromotionState(promotion Object, current []Object) (bool, error) {
	if exactInt(promotion["schema_version"], 1) {
		return Equal(promotion["state_reads"], objectRows(current)), nil
	}
	if (!exactInt(promotion["schema_version"], 2) && !exactInt(promotion["schema_version"], 3)) || !supportedPromotionStateContract(promotion["state_contract"]) {
		return false, recoveryError("unsupported promotion reconciliation contract")
	}
	retained, err := objectArray(promotion["state_reads"], "promotion reconciliation reads")
	if err != nil {
		return false, err
	}
	target, err := ValidateTarget(promotion["target"])
	if err != nil {
		return false, err
	}
	stateContract := promotion["state_contract"].(string)
	before, err := promotionStateProjection(retained, target, stateContract)
	if err != nil {
		return false, err
	}
	after, err := promotionStateProjection(current, target, stateContract)
	if err != nil {
		return false, err
	}
	return Equal(before, after), nil
}

func promotionStateProjection(reads []Object, target Object, stateContract string) ([]Object, error) {
	if !supportedPromotionStateContract(stateContract) {
		return nil, recoveryError("unsupported promotion reconciliation contract")
	}
	projected := make([]Object, 0, len(reads))
	previous := ""
	for _, raw := range reads {
		read, err := Exact(raw, historyCutoverStateFields, "promotion reconciliation read")
		if err != nil {
			return nil, err
		}
		endpoint, ok := read["endpoint"].(string)
		if !ok || !historyCutoverStateRoute.MatchString(endpoint) || !strings.HasPrefix(endpoint, "repos/"+fmt.Sprint(target["repository"])+"/") || endpoint <= previous {
			return nil, recoveryError("promotion reconciliation reads have invalid target identities or ordering")
		}
		previous = endpoint
		copyRead, err := cloneObject(read)
		if err != nil {
			return nil, err
		}
		state, err := object(copyRead["object"], "promotion state object")
		parts := strings.Split(endpoint, "/")
		number, numberErr := positiveInteger(state["number"], "promotion selected number")
		if err != nil || numberErr != nil || fmt.Sprint(number) != parts[4] {
			return nil, recoveryError("promotion state object differs from its selected endpoint")
		}
		if parts[3] == "pulls" {
			if _, err := positiveInteger(state["id"], "promotion PR identity"); err != nil || !nonemptyString(state["node_id"]) || state["html_url"] != fmt.Sprintf("%s/%s/pull/%d", target["server_url"], target["repository"], number) {
				return nil, recoveryError("promotion PR has no exact immutable identity")
			}
			for _, field := range []string{"state", "title", "body", "labels", "draft", "merged", "merged_at", "merge_commit_sha"} {
				if _, exists := state[field]; !exists {
					return nil, recoveryError("promotion PR reconciliation response is incomplete")
				}
			}
			for _, side := range []string{"base", "head"} {
				ref, err := object(state[side], "promotion PR ref")
				if err != nil || !nonemptyString(ref["ref"]) || !settlementSHA40.MatchString(fmt.Sprint(ref["sha"])) {
					return nil, recoveryError("promotion PR ref identity is incomplete")
				}
				repoValue, exists := ref["repo"]
				if !exists || (side == "base" && repoValue == nil) {
					return nil, recoveryError("promotion PR repository evidence is missing")
				}
				// A deleted head repository remains an explicit, exactly compared null.
				if repoValue == nil {
					continue
				}
				repository, err := object(repoValue, "promotion PR repository")
				if err != nil {
					return nil, err
				}
				if _, err := positiveInteger(repository["id"], "promotion repository identity"); err != nil || !nonemptyString(repository["node_id"]) || !historyCutoverStateRoute.MatchString("repos/"+fmt.Sprint(repository["full_name"])+"/pulls/1") || repository["html_url"] != fmt.Sprintf("%s/%s", target["server_url"], repository["full_name"]) || (side == "base" && repository["full_name"] != target["repository"]) {
					return nil, recoveryError("promotion PR repository has an invalid immutable identity")
				}
				if err := projectPromotionRepository(repository, stateContract); err != nil {
					return nil, err
				}
			}
		}
		projected = append(projected, copyRead)
	}
	return projected, nil
}

func projectPromotionRepository(repository Object, stateContract string) error {
	clocks := []string{"pushed_at"}
	if stateContract == promotionStateContract {
		if err := validatePromotionRepositorySettings(repository); err != nil {
			return err
		}
		clocks = append(clocks, "updated_at")
		// Only these derived numeric statistics are outside admission state.
		// Everything else, including unknown fields, remains in the projection.
		for _, field := range []string{"size", "stargazers_count", "watchers_count", "watchers", "forks_count", "forks", "open_issues_count", "open_issues", "network_count", "subscribers_count"} {
			value, exists := repository[field]
			if !exists && field != "size" {
				continue
			}
			if count, err := contract.Integer(value); !exists || err != nil || count < 0 {
				return recoveryError("promotion PR repository statistic %s is missing or malformed", field)
			}
			delete(repository, field)
		}
	}
	for _, field := range clocks {
		clock, exists := repository[field]
		if !exists {
			return recoveryError("promotion PR repository clock %s is missing", field)
		}
		if clock != nil {
			text, ok := clock.(string)
			if _, err := time.Parse(time.RFC3339, text); !ok || err != nil {
				return recoveryError("promotion PR repository clock %s is malformed", field)
			}
		}
		delete(repository, field)
	}
	return nil
}

func validatePromotionRepositorySettings(repository Object) error {
	fullName, ok := repository["full_name"].(string)
	parts := strings.Split(fullName, "/")
	if !ok || len(parts) != 2 || repository["name"] != parts[1] || !nonemptyString(repository["default_branch"]) {
		return recoveryError("promotion PR repository name or default branch is incomplete")
	}
	owner, err := object(repository["owner"], "promotion PR repository owner")
	if err != nil {
		return err
	}
	if _, err := positiveInteger(owner["id"], "promotion repository owner identity"); err != nil || !nonemptyString(owner["node_id"]) || owner["login"] != parts[0] || !nonemptyString(owner["type"]) {
		return recoveryError("promotion PR repository owner identity is incomplete")
	}
	for _, field := range []string{"private", "fork", "archived", "disabled"} {
		if _, ok := repository[field].(bool); !ok {
			return recoveryError("promotion PR repository setting %s is missing or malformed", field)
		}
	}
	visibility, ok := repository["visibility"].(string)
	if !ok || (visibility != "public" && visibility != "private" && visibility != "internal") || (visibility == "public") == repository["private"].(bool) {
		return recoveryError("promotion PR repository visibility is missing or inconsistent")
	}
	return nil
}
