#!/usr/bin/env bash

set -euo pipefail

easysftp_error() {
  local message=$*
  message=${message//$'\r'/ }
  message=${message//$'\n'/ }
  printf '::error::easySFTP action: %s\n' "$message" >&2
  return 1
}

# easysftp_add_mask registers one value with the runner's log masker, so any
# later line that happens to contain it prints as *** instead.
#
# The runner already does this for anything read from secrets.*, which covers
# the documented path. It does not cover a credential produced by an earlier
# step's output, read from a matrix entry or a plain environment variable, or
# returned by a vault action, and it cannot cover a literal somebody inlined
# against advice. See issue #149.
#
# The value is percent-encoded the way the workflow-command protocol expects,
# which matters twice here: a newline inside a credential would otherwise end
# the command and print the remainder as an ordinary log line, in clear text,
# and a literal % would be read back as the start of an escape.
#
# An empty or whitespace-only value is skipped on purpose: asking the runner to
# mask it would redact large parts of the log.
easysftp_add_mask() {
  local value=$1
  if [[ -z "${value//[[:space:]]/}" ]]; then
    return 0
  fi
  value=${value//%/%25}
  value=${value//$'\r'/%0D}
  value=${value//$'\n'/%0A}
  printf '::add-mask::%s\n' "$value"
}

# mask_credentials masks every password and passphrase the action was given,
# direct and proxy, from the environment the caller set up.
#
# The private keys are deliberately not masked. ::add-mask:: is line oriented,
# so a PEM block would have to be masked line by line, and masking the short
# base64 lines of a key garbles unrelated log output for the rest of the job.
# Key material is never printed anywhere, and the passphrase that protects it
# is masked, so the cost would buy nothing.
mask_credentials() {
  easysftp_add_mask "${EASYSFTP_PASSWORD:-}"
  easysftp_add_mask "${EASYSFTP_PASSPHRASE:-}"
  easysftp_add_mask "${EASYSFTP_PROXY_PASSWORD:-}"
  easysftp_add_mask "${EASYSFTP_PROXY_PASSPHRASE:-}"
}

# detect_build_mode picks how to obtain the easySFTP binary from the action
# ref alone (the build-mode input was removed in v3): a ref matching the
# checkout's release version (tag or exact release commit) uses the verified
# prebuilt release asset; every development ref (branches, other commit SHAs,
# local "uses: ./") builds the checkout from source.
detect_build_mode() {
  local action_ref=$1
  local version=$2
  local release_commit=${3:-}
  local major minor patch

  IFS=. read -r major minor patch <<< "${version#v}"
  case "$action_ref" in
    "v$major" | "v$major.$minor" | "v$major.$minor.$patch")
      printf 'prebuilt\n'
      ;;
    *)
      if [[ "$action_ref" =~ ^[0-9a-f]{40}$ ]] && [[ -n "$release_commit" ]] && [[ "$action_ref" == "$release_commit" ]]; then
        printf 'prebuilt\n'
      else
        printf 'source\n'
      fi
      ;;
  esac
}

read_release_version() {
  local version_file=$1
  local -a lines=()

  if [[ ! -f "$version_file" ]]; then
    easysftp_error "version file '$version_file' is missing"
    return 1
  fi

  mapfile -t lines < "$version_file"
  if (( ${#lines[@]} != 3 )) ||
    [[ "${lines[0]}" != '# x-release-please-start-version' ]] ||
    [[ "${lines[2]}" != '# x-release-please-end' ]]; then
    easysftp_error "version file '$version_file' has an invalid format"
    return 1
  fi

  if [[ ! "${lines[1]}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    easysftp_error "version '${lines[1]}' is invalid; expected vMAJOR.MINOR.PATCH"
    return 1
  fi

  printf '%s\n' "${lines[1]}"
}

resolve_release_commit() {
  local version=$1
  local sha ref direct='' peeled=''

  while IFS=$'\t' read -r sha ref; do
    case "$ref" in
      "refs/tags/$version") direct=$sha ;;
      "refs/tags/$version^{}") peeled=$sha ;;
    esac
  # Bounded on purpose (issue #236). This runs on every SHA-pinned job, and
  # an unbounded git talking to github.com is the same hang as an unbounded
  # curl: the step never fails, it just runs until the workflow timeout,
  # which defaults to six hours. The low-speed pair aborts a connection that
  # stops delivering, and GIT_TERMINAL_PROMPT stops a credential prompt from
  # waiting on a stdin nobody is attached to. Resolution here is best
  # effort, so an abort just falls back to a source build.
  done < <(GIT_TERMINAL_PROMPT=0 \
    GIT_HTTP_LOW_SPEED_LIMIT=1000 \
    GIT_HTTP_LOW_SPEED_TIME=30 \
    git ls-remote --exit-code \
    'https://github.com/eiserv/easySFTP.git' \
    "refs/tags/$version" \
    "refs/tags/$version^{}")

  sha=${peeled:-$direct}
  if [[ ! "$sha" =~ ^[0-9a-f]{40}$ ]]; then
    easysftp_error "could not resolve the exact $version release commit from eiserv/easySFTP"
    return 1
  fi
  printf '%s\n' "$sha"
}

resolve_release_asset() {
  case "${1:-}/${2:-}" in
    Linux/X64) printf '%s\n' 'easysftp_linux_x64' ;;
    Linux/ARM64) printf '%s\n' 'easysftp_linux_arm64' ;;
    macOS/X64) printf '%s\n' 'easysftp_macos_x64' ;;
    macOS/ARM64) printf '%s\n' 'easysftp_macos_arm64' ;;
    Windows/X64) printf '%s\n' 'easysftp_windows_x64.exe' ;;
    Windows/ARM64) printf '%s\n' 'easysftp_windows_arm64.exe' ;;
    *)
      easysftp_error "unsupported runner platform '${1:-}/${2:-}'; supported OS values are Linux, macOS, and Windows with X64 or ARM64"
      ;;
  esac
}

sha256_file() {
  local path=$1

  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print tolower($1)}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$path" | awk '{print tolower($1)}'
  else
    easysftp_error "no SHA-256 implementation found (sha256sum or shasum is required)"
  fi
}

verify_release_checksum() {
  local binary=$1
  local checksums=$2
  local asset=$3
  local line hash filename expected='' matches=0 actual

  if [[ ! -f "$checksums" ]]; then
    easysftp_error "checksums.txt is missing"
    return 1
  fi

  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" =~ ^([0-9A-Fa-f]{64})[[:space:]]+([^[:space:]]+)$ ]]; then
      hash=${BASH_REMATCH[1],,}
      filename=${BASH_REMATCH[2]}
      filename=${filename#\*}
      if [[ "$filename" == "$asset" ]]; then
        expected=$hash
        matches=$((matches + 1))
      fi
    fi
  done < "$checksums"

  if (( matches == 0 )); then
    easysftp_error "checksums.txt has no SHA-256 entry for '$asset'"
    return 1
  fi
  if (( matches != 1 )); then
    easysftp_error "checksums.txt contains multiple SHA-256 entries for '$asset'"
    return 1
  fi

  actual=$(sha256_file "$binary")
  if [[ "$actual" != "$expected" ]]; then
    easysftp_error "SHA-256 mismatch for '$asset' (expected $expected, got $actual)"
    return 1
  fi
}

# provenance_repo / provenance_workflow name what a release asset must have
# been built by. They are constants rather than parameters so that a caller
# cannot be talked into accepting an attestation from somewhere else.
provenance_repo='eiserv/easySFTP'
provenance_workflow='eiserv/easySFTP/.github/workflows/release-binaries.yml'

# verify_release_provenance checks a downloaded release binary against the
# build provenance attestation that release-binaries.yml recorded for it.
#
# It exists because the SHA-256 check on its own proves very little (issue
# #146). checksums.txt is downloaded from the same GitHub release as the
# binary, and release assets are mutable by design here: release-binaries.yml
# uploads with --clobber and repair-release.yml exists to re-run that upload
# for an already published tag. Anyone able to write release assets could
# replace both files together and the checksum would still match. The
# attestation is not an asset: it is signed with the workflow's OIDC identity
# and stored by GitHub, so it says which repository, which workflow and which
# commit produced this exact digest, and a replaced asset has no attestation
# that matches it.
#
# A verification that runs and fails is fatal. A verification that cannot run
# at all (no gh, no token, a gh too old for the command) warns and lets the
# run continue on the checksum alone, which is what it had before this
# existed. That asymmetry is deliberate: tampering fails closed, while a
# runner without the tooling keeps working, and the warning names the escape
# hatch that gives a hard guarantee (pin to a non-release commit and the
# action builds from source, which is fully covered by the pin).
verify_release_provenance() {
  local binary=$1
  local asset=$2
  local version=$3
  local token=${GH_TOKEN:-${GITHUB_TOKEN:-}}
  local output=''

  if ! command -v gh >/dev/null 2>&1; then
    provenance_unavailable 'the GitHub CLI (gh) is not installed on this runner'
    return 0
  fi
  if [[ -z "$token" ]]; then
    provenance_unavailable 'no GitHub token was available to read the attestation'
    return 0
  fi
  if ! gh attestation verify --help >/dev/null 2>&1; then
    provenance_unavailable "this runner's gh is too old for 'gh attestation verify'"
    return 0
  fi

  if ! output=$(GH_TOKEN="$token" gh attestation verify "$binary" \
    --repo "$provenance_repo" \
    --signer-workflow "$provenance_workflow" 2>&1); then
    printf '%s\n' "$output" >&2
    easysftp_error "build provenance verification failed for the $version release asset '$asset'; it was not built by $provenance_workflow. Do not use this run's result. Pin the action to a non-release commit to build from source instead."
    return 1
  fi
  printf 'Verified build provenance for %s (%s, built by %s)\n' "$asset" "$version" "$provenance_workflow"
}

# provenance_unavailable reports that the provenance check could not run, and
# says what the run is left with. A warning rather than an error; see
# verify_release_provenance.
provenance_unavailable() {
  printf '::warning::easySFTP action: could not verify the build provenance of the release binary (%s). The SHA-256 check against checksums.txt still ran, but both files come from the same mutable release, so it proves transport integrity only. Pin the action to a non-release commit for a source build that is fully covered by the pin; see docs/security.md.\n' "$1"
}

download_release_file() {
  local version=$1
  local asset=$2
  local output=$3
  local max_size=$4
  local url="https://github.com/eiserv/easySFTP/releases/download/${version}/${asset}"

  # The three timeouts are load-bearing (issue #236). curl supplies none of
  # them by default, so a transfer that stalls after connecting waits
  # forever, and --retry only fires on a failure that completed. Without
  # them the job does not fail, it hangs until the workflow timeout, which
  # defaults to six hours on GitHub-hosted runners and is usually left
  # there. --max-time bounds one attempt (180s is well over what a 6 MB
  # binary needs at any plausible runner speed) and --retry-max-time
  # bounds when a new attempt may still start, so the call as a whole is
  # bounded rather than three full budgets deep. --max-filesize stays a
  # sanity check rather than a bound: curl only enforces it once a
  # Content-Length declares the size or the limit is already exceeded.
  curl \
    --proto '=https' \
    --proto-redir '=https' \
    --location \
    --max-redirs 5 \
    --fail \
    --silent \
    --show-error \
    --connect-timeout 15 \
    --max-time 180 \
    --retry 3 \
    --retry-all-errors \
    --retry-max-time 300 \
    --max-filesize "$max_size" \
    --output "$output" \
    "$url"
}
