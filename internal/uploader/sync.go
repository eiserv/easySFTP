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
	"sort"

	"github.com/pkg/sftp"

	"github.com/eiserv/easySFTP/internal/config"
)

// manifestName is the default file name the sync strategy keeps in each remote
// target to record what it previously uploaded. Only files listed there are
// ever deleted, so files placed on the server by others are left untouched.
// The manifest-name input can override it per run; see Config.SyncManifestName.
const manifestName = config.DefaultManifestName

// manifestVersion is written to every manifest this version of easySFTP
// produces. v1 manifests (hash only, no size/mtime) are still read; see
// readManifest.
const manifestVersion = 2

// manifestEntry records what is known about one file from the last sync.
// Size and MTime enable the size+mtime fast path in hashPlanFiles: a v1
// manifest entry has MTime 0, which never matches and always falls back to a
// full re-hash.
type manifestEntry struct {
	Hash  string `json:"hash"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // local modification time at upload, unix seconds
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
	if err := sess.do(ctx, watch, func(client *sftp.Client) error {
		var err error
		old, err = readManifest(client, base, cfg.SyncManifestName(), log)
		return err
	}); err != nil {
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
	if err := hashPlanFiles(ctx, p.files, cfg.Concurrency, cached); err != nil {
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
		if _, ok := local[rel]; !ok {
			toDelete = append(toDelete, rel)
		}
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
	// from the manifest hashes, which is strictly more precise.
	completed, err := uploadFiles(ctx, cfg, sess, upload, dirs, stats, verb, watch, false, log)
	if err != nil {
		writeRecoveryManifest(ctx, cfg, sess, watch, base, mergedManifest(old, upload, completed, nil), log)
		return err
	}

	var deleted []string // relative paths actually removed, for the recovery manifest
	for _, rel := range toDelete {
		full := path.Join(base, rel)
		if cfg.LogPerFile() {
			log.Infof("%sdelete %s", verb, full)
		}
		if !cfg.DryRun {
			err := sess.do(ctx, watch, func(client *sftp.Client) error {
				// Already-gone counts as deleted: a retried delete may have
				// landed before the connection died.
				if err := client.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				return nil
			})
			if err != nil {
				writeRecoveryManifest(ctx, cfg, sess, watch, base, mergedManifest(old, upload, completed, deleted), log)
				return fmt.Errorf("deleting %q: %w", full, err)
			}
		}
		stats.FilesDeleted++
		deleted = append(deleted, rel)
	}
	stats.FilesSkipped += len(p.files) - len(upload)

	if cfg.DryRun {
		return nil
	}

	deletedFull := make([]string, len(deleted))
	for i, rel := range deleted {
		deletedFull[i] = path.Join(base, rel)
	}
	pruneEmptyDirs(ctx, sess, watch, base, deletedFull)
	if err := sess.do(ctx, watch, func(client *sftp.Client) error {
		return writeManifest(client, base, cfg.SyncManifestName(), manifest{Version: manifestVersion, Files: local})
	}); err != nil {
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
func writeRecoveryManifest(ctx context.Context, cfg *config.Config, sess *session, watch *stallWatchdog, base string, m manifest, log Logger) {
	if cfg.DryRun {
		return
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
// Both the current (v2: hash+size+mtime) and old (v1: hash only) formats are
// accepted; a v1 entry decodes with MTime 0, which never matches the fast
// path in hashPlanFiles, so upgrading from a v1 manifest costs one full
// re-hash and then writes v2 from then on.
func readManifest(client *sftp.Client, dir, name string, log Logger) (manifest, error) {
	empty := manifest{Version: manifestVersion, Files: map[string]manifestEntry{}}
	f, err := client.Open(path.Join(dir, name))
	if err != nil {
		if isConnError(err) {
			return empty, err
		}
		if !errors.Is(err, os.ErrNotExist) {
			log.Warningf("could not open sync manifest in %s (%v); treating as first sync", dir, err)
		}
		return empty, nil
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		if isConnError(err) {
			return empty, err
		}
		log.Warningf("could not read sync manifest in %s (%v); treating as first sync", dir, err)
		return empty, nil
	}

	var v2 manifest
	if err := json.Unmarshal(data, &v2); err == nil && v2.Files != nil {
		return v2, nil
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
		return manifest{Version: v1.Version, Files: files}, nil
	}

	log.Warningf("sync manifest in %s is unreadable; treating as first sync", dir)
	return empty, nil
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
func pruneEmptyDirs(ctx context.Context, sess *session, watch *stallWatchdog, base string, deleted []string) {
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
	// Deepest paths sort last; remove them first so parents can then empty out.
	sort.Sort(sort.Reverse(sort.StringSlice(candidates)))
	for _, dir := range candidates {
		_ = sess.do(ctx, watch, func(client *sftp.Client) error {
			return client.RemoveDirectory(dir)
		})
	}
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
