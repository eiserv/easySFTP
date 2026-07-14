# Contributing to easySFTP

Thanks for your interest in contributing! Bug reports, documentation fixes and
features are all welcome. This document tells you everything you need to get a
change merged.

Please note that this project has a [Code of Conduct](CODE_OF_CONDUCT.md);
by participating you agree to abide by it.

## Reporting bugs & requesting features

- Search the [existing issues](https://github.com/eiserv/easySFTP/issues) first.
- **Bugs:** include your workflow step (redact secrets!), the action version,
  the runner OS and the relevant log output. A `dry-run: true` log is often
  enough to reproduce planning problems.
- **Security vulnerabilities:** do **not** open a public issue — see
  [SECURITY.md](SECURITY.md).
- **Features:** describe the use case, not just the solution. Issues labeled
  [`good first issue`](https://github.com/eiserv/easySFTP/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)
  are a great place to start; comment on an issue before starting bigger work
  so nobody duplicates effort.

## Development setup

You need [Go](https://go.dev/dl/) (version from [go.mod](go.mod)) — nothing
else. No Docker required for tests.

```console
$ git clone https://github.com/eiserv/easySFTP.git
$ cd easySFTP
$ go test ./...            # unit + end-to-end tests (in-process SFTP server)
$ go vet ./...
$ gofmt -l .               # must print nothing
$ go build ./cmd/easysftp
```

### Repository layout

```
action.yml               the composite action: inputs → EASYSFTP_* env vars
cmd/easysftp/            binary entry point
internal/config/         env + YAML config parsing and validation
internal/uploader/       SFTP connection, planning, strategies, transfers
internal/gha/            GitHub Actions helpers (outputs, annotations, summary)
schema/                  JSON Schema for the YAML config file
docs/                    user documentation
```

### Running the binary locally

The binary is configured entirely through `EASYSFTP_*` environment variables —
see [action.yml](action.yml) for the mapping. Example against a local SFTP
server:

```console
$ EASYSFTP_SERVER=localhost EASYSFTP_PORT=2222 \
  EASYSFTP_USERNAME=demo EASYSFTP_PASSWORD=demopass \
  EASYSFTP_UPLOADS="./dist/ => /upload/" EASYSFTP_DRY_RUN=true \
  go run ./cmd/easysftp
```

### Tests

- Unit and end-to-end tests run against an **in-process SFTP server**
  ([internal/uploader/testserver_test.go](internal/uploader/testserver_test.go)),
  so `go test ./...` needs no network and no Docker.
- CI additionally runs the whole action against a real OpenSSH server
  ([.github/workflows/ci.yml](.github/workflows/ci.yml)) on Linux and runs the
  unit tests on Windows.
- New behavior needs a test. Bug fixes need a test that fails without the fix.

## Pull requests

1. Fork, create a branch, make your change.
2. Make sure `go test ./...`, `go vet ./...` and `gofmt -l .` are clean.
3. Open a PR against `main`.

### PR title = Conventional Commit (required)

PRs are **squash-merged**, so the PR title becomes the commit message and must
be a [Conventional Commit](https://www.conventionalcommits.org/) — CI enforces
this. The prefix decides the release bump
(see [docs/RELEASING.md](docs/RELEASING.md)):

| Prefix | Effect | Example |
|---|---|---|
| `fix:` | patch release | `fix: retry on transient EOF` |
| `feat:` | minor release | `feat: add keepalive support` |
| `feat!:` / `BREAKING CHANGE:` | major release | `feat!: fail without host key` |
| `docs:` `ci:` `chore:` `refactor:` `test:` `build:` `perf:` | no release | `docs: fix typo` |

### Review checklist (what the maintainer looks for)

- Behavior changes are covered by tests and documented (README / `docs/` /
  `action.yml` input descriptions).
- No new dependencies unless truly needed.
- Errors are wrapped with context (`fmt.Errorf("...: %w", err)`), log output
  goes through the `Logger`/`gha` helpers.
- Destructive-path changes (anything that deletes remote files) keep the
  [delete guards](docs/strategies.md#delete-guards) intact.

Releases are fully automated — you never bump versions or edit the changelog
by hand. Merged `feat:`/`fix:` PRs ship with the next release PR merge.
