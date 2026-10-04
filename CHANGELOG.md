# Changelog

## 0.2.0 — Unreleased

- Add shared native workflow-history recovery, immutable job handoffs, terminal receipt validation, and uploaded-package checkpoint finalization under `runs`.
- Require a durable publication qualification bound to the exact trusted workflow, candidate bytes, successful verification job, and immutable artifact identities before publication recovery can advance.
- Add current-run zero-operation receipts that require an exact reviewed workflow source and positive skipped evidence for every declared mutation step, preserving every raw planning document for preview-only runs. Historical ambiguous attempts stay held.
- Preserve exact native plans, completed operation receipts, and original source proofs across isolated jobs. Keep unknown writes subject to the apply engine's positive reconciliation.
- Retain bounded pending work across repeated interruptions when the current workflow source and named mutation steps positively prove no dispatch. Resume exact saved journal progress and close pending work only with an actual terminal observer receipt; historical unknown attempts remain held.
- Add a focused portable workflow-recovery skill. Consumer policy remains declarative; shared recovery logic lives in Go.

## 0.1.0 — 2026-10-04

- Add a native Go GitHub CLI extension with repository, issue, Project and execution snapshots; explicit consumer policy; and reviewed backlog, quarter, relationship, governance and review closeout workflows.
- Add PR creation and exact-head finish plans with issue and Project follow-up, plus remote branch cleanup protected by explicit Git leases.
- Protect reused branch heads supporting other open PRs, including drafts, during cleanup and delivery branch removal; incomplete head reads stop the operation.
- Add reviewed mixed issue change sets with complete owner Project discovery, explicit immutable scopes, and late-bound creation receipts for Project and relationship follow-up.
- Add durable per-operation acknowledgements and fresh-process recovery that stops when a provider write has an unknown outcome.
- Add portable Agent Plugin packaging with twelve focused setup, planning, delivery and cleanup skills.
- Add clean-source, checksummed, attested GitHub CLI release builds for Darwin and Linux on amd64 and arm64.
