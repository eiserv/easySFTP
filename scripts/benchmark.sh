#!/usr/bin/env bash
#
# easySFTP upload benchmark (issue #169).
#
# Measures one or two builds of easySFTP against a real SFTP server and writes
# results.json plus summary.md. It collects data only: nothing here fails on a
# slow number, because run-to-run variance against an external host is not
# understood yet.
#
# Beyond the total duration, every measured run is instrumented via
# EASYSFTP_METRICS_FILE (see internal/metrics): phase wall-clock times,
# per-round-trip operation samples, process CPU/RSS, Go allocation and GC
# counters and the run's connection bookkeeping. That is what turns "the small
# scenario is slow" into "the small scenario spends its time in sftp_open".
#
# Everything comes from the environment; .github/workflows/benchmark.yml builds
# the binaries and sets these:
#
#   CANDIDATE_BIN, CANDIDATE_REF  build under test and the ref it was built from
#   BASELINE_BIN,  BASELINE_REF   optional comparison build (may be empty)
#   BENCH_CONNECTIONS             optional: measure the candidate a second time
#                                 with advanced.connections set to this many SSH
#                                 connections (issue #158), as a third build
#                                 label "poolN"
#   BENCH_LINK_PROFILES           optional network profiles to measure over
#                                 (issue #184); empty means the real line only.
#                                 See scripts/benchmark-link.sh for the grammar
#   LINKPROBE_BIN                 optional built cmd/linkprobe; without it the
#                                 result carries an empty probe list
#   LINK_IFACE, LINK_SUDO         see scripts/benchmark-link.sh
#   REPEATS                       measured repeats per scenario (default 3)
#   REMOTE_BASE                   remote directory this benchmark owns
#   OUT_DIR                       results.json and summary.md land here
#   DATASET_DIR                   generated payload
#   LOG_DIR                       per-run logs; must NOT be inside OUT_DIR
#   BENCH_HOST, BENCH_PORT, BENCH_USERNAME, BENCH_PASSWORD, BENCH_KNOWN_HOSTS
#
# Candidate and baseline are interleaved repeat by repeat, not run as two
# blocks: a shared host's throughput drifts over minutes, and a block layout
# would charge that drift to whichever build ran second.

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=scripts/benchmark-lib.sh disable=SC1091
source "$script_dir/benchmark-lib.sh"
# shellcheck source=scripts/benchmark-link.sh disable=SC1091
source "$script_dir/benchmark-link.sh"

# The standard benchmark's scenario set is fixed: adding one here would make
# every stored result before it incomparable. The matrix benchmark
# (scripts/benchmark-matrix.sh) is where new scenarios such as "single" live.
SCENARIOS=(small mixed large)

require_env CANDIDATE_BIN CANDIDATE_REF OUT_DIR DATASET_DIR LOG_DIR \
  REMOTE_BASE BENCH_HOST BENCH_USERNAME BENCH_PASSWORD BENCH_KNOWN_HOSTS
require_tools jq nproc

REPEATS=${REPEATS:-3}
BENCH_PORT=${BENCH_PORT:-22}
BASELINE_BIN=${BASELINE_BIN:-}
BASELINE_REF=${BASELINE_REF:-}
BENCH_CONNECTIONS=${BENCH_CONNECTIONS:-}

if [[ ! "$REPEATS" =~ ^[1-9][0-9]*$ ]]; then
  echo "::error::REPEATS must be a positive integer, got '$REPEATS'" >&2
  exit 1
fi

if [[ -n "$BENCH_CONNECTIONS" && ! "$BENCH_CONNECTIONS" =~ ^[1-9][0-9]*$ ]]; then
  echo "::error::BENCH_CONNECTIONS must be a positive integer, got '$BENCH_CONNECTIONS'" >&2
  exit 1
fi

# Parsed before anything is measured: a typo in a profile must not surface after
# minutes of uploading. Through a command substitution and not a process
# substitution, so a rejected profile actually stops the run under "set -e".
link_profiles_raw=$(link_parse_profiles "${BENCH_LINK_PROFILES:-}")
mapfile -t link_profiles <<<"$link_profiles_raw"
LINK_REQUESTED=("${link_profiles[@]}")

check_remote_base
mkdir -p "$OUT_DIR" "$DATASET_DIR" "$LOG_DIR"
check_log_dir

runs_file="$LOG_DIR/runs.jsonl"
aggregate_file="$LOG_DIR/aggregate.json"
probes_file="$LOG_DIR/link-probes.jsonl"
: >"$runs_file"
: >"$probes_file"

# The profile every run below is measured on. Set by the profile loop; a run
# always carries it, so "baseline" in a stored result means "the real line" and
# not "unknown".
link_profile=baseline

# measure <label> <binary> <ref> <scenario> <repeat> <advanced-yaml>
measure() {
  local label=$1 binary=$2 ref=$3 scenario=$4 repeat=$5 advanced=$6
  local remote="$REMOTE_BASE/$label/$scenario"
  local slug
  slug=$(link_profile_slug "$link_profile")
  local stem="$LOG_DIR/$slug-$label-$scenario-$repeat"
  local code=0

  # Unmeasured: every repeat starts from the same empty remote directory, so
  # repeat 1 (uploading into nothing) measures the same thing as the repeats
  # after it (which would otherwise overwrite existing files). METRICS_FILE is
  # cleared for it, so the pre-clean's own numbers can never be mistaken for
  # the measurement's.
  if ! METRICS_FILE='' run_easysftp "$binary" "$DATASET_DIR/empty" "$remote" clean "$stem.clean.log" "$stem.clean.out"; then
    echo "::warning::pre-clean of $label/$scenario repeat $repeat failed"
    cat "$stem.clean.log"
  fi

  METRICS_FILE="$stem.metrics.json" \
    run_easysftp "$binary" "$DATASET_DIR/$scenario" "$remote" overlay "$stem.log" "$stem.out" "$advanced" || code=$?
  if ((code != 0)); then
    # Into the job log, which masks secrets. Never into the artifact.
    echo "::warning::$label/$scenario repeat $repeat exited $code"
    cat "$stem.log"
  fi

  jq -nc \
    --arg label "$label" \
    --arg ref "$ref" \
    --arg scenario "$scenario" \
    --arg link_profile "$link_profile" \
    --argjson repeat "$repeat" \
    --argjson exit_code "$code" \
    --argjson duration_ms "$(step_number "$stem.out" duration-ms)" \
    --argjson files "$(step_number "$stem.out" files-uploaded)" \
    --argjson bytes "$(step_number "$stem.out" bytes-uploaded)" \
    --argjson retries "$(count_matches "$stem.log" -e retrying -e reconnecting)" \
    --argjson errors "$(count_matches "$stem.log" '^::error::')" \
    --argjson refused "$(count_matches "$stem.log" -e 'could not open connection')" \
    --argjson metrics "$(metrics_json "$stem.metrics.json")" \
    '$ARGS.named' >>"$runs_file"

  echo "$link_profile $label/$scenario repeat $repeat: $(step_number "$stem.out" duration-ms) ms, exit $code"
}

labels=(candidate)
binaries=("$CANDIDATE_BIN")
refs=("$CANDIDATE_REF")
advanced=("")
if [[ -n "$BASELINE_BIN" ]]; then
  labels+=(baseline)
  binaries+=("$BASELINE_BIN")
  refs+=("${BASELINE_REF:-unknown}")
  advanced+=("")
fi
# The connection pool is measured as a third build of the *same* binary, not
# as a separate workflow run: the two numbers are only comparable when they
# are interleaved on the same host in the same minutes.
if [[ -n "$BENCH_CONNECTIONS" ]]; then
  labels+=("pool$BENCH_CONNECTIONS")
  binaries+=("$CANDIDATE_BIN")
  refs+=("$CANDIDATE_REF")
  advanced+=("connections: $BENCH_CONNECTIONS")
fi

generate_dataset "${SCENARIOS[@]}"

# Shaping is only probed for when a profile actually asks for it: a run on the
# real line must not need tc, sudo or NET_ADMIN.
if link_shape_needed "${link_profiles[@]}"; then
  link_shape_probe
  if ((LINK_SHAPING_AVAILABLE != 1)); then
    echo "::warning::link profiles were requested but shaping is unavailable ($LINK_SHAPING_REASON); every profile is measured on the real line" >&2
  fi
fi

# The profile loop is the outermost one: re-applying tc per measured run would
# itself be noise. The price is that drift over the hours falls onto this axis
# instead of spreading across it, which is why each profile is probed twice,
# before and after its own runs, and still shaped for both. A start and an end
# probe of the same profile are comparable; two probes of different profiles are
# not, which is what makes drift visible here at all.
for link_profile in "${link_profiles[@]}"; do
  link_shape_apply "$link_profile" || true
  link_probe "$link_profile" start "$probes_file"
  for scenario in "${SCENARIOS[@]}"; do
    for ((repeat = 1; repeat <= REPEATS; repeat++)); do
      for i in "${!labels[@]}"; do
        measure "${labels[$i]}" "${binaries[$i]}" "${refs[$i]}" "$scenario" "$repeat" "${advanced[$i]}"
      done
    done
  done
  link_probe "$link_profile" end "$probes_file"
done

# The trap does this too, but only on the way out: doing it here keeps the
# cleanup runs below on the same unshaped line every run ends on.
link_shape_clear

# Leave the benchmark directories empty so the payload does not linger on the
# server. Best effort: a cleanup hiccup must not hide the results.
for scenario in "${SCENARIOS[@]}"; do
  for i in "${!labels[@]}"; do
    stem="$LOG_DIR/cleanup-${labels[$i]}-$scenario"
    METRICS_FILE='' run_easysftp "${binaries[$i]}" "$DATASET_DIR/empty" \
      "$REMOTE_BASE/${labels[$i]}/$scenario" clean "$stem.log" "$stem.out" "${advanced[$i]}" ||
      echo "::warning::cleanup of ${labels[$i]}/$scenario failed"
  done
done
# The probe removes its own payload, but a probe that was killed mid-write does
# not, and a leftover would be counted by the next run's remote scan.
if [[ -n "${LINKPROBE_BIN:-}" ]]; then
  METRICS_FILE='' run_easysftp "$CANDIDATE_BIN" "$DATASET_DIR/empty" \
    "$REMOTE_BASE/linkprobe" clean "$LOG_DIR/cleanup-linkprobe.log" "$LOG_DIR/cleanup-linkprobe.out" ||
    echo "::warning::cleanup of the link probe directory failed"
fi

# One aggregate row per (link profile, build, scenario). The timing fields at the
# top level are exactly the ones results.json v1 had, so anything already reading
# a stored benchmark keeps working; everything new sits in its own sub-object.
# With no profiles requested there is exactly one profile, "baseline", and the
# row count is what it always was.
#
# duration_ms.* is wall clock. process.* is what the run cost the machine.
# phases[] is wall clock per phase and adds up to roughly the duration.
# operations[] is cumulative across parallel workers and does not: see the
# "note" field the metrics file carries.
jq -s "
  $JQ_STATS
  group_by([.link_profile, .label, .scenario])
  | map(
      (map(select(.metrics != null)) | map(.metrics)) as \$m
      | {
        label: .[0].label,
        ref: .[0].ref,
        scenario: .[0].scenario,
        link_profile: .[0].link_profile,
        repeats: length,
        failed_runs: (map(select(.exit_code != 0)) | length),
        files: (map(.files) | max),
        bytes: (map(.bytes) | max),
        durations_ms: map(.duration_ms),
        median_ms: (map(.duration_ms) | median),
        min_ms: (map(.duration_ms) | min),
        max_ms: (map(.duration_ms) | max),
        mad_ms: (map(.duration_ms) | mad),
        duration_ms: (map(.duration_ms) | stats),
        retries: (map(.retries) | add),
        errors: (map(.errors) | add),
        refused_connections: (map(.refused) | add),
        process: {
          user_cpu_ms: (\$m | map(.process.user_cpu_ms) | median),
          sys_cpu_ms: (\$m | map(.process.sys_cpu_ms) | median),
          cpu_percent: (\$m | map(.process.cpu_percent) | median),
          max_rss_bytes: (\$m | map(.process.max_rss_bytes) | median),
          go_total_alloc_bytes: (\$m | map(.process.go_total_alloc_bytes) | median),
          go_mallocs: (\$m | map(.process.go_mallocs) | median),
          go_gc_count: (\$m | map(.process.go_gc_count) | median),
          go_gc_pause_total_ms: (\$m | map(.process.go_gc_pause_total_ms) | median),
          go_peak_goroutines: (\$m | map(.process.go_peak_goroutines) | max),
          disk_read_bytes: (\$m | map(.process.disk_read_bytes) | median),
          net_read_bytes: (\$m | map(.process.net_read_bytes) | median),
          net_write_bytes: (\$m | map(.process.net_write_bytes) | median)
        },
        counters: (\$m | map(.counters // {}) | map(to_entries) | add // []
          | group_by(.key)
          | map({key: .[0].key, value: (map(.value) | median)}) | from_entries),
        phases: (\$m | map(.phases // []) | add // []
          | group_by(.name)
          | map({name: .[0].name, median_ms: (map(.wall_ms) | median)})
          | sort_by(-.median_ms)),
        operations: (\$m | map(.operations // []) | add // []
          | group_by(.name)
          | map({
              name: .[0].name,
              count: (map(.count) | median),
              median_total_ms: (map(.total_ms) | median),
              avg_ms: (map(.avg_ms) | median),
              p50_ms: (map(.p50_ms) | median),
              p90_ms: (map(.p90_ms) | median),
              p99_ms: (map(.p99_ms) | median),
              max_ms: (map(.max_ms) | median),
              errors: (map(.errors) | add)
            })
          | sort_by(-.median_total_ms))
      })
  | map(. + {
      mib_per_s: (if .median_ms > 0 then ((.bytes / 1048576) / (.median_ms / 1000) | round2) else 0 end),
      files_per_s: (if .median_ms > 0 then (.files / (.median_ms / 1000) | round2) else 0 end)
    })
" "$runs_file" >"$aggregate_file"

# The reference build every delta is measured against: the baseline when one
# was measured, otherwise the candidate, so a pool run without a baseline is
# still compared against something.
reference_label=candidate
if [[ -n "$BASELINE_BIN" ]]; then
  reference_label=baseline
fi

settings="easySFTP defaults (no advanced.* overrides): concurrency 4, request_concurrency 16, retries 2, timeout 30s, mode overlay"
if [[ -n "$BENCH_CONNECTIONS" ]]; then
  settings="$settings; the pool$BENCH_CONNECTIONS build is the same binary with advanced.connections: $BENCH_CONNECTIONS"
fi
if [[ -n "${BENCH_LINK_PROFILES:-}" ]]; then
  settings="$settings; measured over the link profiles ${link_profiles[*]}"
fi

scenario_docs=$(for scenario in "${SCENARIOS[@]}"; do
  jq -nc --arg k "$scenario" --arg v "$(scenario_description "$scenario")" '{key: $k, value: $v}'
done | jq -s 'from_entries')

# results.json, schema_version 2. Layers, in the order a reader needs them:
#   metadata (candidate_ref, baseline_ref, repeats, runner, settings, env)
#   results   aggregated per build and scenario, incl. process/phases/operations
#   comparison  candidate (and pool) against the reference build
#   runs      every individual repeat, verbatim, metrics included
# v1's top-level keys all still exist and mean the same thing.
# Through a file, not a process substitution: jq's --slurpfile wants a real
# path, and /dev/fd is not one everywhere this may be run.
runs_array="$LOG_DIR/runs.json"
jq -s '.' "$runs_file" >"$runs_array"

jq -n \
  --slurpfile results "$aggregate_file" \
  --slurpfile runs "$runs_array" \
  --arg candidate_ref "$CANDIDATE_REF" \
  --arg baseline_ref "$BASELINE_REF" \
  --arg reference_label "$reference_label" \
  --argjson repeats "$REPEATS" \
  --arg runner "${RUNNER_ENVIRONMENT:-local}, $(uname -sr), $(nproc) cpu" \
  --argjson environment "$(bench_environment)" \
  --argjson link "$(link_json "$probes_file")" \
  --arg settings "$settings" \
  --argjson scenarios "$scenario_docs" \
  "
  $JQ_STATS
  (\$results[0]) as \$r
  | {
     schema_version: 2,
     benchmark_kind: \"standard\",
     candidate_ref: \$candidate_ref,
     baseline_ref: \$baseline_ref,
     repeats: \$repeats,
     runner: \$runner,
     environment: \$environment,
     link: \$link,
     settings: \$settings,
     reference_label: \$reference_label,
     scenarios: \$scenarios,
     note: \"phases are wall clock and add up to the duration; operations are cumulative across parallel workers and do not\",
     results: \$r,
     comparison: [
       \$r[] | select(.label != \$reference_label) as \$c
       | (\$r[] | select(.label == \$reference_label and .scenario == \$c.scenario
            and .link_profile == \$c.link_profile)) as \$b
       | {
           scenario: \$c.scenario,
           label: \$c.label,
           link_profile: \$c.link_profile,
           reference_label: \$reference_label,
           median_ms: \$c.median_ms,
           reference_median_ms: \$b.median_ms,
           delta_ms: (\$c.median_ms - \$b.median_ms),
           delta_percent: pct(\$c.median_ms; \$b.median_ms),
           reference_mad_ms: \$b.mad_ms,
           within_noise: (if (\$b.mad_ms // 0) == 0 then null
             else ((if \$c.median_ms - \$b.median_ms < 0 then \$b.median_ms - \$c.median_ms else \$c.median_ms - \$b.median_ms end) <= \$b.mad_ms) end)
         }
     ],
     runs: \$runs[0]
   }" >"$OUT_DIR/results.json"

# CSV alongside the JSON: one row per (build, scenario), so a spreadsheet or a
# plot can read a stored result without a JSON parser. Deliberately flat and
# aggregate-only; the raw repeats stay in results.json.
#
# link_profile, rtt_p50_ms and control_single_mib_per_s make a row readable on
# its own: without them a throughput number cannot be told apart from the line
# it was measured on. The two link numbers come from the profile's own start
# probe, which is the one taken right before these runs.
jq -r '
  (.link.probes // []) as $probes
  | ["scenario","build","ref","link_profile","rtt_p50_ms","control_single_mib_per_s",
     "repeats","files","bytes","median_ms","min_ms","max_ms","mad_ms",
     "mib_per_s","files_per_s","user_cpu_ms","sys_cpu_ms","cpu_percent","max_rss_bytes",
     "go_gc_count","go_peak_goroutines","net_write_bytes","connections_opened","connections_refused",
     "retries","errors","failed_runs"],
  (.results[]
   | . as $row
   | ([$probes[] | select(.profile == $row.link_profile and .at == "start")] | first) as $p
   | [
    .scenario, .label, .ref, .link_profile,
    ($p.rtt_ms.p50 // null), ($p.control.single_stream_mib_per_s // null),
    .repeats, .files, .bytes, .median_ms, .min_ms, .max_ms, .mad_ms,
    .mib_per_s, .files_per_s, .process.user_cpu_ms, .process.sys_cpu_ms, .process.cpu_percent,
    .process.max_rss_bytes, .process.go_gc_count, .process.go_peak_goroutines, .process.net_write_bytes,
    (.counters.connections_opened // 0), (.counters.connections_refused // 0),
    .retries, .errors, .failed_runs
  ])
  | @csv
' "$OUT_DIR/results.json" >"$OUT_DIR/results.csv"

# summary.md carries the same numbers, readable. The delta column compares
# every build's median against the reference build's; negative means faster.
{
  echo "## easySFTP benchmark"
  echo
  echo "| Setting | Value |"
  echo "|---|---|"
  echo "| Candidate | \`$CANDIDATE_REF\` |"
  echo "| Baseline | \`${BASELINE_REF:-none}\` |"
  echo "| Repeats per scenario | $REPEATS |"
  echo "| Runner | $(uname -sr), $(nproc) cpu |"
  echo "| Link profiles | ${BENCH_LINK_PROFILES:-the real line} |"
  echo "| Settings | $settings |"
  echo
  link_markdown "$OUT_DIR/results.json"
  echo "### Throughput"
  echo
  echo "| Scenario | Build | Profile | Files | Size | Median | Min | Max | MAD | MiB/s | files/s | Retries | Errors | Failed runs | Delta |"
  echo "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"
  for scenario in "${SCENARIOS[@]}"; do
    for label in "${labels[@]}"; do
      for profile in "${link_profiles[@]}"; do
        jq -r --arg s "$scenario" --arg l "$label" --arg p "$profile" '
        (.results[] | select(.scenario == $s and .label == $l and .link_profile == $p)) as $row
        # Through a list: the reference build has no comparison entry, and a
        # bare "as" over an empty generator would drop its whole row.
        | ([.comparison[] | select(.scenario == $s and .label == $l and .link_profile == $p) | .delta_percent] | first) as $delta
        | "| \($s) | \($l) | \($p) | \($row.files) | \((($row.bytes / 1048576) * 10 | round / 10)) MiB "
          + "| \($row.median_ms) ms | \($row.min_ms) ms | \($row.max_ms) ms | \($row.mad_ms) ms "
          + "| \($row.mib_per_s) | \($row.files_per_s) | \($row.retries) | \($row.errors) | \($row.failed_runs) "
          + "| \(if $delta == null then "-" else (if $delta > 0 then "+" else "" end) + ($delta | tostring) + "%" end) |"
        ' "$OUT_DIR/results.json"
      done
    done
  done
  echo
  echo "Delta compares each build's median against the \`$reference_label\` build **on the same link profile**; negative is faster. MAD is the median absolute deviation of the repeats: a delta smaller than it is inside this host's own noise."

  echo
  echo "### Resources (median per run)"
  echo
  echo "| Scenario | Build | Profile | User CPU | Sys CPU | CPU % | Peak RSS | Go allocs | GCs | GC pause | Peak goroutines | Net sent |"
  echo "|---|---|---|---|---|---|---|---|---|---|---|---|"
  # Looped rather than one jq pass over .results, so the rows come out in
  # scenario order like the throughput table above instead of in jq's
  # group_by order.
  for scenario in "${SCENARIOS[@]}"; do
    for label in "${labels[@]}"; do
      for profile in "${link_profiles[@]}"; do
        jq -r --arg s "$scenario" --arg l "$label" --arg p "$profile" \
          '.results[] | select(.scenario == $s and .label == $l and .link_profile == $p)
        | "| \(.scenario) | \(.label) | \(.link_profile) | \(.process.user_cpu_ms) ms | \(.process.sys_cpu_ms) ms | \(.process.cpu_percent)% "
          + "| \(((.process.max_rss_bytes / 1048576) * 10 | round / 10)) MiB "
          + "| \(((.process.go_total_alloc_bytes / 1048576) * 10 | round / 10)) MiB | \(.process.go_gc_count) "
          + "| \(.process.go_gc_pause_total_ms) ms | \(.process.go_peak_goroutines) "
          + "| \(((.process.net_write_bytes / 1048576) * 10 | round / 10)) MiB |"
        ' "$OUT_DIR/results.json"
      done
    done
  done

  echo
  echo "### Where the time goes"
  echo
  echo "Phases are wall clock and add up to roughly the run's duration. Operation totals are **cumulative across parallel workers** and are normally larger than the phase they belong to; read them for their share and their per-call cost, never as wall clock."
  echo
  for scenario in "${SCENARIOS[@]}"; do
    echo "<details><summary><code>$scenario</code> phases and round-trips</summary>"
    echo
    echo "| Build | Profile | Phase | Wall |"
    echo "|---|---|---|---|"
    jq -r --arg s "$scenario" '.results[] | select(.scenario == $s) as $row
      | $row.phases[] | select(.median_ms > 0)
      | "| \($row.label) | \($row.link_profile) | \(.name) | \(.median_ms) ms |"' "$OUT_DIR/results.json"
    echo
    echo "| Build | Profile | Operation | Count | Cumulative | Avg | p50 | p90 | p99 | Max |"
    echo "|---|---|---|---|---|---|---|---|---|---|"
    jq -r --arg s "$scenario" '.results[] | select(.scenario == $s) as $row
      | $row.operations[] | select(.count > 0)
      | "| \($row.label) | \($row.link_profile) | \(.name) | \(.count) | \(.median_total_ms) ms | \(.avg_ms) ms | \(.p50_ms) ms | \(.p90_ms) ms | \(.p99_ms) ms | \(.max_ms) ms |"' "$OUT_DIR/results.json"
    echo
    echo "</details>"
    echo
  done

  # Without this line a pool run whose extra connections the server refused
  # reads as "the pool did nothing", which is the wrong conclusion entirely.
  refused=$(jq '[.results[].refused_connections] | add // 0' "$OUT_DIR/results.json")
  if [[ "$refused" != 0 ]]; then
    echo "**$refused connection(s) were refused by the server** and fell back to the run's first connection, so a pool build measured here had fewer connections than configured."
    echo
  fi
  echo "Data only: these numbers set no threshold and fail no build. Collected to evaluate the single-connection ceiling discussed in issue #158 and to show where a run spends its time."
} >"$OUT_DIR/summary.md"

cat "$OUT_DIR/summary.md"
