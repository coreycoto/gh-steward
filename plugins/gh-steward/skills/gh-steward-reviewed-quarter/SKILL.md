---
name: gh-steward-reviewed-quarter
description: Prepare reviewed GitHub issue and milestone changes from an authored quarterly commitment plan.
---

Use this skill when the user asks to commit selected issues to a quarter or update the associated milestone and rationale. The quarter plan records decisions; the tool does not choose issues or invent rationale.

The authored payload is a structured object with exactly these fields:

```json
{
  "schema_version": 1,
  "quarter": "2026 Q4",
  "quarter_goals": ["A goal chosen by the user"],
  "active_tracks": ["A named track"],
  "commit_issue_numbers": [123],
  "issue_rationale": {"123": "Why this issue is committed to the quarter."}
}
```

`quarter` must be `YYYY QN`; issue numbers are positive whole numbers, and rationale keys must be issue numbers with nonempty text. Prepare and inspect the exact live-backed proposal:

Set `REPOSITORY_URL` to the exact verified HTTPS URL of the selected repository.

```sh
gh steward quarter prepare --repo-root . --repo "$REPOSITORY_URL" --input payload=quarter-plan.json --out quarter-prepare.json
gh steward plan extract --repo-root . --repo "$REPOSITORY_URL" --input envelope=quarter-prepare.json --outer-command quarter-backlog-prepare --plan-command quarter-backlog-apply --out reviewed-plan.json
```

Review the complete source inventory, current milestone assignments, milestone fields, comments, per-issue rationale, before-values, operations, and `sha256`. Applying can create/update a milestone, add rationale comments, and change issue milestone assignments as separate writes.

Apply only after explicit approval of that exact plan:

```sh
gh steward quarter apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
```

On interruption or an unknown result, preserve the same plan and journal. Recovery uses durable receipts plus fresh complete source checks; do not assume an absent artifact proves a prior write did not occur or retry it blindly.
