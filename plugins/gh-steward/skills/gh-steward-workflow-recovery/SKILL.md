---
name: gh-steward-workflow-recovery
description: Integrate or diagnose interrupted GitHub Actions operations using native plans, automatic journals and run artifacts, with one reviewed configuration boundary for old workflow history.
---

Use the native `gh steward runs` commands for recovery. Keep the workflow's plan,
approval and event policy in the trusted checkout and shell adapters thin. Read
[the protocol](references/run-protocol.md) when implementing an adapter.

Verify the staged executable's pinned source, release checksum and provenance.
Installing the plugin does not install the executable or authorize GitHub writes.

Use the reported outcome:

- `fresh`: no unsettled attempt exists within the configured history scope.
- `resumed`: continue the exact retained operation with its original plan and journal.
- `terminal`: the rerun predecessor is settled; do not prepare another target.
- `recovery_needed`: preserve the evidence and explain the unresolved operation.

The apply engine automatically journals writes. An uncertain result must be
positively reconciled before continuation; neither a matching value nor a failed
workflow proves completion. Preserve the original evidence and never blind-replay
an uncertain write. Recovery does not add mutation authority.

Use the trusted control checkout and runtime `GITHUB_WORKFLOW_SHA`. Transfer the
original package between isolated jobs by immutable upload ID and digest. Run
untrusted candidate verification without mutation credentials. Native finalization
checks completion; adapters must not manufacture receipts or edit saved journals.

For adoption from older workflow history, CLI 0.6.0 uses one optional
[`history_start`](references/history-start.md) in the reviewed repository policy.
Resolve or explicitly account for unfinished old operations before selecting it.
This excludes old runs from future automation; it does not settle or replay them.
Do not move it forward to bypass interrupted native work.

Recovery metadata releases, baseline imports, promotion comments and runtime
history-document inputs are retired. Do not recreate them. Retain historical
records as evidence. New scoped checkpoints require the qualified 0.6.0 CLI;
source edits alone do not install it or activate a consumer workflow.
