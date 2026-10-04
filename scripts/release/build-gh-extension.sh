#!/usr/bin/env bash
set -euo pipefail

release_tag="${1:?release tag is required}"
if [[ ! "$release_tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]]; then
  printf 'invalid semantic release tag: %s\n' "$release_tag" >&2
  exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

tag_commit="$(git rev-parse "${release_tag}^{commit}")"
source_revision="$(git rev-parse HEAD)"
if [[ "$tag_commit" != "$source_revision" ]]; then
  printf 'release tag %s does not identify checked-out source %s\n' "$release_tag" "$source_revision" >&2
  exit 1
fi
if ! git diff --quiet || ! git diff --cached --quiet || [[ -n "$(git status --porcelain --untracked-files=all)" ]]; then
  printf 'release build requires a clean source tree\n' >&2
  exit 1
fi

version="${release_tag#v}"
python3 scripts/release/check-plugin-package.py
plugin_version="$(python3 -c 'import json; print(json.load(open("plugins/gh-steward/plugin.json", encoding="utf-8"))["version"])')"
if [[ "$plugin_version" != "$version" ]]; then
  printf 'tag version %s does not match portable plugin version %s\n' "$version" "$plugin_version" >&2
  exit 1
fi

dist_dir="$repo_root/dist"
if [[ -d "$dist_dir" ]] && find "$dist_dir" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
  printf 'release build requires an empty dist directory\n' >&2
  exit 1
fi
mkdir -p "$dist_dir"
platforms=(darwin-amd64 darwin-arm64 linux-amd64 linux-arm64)
for platform in "${platforms[@]}"; do
  goos="${platform%-*}"
  goarch="${platform#*-}"
  asset="gh-steward_${release_tag}_${platform}"
  printf 'building %s from %s\n' "$asset" "$source_revision"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
    -trimpath \
    -ldflags="-s -w -X github.com/coreycoto/gh-steward/internal/cli.Version=${version} -X github.com/coreycoto/gh-steward/internal/cli.SourceRevision=${source_revision} -X github.com/coreycoto/gh-steward/internal/cli.SourceDirty=false" \
    -o "$dist_dir/$asset" .
done

host_asset="$dist_dir/gh-steward_${release_tag}_linux-amd64"
version_output="$("$host_asset" version --json)"
printf '%s\n' "$version_output" | EXPECTED_VERSION="$version" EXPECTED_REVISION="$source_revision" python3 -c '
import json, os, sys
value = json.load(sys.stdin)
expected = {
    "tool": "gh-steward",
    "tool_version": os.environ["EXPECTED_VERSION"],
    "source_revision": os.environ["EXPECTED_REVISION"],
    "source_dirty": False,
    "target": "linux/amd64",
}
for key, wanted in expected.items():
    if value.get(key) != wanted:
        raise SystemExit(f"release binary has invalid {key}: {value.get(key)!r}")
'

(
  cd "$dist_dir"
  sha256sum "gh-steward_${release_tag}_darwin-amd64" \
    "gh-steward_${release_tag}_darwin-arm64" \
    "gh-steward_${release_tag}_linux-amd64" \
    "gh-steward_${release_tag}_linux-arm64" | LC_ALL=C sort -k2 > checksums.txt
)
