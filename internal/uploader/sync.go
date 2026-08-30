package uploader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sort"

	"github.com/pkg/sftp"

	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/metrics"
)

// manifestVersion is written to every manifest this version of easySFTP
// produces. Older manifests are still read: v1 (hash only, no size/mtime)
// and v2 (mtime in whole seconds); see readManifest.
const manifestVersion = 3

// maxManifestBytes caps what readManifest will pull into memory. The manifest
// is a remote file, so its size is not this run's to trust: without a cap, a
// server (or anything that can write the deploy target) turns a sync into an
// unbounded allocation. At roughly 120 bytes per entry this still admits about
// half a million files, far past any plausible deployment, and an over-size
// manifest degrades to a first sync exactly like an unreadable one. See
// issue #223.
//
// A var rather than a const only so the size test can lower it instead of
// seeding 64 MiB through the in-process server; no test in this package runs
// in parallel.
var maxManifestBytes int64 = 64 << 20

// manifestEntry records what is known about one file from the last sync.
// Size and MTime enable the size+mtime fast path in hashPlanFiles: an entry
// with MTime 0 never matches and always falls back to a full re-hash.
//
// v3 records MTime in nanoseconds. v2 recorded whole seconds, which made a
// same-size edit landing in the same wall-clock second as the recorded time
// invisible to the fast path, so the file was silently never re-uploaded
// (issue #162). Nanoseconds narrow that window to the filesystem's actual
// timestamp granularity.
type manifestEntry struct {
	Hash  string `json:"hash"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // local modification time at upload, unix nanoseconds
}

// manifest records what the last sync uploaded, keyed by relative path.
type manifest struct {
	Version int                      `json:"version"`
	Files   map[string]manifestEntry `json:"files"`
}

// executeSync reconciles the remote target with the local tree using the
// manifest: it uploads new/changed files, deletes files that the previous sync
// wrote but are now gone locally, prunes empty directories and rewrites the
// manifest. Unchanged files are skipped.
func executeSync(ctx context.Context, cfg *config.Config, sess *session, p plan, stats *Stats, watch *stallWatchdog, log Logger) error {
	verb := planVerb(cfg)
	base := normalizeRemote(p.pair.Remote)

	// Every remote operation in this function runs through sess.do, so a
	// connection drop during manifest handling or the delete phase redials
	// (within the shared reconnect budget) instead of failing the run, and a
	// hung server trips stall-timeout; see issue #107.
	var old manifest
	endRead := metrics.Phase("manifest_read")
	err := sess.do(ctx, watch, func(client *sftp.Client) error {
		var err error
		old, err = readManifest(client, base, cfg.SyncManifestName(), log)
		return err
	})
	endRead()
	if err != nil {
		return fmt.Errorf("reading sync manifest in %q: %w", base, err)
	}

	// Hash after reading the manifest (not during buildPlan) so that, with
	// sync-fast-path opted in, unchanged files whose size and mtime still
	// match their manifest entry can reuse the stored hash instead of being
	// re-read from disk. See hashPlanFiles.
	var cached map[string]manifestEntry
	if cfg.SyncFastPath {
		cached = old.Files
	}
	endHash := metrics.Phase("hash")
	// Hashing is local CPU and disk work, so its worker count follows the
	// runner rather than the server-facing upload limit. A user may reduce
	// advanced.concurrency because their SFTP server rejects parallel writes;
	// that must not serialize an unrelated local phase (issue #155).
	err = hashPlanFiles(ctx, p.files, runtime.GOMAXPROCS(0), cached)
	endHash()
	if err != nil {
		return fmt.Errorf("hashing local files under %q: %w", p.pair.Local, err)
	}

	local := make(map[string]manifestEntry, len(p.files))
	var upload []fileItem
	for _, f := range p.files {
		local[f.rel] = manifestEntry{Hash: f.hash, Size: f.size, MTime: f.mtime}
		if e, ok := old.Files[f.rel]; !ok || e.Hash != f.hash {
			upload = append(upload, f)
		}
	}

	var toDelete []string // paths relative to base, ascending
	for rel := range old.Files {
		if _, ok := local[rel]; ok {
			continue
		}
		// The manifest lives in the deploy target, so its keys are data the
		// server (or anything else that can write that one file) supplies,
		// and every key that survives here becomes an argument to Remove.
		// readManifest already drops keys that would leave the deployment;
		// this re-checks at the point the key becomes a delete target, which
		// is where the property is worth stating. See safeJoin and issue #223.
		if _, err := safeJoin(base, rel); err != nil {
			log.Warningf("ignoring sync manifest entry in %s: %v", base, err)
			continue
		}
		toDelete = append(toDelete, rel)
	}
	sort.Strings(toDelete)

	// note: manifest is trusted; a file changed on the server out of band
	// is not re-detected until its local content changes. Run clean to reset.
	log.Infof("%ssync: %d to upload, %d to delete, %d unchanged",
		verb, len(upload), len(toDelete), len(p.files)-len(upload))

	if len(toDelete) > 0 {
		if err := checkRemoteRoot(p.pair.Remote); err != nil {
			return err
		}
		if err := checkMaxDeletes(len(toDelete), cfg); err != nil {
			return err
		}
	}

	// Directories are derived from the files actually being uploaded, so an
	// unchanged (or barely changed) sync pays no directory round-trips for
	// the untouched parts of the tree. With dir-mode set, the full plan's
	// directory list is kept instead: dir-mode is documented as applying to
	// every remote directory the run creates or touches.
	dirs := dirsForFiles(upload)
	if cfg.DirMode != nil {
		dirs = p.remoteDirs
	}
	// skip-unchanged is always off here: sync already decided what changed
	// from the manifest hashes, which is strictly more precise. The full plan
	// goes along with the changed subset because the stale-temp sweep must not
	// remove an unchanged planned target that happens to be named like a temp
	// file; see uploadFiles and issue #186.
	completed, err := uploadFiles(ctx, cfg, sess, upload, p.files, dirs, base, stats, verb, watch, false, log)
	if err != nil {
		writeRecoveryManifest(ctx, cfg, sess, watch, base, mergedManifest(old, upload, completed, nil), log)
		return err
	}

	deletePaths := make([]string, len(toDelete))
	for i, rel := range toDelete {
		// Cannot fail: every entry of toDelete passed safeJoin above.
		deletePaths[i], _ = safeJoin(base, rel)
	}
	endSweep := metrics.Phase("delete_sweep")
	deleteResults, deleteErr := deleteRemoteFiles(ctx, cfg, sess, deletePaths, watch, log)
	var deleted []string // relative paths actually removed, for the recovery manifest
	for i, ok := range deleteResults {
		if ok {
			deleted = append(deleted, toDelete[i])
		}
	}
	stats.FilesDeleted += len(deleted)
	endSweep()
	if deleteErr != nil {
		writeRecoveryManifest(ctx, cfg, sess, watch, base, mergedManifest(old, upload, completed, deleted), log)
		return deleteErr
	}
	stats.FilesSkipped += len(p.files) - len(upload)

	if cfg.DryRun {
		return nil
	}

	deletedFull := make([]string, len(deleted))
	for i, rel := range deleted {
		// deleted is a subset of toDelete, so this cannot fail either.
		deletedFull[i], _ = safeJoin(base, rel)
	}
	pruneEmptyDirs(ctx, cfg, sess, watch, base, deletedFull)
	endWrite := metrics.Phase("manifest_write")
	err = sess.do(ctx, watch, func(client *sftp.Client) error {
		return writeManifest(client, base, cfg.SyncManifestName(), manifest{Version: manifestVersion, Files: local})
	})
	endWrite()
	if err != nil {
		return fmt.Errorf("writing sync manifest in %q: %w", base, err)
	}
	return nil
}

// mergedManifest builds the manifest a partially failed run leaves behind:
// files that did upload get their new entry, files that were actually deleted
// drop out, and everything else keeps its old entry, so the manifest keeps
// matching what is really on the server.
func mergedManifest(old manifest, upload []fileItem, completed []bool, deleted []string) manifest {
	files := make(map[string]manifestEntry, len(old.Files))
	for rel, e := range old.Files {
		files[rel] = e
	}
	for i, f := range upload {
		if completed[i] {
			files[f.rel] = manifestEntry{Hash: f.hash, Size: f.size, MTime: f.mtime}
		}
	}
	for _, rel := range deleted {
		delete(files, rel)
	}
	return manifest{Version: manifestVersion, Files: files}
}

// writeRecoveryManifest best-effort persists the merged manifest of a failing
// run, so a retry resumes from the files that did make it instead of
// re-uploading them. The run is already failing; a manifest write error here
// is logged, not returned. It goes through sess.do so a run that failed to a
// connection drop still records its progress on the redialed connection
// (budget permitting).
//
// When the failure was a stall-timeout, sess.do would normally refuse to
// redial (see its watch.fired short-circuit): redialing a server that stalled
// on large transfers usually just stalls again. But the manifest is a single
// small round-trip that will likely go through on a fresh connection even so,
// and recording progress is exactly what saves a large deploy from
// re-uploading everything on the retry. The watchdog has already fired (its
// monitor goroutine has exited), so it protects nothing here anyway; dropping
// it lets do() spend one reconnect from the shared budget for this write, then
// the caller fails the run as before. See issue #115.
func writeRecoveryManifest(ctx context.Context, cfg *config.Config, sess *session, watch *stallWatchdog, base string, m manifest, log Logger) {
	if cfg.DryRun {
		return
	}
	if watch != nil && watch.fired.Load() {
		watch = nil
	}
	err := sess.do(ctx, watch, func(client *sftp.Client) error {
		return writeManifest(client, base, cfg.SyncManifestName(), m)
	})
	if err != nil {
		log.Warningf("could not record partial progress in the sync manifest in %s (a retry will re-upload this run's completed files): %v", base, err)
		return
	}
	log.Infof("recorded partial progress in the sync manifest in %s: a retry will resume from there", base)
}

// readManifest loads the remote manifest. A missing manifest means a first
// sync (empty). A corrupt one is treated the same, with a warning, so a bad
// manifest degrades to "upload everything, delete nothing" instead of failing.
// A connection-class failure is returned as an error instead of being folded
// into "first sync": the caller (sess.do) reconnects and retries the read, so
// a mid-run drop cannot silently discard the manifest.
//
// All three formats are accepted: v3 (hash+size+mtime in nanoseconds), v2
// (hash+size+mtime in whole seconds) and v1 (hash only). A v1 entry decodes
// with MTime 0 and a v2 entry has its whole-second MTime dropped to 0, since
// it must never be compared against the nanosecond values recorded now.
// MTime 0 never matches the fast path in hashPlanFiles, so upgrading from an
// older manifest costs one full re-hash and then writes v3 from then on.
func readManifest(client *sftp.Client, dir, name string, log Logger) (manifest, error) {
	empty := manifest{Version: manifestVersion, Files: map[string]manifestEntry{}}
	manifestPath := path.Join(dir, name)
	f, err := client.Open(manifestPath)
	if err != nil {
		if isConnError(err) {
			return empty, err
		}
		if !errors.Is(err, os.ErrNotExist) {
			// A server that answers a missing file with the generic
			// SSH_FX_FAILURE would otherwise warn on every first sync, which
			// is the one run where no manifest is the expected state. Asking
			// costs a round-trip only when the open already failed.
			absent, aerr := remoteAbsent(client, manifestPath, nil)
			if aerr != nil {
				return empty, aerr
			}
			if !absent {
				log.Warningf("could not open sync manifest in %s (%v); treating as first sync", dir, err)
			}
		}
		return empty, nil
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil {
		if isConnError(err) {
			return empty, err
		}
		log.Warningf("could not read sync manifest in %s (%v); treating as first sync", dir, err)
		return empty, nil
	}
	if int64(len(data)) > maxManifestBytes {
		log.Warningf("sync manifest in %s is larger than %s; treating as first sync (nothing will be deleted this run)", dir, HumanSize(maxManifestBytes))
		return empty, nil
	}

	var m manifest
	if err := json.Unmarshal(data, &m); err == nil && m.Files != nil {
		if m.Version < 3 {
			// A v2 manifest recorded mtimes in whole seconds. Zero them so
			// the fast path in hashPlanFiles never takes a second-resolution
			// value for a nanosecond one; the hashes stay valid, only the
			// one-time full re-hash is paid (exactly like the v1 upgrade).
			for rel, e := range m.Files {
				e.MTime = 0
				m.Files[rel] = e
			}
		}
		return confineManifest(m, dir, log), nil
	}

	var v1 struct {
		Version int               `json:"version"`
		Files   map[string]string `json:"files"`
	}
	if err := json.Unmarshal(data, &v1); err == nil && v1.Files != nil {
		files := make(map[string]manifestEntry, len(v1.Files))
		for rel, hash := range v1.Files {
			files[rel] = manifestEntry{Hash: hash}
		}
		return confineManifest(manifest{Version: v1.Version, Files: files}, dir, log), nil
	}

	log.Warningf("sync manifest in %s is unreadable; treating as first sync", dir)
	return empty, nil
}

// confineManifest drops every entry whose key would not stay under dir. The
// keys easySFTP writes are always plain relative slash paths, so a key that is
// absolute or climbs out was not written by a run of this action; deleting
// what it points at would hand whoever put it there the deploy account's whole
// reach. Dropping is the right degradation: the file it named is simply not
// deleted, and the next successful sync rewrites the manifest from the local
// tree without it. See issue #223.
func confineManifest(m manifest, dir string, log Logger) manifest {
	var dropped []string
	for rel := range m.Files {
		if _, err := safeJoin(dir, rel); err != nil {
			dropped = append(dropped, rel)
		}
	}
	if len(dropped) == 0 {
		return m
	}
	sort.Strings(dropped)
	for _, rel := range dropped {
		delete(m.Files, rel)
	}
	log.Warningf("sync manifest in %s has %d entry/entries that point outside the deployment (first: %q); ignoring them", dir, len(dropped), dropped[0])
	return m
}

// writeManifest atomically writes the manifest into dir under name.
func writeManifest(client *sftp.Client, dir, name string, m manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	target := path.Join(dir, name)
	tmp := target + tmpSuffix
	dst, err := client.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := dst.Write(data); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return renameReplace(client, tmp, target)
}

// pruneEmptyDirs best-effort removes directories left empty by deletions,
// deepest first, walking up to (but not including) base. Each removal runs
// through sess.do: the outcome stays best-effort, but a dropped connection is
// redialed rather than silently failing every remaining removal.
func pruneEmptyDirs(ctx context.Context, cfg *config.Config, sess *session, watch *stallWatchdog, base string, deleted []string) {
	seen := map[string]struct{}{}
	var candidates []string
	for _, f := range deleted {
		for dir := path.Dir(f); dir != base && dir != "." && dir != "/"; dir = path.Dir(dir) {
			if _, ok := seen[dir]; ok {
				break
			}
			seen[dir] = struct{}{}
			candidates = append(candidates, dir)
		}
	}
	defer metrics.Phase("prune_dirs")()
	removeRemoteDirs(ctx, cfg, sess, watch, candidates)
}

// hashFile returns the sha256 hex digest of a local file's contents.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
