# Reviewed legacy evidence

Use this path only to review a legacy workflow attempt that has no accepted native terminal receipt. It keeps archival classification, a live read-only preview, and local chain import as separate steps. A classification alone cannot advance recovery history. Imported records retain the signed evidence and review report as explicit chain-v5 legacy outcomes; they do not create a native plan, apply journal, success receipt, or provider-write authorization. The default recovery chain remains version 4 until a caller explicitly supplies a reviewed version-5 checkpoint.

All three commands print only a small summary. The exact schema-version-2 result, which can contain signed archives, event inputs, source files, receipts, complete history, or checkpoint data, is written only when `--out` is supplied. That path must be a canonical checkout-relative file path; the CLI rejects absolute paths, traversal, symlinks, non-private existing files, and overlap with an input, policy, or import store. It creates missing parents as mode `0700`, writes a mode `0600` file through a pinned checkout root, and does not replace an existing file unless its bytes already match the complete result. `import-legacy` rejects every pre-existing `--out` file before any provider read or store change; omit `--out` or choose a fresh private path for an import retry. Summary `full_result_path` identifies the requested private file; the summary itself never contains the raw packet or full checkpoint. Keep the shell `umask` restrictive when extracting data from the private envelope.

## Trust before classification

The archival packet and its retained review are separate raw JSON objects. The outer evidence object has exactly `schema_version`, `payload`, and `signature`; both schema versions are `1`. The payload has exactly these fields:

```text
schema_version, target, run, attempt, attempt_target, run_packet,
jobs_packet, workflow_sha, event_file, inputs, sources, intended_effects,
receipts, observations, artifacts
```

The review object has exactly:

```text
schema_version, target, run_id, attempt, evidence_sha256, archive_public_key,
workflow_sha, sources, mutators, non_mutator_jobs, artifacts
```

The evidence signature is canonical standard-base64 Ed25519 over the canonical payload bytes prefixed by `gh-steward-legacy-evidence-v1\n`. The review binds the exact evidence-object SHA-256, repository and workflow target, run and attempt, executed workflow SHA, source digests, complete mutator and non-mutator job inventories, and archived artifact identities. File witnesses use exact raw bytes represented as `{"sha256":"LOWERCASE_SHA256","base64":"CANONICAL_BASE64"}`.

The CLI verifies the signature against the public key in the review; this proves consistency, not who owns that key. Authenticate the archive producer and its key independently before relying on the review. The original event must include a host-bearing `repository.html_url` that exactly matches the reviewed repository; a missing or mismatched URL keeps intent unproven. Review the complete executable source closure, including the workflow, called workflows, local actions, scripts, and other executable inputs; review which jobs and steps can mutate and which jobs cannot. Require the original event and inputs, exact run and attempt packets, complete job pages, every intended effect, raw artifact archives, and receipts or observations. Preview compares complete live artifacts against the reviewed witnesses; omitted or ambiguous same-run artifacts remain held. Do not infer the executed source from `head_sha` or a workflow file fetched from the current API. Missing or expired evidence stays held unless the exact archived bytes and witnesses are retained and independently verified. SDK-v1 observations may support an `effect_observed` classification but never prove native terminal completion.

There is no command for signing observations. Obtain the packet and review from the independently controlled archival process. Do not create a key pair locally to certify evidence whose owner or executed source has not been established.

## Three-stage workflow

The examples use placeholders. Supply the exact repository and trusted control checkout for the target workflow. `legacy-evidence.json` must be a raw object shaped as `{"evidence":{...signed packet...},"review":{...review...}}`; do not pass a CLI result envelope.

```sh
umask 077
REPO_URL=https://github.com/OWNER/REPOSITORY
POLICY=.agents/gh-steward-recovery-policy.json

# 1. Offline verification. This command reads no provider state.
gh steward runs legacy-review --repo-root . --repo "$REPO_URL" \
  --input evidence=legacy-evidence.json --out legacy-review-envelope.json \
  > legacy-review-summary.json
jq -e '.data | select(.schema_version == 1 and .sha256 != null)' \
  legacy-review-envelope.json > legacy-report.json

# Start from the previously retained, validated checkpoint for this exact target.
# Use legacy_no_dispatch only for a complete exhaustive skipped-mutator proof;
# use legacy_terminal_receipt only for a complete terminal receipt report.
jq -n --slurpfile report legacy-report.json \
  --slurpfile checkpoint prior-checkpoint.json \
  --arg outcome legacy_no_dispatch \
  '{report:$report[0],checkpoint:$checkpoint[0],outcome:$outcome,dispositions:[]}' \
  > legacy-candidate.json

# 2. Read-only live preview. It fetches complete current Actions history,
# validates the exact live attempt and returns review, history and checkpoint.
gh steward runs legacy-import-preview --repo-root . --repo "$REPO_URL" \
  --policy "$POLICY" --input candidate=legacy-candidate.json \
  --out legacy-preview-envelope.json > legacy-preview-summary.json

REVIEW_SHA=$(jq -er '.data.review.sha256' legacy-preview-envelope.json)
jq -n --slurpfile report legacy-report.json \
  --slurpfile preview legacy-preview-envelope.json \
  '{report:$report[0],review:$preview[0].data.review,
    checkpoint:$preview[0].data.checkpoint,history:$preview[0].data.history}' \
  > legacy-import.json

# Separately review the exact preview digest and add it to the target workflow's
# legacy_import_reviews array in the trusted consumer policy. Review/commit that
# policy change under the normal control-source process; do not auto-enroll it.

# 3. Explicit local append. The store must already be a real private directory
# owned by this user, mode 0700, with no symlink ancestors.
install -d -m 700 .artifacts/gh-steward/legacy-import-store
gh steward runs import-legacy --repo-root . --repo "$REPO_URL" \
  --policy "$POLICY" --input import=legacy-import.json \
  --store .artifacts/gh-steward/legacy-import-store \
  --approve-review-sha "$REVIEW_SHA" --out legacy-import-envelope.json \
  > legacy-import-summary.json

# Preserve the raw chain object when explicitly supplying it to a later recovery.
jq -e '.data.checkpoint' legacy-import-envelope.json > legacy-checkpoint-v5.json
```

All named input documents are raw objects, not schema-version-2 CLI envelopes. The private `--out` file contains the full result envelope; stdout remains a summary even when `--out` is present. Extract `.data` from that private file when passing a result to a later command. The candidate shape is exactly `{report,checkpoint,outcome,dispositions}`. The import shape is exactly `{report,review,checkpoint,history}`. Preview does not persist a settlement or enroll its review digest in policy. Import re-fetches complete history and revalidates the live attempt before appending one exact outcome; reopening the same store verifies and returns an already-imported result. Do not pipe or log a full result; the `--out` file contains retained sensitive evidence and is not an approval artifact.

The policy's optional `legacy_import_reviews` is an exact per-workflow inventory of review SHA-256 values. The preview digest must be reviewed separately and added to the trusted policy before import. In Actions, the trusted control checkout and policy bytes must match the runtime `GITHUB_WORKFLOW_SHA`. `--approve-review-sha` checks that the supplied review is the exact previewed artifact; it is not human approval, a provider permission, or an instruction to dispatch work. A governed human decision is still required for the local history import.

## Outcomes and dispositions

The explicit outcomes are:

- `legacy_no_dispatch`: use only when the report proves complete original intent, source closure, event and inputs, artifacts, and every declared mutation step skipped.
- `legacy_terminal_receipt`: use only when the report proves complete terminal receipts for every intended effect.
- `legacy_operation_disposition`: use only with complete authenticated original intent and one explicit disposition for every intended effect.

For `legacy_operation_disposition`, each array row has exactly `effect_sha256`, `decision`, and `evidence`. `effect_sha256` must identify one original effect exactly once. `decision` is `accepted_observed_effect`, `no_effect_proven`, or `accepted_unobservable_effect`. `evidence` contains one to 32 distinct raw file-proof objects for that specific decision. The preview validates presence and exact bytes of those proofs; the reviewer remains responsible for deciding whether the proof supports the disposition.

Never treat an SDK-v1 observation, a classification label, a missing receipt, or a skipped step name by itself as terminal proof. Incomplete original event/input evidence or executable source closure cannot be cleared by a disposition. Do not fabricate provider acknowledgements or convert an imported outcome into native completion.

An explicit `legacy-checkpoint` supplied to `runs recover` must end at its final reviewed legacy import, contain no native suffix, and have no open prepared frontier. The importer reconstructs the previous checkpoint and binds the import to its exact previous-checkpoint digest. Recovery compares that explicit reviewed v5 prefix with all hosted checkpoints and holds any uncovered suffix or conflicting proof. A later native suffix is accepted only from authenticated hosted checkpoint acquisition and is compared against the explicit legacy prefix. The import and later recovery read are separate actions; importing does not authorize hosted recovery or alter trust policy by itself.
