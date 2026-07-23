#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
# shellcheck source=scripts/action-lib.sh
source "$repo_root/scripts/action-lib.sh"

failures=0

expect_failure() {
  local description=$1
  shift
  if "$@" >/dev/null 2>&1; then
    echo "FAIL: $description unexpectedly succeeded" >&2
    failures=$((failures + 1))
  else
    echo "PASS: $description"
  fi
}

expect_equal() {
  local description=$1 expected=$2 actual=$3
  if [[ "$actual" != "$expected" ]]; then
    echo "FAIL: $description: expected '$expected', got '$actual'" >&2
    failures=$((failures + 1))
  else
    echo "PASS: $description"
  fi
}

expect_equal 'major tag detects prebuilt' 'prebuilt' "$(detect_build_mode v1 v1.2.3)"
expect_equal 'minor tag detects prebuilt' 'prebuilt' "$(detect_build_mode v1.2 v1.2.3)"
expect_equal 'exact tag detects prebuilt' 'prebuilt' "$(detect_build_mode v1.2.3 v1.2.3)"
expect_equal 'branch ref detects source' 'source' "$(detect_build_mode main v1.2.3)"
expect_equal 'empty ref detects source' 'source' "$(detect_build_mode '' v1.2.3)"
expect_equal 'mismatched tag detects source' 'source' "$(detect_build_mode v2 v1.2.3)"
release_sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_equal 'release commit detects prebuilt' 'prebuilt' "$(detect_build_mode "$release_sha" v1.2.3 "$release_sha")"
expect_equal 'other commit detects source' 'source' "$(detect_build_mode bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb v1.2.3 "$release_sha")"
expect_equal 'commit without resolved release detects source' 'source' "$(detect_build_mode "$release_sha" v1.2.3 '')"

expect_equal 'Linux x64 mapping' 'easysftp_linux_x64' "$(resolve_release_asset Linux X64)"
expect_equal 'Linux arm64 mapping' 'easysftp_linux_arm64' "$(resolve_release_asset Linux ARM64)"
expect_equal 'macOS x64 mapping' 'easysftp_macos_x64' "$(resolve_release_asset macOS X64)"
expect_equal 'macOS arm64 mapping' 'easysftp_macos_arm64' "$(resolve_release_asset macOS ARM64)"
expect_equal 'Windows x64 mapping' 'easysftp_windows_x64.exe' "$(resolve_release_asset Windows X64)"
expect_equal 'Windows arm64 mapping' 'easysftp_windows_arm64.exe' "$(resolve_release_asset Windows ARM64)"
expect_failure 'unsupported OS' resolve_release_asset FreeBSD X64
expect_failure 'unsupported architecture' resolve_release_asset Linux RISCV64

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/action" "$tmp/assets" "$tmp/bin" "$tmp/runner"
cp "$repo_root/.easysftp-version" "$tmp/action/.easysftp-version"
# The prebuilt launcher rejects a ref that disagrees with .easysftp-version, so the
# ref has to follow the release-please bump instead of being pinned to one release.
release_version=$(read_release_version "$repo_root/.easysftp-version")
printf '#!/usr/bin/env bash\necho prebuilt-ok\n' > "$tmp/assets/easysftp_linux_x64"
chmod +x "$tmp/assets/easysftp_linux_x64"
hash=$(sha256_file "$tmp/assets/easysftp_linux_x64")
printf '%s  %s\n' "$hash" 'easysftp_linux_x64' > "$tmp/assets/checksums.txt"

cat > "$tmp/bin/curl" <<'MOCK_CURL'
#!/usr/bin/env bash
set -euo pipefail
output=''
url=''
while (( $# )); do
  case "$1" in
    --output)
      output=$2
      shift 2
      ;;
    --max-redirs | --retry | --max-filesize)
      shift 2
      ;;
    --proto | --proto-redir)
      shift 2
      ;;
    --location | --fail | --silent | --show-error | --retry-all-errors)
      shift
      ;;
    *)
      url=$1
      shift
      ;;
  esac
done
cp "$MOCK_ASSET_DIR/${url##*/}" "$output"
MOCK_CURL
chmod +x "$tmp/bin/curl"

cat > "$tmp/bin/git" <<'MOCK_GIT'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" != 'ls-remote' ]]; then
  exit 2
fi
printf '%s\t%s\n' \
  '1111111111111111111111111111111111111111' 'refs/tags/v1.2.3' \
  'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' 'refs/tags/v1.2.3^{}'
MOCK_GIT
chmod +x "$tmp/bin/git"

output_file="$tmp/prebuilt-output"
PATH="$tmp/bin:$PATH" \
MOCK_ASSET_DIR="$tmp/assets" \
ACTION_PATH="$tmp/action" \
ACTION_REF="$release_version" \
RUNNER_OS=Linux \
RUNNER_ARCH=X64 \
RUNNER_TEMP="$tmp/runner" \
GITHUB_OUTPUT="$output_file" \
  bash "$repo_root/scripts/prepare-action.sh"
prepared_binary=$(sed -n 's/^binary=//p' "$output_file")
prebuilt_result=$("$prepared_binary")
expect_equal 'prebuilt execution' 'prebuilt-ok' "$prebuilt_result"

source_output="$tmp/source-output"
source_action_path="$tmp/action"
source_runner_temp="$tmp/runner"
source_github_output="$source_output"
source_runner_os=Linux
if [[ "${RUNNER_OS:-}" == 'Windows' ]] && command -v cygpath >/dev/null 2>&1; then
  source_action_path=$(cygpath -w "$source_action_path")
  source_runner_temp=$(cygpath -w "$source_runner_temp")
  source_github_output=$(cygpath -w "$source_github_output")
  source_runner_os=Windows
fi
ACTION_PATH="$source_action_path" \
ACTION_REF=main \
RUNNER_OS="$source_runner_os" \
RUNNER_ARCH=X64 \
RUNNER_TEMP="$source_runner_temp" \
GITHUB_OUTPUT="$source_github_output" \
  bash "$repo_root/scripts/prepare-action.sh"
expect_equal 'source preparation' 'source' "$(sed -n 's/^build-mode=//p' "$source_output")"

# The build-mode input became a tombstone in v3: setting it must fail with a
# migration hint instead of being honored or ignored.
expect_failure 'removed build-mode input' env \
  INPUT_BUILD_MODE=source \
  ACTION_PATH="$source_action_path" \
  ACTION_REF=main \
  RUNNER_OS="$source_runner_os" \
  RUNNER_ARCH=X64 \
  RUNNER_TEMP="$source_runner_temp" \
  GITHUB_OUTPUT="$tmp/tombstone-output" \
  bash "$repo_root/scripts/prepare-action.sh"

printf '%s\n' '# x-release-please-start-version' '1.2.3' '# x-release-please-end' > "$tmp/bad-version"
expect_failure 'invalid version file' read_release_version "$tmp/bad-version"
expect_equal 'annotated release commit resolution' "$release_sha" "$(PATH="$tmp/bin:$PATH" resolve_release_commit v1.2.3)"

: > "$tmp/missing-checksum"
expect_failure 'missing checksum entry' verify_release_checksum "$tmp/assets/easysftp_linux_x64" "$tmp/missing-checksum" 'easysftp_linux_x64'
printf '%064d  easysftp_linux_x64\n' 0 > "$tmp/wrong-checksum"
expect_failure 'wrong checksum' verify_release_checksum "$tmp/assets/easysftp_linux_x64" "$tmp/wrong-checksum" 'easysftp_linux_x64'

if (( failures != 0 )); then
  echo "$failures action test(s) failed" >&2
  exit 1
fi

echo 'All action launcher tests passed.'
