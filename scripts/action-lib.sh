#!/usr/bin/env bash

set -euo pipefail

easysftp_error() {
  local message=$*
  message=${message//$'\r'/ }
  message=${message//$'\n'/ }
  printf '::error::easySFTP action: %s\n' "$message" >&2
  return 1
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
  done < <(git ls-remote --exit-code \
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

download_release_file() {
  local version=$1
  local asset=$2
  local output=$3
  local max_size=$4
  local url="https://github.com/eiserv/easySFTP/releases/download/${version}/${asset}"

  curl \
    --proto '=https' \
    --proto-redir '=https' \
    --location \
    --max-redirs 5 \
    --fail \
    --silent \
    --show-error \
    --retry 3 \
    --retry-all-errors \
    --max-filesize "$max_size" \
    --output "$output" \
    "$url"
}
