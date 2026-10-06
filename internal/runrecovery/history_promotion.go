package runrecovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/coreycoto/gh-steward/internal/contract"
)

const historyPromotionSettlementSchemaVersion = int64(7)

var historyPromotionFields = []string{"schema_version", "scope", "target", "preview_checkpoint", "policy_sha256", "plans", "state_reads", "excluded_operations", "sha256"}
var promotionChainFields = []string{"schema_version", "target", "history_promotion", "inventory", "settlements", "prepared_frontier", "prepared_terminal_proofs", "sha256"}
var historyPromotionExclusions = []any{"historical-replay", "historical-settlement", "publication"}

// ValidateHistoryPromotion checks artifact identity only. Admission additionally
// requires the independently resolved exact review and unchanged trusted policy.
func ValidateHistoryPromotion(value any) (Object, error) {
	p, err := Exact(value, historyPromotionFields, "history promotion")
	if err != nil {
		return nil, err
	}
	if !exactInt(p["schema_version"], 1) || p["scope"] != "fresh-native" || !IsSHA256(p["policy_sha256"]) || !Equal(p["excluded_operations"], historyPromotionExclusions) {
		return nil, recoveryError("history promotion has an unsupported schema, scope, policy or exclusions")
	}
	target, err := ValidateTarget(p["target"])
	if err != nil {
		return nil, err
	}
	checkpoint, err := Exact(p["preview_checkpoint"], cutoverChainFields, "promotion preview checkpoint")
	if err != nil || !exactInt(checkpoint["schema_version"], historyCutoverSettlementSchemaVersion) || !Equal(checkpoint["target"], target) {
		return nil, recoveryError("promotion requires the exact preview-only checkpoint target")
	}
	baseline, err := ValidateHistoryCutover(checkpoint["history_cutover"])
	if err != nil || !Equal(baseline["target"], target) {
		return nil, recoveryError("promotion changes its quarantined baseline")
	}
	if !Equal(checkpoint["prepared_frontier"], []any{}) || !Equal(checkpoint["prepared_terminal_proofs"], []any{}) || verifyObjectDigest(checkpoint) != nil {
		return nil, recoveryError("promotion preview checkpoint has an open frontier or invalid digest")
	}
	plans, err := object(p["plans"], "promotion plan scopes")
	if err != nil || len(plans) == 0 || len(plans) > 128 {
		return nil, recoveryError("promotion requires a bounded nonempty plan scope")
	}
	for name, raw := range plans {
		scope, err := Exact(raw, []string{"command", "allowed_ops"}, "promotion plan scope")
		command, ok := scope["command"].(string)
		ops, opsErr := stringsArray(scope["allowed_ops"], "promotion operations", false)
		if !policyPlanName.MatchString(name) || name == "workflow-noop" || err != nil || !ok || !nativeCommandPattern.MatchString(command) || command == "workflow-noop" || opsErr != nil || len(ops) > MaxArtifactEntries {
			return nil, recoveryError("promotion contains an invalid exact plan scope")
		}
		for i, op := range ops {
			if i > 0 && op <= ops[i-1] {
				return nil, recoveryError("promotion operations must be sorted and unique")
			}
		}
	}
	reads, err := objectArray(p["state_reads"], "promotion reconciliation reads")
	if err != nil || len(reads) == 0 || len(reads) > 128 {
		return nil, recoveryError("promotion requires explicit bounded live-state reconciliation")
	}
	previous := ""
	for _, raw := range reads {
		read, err := Exact(raw, historyCutoverStateFields, "promotion reconciliation read")
		endpoint, ok := read["endpoint"].(string)
		_, objectErr := object(read["object"], "promotion state object")
		if err != nil || !ok || objectErr != nil || !historyCutoverStateRoute.MatchString(endpoint) || !strings.HasPrefix(endpoint, "repos/"+fmt.Sprint(target["repository"])+"/") || endpoint <= previous {
			return nil, recoveryError("promotion reconciliation reads are foreign, repeated or malformed")
		}
		previous = endpoint
	}
	if err := verifyObjectDigest(p); err != nil {
		return nil, err
	}
	return p, nil
}

func verifyObjectDigest(value Object) error {
	unsigned := Object{}
	for key, entry := range value {
		if key != "sha256" {
			unsigned[key] = entry
		}
	}
	data, err := Canonical(unsigned)
	full, fullErr := Canonical(value)
	if err != nil || fullErr != nil || len(full) > MaxCheckpointBytes || !IsSHA256(value["sha256"]) || SHA256(data) != value["sha256"] {
		return recoveryError("history promotion or checkpoint digest is invalid or exceeds its bound")
	}
	return nil
}

func isHistoryCutoverChain(chain Object) bool {
	return exactInt(chain["schema_version"], historyCutoverSettlementSchemaVersion) || exactInt(chain["schema_version"], historyPromotionSettlementSchemaVersion)
}

func historyCutoverFromChain(chain Object) (Object, error) {
	if exactInt(chain["schema_version"], historyPromotionSettlementSchemaVersion) {
		p, err := ValidateHistoryPromotion(chain["history_promotion"])
		if err != nil {
			return nil, err
		}
		return ValidateHistoryCutover(p["preview_checkpoint"].(Object)["history_cutover"])
	}
	return ValidateHistoryCutover(chain["history_cutover"])
}

func mustHistoryCutoverFromChain(chain Object) Object {
	baseline, _ := historyCutoverFromChain(chain)
	return baseline
}

func (e *Engine) recoverWithPromotion(ctx context.Context, reader ActionsReader, invocation Invocation, promotion Object) (Object, error) {
	return e.recoverWithCheckpoints(ctx, reader, invocation, nil, nil, promotion)
}

func (e *Engine) recheckPromotionState(ctx context.Context, reader ActionsReader, promotion Object) error {
	rows, _ := objectArray(promotion["state_reads"], "promotion state reads")
	endpoints := make([]string, len(rows))
	for i, row := range rows {
		endpoints[i] = fmt.Sprint(row["endpoint"])
	}
	current, err := captureHistoryCutoverState(ctx, reader, e.repository, endpoints)
	if err != nil || !Equal(objectRows(current), promotion["state_reads"]) {
		return recoveryError("live reconciliation state differs from the exact reviewed promotion")
	}
	return nil
}

func parseHistoryPromotionReviewIssue(value any) (*Object, error) {
	issue, err := Exact(value, []string{"number", "trusted_logins"}, "history promotion review issue")
	number, numberErr := positiveInteger(issue["number"], "promotion review issue number")
	logins, loginsErr := stringsArray(issue["trusted_logins"], "promotion trusted reviewers", true)
	if err != nil || numberErr != nil || loginsErr != nil || len(logins) > 128 {
		return nil, recoveryError("history promotion requires a bounded independent review issue")
	}
	seen := map[string]bool{}
	for _, login := range logins {
		if !historyCutoverLogin.MatchString(login) || seen[login] {
			return nil, recoveryError("history promotion trusted reviewer is invalid or repeated")
		}
		seen[login] = true
	}
	copyIssue := Object{"number": number, "trusted_logins": append([]string{}, logins...)}
	// Match the existing review resolver's canonical JSON array representation.
	copyIssue, err = cloneObject(copyIssue)
	return &copyIssue, err
}

func (e *Engine) HistoryPromotionReviewIssues() map[string]Object {
	result := map[string]Object{}
	for name, policy := range e.workflows {
		if policy.historyPromotionReviewIssue != nil {
			copyIssue, err := cloneObject(*policy.historyPromotionReviewIssue)
			if err == nil {
				result[name] = copyIssue
			}
		}
	}
	return result
}

func (e *Engine) ResetHistoryReviews(workflow, kind string) {
	policy := e.workflows[workflow]
	if kind == "PROMOTION" {
		policy.historyPromotionReviews = map[string]bool{}
		delete(e.activePromotions, workflow)
	} else if policy.historyCutoverReviewIssue != nil {
		policy.historyCutoverReviews = map[string]bool{}
	}
	e.workflows[workflow] = policy
}

// AdmitHistoryPromotionReview is used only after the native issue resolver has
// checked the exact independent statement and current reviewer permission.
// Authorship is not human consent; callers must obtain that consent separately.
func (e *Engine) AdmitHistoryPromotionReview(workflow, digest string) error {
	policy, err := e.workflowPolicy(workflow)
	if err != nil || policy.historyPromotionReviewIssue == nil || !IsSHA256(digest) {
		return recoveryError("promotion admission requires its configured independent review channel")
	}
	if policy.historyPromotionReviews == nil {
		policy.historyPromotionReviews = map[string]bool{}
	}
	policy.historyPromotionReviews[digest] = true
	e.workflows[workflow] = policy
	return nil
}

func (e *Engine) ValidateReviewedHistoryPromotion(value any, target any) (Object, error) {
	p, err := ValidateHistoryPromotion(value)
	if err != nil || !Equal(p["target"], target) {
		return nil, recoveryError("promotion is malformed or belongs to another target")
	}
	workflow := fmt.Sprint(p["target"].(Object)["workflow_file"])
	policy, err := e.workflowPolicy(workflow)
	if err != nil || policy.historyPromotionReviewIssue == nil || !policy.historyPromotionReviews[fmt.Sprint(p["sha256"])] || policy.sourceSHA256 != p["policy_sha256"] {
		return nil, recoveryError("promotion lacks an exact current independent review or its trusted policy changed")
	}
	if err := e.validatePromotionScopes(p, policy); err != nil {
		return nil, err
	}
	checkpoint := p["preview_checkpoint"].(Object)
	baseline, err := e.ValidateReviewedHistoryCutover(checkpoint["history_cutover"], target)
	if err != nil {
		return nil, err
	}
	observed, attempts, err := promotionCheckpointHistory(checkpoint, baseline)
	if err != nil {
		return nil, err
	}
	if _, err := e.ValidateChain(checkpoint, target, observed, attempts); err != nil {
		return nil, err
	}
	if prior := e.activePromotions[workflow]; prior != nil && !Equal(prior, p) {
		return nil, recoveryError("one recovery invocation cannot mix different promotion grants")
	}
	if e.activePromotions == nil {
		e.activePromotions = map[string]Object{}
	}
	e.activePromotions[workflow] = p
	return p, nil
}

func (e *Engine) validatePromotionScopes(p Object, policy workflowPolicy) error {
	for name, raw := range p["plans"].(Object) {
		plan, ok := policy.plans[name]
		if !ok || plan.profile == "workflow-noop" {
			return recoveryError("promotion plan is outside its trusted workflow policy")
		}
		ops := make([]string, 0, len(plan.allowedOps))
		for op := range plan.allowedOps {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		if !Equal(raw, Object{"command": plan.command, "allowed_ops": ops}) {
			return recoveryError("promotion changes a reviewed plan command or operation scope")
		}
	}
	return nil
}

func promotionCheckpointHistory(checkpoint, baseline Object) ([]Object, map[int64]int64, error) {
	baseline, err := HistoryCutoverEvidence(baseline)
	if err != nil {
		return nil, nil, err
	}
	byID, attempts := map[int64]Object{}, map[int64]int64{}
	rows, err := objectArray(baseline["run_inventory"], "promotion baseline runs")
	if err != nil {
		return nil, nil, err
	}
	for _, row := range rows {
		id := mustPositive(row["id"])
		byID[id], err = NormalizeRun(row)
		if err != nil {
			return nil, nil, err
		}
		attempts[id] = mustPositive(row["run_attempt"])
	}
	items, err := objectArray(checkpoint["inventory"], "promotion preview native inventory")
	if err != nil {
		return nil, nil, err
	}
	for _, item := range items {
		run, err := object(item["run"], "promotion preview native run")
		id, idErr := positiveInteger(run["id"], "promotion native run ID")
		attempt, attemptErr := positiveInteger(item["settled_attempt"], "promotion native attempt")
		if err != nil || idErr != nil || attemptErr != nil || (byID[id] != nil && !Equal(byID[id], run)) || attempt < attempts[id] {
			return nil, nil, recoveryError("promotion preview checkpoint changes its immutable history")
		}
		byID[id], attempts[id] = run, attempt
	}
	observed := make([]Object, 0, len(byID))
	for _, run := range byID {
		observed = append(observed, run)
	}
	return sortRunRows(observed), attempts, nil
}

// PreviewHistoryPromotion captures only read-only evidence. A supplied preview
// checkpoint must have been acquired by exact hosted artifact identity first.
func (e *Engine) PreviewHistoryPromotion(ctx context.Context, reader ActionsReader, baseline Object, checkpoint Object, planNames, endpoints []string) (Object, error) {
	if reader == nil || len(endpoints) == 0 {
		return nil, errors.New("promotion preview requires a reader and explicit reconciliation endpoints")
	}
	target, err := object(baseline["target"], "promotion baseline target")
	if err != nil {
		return nil, err
	}
	baseline, err = e.ValidateReviewedHistoryCutover(baseline, target)
	if err != nil {
		return nil, err
	}
	workflow := fmt.Sprint(target["workflow_file"])
	policy, err := e.workflowPolicy(workflow)
	if err != nil || policy.historyPromotionReviewIssue == nil {
		return nil, recoveryError("configure the independent promotion review channel before capture")
	}
	rows, err := CompleteWorkflowRuns(ctx, reader, e.repository, workflow)
	if err != nil {
		return nil, err
	}
	observed, attempts, err := promotionLiveHistory(rows, target)
	if err != nil {
		return nil, err
	}
	artifacts, err := CompleteRepositoryArtifacts(ctx, reader, e.repository)
	if err != nil {
		return nil, err
	}
	if checkpoint == nil {
		if err := rejectExistingTargetRecoveryArtifacts(target, artifacts); err != nil {
			return nil, err
		}
		checkpoint, err = HistoryCutoverChain(target, baseline)
		if err != nil {
			return nil, err
		}
	}
	if !exactInt(checkpoint["schema_version"], historyCutoverSettlementSchemaVersion) || !Equal(checkpoint["history_cutover"], baseline) {
		return nil, recoveryError("promotion must preserve its exact preview-only lineage")
	}
	checkpoint, err = e.ValidateChain(checkpoint, target, observed, attempts)
	if err != nil {
		return nil, err
	}
	covered, highwaters, err := promotionCheckpointHistory(checkpoint, baseline)
	if err != nil || !Equal(covered, sortRunRows(append([]Object{}, observed...))) || !Equal(highwaters, attempts) {
		return nil, recoveryError("promotion preview cannot absorb uncaptured later runs or attempts")
	}
	if err := validateHistoryCutoverNativeArtifacts(target, baseline, observed, attempts, artifacts); err != nil {
		return nil, err
	}
	plans := Object{}
	for _, name := range planNames {
		plan, ok := policy.plans[name]
		if !ok || plan.profile == "workflow-noop" || plans[name] != nil {
			return nil, recoveryError("promotion needs unique exact native plan names from trusted policy")
		}
		ops := make([]string, 0, len(plan.allowedOps))
		for op := range plan.allowedOps {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		plans[name] = Object{"command": plan.command, "allowed_ops": ops}
	}
	reads, err := captureHistoryCutoverState(ctx, reader, e.repository, endpoints)
	if err != nil {
		return nil, err
	}
	currentRows, err := CompleteWorkflowRuns(ctx, reader, e.repository, workflow)
	if err != nil || !Equal(sortRunRows(currentRows), sortRunRows(rows)) {
		return nil, recoveryError("workflow history changed during promotion capture")
	}
	currentReads, err := captureHistoryCutoverState(ctx, reader, e.repository, endpoints)
	if err != nil || !Equal(currentReads, reads) {
		return nil, recoveryError("reconciliation state changed during promotion capture")
	}
	p := Object{"schema_version": int64(1), "scope": "fresh-native", "target": target,
		"preview_checkpoint": checkpoint, "policy_sha256": policy.sourceSHA256, "plans": plans,
		"state_reads": objectRows(reads), "excluded_operations": historyPromotionExclusions}
	data, err := Canonical(p)
	if err != nil {
		return nil, err
	}
	p["sha256"] = SHA256(data)
	return ValidateHistoryPromotion(p)
}

func promotionLiveHistory(rows []Object, target Object) ([]Object, map[int64]int64, error) {
	observed, attempts := make([]Object, 0, len(rows)), map[int64]int64{}
	for _, row := range rows {
		run, err := NormalizeRun(row)
		id, idErr := positiveInteger(row["id"], "promotion workflow run")
		attempt, attemptErr := positiveInteger(row["run_attempt"], "promotion workflow attempt")
		if err != nil || idErr != nil || attemptErr != nil || attempts[id] != 0 || !Equal(run["workflow_id"], target["workflow_id"]) || row["status"] != "completed" || !nonemptyString(row["conclusion"]) {
			return nil, nil, recoveryError("promotion requires a complete idle terminal workflow history")
		}
		observed = append(observed, run)
		attempts[id] = attempt
	}
	return sortRunRows(observed), attempts, nil
}

// AcquirePromotionPreviewCheckpoint acquires the exact hosted preview lineage.
// Local files and a remembered digest cannot substitute for this provider read.
func (e *Engine) AcquirePromotionPreviewCheckpoint(ctx context.Context, reader ActionsReader, baseline Object, artifactID int64, digest string) (Object, error) {
	if reader == nil || artifactID < 1 || !settlementArtifactDigest.MatchString(digest) {
		return nil, recoveryError("promotion checkpoint requires an exact hosted upload ID and sha256 digest")
	}
	target, err := object(baseline["target"], "promotion checkpoint target")
	if err != nil {
		return nil, err
	}
	baseline, err = e.ValidateReviewedHistoryCutover(baseline, target)
	if err != nil {
		return nil, err
	}
	rows, err := CompleteRepositoryArtifacts(ctx, reader, e.repository)
	if err != nil {
		return nil, err
	}
	var metadata Object
	for _, row := range rows {
		if exactInt(row["id"], artifactID) {
			if metadata != nil {
				return nil, recoveryError("promotion checkpoint upload ID is duplicated")
			}
			metadata = row
		}
	}
	if metadata == nil || metadata["digest"] != digest {
		return nil, recoveryError("exact promotion checkpoint upload is absent or its digest changed")
	}
	name, ok := metadata["name"].(string)
	run, runErr := object(metadata["workflow_run"], "promotion checkpoint hosted run")
	runID, idErr := positiveInteger(run["id"], "promotion checkpoint hosted run ID")
	if !ok || runErr != nil || idErr != nil {
		return nil, recoveryError("promotion checkpoint hosted identity is malformed")
	}
	artifact, err := artifactIdentity(metadata, name, runID)
	if err != nil {
		return nil, err
	}
	if size, exists := metadata["size_in_bytes"]; exists {
		n, sizeErr := contract.Integer(size)
		if sizeErr != nil || n < 0 || n > MaxArtifactBytes {
			return nil, recoveryError("promotion checkpoint declared size exceeds its bound")
		}
	}
	payload, err := downloadArtifact(ctx, reader, artifact)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxArtifactBytes {
		return nil, recoveryError("promotion checkpoint exceeds its archive bound")
	}
	checkpoint, err := CheckpointFromArchive(payload)
	if err != nil {
		return nil, err
	}
	if !exactInt(checkpoint["schema_version"], historyCutoverSettlementSchemaVersion) || !Equal(checkpoint["history_cutover"], baseline) {
		return nil, recoveryError("promotion checkpoint is not the exact preview-only baseline")
	}
	observed, attempts, err := promotionCheckpointHistory(checkpoint, baseline)
	if err != nil {
		return nil, err
	}
	checkpoint, err = e.ValidateChain(checkpoint, target, observed, attempts)
	if err != nil {
		return nil, err
	}
	var owner Object
	for _, raw := range checkpoint["settlements"].([]any) {
		record := raw.(Object)
		if exactInt(record["run_id"], runID) && name == CheckpointArtifactName(target, runID, mustPositive(record["attempt"])) {
			if owner != nil {
				return nil, recoveryError("promotion checkpoint has ambiguous hosted ownership")
			}
			owner = record
		}
	}
	if owner == nil {
		return nil, recoveryError("promotion checkpoint is not owned by an exact settled preview attempt")
	}
	checked, err := e.ValidateCheckpointArtifact(checkpoint, target, Object{"name": name, "id": artifactID, "digest": digest, "workflow_run_id": runID, "workflow_run_attempt": owner["attempt"]})
	if err != nil || run["head_sha"] != checked["run"].(Object)["head_sha"] {
		return nil, recoveryError("promotion checkpoint upload differs from its settled source")
	}
	return checkpoint, nil
}

func historyPromotionChain(p Object) (Object, error) {
	checkpoint := p["preview_checkpoint"].(Object)
	chain, err := cloneObject(checkpoint)
	if err != nil {
		return nil, err
	}
	chain["schema_version"], chain["history_promotion"] = historyPromotionSettlementSchemaVersion, p
	delete(chain, "history_cutover")
	delete(chain, "sha256")
	data, err := Canonical(chain)
	if err != nil {
		return nil, err
	}
	chain["sha256"] = SHA256(data)
	return chain, verifyObjectDigest(chain)
}

func (e *Engine) RecoverWithHistoryPromotion(ctx context.Context, reader ActionsReader, invocation Invocation, promotion Object) (Object, error) {
	if promotion == nil {
		return nil, recoveryError("explicit promotion requires the separately reviewed artifact")
	}
	return e.recoverWithPromotion(ctx, reader, invocation, promotion)
}

func (e *Engine) promotionAtRoot(root, workflow string) (Object, error) {
	path := filepath.Join(root, "settlement-chain.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if policy, ok := e.workflows[workflow]; ok && policy.historyPromotionReviewIssue != nil {
			return nil, recoveryError("opted-in promotion workflow cannot omit its recovery lineage")
		}
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxCheckpointBytes {
		return nil, recoveryError("promotion lineage is unsafe or unreadable")
	}
	value, err := LoadJSON(path)
	chain, shapeErr := object(value, "promotion lineage")
	if err != nil || shapeErr != nil {
		return nil, recoveryError("promotion lineage is malformed")
	}
	if !exactInt(chain["schema_version"], historyPromotionSettlementSchemaVersion) {
		if policy, ok := e.workflows[workflow]; ok && policy.historyPromotionReviewIssue != nil && !exactInt(chain["schema_version"], historyCutoverSettlementSchemaVersion) {
			return nil, recoveryError("opted-in promotion workflow lost its quarantined lineage")
		}
		return nil, nil
	}
	chain, err = Exact(chain, promotionChainFields, "promoted lineage")
	if err != nil || verifyObjectDigest(chain) != nil {
		return nil, recoveryError("promoted lineage identity is invalid")
	}
	target, err := e.validateTarget(chain["target"])
	if err != nil || target["workflow_file"] != workflow {
		return nil, recoveryError("promoted lineage identifies another workflow")
	}
	return e.ValidateReviewedHistoryPromotion(chain["history_promotion"], target)
}

func (e *Engine) validatePromotionPlanScope(context, entry Object) error {
	p := e.activePromotions[fmt.Sprint(context["workflow_file"])]
	if p == nil {
		return nil
	}
	if context["publication"] != nil {
		return recoveryError("history promotion excludes publication")
	}
	name := fmt.Sprint(entry["name"])
	if name != "workflow-noop" && p["plans"].(Object)[name] == nil {
		return recoveryError("native plan is outside the exact reviewed promotion scope")
	}
	return nil
}
