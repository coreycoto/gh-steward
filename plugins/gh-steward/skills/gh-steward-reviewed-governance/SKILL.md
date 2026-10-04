---
name: gh-steward-reviewed-governance
description: Prepare and apply bounded repository governance changes with explicit taxonomy, label, milestone, issue execution state and existing Project policy.
---

Use this skill for coordinated repository governance changes. Obtain the
consumer's reviewed policy and exact repository identity before preparation.
Ordinary single-resource edits may use native `gh` or the client connector.

The preparation payload contains exactly `kind` and `policy`. Supported kinds
have distinct, closed policy contracts:

- `bootstrap-backlog`: an existing `project`, `typed_issue_labels` map,
  `issue_numbers` selection (empty selects all qualifying issues),
  `valid_statuses`, `status_field` and `target_status`. This adds selected issue
  membership and normalizes status; it does not create a GitHub Project.
- `governance-fix`: `governance_label_by_prefix`, `governance_labels`,
  `retired_labels` and explicit `milestones` entries containing `title`,
  `due_on` and `description`. Compute calendar targets from the consumer's
  policy; the tool supplies no quarter or taxonomy defaults.
- `label-palette`: `labels`, the complete explicit preferred palette entries
  used by `governance labels-plan`. Inspect label identity, color, description,
  ownership and proposed actions before applying.
- `drift-issue`: `title`, `body`, `maintenance_label`, `summary_markdown`,
  `unresolved_manual_drift`, `close_when_clean`, nullable `project`,
  `status_field`, `todo_status`, `done_status`, `priority_field`,
  `priority_value`, `queue_order_field` and `queue_order_step`.
- `execution-state`: exactly `issue_number` (a positive whole number),
  `issue_state` (`OPEN` or `CLOSED`), nullable `project`, `status_field`,
  `target_status` and nullable `work_comment`. A comment has exactly `marker`
  and `body`; capture and review existing marked comments before replacing
  one. This contract sets an explicit target rather than inferring intent from
  a branch name or a PR's current state.

A Project scope has exactly `host`, `owner`, `owner_type`, `number`, `id` and
`title`. Do not infer a Project from authentication, a current checkout or an
unrelated issue. Repository-local configuration formatting remains a consumer
operation; it does not need a GitHub mutation plan.

```sh
gh steward governance prepare --input payload=governance-request.json --out reviewed-plan.json
```

Inspect the exact repository, complete source inventory, before-state, closed
primitive list, affected labels/issues/milestones/Project fields and plan hash.
Apply only within the user's explicit authorization for that reviewed scope:

```sh
gh steward governance apply --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
```

The hash identifies the reviewed artifact; it does not grant permission.
Preparation and passing checks do not authorize a mutation. Preserve the plan
and journal after interruption. Recover through positive evidence of the same
operation; missing evidence or a matching value alone cannot justify replay.
