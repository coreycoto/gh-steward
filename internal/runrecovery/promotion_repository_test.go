package runrecovery

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

func changePromotionRepositoryBookkeeping(state Object) {
	for _, side := range []string{"base", "head"} {
		repo := state[side].(Object)["repo"].(Object)
		repo["pushed_at"], repo["updated_at"] = "2026-03-18T10:00:00Z", "2026-03-18T10:01:00Z"
		repo["size"] = int64(4111)
		repo["stargazers_count"], repo["watchers_count"], repo["watchers"] = int64(11), int64(11), int64(11)
		repo["forks_count"], repo["forks"], repo["network_count"] = int64(3), int64(3), int64(3)
		repo["open_issues_count"], repo["open_issues"], repo["subscribers_count"] = int64(4), int64(4), int64(2)
	}
}

func repositoryPromotionFixture(t *testing.T, withHold bool) (*preparedLifecycle, Object, ActionsReader, Object, Object, Object) {
	t.Helper()
	if !withHold {
		f, b, p, legacy := promotionPRFixture(t)
		return f, b, f.reader, p, legacy, nil
	}
	f, b, reader, legacy, held := promotionHeldFixture(t, true)
	reader.reads[promotionPREndpoint] = promotionPRState()
	p, err := f.engine.PreviewHistoryPromotion(context.Background(), reader, b, nil, []string{"execution"}, []string{promotionPREndpoint}, 7)
	if err != nil {
		t.Fatal(err)
	}
	return f, b, reader, p, legacy, held
}

func prepareRepositoryPromotionAttempt(t *testing.T, f *preparedLifecycle, reader ActionsReader, legacy, held Object) {
	t.Helper()
	f.setAttempt(1, true)
	if held == nil {
		addPromotionLegacy(f, legacy)
	} else {
		addHeldHistoryToCurrent(f, reader.(*heldReaderFixture), legacy, held)
	}
}

func TestRepositoryBookkeepingContractPreservesRawReviewAndHeldLineage(t *testing.T) {
	for _, withHold := range []bool{false, true} {
		t.Run(fmt.Sprint(withHold), func(t *testing.T) {
			f, baseline, reader, p, legacy, held := repositoryPromotionFixture(t, withHold)
			original, err := Canonical(p)
			if err != nil || p["state_contract"] != promotionStateContract {
				t.Fatal("new capture did not seal its contract", err)
			}
			changePromotionRepositoryBookkeeping(f.reader.reads[promotionPREndpoint])
			admitPromotionFixture(t, f.engine, p)
			prepareRepositoryPromotionAttempt(t, f, reader, legacy, held)
			inv := f.invocation(1, t.TempDir())
			result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), reader, inv, p)
			if err != nil || result["outcome"] != "fresh" || result["mode"] != "fresh-native" {
				t.Fatal("derived repository metadata blocked unchanged work", result, err)
			}
			after, err := Canonical(p)
			if err != nil || string(original) != string(after) {
				t.Fatal("projection changed sealed evidence", err)
			}
			value, err := LoadJSON(filepath.Join(inv.PackageRoot, "settlement-chain.json"))
			if err != nil {
				t.Fatal(err)
			}
			chain := value.(Object)
			if !Equal(chain["history_promotion"], p) || !Equal(p["preview_checkpoint"].(Object)["history_cutover"], baseline) || len(chain["inventory"].([]any)) != 0 || len(chain["settlements"].([]any)) != 0 || len(chain["prepared_frontier"].([]any)) != 0 {
				t.Fatal("metadata repair rewrote quarantine or fabricated execution", chain)
			}
		})
	}
}

func TestExistingClockContractsNeverInheritStatisticExclusions(t *testing.T) {
	for _, withHold := range []bool{false, true} {
		for _, change := range []string{"clock", "size", "updated clock", "counter"} {
			t.Run(fmt.Sprintf("held=%t/%s", withHold, change), func(t *testing.T) {
				f, _, reader, p, legacy, held := repositoryPromotionFixture(t, withHold)
				p["state_contract"] = promotionClockStateContract
				resealPromotion(t, p)
				if _, err := ValidateHistoryPromotion(p); err != nil {
					t.Fatal("old schema-2/3 contract lost compatibility", err)
				}
				live := f.reader.reads[promotionPREndpoint]
				publishPromotionTag(live)
				repo := live["base"].(Object)["repo"].(Object)
				switch change {
				case "size":
					repo["size"] = int64(4111)
				case "updated clock":
					repo["updated_at"] = "2026-03-18T10:01:00Z"
				case "counter":
					repo["stargazers_count"] = int64(11)
				}
				admitPromotionFixture(t, f.engine, p)
				prepareRepositoryPromotionAttempt(t, f, reader, legacy, held)
				inv := f.invocation(1, t.TempDir())
				result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), reader, inv, p)
				want := "fresh"
				if change != "clock" {
					want = "recovery_needed"
				}
				if err != nil || result["outcome"] != want {
					t.Fatal("old review inherited different comparison semantics", result, err)
				}
			})
		}
	}
}

func TestExistingClockContractDoesNotRequireNewRepositoryEvidence(t *testing.T) {
	_, _, p, _ := promotionPRFixture(t)
	p["state_contract"] = promotionClockStateContract
	state := p["state_reads"].([]any)[0].(Object)["object"].(Object)
	for _, side := range []string{"base", "head"} {
		repo := state[side].(Object)["repo"].(Object)
		for _, field := range []string{"name", "owner", "private", "visibility", "fork", "archived", "disabled", "default_branch", "updated_at", "size"} {
			delete(repo, field)
		}
	}
	resealPromotion(t, p)
	if _, err := ValidateHistoryPromotion(p); err != nil {
		t.Fatal("existing review acquired new evidence requirements", err)
	}
	live := clonePromotionObject(t, state)
	publishPromotionTag(live)
	if equal, err := equalPromotionState(p, []Object{{"endpoint": promotionPREndpoint, "object": live}}); err != nil || !equal {
		t.Fatal("existing clock-only comparison changed", equal, err)
	}
}

func TestRepositoryBookkeepingAllowsOptionalStatisticsAndNullClocks(t *testing.T) {
	f, _, p, _ := promotionPRFixture(t)
	live := f.reader.reads[promotionPREndpoint]
	for _, side := range []string{"base", "head"} {
		repo := live[side].(Object)["repo"].(Object)
		repo["pushed_at"], repo["updated_at"], repo["size"] = nil, nil, int64(0)
		for _, field := range []string{"stargazers_count", "watchers_count", "watchers", "forks_count", "forks", "open_issues_count", "open_issues", "network_count", "subscribers_count"} {
			delete(repo, field)
		}
	}
	if err := f.engine.recheckPromotionState(context.Background(), f.reader, p); err != nil {
		t.Fatal("documented optional evidence caused false drift", err)
	}
}

func TestRepositoryProjectionRejectsMalformedEvidenceAtCaptureAndValidation(t *testing.T) {
	f, baseline, original, legacy := promotionPRFixture(t)
	changes := map[string]func(Object){
		"missing size":     func(r Object) { delete(r, "size") },
		"missing clock":    func(r Object) { delete(r, "updated_at") },
		"malformed clock":  func(r Object) { r["updated_at"] = "not-a-time" },
		"missing name":     func(r Object) { delete(r, "name") },
		"missing owner":    func(r Object) { delete(r, "owner") },
		"missing owner id": func(r Object) { delete(r["owner"].(Object), "id") },
		"foreign owner":    func(r Object) { r["owner"].(Object)["login"] = "foreign" },
		"bad visibility":   func(r Object) { r["visibility"] = "unknown" },
		"wrong privacy":    func(r Object) { r["private"] = true },
	}
	for _, field := range []string{"private", "fork", "archived", "disabled"} {
		changes["missing "+field] = func(r Object) { delete(r, field) }
		changes["malformed "+field] = func(r Object) { r[field] = "false" }
	}
	for _, field := range []string{"size", "stargazers_count", "watchers_count", "watchers", "forks_count", "forks", "open_issues_count", "open_issues", "network_count", "subscribers_count"} {
		for name, invalid := range map[string]any{"negative": int64(-1), "null": nil, "string": "1", "boolean": true, "fractional": json.Number("1.5"), "overflow": json.Number("9223372036854775808")} {
			changes[field+"/"+name] = func(r Object) { r[field] = invalid }
		}
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			p := clonePromotionObject(t, original)
			repo := p["state_reads"].([]any)[0].(Object)["object"].(Object)["head"].(Object)["repo"].(Object)
			change(repo)
			resealPromotion(t, p)
			if _, err := ValidateHistoryPromotion(p); err == nil {
				t.Fatal("malformed metadata entered a reviewable document")
			}
			reads, _ := objectArray(p["state_reads"], "test reads")
			if equal, err := equalPromotionState(p, reads); err == nil || equal {
				t.Fatal("malformed identical observations were accepted")
			}
		})
	}
	// Live capture also validates these fields instead of sealing incomplete reads.
	state := promotionPRState()
	delete(state["head"].(Object)["repo"].(Object), "size")
	reader := historyCutoverFixture([]Object{legacy}, nil, nil, map[string]Object{promotionPREndpoint: state})
	if _, err := f.engine.PreviewHistoryPromotion(context.Background(), reader, baseline, nil, []string{"execution"}, []string{promotionPREndpoint}); err == nil {
		t.Fatal("live capture admitted a partial repository")
	}
}
