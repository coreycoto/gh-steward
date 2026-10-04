# Repository plugin catalog

`marketplace.json` exposes the portable package in `plugins/gh-steward/` for local Codex discovery. Its source path is relative to this repository root.

This marketplace installs the Agent Plugin instructions only. It does not acquire or update the `gh-steward` executable, authenticate a GitHub account, or authorize repository changes. Manage the GitHub CLI extension with `gh extension install` / `gh extension upgrade`; manage GitHub authentication with `gh auth`; review and approve each mutation plan separately.
