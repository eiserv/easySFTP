#!/usr/bin/env bash
#
# easySFTP connections/concurrency matrix benchmark.
#
# scripts/benchmark.sh answers "is this build faster at the default settings".
# This one answers a different question: how does a code change move the whole
# scaling curve. It sweeps advanced.connections against advanced.concurrency
# (optionally advanced.request_concurrency as a third axis) and records one
# cell per combination, so the result can be read as a heatmap or as a scaling
# curve rather than as a single number.
#
# Candidate and baseline are measured back to back *within* each cell, not as
# two separate sweeps: a shared host drifts over the hour such a sweep takes,
# and a per-sweep layout would charge all of that drift to whichever build ran
# second.
#
# Environment (everything scripts/benchmark.sh reads, plus):
#   MATRIX_CONNECTIONS          advanced.connections values (default "1 2 4 8")
#   MATRIX_CONCURRENCY          advanced.concurrency values (default "1 2 4 8 16")
#   MATRIX_REQUEST_CONCURRENCY  optional third axis; empty (default) leaves
#                               advanced.request_concurrency at easySFTP's own
#                               default and keeps the grid two-dimensional
#   MATRIX_SCENARIOS            default "small large single"
#   REPEATS                     repeats per cell (default 1)
#
# Cost warning: the grid is measured in full. The default 4x5 grid over three
# scenarios and two builds is 120 measured runs plus 120 unmeasured pre-cleans.
# The script prints its own run count before it starts; shrink the axes rather
# than letting a job time out halfway.

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=scripts/benchmark-lib.sh disable=SC1091
source "$script_dir/benchmark-lib.sh"

require_env CANDIDATE_BIN CANDIDATE_REF OUT_DIR DATASET_DIR LOG_DIR \
  REMOTE_BASE BENCH_HOST BENCH_USERNAME BENCH_PASSWORD BENCH_KNOWN_HOSTS
require_tools jq nproc

REPEATS=${REPEATS:-1}
BENCH_PORT=${BENCH_PORT:-22}
BASELINE_BIN=${BASELINE_BIN:-}
BASELINE_REF=${BASELINE_REF:-}
MATRIX_CONNECTIONS=${MATRIX_CONNECTIONS:-"1 2 4 8"}
MATRIX_CONCURRENCY=${MATRIX_CONCURRENCY:-"1 2 4 8 16"}
MATRIX_REQUEST_CONCURRENCY=${MATRIX_REQUEST_CONCURRENCY:-}
MATRIX_SCENARIOS=${MATRIX_SCENARIOS:-"small large single"}

read -r -a connections_axis <<<"$MATRIX_CONNECTIONS"
read -r -a concurrency_axis <<<"$MATRIX_CONCURRENCY"
read -r -a request_axis <<<"$MATRIX_REQUEST_CONCURRENCY"
read -r -a scenarios <<<"$MATRIX_SCENARIOS"
# An empty third axis means "one pass at easySFTP's own default", which is what
# the empty string stands for in the loop below.
if ((${#request_axis[@]} == 0)); then
  request_axis=("")
fi

if [[ ! "$REPEATS" =~ ^[1-9][0-9]*$ ]]; then
  echo "::error::REPEATS must be a positive integer, got '$REPEATS'" >&2
  exit 1
fi
for value in "${connections_axis[@]}" "${concurrency_axis[@]}" "${request_axis[@]}"; do
  if [[ -n "$value" && ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "::error::matrix axis values must be positive integers, got '$value'" >&2
    exit 1
  fi
done
for scenario in "${scenarios[@]}"; do
  scenario_spec "$scenario" >/dev/null
done

check_remote_base
mkdir -p "$OUT_DIR" "$DATASET_DIR" "$LOG_DIR"
check_log_dir

runs_file="$LOG_DIR/matrix-runs.jsonl"
: >"$runs_file"

labels=(candidate)
binaries=("$CANDIDATE_BIN")
refs=("$CANDIDATE_REF")
if [[ -n "$BASELINE_BIN" ]]; then
  labels+=(baseline)
  binaries+=("$BASELINE_BIN")
  refs+=("${BASELINE_REF:-unknown}")
fi
reference_label=candidate
if [[ -n "$BASELINE_BIN" ]]; then
  reference_label=baseline
fi

total=$((${#scenarios[@]} * ${#connections_axis[@]} * ${#concurrency_axis[@]} * ${#request_axis[@]} * ${#labels[@]} * REPEATS))
echo "matrix: ${#scenarios[@]} scenario(s) x ${#connections_axis[@]} connection value(s) x ${#concurrency_axis[@]} concurrency value(s) x ${#request_axis[@]} request-concurrency value(s) x ${#labels[@]} build(s) x $REPEATS repeat(s) = $total measured run(s)"

generate_dataset "${scenarios[@]}"

# measure_cell <label> <binary> <ref> <scenario> <connections> <concurrency> <request> <repeat>
#
# The remote path is per (build, scenario) rather than per cell: every run is
# preceded by an unmeasured clean anyway, so a path per cell would only leave
# more empty directories behind on the server.
measure_cell() {
  local label=$1 binary=$2 ref=$3 scenario=$4 conns=$5 conc=$6 request=$7 repeat=$8
  local remote="$REMOTE_BASE/matrix/$label/$scenario"
  local stem="$LOG_DIR/$label-$scenario-c$conns-w$conc-r${request:-default}-$repeat"
  local code=0

  local advanced="connections: $conns
concurrency: $conc"
  if [[ -n "$request" ]]; then
    advanced="$advanced
request_concurrency: $request"
  fi

  if ! METRICS_FILE= run_easysftp "$binary" "$DATASET_DIR/empty" "$remote" clean \
    "$stem.clean.log" "$stem.clean.out" "$advanced"; then
    echo "::warning::pre-clean of $label/$scenario c$conns/w$conc repeat $repeat failed"
    cat "$stem.clean.log"
  fi

  METRICS_FILE="$stem.metrics.json" \
    run_easysftp "$binary" "$DATASET_DIR/$scenario" "$remote" overlay "$stem.log" "$stem.out" "$advanced" || code=$?
  if ((code != 0)); then
    # Into the job log, which masks secrets. Never into the artifact.
    echo "::warning::$label/$scenario c$conns/w$conc repeat $repeat exited $code"
    cat "$stem.log"
  fi

  jq -nc \
    --arg label "$label" \
    --arg ref "$ref" \
    --arg scenario "$scenario" \
    --argjson connections "$conns" \
    --argjson concurrency "$conc" \
    --argjson request_concurrency "${request:-null}" \
    --argjson repeat "$repeat" \
    --argjson exit_code "$code" \
    --argjson duration_ms "$(step_number "$stem.out" duration-ms)" \
    --argjson files "$(step_number "$stem.out" files-uploaded)" \
    --argjson bytes "$(step_number "$stem.out" bytes-uploaded)" \
    --argjson retries "$(count_matches "$stem.log" -e retrying -e reconnecting)" \
    --argjson errors "$(count_matches "$stem.log" '^::error::')" \
    --argjson metrics "$(metrics_json "$stem.metrics.json")" \
    '$ARGS.named' >>"$runs_file"

  echo "$label/$scenario connections=$conns concurrency=$conc request=${request:-default} repeat $repeat: $(step_number "$stem.out" duration-ms) ms, exit $code"
}

for scenario in "${scenarios[@]}"; do
  for ((repeat = 1; repeat <= REPEATS; repeat++)); do
    for conns in "${connections_axis[@]}"; do
      for conc in "${concurrency_axis[@]}"; do
        for request in "${request_axis[@]}"; do
          # Innermost, so candidate and baseline of one cell are measured
          # within seconds of each other.
          for i in "${!labels[@]}"; do
            measure_cell "${labels[$i]}" "${binaries[$i]}" "${refs[$i]}" \
              "$scenario" "$conns" "$conc" "$request" "$repeat"
          done
        done
      done
    done
  done
done

# Leave the server as we found it. Best effort: a cleanup hiccup must not hide
# the results.
for scenario in "${scenarios[@]}"; do
  for i in "${!labels[@]}"; do
    stem="$LOG_DIR/cleanup-${labels[$i]}-$scenario"
    METRICS_FILE= run_easysftp "${binaries[$i]}" "$DATASET_DIR/empty" \
      "$REMOTE_BASE/matrix/${labels[$i]}/$scenario" clean "$stem.log" "$stem.out" "" ||
      echo "::warning::cleanup of ${labels[$i]}/$scenario failed"
  done
done

cells_file="$LOG_DIR/cells.json"
jq -s "
  $JQ_STATS
  group_by([.label, .scenario, .connections, .concurrency, .request_concurrency])
  | map(
      (map(select(.metrics != null)) | map(.metrics)) as \$m
      | {
        scenario: .[0].scenario,
        label: .[0].label,
        ref: .[0].ref,
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
        connections_refused: (\$m | map(.counters.connections_refused // 0) | add),
        reconnects: (\$m | map(.counters.reconnects // 0) | add),
        upload_phase_ms: (\$m | map(.phases // []) | add // []
          | map(select(.name == \"upload\") | .wall_ms) | median)
      })
  | map(. + {
      mib_per_s: (if .median_ms > 0 then ((.bytes / 1048576) / (.median_ms / 1000) | round2) else 0 end),
      files_per_s: (if .median_ms > 0 then (.files / (.median_ms / 1000) | round2) else 0 end)
    })
  | sort_by([.scenario, .label, .connections, .concurrency, .request_concurrency])
" "$runs_file" >"$cells_file"

settings="matrix sweep; every other advanced.* setting stays at easySFTP's defaults (retries 2, timeout 30s, mode overlay)"

scenario_docs=$(for scenario in "${scenarios[@]}"; do
  jq -nc --arg k "$scenario" --arg v "$(scenario_description "$scenario")" '{key: $k, value: $v}'
done | jq -s 'from_entries')

# matrix.json. "axes" declares the grid explicitly so a heatmap does not have to
# infer it from the cells that happen to be present, "cells" is one row per
# combination, "scaling" is the same data pre-grouped into the curve a reader
# usually wants (per scenario and build, ordered by connections then
# concurrency), and "comparison" pairs each candidate cell with the reference
# build's cell at the same coordinates.
jq -n \
  --slurpfile cells "$cells_file" \
  --arg candidate_ref "$CANDIDATE_REF" \
  --arg baseline_ref "$BASELINE_REF" \
  --arg reference_label "$reference_label" \
  --argjson repeats "$REPEATS" \
  --arg runner "${RUNNER_ENVIRONMENT:-local}, $(uname -sr), $(nproc) cpu" \
  --argjson environment "$(bench_environment)" \
  --arg settings "$settings" \
  --argjson scenarios "$scenario_docs" \
  --argjson connections_axis "$(printf '%s\n' "${connections_axis[@]}" | jq -s '.')" \
  --argjson concurrency_axis "$(printf '%s\n' "${concurrency_axis[@]}" | jq -s '.')" \
  --argjson request_axis "$(printf '%s\n' "${request_axis[@]}" | jq -Rs 'split("\n") | map(select(. != "") | tonumber)')" \
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
     settings: \$settings,
     scenarios: \$scenarios,
     note: \"one cell per (scenario, build, connections, concurrency, request_concurrency); median_ms is wall clock over the cell's repeats\",
     axes: {
       connections: \$connections_axis,
       concurrency: \$concurrency_axis,
       request_concurrency: \$request_axis
     },
     cells: \$c,
     scaling: (\$c | group_by([.scenario, .label])
       | map({
           scenario: .[0].scenario,
           label: .[0].label,
           points: (sort_by([.connections, .concurrency])
             | map({connections, concurrency, request_concurrency, median_ms, mib_per_s, files_per_s,
                    connections_used, connections_refused, max_rss_bytes, user_cpu_ms})),
           best: (sort_by(.median_ms) | .[0]
             | {connections, concurrency, request_concurrency, median_ms, mib_per_s, files_per_s})
         })),
     comparison: [
       \$c[] | select(.label != \$reference_label) as \$x
       | (\$c[] | select(.label == \$reference_label and .scenario == \$x.scenario
            and .connections == \$x.connections and .concurrency == \$x.concurrency
            and .request_concurrency == \$x.request_concurrency)) as \$b
       | {
           scenario: \$x.scenario, label: \$x.label, reference_label: \$reference_label,
           connections: \$x.connections, concurrency: \$x.concurrency,
           request_concurrency: \$x.request_concurrency,
           median_ms: \$x.median_ms, reference_median_ms: \$b.median_ms,
           delta_ms: (\$x.median_ms - \$b.median_ms),
           delta_percent: pct(\$x.median_ms; \$b.median_ms)
         }
     ]
   }" >"$OUT_DIR/matrix.json"

# One flat row per cell: this is the file a heatmap or a scaling plot reads.
jq -r '
  ["scenario","build","ref","connections","concurrency","request_concurrency","repeats",
   "files","bytes","median_ms","min_ms","max_ms","mad_ms","mib_per_s","files_per_s",
   "upload_phase_ms","user_cpu_ms","sys_cpu_ms","cpu_percent","max_rss_bytes","go_gc_count",
   "go_peak_goroutines","connections_opened","connections_used","connections_refused",
   "reconnects","retries","errors","failed_runs"],
  (.cells[] | [
    .scenario, .label, .ref, .connections, .concurrency, .request_concurrency, .repeats,
    .files, .bytes, .median_ms, .min_ms, .max_ms, .mad_ms, .mib_per_s, .files_per_s,
    .upload_phase_ms, .user_cpu_ms, .sys_cpu_ms, .cpu_percent, .max_rss_bytes, .go_gc_count,
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
  echo "| Runner | $(uname -sr), $(nproc) cpu |"
  echo "| connections | $MATRIX_CONNECTIONS |"
  echo "| concurrency | $MATRIX_CONCURRENCY |"
  echo "| request_concurrency | ${MATRIX_REQUEST_CONCURRENCY:-easySFTP default} |"
  echo "| Scenarios | ${scenarios[*]} |"
  echo
  echo "Each grid below is median wall-clock milliseconds: rows are \`advanced.connections\`, columns are \`advanced.concurrency\`. Lower is better. \`connections > concurrency\` is measured, not skipped; easySFTP caps the pool at the concurrency (a connection no worker picks is a handshake for nothing), so those cells are expected to flatten out rather than improve."
  echo

  for scenario in "${scenarios[@]}"; do
    for label in "${labels[@]}"; do
      for request in "${request_axis[@]}"; do
        title="\`$scenario\` / $label"
        if [[ -n "$request" ]]; then
          title="$title / request_concurrency $request"
        fi
        echo "#### $title"
        echo
        printf '| connections \\ concurrency |'
        for conc in "${concurrency_axis[@]}"; do printf ' %s |' "$conc"; done
        printf '\n|---|'
        # "%s": bash's printf reads a leading "-" in the format as an option.
        for _ in "${concurrency_axis[@]}"; do printf '%s' '---|'; done
        printf '\n'
        for conns in "${connections_axis[@]}"; do
          printf '| %s |' "$conns"
          for conc in "${concurrency_axis[@]}"; do
            cell=$(jq -r --arg s "$scenario" --arg l "$label" \
              --argjson conns "$conns" --argjson conc "$conc" \
              --argjson req "${request:-null}" '
              [.cells[] | select(.scenario == $s and .label == $l and .connections == $conns
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

  echo "### Best cell per scenario and build"
  echo
  echo "| Scenario | Build | connections | concurrency | request_concurrency | Median | MiB/s | files/s |"
  echo "|---|---|---|---|---|---|---|---|"
  jq -r '.scaling[] | "| \(.scenario) | \(.label) | \(.best.connections) | \(.best.concurrency) | \(.best.request_concurrency // "default") | \(.best.median_ms) ms | \(.best.mib_per_s) | \(.best.files_per_s) |"' \
    "$OUT_DIR/matrix.json"

  if [[ -n "$BASELINE_BIN" ]]; then
    echo
    echo "### Candidate against baseline, worst and best cell"
    echo
    echo "| Scenario | connections | concurrency | Candidate | Baseline | Delta |"
    echo "|---|---|---|---|---|---|"
    jq -r '.comparison | group_by(.scenario)
      | map(sort_by(.delta_percent // 0) | [.[0], .[-1]]) | flatten | unique_by([.scenario, .connections, .concurrency])
      | .[] | "| \(.scenario) | \(.connections) | \(.concurrency) | \(.median_ms) ms | \(.reference_median_ms) ms | \(if .delta_percent == null then "-" else (if .delta_percent > 0 then "+" else "" end) + (.delta_percent | tostring) + "%" end) |"' \
      "$OUT_DIR/matrix.json"
  fi

  refused=$(jq '[.cells[].connections_refused] | add // 0' "$OUT_DIR/matrix.json")
  if [[ "$refused" != 0 ]]; then
    echo
    echo "**$refused connection(s) were refused by the server** across the sweep and fell back to the run's first connection. Those cells had fewer connections than configured, which is the server's limit showing up in the data, not easySFTP's."
  fi
  echo
  echo "Raw data: \`matrix.json\` (every cell, plus the pre-grouped \`scaling\` view), \`matrix.csv\` (one flat row per cell). Data only: nothing here fails a build."
} >"$OUT_DIR/matrix.md"

cat "$OUT_DIR/matrix.md"
