# Migrating from v1 to v2

v2 has exactly one breaking change: the `delete` input was removed. Everything
else works unchanged; bumping the version is the whole migration for most
workflows.

## The `delete` input became `strategy: clean`

In v1, `delete: true` wiped the remote target before uploading. In v2 that
behavior is one of three [strategies](strategies.md) and is spelled
`strategy: clean`:

```yaml
# v1
- uses: eiserv/easySFTP@v1
  with:
    server: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    password: ${{ secrets.SFTP_PASSWORD }}
    uploads: ./dist/ => /var/www/html/
    delete: true

# v2
- uses: eiserv/easySFTP@v2
  with:
    server: sftp.example.com
    username: ${{ secrets.SFTP_USERNAME }}
    password: ${{ secrets.SFTP_PASSWORD }}
    uploads: ./dist/ => /var/www/html/
    strategy: clean
```

If you used `delete: false` (or did not set `delete` at all), simply remove
the input; the default strategy, `overlay`, keeps the old default behavior of
uploading without deleting anything.

## Worth a look while you are at it

Not required for the migration, but v2 also added:

- [`strategy: sync`](strategies.md#sync), which keeps the remote directory an
  exact mirror of your build without wiping it on every deploy. If you used
  `delete: true` just to get rid of stale files, `sync` is usually the better
  fit, and it re-uploads only what changed.
- [Delete guards](strategies.md#delete-guards): destructive strategies refuse
  the remote root, and `max-deletes` caps how much a single run may delete.
- A [YAML config file](configuration.md#the-yaml-config-file) for deploying
  multiple targets with different strategies in one step.

## Future migrations

This page covers v1 to v2, the only major-version migration so far. Guides for
future major versions will be added here.
