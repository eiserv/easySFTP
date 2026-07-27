# Benchmarks

Upload throughput of easySFTP against a real SFTP server, measured by
[`scripts/benchmark.sh`](../scripts/benchmark.sh) and filed here by
[`scripts/benchmark-store.sh`](../scripts/benchmark-store.sh).

These numbers set no threshold and fail no build. They exist to see where the
time goes (issues #158 and #169), so read them as one host's behaviour on one
day, not as a guarantee.

## Where to look first

| You want | Read |
|---|---|
| The current official numbers | `latest.md` (`latest.json` for the raw data) |
| A listing of everything stored here | `index.json` |
| One specific release | `release-vX.Y.Z.md` / `.json` |
| An older release | `archive/release-vX.Y.Z.*` |

## What is in here

```text
benchmarks/
  latest.json                        copy of the newest official release result
  latest.md                          the same, human readable
  index.json                         every stored result, machine readable
  release-vX.Y.Z.json                official reference of a release
  release-vX.Y.Z.md
  manual-<UTC stamp>-<label>.json    manual or experimental run
  manual-<UTC stamp>-<label>.md
  archive/                           the same file names, past the keep window
```

Every result is a pair: the raw JSON and a Markdown summary of the same run.
The JSON wraps the measurement verbatim under `.benchmark` and adds provenance
around it:

```jsonc
{
  "schema_version": 1,
  "kind": "release",        // or "manual"
  "version": "v3.2.2",      // null for manual runs
  "label": null,            // the manual run's label, null for releases
  "official": true,         // false for everything that is not a release
  "recorded_at": "2026-07-27T12:00:00Z",
  "commit": "…",
  "run_url": "…",
  "benchmark": { /* results.json from scripts/benchmark.sh */ }
}
```

`index.json` lists all of them newest first, with `kind`, `official`,
`archived`, the paths of both files, and the candidate's median milliseconds
per scenario, so a reader (human or agent) does not have to open every file.

## Official versus manual

- **Official** results are `kind: "release"`. They are measured automatically
  after release-please created a tag, against exactly that tag, and they are
  the only results `latest.*` is ever copied from. Both files are also attached
  to that GitHub Release as `benchmark-vX.Y.Z.json` and `benchmark-vX.Y.Z.md`,
  so the numbers sit next to the binaries they describe.
- **Manual** results are `kind: "manual"`, produced by a manually started run
  (a branch, a tag, or a pull request). They are kept alongside the official
  ones and are clearly named, but they never become a reference and never
  overwrite one.
- Release PRs are benchmarked once when they open, so the numbers are visible
  before the merge. That run stores nothing: its result belongs to a release
  that does not exist yet. The official file is the post-tag measurement.

## Where the numbers were measured

The benchmark runs on a fixed self-hosted runner, not on a GitHub-hosted one:
those land in a changing region on changing hardware, which puts the machine
into every delta between two releases. Each result records where it ran in its
`runner` field, and two results are only comparable when that field matches.

Results up to and including v3.3.1 predate this and name only the kernel and
CPU count (`Linux 6.17.0-1020-azure, 4 cpu`); every one of them was measured on
a GitHub-hosted runner. From the next release on the field starts with
`self-hosted`.

## Retention

The current release and the four before it stay in `benchmarks/`. Older
releases move to `benchmarks/archive/`, and manual runs move with them once
they are older than the oldest kept release. Nothing is ever deleted or
rewritten: storing a name that already exists fails on purpose, so a number
that was once published stays exactly as it was published.

## Running a benchmark

The `SFTP benchmark` workflow (`.github/workflows/benchmark.yml`) runs it.
Start it manually from the Actions tab with a `pr` number to benchmark a pull
request, or a `candidate-ref` plus an optional `baseline-ref` to compare two
refs in the same run. A manual run needs an approval on the `benchmark`
environment first, which only a maintainer can give.

If a release ended up without its official result, the same workflow repairs
it: set `release-version` to that tag (for example `v3.3.0`). The run measures
the tag and stores it as the official reference, and it fails rather than
touch a reference that already exists. Everything else is a manual run and
stays one.
