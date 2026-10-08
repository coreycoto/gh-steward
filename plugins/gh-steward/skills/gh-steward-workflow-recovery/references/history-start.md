# History start

Use CLI 0.6.0 or newer when adopting a workflow whose older attempts do not have
usable native recovery evidence. This is a repository-policy change, reviewed
through the ordinary source review process. It is not a new publication workflow.

Before adoption, stop overlapping mutation work and account for any unfinished
historical operations against their actual GitHub targets. Preserve the old
records. Selecting a boundary does not prove that old operations succeeded,
failed or made no writes; it only excludes them from future automatic recovery.

Set `history_start` on the selected workflow in
`.agents/gh-steward-recovery-policy.json`. Copy the immutable identity of the last
excluded run from the complete workflow inventory:

```json
"history_start": {
  "id": 123,
  "created_at": "2026-10-01T12:00:00Z",
  "display_title": "Last excluded run",
  "event": "workflow_dispatch",
  "workflow_id": 456,
  "head_branch": "main",
  "head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}
```

The field is optional. Without it, recovery remains strict over the entire
workflow history. With it, the CLI verifies the exact anchor in the complete
inventory, requires excluded runs to be terminal, and refuses every excluded run
or rerun as an invocation or recovery source. New runs require ordinary native
plans, approvals, journals and receipts. Missing evidence stops work.

Leave this boundary unchanged after adoption. Schema-8 checkpoints retain it and
reject changed or removed policy boundaries. Advancing it past an existing native
checkpoint is rejected, including interrupted checkpoints. Expired or unreadable
checkpoint evidence produces a hold rather than an inferred clean start.

Versions before 0.6.0 cannot use this policy or its schema-8 checkpoints. Upgrade
and qualify the consumer's executable before activating the policy. The old
schema-5/6/7 import, cutover and promotion commands are removed; preserve their
records without interpreting them as native completion. No metadata release,
issue-comment promotion, repository variable or download script is needed.
