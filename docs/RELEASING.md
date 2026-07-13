# Releasing easySFTP

Releases are fully automated with [Release Please](https://github.com/googleapis/release-please)
driven by [Conventional Commits](https://www.conventionalcommits.org/). You never
tag or draft a release by hand.

## How a release happens

1. **Land Conventional Commits on `main`.** PRs are squash-merged, so the **PR
   title** becomes the commit subject and must be a Conventional Commit. The
   [`PR Title`](../.github/workflows/pr-title.yml) workflow enforces this.

   | Prefix              | Version bump | Example                              |
   | ------------------- | ------------ | ------------------------------------ |
   | `fix:`              | patch        | `fix: retry on transient EOF`        |
   | `feat:`             | minor        | `feat: add proxy-jump support`       |
   | `feat!:` / `BREAKING CHANGE:` | major | `feat!: drop password auth`  |
   | `docs:` `ci:` `chore:` `refactor:` `test:` `build:` `perf:` | none* | `docs: fix typo` |

   \* `perf:` shows in the changelog but does not force a bump on its own.

2. **Release Please opens/updates a release PR.** The
   [`Release`](../.github/workflows/release-please.yml) workflow runs on every
   push to `main`. It maintains a single "chore(main): release x.y.z" PR that
   accumulates the changelog and the next version in
   [`.release-please-manifest.json`](../.release-please-manifest.json).

3. **Merge the release PR.** On merge, Release Please:
   - updates [`CHANGELOG.md`](../CHANGELOG.md),
   - creates the **immutable** exact tag `vX.Y.Z`,
   - publishes the GitHub Release.

4. **Moving tags follow automatically.** The `update-major-tags` job force-moves
   the rolling `vX` and `vX.Y` tags onto the new release commit, so
   `uses: eiserv/easySFTP@v1` always points at the latest 1.x.

## Tag policy

| Tag       | Mutable? | Purpose                                             |
| --------- | -------- | --------------------------------------------------- |
| `v1.2.3`  | **No**   | Exact, reproducible pin. Created once, never moved. |
| `v1.2`    | Yes      | Latest patch within 1.2.                            |
| `v1`      | Yes      | Latest release within major version 1.              |

Consumers who want a byte-exact pin can also reference the commit SHA directly.

## Required repository settings

The automation assumes these are configured once in the GitHub UI. They are not
in version control, so they are documented here.

### 1. Allow GitHub Actions to create pull requests

`Settings → Actions → General → Workflow permissions`:

- Read and write permissions is **not** required globally (the workflows request
  their own scopes), **but**
- ✅ **Allow GitHub Actions to create and approve pull requests** must be on, or
  Release Please cannot open its release PR.

### 2. Branch protection for `main`

`Settings → Branches → Add branch ruleset` (or classic branch protection),
targeting `main`:

- ✅ Require a pull request before merging.
- ✅ Require status checks to pass:
  - `Unit tests (ubuntu-latest)`
  - `Unit tests (windows-latest)`
  - `Self-test against a real SFTP server`
  - `Validate Conventional Commit title`
- ✅ Require branches to be up to date before merging.
- ✅ Block force pushes.
- ✅ (Recommended) Require a linear history — matches the squash-merge model.

> Do **not** require signed commits or a second approving review on a
> single-maintainer repo unless you are prepared to review Release Please and
> Dependabot PRs yourself; both bots push under `github-actions[bot]`.

### 3. Tag protection ruleset (keeps exact tags immutable)

`Settings → Rules → Rulesets → New tag ruleset`:

- **Target tags** by pattern (fnmatch): `v*.*.*`
  This matches exact three-part tags like `v1.2.3` **but not** `v1` or `v1.2`.
- ✅ **Restrict updates** and ✅ **Restrict deletions** — makes `vX.Y.Z`
  immutable once published.
- Leave `v1` and `v1.*` **unprotected** so the `update-major-tags` job can
  force-move them. (If you add a second ruleset for those, it must allow the
  `github-actions[bot]` actor to update them.)

If your plan supports it, enabling **immutable releases**
(`Settings → General → ... → Immutable releases`) is a stronger guarantee for the
exact tags than the ruleset alone.

## Dependencies

[Dependabot](../.github/dependabot.yml) opens weekly grouped PRs for Go modules
(`build(deps): ...`) and pinned GitHub Actions (`ci(deps): ...`). Both prefixes
are Conventional Commits, so they pass the PR-title check and are hidden from the
changelog. Review, let CI pass, and merge — no release is cut for them alone.
