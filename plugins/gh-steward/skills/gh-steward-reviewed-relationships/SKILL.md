---
name: gh-steward-reviewed-relationships
description: Audit, plan, validate, or apply explicitly approved GitHub issue dependency and parent-child relationship changes with gh-steward.
---

Use this skill when the user asks to review or change issue relationships. For broader project planning and dependency decisions, compose with an available general project-management workflow skill. This skill covers only the gh-steward commands and their evidence and approval requirements.

1. Confirm the intended repository, GitHub host, and authenticated account with `gh auth status`. Authentication is not permission to write.
2. Read a complete live all-state issue graph with `gh steward snapshot issues --state all`. Preserve the exact repository identity and review the current dependency and hierarchy structure.
3. Use user-authored relationship intent with `gh steward relationships audit`, `validate`, `plan`, or `prepare`. Provide hierarchy policy explicitly when the requested action needs it; never invent a policy or parent/child choice.
4. For a native apply plan, run `gh steward relationships prepare` and inspect the exact repository, captured graph, source completeness, every operation's before/after relationships, cycle/identity constraints, and plan `sha256`. Offline or pure planning results are not live-qualified apply plans.

Set `REPOSITORY_URL` to the exact verified HTTPS URL of the selected repository.
For native apply, the prepare output is an envelope; extract its inner plan:

```sh
gh steward relationships prepare --repo-root . --repo "$REPOSITORY_URL" --input payload=relationship-intent.json --out relationships-prepare.json
gh steward plan extract --repo-root . --repo "$REPOSITORY_URL" --input envelope=relationships-prepare.json --outer-command relationship-prepare --plan-command relationship-apply --out reviewed-plan.json
```

Add `--input hierarchy_policy=...` only when the requested relationship action needs explicit hierarchy policy.

5. Apply only after explicit user approval of that exact plan:

   ```sh
   gh steward relationships apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
   ```

   The flag verifies the chosen artifact; it is not human approval or a grant of GitHub access. Apply must use only the reviewed plan input.

If a mutation has an unknown outcome, preserve the reviewed plan and `.artifacts/gh-steward/journals/` state. Do not create a replacement plan or replay the write. Require positive reconciliation of the same operation before continuing, and stop if evidence is inconclusive.
