---
name: gh-steward-reviewed-review
description: Turn structured repository review findings into a live-evidence backlog plan with explicit issue taxonomy and Project mappings.
---

Use this skill when the user asks to prepare actionable review findings as backlog work. For conducting the review or deciding which findings to accept, use the appropriate review workflow; do not invent findings or policy choices.

Prepare with a structured `schema_version: 1` payload and explicit policy:

```sh
gh steward review prepare --policy review-policy.json --input payload=review-findings.json --out reviewed-plan.json
```

The payload has a nonempty `findings` array. Each finding requires `title`, `summary`, `issue_type` (`Initiative`, `Epic`, `Research`, `Enhancement`, `Bug`, or `Maintenance`), and `severity` (`critical`, `high`, `now`, `medium`, `next`, `low`, or `later`). Optional grouping requires both `group_key` and `backlog_title`; optional fields include `body`, `files`, `evidence`, `existing_issue`, `notes`, `blocked_by_issue_numbers`, and `parent_issue_number`.

The policy supplies the exact `project` scope (`host`, `owner`, `owner_type`, `number`, `id`, `title`), `severity_to_priority` mappings, and `issue_type_labels` mappings. The command validates referenced labels and reconstructs the operations from the captured complete live issue and Project catalog. Review issue identity, labels, relationship effects, before-values, source completeness, operations, and `sha256` before requesting approval.

Apply only after the user explicitly approves that exact plan:

```sh
gh steward review apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
```

Apply accepts only the reviewed plan. The hash verifies the artifact; it is not approval. Preserve the journal and plan after an interrupted or unknown result. Never replay or replace an uncertain operation without positive same-plan recovery evidence.
