# Configuration reference

Everything easySFTP accepts: action inputs, outputs and the YAML config file.

- [Inputs](#inputs)
- [Outputs](#outputs)
- [The `uploads` mapping](#the-uploads-mapping)
- [Ignore patterns](#ignore-patterns)
- [The YAML config file](#the-yaml-config-file)

## Inputs

### Connection

| Input | Required | Default | Description |
|---|---|---|---|
| `server` | ✅ | - | Hostname or IP of the SFTP server. |
| `port` | | `22` | SSH port. |
| `username` | ✅ | - | Username for authentication. |
| `password` | ¹ | - | Password. **Use a secret.** |
| `private-key` | ¹ | - | SSH private key (OpenSSH/PEM format). **Use a secret.** |
| `passphrase` | | - | Passphrase of the private key, if encrypted. |
| `host-key-fingerprint` | | - | Expected SHA256 host key fingerprint(s), one per line (`SHA256:...`). **Strongly recommended**, see [Security](security.md). |
| `known-hosts` | | - | Expected host key(s) in OpenSSH `known_hosts` format (verbatim `ssh-keyscan` output). Alternative to `host-key-fingerprint`; a key matching either is accepted. Hashed entries and `[host]:port` lines are supported. See [Security](security.md). |
| `timeout` | | `30` | Connection timeout in seconds. `0` disables the timeout entirely. Covers the dial and SSH handshake only; for hangs after that, see `stall-timeout`. |
| `stall-timeout` | | `0` (off) | Abort the run when active remote operations make no progress for this many seconds. Covers file transfers and the other remote phases (remote scans, delete sweeps, sync manifest reads and writes). Catches servers that accept the connection and then hang, which would otherwise block the job until the job-level timeout (6 hours by default). On a stall the connection is closed and the run fails with a clear error. Pick a value comfortably above the longest pause a healthy transfer to your server may hit; 60 to 300 is a reasonable range. |

¹ At least one of `password` / `private-key` is required. If both are set, the key is tried first.

### Jump host (bastion)

If the SFTP server is only reachable through a bastion (OpenSSH's
`ssh -J jump.example.com target`), set the `proxy-*` inputs. easySFTP then
connects to the jump host first, with its own credentials and its own host
key verification, and tunnels the SFTP connection to `server` through it.
All inputs mirror the primary connection settings:

| Input | Required | Default | Description |
|---|---|---|---|
| `proxy-server` | ² | - | Hostname or IP of the jump host. Setting any other `proxy-*` input without this one fails the run. |
| `proxy-port` | | `22` | SSH port of the jump host. |
| `proxy-username` | ² | - | Username for the jump host. |
| `proxy-password` | ³ | - | Password for the jump host. **Use a secret.** |
| `proxy-private-key` | ³ | - | SSH private key for the jump host. **Use a secret.** |
| `proxy-passphrase` | | - | Passphrase of the jump host key, if encrypted. |
| `proxy-host-key-fingerprint` | | - | SHA256 fingerprint(s) of the jump host's key, one per line. **Strongly recommended.** |
| `proxy-known-hosts` | | - | Jump host key(s) in `known_hosts` format; alternative to the fingerprint input. |

² Required when using a jump host. ³ At least one of `proxy-password` /
`proxy-private-key` is required when `proxy-server` is set.

Host key verification applies to **each hop independently**: pinning the
target but not the jump host (or vice versa) prints the unverified-host-key
warning for the open hop. The `timeout` input covers each hop's dial. The
`retries` reconnect logic re-establishes the whole chain (jump host and
tunnel) when the connection drops mid-run, and the initial-connection retry
covers either hop: a transient failure reaching the bastion is retried just
like one reaching the target, while a host key mismatch or an authentication
failure on either hop fails immediately.

```yaml
- uses: eiserv/easySFTP@v2
  with:
    server: sftp.internal.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
    host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINT }}
    proxy-server: bastion.example.com
    proxy-username: ${{ secrets.JUMP_USERNAME }}
    proxy-private-key: ${{ secrets.JUMP_PRIVATE_KEY }}
    proxy-host-key-fingerprint: ${{ secrets.JUMP_HOST_KEY_FINGERPRINT }}
    uploads: |
      ./dist/ => /var/www/html/
```

### What to upload

| Input | Required | Default | Description |
|---|---|---|---|
| `uploads` | ² | - | One `local => remote` mapping per line. Directories are uploaded recursively; single files are supported too. |
| `config-file` | | - | Path to a [YAML config file](#the-yaml-config-file) with per-target strategies and delete guards. Mutually exclusive with `uploads`/`strategy`/`ignore`/`ignore-from`/`max-deletes`. |
| `strategy` | | `overlay` | [Reconciliation strategy](strategies.md): `overlay`, `sync` or `clean`. |
| `ignore` | | - | Gitignore-style exclude patterns, one per line. `!` re-includes. |
| `ignore-from` | | - | Path to a file with exclude patterns (e.g. `.sftpignore`). |
| `max-deletes` | | `0` | Abort a `sync`/`clean` run that would delete more files than this (`0` = unlimited). See [delete guards](strategies.md#delete-guards). |

² Required unless `config-file` is set.

> **Removed in v2:** the `delete` input is gone; use `strategy: clean`.
> Passing `delete: true` now fails the run with a migration hint instead of
> silently falling back to `overlay`. The declaration disappears in v3.

### Behavior

| Input | Required | Default | Description |
|---|---|---|---|
| `build-mode` | | `prebuilt` | `prebuilt` downloads the platform-specific release binary and verifies its SHA-256 digest. `source` installs Go and compiles the selected action checkout. |
| `dry-run` | | `false` | Connect and log what would happen, change nothing. |
| `log-level` | | `normal` | `normal` logs one line per uploaded/deleted file. `quiet` suppresses per-file lines (connection info, per-target summaries, warnings and errors remain), keeping the log readable on huge deploys. `verbose` additionally explains every ignore decision (which pattern excluded which file). A dry run always logs per-file lines regardless: inspecting the plan is its whole point. |
| `concurrency` | | `4` | Number of files uploaded in parallel. Also bounds the worker pool that hashes local files for the `sync` strategy. |
| `sftp-request-concurrency` | | `16` | Advanced. Maximum in-flight SFTP requests *per file*: pipelining within one file's transfer, independent of `concurrency` (which controls how many files transfer at once). The two multiply: with the defaults, a single large file can have up to 16 requests in flight, and up to `concurrency` files transfer at a time. Lower it for a small or resource-constrained server; raise it for more throughput per file on a fast link to a capable server. |
| `sync-fast-path` | | `false` | For the `sync` strategy, reuse a file's manifest hash instead of re-reading it when size and modification time still match. See [the sync-fast-path trade-off](strategies.md#sync-fast-path-skip-re-hashing-unchanged-files). |
| `skip-unchanged` | | `false` | For the `overlay` strategy, skip uploading a file when the remote file already exists with the same size (one stat per file). Deliberately coarse but manifest-free; see [the skip-unchanged trade-off](strategies.md#skip-unchanged-skip-same-size-files). Ignored (with a warning) by `sync` and `clean`. |
| `manifest-name` | | `.easysftp-manifest.json` | File name of the `sync` strategy's per-target manifest. Must be a bare file name (no path). An unguessable name mitigates [the manifest being publicly downloadable](security.md#the-sync-manifest-in-web-roots) from web roots. Changing it mid-life starts a fresh manifest and leaves the old file behind on the server. |
| `retries` | | `2` | Retries per file on transient upload errors (exponential backoff). Also covers the initial connection: a transient dial failure (DNS hiccup, restarting sshd) is retried with the same backoff, while host key mismatches and authentication failures fail immediately. Additionally bounds how often a connection that drops mid-run is redialed: when a remote operation fails because the connection died (an upload, but also a remote scan, a delete or a sync manifest read/write), easySFTP reconnects (up to `retries` times per run) and retries the operation against the fresh connection. Completed files stay completed. `0` disables all of these. |
| `dir-mode` | | - | Octal permission (e.g. `755`) applied to every remote directory the run creates or touches, instead of whatever the server's umask produces. |
| `file-mode` | | - | Octal permission (e.g. `644`) applied to every uploaded file, instead of mirroring the local file's permission bits. |
| `preserve-times` | | `false` | Keep each uploaded file's local modification time on the server instead of "now". Useful when the server derives `Last-Modified`/ETag headers from mtimes or server-side tooling compares timestamps. Best-effort like the modes: a refusing server produces one warning per run, not a failure. Does not interact with the `sync` strategy's change detection, which compares content hashes, never remote timestamps. Note that a fresh `actions/checkout` stamps every file with the checkout time; combine with e.g. `git-restore-mtime` if the original times matter. |

`dir-mode` and `file-mode` are best-effort: some servers reject the chmod
(`SETSTAT`) request outright. A failure produces one warning per run, not a
failed deploy, so a restrictive server doesn't turn an otherwise-successful
deploy red. On shared hosting where the web server runs as a different user,
these are useful to force freshly created directories/files to a mode the web
server can actually read (see [troubleshooting.md](troubleshooting.md)).

Like `retries` or `concurrency`, these are run-wide behavior settings, not
per-target deployment shape; they apply the same way with or without
`config-file` and have no per-target override.

Prebuilt binaries support Linux, macOS, and Windows on both x64 and arm64
GitHub-hosted runners. Release refs `@vX`, `@vX.Y`, and `@vX.Y.Z` resolve via
the exact version in `.easysftp-version`. A commit SHA is accepted in prebuilt
mode only when it is the commit behind that exact release tag. Development
refs (`@main`, other commit SHAs, and local `uses: ./`) require
`build-mode: source`; prebuilt mode fails clearly instead of running an older
release.

## Outputs

| Output | Description |
|---|---|
| `files-uploaded` | Number of uploaded files (planned files in dry-run mode). |
| `files-deleted` | Number of remote files removed by the `clean`/`sync` strategy. |
| `files-skipped` | Number of unchanged files skipped (by the `sync` strategy, or by `overlay` with `skip-unchanged`). |
| `bytes-uploaded` | Total bytes transferred (planned bytes in dry-run mode). |
| `duration-ms` | Total runtime in milliseconds. |

A summary table is also written to the job summary of every run. Outputs and
the summary are populated even if a transfer fails partway: the values reflect
the progress completed before the failure, and the summary is clearly marked
as failed. Use `if: ${{ always() }}` when a later reporting or rollback step
must consume these partial results. See [examples](examples.md#using-the-outputs)
for how to consume outputs in later steps.

When `uploads` (or a `config-file`) defines more than one target, the job
summary also breaks the totals down per target (local path, remote path,
strategy, and that target's own uploaded/deleted/skipped/bytes counts), so a
number that looks off in the totals can be traced to the target that produced
it. The `files-*`/`bytes-uploaded` outputs stay run-wide totals; there is no
per-target output.

## The `uploads` mapping

One mapping per line, `local => remote`. Lines starting with `#` are ignored.

```yaml
uploads: |
  # directory => directory (recursive)
  ./dist/ => /var/www/html/

  # single file => exact remote path (rename on the fly)
  ./config/prod.json => /etc/app/config.json

  # single file => into a remote directory (note the trailing slash)
  ./robots.txt => /var/www/html/
```

Rules:

- **Directories** are uploaded recursively. Remote directories are created automatically.
- **Single files** map onto the exact remote path, unless the remote side ends
  with `/`, which means "into this directory" keeping the original file name.
- Single files only support the `overlay` strategy (`sync`/`clean` reconcile a
  directory tree and are rejected for single-file targets).
- Symlinks, sockets and other non-regular files are skipped.

## Ignore patterns

`ignore` (inline) and `ignore-from` (a file, e.g. `.sftpignore`) use
[gitignore syntax](https://git-scm.com/docs/gitignore):

```yaml
ignore: |
  *.map
  *.log
  node_modules/
  !important.log
```

- Patterns are matched against the path **relative to the local root** of each target.
- `!pattern` re-includes files excluded by an earlier pattern.
- `ignore` and `ignore-from` are additive; in the config file, per-target
  `ignore` lists add to the global one.
- An ignored directory (e.g. `node_modules/`) is skipped without being walked
  at all, so huge excluded trees cost nothing during planning. This pruning is
  automatically disabled as soon as any pattern is a `!` re-include, because a
  re-include may point below an ignored directory; results are identical either
  way, only planning speed differs.

## The YAML config file

For multiple targets with different strategies, point `config-file` at a YAML
file. Connection settings (server, credentials, host key) always stay in the
action inputs. **Never put credentials in this file**.

```yaml
- uses: eiserv/easySFTP@v2
  with:
    server: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
    host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINT }}
    config-file: .github/easysftp.yml
```

```yaml
# .github/easysftp.yml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eiserv/easySFTP/main/schema/easysftp.schema.json
version: 1
strategy: overlay          # default for all targets
ignore:
  - "*.map"
guards:
  max_deletes: 200         # 0 = unlimited
targets:
  - local: ./dist/
    remote: /var/www/html/
    strategy: sync
  - local: ./docs/
    remote: /var/www/docs/
    strategy: clean
```

### Fields

| Field | Required | Default | Description |
|---|---|---|---|
| `version` | ✅ | - | Config format version. Must be `1`. |
| `strategy` | | `overlay` | Default strategy for all targets. |
| `ignore` | | - | Global gitignore-style excludes, applied to every target. |
| `guards.max_deletes` | | `0` | Abort a run that would delete more files than this (0 = unlimited). See [delete guards](strategies.md#delete-guards). |
| `targets` | ✅ | - | List of upload targets (at least one). |
| `targets[].local` | ✅ | - | Local file or directory. |
| `targets[].remote` | ✅ | - | Remote path. |
| `targets[].strategy` | | global | Per-target strategy override. |
| `targets[].ignore` | | - | Per-target excludes, additive to the global list. |

Unknown keys are rejected with an error (they are never silently ignored), and
`config-file` cannot be combined with the `uploads`, `strategy`, `ignore`,
`ignore-from` or `max-deletes` inputs.

### Editor support

The `# yaml-language-server` modeline at the top of the file enables
autocomplete and validation in editors from the bundled
[JSON Schema](../schema/easysftp.schema.json). A fully commented example lives
at [easysftp.example.yml](easysftp.example.yml).
