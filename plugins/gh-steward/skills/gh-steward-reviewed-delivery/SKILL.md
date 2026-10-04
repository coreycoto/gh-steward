---
name: gh-steward-reviewed-delivery
description: Prepare and apply one reviewed PR creation or merge closeout plan with issue and Project follow-up, exact source identity, and durable recovery.
---

Use this skill when a consumer composes opening a PR or finishing a linked
issue with its Project and comment policy. Read the consumer's current issue,
branch, project policy and authorized delivery stage first.

The portable plugin supplies guidance. Acquire the compatible native
`gh-steward` executable separately and use the existing native GitHub account;
installation and authentication never grant approval to write.

Prepare `payload.json` with exactly `kind` and `policy`:

- `kind: open-pr`: policy contains exactly `draft` and `execution_state`.
  The draft fixes title, body, head/base branch names, their 40-character SHAs
  and the draft boolean. Execution state uses the six fields described in
  `gh-steward-reviewed-governance`. Its work-comment body may include
  `{{pull_request_url}}` and `{{pull_request_number}}`; only the validated native
  creation acknowledgement can supply these values.
- `kind: finish`: policy contains exactly `pull_request_number`, `merge_method`,
  `keep_branch`, `execution_policy` and `execution_state`. The consumer supplies
  its retained status/link policy and explicit issue/Project target. Preparation
  requires the exact linked issue, current candidate and complete required-check
  evidence. Extra closing references and open PRs based on a branch being
  deleted require separate review. Server-owned automatic branch deletion is
  captured rather than changed implicitly.

```sh
gh steward delivery prepare --repo-root . --input payload=payload.json --out delivery-plan.json
```

Inspect the v2 plan's complete source identities, policy, before/after values
and ordered primitives. Record the human approval for this exact plan before
passing its digest; computing the digest is never approval.

```sh
gh steward delivery apply --repo-root . --input plan=delivery-plan.json --approve-plan-sha APPROVED_PLAN_SHA
```

The outer plan and one journal cover the PR write and its derived follow-up
primitives. Native acknowledgements are persisted before independent reads.
Created IDs cannot authorize extra operations beyond the reviewed template.
Merge dispatch checks the reviewed head; explicit remote deletion uses a Git
lease for the reviewed branch SHA in isolated scratch.

Preserve the exact plan, deterministic journal and acquisition receipt when
execution is interrupted. A fresh process may observe a durably acknowledged
write, then continue the remaining reviewed primitives. A write with no
positive retained acknowledgement remains unresolved and is never replayed.
Consumer-owned local checkout cleanup, deployment and data publication retain
their own delivery boundaries.
