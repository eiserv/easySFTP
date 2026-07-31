#!/usr/bin/env bash
#
# The jq aggregation the benchmark scripts used before issue #190 moved it into
# cmd/easysftp-bench, kept alive as the parity oracle for that move.
#
# This is scaffolding, not a second implementation to maintain. It exists so
# scripts/test-benchmark.sh can aggregate one measurement twice, once here and
# once with the Go command, and diff the two: a rewrite that claims to reproduce
# a stored document has to be shown doing it, on the live scripts and not on a
# fixture captured once. Step 6 of issue #190 deletes this file together with
# the last shell path it checks.
#
# It reads the same manifest the Go command reads, so the two get identical
# inputs by construction:
#
#   MANIFEST   the run manifest a measuring script wrote
#   OUT_DIR    where results.json/matrix.json and their CSV and Markdown land
#
# Nothing here may be "cleaned up". A tidier oracle is an oracle that no longer
# says what the old output was.

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=scripts/benchmark-lib.sh disable=SC1091
source "$script_dir/benchmark-lib.sh"
# shellcheck source=scripts/benchmark-link.sh disable=SC1091
source "$script_dir/benchmark-link.sh"

: "${MANIFEST:?MANIFEST is required}"
: "${OUT_DIR:?OUT_DIR is required}"
require_tools jq
mkdir -p "$OUT_DIR"

# The manifest holds everything the measuring script knew. Read back into the
# variable names the code below already used, so that code stays a move rather
# than a rewrite.
CANDIDATE_REF=$(jq -r '.candidate_ref' "$MANIFEST")
BASELINE_REF=$(jq -r '.baseline_ref' "$MANIFEST")
reference_label=$(jq -r '.reference_label' "$MANIFEST")
REPEATS=$(jq -r '.repeats' "$MANIFEST")
runner=$(jq -r '.runner' "$MANIFEST")
runner_display=$(jq -r '.runner_display' "$MANIFEST")
settings=$(jq -r '.settings' "$MANIFEST")
link_requested=$(jq -r '.link.requested // ""' "$MANIFEST")
connections_display=$(jq -r '.grid.connections_display // ""' "$MANIFEST")
concurrency_display=$(jq -r '.grid.concurrency_display // ""' "$MANIFEST")
request_display=$(jq -r '.grid.request_display // ""' "$MANIFEST")
CANARY_SCENARIO=$(jq -r '.grid.canary.scenario // ""' "$MANIFEST")
CANARY_CONNECTIONS=$(jq -r '.grid.canary.connections // ""' "$MANIFEST")
CANARY_CONCURRENCY=$(jq -r '.grid.canary.concurrency // ""' "$MANIFEST")
BASELINE_BIN=''
if [[ -n "$BASELINE_REF" ]]; then
  BASELINE_BIN='measured'
fi

runs_file=$(jq -r '.files.runs' "$MANIFEST")
deletes_file=$(jq -r '.files.deletes // ""' "$MANIFEST")
probes_file=$(jq -r '.files.probes // ""' "$MANIFEST")
canary_file=$(jq -r '.files.canary // ""' "$MANIFEST")
auto_file=$(jq -r '.files.auto // ""' "$MANIFEST")

LOG_DIR=$(mktemp -d)
trap 'rm -rf "$LOG_DIR"' EXIT
# The intermediate files the aggregation writes. In the measuring scripts these
# were named next to the JSONL; here they are scratch and go away with LOG_DIR.
aggregate_file="$LOG_DIR/aggregate.json"
for name in deletes_file canary_file auto_file probes_file; do
  path=${!name}
  if [[ -z "$path" || ! -f "$path" ]]; then
    printf -v "$name" '%s' "$LOG_DIR/empty-$name.jsonl"
    : >"${!name}"
  fi
done

mapfile -t SCENARIOS < <(jq -r '.scenarios[].name' "$MANIFEST")
mapfile -t labels < <(jq -r '.labels[]' "$MANIFEST")
mapfile -t link_profiles < <(jq -r '.link.profiles[]' "$MANIFEST")
scenarios=("${SCENARIOS[@]}")

# The declared grid, i.e. what was asked for before each scenario's payload
# capped it. A standard manifest has no grid and leaves these empty. The null
# element of the request axis travels as the token "default", which is how it
# travelled through the measuring script too.
mapfile -t connections_axis < <(jq -r '.grid.connections[]?' "$MANIFEST")
mapfile -t concurrency_axis < <(jq -r '.grid.concurrency[]?' "$MANIFEST")
mapfile -t request_axis < <(jq -r '.grid.request_concurrency[]? | if . == null then "default" else tostring end' "$MANIFEST")

declare -A scenario_conn_axis scenario_conc_axis scenario_req_axis
while IFS=$'	' read -r name conn conc req; do
  scenario_conn_axis[$name]=$conn
  scenario_conc_axis[$name]=$conc
  scenario_req_axis[$name]=$req
done < <(jq -r '.scenarios[] | [.name, .connections_display, .concurrency_display, .request_display] | @tsv' "$MANIFEST")

scenario_docs=$(jq -c '[.scenarios[] | {key: .name, value: .description}] | from_entries' "$MANIFEST")
per_scenario_axes=$(jq -c '[.scenarios[] | {key: .name, value: {files, connections, concurrency, request_concurrency}}] | from_entries' "$MANIFEST")

# oracle_link_json is link_json with its shaping half taken from the manifest
# rather than from the globals a measuring run would have set.
oracle_link_json() {
  local probes_json='[]'
  if [[ -s "$probes_file" ]]; then
    probes_json=$(jq -s '.' "$probes_file")
  fi
  jq -n \
    --argjson link "$(jq -c '.link' "$MANIFEST")" \
    --argjson probes "$probes_json" \
    '{
       iface: $link.iface,
       shaping: $link.shaping,
       probes: $probes,
       note: "environment describes the runner and is the comparability key; link is measured and varies per run. Under a network ceiling a code delta cannot be interpreted."
     }'
}

aggregate_standard() {
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

# The delete sweeps, aggregated the same way and kept strictly apart: one row per
# (link profile, build, scenario), sweeps that deleted nothing dropped, and a
# group left with none dropped entirely rather than stored as a row of zeroes.
delete_file="$LOG_DIR/delete.json"
jq -s "
  $JQ_STATS
  $JQ_DELETE
  group_by([.link_profile, .label, .scenario])
  | map({label: .[0].label, scenario: .[0].scenario, link_profile: .[0].link_profile} + delete_agg)
  | map(select(.sweeps > 0))
" "$deletes_file" >"$delete_file"

# reference_label, settings and scenario_docs were built here when this code
# lived in the measuring script. They now come from the manifest, read at the
# top of this file, exactly as they reach the Go command: what is under
# comparison is the aggregation, and feeding the two sides different metadata
# would only prove that two different inputs give two different documents.

# results.json, schema_version 2. Layers, in the order a reader needs them:
#   metadata (candidate_ref, baseline_ref, repeats, runner, settings, env)
#   results   aggregated per build and scenario, incl. process/phases/operations
#   comparison  candidate (and pool) against the reference build
#   deletes   the pre-clean sweeps, aggregated apart from everything above
#   runs      every individual repeat, verbatim, metrics included
# v1's top-level keys all still exist and mean the same thing.
# Through a file, not a process substitution: jq's --slurpfile wants a real
# path, and /dev/fd is not one everywhere this may be run.
runs_array="$LOG_DIR/runs.json"
jq -s '.' "$runs_file" >"$runs_array"

jq -n \
  --slurpfile results "$aggregate_file" \
  --slurpfile runs "$runs_array" \
  --slurpfile deletes "$delete_file" \
  --arg candidate_ref "$CANDIDATE_REF" \
  --arg baseline_ref "$BASELINE_REF" \
  --arg reference_label "$reference_label" \
  --argjson repeats "$REPEATS" \
  --arg runner "$runner" \
  --argjson environment "$(jq -c .environment "$MANIFEST")" \
  --argjson link "$(oracle_link_json)" \
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
     deletes: \$deletes[0],
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
  echo "| Runner | $runner_display |"
  echo "| Link profiles | ${link_requested:-the real line} |"
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
          + "| \($row.median_ms) ms | \($row.min_ms) ms | \($row.max_ms) ms | \(if $row.mad_ms == null then "-" else "\($row.mad_ms) ms" end) "
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

  echo "### Delete sweeps"
  echo
  echo "The pre-clean before every measured run wipes the tree the previous repeat left behind, which makes it a pure delete sweep. It costs no extra time (it has always run) and its numbers never enter the upload tables above. Sweeps that found an empty directory are not counted."
  echo
  echo "| Scenario | Build | Profile | Sweeps | Files deleted | Median | files/s | remote_scan | delete_sweep |"
  echo "|---|---|---|---|---|---|---|---|---|"
  jq -r '
    def phase($n): [.phases[] | select(.name == $n) | .median_ms] | first;
    def ms: if . == null then "-" else "\(.) ms" end;
    .deletes[]
    | "| \(.scenario) | \(.label) | \(.link_profile) | \(.sweeps) | \(.files_deleted) | \(.median_ms) ms "
      + "| \(.deletes_per_s) | \(phase("remote_scan") | ms) | \(phase("delete_sweep") | ms) |"
  ' "$OUT_DIR/results.json"
  echo
  echo "| Scenario | Build | Profile | Operation | Count | Cumulative | p50 | p90 | p99 | Max |"
  echo "|---|---|---|---|---|---|---|---|---|---|"
  # sftp_remove and sftp_rmdir are the numbers issue #157 is about: one
  # round-trip per entry, strictly sequential, no concurrency anywhere near them.
  jq -r '.deletes[] as $d | $d.operations[] | select(.count > 0)
    | "| \($d.scenario) | \($d.label) | \($d.link_profile) | \(.name) | \(.count) | \(.median_total_ms) ms "
      + "| \(.p50_ms) ms | \(.p90_ms) ms | \(.p99_ms) ms | \(.max_ms) ms |"' "$OUT_DIR/results.json"
  echo

  # Without this line a pool run whose extra connections the server refused
  # reads as "the pool did nothing", which is the wrong conclusion entirely.
  refused=$(jq '[.results[].refused_connections] | add // 0' "$OUT_DIR/results.json")
  if [[ "$refused" != 0 ]]; then
    echo "**$refused connection(s) were refused by the server** and fell back to the run's first connection, so a pool build measured here had fewer connections than configured."
    echo
  fi
  echo "Data only: these numbers set no threshold and fail no build. Collected to evaluate the single-connection ceiling discussed in issue #158 and to show where a run spends its time."
} >"$OUT_DIR/summary.md"

}

aggregate_matrix() {
cells_file="$LOG_DIR/cells.json"
jq -s "
  $JQ_STATS
  group_by([.link_profile, .label, .scenario, .connections, .concurrency, .request_concurrency])
  | map(
      (map(select(.metrics != null)) | map(.metrics)) as \$m
      | {
        scenario: .[0].scenario,
        label: .[0].label,
        ref: .[0].ref,
        link_profile: .[0].link_profile,
        connections: .[0].connections,
        concurrency: .[0].concurrency,
        request_concurrency: .[0].request_concurrency,
        repeats: length,
        failed_runs: (map(select(.exit_code != 0)) | length),
        files: (map(.files) | max),
        bytes: (map(.bytes) | max),
        durations_ms: map(.duration_ms),
        median_ms: (map(.duration_ms) | median),
        min_ms: (map(.duration_ms) | min),
        max_ms: (map(.duration_ms) | max),
        mad_ms: (map(.duration_ms) | mad),
        retries: (map(.retries) | add),
        errors: (map(.errors) | add),
        user_cpu_ms: (\$m | map(.process.user_cpu_ms) | median),
        sys_cpu_ms: (\$m | map(.process.sys_cpu_ms) | median),
        cpu_percent: (\$m | map(.process.cpu_percent) | median),
        max_rss_bytes: (\$m | map(.process.max_rss_bytes) | median),
        go_gc_count: (\$m | map(.process.go_gc_count) | median),
        go_peak_goroutines: (\$m | map(.process.go_peak_goroutines) | max),
        net_write_bytes: (\$m | map(.process.net_write_bytes) | median),
        connections_opened: (\$m | map(.counters.connections_opened // 0) | median),
        connections_used: (\$m | map(.counters.connections_used // 0) | median),
        # What the run actually ran with, not what the axis asked for: the
        # request axis has a pass that sets nothing, and a null coordinate on
        # its own does not say which value that was (issue #184, phase 5).
        request_concurrency_used: (\$m | map(.counters.config_request_concurrency) | median),
        connections_refused: (\$m | map(.counters.connections_refused // 0) | add),
        reconnects: (\$m | map(.counters.reconnects // 0) | add),
        upload_phase_ms: (\$m | map(.phases // []) | add // []
          | map(select(.name == \"upload\") | .wall_ms) | median),
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
  | sort_by([.link_profile, .scenario, .label, .connections, .concurrency, .request_concurrency])
" "$runs_file" >"$cells_file"

# The delete sweeps, one row per cell coordinate, sweeps that found an empty
# directory dropped (the first cell of a build and scenario is one) and a
# coordinate left with none dropped with them. Kept apart from cells[]: a delete
# sweep and an upload at the same coordinates are two different measurements.
deletes_agg_file="$LOG_DIR/deletes.json"
jq -s "
  $JQ_STATS
  $JQ_DELETE
  group_by([.link_profile, .label, .scenario, .connections, .concurrency, .request_concurrency])
  | map({
      scenario: .[0].scenario, label: .[0].label, link_profile: .[0].link_profile,
      connections: .[0].connections, concurrency: .[0].concurrency,
      request_concurrency: .[0].request_concurrency
    } + delete_agg)
  | map(select(.sweeps > 0))
  | sort_by([.link_profile, .scenario, .label, .connections, .concurrency, .request_concurrency])
" "$deletes_file" >"$deletes_agg_file"

# The auto runs, one row per (profile, scenario). "chosen" is read out of the
# run's own counters, so it says what easySFTP did rather than what this script
# expects it to do; the regret against the grid is added further down, where the
# cells are in scope.
auto_agg_file="$LOG_DIR/auto.json"
jq -s "
  $JQ_STATS
  group_by([.link_profile, .scenario])
  | map(
      (map(select(.metrics != null)) | map(.metrics)) as \$m
      | {
        scenario: .[0].scenario,
        label: \"auto\",
        ref: .[0].ref,
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
        chosen: {
          connections: (\$m | map(.counters.config_connections) | median),
          concurrency: (\$m | map(.counters.config_concurrency) | median),
          request_concurrency: (\$m | map(.counters.config_request_concurrency) | median)
        }
      })
  | map(. + {
      mib_per_s: (if .median_ms > 0 then ((.bytes / 1048576) / (.median_ms / 1000) | round2) else 0 end),
      files_per_s: (if .median_ms > 0 then (.files / (.median_ms / 1000) | round2) else 0 end)
    })
  | sort_by([.link_profile, .scenario])
" "$auto_file" >"$auto_agg_file"

# per_scenario_axes, settings and scenario_docs were built here when this code
# lived in the measuring script. They now come from the manifest, read at the
# top of this file, exactly as they reach the Go command: what is under
# comparison is the aggregation, and feeding the two sides different metadata
# would only prove that two different inputs give two different documents.

# matrix.json. "axes" declares the grid explicitly so a heatmap does not have to
# infer it from the cells that happen to be present, "cells" is one row per
# combination, "scaling" is the same data pre-grouped into the curve a reader
# usually wants (per scenario and build, ordered by connections then
# concurrency), "comparison" pairs each candidate cell with the reference
# build's cell at the same coordinates, "canary" holds the fixed cell's three
# measurements per profile, and "deletes" holds the pre-clean sweeps at the same
# coordinates as the cells.
#
# A matrix run has no "runs[]" the way a standard result does, so a cell is the
# finest grain there is. That is why it carries its own phases[] and
# operations[] and not just upload_phase_ms: those are hours of measurement that
# used to be aggregated and then dropped (issue #184, phase 2).
#
# "auto" is the policy measurement of phase 5: what easySFTP picked when asked
# to pick, what the grid says was best, and the gap. "scaling[].best_at_axis_max"
# is the other half of that: an optimum sitting on the largest value of an axis
# was not measured, it was cut off, and a policy fitted to such a grid
# extrapolates. Both are reported per scenario and profile, never averaged: the
# whole point is that they differ per link.
jq -n \
  --slurpfile cells "$cells_file" \
  --slurpfile auto "$auto_agg_file" \
  --argjson per_scenario_axes "$per_scenario_axes" \
  --arg candidate_ref "$CANDIDATE_REF" \
  --arg baseline_ref "$BASELINE_REF" \
  --arg reference_label "$reference_label" \
  --argjson repeats "$REPEATS" \
  --arg runner "$runner" \
  --argjson environment "$(jq -c .environment "$MANIFEST")" \
  --argjson link "$(oracle_link_json)" \
  --argjson canary "$(jq -s '.' "$canary_file")" \
  --slurpfile deletes "$deletes_agg_file" \
  --arg settings "$settings" \
  --argjson scenarios "$scenario_docs" \
  --argjson link_axis "$(printf '%s\n' "${link_profiles[@]}" | jq -Rs 'split("\n") | map(select(. != ""))')" \
  --argjson connections_axis "$(printf '%s\n' "${connections_axis[@]}" | jq -s '.')" \
  --argjson concurrency_axis "$(printf '%s\n' "${concurrency_axis[@]}" | jq -s '.')" \
  --argjson request_axis "$(printf '%s\n' "${request_axis[@]}" | jq -Rs 'split("\n") | map(select(. != "") | if . == "default" then null else tonumber end)')" \
  "
  $JQ_STATS
  (\$cells[0]) as \$c
  | {
     schema_version: 2,
     benchmark_kind: \"matrix\",
     candidate_ref: \$candidate_ref,
     baseline_ref: \$baseline_ref,
     reference_label: \$reference_label,
     repeats: \$repeats,
     runner: \$runner,
     environment: \$environment,
     link: \$link,
     canary: \$canary,
     settings: \$settings,
     scenarios: \$scenarios,
     note: \"one cell per (link_profile, scenario, build, connections, concurrency, request_concurrency); median_ms is wall clock over the cell's repeats. Each scenario is swept over the axis values its payload can use (axes.per_scenario), so a cell absent from the declared axes was not skipped, it was the same configuration twice. auto[] is off the grid: it is the settings easySFTP picked for itself, scored against the best cell\",
     axes: {
       link_profiles: \$link_axis,
       connections: \$connections_axis,
       concurrency: \$concurrency_axis,
       request_concurrency: \$request_axis,
       per_scenario: \$per_scenario_axes
     },
     cells: \$c,
     deletes: \$deletes[0],
     scaling: (\$c | group_by([.link_profile, .scenario, .label])
       | map(. as \$g
         | (\$g | sort_by(.median_ms) | .[0]) as \$best
         | {
           scenario: .[0].scenario,
           label: .[0].label,
           link_profile: .[0].link_profile,
           points: (sort_by([.connections, .concurrency])
             | map({connections, concurrency, request_concurrency, request_concurrency_used,
                    median_ms, mib_per_s, files_per_s,
                    connections_used, connections_refused, max_rss_bytes, user_cpu_ms})),
           best: (\$best | {connections, concurrency, request_concurrency, median_ms, mib_per_s, files_per_s}),
           best_at_axis_max: ([
             (if ((\$g | map(.connections) | unique | length) > 1)
                 and \$best.connections == (\$g | map(.connections) | max)
               then \"connections\" else empty end),
             (if ((\$g | map(.concurrency) | unique | length) > 1)
                 and \$best.concurrency == (\$g | map(.concurrency) | max)
               then \"concurrency\" else empty end),
             (if ((\$g | map(.request_concurrency) | unique | length) > 1)
                 and \$best.request_concurrency != null
                 and \$best.request_concurrency == (\$g | map(.request_concurrency) | max)
               then \"request_concurrency\" else empty end)
           ])
         })),
     auto: (\$auto[0] | map(. as \$a
       | ([\$c[] | select(.label == \"candidate\" and .scenario == \$a.scenario
            and .link_profile == \$a.link_profile)] | sort_by(.median_ms) | first) as \$best
       | ([\$c[] | select(.label == \"candidate\" and .scenario == \$a.scenario
            and .link_profile == \$a.link_profile
            and .connections == \$a.chosen.connections
            and .concurrency == \$a.chosen.concurrency
            and .request_concurrency_used == \$a.chosen.request_concurrency)] | first) as \$at
       | \$a + {
           best: (if \$best == null then null
             else (\$best | {connections, concurrency, request_concurrency, median_ms, mib_per_s, files_per_s})
             end),
           chosen_in_grid: (\$at != null),
           chosen_cell_median_ms: (if \$at == null then null else \$at.median_ms end),
           regret_ms: (if \$best == null then null else \$a.median_ms - \$best.median_ms end),
           regret_percent: (if \$best == null then null else pct(\$a.median_ms; \$best.median_ms) end)
         })),
     comparison: [
       \$c[] | select(.label != \$reference_label) as \$x
       | (\$c[] | select(.label == \$reference_label and .scenario == \$x.scenario
            and .link_profile == \$x.link_profile
            and .connections == \$x.connections and .concurrency == \$x.concurrency
            and .request_concurrency == \$x.request_concurrency)) as \$b
       | {
           scenario: \$x.scenario, label: \$x.label, reference_label: \$reference_label,
           link_profile: \$x.link_profile,
           connections: \$x.connections, concurrency: \$x.concurrency,
           request_concurrency: \$x.request_concurrency,
           median_ms: \$x.median_ms, reference_median_ms: \$b.median_ms,
           delta_ms: (\$x.median_ms - \$b.median_ms),
           delta_percent: pct(\$x.median_ms; \$b.median_ms)
         }
     ]
   }" >"$OUT_DIR/matrix.json"

# One flat row per cell: this is the file a heatmap or a scaling plot reads.
#
# link_profile, rtt_p50_ms and control_single_mib_per_s come along so a cell is
# readable on its own. net_write_bytes is here too: cells[] has always carried
# it, and it is the protocol-overhead number (117% of payload for "small").
jq -r '
  (.link.probes // []) as $probes
  | ["scenario","build","ref","link_profile","rtt_p50_ms","control_single_mib_per_s",
     "connections","concurrency","request_concurrency","request_concurrency_used","repeats",
     "files","bytes","median_ms","min_ms","max_ms","mad_ms","mib_per_s","files_per_s",
     "upload_phase_ms","net_write_bytes","user_cpu_ms","sys_cpu_ms","cpu_percent","max_rss_bytes","go_gc_count",
     "go_peak_goroutines","connections_opened","connections_used","connections_refused",
     "reconnects","retries","errors","failed_runs"],
  (.cells[]
   | . as $cell
   | ([$probes[] | select(.profile == $cell.link_profile and .at == "start")] | first) as $p
   | [
    .scenario, .label, .ref, .link_profile,
    ($p.rtt_ms.p50 // null), ($p.control.single_stream_mib_per_s // null),
    .connections, .concurrency, .request_concurrency, .request_concurrency_used, .repeats,
    .files, .bytes, .median_ms, .min_ms, .max_ms, .mad_ms, .mib_per_s, .files_per_s,
    .upload_phase_ms, .net_write_bytes, .user_cpu_ms, .sys_cpu_ms, .cpu_percent, .max_rss_bytes, .go_gc_count,
    .go_peak_goroutines, .connections_opened, .connections_used, .connections_refused,
    .reconnects, .retries, .errors, .failed_runs
  ])
  | @csv
' "$OUT_DIR/matrix.json" >"$OUT_DIR/matrix.csv"

{
  echo "## easySFTP connections/concurrency matrix"
  echo
  echo "| Setting | Value |"
  echo "|---|---|"
  echo "| Candidate | \`$CANDIDATE_REF\` |"
  echo "| Baseline | \`${BASELINE_REF:-none}\` |"
  echo "| Repeats per cell | $REPEATS |"
  echo "| Runner | $runner_display |"
  echo "| connections | $connections_display |"
  echo "| concurrency | $concurrency_display |"
  echo "| request_concurrency | ${request_display:-easySFTP default} |"
  echo "| Link profiles | ${link_requested:-the real line} |"
  echo "| Scenarios | ${scenarios[*]} |"
  echo
  # What each scenario is, next to the numbers: "sync" and "redeploy" are a
  # deploy shape and not only a payload, and a MiB/s of theirs is over the few
  # changed files rather than over the tree.
  echo "| Scenario | Mode | Payload | connections | concurrency | request_concurrency |"
  echo "|---|---|---|---|---|---|"
  for scenario in "${scenarios[@]}"; do
    read -r mode prepopulate _ <<<"$(scenario_shape "$scenario")"
    if ((prepopulate)); then
      mode="$mode, redeployed"
    fi
    echo "| \`$scenario\` | $mode | $(scenario_description "$scenario") | ${scenario_conn_axis[$scenario]} | ${scenario_conc_axis[$scenario]} | ${scenario_req_axis[$scenario]} |"
  done
  echo
  echo "The last three columns are the axis values this scenario was swept over, which are not always the ones requested above. A value larger than the payload's file count is the same configuration under another name (only the per-file upload path spreads over connections and workers), and \`request_concurrency\` is a per-file setting that a payload of small files cannot use at all, so both are capped against the payload rather than measured twice."
  echo
  link_markdown "$OUT_DIR/matrix.json"
  echo "Each grid below is median wall-clock milliseconds: rows are \`advanced.connections\`, columns are \`advanced.concurrency\`. Lower is better. \`connections > concurrency\` is measured, not skipped; easySFTP caps the pool at the concurrency (a connection no worker picks is a handshake for nothing), so those cells are expected to flatten out rather than improve."
  echo

  for scenario in "${scenarios[@]}"; do
    read -r -a axis_conn <<<"${scenario_conn_axis[$scenario]}"
    read -r -a axis_conc <<<"${scenario_conc_axis[$scenario]}"
    read -r -a axis_req <<<"${scenario_req_axis[$scenario]}"
    for label in "${labels[@]}"; do
      for profile in "${link_profiles[@]}"; do
        for request in "${axis_req[@]}"; do
          title="\`$scenario\` / $label / $profile"
          if [[ "$request" != default ]]; then
            title="$title / request_concurrency $request"
          fi
          echo "#### $title"
          echo
          printf '| connections \\ concurrency |'
          for conc in "${axis_conc[@]}"; do printf ' %s |' "$conc"; done
          printf '\n|---|'
          # "%s": bash's printf reads a leading "-" in the format as an option.
          for _ in "${axis_conc[@]}"; do printf '%s' '---|'; done
          printf '\n'
          for conns in "${axis_conn[@]}"; do
            printf '| %s |' "$conns"
            for conc in "${axis_conc[@]}"; do
              req_json=null
              if [[ "$request" != default ]]; then req_json=$request; fi
              cell=$(jq -r --arg s "$scenario" --arg l "$label" --arg p "$profile" \
                --argjson conns "$conns" --argjson conc "$conc" \
                --argjson req "$req_json" '
              [.cells[] | select(.scenario == $s and .label == $l and .link_profile == $p
                 and .connections == $conns
                 and .concurrency == $conc and .request_concurrency == $req)] | first
              | if . == null then "-"
                else "\(.median_ms) ms<br>\(.mib_per_s) MiB/s"
                  + (if .connections_refused > 0 then "<br>\(.connections_refused) refused" else "" end)
                end' "$OUT_DIR/matrix.json")
              printf ' %s |' "$cell"
            done
            printf '\n'
          done
          echo
        done
      done
    done
  done

  echo "### Best cell per scenario, build and link profile"
  echo
  echo "| Scenario | Build | Profile | connections | concurrency | request_concurrency | Median | MiB/s | files/s | On an axis edge |"
  echo "|---|---|---|---|---|---|---|---|---|---|"
  jq -r '.scaling[] | "| \(.scenario) | \(.label) | \(.link_profile) | \(.best.connections) | \(.best.concurrency) | \(.best.request_concurrency // "default") | \(.best.median_ms) ms | \(.best.mib_per_s) | \(.best.files_per_s) | \(if (.best_at_axis_max | length) == 0 then "no" else (.best_at_axis_max | join(", ")) end) |"' \
    "$OUT_DIR/matrix.json"
  echo
  # The check the auto-config work of issue #184 phase 5 rests on: a best cell
  # sitting on the largest value of an axis is a cut-off, not an optimum, and
  # anything fitted to it extrapolates. Only the upper edge is reported; the
  # lower one is 1 and there is nothing below it to sweep.
  boundary=$(jq -r '[.scaling[] | select((.best_at_axis_max | length) > 0)
    | "\(.scenario)/\(.label)/\(.link_profile): \(.best_at_axis_max | join(", "))"] | join("; ")' \
    "$OUT_DIR/matrix.json")
  if [[ -n "$boundary" ]]; then
    echo "**The optimum sits on the edge of the grid** for $boundary. The best value measured is the largest one swept, so the real optimum is at or beyond it and this sweep does not contain it. Extend that axis before fitting anything to these numbers."
  else
    echo "Every best cell is interior to its axes, so each optimum was measured rather than cut off."
  fi

  if [[ -n "$BASELINE_BIN" ]]; then
    echo
    echo "### Candidate against baseline, worst and best cell"
    echo
    echo "| Scenario | Profile | connections | concurrency | Candidate | Baseline | Delta |"
    echo "|---|---|---|---|---|---|---|"
    # Grouped per profile as well: the worst cell of one profile and the best of
    # another are not two ends of one distribution.
    jq -r '.comparison | group_by([.link_profile, .scenario])
      | map(sort_by(.delta_percent // 0) | [.[0], .[-1]]) | flatten | unique_by([.link_profile, .scenario, .connections, .concurrency])
      | .[] | "| \(.scenario) | \(.link_profile) | \(.connections) | \(.concurrency) | \(.median_ms) ms | \(.reference_median_ms) ms | \(if .delta_percent == null then "-" else (if .delta_percent > 0 then "+" else "" end) + (.delta_percent | tostring) + "%" end) |"' \
      "$OUT_DIR/matrix.json"
  fi

  echo
  echo "### What \`auto\` costs (policy regret)"
  echo
  echo "One run per scenario and profile with \`connections\`, \`concurrency\` and \`request_concurrency\` all set to \`auto\`, on the candidate build, measured next to the cells it is scored against. \`Picked\` is read out of the run's own counters, so it is what easySFTP did and not what this script assumes; \`Best\` is the fastest cell of the same scenario and profile. \`Regret\` is the gap between them: how much slower the policy is than the settings a sweep would have chosen. A policy within ~15% on every profile is defensible, one that only wins on the house line is not (issue #184, phase 5; the policy itself is #156)."
  echo
  echo "| Scenario | Profile | Picked (conn/conc/req) | auto | Best cell | Best | Regret | Same cell in grid |"
  echo "|---|---|---|---|---|---|---|---|"
  jq -r '
    def cell($x): if $x == null then "-" else "\($x.connections)/\($x.concurrency)/\($x.request_concurrency // "default")" end;
    .auto[]
    | "| \(.scenario) | \(.link_profile) | \(.chosen.connections)/\(.chosen.concurrency)/\(.chosen.request_concurrency) "
      + "| \(.median_ms) ms | \(cell(.best)) | \(if .best == null then "-" else "\(.best.median_ms) ms" end) "
      + "| \(if .regret_percent == null then "-" else (if .regret_percent > 0 then "+" else "" end) + (.regret_percent | tostring) + "%" end) "
      + "| \(if .chosen_in_grid then "\(.chosen_cell_median_ms) ms" else "not swept" end) |"
  ' "$OUT_DIR/matrix.json"
  echo
  echo "The last column is the same coordinates measured as an ordinary cell. It is a control, not a result: a large gap between it and the \`auto\` column means the two runs saw different conditions, and then the regret next to it is drift rather than policy. Where it says \"not swept\", the settings easySFTP picked are not on this grid at all."

  echo
  echo "### Canary"
  echo
  echo "One fixed cell (\`$CANARY_SCENARIO\`, connections $CANARY_CONNECTIONS, concurrency $CANARY_CONCURRENCY, candidate build) measured at the start, the middle and the end of each profile's grid. Spread is the gap between the fastest and the slowest of them. A spread larger than the deltas below it means the server or the line moved during the sweep, and the whole run is a poor comparison basis."
  echo
  echo "| Profile | Start | Middle | End | Spread |"
  echo "|---|---|---|---|---|"
  jq -r '
    def pick($a): [.[] | select(.at == $a) | .duration_ms] | first;
    def ms: if . == null then "-" else "\(.) ms" end;
    .canary | group_by(.link_profile)[]
    | (map(.duration_ms)) as $d
    | "| \(.[0].link_profile) | \(pick("start") | ms) | \(pick("mid") | ms) | \(pick("end") | ms) "
      + "| \(if ($d | min) > 0 then ((($d | max) - ($d | min)) / ($d | min) * 100 | round | tostring) + "%" else "-" end) |"
  ' "$OUT_DIR/matrix.json"

  echo
  echo "### Delete sweeps"
  echo
  echo "Every cell's pre-clean wipes the tree the cell before it left behind, at that cell's own \`connections\`/\`concurrency\`. It has always run and cost nothing extra; what is new is that it is measured (issue #184, phase 4). Cells whose pre-clean found an empty directory are not listed. Read the two columns on the right against #157: deletions are one round-trip per entry and the pool has nowhere to spread them."
  echo
  echo "<details><summary>Per-cell delete sweeps</summary>"
  echo
  echo "| Scenario | Build | Profile | conn | conc | Files deleted | Median | files/s | remote_scan | delete_sweep | sftp_remove p50 | sftp_rmdir p50 |"
  echo "|---|---|---|---|---|---|---|---|---|---|---|---|"
  jq -r '
    def phase($n): [.phases[] | select(.name == $n) | .median_ms] | first;
    def op($n): [.operations[] | select(.name == $n) | .p50_ms] | first;
    def ms: if . == null then "-" else "\(.) ms" end;
    .deletes[]
    | "| \(.scenario) | \(.label) | \(.link_profile) | \(.connections) | \(.concurrency) "
      + "| \(.files_deleted) | \(.median_ms) ms | \(.deletes_per_s) "
      + "| \(phase("remote_scan") | ms) | \(phase("delete_sweep") | ms) "
      + "| \(op("sftp_remove") | ms) | \(op("sftp_rmdir") | ms) |"
  ' "$OUT_DIR/matrix.json"
  echo
  echo "</details>"

  refused=$(jq '[.cells[].connections_refused] | add // 0' "$OUT_DIR/matrix.json")
  if [[ "$refused" != 0 ]]; then
    echo
    echo "**$refused connection(s) were refused by the server** across the sweep and fell back to the run's first connection. Those cells had fewer connections than configured, which is the server's limit showing up in the data, not easySFTP's."
  fi
  echo
  echo "Raw data: \`matrix.json\` (every cell with its own phases and round-trip percentiles, plus the pre-grouped \`scaling\` view, the \`auto\` policy runs, the \`canary\` runs and the \`deletes\` sweeps), \`matrix.csv\` (one flat row per cell). Data only: nothing here fails a build."
} >"$OUT_DIR/matrix.md"
}

if [[ "$(jq -r '.kind' "$MANIFEST")" == matrix ]]; then
  aggregate_matrix
else
  aggregate_standard
fi
