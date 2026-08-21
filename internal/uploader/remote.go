package uploader

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/pkg/sftp"

	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/metrics"
)

// createRemoteDirs creates every remote directory the plan needs with as few
// SFTP round-trips as possible. It calls MkdirAll only on the deepest (leaf)
// directories: MkdirAll creates any missing parents in the same walk and
// treats an already-existing directory as success, so ancestors are never
// stat'd or created one level at a time. Only when a creation fails does it
// look closer, to report a path that already exists as a file clearly. Leaves
// and directory chmods run through the session's bounded worker pool, with one
// idempotent operation per sess.do retry scope.
func createRemoteDirs(ctx context.Context, sess *session, dirs []string, dirMode *fs.FileMode, watch *stallWatchdog, log Logger) error {
	leaves := leafDirs(dirs)
	err := runBounded(ctx, sess.workers(len(leaves)), len(leaves), func(groupCtx context.Context, i int) error {
		dir := leaves[i]
		return sess.do(groupCtx, watch, func(client *sftp.Client) error {
			done := metrics.Op("sftp_mkdirall")
			err := client.MkdirAll(dir)
			done(err)
			watch.tick()
			if err != nil {
				// Two leaves may share a missing parent. pkg/sftp's MkdirAll
				// normally resolves that race itself, but a connection drop
				// between its failed Mkdir and confirming Lstat can hide the
				// directory another worker just created behind the Mkdir error.
				// Confirm the final state before treating the operation as failed.
				info, statErr := client.Stat(dir)
				watch.tick()
				if statErr == nil && info.IsDir() {
					return nil
				}
				if isConnError(statErr) {
					return statErr
				}
				bad, conflictErr := nonDirConflict(client, dir, watch)
				if conflictErr != nil {
					return conflictErr
				}
				if bad != "" {
					return fmt.Errorf("remote path %q exists but is not a directory", bad)
				}
				return fmt.Errorf("creating remote directory %q: %w", dir, err)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}

	if dirMode != nil {
		var warned atomic.Bool
		_ = runBounded(ctx, sess.workers(len(dirs)), len(dirs), func(groupCtx context.Context, i int) error {
			dir := dirs[i]
			err := sess.do(groupCtx, watch, func(client *sftp.Client) error {
				done := metrics.Op("sftp_chmod_dir")
				err := client.Chmod(dir, dirMode.Perm())
				done(err)
				watch.tick()
				return err
			})
			if err != nil && !warned.Swap(true) {
				// Scoped to this pass (one per deployment), like the file-mode
				// and preserve-times warnings in transfer.go; see issue #121.
				log.Warningf("could not set dir-mode %04o on %s (server may reject SETSTAT); not warning again for this deployment: %v", dirMode.Perm(), dir, err)
			}
			return nil
		})
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
// clear message naming the offending path. Connection errors are returned so
// sess.do can retry the idempotent leaf operation.
func nonDirConflict(client *sftp.Client, dir string, watch *stallWatchdog) (string, error) {
	for _, d := range append(parentDirs(dir), dir) {
		info, err := client.Stat(d)
		watch.tick()
		if isConnError(err) {
			return "", err
		}
		if err == nil && !info.IsDir() {
			return d, nil
		}
	}
	return "", nil
}

// checkRemoteRoot refuses a destructive mode whose target resolves to the
// filesystem root or an unspecific path: the one guard that is always on.
func checkRemoteRoot(remote string) error {
	switch normalizeRemote(remote) {
	case "/", ".", "", "~":
		return fmt.Errorf("refusing a destructive mode on remote root %q; target a specific subdirectory instead", remote)
	}
	return nil
}

// checkMaxDeletes enforces the guards.max_deletes limit (0 means unlimited).
func checkMaxDeletes(n int, cfg *config.Config) error {
	if cfg.Safety.MaxDeletes > 0 && n > cfg.Safety.MaxDeletes {
		return fmt.Errorf("refusing to delete %d files: exceeds safety.max_deletes=%d (raise the limit in the config file, or run with dry-run to inspect the plan)", n, cfg.Safety.MaxDeletes)
	}
	return nil
}

// listRemoteContents returns every regular file and directory under root
// (root itself excluded), directories parents-first. Directories at the same
// depth are listed concurrently: their requests are independent, while the
// breadth-first boundary keeps a deterministic parent-before-child result.
// Each ReadDir has its own sess.do call, so a dropped connection retries only
// that idempotent listing instead of replaying the whole scan.
//
// The worker count is asked for per level rather than fixed for the scan: with
// advanced.concurrency at auto it is the width of the level being listed, and a
// tree whose levels are one directory wide never opens a worker it cannot use.
func listRemoteContents(ctx context.Context, sess *session, root string, watch *stallWatchdog) (files, dirs []string, err error) {
	level := []string{root}
	for len(level) > 0 {
		entries := make([][]fs.FileInfo, len(level))
		err := runBounded(ctx, sess.workers(len(level)), len(level), func(groupCtx context.Context, i int) error {
			dir := level[i]
			err := sess.do(groupCtx, watch, func(client *sftp.Client) error {
				done := metrics.Op("sftp_readdir")
				listed, err := client.ReadDir(dir)
				done(err)
				if err != nil {
					if os.IsNotExist(err) {
						return nil
					}
					return err
				}
				entries[i] = listed
				watch.tick()
				return nil
			})
			if err != nil {
				return fmt.Errorf("listing %q: %w", dir, err)
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}

		var next []string
		for i, dir := range level {
			for _, entry := range entries[i] {
				full := path.Join(dir, entry.Name())
				if entry.IsDir() {
					dirs = append(dirs, full)
					next = append(next, full)
					continue
				}
				files = append(files, full)
			}
		}
		sort.Strings(next)
		level = next
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
