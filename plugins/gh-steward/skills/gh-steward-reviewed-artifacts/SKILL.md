---
name: gh-steward-reviewed-artifacts
description: Inspect a workflow run's complete artifact inventory and prepare reviewed deletion of exact artifact names.
---

Use this skill when the user explicitly asks to remove named GitHub Actions run artifacts. Deletion is destructive; inventory or planning alone does not authorize it.

Inspect the complete inventory for the selected repository and positive run ID:

```sh
gh steward snapshot artifacts --input payload=run.json --out artifacts.json
```

`run.json` contains exactly `run_id`. For deletion, create an authored payload with exactly `run_id`, `names` (nonempty, unique, exact case-sensitive names), and boolean `ignore_missing`, then prepare:

```sh
gh steward artifacts prepare --input payload=artifact-selection.json --out reviewed-plan.json
```

The plan captures every page of the live run inventory and includes one operation for each matching artifact ID. A name may match more than one artifact; review every ID. If `ignore_missing` is false, matching artifacts are still deleted when another requested name is absent; the tool emits the completed receipt and reports a nonzero outcome with the missing names. If true, missing names are recorded and do not make the result nonzero.

Apply only after the user explicitly approves the exact deletion plan:

```sh
gh steward artifacts apply --repo-root . --input plan=reviewed-plan.json --approve-plan-sha EXACT_SHA256
```

Each delete requires a typed HTTP 204 acknowledgement and an independent complete inventory read. Preserve the journal after interruption. An unknown delete is never blindly repeated, and matching/absent inventory alone cannot establish whether an unacknowledged request ran.
