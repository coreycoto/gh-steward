package runrecovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const promotionPREndpoint = "repos/example/widgets/pulls/3"

func promotionPRState() Object {
	repo := Object{"id": int64(42), "node_id": "R_widgets", "full_name": "example/widgets", "html_url": "https://github.com/example/widgets", "private": false, "default_branch": "main", "owner": Object{"login": "example"}, "pushed_at": "2026-03-17T10:00:00Z"}
	return Object{
		"id": int64(30), "node_id": "PR_3", "number": int64(3), "html_url": "https://github.com/example/widgets/pull/3",
		"title": "Reviewed change", "body": "Closes #17", "state": "open", "labels": []any{Object{"name": "automerge"}},
		"draft": false, "merged": false, "merged_at": nil, "merge_commit_sha": nil,
		"base": Object{"ref": "main", "sha": strings.Repeat("a", 40), "repo": cloneNativeObject(repo)},
		"head": Object{"ref": "codex/change", "sha": strings.Repeat("b", 40), "repo": cloneNativeObject(repo)},
	}
}

func promotionPRFixture(t *testing.T) (*preparedLifecycle, Object, Object, Object) {
	t.Helper()
	f, b, _, legacy := promotionFixture(t)
	state := promotionPRState()
	reader := historyCutoverFixture([]Object{legacy}, nil, nil, map[string]Object{promotionPREndpoint: state})
	p, err := f.engine.PreviewHistoryPromotion(context.Background(), reader, b, nil, []string{"execution"}, []string{promotionPREndpoint})
	if err != nil {
		t.Fatal(err)
	}
	f.reader.reads[promotionPREndpoint] = cloneNativeObject(state)
	return f, b, p, legacy
}

func publishPromotionTag(state Object) {
	for _, side := range []string{"base", "head"} {
		state[side].(Object)["repo"].(Object)["pushed_at"] = "2026-03-18T10:00:00Z"
	}
}

func TestPromotionPublicationClockContractPreservesRawEvidence(t *testing.T) {
	f, _, p, legacy := promotionPRFixture(t)
	before, err := Canonical(p)
	if err != nil || !exactInt(p["schema_version"], 2) || p["state_contract"] != promotionStateContract {
		t.Fatal("capture did not bind the versioned comparison contract", err)
	}
	publishPromotionTag(f.reader.reads[promotionPREndpoint])
	admitPromotionFixture(t, f.engine, p)
	f.setAttempt(1, true)
	addPromotionLegacy(f, legacy)
	inv := f.invocation(1, t.TempDir())
	result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), f.reader, inv, p)
	if err != nil || result["outcome"] != "fresh" || result["mode"] != "fresh-native" {
		t.Fatal("tag publication blocked unchanged selected work", result, err)
	}
	after, err := Canonical(p)
	if err != nil || string(before) != string(after) || !strings.Contains(string(after), "2026-03-17T10:00:00Z") {
		t.Fatal("comparison rewrote the raw reviewed evidence", err)
	}
	chain, err := LoadJSON(filepath.Join(inv.PackageRoot, "settlement-chain.json"))
	if err != nil || !Equal(chain.(Object)["history_promotion"], p) {
		t.Fatal("native lineage dropped the exact original document", err)
	}
}

func TestPromotionSchemaOneKeepsFullResponseComparison(t *testing.T) {
	f, _, p, legacy := promotionPRFixture(t)
	p["schema_version"] = int64(1)
	delete(p, "state_contract")
	resealPromotion(t, p)
	if _, err := ValidateHistoryPromotion(p); err != nil {
		t.Fatal("old schema is no longer accepted", err)
	}
	publishPromotionTag(f.reader.reads[promotionPREndpoint])
	admitPromotionFixture(t, f.engine, p)
	f.setAttempt(1, true)
	addPromotionLegacy(f, legacy)
	result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), f.reader, f.invocation(1, t.TempDir()), p)
	if err != nil || result["outcome"] != "recovery_needed" {
		t.Fatal("old review silently inherited new comparison semantics", result, err)
	}
}

func TestPromotionClockContractStillHoldsAllOtherDrift(t *testing.T) {
	changes := map[string]func(Object){
		"PR identity":                func(s Object) { s["id"] = int64(31) },
		"PR node":                    func(s Object) { s["node_id"] = "PR_other" },
		"number":                     func(s Object) { s["number"] = int64(4) },
		"foreign URL":                func(s Object) { s["html_url"] = "https://github.com/foreign/widgets/pull/3" },
		"state":                      func(s Object) { s["state"] = "closed" },
		"draft":                      func(s Object) { s["draft"] = true },
		"merged":                     func(s Object) { s["merged"] = true },
		"merge commit":               func(s Object) { s["merge_commit_sha"] = strings.Repeat("c", 40) },
		"merged time":                func(s Object) { s["merged_at"] = "2026-03-18T10:00:00Z" },
		"title":                      func(s Object) { s["title"] = "Different change" },
		"body":                       func(s Object) { s["body"] = "Closes #99" },
		"labels":                     func(s Object) { s["labels"] = []any{} },
		"missing labels":             func(s Object) { delete(s, "labels") },
		"head SHA":                   func(s Object) { s["head"].(Object)["sha"] = strings.Repeat("c", 40) },
		"base SHA":                   func(s Object) { s["base"].(Object)["sha"] = strings.Repeat("c", 40) },
		"head ref":                   func(s Object) { s["head"].(Object)["ref"] = "different" },
		"base ref":                   func(s Object) { s["base"].(Object)["ref"] = "release" },
		"missing head":               func(s Object) { delete(s, "head") },
		"missing base repository":    func(s Object) { delete(s["base"].(Object), "repo") },
		"deleted head repository":    func(s Object) { s["head"].(Object)["repo"] = nil },
		"repository identity":        func(s Object) { s["head"].(Object)["repo"].(Object)["id"] = int64(43) },
		"repository node":            func(s Object) { s["head"].(Object)["repo"].(Object)["node_id"] = "R_other" },
		"repository privacy":         func(s Object) { s["base"].(Object)["repo"].(Object)["private"] = true },
		"repository owner":           func(s Object) { s["head"].(Object)["repo"].(Object)["owner"] = Object{"login": "foreign"} },
		"default branch":             func(s Object) { s["base"].(Object)["repo"].(Object)["default_branch"] = "release" },
		"unknown repository field":   func(s Object) { s["head"].(Object)["repo"].(Object)["future_policy"] = true },
		"root publication clock":     func(s Object) { s["pushed_at"] = "2026-03-18T10:00:00Z" },
		"missing repository clock":   func(s Object) { delete(s["head"].(Object)["repo"].(Object), "pushed_at") },
		"malformed repository clock": func(s Object) { s["head"].(Object)["repo"].(Object)["pushed_at"] = "unknown" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f, _, p, legacy := promotionPRFixture(t)
			state := f.reader.reads[promotionPREndpoint]
			publishPromotionTag(state)
			change(state)
			admitPromotionFixture(t, f.engine, p)
			f.setAttempt(1, true)
			addPromotionLegacy(f, legacy)
			inv := f.invocation(1, t.TempDir())
			result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), f.reader, inv, p)
			if err != nil || result["outcome"] != "recovery_needed" {
				t.Fatal("changed or incomplete state entered fresh work", result, err)
			}
			for _, name := range []string{"run-context.json", "settlement-chain.json", "plans", "journal"} {
				if _, err := os.Lstat(filepath.Join(inv.PackageRoot, name)); !os.IsNotExist(err) {
					t.Fatal("denied transition advanced executable state", name, err)
				}
			}
		})
	}
}

func TestPromotionComparisonContractIsPartOfTheExactReview(t *testing.T) {
	for _, name := range []string{"missing", "unknown", "schema one with contract", "changed raw clock", "new reviewed digest"} {
		t.Run(name, func(t *testing.T) {
			f, _, original, _ := promotionPRFixture(t)
			p := cloneNativeObject(original)
			switch name {
			case "missing":
				delete(p, "state_contract")
			case "unknown":
				p["state_contract"] = "ignore-all-state"
			case "schema one with contract":
				p["schema_version"] = int64(1)
			case "changed raw clock", "new reviewed digest":
				publishPromotionTag(p["state_reads"].([]any)[0].(Object)["object"].(Object))
			}
			if name != "changed raw clock" {
				resealPromotion(t, p)
			}
			admitPromotionFixture(t, f.engine, original)
			if _, err := f.engine.ValidateReviewedHistoryPromotion(p, p["target"]); err == nil {
				t.Fatal("altered document inherited the original review")
			}
		})
	}
}

type changingPromotionStateReader struct {
	*recoveryReaderFixture
	stateReads int
	change     func(Object)
}

func (r *changingPromotionStateReader) Read(ctx context.Context, endpoint string) (Object, error) {
	value, err := r.recoveryReaderFixture.Read(ctx, endpoint)
	if err != nil || endpoint != promotionPREndpoint {
		return value, err
	}
	r.stateReads++
	copyValue := cloneNativeObject(value)
	if r.stateReads == 2 {
		r.change(copyValue)
	}
	return copyValue, nil
}

func TestPromotionCaptureUsesTheSameComparisonContractAsAdmission(t *testing.T) {
	for _, name := range []string{"tag clock", "business state"} {
		t.Run(name, func(t *testing.T) {
			f, b, _, legacy := promotionPRFixture(t)
			r := &changingPromotionStateReader{recoveryReaderFixture: historyCutoverFixture([]Object{legacy}, nil, nil, map[string]Object{promotionPREndpoint: promotionPRState()}), change: publishPromotionTag}
			if name == "business state" {
				r.change = func(s Object) { s["body"] = "Changed during capture" }
			}
			p, err := f.engine.PreviewHistoryPromotion(context.Background(), r, b, nil, []string{"execution"}, []string{promotionPREndpoint})
			if name == "business state" {
				if err == nil {
					t.Fatal("capture hid business state drift")
				}
			} else if err != nil || p["state_contract"] != promotionStateContract {
				t.Fatal("tag-only change invalidated capture", err)
			}
		})
	}
}

func TestPromotionStateRepairDoesNotSettleDiagnosticOnlyAttempts(t *testing.T) {
	for _, action := range []string{"capture", "recover"} {
		t.Run(action, func(t *testing.T) {
			f, b, p, legacy := promotionPRFixture(t)
			held := recoveryHistoryRun(6, 1, "Held before initialization")
			held["conclusion"] = "failure"
			diagnostic := Object{"id": int64(601), "name": "gh-steward-recovery-diagnostic-task-run-6-attempt-1", "workflow_run": Object{"id": int64(6), "head_sha": held["head_sha"]}}
			if action == "capture" {
				r := historyCutoverFixture([]Object{legacy, held}, []Object{diagnostic}, nil, map[string]Object{promotionPREndpoint: promotionPRState()})
				if _, err := f.engine.PreviewHistoryPromotion(context.Background(), r, b, nil, []string{"execution"}, []string{promotionPREndpoint}); err == nil {
					t.Fatal("state repair absorbed the failed run into a new grant")
				}
				return
			}
			admitPromotionFixture(t, f.engine, p)
			f.setAttempt(1, true)
			addPromotionLegacy(f, legacy)
			packet := f.reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)
			packet["total_count"] = int64(3)
			packet["workflow_runs"] = append(packet["workflow_runs"].([]any), held)
			f.reader.reads[historyCutoverAttemptEndpoint(6, 1)] = held
			f.reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(1), "artifacts": []any{diagnostic}}}
			publishPromotionTag(f.reader.reads[promotionPREndpoint])
			inv := f.invocation(1, t.TempDir())
			result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), f.reader, inv, p)
			if err != nil || result["outcome"] != "recovery_needed" || result["reason"] != "earliest unsettled attempt has no unique exact recovery artifact" {
				t.Fatal("diagnostic became a terminal receipt", result, err)
			}
			for _, name := range []string{"run-context.json", "settlement-chain.json", "plans", "journal"} {
				if _, err := os.Lstat(filepath.Join(inv.PackageRoot, name)); !os.IsNotExist(err) {
					t.Fatal("diagnostic advanced execution or lineage", name, err)
				}
			}
		})
	}
}

func TestPromotionIssueReadsRemainExactAndProviderFailureHolds(t *testing.T) {
	for _, name := range []string{"body", "labels", "clock", "read failure"} {
		t.Run(name, func(t *testing.T) {
			f, _, p, legacy := promotionFixture(t)
			endpoint := "repos/example/widgets/issues/17"
			state := cloneNativeObject(f.reader.reads[endpoint])
			switch name {
			case "body":
				state["body"] = "Different intent"
			case "labels":
				state["labels"] = []any{Object{"name": "different"}}
			case "clock":
				state["pushed_at"] = "2026-03-18T10:00:00Z"
			}
			f.reader.reads[endpoint] = state
			if name == "read failure" {
				delete(f.reader.reads, endpoint)
			}
			admitPromotionFixture(t, f.engine, p)
			f.setAttempt(1, true)
			addPromotionLegacy(f, legacy)
			result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), f.reader, f.invocation(1, t.TempDir()), p)
			if err != nil || result["outcome"] != "recovery_needed" {
				t.Fatal("issue drift or failed read was ignored", result, err)
			}
		})
	}
}

func TestPromotionClockContractPreservesExplicitNullHeadRepository(t *testing.T) {
	f, b, _, legacy := promotionPRFixture(t)
	state := promotionPRState()
	state["head"].(Object)["repo"] = nil
	state["base"].(Object)["repo"].(Object)["pushed_at"] = nil
	r := historyCutoverFixture([]Object{legacy}, nil, nil, map[string]Object{promotionPREndpoint: state})
	p, err := f.engine.PreviewHistoryPromotion(context.Background(), r, b, nil, []string{"execution"}, []string{promotionPREndpoint})
	if err != nil {
		t.Fatal("explicit provider null was rejected", err)
	}
	live := cloneNativeObject(state)
	live["base"].(Object)["repo"].(Object)["pushed_at"] = "2026-03-18T10:00:00Z"
	f.reader.reads[promotionPREndpoint] = live
	admitPromotionFixture(t, f.engine, p)
	if err := f.engine.recheckPromotionState(context.Background(), f.reader, p); err != nil {
		t.Fatal("null head and valid repository publication changed selected work", err)
	}
	live["head"].(Object)["repo"] = promotionPRState()["head"].(Object)["repo"]
	if err := f.engine.recheckPromotionState(context.Background(), f.reader, p); err == nil {
		t.Fatal("recreated head repository inherited the deleted incarnation")
	}
}
