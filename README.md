# easySFTP

[![CI](https://github.com/eiserv/easySFTP/actions/workflows/ci.yml/badge.svg)](https://github.com/eiserv/easySFTP/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Fast, secure and simple SFTP uploads for GitHub Actions.**

Deploy your build output to any SFTP server — from a three-line workflow step
up to fully configured multi-target deployments.

- ⚡ **Fast**: written in Go, files are transferred in parallel with concurrent
  SFTP write requests per file.
- 🔒 **Secure**: optional host key pinning verifies the server's identity;
  atomic per-file uploads never leave half-written files; credentials are only
  ever read from environment variables.
- 🧩 **Simple, but configurable**: sensible defaults for the simple case;
  gitignore-style excludes, sync/clean strategies, delete guards, dry runs,
  retries and outputs for the complex ones.
- 🖥️ **Cross-platform**: runs natively on `ubuntu-*`, `macos-*` and
  `windows-*` runners — no Docker required.

## Table of contents

- [Quick start](#quick-start)
- [Inputs & outputs](#inputs--outputs)
- [Strategies](#strategies)
- [Config file for multiple targets](#config-file-for-multiple-targets)
- [Security](#security)
- [Documentation](#documentation)
- [Versioning](#versioning)
- [Why another SFTP action?](#why-another-sftp-action)
- [Contributing](#contributing)
- [License](#license)

## Quick start

```yaml
- name: Deploy via SFTP
  uses: eiserv/easySFTP@v1
  with:
    server: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    password: ${{ secrets.SFTP_PASSWORD }}
    uploads: ./dist/ => /var/www/html/
```

That's it. Everything else is optional. More recipes — key auth, multi-target
deploys, PR previews, `.sftpignore` — live in [docs/examples.md](docs/examples.md).

## Inputs & outputs

The most used inputs:

| Input | Default | Description |
|---|---|---|
| `server` / `port` / `username` | — / `22` / — | Where and as whom to connect. |
| `password` / `private-key` / `passphrase` | — | Authentication — at least one of password/key. **Use secrets.** |
| `host-key-fingerprint` | — | Pin the server's SHA256 host key(s). **Strongly recommended.** |
| `uploads` | — | One `local => remote` mapping per line; directories are recursive. |
| `strategy` | `overlay` | How the remote side is reconciled: `overlay`, `sync` or `clean`. |
| `ignore` / `ignore-from` | — | Gitignore-style excludes (inline / from a file). |
| `dry-run` | `false` | Log what would happen, change nothing. |
| `concurrency` / `retries` / `timeout` | `4` / `2` / `30` | Parallelism, per-file retries, connection timeout (s). |

Outputs: `files-uploaded`, `files-deleted`, `files-skipped`, `bytes-uploaded`,
`duration-ms` — plus a summary table in the job summary.

➡ Full reference with every input, output and rule:
[docs/configuration.md](docs/configuration.md)

## Strategies

| Strategy | Uploads | Deletes | Use for |
|---|---|---|---|
| `overlay` (default) | all files | nothing | adding/updating files, leaving everything else in place |
| `sync` | new & changed files | files a previous sync uploaded but that are now gone locally | keeping a directory an exact mirror of your build, safely |
| `clean` | all files | **everything** in the remote target first | a guaranteed-fresh deploy |

`sync` is manifest-based — it only ever deletes files it uploaded itself, skips
unchanged files by content hash, and re-deploys only transfer what changed.
Destructive strategies are protected by [delete guards](docs/strategies.md#delete-guards):
the remote root is always refused, and `max_deletes` caps how much a single run
may delete. Preview anything with `dry-run: true`.

➡ Details, manifest semantics and guard rules: [docs/strategies.md](docs/strategies.md)

## Config file for multiple targets

Multiple targets with different strategies? Point `config-file` at a YAML file
(connection settings stay in inputs/secrets):

```yaml
# .github/easysftp.yml
version: 1
guards:
  max_deletes: 200
targets:
  - local: ./dist/
    remote: /var/www/html/
    strategy: sync
  - local: ./docs/
    remote: /var/www/docs/
    strategy: clean
```

A JSON Schema for editor autocomplete/validation is bundled — see
[docs/configuration.md](docs/configuration.md#the-yaml-config-file) and the
commented [example config](docs/easysftp.example.yml).

## Security

Two rules cover most of it:

1. **Pin the host key.** Without `host-key-fingerprint`, any server is
   accepted — set it so a man-in-the-middle fails the deploy instead:

   ```console
   $ ssh-keyscan sftp.example.com | ssh-keygen -lf -
   ```

2. **Keep credentials in encrypted secrets**, prefer key-based auth, and use a
   least-privilege deploy account.

➡ Full guide (key setup, chroot, SHA pinning): [docs/security.md](docs/security.md) ·
Vulnerability reports: [SECURITY.md](SECURITY.md)

## Documentation

| | |
|---|---|
| [Configuration reference](docs/configuration.md) | All inputs, outputs, mappings, ignore rules, config file. |
| [Strategies](docs/strategies.md) | `overlay`/`sync`/`clean`, manifest, delete guards, dry runs. |
| [Examples & use cases](docs/examples.md) | Copy-paste recipes for common deployments. |
| [Security guide](docs/security.md) | Host key pinning, credentials, supply chain. |
| [Troubleshooting & FAQ](docs/troubleshooting.md) | Common errors and fixes. |

## Versioning

easySFTP follows [Semantic Versioning](https://semver.org):

```yaml
uses: eiserv/easySFTP@v1        # latest 1.x — recommended, gets fixes & features
uses: eiserv/easySFTP@v1.2.3    # exact, immutable pin
```

`v1`/`v1.2` are rolling tags; `v1.2.3` and commit SHAs never move. Releases and
the changelog are generated automatically from Conventional Commits — see
[docs/RELEASING.md](docs/RELEASING.md).

## Why another SFTP action?

This project is inspired by [Dylan700/sftp-upload-action](https://github.com/Dylan700/sftp-upload-action),
which is no longer actively maintained. easySFTP is a clean reimplementation in Go:

- compiled static binary instead of a Node.js runtime (fast startup, parallel transfers)
- works on Linux, macOS **and** Windows runners (no Docker required)
- host key verification, atomic uploads, retries with backoff, structured
  outputs and a job summary
- end-to-end test suite against an in-process SFTP server, plus a CI self-test
  against a real OpenSSH server

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](CONTRIBUTING.md) for the dev
setup, test suite and PR conventions, and the
[good first issues](https://github.com/eiserv/easySFTP/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)
for places to start. This project follows a
[Code of Conduct](CODE_OF_CONDUCT.md).

## License

[MIT](LICENSE)
