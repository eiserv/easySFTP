# Benchmarks

Upload performance of easySFTP against a real SFTP server, measured, aggregated
and filed here by [`cmd/easysftp-bench`](../cmd/easysftp-bench):

| Command | What it does |
|---|---|
| `easysftp-bench standard` | throughput at the default settings |
| `easysftp-bench matrix` | a connections/concurrency sweep |
| `easysftp-bench aggregate` | one measured run into the documents below |
| `easysftp-bench store` | files a result set in this directory |

Issue #190 moved that work off the shell in steps, and the last of them removed
the scripts. The documents themselves did not change with the move: before the
shell went, both implementations were run against the same stubbed inputs and
their measurements and outputs diffed, so every result stored here reads the
same way whichever implementation wrote it.

The analysis in [`analysis/`](analysis/) reads these files and is optional; the
commands above need nothing from it. It plots the sweep as a grid and as
curves, where a deployment's wall clock goes, what the round-trips cost, the
delete sweeps, the measurement against the link's own control, and what
easySFTP picks for itself against what a sweep would have picked.

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
| A sweep as a picture, where the time went, what auto picked | `analysis/README.md` |

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
  analysis/                           optional plots of all of the above
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
| `link` | the network path: probed RTT, a throughput control, the server's load, and which shaping was asked for (see "The link profile" below) |
| `scenarios` | the payload behind each scenario name |
| `results[]` | one row per (build, scenario, link profile): the aggregate |
| `results[].link_profile` | which link profile the row was measured over; `baseline` means the real line |
| `results[].duration_ms` | `values`, `median`, `min`, `max`, `mad`, `samples` |
| `results[].process` | CPU time, peak RSS, Go allocations, GC count and pause, peak goroutines, disk and network bytes |
| `results[].phases[]` | wall clock per phase (connect, local_scan, remote_scan, hash, create_dirs, sweep_stale_temps, upload, delete_sweep, manifest_read, manifest_write, prune_dirs, cleanup) |
| `results[].operations[]` | per round-trip: count, cumulative total, average, p50/p90/p99, max, errors |
| `results[].counters` | connections opened/used/refused, reconnects, retries, stalls, errors, the settings the run used (`config_*`), what the policy first picked (`auto_initial_*`), how it moved afterwards (`auto_changes`, `auto_spread_increases`, `auto_spread_decreases`, `auto_final_connections`), the features it read (`workload_*`, including `workload_p50_bytes`, `workload_p90_bytes` and `workload_small_files`) and the link it measured (`link_rtt_us`, `link_handshake_us`, `link_stream_bytes_per_second`, `link_bdp_bytes`) |
| `comparison[]` | each build against the reference build: `delta_ms`, `delta_percent`, and `within_noise` (is the delta smaller than the reference's MAD?) |
| `deletes[]` | the delete sweeps, one row per (build, scenario, link profile); see "The delete sweeps" below |
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
| `axes` | the grid as requested: `link_profiles`, `connections`, `concurrency`, `request_concurrency` |
| `axes.per_scenario` | the grid as measured, per scenario, plus the `files` count it was capped against; see "The per-scenario grid" below |
| `link` | the same object a standard run carries, one probe pair per swept profile |
| `cells[]` | one row per (link profile, scenario, build, connections, concurrency, request_concurrency) with duration, throughput, files/s, network bytes, CPU, peak RSS, connections opened/used/refused, reconnects, retries, errors |
| `cells[].request_concurrency_used` | the value the run actually ran with, read from its counters; `request_concurrency` is null on the pass that sets nothing, and that null alone does not say which value it was |
| `cells[].phases[]` | wall clock per phase, the same list a standard run's `results[].phases[]` carries |
| `cells[].operations[]` | per round-trip: count, cumulative total, average, p50/p90/p99, max, errors |
| `scaling[]` | the same cells pre-grouped per link profile, scenario and build, ordered along the axes, plus the `best` cell |
| `scaling[].best_at_axis_max` | the axes whose largest swept value *is* the best cell, i.e. where the optimum was cut off rather than measured |
| `auto[]` | the settings easySFTP picked for itself, and the regret against the best cell; see "What `auto` costs" below |
| `comparison[]` | candidate against baseline at identical coordinates, **including the same link profile** |
| `canary[]` | the fixed cell, measured at the `start`, the `mid` and the `end` of each profile's grid |
| `deletes[]` | the delete sweeps, one row per cell coordinate; see "The delete sweeps" below |

A matrix run has no `runs[]`: a cell is its finest grain, which is why the cell
itself carries the phase and round-trip detail rather than only the
`upload_phase_ms` it used to keep (issue #184, phase 2). Results stored before
that change have `upload_phase_ms` alone.

The CSV next to it is the same `cells[]` flattened, one row per cell, which is
what a heatmap or a scaling plot reads. The nested `phases[]` and
`operations[]` stay out of it and live in the JSON only.

### The per-scenario grid

Up to issue #184 phase 5 every scenario was swept over the same axes, and the
stored results show what that bought: `single` (one 32 MiB file) has 30 cells
holding the same 0.38 MiB/s, because one file cannot be spread over connections
or over workers. Meanwhile the optimum of every small-file scenario sat at
`concurrency: 32`, the largest value swept, so the grid could report a boundary
and never an optimum.

The axes are therefore capped against the payload, per scenario, by
`scenario.AxisFor`
([`internal/benchmark/scenario`](../internal/benchmark/scenario)):

- **A value above the file count is dropped** (capped and deduplicated). Only
  the per-file upload path spreads over connections and workers, so at most
  *file count* files are ever in flight; measuring `concurrency: 16` on a
  one-file payload measures `concurrency: 1` under another name.
- **`request_concurrency` is only swept where a file is at least 1 MiB.** It is
  `sftp.MaxConcurrentRequestsPerFile`, how many write requests of *one* file may
  be in flight. pkg/sftp writes 32 KiB packets, so a 4 KiB file is a single
  packet and a value above the default of 16 needs at least 32 packets before it
  has anything left to pipeline.

What that pays for is the two things phase 5 asks for: a `concurrency` axis that
runs to 64 so the optimum can be interior, and a `request_concurrency` axis that
is swept for real instead of being declared and left empty. `axes` keeps what
was requested, `axes.per_scenario` records what each scenario was actually
measured over, and `matrix.md` prints both next to the scenario table. A cell
missing from the declared grid was not skipped; it would have been a duplicate.

`scaling[].best_at_axis_max` is the honesty check on top: it names the axes
whose largest swept value is the best cell. Where it is non-empty the optimum is
at or beyond the edge of the sweep, and anything fitted to those numbers
extrapolates.

### What `auto` costs

Every sweep also measures one run per scenario and link profile with
`connections`, `concurrency` and `request_concurrency` all set to `auto`, on the
candidate build, and scores it against the grid:

| Field | What it is |
|---|---|
| `chosen` | the `connections`, `concurrency` and `request_concurrency` the run ended up using, read from its own counters rather than assumed |
| `chosen.initial_*`, `chosen.changes` | what the policy picked before the transfer taught it anything, and how many times it moved the connection spread while the transfer ran; equal to `chosen` on a run the runtime stage left alone |
| `chosen.spread_increases`, `chosen.spread_decreases`, `chosen.final_connections` | which way those changes went and where the run settled. A growth step that does not pay for itself is taken back without closing anything (issue #215), so the plain `connections` above is a high-water mark and this is the number the last deployment ran on |
| `connections_opened`, `connections_used`, `connections_refused` | what the pool actually was: the handshakes the run paid, the slots a file was really handed to, and whether the server turned one down. A cell has carried these since #190; an auto row did not, which is how a spread nothing read could pass for a configuration that was measured (issue #217) |
| `workload` | the features the policy was looking at: `files`, `bytes`, `largest_bytes`, `probes`, the shape of the set (`p50_bytes`, `p90_bytes`, `small_files`, the files that fit in one 32 KiB write packet), and the link it measured itself (`rtt_ms`, `handshake_ms`, and `stream_bytes_per_second` / `bdp_bytes` where a throughput was known) |
| `median_ms`, `min_ms`, `max_ms`, `mad_ms`, `durations_ms`, `mib_per_s`, `files_per_s` | the same statistics a cell carries |
| `best` | the fastest candidate cell of that scenario and profile |
| `regret_ms`, `regret_percent` | the gap: how much slower the policy is than the settings a sweep would have picked |
| `chosen_in_grid`, `chosen_cell_median_ms` | the same coordinates measured as an ordinary cell, when the grid contains them |

`workload` and the `initial_*` fields are null in every result stored before
issue #209, where `auto` was a fixed 1/4/16 and there was nothing to record.
`workload.rtt_ms` is easySFTP's own probe, taken on its first connection, and is
a different measurement from `link.probes[]`, which comes from `cmd/linkprobe`
and never goes through the uploader. Both are kept, because the interesting case
is the one where they disagree.

Five things about it:

- **It is not a cell.** `auto` does not sit at a coordinate, it chooses one, so
  it stays out of `cells[]`, `scaling[]`, `comparison[]` and the CSV. A build
  label in the grid would have measured the same policy at every coordinate.
- **`chosen_cell_median_ms` is a control, not a result.** It is the same
  settings measured a second time, as a cell. A large gap between it and the
  `auto` run means the two saw different conditions, and the regret next to it
  is then drift rather than policy.
- **The regret is per link profile on purpose.** `RTT`, the per-connection
  operation ceiling and the per-connection bandwidth are properties of one line
  (see "The link profile"), and a policy that is only good on the benchmark
  host's line is the failure class of #62 all over again. A policy within ~15%
  of optimum on every profile is defensible; one that only wins here is not.
- **The grid cannot score the runtime stage.** A cell is a fixed configuration
  measured from its first byte, so there is nothing in it for the controller to
  react to. What `chosen.changes` records is that the controller acted; whether
  it acted well shows up in `median_ms` and nowhere else.
- **`chosen.connections` is a setting, `connections_used` is an outcome.** A
  file takes its connection when its worker picks it up and keeps it for the
  whole transfer, so a spread raised after the last file started is a number no
  transfer read. Where `connections_used` is below `chosen.connections` the
  control cell is not the same configuration as the `auto` run and the regret
  next to it is not measuring the policy: that is what the `mixed` row of
  `matrix-20260821T123508Z-v3.6.0` turned out to be (issue #217). easySFTP no
  longer takes such a step, so the two should now agree unless the server
  refused a connection.

The policy itself lives in `internal/autotune` (issues #209 and #215), and its own test
replays it against every sweep committed here and fails when the regret goes
over the target. That test is the fast feedback loop; the sweep is the
measurement it is fitted to.

### The canary

A sweep runs for hours against a server that is fixed but not constant, and
nothing in a grid says whether the machine that measured the last cell was still
the machine that measured the first. So one fixed cell (`small`, `connections: 1`,
`concurrency: 4`, candidate build) is measured three times per link profile: at
the start of that profile's grid, in the middle of it, and at the end. All three
are stored, and `matrix.md` reports them with the spread between the fastest and
the slowest.

Read it before reading the grid: when the spread is larger than the deltas in
the grid, the run measured drift and not settings. The cell is a constant on
purpose, so canaries are comparable both within one run and across runs.

The middle canary needs a middle to sit in; a grid of a single measured run per
profile only gets a start and an end.

### The delete sweeps

Both scripts have always run a `clean` deployment of an empty directory before
every measured run, so that each run starts from the same empty remote
directory. That pre-clean *is* a pure delete sweep, and until issue #184 phase 4
its numbers were thrown away. They are now kept, in a `deletes[]` block of their
own: no additional run, no additional minute, and the only place deletions are
measured at all.

| Field | What it is |
|---|---|
| the coordinates | build, scenario and link profile; a matrix row adds `connections`, `concurrency` and `request_concurrency`, the same coordinates its cell has |
| `sweeps`, `failed_sweeps` | how many sweeps the row aggregates, and how many of them exited non-zero |
| `files_deleted` | how many entries the sweep found to delete |
| `median_ms`, `min_ms`, `max_ms`, `mad_ms`, `durations_ms`, `deletes_per_s` | the same statistics an upload row carries |
| `phases[]` | wall clock, `remote_scan` and `delete_sweep` |
| `operations[]` | per round-trip, `sftp_remove` and `sftp_rmdir` with count, cumulative total, p50/p90/p99 and max |

Read the round-trips against issue #157: a deletion is one round-trip per entry
and nothing spreads them over the connection pool, so a matrix row's
`sftp_remove` count and percentiles at different `connections` are what turns
that from a plausible claim into a measured one.

Two things kept deliberately separate:

- **A sweep that found nothing is not counted.** The first pre-clean of a build
  and scenario runs against an empty remote directory and measures the scan
  alone; a median over that and a real sweep describes neither. A coordinate
  left with no sweep at all has no row.
- **Delete numbers never touch upload numbers.** They are aggregated from their
  own metrics files into their own block; no `results[]` or `cells[]` row
  contains a `delete_sweep` phase or an `sftp_remove` round-trip. The CSVs stay
  upload-only too: `deletes[]` lives in the JSON.

### The deploy shapes

A scenario is not only a payload. Every result up to issue #184 was a full
upload into an empty target in `mode: overlay`, which is the rarest deploy there
is; phase 3 added the shapes a real deploy actually has. The mode and the
layout live in `scenario.ShapeOf`
([`internal/benchmark/scenario`](../internal/benchmark/scenario)), and
`matrix.md` prints them in a table next to the grids so a number is readable
without going back to the source.

| Scenario | What it is | Why |
|---|---|---|
| `small`, `mixed`, `large`, `single` | full upload into an empty target, `overlay` | the original set, unchanged, so stored results stay comparable |
| `redeploy` | 500 x 4 KiB deployed twice, 3 files changed, `overlay` plus `advanced.skip_unchanged` | the common CI case; the only shape where `remote_scan` and the skip path are measured against a populated target |
| `sync` | the same, in `mode: sync` | `manifest_read`, `hash`, `manifest_write` and `prune_dirs` are empty in every result measured in `overlay`, and sync is the only CPU-bound path in the product |
| `deep` | 400 x 4 KiB, 7 directory levels, a handful of files per directory | separates `create_dirs` and `sftp_mkdirall` cost from transfer cost, which at this RTT is a large share of a `node_modules`-shaped deploy |
| `bulk` | 2000 x 4 KiB | enough files for the per-run fixed costs to fall away against the per-file cost |
| `calib-<count>x<size>` | uniform, e.g. `calib-100x64k`, `calib-10x16m` | not meant to be realistic: one size per scenario is what `t_file = r · RTT + size / B` can be fitted against, and `mixed` mixes three sizes and structurally cannot give that |

Only the matrix benchmark takes them, through `MATRIX_SCENARIOS` (the
`scenarios` workflow input); the standard benchmark's set stays fixed, because
adding to it would make every stored release result incomparable.

Two things to know before reading a `redeploy` or `sync` cell:

- **Its cell cost roughly twice its measured time.** The tree is deployed once
  unmeasured to create the state the measured run redeploys over, and only the
  second run is measured. The script says which scenarios do this before it
  starts.
- **Its MiB/s is over the changed files, not over the tree.** Three files
  changed out of 500 is the point; the duration is mostly scan and manifest
  work, and reading its throughput as transfer throughput is a mistake.

The change between the two deploys appends a few hundred bytes rather than
rewriting in place, because it has to be visible to both detectors:
`mode: sync` compares content hashes, but `advanced.skip_unchanged` compares the
remote file's *size* only, and a same-size rewrite would be skipped as
unchanged.

### The link profile

`environment` says which machine measured. It said nothing at all about the path
to the server, and that path is most of what these numbers are (issue #184).
Every result from this change on carries a `link` object next to it:

```jsonc
"link": {
  "iface": "eth0",                  // the interface towards the server, null when unknown
  "shaping": {
    "available": true,              // could tc actually be used on this runner
    "reason": null,                 // why not, when it could not
    "requested": ["baseline", "+50ms/5mbit"],
    "applied": ["baseline", "+50ms/5mbit"]
  },
  "probes": [
    { "profile": "baseline", "at": "start",   // "start" and "end" of that profile's own runs
      "handshake_ms": 41.2,
      "rtt_ms": { "p50": 18.4, "p90": 21.0, "min": 17.1, "max": 44.2, "samples": 21 },
      "control": { "streams": 4, "bytes": 8388608,
                   "single_stream_mib_per_s": 0.41, "n_stream_mib_per_s": 1.6 },
      "host_load": { "available": true, "method": "sftp:/proc/loadavg", "load1": 0.9 },
      "errors": [] }
  ]
}
```

Three things to know when reading it:

- **`environment` is the comparability key, `link` is a measurement.** Two
  results are only comparable when `environment` matches; `link` varies from run
  to run by design, which is exactly why it must not be part of that check.
- **The control measurement is not easySFTP.** It comes from
  [`cmd/linkprobe`](../cmd/linkprobe), which uses `x/crypto/ssh` and `pkg/sftp`
  directly and imports nothing from `internal/uploader`. It separates "the line
  is slow" from "easySFTP is slow", not `pkg/sftp` from the line. When a
  scenario's own MiB/s sits at the single-stream control, the run was network
  bound and a code delta on it says nothing.
- **Each profile is probed twice**, before and after its own measured runs. A
  start and an end probe of the *same* profile are comparable, and that is what
  makes drift over a multi-hour sweep visible. Two probes of different profiles
  are not.

An absent `link` object, or one with `probes: []`, means no probe binary was
available for that run. That is honest and readable; an invented entry would not
be.

#### Requesting a shaped link

Both workflows take a `link-profiles` input (`BENCH_LINK_PROFILES` and
`MATRIX_LINK_PROFILES` when running the commands directly), space separated.
Empty, the default, means the real line only, recorded under the profile name
`baseline`. The grammar, implemented in
[`internal/benchmark/link`](../internal/benchmark/link):

| Profile | What it applies |
|---|---|
| `baseline`, `unshaped` | nothing, the real line |
| `+50ms` | `netem delay 50ms` on egress, so RTT goes up by 50 ms, not 100 |
| `+50ms/5mbit` | the same plus a `tbf` rate of 5 Mbit |
| `baseline/5mbit` | the rate alone |

The axis multiplies everything: four profiles over a matrix grid are four times
the hours, and the matrix script prints its run count before it starts for that
reason.

Shaping needs `NET_ADMIN` on the runner. Where it is missing, the run does not
fail: it records `shaping.available: false` with a reason, measures every profile
on the real line, and the profile names then say what was asked for rather than
what happened. The library applies shaping through a trap on EXIT, INT and TERM
that removes the qdisc again, because a runner left shaped by an aborted run
makes every later measurement on it quietly wrong.

### Where the instrumentation comes from

The measured runs set `EASYSFTP_METRICS_FILE`; easySFTP then writes its phase,
operation, counter and process metrics there (see
[`internal/metrics`](../internal/metrics)). The variable is not an `action.yml`
input and has no effect on a normal deploy: with it unset, the instrumentation
is inert and the connection is not even wrapped in its byte counter.

### The two top-level exports

`index.json` lists every result newest first, with `kind`, `official`,
`archived`, `benchmark_kind`, the environment, the link profiles it was measured
over, the probed RTT p50 of its baseline profile, the paths of all its files, and
the candidate's median milliseconds per scenario (or, for a matrix run, its
best cell), so a reader does not have to open every file.

`trend.csv` is the same set flattened for plotting: one row per stored
non-matrix result, scenario and link profile, carrying the timestamp, the
version, the runner, the link the row was measured over, the duration statistics
**and the throughput**, which `index.json` deliberately does not. It is what a
"runtime and throughput over releases" chart reads, and the `rtt_p50_ms` column
is what keeps such a chart from reading a slower line as a slower release. Both
files are regenerated in full on every store, so they always describe what is
actually on disk. Columns whose data predates the metric (peak RSS and CPU time
on schema 1 results, the link columns on anything measured before the probe
existed) are empty rather than zero.

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
  and can never become one. A release produces one of these too, labelled with
  its tag (`matrix/matrix-<stamp>-vX.Y.Z.*`): the official number says how the
  release behaved at the default cell, the sweep next to it says how it behaves
  across the settings and across every scenario. The label being a version
  changes nothing about the kind, so it stays out of `latest.*` and out of
  `trend.csv` like every other sweep.
- Release PRs are benchmarked once when they open, so the numbers are visible
  before the merge. That run stores nothing: its result belongs to a release
  that does not exist yet. The official file is the post-tag measurement.

## Where the numbers were measured

The benchmark runs on a fixed self-hosted runner, not on a GitHub-hosted one:
those land in a changing region on changing hardware, which puts the machine
into every delta between two releases. Each result records where it ran in its
`runner` field (and, from schema 2 on, in the richer `environment` object), and
two results are only comparable when those match.

A fixed runner and a fixed server are still not a fixed *path*, and a matrix run
takes hours. That is what the `link` object above is for: it records the path
each result was measured over instead of leaving it as an invisible constant.

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

A single repeat has no measured spread, so `mad_ms` is then `null` (empty in the
CSV) rather than 0, which would read as perfect precision. That is the normal
case for a matrix run, whose `REPEATS` default is 1 because the grid is already
hours: its deltas have no noise floor to compare against, and the canary above is
what stands in for one. Results stored before this change carry 0 there.

Before reading a delta at all, check the run's single-stream control against the
scenario's own MiB/s. A scenario sitting at the control was limited by the path,
not by the code, and no delta measured there means anything. Every
`comparison[]` entry pairs two cells of the same `link_profile` for the same
reason.

There are deliberately no thresholds here and nothing fails a build. The data
format is meant to make automatic regression detection possible later, not to
pretend it is already reliable.

## Retention

The current release and the four before it stay in `releases/`. Older releases
move to `archive/releases/`, and manual and matrix runs move with them once
they are older than the oldest kept release. Nothing is ever deleted or
rewritten: storing a name that already exists fails on purpose, so a number
that was once published stays exactly as it was published.

If results are ever moved by hand, `KIND=reindex go run ./cmd/easysftp-bench store`
rebuilds `index.json`, `trend.csv` and `latest.*` from whatever is on disk
without storing anything new.

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

Setting `link-profiles` measures every scenario once per profile and records the
probed link next to the numbers; see "The link profile" above for the grammar and
for what happens without `NET_ADMIN`. `link-iface` overrides the interface, which
is otherwise derived from the route to the benchmark host.

If a release ended up without its official result, the same workflow repairs
it: set `release-version` to that tag (for example `v3.3.0`). The run measures
the tag and stores it as the official reference, and it fails rather than
touch a reference that already exists. Everything else is a manual run and
stays one.

The **`SFTP benchmark matrix`** workflow
([`.github/workflows/benchmark-matrix.yml`](../.github/workflows/benchmark-matrix.yml))
runs the sweep. Its `connections`, `concurrency` and `request-concurrency`
inputs are the axes and `link-profiles` adds a fourth. Its `scenarios` input is
where the deploy shapes above are requested; the default stays
`small large single`, since each added scenario multiplies the grid the same way
a link profile does.
Candidate and baseline are measured cell by cell, back to back, for the same
reason the standard benchmark interleaves its repeats. The grid is tens of
measured runs per scenario and takes hours, and each extra link profile
multiplies that; shrink the axes for a quick look. The script prints its run
count before it starts, per scenario and with the canary and `auto` runs
included, and it prints the axes each scenario was reduced to (see "The
per-scenario grid").

Setting `request-concurrency` to the single token `default` turns that axis off
again, which is the grid every result before issue #184 phase 5 was measured on.

### What a release measures

Nothing is triggered by the release event itself; each of these is a `uses:`
call from `release-please.yml`, gated on the release actually having been
created, for the reason in that file's comment. They run in order, because the
first two measure the same host:

1. **`SFTP benchmark`** measures the tag at the default settings and stores the
   official reference (`releases/release-vX.Y.Z.*`, copied to `latest.*`, and
   attached to the GitHub Release).
2. **`SFTP benchmark matrix`** sweeps the same tag over every scenario the
   harness knows, with the axes reduced per scenario as described above, and
   stores it under the tag as a matrix result. That is hours of measuring, which
   is why the job may take a day; the release itself does not wait for any of
   it.
3. **`Benchmark analysis`** draws both of them with the optional Python layer
   and uploads the figures as a run artifact plus one
   `benchmark-analysis-vX.Y.Z.zip` on the release page. It runs on a
   GitHub-hosted runner, contacts nothing, and reads only what the two jobs
   above committed here.

A release whose standard run failed still gets its sweep, and a release whose
sweep failed still gets its figures: the later jobs read whatever is stored
rather than requiring the earlier ones to have succeeded. Only a *cancelled*
standard run stops the sweep, since someone stopping a measurement by hand did
not mean to start a longer one.

Each of the three can also be started by hand from the Actions tab, which is how
a release that missed one is repaired.

### About `connections` above `concurrency`

The sweep deliberately includes cells where `connections > concurrency`, and
easySFTP deliberately does not honour them: `poolSize` in
[`internal/uploader/session.go`](../internal/uploader/session.go) caps the pool
at the concurrency and says so in the log. Only the per-file upload path spreads
over the pool, and at most `concurrency` files are ever in flight, so a
connection past that number buys a handshake and no parallelism. Those cells are
in the grid so the data shows that flattening rather than leaving a gap where a
reader has to take the cap on trust.

The per-scenario cap above does not remove them: it caps both axes at the same
number, so a scenario with at least two files still has its `connections >
concurrency` cells. A one-file scenario has exactly one configuration and
therefore one cell, which is the same statement made in one row instead of
thirty.
