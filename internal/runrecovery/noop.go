package runrecovery

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/coreycoto/gh-steward/internal/contract"
)

const noopDecisionPath = "decisions/workflow-noop.json"

var noopDecisionFields = []string{"schema_version", "decision", "repository", "server_url", "workflow_file", "run_id", "attempt", "recovery_key", "attempt_target", "workflow_sha", "event_name", "event_sha256"}
var noopObservationFields = []string{"schema_version", "outcome", "target", "run", "run_id", "attempt", "recovery_key", "chain_sha256"}
var noopDataFields = []string{"status", "decision", "proposal", "previews", "decision_sha256", "event_sha256", "observation_sha256", "workflow_api_sha256", "workflow_source_sha256", "jobs_sha256", "chain_sha256", "run_id", "attempt", "workflow_file", "recovery_key", "attempt_target", "workflow_sha", "event_name", "recovery_outcome"}
var noopDecisions = map[string]bool{"no-change": true, "preview-only": true, "review-declined": true, "prerequisite-unavailable": true, "ineligible-trigger": true, "already-settled": true}

const MaxNoopPreviewFiles = 64

var noopPreviewPath = regexp.MustCompile(`^previews/[A-Za-z0-9][A-Za-z0-9._-]{0,120}\.json$`)

// Preview documents retain planning output without becoming executable plans or
// implying a review. Source and exhaustive skipped-mutator proof settle the run.
func noopPreviewReferences(decision Object) ([]Object, error) {
	if decision["decision"] != "preview-only" {
		if decision["previews"] != nil {
			return nil, errors.New("preview documents are outside the exact preview-only decision")
		}
		return nil, nil
	}
	if decision["proposal"] != nil {
		return nil, errors.New("preview-only decision cannot invent proposal review semantics")
	}
	rows, err := array(decision["previews"], "inert preview documents")
	if err != nil || len(rows) == 0 || len(rows) > MaxNoopPreviewFiles {
		return nil, errors.New("preview-only decision requires a bounded nonempty document inventory")
	}
	refs := make([]Object, 0, len(rows))
	seen := map[string]bool{}
	for _, raw := range rows {
		ref, err := Exact(raw, []string{"path", "sha256"}, "inert preview reference")
		if err != nil {
			return nil, err
		}
		relative, ok := ref["path"].(string)
		if !ok || !noopPreviewPath.MatchString(relative) || seen[relative] || !IsSHA256(ref["sha256"]) {
			return nil, errors.New("preview document path or raw digest is invalid or duplicated")
		}
		seen[relative] = true
		refs = append(refs, ref)
	}
	return refs, nil
}

type NoopOptions struct {
	Invocation
	WorkflowSHA, ToolVersion string
}

type noopPolicy struct {
	sourceSHA256 string
	mutators     []Object
	previous     map[string]*noopPolicy
}

func parseNoopPolicy(value Object) (*noopPolicy, error) {
	fields, err := exactWithOptional(value, []string{"kind", "workflow_source_sha256", "mutators"}, []string{"previous_sources"}, "current no-op source policy")
	if err != nil {
		return nil, err
	}
	digest, ok := fields["workflow_source_sha256"].(string)
	if fields["kind"] != "local-noop" || !ok || !IsSHA256(digest) {
		return nil, errors.New("current no-op requires an exact reviewed workflow content digest")
	}
	rows, err := array(fields["mutators"], "exhaustive current mutator jobs")
	if err != nil || len(rows) == 0 || len(rows) > 128 {
		return nil, errors.New("current no-op requires a bounded exhaustive mutation inventory")
	}
	seen := map[string]bool{}
	mutators := []Object{}
	for _, row := range rows {
		entry, err := Exact(row, []string{"job", "steps"}, "current mutator inventory")
		if err != nil {
			return nil, err
		}
		name, ok := entry["job"].(string)
		steps, err := stringsArray(entry["steps"], "current mutator steps", true)
		if !ok || !nonemptyString(name) || seen[name] || err != nil || len(steps) > 512 {
			return nil, errors.New("current no-op mutation inventory is ambiguous or incomplete")
		}
		seen[name] = true
		mutators = append(mutators, Object{"job": name, "steps": steps})
	}
	previous := map[string]*noopPolicy{}
	if value, exists := fields["previous_sources"]; exists {
		rows, err := array(value, "previous qualified no-op sources")
		if err != nil || len(rows) > 64 {
			return nil, errors.New("prior no-op source inventory is malformed or oversized")
		}
		for _, row := range rows {
			source, err := Exact(row, []string{"workflow_source_sha256", "mutators"}, "previous qualified no-op source")
			if err != nil {
				return nil, err
			}
			candidate := Object{"kind": "local-noop", "workflow_source_sha256": source["workflow_source_sha256"], "mutators": source["mutators"]}
			parsed, err := parseNoopPolicy(candidate)
			if err != nil {
				return nil, err
			}
			if parsed.sourceSHA256 == digest || previous[parsed.sourceSHA256] != nil {
				return nil, errors.New("prior no-op source identity is duplicated")
			}
			previous[parsed.sourceSHA256] = parsed
		}
	}
	return &noopPolicy{sourceSHA256: digest, mutators: mutators, previous: previous}, nil
}

func validateNoopWorkflowSource(packet Object, workflow, digest string) error {
	if packet["type"] != "file" || packet["path"] != ".github/workflows/"+workflow || packet["encoding"] != "base64" {
		return errors.New("current no-op workflow source is not the exact file")
	}
	content, ok := packet["content"].(string)
	if !ok || len(content) > MaxFileBytes*2 {
		return errors.New("current no-op workflow source is missing or oversized")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(strings.ReplaceAll(content, "\n", ""))
	if err != nil || len(raw) > MaxFileBytes || SHA256(raw) != digest {
		return errors.New("current no-op workflow differs from the reviewed exhaustive mutation inventory")
	}
	gitBlob := sha1.New()
	fmt.Fprintf(gitBlob, "blob %d%c", len(raw), 0)
	gitBlob.Write(raw)
	if packet["sha"] != hex.EncodeToString(gitBlob.Sum(nil)) {
		return errors.New("current no-op workflow Git blob identity differs")
	}
	return nil
}

func validateNoopJobs(packet Object, policy *noopPolicy, id, attempt int64) error {
	fields, err := Exact(packet, []string{"run_id", "attempt", "pages"}, "current no-op jobs proof")
	if err != nil || !exactInt(fields["run_id"], id) || !exactInt(fields["attempt"], attempt) {
		return errors.New("no-op jobs proof belongs to another exact attempt")
	}
	pages, err := array(fields["pages"], "current no-op complete jobs pages")
	if err != nil {
		return err
	}
	jobs, err := completePages(pages, "jobs")
	if err != nil {
		return err
	}
	for _, mutator := range policy.mutators {
		var matched Object
		for _, job := range jobs {
			if job["name"] != mutator["job"] {
				continue
			}
			if matched != nil || !exactInt(job["run_id"], id) {
				return errors.New("no-op mutation job is duplicated or belongs to another run")
			}
			if value, exists := job["run_attempt"]; exists && !exactInt(value, attempt) {
				return errors.New("no-op mutation job belongs to another attempt")
			}
			matched = job
		}
		if matched == nil {
			return errors.New("no-op jobs inventory is missing a declared mutation job")
		}
		if matched["status"] == "completed" && matched["conclusion"] == "skipped" {
			continue
		}
		steps, err := array(matched["steps"], "current mutation job steps")
		if err != nil {
			return err
		}
		for _, name := range mutator["steps"].([]string) {
			count := 0
			for _, raw := range steps {
				step, err := object(raw, "current mutation step")
				if err != nil {
					return err
				}
				if step["name"] != name {
					continue
				}
				count++
				if step["status"] != "completed" || step["conclusion"] != "skipped" {
					return errors.New("no-op mutation-capable step was started or is not positively skipped")
				}
			}
			if count != 1 {
				return errors.New("no-op mutation step inventory is missing or duplicated")
			}
		}
	}
	return nil
}

func settlementPrefixDigest(prefix any) (string, error) {
	encoded, err := Canonical(prefix)
	if err != nil {
		return "", err
	}
	return SHA256(encoded), nil
}

func persistRecoveryObservation(root string, invocation Invocation, run, target, chain Object, outcome string) error {
	digest, err := settlementPrefixDigest(chain["settlements"])
	if err != nil {
		return err
	}
	observation := Object{"schema_version": 1, "outcome": outcome, "target": target, "run": run, "run_id": invocation.RunID, "attempt": invocation.Attempt, "recovery_key": invocation.RecoveryKey, "chain_sha256": digest}
	return persistPackageJSON(root, "recovery-observation.json", observation)
}

// FinishNoop creates a real terminal v2 zero-operation result for this current
// protocol invocation. It cannot reclassify an old run, remove a dispatch
// journal or settle an unknown predecessor.
func (e *Engine) FinishNoop(ctx context.Context, reader ActionsReader, options NoopOptions) (Object, error) {
	if !nonemptyString(options.RunName) || !nonemptyString(options.RecoveryKey) || !settlementSHA40.MatchString(options.WorkflowSHA) || !nonemptyString(options.ToolVersion) {
		return nil, errors.New("current no-op requires exact run, target, workflow source and tool identities")
	}
	root, err := runnerDirectory(options.PackageRoot, options.RunnerTemp, false, false)
	if err != nil {
		return nil, err
	}
	runContext, err := e.invocationContext(root, options.Invocation)
	if err != nil {
		return nil, err
	}
	if err := e.validateRunContext(runContext); err != nil {
		return nil, err
	}
	plans, _ := array(runContext["plans"], "current no-op plans")
	steps, _ := array(runContext["dispatch_steps"], "current no-op dispatch steps")
	if runContext["phase"] != "prepared" || len(plans) != 0 || len(steps) != 0 || runContext["trusted_source_sha"] != options.WorkflowSHA {
		return nil, errors.New("no-op completion requires a prepared, undispatched, empty current context")
	}
	for _, field := range []string{"publication", "parent_merge", "recovered_from_run_id", "recovered_from_attempt", "branch_cleanup_outcome"} {
		if _, exists := runContext[field]; exists {
			return nil, errors.New("no-op completion cannot replace publication, recovery or dispatch evidence")
		}
	}
	allowed := map[string]bool{"run-context.json": true, "settlement-chain.json": true, "recovery-observation.json": true, "events/trigger-event.json": true, noopDecisionPath: true, "proposals/candidate.json": true, "proposals/review.json": true}
	files, err := retainedPackageFiles(root)
	if err != nil {
		return nil, err
	}
	if len(files) < 5 || len(files) > 7+MaxNoopPreviewFiles {
		return nil, errors.New("no-op package has missing or preexisting execution evidence")
	}
	for _, file := range files {
		if !allowed[file] && !noopPreviewPath.MatchString(file) {
			return nil, errors.New("no-op cannot relabel existing plan, journal, publication or other execution files")
		}
	}
	runs, run, target, observed, attempts, err := e.invocationHistory(ctx, reader, options.Invocation)
	if err != nil {
		return nil, err
	}
	active := false
	for _, current := range runs {
		if exactInt(current["id"], options.RunID) && current["status"] == "in_progress" {
			active = true
		}
	}
	if !active {
		return nil, errors.New("no-op can only finish the currently executing workflow attempt")
	}
	wf, err := e.workflowPolicy(options.Workflow)
	if err != nil {
		return nil, err
	}
	policy := wf.plans["workflow-noop"].approval.noop
	if policy == nil {
		return nil, errors.New("no-op workflow has no reviewed exhaustive mutation-source policy")
	}
	sourcePacket, err := reader.Read(ctx, fmt.Sprintf("repos/%s/contents/.github/workflows/%s?ref=%s", e.repository.FullName(), options.Workflow, options.WorkflowSHA))
	if err != nil {
		return nil, err
	}
	if err := validateNoopWorkflowSource(sourcePacket, options.Workflow, policy.sourceSHA256); err != nil {
		return nil, err
	}
	jobPages, err := reader.Pages(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", e.repository.FullName(), options.RunID, options.Attempt))
	if err != nil {
		return nil, err
	}
	jobsPacket := Object{"run_id": options.RunID, "attempt": options.Attempt, "pages": jobPages}
	if err := validateNoopJobs(jobsPacket, policy, options.RunID, options.Attempt); err != nil {
		return nil, err
	}
	chainValue, err := LoadJSON(filepath.Join(root, "settlement-chain.json"))
	if err != nil {
		return nil, err
	}
	chain, err := e.ValidateChain(chainValue, target, observed, attempts)
	if err != nil {
		return nil, err
	}
	pending, err := PendingAttempts(chain, observed, attempts, options.RunID, options.Attempt)
	if err != nil || len(pending) != 0 {
		return nil, errors.New("no-op cannot bypass an unsettled predecessor")
	}
	observationBytes, err := ReadPackageFile(root, "recovery-observation.json")
	if err != nil {
		return nil, err
	}
	observationValue, err := DecodeValue(observationBytes)
	if err != nil {
		return nil, err
	}
	observation, err := Exact(observationValue, noopObservationFields, "current recovery observation")
	if err != nil {
		return nil, err
	}
	prefixDigest, err := settlementPrefixDigest(chain["settlements"])
	if err != nil {
		return nil, err
	}
	if !Equal(observation["target"], target) || !Equal(observation["run"], run) || !exactInt(observation["run_id"], options.RunID) || !exactInt(observation["attempt"], options.Attempt) || observation["recovery_key"] != options.RecoveryKey || observation["chain_sha256"] != prefixDigest {
		return nil, errors.New("current no-op lost the exact fresh or fully settled recovery observation")
	}
	eventBytes, err := ReadPackageFile(root, "events/trigger-event.json")
	if err != nil {
		return nil, err
	}
	decisionBytes, err := ReadPackageFile(root, noopDecisionPath)
	if err != nil {
		return nil, err
	}
	decisionValue, err := DecodeValue(decisionBytes)
	if err != nil {
		return nil, err
	}
	decision, err := exactWithOptional(decisionValue, noopDecisionFields, []string{"proposal", "previews"}, "current no-op decision")
	if err != nil {
		return nil, err
	}
	previews, err := noopPreviewReferences(decision)
	if err != nil {
		return nil, err
	}
	expectedFiles := 5
	if decision["proposal"] != nil {
		proposal, err := Exact(decision["proposal"], []string{"candidate_sha256", "review_sha256"}, "inert proposal evidence")
		if err != nil {
			return nil, err
		}
		expectedFiles++
		if proposal["review_sha256"] != nil {
			expectedFiles++
		}
	}
	expectedFiles += len(previews)
	if len(files) != expectedFiles {
		return nil, errors.New("no-op inert candidate evidence is missing or unreferenced")
	}
	data := Object{
		"status": "completed", "decision": decision["decision"], "proposal": decision["proposal"], "previews": decision["previews"], "decision_sha256": SHA256(decisionBytes),
		"event_sha256": SHA256(eventBytes), "observation_sha256": SHA256(observationBytes), "chain_sha256": prefixDigest,
		"run_id": options.RunID, "attempt": options.Attempt, "workflow_file": options.Workflow, "recovery_key": options.RecoveryKey,
		"attempt_target": runContext["attempt_target"], "workflow_sha": options.WorkflowSHA, "event_name": run["event"], "recovery_outcome": observation["outcome"],
	}
	for relative, value := range map[string]Object{"events/workflow-source.json": sourcePacket, "events/current-jobs.json": jobsPacket} {
		if err := persistPackageJSON(root, relative, value); err != nil {
			return nil, err
		}
	}
	sourceBytes, err := ReadPackageFile(root, "events/workflow-source.json")
	if err != nil {
		return nil, err
	}
	jobsBytes, err := ReadPackageFile(root, "events/current-jobs.json")
	if err != nil {
		return nil, err
	}
	data["workflow_api_sha256"], data["workflow_source_sha256"], data["jobs_sha256"] = SHA256(sourceBytes), policy.sourceSHA256, SHA256(jobsBytes)
	sources := Object{}
	for name, digest := range map[string]string{"decision": SHA256(decisionBytes), "event": SHA256(eventBytes), "history": SHA256(observationBytes), "workflow": SHA256(sourceBytes), "jobs": SHA256(jobsBytes)} {
		sources[name] = Object{"live": true, "complete": true, "sha256": digest}
	}
	if decision["proposal"] != nil {
		proposal := decision["proposal"].(Object)
		for _, name := range []string{"candidate", "review"} {
			if name == "review" && proposal["review_sha256"] == nil {
				continue
			}
			raw, err := ReadPackageFile(root, "proposals/"+name+".json")
			if err != nil {
				return nil, err
			}
			sources[name] = Object{"live": true, "complete": true, "sha256": SHA256(raw)}
		}
	}
	for index, ref := range previews {
		raw, err := ReadPackageFile(root, ref["path"].(string))
		if err != nil {
			return nil, err
		}
		sources[fmt.Sprintf("preview-%d", index)] = Object{"live": true, "complete": true, "sha256": SHA256(raw)}
	}
	plan, err := contract.PreparePlan("workflow-noop", e.repository, sources, data, nil, time.Now())
	if err != nil {
		return nil, err
	}
	planRepository := plan.Repository.Object()
	identity := Object{"schema_version": 2, "repository": planRepository, "command": plan.Command, "plan_sha256": plan.SHA256}
	encodedIdentity, err := Canonical(identity)
	if err != nil {
		return nil, err
	}
	journalID := SHA256(encodedIdentity)
	entry := Object{"name": "workflow-noop", "path": "plans/workflow-noop.json", "command": plan.Command, "sha256": plan.SHA256, "journal_id": journalID, "status": "completed"}
	runContext["plans"], runContext["phase"] = []any{entry}, "completed"
	if err := e.validateRunContext(runContext); err != nil {
		return nil, err
	}
	if err := e.ValidateRecoveryPlan(runContext, entry, plan.Object(), root); err != nil {
		return nil, err
	}
	terminal := Object{"status": "completed", "command": plan.Command, "repository": planRepository, "plan_sha256": plan.SHA256, "receipts": []any{}}
	journal := Object{"identity": identity, "steps": []any{}, "result": terminal}
	apply := Object{"schema_version": 2, "tool_version": options.ToolVersion, "command": plan.Command, "repository": planRepository, "data": terminal}
	// Context is published last. A interrupted local write preserves its
	// partial files and cannot be converted to a new no-op by a later retry.
	for _, item := range []struct {
		path  string
		value any
	}{
		{"plans/workflow-noop.json", plan.Object()},
		{"journal/" + journalID + ".json", journal},
		{"apply-results/workflow-noop.json", apply},
	} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(item.path))); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("no-op output already exists")
		}
		if err := persistPackageJSON(root, item.path, item.value); err != nil {
			return nil, err
		}
	}
	if err := persistPackageJSON(root, "run-context.json", runContext); err != nil {
		return nil, err
	}
	return Object{"outcome": "completed", "plan_sha256": plan.SHA256, "journal_id": journalID}, nil
}

func (e *Engine) validateWorkflowNoop(runContext, entry, plan Object, reader *policyReader) error {
	parsed, err := contract.ParsePlan(plan)
	if err != nil || parsed.Command != "workflow-noop" || len(parsed.Operations) != 0 {
		return errors.New("workflow no-op must be one native zero-operation plan")
	}
	if entry["name"] != "workflow-noop" || entry["path"] != "plans/workflow-noop.json" || entry["status"] != "completed" || runContext["phase"] != "completed" {
		return errors.New("workflow no-op has an unexpected manifest or phase")
	}
	entries, err := array(runContext["plans"], "no-op manifest")
	if err != nil || len(entries) != 1 {
		return errors.New("workflow no-op cannot mix execution plans")
	}
	steps, err := array(runContext["dispatch_steps"], "no-op dispatch inventory")
	if err != nil || len(steps) != 0 {
		return errors.New("workflow no-op cannot claim a dispatch inventory")
	}
	for _, field := range []string{"publication", "parent_merge", "recovered_from_run_id", "recovered_from_attempt", "branch_cleanup_outcome"} {
		if _, exists := runContext[field]; exists {
			return errors.New("workflow no-op cannot reinterpret publication or recovery evidence")
		}
	}
	data, err := Exact(plan["data"], noopDataFields, "workflow no-op data")
	if err != nil || data["status"] != "completed" {
		return errors.New("workflow no-op data is not terminal")
	}
	wf, err := e.workflowPolicy(fmt.Sprint(runContext["workflow_file"]))
	if err != nil {
		return err
	}
	policy := wf.plans["workflow-noop"].approval.noop
	if policy == nil {
		return errors.New("workflow no-op has no exhaustive source policy")
	}
	if source, ok := data["workflow_source_sha256"].(string); ok && source != policy.sourceSHA256 {
		policy = policy.previous[source]
		if policy == nil {
			return errors.New("historical no-op source has not been explicitly retained as qualified")
		}
	}
	workflowSource, sourceBytes, err := reader.read("events/workflow-source.json", "current no-op workflow source")
	if err != nil {
		return err
	}
	if data["workflow_source_sha256"] != policy.sourceSHA256 || data["workflow_api_sha256"] != SHA256(sourceBytes) {
		return errors.New("no-op workflow source witness differs")
	}
	if err := validateNoopWorkflowSource(workflowSource, fmt.Sprint(runContext["workflow_file"]), policy.sourceSHA256); err != nil {
		return err
	}
	jobs, jobsBytes, err := reader.read("events/current-jobs.json", "current no-op jobs")
	if err != nil {
		return err
	}
	if data["jobs_sha256"] != SHA256(jobsBytes) {
		return errors.New("no-op jobs witness differs")
	}
	decision, decisionBytes, err := reader.read(noopDecisionPath, "workflow no-op decision")
	if err != nil {
		return err
	}
	decision, err = exactWithOptional(decision, noopDecisionFields, []string{"proposal", "previews"}, "workflow no-op decision")
	if err != nil {
		return err
	}
	event, eventBytes, err := reader.read("events/trigger-event.json", "workflow no-op event")
	if err != nil {
		return err
	}
	observation, observationBytes, err := reader.read("recovery-observation.json", "workflow no-op history observation")
	if err != nil {
		return err
	}
	observation, err = Exact(observation, noopObservationFields, "workflow no-op history observation")
	if err != nil {
		return err
	}
	target, err := e.validateTarget(observation["target"])
	if err != nil {
		return err
	}
	run, err := Exact(observation["run"], immutableRunFields, "workflow no-op source run")
	if err != nil {
		return err
	}
	runID, err := positiveInteger(runContext["workflow_run_id"], "no-op run ID")
	if err != nil {
		return err
	}
	attempt, err := positiveInteger(runContext["workflow_run_attempt"], "no-op attempt")
	if err != nil {
		return err
	}
	if err := validateNoopJobs(jobs, policy, runID, attempt); err != nil {
		return err
	}
	if _, err := validateRunIdentity(run, runID, attempt, target); err != nil {
		return err
	}
	name, ok := decision["decision"].(string)
	if !ok || !noopDecisions[name] || !exactInt(decision["schema_version"], 1) || !exactInt(observation["schema_version"], 1) {
		return errors.New("workflow no-op decision is unsupported")
	}
	if !Equal(data["proposal"], decision["proposal"]) {
		return errors.New("workflow no-op changed its inert candidate evidence")
	}
	if !Equal(data["previews"], decision["previews"]) {
		return errors.New("workflow no-op changed its inert preview document inventory")
	}
	previews, err := noopPreviewReferences(decision)
	if err != nil {
		return err
	}
	outcome := observation["outcome"]
	if (outcome != "fresh" && outcome != "terminal") || (outcome == "terminal" && name != "already-settled") || (outcome == "fresh" && name == "already-settled") {
		return errors.New("workflow no-op decision is not allowed for this exact recovery outcome")
	}
	if outcome == "terminal" && attempt <= 1 {
		return errors.New("settled-observer no-op requires an exact rerun")
	}
	if !IsSHA256(observation["chain_sha256"]) || data["chain_sha256"] != observation["chain_sha256"] || data["observation_sha256"] != SHA256(observationBytes) || data["event_sha256"] != SHA256(eventBytes) || data["decision_sha256"] != SHA256(decisionBytes) {
		return errors.New("workflow no-op did not retain exact decision, event and predecessor proof bytes")
	}
	if target["workflow_file"] != runContext["workflow_file"] || run["display_title"] != runContext["run_name"] || run["event"] != data["event_name"] || runContext["trusted_source_sha"] != data["workflow_sha"] || !settlementSHA40.MatchString(fmt.Sprint(data["workflow_sha"])) {
		return errors.New("workflow no-op differs from the exact control source or invocation")
	}
	for _, field := range []string{"workflow_file", "recovery_key", "attempt_target"} {
		if !Equal(data[field], runContext[field]) || !Equal(decision[field], runContext[field]) {
			return errors.New("workflow no-op changes its exact target")
		}
	}
	for _, pair := range []struct {
		field string
		value any
	}{{"run_id", runID}, {"attempt", attempt}, {"workflow_sha", data["workflow_sha"]}, {"event_name", data["event_name"]}, {"event_sha256", data["event_sha256"]}} {
		if !Equal(decision[pair.field], pair.value) || !Equal(data[pair.field], pair.value) {
			return errors.New("workflow no-op decision identity differs from its terminal plan")
		}
	}
	if !exactInt(observation["run_id"], runID) || !exactInt(observation["attempt"], attempt) || observation["recovery_key"] != runContext["recovery_key"] || data["decision"] != name || data["recovery_outcome"] != outcome || decision["repository"] != e.repository.FullName() || decision["server_url"] != "https://"+e.repository.Host {
		return errors.New("workflow no-op decision belongs to another exact invocation")
	}
	repository, err := object(event["repository"], "no-op trigger repository")
	if err != nil || repository["full_name"] != e.repository.FullName() {
		return errors.New("no-op trigger event belongs to another repository")
	}
	if htmlURL, exists := repository["html_url"]; exists && htmlURL != e.repository.URL {
		return errors.New("no-op trigger event repository host differs")
	}
	expectedSources := Object{}
	for source, digest := range map[string]string{"decision": SHA256(decisionBytes), "event": SHA256(eventBytes), "history": SHA256(observationBytes), "workflow": SHA256(sourceBytes), "jobs": SHA256(jobsBytes)} {
		expectedSources[source] = Object{"live": true, "complete": true, "sha256": digest}
	}
	if decision["proposal"] != nil {
		proposal, err := Exact(decision["proposal"], []string{"candidate_sha256", "review_sha256"}, "declined inert proposal")
		if err != nil {
			return err
		}
		if name != "review-declined" && name != "no-change" {
			return errors.New("inert proposal is outside a declined or empty candidate decision")
		}
		if name == "review-declined" && !IsSHA256(proposal["review_sha256"]) {
			return errors.New("negative review lost its exact raw review digest")
		}
		if name == "no-change" && proposal["review_sha256"] != nil {
			return errors.New("empty native candidate must not invent a review approval")
		}
		for _, source := range []string{"candidate", "review"} {
			if source == "review" && proposal["review_sha256"] == nil {
				continue
			}
			value, raw, err := reader.read("proposals/"+source+".json", "declined proposal "+source)
			if err != nil || proposal[source+"_sha256"] != SHA256(raw) {
				return errors.New("declined proposal lost its exact candidate or review bytes")
			}
			if source == "review" && !contains([]string{"noop", "blocked", "declined", "rejected"}, fmt.Sprint(value["status"])) {
				return errors.New("inert proposal review is not an explicit negative decision")
			}
			if source == "candidate" && name == "no-change" {
				candidate, err := contract.ParsePlan(value)
				if err != nil || candidate.Repository != e.repository {
					return errors.New("inert candidate is not a valid native v2 plan for this repository")
				}
				if len(candidate.Operations) != 0 {
					return errors.New("no-change candidate is not an empty native plan")
				}
			}
			expectedSources[source] = Object{"live": true, "complete": true, "sha256": SHA256(raw)}
		}
	} else if name == "review-declined" {
		return errors.New("declined decision lost its inert proposal evidence")
	}
	for index, ref := range previews {
		value, raw, err := reader.read(ref["path"].(string), "inert preview document")
		if err != nil || len(value) == 0 || ref["sha256"] != SHA256(raw) {
			return errors.New("preview document lost its exact nonempty JSON object bytes")
		}
		_, hasNativeOperations := value["operations"]
		_, hasNativeCommand := value["command"]
		if exactInt(value["schema_version"], 2) && (hasNativeOperations || hasNativeCommand) {
			candidate, err := contract.ParsePlan(value)
			if err != nil || candidate.Repository != e.repository {
				return errors.New("inert native preview plan is invalid or belongs to another repository")
			}
		}
		expectedSources[fmt.Sprintf("preview-%d", index)] = Object{"live": true, "complete": true, "sha256": SHA256(raw)}
	}
	if !Equal(plan["sources"], expectedSources) {
		return errors.New("no-op source provenance differs from its retained proof bytes")
	}
	return nil
}

// validateNoopFrontier binds each no-op to the immutable prefix preceding it.
// It avoids embedding the growing checkpoint recursively in every plan proof.
func validateNoopFrontier(record Object, prefix []any) error {
	settled, err := object(record["settlement"], "settlement")
	if err != nil {
		return err
	}
	if settled["kind"] != "terminal" {
		return nil
	}
	plans, err := array(settled["plans"], "settlement plans")
	if err != nil {
		return err
	}
	for _, raw := range plans {
		proof, err := object(raw, "settlement plan")
		if err != nil {
			return err
		}
		if proof["command"] != "workflow-noop" {
			continue
		}
		value, err := LoadFileProof(proof["plan_file"], "workflow no-op plan")
		if err != nil {
			return err
		}
		plan, err := object(value, "workflow no-op plan")
		if err != nil {
			return err
		}
		data, err := object(plan["data"], "workflow no-op data")
		if err != nil {
			return err
		}
		digest, err := settlementPrefixDigest(prefix)
		if err != nil || data["chain_sha256"] != digest {
			return errors.New("workflow no-op predecessor prefix differs from the settled chain")
		}
	}
	return nil
}
