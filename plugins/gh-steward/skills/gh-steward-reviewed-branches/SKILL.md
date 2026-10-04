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

```sh
gh steward branches prepare --repo-root . --input payload=branches.json --out branches-plan.json
```

Preparation captures the complete repository identity, default branch, selected
merged PRs, native branch refs and all open PRs based on each selected branch.
The selected PR must belong to this repository, be merged and non-draft, and
identify the exact branch and SHA. Default branches and branches with open
dependents cannot be deleted. A draft dependent PR still protects its base.

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
