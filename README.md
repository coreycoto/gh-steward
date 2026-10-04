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

Live `prepare` commands read the selected repository through the authenticated GitHub CLI transport and produce a schema-version 2 reviewed plan. Inspect the complete plan, including its repository and Project identities, source completeness, before-state values, operations, and `sha256`, before requesting approval.

For a backlog audit, provide an all-state issue graph joined to the exact Project and an explicit policy describing repository/Project identity, issue taxonomy, excluded prefixes, status values, priority mappings, and queue-order field. The optional queue snapshot is corroborating evidence; the audit reads raw graph fields so it can report malformed values instead of treating them as authorized queue state:

```sh
gh steward snapshot issues --repo-root . --state all --join-project --project-owner OWNER --project-owner-type Organization --project-number NUMBER --out issue-graph.json
gh steward backlog audit --policy backlog-audit-policy.json --input issue_graph=issue-graph.json
```

For a backlog rebalance, provide authored decisions as `payload`, the explicit queue `policy`, and `options` containing `queue_mode` and positive whole-number `rank_step`. Optional `status_values` maps only emitted canonical Todo/Done status writes to actual Project options. Optional `linked_pr_policy` must explicitly provide `marker_prefix` and `pr_number_pattern`; the CLI captures complete live linked-PR evidence when archive decisions need it. Select the Project with `--project-owner`, `--project-owner-type`, and `--project-number` (and `--project-id` when known). Never infer consumer option names or archive eligibility.

Mixed issue changes use `backlog-mutations prepare --input payload=FILE --input projects=FILE`. The authored v1 payload contains `schema_version: 1` and a nonempty `issues` array for explicit creation, edits, milestone assignment, Project fields, and parent/dependency changes. Each entry has an existing `issue_number` or new `client_id`; client references name earlier creations. The separate Project input contains `projects`, an array of exact `host`, `owner`, `owner_type`, `number`, `id`, and `title` identities, or an empty array when none are requested. The tool captures complete live evidence and retains the authored intent in a reviewed v2 plan. See the [backlog skill](plugins/gh-steward/skills/gh-steward-reviewed-backlog/SKILL.md) for field and recovery contracts.

Review findings use `review prepare --policy FILE --input payload=FILE`. The policy explicitly identifies a Project and maps severity to consumer priority values and issue types to existing labels. The structured findings payload uses `schema_version: 1` and a nonempty `findings` array. Quarter planning uses `quarter prepare --input payload=FILE` with a structured `schema_version: 1` object containing `quarter` (`YYYY QN`), `quarter_goals`, `active_tracks`, `commit_issue_numbers`, and per-issue `issue_rationale`.

Exact-head merge preparation uses `merge prepare --policy FILE --input event=FILE`. The event is a GitHub `workflow_run` event; policy explicitly supplies `workflow_name`, `workflow_event`, `required_label`, `pass_bucket`, `merge_method`, `branch_patterns`, and `trusted_logins`. Review the captured pull request head, checks, labels, trigger, repository identity, and the resulting eligibility before approval.

Execution synchronization uses `execution prepare --input selector=FILE --policy FILE`. The selector has exactly `issue_number`, `pull_request_number`, `skip_project_sync`, and nullable `project`; select one issue or pull request, and choose explicitly whether Project synchronization is skipped. The policy explicitly maps `statuses.done`, `statuses.active`, and `statuses.todo`, and supplies `status_field`, `pr_link_marker_prefix`, `pr_link_number_pattern`, `linked_issue_marker_prefix`, and `link_state_marker_prefix`.

Use `snapshot execution --input selector=FILE --policy FILE` with the same selector and policy for read-only verification. It captures the linked issue, exact PR head, complete closing references, branches and selected Project without creating a mutation plan or journal.

Run artifact deletion uses `artifacts prepare --input payload=FILE`, where the payload has exactly `run_id`, `names` (exact, case-sensitive names), and boolean `ignore_missing`. It captures the complete run inventory and plans deletion of every matching artifact ID for each requested name. When `ignore_missing` is false, existing matches are still deleted, then the command reports a completed receipt with a nonzero outcome if any requested names were absent. Inspect this partial-success behavior before authorizing cleanup.

Governance uses `governance prepare --input payload=FILE`, where the payload contains exactly `kind` and `policy`. The closed kinds are `bootstrap-backlog`, `governance-fix`, `label-palette`, `drift-issue`, and `execution-state`; each takes explicit consumer policy. Bootstrap targets an existing Project, while governance fixes normalize issue labels and caller-supplied milestone targets. Execution state selects one issue and an explicit issue/Project status and optional marked comment. The tool does not invent Project, taxonomy, calendar, label ownership, or priority defaults. See the [governance skill](plugins/gh-steward/skills/gh-steward-reviewed-governance/SKILL.md) for policy fields.

Run `gh steward help` for the complete command map, including offline pure planning commands and snapshot inputs. For reviewed operations, apply accepts only the saved reviewed plan; it rejects new selectors and policy overrides.

Review closeout uses `closeout prepare --input summary=FILE --policy FILE` with the reviewed `review closeout-audit` summary. Its explicit policy contains `review_backlog`, `governance_check`, and `backlog_audit`; the audit policies are null for ordinary review mode and exact consumer policies for epic closeout. One reviewed plan covers the follow-up changes and marked audit comment. The final comment uses only validated created-issue acknowledgements and the complete projected after-state, so assigned issue numbers are recorded without changing the reviewed action set. See the [closeout skill](plugins/gh-steward/skills/gh-steward-reviewed-closeout/SKILL.md).

PR delivery uses `delivery prepare --input payload=FILE` with `kind: open-pr` or `kind: finish` and the exact corresponding policy. One reviewed plan combines PR creation or exact-head merge with the linked issue, Project and marked-comment follow-up. Interactive finish captures complete required checks and current mergeability; scheduled workflow-run merge retains its separate event and trust policy. Inspect server-owned automatic branch deletion and any dependent open PRs before approval. See the [delivery skill](plugins/gh-steward/skills/gh-steward-reviewed-delivery/SKILL.md).

Remote branch cleanup uses `branches prepare --input payload=FILE` with exactly `branches`, an array of selected `name`, `sha` and `pull_request_number` objects. Preparation requires a complete merged-PR inventory, the exact current branch SHA, a non-default branch and no open PR based on it. Deletion uses an explicit Git ref lease in isolated scratch. Local checkout cleanup remains a consumer operation. See the [branch cleanup skill](plugins/gh-steward/skills/gh-steward-reviewed-branches/SKILL.md).

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
