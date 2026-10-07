package runrecovery

import (
	"fmt"
	"strings"
	"time"
)

// The contract is sealed into schema-2 promotions. Only the publication clock
// at the two named PR repository paths is outside reconciliation state. Raw
// responses remain in the reviewed document; every other field stays exact.
const promotionStateContract = "github-rest-pr-repository-clock-v1"

func equalPromotionState(promotion Object, current []Object) (bool, error) {
	if exactInt(promotion["schema_version"], 1) {
		return Equal(promotion["state_reads"], objectRows(current)), nil
	}
	if (!exactInt(promotion["schema_version"], 2) && !exactInt(promotion["schema_version"], 3)) || promotion["state_contract"] != promotionStateContract {
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
	before, err := promotionStateProjection(retained, target)
	if err != nil {
		return false, err
	}
	after, err := promotionStateProjection(current, target)
	if err != nil {
		return false, err
	}
	return Equal(before, after), nil
}

func promotionStateProjection(reads []Object, target Object) ([]Object, error) {
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
				clock, exists := repository["pushed_at"]
				if !exists {
					return nil, recoveryError("promotion PR repository publication clock is missing")
				}
				if clock != nil {
					text, ok := clock.(string)
					if _, err := time.Parse(time.RFC3339, text); !ok || err != nil {
						return nil, recoveryError("promotion PR repository publication clock is malformed")
					}
				}
				delete(repository, "pushed_at")
			}
		}
		projected = append(projected, copyRead)
	}
	return projected, nil
}
