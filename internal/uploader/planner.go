package uploader

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	ignore "github.com/sabhiram/go-gitignore"
	"golang.org/x/sync/errgroup"

	"github.com/eiserv/easySFTP/internal/config"
)

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

// planOptions carries the ignore/walk-shaping inputs buildPlan needs beyond
// the pair and strategy it is planning: the matcher and its two derived knobs
// (pruneDirs, verbose) plus the effective manifest name. Grouping them keeps
// buildPlan's signature short and, in particular, takes the easy-to-transpose
// bool and nil-able Logger out of the positional argument list (they are named
// fields here instead).
//
//   - pruneDirs: when set, a directory that itself matches the ignore patterns
//     is skipped entirely instead of descended into, so an ignored
//     node_modules/ with hundreds of thousands of entries costs one match
//     instead of a full walk. Callers must clear it when the patterns contain
//     "!" re-includes, which could re-include files below an otherwise ignored
//     directory.
//   - verbose: when non-nil, gets one line per ignore decision (which pattern
//     excluded which file or directory), the detail needed to debug patterns.
//   - manifestName: the effective sync manifest file name; a local file by that
//     name is never uploaded, so a target's own manifest can't be clobbered.
type planOptions struct {
	matcher      *ignore.GitIgnore
	pruneDirs    bool
	verbose      Logger
	manifestName string
}

// buildPlan walks the local side of an upload pair and computes the remote
// file and directory layout, honoring the ignore patterns. It does not touch
// the network, so config/path errors surface before a connection is made.
// Content hashing for the sync strategy happens later, once connected: see
// executeSync.
func buildPlan(pair config.UploadPair, strategy config.Strategy, opts planOptions) (plan, error) {
	matcher, verbose := opts.matcher, opts.verbose
	p := plan{pair: pair, strategy: strategy}
	remoteBase := normalizeRemote(pair.Remote)

	info, err := os.Stat(pair.Local)
	if err != nil {
		return p, fmt.Errorf("local path %q: %w", pair.Local, err)
	}

	// sync and clean reconcile a directory tree; they are meaningless for a
	// single file and would delete the wrong things, so reject that up front.
	if !info.IsDir() && strategy != config.StrategyOverlay {
		return p, fmt.Errorf("mode %q requires a directory, but local path %q is a single file (use the overlay mode)", strategy, pair.Local)
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
			if opts.pruneDirs && rel != "." {
				if matched, pat := matcher.MatchesPathHow(rel + "/"); matched {
					if verbose != nil {
						verbose.Infof("skip %s/ and everything below it (ignore pattern %q)", rel, pat.Line)
					}
					return filepath.SkipDir
				}
			}
			return nil
		}
		if rel == opts.manifestName {
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

// effectiveStrategy resolves the strategy for a pair, defaulting to overlay for
// callers that construct a Config directly.
func effectiveStrategy(pair config.UploadPair) config.Strategy {
	if pair.Strategy != "" {
		return pair.Strategy
	}
	return config.StrategyOverlay
}
