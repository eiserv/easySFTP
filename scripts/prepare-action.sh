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
if [[ "${ACTION_REF:-}" =~ ^[0-9a-f]{40}$ ]]; then
  # Best effort: an unresolvable release commit (network hiccup, unpublished
  # tag) falls back to a source build of this exact checkout instead of
  # failing the run or substituting a stale release binary.
  release_commit=$(resolve_release_commit "$version" 2>/dev/null || true)
fi
mode=$(detect_build_mode "${ACTION_REF:-}" "$version" "$release_commit")

if [[ "$mode" == 'prebuilt' ]]; then
  asset=$(resolve_release_asset "${RUNNER_OS:-}" "${RUNNER_ARCH:-}")
  checksums="$work_dir/checksums.txt"

  download_release_file "$version" 'checksums.txt' "$checksums" 1048576
  download_release_file "$version" "$asset" "$binary" 104857600
  verify_release_checksum "$binary" "$checksums" "$asset"
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
