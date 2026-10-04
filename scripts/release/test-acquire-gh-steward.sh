#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
acquire="$repo_root/scripts/release/acquire-gh-steward.sh"
tmp_root="$(mktemp -d)"
trap 'rm -rf "$tmp_root"' EXIT

stub_bin="$tmp_root/bin"
fixture_dir="$tmp_root/fixtures"
gh_config="$tmp_root/gh-config"
mkdir -p "$stub_bin" "$fixture_dir" "$gh_config"

good_revision=0123456789abcdef0123456789abcdef01234567
bad_revision=abcdef0123456789abcdef0123456789abcdef01
version=0.1.0
tag="v$version"
case "$(uname -s)/$(uname -m)" in
  Darwin/x86_64) platform=darwin-amd64; expected_target=darwin/amd64 ;;
  Darwin/arm64|Darwin/aarch64) platform=darwin-arm64; expected_target=darwin/arm64 ;;
  Linux/x86_64) platform=linux-amd64; expected_target=linux/amd64 ;;
  Linux/aarch64|Linux/arm64) platform=linux-arm64; expected_target=linux/arm64 ;;
  *) echo 'acquisition test requires a supported Darwin/Linux host target' >&2; exit 1 ;;
esac
asset="gh-steward_${tag}_${platform}"
log="$tmp_root/gh.log"

cat >"$stub_bin/gh" <<'STUB_GH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$GH_STUB_LOG"
case "${1:-}/${2:-}" in
  release/download)
    [[ "$3" == "$GH_EXPECTED_TAG" ]] || { echo 'unexpected release tag' >&2; exit 41; }
    shift 3
    repo=""
    dir=""
    patterns=()
    while (($#)); do
      case "$1" in
        --repo) repo="$2"; shift 2 ;;
        --pattern) patterns+=("$2"); shift 2 ;;
        --dir) dir="$2"; shift 2 ;;
        *) echo "unexpected release-download argument: $1" >&2; exit 42 ;;
      esac
    done
    [[ "$repo" == coreycoto/gh-steward ]] || { echo 'wrong release repository' >&2; exit 43; }
    [[ " ${patterns[*]} " == *" $GH_EXPECTED_ASSET_PATTERN "* ]] || { echo 'asset pattern missing' >&2; exit 44; }
    [[ " ${patterns[*]} " == *' checksums.txt '* ]] || { echo 'checksum pattern missing' >&2; exit 45; }
    [[ -n "$dir" && -d "$dir" ]] || { echo 'download directory missing' >&2; exit 46; }
    cp "$GH_FIXTURE_DIR"/* "$dir/"
    chmod 0644 "$dir/$GH_EXPECTED_ASSET_NAME"
    ;;
  attestation/verify)
    file="$3"
    shift 3
    repo="" workflow="" signer_digest="" source_digest="" source_ref="" predicate="" deny_self_hosted="false"
    while (($#)); do
      case "$1" in
        --repo) repo="$2"; shift 2 ;;
        --signer-workflow) workflow="$2"; shift 2 ;;
        --signer-digest) signer_digest="$2"; shift 2 ;;
        --source-digest) source_digest="$2"; shift 2 ;;
        --source-ref) source_ref="$2"; shift 2 ;;
        --predicate-type) predicate="$2"; shift 2 ;;
        --deny-self-hosted-runners) deny_self_hosted="true"; shift ;;
        *) echo "unexpected attestation argument: $1" >&2; exit 47 ;;
      esac
    done
    [[ "${GH_STUB_ATTESTATION:-pass}" == pass ]] || { echo 'simulated attestation rejection' >&2; exit 48; }
    [[ "$repo" == coreycoto/gh-steward ]] || { echo 'wrong attestation repository' >&2; exit 49; }
    [[ "$workflow" == coreycoto/gh-steward/.github/workflows/release.yml ]] || { echo 'wrong signer workflow' >&2; exit 50; }
    [[ "$signer_digest" == "$GH_EXPECTED_SOURCE_COMMIT" ]] || { echo 'wrong signer digest' >&2; exit 51; }
    [[ "$source_digest" == "$GH_EXPECTED_SOURCE_COMMIT" ]] || { echo 'wrong source digest' >&2; exit 52; }
    [[ "$source_ref" == "refs/tags/$GH_EXPECTED_TAG" ]] || { echo 'wrong source ref' >&2; exit 53; }
    [[ "$predicate" == https://slsa.dev/provenance/v1 ]] || { echo 'wrong predicate' >&2; exit 54; }
    [[ "$deny_self_hosted" == true ]] || { echo 'self-hosted runners were not rejected' >&2; exit 55; }
    [[ -f "$file" ]] || { echo 'attested file missing' >&2; exit 56; }
    printf 'verified attestation for %s\n' "$(basename "$file")"
    ;;
  *) echo "unexpected gh invocation: $*" >&2; exit 57 ;;
esac
STUB_GH
chmod +x "$stub_bin/gh"

cat >"$stub_bin/curl" <<'STUB_CURL'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$CURL_STUB_LOG"
echo 'unexpected curl invocation' >&2
exit 58
STUB_CURL
chmod +x "$stub_bin/curl"

cat >"$fixture_dir/$asset" <<'STUB_BINARY'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == version && "${2:-}" == --json ]]; then
  python3 - <<'PY'
import json, os
print(json.dumps({
    "schema_version": 2,
    "tool": "gh-steward",
    "tool_version": os.environ.get("FAKE_BINARY_VERSION", "0.1.0"),
    "source_revision": os.environ.get("FAKE_BINARY_SOURCE", "0123456789abcdef0123456789abcdef01234567"),
    "source_dirty": os.environ.get("FAKE_BINARY_DIRTY", "false") == "true",
    "target": os.environ.get("FAKE_BINARY_TARGET", os.environ["FAKE_BINARY_TARGET_DEFAULT"]),
}))
PY
  exit
fi
echo 'unexpected staged-binary invocation' >&2
exit 59
STUB_BINARY
chmod +x "$fixture_dir/$asset"
python3 - "$fixture_dir/$asset" "$fixture_dir/checksums.txt" <<'PY'
import hashlib, pathlib, sys
asset, checksums = map(pathlib.Path, sys.argv[1:])
checksums.write_text(f"{hashlib.sha256(asset.read_bytes()).hexdigest()}  {asset.name}\n")
PY

export PATH="$stub_bin:$PATH"
export GH_CONFIG_DIR="$gh_config"
export GH_FIXTURE_DIR="$fixture_dir"
export GH_STUB_LOG="$log"
export CURL_STUB_LOG="$tmp_root/curl.log"
export GH_EXPECTED_TAG="$tag"
export GH_EXPECTED_ASSET_PATTERN="gh-steward_${tag}_*"
export GH_EXPECTED_ASSET_NAME="$asset"
export GH_EXPECTED_SOURCE_COMMIT="$good_revision"
export FAKE_BINARY_TARGET_DEFAULT="$expected_target"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_no_partial_install() {
  local dest="$1"
  [[ ! -e "$dest/gh-steward" && ! -L "$dest/gh-steward" ]] || fail "unexpected installed binary in $dest"
  [[ -f "$dest/sentinel" ]] || fail "destination sentinel was lost in $dest"
  [[ "$(cat "$dest/sentinel")" == keep ]] || fail "destination sentinel changed in $dest"
}

make_destination() {
  local dest="$1"
  mkdir -p "$dest"
  printf 'keep\n' >"$dest/sentinel"
}

run_rejected() {
  local label="$1" expected_revision="$2" expected_digest="$3" fake_version="$4" fake_source="$5" fake_dirty="$6" fake_target="$7" attestation="$8"
  local dest="$tmp_root/$label-dest" before_calls
  local -a acquire_args=("$version" "$expected_revision" "$dest")
  [[ -z "$expected_digest" ]] || acquire_args+=("$expected_digest")
  make_destination "$dest"
  before_calls="$(wc -l <"$log" 2>/dev/null || true)"
  if FAKE_BINARY_VERSION="$fake_version" FAKE_BINARY_SOURCE="$fake_source" FAKE_BINARY_DIRTY="$fake_dirty" FAKE_BINARY_TARGET="$fake_target" GH_STUB_ATTESTATION="$attestation" \
      "$acquire" "${acquire_args[@]}" >"$tmp_root/$label.out" 2>"$tmp_root/$label.err"; then
    fail "$label unexpectedly succeeded"
  fi
  assert_no_partial_install "$dest"
  [[ ! -s "$CURL_STUB_LOG" ]] || fail "curl was called during $label"
  [[ -d "$GH_CONFIG_DIR" && -z "$(find "$GH_CONFIG_DIR" -mindepth 1 -print -quit)" ]] || fail "GitHub configuration changed during $label"
  [[ "$(wc -l <"$log")" -gt "$before_calls" ]] || fail "gh stubs were not exercised for $label"
}

# A good candidate is staged into the requested cache without changing GH state.
success_dest="$tmp_root/success"
make_destination "$success_dest"
expected_digest="$(python3 - "$fixture_dir/$asset" <<'PY'
import hashlib, pathlib, sys
print(hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest())
PY
)"
receipt="$("$acquire" "$version" "$good_revision" "$success_dest" "$expected_digest")"
[[ -x "$success_dest/gh-steward" ]] || fail 'successful asset is not executable'
[[ "$(cat "$success_dest/sentinel")" == keep ]] || fail 'successful staging removed unrelated cache content'
[[ -z "$(find "$GH_CONFIG_DIR" -mindepth 1 -print -quit)" ]] || fail 'installer modified GH configuration'
[[ ! -s "$CURL_STUB_LOG" ]] || fail 'installer called curl'
RECEIPT="$receipt" EXPECTED_DIGEST="$expected_digest" EXPECTED_REVISION="$good_revision" EXPECTED_TARGET="$expected_target" EXPECTED_ASSET="$asset" python3 - <<'PY'
import json, os
value = json.loads(os.environ["RECEIPT"])
expected = {
    "schema_version": 1,
    "tool": "gh-steward",
    "tool_version": "0.1.0",
    "source_revision": os.environ["EXPECTED_REVISION"],
    "source_dirty": False,
    "target": os.environ["EXPECTED_TARGET"],
    "asset": os.environ["EXPECTED_ASSET"],
    "asset_sha256": os.environ["EXPECTED_DIGEST"],
}
for key, wanted in expected.items():
    if value.get(key) != wanted:
        raise SystemExit(f"receipt {key} mismatch: {value.get(key)!r}")
if not value.get("path", "").endswith("/gh-steward"):
    raise SystemExit("receipt path mismatch")
PY

# Failed validation preserves existing destination contents and never installs globally.
run_rejected attestation "$good_revision" "" 0.1.0 "$good_revision" false "$expected_target" fail
run_rejected source-digest "$bad_revision" "" 0.1.0 "$good_revision" false "$expected_target" pass
run_rejected caller-digest "$good_revision" "$(printf '0%.0s' {1..64})" 0.1.0 "$good_revision" false "$expected_target" pass
run_rejected wrong-version "$good_revision" "" 0.2.0 "$good_revision" false "$expected_target" pass
run_rejected dirty-binary "$good_revision" "" 0.1.0 "$good_revision" true "$expected_target" pass
run_rejected wrong-source "$good_revision" "" 0.1.0 "$bad_revision" false "$expected_target" pass
if [[ "$expected_target" == darwin/arm64 ]]; then wrong_target=linux/arm64; else wrong_target=darwin/arm64; fi
run_rejected wrong-target "$good_revision" "" 0.1.0 "$good_revision" false "$wrong_target" pass

# A bad checksum fails before staging and keeps preexisting cache files intact.
bad_checksum_dest="$tmp_root/bad-checksum-dest"
make_destination "$bad_checksum_dest"
printf '%064d  %s\n' 0 "$asset" >"$fixture_dir/checksums.txt"
if "$acquire" "$version" "$good_revision" "$bad_checksum_dest" >"$tmp_root/bad-checksum.out" 2>"$tmp_root/bad-checksum.err"; then
  fail 'bad checksum unexpectedly succeeded'
fi
assert_no_partial_install "$bad_checksum_dest"

# Refuse to replace an existing acquisition atomically, without overwriting it.
occupied_dest="$tmp_root/occupied"
make_destination "$occupied_dest"
printf 'original\n' >"$occupied_dest/gh-steward"
if "$acquire" "$version" "$good_revision" "$occupied_dest" >"$tmp_root/occupied.out" 2>"$tmp_root/occupied.err"; then
  fail 'existing target unexpectedly replaced'
fi
[[ "$(cat "$occupied_dest/gh-steward")" == original ]] || fail 'existing target was modified'

# Unsupported host targets are rejected before any release or extension operation.
if PATH="$stub_bin:$PATH" GH_CONFIG_DIR="$gh_config" GH_FIXTURE_DIR="$fixture_dir" GH_STUB_LOG="$log" CURL_STUB_LOG="$CURL_STUB_LOG" GH_EXPECTED_TAG="$tag" GH_EXPECTED_ASSET_PATTERN="gh-steward_${tag}_*" GH_EXPECTED_SOURCE_COMMIT="$good_revision" \
    bash -c 'uname() { printf "MINGW64_NT-10.0\n"; }; export -f uname; exec "$1" "$2" "$3" "$4"' _ "$acquire" "$version" "$good_revision" "$tmp_root/unsupported" >"$tmp_root/unsupported.out" 2>"$tmp_root/unsupported.err"; then
  fail 'unsupported target unexpectedly succeeded'
fi

# gh must never receive extension installation or other unmodeled commands.
if grep -E 'extension install|extension upgrade' "$log" >/dev/null; then
  fail 'installer attempted to install or upgrade the global gh extension'
fi
[[ ! -s "$CURL_STUB_LOG" ]] || fail 'curl was invoked'
echo 'acquisition contract tests passed (provider and network commands stubbed)'
