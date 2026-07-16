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

// manifestName is the file the sync strategy keeps in each remote target to
// record what it previously uploaded. Only files listed here are ever deleted,
// so files placed on the server by others are left untouched.
const manifestName = ".easysftp-manifest.json"

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
func executeSync(ctx context.Context, cfg *config.Config, client *sftp.Client, p plan, stats *Stats, log Logger) error {
	verb := planVerb(cfg)
	base := normalizeRemote(p.pair.Remote)

	old := readManifest(client, base, log)

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

	var toDelete []string
	for rel := range old.Files {
		if _, ok := local[rel]; !ok {
			toDelete = append(toDelete, path.Join(base, rel))
		}
	}
	sort.Strings(toDelete)

	// note: manifest is trusted — a file changed on the server out of band
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

	if err := uploadFiles(ctx, cfg, client, upload, p.remoteDirs, stats, verb, log); err != nil {
		return err
	}

	for _, full := range toDelete {
		log.Infof("%sdelete %s", verb, full)
		if !cfg.DryRun {
			if err := client.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("deleting %q: %w", full, err)
			}
		}
		stats.FilesDeleted++
	}
	stats.FilesSkipped += len(p.files) - len(upload)

	if cfg.DryRun {
		return nil
	}

	pruneEmptyDirs(client, base, toDelete)
	if err := writeManifest(client, base, manifest{Version: manifestVersion, Files: local}); err != nil {
		return fmt.Errorf("writing sync manifest in %q: %w", base, err)
	}
	return nil
}

// readManifest loads the remote manifest. A missing manifest means a first
// sync (empty). A corrupt one is treated the same, with a warning, so a bad
// manifest degrades to "upload everything, delete nothing" instead of failing.
//
// Both the current (v2: hash+size+mtime) and old (v1: hash only) formats are
// accepted; a v1 entry decodes with MTime 0, which never matches the fast
// path in hashPlanFiles, so upgrading from a v1 manifest costs one full
// re-hash and then writes v2 from then on.
func readManifest(client *sftp.Client, dir string, log Logger) manifest {
	empty := manifest{Version: manifestVersion, Files: map[string]manifestEntry{}}
	f, err := client.Open(path.Join(dir, manifestName))
	if err != nil {
		return empty
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		log.Warningf("could not read sync manifest in %s (%v) — treating as first sync", dir, err)
		return empty
	}

	var v2 manifest
	if err := json.Unmarshal(data, &v2); err == nil && v2.Files != nil {
		return v2
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
		return manifest{Version: v1.Version, Files: files}
	}

	log.Warningf("sync manifest in %s is unreadable — treating as first sync", dir)
	return empty
}

// writeManifest atomically writes the manifest into dir.
func writeManifest(client *sftp.Client, dir string, m manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	target := path.Join(dir, manifestName)
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
// deepest first, walking up to (but not including) base.
func pruneEmptyDirs(client *sftp.Client, base string, deleted []string) {
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
		_ = client.RemoveDirectory(dir)
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
