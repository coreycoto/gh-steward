# Complete large-history evidence

This capability requires CLI 0.5.0 or newer. Use the same
`cutover-preview` and `cutover-validate` commands. Capture keeps small snapshots
in baseline schema 1 and automatically uses schema 2 when the complete raw
snapshot needs compression to fit the 8 MiB file/checkpoint budget, reserving
4 KiB for ordinary envelope/checkpoint metadata. There is no new activation or
format-selection flag. Compression does not review, settle, replay or activate
anything.

## Representation and review identity

Baseline schema 2 has exactly `schema_version`, `scope`, `target`, `evidence` and
`sha256`. Scope remains `preview-only`. `evidence` has exactly:

- `encoding`: `gzip-ndjson-v1`.
- `compressed_bytes` and `uncompressed_bytes`: exact positive byte counts.
- `record_count`: the complete stream's exact record count, including its header.
- `compressed_sha256` and `uncompressed_sha256`: exact byte identities.
- `data`: strict canonical base64 of one complete gzip member.

The outer `sha256` seals the canonical baseline without that field. Independent
review must use this outer digest. The raw baseline digest in the archived header
and either byte digest identify evidence; none can substitute for review of the
complete compressed document. A same-evidence recapture is deterministic, but
changing its representation or any raw field creates a different review identity.
Existing schema-1 snapshots retain their identity and validation contract.

The decompressed stream consists of canonical JSON objects, one per newline:

1. An exact `{kind: "header", data: HEADER}` record. The header retains schema 1,
   preview-only scope, exact target, raw workflow identity, raw baseline digest
   and exact run/attempt/artifact/state counts.
2. Ordered records with exactly `kind`, zero-based `index` and raw `data`.
   Groups appear in order: `run`, `attempt`, `artifact`, `state`. Each group must
   contain its declared complete count and consecutive indices.

The raw schema-1 baseline is reconstructed and fully validated after bounded
stream acquisition. Every original run response, every exact attempt response,
artifact object and selected state object remains whole. Attempt outcomes stay
`unknown` with `quarantined-never-replay` handling. Duplicate JSON keys, malformed
records, wrong groups or indices, missing/extra rows, foreign identities,
inconsistent attempt high-waters, nonterminal attempts, unsupported native
lineage, unordered state reads and changed raw baseline hashes are rejected.
A canonical record stream permits neither trailing documents nor missing final
newline frames. Truncated gzip streams, failed checksums, trailing gzip members
and arbitrary trailing bytes are rejected, even if their outer digest is resealed.

CLI summaries expose inventory counts, the schema and archive byte counts and
identities. Raw provider bodies remain in the mode-0600 artifact. Offline
validation remains a shape/digest/evidence check; it never claims current live
freshness or approval. The Go `HistoryCutoverEvidence` inspection boundary returns
validated raw evidence for a reviewer; retain the original complete document for
policy review, checkpointing and promotion.

## Resource limits

The normal file/checkpoint limit stays **8 MiB**. Native archive acquisition stays
bounded to 32 MiB per archive, 1,000 entries, 1,024 checkpoint artifacts and
256 MiB per scan category. Existing native transport/JSON input limits remain
unchanged. The new compressed evidence path additionally enforces:

| Resource | Maximum |
| --- | --- |
| Compressed evidence bytes | 4 MiB |
| Decompressed canonical record stream | 64 MiB |
| Individual typed record | 1 MiB |
| JSON nodes in one record | 16,384 |
| JSON nodes retained across the stream | 2,000,000 |
| JSON nesting depth | 64 |
| Runs, exact attempts, artifact metadata rows | 100,000 each |
| Selected issue/PR state reads | 128 |

Capture charges each provider object before cloning/retaining it, including a
framing reserve, against per-record, aggregate byte and node budgets. Encoding
uses a bounded compressed writer and a running decompressed byte counter.
Validation checks declared bounds and compressed identity before decompression,
then streams through a 64 MiB limit and a record scanner capped at 1 MiB. Each
record uses the existing strict JSON decoder. Memory for the retained evidence
is bounded by the byte/node/count budgets; a record cannot hide an arbitrarily
large object. These are evidence limits, not a promise about the Go runtime's
whole-process RSS. A dishonest size declaration cannot bypass actual stream
limits. Exceeding any bound rejects the capture or checkpoint, never a partial
or sampled inventory.

## Recovery and failure handling

Schema-6 checkpoints retain the complete compressed baseline and its outer
review identity outside native inventory and settlements. Qualified current-run
no-op receipts bind that same identity. Complete current history must still
contain the exact immutable prefix and attempt high-waters; newer runs and
reruns remain pending. A fresh process authenticates the exact hosted checkpoint
and preserves the same sealed archive. Missing review, changed identity,
conflicting checkpoints or unsafe archives hold recovery.

Schema-7 promotion may retain that compressed preview baseline, with its separate
exact promotion review and native scope requirements. It does not normalize the
baseline to a new identity, remove records or approve historical operations.
Producer source delivery, release qualification, live evidence capture, exact
review and consumer activation remain separate stages. Preserve an earlier
failed capture receipt; a qualified newer executable can make a fresh read-only
capture only within the user's authorized evidence scope.
