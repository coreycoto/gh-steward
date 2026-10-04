---
name: gh-steward-reviewed-execution
description: Prepare reviewed issue or pull-request execution synchronization with an explicit selector and consumer status/link policy.
---

Use this skill when the user asks to synchronize a specific issue or pull request's execution state, Project status, and maintained relationship/link comments. Preserve the selected item and consumer policy exactly.

`selector` must contain exactly `issue_number`, `pull_request_number`, `skip_project_sync`, and nullable `project`. Select exactly one positive issue or pull request number and set the other number to zero. If `skip_project_sync` is true, `project` must be null; otherwise provide the exact Project scope with `host`, `owner`, `owner_type`, `number`, `id`, and `title`.

The `policy` object has exactly `statuses` (`done`, `active`, `todo`), `status_field`, `pr_link_marker_prefix`, `pr_link_number_pattern`, `linked_issue_marker_prefix`, and `link_state_marker_prefix`. Map real consumer status values and stable comment markers explicitly; the tool does not invent them.

Prepare from live state and inspect issue/PR identity, linked pull-request evidence, Project membership and status, comment markers, before-state, operations, and `sha256`:

```sh
gh steward execution prepare --input selector=selector.json --policy execution-policy.json --out reviewed-plan.json
```

Apply only after explicit approval of the exact plan:

```sh
gh steward execution apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
```

Apply accepts no selector or policy overrides. If a write is interrupted or uncertain, preserve the plan and journal and use the same plan's positive recovery path. Do not repeat an issue, Project, or comment mutation based only on absence or a matching value.
