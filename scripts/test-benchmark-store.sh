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

matrix_results="$work/matrix.json"
matrix_csv="$work/matrix.csv"

cat >"$results" <<'JSON'
{
  "schema_version": 2,
  "benchmark_kind": "standard",
  "candidate_ref": "test-ref",
  "baseline_ref": "",
  "repeats": 1,
  "runner": "self-hosted, Linux 6.1.0, 4 cpu",
  "environment": { "os": "Linux", "cpus": 4 },
  "results": [
    { "label": "candidate", "scenario": "small", "median_ms": 1000 },
    { "label": "baseline", "scenario": "small", "median_ms": 1200 }
  ]
}
JSON
cat >"$matrix_results" <<'JSON'
{
  "schema_version": 2,
  "benchmark_kind": "matrix",
  "candidate_ref": "test-ref",
  "axes": { "connections": [1, 2], "concurrency": [1, 4] },
  "cells": [
    { "scenario": "small", "label": "candidate", "connections": 1, "concurrency": 1, "median_ms": 1400 },
    { "scenario": "small", "label": "candidate", "connections": 2, "concurrency": 4, "median_ms": 800 }
  ],
  "scaling": [
    { "scenario": "small", "label": "candidate", "best": { "connections": 2, "concurrency": 4, "median_ms": 800 } }
  ]
}
JSON
printf 'scenario,build,median_ms\nsmall,candidate,800\n' >"$matrix_csv"
echo "## easySFTP benchmark" >"$summary"

failures=0

pass() { echo "PASS: $1"; }
fail() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}

store() { # store <kind> <version-or-label> <recorded_at>
  local kind=$1 name=$2 stamp=$3 version="" json=$results csv=""
  if [[ $kind == release ]]; then
    version=$name
  fi
  if [[ $kind == matrix ]]; then
    json=$matrix_results
    csv=$matrix_csv
  fi
  RESULTS_JSON="$json" SUMMARY_MD="$summary" RESULTS_CSV="$csv" \
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
store matrix sweep 2026-07-20T00:00:00Z
store release v1.2.0 2026-08-01T00:00:00Z

for version in v1.0.0 v1.0.1 v1.0.2; do
  expect_absent "releases/release-$version.json"
  expect_file "archive/releases/release-$version.json"
  expect_file "archive/releases/release-$version.md"
done
for version in v1.0.9 v1.0.10 v1.1.0 v1.1.1 v1.2.0; do
  expect_file "releases/release-$version.json"
  expect_file "releases/release-$version.md"
done

expect_equal 'latest.json points at the newest release' v1.2.0 \
  "$(jq -r .version "$BENCH_DIR/latest.json")"
expect_equal 'latest.md carries the newest release' 1 \
  "$(grep -c 'release v1.2.0' "$BENCH_DIR/latest.md")"
expect_equal 'the envelope is schema_version 2' 2 \
  "$(jq -r .schema_version "$BENCH_DIR/latest.json")"

expect_absent manual/manual-20260115T000000Z-old-run.json
expect_file archive/manual/manual-20260115T000000Z-old-run.json
expect_file manual/manual-20260615T000000Z-new-run.json
expect_equal 'a manual run is not an official reference' false \
  "$(jq -r .official "$BENCH_DIR/manual/manual-20260615T000000Z-new-run.json")"

# A matrix run is filed under its own kind, keeps its CSV and never becomes a
# reference: it sweeps settings a normal deploy does not use.
expect_file matrix/matrix-20260720T000000Z-sweep.json
expect_file matrix/matrix-20260720T000000Z-sweep.md
expect_file matrix/matrix-20260720T000000Z-sweep.csv
expect_equal 'a matrix run is not an official reference' false \
  "$(jq -r .official "$BENCH_DIR/matrix/matrix-20260720T000000Z-sweep.json")"
expect_equal 'latest.json is still the release, not the matrix run' v1.2.0 \
  "$(jq -r .version "$BENCH_DIR/latest.json")"
expect_equal 'index reports the best cell of a matrix run' 800 \
  "$(jq -r '.entries[] | select(.benchmark_kind == "matrix") | .best_ms.small' "$BENCH_DIR/index.json")"
expect_equal 'index tells a matrix entry apart from a standard one' matrix \
  "$(jq -r '.entries[] | select(.kind == "matrix") | .benchmark_kind' "$BENCH_DIR/index.json")"

expect_equal 'index lists every stored result' 11 \
  "$(jq '.entries | length' "$BENCH_DIR/index.json")"
expect_equal 'index marks the archived results' 4 \
  "$(jq '[.entries[] | select(.archived)] | length' "$BENCH_DIR/index.json")"
expect_equal 'index names the latest release' v1.2.0 \
  "$(jq -r .latest_release "$BENCH_DIR/index.json")"
expect_equal 'index carries the candidate medians' 1000 \
  "$(jq -r '.entries[0].median_ms.small' "$BENCH_DIR/index.json")"
expect_equal 'index links the file it describes' releases/release-v1.2.0.json \
  "$(jq -r '.entries[0].json' "$BENCH_DIR/index.json")"
expect_equal 'index carries the environment forward' Linux \
  "$(jq -r '.entries[0].environment.os' "$BENCH_DIR/index.json")"
expect_equal 'the raw measurement is kept verbatim' test-ref \
  "$(jq -r .benchmark.candidate_ref "$BENCH_DIR/latest.json")"

# trend.csv is the flat "over releases" export: a header plus one row per
# candidate scenario of every stored non-matrix result (10 of those, one
# scenario each).
expect_equal 'trend.csv covers every stored standard result' 11 \
  "$(wc -l <"$BENCH_DIR/trend.csv")"
expect_equal 'trend.csv carries the throughput index.json lacks' '"scenario","files","bytes","median_ms"' \
  "$(head -1 "$BENCH_DIR/trend.csv" | cut -d, -f7-10)"
expect_equal 'trend.csv leaves matrix runs out' 0 \
  "$(grep -c '"matrix"' "$BENCH_DIR/trend.csv" || true)"

expect_failure 'storing a release twice' store release v1.2.0 2026-08-02T00:00:00Z
expect_failure 'storing an archived release again' store release v1.0.0 2026-08-02T00:00:00Z
expect_failure 'a non-release version' store release 1.2.0 2026-08-02T00:00:00Z
expect_failure 'an unknown kind' store nightly whatever 2026-08-02T00:00:00Z

# A manual run must not be able to write outside benchmarks/, whatever the
# workflow input says.
store manual '../../escaped run' 2026-08-16T00:00:00Z
expect_file 'manual/manual-20260816T000000Z-..-..-escaped-run.json'
expect_absent '../../escaped run.json'
expect_equal 'latest.json still points at the newest release' v1.2.0 \
  "$(jq -r .version "$BENCH_DIR/latest.json")"

if ((failures > 0)); then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "all benchmark-store checks passed"
