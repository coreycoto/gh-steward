---
name: gh-steward-workflow-recovery
description: Integrate or diagnose gh-steward recovery across GitHub Actions attempts and isolated jobs, including exact plan handoffs, publication qualification, skipped-run receipts, and durable checkpoints.
---

Use the native `gh steward runs` commands for shared recovery mechanics. Keep consumer-specific approval, event, and domain policy in the trusted checkout; keep shell adapters thin. Read [the protocol reference](references/run-protocol.md) when implementing a workflow or interpreting a held package.

Confirm the staged executable's release version, source revision, clean-source flag, checksum, and provenance before using it. The `runs` commands first appear in the 0.2.0 candidate; 0.1.0 does not support them. Installing this plugin does not install the CLI or authorize writes.

Treat the recovery outcomes distinctly:

- `fresh`: every prior exact attempt is positively settled; ordinary preparation may proceed within existing authorization.
- `resumed`: continue only the retained exact source target and native journals. Completed operations stay complete; the apply engine must positively reconcile unknown writes.
- `terminal`: the rerun predecessor is settled. Preserve the frontier and finish this observer without preparing a new target.
- `recovery_needed`: retain the reason and all evidence. Missing artifacts, old SDK output, and skipped legacy step names do not prove absence of writes.

Use the trusted control checkout and actual runtime `GITHUB_WORKFLOW_SHA` for policy and publication qualification. Run untrusted source or candidate verification in isolated jobs without mutation credentials. Transfer one final preparation artifact by exact upload ID and digest; acquire it in the consuming job before use. Choose default `apply` for native plans and `transport` for evidence-only or publication packages.

Keep current-run no-op completion separate from plan preparation. Only `finish-noop` creates its typed zero-operation receipt after positive skipped evidence for the exhaustive reviewed mutation inventory. Preserve declined proposals as inert evidence. For `preview-only`, retain each raw planning report under `previews/` with its exact hash; do not invent a review or treat preview operations as executed. Do not mark an approved or started apply as a no-op, rewrite the source proof, invent provider acknowledgements, or backfill legacy attempts.

After an actual normal artifact upload, `finalize` verifies exact retained bytes and creates a new checkpoint if the invocation is terminal. Keep the original upload and checkpoint receipts. A green workflow, upload success, or context phase alone does not establish terminal execution.
