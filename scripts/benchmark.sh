#!/usr/bin/env bash
#
# easySFTP upload benchmark (issue #169).
#
# Measures one or two builds of easySFTP against a real SFTP server and writes
# results.json plus summary.md. It collects data only: nothing here fails on a
# slow number, because run-to-run variance against an external host is not
# understood yet.
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
#
# "Median" here is the lower middle value for an even repeat count, which is
# why an odd number of repeats is the better choice.

set -euo pipefail

# Scenario payloads, as "<count>:<KiB each>" groups. Fixed on purpose: a
# benchmark whose payload changes between runs measures nothing.
scenario_spec() {
  case $1 in
  small) echo '300:4' ;;               # per-file round-trip overhead dominates
  mixed) echo '40:16 12:256 4:2048' ;; # roughly a built website
  large) echo '2:16384' ;;             # raw transfer throughput
  *)
    echo "::error::unknown scenario $1" >&2
    return 1
    ;;
  esac
}

SCENARIOS=(small mixed large)

require_env() {
  local name missing=0
  for name in "$@"; do
    if [[ -z "${!name:-}" ]]; then
      echo "::error::$name is required but empty" >&2
      missing=1
    fi
  done
  if ((missing)); then
    echo "::error::See the secret list in .github/workflows/benchmark.yml" >&2
    exit 1
  fi
}

require_env CANDIDATE_BIN CANDIDATE_REF OUT_DIR DATASET_DIR LOG_DIR \
  REMOTE_BASE BENCH_HOST BENCH_USERNAME BENCH_PASSWORD BENCH_KNOWN_HOSTS

# Checked because the benchmark runs on a self-hosted runner, where a
# GitHub-hosted image's preinstalled tools are not a given. Better here than
# after minutes of measuring.
for cmd in jq nproc; do
  command -v "$cmd" >/dev/null || {
    echo "::error::$cmd is required but not installed on this runner" >&2
    exit 1
  }
done

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

# The benchmark wipes everything below "$REMOTE_BASE/<build>/<scenario>" before
# every repeat, so a too-broad path is a data-loss bug, not a slow benchmark.
# easySFTP itself refuses "/"; this refuses the rest of the obvious mistakes.
case "$REMOTE_BASE" in
/ | . | .. | */)
  echo "::error::REMOTE_BASE ('$REMOTE_BASE') must be a dedicated directory such as /easysftp-benchmark, without a trailing slash" >&2
  exit 1
  ;;
esac

mkdir -p "$OUT_DIR" "$DATASET_DIR" "$LOG_DIR"

# Run logs echo the host name and the user name, both secrets here. OUT_DIR is
# uploaded as a workflow artifact, where nothing is masked, so the logs must
# live outside it.
out_abs=$(cd "$OUT_DIR" && pwd -P)
log_abs=$(cd "$LOG_DIR" && pwd -P)
if [[ "$log_abs" == "$out_abs" || "$log_abs" == "$out_abs"/* ]]; then
  echo "::error::LOG_DIR must not be inside OUT_DIR: run logs name the host and user, and OUT_DIR is uploaded as an artifact" >&2
  exit 1
fi

runs_file="$LOG_DIR/runs.jsonl"
aggregate_file="$LOG_DIR/aggregate.json"
: >"$runs_file"

generate_dataset() {
  local scenario dir spec count size index sub
  local -a specs
  mkdir -p "$DATASET_DIR/empty"
  for scenario in "${SCENARIOS[@]}"; do
    dir="$DATASET_DIR/$scenario"
    rm -rf "$dir"
    index=0
    read -r -a specs <<<"$(scenario_spec "$scenario")"
    for spec in "${specs[@]}"; do
      count=${spec%%:*}
      size=${spec##*:}
      while ((count-- > 0)); do
        # Spread over subdirectories so remote directory creation is part of
        # the measurement, as it is in a real site upload.
        sub="$dir/part$((index % 8))"
        mkdir -p "$sub"
        head -c "$((size * 1024))" /dev/urandom >"$sub/file$index.bin"
        index=$((index + 1))
      done
    done
    echo "dataset $scenario: $index file(s), $(du -sh "$dir" | cut -f1)"
  done
}

# run_easysftp <binary> <source> <remote> <mode> <log> <outputs-file> [connections]
#
# Without a connections value this is an inline run, exactly what a workflow
# with source/target inputs does. With one, the run goes through a generated
# config file instead: advanced.connections has no input (every non-secret
# setting has exactly one home in v3), and inline inputs may not be combined
# with a config file. That file names the host and the user, so it is written
# to LOG_DIR, which is never uploaded as an artifact.
#
# BENCH_* are never assigned here: they come from the environment and
# require_env has already refused an empty one.
# shellcheck disable=SC2153
run_easysftp() {
  local binary=$1 source=$2 remote=$3 mode=$4 log=$5 outputs=$6 connections=${7:-}
  : >"$outputs"
  local -a vars=(GITHUB_OUTPUT="$outputs" EASYSFTP_PASSWORD="$BENCH_PASSWORD")
  if [[ -n "$connections" ]]; then
    local config="$LOG_DIR/config.yml"
    {
      echo "version: 3"
      echo "connection:"
      echo "  host: \"$BENCH_HOST\""
      echo "  port: $BENCH_PORT"
      echo "  username: \"$BENCH_USERNAME\""
      echo "  known_hosts: |"
      printf '%s\n' "$BENCH_KNOWN_HOSTS" | sed 's/^/    /'
      echo "deployments:"
      echo "  benchmark:"
      echo "    source: \"$source\""
      echo "    target: \"$remote\""
      echo "    mode: $mode"
      echo "advanced:"
      echo "  connections: $connections"
    } >"$config"
    vars+=(EASYSFTP_CONFIG="$config")
  else
    vars+=(
      EASYSFTP_HOST="$BENCH_HOST"
      EASYSFTP_PORT="$BENCH_PORT"
      EASYSFTP_USERNAME="$BENCH_USERNAME"
      EASYSFTP_KNOWN_HOSTS="$BENCH_KNOWN_HOSTS"
      EASYSFTP_SOURCE="$source"
      EASYSFTP_TARGET="$remote"
      EASYSFTP_MODE="$mode"
    )
  fi
  env "${vars[@]}" "$binary" >"$log" 2>&1
}

# step_number <outputs-file> <name>: reads a number written in the
# GITHUB_OUTPUT heredoc format, 0 when the run died before writing it.
step_number() {
  local value
  value=$(awk -v key="$2" 'index($0, key "<<") == 1 { getline; print; exit }' "$1")
  if [[ "$value" =~ ^[0-9]+$ ]]; then
    echo "$value"
  else
    echo 0
  fi
}

# count_matches <file> <grep args...>: always prints a number, so the callers
# can hand it straight to "jq --argjson".
count_matches() {
  local file=$1 count
  shift
  count=$(grep -c "$@" "$file" 2>/dev/null || true)
  if [[ "$count" =~ ^[0-9]+$ ]]; then
    echo "$count"
  else
    echo 0
  fi
}

# measure <label> <binary> <ref> <scenario> <repeat> <connections>
measure() {
  local label=$1 binary=$2 ref=$3 scenario=$4 repeat=$5 connections=$6
  local remote="$REMOTE_BASE/$label/$scenario"
  local stem="$LOG_DIR/$label-$scenario-$repeat"
  local code=0

  # Unmeasured: every repeat starts from the same empty remote directory, so
  # repeat 1 (uploading into nothing) measures the same thing as the repeats
  # after it (which would otherwise overwrite existing files).
  if ! run_easysftp "$binary" "$DATASET_DIR/empty" "$remote" clean "$stem.clean.log" "$stem.clean.out" "$connections"; then
    echo "::warning::pre-clean of $label/$scenario repeat $repeat failed"
    cat "$stem.clean.log"
  fi

  run_easysftp "$binary" "$DATASET_DIR/$scenario" "$remote" overlay "$stem.log" "$stem.out" "$connections" || code=$?
  if ((code != 0)); then
    # Into the job log, which masks secrets. Never into the artifact.
    echo "::warning::$label/$scenario repeat $repeat exited $code"
    cat "$stem.log"
  fi

  jq -nc \
    --arg label "$label" \
    --arg ref "$ref" \
    --arg scenario "$scenario" \
    --argjson repeat "$repeat" \
    --argjson exit_code "$code" \
    --argjson duration_ms "$(step_number "$stem.out" duration-ms)" \
    --argjson files "$(step_number "$stem.out" files-uploaded)" \
    --argjson bytes "$(step_number "$stem.out" bytes-uploaded)" \
    --argjson retries "$(count_matches "$stem.log" -e retrying -e reconnecting)" \
    --argjson errors "$(count_matches "$stem.log" '^::error::')" \
    --argjson refused "$(count_matches "$stem.log" -e 'could not open connection')" \
    '$ARGS.named' >>"$runs_file"

  echo "$label/$scenario repeat $repeat: $(step_number "$stem.out" duration-ms) ms, exit $code"
}

labels=(candidate)
binaries=("$CANDIDATE_BIN")
refs=("$CANDIDATE_REF")
conns=("")
if [[ -n "$BASELINE_BIN" ]]; then
  labels+=(baseline)
  binaries+=("$BASELINE_BIN")
  refs+=("${BASELINE_REF:-unknown}")
  conns+=("")
fi
# The connection pool is measured as a third build of the *same* binary, not
# as a separate workflow run: the two numbers are only comparable when they
# are interleaved on the same host in the same minutes.
if [[ -n "$BENCH_CONNECTIONS" ]]; then
  labels+=("pool$BENCH_CONNECTIONS")
  binaries+=("$CANDIDATE_BIN")
  refs+=("$CANDIDATE_REF")
  conns+=("$BENCH_CONNECTIONS")
fi

generate_dataset

for scenario in "${SCENARIOS[@]}"; do
  for ((repeat = 1; repeat <= REPEATS; repeat++)); do
    for i in "${!labels[@]}"; do
      measure "${labels[$i]}" "${binaries[$i]}" "${refs[$i]}" "$scenario" "$repeat" "${conns[$i]}"
    done
  done
done

# Leave the benchmark directories empty so the payload does not linger on the
# server. Best effort: a cleanup hiccup must not hide the results.
for scenario in "${SCENARIOS[@]}"; do
  for i in "${!labels[@]}"; do
    stem="$LOG_DIR/cleanup-${labels[$i]}-$scenario"
    run_easysftp "${binaries[$i]}" "$DATASET_DIR/empty" \
      "$REMOTE_BASE/${labels[$i]}/$scenario" clean "$stem.log" "$stem.out" "${conns[$i]}" ||
      echo "::warning::cleanup of ${labels[$i]}/$scenario failed"
  done
done

jq -s '
  def median: sort | .[(((length - 1) / 2) | floor)];
  group_by([.label, .scenario])
  | map({
      label: .[0].label,
      ref: .[0].ref,
      scenario: .[0].scenario,
      repeats: length,
      failed_runs: (map(select(.exit_code != 0)) | length),
      files: (map(.files) | max),
      bytes: (map(.bytes) | max),
      durations_ms: map(.duration_ms),
      median_ms: (map(.duration_ms) | median),
      min_ms: (map(.duration_ms) | min),
      max_ms: (map(.duration_ms) | max),
      retries: (map(.retries) | add),
      errors: (map(.errors) | add),
      refused_connections: (map(.refused) | add)
    })
  | map(. + {
      mib_per_s: (if .median_ms > 0 then (.bytes / 1048576) / (.median_ms / 1000) else 0 end),
      files_per_s: (if .median_ms > 0 then .files / (.median_ms / 1000) else 0 end)
    })
' "$runs_file" >"$aggregate_file"

settings="easySFTP defaults (no advanced.* overrides): concurrency 4, request_concurrency 16, retries 2, timeout 30s, mode overlay"
if [[ -n "$BENCH_CONNECTIONS" ]]; then
  settings="$settings; the pool$BENCH_CONNECTIONS build is the same binary with advanced.connections: $BENCH_CONNECTIONS"
fi

jq -n \
  --slurpfile results "$aggregate_file" \
  --arg candidate_ref "$CANDIDATE_REF" \
  --arg baseline_ref "$BASELINE_REF" \
  --argjson repeats "$REPEATS" \
  --arg runner "${RUNNER_ENVIRONMENT:-local}, $(uname -sr), $(nproc) cpu" \
  --arg settings "$settings" \
  '{
     candidate_ref: $candidate_ref,
     baseline_ref: $baseline_ref,
     repeats: $repeats,
     runner: $runner,
     settings: $settings,
     scenarios: {
       small: "300 files x 4 KiB",
       mixed: "40 x 16 KiB + 12 x 256 KiB + 4 x 2 MiB",
       large: "2 files x 16 MiB"
     },
     results: $results[0]
   }' >"$OUT_DIR/results.json"

# summary.md carries the same numbers, readable. The delta column compares
# every build's median against the reference build's; negative means faster.
# The reference is the baseline when one was measured, otherwise the candidate,
# so a pool run without a baseline is still compared against something.
reference_label=candidate
if [[ -n "$BASELINE_BIN" ]]; then
  reference_label=baseline
fi
{
  echo "## easySFTP benchmark"
  echo
  echo "| Setting | Value |"
  echo "|---|---|"
  echo "| Candidate | \`$CANDIDATE_REF\` |"
  echo "| Baseline | \`${BASELINE_REF:-none}\` |"
  echo "| Repeats per scenario | $REPEATS |"
  echo "| Runner | $(uname -sr), $(nproc) cpu |"
  echo "| Settings | $settings |"
  echo
  echo "| Scenario | Build | Files | Size | Median | Min | Max | MiB/s | files/s | Retries | Errors | Failed runs | Delta |"
  echo "|---|---|---|---|---|---|---|---|---|---|---|---|---|"
  for scenario in "${SCENARIOS[@]}"; do
    baseline_median=$(jq -r --arg s "$scenario" --arg r "$reference_label" \
      'map(select(.scenario == $s and .label == $r)) | .[0].median_ms // empty' "$aggregate_file")
    for label in "${labels[@]}"; do
      row=$(jq -r --arg s "$scenario" --arg l "$label" '
        map(select(.scenario == $s and .label == $l)) | .[0]
        | [.files, (.bytes / 1048576 * 10 | round / 10),
           .median_ms, .min_ms, .max_ms,
           (.mib_per_s * 10 | round / 10), (.files_per_s * 10 | round / 10),
           .retries, .errors, .failed_runs]
        | @tsv' "$aggregate_file")
      IFS=$'\t' read -r files mib median min max mibs fps retries errors failed <<<"$row"
      delta='-'
      if [[ "$label" != "$reference_label" && -n "$baseline_median" && "$baseline_median" != 0 ]]; then
        delta=$(awk -v c="$median" -v b="$baseline_median" 'BEGIN { printf "%+.1f%%", (c - b) / b * 100 }')
      fi
      echo "| $scenario | $label | $files | ${mib} MiB | ${median} ms | ${min} ms | ${max} ms | $mibs | $fps | $retries | $errors | $failed | $delta |"
    done
  done
  echo
  echo "Delta compares each build's median against the \`$reference_label\` build; negative is faster."
  # Without this line a pool run whose extra connections the server refused
  # reads as "the pool did nothing", which is the wrong conclusion entirely.
  refused=$(jq '[.[].refused_connections] | add' "$aggregate_file")
  if [[ "$refused" != 0 ]]; then
    echo
    echo "**$refused connection(s) were refused by the server** and fell back to the run's first connection, so a pool build measured here had fewer connections than configured."
  fi
  echo
  echo "Data only: these numbers set no threshold and fail no build. Collected to evaluate the single-connection ceiling discussed in issue #158."
} >"$OUT_DIR/summary.md"

cat "$OUT_DIR/summary.md"
