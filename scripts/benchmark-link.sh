#!/usr/bin/env bash
#
# The link half of the benchmark measurement (issue #184, phase 1).
#
# scripts/benchmark-lib.sh measures easySFTP. This file measures the path
# easySFTP runs over, and optionally shapes it, so RTT and bandwidth become
# variables instead of invisible constants. Two things live here:
#
#   link_probe          run cmd/linkprobe once and record its document
#   link_shape_*        apply a "tc" profile, and take it down again
#
# Sourced, never executed. It reads:
#
#   LINKPROBE_BIN   built cmd/linkprobe; empty means "do not probe", so a local
#                   run without it still works and stores an empty probe list
#   LINK_IFACE      interface to shape; derived from the route to BENCH_HOST
#   LINK_SUDO       "0"/"no" never uses sudo, a command string overrides it,
#                   empty means "sudo -n when not root"
#   REMOTE_BASE, BENCH_HOST, BENCH_PORT, BENCH_USERNAME, BENCH_PASSWORD,
#   BENCH_KNOWN_HOSTS
#
# Profile grammar, validated before a single byte is measured:
#
#   baseline | unshaped        the real line, untouched
#   +<N>ms                     netem delay, so +50ms means +50 ms of RTT
#   <delay>/<rate>             the same plus a tbf rate, e.g. +50ms/5mbit
#   baseline/<rate>            rate only, e.g. baseline/5mbit
#
# Shaping is egress only, which is the direction a deploy sends bytes in. The
# return path stays untouched, so a delay of N adds N to the round-trip rather
# than 2N.

# Set by link_shape_probe, read by link_shaping_json and the callers.
# shellcheck disable=SC2034
LINK_SHAPING_AVAILABLE=0
LINK_SHAPING_REASON="no profile asked for shaping, so it was never probed for"
LINK_APPLIED=()
LINK_REQUESTED=()

# link_parse_profiles <raw>: prints one validated profile per line, "baseline"
# when raw is empty. Fails the run on a typo, which is the whole point of doing
# this before the measuring starts rather than after hours of it.
link_parse_profiles() {
  local raw=$1 profile
  local -a profiles
  read -r -a profiles <<<"$raw"
  if ((${#profiles[@]} == 0)); then
    echo baseline
    return 0
  fi
  for profile in "${profiles[@]}"; do
    if ! [[ "$profile" =~ ^(baseline|unshaped|\+[0-9]+ms)(/[0-9]+(kbit|mbit|gbit))?$ ]]; then
      echo "::error::link profile '$profile' is not understood. Use baseline, +50ms, +50ms/5mbit or baseline/5mbit" >&2
      return 1
    fi
    echo "$profile"
  done
}

# link_profile_delay <profile>: the netem delay, empty when the profile has none.
link_profile_delay() {
  local head=${1%%/*}
  case $head in
  baseline | unshaped) ;;
  +*) echo "${head#+}" ;;
  esac
}

# link_profile_slug <profile>: the profile as a filename component. Profiles
# contain "+" and "/", and a log file called "+50ms/5mbit-small-1.log" is a
# missing directory, not a log.
link_profile_slug() {
  echo "${1//[^a-zA-Z0-9]/_}"
}

# link_profile_rate <profile>: the tbf rate, empty when the profile has none.
link_profile_rate() {
  case $1 in
  */*) echo "${1##*/}" ;;
  esac
}

# link_shape_needed <profile>...: whether any of these profiles asks for shaping.
# A sweep that only wants the real line must not need tc, sudo or NET_ADMIN.
link_shape_needed() {
  local profile
  for profile in "$@"; do
    if [[ -n "$(link_profile_delay "$profile")$(link_profile_rate "$profile")" ]]; then
      return 0
    fi
  done
  return 1
}

# link_resolve_iface: the interface packets to the SFTP server leave through.
#
# Only the interface name is ever printed: "ip route get" also reports the
# server's address, and BENCH_HOST is a secret in this repository.
link_resolve_iface() {
  local iface=''
  if [[ -n "${LINK_IFACE:-}" ]]; then
    echo "$LINK_IFACE"
    return 0
  fi
  command -v ip >/dev/null || return 1
  iface=$(ip route get "${BENCH_HOST:-}" 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit }}')
  if [[ -z "$iface" ]]; then
    iface=$(ip route show default 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit }}')
  fi
  [[ -n "$iface" ]] || return 1
  echo "$iface"
}

# link_tc: the tc invocation, sudo included when one is needed and allowed.
# Printed as an array so a caller can run it without re-deciding.
link_tc() {
  case "${LINK_SUDO:-}" in
  0 | no | none) printf '%s\n' tc ;;
  '')
    if [[ ${EUID:-$(id -u)} -eq 0 ]]; then
      printf '%s\n' tc
    else
      printf '%s\n' sudo -n tc
    fi
    ;;
  *) printf '%s\n' "${LINK_SUDO}" tc ;;
  esac
}

# link_shape_probe: decides once whether shaping is possible, by doing it.
#
# Checking for "tc" and for root is not enough (a container may have the binary
# and no NET_ADMIN, or CAP_NET_ADMIN and no sudo), so this adds a real qdisc and
# removes it again. Sets LINK_SHAPING_AVAILABLE and LINK_SHAPING_REASON; never
# fails the run, because a benchmark of the real line is still a benchmark.
link_shape_probe() {
  local -a tc
  LINK_SHAPING_AVAILABLE=0

  command -v tc >/dev/null || {
    LINK_SHAPING_REASON="tc is not installed on this runner (iproute2)"
    return 0
  }
  if ! LINK_IFACE=$(link_resolve_iface); then
    LINK_SHAPING_REASON="could not determine the interface towards the server; set LINK_IFACE"
    return 0
  fi
  read -r -a tc <<<"$(link_tc | tr '\n' ' ')"
  if ! "${tc[@]}" qdisc show dev "$LINK_IFACE" >/dev/null 2>&1; then
    LINK_SHAPING_REASON="cannot run '${tc[*]}' on $LINK_IFACE (NET_ADMIN missing, or sudo needs a password)"
    return 0
  fi
  # The real thing: a netem qdisc with no delay, removed immediately.
  if ! "${tc[@]}" qdisc add dev "$LINK_IFACE" root handle 1: netem >/dev/null 2>&1; then
    LINK_SHAPING_REASON="adding a qdisc on $LINK_IFACE was refused (NET_ADMIN missing, or something else already shapes it)"
    return 0
  fi
  "${tc[@]}" qdisc del dev "$LINK_IFACE" root >/dev/null 2>&1 || true
  LINK_SHAPING_AVAILABLE=1
  LINK_SHAPING_REASON=""
  echo "link shaping is available on $LINK_IFACE"
}

# link_shape_clear: back to the unshaped line. Idempotent, and quiet about a
# qdisc that is already gone.
link_shape_clear() {
  local -a tc
  [[ -n "${LINK_IFACE:-}" ]] || return 0
  read -r -a tc <<<"$(link_tc | tr '\n' ' ')"
  "${tc[@]}" qdisc del dev "$LINK_IFACE" root >/dev/null 2>&1 || true
}

# link_shape_trap installs the cleanup exactly once, the first time shaping is
# applied.
#
# This is the safety-critical part of the file. Without it an aborted or timed
# out run leaves the runner shaped, and every later measurement on that machine
# is quietly wrong with nothing in its output to say so.
link_shape_trap() {
  [[ -z "${LINK_TRAP_INSTALLED:-}" ]] || return 0
  LINK_TRAP_INSTALLED=1
  trap 'link_shape_clear' EXIT
  trap 'link_shape_clear; exit 130' INT
  trap 'link_shape_clear; exit 143' TERM
}

# link_shape_apply <profile>: shapes the link for this profile, or reports that
# it could not. Returns non-zero on failure so the caller can record that the
# cell was measured unshaped instead of pretending it was not.
link_shape_apply() {
  local profile=$1 delay rate
  local -a tc
  delay=$(link_profile_delay "$profile")
  rate=$(link_profile_rate "$profile")

  # An unshaped profile is not a no-op: whatever the previous profile applied
  # has to come off first.
  link_shape_clear
  if [[ -z "$delay" && -z "$rate" ]]; then
    LINK_APPLIED+=("$profile")
    return 0
  fi
  if ((LINK_SHAPING_AVAILABLE != 1)); then
    echo "::warning::link profile $profile requested but shaping is unavailable ($LINK_SHAPING_REASON); measuring the real line instead" >&2
    return 1
  fi

  link_shape_trap
  read -r -a tc <<<"$(link_tc | tr '\n' ' ')"

  if [[ -n "$delay" ]]; then
    if ! "${tc[@]}" qdisc add dev "$LINK_IFACE" root handle 1: netem delay "$delay" >/dev/null 2>&1; then
      echo "::warning::could not add netem delay $delay on $LINK_IFACE; measuring the real line instead" >&2
      link_shape_clear
      return 1
    fi
    if [[ -n "$rate" ]]; then
      # tbf below netem, so packets are delayed and then metered. burst and
      # latency are tbf's own buffer sizing, not part of the profile: too small
      # a burst throttles below the configured rate.
      if ! "${tc[@]}" qdisc add dev "$LINK_IFACE" parent 1: handle 2: tbf rate "$rate" burst 32kbit latency 400ms >/dev/null 2>&1; then
        echo "::warning::could not add a tbf rate of $rate on $LINK_IFACE; measuring the real line instead" >&2
        link_shape_clear
        return 1
      fi
    fi
  else
    if ! "${tc[@]}" qdisc add dev "$LINK_IFACE" root handle 1: tbf rate "$rate" burst 32kbit latency 400ms >/dev/null 2>&1; then
      echo "::warning::could not add a tbf rate of $rate on $LINK_IFACE; measuring the real line instead" >&2
      link_shape_clear
      return 1
    fi
  fi

  LINK_APPLIED+=("$profile")
  echo "link profile $profile applied on $LINK_IFACE"
}

# link_probe <profile> <at> <probes-file>: appends one probe document, wrapped
# with the profile it belongs to and whether it was taken before or after the
# measured runs.
#
# Appends nothing at all when there is no probe binary: an empty probes list is
# honest, an invented entry is not.
link_probe() {
  local profile=$1 at=$2 probes=$3 document
  if [[ -z "${LINKPROBE_BIN:-}" ]]; then
    return 0
  fi
  if [[ ! -x "$LINKPROBE_BIN" ]]; then
    echo "::warning::LINKPROBE_BIN ('$LINKPROBE_BIN') is not executable; the link is not being probed" >&2
    return 0
  fi

  # The probe's own stderr goes to the job log, which masks secrets. Its stdout
  # is the document and is stored, which is why cmd/linkprobe keeps the host and
  # the user out of it.
  if ! document=$(
    LINKPROBE_HOST="$BENCH_HOST" \
      LINKPROBE_PORT="${BENCH_PORT:-22}" \
      LINKPROBE_USERNAME="$BENCH_USERNAME" \
      LINKPROBE_PASSWORD="$BENCH_PASSWORD" \
      LINKPROBE_KNOWN_HOSTS="$BENCH_KNOWN_HOSTS" \
      LINKPROBE_REMOTE_PATH="${REMOTE_BASE:-}/linkprobe" \
      "$LINKPROBE_BIN"
  ); then
    echo "::warning::the link probe for profile $profile ($at) failed; no document stored" >&2
    return 0
  fi
  if ! jq -e . >/dev/null 2>&1 <<<"$document"; then
    echo "::warning::the link probe for profile $profile ($at) produced no JSON; no document stored" >&2
    return 0
  fi

  local wrapped
  wrapped=$(jq -c --arg profile "$profile" --arg at "$at" '{profile: $profile, at: $at} + .' <<<"$document")
  printf '%s\n' "$wrapped" >>"$probes"

  jq -r '"link probe \(.profile) (\(.at)): rtt p50 \(.rtt_ms.p50 // "n/a") ms, control "
    + "\(.control.single_stream_mib_per_s // "n/a") MiB/s single / \(.control.n_stream_mib_per_s // "n/a") MiB/s multi"' \
    <<<"$wrapped"
}

# link_manifest_json <requested-input> <profile>...: the shaping half of the
# link object, for the run manifest (issue #190).
#
# The probes are deliberately not in here: they are read from the probe file by
# whatever assembles the result, so a run without a probe binary simply has
# none. This carries only what the shell knows and the aggregation cannot
# derive: which interface was shaped, whether shaping was possible, what was
# asked for, what was applied, and the raw input string the summary prints.
link_manifest_json() {
  local requested=$1
  shift
  jq -n \
    --arg iface "${LINK_IFACE:-}" \
    --argjson available "$((LINK_SHAPING_AVAILABLE == 1 ? 1 : 0))" \
    --arg reason "$LINK_SHAPING_REASON" \
    --argjson profile_requested "$(printf '%s\n' "${LINK_REQUESTED[@]+"${LINK_REQUESTED[@]}"}" | jq -Rs 'split("\n") | map(select(. != ""))')" \
    --argjson applied "$(printf '%s\n' "${LINK_APPLIED[@]+"${LINK_APPLIED[@]}"}" | jq -Rs 'split("\n") | map(select(. != ""))')" \
    --argjson profiles "$(printf '%s\n' "$@" | jq -Rs 'split("\n") | map(select(. != ""))')" \
    --arg requested "$requested" \
    '{
       iface: (if $iface == "" then null else $iface end),
       shaping: {
         available: ($available == 1),
         reason: (if $reason == "" then null else $reason end),
         requested: $profile_requested,
         applied: $applied
       },
       profiles: $profiles,
       requested: $requested
     }'
}

# link_json <probes-file>: the "link" object a result stores. Always valid JSON,
# even when nothing was probed and nothing was shaped.
link_json() {
  local probes=$1
  local probes_json='[]'
  if [[ -s "$probes" ]]; then
    probes_json=$(jq -s '.' "$probes")
  fi
  jq -n \
    --arg iface "${LINK_IFACE:-}" \
    --argjson available "$((LINK_SHAPING_AVAILABLE == 1 ? 1 : 0))" \
    --arg reason "$LINK_SHAPING_REASON" \
    --argjson requested "$(printf '%s\n' "${LINK_REQUESTED[@]+"${LINK_REQUESTED[@]}"}" | jq -Rs 'split("\n") | map(select(. != ""))')" \
    --argjson applied "$(printf '%s\n' "${LINK_APPLIED[@]+"${LINK_APPLIED[@]}"}" | jq -Rs 'split("\n") | map(select(. != ""))')" \
    --argjson probes "$probes_json" \
    '{
       iface: (if $iface == "" then null else $iface end),
       shaping: {
         available: ($available == 1),
         reason: (if $reason == "" then null else $reason end),
         requested: $requested,
         applied: $applied
       },
       probes: $probes,
       note: "environment describes the runner and is the comparability key; link is measured and varies per run. Under a network ceiling a code delta cannot be interpreted."
     }'
}

# link_markdown <result-json>: the "The link" section of a summary, or nothing
# when the result carries no link object at all.
link_markdown() {
  local result=$1
  jq -e '.link' "$result" >/dev/null 2>&1 || return 0
  echo "### The link"
  echo
  echo "| Profile | When | RTT p50 | RTT p90 | Handshake | Control 1 stream | Control N streams | Host load |"
  echo "|---|---|---|---|---|---|---|---|"
  jq -r '.link.probes[]?
    | "| \(.profile // "-") | \(.at // "-") | \(.rtt_ms.p50 // "-") ms | \(.rtt_ms.p90 // "-") ms "
      + "| \(.handshake_ms // "-") ms | \(.control.single_stream_mib_per_s // "-") MiB/s "
      + "| \(.control.n_stream_mib_per_s // "-") MiB/s "
      + "| \(if .host_load.available then (.host_load.load1 | tostring) else "n/a" end) |"' "$result"
  if [[ "$(jq -r '.link.probes | length' "$result")" == "0" ]]; then
    echo "| - | - | - | - | - | - | - | - |"
    echo
    echo "No link probe ran for this result, so the numbers above cannot be told apart from the line they were measured on."
  fi
  echo
  # Three distinct states, and conflating the first two is exactly the kind of
  # thing that makes a stored result unreadable a month later: nothing was
  # asked for, something was asked for and could not be done, or it was done.
  jq -r '.link.shaping
    | if ((.requested // []) | map(select(test("[+/]"))) | length) == 0 then
        "No link shaping was requested: every profile here is the real line."
      elif .available then
        "Link shaping was available. Requested profiles: \(.requested | join(", ")); applied: \((.applied | join(", ")) // "none")."
      else
        "Link shaping was **not** available (\(.reason // "unknown")), so every profile was measured on the real line and the profile names say what was asked for, not what happened."
      end' "$result"
  echo
  echo "The control measurement uses \`x/crypto/ssh\` and \`pkg/sftp\` directly, never easySFTP's uploader. It separates \"the line is slow\" from \"easySFTP is slow\", and a single-stream control close to a scenario's own MiB/s means the run was network bound, where a code delta says nothing."
  echo
}
