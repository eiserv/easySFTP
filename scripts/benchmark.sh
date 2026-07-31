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
#   BENCH_TOOL                    optional built cmd/easysftp-bench, which turns
#                                 the JSONL measured here into results.json,
#                                 results.csv and summary.md (issue #190).
#                                 Without it the command is run from source
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
probes_file="$LOG_DIR/link-probes.jsonl"
deletes_file="$LOG_DIR/deletes.jsonl"
: >"$runs_file"
: >"$probes_file"
: >"$deletes_file"

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

  # Unmeasured *as an upload*: every repeat starts from the same empty remote
  # directory, so repeat 1 (uploading into nothing) measures the same thing as
  # the repeats after it (which would otherwise overwrite existing files).
  #
  # It is instrumented all the same, into its own metrics file and its own
  # aggregate (issue #184, phase 4): a clean deployment of an empty directory is
  # a pure delete sweep, and it is the only place deletions are measured at all.
  # Nothing of it ever reaches the upload numbers; the two files never meet.
  local clean_code=0
  METRICS_FILE="$stem.clean.metrics.json" \
    run_easysftp "$binary" "$DATASET_DIR/empty" "$remote" clean "$stem.clean.log" "$stem.clean.out" || clean_code=$?
  if ((clean_code != 0)); then
    echo "::warning::pre-clean of $label/$scenario repeat $repeat exited $clean_code"
    cat "$stem.clean.log"
  fi
  jq -c \
    --arg label "$label" \
    --arg scenario "$scenario" \
    --arg link_profile "$link_profile" \
    --argjson repeat "$repeat" \
    '. + $ARGS.named' <<<"$(delete_json "$stem" "$clean_code")" >>"$deletes_file"

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

# Everything below is aggregation and reporting, and none of it happens here any
# more: the manifest describes the run, cmd/easysftp-bench reads it together with
# the JSONL above and writes results.json, results.csv and summary.md (issue
# #190). What stayed in this script is what needs a host: payload generation,
# running the builds, shaping the link and probing it.
#
# The display strings travel verbatim rather than being rebuilt on the other
# side. This script already has them, and a summary table that reformats itself
# during a rewrite is a change to a stored document made by accident.
settings="easySFTP defaults (no advanced.* overrides): concurrency 4, request_concurrency 16, retries 2, timeout 30s, mode overlay"
if [[ -n "$BENCH_CONNECTIONS" ]]; then
  settings="$settings; the pool$BENCH_CONNECTIONS build is the same binary with advanced.connections: $BENCH_CONNECTIONS"
fi
if [[ -n "${BENCH_LINK_PROFILES:-}" ]]; then
  settings="$settings; measured over the link profiles ${link_profiles[*]}"
fi

# The reference build every delta is measured against: the baseline when one
# was measured, otherwise the candidate, so a pool run without a baseline is
# still compared against something.
reference_label=candidate
if [[ -n "$BASELINE_BIN" ]]; then
  reference_label=baseline
fi

scenario_manifest=$(for scenario in "${SCENARIOS[@]}"; do
  jq -nc --arg name "$scenario" --arg description "$(scenario_description "$scenario")"     '$ARGS.named'
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
  --argjson link "$(link_manifest_json "${BENCH_LINK_PROFILES:-}" "${link_profiles[@]}")" \
  --arg runs_file "$runs_file" \
  --arg deletes_file "$deletes_file" \
  --arg probes_file "$probes_file" \
  '{
     kind: "standard",
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
     files: {runs: $runs_file, deletes: $deletes_file, probes: $probes_file}
   }' >"$manifest"

bench_tool aggregate --manifest "$manifest" --out "$OUT_DIR"

cat "$OUT_DIR/summary.md"
