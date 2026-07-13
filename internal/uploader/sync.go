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

// manifest records the relative paths and content hashes of the last sync.
type manifest struct {
	Version int               `json:"version"`
	Files   map[string]string `json:"files"` // rel path => sha256 hex
}

// executeSync reconciles the remote target with the local tree using the
// manifest: it uploads new/changed files, deletes files that the previous sync
// wrote but are now gone locally, prunes empty directories and rewrites the
// manifest. Unchanged files are skipped.
func executeSync(ctx context.Context, cfg *config.Config, client *sftp.Client, p plan, stats *Stats, log Logger) error {
	verb := planVerb(cfg)
	base := normalizeRemote(p.pair.Remote)

	old := readManifest(client, base, log)

	local := make(map[string]string, len(p.files))
	var upload []fileItem
	for _, f := range p.files {
		local[f.rel] = f.hash
		if h, ok := old.Files[f.rel]; !ok || h != f.hash {
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

	// ponytail: manifest is trusted — a file changed on the server out of band
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
	if err := writeManifest(client, base, manifest{Version: 1, Files: local}); err != nil {
		return fmt.Errorf("writing sync manifest in %q: %w", base, err)
	}
	return nil
}

// readManifest loads the remote manifest. A missing manifest means a first
// sync (empty). A corrupt one is treated the same, with a warning, so a bad
// manifest degrades to "upload everything, delete nothing" instead of failing.
func readManifest(client *sftp.Client, dir string, log Logger) manifest {
	empty := manifest{Version: 1, Files: map[string]string{}}
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
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil || m.Files == nil {
		log.Warningf("sync manifest in %s is unreadable — treating as first sync", dir)
		return empty
	}
	return m
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
