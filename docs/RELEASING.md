# Releasing easySFTP

Releases use [Release Please](https://github.com/googleapis/release-please) and
[Conventional Commits](https://www.conventionalcommits.org/). Do not create
version tags or releases by hand.

## Release flow

1. Land Conventional Commits on `main`. PRs are squash-merged, so the PR title
   becomes the commit subject and is checked by the `PR Title` workflow.

   | Prefix | Version bump | Example |
   |---|---|---|
   | `fix:` | patch | `fix: retry on transient EOF` |
   | `feat:` | minor | `feat: add proxy-jump support` |
   | `feat!:` / `BREAKING CHANGE:` | major | `feat!: drop password auth` |
   | `docs:` `ci:` `chore:` `refactor:` `test:` `build:` `perf:` | none* | `perf: reduce startup time` |

   \* `perf:` appears in the changelog but does not force a bump by itself.

2. The `Release` workflow runs on pushes to `main`. Release Please maintains a
   release PR containing `CHANGELOG.md`, `.release-please-manifest.json`, and
   the exact `vMAJOR.MINOR.PATCH` value in `.easysftp-version`.

3. Merging the release PR makes Release Please create the immutable exact tag
   and GitHub Release. Only when that release was created in the current
   `push` run does the binary pipeline start.

4. The binary pipeline validates that the exact tag exists, is reachable from
   `main`, matches `.easysftp-version`, and already has a GitHub Release. It
   tests the exact tagged source and cross-compiles six binaries with
   `CGO_ENABLED=0`, `-trimpath`, and `-ldflags="-s -w"`:

   - Linux amd64 / arm64
   - macOS amd64 / arm64
   - Windows amd64 / arm64

5. Each produced artifact is run on its native hosted runner with the
   network-free `easysftp --help` smoke test. Build and test jobs have only
   `contents: read`.

6. After every smoke test passes, the upload job generates `checksums.txt` and
   uploads (or deliberately replaces) the complete asset set on the exact
   release. Only this job has release-upload write access.

7. Only after upload succeeds are rolling `vX` and `vX.Y` tags force-moved to
   the exact release commit. A failed build, test, or upload leaves rolling
   tags unchanged. The exact `vX.Y.Z` tag is never moved.

8. After a **major** release, update the pinned tag in every doc snippet: the
   `uses: eiserv/easySFTP@vX` examples in `README.md`, `docs/examples.md`,
   `docs/configuration.md`, and `docs/security.md`, plus the README
   "Versioning" section. They do not update themselves (see issue #63).

## Stable asset names

```text
easysftp_linux_x64
easysftp_linux_arm64
easysftp_macos_x64
easysftp_macos_arm64
easysftp_windows_x64.exe
easysftp_windows_arm64.exe
checksums.txt
```

## Repairing a failed binary publication

Use `Actions → Repair release binaries → Run workflow` and enter an existing
exact tag such as `v1.2.3`. The workflow never creates or chooses a version. It
fails unless the tag is an exact SemVer release reachable from `main`, the
GitHub Release already exists, and `.easysftp-version` matches it.

The repair rebuilds and tests the binaries from the exact tagged commit,
replaces the known assets with `gh release upload --clobber`, regenerates
checksums, and moves rolling tags only after success. Do not use PR artifacts
or locally built files to repair a release.

## Tag policy

| Tag | Mutable? | Purpose |
|---|---|---|
| `v1.2.3` | **No** | Exact source/release identity, created once by Release Please. |
| `v1.2` | Yes | Latest fully published patch within 1.2. |
| `v1` | Yes | Latest fully published release within major version 1. |

## Required repository settings

### GitHub Actions

In `Settings → Actions → General → Workflow permissions`, enable **Allow GitHub
Actions to create and approve pull requests**. Global read/write permission is
not needed because workflows declare job-level scopes.

The repository must allow the official GitHub-hosted arm64 labels used by the
smoke matrix (`ubuntu-24.04-arm`, `macos-15`, and `windows-11-arm`) as well as
their x64 counterparts. If GitHub changes label availability for the repository
plan, update only the runner labels; do not replace native smoke tests with
cross-platform emulation.

### Branch protection for `main`

Require pull requests, up-to-date branches, and these CI checks:

- `Action launcher and metadata (ubuntu-latest)`
- `Action launcher and metadata (windows-latest)`
- `Unit tests (ubuntu-latest)`
- `Unit tests (windows-latest)`
- `Self-test against a real SFTP server`
- `Validate Conventional Commit title`

Block force pushes to `main`. A linear squash-merge history is recommended.

### Tag and release settings

Create a tag ruleset matching `v*.*.*` that restricts updates and deletions.
Leave `vX` and `vX.Y` updateable by `github-actions[bot]` so the post-upload job
can move them.

Do **not** enable GitHub's immutable-releases setting with this flow: Release
Please creates the Release before the trusted binary jobs attach assets, and
the controlled repair path must be able to replace those assets. Exact tag
immutability is enforced by the tag ruleset instead.

## Dependencies

Dependabot opens grouped updates for Go modules and pinned GitHub Actions. All
external actions in CI and release workflows must remain pinned to full commit
SHAs. Review and merge those PRs only after CI passes.
