# Migrating from Dylan700/sftp-upload-action

easySFTP is a from-scratch Go implementation inspired by
[Dylan700/sftp-upload-action][dylan], whose last release was in 2024. If you
are coming from it, this page is the whole switch: an input-by-input mapping,
a before/after workflow, and the handful of behavior differences that are
worth knowing before the first run.

Migrating from an older **easySFTP** version instead? See
[Migrating from v2 to v3](migration-v3.md).

- [The five-minute version](#the-five-minute-version)
- [Input mapping](#input-mapping)
- [`uploads` becomes `source` + `target`](#uploads-becomes-source--target)
- [`ignore` becomes `exclude`](#ignore-becomes-exclude)
- [`delete: true` becomes a mode](#delete-true-becomes-a-mode)
- [Host key verification is required](#host-key-verification-is-required)
- [Behavior differences worth knowing](#behavior-differences-worth-knowing)
- [What has no equivalent](#what-has-no-equivalent)

## The five-minute version

Before:

```yaml
- uses: actions/checkout@v6
- name: SFTP upload
  uses: Dylan700/sftp-upload-action@latest
  with:
    server: sftp.example.com
    username: deploy
    password: ${{ secrets.SFTP_PASSWORD }}
    port: 22
    uploads: |
      ./dist/ => ./www/public_html/
    ignore: |
      *.git
      */**/*git*
    delete: "true"
```

After:

```yaml
- uses: actions/checkout@v6
- name: SFTP upload
  uses: eiserv/easySFTP@v3
  with:
    host: sftp.example.com
    username: deploy
    password: ${{ secrets.SFTP_PASSWORD }}
    port: "22"
    source: ./dist/
    target: /var/www/public_html/
    mode: clean
    exclude: |
      .git/
    host-key: ${{ secrets.SFTP_HOST_KEY }}
```

Four things changed beyond the renames: the remote path is absolute, the
ignore patterns are gitignore syntax, `delete: true` became `mode: clean`
(read [`mode: sync`](#delete-true-becomes-a-mode) before you settle on
`clean`), and the connection is now verified against a pinned host key.

You do not have to get every rename right on the first try. Every removed
input name below is still declared by `action.yml` purely so that passing it
fails the run with a message naming its replacement, instead of the runner
silently dropping it.

## Input mapping

| Dylan700/sftp-upload-action | easySFTP | Notes |
|---|---|---|
| `server` | `host` | Same meaning, new name. |
| `username` | `username` | Unchanged. |
| `password` | `password` | Unchanged. Keep it in a secret. |
| `key` | `private-key` | PEM/OpenSSH private key. Keep it in a secret. |
| `passphrase` | `passphrase` | Unchanged. |
| `port` | `port` | Unchanged, still defaults to 22. |
| `uploads` | `source` + `target` | One mapping per deployment, see [below](#uploads-becomes-source--target). |
| `ignore` | `exclude` | Gitignore syntax, not globs, see [below](#ignore-becomes-exclude). |
| `ignore-from` | `exclude` | No file-based input; paste the patterns, or use a config file. |
| `delete` | `mode` | `delete: true` → `mode: clean`, see [below](#delete-true-becomes-a-mode). |
| `dry-run` | `dry-run` | Unchanged, and considerably more detailed. |
| *(none)* | `host-key` / `known-hosts` / `allow-any-host-key` | **New and required**, see [below](#host-key-verification-is-required). |
| *(none)* | `file-mode`, `dir-mode`, `preserve-times` | Optional. Worth setting `file-mode` on Windows runners. |
| *(none)* | `log-level` | `normal` (default), `verbose` (one line per file), `debug`. |
| *(none)* | `config` | A YAML file for multiple deployments and advanced settings. |

## `uploads` becomes `source` + `target`

The `folder/ => upload_folder/` mini-syntax is gone. One mapping is two
inputs:

```yaml
    source: ./dist/
    target: /var/www/public_html/
```

**Relative remote paths still work.** `./www/public_html/` is interpreted
relative to the directory the SFTP session starts in (usually the login
user's home), exactly as before. Absolute paths are recommended anyway,
because they survive a server-side home-directory change.

**A directory `source` always uploads its contents into `target`**, with or
without a trailing slash, which is what `folder/ => upload_folder/` did too.
(A trailing slash only means something for a *single-file* `source`: then it
selects "into this directory" instead of "to this exact path", which lets you
rename a file while uploading it. See [Examples](examples.md).)

If you had **several mappings** in one `uploads` block, that is the one case
that needs a config file, because inline mode covers exactly one deployment:

```yaml
- uses: eiserv/easySFTP@v3
  with:
    config: .github/easysftp.yml
    password: ${{ secrets.SFTP_PASSWORD }}
```

```yaml
# .github/easysftp.yml
version: 3

connection:
  host: sftp.example.com
  username: deploy
  host_key: |
    SHA256:nThbg6kXUpJWGl7E1IGOCspRomTxdCARLviKw6E5SY8

deployments:
  site:
    source: ./html/
    target: /var/www/public_html/
  sources:
    source: ./src/
    target: /var/www/src/
```

Deployments run in the order they are listed, and each gets its own row in the
job summary. The full file format is in
[Configuration](configuration.md).

## `ignore` becomes `exclude`

Both take one pattern per line and both support `!` negation, but the syntax
underneath is different: `ignore` took glob patterns, `exclude` takes real
[gitignore syntax](https://git-scm.com/docs/gitignore). In practice the
patterns get *shorter*:

| Intent | `ignore` (before) | `exclude` (after) |
|---|---|---|
| Drop the `.git` directory | `*.git`<br>`*/**/*git*` | `.git/` |
| Drop all `.map` files | `**/*.map` | `*.map` |
| Drop `node_modules` anywhere | `**/node_modules/**` | `node_modules/` |
| Upload only `index.html` | `*`<br>`!index.html` | `*`<br>`!index.html` |

Two rules carry the change:

- A pattern without a slash matches at **every** level, so `*.map` already
  means "anywhere". You rarely need `**/`.
- A trailing slash means "directory", and easySFTP then skips walking into it
  entirely, which is also why `node_modules/` is much faster than a pattern
  that matches every file inside it.

Patterns match against the path relative to the local root of the deployment,
not the repository root. Test them with `dry-run: true` before trusting them,
and use `log-level: debug` to see which pattern excluded which file.

`ignore-from` has no counterpart: put the patterns in `exclude`, or move the
deployment into a config file, where `exclude` is a normal YAML list.

## `delete: true` becomes a mode

`delete: true` wiped the remote upload directory and then uploaded. easySFTP
calls that `mode: clean` and it behaves the same way, but there is now a
better option in between:

| Mode | What it does | Closest old behavior |
|---|---|---|
| `overlay` (default) | Uploads, never deletes. | `delete: false` |
| `sync` | Uploads changed files and deletes only files that the previous run uploaded and that are gone now. | *(no equivalent)* |
| `clean` | Wipes the remote target, then uploads everything. | `delete: true` |

**Prefer `sync` for a website.** `clean` means the site is missing between the
wipe and the last file, and it re-uploads everything on every run. `sync`
tracks what it uploaded in a small manifest file inside the target, so it
uploads only what changed, deletes only what it is responsible for, and never
touches files that something else put there. Full comparison in
[Strategies](strategies.md).

Whichever you pick, the destructive modes now have guards the old action did
not have: they refuse to run against `/` or `.`, and `safety.max_deletes` in a
config file can abort a run that suddenly wants to delete more files than you
expect.

## Host key verification is required

This is the one change that will stop a copy-pasted workflow on the first run,
and it is deliberate. Dylan700/sftp-upload-action does not verify the server's
host key, so a machine-in-the-middle can accept your deploy, keep your
credentials and hand you a success. easySFTP refuses to connect unless you
either pin the key or explicitly say you do not want to.

Get the fingerprint once, from a machine you trust:

```bash
ssh-keyscan sftp.example.com | ssh-keygen -lf -
```

Put the `SHA256:...` line(s) into the `host-key` input (one per line; any
match is accepted, so you can pin all of the server's keys at once). The
verbatim `ssh-keyscan` output works too, via `known-hosts`.

The fingerprint is a **public** value, so it does not have to live in a
secret, though many people keep it in one anyway to avoid leaking the
hostname.

If you knowingly want the old behavior, it is one input:

```yaml
    allow-any-host-key: "true"
```

That is a supported choice, not a deprecated one, but it is an explicit choice
now, and the job summary reports the connection as unverified. See the
[Security guide](security.md).

## Behavior differences worth knowing

- **Uploads are atomic per file.** Each file is written to a temporary name in
  the target directory and renamed over the final name, so a visitor never
  sees a half-written file, and a failed run leaves the old file in place. On
  servers without `posix-rename@openssh.com` the rename is not atomic; see
  [Troubleshooting](troubleshooting.md).
- **Transfers run in parallel** (tunable via `advanced.concurrency` in a
  config file), which is most of the speed difference.
- **Failures retry** with exponential backoff, and a dropped connection is
  redialed rather than failing the run.
- **There are outputs**: `files-uploaded`, `files-deleted`, `files-skipped`,
  `bytes-uploaded`, `duration-ms`, plus a job summary table. Handy for a
  Slack/Discord notification step or for asserting that a deploy did anything
  at all.
- **`dry-run` prints the whole plan**, per file, including which files would be
  deleted. Use it as the first step of the migration: run once with
  `dry-run: true` and compare the file list against what you expect before
  letting it write anything.
- **Windows runners work**, but they have no POSIX permission bits to mirror,
  so uploaded files end up world-writable unless you set `file-mode: "0644"`.
  See [Configuration](configuration.md).

## What has no equivalent

- **`ignore-from`**: no file-based pattern input. Paste the patterns into
  `exclude`, or use a config file.
- **Multiple mappings in one `with:` block**: use a config file (above). This
  is intentional, not a gap: the mini-syntax is what made multi-mapping
  workflows hard to read and hard to validate.
- **`uses: ...@latest`**: easySFTP publishes `@v3` (moving major tag) and exact
  `@v3.x.y` tags. Pin `@v3` for automatic patches, or a full commit SHA if
  your policy requires it.

[dylan]: https://github.com/Dylan700/sftp-upload-action
