# Workflow recovery protocol

## Trust and identity

Run every recovery command against the trusted control checkout with explicit `--repo-root`, `--repo`, and `--policy`. The policy path is checkout-relative. The workflow file, numeric run ID, positive whole-number attempt, immutable display title, and semantic recovery key identify the invocation. The workflow history target additionally binds the GitHub server and numeric workflow ID. `head_sha` identifies source code; it is not a replacement for the runtime's trusted `GITHUB_WORKFLOW_SHA`.

When `GITHUB_ACTIONS=true`, the CLI checks that the control checkout HEAD equals the actual `GITHUB_WORKFLOW_SHA` and that the policy's raw bytes equal that commit's blob. `--workflow-sha` for `finish-noop`, `verify-publication`, and `acquire-publication-candidate` must equal the runtime value. `context-start` requires the same value in its input's `trusted_source_sha`, without an additional flag. `finalize` derives the runtime SHA when the flag is omitted and rejects a different supplied value. These checks run before provider reads; offline qualification outside Actions continues to accept explicit source arguments.

`--package-root` must identify a real direct child of `--runner-temp` (default `RUNNER_TEMP`). Recovery and handoff create an empty package; context commands operate on that existing directory. A journal root points to the native engine's checkout-local `.artifacts/gh-steward/journals/`. An existing different journal is a hold.

## Command boundaries

`runs recover` needs `--workflow`, `--run-id`, `--attempt`, `--run-name`, `--recovery-key`, and `--package-root`. It reads complete workflow history and artifacts, then persists a settlement frontier, restored source, or typed hold. A resumed source must have either positive durable native dispatch identity or a verified current-source `prepared-qualification.json` proving that every exact mutation-capable step was skipped. A prepared-only legacy source without that qualification remains held. Recovery never replays a provider write.

For native plans that have no journal or apply result, run `runs qualify-prepared` before uploading the normal recovery package. It takes the same invocation flags plus `--workflow-sha`; Actions defaults that value to the actual `GITHUB_WORKFLOW_SHA`. Qualification requires the exact current raw workflow digest and complete current-attempt job pages. Each pending plan's `prepared_recovery` policy lists its exact mutation jobs and steps. The current apply job may still be `in_progress` while the qualifier runs, but each named mutation step must have a unique completed `skipped` result. Missing, started, failed, canceled, duplicated, or otherwise ambiguous mutation evidence fails closed. Qualification is current-attempt-only: `previous_sources` validates already-retained positive evidence and never qualifies a new run.

Pending prepared uploads advance the bounded schema-version 4 `prepared_frontier`; they do not create a terminal settlement. Keep each attempt's own immutable normal upload and checkpoint receipt. Repeated interrupted observers retain the original source context byte-for-byte and record both lineages: `recovered_from_run_id`/`recovered_from_attempt` name the immediately observed source, while the paired `plan_origin_run_id`/`plan_origin_attempt` preserve the original run and attempt that created the exact native plan. Do not rewrite source context or invent a journal, dispatch ID, acknowledgement, approval, or source bytes. A positively completed parent plan and prepared child plan keep their original phase/status relationship and exact parent plan, journal, and apply-result files.

On restore, `context-install-journal` may report `no-journal-required` only for the exact source-qualified prepared plan with no journal/result in the package. This is not terminal progress. The observer's actual native engine execution must produce the durable journal and terminal apply result. Only those actual observer receipts can close the prepared frontier and settle its covered attempts. Unknown writes, mixed prepared and journaled progress, no-op outcomes, and publication remain governed by their separate proof paths and cannot be closed by prepared qualification.

`runs acquire-handoff` takes the same invocation flags plus the exact `--artifact-id` and `--artifact-digest sha256:DIGEST` returned by the upload step. The artifact name is derived from the workflow target and invocation, with suffix `-handoff-00`. Default `--purpose apply` requires policy-valid native plans and a complete frontier or one positively validated restored source. `--purpose transport` conveys evidence without apply authority. Never select a handoff by a prefix search or upload an intermediate package under the final name.

The local context actions take raw JSON through one named input: `context-start`, `context-observe`, and `context-observe-source` use `--input context=FILE`; `context-record-plan` uses `--input plan=FILE`. `context-phase` takes `--phase` and optional `--reason`; `context-mark-plan` takes `--name` and `--status`. `context-capture-journal` and `context-install-journal` take `--name` and `--journal-root`. These actions validate and persist manifests; they do not grant authorization. Record only a separately reviewed, nonempty native plan. Same-run cleanup may append only through the declared completed-merge parent contract with its full retained proof.

`runs acquire-publication-candidate` takes the invocation flags, `--workflow-sha`, and the exact verifier upload ID/digest. Run it only on a fresh package containing the recovery frontier and observation. The verifier archive contains exactly `publication/candidate.json`, `publication/patch.diff`, `publication/result.json`, and `events/trigger-event.json`. Acquisition validates the current attempt, source, raw bytes, upstream artifact metadata, and unique successful verification job before retaining those files plus candidate archive/metadata/jobs evidence. It creates no intent or qualification. Existing or recovered evidence cannot be replaced.

After separate publisher preparation, `runs verify-publication` takes the invocation flags and `--workflow-sha`. Before any publication acknowledgement, it checks the source candidate, raw files, immutable upstream/candidate artifacts, and unique successful verification job. It exclusively creates `publication/qualification.json`; a different existing qualification cannot be overwritten. Recovery retains the original qualification even when an observer runs newer control source. Only the positively recoverable PR-verification stage can resume; unknown ref push and PR creation remain held.

`runs finalize` takes `--workflow`, `--run-id`, `--attempt`, `--package-root`, actual normal upload ID/digest, and a new direct-child `--checkpoint` destination. It derives the actual workflow SHA in Actions when `--workflow-sha` is omitted; source qualification outside Actions requires an explicit `--workflow-sha`. It validates terminal plan journals/results or publication proof and the exact uploaded archive inventory. A valid prepared upload creates an open prepared-frontier checkpoint, not a settlement. Later, only actual terminal native observer receipts can create the proof that closes that frontier; ordinary terminal attempts append their exact source and observer in order. A nonterminal or unqualified context returns `pending` without a checkpoint.

Every command returns the native schema-version 2 machine envelope. `--github-output` optionally appends bounded single-line data to an existing regular Actions output file. `runs digest --input document=FILE` hashes the complete raw JSON document with lexical number tokens preserved, including any existing result envelope.

## Declarative consumer policy

The strict policy has `schema_version: 1` and a `workflows` map keyed by exact `.yml` or `.yaml` filenames. Each workflow contains `mutator_step_alternatives`, empty `reviewed_source_shas`, boolean `allow_publication`, and a `plans` map. Each plan declares `command`, `domain_profile`, `allowed_operation_kinds`, `attempt_target`, `approval`, `event`, and nullable `parent_merge`. Unsupported fields, profiles, or approval contracts are rejected. This is data; it cannot execute consumer code. Keep `reviewed_source_shas` empty: historical source qualification from a run's code head is unsupported.

Use a stable whole-workflow concurrency group with `cancel-in-progress: false` and scalar `queue: max`. The default single pending slot discards waiting invocations before receipt persistence. GitHub permits up to 100 pending runs and cancels overflow; queue order is based on when runs start waiting, not guaranteed dispatch order. Canceled or out-of-order predecessors remain visible and held without positive settlement evidence. Never turn queue cancellation into a no-write receipt or truncate history to clear it.

Current no-op approval uses `kind: local-noop`, `workflow_source_sha256` (the raw trusted workflow file hash), and `mutators`, an exhaustive array of exact job display names and mutation-capable step names. Native plan entries may independently declare `prepared_recovery` with `workflow_source_sha256` and an exhaustive `mutators` inventory for that plan. Every pending plan being qualified must resolve to the same raw workflow digest. When changing a source, retain the prior hash and exact inventory in optional `previous_sources`, an array of objects with exactly `workflow_source_sha256` and `mutators`. Prior entries validate retained positive evidence only; they cannot qualify a new invocation under old source or classify legacy attempts. No-op evidence and prepared native plan evidence use separate policy entries and separate finalization paths.

## Optional unknown-history boundary

CLI 0.4.0 supports a separately reviewed, preview-only `history_cutover` in chain schema 6. It preserves a complete legacy inventory with unknown outcomes outside the native settlement list. It never substitutes an age cutoff for evidence or imports a fabricated native result. Default schema-4/5 recovery remains strict. Read [history-cutover.md](history-cutover.md) for capture, independent review, revocation, scope and immutable-prefix contracts. Native current-run no-op evidence still follows the rules below.

CLI 0.5.0 supports schema 7 for [explicit fresh-native promotion](history-promotion.md). Its `history_promotion` embeds the exact schema-6 preview checkpoint and a separately reviewed selected native scope. It preserves the preview's native prefix and quarantined baseline; it does not copy old unknown attempts into native settlements. Publication is excluded. Every native attempt continues to require the complete ordinary policy, acquisition, before-state, approval, journal, prepared frontier and terminal proof contracts. Review and policy drift hold work, including context commands in opted-in workflows.

Large captures can use [baseline schema 2](history-archive.md) inside schema-6/7 checkpoints. This preserves a bounded compressed stream of complete raw records, not a summary of history. The outer baseline digest remains the review identity through current no-op receipts and fresh-process checkpoint acquisition. File/checkpoint, complete-inventory, immutable-prefix, active-attempt and native-lineage guards remain in force.

## Current no-op evidence

Create a prepared empty-dispatch context only after the workflow chooses a plan-free outcome. Keep `settlement-chain.json`, `recovery-observation.json`, `events/trigger-event.json`, and `decisions/workflow-noop.json`. The decision has these exact identity fields, plus only its decision-specific `proposal` or `previews` evidence:

```json
{
  "schema_version": 1,
  "decision": "no-change",
  "repository": "owner/repo",
  "server_url": "https://github.com",
  "workflow_file": "task.yml",
  "run_id": 123,
  "attempt": 1,
  "recovery_key": "exact-target-key",
  "attempt_target": {"event_sha256": "RAW_EVENT_SHA256"},
  "workflow_sha": "RUNTIME_CONTROL_COMMIT_SHA",
  "event_name": "workflow_dispatch",
  "event_sha256": "RAW_EVENT_SHA256"
}
```

The closed decisions are `no-change`, `preview-only`, `review-declined`, `prerequisite-unavailable`, `ineligible-trigger`, and `already-settled`. Only a fully settled rerun may use `already-settled`. `review-declined` additionally retains `proposals/candidate.json` and an explicit negative `proposals/review.json`, with `proposal: {candidate_sha256, review_sha256}` hashes of their raw bytes. An empty native v2 candidate may use `no-change` with only `proposals/candidate.json` and `proposal: {candidate_sha256, review_sha256: null}`; it must have no operations and belong to the exact repository. Proposals remain inert and never become executable context plans.

Use `preview-only` for a completed planning preview without application or review. Its decision includes `previews`, an array of 1–64 unique references with exactly `path` and `sha256`. Paths have the form `previews/<safe-name>.json`; hashes bind each complete raw JSON object. Retain every planning document separately, including multiple native candidates or a normalized v1 plan and its delta. Native v2 plans are parsed and checked against the exact repository; other JSON reports remain inert evidence. A preview decision has no `proposal` or fabricated review. The receipt contains zero operations and grants no apply authority. Missing outputs use a truthful prerequisite failure; an apply request must never be classified as a preview by the caller.

Invoke `finish-noop` only after all mutation-capable jobs report positively skipped, or every declared mutation-capable step reports positively skipped. It fetches the trusted raw workflow source and complete current-attempt jobs itself. Started, queued, missing, duplicated, cancelled, or successful mutation steps cannot establish a no-op. It binds the no-op to the exact predecessor frontier and retains the source and jobs witnesses, then creates `plans/workflow-noop.json`, its native journal, and `apply-results/workflow-noop.json` before writing the completed context last. Do not edit these files to manufacture completion.

## Holds and bounded acquisition

Archives permit at most 32 MiB compressed data, 1,000 entries, and 8 MiB per retained file; reject symlinks, traversal, duplicates, extra uploaded files, and changed bytes. A publication candidate ZIP is retained as one file and therefore also obeys the 8 MiB file limit. Cumulative checkpoint and normal recovery acquisition each stop at 1,024 artifacts or 256 MiB. A limit never creates a history cutoff. Retain the typed hold and positively settled prefix. Authenticated checkpoint compaction is a future capability.

Legacy attempts without native terminal receipts or authenticated source proof remain unresolved. Report that limit separately from qualification of the new implementation. Do not delete historical runs, add source allowlists, fabricate acknowledgements, or widen credentials as recovery shortcuts.
