# easySFTP

[![CI](https://github.com/eiserv/easySFTP/actions/workflows/ci.yml/badge.svg)](https://github.com/eiserv/easySFTP/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Fast, secure and simple SFTP uploads for GitHub Actions.**

Deploy your build output to any SFTP server — from a three-line workflow step up to
fully configured multi-target deployments.

- ⚡ **Fast**: written in Go, files are transferred in parallel with concurrent
  SFTP write requests per file.
- 🔒 **Secure**: optional host key pinning verifies the server's identity;
  credentials are only ever read from environment variables.
- 🧩 **Simple, but configurable**: sensible defaults for the simple case,
  gitignore-style excludes, delete mode, dry runs, retries and outputs for the
  complex ones.
- 🖥️ **Cross-platform**: runs on `ubuntu-*`, `macos-*` and `windows-*` runners.

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

That's it. Everything else is optional.

## Full example

```yaml
name: Deploy

on:
  push:
    branches: [main]

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5

      # ... build your project ...

      - name: Deploy via SFTP
        id: deploy
        uses: eiserv/easySFTP@v1
        with:
          server: sftp.example.com
          port: 22
          username: ${{ secrets.SFTP_USERNAME }}
          private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
          passphrase: ${{ secrets.SFTP_PASSPHRASE }}
          host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINT }}
          uploads: |
            ./dist/ => /var/www/html/
            ./docs/ => /var/www/docs/
            ./robots.txt => /var/www/html/robots.txt
          ignore: |
            *.map
            *.log
            node_modules/
          delete: true
          concurrency: 8

      - name: Report
        run: echo "Uploaded ${{ steps.deploy.outputs.files-uploaded }} files (${{ steps.deploy.outputs.bytes-uploaded }} bytes) in ${{ steps.deploy.outputs.duration-ms }} ms"
```

## Inputs

| Input | Required | Default | Description |
|---|---|---|---|
| `server` | ✅ | — | Hostname or IP of the SFTP server. |
| `port` | | `22` | SSH port. |
| `username` | ✅ | — | Username for authentication. |
| `password` | ¹ | — | Password. **Use a secret.** |
| `private-key` | ¹ | — | SSH private key (OpenSSH/PEM format). **Use a secret.** |
| `passphrase` | | — | Passphrase of the private key, if encrypted. |
| `host-key-fingerprint` | | — | Expected SHA256 host key fingerprint(s), one per line (`SHA256:...`). **Strongly recommended**, see [Security](#security). |
| `uploads` | ✅ | — | One `local => remote` mapping per line. Directories are uploaded recursively; single files are supported too. |
| `ignore` | | — | Gitignore-style exclude patterns, one per line. `!` re-includes. |
| `ignore-from` | | — | Path to a file with exclude patterns (e.g. `.sftpignore`). |
| `delete` | | `false` | Delete everything inside each remote target directory before uploading (clean deploy). |
| `dry-run` | | `false` | Connect and log what would happen, change nothing. |
| `concurrency` | | `4` | Number of files uploaded in parallel. |
| `retries` | | `2` | Retries per file on transient upload errors (exponential backoff). |
| `timeout` | | `30` | Connection timeout in seconds. |

¹ At least one of `password` / `private-key` is required. If both are set, the key is tried first.

### The `uploads` mapping

```yaml
uploads: |
  # directory => directory (recursive)
  ./dist/ => /var/www/html/

  # single file => exact remote path (rename on the fly)
  ./config/prod.json => /etc/app/config.json

  # single file => into a remote directory (note the trailing slash)
  ./robots.txt => /var/www/html/
```

Remote directories are created automatically.

## Outputs

| Output | Description |
|---|---|
| `files-uploaded` | Number of uploaded files (planned files in dry-run mode). |
| `files-deleted` | Number of remote files removed by delete mode. |
| `bytes-uploaded` | Total bytes transferred. |
| `duration-ms` | Total runtime in milliseconds. |

A summary table is also written to the job summary of every run.

## Security

### Pin the host key (recommended)

Without `host-key-fingerprint`, easySFTP prints a warning and accepts any host
key — convenient, but vulnerable to man-in-the-middle attacks. Pin your
server's keys once:

```console
$ ssh-keyscan sftp.example.com | ssh-keygen -lf -
256  SHA256:nThbg6kXUpJWGl7E1IGOCspRomTxdCARLviKw6E5SY8 sftp.example.com (ED25519)
256  SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM sftp.example.com (ECDSA)
3072 SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s sftp.example.com (RSA)
```

Store the `SHA256:...` values as a secret (one per line) and pass them as
`host-key-fingerprint` — the connection is accepted if the server presents a
key matching any of them:

```yaml
host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINTS }}
```

If the server's keys ever change unexpectedly, the deploy fails instead of
talking to an impostor.

### Credentials

Always store `password`, `private-key` and `passphrase` as
[encrypted secrets](https://docs.github.com/en/actions/security-guides/encrypted-secrets)
— never hardcode them in the workflow. easySFTP receives them via environment
variables and never prints them.

## Why another SFTP action?

This project is inspired by [Dylan700/sftp-upload-action](https://github.com/Dylan700/sftp-upload-action),
which is no longer actively maintained. easySFTP is a clean reimplementation in Go:

- compiled static binary instead of a Node.js runtime (fast startup, parallel transfers)
- works on Linux, macOS **and** Windows runners (no Docker required)
- host key verification, retries with backoff, structured outputs and a job summary
- end-to-end test suite that runs against a real in-process SFTP server
  plus a CI self-test against a real OpenSSH server

## Development

```console
$ go test ./...   # unit + end-to-end tests (in-process SFTP server, no Docker needed)
$ go vet ./...
$ go build ./cmd/easysftp
```

The binary is configured entirely through `EASYSFTP_*` environment variables —
see [action.yml](action.yml) for the mapping. CI additionally runs the action
against a real OpenSSH SFTP server (see [.github/workflows/ci.yml](.github/workflows/ci.yml)).

## License

[MIT](LICENSE)
