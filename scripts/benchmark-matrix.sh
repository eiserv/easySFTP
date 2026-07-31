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
#   BENCH_TOOL                  optional built cmd/easysftp-bench, which turns
#                               the JSONL measured here into matrix.json,
#                               matrix.csv and matrix.md (issue #190). Without
#                               it the command is run from source
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
# Everything below is aggregation and reporting, and none of it happens here any
# more: the manifest describes the sweep, cmd/easysftp-bench reads it together
# with the JSONL above and writes matrix.json, matrix.csv and matrix.md (issue
# #190). What stayed in this script is what needs a host: payload generation,
# running the cells, shaping the link and probing it.
settings="matrix sweep; every other advanced.* setting stays at easySFTP's defaults (retries 2, timeout 30s). The mode belongs to the scenario, see scenarios below: a redeploy scenario is deployed once unmeasured and then measured over itself with $SCENARIO_CHANGED_FILES file(s) changed. Each scenario is swept over the axis values its payload can use, see axes.per_scenario"
if [[ -n "$MATRIX_LINK_PROFILES" ]]; then
  settings="$settings; swept over the link profiles ${link_profiles[*]}"
fi

# One entry per scenario, carrying both what it is and the axes its payload
# allowed. The display strings are the ones the summary prints, kept verbatim so
# a rewrite cannot reformat a stored document by accident.
scenario_manifest=$(for scenario in "${scenarios[@]}"; do
  read -r mode prepopulate _ <<<"$(scenario_shape "$scenario")"
  if ((prepopulate)); then
    mode="$mode, redeployed"
  fi
  read -r -a axis_conn <<<"${scenario_conn_axis[$scenario]}"
  read -r -a axis_conc <<<"${scenario_conc_axis[$scenario]}"
  read -r -a axis_req <<<"${scenario_req_axis[$scenario]}"
  jq -nc \
    --arg name "$scenario" \
    --arg description "$(scenario_description "$scenario")" \
    --arg mode "$mode" \
    --argjson files "$(scenario_files "$scenario")" \
    --arg connections_display "${scenario_conn_axis[$scenario]}" \
    --arg concurrency_display "${scenario_conc_axis[$scenario]}" \
    --arg request_display "${scenario_req_axis[$scenario]}" \
    --argjson connections "$(json_array_of "${axis_conn[@]}")" \
    --argjson concurrency "$(json_array_of "${axis_conc[@]}")" \
    --argjson request_concurrency "$(json_array_of "${axis_req[@]}")" \
    '$ARGS.named'
done | jq -s '.')

manifest="$LOG_DIR/manifest.json"
jq -n \
  --arg candidate_ref "$CANDIDATE_REF" \
  --arg baseline_ref "$BASELINE_REF" \
  --arg reference_label "$reference_label" \
  --argjson repeats "$REPEATS" \
  --arg runner "${RUNNER_ENVIRONMENT:-local}, $(uname -sr), $(nproc) cpu" \
  --arg runner_display "$(uname -sr), $(nproc) cpu" \
  --argjson environment "$(bench_environment)" \
  --arg settings "$settings" \
  --argjson labels "$(json_strings_of "${labels[@]}")" \
  --argjson scenarios "$scenario_manifest" \
  --argjson link "$(link_manifest_json "$MATRIX_LINK_PROFILES" "${link_profiles[@]}")" \
  --argjson connections_axis "$(json_array_of "${connections_axis[@]}")" \
  --argjson concurrency_axis "$(json_array_of "${concurrency_axis[@]}")" \
  --argjson request_axis "$(json_array_of "${request_axis[@]}")" \
  --arg connections_display "$MATRIX_CONNECTIONS" \
  --arg concurrency_display "$MATRIX_CONCURRENCY" \
  --arg request_display "${MATRIX_REQUEST_CONCURRENCY:-}" \
  --arg canary_scenario "$CANARY_SCENARIO" \
  --argjson canary_connections "$CANARY_CONNECTIONS" \
  --argjson canary_concurrency "$CANARY_CONCURRENCY" \
  --arg runs_file "$runs_file" \
  --arg deletes_file "$deletes_file" \
  --arg probes_file "$probes_file" \
  --arg canary_file "$canary_file" \
  --arg auto_file "$auto_file" \
  '{
     kind: "matrix",
     candidate_ref: $candidate_ref,
     baseline_ref: $baseline_ref,
     reference_label: $reference_label,
     repeats: $repeats,
     runner: $runner,
     runner_display: $runner_display,
     environment: $environment,
     settings: $settings,
     labels: $labels,
     scenarios: $scenarios,
     link: $link,
     grid: {
       connections: $connections_axis,
       concurrency: $concurrency_axis,
       request_concurrency: $request_axis,
       connections_display: $connections_display,
       concurrency_display: $concurrency_display,
       request_display: $request_display,
       canary: {
         scenario: $canary_scenario,
         connections: $canary_connections,
         concurrency: $canary_concurrency
       }
     },
     files: {
       runs: $runs_file, deletes: $deletes_file, probes: $probes_file,
       canary: $canary_file, auto: $auto_file
     }
   }' >"$manifest"

bench_tool aggregate --manifest "$manifest" --out "$OUT_DIR"

cat "$OUT_DIR/matrix.md"
