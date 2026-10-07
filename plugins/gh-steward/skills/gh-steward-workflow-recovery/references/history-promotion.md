# Fresh native work after quarantined history

This capability requires CLI 0.5.0 or newer. Qualify a released executable and
its exact source before consumer activation. Keep source delivery,
read-only capture, human review and consumer activation as separate steps.

Promotion preserves unknown history while selecting future native work. It never
imports an old outcome, retries a quarantined operation, grants publication or
replaces the ordinary per-plan authorization and live before-state checks.

## Capture

First configure `history_promotion_review_issue` in the trusted workflow policy:

```json
"history_promotion_review_issue": {
  "number": 18,
  "trusted_logins": ["maintainer"]
}
```

The baseline still needs its own exact `history_cutover_reviews` or independent
`history_cutover_review_issue` admission. Promotion has no static digest allowlist.
Commit the complete intended workflow policy before capture. Policy changes
invalidate a promotion instead of inheriting its approval. Let source-triggered
runs finish with their own native preview receipts before capture.

Capture while the workflow is idle and all current attempts are terminal. Select
native plan names already declared in policy and explicit same-repository issue
or PR GET endpoints whose live state matters to the transition. Reconcile any
Project or external state separately when applicable; the selected issue/PR
observations do not claim those systems were reconciled.

```sh
gh steward runs promotion-preview --repo-root . --repo "$REPO_URL" \
  --policy .agents/gh-steward-recovery-policy.json --workflow task.yml \
  --input baseline=.artifacts/gh-steward/cutover-preview.json \
  --plan execution --state-read repos/owner/repo/issues/17 \
  --out .artifacts/gh-steward/promotion-preview.json
gh steward runs promotion-validate --repo-root . --repo "$REPO_URL" \
  --input promotion=.artifacts/gh-steward/promotion-preview.json
```

If preview runs have already produced native lineage, also supply the exact
hosted schema-6 checkpoint upload with `--checkpoint-artifact-id ID` and
`--checkpoint-artifact-digest sha256:DIGEST`. Both are required. The CLI acquires
and verifies that archive from the complete hosted artifact inventory, including
name, owner run, source SHA, digest and exact native settlement prefix. A local
checkpoint cannot replace that acquisition. Omitting these flags is accepted
only when there are no target native artifacts and the baseline, plus any
explicitly qualified diagnostic holds described below, covers every current attempt.

Capture preserves the complete preview checkpoint, including any qualified
native no-op prefix. It rejects uncovered later runs or reruns, partial history,
active attempts, an open frontier, different baselines or unsafe hosted artifacts.
It checks history and selected state again before sealing. The full schema-2
result envelope is retained privately (0600); stdout is a summary. The raw schema-2
promotion has `scope: fresh-native`, exact target and preview checkpoint,
`policy_sha256`, a selected plan-name map of exact commands and allowed operation
kinds, selected raw `state_reads`, the versioned `state_contract`, fixed exclusions
and a canonical `sha256`.
Both the promotion and resulting checkpoint must fit the 8 MiB artifact bound.
Offline validation checks shape and digest, not approval or live freshness.

### Reconciliation contract

New captures seal `state_contract: github-rest-pr-repository-clock-v1` into the
promotion's digest. This contract compares complete issue and PR responses,
excluding only `base.repo.pushed_at` and `head.repo.pushed_at` in PR endpoint
responses. Publishing a tag changes those repository clocks without changing the
selected PR. PR and repository identities, refs, commit SHAs, merge state, labels,
body, policy fields and every other response field remain exact. The named clocks
must be present and have a valid timestamp or explicit null. A missing or malformed
PR, ref or repository identity holds the transition.

The complete raw responses, including both clocks, stay sealed in `state_reads`.
Comparison never edits the reviewed document or its retained lineage. The same
contract is used during capture and first admission. An unknown contract or a
changed document requires a new supported capture and exact review; it cannot
reuse an existing approval.

Schema-1 promotions remain supported with their original full-response equality.
They never inherit this contract. CLI 0.5.0 accepts only schema 1; qualify a released
build containing schema-2 support before capturing or activating a new document.
The native checkpoint remains schema 7 and embeds the exact promotion unchanged.

### Diagnostic holds after the baseline

A failed first attempt from `workflow_dispatch` or `workflow_run` may stop before
context initialization and upload only its recovery diagnostic. Source builds
with schema-3 support can capture its separate positive no-dispatch proof by
adding `--held-run-id ID` to `promotion-preview` (repeatable; at most 16).
Selection is explicit; the default still rejects uncovered diagnostic-only runs.

The CLI requires complete idle history, one immutable diagnostic upload with a
verified ZIP digest, its original manifest and report, GitHub's authenticated
[`WorkflowRun.file` witness](https://docs.github.com/en/graphql/reference/actions#workflowrunfile)
with an immutable commit URL, the executed file's exact
Git blob and content hash, and complete terminal attempt jobs. The source must
match the trusted `workflow-noop` policy's current or previous exhaustive mutation
inventory. Every declared mutation job or step must be positively skipped.
Missing, expired, duplicated, cancelled, started, foreign or competing execution
evidence holds capture. Both supported diagnostic manifest layouts retain their
original ZIP bytes; neither is converted to a context, plan, journal or receipt.

The raw schema-3 promotion adds `held_attempts` with disposition
`diagnostic-only-no-dispatch`. Each selected run must be absent from both the
original baseline and native preview inventory. Its proof is sealed into the
same promotion review digest, and source, upload and jobs are checked again during
capture and first fresh admission. Run identity and terminal status are compared
explicitly; unrelated mutable repository metadata embedded in that API response
is retained without being a no-dispatch identity requirement. Workflow source is
compared by its immutable executed-file witness, Git blob and exact content hash;
expiring private download URLs are transport metadata and remain privately retained.
The PR reconciliation
contract is unchanged. Stdout reports only the selected run IDs and disposition.

Schema 7 keeps this ledger inside the exact promotion, separate from unknown
quarantine and actual native settlements. The held run cannot become executable
work, a resumable frontier or a native completion. Its later attempts remain
uncovered, and trying to run it again holds. Fresh native interruption, journals
and terminal checkpoint validation retain their ordinary contracts. Once an actual
native checkpoint exists, its sealed proof can survive diagnostic-upload expiry.
Old schema-1/2 promotions cannot inherit hold coverage or reuse their approval.
Qualify a released schema-3 executable and obtain review of the exact new promotion
and separately authorized activation; source merge alone performs neither.

The preview checkpoint may contain a [compressed large-history baseline](history-archive.md).
Promotion keeps its complete sealed document and reviewed digest. It does not
replace that digest with the inner evidence digest or drop raw records to fit.

## Independent review

Present the exact promotion, selected work and exclusions to the user. GitHub
identity and write permission do not establish human consent. An agent may post
an approval statement on a user's behalf only after the user approves that exact
artifact and scope. Generic automation approval, a source merge, a baseline
review or a digest flag cannot authorize promotion.

In the configured issue, a trusted reviewer with current write, maintain or admin
permission can record this exact first line:

```text
APPROVE HISTORY PROMOTION owner/repo#18 task.yml EXACT_PROMOTION_SHA256
```

Revoke it with:

```text
REVOKE HISTORY PROMOTION owner/repo#18 task.yml EXACT_PROMOTION_SHA256
```

Cutover and promotion statements are independent, even in the same issue. The
latest substantive statement by comment ID from each trusted reviewer for the
workflow and review kind supersedes that reviewer's earlier statement. Complete
pagination, issue identity and counts, duplicate IDs, comment identity, timestamps,
User authorship and current repository permission are checked. Other reviewers'
current statements remain independent. Closing the issue does not revoke them.
Deleted/edited statements, lost write access and read failures remove admission
or fail closed. Every provider-backed recovery action and every context action
in a workflow with a promotion channel resolves current reviews anew.

## Activation and native evidence

After exact review and separately authorized consumer activation, the workflow
can provide the reviewed document to recovery:

```sh
gh steward runs recover --repo-root . --repo "$REPO_URL" \
  --policy .agents/gh-steward-recovery-policy.json --workflow task.yml \
  --run-id "$GITHUB_RUN_ID" --attempt "$GITHUB_RUN_ATTEMPT" \
  --run-name "$RUN_NAME" --recovery-key "$RECOVERY_KEY" \
  --runner-temp "$RUNNER_TEMP" --package-root "$PACKAGE_ROOT" \
  --input history-promotion=.artifacts/gh-steward/promotion-preview.json
```

Do not mix promotion with a legacy checkpoint or cutover input. The first fresh
native attempt rechecks the reviewed selected state; drift holds the transition.
Subsequent native operations use their own complete live preflight and exact
reviewed before-state. A `fresh` outcome with `mode: fresh-native` admits only the
selected native scope. It does not grant the per-plan approval or assert anything
about historical effects. An old quarantined invocation or its unproved rerun
holds instead of becoming fresh native work.

Schema 7 retains `history_promotion`, embedding the exact preview checkpoint,
outside its native inventory, settlements and prepared frontiers. Historical
unknowns stay visible and quarantined. Every successor carries the same grant;
changed grants or baselines, forks, dropped prefixes, missing lineage and policy
drift hold work. An opted-in workflow cannot initialize a native context without
its quarantined lineage or downgrade to schema 4/5. Schema 6 stays preview-only.
Executable plans outside the selected names and all publication contexts are
rejected. Publication and expanded permissions require a separate future design
and authorization; this grant cannot enable them.

Interrupted native work retains the full original plans, event and review files,
durable journal or source-qualified no-dispatch proof, exact prepared frontier
and immutable uploads. Recovery authenticates those packages and their same
promotion before resumption. Unknown writes require positive reconciliation of
the original operation; they are never dispatched again merely to clear a hold.
Terminal native receipts close only that native suffix. Ordinary schema-4/5
recovery and schema-6 preview behavior retain their existing strict contracts.

Disabling a workflow is an immediate operational stop. Removing its promotion
route or installing an older executable cannot silently resume a schema-7 chain;
unsupported or unreviewed lineage remains held. Preserve failed reports and
source proofs. Never rewrite a baseline or delete a native journal to bypass a
hold, and never replay historical data-bearing operations to obtain green CI.

A run held before context initialization may have only a diagnostic upload.
Schema-2 state comparison does not settle that attempt, manufacture a no-op receipt
or absorb it into the quarantined baseline. Complete history still includes it;
without a qualified native receipt or the explicit schema-3 reconciliation proof,
subsequent capture and recovery remain held. Deleting a published promotion asset
does not settle its failed run or undo its recorded revocation.
