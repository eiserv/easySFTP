#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=scripts/action-lib.sh
source "$script_dir/action-lib.sh"

if [[ -n "${INPUT_BUILD_MODE:-}" ]]; then
  easysftp_error "the 'build-mode' input was removed in easySFTP v3; the build mode is now selected automatically from the action ref. See docs/migration-v3.md"
fi

action_path=${ACTION_PATH:?ACTION_PATH is required}
action_path=${action_path//\\//}
while [[ "$action_path" == *'/./'* ]]; do
  action_path=${action_path//\/\.\//\/}
done
action_path=${action_path%/.}
action_path=${action_path%/}
temp_root=${RUNNER_TEMP:?RUNNER_TEMP is required}
temp_root=${temp_root//\\//}
output_file=${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}
output_file=${output_file//\\//}
work_dir=$(mktemp -d "$temp_root/easysftp-action.XXXXXX")

if [[ "${RUNNER_OS:-}" == 'Windows' ]]; then
  binary="$work_dir/easysftp.exe"
else
  binary="$work_dir/easysftp"
fi

version=$(read_release_version "$action_path/.easysftp-version")
release_commit=''
if [[ "${ACTION_REF:-}" =~ ^[0-9a-f]{40}$ ]] ||
  is_release_tag_ref "${ACTION_REF:-}" "$version"; then
  # Best effort in both cases, for different reasons. A full-SHA ref that
  # cannot be resolved falls back to a source build of this exact checkout
  # instead of failing the run or substituting a stale release binary. A tag
  # ref that cannot be resolved still downloads the release binary, and the
  # warning below says what that costs the provenance check (issue #284).
  release_commit=$(resolve_release_commit "$version" 2>/dev/null || true)
fi
mode=$(detect_build_mode "${ACTION_REF:-}" "$version" "$release_commit")

if [[ "$mode" == 'prebuilt' && -z "$release_commit" ]]; then
  # Only a tag ref reaches here: a full-SHA ref without a resolved release
  # commit selects the source build above. The provenance check still runs,
  # but without --source-digest it accepts a binary built by the trusted
  # workflow from any commit, so the run says so instead of looking fully
  # verified (issue #284).
  echo "::warning::easySFTP action: could not resolve the exact $version release commit, so the provenance check runs without a source-digest pin and accepts any binary built by the trusted release workflow. The checksum check still ran; this is a resolution failure, not a verification failure."
fi

if [[ "$mode" == 'prebuilt' ]]; then
  asset=$(resolve_release_asset "${RUNNER_OS:-}" "${RUNNER_ARCH:-}")
  checksums="$work_dir/checksums.txt"

  download_release_file "$version" 'checksums.txt' "$checksums" 1048576
  download_release_file "$version" "$asset" "$binary" 104857600
  verify_release_checksum "$binary" "$checksums" "$asset"
  # The checksum proves the download was not corrupted in transit; the
  # attestation is what proves the asset is the one this repository's release
  # workflow built. See verify_release_provenance and issue #146.
  # A release ref (a tag spelling or a full SHA) adds one more invariant:
  # the attested source commit must be the exact commit the release was
  # built from, not merely some release built by the trusted workflow. For a
  # full-SHA ref that is also the commit the user pinned (issue #284).
  verify_release_provenance "$binary" "$asset" "$version" "$release_commit"
  chmod +x "$binary"
  echo "Using verified easySFTP $version release asset $asset"
else
  echo "Ref '${ACTION_REF:-<local>}' is not the $version release; building easySFTP from source"
fi

{
  echo "build-mode=$mode"
  echo "binary=$binary"
  echo "action-dir=$action_path"
} >> "$output_file"
