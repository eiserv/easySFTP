#!/usr/bin/env bash
#
# Shared measurement core of scripts/benchmark.sh and
# scripts/benchmark-matrix.sh: payload generation, running one easySFTP build,
# reading its step outputs and its metrics file, and the jq statistics both
# scripts aggregate with.
#
# Sourced, never executed. The caller is responsible for validating and
# exporting the environment described in its own header; this file only reads:
#
#   LOG_DIR DATASET_DIR REMOTE_BASE
#   BENCH_HOST BENCH_PORT BENCH_USERNAME BENCH_PASSWORD BENCH_KNOWN_HOSTS
#
# Nothing here writes to OUT_DIR: run logs and the generated config file name
# the host and the user, and OUT_DIR is uploaded as a workflow artifact, where
# nothing is masked.

# Scenario payloads, as "<count>:<KiB each>" groups. Fixed on purpose: a
# benchmark whose payload changes between runs measures nothing.
#
# "single" exists for the matrix benchmark: one large file cannot be spread
# over connections at all, which makes it the control against which a
# connections/concurrency curve is read.
scenario_spec() {
  case $1 in
  small) echo '300:4' ;;               # per-file round-trip overhead dominates
  mixed) echo '40:16 12:256 4:2048' ;; # roughly a built website
  large) echo '2:16384' ;;             # raw transfer throughput
  single) echo '1:32768' ;;            # one 32 MiB file, no parallelism to find
  *)
    echo "::error::unknown scenario $1" >&2
    return 1
    ;;
  esac
}

scenario_description() {
  case $1 in
  small) echo '300 files x 4 KiB' ;;
  mixed) echo '40 x 16 KiB + 12 x 256 KiB + 4 x 2 MiB' ;;
  large) echo '2 files x 16 MiB' ;;
  single) echo '1 file x 32 MiB' ;;
  *) echo 'unknown' ;;
  esac
}

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

# require_tools: checked because the benchmark runs on a self-hosted runner,
# where a GitHub-hosted image's preinstalled tools are not a given. Better here
# than after minutes of measuring.
require_tools() {
  local cmd
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null || {
      echo "::error::$cmd is required but not installed on this runner" >&2
      exit 1
    }
  done
}

# check_remote_base: the benchmark wipes everything below
# "$REMOTE_BASE/<build>/<scenario>" before every repeat, so a too-broad path is
# a data-loss bug, not a slow benchmark. easySFTP itself refuses "/"; this
# refuses the rest of the obvious mistakes.
check_remote_base() {
  case "$REMOTE_BASE" in
  / | . | .. | */)
    echo "::error::REMOTE_BASE ('$REMOTE_BASE') must be a dedicated directory such as /easysftp-benchmark, without a trailing slash" >&2
    exit 1
    ;;
  esac
}

# check_log_dir: run logs echo the host name and the user name, both secrets
# here. OUT_DIR is uploaded as a workflow artifact, where nothing is masked, so
# the logs must live outside it.
check_log_dir() {
  local out_abs log_abs
  out_abs=$(cd "$OUT_DIR" && pwd -P)
  log_abs=$(cd "$LOG_DIR" && pwd -P)
  if [[ "$log_abs" == "$out_abs" || "$log_abs" == "$out_abs"/* ]]; then
    echo "::error::LOG_DIR must not be inside OUT_DIR: run logs name the host and user, and OUT_DIR is uploaded as an artifact" >&2
    exit 1
  fi
}

# generate_dataset <scenario>...: writes each scenario's payload under
# DATASET_DIR, plus an empty directory used for the pre-clean and cleanup runs.
generate_dataset() {
  local scenario dir spec count size index sub
  local -a specs
  mkdir -p "$DATASET_DIR/empty"
  for scenario in "$@"; do
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

# run_easysftp <binary> <source> <remote> <mode> <log> <outputs-file> [advanced-yaml]
#
# Without an advanced block this is an inline run, exactly what a workflow with
# source/target inputs does. With one, the run goes through a generated config
# file instead: advanced.* settings have no inputs (every non-secret setting has
# exactly one home in v3), and inline inputs may not be combined with a config
# file. That file names the host and the user, so it is written to LOG_DIR.
#
# The caller may set METRICS_FILE to have the run write its instrumentation
# there (see internal/metrics); an empty value leaves the run uninstrumented.
#
# BENCH_* are never assigned here: they come from the environment and the
# caller's require_env has already refused an empty one.
# shellcheck disable=SC2153
run_easysftp() {
  local binary=$1 source=$2 remote=$3 mode=$4 log=$5 outputs=$6 advanced=${7:-}
  : >"$outputs"
  local -a vars=(GITHUB_OUTPUT="$outputs" EASYSFTP_PASSWORD="$BENCH_PASSWORD")
  if [[ -n "${METRICS_FILE:-}" ]]; then
    rm -f "$METRICS_FILE"
    vars+=(EASYSFTP_METRICS_FILE="$METRICS_FILE")
  fi
  if [[ -n "$advanced" ]]; then
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
      printf '%s\n' "$advanced" | sed 's/^/  /'
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

# metrics_json <file>: the run's metrics document, or "null" when the run died
# before writing one (or wrote something unreadable). Always prints valid JSON
# so callers can pass it to "jq --argjson" unconditionally.
metrics_json() {
  if [[ -f "$1" ]] && jq -e . "$1" >/dev/null 2>&1; then
    cat "$1"
  else
    echo null
  fi
}

# JQ_STATS is prepended to every aggregation filter in both benchmark scripts.
#
# "median" is the lower middle value for an even sample count, which is why an
# odd number of repeats is the better choice. "mad" is the median absolute
# deviation: a spread metric that a single slow repeat (the normal failure mode
# of a shared host) does not blow up, unlike a standard deviation.
#
# "mad" is null below two samples rather than 0: a single repeat has no measured
# spread at all, and a 0 there reads as perfect precision (issue #184, phase 2).
# SC2034: used by the scripts that source this file. SC2016: the "$xs"/"$m" in
# here are jq variables, so single quotes are exactly right.
# shellcheck disable=SC2034,SC2016
JQ_STATS='
  def median: if length == 0 then 0 else (sort | .[(((length - 1) / 2) | floor)]) end;
  def mad: . as $xs
    | if length < 2 then null
      else ($xs | median) as $m
        | ($xs | map(if . - $m < 0 then $m - . else . - $m end) | median)
      end;
  def stats: {
      values: .,
      median: median,
      min: (min // 0),
      max: (max // 0),
      mad: mad,
      samples: length
    };
  def round1: (. * 10 | round) / 10;
  def round2: (. * 100 | round) / 100;
  def pct(a; b): if (b // 0) == 0 then null else (((a - b) / b) * 100 | round2) end;
'

# bench_environment: the machine and toolchain a result was measured on, as
# JSON. Two results are only comparable when this matches; benchmarks/README.md
# says so and index.json carries it forward.
bench_environment() {
  local cpu_model
  cpu_model=$(awk -F': ' '/^model name/ { print $2; exit }' /proc/cpuinfo 2>/dev/null || true)
  jq -nc \
    --arg runner "${RUNNER_ENVIRONMENT:-local}" \
    --arg os "$(uname -s)" \
    --arg kernel "$(uname -r)" \
    --arg arch "$(uname -m)" \
    --arg cpu_model "${cpu_model:-unknown}" \
    --argjson cpus "$(nproc)" \
    --arg go_version "$(go version 2>/dev/null | awk '{print $3}')" \
    --arg runner_name "${RUNNER_NAME:-}" \
    '$ARGS.named | with_entries(select(.value != ""))'
}
