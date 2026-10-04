#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
temporary_dir="$(mktemp -d)"
trap 'rm -rf "$temporary_dir"' EXIT

cd "$repo_root"
for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
  goos="${target%/*}"
  goarch="${target#*/}"
  printf 'building %s/%s\n' "$goos" "$goarch"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -o "$temporary_dir/gh-steward-$goos-$goarch" .
done
