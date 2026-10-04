---
name: gh-steward-reviewed-backlog
description: Inspect, prepare, review, or apply explicitly approved GitHub backlog changes with gh-steward, including issue creation and edits, milestones, Project fields, relationships, rebalancing, and eligible archive changes.
---

Use this skill when the user asks to audit, inspect, or change a GitHub issue backlog. For goal definition, sequencing, and general backlog decision-making, compose with an available general project-management workflow skill; use this skill only for the gh-steward-specific source, plan, and apply contract.

## Audit

`backlog audit` is read-only analysis of an all-state issue graph joined to one explicit Project. Capture it with `gh steward snapshot issues --state all --join-project` and the exact Project selectors, then run:

```sh
gh steward backlog audit --policy backlog-audit-policy.json --input issue_graph=issue-graph.json
```

The consumer-owned policy identifies the repository and Project, issue-type title-prefix taxonomy, excluded prefixes, status field and valid/done values, priority field and Now/Next/Later mappings, and queue-order field. A complete queue snapshot is optional corroborating evidence via `--input queue_snapshot=queue.json`; when supplied, it must match the graph's repository, Project, and membership. Audit findings are diagnostic and do not authorize a rebalance.

## Inspect and prepare

1. Confirm the intended GitHub host, repository, account, and explicit GitHub Project scope. Check `gh auth status`; authentication does not authorize a write.
2. Read current state with `gh steward snapshot backlog` or `gh steward snapshot queue` using the intended repo root and explicit Project selectors. A live apply plan needs complete live issue and Project snapshots. Treat offline planning outputs and incomplete snapshots as previews only.
3. For a rebalance, obtain the user's authored decision input and explicit queue policy. Do not infer the Now/Next/Later assignment, Project option mappings, rank step, linked-pull-request policy, or archive eligibility. Use `gh steward backlog prepare` with the named `payload`, the queue `policy`, explicit `options` containing `queue_mode` and positive integer `rank_step`, and the intended Project owner/type/number. `status_values` is needed only when a generated write changes status; it maps canonical `Todo` or `Done` to an actual Project option. Optional `linked_pr_policy` must define `marker_prefix` and `pr_number_pattern`; archive decisions then require captured live linked-PR evidence. A prior audit may be supplied as `backlog_audit`, but does not replace live source validation.
4. Inspect the entire prepared plan: repository and Project identity, live and complete source evidence, every operation and its before/after values, archive prerequisites, and the exact `sha256`. Confirm the proposed changes match the user's intent. The generated plan is not self-approval.

## Apply

For a mixed issue change set, use `backlog-mutations prepare --input payload=authored-issues.json --input projects=selected-projects.json`. The authored payload contains exactly `schema_version: 1` and a nonempty `issues` array. Each entry identifies an existing issue with `issue_number` or a new issue with `client_id`. New issues require `title` and `body`; existing issues need an explicit change. Optional fields are `title`, `body`, `labels`, nullable milestone title, `project`, `relationships`, and boolean `allow_duplicate_title`.

The Project input contains exactly `projects`, an array of selected identities with `host`, `owner`, `owner_type`, `number`, `id`, and `title`. Use an empty array when the intent has no Project changes. Resolve requested titles against complete owner discovery, reject missing or duplicate matches, and verify the selected Project identity. Do not infer an owner or a title match. The optional per-issue `project` object may name `title` and set `ensure_membership`, `status`, `priority`, and `queue_order`; queue order requires priority. Omitting its title is allowed only when the separate explicit scopes identify one Project. Review the exact field definitions and options in the captured plan.

The optional `relationships` object sets nullable `parent` and/or the complete desired `blocked_by` list. References identify existing issues with `issue_number` or earlier new entries with `client_id`. Inspect additions and removals separately. This path preserves the authored v1 intent inside a reviewed v2 live plan; it does not apply an unreviewed legacy payload.

Only after the user explicitly approves that exact plan, use the exact same file and hash:

```sh
gh steward backlog apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
gh steward backlog-mutations apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
```

Do not add selectors or policy to apply; its scope must come from the reviewed plan. The `--approve-plan-sha` value is an artifact-integrity check, not authorization. If the user has not approved the exact proposal, stop after preparing and explaining the plan.

Apply returns verified primitive `receipts`, including provider acknowledgements for created issues and subsequent operations. Preserve those receipts with the original plan and journal; a matching remote issue alone does not prove an uncertain creation succeeded.

Archive operations may include separate close, Done-status, and archive writes. Review each primitive and its prerequisites; never collapse these into a single assumed mutation.

If apply reports an unknown or interrupted outcome, preserve the original plan and the checkout-local `.artifacts/gh-steward/journals/` records. Do not make a replacement plan or manually replay an operation. Continue only through the same plan's read-only positive reconciliation path, which must block if it cannot prove the exact accepted write and fresh complete state. If the evidence remains unknown, stop and report the ambiguity.
