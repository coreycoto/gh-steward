---
name: gh-steward-reviewed-merge
description: Qualify a specific GitHub Actions workflow-run pull request candidate and prepare its exact reviewed merge operation.
---

Use this skill only when the user asks to evaluate or prepare a particular automated merge candidate. Merging is a repository mutation; eligibility or a passing CI run is not user approval.

Provide the selected GitHub `workflow_run` event as `event` and an explicit policy as `policy`:

```sh
gh steward merge prepare --policy merge-policy.json --input event=workflow-run-event.json --out reviewed-plan.json
```

The policy contains `workflow_name`, `workflow_event`, `required_label`, `pass_bucket`, `merge_method`, `branch_patterns`, and `trusted_logins`. These are consumer decisions; do not infer trusted automation, branch patterns, or merge method. Review the captured repository, triggering event, pull request number and exact head, labels, required checks, eligibility findings, before-state, operation, and `sha256`.

Apply only after the user explicitly approves merging this exact candidate:

```sh
gh steward merge apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
```

Apply rejects selectors and policy overrides. Preserve the original plan and journal after any unknown outcome; recovery must positively reconcile the same exact-head merge without re-dispatching an uncertain merge.
