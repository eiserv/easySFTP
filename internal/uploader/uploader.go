// Package uploader implements the SFTP upload logic of easySFTP:
// connecting, planning uploads, syncing files and optional remote cleanup.
//
// The package is split by concern: planner.go builds the local transfer plan,
// transfer.go performs the uploads, retry.go wraps a single upload in the
// retry/reconnect loop, remote.go holds the remote-path and remote-directory
// helpers, connection.go dials the server (optionally through a jump host),
// hostkeys.go verifies host keys, session.go owns the live client pair and
// its reconnects, sync.go implements the sync strategy and its manifest, and
// stall.go the stall watchdog. This file ties them together: Run and the
// per-strategy dispatch.
package uploader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pkg/sftp"
	ignore "github.com/sabhiram/go-gitignore"

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

	// Targets breaks the totals above down per deployment, in plan order.
	Targets []TargetStats
}

// TargetStats summarizes what a run did (or would do) for a single
// deployment, so the job summary can break a deploy down per deployment.
type TargetStats struct {
	Name          string // deployment name from the config file; "" inline
	Local         string
	Remote        string
	Strategy      config.Strategy
	FilesUploaded int
	FilesDeleted  int
	FilesSkipped  int
	BytesUploaded int64
	Duration      time.Duration
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
		// verbose is nil unless log-level is debug; buildPlan then explains
		// every exclude decision.
		var verbose Logger
		if cfg.Debug() {
			verbose = log
		}
		p, err := buildPlan(pair, st, planOptions{
			matcher:      matcher,
			pruneDirs:    !hasNegation(lines),
			verbose:      verbose,
			manifestName: cfg.SyncManifestName(),
		})
		if err != nil {
			return stats, err
		}
		if p.skippedNonRegular > 0 {
			log.Warningf("skipped %d non-regular file(s) (symlinks, sockets, …) under %s: SFTP uploads regular files only",
				p.skippedNonRegular, pair.Local)
		}
		plans = append(plans, p)
	}

	sess, err := newSession(ctx, cfg, log)
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
		planStart := time.Now()
		err := executePlan(ctx, cfg, sess, p, stats, watch, log)
		// Recorded from the before/after delta (not threaded through
		// executePlan) so a target's partial progress on failure is
		// still captured, matching the totals' own partial-progress
		// behavior.
		ts := TargetStats{
			Name:          p.pair.Name,
			Local:         p.pair.Local,
			Remote:        p.pair.Remote,
			Strategy:      p.strategy,
			FilesUploaded: stats.FilesUploaded - before.FilesUploaded,
			FilesDeleted:  stats.FilesDeleted - before.FilesDeleted,
			FilesSkipped:  stats.FilesSkipped - before.FilesSkipped,
			BytesUploaded: stats.BytesUploaded - before.BytesUploaded,
			Duration:      time.Since(planStart),
		}
		stats.Targets = append(stats.Targets, ts)
		if err == nil {
			logDeploymentSummary(cfg, p.pair, ts, log)
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

// logDeploymentSummary logs the compact one-line result of a completed
// deployment, the core of the default (non-verbose) log output.
func logDeploymentSummary(cfg *config.Config, pair config.UploadPair, ts TargetStats, log Logger) {
	if cfg.DryRun {
		log.Infof("deployment %s: %d file(s) to upload (%s), %d to delete, %d unchanged (dry-run)",
			pair.Label(), ts.FilesUploaded, humanSize(ts.BytesUploaded), ts.FilesDeleted, ts.FilesSkipped)
		return
	}
	log.Infof("deployment %s: uploaded %d file(s) (%s), deleted %d, skipped %d unchanged, took %s",
		pair.Label(), ts.FilesUploaded, humanSize(ts.BytesUploaded), ts.FilesDeleted, ts.FilesSkipped,
		ts.Duration.Round(time.Millisecond))
}

// executePlan performs (or previews) one plan according to its strategy.
func executePlan(ctx context.Context, cfg *config.Config, sess *session, p plan, stats *Stats, watch *stallWatchdog, log Logger) error {
	header := fmt.Sprintf("%s => %s [%s] (%d local files)", p.pair.Local, p.pair.Remote, p.strategy, len(p.files))
	if p.pair.Name != "" {
		header = fmt.Sprintf("%s: %s", p.pair.Name, header)
	}
	log.Group(header)
	defer log.EndGroup()

	if cfg.SkipUnchanged && p.strategy != config.StrategyOverlay {
		log.Warningf("advanced.skip_unchanged only applies to the overlay mode; ignoring it for this %s deployment", p.strategy)
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

	if p.strategy == config.StrategyClean {
		base := normalizeRemote(p.pair.Remote)
		if err := checkRemoteRoot(p.pair.Remote); err != nil {
			return err
		}
		// The whole sweep runs through sess.do, so a connection drop mid-scan
		// or mid-delete redials (within the shared reconnect budget) instead
		// of failing the run; see issue #107. The scan is idempotent and
		// simply reruns on a fresh client.
		var files, dirs []string
		err := sess.do(ctx, watch, func(client *sftp.Client) error {
			var err error
			files, dirs, err = listRemoteContents(client, base, watch)
			return err
		})
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
				err := sess.do(ctx, watch, func(client *sftp.Client) error {
					// Already-gone counts as deleted: a retried delete may
					// have landed before the connection died.
					if err := client.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
					return nil
				})
				if err != nil {
					return fmt.Errorf("deleting %q: %w", f, err)
				}
			}
			stats.FilesDeleted++
		}
		// Remove the now-empty directories, deepest first. Best effort: a
		// directory that is not empty (e.g. an unreadable entry) is left be.
		for i := len(dirs) - 1; i >= 0; i-- {
			if !cfg.DryRun {
				dir := dirs[i]
				_ = sess.do(ctx, watch, func(client *sftp.Client) error {
					return client.RemoveDirectory(dir)
				})
			}
		}
	}

	skipUnchanged := cfg.SkipUnchanged && p.strategy == config.StrategyOverlay
	_, err := uploadFiles(ctx, cfg, sess, p.files, p.remoteDirs, stats, verb, watch, skipUnchanged, log)
	return err
}

// planVerb returns the log prefix that distinguishes a dry run from a real one.
func planVerb(cfg *config.Config) string {
	if cfg.DryRun {
		return "[dry-run] would "
	}
	return ""
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
