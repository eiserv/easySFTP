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
| `timeout` | | `30` | Connection timeout in seconds. |

¹ At least one of `password` / `private-key` is required. If both are set, the key is tried first.

### What to upload

| Input | Required | Default | Description |
|---|---|---|---|
| `uploads` | ² | - | One `local => remote` mapping per line. Directories are uploaded recursively; single files are supported too. |
| `config-file` | | - | Path to a [YAML config file](#the-yaml-config-file) with per-target strategies and delete guards. Mutually exclusive with `uploads`/`strategy`/`ignore`/`ignore-from`. |
| `strategy` | | `overlay` | [Reconciliation strategy](strategies.md): `overlay`, `sync` or `clean`. |
| `ignore` | | - | Gitignore-style exclude patterns, one per line. `!` re-includes. |
| `ignore-from` | | - | Path to a file with exclude patterns (e.g. `.sftpignore`). |

² Required unless `config-file` is set.

> **Removed in v2:** the `delete` input is gone — use `strategy: clean`.
> Passing `delete: true` now fails the run with a migration hint instead of
> silently falling back to `overlay`. The declaration disappears in v3.

### Behavior

| Input | Required | Default | Description |
|---|---|---|---|
| `build-mode` | | `prebuilt` | `prebuilt` downloads the platform-specific release binary and verifies its SHA-256 digest. `source` installs Go and compiles the selected action checkout. |
| `dry-run` | | `false` | Connect and log what would happen, change nothing. |
| `concurrency` | | `4` | Number of files uploaded in parallel. Also bounds the worker pool that hashes local files for the `sync` strategy. |
| `retries` | | `2` | Retries per file on transient upload errors (exponential backoff). |

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
| `files-skipped` | Number of unchanged files skipped by the `sync` strategy. |
| `bytes-uploaded` | Total bytes transferred. |
| `duration-ms` | Total runtime in milliseconds. |

A summary table is also written to the job summary of every run. Outputs and
the summary are populated even if a transfer fails partway: the values reflect
the progress completed before the failure, and the summary is clearly marked
as failed. Use `if: ${{ always() }}` when a later reporting or rollback step
must consume these partial results. See [examples](examples.md#using-the-outputs)
for how to consume outputs in later steps.

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

## The YAML config file

For multiple targets with different strategies, point `config-file` at a YAML
file. Connection settings (server, credentials, host key) always stay in the
action inputs. **Never put credentials in this file**.

```yaml
- uses: eiserv/easySFTP@v1
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
`config-file` cannot be combined with the `uploads`, `strategy`, `ignore` or
`ignore-from` inputs.

### Editor support

The `# yaml-language-server` modeline at the top of the file enables
autocomplete and validation in editors from the bundled
[JSON Schema](../schema/easysftp.schema.json). A fully commented example lives
at [easysftp.example.yml](easysftp.example.yml).
