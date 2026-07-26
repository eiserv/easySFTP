# Provider notes

Most SFTP deploy pain is not easySFTP, it is the server: a chrooted home that
makes your absolute path wrong, SSH that has to be switched on in a control
panel first, a non-standard port, an account that speaks SFTP but has no
shell. This page collects what is known about specific providers.

**These entries are community-maintained.** Nobody can test every host, so
each entry carries the date it was last verified and by whom. If yours is
missing or out of date, [add it](#adding-a-provider): the
[template](#entry-template) is at the bottom, and a working deploy at one
provider is all you need to contribute.

- [First: find your own settings](#first-find-your-own-settings)
- [Hetzner Storage Box](#hetzner-storage-box)
- [cPanel and similar shared webhosting](#cpanel-and-similar-shared-webhosting)
- [Adding a provider](#adding-a-provider)

## First: find your own settings

This works everywhere and answers most questions faster than looking for your
provider below.

**1. The host key.** Never take the fingerprint from the server you are about
to trust; take it from a machine you already trust, once:

```bash
ssh-keyscan -p 22 sftp.example.com | ssh-keygen -lf -
```

Every `SHA256:...` line it prints can go into `host-key` (one per line, any
match is accepted). If your provider publishes its fingerprints on a status or
docs page, compare them; that is the strongest check you can make.

**2. The remote path.** The path you need is the one *the SFTP session* sees,
which is not always the one the control panel shows. Two reliable ways to find
it:

- Connect once by hand and look: `sftp user@host` then `pwd`. That prints the
  directory a relative `target` is resolved against.
- Or skip the question entirely: **relative targets work.** `target: public_html/`
  is resolved from the login directory, which is exactly what you want on a
  chrooted account, and it keeps working if the provider moves your home.

**3. Prove it before it writes anything.** Run the real workflow once with
`dry-run: true`. It connects, verifies the host key, resolves the paths and
prints the full plan without touching the server. Every path and permission
problem below shows up there for free.

```yaml
- uses: eiserv/easySFTP@v3
  with:
    host: sftp.example.com
    username: deploy
    password: ${{ secrets.SFTP_PASSWORD }}
    host-key: ${{ secrets.SFTP_HOST_KEY }}
    source: ./dist/
    target: public_html/
    dry-run: "true"
```

One thing you never have to worry about: **easySFTP never opens a shell.** It
speaks the SFTP subsystem only, so an account with SFTP but no interactive
shell (very common on managed hosting, and the default on some storage
products) is fully supported. Tools that fail on such accounts usually do so
because they shell out to `mkdir`/`chmod`; easySFTP uses the SFTP protocol
operations instead.

## Hetzner Storage Box

*Last verified: 2026-07-26, against the [Hetzner docs][hetzner-ssh], not a live account.*

- **Port 22 is SFTP/SCP only, port 23 is the full SSH service.** easySFTP needs
  only the SFTP subsystem, so **use port 22**. (Port 23 also works.)
- **Username and host share the ID**: user `uXXXXXX`, host
  `uXXXXXX.your-storagebox.de`. Sub-accounts use their own `uXXXXXX-subN`
  username.
- **Only `/home` is writable**, and Hetzner recommends addressing files
  relatively. Use `target: deploy/site/` rather than an absolute path.
- **Password authentication cannot be disabled**, so treat the password as a
  live credential even after you switch to a key. Keys must be in normal
  OpenSSH format (RSA, ECDSA or ED25519).
- A Storage Box is storage, not a webserver: `file-mode` / `dir-mode` matter
  much less here than on a hosting account.

```yaml
- uses: eiserv/easySFTP@v3
  with:
    host: u123456.your-storagebox.de
    port: "22"
    username: u123456
    password: ${{ secrets.STORAGEBOX_PASSWORD }}
    host-key: ${{ secrets.STORAGEBOX_HOST_KEY }}
    source: ./dist/
    target: backups/site/
    mode: sync
```

## cPanel and similar shared webhosting

*Last verified: 2026-07-26, from vendor documentation and the standard cPanel
layout, not a live account. Layouts vary between resellers, so confirm with
step 2 above.*

- **SFTP runs on the SSH port**, usually 22, but resellers very often move it
  (2222 and 22222 are common). The port is in the control panel, not guessable.
- **SSH/SFTP is frequently disabled until you ask for it.** If the handshake
  times out or authentication fails for an account that works in the web File
  Manager, that is usually why: open a support ticket rather than debugging the
  workflow.
- **The primary document root is `public_html`** inside the account's home
  (`/home/<user>/public_html`). Addon domains and subdomains get their own
  directory, typically alongside or beneath it. The File Manager shows the full
  path, and `sftp user@host` + `pwd` confirms what the session actually sees.
- **Prefer the relative `target: public_html/`.** Jailed accounts are the norm
  here, and a relative target is immune to whether the jail rewrites the
  visible path.
- **Set `file-mode: "0644"` and `dir-mode: "0755"`** if pages come back as 403
  after a successful deploy, and especially if you deploy from a Windows
  runner, which has no POSIX bits for easySFTP to mirror. See
  [Troubleshooting](troubleshooting.md).

```yaml
- uses: eiserv/easySFTP@v3
  with:
    host: sftp.example.com
    port: "22"
    username: myaccount
    private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
    host-key: ${{ secrets.SFTP_HOST_KEY }}
    source: ./dist/
    target: public_html/
    mode: sync
    file-mode: "0644"
    dir-mode: "0755"
```

## Adding a provider

Open a PR that appends a section to this page. What makes an entry useful is
the part that is *not* obvious: the port nobody expects, the path the control
panel does not show, the setting that has to be enabled first. Skip anything
that is already true everywhere.

Please only add what you have actually run. An entry that says "verified from
the vendor's documentation" is still welcome, it just has to say so, like the
two above do.

### Entry template

````markdown
## Provider name

*Last verified: YYYY-MM-DD by @your-handle, on a live account / from vendor docs.*

- **Port**: … (and where to find it in the control panel)
- **Paths**: what the SFTP session's working directory is, and where the
  document root sits relative to it
- **Auth**: key formats accepted, whether password auth can be turned off,
  anything that has to be enabled first
- **Quirks**: anything that made a deploy fail in a way the error did not explain

```yaml
- uses: eiserv/easySFTP@v3
  with:
    …a known-good configuration…
```
````

[hetzner-ssh]: https://docs.hetzner.com/storage/storage-box/access/access-ssh-rsync-borg/
