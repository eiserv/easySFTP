#!/usr/bin/env bash
#
# Self-check for scripts/benchmark.sh and scripts/benchmark-matrix.sh.
#
# Neither script cares what the "easySFTP binary" it runs actually is: it sets
# EASYSFTP_* plus GITHUB_OUTPUT and EASYSFTP_METRICS_FILE and runs it. So this
# test substitutes a stub that writes plausible step outputs and a plausible
# metrics file, and then asserts on the JSON, CSV and Markdown that come out.
#
# That covers what actually breaks in these scripts (the jq aggregation, the
# schema, the delta arithmetic, the CSV columns) without needing an SFTP server,
# a network or a real build. What it deliberately does not cover is whether
# easySFTP itself is fast; that is what the real benchmark is for.

set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
work=$(mktemp -d)
# KEEP_WORK=1 leaves the generated results behind: when a check fails, the
# interesting thing is almost always the JSON it failed on.
if [[ -z ${KEEP_WORK:-} ]]; then
  trap 'rm -rf "$work"' EXIT
else
  echo "working directory: $work"
fi

failures=0
pass() { echo "PASS: $1"; }
fail() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}
expect_equal() {
  local description=$1 expected=$2 actual=$3
  if [[ "$actual" == "$expected" ]]; then
    pass "$description"
  else
    fail "$description: expected '$expected', got '$actual'"
  fi
}
expect_nonempty() {
  if [[ -n "$2" && "$2" != "null" ]]; then pass "$1"; else fail "$1 is empty"; fi
}

# The stub. It reads the two settings the scripts vary (the source tree and,
# from the generated config file, advanced.*), reports a duration derived from
# them, and writes a metrics file in the real schema. The duration is
# deterministic apart from a small jitter, so the statistics have something to
# chew on without the test being flaky.
stub=$work/easysftp-stub
cat >"$stub" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail

source_dir=${EASYSFTP_SOURCE:-}
connections=1
concurrency=4
if [[ -n "${EASYSFTP_CONFIG:-}" ]]; then
  source_dir=$(awk -F'"' '/source:/ { print $2; exit }' "$EASYSFTP_CONFIG")
  connections=$(awk '/^  connections:/ { print $2; exit }' "$EASYSFTP_CONFIG")
  concurrency=$(awk '/^  concurrency:/ { print $2; exit }' "$EASYSFTP_CONFIG")
  connections=${connections:-1}
  concurrency=${concurrency:-4}
fi

files=0
bytes=0
if [[ -d "$source_dir" ]]; then
  files=$(find "$source_dir" -type f | wc -l)
  bytes=$(find "$source_dir" -type f -printf '%s\n' | awk '{ s += $1 } END { print s + 0 }')
fi

# More connections and more workers finish sooner, with a floor: exactly the
# shape a scaling curve should have, so the matrix output can be checked
# against a known answer.
parallel=$((connections < concurrency ? connections : concurrency))
duration=$((200 + files * 4 / parallel + RANDOM % 7))

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "files-uploaded<<EOF"; echo "$files"; echo "EOF"
    echo "bytes-uploaded<<EOF"; echo "$bytes"; echo "EOF"
    echo "duration-ms<<EOF"; echo "$duration"; echo "EOF"
  } >>"$GITHUB_OUTPUT"
fi

if [[ -n "${EASYSFTP_METRICS_FILE:-}" ]]; then
  cat >"$EASYSFTP_METRICS_FILE" <<JSON
{
  "schema_version": 1,
  "note": "stub",
  "process": {
    "wall_ms": $duration, "user_cpu_ms": 40, "sys_cpu_ms": 12, "cpu_percent": 26,
    "max_rss_bytes": 41943040, "go_total_alloc_bytes": 10485760, "go_mallocs": 5000,
    "go_heap_sys_bytes": 20971520, "go_gc_count": 3, "go_gc_pause_total_ms": 0.4,
    "go_peak_goroutines": $((parallel + 5)), "go_max_procs": 4,
    "disk_read_bytes": $bytes, "disk_write_bytes": 0,
    "net_read_bytes": 4096, "net_write_bytes": $bytes
  },
  "phases": [
    {"name": "upload", "wall_ms": $((duration - 60)), "count": 1},
    {"name": "connect", "wall_ms": 40, "count": 1},
    {"name": "local_scan", "wall_ms": 15, "count": 1},
    {"name": "create_dirs", "wall_ms": 5, "count": 1}
  ],
  "operations": [
    {"name": "file_upload", "count": $files, "errors": 0, "total_ms": $((duration * 2)),
     "avg_ms": 3.5, "min_ms": 1, "p50_ms": 3, "p90_ms": 6, "p99_ms": 9, "max_ms": 11},
    {"name": "sftp_open", "count": $files, "errors": 0, "total_ms": $duration,
     "avg_ms": 1.7, "min_ms": 1, "p50_ms": 1.5, "p90_ms": 3, "p99_ms": 5, "max_ms": 6}
  ],
  "counters": {
    "connections_opened": $connections, "connections_used": $connections,
    "connections_refused": 0, "reconnects": 0, "retries": 0, "stalls": 0, "errors": 0,
    "config_connections": $connections, "config_concurrency": $concurrency,
    "files_uploaded": $files, "bytes_uploaded": $bytes
  }
}
JSON
fi
STUB
chmod +x "$stub"

export CANDIDATE_BIN="$stub" CANDIDATE_REF="candidate-ref"
export BASELINE_BIN="$stub" BASELINE_REF="baseline-ref"
export REMOTE_BASE=/easysftp-benchmark-test
export BENCH_HOST=example.invalid BENCH_PORT=22 BENCH_USERNAME=tester
export BENCH_PASSWORD=secret BENCH_KNOWN_HOSTS="example.invalid ssh-ed25519 AAAA"
export DATASET_DIR="$work/data"

echo "== scripts/benchmark.sh =="
export OUT_DIR="$work/out" LOG_DIR="$work/logs" REPEATS=3 BENCH_CONNECTIONS=2
bash "$repo_root/scripts/benchmark.sh" >"$work/benchmark.stdout" 2>&1 ||
  fail "benchmark.sh exited non-zero (see $work/benchmark.stdout)"

results="$OUT_DIR/results.json"
if [[ ! -f $results ]]; then
  echo "FAIL: benchmark.sh produced no results.json" >&2
  cat "$work/benchmark.stdout" >&2
  exit 1
fi

expect_equal 'results.json is schema_version 2' 2 "$(jq -r .schema_version "$results")"
# Everything a stored v1 result exposed must still be there and still mean the
# same thing: benchmarks/ holds files that were written against v1.
expect_equal 'the v1 top-level keys survive' true \
  "$(jq '["candidate_ref","baseline_ref","repeats","runner","settings","scenarios","results"] | all(. as $k | $k | IN($ARGS.named.keys[]))' \
    --argjson keys "$(jq '[keys[]]' "$results")" "$results")"
expect_equal 'the v1 result fields survive' true \
  "$(jq '.results[0] | has("label") and has("scenario") and has("median_ms") and has("min_ms") and has("max_ms") and has("durations_ms") and has("mib_per_s") and has("files_per_s")' "$results")"
expect_equal 'three builds x three scenarios were aggregated' 9 "$(jq '.results | length' "$results")"
expect_equal 'every repeat is kept raw' 27 "$(jq '.runs | length' "$results")"
expect_equal 'no run failed' 0 "$(jq '[.results[].failed_runs] | add' "$results")"
expect_nonempty 'the environment block is populated' "$(jq -r '.environment.os' "$results")"
expect_nonempty 'the candidate has phase metrics' \
  "$(jq -r '[.results[] | select(.label == "candidate" and .scenario == "small") | .phases[] | select(.name == "upload") | .median_ms] | first' "$results")"
expect_equal 'operations are broken down per round-trip' 'file_upload sftp_open' \
  "$(jq -r '[.results[] | select(.label == "candidate" and .scenario == "small") | .operations[].name] | sort | join(" ")' "$results")"
expect_equal 'the small scenario counts one open per file' 300 \
  "$(jq -r '[.results[] | select(.label == "candidate" and .scenario == "small") | .operations[] | select(.name == "sftp_open") | .count] | first' "$results")"
expect_equal 'process metrics are aggregated' 41943040 \
  "$(jq -r '[.results[] | select(.label == "candidate" and .scenario == "small") | .process.max_rss_bytes] | first' "$results")"

# The pool build gets twice the parallelism from the stub, so it must come out
# ahead of the single-connection baseline. This is the check that the delta
# arithmetic has its sign the right way round.
expect_equal 'the comparison covers candidate and pool for every scenario' 6 \
  "$(jq '.comparison | length' "$results")"
pool_delta=$(jq -r '[.comparison[] | select(.label == "pool2" and .scenario == "small") | .delta_percent] | first' "$results")
if [[ -n $pool_delta ]] && awk -v d="$pool_delta" 'BEGIN { exit !(d < 0) }'; then
  pass "a faster build reports a negative delta ($pool_delta%)"
else
  fail "the pool build should be faster than the baseline, got delta $pool_delta%"
fi
expect_equal 'the MAD of the repeats is reported' 3 \
  "$(jq '[.results[] | select(.label == "candidate") | .mad_ms] | length' "$results")"

expect_equal 'the CSV has a header plus one row per build and scenario' 10 \
  "$(wc -l <"$OUT_DIR/results.csv")"
expect_equal 'the CSV names its columns' '"scenario","build"' \
  "$(head -1 "$OUT_DIR/results.csv" | cut -d, -f1,2)"

for needle in '## easySFTP benchmark' '### Throughput' '### Resources' '### Where the time goes'; do
  if grep -qF "$needle" "$OUT_DIR/summary.md"; then
    pass "summary.md has '$needle'"
  else
    fail "summary.md is missing '$needle'"
  fi
done
# The file count in the third column is what distinguishes a throughput row
# from the resources table's row for the same scenario and build.
expect_equal 'summary.md has a throughput row for every build and scenario' 9 \
  "$(grep -cE '^\| (small|mixed|large) \| (candidate|baseline|pool2) \| [0-9]+ \|' "$OUT_DIR/summary.md")"
expect_equal 'summary.md has a resources row for every build and scenario' 9 \
  "$(grep -cE '^\| (small|mixed|large) \| (candidate|baseline|pool2) \| [0-9.]+ ms \|' "$OUT_DIR/summary.md")"

echo
echo "== scripts/benchmark-matrix.sh =="
export OUT_DIR="$work/matrix-out" LOG_DIR="$work/matrix-logs" REPEATS=1
export MATRIX_CONNECTIONS="1 2" MATRIX_CONCURRENCY="1 4" MATRIX_SCENARIOS="small single"
unset BENCH_CONNECTIONS
bash "$repo_root/scripts/benchmark-matrix.sh" >"$work/matrix.stdout" 2>&1 ||
  fail "benchmark-matrix.sh exited non-zero (see $work/matrix.stdout)"

matrix="$OUT_DIR/matrix.json"
if [[ ! -f $matrix ]]; then
  echo "FAIL: benchmark-matrix.sh produced no matrix.json" >&2
  cat "$work/matrix.stdout" >&2
  exit 1
fi

expect_equal 'matrix.json is schema_version 2' 2 "$(jq -r .schema_version "$matrix")"
expect_equal 'matrix.json says what kind it is' matrix "$(jq -r .benchmark_kind "$matrix")"
# 2 connections x 2 concurrency x 2 scenarios x 2 builds.
expect_equal 'every cell of the grid was measured' 16 "$(jq '.cells | length' "$matrix")"
expect_equal 'connections > concurrency is measured, not rejected' 1 \
  "$(jq '[.cells[] | select(.connections == 2 and .concurrency == 1 and .label == "candidate" and .scenario == "small")] | length' "$matrix")"
expect_nonempty 'cells carry the used connection count' \
  "$(jq -r '[.cells[] | .connections_used] | first' "$matrix")"
expect_equal 'cells carry throughput and file rate' 16 \
  "$(jq '[.cells[] | select(.mib_per_s != null and .files_per_s != null)] | length' "$matrix")"
expect_equal 'cells carry CPU and RSS' 16 \
  "$(jq '[.cells[] | select(.max_rss_bytes != null and .user_cpu_ms != null)] | length' "$matrix")"
expect_equal 'the scaling view is grouped per scenario and build' 4 \
  "$(jq '.scaling | length' "$matrix")"

# The heatmap axes must be explicit: a plot should not have to infer the grid
# from the cells it happens to find.
expect_equal 'the axes are declared' '1 2' "$(jq -r '.axes.connections | join(" ")' "$matrix")"
expect_equal 'the concurrency axis is declared' '1 4' "$(jq -r '.axes.concurrency | join(" ")' "$matrix")"

expect_equal 'the matrix CSV has a header plus one row per cell' 17 \
  "$(wc -l <"$OUT_DIR/matrix.csv")"
if grep -qF '## easySFTP connections/concurrency matrix' "$OUT_DIR/matrix.md"; then
  pass "matrix.md has its heading"
else
  fail "matrix.md is missing its heading"
fi
if grep -qE '^\| *1 \|' "$OUT_DIR/matrix.md"; then
  pass "matrix.md renders a grid"
else
  fail "matrix.md renders no grid"
fi

if ((failures > 0)); then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "all benchmark script checks passed"
