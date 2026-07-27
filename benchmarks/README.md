# Benchmarks

Upload performance of easySFTP against a real SFTP server, measured by
[`scripts/benchmark.sh`](../scripts/benchmark.sh) (throughput at the default
settings) and [`scripts/benchmark-matrix.sh`](../scripts/benchmark-matrix.sh)
(a connections/concurrency sweep), and filed here by
[`scripts/benchmark-store.sh`](../scripts/benchmark-store.sh).

These numbers set no threshold and fail no build. They exist to see where the
time goes (issues #158 and #169), so read them as one host's behaviour on
one day, not as a guarantee.

## Where to look first

| You want | Read |
|---|---|
| The current official numbers | `latest.md` (`latest.json` for the raw data) |
| A listing of everything stored here | `index.json` |
| One specific release | `releases/release-vX.Y.Z.md` / `.json` |
| A connections/concurrency sweep | `matrix/matrix-*.md` / `.json` / `.csv` |
| Runtime and throughput across releases | `trend.csv` |
| An older result | `archive/<kind>/…` |

## Layout

```text
benchmarks/
  latest.json                         copy of the newest official release result
  latest.md                           the same, human readable
  index.json                          every stored result, machine readable
  trend.csv                           one flat row per stored result and scenario
  releases/
    release-vX.Y.Z.json               official reference of a release
    release-vX.Y.Z.md
    release-vX.Y.Z.csv                flat per-scenario export
  manual/
    manual-<UTC stamp>-<label>.json   manual or experimental run
    manual-<UTC stamp>-<label>.md
  matrix/
    matrix-<UTC stamp>-<label>.json   connections/concurrency sweep
    matrix-<UTC stamp>-<label>.md
    matrix-<UTC stamp>-<label>.csv    one flat row per cell
  archive/
    releases/  manual/  matrix/       the same names, past the keep window
```

`latest.json` and `latest.md` stay at the top level on purpose: they are the
one pair of links that must never move. Everything else lives under its kind.
Results measured before this layout existed sat directly in `benchmarks/`; they
were moved into `releases/` and `manual/`, contents untouched.

## The stored files

Every result is a pair (a matrix run is a triple, with its CSV). The JSON wraps
the measurement verbatim under `.benchmark` and adds provenance around it:

```jsonc
{
  "schema_version": 2,      // of this envelope; 1 predates the kind subdirectories
  "kind": "release",        // or "manual" or "matrix"
  "version": "v3.3.2",      // null unless kind is "release"
  "label": null,            // the manual/matrix run's label, null for releases
  "official": true,         // false for everything that is not a release
  "recorded_at": "2026-07-27T12:00:00Z",
  "commit": "…",
  "run_url": "…",
  "benchmark": { /* results.json or matrix.json, with its own schema_version */ }
}
```

### What `.benchmark` holds (standard runs, `schema_version` 2)

Separated by what a reader is after, so nothing has to be recomputed from
Markdown:

| Key | What it is |
|---|---|
| `candidate_ref`, `baseline_ref`, `repeats`, `runner`, `settings` | what was measured and how |
| `environment` | OS, kernel, architecture, CPU model, CPU count, Go version |
| `scenarios` | the payload behind each scenario name |
| `results[]` | one row per (build, scenario): the aggregate |
| `results[].duration_ms` | `values`, `median`, `min`, `max`, `mad`, `samples` |
| `results[].process` | CPU time, peak RSS, Go allocations, GC count and pause, peak goroutines, disk and network bytes |
| `results[].phases[]` | wall clock per phase (connect, local_scan, remote_scan, hash, create_dirs, upload, delete_sweep, manifest_read, manifest_write, prune_dirs, cleanup) |
| `results[].operations[]` | per round-trip: count, cumulative total, average, p50/p90/p99, max, errors |
| `results[].counters` | connections opened/used/refused, reconnects, retries, stalls, errors |
| `comparison[]` | each build against the reference build: `delta_ms`, `delta_percent`, and `within_noise` (is the delta smaller than the reference's MAD?) |
| `runs[]` | every individual repeat, verbatim, including its own metrics document |

**Phases are wall clock. Operations are not.** A phase adds up to roughly the
run's duration. An operation total is cumulative across the parallel upload
workers, so it is normally *larger* than the phase it happened in; read it for
its share of the work and its per-call cost, never as elapsed time. The
`note` field in every result says the same thing.

`schema_version` 1 results (up to and including v3.3.2) carry only
`candidate_ref`, `baseline_ref`, `repeats`, `runner`, `settings`, `scenarios`
and a smaller `results[]`. Every key they had still exists and still means the
same thing in version 2.

### What `.benchmark` holds (matrix runs)

| Key | What it is |
|---|---|
| `axes` | the grid, declared: `connections`, `concurrency`, `request_concurrency` |
| `cells[]` | one row per (scenario, build, connections, concurrency, request_concurrency) with duration, throughput, files/s, CPU, peak RSS, connections opened/used/refused, reconnects, retries, errors |
| `scaling[]` | the same cells pre-grouped per scenario and build, ordered along the axes, plus the `best` cell |
| `comparison[]` | candidate against baseline at identical coordinates |

The CSV next to it is the same `cells[]` flattened, one row per cell, which is
what a heatmap or a scaling plot reads.

### Where the instrumentation comes from

The measured runs set `EASYSFTP_METRICS_FILE`; easySFTP then writes its phase,
operation, counter and process metrics there (see
[`internal/metrics`](../internal/metrics)). The variable is not an `action.yml`
input and has no effect on a normal deploy: with it unset, the instrumentation
is inert and the connection is not even wrapped in its byte counter.

### The two top-level exports

`index.json` lists every result newest first, with `kind`, `official`,
`archived`, `benchmark_kind`, the environment, the paths of all its files, and
the candidate's median milliseconds per scenario (or, for a matrix run, its
best cell), so a reader does not have to open every file.

`trend.csv` is the same set flattened for plotting: one row per stored
non-matrix result and scenario, carrying the timestamp, the version, the
runner, the duration statistics **and the throughput**, which `index.json`
deliberately does not. It is what a "runtime and throughput over releases"
chart reads. Both files are regenerated in full on every store, so they always
describe what is actually on disk. Columns whose data predates the metric
(peak RSS and CPU time on schema 1 results) are empty rather than zero.

## Official versus manual versus matrix

- **Official** results are `kind: "release"`. They are measured automatically
  after release-please created a tag, against exactly that tag, and they are
  the only results `latest.*` is ever copied from. Their files are also
  attached to that GitHub Release as `benchmark-vX.Y.Z.*`, so the numbers sit
  next to the binaries they describe.
- **Manual** results are `kind: "manual"`, produced by a manually started run
  (a branch, a tag, or a pull request). They are kept alongside the official
  ones and are clearly named, but they never become a reference.
- **Matrix** results are `kind: "matrix"`. They sweep settings a normal deploy
  does not use, so they answer a different question than a release number does
  and can never become one.
- Release PRs are benchmarked once when they open, so the numbers are visible
  before the merge. That run stores nothing: its result belongs to a release
  that does not exist yet. The official file is the post-tag measurement.

## Where the numbers were measured

The benchmark runs on a fixed self-hosted runner, not on a GitHub-hosted one:
those land in a changing region on changing hardware, which puts the machine
into every delta between two releases. Each result records where it ran in its
`runner` field (and, from schema 2 on, in the richer `environment` object), and
two results are only comparable when those match.

Results up to and including v3.3.1 predate this and name only the kernel and
CPU count (`Linux 6.17.0-1020-azure, 4 cpu`); every one of them was measured on
a GitHub-hosted runner. From v3.3.2 on the field starts with `self-hosted`.

## Reading a delta

Every repeat's individual value is kept, alongside the median, min, max and the
**median absolute deviation** (MAD). MAD is the spread metric to compare a
delta against: a single slow repeat, which is the normal failure mode of a
shared host, moves it far less than it moves a standard deviation. A candidate
whose delta is smaller than the baseline's MAD has not been shown to be
faster or slower, and `comparison[].within_noise` says so directly.

There are deliberately no thresholds here and nothing fails a build. The data
format is meant to make automatic regression detection possible later, not to
pretend it is already reliable.

## Retention

The current release and the four before it stay in `releases/`. Older releases
move to `archive/releases/`, and manual and matrix runs move with them once
they are older than the oldest kept release. Nothing is ever deleted or
rewritten: storing a name that already exists fails on purpose, so a number
that was once published stays exactly as it was published.

If results are ever moved by hand, `KIND=reindex bash scripts/benchmark-store.sh`
rebuilds `index.json` and `latest.*` from whatever is on disk without storing
anything new.

## Running a benchmark

The **`SFTP benchmark`** workflow
([`.github/workflows/benchmark.yml`](../.github/workflows/benchmark.yml)) runs
the standard measurement. Start it manually from the Actions tab with a `pr`
number to benchmark a pull request, or a `candidate-ref` plus an optional
`baseline-ref` to compare two refs in the same run. A manual run needs an
approval on the `benchmark` environment first, which only a maintainer can give.

Setting `connections` measures a third build labelled `poolN`: the candidate
binary again, this time with `advanced.connections: N` (issue #158). It runs
interleaved with the others, since two numbers from two separate runs against a
shared host are not comparable. The `Delta` column then compares every build
against the baseline, or against the candidate when no baseline was given.

If a release ended up without its official result, the same workflow repairs
it: set `release-version` to that tag (for example `v3.3.0`). The run measures
the tag and stores it as the official reference, and it fails rather than
touch a reference that already exists. Everything else is a manual run and
stays one.

The **`SFTP benchmark matrix`** workflow
([`.github/workflows/benchmark-matrix.yml`](../.github/workflows/benchmark-matrix.yml))
runs the sweep. Its `connections` and `concurrency` inputs are the axes, and
`request-concurrency` adds an optional third one. Candidate and baseline are
measured cell by cell, back to back, for the same reason the standard benchmark
interleaves its repeats. The default grid is over a hundred measured runs and
takes hours; shrink the axes for a quick look.

### About `connections` above `concurrency`

The sweep deliberately includes cells where `connections > concurrency`, and
easySFTP deliberately does not honour them: `poolSize` in
[`internal/uploader/session.go`](../internal/uploader/session.go) caps the pool
at the concurrency and says so in the log. Only the per-file upload path spreads
over the pool, and at most `concurrency` files are ever in flight, so a
connection past that number buys a handshake and no parallelism. Those cells are
in the grid so the data shows that flattening rather than leaving a gap where a
reader has to take the cap on trust.
