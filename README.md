# gh-steward

`gh-steward` is a GitHub CLI extension for inspecting repository state and preparing reviewed governance, issue, Project, relationship, delivery, merge, execution, branch cleanup, and workflow-run artifact operations. It keeps its reusable planning logic in Go and reads and writes GitHub through the authenticated GitHub CLI transport.

The tool separates source installation, plugin discovery, GitHub authentication, and permission to mutate a repository. Installing either the extension or its Agent Plugin does not authorize a GitHub write. Applying a plan requires separate user approval of the exact plan hash.

## Install the GitHub CLI extension

After the first tagged release is published, install the executable with an explicit version pin:

```sh
gh release verify v0.1.0 --repo coreycoto/gh-steward
gh extension install coreycoto/gh-steward --pin v0.1.0
gh steward version --json
```

The initial release builds native assets for `darwin-amd64`, `darwin-arm64`, `linux-amd64`, and `linux-arm64`. Apply support is qualified only on those four targets. The release also includes `checksums.txt` and GitHub artifact provenance attestations.

Before using an installed binary in an automation or consumer adapter, verify `gh steward version --json`. Its `tool_version`, `source_revision`, `source_dirty`, `target`, and `go_version` identify the binary. A release binary must report the requested release version, the immutable tagged commit, `source_dirty: false`, and one of the supported targets. Verify the downloaded binary against `checksums.txt` and its GitHub attestation when acquiring a release asset directly.

For consumer adapters that need an exact release asset without changing a runner's global extension installation, use the repository acquisition script:

```sh
scripts/release/acquire-gh-steward.sh 0.1.0 SOURCE_COMMIT_SHA "$RUNNER_TEMP/gh-steward"
"$RUNNER_TEMP/gh-steward/gh-steward" version --json
```

Pass the expected 40-character source commit from the reviewed release and, when the consumer lock records it, the expected asset digest as the fourth argument. The script selects the current supported platform, downloads only that explicit tag, checks every published asset against the release checksum list, verifies the binary and checksum attestations against the release workflow, source commit, and tag ref, validates the binary's reported provenance, and stages it atomically. Its JSON output includes the resolved asset filename, SHA-256, target, exact source revision, and staged path. It never changes `gh`'s active extension installation. Never pass a moving branch or a "latest" release in place of the tag and source commit.

GitHub CLI authentication is configured separately. Run `gh auth status` and confirm the intended GitHub host and account before live reads. Authentication only identifies the GitHub principal; it does not grant approval for writes or expand repository permissions.

## Discover the Agent Plugin

This repository includes a portable Agent Plugins 1.0.0 package at `plugins/gh-steward/` and a repo-local marketplace at `.agents/plugins/marketplace.json`. Its focused skills cover setup, governance, backlog audit and rebalancing, review findings and closeout, quarter commitments, relationships, PR delivery, exact-head merge, execution synchronization, branch cleanup, and run artifact deletion. They complement general project-management workflows; they do not replace goal setting, prioritization decisions, or user review.

Add this repository as a Codex marketplace source with `codex plugin marketplace add coreycoto/gh-steward`, then inspect the source with `codex plugin marketplace list`. Installing or enabling the plugin makes its instructions discoverable. It does not install the `gh` executable, the `gh-steward` extension, or authenticate with GitHub.

For a pinned consumer, register the qualified source commit and select its package:

```sh
codex plugin marketplace add coreycoto/gh-steward --ref QUALIFIED_SOURCE_COMMIT
codex plugin add gh-steward@gh-steward-marketplace
```

## Inspect, prepare, and apply

Use `gh steward help` for the complete command contract. The principal live inspection commands include:

```sh
gh steward snapshot repo --repo-root .
gh steward snapshot issues --repo-root . --state all
gh steward snapshot project --repo-root . --project-owner OWNER --project-owner-type Organization --project-number NUMBER
gh steward snapshot projects --repo-root . --project-owner OWNER --project-owner-type Organization
gh steward snapshot backlog --repo-root . --join-project --project-owner OWNER --project-owner-type Organization --project-number NUMBER
```

Project selectors must identify the intended live Project. Owner discovery returns the complete collection accessible to the authenticated principal, preserving immutable owner identity across pages. Consumers resolving an exact title must reject missing or duplicate matches and then read the selected Project by number and ID. Supply an explicit queue policy when requesting a queue snapshot. Snapshot output is evidence for review, not permission to mutate.

Live `prepare` commands read the selected repository through the authenticated GitHub CLI transport and write a schema-version 2 result envelope. Extract its inner plan before applying: set `REPOSITORY_URL` to the exact verified HTTPS URL of the selected repository, then pass the command IDs for that prepare result and inner plan to the provider-free `plan extract` command. It verifies the envelope, repository, source evidence, hash, and commands, then writes the canonical plan without changing signed numeric tokens.

For example, extracting an execution plan uses:

```sh
gh steward plan extract --repo-root . --repo "$REPOSITORY_URL" --input envelope=execution-prepare.json --outer-command execution-sync-prepare --plan-command execution-sync --out reviewed-plan.json
```

Use `gh steward plan validate --repo-root . --repo "$REPOSITORY_URL" --input plan=reviewed-plan.json --plan-command execution-sync` to validate a standalone plan before registering it in a local workflow context. Validation checks the plan's canonical shape, repository, command, complete live source evidence, capture time, and digest; it writes only a small result envelope to stdout and leaves the plan unchanged. Inspect the complete extracted plan, including its repository and Project identities, source completeness, before-state values, operations, and `sha256`, before requesting approval.

For a backlog audit, provide an all-state issue graph joined to the exact Project and an explicit policy describing repository/Project identity, issue taxonomy, excluded prefixes, status values, priority mappings, and queue-order field. The optional queue snapshot is corroborating evidence; the audit reads raw graph fields so it can report malformed values instead of treating them as authorized queue state:

```sh
gh steward snapshot issues --repo-root . --state all --join-project --project-owner OWNER --project-owner-type Organization --project-number NUMBER --out issue-graph.json
gh steward backlog audit --policy backlog-audit-policy.json --input issue_graph=issue-graph.json
```

For a backlog rebalance, provide authored decisions as `payload`, the explicit queue `policy`, and `options` containing `queue_mode` and positive whole-number `rank_step`. Optional `status_values` maps only emitted canonical Todo/Done status writes to actual Project options. Optional `linked_pr_policy` must explicitly provide `marker_prefix` and `pr_number_pattern`; the CLI captures complete live linked-PR evidence when archive decisions need it. Select the Project with `--project-owner`, `--project-owner-type`, and `--project-number` (and `--project-id` when known). Never infer consumer option names or archive eligibility.

Mixed issue changes use `backlog-mutations prepare --input payload=FILE --input projects=FILE`. The authored v1 payload contains `schema_version: 1` and a nonempty `issues` array for explicit creation, edits, milestone assignment, Project fields, and parent/dependency changes. Each entry has an existing `issue_number` or new `client_id`; client references name earlier creations. The separate Project input contains `projects`, an array of exact `host`, `owner`, `owner_type`, `number`, `id`, and `title` identities, or an empty array when none are requested. The tool captures complete live evidence and retains the authored intent in a reviewed v2 plan. See the [backlog skill](plugins/gh-steward/skills/gh-steward-reviewed-backlog/SKILL.md) for field and recovery contracts.

Backlog review, governance, execution, and mutation planning use normalized Project field types and exact named-option identities; numeric queue values require a `NUMBER` field.

Review findings use `review prepare --policy FILE --input payload=FILE`. The policy explicitly identifies a Project and maps severity to consumer priority values and issue types to existing labels. The structured findings payload uses `schema_version: 1` and a nonempty `findings` array. Quarter planning uses `quarter prepare --input payload=FILE` with a structured `schema_version: 1` object containing `quarter` (`YYYY QN`), `quarter_goals`, `active_tracks`, `commit_issue_numbers`, and per-issue `issue_rationale`.

Exact-head merge preparation uses `merge prepare --policy FILE --input event=FILE`. The event is a GitHub `workflow_run` event; policy explicitly supplies `workflow_name`, `workflow_event`, `required_label`, `pass_bucket`, `merge_method`, `branch_patterns`, and `trusted_logins`. Review the captured pull request head, checks, labels, trigger, repository identity, and the resulting eligibility before approval.

Execution synchronization uses `execution prepare --input selector=FILE --policy FILE`. The selector has exactly `issue_number`, `pull_request_number`, `skip_project_sync`, and nullable `project`; select one issue or pull request, and choose explicitly whether Project synchronization is skipped. The policy explicitly maps `statuses.done`, `statuses.active`, and `statuses.todo`, and supplies `status_field`, `pr_link_marker_prefix`, `pr_link_number_pattern`, `linked_issue_marker_prefix`, and `link_state_marker_prefix`.

Use `snapshot execution --input selector=FILE --policy FILE` with the same selector and policy for read-only verification. It captures the linked issue, exact PR head, complete closing references, branches and selected Project without creating a mutation plan or journal.

Run artifact deletion uses `artifacts prepare --input payload=FILE`, where the payload has exactly `run_id`, `names` (exact, case-sensitive names), and boolean `ignore_missing`. It captures the complete run inventory and plans deletion of every matching artifact ID for each requested name. When `ignore_missing` is false, existing matches are still deleted, then the command reports a completed receipt with a nonzero outcome if any requested names were absent. Inspect this partial-success behavior before authorizing cleanup.

Governance uses `governance prepare --input payload=FILE`, where the payload contains exactly `kind` and `policy`. The closed kinds are `bootstrap-backlog`, `governance-fix`, `label-palette`, `drift-issue`, and `execution-state`; each takes explicit consumer policy. Bootstrap targets an existing Project, while governance fixes normalize issue labels and caller-supplied milestone targets. Execution state selects one issue and an explicit issue/Project status and optional marked comment. The tool does not invent Project, taxonomy, calendar, label ownership, or priority defaults. See the [governance skill](plugins/gh-steward/skills/gh-steward-reviewed-governance/SKILL.md) for policy fields.

Run `gh steward help` for the complete command map, including offline pure planning commands and snapshot inputs. For reviewed operations, apply accepts only the saved reviewed plan; it rejects new selectors and policy overrides.

Review closeout uses `closeout prepare --input summary=FILE --policy FILE` with the reviewed `review closeout-audit` summary. Its explicit policy contains `review_backlog`, `governance_check`, and `backlog_audit`; the audit policies are null for ordinary review mode and exact consumer policies for epic closeout. One reviewed plan covers the follow-up changes and marked audit comment. The final comment uses only validated created-issue acknowledgements and the complete projected after-state, so assigned issue numbers are recorded without changing the reviewed action set. See the [closeout skill](plugins/gh-steward/skills/gh-steward-reviewed-closeout/SKILL.md).

PR delivery uses `delivery prepare --input payload=FILE` with `kind: open-pr` or `kind: finish` and the exact corresponding policy. One reviewed plan combines PR creation or exact-head merge with the linked issue, Project and marked-comment follow-up. Interactive finish captures complete required checks and current mergeability; scheduled workflow-run merge retains its separate event and trust policy. Inspect server-owned automatic branch deletion and any dependent open PRs before approval. See the [delivery skill](plugins/gh-steward/skills/gh-steward-reviewed-delivery/SKILL.md).

Remote branch cleanup uses `branches prepare --input payload=FILE` with exactly `branches`, an array of selected `name`, `sha` and `pull_request_number` objects. Preparation requires the exact merged same-repository PR head, a non-default branch and no open PR using it as a base or head. Draft PRs also protect the branch. Native preparation retains complete branch inventory and the server's current branch retention setting. A present branch must match its reviewed SHA; a positively absent branch becomes a typed `data.already_absent` outcome with no deletion operation. Apply rechecks this evidence even for an all-absent plan and returns empty receipts after completing its zero-write journal. It never claims that this attempt removed an already-absent branch. Delivery with branch removal allows its own selected PR to remain open until the reviewed merge, while other open heads block removal. Actual deletion uses an explicit Git ref lease in isolated scratch. Local checkout cleanup remains a consumer operation. See the [branch cleanup skill](plugins/gh-steward/skills/gh-steward-reviewed-branches/SKILL.md).

After the user explicitly approves the exact plan, pass that same plan and its exact hash to the corresponding apply command:

```sh
gh steward backlog apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward backlog-mutations apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward relationships apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward review apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward quarter apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward merge apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward execution apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward artifacts apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward governance apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward closeout apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward delivery apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward branches apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
```

Apply takes its repository, Project, policy, and mutation scope from the reviewed plan. `--approve-plan-sha` checks that the supplied artifact matches the reviewed hash; the flag itself is not user approval or a GitHub permission grant. Do not invent or fill in a hash on the user's behalf.

The extension records per-operation intent and results in a private, checkout-local journal under `.artifacts/gh-steward/journals/`. If a write has an unknown outcome, preserve the original plan and journal. Do not construct a replacement plan or manually replay a write. Recovery must positively reconcile the exact operation before continuing; an absent or matching value alone is not proof that an uncertain write did or did not happen. If the evidence is inconclusive, stop and request a fresh human decision.

## Recover isolated workflow attempts

The released 0.2.0 executable adds `runs` commands for shared workflow recovery; 0.1.0 does not contain them. These commands read GitHub Actions history and persist local evidence; provider mutations remain in the separately authorized domain apply commands.

Consumers keep a reviewed `.agents/gh-steward-recovery-policy.json` in their trusted control checkout. It declares exact workflow files, mutation steps, plan profiles, approval/event contracts, and optional publication support. `runs recover` scans complete workflow attempts and artifact inventories, validates immutable checkpoints, and returns `fresh`, `resumed`, `terminal`, or `recovery_needed`. A held predecessor prevents fresh work. A settled rerun returns `terminal` and cannot prepare another target. The scanner does not infer safety from an old skipped step, missing artifact, or unsupported plan.

Serialize the complete workflow with a stable repository/workflow concurrency group, `cancel-in-progress: false`, and `queue: max`. GitHub's default single-pending queue cancels a waiting invocation when another arrives, before that invocation can save a receipt. Its maximum queue retains up to 100 pending runs; overflow cancellation still creates an unresolved predecessor. Queue processing does not guarantee dispatch order, so recovery must retain and check every predecessor even with this configuration. Preserve a hold for bounded review instead of inferring no writes or automatically replaying a canceled target. See [GitHub's concurrency contract](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency).

In GitHub Actions, the CLI requires the control checkout HEAD and policy bytes to match `GITHUB_WORKFLOW_SHA`. Source-qualified commands require `--workflow-sha` to equal that runtime value, and `context-start` binds its input's `trusted_source_sha` to it without an additional flag. `finalize` derives the runtime value when its flag is omitted. Trigger/source commits cannot substitute for the executed workflow's revision. Outside Actions, explicit source arguments support offline qualification.

Invocation packages are real direct children of `RUNNER_TEMP`. Transfer one final preparation package through an immutable upload ID and SHA-256 digest with `runs acquire-handoff`. Its default `apply` purpose validates all native plans, approval evidence, and predecessors. `transport` transfers evidence for publication or no-op handling without granting apply authority. Keep the original source plan, journals, event, review, and recovery proof byte-exact. Context commands manage the manifest; they do not create approval.

Before starting a recovery context, validate each saved standalone native plan with `plan validate`. Register a Go-produced plan directly with `runs context-record-plan --input native-plan=PLANFILE --name NAME --relative PACKAGE_RELATIVE_PATH --plan-command EXPECTED`, together with the trusted checkout, policy, runner-temp, and package-root arguments required by `runs`. The command revalidates the plan's repository and command and preserves its canonical bytes. Optional review metadata uses `--review-path` and `--review-sha256` together. Context registration records plan identity; it does not approve or apply the plan.

Publication uses a trusted control checkout at the runtime's `GITHUB_WORKFLOW_SHA`. On a fresh invocation, `runs acquire-publication-candidate` acquires the isolated verifier's exact upload ID and digest, retaining its four source files, raw archive, metadata and successful job evidence. Prepare the exact intent separately, then use `runs verify-publication` to revalidate it and exclusively write `publication/qualification.json`. Preserve that qualification before the first publisher write. Recovered publication retains its original candidate and qualification. Unknown push or PR creation outcomes remain held. `runs finalize` requires the actual normal upload receipt; it derives the trusted workflow SHA in Actions and requires an explicit `--workflow-sha` for publication outside Actions. It verifies the archive's exact file inventory before creating a new checkpoint.

`runs finish-noop` can close a new qualified invocation only after complete current-attempt job evidence proves every declared mutation-capable step was skipped. It creates a real native zero-operation plan, journal, and result. It cannot erase a plan or classify an older ambiguous attempt. Workflow-source changes retain explicitly reviewed prior qualified source digests and mutation inventories in `previous_sources`; this preserves existing positive proofs without qualifying legacy runs.

Archive acquisition is bounded to 1,024 checkpoint artifacts and 256 MiB per scan category, with 32 MiB archives, 1,000 entries and 8 MiB individual files. Exceeding a budget creates a hold; it never truncates history. Authenticated checkpoint compaction is not implemented. See the [workflow-recovery skill](plugins/gh-steward/skills/gh-steward-workflow-recovery/SKILL.md) and its [protocol reference](plugins/gh-steward/skills/gh-steward-workflow-recovery/references/run-protocol.md) for integration contracts.

Legacy attempts without native receipts remain held until their exact archived evidence is independently reviewed. The provider-free `runs legacy-review` command verifies a signed evidence packet; a separate read-only preview and explicit local import can append a provenance-preserving chain-v5 outcome. These commands print summaries only; `--out` writes the full envelope to a private mode-0600 checkout file. Explicit legacy checkpoints must end at their final reviewed import and are compared with hosted checkpoints before recovery. See the [legacy evidence reference](plugins/gh-steward/skills/gh-steward-workflow-recovery/references/legacy-evidence.md) for raw input contracts and trust boundaries. This does not create native plans, journals, or provider receipts.

When legacy evidence has expired, 0.4.0 adds an opt-in [reviewed history cutover](plugins/gh-steward/skills/gh-steward-workflow-recovery/references/history-cutover.md). `runs cutover-preview` captures complete terminal run/attempt inventories, current artifact metadata and selected same-repository issue/PR reads. Every old outcome remains `unknown` and quarantined. It never imports or replays those operations. `runs cutover-validate` checks the retained baseline offline; it does not approve or activate it.

An explicitly reviewed baseline can start a schema-6 **preview-only** recovery chain. New attempts still require native evidence and any later legacy rerun remains pending. The baseline grants no apply or publication authority, and a schema-6 chain cannot register executable plans, resume mutations or promote a publication. It may retain read-only proposals and positively qualified current-run no-op receipts. Exact review comes from a trusted policy digest or an independently configured issue-comment channel, rechecked against complete comments and the author's current write access. Keep failed recovery reports in separate diagnostic artifacts; a diagnostic upload never settles an attempt.

CLI 0.5.0 supports [explicit fresh-native promotion](plugins/gh-steward/skills/gh-steward-workflow-recovery/references/history-promotion.md). `runs promotion-preview` binds the exact preview checkpoint, trusted workflow policy, selected future plan scopes and current issue/PR reads. A separate issue review of that exact promotion can seed schema 7; every checkpoint retains the same quarantined baseline and promotion. Capture and offline validation do not activate it. Native plans still require their own approval, complete live before-state and operation inventory, durable journals and terminal receipts. Promotion excludes historical replay, historical settlement and publication. Opted-in context commands recheck review and current reviewer permission; changed policy, lost lineage or revocation blocks native work.

CLI 0.5.1 captures schema-2 promotions with a sealed comparison contract that tolerates only tag-publication changes to PR `base.repo.pushed_at` and `head.repo.pushed_at`. Complete raw evidence remains retained; every other field stays exact. Schema-1 grants keep full-response comparison. Qualify the exact released executable before adoption. Diagnostic-only failed attempts require the separate proof below and must not be hidden by a new baseline or treated as successful native receipts.

For an explicitly selected failed first attempt after that unchanged baseline, CLI 0.5.1 supports `promotion-preview --held-run-id ID`. Schema-3 promotions retain the exact diagnostic ZIP, GitHub's authenticated executed-workflow file, reviewed exhaustive mutation inventory and complete positively skipped mutators. This separate `held_attempts` ledger covers only proved absence of dispatch; it creates no native completion, permits no rerun of the held run and never changes the original quarantine. Capture and first fresh admission recheck the evidence. Missing or ambiguous proof still holds. Qualify the exact released executable, review the exact new promotion and authorize activation before adoption.

CLI 0.5.0 also supports [complete large-history archives](plugins/gh-steward/skills/gh-steward-workflow-recovery/references/history-archive.md). Capture automatically uses a sealed schema-2 gzip/NDJSON baseline when the complete raw snapshot needs compression to fit the 8 MiB file/checkpoint budget; smaller schema-1 snapshots retain their format. Every raw run, exact attempt, artifact and selected state read is preserved. Checkpoints and ordinary files retain their 8 MiB bound. Archive limits are explicit: 4 MiB compressed, 64 MiB decompressed, 1 MiB per typed record, 16,384 JSON nodes per record and two million nodes in aggregate. Exceeding a limit holds capture or validation without truncation. Counts and byte identities appear in summaries; full provider evidence remains in the private artifact. Compression grants no review or activation authority.

## Development and release

Run tests and checks without GitHub credentials or live provider writes:

```sh
go test ./...
go test -race ./...
go vet ./...
go test -run '^$' -bench '^BenchmarkLCSForLargeQueue$' -benchtime=1x -count=1 ./internal/planning
scripts/release/check-build-matrix.sh
python3 scripts/release/check-plugin-package.py
```

Tagged releases require the matching portable plugin version and tool version. CI and the release workflow run provider-free journal, workflow, race and acquisition checks natively on all four supported targets. Publication waits for every target to pass at the exact tag. The release workflow builds from that clean tagged commit, stamps the binary with its version and source revision, publishes SHA-256 checksums, and requests GitHub artifact attestations. This repository does not publish a GitHub Actions Marketplace action.

See [AGENTS.md](AGENTS.md) for the contributor contract and [CHANGELOG.md](CHANGELOG.md) for user-visible changes.
