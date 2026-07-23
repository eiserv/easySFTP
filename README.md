# easySFTP

[![CI](https://github.com/eiserv/easySFTP/actions/workflows/ci.yml/badge.svg)](https://github.com/eiserv/easySFTP/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Fast, secure and boring SFTP deploys for GitHub Actions.**

easySFTP uploads your build output to any SFTP server. The common case is a
few self-explanatory lines; complex, professional setups stay fully
configurable through one optional config file.

## Quick start

Add two secrets to your repository (`SFTP_USERNAME`, `SFTP_PASSWORD`), pin the
server's host key, and deploy a directory:

```yaml
- name: Deploy via SFTP
  uses: eiserv/easySFTP@v3
  with:
    host: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    password: ${{ secrets.SFTP_PASSWORD }}
    host-key: ${{ secrets.SFTP_HOST_KEY }}
    source: dist
    target: /var/www/html
```

That's the whole thing. You only need to understand four things: where the
server is, how to authenticate, what to upload, and where to put it.

Get the host key once with `ssh-keyscan sftp.example.com | ssh-keygen -lf -`
and store the `SHA256:...` line(s) as the `SFTP_HOST_KEY` secret. (Without it,
the deploy fails rather than trusting an unverified server; you can opt out
with `allow-any-host-key: true`, but that allows man-in-the-middle attacks.)

> **Not just simple deploys.** easySFTP is boring on purpose for the common
> case, but complex and professional setups are fully supported through an
> optional [config file](#need-more-multiple-deployments-and-advanced-control):
> multiple deployment targets, per-target modes, proxy/bastion connections,
> delete guards, advanced excludes, permissions, retries and timeouts,
> performance tuning, and sync-manifest configuration. Start inline; add a
> config file when you actually need one.

## Table of contents

- [Quick start](#quick-start)
- [Why easySFTP?](#why-easysftp)
- [Inputs & outputs](#inputs--outputs)
- [Deployment modes](#deployment-modes)
- [Need more? Multiple deployments and advanced control](#need-more-multiple-deployments-and-advanced-control)
- [Security](#security)
- [Documentation](#documentation)
- [Versioning](#versioning)
- [Contributing](#contributing)
- [License](#license)

## Why easySFTP?

I needed an SFTP upload step for a GitHub Actions deploy, and none of the
existing options really fit. They were either no longer actively maintained,
not resource-friendly, hard or barely configurable, or they simply did not work
reliably. So I built my own and made it available for everyone with the same
problem: an action that stays out of your way for the simple case and still
handles the complex ones.

Here is how easySFTP compares to other open-source actions that tackle the same
job:

| Feature | easySFTP | [Dylan700/sftp&#8209;upload&#8209;action][dylan] | [SamKirkland/FTP&#8209;Deploy&#8209;Action][sam] | [wlixcc/SFTP&#8209;Deploy&#8209;Action][wlixcc] | [wangyucode/sftp&#8209;upload&#8209;action][wang] |
|---|---|---|---|---|---|
| Protocol | SFTP | SFTP | FTP / FTPS only | SFTP | SFTP |
| Implementation | Go binary | Node.js | Node.js | Docker (rsync) | Python |
| Linux / macOS / Windows | yes / yes / yes | yes / yes / yes | yes / yes / yes | Linux only (Docker) | needs Python on runner |
| Host key verification | yes (pinned fingerprints) | no | n/a | not documented | not documented |
| Atomic per-file upload | yes | no | no | rsync temp file | no |
| Skip unchanged files | yes (SHA256 manifest) | no | yes (state file) | partial (rsync) | yes (MD5 hashes) |
| Delete removed files | yes (tracked, via sync) | yes (full wipe) | yes | yes (full wipe) | yes (tracked) |
| Delete safety guards | yes (root refusal, max_deletes) | no | no | no | no |
| Multiple targets / modes | yes (config file) | multiple mappings | single directory | single directory | single directory |
| Dry run | yes | no | yes | no | no |
| Actively maintained | yes | last release 2024 | yes | yes | yes |

The matrix reflects each project's public documentation as of July 2026.
"Not documented" means the feature was not found in that action's README, not
that it is impossible. SamKirkland's FTP-Deploy-Action is by far the most
popular deploy action, but it speaks FTP/FTPS rather than SFTP, so it is listed
for context rather than as a direct SFTP alternative.

easySFTP is a clean, from-scratch implementation in Go, inspired by the
no-longer-maintained [Dylan700/sftp-upload-action][dylan]:

- compiled static binary instead of a Node.js runtime (fast startup, parallel transfers)
- works on Linux, macOS **and** Windows runners, with no Docker required
- host key verification, atomic uploads, retries with backoff, structured
  outputs and a job summary
- end-to-end test suite against an in-process SFTP server, plus a CI self-test
  against a real OpenSSH server

[dylan]: https://github.com/Dylan700/sftp-upload-action
[sam]: https://github.com/SamKirkland/FTP-Deploy-Action
[wlixcc]: https://github.com/wlixcc/SFTP-Deploy-Action
[wang]: https://github.com/wangyucode/sftp-upload-action

## Inputs & outputs

Inline mode uses a small, self-explanatory set of inputs:

| Input | Default | Description |
|---|---|---|
| `host` / `port` / `username` | - / `22` / - | Where and as whom to connect. |
| `password` / `private-key` / `passphrase` | - | Authentication, at least one of password/key. **Use secrets.** |
| `host-key` / `known-hosts` | - | Pin the server's host key. **Required** (or opt out with `allow-any-host-key`). |
| `source` / `target` | - | Local path to upload, and where it goes on the server. |
| `mode` | `overlay` | How the remote side is reconciled: `overlay`, `sync` or `clean`. |
| `exclude` | - | Gitignore-style excludes, one per line. |
| `config` | - | Path to a YAML config file (replaces all inline connection/deployment inputs). |
| `dry-run` | `false` | Log what would happen, change nothing. |
| `log-level` | `normal` | `normal`, `verbose` (per-file lines) or `debug` (exclude decisions). |

Outputs: `files-uploaded`, `files-deleted`, `files-skipped`, `bytes-uploaded`,
`duration-ms`, plus a summary table in the job summary. If a transfer fails
partway, the outputs contain the progress completed before the failure and the
summary is marked as failed.

➡ Full reference with every input, the config file, and every rule:
[docs/configuration.md](docs/configuration.md)

## Deployment modes

| Mode | Uploads | Deletes | Use for |
|---|---|---|---|
| `overlay` (default) | all files | nothing | adding/updating files, leaving everything else in place |
| `sync` | new & changed files | files a previous sync uploaded but that are now gone locally | keeping a directory an exact mirror of your build, safely |
| `clean` | all files | **everything** in the remote target first | a guaranteed-fresh deploy |

`sync` is manifest-based: it only ever deletes files it uploaded itself, skips
unchanged files by content hash, and only transfers what changed on re-deploys.
Destructive modes are protected by [delete guards](docs/strategies.md#delete-guards):
the remote root is always refused, and `max_deletes` caps how much a single run
may delete. Preview anything with `dry-run: true`.

➡ Details, manifest semantics and guard rules: [docs/strategies.md](docs/strategies.md)

## Need more? Multiple deployments and advanced control

Point `config` at a YAML file. In config mode **every** non-secret setting
lives in that one file (connection included); only credentials, `dry-run` and
`log-level` stay in the workflow. There is no mixed mode and no precedence to
reason about.

```yaml
- uses: eiserv/easySFTP@v3
  with:
    config: .github/easysftp.yml
    private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
```

```yaml
# .github/easysftp.yml
version: 3
connection:
  host: sftp.example.com
  username: deploy
  host_key: |
    SHA256:nThbg6kXUpJWGl7E1IGOCspRomTxdCARLviKw6E5SY8
defaults:
  mode: sync
  exclude:
    - "*.map"
safety:
  max_deletes: 500
deployments:
  website:
    source: dist
    target: /var/www/html
  documentation:
    source: docs/build
    target: /var/www/docs
    mode: clean
```

The config file supports named deployments, per-target modes and excludes,
proxy/bastion connections, delete guards, permissions, retries/timeouts,
performance tuning and sync-manifest settings. A JSON Schema for editor
autocomplete/validation is bundled. See
[docs/configuration.md](docs/configuration.md#the-config-file) and the
commented [example config](docs/easysftp.example.yml).

**Upgrading from v2?** The inputs were renamed and the advanced knobs moved
into the config file. See the [v3 migration guide](docs/migration-v3.md).

## Security

Two rules cover most of it:

1. **Pin the host key.** `host-key` is required in v3: without it (and without
   the explicit `allow-any-host-key` opt-out), the deploy fails instead of
   trusting an unverified server. Get the fingerprints with:

   ```console
   $ ssh-keyscan sftp.example.com | ssh-keygen -lf -
   ```

2. **Keep credentials in encrypted secrets**, prefer key-based auth, and use a
   least-privilege deploy account.

➡ Full guide (key setup, chroot, SHA pinning): [docs/security.md](docs/security.md) ·
Vulnerability reports: [SECURITY.md](SECURITY.md)

## Documentation

Structured from quick start to full reference:

| | |
|---|---|
| [Configuration reference](docs/configuration.md) | Quick start, common config, multiple/advanced deployments, the config file, full reference. |
| [Deployment modes](docs/strategies.md) | `overlay`/`sync`/`clean`, manifest, delete guards, dry runs. |
| [Examples & use cases](docs/examples.md) | Copy-paste recipes for common deployments. |
| [Security guide](docs/security.md) | Host key pinning, credentials, supply chain. |
| [Troubleshooting & FAQ](docs/troubleshooting.md) | Common errors and fixes. |
| [v3 migration guide](docs/migration-v3.md) | Moving a v2 (or v1) setup to v3. |

## Versioning

easySFTP follows [Semantic Versioning](https://semver.org):

```yaml
uses: eiserv/easySFTP@v3        # latest 3.x, recommended, gets fixes & features
uses: eiserv/easySFTP@v3.0.0    # exact, immutable pin
```

`v3`, `v3.0` and `v3.0.0` use the exact version recorded in
`.easysftp-version` and download the corresponding asset only from this
repository's GitHub Release. The asset's SHA-256 digest is checked against
`checksums.txt` before it is executed. `v3`/`v3.0` are rolling tags; exact tags
never move.

The build mode is selected automatically from the action ref: release tags
(and the exact release commit SHA) download the verified prebuilt binary;
development refs (`@main`, other commit SHAs, local `uses: ./`) build the
checked-out source with `CGO_ENABLED=0`, `-trimpath`, and stripped symbols.
Releases and the changelog remain managed by Release Please and Conventional
Commits. See [docs/RELEASING.md](docs/RELEASING.md).

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](CONTRIBUTING.md) for the dev
setup, test suite and PR conventions, and the
[good first issues](https://github.com/eiserv/easySFTP/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)
for places to start. This project follows a
[Code of Conduct](CODE_OF_CONDUCT.md).

## License

[MIT](LICENSE)
