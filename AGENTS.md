# Repository guidance

`gh-steward` is a public, general-purpose GitHub CLI extension and Agent Plugin. Keep code, plugin instructions, examples, and release metadata useful across repositories. Do not add private organization names, internal repository topology, customer data, credentials, or captured agent transcripts.

## Boundaries

- `plugins/gh-steward/plugin.json` is the portable Agent Plugins 1.0.0 manifest. Keep portable skills under its immediate `skills/<name>/SKILL.md` directories. Client-specific metadata belongs under `extensions` using a reverse-domain namespace. Do not replace the portable manifest with a Codex-only manifest.
- Plugin skills explain workflows and command contracts; the Go CLI owns validation, source reads, authorization checks at its boundary, and mutations. Do not duplicate executable business logic in skill prose or imply that installing a plugin installs or authenticates the CLI.
- GitHub authentication, target identity, user authorization, and repository permissions are distinct. `gh auth status` is an identity check only. `--approve-plan-sha` is an exact-artifact check only; it is never a grant of human approval.
- Apply accepts a canonical reviewed v2 plan with complete live evidence and recomputes its exact operations. Keep before-state, full-inventory, and receipt validation strict. Unknown writes are never blind-replayed; preserve the durable journal and require positive reconciliation of the exact operation.
- Keep provider reads/writes inside the typed native and workflow boundaries. Unit tests use fixtures and fake providers, never live GitHub state.

## Supported builds

The initial release and apply qualification cover `darwin-amd64`, `darwin-arm64`, `linux-amd64`, and `linux-arm64`. Do not add another apply target until its journal locking, atomic persistence, interrupted-write recovery, and end-to-end workflow are qualified. The release build script must keep its target allowlist explicit.

## Validation

For changes that affect Go behavior or public commands, run:

```sh
go test ./...
go test -race ./...
go vet ./...
go test -run '^$' -bench '^BenchmarkLCSForLargeQueue$' -benchtime=1x -count=1 ./internal/planning
scripts/release/check-build-matrix.sh
```

For plugin or release metadata changes, also run:

```sh
python3 scripts/release/check-plugin-package.py
```

Keep validation provider-free. Do not install the extension into a user's active GitHub CLI environment while developing.

## Releases

- Keep the extension and portable plugin on the same semantic version.
- Create a release only from a clean version tag whose commit is the exact checked-out source.
- Preserve the immutable source commit in `version --json`; release binaries report `source_dirty: false`.
- Publish only the four qualified Darwin/Linux assets, a SHA-256 checksum file, and build provenance attestations.
- Pin GitHub Actions to full commit SHAs. Do not publish a GitHub Actions Marketplace action.
- Do not create tags, releases, or commits unless the parent task explicitly authorizes that delivery stage.
