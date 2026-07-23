# Examples & use cases

Copy-paste recipes for common deployments. All of them assume credentials are
stored as [encrypted secrets](https://docs.github.com/en/actions/security-guides/encrypted-secrets).

- [Deploy a static site](#deploy-a-static-site)
- [Mirror a build with sync](#mirror-a-build-with-sync)
- [Multiple deployments with a config file](#multiple-deployments-with-a-config-file)
- [Key-based authentication](#key-based-authentication)
- [Deploy through a jump host (bastion)](#deploy-through-a-jump-host-bastion)
- [Upload a single file (with rename)](#upload-a-single-file-with-rename)
- [Preview a deploy in pull requests](#preview-a-deploy-in-pull-requests)
- [Using the outputs](#using-the-outputs)
- [Excludes](#excludes)
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
        uses: eiserv/easySFTP@v3
        with:
          host: sftp.example.com
          username: ${{ secrets.SFTP_USERNAME }}
          password: ${{ secrets.SFTP_PASSWORD }}
          host-key: ${{ secrets.SFTP_HOST_KEY }}
          source: ./dist/
          target: /var/www/html/
```

## Mirror a build with sync

`sync` keeps the remote directory an exact mirror of your build output:
unchanged files are skipped, files removed locally are deleted remotely, but
only files easySFTP itself uploaded ([manifest-based](strategies.md#sync), so
user uploads and server-generated files survive):

```yaml
- uses: eiserv/easySFTP@v3
  with:
    host: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    password: ${{ secrets.SFTP_PASSWORD }}
    host-key: ${{ secrets.SFTP_HOST_KEY }}
    source: ./dist/
    target: /var/www/html/
    mode: sync
```

## Multiple deployments with a config file

Different directories, different modes, one connection. In config mode every
non-secret setting lives in the file; the workflow step carries only the
config path and credentials:

```yaml
- uses: eiserv/easySFTP@v3
  with:
    config: .github/easysftp.yml
    private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
```

```yaml
# .github/easysftp.yml
version: 3
connection:
  host: sftp.example.com
  username: deploy
  host_key: |
    SHA256:nThbg6kXUpJWGl7E1IGOCspRomTxdCARLviKw6E5SY8
defaults:
  mode: overlay
  exclude:
    - "*.map"
safety:
  max_deletes: 200
deployments:
  website:
    source: ./dist/
    target: /var/www/html/
    mode: sync
  documentation:
    source: ./docs/
    target: /var/www/docs/
    mode: clean
  robots:
    source: ./robots.txt
    target: /var/www/html/robots.txt
```

Full field reference: [configuration.md](configuration.md#the-config-file).

## Key-based authentication

Preferred over passwords, see the [security guide](security.md#credentials):

```yaml
- uses: eiserv/easySFTP@v3
  with:
    host: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
    passphrase: ${{ secrets.SFTP_PASSPHRASE }}   # only if the key is encrypted
    host-key: ${{ secrets.SFTP_HOST_KEY }}
    source: ./dist/
    target: /var/www/html/
```

## Deploy through a jump host (bastion)

For servers that are not reachable from the public internet, connect through
a bastion like OpenSSH's `ProxyJump`. In v3 the jump host's connection lives
in the config file (`connection.proxy`); only its credentials stay inputs.
Each hop has its own credentials and its own host key verification (see
[configuration](configuration.md#connection)):

```yaml
- uses: eiserv/easySFTP@v3
  with:
    config: .github/easysftp.yml
    private-key: ${{ secrets.SFTP_PRIVATE_KEY }}
    proxy-private-key: ${{ secrets.JUMP_PRIVATE_KEY }}
```

```yaml
# .github/easysftp.yml
version: 3
connection:
  host: sftp.internal.example.com
  username: deploy
  host_key: |
    SHA256:nThbg6kXUpJWGl7E1IGOCspRomTxdCARLviKw6E5SY8
  proxy:
    host: bastion.example.com
    username: jumper
    host_key: |
      SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM
deployments:
  website:
    source: ./dist/
    target: /var/www/html/
```

## Upload a single file (with rename)

A single file maps onto the exact remote path, so you can rename on the fly.
A trailing `/` on the target means "into this directory" instead:

```yaml
- uses: eiserv/easySFTP@v3
  with:
    host: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    password: ${{ secrets.SFTP_PASSWORD }}
    host-key: ${{ secrets.SFTP_HOST_KEY }}
    source: ./config/prod.json
    target: /etc/app/config.json
```

For more than one file, use a config file with multiple named deployments.

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
        uses: eiserv/easySFTP@v3
        with:
          host: sftp.example.com
          username: ${{ secrets.SFTP_USERNAME }}
          password: ${{ secrets.SFTP_PASSWORD }}
          host-key: ${{ secrets.SFTP_HOST_KEY }}
          source: ./dist/
          target: /var/www/html/
          mode: sync
          dry-run: true
```

## Using the outputs

Give the step an `id` and read the outputs in later steps:

```yaml
- name: Deploy via SFTP
  id: deploy
  uses: eiserv/easySFTP@v3
  with:
    # ...

- name: Report
  run: |
    echo "Uploaded ${{ steps.deploy.outputs.files-uploaded }} files"
    echo "Deleted  ${{ steps.deploy.outputs.files-deleted }} files"
    echo "Skipped  ${{ steps.deploy.outputs.files-skipped }} unchanged"
    echo "${{ steps.deploy.outputs.bytes-uploaded }} bytes in ${{ steps.deploy.outputs.duration-ms }} ms"
```

## Excludes

Keep noise out of the upload. Inline, one pattern per line:

```yaml
- uses: eiserv/easySFTP@v3
  with:
    # ...
    source: ./dist/
    target: /var/www/html/
    exclude: |
      *.map
      *.log
      node_modules/
      !keep-this.log
```

In a config file, split them into a global `defaults.exclude` and
per-deployment `exclude` lists (they add up):

```yaml
defaults:
  exclude:
    - "*.map"
deployments:
  website:
    source: ./dist/
    target: /var/www/html/
    exclude:
      - node_modules/
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
      - uses: eiserv/easySFTP@v3
        with:
          host: sftp.example.com
          username: ${{ secrets.SFTP_USERNAME }}
          password: ${{ secrets.SFTP_PASSWORD }}
          host-key: ${{ secrets.SFTP_HOST_KEY }}
          source: .\build\
          target: /var/www/html/
```
