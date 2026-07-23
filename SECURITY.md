# Security Policy

## Reporting a vulnerability

Please do **not** open a public issue for security vulnerabilities.
Instead, use [GitHub's private vulnerability reporting](https://github.com/eiserv/easySFTP/security/advisories/new)
or contact the maintainer directly. You will receive a response as soon as possible.

## Hardening recommendations for users

- Always set the `host-key` input (or `known-hosts`) so the server's identity is verified. In v3 a run without a pinned key fails unless you explicitly opt out with `allow-any-host-key: true`, which is not recommended
- Store all credentials (`password`, `private-key`, `passphrase`) as encrypted GitHub Actions secrets
- Prefer key-based authentication over passwords
- Release refs download only this repository's platform binary and verify it against the release's `checksums.txt`
- The build mode is selected automatically from the action ref: release tags and the exact release commit SHA use the verified prebuilt binary; every other ref builds from source, so a stale release binary is never substituted for newer source
