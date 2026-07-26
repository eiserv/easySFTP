<!--
The PR title must be a Conventional Commit: PRs are squash-merged, so the
title becomes the commit message and decides the release bump.
  fix: …   patch      feat: …  minor      feat!: …  major
  docs: ci: chore: refactor: test: build: perf:     no release
CI enforces this. See CONTRIBUTING.md.
-->

## What & why

<!-- What changes, and which problem it solves. Link the issue: Closes #123 -->

## Checklist

- [ ] `go test ./...`, `go vet ./...` and `gofmt -l .` are clean
- [ ] Behavior changes have a test (bug fixes: one that fails without the fix)
- [ ] Docs updated where affected: `README.md`, `docs/`, `action.yml` input
      descriptions, `schema/easysftp.schema.json`
- [ ] New external dependency in CI is pinned by full commit SHA / `@sha256:` digest
- [ ] `CHANGELOG.md` **not** hand-edited (release-please generates it)

## Notes for the reviewer

<!-- Anything that is not obvious from the diff: tradeoffs you weighed,
     things you deliberately left out, manual testing you did. -->
