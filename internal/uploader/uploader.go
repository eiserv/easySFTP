// Package uploader implements the SFTP upload logic of easySFTP:
// connecting, planning uploads, syncing files and optional remote cleanup.
package uploader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	ignore "github.com/sabhiram/go-gitignore"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/sync/errgroup"

	"github.com/eiserv/easySFTP/internal/config"
)

// Logger is the minimal logging interface the uploader needs.
type Logger interface {
	Infof(format string, args ...any)
	Warningf(format string, args ...any)
	Group(name string)
	EndGroup()
}

// Stats summarizes what a run did (or, in dry-run mode, would do).
type Stats struct {
	FilesUploaded int
	FilesDeleted  int
	FilesSkipped  int // unchanged files skipped (sync, or overlay with skip-unchanged)
	BytesUploaded int64
	Duration      time.Duration

	// Targets breaks the totals above down per upload pair. It is only
	// populated when the config has more than one pair: a single-target run
	// has nothing to break down, so it stays nil and callers can use that to
	// decide whether a per-target table is worth showing.
	Targets []TargetStats
}

// TargetStats summarizes what a run did (or would do) for a single upload
// pair, so a multi-target deploy can be broken down in the job summary.
type TargetStats struct {
	Local         string
	Remote        string
	Strategy      config.Strategy
	FilesUploaded int
	FilesDeleted  int
	FilesSkipped  int
	BytesUploaded int64
}

// fileItem is a single planned file transfer.
type fileItem struct {
	localPath  string // absolute or workspace-relative OS path
	remotePath string // slash-separated remote path
	rel        string // slash path relative to the remote base (manifest key)
	size       int64
	mtime      int64 // local modification time, unix seconds
	mode       fs.FileMode
	hash       string // sha256 hex of the local content; only set for the sync strategy
}

// plan is the complete set of transfers for one upload pair.
type plan struct {
	pair              config.UploadPair
	strategy          config.Strategy
	files             []fileItem
	remoteDirs        []string // directories to create, sorted parents-first
	skippedNonRegular int      // symlinks, sockets, etc. skipped during the walk
}

// Run executes the configured upload and returns transfer statistics.
func Run(ctx context.Context, cfg *config.Config, log Logger) (*Stats, error) {
	start := time.Now()
	stats := &Stats{}
	defer func() { stats.Duration = time.Since(start) }()

	// Build the full local plan first so config/path errors surface before
	// we touch the network.
	plans := make([]plan, 0, len(cfg.Uploads))
	for _, pair := range cfg.Uploads {
		st := effectiveStrategy(pair)
		lines := append(append([]string{}, cfg.IgnoreLines...), pair.Ignore...)
		matcher := ignore.CompileIgnoreLines(lines...)
		// verbose is nil unless log-level is verbose; buildPlan then explains
		// every ignore decision.
		var verbose Logger
		if cfg.Verbose() {
			verbose = log
		}
		p, err := buildPlan(pair, st, matcher, !hasNegation(lines), verbose, cfg.SyncManifestName())
		if err != nil {
			return stats, err
		}
		if p.skippedNonRegular > 0 {
			log.Warningf("skipped %d non-regular file(s) (symlinks, sockets, …) under %s: SFTP uploads regular files only",
				p.skippedNonRegular, pair.Local)
		}
		plans = append(plans, p)
	}

	sess, err := newSession(cfg, log)
	if err != nil {
		return stats, err
	}
	defer sess.close()

	keepaliveCtx, stopKeepalives := context.WithCancel(ctx)
	defer stopKeepalives()
	go sendKeepalives(keepaliveCtx, sess.currentSSH, keepaliveInterval)

	var watch *stallWatchdog
	if cfg.StallTimeout > 0 {
		watch = startStallWatchdog(cfg.StallTimeout, func() { sess.currentSSH().Close() }, log)
		defer watch.stop()
	}

	for _, p := range plans {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		before := *stats
		err := executePlan(ctx, cfg, sess, p, stats, watch, log)
		if len(plans) > 1 {
			// Recorded from the before/after delta (not threaded through
			// executePlan) so a target's partial progress on failure is
			// still captured, matching the totals' own partial-progress
			// behavior.
			stats.Targets = append(stats.Targets, TargetStats{
				Local:         p.pair.Local,
				Remote:        p.pair.Remote,
				Strategy:      p.strategy,
				FilesUploaded: stats.FilesUploaded - before.FilesUploaded,
				FilesDeleted:  stats.FilesDeleted - before.FilesDeleted,
				FilesSkipped:  stats.FilesSkipped - before.FilesSkipped,
				BytesUploaded: stats.BytesUploaded - before.BytesUploaded,
			})
		}
		if err != nil {
			if watch != nil && watch.fired.Load() {
				return stats, fmt.Errorf("transfer stalled: no progress for %s, connection closed (stall-timeout): %w", cfg.StallTimeout, err)
			}
			return stats, err
		}
	}

	return stats, nil
}

// hasNegation reports whether any ignore line is a "!" re-include. Directory
// pruning is disabled in that case: a pruned directory can never have files
// below it re-included, so pruning only runs when no pattern could do that.
func hasNegation(lines []string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, "!") {
			return true
		}
	}
	return false
}

// buildPlan walks the local side of an upload pair and computes the remote
// file and directory layout, honoring the ignore patterns. It does not touch
// the network, so config/path errors surface before a connection is made.
// Content hashing for the sync strategy happens later, once connected: see
// executeSync.
//
// With pruneDirs set, a directory that itself matches the ignore patterns is
// skipped entirely instead of descended into, so an ignored node_modules/ with
// hundreds of thousands of entries costs one match instead of a full walk.
// Callers must clear it when the patterns contain "!" re-includes, which could
// re-include files below an otherwise ignored directory.
//
// verbose, when non-nil, gets one line per ignore decision (which pattern
// excluded which file or directory), the level of detail needed to debug
// ignore patterns.
//
// manifestName is the effective sync manifest file name; a local file by that
// name is never uploaded, so a target's own manifest can't be clobbered.
func buildPlan(pair config.UploadPair, strategy config.Strategy, matcher *ignore.GitIgnore, pruneDirs bool, verbose Logger, manifestName string) (plan, error) {
	p := plan{pair: pair, strategy: strategy}
	remoteBase := normalizeRemote(pair.Remote)

	info, err := os.Stat(pair.Local)
	if err != nil {
		return p, fmt.Errorf("local path %q: %w", pair.Local, err)
	}

	// sync and clean reconcile a directory tree; they are meaningless for a
	// single file and would delete the wrong things, so reject that up front.
	if !info.IsDir() && strategy != config.StrategyOverlay {
		return p, fmt.Errorf("strategy %q requires a directory, but local path %q is a single file (use the overlay strategy)", strategy, pair.Local)
	}

	// A single file maps directly onto the remote path. A trailing slash on
	// the remote side means "into this directory".
	if !info.IsDir() {
		remotePath := remoteBase
		if strings.HasSuffix(pair.Remote, "/") || remoteBase == "." {
			remotePath = path.Join(remoteBase, filepath.Base(pair.Local))
		}
		if matched, pat := matcher.MatchesPathHow(filepath.Base(pair.Local)); matched {
			if verbose != nil {
				verbose.Infof("skip %s (ignore pattern %q)", filepath.Base(pair.Local), pat.Line)
			}
			return p, nil
		}
		p.files = append(p.files, fileItem{
			localPath:  pair.Local,
			remotePath: remotePath,
			rel:        filepath.Base(pair.Local),
			size:       info.Size(),
			mtime:      info.ModTime().Unix(),
			mode:       info.Mode(),
		})
		p.remoteDirs = parentDirs(remotePath)
		return p, nil
	}

	err = filepath.WalkDir(pair.Local, func(fpath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(pair.Local, fpath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// The trailing slash lets directory-only patterns ("dist/") match.
			if pruneDirs && rel != "." {
				if matched, pat := matcher.MatchesPathHow(rel + "/"); matched {
					if verbose != nil {
						verbose.Infof("skip %s/ and everything below it (ignore pattern %q)", rel, pat.Line)
					}
					return filepath.SkipDir
				}
			}
			return nil
		}
		if rel == manifestName {
			return nil
		}
		if matched, pat := matcher.MatchesPathHow(rel); matched {
			if verbose != nil {
				verbose.Infof("skip %s (ignore pattern %q)", rel, pat.Line)
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			// Symlinks, sockets etc. are skipped; SFTP uploads regular files.
			p.skippedNonRegular++
			return nil
		}
		item := fileItem{
			localPath:  fpath,
			remotePath: path.Join(remoteBase, rel),
			rel:        rel,
			size:       fi.Size(),
			mtime:      fi.ModTime().Unix(),
			mode:       fi.Mode(),
		}
		p.files = append(p.files, item)
		return nil
	})
	if err != nil {
		return p, fmt.Errorf("walking local path %q: %w", pair.Local, err)
	}

	p.remoteDirs = dirsForFiles(p.files)
	return p, nil
}

// dirsForFiles returns the set of ancestor directories of the given files,
// sorted so parents come before their children (creation order is safe).
func dirsForFiles(files []fileItem) []string {
	dirSet := map[string]struct{}{}
	for _, f := range files {
		for _, dir := range parentDirs(f.remotePath) {
			dirSet[dir] = struct{}{}
		}
	}
	dirs := make([]string, 0, len(dirSet))
	for dir := range dirSet {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// hashPlanFiles fills in each file's sha256 content hash, hashing through a
// worker pool bounded to concurrency workers so a large tree uses the runner's
// CPU instead of being read and hashed one file at a time. It is used only by
// the sync strategy, whose changed-file detection compares content hashes.
//
// cached is the previous sync's manifest entries, keyed by relative path. When
// a file's size and mtime still match its cached entry, its hash is reused
// instead of re-reading the file (the same fast path rsync's "quick check"
// uses). A cached entry with mtime 0 (a manifest written before this fast
// path existed) never matches, so upgrading from an older manifest costs one
// full re-hash and nothing more.
func hashPlanFiles(ctx context.Context, files []fileItem, concurrency int, cached map[string]manifestEntry) error {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i := range files {
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry, ok := cached[files[i].rel]; ok && entry.MTime != 0 &&
				entry.Size == files[i].size && entry.MTime == files[i].mtime {
				files[i].hash = entry.Hash
				return nil
			}
			hash, err := hashFile(files[i].localPath)
			if err != nil {
				return err
			}
			files[i].hash = hash
			return nil
		})
	}
	return g.Wait()
}

// executePlan performs (or previews) one plan according to its strategy.
func executePlan(ctx context.Context, cfg *config.Config, sess *session, p plan, stats *Stats, watch *stallWatchdog, log Logger) error {
	log.Group(fmt.Sprintf("%s => %s [%s] (%d local files)", p.pair.Local, p.pair.Remote, p.strategy, len(p.files)))
	defer log.EndGroup()

	if cfg.SkipUnchanged && p.strategy != config.StrategyOverlay {
		log.Warningf("skip-unchanged only applies to the overlay strategy; ignoring it for this %s target", p.strategy)
	}

	if p.strategy == config.StrategySync {
		return executeSync(ctx, cfg, sess, p, stats, watch, log)
	}
	return executeOverlayOrClean(ctx, cfg, sess, p, stats, watch, log)
}

// executeOverlayOrClean uploads the plan, first wiping the remote target when
// the strategy is clean.
func executeOverlayOrClean(ctx context.Context, cfg *config.Config, sess *session, p plan, stats *Stats, watch *stallWatchdog, log Logger) error {
	verb := planVerb(cfg)
	// Non-upload operations (the clean sweep below) use a client snapshot;
	// only the per-file upload path is reconnect-aware.
	client, _ := sess.current()

	if p.strategy == config.StrategyClean {
		base := normalizeRemote(p.pair.Remote)
		if err := checkRemoteRoot(p.pair.Remote); err != nil {
			return err
		}
		files, dirs, err := listRemoteContents(client, base)
		if err != nil {
			return fmt.Errorf("scanning remote directory %q: %w", p.pair.Remote, err)
		}
		if err := checkMaxDeletes(len(files), cfg); err != nil {
			return err
		}
		for _, f := range files {
			if cfg.LogPerFile() {
				log.Infof("%sdelete %s", verb, f)
			}
			if !cfg.DryRun {
				if err := client.Remove(f); err != nil {
					return fmt.Errorf("deleting %q: %w", f, err)
				}
			}
			stats.FilesDeleted++
		}
		// Remove the now-empty directories, deepest first. Best effort: a
		// directory that is not empty (e.g. an unreadable entry) is left be.
		for i := len(dirs) - 1; i >= 0; i-- {
			if !cfg.DryRun {
				_ = client.RemoveDirectory(dirs[i])
			}
		}
	}

	skipUnchanged := cfg.SkipUnchanged && p.strategy == config.StrategyOverlay
	_, err := uploadFiles(ctx, cfg, sess, p.files, p.remoteDirs, stats, verb, watch, skipUnchanged, log)
	return err
}

// uploadFiles creates the needed remote directories and uploads files in
// parallel (or, in dry-run mode, only logs what it would do). With
// skipUnchanged set, a file whose remote counterpart already exists with the
// same size is skipped instead of uploaded; the stat happens inside the
// parallel workers so its latency is amortized by the concurrency.
//
// It returns which files completed, indexed like files, so that on a partial
// failure the caller knows what actually made it to the server (the sync
// strategy uses this to persist a recovery manifest).
func uploadFiles(ctx context.Context, cfg *config.Config, sess *session, files []fileItem, dirs []string, stats *Stats, verb string, watch *stallWatchdog, skipUnchanged bool, log Logger) ([]bool, error) {
	// Declared before the first failure point: callers index the returned
	// slice by file, so it must be sized even when nothing was uploaded.
	completed := make([]bool, len(files))
	skipped := make([]bool, len(files))

	if !cfg.DryRun {
		client, _ := sess.current()
		if err := createRemoteDirs(client, dirs, cfg.DirMode, log); err != nil {
			return completed, err
		}
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(cfg.Concurrency)
	results := make([]int64, len(files))
	// modeWarned is only armed when file-mode is an explicit override: a
	// mirrored local mode (the default) stays silent on failure, as before.
	var modeWarned *atomic.Bool
	if cfg.FileMode != nil {
		modeWarned = new(atomic.Bool)
	}
	// timesWarned doubles as the preserve-times switch: nil means off. The
	// user explicitly asked for preserved times, so a refusing server warns
	// (once per run); staying silent would defeat the point of the input.
	var timesWarned *atomic.Bool
	if cfg.PreserveTimes {
		timesWarned = new(atomic.Bool)
	}

	for i, f := range files {
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			// The stat is read-only, so it also runs in dry-run mode: the
			// preview then reports the same skips the real run would.
			if skipUnchanged {
				client, _ := sess.current()
				if fi, err := client.Stat(f.remotePath); err == nil && fi.Mode().IsRegular() && fi.Size() == f.size {
					if cfg.LogPerFile() {
						log.Infof("%sskip %s (remote file has the same size)", verb, f.remotePath)
					}
					skipped[i] = true
					return nil
				}
			}
			if cfg.LogPerFile() {
				log.Infof("%supload %s => %s (%s)", verb, f.localPath, f.remotePath, humanSize(f.size))
			}
			if cfg.DryRun {
				// Report the planned byte count so bytes-uploaded matches the
				// "planned counts" contract of the other dry-run outputs.
				results[i] = f.size
				completed[i] = true
				return nil
			}
			mode := f.mode.Perm()
			if cfg.FileMode != nil {
				mode = *cfg.FileMode
			}
			n, err := uploadFileWithRetry(ctx, f, i, mode, sess, cfg.Retries, watch, log, modeWarned, timesWarned)
			if err != nil {
				return err
			}
			results[i] = n
			completed[i] = true
			return nil
		})
	}
	runErr := g.Wait()
	for i, n := range results {
		switch {
		case skipped[i]:
			stats.FilesSkipped++
		case completed[i]:
			stats.FilesUploaded++
			stats.BytesUploaded += n
		}
	}
	return completed, runErr
}

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

// planVerb returns the log prefix that distinguishes a dry run from a real one.
func planVerb(cfg *config.Config) string {
	if cfg.DryRun {
		return "[dry-run] would "
	}
	return ""
}

// effectiveStrategy resolves the strategy for a pair, defaulting to overlay for
// callers that construct a Config directly.
func effectiveStrategy(pair config.UploadPair) config.Strategy {
	if pair.Strategy != "" {
		return pair.Strategy
	}
	return config.StrategyOverlay
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

// tmpSuffix is appended to the remote path while a file is still uploading.
// It keeps the temp file in the same directory as its target so the final
// rename stays on one filesystem and is atomic.
const tmpSuffix = ".easysftp-tmp"

// uploadFileWithRetry uploads one file, retrying transient failures with
// exponential backoff. It stops early when the context is cancelled or the
// error is permanent (see isRetryable), so a doomed transfer fails fast.
// When a failure looks connection-class, the session is asked to reconnect
// first, so the retry runs against a live client instead of the dead one.
//
// index is the file's position in the plan and is folded into the temp
// path (see uploadFile) so two planned transfers never race over the same
// temporary name, even if one target's path happens to literally be
// another's plus tmpSuffix.
func uploadFileWithRetry(ctx context.Context, f fileItem, index int, mode fs.FileMode, sess *session, retries int, watch *stallWatchdog, log Logger, modeWarned, timesWarned *atomic.Bool) (int64, error) {
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			log.Warningf("retrying upload of %s in %s (attempt %d/%d): %v", f.localPath, backoff, attempt+1, retries+1, lastErr)
			if err := sleepCtx(ctx, backoff); err != nil {
				return 0, err
			}
		}
		client, gen := sess.current()
		if attempt > 0 {
			// A previous attempt may have left its temp file behind (a dead
			// connection cannot run the normal cleanup). Clear it so the
			// fresh attempt starts from a clean slate; harmless when absent.
			_ = client.Remove(fmt.Sprintf("%s%s.%d", f.remotePath, tmpSuffix, index))
		}
		n, err := uploadFile(ctx, f, index, mode, client, watch, log, modeWarned, timesWarned)
		if err == nil {
			return n, nil
		}
		lastErr = err
		if !isRetryable(err) {
			break
		}
		// The watchdog closed the connection because the server stopped
		// making progress. That reads as a connection drop, but redialing
		// would just stall again with the watchdog already spent, so fail
		// fast instead: this is exactly what stall-timeout is for.
		if watch != nil && watch.fired.Load() {
			break
		}
		if isConnError(err) && attempt < retries {
			if _, rerr := sess.reconnect(ctx, gen); rerr != nil {
				return 0, fmt.Errorf("uploading %q to %q: %w (%v)", f.localPath, f.remotePath, lastErr, rerr)
			}
		}
	}
	return 0, fmt.Errorf("uploading %q to %q: %w", f.localPath, f.remotePath, lastErr)
}

// uploadFile atomically uploads one file: it streams the content into a
// temporary sibling and, only once that fully succeeds, renames it over the
// target. Any failure removes the temporary file so a broken or partial upload
// never replaces the live file and no debris is left behind.
func uploadFile(ctx context.Context, f fileItem, index int, mode fs.FileMode, client *sftp.Client, watch *stallWatchdog, log Logger, modeWarned, timesWarned *atomic.Bool) (int64, error) {
	// Active per attempt (not around the whole retry loop) so retry backoff
	// sleeps do not count as transfer silence.
	if watch != nil {
		watch.begin()
		defer watch.end()
	}

	src, err := os.Open(f.localPath)
	if err != nil {
		return 0, err
	}
	defer src.Close()

	// The index makes the temp path unique per planned transfer, so it can't
	// collide with another planned file whose real name is this one's plus
	// tmpSuffix (see issue #42).
	tmpPath := fmt.Sprintf("%s%s.%d", f.remotePath, tmpSuffix, index)
	dst, err := client.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return 0, err
	}

	// ctxReader aborts the copy promptly when the deployment is cancelled,
	// instead of streaming a whole large file first.
	var reader io.Reader = &ctxReader{ctx: ctx, r: src}
	if watch != nil {
		reader = watch.reader(reader)
	}
	n, err := io.Copy(dst, reader)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		cleanupTmp(client, tmpPath, log)
		return n, err
	}

	// Best effort: mirrors the local permission bits, or the file-mode
	// override when set. Some servers reject SETSTAT; an explicit override
	// warns once per run so the user knows it isn't taking effect, but a
	// mirrored local mode stays silent as before.
	if cerr := client.Chmod(tmpPath, mode); cerr != nil && modeWarned != nil && !modeWarned.Swap(true) {
		log.Warningf("could not set file-mode %04o on %s (server may reject SETSTAT); not warning again this run: %v", mode, f.remotePath, cerr)
	}

	if err := renameReplace(client, tmpPath, f.remotePath); err != nil {
		cleanupTmp(client, tmpPath, log)
		return n, fmt.Errorf("replacing %q: %w", f.remotePath, err)
	}

	// preserve-times (timesWarned non-nil): keep the local modification time
	// instead of "now". After the rename, so the request targets the final
	// path; a failure warns once per run and never fails the deploy.
	if timesWarned != nil {
		mtime := time.Unix(f.mtime, 0)
		if cerr := client.Chtimes(f.remotePath, mtime, mtime); cerr != nil && !timesWarned.Swap(true) {
			log.Warningf("could not preserve the modification time on %s (server may reject SETSTAT); not warning again this run: %v", f.remotePath, cerr)
		}
	}
	return n, nil
}

// renameReplace atomically moves tmp onto final. It prefers the
// posix-rename@openssh.com extension (a true atomic overwrite) and falls back
// to a plain remove+rename for servers that do not support it.
func renameReplace(client *sftp.Client, tmp, final string) error {
	err := client.PosixRename(tmp, final)
	if err == nil {
		return nil
	}
	var se *sftp.StatusError
	if !errors.As(err, &se) || se.FxCode() != sftp.ErrSSHFxOpUnsupported {
		return err
	}
	// note: non-atomic fallback, a brief window where final is missing.
	// Only reached on servers lacking posix-rename; unavoidable there.
	_ = client.Remove(final)
	return client.Rename(tmp, final)
}

// cleanupTmp best-effort removes a leftover temp file, warning (but not
// failing) if the server refuses, so an orphan is at least visible in the log.
func cleanupTmp(client *sftp.Client, tmpPath string, log Logger) {
	if err := client.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warningf("could not remove temporary file %s: %v", tmpPath, err)
	}
}

// isRetryable reports whether an error is worth another attempt. Permanent
// failures (bad permissions, missing paths) and a cancelled/expired context
// are not retried; transient ones (dropped connections, timeouts, EOF) are.
func isRetryable(err error) bool {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, os.ErrPermission), errors.Is(err, os.ErrNotExist):
		return false
	default:
		return true
	}
}

// sleepCtx waits for d, returning early with the context error if the
// deployment is cancelled meanwhile.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ctxReader makes an io.Copy abort as soon as the context is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// keepaliveInterval is how often sendKeepalives pings the connection. It is
// deliberately not configurable: it's cheap, harmless to send more often than
// strictly needed, and there's no evidence yet that any user needs a
// different value.
const keepaliveInterval = 30 * time.Second

// sendKeepalives periodically sends an SSH keepalive request until ctx is
// canceled. This keeps long or idle-looking transfers alive across NAT
// gateways and firewalls that drop idle TCP connections, and answers sshd's
// own ClientAliveInterval probes so the server doesn't disconnect us first.
// client is a getter (not a fixed *ssh.Client) so one loop follows the
// session across reconnects; interval is a parameter so tests can drive it
// with a short tick instead of waiting 30s.
func sendKeepalives(ctx context.Context, client func() *ssh.Client, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _, _ = client().SendRequest("keepalive@openssh.com", true, nil)
		}
	}
}

// connect dials the server and opens an SFTP session on top of SSH.
func connect(cfg *config.Config, log Logger) (*ssh.Client, *sftp.Client, error) {
	auth, err := authMethods(cfg)
	if err != nil {
		return nil, nil, err
	}

	hostKeyCallback, err := hostKeyCallback(cfg, log)
	if err != nil {
		return nil, nil, err
	}

	sshConfig := &ssh.ClientConfig{
		User:            cfg.Username,
		Auth:            auth,
		HostKeyCallback: hostKeyCallback,
		Timeout:         cfg.Timeout,
	}

	addr := net.JoinHostPort(cfg.Server, fmt.Sprintf("%d", cfg.Port))
	log.Infof("connecting to %s as %s ...", addr, cfg.Username)
	sshClient, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to %s: %w", addr, err)
	}

	sftpClient, err := sftp.NewClient(sshClient,
		sftp.UseConcurrentWrites(true),
		sftp.MaxConcurrentRequestsPerFile(cfg.SftpRequestConcurrency),
	)
	if err != nil {
		sshClient.Close()
		return nil, nil, fmt.Errorf("opening SFTP session: %w", err)
	}
	return sshClient, sftpClient, nil
}

func authMethods(cfg *config.Config) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if key := strings.TrimSpace(cfg.PrivateKey); key != "" {
		var signer ssh.Signer
		var err error
		if cfg.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key+"\n"), []byte(cfg.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(key + "\n"))
		}
		if err != nil {
			return nil, fmt.Errorf("parsing private key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
	}
	return methods, nil
}

func hostKeyCallback(cfg *config.Config, log Logger) (ssh.HostKeyCallback, error) {
	want := cfg.HostKeyFingerprints
	if len(want) == 0 && cfg.KnownHosts == "" {
		log.Warningf("no host-key-fingerprint or known-hosts configured; the server's identity will NOT be verified. " +
			"Run 'ssh-keyscan <server>' and set the known-hosts input (or convert with 'ssh-keygen -lf -' " +
			"and set host-key-fingerprint) to pin it.")
		return ssh.InsecureIgnoreHostKey(), nil
	}
	for _, fp := range want {
		if !strings.HasPrefix(fp, "SHA256:") {
			return nil, fmt.Errorf("host-key-fingerprint must be a SHA256 fingerprint like 'SHA256:...', got %q", fp)
		}
	}
	var khCallback ssh.HostKeyCallback
	if cfg.KnownHosts != "" {
		var err error
		if khCallback, err = knownHostsCallback(cfg.KnownHosts); err != nil {
			return nil, err
		}
	}
	// A key matching either input is accepted, mirroring how multiple
	// fingerprints already OR together: users can pin every key their server
	// presents, in whichever format they have at hand.
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		got := ssh.FingerprintSHA256(key)
		for _, fp := range want {
			if got == fp {
				return nil
			}
		}
		if khCallback != nil && khCallback(hostname, remote, key) == nil {
			return nil
		}
		accepted := append([]string{}, want...)
		if khCallback != nil {
			accepted = append(accepted, "the known-hosts entries")
		}
		return fmt.Errorf("host key mismatch for %s: got %s, want one of: %s", hostname, got, strings.Join(accepted, ", "))
	}, nil
}

// knownHostsCallback builds a host key verifier from raw OpenSSH known_hosts
// lines (e.g. ssh-keyscan output). x/crypto's knownhosts parser only reads
// files, so the lines are staged in a temp file that is removed again right
// after parsing.
func knownHostsCallback(data string) (ssh.HostKeyCallback, error) {
	f, err := os.CreateTemp("", "easysftp-known-hosts-*")
	if err != nil {
		return nil, fmt.Errorf("staging known-hosts: %w", err)
	}
	defer os.Remove(f.Name())
	_, werr := f.WriteString(data + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, fmt.Errorf("staging known-hosts: %w", werr)
	}
	cb, err := knownhosts.New(f.Name())
	if err != nil {
		return nil, fmt.Errorf("parsing known-hosts: %w", err)
	}
	return cb, nil
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

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
