# Migrating from v2 to v3

v3 simplifies easySFTP's configuration model around one rule:

> Inline inputs for an ordinary single-target deployment. One config file for
> everything complex. GitHub Secrets only for secret values. No mixed mode.

Every non-secret setting now has exactly one home. There is no precedence
between workflow inputs and the config file, because the two can no longer
overlap. Old v2 inputs fail with a migration hint instead of being silently
ignored.

- [The two v3 modes](#the-two-v3-modes)
- [Breaking changes at a glance](#breaking-changes-at-a-glance)
- [Renamed and removed inputs](#renamed-and-removed-inputs)
- [Migrating a simple v2 workflow](#migrating-a-simple-v2-workflow)
- [Migrating multiple upload mappings](#migrating-multiple-upload-mappings)
- [Migrating a v1 config file](#migrating-a-v1-config-file)
- [Migrating advanced settings](#migrating-advanced-settings)
- [Migrating a jump host (bastion) setup](#migrating-a-jump-host-bastion-setup)
- [Host key verification is now required](#host-key-verification-is-now-required)
- [Logging changes](#logging-changes)

## The two v3 modes

**Inline mode**: one deployment, configured entirely in `with:`.

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

**Config mode**: `config` points at a YAML file (version 3) that holds every
non-secret setting, including the connection. Only credentials, `dry-run` and
`log-level` may be combined with it.

```yaml
- uses: eiserv/easySFTP@v3
  with:
    config: .github/easysftp.yml
    private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
```

## Breaking changes at a glance

- `server` was renamed to `host`, `uploads` was replaced by `source` +
  `target`, `strategy` is now `mode`, `ignore` is now `exclude`,
  `host-key-fingerprint` is now `host-key`, `config-file` is now `config`.
- Multiple upload mappings require a config file; inline mode supports
  exactly one deployment.
- All advanced/tuning inputs (`concurrency`, `retries`, `timeout`,
  `stall-timeout`, `sftp-request-concurrency`, `sync-fast-path`,
  `skip-unchanged`, `manifest-name`, `dir-mode`, `file-mode`,
  `preserve-times`, `max-deletes`) moved into the config file.
- All proxy/bastion connection inputs moved into the config file
  (`connection.proxy`); only the proxy credentials remain inputs.
- The config file format changed: `version: 3`, connection settings live in
  the file, and targets became **named deployments**.
- **Host key verification is required.** A run without `host-key` /
  `known-hosts` fails unless `allow-any-host-key: true` is set explicitly
  (v2 printed a warning and accepted any key).
- `build-mode` was removed; the build mode is selected automatically from
  the action ref (release tags download the verified prebuilt binary,
  development refs build from source).
- The default log no longer prints one line per file; per-file output moved
  to `log-level: verbose` (see [Logging changes](#logging-changes)).
- The `delete` tombstone input from v1 was finally removed.

What did **not** change: the three reconciliation behaviors (`overlay`,
`sync`, `clean`, now under the name "mode"), the manifest format (a v2 sync
manifest is picked up seamlessly), delete guards, atomic uploads, retries and
reconnects, outputs, and the job summary outputs' names.

## Renamed and removed inputs

| v2 input | v3 replacement |
|---|---|
| `server` | `host` input, or `connection.host` in the config file |
| `port` | `port` input, or `connection.port` |
| `username` | `username` input, or `connection.username` |
| `password`, `private-key`, `passphrase` | unchanged (credentials stay inputs) |
| `host-key-fingerprint` | `host-key` input, or `connection.host_key` |
| `known-hosts` | `known-hosts` input, or `connection.known_hosts` |
| `uploads` | `source` + `target` inputs, or `deployments` in the config file |
| `strategy` | `mode` input, or `defaults.mode` / per-deployment `mode` |
| `ignore` | `exclude` input, or `defaults.exclude` / per-deployment `exclude` |
| `ignore-from` | removed; put the patterns in `exclude` or the config file |
| `config-file` | `config` (new file format, version 3) |
| `max-deletes` | `safety.max_deletes` in the config file |
| `concurrency` | `advanced.concurrency` in the config file |
| `sftp-request-concurrency` | `advanced.request_concurrency` in the config file |
| `retries` | `advanced.retries` in the config file |
| `timeout` | `advanced.timeout` in the config file |
| `stall-timeout` | `advanced.stall_timeout` in the config file |
| `skip-unchanged` | `advanced.skip_unchanged` in the config file |
| `sync-fast-path` | `sync.fast_path` in the config file |
| `manifest-name` | `sync.manifest` in the config file |
| `dir-mode` | `permissions.directories` in the config file |
| `file-mode` | `permissions.files` in the config file |
| `preserve-times` | `permissions.preserve_times` in the config file |
| `proxy-server` | `connection.proxy.host` in the config file |
| `proxy-port` | `connection.proxy.port` in the config file |
| `proxy-username` | `connection.proxy.username` in the config file |
| `proxy-host-key-fingerprint` | `connection.proxy.host_key` in the config file |
| `proxy-known-hosts` | `connection.proxy.known_hosts` in the config file |
| `proxy-password`, `proxy-private-key`, `proxy-passphrase` | unchanged (credentials stay inputs) |
| `build-mode` | removed; selected automatically from the action ref |
| `delete` | removed in v2 already; use `mode: clean` |
| `dry-run`, `log-level` | unchanged (run-wide switches, valid in both modes) |

## Migrating a simple v2 workflow

Before (v2):

```yaml
- uses: eiserv/easySFTP@v2
  with:
    server: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    password: ${{ secrets.SFTP_PASSWORD }}
    host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINT }}
    uploads: ./dist/ => /var/www/html/
    strategy: sync
    ignore: "*.map"
```

After (v3):

```yaml
- uses: eiserv/easySFTP@v3
  with:
    host: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    password: ${{ secrets.SFTP_PASSWORD }}
    host-key: ${{ secrets.SFTP_HOST_KEY_FINGERPRINT }}
    source: ./dist/
    target: /var/www/html/
    mode: sync
    exclude: "*.map"
```

## Migrating multiple upload mappings

Multiple `uploads` lines become named deployments in a config file:

Before (v2):

```yaml
- uses: eiserv/easySFTP@v2
  with:
    server: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    password: ${{ secrets.SFTP_PASSWORD }}
    host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINT }}
    uploads: |
      ./dist/ => /var/www/html/
      ./docs/build/ => /var/www/docs/
```

After (v3):

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
  host_key: ${SFTP_HOST_KEY}   # or paste the SHA256:... fingerprints
deployments:
  website:
    source: ./dist/
    target: /var/www/html/
  documentation:
    source: ./docs/build/
    target: /var/www/docs/
```

Note that the username and host key are not secrets; committing them to the
config file is fine (and keeps the workflow step minimal). If you prefer to
keep the host key in a secret anyway, inline mode is the way to do that.

## Migrating a v1 config file

The v1 file (`version: 1`, `targets` list, connection in the workflow) is
rejected by v3 with a clear error. Convert it:

Before (v1 format):

```yaml
version: 1
strategy: overlay
ignore:
  - "*.map"
guards:
  max_deletes: 200
targets:
  - local: ./dist/
    remote: /var/www/html/
    strategy: sync
```

After (v3 format):

```yaml
version: 3
connection:
  host: sftp.example.com        # moved out of the workflow
  username: deploy
  host_key: |
    SHA256:nThbg6kXUpJWGl7E1IGOCspRomTxdCARLviKw6E5SY8
defaults:
  mode: overlay
  exclude:
    - "*.map"
safety:
  max_deletes: 200
deployments:
  website:                      # targets got names
    source: ./dist/             # local  -> source
    target: /var/www/html/      # remote -> target
    mode: sync                  # strategy -> mode
```

## Migrating advanced settings

Advanced inputs moved into structured config sections. A v2 step like:

```yaml
    retries: 4
    timeout: 60
    stall-timeout: 120
    concurrency: 8
    sftp-request-concurrency: 8
    dir-mode: "755"
    file-mode: "644"
    preserve-times: "true"
    manifest-name: .deploy-manifest.json
    sync-fast-path: "true"
    max-deletes: 500
```

becomes, in the config file:

```yaml
advanced:
  retries: 4
  timeout: 60
  stall_timeout: 120
  concurrency: 8
  request_concurrency: 8
permissions:
  directories: "0755"
  files: "0644"
  preserve_times: true
sync:
  manifest: .deploy-manifest.json
  fast_path: true
safety:
  max_deletes: 500
```

If you tuned these in a single-target v2 workflow, you now need a (small)
config file; the automatic defaults cover the common case without any of
them.

## Migrating a jump host (bastion) setup

The proxy connection moved into the config file; only the credentials stay
inputs:

```yaml
- uses: eiserv/easySFTP@v3
  with:
    config: .github/easysftp.yml
    private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
    proxy-private-key: ${{ secrets.JUMP_PRIVATE_KEY }}
```

```yaml
# .github/easysftp.yml
version: 3
connection:
  host: sftp.internal.example.com
  username: deploy
  host_key: |
    SHA256:...
  proxy:
    host: bastion.example.com
    username: jumper
    host_key: |
      SHA256:...
deployments:
  website:
    source: ./dist/
    target: /var/www/html/
```

## Host key verification is now required

In v2, a run without `host-key-fingerprint`/`known-hosts` printed a warning
and accepted **any** host key. In v3 such a run fails. Either pin the key
(recommended, takes one command):

```console
$ ssh-keyscan sftp.example.com | ssh-keygen -lf -
```

```yaml
    host-key: ${{ secrets.SFTP_HOST_KEY }}
```

or opt out explicitly, accepting that a man-in-the-middle can intercept the
deployment and the credentials:

```yaml
    allow-any-host-key: "true"
```

The opt-out still logs a warning on every run. In the config file the
equivalents are `connection.host_key`, `connection.known_hosts` and
`connection.allow_any_host_key` (and the same fields under
`connection.proxy` for the jump-host hop).

## Logging changes

The default log is compact in v3: connection status, one summary line per
deployment, warnings, errors and the final totals. What v2 logged by default
(one line per uploaded/deleted file) moved to `log-level: verbose`; v2's
`verbose` (exclude-pattern explanations) is now `log-level: debug`; v2's
`quiet` is gone because `normal` now covers it. Dry runs still always log
the full per-file plan.
