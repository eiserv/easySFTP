# Security guide

How to run easySFTP safely. See also the project's
[security policy](../SECURITY.md) for reporting vulnerabilities.

## Pin the host key (strongly recommended)

Without `host-key-fingerprint`, easySFTP prints a warning and accepts **any**
host key. Convenient for a first test, but vulnerable to man-in-the-middle
attacks. Pin your server's keys once:

```console
$ ssh-keyscan sftp.example.com | ssh-keygen -lf -
256  SHA256:nThbg6kXUpJWGl7E1IGOCspRomTxdCARLviKw6E5SY8 sftp.example.com (ED25519)
256  SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM sftp.example.com (ECDSA)
3072 SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s sftp.example.com (RSA)
```

Store the `SHA256:...` values as a secret (one per line) and pass them as
`host-key-fingerprint`. The connection is accepted if the server presents a
key matching **any** of them, so you can simply pin all of your server's keys:

```yaml
host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINTS }}
```

If the server's keys ever change unexpectedly, the deploy fails instead of
talking to an impostor. When you migrate servers, update the secret with the
new fingerprints.

## Credentials

- Always store `password`, `private-key` and `passphrase` as
  [encrypted secrets](https://docs.github.com/en/actions/security-guides/encrypted-secrets)
  and never hardcode them in a workflow file.
- easySFTP receives credentials via environment variables and never prints them.
- **Prefer key-based authentication** over passwords. Generate a dedicated
  deploy key and restrict what its account can do on the server:

  ```console
  $ ssh-keygen -t ed25519 -f deploy_key -N "" -C "gh-actions deploy"
  ```

  Put the private key into a secret, the public key into the server's
  `authorized_keys`.

## Least privilege on the server

- Use a dedicated deploy user that can only write to the deployment target,
  not a personal or root account.
- Consider a chrooted SFTP-only account (`ForceCommand internal-sftp` in
  `sshd_config`) so the deploy credentials cannot open a shell.

## Supply-chain safety

- Release refs use `build-mode: prebuilt` by default. The launcher validates
  `.easysftp-version`, maps only the supported OS/architecture pairs, downloads
  only the matching binary and `checksums.txt` from
  `eiserv/easySFTP`'s exact GitHub Release, and verifies SHA-256 before
  execution. Release downloads may follow GitHub's HTTPS redirect to its own
  release-asset CDN; no third-party download source is configured.
- Pin the action to a major tag for convenience (`eiserv/easySFTP@v2`) or to
  the full commit SHA of an exact release. Prebuilt mode verifies that a SHA is
  the commit behind the version file's release tag. For any development SHA,
  explicitly build that checkout from source so a stale release binary can
  never be substituted:

  ```yaml
  - uses: eiserv/easySFTP@<commit-sha>
    with:
      build-mode: source
  ```

- Exact version tags (`v1.2.3`) are immutable once published; `v1` and `v1.2`
  are rolling tags, see [RELEASING.md](RELEASING.md#tag-policy).
- `build-mode: source` installs Go and compiles the selected action checkout.
  Use it for `@main`, local actions, non-release commit SHAs, or source-level
  debugging.
- Grant the deploy job only the permissions it needs
  (`permissions: contents: read` is enough for easySFTP itself).
