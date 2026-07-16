# Troubleshooting & FAQ

Common errors, what they mean and how to fix them. If your problem is not
listed, [open an issue](https://github.com/eiserv/easySFTP/issues), ideally
with the log of a `dry-run: true` run.

## Action startup problems

### `prebuilt mode requires a release tag ref ... Use build-mode: source`

Prebuilt mode accepts `@vX`, `@vX.Y`, `@vX.Y.Z`, or the full commit SHA behind
that exact release. For `@main`, other commit SHAs, or local `uses: ./`, add
`build-mode: source`. This avoids silently running the last published binary
for newer source code.

### `SHA-256 mismatch` or `checksums.txt has no SHA-256 entry`

The release is incomplete or an asset does not match its published checksum.
The binary is not executed. Retry after the maintainer repairs the exact
release; do not bypass checksum verification.

## Connection problems

### `connecting to <host>:22: dial tcp ...: i/o timeout`

The runner cannot reach the server.

- Check `server` and `port`.
- Many hosters firewall SSH to allowlisted IPs. GitHub-hosted runners use
  [changing IP ranges](https://docs.github.com/en/actions/using-github-hosted-runners/about-github-hosted-runners#ip-addresses),
  so an IP allowlist usually requires a self-hosted runner or a relaxed rule.
- Raise `timeout` (default 30 s) if the server is just slow to accept
  connections.

### `connecting to <host>:22: ssh: handshake failed: ... unable to authenticate`

The server rejected the credentials.

- Verify the secret names in your workflow match the configured secrets
  (a missing secret silently expands to an empty string).
- Password auth: some servers disable it entirely (`PasswordAuthentication no`)
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

### `host-key-fingerprint must be a SHA256 fingerprint like 'SHA256:...'`

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

### Site deploys "successfully" but returns 403s, or files land with the wrong owner-readable bits

Directories created by the run get whatever the server's umask produces, and
uploaded files mirror their local permission bits. On shared hosting where the
web server runs as a different user than your SFTP account, that default can
produce directories the web server can't read. Set `dir-mode` (and, if needed,
`file-mode`) to force a known-good permission, e.g. `dir-mode: "755"` and
`file-mode: "644"`. Both are best-effort — see [configuration.md](configuration.md#behavior).

### `replacing "<path>": ...` or leftover `.easysftp-tmp` files

easySFTP uploads to a temporary sibling file and renames it over the target
(atomic on servers supporting the `posix-rename@openssh.com` extension, with a
remove+rename fallback otherwise). A hard crash mid-upload can leave a
`*.easysftp-tmp` file behind; it is safe to delete and will be cleaned up by
the next successful upload of the same file.

### Ignored files are uploaded anyway / patterns don't match

- Patterns match against the path **relative to the local root of each
  target**, not the repository root.
- Directory patterns need a trailing slash (`node_modules/`), just like
  gitignore.
- Test your patterns cheaply with `dry-run: true`.

### Symlinks are missing on the server

Symlinks, sockets and other non-regular files are skipped by design. SFTP
uploads regular file content. If your build output contains symlinks (e.g.
pnpm's `node_modules`), upload a bundled/dereferenced build instead.

## Strategy questions

### `sync` did not delete a file I removed locally

`sync` only deletes files listed in its manifest: files it uploaded itself.
Files that were already on the server before your first sync are never
touched. Run `strategy: clean` once for a fresh start, then continue with
`sync`. See [strategies](strategies.md#sync).

### What is `.easysftp-manifest.json` on my server?

The [sync manifest](strategies.md#sync): the list of files (with content
hashes) the last sync uploaded. Leave it in place. Without it, the next sync
re-uploads everything and deletes nothing. It is excluded from uploads and
never deleted by `sync` itself.

### `refusing a destructive strategy on remote root`

`sync` and `clean` refuse to operate on `/` (or `.`) as the remote target,
always. Deploy into a specific subdirectory instead. This guard cannot be
disabled; see [delete guards](strategies.md#delete-guards).

### `refusing to delete N files: exceeds guards.max_deletes`

Your run would delete more files than `guards.max_deletes` allows. Inspect the
plan with `dry-run: true`; if the deletions are intended, raise (or remove)
the limit in the config file.

## Configuration errors

### `when 'config-file' is set, put targets/strategy/ignore/guards in the file`

`config-file` replaces the `uploads`, `strategy`, `ignore` and `ignore-from`
inputs. Remove them from the step. Connection inputs stay.

### `the 'delete' input was removed in v2 — use 'strategy: clean' instead`

Replace `delete: true` with `strategy: clean` in your step. The inputs are
equivalent; `delete` only remains declared so that this error fires instead of
the run silently degrading to the `overlay` default.

### `strategy "sync" requires a directory, but local path ... is a single file`

`sync` and `clean` reconcile a directory tree; for single files use `overlay`
(the default).

### `config-file "..." is not valid: field <x> not found`

The config file rejects unknown keys instead of silently ignoring them,
usually a typo. Enable [editor validation](configuration.md#editor-support)
via the JSON Schema to catch these while typing.
