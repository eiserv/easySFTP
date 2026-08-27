# Troubleshooting & FAQ

Common errors, what they mean and how to fix them. If your problem is not
listed, [open an issue](https://github.com/eiserv/easySFTP/issues), ideally
with the log of a `dry-run: true` run.

Ports, chrooted paths and "SSH has to be enabled first" are usually not
errors but provider quirks: see [Provider notes](providers.md), which also
explains how to find your own port, path and host key in three commands, and
lists [what easySFTP needs from a server](providers.md#what-easysftp-needs-from-a-server)
if yours is not OpenSSH.

## Action startup problems

### `the '<x>' input was ... in easySFTP v3`

You are passing a v2 input that was renamed or moved in v3 (e.g. `server`,
`uploads`, `strategy`, `ignore`, `host-key-fingerprint`, `config-file`, or an
advanced/proxy input). The error names the v3 replacement; the full mapping is
in the [migration guide](migration-v3.md).

### The build takes longer than expected on `@main` or a commit SHA

The build mode is chosen automatically from the action ref. Release tags
(`@v3`, `@v3.0`, `@v3.0.0`) and the exact release commit SHA download the
verified prebuilt binary; every other ref (`@main`, other commit SHAs, local
`uses: ./`) builds from source, which installs Go first. This is intended: it
avoids silently running the last published binary for newer source code. There
is no `build-mode` input in v3; setting it fails with a migration hint.

### `SHA-256 mismatch` or `checksums.txt has no SHA-256 entry`

The release is incomplete or an asset does not match its published checksum.
The binary is not executed. Retry after the maintainer repairs the exact
release; do not bypass checksum verification.

## Connection problems

### `connecting to <host>:22: dial tcp ...: i/o timeout`

The runner cannot reach the server.

- Check `host` and `port`.
- Many hosters firewall SSH to allowlisted IPs. GitHub-hosted runners use
  [changing IP ranges](https://docs.github.com/en/actions/using-github-hosted-runners/about-github-hosted-runners#ip-addresses),
  so an IP allowlist usually requires a self-hosted runner or a relaxed rule.
- Raise the timeout (`advanced.timeout` in the config file, default 30 s) if
  the server is just slow to accept connections.

### A large deploy dies partway through with an EOF or "connection lost"

`timeout` only bounds the initial connection, not the whole run. Once
connected, easySFTP sends an SSH keepalive every 30 seconds for the rest of
the run, so idle-looking connections survive NAT gateways/firewalls that
drop them, and the server's own `ClientAliveInterval` probes get answered.
This is automatic and not configurable. If transfers still die mid-run, the
connection itself is being reset (not just idled out); check for a very
short server-side session/idle limit, or a flaky network path.

### `the server would not open more than N SSH connection(s)`

easySFTP sizes its connection pool itself (see [transfer tuning](tuning.md)),
and this host allows fewer connections per account than it asked for. sshd's
`MaxStartups` and the per-account limits of shared hosting are the usual
reasons.

It is a degradation, not a failure: the deploy finishes on the connections it
has, and easySFTP stops asking for more for the rest of the run. If you see it
on every deploy, pin the number your host allows and the warning goes away:

```yaml
advanced:
  connections: 1
```

The other two settings stay adaptive; each one is resolved on its own.

### `connecting to <host>:22: ssh: handshake failed: ... unable to authenticate`

The server rejected the credentials.

- Verify the secret names in your workflow match the configured secrets
  (a missing secret silently expands to an empty string).
- Password auth: some servers disable it entirely (`PasswordAuthentication no`);
  use a key instead.
- Key auth: the key must be in OpenSSH or PEM format. If it is encrypted, set
  `passphrase`. Check that the *public* key is in the server's
  `authorized_keys`.

### `host key mismatch for <host>: got SHA256:..., want one of: ...`

The server presented a key that matches none of your pinned fingerprints.

- If the server was migrated or its keys rotated, re-run
  `ssh-keyscan <server> | ssh-keygen -lf -` and update the secret.
- If you did **not** expect a key change, stop and investigate. This is
  exactly the man-in-the-middle situation pinning exists for.
- You can pin multiple fingerprints (one per line); the connection is accepted
  if any matches.

### `the identity of <host> cannot be verified: no host-key or known-hosts configured`

New in v3: a run without a pinned host key now fails instead of warning and
connecting anyway. Set `host-key` (SHA256 fingerprints) or `known-hosts`
(`ssh-keyscan` output). To connect without verification anyway (**not
recommended**, allows man-in-the-middle attacks), set `allow-any-host-key:
true`. In the config file the fields are `connection.host_key`,
`connection.known_hosts` and `connection.allow_any_host_key`.

### `host-key must be a SHA256 fingerprint like 'SHA256:...'`

Pass the fingerprint (`SHA256:nThbg...`), not the raw `ssh-keyscan` line and
not an MD5 fingerprint. Get the right format with:

```console
ssh-keyscan sftp.example.com | ssh-keygen -lf -
```

## Upload problems

### `permission denied` while uploading

The SFTP user cannot write to the target directory. Check ownership and
permissions on the server. With chrooted SFTP setups remember that paths are
relative to the chroot: `/upload/...`, not `/home/user/upload/...`.

### Which upload failures easySFTP retries

A failure the server itself reported reads like
`sftp: "Disk quota exceeded" (SSH_FX_FAILURE)`. The quoted text is the server's
own explanation and is usually the only part worth reading; the `SSH_FX_*` name
after it is the protocol status code it sent, which decides whether easySFTP
tries again.

Codes that can only ever come back the same way fail the file immediately,
whatever `retries` is set to: `SSH_FX_NO_SUCH_FILE` and
`SSH_FX_PERMISSION_DENIED` (which reach you as `file does not exist` and
`permission denied`), plus `SSH_FX_OP_UNSUPPORTED` and `SSH_FX_BAD_MESSAGE`,
whose failures add `retrying would not have helped` so the skipped retries are
not a mystery.

`SSH_FX_FAILURE` is retried, because it is a catch-all: servers use it both for
permanent conditions (a disk quota, a full or read-only filesystem, a path
length limit) and for transient ones, and only the message text tells them
apart. easySFTP does not guess from that text, so a quota-exceeded deploy pays
the retry backoff for the files it has in flight when the quota runs out
(remaining files are not started once one fails). If you already know the cause
is permanent, `advanced.retries: 0` in the config file makes such a run fail
immediately.

### Site deploys "successfully" but returns 403s, or files land with the wrong owner-readable bits

Directories created by the run get whatever the server's umask produces, and
uploaded files mirror their local permission bits. On shared hosting where the
web server runs as a different user than your SFTP account, that default can
produce directories the web server can't read. Force a known-good permission
with the `dir-mode` and `file-mode` inputs:

```yaml
    dir-mode: "0755"
    file-mode: "0644"
```

In config mode the same settings are `permissions.directories` and
`permissions.files`. Both are best-effort; see
[configuration.md](configuration.md#permissions-inline-mode).

Deploying from a **Windows runner** makes this more likely: Windows has no
POSIX permission bits, so the mirrored local mode is `0666` for every writable
file (`0444` for read-only ones), and your files land world-writable. Setting
`file-mode` explicitly is the fix, and is worth doing even when the deploy
appears to work.

### `replacing "<path>": ...` or leftover `.easysftp-tmp` files

easySFTP uploads to a temporary sibling file (named `<path>.easysftp-tmp.<n>`)
and renames it over the target. On a server supporting the
`posix-rename@openssh.com` extension that rename is atomic. Without it, the
live file is first moved aside to `<path>.easysftp-tmp.bak`, the upload is
renamed into place, and the backup is removed; if the rename fails, the backup
is put back, so a failed publish never costs you the file it was replacing.
A hard kill mid-upload (a cancelled workflow past its grace period, a job
timeout, a reclaimed runner) can leave either file behind; both are safe to
delete manually.

If **every** overwrite fails this way while new files upload fine, the server
is refusing the rename rather than lacking the extension: easySFTP only falls
back to the move-aside path on a server that does not offer atomic rename, and
never removes a live file to retry a rename the server has already declined. Check
write permissions on the existing target files and any policy on the server
that forbids replacing them. See
[what easySFTP needs from a server](providers.md#what-easysftp-needs-from-a-server).

Later deploys clean these up automatically: before uploading, every run
sweeps the directories receiving files in that run, plus the deployment's
target directory itself, and removes temp files older than an hour (logged
as `removed stale temporary file ...`). Nothing above the target directory
is ever listed or touched. The age margin keeps the sweep from deleting a
concurrently running deploy's in-progress upload. The sweep intentionally
runs in every mode, `overlay` included, and is on by default: even though
overlay never deletes your files, the sweep only ever removes the action's
own `*.easysftp-tmp` / `*.easysftp-tmp.<n>` / `*.easysftp-tmp.bak` debris,
never a file it did not name itself. A real file of yours that happens to carry such a name is part
of the deployment and is therefore never swept, in `sync` too, where it is
protected even on runs that leave it unchanged. An orphan in a directory
that no later deploy uploads into
stays until one does, or until you delete it manually. If the target is a
public web root, also add a deny rule for these names next to the manifest
rule; see [security.md](security.md#temporary-upload-files-in-web-roots).

### Ignored files are uploaded anyway / patterns don't match

- Patterns match against the path **relative to the local root of each
  deployment**, not the repository root.
- Directory patterns need a trailing slash (`node_modules/`), just like
  gitignore.
- Test your patterns cheaply with `dry-run: true`.

### Symlinks are missing on the server

Symlinks, sockets and other non-regular files are skipped by design. SFTP
uploads regular file content. If your build output contains symlinks (e.g.
pnpm's `node_modules`), upload a bundled/dereferenced build instead.

When a deployment has any non-regular files, the log shows one aggregated
warning per deployment (not one per file), e.g.:

```
::warning::skipped 37 non-regular file(s) (symlinks, sockets, …) under ./dist/: SFTP uploads regular files only
```

No warning is logged when there is nothing to skip.

## Strategy questions

### `sync` did not delete a file I removed locally

`sync` only deletes files listed in its manifest: files it uploaded itself.
Files that were already on the server before your first sync are never
touched. Run `mode: clean` once for a fresh start, then continue with
`sync`. See [deployment modes](strategies.md#sync).

### What is `.easysftp-manifest.json` on my server?

The [sync manifest](strategies.md#sync): the list of files (with content
hashes) the last sync uploaded. Leave it in place. Without it, the next sync
re-uploads everything and deletes nothing. It is excluded from uploads and
never deleted by `sync` itself.

### `refusing a destructive mode on remote root`

`sync` and `clean` refuse a remote target that resolves to `/`, `.`, `~` or
the empty string, and one that climbs above the directory the session starts
in (`..`, `../..`, `dist/../..`). Deploy into a specific subdirectory instead.

If you did not write `..` anywhere, check how the target is built: an
expression such as `${{ inputs.dir }}/..` lands here when `inputs.dir` is
empty, and so does a path whose last component was stripped by a shell.

This guard cannot be disabled; see
[delete guards](strategies.md#delete-guards).

### `refusing to delete N files: exceeds safety.max_deletes`

Your run would delete more files than `safety.max_deletes` allows. Inspect the
plan with `dry-run: true`; if the deletions are intended, raise (or remove)
the limit in the config file.

## Configuration errors

### `when 'config' is set, all non-secret settings come from the config file`

You set `config` and also an inline connection/deployment input (like `host`,
`source`, `target`, `mode` or `exclude`). v3 has no mixed mode: remove the
inline input, or drop `config` and configure everything inline. Only
credentials, `dry-run` and `log-level` may be combined with `config`.

### `easySFTP could not determine what to deploy`

Inline mode needs both `source` and `target`. Add them, or switch to a config
file (`config`) for multiple deployments; the error spells out both fixes.

### `unknown option "<x>" at "<path>"; did you mean "<y>"?`

The config file rejects unknown keys instead of silently ignoring them,
usually a typo. The error gives the location and, when close enough, a
suggestion. Enable [editor validation](configuration.md#validation) via the
JSON Schema to catch these while typing.

### `'version' must be 3` / a v1 config file is rejected

v3 changed the config file format (named deployments, connection in the file).
Convert your `version: 1` file following the
[migration guide](migration-v3.md#migrating-a-v1-config-file).

### `mode "sync" requires a directory, but local path ... is a single file`

`sync` and `clean` reconcile a directory tree; for single files use `overlay`
(the default).
