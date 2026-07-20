# Security guide

How to run easySFTP safely. See also the project's
[security policy](../SECURITY.md) for reporting vulnerabilities.

## Pin the host key (strongly recommended)

Without `host-key-fingerprint` or `known-hosts`, easySFTP prints a warning
and accepts **any** host key. Convenient for a first test, but vulnerable to
man-in-the-middle attacks. Pin your server's keys once, in whichever format
you already have:

**Option A: `known-hosts`** takes raw OpenSSH `known_hosts` lines, exactly
what `ssh-keyscan` prints (or the server's lines from your own
`~/.ssh/known_hosts`), no conversion step:

```console
$ ssh-keyscan sftp.example.com
sftp.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...
sftp.example.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNT...
sftp.example.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQ...
```

```yaml
known-hosts: ${{ secrets.SFTP_KNOWN_HOSTS }}
```

Hashed entries (`|1|...`) and `[host]:port` entries for non-standard ports
(what `ssh-keyscan -p 2222` prints) work too.

**Option B: `host-key-fingerprint`** takes SHA256 fingerprints, one per line:

```console
$ ssh-keyscan sftp.example.com | ssh-keygen -lf -
256  SHA256:nThbg6kXUpJWGl7E1IGOCspRomTxdCARLviKw6E5SY8 sftp.example.com (ED25519)
256  SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM sftp.example.com (ECDSA)
3072 SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s sftp.example.com (RSA)
```

```yaml
host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINTS }}
```

Either way, the connection is accepted if the server presents a key matching
**any** pinned entry (across both inputs, if you set both), so you can simply
pin all of your server's keys. If the server's keys ever change unexpectedly,
the deploy fails instead of talking to an impostor. When you migrate servers,
update the secret with the new keys.

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

## The sync manifest in web roots

The `sync` strategy keeps its manifest (default `.easysftp-manifest.json`)
inside each deploy target. When the target is a public web root, the manifest
is served like any other file and discloses the deployment's complete relative
file list plus a SHA-256 hash of each file's content. That is information
disclosure, not compromise, but it maps out paths that are not linked anywhere
(admin bundles, backups, generated files) and lets anyone confirm exact file
contents by hash. Being a dotfile is not protection: Apache's default `.ht*`
rules do not cover it, and nginx setups vary.

Pick one (or both):

**Deny it in the web server** (recommended; also covers a manifest left behind
by earlier deploys). nginx:

```nginx
location = /.easysftp-manifest.json { deny all; }
```

Apache (vhost or `.htaccess`):

```apache
<Files ".easysftp-manifest.json">
    Require all denied
</Files>
```

If you use a custom `manifest-name`, adjust the path/name accordingly.

**Give it an unguessable name** with the `manifest-name` input, e.g. a random
suffix stored as a repository secret:

```yaml
- uses: eiserv/easySFTP@v2
  with:
    # ...
    strategy: sync
    manifest-name: ${{ secrets.EASYSFTP_MANIFEST_NAME }}  # e.g. .manifest-c4f81b52.json
```

This mitigates casual discovery, but the file is still served if its name
leaks (or if the server lists directory indexes; disable autoindexing).
Changing the name mid-life starts a fresh manifest: the next sync re-uploads
everything, tracks deletions from scratch, and leaves the old manifest file
behind; delete the old file manually.

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
