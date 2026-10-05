---
name: gh-steward-workflow-recovery
description: Integrate or diagnose gh-steward recovery across GitHub Actions attempts and isolated jobs, including exact handoffs, durable checkpoints, reviewed legacy imports, and preview-only cutovers that quarantine unknown history.
---

Use the native `gh steward runs` commands for shared recovery mechanics. Keep consumer-specific approval, event, and domain policy in the trusted checkout; keep shell adapters thin. Read [the protocol reference](references/run-protocol.md) when implementing a workflow or interpreting a held package.

Confirm the staged executable's release version, source revision, clean-source flag, checksum, and provenance before using it. The `runs` commands shipped in 0.2.0; 0.1.0 does not support them. Installing this plugin does not install the CLI or authorize writes.

Treat the recovery outcomes distinctly:

- `fresh`: every prior exact attempt is positively settled under the default protocol. With an explicitly reviewed schema-6 cutover, this refers only to the native suffix: quarantined old attempts remain unknown and only read-only preview work may proceed.
- `resumed`: continue only the retained exact source target and native journals. Completed operations stay complete; the apply engine must positively reconcile unknown writes.
- `terminal`: the rerun predecessor is settled. Preserve the frontier and finish this observer without preparing a new target.
- `recovery_needed`: retain the reason and all evidence in a separate diagnostic artifact even when the guard fails. Missing artifacts, old SDK output, and skipped legacy step names do not prove absence of writes. A diagnostic report is not a settlement or checkpoint.

Use the trusted control checkout and actual runtime `GITHUB_WORKFLOW_SHA` for policy and publication qualification. Run untrusted source or candidate verification in isolated jobs without mutation credentials. Transfer one final preparation artifact by exact upload ID and digest; acquire it in the consuming job before use. Choose default `apply` for native plans and `transport` for evidence-only or publication packages.

Keep current-run no-op completion separate from plan preparation. Only `finish-noop` creates its typed zero-operation receipt after positive skipped evidence for the exhaustive reviewed mutation inventory. Preserve declined proposals as inert evidence. For `preview-only`, retain each raw planning report under `previews/` with its exact hash; do not invent a review or treat preview operations as executed. Do not mark an approved or started apply as a no-op, rewrite the source proof, invent provider acknowledgements, or backfill legacy attempts.

After an actual normal artifact upload, `finalize` verifies exact retained bytes and creates a new checkpoint if the invocation is terminal. Keep the original upload and checkpoint receipts. A green workflow, upload success, or context phase alone does not establish terminal execution.

Branch cleanup in 0.3.0 can retain a typed `already_absent` outcome with zero
operations after complete live evidence proves the exact merged PR's branch
is absent. Keep the actual completed merge parent and its existing reviewed
cleanup child. Interrupted recovery may resume that exact zero-write child;
it never replays the parent or creates a new child in an observer context.
An absent branch cannot resolve an uncertain deletion without the original
positive acknowledgement.

## Reviewed legacy evidence

Use `runs legacy-review` for offline verification of one exact signed attempt, then use the read-only import preview to capture complete current Actions history and an explicit local import only after its exact review digest is separately listed in the trusted workflow policy. These commands print summaries only; `--out` writes the full sensitive envelope to a private mode-0600 file. A review digest or `--approve-review-sha` is an artifact identity check, not human approval or a provider-write grant. The import preserves the raw report and archival packet as a chain-v5 legacy outcome; it does not synthesize a native plan, journal, or completion receipt. An explicit legacy checkpoint ends at its final reviewed import, with no native suffix or open prepared frontier; any later native suffix must come from authenticated hosted checkpoint acquisition and match the explicit prefix. SDK-v1 observations can be retained as effects, but never establish terminal completion. Read the [legacy evidence reference](references/legacy-evidence.md) for exact document shapes, sample commands, disposition rules, and the archive-key trust boundary.

The legacy review and import commands require CLI 0.3.0 or later.

## Reviewed unknown-history cutover

Use the [history cutover reference](references/history-cutover.md) when original legacy evidence is unavailable. CLI 0.4.0 captures a complete terminal history, keeps every old attempt explicitly unknown and quarantined, and requires review of that exact baseline. Capture is read-only; offline validation checks shape and digest only. Keep publication, installation, consumer promotion and baseline activation as separate delivery decisions when the task reserves them.

A baseline must never absorb uncaptured later attempts or be changed to clear a hold. Schema-6 recovery is preview-only and blocks executable plan registration, mutation resumption and publication. Every future native attempt still needs its own qualified evidence. Preserve the same reviewed baseline in all checkpoints. A configured GitHub issue can record review independently of source commits; authenticated authorship is not proof of human consent, so obtain the user's approval for the exact baseline before recording an approval statement on their behalf.
