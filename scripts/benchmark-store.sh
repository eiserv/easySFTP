#!/usr/bin/env bash
#
# Store one benchmark result set in benchmarks/.
#
# scripts/benchmark.sh measures and writes results.json plus summary.md into a
# throwaway directory. This script turns such a pair into a permanent entry:
#
#   benchmarks/release-vX.Y.Z.{json,md}          official reference of a release
#   benchmarks/manual-<stamp>-<label>.{json,md}  manual or experimental run
#   benchmarks/latest.{json,md}                  copy of the newest kept release
#   benchmarks/index.json                        machine readable listing
#   benchmarks/archive/                          everything past the keep window
#
# Two rules the callers depend on:
#   - A stored file is never rewritten. Storing a name that already exists
#     fails, in benchmarks/ and in benchmarks/archive/ alike.
#   - latest.* is only ever a copy of a release entry, so a manual run can
#     never overwrite an official number.
#
# Only latest.*, index.json and the archive/ moves change on a later run; the
# historical files themselves are moved, never edited.
#
# Environment:
#   RESULTS_JSON    results.json from scripts/benchmark.sh (required)
#   SUMMARY_MD      summary.md from scripts/benchmark.sh (required)
#   KIND            "release" or "manual" (required)
#   VERSION         vMAJOR.MINOR.PATCH, required when KIND=release
#   LABEL           short slug for a manual run (default "manual")
#   RECORDED_AT     ISO 8601 UTC timestamp (default: now)
#   COMMIT          commit the benchmarked build came from (optional)
#   RUN_URL         workflow run that produced it (optional)
#   BENCH_DIR       target directory (default "benchmarks")
#   KEEP_RELEASES   releases kept outside archive/ (default 5: current plus 4)

set -euo pipefail

BENCH_DIR=${BENCH_DIR:-benchmarks}
KEEP_RELEASES=${KEEP_RELEASES:-5}
RECORDED_AT=${RECORDED_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
LABEL=${LABEL:-manual}
VERSION=${VERSION:-}
COMMIT=${COMMIT:-}
RUN_URL=${RUN_URL:-}

die() {
  echo "::error::$*" >&2
  exit 1
}

[[ -n ${RESULTS_JSON:-} ]] || die "RESULTS_JSON is required"
[[ -n ${SUMMARY_MD:-} ]] || die "SUMMARY_MD is required"
[[ -f $RESULTS_JSON ]] || die "RESULTS_JSON ('$RESULTS_JSON') does not exist"
[[ -f $SUMMARY_MD ]] || die "SUMMARY_MD ('$SUMMARY_MD') does not exist"
jq -e . "$RESULTS_JSON" >/dev/null || die "RESULTS_JSON ('$RESULTS_JSON') is not valid JSON"
[[ $KEEP_RELEASES =~ ^[1-9][0-9]*$ ]] || die "KEEP_RELEASES must be a positive integer, got '$KEEP_RELEASES'"
[[ $RECORDED_AT =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] ||
  die "RECORDED_AT must look like 2026-07-27T12:00:00Z, got '$RECORDED_AT'"

slug=""
case ${KIND:-} in
release)
  # Exactly the tag release-please creates: anything else sorts wrong here and
  # does not line up with .easysftp-version.
  [[ $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
    die "VERSION must be vMAJOR.MINOR.PATCH for a release benchmark, got '$VERSION'"
  stem="release-$VERSION"
  title="release $VERSION"
  kind_note="official reference"
  ;;
manual)
  # LABEL is a workflow input: keep it inside BENCH_DIR, and inside what a
  # Windows checkout can write (no colons, so the timestamp loses them too).
  slug=${LABEL//[^A-Za-z0-9._-]/-}
  [[ -n ${slug//-/} ]] || die "LABEL must contain at least one letter or digit"
  stem="manual-$(printf '%s' "$RECORDED_AT" | tr -d ':-')-$slug"
  title="manual run $slug"
  kind_note="not an official reference"
  VERSION=""
  ;;
*)
  die "KIND must be 'release' or 'manual', got '${KIND:-}'"
  ;;
esac

mkdir -p "$BENCH_DIR"

for existing in "$BENCH_DIR/$stem".{json,md} "$BENCH_DIR/archive/$stem".{json,md}; do
  if [[ -e $existing ]]; then
    die "$existing already exists: stored benchmarks are never rewritten"
  fi
done

# The envelope keeps the measurement verbatim under .benchmark and adds only
# provenance around it, so a reader never has to guess which run a number is.
# The variable is "run_label", not "label": "label" is a jq keyword, and jq 1.6
# (what Debian/Ubuntu LTS and therefore the self-hosted runner ship) rejects
# "$label" in a filter with "unexpected label, expecting IDENT or __loc__".
# The *key* stays "label", which every jq parses.
jq -n \
  --arg kind "$KIND" \
  --arg version "$VERSION" \
  --arg run_label "$slug" \
  --arg recorded_at "$RECORDED_AT" \
  --arg commit "$COMMIT" \
  --arg run_url "$RUN_URL" \
  --slurpfile benchmark "$RESULTS_JSON" \
  '{
     schema_version: 1,
     kind: $kind,
     version: (if $version == "" then null else $version end),
     label: (if $run_label == "" then null else $run_label end),
     official: ($kind == "release"),
     recorded_at: $recorded_at,
     commit: (if $commit == "" then null else $commit end),
     run_url: (if $run_url == "" then null else $run_url end),
     benchmark: $benchmark[0]
   }' >"$BENCH_DIR/$stem.json"

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
  echo
  cat "$SUMMARY_MD"
} >"$BENCH_DIR/$stem.md"

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

archive() {
  local name=$1
  mkdir -p "$BENCH_DIR/archive"
  mv "$BENCH_DIR/$name.json" "$BENCH_DIR/archive/$name.json"
  if [[ -e "$BENCH_DIR/$name.md" ]]; then
    mv "$BENCH_DIR/$name.md" "$BENCH_DIR/archive/$name.md"
  fi
  echo "archived $name"
}

# Releases are kept by version order, not by date: a late benchmark of an old
# release must not push a newer one out of the window.
releases=()
while IFS= read -r version; do
  releases+=("$version")
done < <(list_stems "$BENCH_DIR"/release-v*.json | sed 's/^release-//' | sort -V)

if ((${#releases[@]} > KEEP_RELEASES)); then
  drop=$((${#releases[@]} - KEEP_RELEASES))
  for ((i = 0; i < drop; i++)); do
    archive "release-${releases[i]}"
  done
  releases=("${releases[@]:drop}")
fi

newest=""
cutoff=""
if ((${#releases[@]} > 0)); then
  newest=${releases[-1]}
  cp "$BENCH_DIR/release-$newest.json" "$BENCH_DIR/latest.json"
  cp "$BENCH_DIR/release-$newest.md" "$BENCH_DIR/latest.md"

  kept=()
  for version in "${releases[@]}"; do
    kept+=("$BENCH_DIR/release-$version.json")
  done
  cutoff=$(jq -rs 'map(.recorded_at) | min' "${kept[@]}")
fi

# Manual runs follow the same window: anything recorded before the oldest kept
# release is history too. ISO 8601 UTC compares correctly as a string.
if [[ -n $cutoff && $cutoff != null ]]; then
  while IFS= read -r name; do
    recorded=$(jq -r '.recorded_at // ""' "$BENCH_DIR/$name.json")
    if [[ -n $recorded && $recorded < $cutoff ]]; then
      archive "$name"
    fi
  done < <(list_stems "$BENCH_DIR"/manual-*.json)
fi

# index.json is the entry point for anything that reads this directory without
# opening every file, agents included.
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
      archived: ($path | startswith("archive/")),
      json: $path,
      markdown: ($path | sub("\\.json$"; ".md")),
      median_ms: (.benchmark.results // []
        | map(select(.label == "candidate"))
        | map({key: .scenario, value: .median_ms})
        | from_entries)
    }' "$f"
  done
}

stored=()
for path in "$BENCH_DIR"/release-v*.json "$BENCH_DIR"/manual-*.json \
  "$BENCH_DIR"/archive/release-v*.json "$BENCH_DIR"/archive/manual-*.json; do
  if [[ -e $path ]]; then
    stored+=("$path")
  fi
done

entries "${stored[@]}" | jq -s \
  --arg generated "$RECORDED_AT" \
  --arg latest "$newest" \
  --argjson keep "$KEEP_RELEASES" \
  '{
     schema_version: 1,
     generated_at: $generated,
     generated_by: "scripts/benchmark-store.sh",
     documentation: "benchmarks/README.md",
     keep_releases: $keep,
     latest_release: (if $latest == "" then null else $latest end),
     entries: (sort_by(.recorded_at) | reverse)
   }' >"$BENCH_DIR/index.json"

echo "stored $stem.json and $stem.md in $BENCH_DIR"
