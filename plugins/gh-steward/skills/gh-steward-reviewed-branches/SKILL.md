---
name: gh-steward-reviewed-branches
description: Prepare and apply reviewed remote branch cleanup with complete merged-PR and dependency evidence, exact Git leases, and durable recovery.
---

Use this skill for a bounded selection of obsolete remote branches. Read the
consumer's cleanup policy and authorized delivery stage before selecting them.
Acquire the compatible native executable separately; installing this plugin
does not grant permission to delete branches.

The preparation payload contains exactly `branches`, an array of objects with
exactly `name`, `sha` and `pull_request_number`. Names must be valid Git branch
names, SHAs must be the complete current 40-character lowercase commit IDs,
and PR numbers must be positive whole numbers. Duplicate branches are rejected.
An empty selection is a verified no-op.

Set `REPOSITORY_URL` to the exact verified HTTPS URL of the selected repository.
Preparation writes a result envelope; extract and inspect its inner plan before
requesting approval:

```sh
gh steward branches prepare --repo-root . --repo "$REPOSITORY_URL" --input payload=branches.json --out branches-prepare.json
gh steward plan extract --repo-root . --repo "$REPOSITORY_URL" --input envelope=branches-prepare.json --outer-command branch-cleanup-prepare --plan-command branch-cleanup-apply --out branches-plan.json
```

Preparation captures the complete repository identity, default branch, selected
merged PRs, native branch refs and all open PRs using each selected branch as a
base or head.
The selected PR must belong to this repository, be merged and non-draft, and
identify the exact branch and SHA. Default branches and branches with open
dependents or reused open heads cannot be deleted. Draft PRs protect both their
base and head branches. A merged PR's exact SHA alone does not prove that its
branch is unused.

Review the exact v2 plan and obtain approval for its deletion scope and hash:

```sh
gh steward branches apply --repo-root . --input plan=branches-plan.json --approve-plan-sha APPROVED_PLAN_SHA
```

The hash binds the reviewed artifact; it is not approval by itself. Apply
rechecks the complete inventory and uses an explicit ref-and-SHA Git lease.
Native Git runs in isolated scratch with the existing GitHub CLI credential
helper. It does not alter the active checkout, install an extension or change
authentication. Local branches, worktrees, switching and fetching remain the
consumer's responsibility.

Preserve the plan and deterministic journal after interruption. A retained,
validated native deletion acknowledgement can support fresh-process recovery
and continuation of the remaining reviewed selection. A missing branch alone
cannot resolve an uncertain write. Without positive evidence, stop rather than
retrying a deletion or generating a replacement plan.
