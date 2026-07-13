# Security Policy

## Reporting a vulnerability

Please do **not** open a public issue for security vulnerabilities.
Instead, use [GitHub's private vulnerability reporting](https://github.com/eiserv/easySFTP/security/advisories/new)
or contact the maintainer directly. You will receive a response as soon as possible.

## Hardening recommendations for users

- Always set the `host-key-fingerprint` input so the server's identity is verified
- Store all credentials (`password`, `private-key`, `passphrase`) as encrypted GitHub Actions secrets
- Prefer key-based authentication over passwords
- Pin this action to a full commit SHA in your workflows if you require maximum supply-chain safety
