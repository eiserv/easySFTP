# Strategies

A *strategy* decides how each target's remote directory is reconciled with
your local files.

| Strategy | Uploads | Deletes | Use for |
|---|---|---|---|
| `overlay` (default) | all files | nothing | adding/updating files, leaving everything else in place |
| `sync` | new & changed files | files a previous sync uploaded but that are now gone locally | keeping a directory an exact mirror of your build, safely |
| `clean` | all files | **everything** in the remote target first | a guaranteed-fresh deploy |

Set the strategy globally with the `strategy` input, or per target in the
[config file](configuration.md#the-yaml-config-file).

## `overlay`

Uploads every local file on top of whatever is already on the server. Nothing
is ever deleted. This is the default and the backwards-compatible behavior.

Uploads are **atomic per file**: content is streamed to a temporary sibling
file (`<name>.easysftp-tmp`) and renamed over the target only once the transfer
fully succeeded — a broken connection never leaves a half-written file where
the live one was.

## `sync`

Uploads new and changed files, and deletes remote files that a previous sync
uploaded but that no longer exist locally. Unchanged files (compared by SHA256
content hash) are skipped entirely, so re-deploys only transfer what actually
changed.

`sync` is **manifest-based**: it keeps a small `.easysftp-manifest.json` in
each target directory listing the relative path and content hash of every file
it uploaded. Only files listed in the manifest are ever deleted — files put on
the server by anyone else (uploads from other tools, server-generated files,
user content) are never touched.

Notes:

- The first sync into a directory uploads everything and creates the manifest.
- A missing or corrupt manifest degrades safely to "upload everything, delete
  nothing" (with a warning).
- The manifest trusts itself: a file changed *on the server* out of band is not
  re-detected until its local content changes. Run `clean` once to reset.
- Directories left empty by deletions are pruned automatically.

## `clean`

Deletes **everything** inside the remote target directory first, then uploads
all local files. Use it when you want a guaranteed-fresh deploy and nothing in
the target directory needs to survive.

The `delete: true` input is a legacy alias for `strategy: clean`.

## Delete guards

Two safety nets apply before `sync` or `clean` delete anything:

- **Remote root is refused — always.** A target that resolves to `/` (or `.`)
  is rejected outright. No strategy will ever wipe a server root.
- **`guards.max_deletes`** ([config file](configuration.md#fields) only) aborts
  a run that would delete more files than the limit, catching a
  misconfiguration before it does damage. `0` means unlimited.

## Dry runs

Preview any strategy without touching the server:

```yaml
dry-run: true
```

easySFTP connects, plans everything and logs exactly what would be uploaded,
skipped and deleted — but changes nothing. The `files-uploaded` /
`files-deleted` outputs report the *planned* counts, so you can use a dry run
in a pull request to preview a deploy.
