#!/usr/bin/env bash
#
# Store one benchmark result set in benchmarks/.
#
# scripts/benchmark.sh and scripts/benchmark-matrix.sh measure and write their
# results into a throwaway directory. This script turns such a set into a
# permanent entry, filed by kind:
#
#   benchmarks/releases/release-vX.Y.Z.{json,md}       official reference
#   benchmarks/manual/manual-<stamp>-<label>.{json,md} manual or experimental
#   benchmarks/matrix/matrix-<stamp>-<label>.{json,md,csv}  matrix sweep
#   benchmarks/latest.{json,md}                        copy of the newest release
#   benchmarks/index.json                              machine readable listing
#   benchmarks/archive/<kind>/                         everything past the window
#
# Three rules the callers depend on:
#   - A stored file is never rewritten. Storing a name that already exists
#     fails, in the live directory and in the archive alike.
#   - latest.* is only ever a copy of a release entry, so a manual or matrix
#     run can never overwrite an official number. It stays at the top of
#     benchmarks/ so its link never moves.
#   - A matrix run is never official: it sweeps settings that a normal deploy
#     does not use, so it answers a different question than a release number.
#
# Only latest.*, index.json and the archive moves change on a later run; the
# historical files themselves are moved, never edited.
#
# Environment:
#   RESULTS_JSON    results.json / matrix.json from the measuring script
#                   (required unless KIND=reindex)
#   SUMMARY_MD      summary.md / matrix.md from the measuring script
#                   (required unless KIND=reindex)
#   RESULTS_CSV     optional flat export stored next to the pair
#   KIND            "release", "manual", "matrix", or "reindex" to rebuild
#                   index.json and latest.* from what is already on disk
#                   (which is how results moved by hand are picked up)
#   VERSION         vMAJOR.MINOR.PATCH, required when KIND=release
#   LABEL           short slug for a manual or matrix run (default "manual")
#   RECORDED_AT     ISO 8601 UTC timestamp (default: now)
#   COMMIT          commit the benchmarked build came from (optional)
#   RUN_URL         workflow run that produced it (optional)
#   BENCH_DIR       target directory (default "benchmarks")
#   KEEP_RELEASES   releases kept outside the archive (default 5: current plus 4)

set -euo pipefail

BENCH_DIR=${BENCH_DIR:-benchmarks}
KEEP_RELEASES=${KEEP_RELEASES:-5}
RECORDED_AT=${RECORDED_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
LABEL=${LABEL:-manual}
VERSION=${VERSION:-}
COMMIT=${COMMIT:-}
RUN_URL=${RUN_URL:-}
RESULTS_CSV=${RESULTS_CSV:-}

die() {
  echo "::error::$*" >&2
  exit 1
}

[[ $KEEP_RELEASES =~ ^[1-9][0-9]*$ ]] || die "KEEP_RELEASES must be a positive integer, got '$KEEP_RELEASES'"
[[ $RECORDED_AT =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] ||
  die "RECORDED_AT must look like 2026-07-27T12:00:00Z, got '$RECORDED_AT'"

if [[ ${KIND:-} != reindex ]]; then
  [[ -n ${RESULTS_JSON:-} ]] || die "RESULTS_JSON is required"
  [[ -n ${SUMMARY_MD:-} ]] || die "SUMMARY_MD is required"
  [[ -f $RESULTS_JSON ]] || die "RESULTS_JSON ('$RESULTS_JSON') does not exist"
  [[ -f $SUMMARY_MD ]] || die "SUMMARY_MD ('$SUMMARY_MD') does not exist"
  jq -e . "$RESULTS_JSON" >/dev/null || die "RESULTS_JSON ('$RESULTS_JSON') is not valid JSON"
  [[ -z $RESULTS_CSV || -f $RESULTS_CSV ]] || die "RESULTS_CSV ('$RESULTS_CSV') does not exist"
fi

# slugify <text>: keeps a workflow input inside BENCH_DIR and inside what a
# Windows checkout can write (no colons, so the timestamp loses them too).
slugify() {
  local slug=${1//[^A-Za-z0-9._-]/-}
  [[ -n ${slug//-/} ]] || die "LABEL must contain at least one letter or digit"
  printf '%s' "$slug"
}

slug=""
stem=""
subdir=""
case ${KIND:-} in
reindex)
  # Store nothing; only the bookkeeping at the bottom runs.
  ;;
release)
  # Exactly the tag release-please creates: anything else sorts wrong here and
  # does not line up with .easysftp-version.
  [[ $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
    die "VERSION must be vMAJOR.MINOR.PATCH for a release benchmark, got '$VERSION'"
  subdir=releases
  stem="release-$VERSION"
  title="release $VERSION"
  kind_note="official reference"
  ;;
manual)
  slug=$(slugify "$LABEL")
  subdir=manual
  stem="manual-$(printf '%s' "$RECORDED_AT" | tr -d ':-')-$slug"
  title="manual run $slug"
  kind_note="not an official reference"
  VERSION=""
  ;;
matrix)
  slug=$(slugify "$LABEL")
  subdir=matrix
  stem="matrix-$(printf '%s' "$RECORDED_AT" | tr -d ':-')-$slug"
  title="connections/concurrency matrix $slug"
  kind_note="a settings sweep, never an official reference"
  VERSION=""
  ;;
*)
  die "KIND must be 'release', 'manual', 'matrix' or 'reindex', got '${KIND:-}'"
  ;;
esac

if [[ -n $subdir ]]; then
  mkdir -p "$BENCH_DIR/$subdir"

  for existing in "$BENCH_DIR/$subdir/$stem".{json,md,csv} "$BENCH_DIR/archive/$subdir/$stem".{json,md,csv}; do
    if [[ -e $existing ]]; then
      die "$existing already exists: stored benchmarks are never rewritten"
    fi
  done
fi

# The envelope keeps the measurement verbatim under .benchmark and adds only
# provenance around it, so a reader never has to guess which run a number is.
# The variable is "run_label", not "label": "label" is a jq keyword, and jq 1.6
# (what Debian/Ubuntu LTS and therefore the self-hosted runner ship) rejects
# "$label" in a filter with "unexpected label, expecting IDENT or __loc__".
# The *key* stays "label", which every jq parses.
#
# schema_version 2 is the envelope's own version: the directory layout gained a
# level and the entry now records which measuring script produced it. Envelopes
# already stored as version 1 keep their number and stay readable; .benchmark
# carries its own schema_version.
if [[ -n $subdir ]]; then
  jq -n \
    --arg kind "$KIND" \
    --arg version "$VERSION" \
    --arg run_label "$slug" \
    --arg recorded_at "$RECORDED_AT" \
    --arg commit "$COMMIT" \
    --arg run_url "$RUN_URL" \
    --slurpfile benchmark "$RESULTS_JSON" \
    '{
     schema_version: 2,
     kind: $kind,
     version: (if $version == "" then null else $version end),
     label: (if $run_label == "" then null else $run_label end),
     official: ($kind == "release"),
     recorded_at: $recorded_at,
     commit: (if $commit == "" then null else $commit end),
     run_url: (if $run_url == "" then null else $run_url end),
       benchmark: $benchmark[0]
     }' >"$BENCH_DIR/$subdir/$stem.json"

  {
    echo "# easySFTP benchmark: $title"
    echo
    echo "| Field | Value |"
    echo "|---|---|"
    echo "| Kind | $KIND ($kind_note) |"
    if [[ -n $VERSION ]]; then echo "| Version | \`$VERSION\` |"; fi
    echo "| Recorded | $RECORDED_AT |"
    if [[ -n $COMMIT ]]; then echo "| Commit | \`$COMMIT\` |"; fi
    if [[ -n $RUN_URL ]]; then echo "| Workflow run | $RUN_URL |"; fi
    echo "| Raw data | [$stem.json]($stem.json) |"
    if [[ -n $RESULTS_CSV ]]; then echo "| Flat export | [$stem.csv]($stem.csv) |"; fi
    echo
    cat "$SUMMARY_MD"
  } >"$BENCH_DIR/$subdir/$stem.md"

  if [[ -n $RESULTS_CSV ]]; then
    cp "$RESULTS_CSV" "$BENCH_DIR/$subdir/$stem.csv"
  fi
fi

# list_stems <path>...: base names without the .json suffix, one per line.
# Unmatched globs arrive as their own pattern and are skipped.
list_stems() {
  local f base
  for f in "$@"; do
    [[ -e $f ]] || continue
    base=${f##*/}
    echo "${base%.json}"
  done
}

# archive <kind-subdir> <stem>: moves an entry and everything belonging to it
# into the archive, keeping the per-kind subdirectory.
archive() {
  local dir=$1 name=$2 ext
  mkdir -p "$BENCH_DIR/archive/$dir"
  mv "$BENCH_DIR/$dir/$name.json" "$BENCH_DIR/archive/$dir/$name.json"
  for ext in md csv; do
    if [[ -e "$BENCH_DIR/$dir/$name.$ext" ]]; then
      mv "$BENCH_DIR/$dir/$name.$ext" "$BENCH_DIR/archive/$dir/$name.$ext"
    fi
  done
  echo "archived $dir/$name"
}

# Releases are kept by version order, not by date: a late benchmark of an old
# release must not push a newer one out of the window.
releases=()
while IFS= read -r version; do
  releases+=("$version")
done < <(list_stems "$BENCH_DIR"/releases/release-v*.json | sed 's/^release-//' | sort -V)

if ((${#releases[@]} > KEEP_RELEASES)); then
  drop=$((${#releases[@]} - KEEP_RELEASES))
  for ((i = 0; i < drop; i++)); do
    archive releases "release-${releases[i]}"
  done
  releases=("${releases[@]:drop}")
fi

newest=""
cutoff=""
if ((${#releases[@]} > 0)); then
  newest=${releases[-1]}
  cp "$BENCH_DIR/releases/release-$newest.json" "$BENCH_DIR/latest.json"
  cp "$BENCH_DIR/releases/release-$newest.md" "$BENCH_DIR/latest.md"

  kept=()
  for version in "${releases[@]}"; do
    kept+=("$BENCH_DIR/releases/release-$version.json")
  done
  cutoff=$(jq -rs 'map(.recorded_at) | min' "${kept[@]}")
fi

# Manual and matrix runs follow the same window: anything recorded before the
# oldest kept release is history too. ISO 8601 UTC compares correctly as a
# string.
if [[ -n $cutoff && $cutoff != null ]]; then
  for dir in manual matrix; do
    while IFS= read -r name; do
      recorded=$(jq -r '.recorded_at // ""' "$BENCH_DIR/$dir/$name.json")
      if [[ -n $recorded && $recorded < $cutoff ]]; then
        archive "$dir" "$name"
      fi
    done < <(list_stems "$BENCH_DIR/$dir/$dir"-*.json)
  done
fi

# index.json is the entry point for anything that reads this directory without
# opening every file, agents included. Every entry carries the paths of its
# files, so a consumer never has to reconstruct the layout.
entries() {
  local f rel
  for f in "$@"; do
    rel=${f#"$BENCH_DIR"/}
    jq -c --arg path "$rel" '{
      kind: .kind,
      version: .version,
      label: .label,
      official: .official,
      recorded_at: .recorded_at,
      commit: .commit,
      run_url: .run_url,
      candidate_ref: (.benchmark.candidate_ref // null),
      benchmark_kind: (.benchmark.benchmark_kind // "standard"),
      benchmark_schema_version: (.benchmark.schema_version // 1),
      runner: (.benchmark.runner // null),
      environment: (.benchmark.environment // null),
      # The link the result was measured over (issue #184). environment says
      # which machine, this says which path, and without the second one two
      # results from different days are not comparable even when the first
      # matches. Empty on everything measured before the probe existed.
      link_profiles: (.benchmark.link.shaping.requested // []),
      # The opening probe of the baseline profile, i.e. the real line as the run
      # found it. Here so that "was that release measured on a slower line" can
      # be answered without opening every file.
      rtt_p50_ms: (((.benchmark.link.probes // [])
        | map(select(.at == "start"))
        | (map(select(.profile == "baseline")) + .)
        | map(.rtt_ms.p50) | map(select(. != null)) | first) // null),
      archived: ($path | startswith("archive/")),
      json: $path,
      markdown: ($path | sub("\\.json$"; ".md")),
      # The candidate medians per scenario, so a trend over releases can be
      # plotted straight from this file. A matrix entry has no single median
      # per scenario (that is the point of a sweep), so it reports its best
      # cell instead.
      median_ms: (if (.benchmark.benchmark_kind // "standard") == "matrix" then {}
        else (.benchmark.results // []
          | map(select(.label == "candidate"))
          | map({key: .scenario, value: .median_ms})
          | from_entries) end),
      best_ms: (if (.benchmark.benchmark_kind // "standard") == "matrix"
        then (.benchmark.scaling // []
          | map(select(.label == "candidate"))
          | map({key: .scenario, value: .best.median_ms})
          | from_entries)
        else {} end)
    }' "$f"
  done
}

stored=()
for path in "$BENCH_DIR"/releases/release-v*.json "$BENCH_DIR"/manual/manual-*.json \
  "$BENCH_DIR"/matrix/matrix-*.json \
  "$BENCH_DIR"/archive/releases/release-v*.json "$BENCH_DIR"/archive/manual/manual-*.json \
  "$BENCH_DIR"/archive/matrix/matrix-*.json; do
  if [[ -e $path ]]; then
    stored+=("$path")
  fi
done

entries "${stored[@]}" | jq -s \
  --arg generated "$RECORDED_AT" \
  --arg latest "$newest" \
  --argjson keep "$KEEP_RELEASES" \
  '{
     schema_version: 2,
     generated_at: $generated,
     generated_by: "scripts/benchmark-store.sh",
     documentation: "benchmarks/README.md",
     layout: {
       releases: "releases/release-vX.Y.Z.{json,md}",
       manual: "manual/manual-<stamp>-<label>.{json,md}",
       matrix: "matrix/matrix-<stamp>-<label>.{json,md,csv}",
       archive: "archive/<kind>/<same names>",
       latest: "latest.{json,md}"
     },
     keep_releases: $keep,
     latest_release: (if $latest == "" then null else $latest end),
     entries: (sort_by(.recorded_at) | reverse)
   }' >"$BENCH_DIR/index.json"

# trend.csv: one row per (stored standard result, scenario, link profile), so a
# plot of runtime and throughput across releases needs neither a JSON parser nor
# a pass over every file. index.json carries the medians but not the throughput,
# which is exactly the second axis such a plot wants. Matrix results are left
# out: their point is that they have no single number per scenario.
#
# link_profile, rtt_p50_ms and control_single_mib_per_s ride along because a
# trend line across months is otherwise a trend of the line as much as of the
# code. A result measured before the probe existed has one row per scenario with
# the profile empty, exactly as before.
{
  echo '"recorded_at","kind","version","label","candidate_ref","runner","scenario","link_profile","rtt_p50_ms","control_single_mib_per_s","files","bytes","median_ms","min_ms","max_ms","mad_ms","mib_per_s","files_per_s","max_rss_bytes","user_cpu_ms","archived","json"'
  for path in "${stored[@]}"; do
    jq -r --arg path "${path#"$BENCH_DIR"/}" '
      select((.benchmark.benchmark_kind // "standard") != "matrix")
      | . as $e
      | (.benchmark.link.probes // []) as $probes
      | (.benchmark.results // [])[]
      | select(.label == "candidate")
      | . as $row
      | ([$probes[] | select(.profile == ($row.link_profile // "baseline") and .at == "start")] | first) as $p
      | [$e.recorded_at, $e.kind, $e.version, $e.label,
         ($e.benchmark.candidate_ref // null), ($e.benchmark.runner // null),
         .scenario, (.link_profile // null),
         ($p.rtt_ms.p50 // null), ($p.control.single_stream_mib_per_s // null),
         .files, .bytes, .median_ms, .min_ms, .max_ms,
         (.mad_ms // null), (.mib_per_s // null), (.files_per_s // null),
         (.process.max_rss_bytes // null), (.process.user_cpu_ms // null),
         ($path | startswith("archive/")), $path]
      | @csv' "$path"
  done
} >"$BENCH_DIR/trend.csv"

if [[ -n $subdir ]]; then
  echo "stored $subdir/$stem.json and $subdir/$stem.md in $BENCH_DIR"
else
  echo "reindexed $BENCH_DIR: ${#stored[@]} entr(y|ies)"
fi
