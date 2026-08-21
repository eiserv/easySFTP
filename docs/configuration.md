# Configuration reference

easySFTP has exactly two configuration modes, and never mixes them:

1. **Inline mode** for the common single-target deploy: connection and one
   deployment in the workflow's `with:`.
2. **Config mode** for everything complex: `config` points at a YAML file
   that holds every non-secret setting. Only credentials, `dry-run` and
   `log-level` may be combined with it.

Every non-secret setting has exactly one home, so there is never a precedence
question between the workflow and the config file.

- [Quick start (inline mode)](#quick-start-inline-mode)
- [Common configuration](#common-configuration)
- [Config mode (multiple and advanced deployments)](#config-mode-multiple-and-advanced-deployments)
- [The config file](#the-config-file)
- [The `source` / `target` mapping](#the-source--target-mapping)
- [Exclude patterns](#exclude-patterns)
- [Outputs](#outputs)
- [Complete input reference](#complete-input-reference)

## Quick start (inline mode)

```yaml
- uses: eiserv/easySFTP@v3
  with:
    host: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    password: ${{ secrets.SFTP_PASSWORD }}
    host-key: ${{ secrets.SFTP_HOST_KEY }}
    source: dist
    target: /var/www/html
```

The four things you have to understand are `host` (where), `username` +
`password`/`private-key` (who), `source` (what) and `target` (where to).
Everything else has a sensible default.

## Common configuration

### Connection (inline mode)

| Input | Required | Default | Description |
|---|---|---|---|
| `host` | ✅ | - | Hostname or IP of the SFTP server. |
| `port` | | `22` | SSH port. |
| `username` | ✅ | - | Username for authentication. |
| `password` | ¹ | - | Password. **Use a secret.** |
| `private-key` | ¹ | - | SSH private key (OpenSSH/PEM format). **Use a secret.** |
| `passphrase` | | - | Passphrase of the private key, if encrypted. |
| `host-key` | ² | - | Expected SHA256 host key fingerprint(s), one per line (`SHA256:...`). See [Security](security.md). |
| `known-hosts` | ² | - | Expected host key(s) in OpenSSH `known_hosts` format (verbatim `ssh-keyscan` output). Alternative to `host-key`; a key matching either is accepted. |
| `allow-any-host-key` | ² | `false` | Explicitly skip host key verification. **Not recommended** (allows man-in-the-middle attacks). |

¹ At least one of `password` / `private-key` is required. If both are set, the key is tried first.
² You must set `host-key` **or** `known-hosts`, **or** explicitly opt out with `allow-any-host-key: true`. A run with none of the three fails. This is the one behavior change v3 makes on purpose; see [Security](security.md).

### Deployment (inline mode)

| Input | Required | Default | Description |
|---|---|---|---|
| `source` | ✅ | - | Local path (file or directory) to upload. |
| `target` | ✅ | - | Remote destination path. |
| `mode` | | `overlay` | [Reconciliation mode](strategies.md): `overlay`, `sync` or `clean`. |
| `exclude` | | - | Gitignore-style exclude patterns, one per line. `!` re-includes. |

### Permissions (inline mode)

| Input | Default | Description |
|---|---|---|
| `file-mode` | mirror local | Octal permission (e.g. `"0644"`) for every uploaded file. |
| `dir-mode` | server umask | Octal permission (e.g. `"0755"`) for every remote directory the run creates or touches. |
| `preserve-times` | `false` | Keep each uploaded file's local modification time on the server. |

All three are best-effort: a server that rejects the `SETSTAT` request
produces one warning per deployment, not a failure. Their config-mode
equivalents are [`permissions.*`](#permissions); like every other non-secret
setting they belong to exactly one mode, so setting them alongside `config`
fails the run. Set at least `file-mode: "0644"` when you deploy from a
Windows runner (see the [note below](#permissions)).

### Run-wide switches (both modes)

| Input | Default | Description |
|---|---|---|
| `dry-run` | `false` | Connect and log what would happen, change nothing. |
| `log-level` | `normal` | `normal` logs connection status, one summary per deployment, warnings and errors. `verbose` additionally logs one line per uploaded/deleted/skipped file. `debug` additionally explains every exclude decision. A dry run always logs per-file lines regardless: inspecting the plan is its whole point. |

These two are the only non-secret settings valid in both modes: they change
how a run reports, not what it deploys, so they never belong to "one source of
truth" for a deployment.

## Config mode (multiple and advanced deployments)

When `config` is set, all non-secret settings come from the file. The workflow
step shrinks to the config path plus credentials:

```yaml
- uses: eiserv/easySFTP@v3
  with:
    config: .github/easysftp.yml
    private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
    # optional, only for a jump host:
    # proxy-private-key: ${{ secrets.JUMP_PRIVATE_KEY }}
```

Setting any inline connection/deployment/permission input (`host`, `port`,
`username`, `host-key`, `known-hosts`, `allow-any-host-key`, `source`,
`target`, `mode`, `exclude`, `file-mode`, `dir-mode`, `preserve-times`)
alongside `config` fails the run: there is no mixed mode.

The only inputs that combine with `config` are the credentials (`password`,
`private-key`, `passphrase`, and the `proxy-*` credential counterparts) and
the run-wide switches (`dry-run`, `log-level`).

## The config file

A commented, ready-to-copy example lives at
[easysftp.example.yml](easysftp.example.yml). The full structure:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/eiserv/easySFTP/main/schema/easysftp.schema.json
version: 3

connection:
  host: sftp.example.com
  port: 22                     # optional
  username: deploy
  host_key: |
    SHA256:nThbg6kXUpJWGl7E1IGOCspRomTxdCARLviKw6E5SY8
  # known_hosts: |             # alternative to host_key
  #   sftp.example.com ssh-ed25519 AAAA...
  # allow_any_host_key: true   # explicit opt-out (not recommended)
  # proxy:                     # optional jump host / bastion
  #   host: bastion.example.com
  #   username: jumper
  #   host_key: |
  #     SHA256:...

defaults:
  mode: overlay                # default mode for every deployment
  exclude:
    - "*.map"

deployments:                   # at least one named deployment
  website:
    source: dist
    target: /var/www/html
    mode: sync
    exclude:
      - node_modules/
  documentation:
    source: docs/build
    target: /var/www/docs
    mode: clean

safety:
  max_deletes: 500             # 0 = unlimited

advanced:
  retries: 2
  timeout: 30
  stall_timeout: 0
  concurrency: auto            # or a number; see docs/tuning.md
  request_concurrency: auto
  connections: auto            # SSH connections uploads spread over
  skip_unchanged: false

permissions:
  files: "0644"
  directories: "0755"
  preserve_times: false

sync:
  fast_path: false
  manifest: .easysftp-manifest.json
```

### Sections

| Section | Required | Description |
|---|---|---|
| `version` | ✅ | Must be `3`. |
| `connection` | ✅ | Where and as whom to connect. Credentials are **not** here; they stay inputs. |
| `defaults` | | `mode` and `exclude` defaults applied to every deployment. |
| `deployments` | ✅ | A **map** of named deployments (at least one). The name appears in logs and the job summary. |
| `safety` | | `max_deletes` (0 = unlimited); see [delete guards](strategies.md#delete-guards). |
| `advanced` | | Transfer tuning; the defaults suit most deploys. |
| `permissions` | | Remote file/dir modes and `preserve_times` (all best-effort). |
| `sync` | | The sync mode's manifest name and fast-path. |

#### `connection`

| Field | Required | Default | Description |
|---|---|---|---|
| `host` | ✅ | - | Hostname or IP of the SFTP server. |
| `port` | | `22` | SSH port. |
| `username` | ✅ | - | Username for authentication. |
| `host_key` | ³ | - | SHA256 fingerprint(s), one per line. |
| `known_hosts` | ³ | - | `known_hosts`-format host key(s); alternative to `host_key`. |
| `allow_any_host_key` | ³ | `false` | Explicit opt-out of host key verification. |
| `proxy` | | - | Optional jump host; same fields (`host`, `port`, `username`, `host_key`, `known_hosts`, `allow_any_host_key`). |

³ As in inline mode, exactly one of `host_key`/`known_hosts`/`allow_any_host_key` is required, per hop.

#### `deployments.<name>`

| Field | Required | Default | Description |
|---|---|---|---|
| `source` | ✅ | - | Local file or directory to upload. |
| `target` | ✅ | - | Remote path. |
| `mode` | | `defaults.mode` | Per-deployment mode override. |
| `exclude` | | - | Per-deployment excludes, additive to `defaults.exclude`. |

#### `advanced`

| Field | Default | Description |
|---|---|---|
| `retries` | `2` | Retries per file on transient errors, and the reconnect budget for dropped connections. Failures the server reports with a permanent status code are never retried; see [which upload failures are retried](troubleshooting.md#which-upload-failures-easysftp-retries). `0` disables. |
| `timeout` | `30` | Connection timeout in seconds. `0` disables. |
| `stall_timeout` | `0` (off) | Abort when active remote operations make no progress for this many seconds. |
| `concurrency` | `auto` | Files uploaded in parallel, and independent remote scan / delete requests. `auto` sizes it to the work (see [transfer tuning](tuning.md)). Sync hashing uses the runner's available Go CPU parallelism independently. |
| `request_concurrency` | `auto` | Max in-flight SFTP requests per file (pipelining within one transfer). `auto` sizes it to the largest file. |
| `connections` | `auto` | SSH connections the parallel uploads spread over. Never more than `concurrency`. `auto` opens another one only while it would save more time than its handshake costs. See below. |
| `skip_unchanged` | `false` | For `overlay`, skip a file whose remote counterpart has the same size (coarse; `sync` compares content hashes). |

##### `connections`: more than one TCP flow

A single SSH connection carries the whole deploy over one TCP congestion window
and one cipher stream, no matter how high `concurrency` and
`request_concurrency` are. On a long-distance link that window is what caps
throughput, and neither knob can lift it.

`connections: 4` opens up to four connections and spreads the parallel uploads
over them. Remote scans and deletes issue up to `concurrency` independent
requests over the first connection; the sync manifest stays there too. This
keeps the server-facing parallelism under one limit without paying for extra
SSH handshakes during metadata-only work.

At `auto` (the default) easySFTP decides per deployment, because a connection
is not free:

- **Each connection repeats the handshake**, including host key verification.
  A short run of a few files pays that for nothing, so `auto` opens another
  connection only while the time it would save is larger than the handshake it
  costs. A one-second redeploy gets one connection; a two-thousand-file upload
  over a link with real latency gets several. [Transfer tuning](tuning.md)
  explains the rule and how to read the log line it prints.
- **Server limits are real.** sshd's `MaxStartups` and per-account connection
  limits on shared hosting will refuse the extra connections. easySFTP then
  warns, finishes the deploy on the ones it has and stops asking for more, so a
  too-high value costs a warning, not a failed deploy.
- **The `retries` budget is shared.** Reconnects are counted per run, not per
  connection.
- **The stall watchdog closes all of them.** `stall_timeout` only knows that
  the run stopped moving bytes, not which connection stalled.

Pin a number when your host has a connection limit you already know, or when
you measured something better than what `auto` picks. easySFTP's own numbers
live in [`benchmarks/`](../benchmarks/README.md).

#### `permissions`

| Field | Default | Description |
|---|---|---|
| `files` | mirror local | Octal permission (e.g. `"0644"`) for every uploaded file. On Windows runners there are no local bits to mirror; see the note below. |
| `directories` | server umask | Octal permission (e.g. `"0755"`) for every remote directory the run creates or touches. |
| `preserve_times` | `false` | Keep each uploaded file's local modification time on the server. |

All three are best-effort: a server that rejects the `SETSTAT` request
produces one warning per deployment (not one per file, and not a failure), so
a multi-deployment run shows which deployments are affected. Servers without
POSIX permissions behind them reject it as a matter of course; see
[what easySFTP needs from a server](providers.md#what-easysftp-needs-from-a-server).
In inline mode the same three settings are the `file-mode`, `dir-mode` and
`preserve-times` [inputs](#permissions-inline-mode).

> **Windows runners:** Windows has no POSIX permission bits, so "mirror local"
> has nothing to mirror. Go reports `0666` for every writable file and `0444`
> for read-only ones (and loses the executable bit), which is what the run
> then requests on the server. The same deploy from `windows-latest` therefore
> uploads world-writable `666` files where `ubuntu-latest` would upload `644`,
> which some web servers reject and security scans flag. Set `files: "0644"`
> (and `directories: "0755"`) explicitly when you deploy from a Windows
> runner, or the equivalent `file-mode` / `dir-mode` inputs if you deploy
> inline.

#### `sync`

| Field | Default | Description |
|---|---|---|
| `fast_path` | `false` | Reuse a file's manifest hash when size+mtime still match (rsync-style quick check). See [strategies](strategies.md#syncfast_path-skip-re-hashing-unchanged-files). |
| `manifest` | `.easysftp-manifest.json` | Manifest file name (bare, no path). An unguessable name mitigates [the manifest being publicly downloadable](security.md#the-sync-manifest-in-web-roots). |

### Validation

Unknown keys are rejected with a location and a suggestion:

```text
config ".github/easysftp.yml": unknown option "concurency" at "advanced.concurency"; did you mean "concurrency"?
```

A `version: 1` file (or a `targets` list) is rejected with a pointer to the
[migration guide](migration-v3.md). The bundled
[JSON Schema](../schema/easysftp.schema.json) gives the same validation live
in editors that honor the `# yaml-language-server` modeline.

## The `source` / `target` mapping

In inline mode, `source` is one local path and `target` its remote
destination:

- **Directories** are uploaded recursively. Remote directories are created
  automatically.
- **Single files** map onto the exact remote path, unless `target` ends with
  `/`, which means "into this directory" keeping the original file name.
- Single files only support the `overlay` mode (`sync`/`clean` reconcile a
  directory tree and are rejected for single-file targets).
- Symlinks, sockets and other non-regular files are skipped.

```yaml
    source: ./config/prod.json
    target: /etc/app/config.json     # rename on the fly
```

For more than one mapping, use a config file with multiple named
[deployments](#deploymentsname).

## Exclude patterns

`exclude` (inline, one pattern per line) and `defaults.exclude` /
per-deployment `exclude` (config file) use
[gitignore syntax](https://git-scm.com/docs/gitignore):

```yaml
exclude: |
  *.map
  *.log
  node_modules/
  !important.log
```

- Patterns are matched against the path **relative to the local root** of each
  deployment.
- `!pattern` re-includes files excluded by an earlier pattern.
- In the config file, per-deployment `exclude` lists add to `defaults.exclude`.
- An ignored directory (e.g. `node_modules/`) is skipped without being walked
  at all, so huge excluded trees cost nothing during planning. This pruning is
  automatically disabled as soon as any pattern is a `!` re-include, because a
  re-include may point below an ignored directory; results are identical either
  way, only planning speed differs.

## Outputs

| Output | Description |
|---|---|
| `files-uploaded` | Number of uploaded files (planned files in dry-run mode). |
| `files-deleted` | Number of remote files removed by the `clean`/`sync` mode. |
| `files-skipped` | Number of unchanged files skipped (by `sync`, or by `overlay` with `advanced.skip_unchanged`). |
| `bytes-uploaded` | Total bytes transferred (planned bytes in dry-run mode). |
| `duration-ms` | Total runtime in milliseconds. |

A summary is also written to the job summary of every run: status, host-key
verification status, the configuration source (inline or the config path), the
run totals, and, when there is more than one deployment (or one named
deployment), a per-deployment breakdown with each deployment's name, source,
target, mode and its own counts. Outputs and the summary are populated even if
a transfer fails partway; the values reflect progress before the failure and
the summary is marked as failed. Use `if: ${{ always() }}` when a later
reporting or rollback step must consume these partial results. See
[examples](examples.md#using-the-outputs).

The `files-*`/`bytes-uploaded` outputs stay run-wide totals; there is no
per-deployment output. Sizes are rendered for humans in the summary and the
log (`70.0 MiB (73,400,320 bytes)`, and just `70.0 MiB` in the per-deployment
table); the `bytes-uploaded` output stays a plain integer for machines.

## Complete input reference

Every action input, for reference. In v3 the action surface is deliberately
small; the advanced knobs moved into the config file (see
[the config file](#the-config-file)).

| Input | Mode | Description |
|---|---|---|
| `host`, `port`, `username` | inline | Connection target. |
| `password`, `private-key`, `passphrase` | both | Credentials (always from secrets). |
| `host-key`, `known-hosts`, `allow-any-host-key` | inline | Host key verification. |
| `source`, `target`, `mode`, `exclude` | inline | The single deployment. |
| `file-mode`, `dir-mode`, `preserve-times` | inline | Remote permissions and timestamps (`permissions.*` in config mode). |
| `config` | config | Path to the version-3 config file. |
| `proxy-password`, `proxy-private-key`, `proxy-passphrase` | config | Jump-host credentials (the rest of the proxy config is in the file). |
| `dry-run`, `log-level` | both | Run-wide switches. |

Removed v2 inputs (`server`, `uploads`, `strategy`, `ignore`, `ignore-from`,
`config-file`, `host-key-fingerprint`, `max-deletes`, `concurrency`,
`sftp-request-concurrency`, `retries`, `timeout`, `stall-timeout`,
`sync-fast-path`, `skip-unchanged`, `manifest-name`,
the `proxy-server`/`proxy-port`/`proxy-username`/
`proxy-host-key-fingerprint`/`proxy-known-hosts` connection inputs,
`build-mode`, and the `delete` tombstone) still fail loudly with a migration
hint rather than being silently ignored. See the
[migration guide](migration-v3.md) for the full mapping.
