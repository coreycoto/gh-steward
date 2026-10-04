---
name: gh-steward-setup
description: Install or inspect the gh-steward GitHub CLI extension and its Agent Plugin, confirm source provenance and the intended GitHub identity, or distinguish those setup steps when a user asks how to start using the tool.
---

Use this skill for tool setup and provenance checks. Installing the instruction plugin, installing the GitHub CLI extension, authenticating to GitHub, and authorizing a write are separate steps.

The portable package also provides focused skills for backlog audit/rebalancing, review findings, quarter commitments, issue relationships, exact-head merges, execution synchronization, and workflow-run artifact deletion. Load only the workflow skill that matches the user's request.

1. For Codex plugin discovery, use the repository's Agent Plugin marketplace. Adding or installing that plugin exposes instructions; it does not install a native executable.
2. After a public extension release exists, install the executable with an explicit release pin, for example `gh extension install coreycoto/gh-steward --pin v0.1.0`. Do not install an unpinned latest release when a workflow needs reproducible tool provenance.
3. Inspect `gh extension list` and run `gh steward version --json`. Confirm `tool` is `gh-steward`, `tool_version` is the intended release, `source_dirty` is `false`, `source_revision` is the expected tagged commit, and `target` is one of `darwin/amd64`, `darwin/arm64`, `linux/amd64`, or `linux/arm64`.
4. For a consumer or isolated runner working from a source checkout, acquire the exact asset without changing the active `gh` installation by running `scripts/release/acquire-gh-steward.sh VERSION SOURCE_COMMIT DEST_DIR [EXPECTED_ASSET_SHA256]`. It verifies checksums and the release provenance bound to the repository, signer workflow, source commit, and tag, then prints a JSON receipt describing the staged file. Stop on a missing or mismatched checksum, attestation, revision, version, digest, or target. For a manually downloaded release, verify its checksum and run `gh attestation verify <asset> --repo coreycoto/gh-steward --signer-workflow coreycoto/gh-steward/.github/workflows/release.yml --source-digest <commit-sha> --source-ref refs/tags/<tag> --signer-digest <commit-sha> --deny-self-hosted-runners`; verify the release record with `gh release verify <tag> --repo coreycoto/gh-steward`.
5. Before any live repository command, run `gh auth status` and confirm that the selected host and account are the user's intended identity. Never print, inspect, or copy authentication tokens.

Authentication proves which GitHub principal the CLI will use; it does not approve mutations. A reviewed plan hash likewise identifies an artifact but does not grant authorization. Require the user's explicit approval of the exact plan before applying it.

The initial release supports apply only on the four listed Darwin and Linux targets. Do not infer support for another operating system or architecture from the source compiling there.

For a read-only Project title lookup, use `gh steward snapshot projects --project-owner LOGIN --project-owner-type User|Organization`. Its complete accessible collection preserves owner identity and every Project number, ID and URL. Reject no match or multiple exact title matches, then use `snapshot project` with the selected number and ID to capture fields and items. Do not infer access to private Projects omitted by the authenticated principal's visibility.
