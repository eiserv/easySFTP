# Deployment modes

A *mode* decides how each deployment's remote directory is reconciled with
your local files.

| Mode | Uploads | Deletes | Use for |
|---|---|---|---|
| `overlay` (default) | all files | nothing | adding/updating files, leaving everything else in place |
| `sync` | new & changed files | files a previous sync uploaded but that are now gone locally | keeping a directory an exact mirror of your build, safely |
| `clean` | all files | **everything** in the remote target first | a guaranteed-fresh deploy |

Set the mode inline with the `mode` input, or in the
[config file](configuration.md#the-config-file) as `defaults.mode` (global)
or a per-deployment `mode`.

Before uploading, easySFTP creates remote directory leaves in parallel and
checks touched directories for stale temporary files with up to
`advanced.concurrency` SFTP requests. Directory permission updates use the
same bound. These metadata requests stay on the first SSH connection.

> **Renamed in v3.** What v2 called the `strategy` input (and `strategy:` in
> the config file) is the `mode` input and `mode:` field in v3. The three
> behaviors are unchanged. See the [migration guide](migration-v3.md).

## `overlay`

Uploads every local file on top of whatever is already on the server. Nothing
is ever deleted. This is the default and the backwards-compatible behavior.

Uploads are **atomic per file**: content is streamed to a temporary sibling
file (`<name>.easysftp-tmp.<n>`, `<n>` being the file's position in the plan so
two planned transfers never share a temp name) and renamed over the target
only once the transfer fully succeeded. A broken connection never leaves a
half-written file where the live one was. Servers without
`posix-rename@openssh.com` cannot do that swap in one step, so easySFTP parks
the live file under `<name>.easysftp-tmp.bak` first and puts it back if the
rename fails: the target is briefly missing there, but never lost.

### `advanced.skip_unchanged`: skip same-size files

By default, `overlay` re-uploads **every** file on every deploy. Setting
`advanced.skip_unchanged: true` in the config file stats each remote file
first and skips the upload when the remote file already exists with the same
size, which turns a re-deploy of a mostly-unchanged tree from "transfer
everything" into "one cheap stat per unchanged file". Skipped files count into
the `files-skipped` output and are left completely untouched (no permission
change either).

**The trade-off, precisely:** the comparison is size-only. An edit that keeps
the file size identical is invisible to it and will *not* be uploaded. Remote
modification times are not compared; without controlling them at upload time
they mean nothing. If you need exact change detection, use the `sync`
mode, whose manifest compares content hashes; `skip_unchanged` is for
targets where you want cheap incremental behavior *without* easySFTP writing
any state (like the sync manifest) to the server.

`skip_unchanged` only applies to `overlay` deployments. `sync` already skips
unchanged files exactly, and `clean` wipes the target first, so both ignore
the setting (with a warning).

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
- A run that fails partway still records what it actually changed in the
  manifest (best effort), so a retried deploy resumes where it left off
  instead of re-uploading files that already made it.
- Local files are hashed in parallel using the runner's available Go CPU
  parallelism. This is independent of `advanced.concurrency`, which can be
  lowered for an SFTP server without serializing local hashing.
- Remote directories are only created (or confirmed) for files actually being
  uploaded, so a sync with nothing to do costs just the manifest read and
  rewrite, no per-directory round-trips. Exception: with `permissions.directories`
  set, every directory of the plan is still chmod'd, per its documented
  "creates or touches" semantics.

> **⚠️ The manifest is publicly downloadable in web-root deployments.** The
> manifest lives *inside* the deploy target, so when that target is a public
> web root it is served like any other file
> (`https://example.com/.easysftp-manifest.json`). It discloses the complete
> relative file list of the deployment plus each file's SHA-256 content hash;
> useful reconnaissance for paths not linked anywhere. Most default server
> configs do **not** block it (Apache's stock `.ht*` rule does not cover it).
> Mitigations, strongest first: deny it in the web server config (copy-paste
> snippets in [security.md](security.md#the-sync-manifest-in-web-roots)), or
> give it an unguessable name with the `sync.manifest` config field.

### `sync.fast_path`: skip re-hashing unchanged files

By default, `sync` re-reads and re-hashes **every** local file on every run to
decide what changed; that's what makes its "unchanged" comparison exact. For
a large tree where almost nothing changes, that's still a lot of local I/O.

Setting `sync.fast_path: true` in the config file adds a cheaper check first:
if a file's size and modification time still match what the manifest recorded
for it last time, its stored hash is reused and the file is never re-read.
This is the same trade rsync's "quick check" makes.

**The trade-off, precisely:** a file whose content changed *without* its size
or modification time changing is invisible to this check and will be missed.
Modification times are compared at nanosecond resolution, so on common
filesystems (ext4, APFS, NTFS) that takes two same-size edits with
bit-identical timestamps; on filesystems with coarse timestamps (FAT stores
2-second mtimes, and some network filesystems round to seconds), the window
stays as wide as the filesystem's own clock.
Without `fast_path`, `sync` never misses a content change, because it
always compares actual content hashes; this is what you give up in exchange
for skipping local reads.

**When it actually helps:** local modification times need to be meaningful
across runs for the check to ever hit. A fresh `actions/checkout` gives every
file a brand-new modification time on every run; `fast_path` has
nothing to skip there and degrades gracefully to full hashing. It pays off
when the local tree is a restored build cache (`actions/cache`, a persisted
runner, `git restore-timestamps`, …) whose unchanged files keep their old
modification times between runs.

If you suspect a stale hash was reused for the wrong reason, delete the
target's `.easysftp-manifest.json` (or run `mode: clean` once) to force a
full re-hash on the next sync.

Manifests written by older easySFTP versions are read seamlessly, but their
recorded modification times are not comparable (v1 recorded none, v2 recorded
whole seconds); the first sync after upgrading re-hashes everything once and
then records nanosecond times from there on.

## `clean`

Deletes **everything** inside the remote target directory first, then uploads
all local files. Use it when you want a guaranteed-fresh deploy and nothing in
the target directory needs to survive.

The recursive scan and file deletions run with up to `advanced.concurrency`
independent SFTP requests. Empty directories are removed deepest-first, with
siblings at the same depth handled in parallel. `sync` uses the same bounded
delete pool. The guards below are evaluated against the complete file list
before any delete worker starts.

## Delete guards

Two safety nets apply before `sync` or `clean` delete anything:

- **Remote root is always refused.** A target that resolves to `/`, `.`, `~`
  or the empty string is rejected outright. So is one that climbs above the
  directory the session starts in, such as `..`, `../..` or `dist/../..`,
  which is where a target built from a workflow expression tends to land when
  the expression is empty or has a component too many. No mode will ever wipe
  a server root or a login directory. A relative target that stays put, like
  `www/public_html`, is unaffected.
- **`max_deletes`** aborts a run that would delete more files than the limit,
  catching a misconfiguration before it does damage. `0` means unlimited. Set
  it via `safety.max_deletes` in the [config file](configuration.md#sections).

## Dry runs

Preview any mode without touching the server:

```yaml
dry-run: true
```

easySFTP connects, plans everything and logs exactly what would be uploaded,
skipped and deleted, but changes nothing. The `files-uploaded` /
`files-deleted` outputs report the *planned* counts, so you can use a dry run
in a pull request to preview a deploy.
