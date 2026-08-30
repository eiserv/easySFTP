# Security guide

How to run easySFTP safely. See also the project's
[security policy](../SECURITY.md) for reporting vulnerabilities.

## Pin the host key (required)

In v3, host key verification is **required**: a run without `host-key` or
`known-hosts` fails, instead of silently trusting whatever key the server
presents (v2 only warned). To connect anyway without verification, you must
opt out explicitly with `allow-any-host-key: true` (in the config file:
`connection.allow_any_host_key: true`), which still logs a warning on every
run and leaves you open to man-in-the-middle attacks. Pinning is the safe
default; do it once, in whichever format you already have:

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

**Option B: `host-key`** takes SHA256 fingerprints, one per line:

```console
$ ssh-keyscan sftp.example.com | ssh-keygen -lf -
256  SHA256:nThbg6kXUpJWGl7E1IGOCspRomTxdCARLviKw6E5SY8 sftp.example.com (ED25519)
256  SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM sftp.example.com (ECDSA)
3072 SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s sftp.example.com (RSA)
```

```yaml
host-key: ${{ secrets.SFTP_HOST_KEY }}
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
- The action's first step registers the passwords and passphrases it was
  given with the runner's log masker (`::add-mask::`), before anything else
  in the action runs, so any later line that happened to contain one would
  print as `***`. The runner already does this for values read from
  `secrets.*`; this covers the cases it does not, such as a credential
  produced by an earlier step's output, a matrix entry, a plain environment
  variable or a vault action. It is defence in depth, not a licence to
  inline a credential: a masked value is still sitting in the workflow file
  and in the job's environment.
- Private keys are deliberately not masked. `::add-mask::` works line by line,
  so masking a PEM block would mean masking its short base64 lines, which
  garbles unrelated log output. Key material is never printed, and the
  passphrase protecting it is masked.
- **Prefer key-based authentication** over passwords. Generate a dedicated
  deploy key and restrict what its account can do on the server:

  ```console
  $ ssh-keygen -t ed25519 -f deploy_key -N "" -C "gh-actions deploy"
  ```

  Put the private key into a secret, the public key into the server's
  `authorized_keys`.

## The sync manifest in web roots

The `sync` strategy keeps its manifest (default `.easysftp-manifest.json`)
inside each deploy target. The file is both **read by anyone who can reach the
target over HTTP** and **parsed by the next run**, so it matters twice.

**Read.** When the target is a public web root, the manifest is served like any
other file and discloses the deployment's complete relative file list plus a
SHA-256 hash of each file's content. That is information disclosure, not
compromise, but it maps out paths that are not linked anywhere (admin bundles,
backups, generated files) and lets anyone confirm exact file contents by hash.
Being a dotfile is not protection: Apache's default `.ht*` rules do not cover
it, and nginx setups vary.

**Parsed.** The next `sync` run reads the manifest and deletes every entry that
is no longer present locally, so its keys are delete targets. Anything else
able to write that one file (a CMS upload directory, a second workflow, a
compromised script, a shared-hosting neighbour) would otherwise be choosing
where the deploy account deletes. easySFTP confines them: a key that is
absolute or climbs out of the deployment is ignored with a warning and nothing
is removed for it, and the same confinement applies to the entry names a server
returns from a directory listing during a `clean` scan or the stale-temp sweep.
The manifest is also read under a size cap and an over-size one is treated as a
first sync (upload everything, delete nothing). Write access to the manifest
still lets someone suppress deletions or force re-uploads inside the
deployment, so the guidance below is worth following either way.

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

If you use a custom `sync.manifest` name, adjust the path/name accordingly.

**Give it an unguessable name** with the `sync.manifest` config field, e.g. a
random suffix:

```yaml
# .github/easysftp.yml
version: 3
connection:
  host: sftp.example.com
  username: deploy
  host_key: |
    SHA256:...
deployments:
  website:
    source: ./dist/
    target: /var/www/html/
    mode: sync
sync:
  manifest: .manifest-c4f81b52.json
```

This mitigates casual discovery, but the file is still served if its name
leaks (or if the server lists directory indexes; disable autoindexing).
Changing the name mid-life starts a fresh manifest: the next sync re-uploads
everything, tracks deletions from scratch, and leaves the old manifest file
behind; delete the old file manually.

## Temporary upload files in web roots

Uploads stream into a temporary sibling file (`<name>.easysftp-tmp.<n>`) that
is renamed over the target on success. On a server without
`posix-rename@openssh.com` the live file is briefly parked under
`<name>.easysftp-tmp.bak` while that rename happens, so it can be put back if
the rename fails. A run killed hard (cancelled workflow, job timeout,
reclaimed runner) can leave either kind of file behind, and until the
next deploy sweeps them (see
[troubleshooting.md](troubleshooting.md#replacing-path--or-leftover-easysftp-tmp-files))
a partially written copy of the file sits in the target, served over HTTP
when the target is a public web root, possibly as plain text depending on
MIME handling. Deny the pattern in the web server, next to the manifest rule
above. nginx:

```nginx
location ~ \.easysftp-tmp(\.\d+|\.bak)?$ { deny all; }
```

Apache (vhost or `.htaccess`):

```apache
<FilesMatch "\.easysftp-tmp(\.[0-9]+|\.bak)?$">
    Require all denied
</FilesMatch>
```

## Least privilege on the server

- Use a dedicated deploy user that can only write to the deployment target,
  not a personal or root account.
- Consider a chrooted SFTP-only account (`ForceCommand internal-sftp` in
  `sshd_config`) so the deploy credentials cannot open a shell.

## Supply-chain safety

- Release refs download a verified prebuilt binary automatically. The launcher
  validates `.easysftp-version`, maps only the supported OS/architecture pairs,
  downloads only the matching binary and `checksums.txt` from
  `eiserv/easySFTP`'s exact GitHub Release, and checks SHA-256 before
  execution. Release downloads may follow GitHub's HTTPS redirect to its own
  release-asset CDN; no third-party download source is configured.
- **The SHA-256 check is not what makes the binary trustworthy.**
  `checksums.txt` is downloaded from the same GitHub Release as the binary, and
  release assets are mutable: the release workflow uploads with `--clobber`,
  and a repair workflow exists to re-run that upload for a tag that is already
  published. Anyone able to write release assets could replace both files
  together. On its own the checksum proves the download was not corrupted in
  transit, and nothing more.
- **Build provenance is what makes it trustworthy.** Every release binary
  carries a [build provenance
  attestation](https://docs.github.com/actions/security-guides/using-artifact-attestations-to-establish-provenance-for-builds)
  signed with the release workflow's OIDC identity and stored by GitHub, not as
  a release asset. It binds the binary's exact digest to this repository, to
  `.github/workflows/release-binaries.yml` and to the release commit. The
  launcher verifies it with `gh attestation verify` before running the binary,
  pinning both `--repo` and `--signer-workflow`, and **fails the run** if the
  check runs and does not pass. When the action ref is a full commit SHA, the
  launcher also pins `--source-digest` to that exact SHA. This prevents a
  mutable release asset from being replaced with a validly attested binary
  built by the same workflow from a different commit. You can run the same
  check yourself:

  ```bash
  gh attestation verify easysftp_linux_x64 --repo eiserv/easySFTP \
    --signer-workflow eiserv/easySFTP/.github/workflows/release-binaries.yml \
    --source-digest '<full release commit SHA>'
  ```

  If the check cannot run at all (no `gh` on the runner, no token, or a `gh`
  older than the `attestation` command) the run warns and continues on the
  checksum alone, so a runner without the tooling is not broken by it. The
  warning names the escape hatch below.
- Pin the action to a major tag for convenience (`eiserv/easySFTP@v3`) or to
  the full commit SHA of an exact release; both use the verified prebuilt
  binary. Any development ref (`@main`, a non-release commit SHA, or local
  `uses: ./`) builds the checked-out source from scratch instead, so a stale
  release binary can never be substituted, and the executed artifact is
  covered entirely by the ref you pinned. That source build is the escape
  hatch for a policy that will not accept a run-time download at all; it costs
  a Go build, roughly 30 to 60 seconds. The build mode is selected
  automatically from the ref; there is no `build-mode` input to get wrong.
- Exact version **tags** (`v3.0.0`) are immutable once published; `v3` and
  `v3.0` are rolling tags, see [RELEASING.md](RELEASING.md#tag-policy). The
  release **assets** behind an exact tag are a separate thing and are mutable,
  which is why the attestation above exists.
- Grant the deploy job only the permissions it needs
  (`permissions: contents: read` is enough for easySFTP itself).
