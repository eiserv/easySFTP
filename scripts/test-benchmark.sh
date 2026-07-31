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

# parity <label> <prefix>: the same measurement aggregated twice, once by
# cmd/easysftp-bench and once by the jq implementation it replaced (issue #190).
#
# scripts/benchmark-aggregate-jq.sh reads the same manifest the Go command
# reads, so both get identical inputs by construction and a difference can only
# come from the aggregation itself. That is the check that makes "a behavioural
# rewrite" something other than a claim, and it runs against the live scripts
# rather than against a fixture captured once.
#
# The JSON is compared as values, because object key order is not data. The CSV
# and the Markdown are compared byte for byte, because there the layout *is* the
# document.
parity() {
  local label=$1 prefix=$2
  local oracle="$work/$label-oracle"
  local manifest="$LOG_DIR/manifest.json"
  local summary=summary.md
  if [[ "$prefix" == matrix ]]; then
    summary=matrix.md
  fi

  if [[ ! -f "$manifest" ]]; then
    fail "$label: the run wrote no manifest"
    return
  fi
  mkdir -p "$oracle"
  if ! MANIFEST="$manifest" OUT_DIR="$oracle" bash "$repo_root/scripts/benchmark-aggregate-jq.sh" \
    >"$work/$label-oracle.log" 2>&1; then
    fail "$label: the jq oracle exited non-zero (see $work/$label-oracle.log)"
    return
  fi

  if diff <(jq -S . "$OUT_DIR/$prefix.json") <(jq -S . "$oracle/$prefix.json") >"$work/$label-json.diff"; then
    pass "$label: Go and jq produce the same $prefix.json"
  else
    fail "$label: Go and jq disagree on $prefix.json (see $work/$label-json.diff)"
  fi
  if diff "$OUT_DIR/$prefix.csv" "$oracle/$prefix.csv" >"$work/$label-csv.diff"; then
    pass "$label: Go and jq produce the same $prefix.csv"
  else
    fail "$label: Go and jq disagree on $prefix.csv (see $work/$label-csv.diff)"
  fi
  if diff "$OUT_DIR/$summary" "$oracle/$summary" >"$work/$label-md.diff"; then
    pass "$label: Go and jq produce the same $summary"
  else
    fail "$label: Go and jq disagree on $summary (see $work/$label-md.diff)"
  fi
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
target=${EASYSFTP_TARGET:-}
mode=${EASYSFTP_MODE:-}
connections=1
concurrency=4
request_concurrency=16
skip_unchanged=false
if [[ -n "${EASYSFTP_CONFIG:-}" ]]; then
  source_dir=$(awk -F'"' '/source:/ { print $2; exit }' "$EASYSFTP_CONFIG")
  target=$(awk -F'"' '/target:/ { print $2; exit }' "$EASYSFTP_CONFIG")
  mode=$(awk '/^    mode:/ { print $2; exit }' "$EASYSFTP_CONFIG")
  connections=$(awk '/^  connections:/ { print $2; exit }' "$EASYSFTP_CONFIG")
  concurrency=$(awk '/^  concurrency:/ { print $2; exit }' "$EASYSFTP_CONFIG")
  request_concurrency=$(awk '/^  request_concurrency:/ { print $2; exit }' "$EASYSFTP_CONFIG")
  # "auto" resolves to easySFTP's own defaults here, exactly as autoInt.or does
  # in internal/config/configfile.go: an auto run has to come out somewhere on
  # the grid, otherwise the regret arithmetic has nothing to compare.
  connections=${connections:-1}
  concurrency=${concurrency:-4}
  request_concurrency=${request_concurrency:-16}
  case $connections in auto) connections=1 ;; esac
  case $concurrency in auto) concurrency=4 ;; esac
  case $request_concurrency in auto) request_concurrency=16 ;; esac
  if grep -q '^  skip_unchanged: true' "$EASYSFTP_CONFIG"; then
    skip_unchanged=true
  fi
fi

# Into the run log, which is what the checks on the deploy shapes read: the mode
# and skip_unchanged are chosen per scenario, and nothing else in the output
# would show which ones a cell actually ran with.
echo "stub: mode=${mode:-none} skip_unchanged=$skip_unchanged source=$source_dir"

files=0
bytes=0
if [[ -d "$source_dir" ]]; then
  files=$(find "$source_dir" -type f | wc -l | tr -d ' ')
  # "wc -c" rather than "find -printf '%s'": the latter is GNU-only, and this
  # self-check should be runnable on a maintainer's machine as well as in CI.
  bytes=$(find "$source_dir" -type f -exec wc -c {} + | awk '$2 != "total" { s += $1 } END { print s + 0 }')
fi

# Just enough remote state to make a "clean" run delete what an earlier run put
# there: one file per remote target holding its file count. Without it every
# pre-clean would report zero deletions, and the delete aggregation (issue #184,
# phase 4) would have nothing to aggregate.
deleted=0
if [[ -n "${EASYSFTP_STUB_STATE:-}" && -n "$target" ]]; then
  mkdir -p "$EASYSFTP_STUB_STATE"
  state="$EASYSFTP_STUB_STATE/${target//\//_}"
  if [[ "$mode" == clean ]]; then
    deleted=$(cat "$state" 2>/dev/null || echo 0)
    rm -f "$state"
  else
    echo "$files" >"$state"
  fi
fi

# More connections and more workers finish sooner, with a floor: exactly the
# shape a scaling curve should have, so the matrix output can be checked
# against a known answer.
parallel=$((connections < concurrency ? connections : concurrency))
duration=$((200 + (files + deleted) * 4 / parallel + RANDOM % 7))

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "files-uploaded<<EOF"; echo "$files"; echo "EOF"
    echo "bytes-uploaded<<EOF"; echo "$bytes"; echo "EOF"
    echo "files-deleted<<EOF"; echo "$deleted"; echo "EOF"
    echo "duration-ms<<EOF"; echo "$duration"; echo "EOF"
  } >>"$GITHUB_OUTPUT"
fi

# A clean deployment runs different phases and different round-trips than an
# upload, and the delete aggregation reads exactly those.
if [[ "$mode" == clean ]]; then
  phases='[{"name": "remote_scan", "wall_ms": '$((duration / 3))', "count": 1},
    {"name": "delete_sweep", "wall_ms": '$((duration * 2 / 3))', "count": 1},
    {"name": "connect", "wall_ms": 40, "count": 1}]'
  operations='[{"name": "sftp_remove", "count": '"$deleted"', "errors": 0, "total_ms": '$((duration / 2))',
     "avg_ms": 2.1, "min_ms": 1, "p50_ms": 2, "p90_ms": 4, "p99_ms": 7, "max_ms": 8},
    {"name": "sftp_rmdir", "count": 8, "errors": 0, "total_ms": 24,
     "avg_ms": 3, "min_ms": 2, "p50_ms": 3, "p90_ms": 4, "p99_ms": 5, "max_ms": 5}]'
else
  phases='[{"name": "upload", "wall_ms": '$((duration - 60))', "count": 1},
    {"name": "connect", "wall_ms": 40, "count": 1},
    {"name": "local_scan", "wall_ms": 15, "count": 1},
    {"name": "create_dirs", "wall_ms": 5, "count": 1}]'
  operations='[{"name": "file_upload", "count": '"$files"', "errors": 0, "total_ms": '$((duration * 2))',
     "avg_ms": 3.5, "min_ms": 1, "p50_ms": 3, "p90_ms": 6, "p99_ms": 9, "max_ms": 11},
    {"name": "sftp_open", "count": '"$files"', "errors": 0, "total_ms": '"$duration"',
     "avg_ms": 1.7, "min_ms": 1, "p50_ms": 1.5, "p90_ms": 3, "p99_ms": 5, "max_ms": 6}]'
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
  "phases": $phases,
  "operations": $operations,
  "counters": {
    "connections_opened": $connections, "connections_used": $connections,
    "connections_refused": 0, "reconnects": 0, "retries": 0, "stalls": 0, "errors": 0,
    "config_connections": $connections, "config_concurrency": $concurrency,
    "config_request_concurrency": $request_concurrency,
    "files_uploaded": $files, "bytes_uploaded": $bytes, "files_deleted": $deleted
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
# The stub's stand-in for the remote server, so a pre-clean has something to
# delete; see the stub above.
export EASYSFTP_STUB_STATE="$work/remote-state"

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
parity standard results

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
  "$(jq '[.results[] | select(.label == "candidate") | .mad_ms | select(. != null)] | length' "$results")"

# The pre-clean is a delete sweep and is now stored as one (issue #184, phase 4).
# Three repeats mean two sweeps that found something: the first pre-clean of a
# build and scenario runs against an empty directory and is dropped.
expect_equal 'every build and scenario has a delete row' 9 "$(jq '.deletes | length' "$results")"
expect_equal 'empty sweeps are not counted' 2 \
  "$(jq -r '[.deletes[] | select(.label == "candidate" and .scenario == "small") | .sweeps] | first' "$results")"
expect_equal 'a delete row says how much it deleted' 300 \
  "$(jq -r '[.deletes[] | select(.label == "candidate" and .scenario == "small") | .files_deleted] | first' "$results")"
expect_equal 'the delete sweep keeps its own phases' 'connect delete_sweep remote_scan' \
  "$(jq -r '[.deletes[0].phases[].name] | sort | join(" ")' "$results")"
expect_equal 'the delete sweep keeps the round-trips issue #157 is about' 'sftp_remove sftp_rmdir' \
  "$(jq -r '[.deletes[0].operations[].name] | sort | join(" ")' "$results")"
expect_nonempty 'sftp_remove carries percentiles' \
  "$(jq -r '[.deletes[0].operations[] | select(.name == "sftp_remove") | .p90_ms] | first' "$results")"
# The whole point of a separate block: an upload aggregate must not have grown a
# delete phase, and the delete numbers must not have moved an upload median.
expect_equal 'no upload result picked up a delete phase' 0 \
  "$(jq '[.results[].phases[] | select(.name == "delete_sweep")] | length' "$results")"
expect_equal 'no upload result picked up a delete round-trip' 0 \
  "$(jq '[.results[].operations[] | select(.name == "sftp_remove")] | length' "$results")"

expect_equal 'the CSV has a header plus one row per build and scenario' 10 \
  "$(line_count "$OUT_DIR/results.csv")"
expect_equal 'the CSV names its columns' '"scenario","build"' \
  "$(head -1 "$OUT_DIR/results.csv" | cut -d, -f1,2)"

for needle in '## easySFTP benchmark' '### Throughput' '### Resources' '### Where the time goes' '### Delete sweeps'; do
  if grep -qF "$needle" "$OUT_DIR/summary.md"; then
    pass "summary.md has '$needle'"
  else
    fail "summary.md is missing '$needle'"
  fi
done
# The file count and the payload size in columns four and five are what
# distinguish a throughput row from the resources and delete-sweep tables, which
# start with the same scenario, build and link profile.
expect_equal 'summary.md has a throughput row for every build and scenario' 9 \
  "$(grep -cE '^\| (small|mixed|large) \| (candidate|baseline|pool2) \| baseline \| [0-9]+ \| [0-9.]+ MiB \|' "$OUT_DIR/summary.md")"
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
parity standard-link results

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
parity matrix matrix

matrix="$OUT_DIR/matrix.json"
if [[ ! -f $matrix ]]; then
  echo "FAIL: benchmark-matrix.sh produced no matrix.json" >&2
  cat "$work/matrix.stdout" >&2
  exit 1
fi

expect_equal 'matrix.json is schema_version 2' 2 "$(jq -r .schema_version "$matrix")"
expect_equal 'matrix.json says what kind it is' matrix "$(jq -r .benchmark_kind "$matrix")"
# The grid is per scenario (issue #184, phase 5): "small" (300 files) is swept
# over 2 connections x 2 concurrency at easySFTP's own request_concurrency, and
# "single" (one 32 MiB file) collapses to a single connections/concurrency cell
# but is the one scenario the request axis applies to. 2 builds each: 8 + 6.
expect_equal 'every cell of the grid was measured' 14 "$(jq '.cells | length' "$matrix")"
expect_equal 'a one-file scenario is not measured at four identical coordinates' 3 \
  "$(jq '[.cells[] | select(.scenario == "single" and .label == "candidate")] | length' "$matrix")"
expect_equal 'the request axis is swept where a file is large enough for it' '1 16 64' \
  "$(jq -r '[.cells[] | select(.scenario == "single") | .request_concurrency] | unique | sort | join(" ")' "$matrix")"
expect_equal 'and left at easySFTP its own value where it cannot matter' 'null' \
  "$(jq -r '[.cells[] | select(.scenario == "small") | .request_concurrency] | unique | map(tostring) | join(" ")' "$matrix")"
expect_equal 'a cell records the request_concurrency it actually ran with' 16 \
  "$(jq -r '[.cells[] | select(.scenario == "small") | .request_concurrency_used] | first' "$matrix")"
expect_equal 'the per-scenario axes are declared next to the requested ones' '1 2/1 4/null' \
  "$(jq -r '.axes.per_scenario.small | "\(.connections | join(" "))/\(.concurrency | join(" "))/\(.request_concurrency | map(tostring) | join(" "))"' "$matrix")"
expect_equal 'a scenario declares the file count its axes were capped against' 1 \
  "$(jq -r '.axes.per_scenario.single.files' "$matrix")"
expect_equal 'connections > concurrency is measured, not rejected' 1 \
  "$(jq '[.cells[] | select(.connections == 2 and .concurrency == 1 and .label == "candidate" and .scenario == "small")] | length' "$matrix")"
expect_nonempty 'cells carry the used connection count' \
  "$(jq -r '[.cells[] | .connections_used] | first' "$matrix")"
expect_equal 'cells carry throughput and file rate' 14 \
  "$(jq '[.cells[] | select(.mib_per_s != null and .files_per_s != null)] | length' "$matrix")"
expect_equal 'cells carry CPU and RSS' 14 \
  "$(jq '[.cells[] | select(.max_rss_bytes != null and .user_cpu_ms != null)] | length' "$matrix")"
expect_equal 'the scaling view is grouped per scenario and build' 4 \
  "$(jq '.scaling | length' "$matrix")"

# A matrix run has no runs[], so a cell is the finest grain there is: the phase
# and round-trip detail has to survive into it (issue #184, phase 2).
expect_equal 'every cell keeps its phases' 14 \
  "$(jq '[.cells[] | select((.phases | length) > 0)] | length' "$matrix")"
expect_equal 'a cell keeps the phases that are not the upload' 'connect create_dirs local_scan upload' \
  "$(jq -r '[.cells[0].phases[].name] | sort | join(" ")' "$matrix")"
expect_equal 'every cell keeps its round-trip percentiles' 14 \
  "$(jq '[.cells[] | select(([.operations[] | select(.name == "sftp_open") | .p90_ms] | first) != null)] | length' "$matrix")"
expect_nonempty 'upload_phase_ms is still there for anything reading it' \
  "$(jq -r '.cells[0].upload_phase_ms' "$matrix")"

# A single repeat has no measured spread; 0 there would read as precision.
expect_equal 'a one-repeat cell reports no MAD' 14 \
  "$(jq '[.cells[] | select(.mad_ms == null)] | length' "$matrix")"

# The policy measurement of issue #184 phase 5. The stub resolves "auto" to
# easySFTP's own defaults (1/4/16), and gets faster with more parallelism, so
# the best cell of "small" is 2/4 and auto must come out behind it.
expect_equal 'auto is measured once per scenario and profile' 2 \
  "$(jq '.auto | length' "$matrix")"
expect_equal 'auto is not a cell' 0 \
  "$(jq '[.cells[] | select(.label == "auto")] | length' "$matrix")"
expect_equal 'auto stays out of the scaling view and the comparison' 0 \
  "$(jq '[(.scaling[], .comparison[]) | select(.label == "auto")] | length' "$matrix")"
expect_equal 'auto reports the settings it picked, read from its own counters' '1/4/16' \
  "$(jq -r '[.auto[] | select(.scenario == "small") | "\(.chosen.connections)/\(.chosen.concurrency)/\(.chosen.request_concurrency)"] | first' "$matrix")"
expect_equal 'auto is scored against the best cell of its scenario' '2/4' \
  "$(jq -r '[.auto[] | select(.scenario == "small") | "\(.best.connections)/\(.best.concurrency)"] | first' "$matrix")"
regret=$(jq -r '[.auto[] | select(.scenario == "small") | .regret_percent] | first' "$matrix")
if [[ -n $regret ]] && awk -v r="$regret" 'BEGIN { exit !(r > 0) }'; then
  pass "a policy that picks a slower cell reports a positive regret ($regret%)"
else
  fail "auto picked the slower cell, so the regret should be positive, got $regret%"
fi
# The control column: the same coordinates measured as an ordinary cell. For
# "single" the picked concurrency is not on the grid at all, and that has to be
# said rather than silently paired with something else.
expect_equal 'the picked cell is cross-checked against the grid' true \
  "$(jq -r '[.auto[] | select(.scenario == "small") | .chosen_in_grid] | first' "$matrix")"
expect_equal 'a pick outside the grid is reported as such' 'false null' \
  "$(jq -r '[.auto[] | select(.scenario == "single") | "\(.chosen_in_grid) \(.chosen_cell_median_ms)"] | first' "$matrix")"

# An optimum on the largest swept value of an axis was cut off, not measured.
# "small" is fastest at 2/4 here, the largest value of both of its axes.
expect_equal 'a best cell on the edge of an axis is flagged' 'connections concurrency' \
  "$(jq -r '[.scaling[] | select(.scenario == "small" and .label == "candidate") | .best_at_axis_max[]] | join(" ")' "$matrix")"
if grep -qF '**The optimum sits on the edge of the grid**' "$OUT_DIR/matrix.md"; then
  pass "matrix.md says when the optimum was cut off"
else
  fail "matrix.md hides that the optimum sits on an axis edge"
fi
if grep -qF 'costs (policy regret)' "$OUT_DIR/matrix.md"; then
  pass "matrix.md reports the policy regret"
else
  fail "matrix.md is missing its policy regret section"
fi

# Start, middle and end of the grid: 2 connections x 2 concurrency x 2 scenarios
# x 2 builds is 16 cells, so the middle canary has a middle to sit in.
expect_equal 'the canary is measured three times' 3 \
  "$(jq '.canary | length' "$matrix")"
expect_equal 'the canary says when each run happened' 'start mid end' \
  "$(jq -r '[.canary[].at] | join(" ")' "$matrix")"
expect_equal 'the canary is one fixed cell' 'small/1/4' \
  "$(jq -r '[.canary[] | "\(.scenario)/\(.connections)/\(.concurrency)"] | unique | join(" ")' "$matrix")"
# One delete row per cell, minus the first cell of each (build, scenario): its
# pre-clean has an empty directory in front of it (issue #184, phase 4).
expect_equal 'every cell but the first of its build and scenario has a delete row' 10 \
  "$(jq '.deletes | length' "$matrix")"
# The auto runs deploy into their own remote directory and their pre-clean is
# left uninstrumented: their coordinates are not a coordinate of the grid, so a
# delete row of them would sit under settings nobody swept.
expect_equal 'the auto runs contribute no delete rows' 0 \
  "$(jq '[.deletes[] | select(.label == "auto")] | length' "$matrix")"
expect_equal 'a delete row carries the cell coordinates it was measured at' true \
  "$(jq '.deletes[0] | has("connections") and has("concurrency") and has("request_concurrency")' "$matrix")"
expect_equal 'a matrix delete row keeps its sweep phase' 10 \
  "$(jq '[.deletes[] | select([.phases[] | select(.name == "delete_sweep")] | length == 1)] | length' "$matrix")"
expect_nonempty 'a matrix delete row keeps the sftp_remove percentiles' \
  "$(jq -r '[.deletes[0].operations[] | select(.name == "sftp_remove") | .p50_ms] | first' "$matrix")"
expect_equal 'no cell picked up a delete phase' 0 \
  "$(jq '[.cells[].phases[] | select(.name == "delete_sweep")] | length' "$matrix")"
if grep -qF '### Delete sweeps' "$OUT_DIR/matrix.md"; then
  pass "matrix.md reports the delete sweeps"
else
  fail "matrix.md is missing its delete sweep section"
fi
if grep -qF '### Canary' "$OUT_DIR/matrix.md"; then
  pass "matrix.md reports the canary"
else
  fail "matrix.md is missing its canary section"
fi

# The heatmap axes must be explicit: a plot should not have to infer the grid
# from the cells it happens to find.
expect_equal 'the axes are declared' '1 2' "$(jq -r '.axes.connections | join(" ")' "$matrix")"
expect_equal 'the concurrency axis is declared' '1 4' "$(jq -r '.axes.concurrency | join(" ")' "$matrix")"

expect_equal 'the matrix CSV has a header plus one row per cell' 15 \
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
parity matrix-link matrix

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
# The canary axis multiplies with the profiles: drift inside one profile is what
# it detects, and a canary of another profile is not a comparison for it.
expect_equal 'each profile gets its own canary triple' \
  'baseline/start baseline/mid baseline/end +150ms/start +150ms/mid +150ms/end' \
  "$(jq -r '[.canary[] | "\(.link_profile)/\(.at)"] | join(" ")' "$matrix_link")"
expect_equal 'the matrix CSV carries the link columns' \
  '"link_profile","rtt_p50_ms","control_single_mib_per_s"' \
  "$(head -1 "$OUT_DIR/matrix.csv" | cut -d, -f4-6)"
# cells[] has always carried net_write_bytes; benchmarks/README.md calls the CSV
# the same cells flattened, so it belongs in there (issue #184, phase 2).
expect_equal 'the matrix CSV carries net_write_bytes' 1 \
  "$(head -1 "$OUT_DIR/matrix.csv" | grep -c 'net_write_bytes')"
expect_equal 'the matrix renders one grid per build and profile' 4 \
  "$(grep -c '^#### ' "$OUT_DIR/matrix.md")"

echo
echo "== scenario shapes (issue #184, phase 3) =="
# The payload side, checked directly against the library: the deep layout, the
# calibration grammar and the mutation are all logic a stub run would only
# exercise by accident.
# shellcheck source=scripts/benchmark-lib.sh disable=SC1091
source "$repo_root/scripts/benchmark-lib.sh"

expect_equal 'the calibration grammar parses a KiB size' '10:64' "$(scenario_spec calib-10x64k)"
expect_equal 'the calibration grammar parses a MiB size' '1000:16384' "$(scenario_spec calib-1000x16m)"
if scenario_spec calib-nonsense >/dev/null 2>&1; then
  fail "a malformed calibration scenario is accepted"
else
  pass "a malformed calibration scenario is rejected"
fi
# The per-scenario grid of issue #184 phase 5. Both rules are properties of the
# payload: an axis value above the file count is the same configuration twice,
# and request_concurrency is per file, so small files cannot use it at all.
expect_equal 'the file count is summed over the payload groups' 56 "$(scenario_files mixed)"
expect_equal 'the largest file of a payload decides the request axis' 2048 "$(scenario_max_kib mixed)"
expect_equal 'the request axis applies where a file is large enough' 1 "$(scenario_sweeps_requests single)"
expect_equal 'and not to a payload of 4 KiB files' 0 "$(scenario_sweeps_requests small)"
expect_equal 'an axis is capped at the file count and deduplicated' '1' \
  "$(axis_for_scenario single 1 2 4 8 | tr '\n' ' ' | sed 's/ $//')"
expect_equal 'a payload that can use the whole axis keeps it' '1 2 4 8' \
  "$(axis_for_scenario small 1 2 4 8 | tr '\n' ' ' | sed 's/ $//')"
expect_equal 'a partial cap keeps the values below it' '1 2' \
  "$(axis_for_scenario large 1 2 4 8 | tr '\n' ' ' | sed 's/ $//')"

expect_equal 'a redeploy scenario declares its shape' 'overlay 1 flat' "$(scenario_shape redeploy)"
expect_equal 'the sync scenario is measured in mode sync' 'sync 1 flat' "$(scenario_shape sync)"
expect_equal 'the scenarios that predate this keep the old shape' 'overlay 0 flat' "$(scenario_shape small)"

shapes="$work/shapes"
(DATASET_DIR="$shapes" && generate_dataset deep calib-10x64k >/dev/null)
expect_equal 'the deep payload has the file count of its spec' 400 \
  "$(find "$shapes/deep" -type f | wc -l | tr -d ' ')"
# 7 levels of two directories each: many directories holding a handful of files,
# which is what separates create_dirs cost from transfer cost.
expect_equal 'the deep payload nests 7 levels' 128 \
  "$(find "$shapes/deep" -mindepth 7 -maxdepth 7 -type d | wc -l | tr -d ' ')"
expect_equal 'nothing sits deeper than that' 0 \
  "$(find "$shapes/deep" -mindepth 8 -type d | wc -l | tr -d ' ')"
expect_equal 'a calibration payload is uniform' '10 65536' \
  "$(find "$shapes/calib-10x64k" -type f -exec wc -c {} + | awk '$2 != "total" { n += 1; s[$1] = 1 } END { for (k in s) u = k; print n, u }')"

scenario_mutate "$shapes/calib-10x64k" "$SCENARIO_CHANGED_FILES"
expect_equal 'the mutation changes exactly the files it says' 3 \
  "$(find "$shapes/calib-10x64k" -type f -exec wc -c {} + | awk '$2 != "total" && $1 == 66048 { n += 1 } END { print n + 0 }')"

echo
echo "== scripts/benchmark-matrix.sh over the deploy shapes =="
# One cell, one build, three scenarios: what is under test is that a scenario
# carries a mode and a base deploy, not the grid.
export OUT_DIR="$work/shape-out" LOG_DIR="$work/shape-logs" REPEATS=1
export MATRIX_CONNECTIONS="1" MATRIX_CONCURRENCY="2"
export MATRIX_SCENARIOS="redeploy sync calib-10x64k"
unset MATRIX_LINK_PROFILES LINKPROBE_BIN BASELINE_BIN BASELINE_REF
bash "$repo_root/scripts/benchmark-matrix.sh" >"$work/matrix-shapes.stdout" 2>&1 ||
  fail "benchmark-matrix.sh over the deploy shapes exited non-zero (see $work/matrix-shapes.stdout)"
parity matrix-shapes matrix

shape_matrix="$OUT_DIR/matrix.json"
if [[ ! -f $shape_matrix ]]; then
  echo "FAIL: the deploy shape run produced no matrix.json" >&2
  cat "$work/matrix-shapes.stdout" >&2
  exit 1
fi

expect_equal 'every deploy shape produced a cell' 'calib-10x64k redeploy sync' \
  "$(jq -r '[.cells[].scenario] | unique | join(" ")' "$shape_matrix")"
expect_equal 'the sync scenario was measured in mode sync' 1 \
  "$(grep -lc 'stub: mode=sync' "$LOG_DIR"/baseline-candidate-sync-*[0-9].log | wc -l | tr -d ' ')"
expect_equal 'the redeploy scenario was measured with skip_unchanged' 1 \
  "$(grep -lc 'stub: mode=overlay skip_unchanged=true' "$LOG_DIR"/baseline-candidate-redeploy-*[0-9].log | wc -l | tr -d ' ')"
# The unmeasured deploy the measured run redeploys over. Its metrics must not
# exist: a base deploy counted as a measurement would report the fresh upload
# this scenario exists to *not* measure.
# Two cells plus the two auto runs of the same scenarios: the policy run is a
# deployment of that scenario like any other, so it gets the base deploy too.
expect_equal 'a redeploy cell laid down an unmeasured base deploy first' 4 \
  "$(find "$LOG_DIR" -name '*.base.log' | wc -l | tr -d ' ')"
expect_equal 'a scenario without a base deploy runs one deploy only' 0 \
  "$(find "$LOG_DIR" -name '*calib-10x64k*.base.log' | wc -l | tr -d ' ')"
expect_equal 'the calibration scenario is documented by its spec' \
  '10 files x 64 KiB, uniform (calibration)' \
  "$(jq -r '.scenarios["calib-10x64k"]' "$shape_matrix")"
# The backticks around the scenario name are matched as "." so this pattern
# stays a single-quoted string shellcheck does not read as an expansion.
if grep -qE '^\| .sync. \| sync, redeployed \|' "$OUT_DIR/matrix.md"; then
  pass "matrix.md says which mode a scenario was measured in"
else
  fail "matrix.md does not say which mode a scenario was measured in"
fi

echo
echo "== the request_concurrency axis turned off (issue #184, phase 5) =="
# The axis has a real default now, so the two-dimensional grid needs a way to be
# asked for. The token "default" is it: one pass that sets nothing.
export OUT_DIR="$work/req-out" LOG_DIR="$work/req-logs" REPEATS=1
export MATRIX_CONNECTIONS="1" MATRIX_CONCURRENCY="1" MATRIX_SCENARIOS="single"
export MATRIX_REQUEST_CONCURRENCY="default"
bash "$repo_root/scripts/benchmark-matrix.sh" >"$work/matrix-req.stdout" 2>&1 ||
  fail "benchmark-matrix.sh with the request axis off exited non-zero (see $work/matrix-req.stdout)"
parity matrix-req matrix

req_matrix="$OUT_DIR/matrix.json"
if [[ ! -f $req_matrix ]]; then
  echo "FAIL: the request-axis run produced no matrix.json" >&2
  cat "$work/matrix-req.stdout" >&2
  exit 1
fi
expect_equal 'the default token keeps the grid two-dimensional' 1 \
  "$(jq '.cells | length' "$req_matrix")"
expect_equal 'a pass that sets nothing is stored as a null coordinate' 'null' \
  "$(jq -r '.cells[0].request_concurrency | tostring' "$req_matrix")"
expect_equal 'and the declared axis says so too' 'null' \
  "$(jq -r '.axes.request_concurrency | map(tostring) | join(" ")' "$req_matrix")"
expect_equal 'the run still records what easySFTP used' 16 \
  "$(jq -r '.cells[0].request_concurrency_used' "$req_matrix")"
# Everything the script reads is exported already, so the bad value is the only
# thing that changes here; it is rejected during validation, before any run.
if MATRIX_REQUEST_CONCURRENCY=fast bash "$repo_root/scripts/benchmark-matrix.sh" >/dev/null 2>&1; then
  fail "a nonsense request_concurrency axis value is accepted"
else
  pass "a nonsense request_concurrency axis value is rejected"
fi

if ((failures > 0)); then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "all benchmark script checks passed"
