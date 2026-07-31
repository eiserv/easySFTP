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
#   MATRIX_CONCURRENCY          advanced.concurrency values (default
#                               "1 2 4 8 16 32 64"; the stored sweeps put the
#                               optimum of every small-file scenario at 32, the
#                               edge of the old grid, so the old default could
#                               only ever report a boundary, issue #184 phase 5)
#   MATRIX_REQUEST_CONCURRENCY  advanced.request_concurrency values (default
#                               "1 16 64"; 16 is easySFTP's own value). The
#                               literal token "default" is a pass that sets
#                               nothing, so "default" alone is the old
#                               two-dimensional grid. Applied only to scenarios
#                               whose files are large enough for the setting to
#                               do anything, see scenario_sweeps_requests in
#                               scripts/benchmark-lib.sh
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
# Cost warning: the grid is measured in full, but it is measured *per scenario*.
# An axis value above a scenario's file count is the same configuration under
# another name (only the per-file upload path spreads over connections and
# workers), so axis_for_scenario caps and deduplicates both axes against the
# payload: "single" is one cell, not the 30 identical ones the stored sweeps
# hold. The script prints its own run count before it starts; shrink the axes
# rather than letting a job time out halfway.
#
# On top of the grid, three canary runs per link profile (issue #184, phase 2):
# one fixed cell measured at the start, the middle and the end of that profile's
# grid. A sweep takes hours against a server that is fixed but not constant, and
# three values that disagree are the only signal that the whole run is not a
# comparison basis.
#
# And one "auto" run per scenario, profile and repeat (issue #184, phase 5): the
# candidate build with every knob left at "auto", i.e. whatever easySFTP picks
# for itself. It is not a cell, because auto does not sit at a coordinate, it
# chooses one; what it is measured for is the *regret* of that choice, the gap
# between it and the best cell of the same scenario and profile. That number is
# what keeps #156 honest, and it is the reason the auto runs are kept out of
# cells[], scaling[] and comparison[].

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
MATRIX_CONCURRENCY=${MATRIX_CONCURRENCY:-"1 2 4 8 16 32 64"}
MATRIX_REQUEST_CONCURRENCY=${MATRIX_REQUEST_CONCURRENCY:-"1 16 64"}
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
# The third axis carries the token "default" for "set nothing and let easySFTP
# pick", so a two-dimensional grid stays expressible now that the axis has a
# real default. It travels as that token rather than as an empty string because
# it has to survive being stored in the per-scenario axis strings below.
if ((${#request_axis[@]} == 0)); then
  request_axis=(default)
fi

if [[ ! "$REPEATS" =~ ^[1-9][0-9]*$ ]]; then
  echo "::error::REPEATS must be a positive integer, got '$REPEATS'" >&2
  exit 1
fi
for value in "${connections_axis[@]}" "${concurrency_axis[@]}"; do
  if [[ ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "::error::matrix axis values must be positive integers, got '$value'" >&2
    exit 1
  fi
done
for value in "${request_axis[@]}"; do
  if [[ "$value" != default && ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "::error::request_concurrency axis values must be positive integers or the token 'default', got '$value'" >&2
    exit 1
  fi
done
for scenario in "${scenarios[@]}"; do
  scenario_spec "$scenario" >/dev/null
done

# The grid is per scenario, not global (issue #184, phase 5). Two payload facts
# decide it, and both are properties of the payload rather than of the code:
# a value above the file count cannot be used by anything (axis_for_scenario),
# and request_concurrency is per file, so a payload of 4 KiB files cannot show
# it at all (scenario_sweeps_requests). Computed once, up front, because the run
# count printed below has to be the count that is actually measured.
declare -A scenario_conn_axis scenario_conc_axis scenario_req_axis
for scenario in "${scenarios[@]}"; do
  scenario_conn_axis[$scenario]=$(axis_for_scenario "$scenario" "${connections_axis[@]}" | tr '\n' ' ')
  scenario_conc_axis[$scenario]=$(axis_for_scenario "$scenario" "${concurrency_axis[@]}" | tr '\n' ' ')
  if (($(scenario_sweeps_requests "$scenario"))); then
    scenario_req_axis[$scenario]=${request_axis[*]}
  else
    scenario_req_axis[$scenario]=default
  fi
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
auto_file="$LOG_DIR/auto.jsonl"
: >"$runs_file"
: >"$probes_file"
: >"$canary_file"
: >"$deletes_file"
: >"$auto_file"

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
# Summed per scenario, since each one has its own axes.
cells_per_profile=0
for scenario in "${scenarios[@]}"; do
  read -r -a axis_conn <<<"${scenario_conn_axis[$scenario]}"
  read -r -a axis_conc <<<"${scenario_conc_axis[$scenario]}"
  read -r -a axis_req <<<"${scenario_req_axis[$scenario]}"
  cells_per_profile=$((cells_per_profile +
    ${#axis_conn[@]} * ${#axis_conc[@]} * ${#axis_req[@]} * ${#labels[@]} * REPEATS))
  echo "matrix: $scenario ($(scenario_files "$scenario") file(s)) sweeps connections ${axis_conn[*]}, concurrency ${axis_conc[*]}, request_concurrency ${axis_req[*]}"
done
total=$((cells_per_profile * ${#link_profiles[@]}))
canary_total=$((3 * ${#link_profiles[@]}))
auto_total=$((${#scenarios[@]} * REPEATS * ${#link_profiles[@]}))
echo "matrix: ${#scenarios[@]} scenario(s) x ${#link_profiles[@]} link profile(s) x ${#labels[@]} build(s) x $REPEATS repeat(s) = $total measured run(s), plus up to $canary_total canary run(s) and $auto_total auto run(s)"

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

# measure_deploy <binary> <scenario> <remote> <stem> <advanced> <instrument-clean> <what>
#
# One deployment of a scenario, run exactly the way its shape says: a pre-clean,
# the unmeasured base deploy plus mutation a redeploy scenario needs, then the
# measured run. Shared by the cells and the auto runs, which differ only in the
# advanced block they pass and in what they do with the result.
#
# Sets MEASURE_CODE and MEASURE_CLEAN_CODE for the caller. The pre-clean is only
# instrumented where its numbers are wanted, which is the cells: an auto run's
# coordinates are not a coordinate of the grid, so a delete row of it would sit
# in deletes[] under settings nobody asked for.
measure_deploy() {
  local binary=$1 scenario=$2 remote=$3 stem=$4 advanced=$5 instrument_clean=$6 what=$7
  local mode prepopulate
  read -r mode prepopulate _ <<<"$(scenario_shape "$scenario")"

  # Kept off the pre-clean: skip_unchanged applies to overlay only, and a clean
  # deployment carrying it just logs a warning about being ignored.
  local deploy_advanced=$advanced
  if ((prepopulate)) && [[ "$mode" == overlay ]]; then
    deploy_advanced="$advanced
skip_unchanged: true"
  fi

  # The pre-clean wipes what the previous run of this (build, scenario) left
  # behind, which makes it a pure delete sweep at this run's own settings. Where
  # it is instrumented it goes into its own file and into "deletes" (issue #184,
  # phase 4), never into the cell: no extra run, no extra minute, and the only
  # measurement of deletions there is.
  MEASURE_CLEAN_CODE=0
  local clean_metrics=''
  if ((instrument_clean)); then
    clean_metrics="$stem.clean.metrics.json"
  fi
  METRICS_FILE="$clean_metrics" \
    run_easysftp "$binary" "$DATASET_DIR/empty" "$remote" clean \
    "$stem.clean.log" "$stem.clean.out" "$advanced" || MEASURE_CLEAN_CODE=$?
  if ((MEASURE_CLEAN_CODE != 0)); then
    echo "::warning::pre-clean of $what exited $MEASURE_CLEAN_CODE"
    cat "$stem.clean.log"
  fi

  # The deploy the measured run redeploys over. Unmeasured, with the same
  # settings: what is under test is the second run, and a base laid down at
  # other settings would put a different remote tree under each cell.
  if ((prepopulate)); then
    if ! METRICS_FILE='' run_easysftp "$binary" "$DATASET_DIR/$scenario" "$remote" "$mode" \
      "$stem.base.log" "$stem.base.out" "$deploy_advanced"; then
      echo "::warning::base deploy of $what failed"
      cat "$stem.base.log"
    fi
    scenario_mutate "$DATASET_DIR/$scenario" "$SCENARIO_CHANGED_FILES"
  fi

  MEASURE_CODE=0
  METRICS_FILE="$stem.metrics.json" \
    run_easysftp "$binary" "$DATASET_DIR/$scenario" "$remote" "$mode" "$stem.log" "$stem.out" "$deploy_advanced" || MEASURE_CODE=$?
  if ((MEASURE_CODE != 0)); then
    # Into the job log, which masks secrets. Never into the artifact.
    echo "::warning::$what exited $MEASURE_CODE"
    cat "$stem.log"
  fi
}

# measure_cell <label> <binary> <ref> <scenario> <connections> <concurrency> <request> <repeat>
#
# The remote path is per (build, scenario) rather than per cell: every run is
# preceded by an unmeasured clean anyway, so a path per cell would only leave
# more empty directories behind on the server.
#
# What is measured is the scenario's mode, not always overlay, and a
# prepopulated scenario gets an unmeasured full deploy plus a small local change
# in between (issue #184, phase 3). Both come from scenario_shape, via
# measure_deploy.
measure_cell() {
  local label=$1 binary=$2 ref=$3 scenario=$4 conns=$5 conc=$6 request=$7 repeat=$8
  local remote="$REMOTE_BASE/matrix/$label/$scenario"
  local slug
  slug=$(link_profile_slug "$link_profile")
  local stem="$LOG_DIR/$slug-$label-$scenario-c$conns-w$conc-r$request-$repeat"
  local what="$label/$scenario c$conns/w$conc/r$request repeat $repeat"
  local code=0

  local advanced="connections: $conns
concurrency: $conc"
  # The token "default" is the pass that sets nothing and leaves easySFTP its
  # own value; it is stored as a null coordinate.
  local request_json=null
  if [[ "$request" != default ]]; then
    advanced="$advanced
request_concurrency: $request"
    request_json=$request
  fi

  measure_deploy "$binary" "$scenario" "$remote" "$stem" "$advanced" 1 "$what"
  code=$MEASURE_CODE

  jq -c \
    --arg label "$label" \
    --arg scenario "$scenario" \
    --arg link_profile "$link_profile" \
    --argjson connections "$conns" \
    --argjson concurrency "$conc" \
    --argjson request_concurrency "$request_json" \
    --argjson repeat "$repeat" \
    '. + $ARGS.named' <<<"$(delete_json "$stem" "$MEASURE_CLEAN_CODE")" >>"$deletes_file"

  jq -nc \
    --arg label "$label" \
    --arg ref "$ref" \
    --arg scenario "$scenario" \
    --arg link_profile "$link_profile" \
    --argjson connections "$conns" \
    --argjson concurrency "$conc" \
    --argjson request_concurrency "$request_json" \
    --argjson repeat "$repeat" \
    --argjson exit_code "$code" \
    --argjson duration_ms "$(step_number "$stem.out" duration-ms)" \
    --argjson files "$(step_number "$stem.out" files-uploaded)" \
    --argjson bytes "$(step_number "$stem.out" bytes-uploaded)" \
    --argjson retries "$(count_matches "$stem.log" -e retrying -e reconnecting)" \
    --argjson errors "$(count_matches "$stem.log" '^::error::')" \
    --argjson metrics "$(metrics_json "$stem.metrics.json")" \
    '$ARGS.named' >>"$runs_file"

  echo "$link_profile $label/$scenario connections=$conns concurrency=$conc request=$request repeat $repeat: $(step_number "$stem.out" duration-ms) ms, exit $code"
}

# measure_auto <scenario> <repeat>: the candidate build with connections,
# concurrency and request_concurrency all left at "auto" (issue #184, phase 5).
#
# This is the policy under test, not a coordinate: auto picks its own settings,
# so what the run is measured for is the gap to the best cell of the same
# scenario and profile. Which settings it picked is read back out of the run's
# own counters (config_connections and friends, see internal/uploader) rather
# than assumed here, because the point of the measurement is what easySFTP did,
# not what this script believes it does.
#
# It runs inside the repeat loop, next to the cells it is compared against, and
# on the candidate build only: a regret against a baseline build's grid would
# compare two different products.
measure_auto() {
  local scenario=$1 repeat=$2 slug
  slug=$(link_profile_slug "$link_profile")
  local stem="$LOG_DIR/auto-$slug-$scenario-$repeat"
  local remote="$REMOTE_BASE/matrix/auto/$scenario"
  local what="auto/$scenario repeat $repeat"
  local advanced="connections: auto
concurrency: auto
request_concurrency: auto"

  measure_deploy "$CANDIDATE_BIN" "$scenario" "$remote" "$stem" "$advanced" 0 "$what"

  jq -nc \
    --arg label auto \
    --arg ref "$CANDIDATE_REF" \
    --arg scenario "$scenario" \
    --arg link_profile "$link_profile" \
    --argjson repeat "$repeat" \
    --argjson exit_code "$MEASURE_CODE" \
    --argjson duration_ms "$(step_number "$stem.out" duration-ms)" \
    --argjson files "$(step_number "$stem.out" files-uploaded)" \
    --argjson bytes "$(step_number "$stem.out" bytes-uploaded)" \
    --argjson metrics "$(metrics_json "$stem.metrics.json")" \
    '$ARGS.named' >>"$auto_file"

  echo "$link_profile auto/$scenario repeat $repeat: $(step_number "$stem.out" duration-ms) ms, exit $MEASURE_CODE"
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
    read -r -a axis_conn <<<"${scenario_conn_axis[$scenario]}"
    read -r -a axis_conc <<<"${scenario_conc_axis[$scenario]}"
    read -r -a axis_req <<<"${scenario_req_axis[$scenario]}"
    for ((repeat = 1; repeat <= REPEATS; repeat++)); do
      # Before this repeat's cells, so the policy run and the grid it is scored
      # against sit inside the same stretch of the sweep.
      measure_auto "$scenario" "$repeat"
      for conns in "${axis_conn[@]}"; do
        for conc in "${axis_conc[@]}"; do
          for request in "${axis_req[@]}"; do
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
  stem="$LOG_DIR/cleanup-auto-$scenario"
  METRICS_FILE='' run_easysftp "$CANDIDATE_BIN" "$DATASET_DIR/empty" \
    "$REMOTE_BASE/matrix/auto/$scenario" clean "$stem.log" "$stem.out" "" ||
    echo "::warning::cleanup of auto/$scenario failed"
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

# The axes each scenario was actually swept over, as JSON. The declared axes are
# what was asked for; these are what the payload allows, and a reader plotting a
# heatmap needs the second kind (issue #184, phase 5).
per_scenario_axes=$(for scenario in "${scenarios[@]}"; do
  read -r -a axis_conn <<<"${scenario_conn_axis[$scenario]}"
  read -r -a axis_conc <<<"${scenario_conc_axis[$scenario]}"
  read -r -a axis_req <<<"${scenario_req_axis[$scenario]}"
  jq -nc --arg k "$scenario" \
    --argjson files "$(scenario_files "$scenario")" \
    --argjson conn "$(printf '%s\n' "${axis_conn[@]}" | jq -s '.')" \
    --argjson conc "$(printf '%s\n' "${axis_conc[@]}" | jq -s '.')" \
    --argjson req "$(printf '%s\n' "${axis_req[@]}" | jq -Rs 'split("\n") | map(select(. != "") | if . == "default" then null else tonumber end)')" \
    '{key: $k, value: {files: $files, connections: $conn, concurrency: $conc, request_concurrency: $req}}'
done | jq -s 'from_entries')

settings="matrix sweep; every other advanced.* setting stays at easySFTP's defaults (retries 2, timeout 30s). The mode belongs to the scenario, see scenarios below: a redeploy scenario is deployed once unmeasured and then measured over itself with $SCENARIO_CHANGED_FILES file(s) changed. Each scenario is swept over the axis values its payload can use, see axes.per_scenario"
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

cat "$OUT_DIR/matrix.md"
