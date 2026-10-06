# Reviewed unknown-history cutover

Use this capability only when a complete legacy inventory can be captured but
its original evidence cannot establish exact outcomes. It starts new read-only
tracking; it does not repair or re-execute historical effects. CLI 0.4.0 or later
is required. Publishing the capability and accepting one baseline are separate
decisions.

## Capture and review

Capture after the target workflow is idle and all runs and attempts are terminal.
Use the exact trusted checkout and recovery policy. Preserve current issue/PR
reads relevant to the target, and inspect live Project state independently when
the automation manages Projects. Selected reads are observations, not evidence
that any historical write succeeded or did not happen.

```sh
gh steward runs cutover-preview --repo-root . --repo "$REPO_URL" \
  --policy .agents/gh-steward-recovery-policy.json --workflow task.yml \
  --state-read repos/owner/repo/issues/17 \
  --out .artifacts/gh-steward/cutover-preview.json
gh steward runs cutover-validate --repo-root . --repo "$REPO_URL" \
  --input baseline=.artifacts/gh-steward/cutover-preview.json
```

`--out` is mandatory for capture. The full schema-2 envelope is retained in a
private mode-0600 file; stdout contains only identity and validation summaries.
Validation accepts that envelope or its raw `data` baseline and makes no provider
reads. Validation does not establish live freshness, approval or activation.

The baseline has schema 1, `scope: "preview-only"`, exact workflow-history-v2
`target`, raw workflow identity, complete `run_inventory`, every exact attempt
response, complete repository `artifact_inventory`, selected `state_reads`, and
a canonical `sha256`. Every attempt explicitly has `outcome: "unknown"` and
`handling: "quarantined-never-replay"`. Capture rejects active, missing,
duplicated, foreign, oversized or incomplete evidence. It also rejects existing
native recovery artifacts, preserving positive native lineage rather than
absorbing it into an unknown baseline. The whole baseline must fit the 8 MiB
checkpoint limit; the CLI never truncates it. This unreleased source also supports
a [bounded compressed schema-2 archive](history-archive.md) for larger complete
snapshots while retaining the same 8 MiB checkpoint/file limit. Capture selects
the representation automatically. Existing schema-1 snapshots keep their
identity and remain valid. A compressed baseline retains its own reviewed digest;
its decompressed inner digest cannot substitute for that review identity.

## Independent exact review

Each workflow may configure either `history_cutover_reviews: ["EXACT_SHA256"]`
in trusted policy or an independent issue channel:

```json
"history_cutover_review_issue": {
  "number": 17,
  "trusted_logins": ["maintainer"]
}
```

Do not configure both channels for the same workflow. Commit the review channel
before capturing the baseline: publishing a source change may itself create
another workflow attempt. Once those attempts finish, capture a fresh baseline
and have it independently reviewed. A source-independent review avoids creating
another uncaptured predecessor simply to commit its approval.

A trusted reviewer with current write, maintain or admin access can record this
exact first line in the configured issue:

```text
APPROVE HISTORY CUTOVER owner/repo#17 task.yml EXACT_SHA256
```

Revoke that review with:

```text
REVOKE HISTORY CUTOVER owner/repo#17 task.yml EXACT_SHA256
```

The latest substantive comment by ID from each trusted reviewer for that workflow
supersedes their earlier statement, including an approval for another digest.
Unrelated discussion does not change it. Complete comment pagination and counts,
unique IDs, exact issue identity, User authorship, timestamps and current author
permission are checked on each provider-backed recovery command. Deleted or
edited statements and lost write access remove their current approval. One
reviewer's revocation removes their approval; other configured reviewers' current
approvals remain independent. Closing the issue does not revoke its statements.
Provider read failures fail closed. Context commands remain provider-free unless
the workflow opts into the separate promotion review channel; those commands
then recheck both applicable review channels before native context changes.

GitHub authentication establishes who authored a comment, not whether a human
approved it. An agent must obtain approval of the exact baseline before recording
such a statement on a user's behalf. The statement approves only the unknown
history boundary and never grants mutation, release or deployment authority.

## Recovery and scope

Supply one reviewed baseline when starting recovery:

```sh
gh steward runs recover --repo-root . --repo "$REPO_URL" \
  --policy .agents/gh-steward-recovery-policy.json --workflow task.yml \
  --run-id "$GITHUB_RUN_ID" --attempt "$GITHUB_RUN_ATTEMPT" \
  --run-name "$RUN_NAME" --recovery-key "$RECOVERY_KEY" \
  --runner-temp "$RUNNER_TEMP" --package-root "$PACKAGE_ROOT" \
  --input history-cutover=.artifacts/gh-steward/cutover-preview.json
```

Do not mix a baseline with a legacy checkpoint. Recovery re-reads complete current
history and verifies the baseline's immutable run identities and attempt
high-waters. Every uncaptured newer run and rerun requires strict native proof;
unknown or active successors hold recovery. The baseline never silently grows.
Old artifact expiration does not change the retained point-in-time snapshot.

The schema-6 checkpoint keeps `history_cutover` separate from native `inventory`
and `settlements`. Every successor preserves exactly the same sealed baseline;
changed baselines, missing predecessors, incompatible schema histories and forks
hold recovery. A `fresh` outcome means the native suffix is clear for preview
only. It does not mean the quarantined historical attempts were settled.

Read-only planning may retain inert proposals. Current-run `finish-noop` still
needs positive native skipped-mutator evidence and creates a real receipt for
that current run. The chain cannot register executable plans, install journals
for mutation resumption, acquire an apply handoff or promote publication. Use
the separately reviewed [promotion protocol](history-promotion.md) for selected
fresh native work; a baseline review cannot authorize that transition.
Default recovery without an approved baseline keeps its existing strict behavior.

Keep failed reports in separate diagnostic artifacts before the guard stops the
workflow. Diagnostics must bind the report bytes and exact repository, workflow,
source, run and attempt. They never count as native recovery packages or settle
an attempt. Preserve original failed receipts and do not rerun old data-bearing
operations to obtain a green workflow.
