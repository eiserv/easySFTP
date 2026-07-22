package uploader

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/pkg/sftp"

	"github.com/eiserv/easySFTP/internal/config"
)

// createRemoteDirs creates every remote directory the plan needs with as few
// SFTP round-trips as possible. It calls MkdirAll only on the deepest (leaf)
// directories: MkdirAll creates any missing parents in the same walk and
// treats an already-existing directory as success, so ancestors are never
// stat'd or created one level at a time. Only when a creation fails does it
// look closer, to report a path that already exists as a file clearly.
func createRemoteDirs(client *sftp.Client, dirs []string, dirMode *fs.FileMode, log Logger) error {
	for _, dir := range leafDirs(dirs) {
		if err := client.MkdirAll(dir); err != nil {
			if bad := nonDirConflict(client, dir); bad != "" {
				return fmt.Errorf("remote path %q exists but is not a directory", bad)
			}
			return fmt.Errorf("creating remote directory %q: %w", dir, err)
		}
	}

	if dirMode != nil {
		warned := false
		for _, dir := range dirs {
			if err := client.Chmod(dir, dirMode.Perm()); err != nil && !warned {
				log.Warningf("could not set dir-mode %04o on %s (server may reject SETSTAT); not warning again this run: %v", dirMode.Perm(), dir, err)
				warned = true
			}
		}
	}
	return nil
}

// leafDirs reduces a directory set to just the deepest members: those that are
// not the parent of another directory in the set. The plan already lists every
// ancestor of every file, so calling MkdirAll on the leaves alone still creates
// the whole tree, just with far fewer calls on deep hierarchies, where each
// leaf's parents would otherwise be created and checked one level at a time.
func leafDirs(dirs []string) []string {
	hasChild := make(map[string]struct{}, len(dirs))
	for _, d := range dirs {
		hasChild[path.Dir(d)] = struct{}{}
	}
	leaves := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if _, parent := hasChild[d]; !parent {
			leaves = append(leaves, d)
		}
	}
	sort.Strings(leaves)
	return leaves
}

// nonDirConflict returns the shallowest ancestor of dir (dir itself included)
// that exists on the server but is not a directory, or "" if there is none. It
// is consulted only after MkdirAll fails, to turn a low-level error into a
// clear message naming the offending path.
func nonDirConflict(client *sftp.Client, dir string) string {
	for _, d := range append(parentDirs(dir), dir) {
		if info, err := client.Stat(d); err == nil && !info.IsDir() {
			return d
		}
	}
	return ""
}

// checkRemoteRoot refuses a destructive strategy whose target resolves to the
// filesystem root or an unspecific path: the one guard that is always on.
func checkRemoteRoot(remote string) error {
	switch normalizeRemote(remote) {
	case "/", ".", "", "~":
		return fmt.Errorf("refusing a destructive strategy on remote root %q; target a specific subdirectory instead", remote)
	}
	return nil
}

// checkMaxDeletes enforces the guards.max_deletes limit (0 means unlimited).
func checkMaxDeletes(n int, cfg *config.Config) error {
	if cfg.Guards.MaxDeletes > 0 && n > cfg.Guards.MaxDeletes {
		return fmt.Errorf("refusing to delete %d files: exceeds guards.max_deletes=%d (raise the limit, or run with dry-run to inspect the plan)", n, cfg.Guards.MaxDeletes)
	}
	return nil
}

// listRemoteContents returns every regular file and directory under dir
// (recursively, dir itself excluded), directories parents-first. A missing
// dir yields empty lists and no error.
func listRemoteContents(client *sftp.Client, dir string) (files, dirs []string, err error) {
	entries, err := client.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	for _, e := range entries {
		full := path.Join(dir, e.Name())
		if e.IsDir() {
			dirs = append(dirs, full)
			subFiles, subDirs, err := listRemoteContents(client, full)
			if err != nil {
				return nil, nil, err
			}
			files = append(files, subFiles...)
			dirs = append(dirs, subDirs...)
			continue
		}
		files = append(files, full)
	}
	return files, dirs, nil
}

// normalizeRemote converts a remote path to a clean slash path.
func normalizeRemote(remote string) string {
	return path.Clean(strings.ReplaceAll(remote, "\\", "/"))
}

// parentDirs returns all ancestor directories of a remote file path,
// shallowest first, excluding "." and "/".
func parentDirs(remotePath string) []string {
	var dirs []string
	for dir := path.Dir(remotePath); dir != "." && dir != "/"; dir = path.Dir(dir) {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}
