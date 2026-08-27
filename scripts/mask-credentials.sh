#!/usr/bin/env bash

# Runs as the action's first step, before anything else has a chance to log.
# Everything after this point is covered: the prepare step, the Go build, the
# upload itself, and any error text from any of them that quotes a value.
# See mask_credentials in action-lib.sh and issue #149.

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=scripts/action-lib.sh
source "$script_dir/action-lib.sh"

mask_credentials
