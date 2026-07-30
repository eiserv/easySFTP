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
#   MATRIX_SCENARIOS            default "small large single". Beyond those:
#                               "mixed", plus the deploy shapes of issue #184
#                               phase 3, "redeploy", "sync", "deep", "bulk" and
#                               the "calib-<count>x<size>" family (for example
#                               "calib-100x64k"). See scenario_shape in
#                               scripts/benchmark-lib.sh: a scenario carries a
#                               mode and a layout, not only a payload
#   MATRIX_LINK_PROFILES        optional network profiles to sweep over (issue
#                               #184); empty means the real line only. See
#                               scripts/benchmark-link.sh for the grammar. This
#                               axis multiplies the whole grid, so four profiles
#                               are four times the hours
#   LINKPROBE_BIN               optional built cmd/linkprobe; without it the
#                               result carries an empty probe list
#   LINK_IFACE, LINK_SUDO       see scripts/benchmark-link.sh
#   REPEATS                     repeats per cell (default 1)
#
# Cost warning: the grid is measured in full. The default 4x5 grid over three
# scenarios and two builds is 120 measured runs plus 120 unmeasured pre-cleans.
# The script prints its own run count before it starts; shrink the axes rather
# than letting a job time out halfway.
#
# On top of the grid, three canary runs per link profile (issue #184, phase 2):
# one fixed cell measured at the start, the middle and the end of that profile's
# grid. A sweep takes hours against a server that is fixed but not constant, and
# three values that disagree are the only signal that the whole run is not a
# comparison basis.

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=scripts/benchmark-lib.sh disable=SC1091
source "$script_dir/benchmark-lib.sh"
# shellcheck source=scripts/benchmark-link.sh disable=SC1091
source "$script_dir/benchmark-link.sh"

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
MATRIX_LINK_PROFILES=${MATRIX_LINK_PROFILES:-}

# The canary cell. Deliberately constants and not environment: three canaries of
# one run are compared against each other, and two runs' canaries against each
# other, which only works while the cell stays the same everywhere.
CANARY_SCENARIO=small
CANARY_CONNECTIONS=1
CANARY_CONCURRENCY=4

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
# Through a command substitution and not a process substitution, so a rejected
# profile stops the sweep under "set -e" instead of hours later.
link_profiles_raw=$(link_parse_profiles "$MATRIX_LINK_PROFILES")
mapfile -t link_profiles <<<"$link_profiles_raw"
LINK_REQUESTED=("${link_profiles[@]}")

check_remote_base
mkdir -p "$OUT_DIR" "$DATASET_DIR" "$LOG_DIR"
check_log_dir

runs_file="$LOG_DIR/matrix-runs.jsonl"
probes_file="$LOG_DIR/link-probes.jsonl"
canary_file="$LOG_DIR/canary.jsonl"
deletes_file="$LOG_DIR/deletes.jsonl"
: >"$runs_file"
: >"$probes_file"
: >"$canary_file"
: >"$deletes_file"

# The profile every cell below is measured on; set by the profile loop.
link_profile=baseline

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

# Cells of one profile's grid, which is also what the middle canary counts to.
cells_per_profile=$((${#scenarios[@]} * ${#connections_axis[@]} * ${#concurrency_axis[@]} * ${#request_axis[@]} * ${#labels[@]} * REPEATS))
total=$((cells_per_profile * ${#link_profiles[@]}))
canary_total=$((3 * ${#link_profiles[@]}))
echo "matrix: ${#scenarios[@]} scenario(s) x ${#connections_axis[@]} connection value(s) x ${#concurrency_axis[@]} concurrency value(s) x ${#request_axis[@]} request-concurrency value(s) x ${#link_profiles[@]} link profile(s) x ${#labels[@]} build(s) x $REPEATS repeat(s) = $total measured run(s), plus up to $canary_total canary run(s)"

# A prepopulated scenario deploys its tree twice per cell, and only the second
# one is measured. Worth saying before the hours start rather than after.
prepopulated=()
for scenario in "${scenarios[@]}"; do
  read -r _ prepopulate _ <<<"$(scenario_shape "$scenario")"
  if ((prepopulate)); then
    prepopulated+=("$scenario")
  fi
done
if ((${#prepopulated[@]} > 0)); then
  echo "matrix: ${prepopulated[*]} are redeploy scenarios; each of their cells runs an unmeasured full deploy first, so those cells cost roughly twice their measured time"
fi

# The canary payload has to exist even when its scenario is not swept.
dataset_scenarios=("${scenarios[@]}")
if [[ " ${scenarios[*]} " != *" $CANARY_SCENARIO "* ]]; then
  dataset_scenarios+=("$CANARY_SCENARIO")
fi
generate_dataset "${dataset_scenarios[@]}"

# measure_cell <label> <binary> <ref> <scenario> <connections> <concurrency> <request> <repeat>
#
# The remote path is per (build, scenario) rather than per cell: every run is
# preceded by an unmeasured clean anyway, so a path per cell would only leave
# more empty directories behind on the server.
#
# What is measured is the scenario's mode, not always overlay, and a
# prepopulated scenario gets an unmeasured full deploy plus a small local change
# in between (issue #184, phase 3). Both come from scenario_shape.
measure_cell() {
  local label=$1 binary=$2 ref=$3 scenario=$4 conns=$5 conc=$6 request=$7 repeat=$8
  local remote="$REMOTE_BASE/matrix/$label/$scenario"
  local slug
  slug=$(link_profile_slug "$link_profile")
  local stem="$LOG_DIR/$slug-$label-$scenario-c$conns-w$conc-r${request:-default}-$repeat"
  local code=0
  local mode prepopulate
  read -r mode prepopulate _ <<<"$(scenario_shape "$scenario")"

  local advanced="connections: $conns
concurrency: $conc"
  if [[ -n "$request" ]]; then
    advanced="$advanced
request_concurrency: $request"
  fi
  # Kept off the pre-clean: skip_unchanged applies to overlay only, and a clean
  # deployment carrying it just logs a warning about being ignored.
  local deploy_advanced=$advanced
  if ((prepopulate)) && [[ "$mode" == overlay ]]; then
    deploy_advanced="$advanced
skip_unchanged: true"
  fi

  # The pre-clean wipes what the previous cell of this (build, scenario) left
  # behind, which makes it a pure delete sweep at this cell's own settings. It is
  # instrumented into its own file and aggregated into "deletes" (issue #184,
  # phase 4), never into the cell: no extra run, no extra minute, and the only
  # measurement of deletions there is.
  local clean_code=0
  METRICS_FILE="$stem.clean.metrics.json" \
    run_easysftp "$binary" "$DATASET_DIR/empty" "$remote" clean \
    "$stem.clean.log" "$stem.clean.out" "$advanced" || clean_code=$?
  if ((clean_code != 0)); then
    echo "::warning::pre-clean of $label/$scenario c$conns/w$conc repeat $repeat exited $clean_code"
    cat "$stem.clean.log"
  fi
  jq -c \
    --arg label "$label" \
    --arg scenario "$scenario" \
    --arg link_profile "$link_profile" \
    --argjson connections "$conns" \
    --argjson concurrency "$conc" \
    --argjson request_concurrency "${request:-null}" \
    --argjson repeat "$repeat" \
    '. + $ARGS.named' <<<"$(delete_json "$stem" "$clean_code")" >>"$deletes_file"

  # The deploy the measured run redeploys over. Unmeasured, with the cell's own
  # settings: what is under test is the second run, and a base laid down at
  # other settings would put a different remote tree under each cell.
  if ((prepopulate)); then
    if ! METRICS_FILE='' run_easysftp "$binary" "$DATASET_DIR/$scenario" "$remote" "$mode" \
      "$stem.base.log" "$stem.base.out" "$deploy_advanced"; then
      echo "::warning::base deploy of $label/$scenario c$conns/w$conc repeat $repeat failed"
      cat "$stem.base.log"
    fi
    scenario_mutate "$DATASET_DIR/$scenario" "$SCENARIO_CHANGED_FILES"
  fi

  METRICS_FILE="$stem.metrics.json" \
    run_easysftp "$binary" "$DATASET_DIR/$scenario" "$remote" "$mode" "$stem.log" "$stem.out" "$deploy_advanced" || code=$?
  if ((code != 0)); then
    # Into the job log, which masks secrets. Never into the artifact.
    echo "::warning::$label/$scenario c$conns/w$conc repeat $repeat exited $code"
    cat "$stem.log"
  fi

  jq -nc \
    --arg label "$label" \
    --arg ref "$ref" \
    --arg scenario "$scenario" \
    --arg link_profile "$link_profile" \
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

  echo "$link_profile $label/$scenario connections=$conns concurrency=$conc request=${request:-default} repeat $repeat: $(step_number "$stem.out" duration-ms) ms, exit $code"
}

# measure_canary <at>: the fixed cell, always the candidate build, run three
# times per profile ("start", "mid", "end"). Its numbers are kept apart from
# cells[] on purpose: they measure the server's steadiness, not a coordinate of
# the grid, and mixing them into an aggregate would hide exactly what they are
# there to show.
measure_canary() {
  local at=$1 slug stem code=0
  slug=$(link_profile_slug "$link_profile")
  stem="$LOG_DIR/canary-$slug-$at"
  local remote="$REMOTE_BASE/matrix/canary"
  local advanced="connections: $CANARY_CONNECTIONS
concurrency: $CANARY_CONCURRENCY"

  if ! METRICS_FILE='' run_easysftp "$CANDIDATE_BIN" "$DATASET_DIR/empty" "$remote" clean \
    "$stem.clean.log" "$stem.clean.out" "$advanced"; then
    echo "::warning::pre-clean of the $at canary on $link_profile failed"
  fi

  METRICS_FILE='' run_easysftp "$CANDIDATE_BIN" "$DATASET_DIR/$CANARY_SCENARIO" "$remote" \
    overlay "$stem.log" "$stem.out" "$advanced" || code=$?
  if ((code != 0)); then
    echo "::warning::the $at canary on $link_profile exited $code"
  fi

  jq -nc \
    --arg link_profile "$link_profile" \
    --arg at "$at" \
    --arg scenario "$CANARY_SCENARIO" \
    --argjson connections "$CANARY_CONNECTIONS" \
    --argjson concurrency "$CANARY_CONCURRENCY" \
    --argjson exit_code "$code" \
    --argjson duration_ms "$(step_number "$stem.out" duration-ms)" \
    --argjson files "$(step_number "$stem.out" files-uploaded)" \
    --argjson bytes "$(step_number "$stem.out" bytes-uploaded)" \
    '$ARGS.named' >>"$canary_file"

  echo "$link_profile canary $at: $(step_number "$stem.out" duration-ms) ms, exit $code"
}

# Shaping is only probed for when a profile actually asks for it, so a sweep of
# the real line needs neither tc nor NET_ADMIN.
if link_shape_needed "${link_profiles[@]}"; then
  link_shape_probe
  if ((LINK_SHAPING_AVAILABLE != 1)); then
    echo "::warning::link profiles were requested but shaping is unavailable ($LINK_SHAPING_REASON); every profile is swept on the real line" >&2
  fi
fi

# The profile loop is the outermost one: re-applying tc per cell would itself be
# noise, and a sweep already takes hours. Each profile is probed before and
# after its own grid, so drift inside a profile is visible; two probes of
# different profiles are not comparable and are not meant to be.
for link_profile in "${link_profiles[@]}"; do
  link_shape_apply "$link_profile" || true
  link_probe "$link_profile" start "$probes_file"
  measure_canary start
  # Counted in measured runs rather than in wall clock, so the middle canary
  # sits in the middle of the work and not of an estimate. A grid of one cell
  # has no middle, and then there are two canaries instead of three.
  runs_done=0
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
              runs_done=$((runs_done + 1))
              if ((runs_done == cells_per_profile / 2)); then
                measure_canary mid
              fi
            done
          done
        done
      done
    done
  done
  measure_canary end
  link_probe "$link_profile" end "$probes_file"
done

# The trap does this too, but only on the way out: doing it here keeps the
# cleanup runs below on the unshaped line.
link_shape_clear

# Leave the server as we found it. Best effort: a cleanup hiccup must not hide
# the results.
for scenario in "${scenarios[@]}"; do
  for i in "${!labels[@]}"; do
    stem="$LOG_DIR/cleanup-${labels[$i]}-$scenario"
    METRICS_FILE='' run_easysftp "${binaries[$i]}" "$DATASET_DIR/empty" \
      "$REMOTE_BASE/matrix/${labels[$i]}/$scenario" clean "$stem.log" "$stem.out" "" ||
      echo "::warning::cleanup of ${labels[$i]}/$scenario failed"
  done
done
METRICS_FILE='' run_easysftp "$CANDIDATE_BIN" "$DATASET_DIR/empty" \
  "$REMOTE_BASE/matrix/canary" clean "$LOG_DIR/cleanup-canary.log" "$LOG_DIR/cleanup-canary.out" "" ||
  echo "::warning::cleanup of the canary directory failed"
# A probe that was killed mid-write leaves its payload behind, and the next
# run's remote scan would count it.
if [[ -n "${LINKPROBE_BIN:-}" ]]; then
  METRICS_FILE='' run_easysftp "$CANDIDATE_BIN" "$DATASET_DIR/empty" \
    "$REMOTE_BASE/linkprobe" clean "$LOG_DIR/cleanup-linkprobe.log" "$LOG_DIR/cleanup-linkprobe.out" "" ||
    echo "::warning::cleanup of the link probe directory failed"
fi

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

settings="matrix sweep; every other advanced.* setting stays at easySFTP's defaults (retries 2, timeout 30s). The mode belongs to the scenario, see scenarios below: a redeploy scenario is deployed once unmeasured and then measured over itself with $SCENARIO_CHANGED_FILES file(s) changed"
if [[ -n "$MATRIX_LINK_PROFILES" ]]; then
  settings="$settings; swept over the link profiles ${link_profiles[*]}"
fi

scenario_docs=$(for scenario in "${scenarios[@]}"; do
  jq -nc --arg k "$scenario" --arg v "$(scenario_description "$scenario")" '{key: $k, value: $v}'
done | jq -s 'from_entries')

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
jq -n \
  --slurpfile cells "$cells_file" \
  --arg candidate_ref "$CANDIDATE_REF" \
  --arg baseline_ref "$BASELINE_REF" \
  --arg reference_label "$reference_label" \
  --argjson repeats "$REPEATS" \
  --arg runner "${RUNNER_ENVIRONMENT:-local}, $(uname -sr), $(nproc) cpu" \
  --argjson environment "$(bench_environment)" \
  --argjson link "$(link_json "$probes_file")" \
  --argjson canary "$(jq -s '.' "$canary_file")" \
  --slurpfile deletes "$deletes_agg_file" \
  --arg settings "$settings" \
  --argjson scenarios "$scenario_docs" \
  --argjson link_axis "$(printf '%s\n' "${link_profiles[@]}" | jq -Rs 'split("\n") | map(select(. != ""))')" \
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
     link: \$link,
     canary: \$canary,
     settings: \$settings,
     scenarios: \$scenarios,
     note: \"one cell per (link_profile, scenario, build, connections, concurrency, request_concurrency); median_ms is wall clock over the cell's repeats\",
     axes: {
       link_profiles: \$link_axis,
       connections: \$connections_axis,
       concurrency: \$concurrency_axis,
       request_concurrency: \$request_axis
     },
     cells: \$c,
     deletes: \$deletes[0],
     scaling: (\$c | group_by([.link_profile, .scenario, .label])
       | map({
           scenario: .[0].scenario,
           label: .[0].label,
           link_profile: .[0].link_profile,
           points: (sort_by([.connections, .concurrency])
             | map({connections, concurrency, request_concurrency, median_ms, mib_per_s, files_per_s,
                    connections_used, connections_refused, max_rss_bytes, user_cpu_ms})),
           best: (sort_by(.median_ms) | .[0]
             | {connections, concurrency, request_concurrency, median_ms, mib_per_s, files_per_s})
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
     "connections","concurrency","request_concurrency","repeats",
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
    .connections, .concurrency, .request_concurrency, .repeats,
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
  echo "| Runner | $(uname -sr), $(nproc) cpu |"
  echo "| connections | $MATRIX_CONNECTIONS |"
  echo "| concurrency | $MATRIX_CONCURRENCY |"
  echo "| request_concurrency | ${MATRIX_REQUEST_CONCURRENCY:-easySFTP default} |"
  echo "| Link profiles | ${MATRIX_LINK_PROFILES:-the real line} |"
  echo "| Scenarios | ${scenarios[*]} |"
  echo
  # What each scenario is, next to the numbers: "sync" and "redeploy" are a
  # deploy shape and not only a payload, and a MiB/s of theirs is over the few
  # changed files rather than over the tree.
  echo "| Scenario | Mode | Payload |"
  echo "|---|---|---|"
  for scenario in "${scenarios[@]}"; do
    read -r mode prepopulate _ <<<"$(scenario_shape "$scenario")"
    if ((prepopulate)); then
      mode="$mode, redeployed"
    fi
    echo "| \`$scenario\` | $mode | $(scenario_description "$scenario") |"
  done
  echo
  link_markdown "$OUT_DIR/matrix.json"
  echo "Each grid below is median wall-clock milliseconds: rows are \`advanced.connections\`, columns are \`advanced.concurrency\`. Lower is better. \`connections > concurrency\` is measured, not skipped; easySFTP caps the pool at the concurrency (a connection no worker picks is a handshake for nothing), so those cells are expected to flatten out rather than improve."
  echo

  for scenario in "${scenarios[@]}"; do
    for label in "${labels[@]}"; do
      for profile in "${link_profiles[@]}"; do
        for request in "${request_axis[@]}"; do
          title="\`$scenario\` / $label / $profile"
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
              cell=$(jq -r --arg s "$scenario" --arg l "$label" --arg p "$profile" \
                --argjson conns "$conns" --argjson conc "$conc" \
                --argjson req "${request:-null}" '
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
  echo "| Scenario | Build | Profile | connections | concurrency | request_concurrency | Median | MiB/s | files/s |"
  echo "|---|---|---|---|---|---|---|---|---|"
  jq -r '.scaling[] | "| \(.scenario) | \(.label) | \(.link_profile) | \(.best.connections) | \(.best.concurrency) | \(.best.request_concurrency // "default") | \(.best.median_ms) ms | \(.best.mib_per_s) | \(.best.files_per_s) |"' \
    "$OUT_DIR/matrix.json"

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
  echo "Raw data: \`matrix.json\` (every cell with its own phases and round-trip percentiles, plus the pre-grouped \`scaling\` view, the \`canary\` runs and the \`deletes\` sweeps), \`matrix.csv\` (one flat row per cell). Data only: nothing here fails a build."
} >"$OUT_DIR/matrix.md"

cat "$OUT_DIR/matrix.md"
