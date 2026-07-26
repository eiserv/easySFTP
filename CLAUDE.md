# CLAUDE.md

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
  during a non-transfer phase like a delete sweep or remote scan) and
  request-triggered hangs (`withStallOnRequest`). Follow that pattern (wrap
  the relevant `Handlers` field, add a `serverOption`) for new
  fault-injection needs instead of building a new fake server. Note that
  `withDropOnRequest` closes *every* live connection, including any
  `verifyClient` session opened before the drop fires; open verification
  clients after the run, not before.
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
- The self-test job in `.github/workflows/ci.yml` is the only place a real
  OpenSSH server is exercised, and the only place `action.yml`'s composite
  wiring runs end to end. Unit tests set `EASYSFTP_*` directly and never see
  declared input defaults, which is how #62 shipped green.

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

## Release process

`CHANGELOG.md` and `.easysftp-version` are generated by release-please from
Conventional Commit messages; **never hand-edit `CHANGELOG.md`**. PR titles
must be Conventional Commits (CI enforces this; squash-merge makes the PR
title the commit message; see `CONTRIBUTING.md`).

## Automated issue-driving sessions

This repo receives autonomous sessions that each pick an open issue, resolve
it (making a judgment call where the issue leaves one open; usability and
user choice win ties, per the principles above), and open a draft PR. No one
watches these sessions live; everything that matters must end up in the PR
description or in an issue.

- If you notice something during unrelated work (a typo, an inconsistency,
  a possible optimization, something you're not confident enough to fix
  right now), file it as a new GitHub issue rather than letting it evaporate
  with the session. Label it `needs-check` if it genuinely needs a human or
  a future session's closer look before acting. The label exists in the repo
  now; note that `gh issue create --label` does *not* auto-create missing
  labels (it errors), so a new label needs `gh label create` first.
- Before working an issue, sanity-check it's still current (code may have
  moved on since it was filed). If it's stale/already fixed/no longer
  applicable, close it (with a reason) instead of implementing something
  moot, and pick another.
- New feature ideas that fit the SFTP-deploy niche are welcome as new issues;
  don't implement speculative features nobody asked for in the same PR as an
  unrelated fix.
- One issue, one PR. Keep unrelated cleanups you notice out of the diff;
  file them instead (see above) so the PR stays reviewable.
