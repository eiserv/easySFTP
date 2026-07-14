# Examples & use cases

Copy-paste recipes for common deployments. All of them assume credentials are
stored as [encrypted secrets](https://docs.github.com/en/actions/security-guides/encrypted-secrets).

- [Deploy a static site](#deploy-a-static-site)
- [Mirror a build with sync](#mirror-a-build-with-sync)
- [Multiple targets with a config file](#multiple-targets-with-a-config-file)
- [Key-based authentication](#key-based-authentication)
- [Upload a single file (with rename)](#upload-a-single-file-with-rename)
- [Preview a deploy in pull requests](#preview-a-deploy-in-pull-requests)
- [Using the outputs](#using-the-outputs)
- [Excludes via .sftpignore](#excludes-via-sftpignore)
- [Windows and macOS runners](#windows-and-macos-runners)

## Deploy a static site

The minimal setup: build, then upload the output on top of what's there:

```yaml
name: Deploy
on:
  push:
    branches: [main]

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - run: npm ci && npm run build

      - name: Deploy via SFTP
        uses: eiserv/easySFTP@v1
        with:
          server: sftp.example.com
          username: ${{ secrets.SFTP_USERNAME }}
          password: ${{ secrets.SFTP_PASSWORD }}
          host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINT }}
          uploads: ./dist/ => /var/www/html/
```

## Mirror a build with sync

`sync` keeps the remote directory an exact mirror of your build output:
unchanged files are skipped, files removed locally are deleted remotely, but
only files easySFTP itself uploaded ([manifest-based](strategies.md#sync), so
user uploads and server-generated files survive):

```yaml
- uses: eiserv/easySFTP@v1
  with:
    server: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    password: ${{ secrets.SFTP_PASSWORD }}
    host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINT }}
    uploads: ./dist/ => /var/www/html/
    strategy: sync
```

## Multiple targets with a config file

Different directories, different strategies, one connection:

```yaml
- uses: eiserv/easySFTP@v1
  with:
    server: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
    host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINT }}
    config-file: .github/easysftp.yml
```

```yaml
# .github/easysftp.yml
version: 1
strategy: overlay
ignore:
  - "*.map"
guards:
  max_deletes: 200
targets:
  - local: ./dist/
    remote: /var/www/html/
    strategy: sync
  - local: ./docs/
    remote: /var/www/docs/
    strategy: clean
  - local: ./robots.txt
    remote: /var/www/html/robots.txt
```

Full field reference: [configuration.md](configuration.md#the-yaml-config-file).

## Key-based authentication

Preferred over passwords, see the [security guide](security.md#credentials):

```yaml
- uses: eiserv/easySFTP@v1
  with:
    server: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
    passphrase: ${{ secrets.SFTP_PASSPHRASE }}   # only if the key is encrypted
    host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINT }}
    uploads: ./dist/ => /var/www/html/
```

## Upload a single file (with rename)

A single file maps onto the exact remote path, so you can rename on the fly.
A trailing `/` on the remote side means "into this directory" instead:

```yaml
uploads: |
  ./config/prod.json => /etc/app/config.json
  ./robots.txt => /var/www/html/
```

## Preview a deploy in pull requests

Run the real plan against the real server without changing anything, and post
the numbers to the job summary:

```yaml
name: Deploy preview
on: pull_request

jobs:
  preview:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - run: npm ci && npm run build

      - name: What would deploy?
        uses: eiserv/easySFTP@v1
        with:
          server: sftp.example.com
          username: ${{ secrets.SFTP_USERNAME }}
          password: ${{ secrets.SFTP_PASSWORD }}
          host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINT }}
          uploads: ./dist/ => /var/www/html/
          strategy: sync
          dry-run: true
```

## Using the outputs

Give the step an `id` and read the outputs in later steps:

```yaml
- name: Deploy via SFTP
  id: deploy
  uses: eiserv/easySFTP@v1
  with:
    # ...

- name: Report
  run: |
    echo "Uploaded ${{ steps.deploy.outputs.files-uploaded }} files"
    echo "Deleted  ${{ steps.deploy.outputs.files-deleted }} files"
    echo "Skipped  ${{ steps.deploy.outputs.files-skipped }} unchanged"
    echo "${{ steps.deploy.outputs.bytes-uploaded }} bytes in ${{ steps.deploy.outputs.duration-ms }} ms"
```

## Excludes via .sftpignore

Keep the exclude list out of the workflow file:

```yaml
- uses: eiserv/easySFTP@v1
  with:
    # ...
    uploads: ./dist/ => /var/www/html/
    ignore-from: .sftpignore
```

```gitignore
# .sftpignore, gitignore syntax
*.map
*.log
node_modules/
!keep-this.log
```

## Windows and macOS runners

Nothing changes: easySFTP is a compiled Go binary and runs natively on
`ubuntu-*`, `macos-*` and `windows-*` runners (no Docker required):

```yaml
jobs:
  deploy:
    runs-on: windows-latest
    steps:
      - uses: actions/checkout@v5
      - uses: eiserv/easySFTP@v1
        with:
          server: sftp.example.com
          username: ${{ secrets.SFTP_USERNAME }}
          password: ${{ secrets.SFTP_PASSWORD }}
          host-key-fingerprint: ${{ secrets.SFTP_HOST_KEY_FINGERPRINT }}
          uploads: .\build\ => /var/www/html/
```
