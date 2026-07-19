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
file (`<name>.easysftp-tmp.<n>`, `<n>` being the file's position in the plan so
two planned transfers never share a temp name) and renamed over the target
only once the transfer fully succeeded. A broken connection never leaves a
half-written file where the live one was.

## `sync`

Uploads new and changed files, and deletes remote files that a previous sync
uploaded but that no longer exist locally. Unchanged files (compared by SHA256
content hash) are skipped entirely, so re-deploys only transfer what actually
changed.

`sync` is **manifest-based**: it keeps a small `.easysftp-manifest.json` in
each target directory listing the relative path and content hash of every file
it uploaded. Only files listed in the manifest are ever deleted. Files put on
the server by anyone else (uploads from other tools, server-generated files,
user content) are never touched.

Notes:

- The first sync into a directory uploads everything and creates the manifest.
- A missing or corrupt manifest degrades safely to "upload everything, delete
  nothing" (with a warning).
- The manifest trusts itself: a file changed *on the server* out of band is not
  re-detected until its local content changes. Run `clean` once to reset.
- Directories left empty by deletions are pruned automatically.
- Local files are hashed in parallel through a worker pool bounded by
  `concurrency`, so planning a large tree uses the available runner CPU.

### `sync-fast-path`: skip re-hashing unchanged files

By default, `sync` re-reads and re-hashes **every** local file on every run to
decide what changed; that's what makes its "unchanged" comparison exact. For
a large tree where almost nothing changes, that's still a lot of local I/O.

Setting `sync-fast-path: true` adds a cheaper check first: if a file's size
and modification time still match what the manifest recorded for it last
time, its stored hash is reused and the file is never re-read. This is the
same trade rsync's "quick check" makes.

**The trade-off, precisely:** a file whose content changed *without* its size
or modification time changing is invisible to this check and will be missed;
for example, two edits that happen to produce the same file size within the
same mtime second (mtimes have one-second resolution on most filesystems).
Without `sync-fast-path`, `sync` never misses a content change, because it
always compares actual content hashes; this is what you give up in exchange
for skipping local reads.

**When it actually helps:** local modification times need to be meaningful
across runs for the check to ever hit. A fresh `actions/checkout` gives every
file a brand-new modification time on every run; `sync-fast-path` has
nothing to skip there and degrades gracefully to full hashing. It pays off
when the local tree is a restored build cache (`actions/cache`, a persisted
runner, `git restore-timestamps`, …) whose unchanged files keep their old
modification times between runs.

If you suspect a stale hash was reused for the wrong reason, delete the
target's `.easysftp-manifest.json` (or run `strategy: clean` once) to force a
full re-hash on the next sync.

Manifests written before this option existed have no recorded modification
time; the first sync after upgrading re-hashes everything once and then
starts recording it.

## `clean`

Deletes **everything** inside the remote target directory first, then uploads
all local files. Use it when you want a guaranteed-fresh deploy and nothing in
the target directory needs to survive.

The `delete: true` input was removed in v2; `strategy: clean` replaces it.

## Delete guards

Two safety nets apply before `sync` or `clean` delete anything:

- **Remote root is always refused.** A target that resolves to `/` (or `.`)
  is rejected outright. No strategy will ever wipe a server root.
- **`max_deletes`** aborts a run that would delete more files than the limit,
  catching a misconfiguration before it does damage. `0` means unlimited. Set
  it via the `max-deletes` input, or `guards.max_deletes` in the
  [config file](configuration.md#fields) when using one.

## Dry runs

Preview any strategy without touching the server:

```yaml
dry-run: true
```

easySFTP connects, plans everything and logs exactly what would be uploaded,
skipped and deleted, but changes nothing. The `files-uploaded` /
`files-deleted` outputs report the *planned* counts, so you can use a dry run
in a pull request to preview a deploy.
