#!/usr/bin/env bash
#
# Self-check for scripts/benchmark-store.sh: retention window, archiving,
# the latest.* pointer and the refusal to rewrite a stored result. Runs
# against a temporary directory, never against the repository's benchmarks/.

set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

export BENCH_DIR="$work/benchmarks"
results="$work/results.json"
summary="$work/summary.md"

cat >"$results" <<'JSON'
{
  "candidate_ref": "test-ref",
  "baseline_ref": "",
  "repeats": 1,
  "results": [
    { "label": "candidate", "scenario": "small", "median_ms": 1000 },
    { "label": "baseline", "scenario": "small", "median_ms": 1200 }
  ]
}
JSON
echo "## easySFTP benchmark" >"$summary"

failures=0

pass() { echo "PASS: $1"; }
fail() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}

store() { # store <kind> <version-or-label> <recorded_at>
  local kind=$1 name=$2 stamp=$3 version=""
  if [[ $kind == release ]]; then
    version=$name
  fi
  RESULTS_JSON="$results" SUMMARY_MD="$summary" \
    KIND="$kind" VERSION="$version" LABEL="$name" RECORDED_AT="$stamp" \
    bash "$repo_root/scripts/benchmark-store.sh" >/dev/null
}

expect_file() {
  if [[ -e "$BENCH_DIR/$1" ]]; then pass "$1 exists"; else fail "$1 is missing"; fi
}

expect_absent() {
  if [[ -e "$BENCH_DIR/$1" ]]; then fail "$1 should not exist"; else pass "$1 absent"; fi
}

expect_equal() {
  local description=$1 expected=$2 actual=$3
  if [[ "$actual" == "$expected" ]]; then
    pass "$description"
  else
    fail "$description: expected '$expected', got '$actual'"
  fi
}

expect_failure() {
  local description=$1
  shift
  if "$@" >/dev/null 2>&1; then
    fail "$description unexpectedly succeeded"
  else
    pass "$description"
  fi
}

# Eight releases plus two manual runs, oldest first. Three releases fall out
# of the five-wide window, which is where version order and plain string order
# disagree: sorted as text, v1.0.10 would be archived and v1.0.2 kept.
store release v1.0.0 2026-01-01T00:00:00Z
store manual old-run 2026-01-15T00:00:00Z
store release v1.0.1 2026-02-01T00:00:00Z
store release v1.0.2 2026-03-01T00:00:00Z
store release v1.0.9 2026-04-01T00:00:00Z
store release v1.0.10 2026-05-01T00:00:00Z
store release v1.1.0 2026-06-01T00:00:00Z
store manual new-run 2026-06-15T00:00:00Z
store release v1.1.1 2026-07-01T00:00:00Z
store release v1.2.0 2026-08-01T00:00:00Z

for version in v1.0.0 v1.0.1 v1.0.2; do
  expect_absent "release-$version.json"
  expect_file "archive/release-$version.json"
  expect_file "archive/release-$version.md"
done
for version in v1.0.9 v1.0.10 v1.1.0 v1.1.1 v1.2.0; do
  expect_file "release-$version.json"
  expect_file "release-$version.md"
done

expect_equal 'latest.json points at the newest release' v1.2.0 \
  "$(jq -r .version "$BENCH_DIR/latest.json")"
expect_equal 'latest.md carries the newest release' 1 \
  "$(grep -c 'release v1.2.0' "$BENCH_DIR/latest.md")"

expect_absent manual-20260115T000000Z-old-run.json
expect_file archive/manual-20260115T000000Z-old-run.json
expect_file manual-20260615T000000Z-new-run.json
expect_equal 'a manual run is not an official reference' false \
  "$(jq -r .official "$BENCH_DIR/manual-20260615T000000Z-new-run.json")"

expect_equal 'index lists every stored result' 10 \
  "$(jq '.entries | length' "$BENCH_DIR/index.json")"
expect_equal 'index marks the archived results' 4 \
  "$(jq '[.entries[] | select(.archived)] | length' "$BENCH_DIR/index.json")"
expect_equal 'index names the latest release' v1.2.0 \
  "$(jq -r .latest_release "$BENCH_DIR/index.json")"
expect_equal 'index carries the candidate medians' 1000 \
  "$(jq -r '.entries[0].median_ms.small' "$BENCH_DIR/index.json")"
expect_equal 'the raw measurement is kept verbatim' test-ref \
  "$(jq -r .benchmark.candidate_ref "$BENCH_DIR/latest.json")"

expect_failure 'storing a release twice' store release v1.2.0 2026-08-02T00:00:00Z
expect_failure 'storing an archived release again' store release v1.0.0 2026-08-02T00:00:00Z
expect_failure 'a non-release version' store release 1.2.0 2026-08-02T00:00:00Z
expect_failure 'an unknown kind' store nightly whatever 2026-08-02T00:00:00Z

# A manual run must not be able to write outside benchmarks/, whatever the
# workflow input says.
store manual '../../escaped run' 2026-08-16T00:00:00Z
expect_file 'manual-20260816T000000Z-..-..-escaped-run.json'
expect_absent '../../escaped run.json'
expect_equal 'latest.json still points at the newest release' v1.2.0 \
  "$(jq -r .version "$BENCH_DIR/latest.json")"

if ((failures > 0)); then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "all benchmark-store checks passed"
