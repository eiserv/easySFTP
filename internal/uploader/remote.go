package uploader

import (
	"context"
	"errors"
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

// remoteAbsent reports whether the server has nothing at p. It is the
// tie-breaker for an operation that failed without saying why.
//
// OpenSSH answers a missing path with SSH_FX_NO_SUCH_FILE, which pkg/sftp
// turns into os.ErrNotExist, so "it was not there" is normally visible in the
// error itself. Other implementations answer the generic SSH_FX_FAILURE,
// which on its own is indistinguishable from a refusal, and easySFTP has two
// places where reading the one as the other ends the run: listing a clean
// deployment's target before it exists, and deleting a file that is already
// gone (issue #152). A stat settles it, and only on the error path, so a
// server that does say SSH_FX_NO_SUCH_FILE never pays the round-trip.
//
// Lstat, not Stat: a dangling symlink is still an entry, and a delete that
// reported it gone would leave it behind.
//
// The stat's own answer is read the same way round. A connection-class
// failure is returned as an error, so sess.do redials and reruns the
// idempotent operation rather than reading a dead connection as an empty
// server. SSH_FX_PERMISSION_DENIED counts as present: a server with a
// specific code for a refusal that chose to use it is being specific, not
// coarse, and the caller's original error is the one worth reporting.
// Everything else counts as absent, which is the safe direction for both
// callers, since a listing that finds nothing deletes nothing.
func remoteAbsent(client *sftp.Client, p string, watch *stallWatchdog) (bool, error) {
	_, err := client.Lstat(p)
	watch.tick()
	switch {
	case err == nil:
		return false, nil
	case isConnError(err):
		return false, err
	case errors.Is(err, os.ErrPermission):
		return false, nil
	default:
		return true, nil
	}
}

// checkRemoteRoot refuses a destructive mode whose target resolves to the
// filesystem root or an unspecific path: the one guard that is always on.
//
// It also refuses a relative target that escapes upward. path.Clean("..") is
// "..", which is not the root and used to pass, so "mode: clean" with a target
// of ".." resolved against the SFTP session's working directory (the login
// user's home on nearly every server) and deleted what it found there. That is
// reachable by accident rather than only by malice: a target built from an
// expression, a variable that resolves to the empty string, or a shell that
// stripped the last path component all land on exactly these values, which is
// the class of mistake this guard exists to catch (issue #222).
//
// A relative target that stays put ("www/public_html") is still allowed. It is
// the documented behaviour and a great many workflows use it.
func checkRemoteRoot(remote string) error {
	normalized := normalizeRemote(remote)
	switch {
	case normalized == "/", normalized == ".", normalized == "", normalized == "~":
		return fmt.Errorf("refusing a destructive mode on remote root %q; target a specific subdirectory instead", remote)
	case normalized == "..", strings.HasPrefix(normalized, "../"):
		return fmt.Errorf("refusing a destructive mode on remote root %q: it resolves to %q, above the directory the session starts in; target a specific subdirectory instead", remote, normalized)
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
func listRemoteContents(ctx context.Context, sess *session, root string, watch *stallWatchdog, log Logger) (files, dirs []string, err error) {
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
					// A directory that is not there has nothing to list: a
					// clean deployment whose target does not exist yet, or a
					// subdirectory removed between its parent's listing and
					// this one. Servers that do not say so in the status code
					// are asked directly; see remoteAbsent.
					if os.IsNotExist(err) {
						return nil
					}
					absent, aerr := remoteAbsent(client, dir, watch)
					if aerr != nil {
						return aerr
					}
					if absent {
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
				// The name comes from the server. Everything collected here
				// is deleted a moment later, so an entry that does not name
				// something inside the directory it was listed from is
				// dropped rather than followed; see safeJoin and issue #223.
				full, err := safeChild(dir, entry.Name())
				if err != nil {
					log.Warningf("skipping remote entry while scanning %s: %v", dir, err)
					continue
				}
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

// safeJoin joins a remote-supplied relative path onto base and refuses
// anything whose result would not stay strictly under it.
//
// path.Join cleans, it does not confine: path.Join("/var/www", "../../etc")
// is "/etc". Three inputs reach this package from the server rather than from
// the configuration, and all three end up as arguments to Remove or
// RemoveDirectory: the keys of the sync manifest (a file that lives in the
// deploy target, so anything else able to write there can choose them), and
// the entry names a server returns from ReadDir during the clean-mode scan and
// the stale-temp sweep. Whoever controls one of those should not thereby
// control where this run deletes; see issue #223.
//
// The three are not equally exposed. The manifest is the one an attacker needs
// no server access for, and its keys arrive exactly as written. Listing names
// pass through pkg/sftp's client first, which drops a literal "." or ".." and
// reduces everything else to its last component, so the only escape left on
// that path is a name whose base climbs ("sub/.." arrives as ".."). That is
// still an escape, and the guard is the same one, so both go through it.
//
// Rejecting a leading ".." after path.Clean is enough on its own (Clean
// resolves every interior "..", so a cleaned relative path that does not start
// with ".." cannot escape), but the containment check is kept as the property
// the caller actually cares about, stated where a reader can see it.
func safeJoin(base, rel string) (string, error) {
	if rel == "" || rel == "." {
		return "", fmt.Errorf("refusing remote-supplied path %q: it names no file under %q", rel, base)
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("refusing remote-supplied path %q: it is absolute, not relative to %q", rel, base)
	}
	cleaned := path.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("refusing remote-supplied path %q: it resolves to %q, outside %q", rel, path.Join(base, cleaned), base)
	}
	full := path.Join(base, cleaned)
	if !within(base, full) {
		return "", fmt.Errorf("refusing remote-supplied path %q: it resolves to %q, outside %q", rel, full, base)
	}
	return full, nil
}

// safeChild is safeJoin for a single entry name from a directory listing,
// where a path separator is already wrong: ReadDir returns names, not paths,
// so a server that answers with one is not describing its own directory.
func safeChild(dir, name string) (string, error) {
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("refusing remote-supplied directory entry %q in %q: an entry name cannot contain %q", name, dir, "/")
	}
	return safeJoin(dir, name)
}

// within reports whether full is strictly below base, both as slash paths.
func within(base, full string) bool {
	b := path.Clean(base)
	if b == "." {
		// A relative base resolved against the session's working directory.
		// Everything that is itself relative and does not climb is under it.
		return !strings.HasPrefix(full, "/") && full != ".." && !strings.HasPrefix(full, "../")
	}
	if full == b {
		return false
	}
	return strings.HasPrefix(full, strings.TrimSuffix(b, "/")+"/")
}
