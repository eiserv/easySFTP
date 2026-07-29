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
# line_count: "wc -l" pads its output on the BSDs, and an expectation of "10"
# must not fail against "      10" just because the check ran on a Mac.
line_count() { echo $(($(wc -l <"$1"))); }

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
  files=$(find "$source_dir" -type f | wc -l | tr -d ' ')
  # "wc -c" rather than "find -printf '%s'": the latter is GNU-only, and this
  # self-check should be runnable on a maintainer's machine as well as in CI.
  bytes=$(find "$source_dir" -type f -exec wc -c {} + | awk '$2 != "total" { s += $1 } END { print s + 0 }')
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

# The link probe stub. cmd/linkprobe writes one JSON document to stdout and the
# scripts only wrap it, so a stub document is enough to check the wrapping, the
# CSV columns and the summary section without a network.
probe_stub=$work/linkprobe-stub
cat >"$probe_stub" <<'PROBE'
#!/usr/bin/env bash
set -euo pipefail
for name in LINKPROBE_HOST LINKPROBE_USERNAME LINKPROBE_PASSWORD LINKPROBE_KNOWN_HOSTS; do
  if [[ -z ${!name:-} ]]; then
    echo "::error::$name is required but empty" >&2
    exit 1
  fi
done
cat <<JSON
{
  "schema_version": 1,
  "note": "stub",
  "measured_at": "2026-07-30T12:00:00Z",
  "handshake_ms": 41.2,
  "rtt_ms": {"p50": 18.4, "p90": 21, "min": 17.1, "max": 44.2, "samples": 21},
  "control": {"streams": 4, "bytes": 8388608,
              "single_stream_mib_per_s": 0.41, "n_stream_mib_per_s": 1.6, "note": "stub"},
  "host_load": {"available": true, "method": "sftp:/proc/loadavg",
                "load1": 0.9, "load5": 1.1, "load15": 1.0},
  "errors": []
}
JSON
PROBE
chmod +x "$probe_stub"

export CANDIDATE_BIN="$stub" CANDIDATE_REF="candidate-ref"
export BASELINE_BIN="$stub" BASELINE_REF="baseline-ref"
export REMOTE_BASE=/easysftp-benchmark-test
export BENCH_HOST=example.invalid BENCH_PORT=22 BENCH_USERNAME=tester
export BENCH_PASSWORD=secret BENCH_KNOWN_HOSTS="example.invalid ssh-ed25519 AAAA"
export DATASET_DIR="$work/data"

# Shaping must never actually happen here. This check may run as root on a
# maintainer's box or on a self-hosted runner, and a netem qdisc left on a real
# interface by a test is a broken machine, not a failed assertion. A name no
# interface has plus a refused sudo makes every "tc" call fail, which is exactly
# the degradation path the checks below assert on.
export LINK_IFACE=easysftp-selfcheck0 LINK_SUDO=0

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
  "$(line_count "$OUT_DIR/results.csv")"
expect_equal 'the CSV names its columns' '"scenario","build"' \
  "$(head -1 "$OUT_DIR/results.csv" | cut -d, -f1,2)"

for needle in '## easySFTP benchmark' '### Throughput' '### Resources' '### Where the time goes'; do
  if grep -qF "$needle" "$OUT_DIR/summary.md"; then
    pass "summary.md has '$needle'"
  else
    fail "summary.md is missing '$needle'"
  fi
done
# The file count in the fourth column is what distinguishes a throughput row
# from the resources table's row for the same scenario, build and link profile.
expect_equal 'summary.md has a throughput row for every build and scenario' 9 \
  "$(grep -cE '^\| (small|mixed|large) \| (candidate|baseline|pool2) \| baseline \| [0-9]+ \|' "$OUT_DIR/summary.md")"
expect_equal 'summary.md has a resources row for every build and scenario' 9 \
  "$(grep -cE '^\| (small|mixed|large) \| (candidate|baseline|pool2) \| baseline \| [0-9.]+ ms \|' "$OUT_DIR/summary.md")"

# Without LINKPROBE_BIN the result still has to carry a valid link object, with
# an empty probe list rather than an invented entry.
expect_equal 'a run without a probe binary reports no probes' 0 \
  "$(jq '.link.probes | length' "$results")"
expect_equal 'the implicit profile is named' baseline \
  "$(jq -r '[.results[].link_profile] | unique | join(" ")' "$results")"
expect_equal 'an unprobed run says shaping was never asked for' false \
  "$(jq -r '.link.shaping.available' "$results")"
if grep -qF 'No link probe ran for this result' "$OUT_DIR/summary.md"; then
  pass "summary.md says when the link was not probed"
else
  fail "summary.md hides that the link was not probed"
fi

echo
echo "== scripts/benchmark.sh over link profiles =="
# A second, smaller run: two profiles, a probe binary, and shaping that cannot
# be applied. That last part is the common case on a runner without NET_ADMIN,
# and it must produce a complete result rather than a failed run.
export OUT_DIR="$work/link-out" LOG_DIR="$work/link-logs" REPEATS=1
export BENCH_LINK_PROFILES="baseline +50ms/5mbit" LINKPROBE_BIN="$probe_stub"
unset BENCH_CONNECTIONS
bash "$repo_root/scripts/benchmark.sh" >"$work/link.stdout" 2>&1 ||
  fail "benchmark.sh over link profiles exited non-zero (see $work/link.stdout)"

link_results="$OUT_DIR/results.json"
if [[ ! -f $link_results ]]; then
  echo "FAIL: the link profile run produced no results.json" >&2
  cat "$work/link.stdout" >&2
  exit 1
fi

# 2 builds x 3 scenarios x 2 profiles.
expect_equal 'every build, scenario and profile was aggregated' 12 \
  "$(jq '.results | length' "$link_results")"
expect_equal 'both profiles appear in the results' '+50ms/5mbit baseline' \
  "$(jq -r '[.results[].link_profile] | unique | sort | join(" ")' "$link_results")"
# Two probes per profile, before and after that profile's own runs: a start and
# an end of the same profile are what makes drift visible.
expect_equal 'each profile is probed at its start and its end' 4 \
  "$(jq '.link.probes | length' "$link_results")"
expect_equal 'the probes say which profile and when' 'baseline/start baseline/end +50ms/5mbit/start +50ms/5mbit/end' \
  "$(jq -r '[.link.probes[] | "\(.profile)/\(.at)"] | join(" ")' "$link_results")"
expect_equal 'a probe document is kept whole' 18.4 \
  "$(jq -r '.link.probes[0].rtt_ms.p50' "$link_results")"
# The point of the whole exercise: shaping that could not be applied is recorded
# as such, and the profile names then say what was asked for, not what happened.
expect_equal 'unavailable shaping is recorded, not fatal' false \
  "$(jq -r '.link.shaping.available' "$link_results")"
expect_nonempty 'unavailable shaping carries a reason' \
  "$(jq -r '.link.shaping.reason' "$link_results")"
expect_equal 'the requested profiles are recorded even when unshaped' '+50ms/5mbit baseline' \
  "$(jq -r '.link.shaping.requested | sort | join(" ")' "$link_results")"
expect_equal 'only the unshaped profile counts as applied' baseline \
  "$(jq -r '.link.shaping.applied | join(" ")' "$link_results")"
# A comparison across profiles would be meaningless, so every entry must pair
# two cells of the same one.
expect_equal 'the comparison stays inside one profile' 6 \
  "$(jq '[.comparison[] | select(.link_profile != null)] | length' "$link_results")"
expect_equal 'the CSV carries the link a row was measured over' \
  '"link_profile","rtt_p50_ms","control_single_mib_per_s"' \
  "$(head -1 "$OUT_DIR/results.csv" | cut -d, -f4-6)"
expect_equal 'the CSV fills those columns from the profile start probe' 12 \
  "$(grep -c ',18.4,0.41,' "$OUT_DIR/results.csv")"
if grep -qF '### The link' "$OUT_DIR/summary.md"; then
  pass "summary.md has a link section"
else
  fail "summary.md is missing its link section"
fi

echo
echo "== scripts/benchmark-matrix.sh =="
unset BENCH_LINK_PROFILES LINKPROBE_BIN
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
  "$(line_count "$OUT_DIR/matrix.csv")"
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
expect_equal 'the matrix declares its link axis too' baseline \
  "$(jq -r '.axes.link_profiles | join(" ")' "$matrix")"
expect_equal 'a sweep without a probe binary reports no probes' 0 \
  "$(jq '.link.probes | length' "$matrix")"

echo
echo "== scripts/benchmark-matrix.sh over link profiles =="
# A one-cell grid: what is under test here is the profile axis multiplying the
# grid and the per-profile grouping, not the grid itself.
export OUT_DIR="$work/matrix-link-out" LOG_DIR="$work/matrix-link-logs"
export MATRIX_CONNECTIONS="1" MATRIX_CONCURRENCY="1" MATRIX_SCENARIOS="small"
export MATRIX_LINK_PROFILES="baseline +150ms" LINKPROBE_BIN="$probe_stub"
bash "$repo_root/scripts/benchmark-matrix.sh" >"$work/matrix-link.stdout" 2>&1 ||
  fail "benchmark-matrix.sh over link profiles exited non-zero (see $work/matrix-link.stdout)"

matrix_link="$OUT_DIR/matrix.json"
if [[ ! -f $matrix_link ]]; then
  echo "FAIL: the matrix link profile run produced no matrix.json" >&2
  cat "$work/matrix-link.stdout" >&2
  exit 1
fi

# 1 cell x 2 builds x 2 profiles: the profile axis multiplies the grid, which is
# the cost the script warns about before it starts.
expect_equal 'the profile axis multiplies the grid' 4 \
  "$(jq '.cells | length' "$matrix_link")"
expect_equal 'the link axis is declared in order' 'baseline +150ms' \
  "$(jq -r '.axes.link_profiles | join(" ")' "$matrix_link")"
expect_equal 'the scaling view is grouped per profile as well' 4 \
  "$(jq '.scaling | length' "$matrix_link")"
expect_equal 'the comparison pairs cells of the same profile' 2 \
  "$(jq '[.comparison[] | select(.link_profile != null)] | length' "$matrix_link")"
expect_equal 'each profile got its own pair of probes' 4 \
  "$(jq '.link.probes | length' "$matrix_link")"
expect_equal 'the matrix CSV carries the link columns' \
  '"link_profile","rtt_p50_ms","control_single_mib_per_s"' \
  "$(head -1 "$OUT_DIR/matrix.csv" | cut -d, -f4-6)"
# cells[] has always carried net_write_bytes; benchmarks/README.md calls the CSV
# the same cells flattened, so it belongs in there (issue #184, phase 2).
expect_equal 'the matrix CSV carries net_write_bytes' 1 \
  "$(head -1 "$OUT_DIR/matrix.csv" | grep -c 'net_write_bytes')"
expect_equal 'the matrix renders one grid per build and profile' 4 \
  "$(grep -c '^#### ' "$OUT_DIR/matrix.md")"

if ((failures > 0)); then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "all benchmark script checks passed"
