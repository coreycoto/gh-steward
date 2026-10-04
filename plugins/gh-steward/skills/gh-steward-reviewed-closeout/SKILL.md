---
name: gh-steward-reviewed-closeout
description: Qualify review findings or epic closeout, then prepare and apply one reviewed follow-up plan with a durable audit comment.
---

Use this skill when a reviewed finding set needs backlog follow-ups and a
recorded closeout verdict. Obtain the consumer's exact repository, Project,
severity mapping, issue taxonomy, and governance policies. Follow the
consumer's delivery and authorization rules.

Validate the findings with `gh steward review closeout-validate` and capture
complete live source evidence. Build the review or epic-closeout summary with
`gh steward review closeout-audit`. Inspect its findings, proposed follow-ups,
epic identity, child and blocker evidence, and qualification verdict.

Set `REPOSITORY_URL` to the exact verified HTTPS URL of the selected repository.

Prepare the reviewed workflow:

```sh
gh steward closeout prepare --repo-root . --repo "$REPOSITORY_URL" --input summary=closeout-summary.json --policy closeout-policy.json --out closeout-prepare.json
gh steward plan extract --repo-root . --repo "$REPOSITORY_URL" --input envelope=closeout-prepare.json --outer-command review-closeout-prepare --plan-command review-closeout-apply --out reviewed-plan.json
```

The policy has exactly `review_backlog`, `governance_check`, and `backlog_audit`.
`review_backlog` identifies the existing Project and supplies consumer severity
and issue-type mappings. For ordinary review mode the two audit policies are
null. Epic closeout requires explicit governance and backlog audit policies
for the same repository and Project. Preparation refreshes the complete source
and rebuilds the proposed actions before recording the plan hash.

Review the complete plan and its exact hash. Apply within the user's authorized
scope using the separately approved hash:

```sh
gh steward closeout apply --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
```

The durable journal covers each follow-up and the final marked audit comment.
The comment derives assigned issue numbers from validated native acknowledgements
and a complete projected after-state. Retain the plan, journal, and receipts for
fresh-process recovery; completed operations do not dispatch again. An unknown
operation requires positive recovery evidence before later operations proceed.
Report the terminal verdict and any remaining consumer qualification explicitly.
