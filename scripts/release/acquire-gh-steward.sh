#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'usage: %s VERSION SOURCE_COMMIT DEST_DIR [EXPECTED_ASSET_SHA256]\n' "$0" >&2
  exit 2
}

[[ $# -eq 3 || $# -eq 4 ]] || usage
version="$1"
source_commit="$2"
destination="$3"
expected_asset_sha256="${4:-}"

if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]]; then
  printf 'version must be a semantic release version without a v prefix\n' >&2
  exit 2
fi
if [[ ! "$source_commit" =~ ^[0-9a-f]{40}$ ]]; then
  printf 'source commit must be a lowercase 40-character Git SHA\n' >&2
  exit 2
fi
if [[ -n "$expected_asset_sha256" && ! "$expected_asset_sha256" =~ ^[0-9a-f]{64}$ ]]; then
  printf 'expected asset digest must be a lowercase SHA-256 hex value\n' >&2
  exit 2
fi
command -v gh >/dev/null || { printf 'GitHub CLI (gh) is required\n' >&2; exit 1; }
command -v python3 >/dev/null || { printf 'Python 3 is required for strict JSON validation\n' >&2; exit 1; }

os_name="$(uname -s)"
arch_name="$(uname -m)"
case "$os_name/$arch_name" in
  Darwin/x86_64) platform="darwin-amd64"; target="darwin/amd64" ;;
  Darwin/arm64|Darwin/aarch64) platform="darwin-arm64"; target="darwin/arm64" ;;
  Linux/x86_64) platform="linux-amd64"; target="linux/amd64" ;;
  Linux/aarch64|Linux/arm64) platform="linux-arm64"; target="linux/arm64" ;;
  *) printf 'unsupported gh-steward acquisition target: %s/%s\n' "$os_name" "$arch_name" >&2; exit 1 ;;
esac

release_tag="v${version}"
asset_name="gh-steward_${release_tag}_${platform}"
signer_workflow="coreycoto/gh-steward/.github/workflows/release.yml"
temp_dir="$(mktemp -d)"
trap 'rm -rf "$temp_dir"' EXIT

gh release download "$release_tag" \
  --repo coreycoto/gh-steward \
  --pattern "gh-steward_${release_tag}_*" \
  --pattern checksums.txt \
  --dir "$temp_dir"

if [[ ! -f "$temp_dir/checksums.txt" || ! -f "$temp_dir/$asset_name" ]]; then
  printf 'release %s is missing the expected checksums or target asset\n' "$release_tag" >&2
  exit 1
fi
case "$os_name" in
  Darwin) (cd "$temp_dir" && shasum -a 256 --check --strict checksums.txt) >&2 ;;
  Linux) (cd "$temp_dir" && sha256sum --check --strict checksums.txt) >&2 ;;
esac

for verified_file in "$asset_name" checksums.txt; do
  gh attestation verify "$temp_dir/$verified_file" \
    --repo coreycoto/gh-steward \
    --signer-workflow "$signer_workflow" \
    --signer-digest "$source_commit" \
    --source-digest "$source_commit" \
    --source-ref "refs/tags/$release_tag" \
    --predicate-type https://slsa.dev/provenance/v1 \
    --deny-self-hosted-runners >&2
done

asset_sha256="$(python3 - "$temp_dir/$asset_name" <<'PY'
import hashlib
import pathlib
import sys

digest = hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest()
print(digest)
PY
)"
if [[ -n "$expected_asset_sha256" && "$asset_sha256" != "$expected_asset_sha256" ]]; then
  printf 'verified asset digest does not match the caller-pinned SHA-256\n' >&2
  exit 1
fi

chmod 0755 "$temp_dir/$asset_name"
"$temp_dir/$asset_name" version --json | EXPECTED_VERSION="$version" EXPECTED_REVISION="$source_commit" EXPECTED_TARGET="$target" python3 -c '
import json, os, sys
value = json.load(sys.stdin)
expected = {
    "schema_version": 2,
    "tool": "gh-steward",
    "tool_version": os.environ["EXPECTED_VERSION"],
    "source_revision": os.environ["EXPECTED_REVISION"],
    "source_dirty": False,
    "target": os.environ["EXPECTED_TARGET"],
}
for key, wanted in expected.items():
    if value.get(key) != wanted:
        raise SystemExit(f"release binary has invalid {key}: {value.get(key)!r}")
'

mkdir -p "$destination"
if [[ -L "$destination" || ! -d "$destination" ]]; then
  printf 'destination must be a real directory, not a symlink\n' >&2
  exit 1
fi
destination="$(cd "$destination" && pwd -P)"
if [[ -e "$destination/gh-steward" || -L "$destination/gh-steward" ]]; then
  printf 'destination already contains gh-steward; choose a fresh scratch or cache directory\n' >&2
  exit 1
fi
staged_file="$(mktemp "$destination/.gh-steward.XXXXXX")"
trap 'rm -rf "$temp_dir"; if [[ -n "${staged_file:-}" ]]; then rm -f "$staged_file"; fi' EXIT
cp "$temp_dir/$asset_name" "$staged_file"
chmod 0755 "$staged_file"
ln "$staged_file" "$destination/gh-steward"
rm -f "$staged_file"
staged_file=""

python3 - "$destination/gh-steward" "$asset_name" "$asset_sha256" "$version" "$source_commit" "$target" <<'PY'
import json
import sys

path, asset, digest, version, revision, target = sys.argv[1:]
print(json.dumps({
    "schema_version": 1,
    "tool": "gh-steward",
    "tool_version": version,
    "source_revision": revision,
    "source_dirty": False,
    "target": target,
    "asset": asset,
    "asset_sha256": digest,
    "path": path,
}, sort_keys=True, separators=(",", ":")))
PY
