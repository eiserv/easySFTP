# AGENTS.md

Working notes for automated/agentic sessions on this repo. Human-facing docs
live under `docs/`; this file is about *how to work on the codebase*, not
what it does.

Style note: avoid em-dashes in anything written here (code comments, error
messages, docs, PR and issue text). Use a comma, semicolon, colon, or
parentheses instead. The generated `CHANGELOG.md` is exempt. See issue #79.

## What this project is (and isn't)

easySFTP is a GitHub Action for SFTP deploys: connect, plan, upload, optional
delete/sync. Keep it scoped to that niche. Do not turn it into a generic
deployment/orchestration tool (no built-in build steps, no non-SFTP
protocols, no CI orchestration). New features should make SFTP deploys more
usable, configurable, or robust, not add unrelated capabilities.

Guiding principles for changes here:
- **YAGNI/KISS.** Don't add config-file/per-target plumbing for a knob unless
  something actually needs per-target granularity. See "Two categories of
  settings" below; most new inputs belong in the simpler category.
- **Usability and configurability over enforced opinions.** This action is a
  tool, not a policy. Prefer optional, off-by-default inputs that let the
  user pick their own security/performance tradeoffs, over hard-coded
  "secure by default" behavior that removes choice (host-key verification is
  the one deliberate exception, see `docs/security.md` and issue #6, and
  even that is being evolved as an explicit opt-out, not a silent default).
  When in doubt, default to today's existing behavior and make the new
  behavior opt-in.
- Every input added to `action.yml` needs: an `EASYSFTP_*` env var wired in
  the `Upload via SFTP` step, parsing/validation in `internal/config/config.go`,
  a row in `docs/configuration.md`, and (if it's a real behavior change) a
  test. Don't forget `action.yml` input descriptions are user-facing docs too.
  Two drift-check lists must also be extended, or tests fail: `wantInputs` in
  `internal/actionmeta/actionmeta_test.go` (the actionmeta test errors on any
  wired env var missing from it), and the cleared-env list in `setBaseEnv`
  (`internal/config/config_test.go`), which keeps config tests hermetic when
  the ambient environment sets `EASYSFTP_*` variables.

## Where a setting lives (v3)

v3 replaced the old two-category split with one rule: **every non-secret
setting has exactly one home.** Check `action.yml` before assuming an input
exists; most v2 inputs are now "Removed in v3" stubs that only exist to
produce a migration error (`internal/config/config.go` holds the list and its
messages).

The complete v3 input surface is:

1. **Inline mode**: `host`, `port`, `username`, `host-key`, `known-hosts`,
   `allow-any-host-key`, `source`, `target`, `mode`, `exclude`, `file-mode`,
   `dir-mode`, `preserve-times`. Setting any of these together with `config`
   fails the run (the check at the top of `config.Load()`): there is no mixed
   mode.
   **Never give these inputs a `default:` in `action.yml`**: the runner
   exports declared defaults unconditionally, so the mutual-exclusion check
   sees them as "user-set" and rejects every config-file run; that's how
   #62 shipped in v2.0.0. `TestLoadConfigFileWithActionDefaults`
   (`internal/config/config_test.go`) guards against reintroducing this.
2. **Config mode**: `config`, pointing at a `version: 3` YAML file that owns
   everything non-secret, including what used to be run-wide inputs
   (`advanced.*`, `permissions.*`, `sync.*`, `safety.max_deletes`). Parsed in
   `configfile.go`, validated by `schema/easysftp.schema.json`.
3. **Valid in both modes**: credentials (`password`, `private-key`,
   `passphrase`, `proxy-*` credentials) plus `dry-run` and `log-level`. These
   change how a run authenticates or reports, never what it deploys.

So a new knob is normally a config-file field, not an input. Adding it as an
input means arguing why it belongs in category 1 or 3.

`file-mode` / `dir-mode` / `preserve-times` are the worked example (issue
#133): they are category 1, mirrored by `permissions.*` in category 2, so a
Windows deploy that needs `file-mode: "0644"` does not have to convert its
whole workflow into a config file. Each setting still has exactly one home
*per mode*, which is what keeps "no precedence question" true. Per-deployment
granularity is a separate, later decision; don't build it speculatively.

## The adaptive transport settings (`auto`)

`advanced.connections`, `advanced.concurrency` and `advanced.request_concurrency`
default to `auto`, and since issue #209 `auto` is a real policy rather than a
synonym for a fixed 1/4/16. `internal/autotune` is that policy: a cost model
with no clock, no I/O and no state, plus a small runtime controller. Read its
package comment before changing anything here; the short version:

- `concurrency` is the number of independent items the phase has, capped at 64.
  There is no trade-off (a worker with nothing to do never starts), which is
  why nothing measures it and why the runtime controller never moves it.
- `request_concurrency` follows the largest file (it counts 32 KiB write
  packets in flight for *one* file) and is capped by the SSH channel window,
  which is 64 packets. It is fixed when an SFTP client is created, so it is
  resolved once, run-wide, before the first connection.
- `connections` is the only interesting one. `T(k) = W/k + (k-1)*H` is smallest
  at `k = sqrt(W/H)`: open another connection while it saves more than its
  handshake costs. Everything else in the package exists to estimate `W`.

Three rules that are load-bearing:

1. **A number in the config file is never touched**, at any stage.
   `config.AutoSettings` records which of the three the user left to easySFTP;
   the int fields keep the old fixed defaults as a fallback. A `config.Config`
   built directly (every uploader test) therefore has all three *pinned*, which
   is why the pre-existing tests still measure what they measured.
2. **The pool only grows.** `session.conns` is allocated once to the ceiling
   and `session.spread` is what moves; slots are dialed on first use, so
   widening costs nothing until a worker lands on a fresh slot. A refused
   connection latches `session.refused`, pulls the spread back and stops the
   run asking again.
3. **Never derive parallelism from `runtime.NumCPU()`.** The constraint is the
   server and the path, not the runner (issue #156 says so explicitly). The one
   place the runner's CPU count belongs is sync hashing, which is local work
   (issue #155).

The three stages and where they run:

| Stage | Where | What it adds |
|---|---|---|
| 1, workload | `uploader.Run` (run-wide) and `uploadFiles` (per deployment) | the plan: files, bytes, largest file, metadata round-trips |
| 2, link | `newSession` via `probeLink` | the handshake it just paid, and an RTT from three `SSH_FXP_REALPATH` round-trips |
| 3, runtime | `startTuningController` inside `uploadFiles` | the throughput the transfer is actually achieving |

An overlay deployment with `advanced.skip_unchanged` is the case stage 1 cannot
settle: the upload set is decided file by file while the run goes. It is
planned as metadata only (`Workload.Unknown`), which is what keeps a redeploy
that changed three files from paying for a pool, and stage 3 widens it if the
observed ratio says most files really are being sent.

`internal/autotune/regret_test.go` is the acceptance test issue #209 asks for:
it replays the policy over every sweep committed under `benchmarks/matrix/`
with at least three repeats and fails when the regret against the best measured
cell goes over 15% (a gap under 300 ms passes on its absolute size, which is
what the issue allows for sub-two-second runs). A sibling test asserts that the
old fixed 1/4/16 would *fail* it, so the replay cannot quietly stop
discriminating. **Changing a constant in `internal/autotune` means running that
test**, and a new stored sweep that fails it means the policy needs refitting,
not that the test needs relaxing.

## Testing quirks

- `internal/uploader/testserver_test.go` runs an in-process SSH/SFTP server
  backed by `sftp.InMemHandler()` (from `github.com/pkg/sftp`). Its `Setstat`
  (chmod) implementation **ignores permission bits entirely**; it only
  handles size truncation and always returns success, and calling it on a
  directory path returns `os.ErrInvalid` regardless of what you set. That
  means:
  - You cannot assert an actual remote mode changed via `client.Stat(...)`
    after a chmod in tests.
  - Use `setstatRecorder` (wraps `FileCmd`, records path+mode of every
    `Setstat` request with the permissions flag set) to assert *what was
    requested*.
  - Use `withFailSetstat()` / `faultySetstat` to simulate a server that
    rejects chmod, and `recordingLogger` to assert on warnings produced.
  - Directory chmod against this fake server always errors (see above);
    that's a fake-server limitation, not a bug in the code under test.
- Similar fault-injection wrappers exist for rename (`faultyRename`,
  `withFailRename()`), connection drops (`withDropAfter`,
  `withDropFirstConnAfter`), request-triggered drops
  (`withDropOnRequest(method, path)`, which kills the live connection the
  first time a matching SFTP request arrives; use it to simulate a drop
  during a non-transfer phase like directory setup, a stale-temp sweep, a
  delete sweep or remote scan) and
  request-triggered hangs (`withStallOnRequest`) and refused connections
  (`withMaxConns(n)`, which closes everything accepted beyond the first n; the
  connection pool's degradation path uses it). Follow that pattern (wrap
  the relevant `Handlers` field, add a `serverOption`) for new
  fault-injection needs instead of building a new fake server. Note that
  `withDropOnRequest` closes *every* live connection, including any
  `verifyClient` session opened before the drop fires; open verification
  clients after the run, not before.
- Simulating a **non-OpenSSH server** takes two pieces, because the fake server
  serves `posix-rename@openssh.com` whether or not it announces it:
  `withFailPosixRename(code)` makes the extension fail with one exact SFTP
  status (any `sftp.ErrSSHFx*` value; plain `Rename` keeps working, so the
  remove+rename fallback can succeed), and `unadvertisePosixRename(t)` leaves
  the extension out of the announced list a client reads. The second mutates a
  pkg/sftp package-level variable, so it is process-global and restores itself
  via `t.Cleanup`; no test in this package runs in parallel, which is what
  makes that safe.
- `withCoarseStatus(code)` is the other half of a non-OpenSSH server: it
  answers every "not there" with one generic status code (`SSH_FX_FAILURE`,
  `SSH_FX_BAD_MESSAGE`) instead of `SSH_FX_NO_SUCH_FILE`, across all four
  handler roles at once, so a missing path looks the same whether the run
  listed, stat'd, removed or opened it. It intercepts exactly the errors that
  satisfy `os.ErrNotExist` and passes real refusals through, which is what
  makes `remoteAbsent`'s tie-break testable in both directions (issue #152).
  Give it before `withFailOpen`/`withFailList` when a test needs both.
- A run may hold more than one connection (`advanced.connections`, issue
  #158; how many is the policy's, issue #209). Only the per-file upload path
  spreads over them, by file index modulo `session.spread`; `session.do` and
  therefore everything else always uses the first one. Remote scans, directory
  creation/chmod, stale-temp sweeps, file deletes and same-depth directory
  deletes may call `session.do` concurrently, bounded by
  `session.workers(items)`; they still open no extra connections. A connection
  the server refuses is not an error: that pool slot falls back to the first
  connection after one warning, and the run stops asking. The reconnect budget
  stays run-wide and the stall watchdog closes every connection.
- Parallel `MkdirAll` calls for different leaves can share a missing parent.
  `pkg/sftp` normally resolves that race, but a connection drop between its
  failed `Mkdir` and confirming `Lstat` can surface a misleading already-exists
  error. `createRemoteDirs` confirms the completed leaf before failing and
  turns a failed confirmation connection into a `session.do` retry; keep that
  final-state check.
- Every remote operation outside the per-file upload path must go through
  `session.do` (see `internal/uploader/session.go`): it redials on
  connection-class errors sharing the `retries` reconnect budget and marks
  the operation active for the stall watchdog. Ops passed to it must be
  idempotent, because a retried op may have partially or fully taken effect
  before the connection died. Multi-round-trip helpers called inside a `do`
  op should call `watch.tick()` (nil-safe) after each completed round-trip
  so a long healthy phase is not mistaken for a stall.
- Every `FileCmder` wrapper in `testserver_test.go` must implement
  `PosixRename` (delegate via `posixRenamePassthrough`): pkg/sftp serves
  posix-rename only when the outermost `FileCmder` implements
  `PosixRenameFileCmder` and otherwise downgrades it to plain `Rename`, which
  fails when the target exists. A wrapper without the method makes every
  overwriting rename (manifest rewrites, re-uploads of existing files) fail
  with "file already exists", far from the wrapper's apparent concern.
- The in-memory `memFile` implements pkg/sftp's `TransferError` interface:
  when a connection dies mid-write, the request server stores the transfer
  error *into the shared file object*, and every later write through that
  same in-memory file (from any connection) returns the stale error, e.g.
  `sftp: "error reading packet body: ..."`. Real servers do not behave like
  this. The production retry path sidesteps it by removing the leftover temp
  file before a re-attempt, which also matters on real servers (stale
  handles/locks); keep that in mind before "simplifying" it away.
- The stall watchdog is one-shot: once `monitor` fires (closes the connection,
  sets `fired`), its goroutine exits, so `fired` stays true forever and a
  watchdog passed to any later `session.do` protects nothing. `session.do`
  deliberately refuses to redial once `fired` is set (a stalled server usually
  just stalls again). `writeRecoveryManifest` is the one exception: it drops
  the spent watchdog so `do` may spend one reconnect to record partial progress
  before the run fails (issue #115). Keep this asymmetry in mind before routing
  a new post-stall operation through `do`.
- Run `go test -race ./...` before committing; uploads are parallelized
  (`errgroup` + `cfg.Concurrency`), so races are the most likely regression
  class in `internal/uploader`. `-race` needs cgo: on a machine without a C
  toolchain it fails with "-race requires cgo". In that case run plain
  `go test ./...`, say so in the PR, and rely on CI for the race pass.

## CI

- Everything CI pulls from outside the repo is pinned by hash: actions by
  their full commit SHA, and the self-test's SFTP container by `@sha256:`
  digest with the readable tag as a trailing comment. Dependabot does not
  update inline `docker run` digests, so that one is bumped by hand
  (`docker buildx imagetools inspect atmoz/sftp:alpine`). Keep new external
  dependencies pinned the same way.
- Nothing may hang off `on: release`. release-please creates the release with
  the `GITHUB_TOKEN`, and events raised by that token never start a workflow
  run, so such a job silently never runs (v3.3.0 shipped without its benchmark
  that way). Post-release work is a `uses:` call from `release-please.yml`,
  gated on `needs.release-please.outputs.release_created`, as
  `release-binaries` and `benchmark` both do.
- The self-test job in `.github/workflows/ci.yml` is the only place a real
  OpenSSH server is exercised, and the only place `action.yml`'s composite
  wiring runs end to end. Unit tests set `EASYSFTP_*` directly and never see
  declared input defaults, which is how #62 shipped green.

## Benchmarks

`scripts/benchmark.sh` measures throughput at the default settings,
`scripts/benchmark-matrix.sh` sweeps `advanced.connections` against
`advanced.concurrency`, both on top of the shared `scripts/benchmark-lib.sh`
(payload generation, running a build, reading its outputs, the jq statistics)
and `scripts/benchmark-link.sh` (the probed link profile and `tc` shaping).
A scenario there carries a *shape* as well as a payload (`scenario_shape`: mode,
whether the measured run redeploys over an unmeasured one, flat or deep
layout). The deploy-shape scenarios (`redeploy`, `sync`, `deep`, `bulk`,
`calib-*`) are matrix-only: `benchmark.sh`'s set is fixed, because adding to it
makes every stored release result incomparable.
`scripts/benchmark-store.sh` files a result under `benchmarks/`;
`benchmarks/README.md` documents the layout and the JSON schema and is the page
to keep in sync when any of those change. Read `benchmarks/index.json` before
opening single files.

Results are filed by kind: `benchmarks/releases/`, `manual/`, `matrix/`, plus
`archive/<kind>/`. Only `latest.{json,md}` and `index.json` sit at the top
level, because those links must not move. `KIND=reindex bash
scripts/benchmark-store.sh` rebuilds both from what is on disk (that is how the
migration into this layout was done).

Three invariants the whole thing rests on: a stored result is never rewritten
(storing an existing name fails), `latest.*` is only ever a copy of a
`kind: "release"` entry, and a matrix run is never official. Two CI-run
self-checks pin them (both need `jq`): `scripts/test-benchmark-store.sh` for
the retention window, archiving and the invariants, and
`scripts/test-benchmark.sh`, which drives both measuring scripts end to end
against a stub binary so the jq aggregation, schema and CSV columns are checked
without an SFTP server. The other half (that a *real* run produces the metrics
those scripts aggregate) is asserted by the container self-test in `ci.yml`.

The link a result was measured over is recorded in a top-level `link` object
(issue #184, phase 1) by `cmd/linkprobe`, a separate binary that imports nothing
from `internal/uploader`: a control measurement taken through the code under test
would not be a control. Three things to keep true when touching it: `environment`
stays the comparability key and `link` stays a measurement (so it must not move
into `environment`), the probe report never names the host or the user (it lands
in `results.json`, which is an artifact and is committed), and `tc` shaping is
applied only behind the EXIT/INT/TERM trap in `benchmark-link.sh`, because a
runner left shaped makes every later measurement on it quietly wrong. Shaping
that is unavailable is recorded, never fatal.

The `clean` deployment of an empty directory that runs before every measured run
is instrumented too, into its own metrics file and its own `deletes[]` block
(issue #184, phase 4): it is a pure delete sweep and the only measurement of
deletions there is. Do not "simplify" it back to `METRICS_FILE=''`, and keep its
numbers out of `results[]` / `cells[]`: an upload aggregate that grew a
`delete_sweep` phase is a bug, and `scripts/test-benchmark.sh` asserts it did
not.

Instrumentation lives in `internal/metrics` and is off unless
`EASYSFTP_METRICS_FILE` names a path. It is deliberately **not** an
`action.yml` input, so none of the drift-check lists apply to it. When metrics
are off, `metrics.Enabled()` is false and `dialSSH` takes the plain `ssh.Dial`
path: the byte-counting `net.Conn` must never end up in a production transfer.
Phases are wall clock, operation samples are cumulative across the parallel
upload workers; do not present the two as the same kind of number.

## Behavior worth knowing before you change it

- Uploads from **Windows runners** have no local permission bits to mirror:
  Go synthesizes `0666` for writable files, `0444` for read-only ones, loses
  the executable bit, and reports `0777` for directories (verified on Windows
  11 / Go 1.25). That is what a run then requests via SETSTAT. Documented in
  `docs/configuration.md`, `docs/troubleshooting.md` and `docs/examples.md`;
  keep those in sync if the mode handling changes.
- The three best-effort SETSTAT warnings (file-mode, dir-mode, preserve-times)
  are warn-once **per deployment**, not per run: the flags live in
  `transferEnv` (built in `uploadFiles`) and in `createRemoteDirs`' local
  `warned`, both of which run once per deployment. The message texts say
  "for this deployment" and a test pins it; see issue #121.

## Vocabulary

Two words that used to be one (issue #134):

- **deployment** = one `source`/`target` mapping plus its mode and excludes,
  i.e. one entry under `deployments:` (or the single inline pair). Use it for
  anything that counts, groups or reports per mapping: `DeploymentStats`,
  `Stats.Deployments`, "one warning per deployment", the `#### Deployments`
  job-summary table.
- **target** = the *remote path* only, matching the `target` input and the
  `Target` column of that table. `checkRemoteRoot`, "remote target directory".

Don't reintroduce "target" as a synonym for deployment.

## docs/providers.md is evidence-only

Provider entries are the one page where a plausible guess is worse than a gap:
a wrong port or path costs a user a broken deploy and they cannot tell it came
from nobody's machine. Every entry carries a `*Last verified: DATE, source*`
line, and an automated session may only add what it can ground in the vendor's
own documentation (link it) and must say so in that line. Never write "verified
on a live account" for something you did not run. When in doubt, extend the
generic "find your own settings" section instead, which is grounded in
easySFTP's behavior rather than in a provider's.

## Release process

`CHANGELOG.md` and `.easysftp-version` are generated by release-please from
Conventional Commit messages; **never hand-edit `CHANGELOG.md`**. PR titles
must be Conventional Commits (CI enforces this; squash-merge makes the PR
title the commit message; see `CONTRIBUTING.md`).
