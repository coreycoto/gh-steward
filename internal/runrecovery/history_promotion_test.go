package runrecovery

import (
	"context"
	"fmt"
	"github.com/coreycoto/gh-steward/internal/apply"
	"github.com/coreycoto/gh-steward/internal/contract"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func promotionFixture(t *testing.T) (*preparedLifecycle, Object, Object, Object) {
	t.Helper()
	f := newPreparedLifecycle(t, 1)
	legacy := recoveryHistoryRun(5, 1, "Quarantined")
	state := Object{"number": int64(17), "state": "open", "body": "reviewed live state"}
	reader := historyCutoverFixture([]Object{legacy}, nil, map[string]Object{historyCutoverAttemptEndpoint(5, 1): historyCutoverAttemptResponse(legacy, 1, legacy["created_at"].(string))}, map[string]Object{"repos/example/widgets/issues/17": state})
	baseline := captureHistoryCutoverFixture(t, reader)
	policyValue, err := DecodeValue(f.policyBytes)
	if err != nil {
		t.Fatal(err)
	}
	policy := policyValue.(Object)
	wf := policy["workflows"].(Object)["task.yml"].(Object)
	wf["history_cutover_reviews"] = []any{baseline["sha256"]}
	wf["history_promotion_review_issue"] = Object{"number": int64(18), "trusted_logins": []any{"maintainer"}}
	// The policy may contain other native work; promotion selects only execution.
	wf["plans"].(Object)["other-execution"] = cloneNativeObject(wf["plans"].(Object)["execution"].(Object))
	f.policyBytes, err = Canonical(policy)
	if err != nil {
		t.Fatal(err)
	}
	f.engine = f.newProcessEngine(t)
	promotion, err := f.engine.PreviewHistoryPromotion(context.Background(), reader, baseline, nil, []string{"execution"}, []string{"repos/example/widgets/issues/17"})
	if err != nil {
		t.Fatal(err)
	}
	f.reader.reads["repos/example/widgets/issues/17"] = state
	return f, baseline, promotion, legacy
}

func admitPromotionFixture(t *testing.T, e *Engine, p Object) {
	t.Helper()
	if err := e.AdmitHistoryPromotionReview("task.yml", fmt.Sprint(p["sha256"])); err != nil {
		t.Fatal(err)
	}
}

func addPromotionLegacy(f *preparedLifecycle, legacy Object) {
	packet := f.reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)
	packet["total_count"] = int64(2)
	packet["workflow_runs"] = append([]any{legacy}, packet["workflow_runs"].([]any)...)
}

func resealPromotion(t *testing.T, p Object) Object {
	t.Helper()
	unsigned := Object{}
	for k, v := range p {
		if k != "sha256" {
			unsigned[k] = v
		}
	}
	data, err := Canonical(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	p["sha256"] = SHA256(data)
	return p
}

func TestHistoryPromotionSeparatesCaptureIdentityAndCurrentReview(t *testing.T) {
	f, baseline, p, _ := promotionFixture(t)
	if _, err := ValidateHistoryPromotion(p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.ValidateReviewedHistoryPromotion(p, p["target"]); err == nil {
		t.Fatal("capture silently authorized promotion")
	}
	if !Equal(p["preview_checkpoint"].(Object)["history_cutover"], baseline) {
		t.Fatal("capture changed quarantine")
	}
	admitPromotionFixture(t, f.engine, p)
	if _, err := f.engine.ValidateReviewedHistoryPromotion(p, p["target"]); err != nil {
		t.Fatal(err)
	}
	f.engine.ResetHistoryReviews("task.yml", "PROMOTION")
	if _, err := f.engine.ValidateReviewedHistoryPromotion(p, p["target"]); err == nil {
		t.Fatal("cached admission survived revocation")
	}
	fresh := f.newProcessEngine(t)
	admitPromotionFixture(t, fresh, p)
	policyValue, _ := DecodeValue(f.policyBytes)
	wf := policyValue.(Object)["workflows"].(Object)["task.yml"].(Object)
	wf["allow_publication"] = true
	changed, err := NewEngine(policyValue.(Object), f.repository)
	if err != nil {
		t.Fatal(err)
	}
	admitPromotionFixture(t, changed, p)
	if _, err := changed.ValidateReviewedHistoryPromotion(p, p["target"]); err == nil {
		t.Fatal("policy expansion inherited old approval")
	}
	if _, err := fresh.ValidateReviewedHistoryPromotion(p, mustDifferentCutoverTarget(t, p["target"])); err == nil {
		t.Fatal("foreign workflow inherited review")
	}
}

func TestHistoryPromotionRejectsTamperScopeExpansionAndOpenFrontier(t *testing.T) {
	for _, kind := range []string{"digest", "publication", "scope", "target", "state", "plan", "checkpoint", "frontier", "policy", "quarantine"} {
		t.Run(kind, func(t *testing.T) {
			f, _, original, _ := promotionFixture(t)
			p := cloneNativeObject(original)
			switch kind {
			case "digest":
				p["sha256"] = strings.Repeat("a", 64)
			case "publication":
				p["excluded_operations"] = []any{"historical-replay", "historical-settlement"}
			case "scope":
				p["scope"] = "all-native"
			case "target":
				p["target"].(Object)["repository"] = "other/widgets"
			case "state":
				p["state_reads"].([]any)[0].(Object)["endpoint"] = "repos/other/widgets/issues/17"
			case "plan":
				p["plans"].(Object)["other-execution"] = cloneNativeObject(p["plans"].(Object)["execution"].(Object))
			case "checkpoint":
				p["preview_checkpoint"].(Object)["sha256"] = strings.Repeat("a", 64)
			case "frontier":
				c := p["preview_checkpoint"].(Object)
				c["prepared_frontier"] = []any{Object{"kind": "journaled"}}
				resealPromotion(t, c)
			case "policy":
				p["policy_sha256"] = strings.Repeat("a", 64)
			case "quarantine":
				b := p["preview_checkpoint"].(Object)["history_cutover"].(Object)
				b["attempts"].([]any)[0].(Object)["outcome"] = "completed"
				resealHistoryCutoverFixture(t, b)
				resealPromotion(t, p["preview_checkpoint"].(Object))
			}
			if kind != "digest" {
				resealPromotion(t, p)
			}
			admitPromotionFixture(t, f.engine, original)
			if _, err := f.engine.ValidateReviewedHistoryPromotion(p, p["target"]); err == nil {
				t.Fatal("altered promotion reused original review")
			}
			if kind == "plan" { // Even a new explicit review must select only unchanged policy scopes.
				p["plans"].(Object)["execution"].(Object)["allowed_ops"] = []any{"branch-delete"}
				resealPromotion(t, p)
				f.engine.ResetHistoryReviews("task.yml", "PROMOTION")
				admitPromotionFixture(t, f.engine, p)
				if _, err := f.engine.ValidateReviewedHistoryPromotion(p, p["target"]); err == nil {
					t.Fatal("review overrode native policy")
				}
			}
		})
	}
}

func TestPromotionPreviewRejectsUncoveredHistoryAndPartialEvidence(t *testing.T) {
	for _, kind := range []string{"later-run", "later-rerun", "active", "partial", "existing-lineage", "foreign-state", "duplicate-plan", "missing-state", "unreviewed"} {
		t.Run(kind, func(t *testing.T) {
			f, b, _, legacy := promotionFixture(t)
			r := historyCutoverFixture([]Object{legacy}, nil, nil, map[string]Object{"repos/example/widgets/issues/17": f.reader.reads["repos/example/widgets/issues/17"]})
			rows := r.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)
			plans, reads := []string{"execution"}, []string{"repos/example/widgets/issues/17"}
			switch kind {
			case "later-run":
				rows["total_count"] = int64(2)
				rows["workflow_runs"] = []any{legacy, recoveryHistoryRun(6, 1, "Later")}
			case "later-rerun":
				legacy["run_attempt"] = int64(2)
			case "active":
				legacy["status"] = "in_progress"
			case "partial":
				rows["total_count"] = int64(2)
			case "existing-lineage":
				r.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(1), "artifacts": []any{Object{"id": int64(1), "name": RecoveryArtifactName(b["target"], 6, 1)}}}}
			case "foreign-state":
				reads = []string{"repos/other/widgets/issues/17"}
			case "duplicate-plan":
				plans = []string{"execution", "execution"}
			case "missing-state":
				reads = nil
			case "unreviewed":
				wf := f.engine.workflows["task.yml"]
				wf.historyCutoverReviews = map[string]bool{}
				f.engine.workflows["task.yml"] = wf
			}
			if _, err := f.engine.PreviewHistoryPromotion(context.Background(), r, b, nil, plans, reads); err == nil {
				t.Fatal("unsafe promotion capture succeeded")
			}
			for _, call := range r.calls {
				if strings.HasPrefix(call, "ZIP ") {
					t.Fatal("preview replayed an unknown package")
				}
			}
		})
	}
}

func TestPromotedFreshNativeWorkPreservesQuarantineAndScope(t *testing.T) {
	f, b, p, legacy := promotionFixture(t)
	admitPromotionFixture(t, f.engine, p)
	f.setAttempt(1, true)
	addPromotionLegacy(f, legacy)
	inv := f.invocation(1, t.TempDir())
	result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), f.reader, inv, p)
	if err != nil || result["outcome"] != "fresh" || result["mode"] != "fresh-native" {
		t.Fatalf("promotion did not start exact fresh work: %#v %v", result, err)
	}
	value, err := LoadJSON(filepath.Join(inv.PackageRoot, "settlement-chain.json"))
	if err != nil {
		t.Fatal(err)
	}
	chain := value.(Object)
	if !exactInt(chain["schema_version"], 7) || !Equal(chain["history_promotion"], p) || len(chain["settlements"].([]any)) != 0 || !Equal(p["preview_checkpoint"].(Object)["history_cutover"], b) {
		t.Fatal("promotion invented historical settlements or dropped lineage")
	}
	f.prepareFirstAttempt(t, inv.PackageRoot)
	before, _ := ReadPackageFile(inv.PackageRoot, "run-context.json")
	entry := mustPlanEntry(t, inv.PackageRoot, "execution")
	context := mustRunContext(t, inv.PackageRoot)
	entry["name"] = "other-execution"
	if err := f.engine.ValidateRecoveryPlan(context, entry, f.plan, inv.PackageRoot); err == nil {
		t.Fatal("unselected plan entered promoted workflow")
	}
	context["publication"] = Object{"stage": "prepared"}
	if err := f.engine.validateRunContext(context); err == nil {
		t.Fatal("promotion enabled publication")
	}
	f.engine.ResetHistoryReviews("task.yml", "PROMOTION")
	if _, err := f.engine.MarkContextPlan(inv.PackageRoot, "execution", "dispatching"); err == nil {
		t.Fatal("revoked promotion marked native dispatch")
	}
	after, _ := ReadPackageFile(inv.PackageRoot, "run-context.json")
	if !Equal(before, after) {
		t.Fatal("denied operation rewrote context")
	}
	for _, kind := range []string{"absent", "downgraded"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			e := f.newProcessEngine(t)
			admitPromotionFixture(t, e, p)
			if kind == "downgraded" {
				empty, _ := EmptyChain(p["target"])
				if err := persistPackageJSON(root, "settlement-chain.json", empty); err != nil {
					t.Fatal(err)
				}
			}
			input := Object{"workflow_file": "task.yml", "repository": "example/widgets", "recovery_key": "task", "run_name": "Current", "run_id": int64(101), "attempt": int64(1), "attempt_target": Object{"kind": "issue", "number": int64(17)}, "dispatch_steps": []any{"Apply execution"}}
			if _, err := e.InitializeContext(root, input); err == nil {
				t.Fatal("missing promotion lineage initialized native context")
			}
			if _, err := os.Lstat(filepath.Join(root, "run-context.json")); !os.IsNotExist(err) {
				t.Fatal("denied start persisted executable context")
			}
		})
	}
}

func TestPromotedRecoveryHoldsChangedLiveStateAndQuarantinedRerun(t *testing.T) {
	for _, kind := range []string{"changed-state", "quarantined-rerun", "unreviewed", "missing-lineage"} {
		t.Run(kind, func(t *testing.T) {
			f, _, p, legacy := promotionFixture(t)
			if kind != "unreviewed" {
				admitPromotionFixture(t, f.engine, p)
			}
			f.setAttempt(1, true)
			addPromotionLegacy(f, legacy)
			inv := f.invocation(1, t.TempDir())
			if kind == "changed-state" {
				f.reader.reads["repos/example/widgets/issues/17"] = Object{"number": int64(17), "state": "closed"}
			}
			if kind == "quarantined-rerun" {
				legacy["run_attempt"] = int64(2)
				legacy["status"] = "in_progress"
				legacy["conclusion"] = nil
				inv.RunID = 5
				inv.Attempt = 2
				inv.RunName = "Quarantined"
			}
			var result Object
			var err error
			if kind == "missing-lineage" {
				result, err = f.engine.Recover(context.Background(), f.reader, inv)
			} else {
				result, err = f.engine.RecoverWithHistoryPromotion(context.Background(), f.reader, inv, p)
			}
			if err != nil || result["outcome"] != "recovery_needed" {
				t.Fatalf("unsafe recovery was not held: %#v %v", result, err)
			}
			for _, path := range []string{"run-context.json", "plans", "journal", "settlement-chain.json"} {
				if _, err := os.Lstat(filepath.Join(inv.PackageRoot, path)); !os.IsNotExist(err) {
					t.Fatal("hold advanced executable state", path, err)
				}
			}
		})
	}
}

func TestPromotedPreparedInterruptionResumesThroughFreshProcessAndNativeReceipt(t *testing.T) {
	f, b, p, legacy := promotionFixture(t)
	admitPromotionFixture(t, f.engine, p)
	f.setAttempt(1, true)
	addPromotionLegacy(f, legacy)
	inv1 := f.invocation(1, t.TempDir())
	if result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), f.reader, inv1, p); err != nil || result["outcome"] != "fresh" {
		t.Fatal(result, err)
	}
	f.prepareFirstAttempt(t, inv1.PackageRoot)
	if _, err := f.engine.MarkContextPlan(inv1.PackageRoot, "execution", "dispatching"); err != nil {
		t.Fatal(err)
	}
	f.qualifyCurrent(t, 1, inv1, "in_progress")
	f.finalizePending(t, 1, inv1, inv1.PackageRoot)
	f.engine = f.newProcessEngine(t)
	admitPromotionFixture(t, f.engine, p)
	f.setAttempt(2, true)
	addPromotionLegacy(f, legacy)
	root2 := t.TempDir()
	inv2 := f.invocation(2, root2)
	result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), f.reader, inv2, p)
	if err != nil || result["outcome"] != "resumed" {
		t.Fatalf("promoted prepared source lost across restart: %#v %v", result, err)
	}
	observed, err := f.engine.ObserveSourceContext(inv2.PackageRoot, Object{"current_run_id": preparedLifecycleRunID, "current_attempt": int64(2), "current_run_name": preparedLifecycleRunName, "workflow_file": "task.yml", "repository": "example/widgets", "trusted_source_sha": f.workflowSHA})
	if err != nil || !exactInt(observed["recovered_from_attempt"], 1) {
		t.Fatal(observed, err)
	}
	adapter := &preparedLifecycleAdapter{dispatches: map[string]int{}}
	journalRoot := filepath.Join(realTestPath(t, root2), "native-apply", ".artifacts", "gh-steward", "journals")
	if err := os.MkdirAll(journalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if installed, err := f.engine.InstallRestoredJournal(inv2.PackageRoot, journalRoot, "execution"); err != nil || installed["outcome"] != "no-journal-required" {
		t.Fatal(installed, err)
	}
	finishPreparedExecution(t, f, inv2, root2, adapter)
	if adapter.dispatches["execution:comment-1"] != 1 {
		t.Fatal("interruption replayed or lost native dispatch", adapter.dispatches)
	}
	_, payload := finalizeCompletedAttempt(t, f, 2, inv2)
	closed, err := CheckpointFromArchive(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !exactInt(closed["schema_version"], 7) || !Equal(closed["history_promotion"], p) || len(closed["prepared_frontier"].([]any)) != 0 || len(closed["settlements"].([]any)) != 2 {
		t.Fatal("terminal proof lost promoted lineage")
	}
	retained, _ := historyCutoverFromChain(closed)
	if !Equal(retained, b) || retained["attempts"].([]any)[0].(Object)["outcome"] != "unknown" {
		t.Fatal("native resume reclassified historical quarantine")
	}
	f.engine = f.newProcessEngine(t)
	admitPromotionFixture(t, f.engine, p)
	f.setAttempt(3, true)
	addPromotionLegacy(f, legacy)
	inv3 := f.invocation(3, t.TempDir())
	if result, err := f.engine.Recover(context.Background(), f.reader, inv3); err != nil || result["outcome"] != "terminal" {
		t.Fatalf("hosted promoted checkpoint did not select across process: %#v %v", result, err)
	}
	revoked := f.newProcessEngine(t)
	inv4 := f.invocation(3, t.TempDir())
	if result, err := revoked.Recover(context.Background(), f.reader, inv4); err != nil || result["outcome"] != "recovery_needed" {
		t.Fatal("revoked hosted checkpoint resumed native work", result, err)
	}
}

func TestPromotionAcquiresExactHostedPreviewCheckpointWithoutDroppingNativePrefix(t *testing.T) {
	legacy := recoveryHistoryRun(5, 1, "Quarantined")
	baseline := captureHistoryCutoverFixture(t, historyCutoverFixture([]Object{legacy}, nil, map[string]Object{historyCutoverAttemptEndpoint(5, 1): historyCutoverAttemptResponse(legacy, 1, legacy["created_at"].(string))}, nil))
	policy := workflowNoopPolicy()
	wf := policy["workflows"].(Object)["task.yml"].(Object)
	wf["mutator_step_alternatives"] = []any{[]any{"Apply reviewed change"}}
	noop := wf["plans"].(Object)["workflow-noop"].(Object)
	noop["approval"] = Object{"kind": "local-noop", "workflow_source_sha256": SHA256([]byte(noopWorkflowSource)), "mutators": []any{Object{"job": "Mutation job", "steps": []any{"Apply reviewed change"}}}}
	nativeFixture := newPreparedLifecycle(t, 1)
	nativePolicy, _ := DecodeValue(nativeFixture.policyBytes)
	wf["plans"].(Object)["execution"] = nativePolicy.(Object)["workflows"].(Object)["task.yml"].(Object)["plans"].(Object)["execution"]
	wf["history_cutover_reviews"] = []any{baseline["sha256"]}
	wf["history_promotion_review_issue"] = Object{"number": int64(18), "trusted_logins": []any{"maintainer"}}
	engine, err := NewEngine(policy, historyCutoverTestRepository)
	if err != nil {
		t.Fatal(err)
	}
	reader := readerWithCurrentNoop(10, 1)
	packet := reader.pages["repos/example/widgets/actions/workflows/task.yml/runs?per_page=100"][0].(Object)
	current := packet["workflow_runs"].([]any)[0].(Object)
	packet["total_count"] = int64(2)
	packet["workflow_runs"] = []any{legacy, current}
	temp := t.TempDir()
	options := historyCutoverNoopOptions(t, engine, reader, baseline, temp, 10, 1)
	if _, err := engine.FinishNoop(context.Background(), reader, options); err != nil {
		t.Fatal(err)
	}
	current["status"], current["conclusion"] = "completed", "success"
	artifact, _ := uploadFixturePackage(t, reader, engine, options.Invocation, 501)
	reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(1), "artifacts": []any{artifact}}}
	result, err := engine.Finalize(context.Background(), reader, FinalizeOptions{Invocation: options.Invocation, ArtifactID: 501, ArtifactDigest: artifact["digest"].(string), Checkpoint: filepath.Join(temp, "checkpoint")})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result["checkpoint_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	payload := recoveryZip(t, recoveryZipEntry{name: "settlement-chain.json", data: data})
	metadata := Object{"id": int64(502), "name": CheckpointArtifactName(baseline["target"], 10, 1), "expired": false, "digest": "sha256:" + SHA256(payload), "workflow_run": Object{"id": int64(10), "head_sha": strings.Repeat("a", 40)}}
	reader.archives[502] = payload
	reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(2), "artifacts": []any{artifact, metadata}}}
	reader.reads["repos/example/widgets/issues/17"] = Object{"number": int64(17), "state": "open"}
	checkpoint, err := engine.AcquirePromotionPreviewCheckpoint(context.Background(), reader, baseline, 502, metadata["digest"].(string))
	if err != nil {
		t.Fatal(err)
	}
	p, err := engine.PreviewHistoryPromotion(context.Background(), reader, baseline, checkpoint, []string{"execution"}, []string{"repos/example/widgets/issues/17"})
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(p["preview_checkpoint"], checkpoint) || len(checkpoint["settlements"].([]any)) != 1 {
		t.Fatal("promotion erased preview native settlement")
	}
	admitPromotionFixture(t, engine, p)
	next := recoveryHistoryRun(11, 1, "Current")
	next["status"], next["conclusion"] = "in_progress", nil
	packet["total_count"] = int64(3)
	packet["workflow_runs"] = []any{legacy, current, next}
	tempNext := t.TempDir()
	inv := Invocation{Workflow: "task.yml", RunID: 11, Attempt: 1, RunName: "Current", RecoveryKey: "task", PackageRoot: filepath.Join(tempNext, "invocation"), RunnerTemp: tempNext}
	if result, err := engine.RecoverWithHistoryPromotion(context.Background(), reader, inv, p); err != nil || result["outcome"] != "fresh" || result["mode"] != "fresh-native" {
		t.Fatal(result, err)
	}
	promoted, _ := historyPromotionChain(p)
	observations, attempts, _ := promotionLiveHistory([]Object{legacy, current, next}, baseline["target"].(Object))
	changed := cloneNativeObject(checkpoint)
	changed["history_cutover"].(Object)["workflow"].(Object)["name"] = "changed"
	resealHistoryCutoverFixture(t, changed["history_cutover"].(Object))
	resealPromotion(t, changed)
	if _, err := engine.SelectCheckpoint([]Object{promoted, changed}, baseline["target"], observations, attempts); err == nil {
		t.Fatal("promotion accepted a forked baseline")
	}
	for _, kind := range []string{"missing", "digest", "expired", "head", "name", "duplicate", "download"} {
		t.Run(kind, func(t *testing.T) {
			original := cloneNativeObject(metadata)
			rows := []any{artifact, original}
			digest := fmt.Sprint(metadata["digest"])
			switch kind {
			case "missing":
				rows = []any{artifact}
			case "digest":
				digest = "sha256:" + strings.Repeat("b", 64)
			case "expired":
				original["expired"] = true
			case "head":
				original["workflow_run"].(Object)["head_sha"] = strings.Repeat("b", 40)
			case "name":
				original["name"] = CheckpointArtifactName(baseline["target"], 10, 2)
			case "duplicate":
				rows = append(rows, original)
			case "download":
				reader.archives[502] = []byte("tampered ZIP")
				defer func() { reader.archives[502] = payload }()
			}
			reader.pages["repos/example/widgets/actions/artifacts?per_page=100"] = []any{Object{"total_count": int64(len(rows)), "artifacts": rows}}
			if _, err := engine.AcquirePromotionPreviewCheckpoint(context.Background(), reader, baseline, 502, digest); err == nil {
				t.Fatal("unqualified hosted checkpoint admitted", kind)
			}
		})
	}
}

func TestPromotedUnknownNativeWriteIsObservedAndNeverRedispatched(t *testing.T) {
	f, _, p, legacy := promotionFixture(t)
	admitPromotionFixture(t, f.engine, p)
	f.setAttempt(1, true)
	addPromotionLegacy(f, legacy)
	temp1 := t.TempDir()
	inv1 := f.invocation(1, temp1)
	if result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), f.reader, inv1, p); err != nil || result["outcome"] != "fresh" {
		t.Fatal(result, err)
	}
	f.prepareFirstAttempt(t, inv1.PackageRoot)
	if _, err := f.engine.MarkContextPlan(inv1.PackageRoot, "execution", "dispatching"); err != nil {
		t.Fatal(err)
	}
	parsed, err := contract.ParsePlan(f.plan)
	if err != nil {
		t.Fatal(err)
	}
	applyRoot := filepath.Join(realTestPath(t, temp1), "native-apply")
	if err := os.MkdirAll(applyRoot, 0700); err != nil {
		t.Fatal(err)
	}
	id := parsed.Operations[0].ID
	adapter := &preparedLifecycleAdapter{stopAfter: id, dispatches: map[string]int{}}
	if _, err := (apply.Engine{Root: applyRoot, Repository: f.repository, Command: "execution-sync", Adapter: adapter}).Apply(context.Background(), f.plan); err == nil {
		t.Fatal("interruption claimed completion")
	}
	if adapter.dispatches[id] != 1 {
		t.Fatal("fixture did not persist exactly one acknowledgement")
	}
	if _, err := f.engine.CaptureJournal(inv1.PackageRoot, filepath.Join(applyRoot, ".artifacts", "gh-steward", "journals"), "execution"); err != nil {
		t.Fatal(err)
	}
	payload1 := recoveryPackageZIP(t, inv1.PackageRoot)
	f.addArtifact(t, RecoveryArtifactName(mustTarget(t, f), preparedLifecycleRunID, 1), 1, payload1)
	f.engine = f.newProcessEngine(t)
	admitPromotionFixture(t, f.engine, p)
	f.setAttempt(2, true)
	addPromotionLegacy(f, legacy)
	temp2 := t.TempDir()
	inv2 := f.invocation(2, temp2)
	if result, err := f.engine.RecoverWithHistoryPromotion(context.Background(), f.reader, inv2, p); err != nil || result["outcome"] != "resumed" {
		t.Fatal(result, err)
	}
	if _, err := f.engine.ObserveSourceContext(inv2.PackageRoot, Object{"current_run_id": preparedLifecycleRunID, "current_attempt": int64(2), "current_run_name": preparedLifecycleRunName, "workflow_file": "task.yml", "repository": "example/widgets", "trusted_source_sha": f.workflowSHA}); err != nil {
		t.Fatal(err)
	}
	installRoot := filepath.Join(realTestPath(t, temp2), "native-apply")
	journalRoot := filepath.Join(installRoot, ".artifacts", "gh-steward", "journals")
	if err := os.MkdirAll(journalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.InstallRestoredJournal(inv2.PackageRoot, journalRoot, "execution"); err != nil {
		t.Fatal(err)
	}
	resume := &preparedLifecycleAdapter{dispatches: map[string]int{}}
	finishPreparedExecution(t, f, inv2, temp2, resume)
	if resume.dispatches[id] != 0 || len(resume.observed) != 1 || resume.observed[0] != id {
		t.Fatal("saved unknown write was redispatched instead of positively observed", resume.dispatches, resume.observed)
	}
	_, payload := finalizeCompletedAttempt(t, f, 2, inv2)
	closed, err := CheckpointFromArchive(payload)
	if err != nil || !exactInt(closed["schema_version"], 7) || !Equal(closed["history_promotion"], p) {
		t.Fatal("journaled terminal lost promotion", closed, err)
	}
}
