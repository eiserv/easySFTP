# Benchmark link profile (issue #184, phase 1)

Record and control the network path the benchmarks measure over.

## Why

`environment` describes the runner (OS, kernel, arch, CPU, Go version) and
nothing about the path to the SFTP server. That path is most of what the
numbers measure. The stored matrix data shows a per-connection ceiling of
roughly 0.38 MiB/s and roughly 45 files/s, and nothing in the stored data can
decide whether that is the SSH channel window, `pkg/sftp`, the uplink or
server-side shaping. Under a network ceiling a code change is invisible, which
makes `large` and `single` near-worthless as a regression signal.

Phase 1 turns the one fixed runner and the one fixed server from a limitation
into a measurement axis: the link is probed and recorded per run, and it can be
deliberately shaped so RTT and bandwidth become variables instead of invisible
constants.

Scope: record and control only. No thresholds on the probed values, no fitted
coefficients, no auto-config. `r`, `K_ops` and `B_conn` belong to phase 5, and
per the issue never as shipped constants.

## Components

### 1. `internal/linkprobe` and `cmd/linkprobe`

A separate binary that uses only `x/crypto/ssh` and `pkg/sftp` and touches no
product code from `internal/uploader`, `internal/session` or `internal/config`.
Host keys are verified from the same known-hosts material a real run uses, so
the probe brings no insecure shortcut into the repository.

It writes one JSON document to stdout and measures:

| Field | Measurement |
|---|---|
| `handshake_ms` | dial, SSH handshake, SFTP subsystem |
| `rtt_ms` | N sequential `Stat` calls on a fixed path, each timed individually, as p50/p90/min/max/samples (default N=20) |
| `control` | single-stream and N-stream bytes/s: own connections, own files, maximal `pkg/sftp` settings, cleaned up afterwards |
| `host_load` | best effort, in order: SFTP read of `/proc/loadavg`, then `exec uptime`, otherwise `{"available": false, "reason": ...}`; the successful path is named in `method` |

The control measurement shares the libraries with easySFTP, not the product
code, and the document says so. It separates "easySFTP's upload pipeline is
slow" from "the line is slow", not `pkg/sftp` from the line.

`internal/linkprobe` has its own tests against an in-process SSH/SFTP server,
following the pattern of `internal/uploader/testserver_test.go`: the shape of
the document, the sample count, that the control files are removed again, and
that a missing `/proc/loadavg` lands as `available: false` rather than as an
error.

### 2. `scripts/benchmark-link.sh`

A shared library sourced by both benchmark scripts, alongside
`benchmark-lib.sh`.

- Profile syntax: `baseline`, `+50ms`, `+50ms/5mbit`, `unshaped/5mbit`. Every
  profile is parsed and validated before the first measurement, not after
  hours.
- `LINK_IFACE` is otherwise derived from `ip route get $BENCH_HOST`.
- Availability is probed once at the start: `tc` present, interface
  resolvable, and a probe `add` plus `del` succeeds (via `sudo -n` when not
  root). Without `NET_ADMIN` the axis is reduced to `baseline` with a warning
  and `link.shaping.available: false` is stored, rather than losing the run.
- Applied as `netem` root plus, when a rate is given, `tbf` as a child qdisc.
- Safety critical: as soon as shaping is applied for the first time, a trap on
  EXIT, ERR and INT is installed that runs `tc qdisc del`. Without it the
  runner stays shaped after an abort and every later measurement on it is
  silently wrong.

The default is empty, which means one unshaped pass named `baseline`, exactly
what `MATRIX_REQUEST_CONCURRENCY` does today.

### 3. Integration into both benchmark scripts

New environment: `BENCH_LINK_PROFILES` / `MATRIX_LINK_PROFILES`,
`LINKPROBE_BIN`, `LINK_IFACE`, `LINK_SUDO`.

The profile loop is the outermost one, around the existing loops: changing `tc`
per measured run would itself be noise. The cost is that drift over the hours
falls onto the profile axis instead of spreading across it, and that is exactly
why every profile probes the link again, plus one closing probe after the last
profile. Drift is recorded rather than invisible.

`LINKPROBE_BIN` is optional. When it is empty the script warns and stores
`link.probes: []`, so a local run without a built probe still works. The
workflows build it like the candidate and the baseline.

New top-level object, identical in `results.json` and `matrix.json`:

```jsonc
"link": {
  "iface": "eth0",
  "shaping": { "available": true, "requested": ["baseline", "+50ms/5mbit"], "applied": [...] },
  "probes": [
    { "profile": "baseline", "at": "start", "measured_at": "...",
      "handshake_ms": 41.2,
      "rtt_ms": { "p50": 18.4, "p90": 21.0, "min": 17.1, "max": 44.2, "samples": 20 },
      "control": { "streams": 4, "bytes": 8388608,
                   "single_stream_mib_per_s": 0.41, "n_stream_mib_per_s": 1.6 },
      "host_load": { "available": true, "method": "sftp:/proc/loadavg",
                     "load1": 0.9, "load5": 1.1, "load15": 1.0 } }
  ]
}
```

Every `runs[]` row and every `cells[]` row gains `link_profile`; the
aggregation and `comparison[]` group by it as well. With an empty axis the
profile is called `baseline` and nothing else changes, so stored results stay
readable. `axes.link_profiles` joins the matrix axis declaration, and the grids
are rendered per profile the way they are rendered per `request_concurrency`
today.

Both CSVs gain `link_profile`, `rtt_p50_ms` and `control_single_mib_per_s`, so
a row is readable on its own. `summary.md` and `matrix.md` gain a "The link"
section with one row per profile, plus the sentence the data needs: under a
network ceiling a code delta cannot be interpreted.

### 4. Documentation and self-checks

- `benchmarks/README.md`: `link` in both key tables, and a section on the link
  profile (what a control measurement is for, that `environment` stays the
  comparability key while `link` is measured, how shaping is requested and that
  it degrades cleanly without `NET_ADMIN`).
- `scripts/benchmark-store.sh`: `index.json` entries and `trend.csv` gain
  `rtt_p50_ms` (null when not measured). That is what makes "falsifiable across
  days" redeemable without opening every file.
- `scripts/test-benchmark.sh`: a linkprobe stub; asserts `link.probes[]`, the
  new columns, that a run without `LINKPROBE_BIN` still produces valid JSON
  with `probes: []`, and that unavailable shaping is recorded and not fatal.
- `scripts/test-benchmark-store.sh`: the fixture gains a `link` block; asserts
  it is kept verbatim under `.benchmark.link` and that `rtt_p50_ms` appears in
  the index and the trend.
- Both workflows: build the probe binary, set `LINKPROBE_BIN`, add the
  `link-profiles` and `link-iface` inputs.

CI covers this without changes: `shellcheck scripts/*.sh` picks up the new
library, `go vet ./...` and `go test -race ./...` the new package.

## Out of scope

Everything after phase 1: keeping the per-cell metrics detail (phase 2), the
redeploy, sync, deep-tree and calibration datasets (phase 3), the delete sweep
result (phase 4), and the grid extension plus auto-config (phase 5, which
changes product behaviour and belongs to #156).
