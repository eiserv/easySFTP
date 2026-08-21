package uploader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/pkg/sftp"

	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/metrics"
)

// deleteRemoteFiles removes paths through a bounded worker pool. The returned
// slice is aligned with paths and records every delete that completed, even
// when another worker failed. Sync uses that exact accounting for its recovery
// manifest, and clean uses it for partial-progress statistics.
func deleteRemoteFiles(ctx context.Context, cfg *config.Config, sess *session, paths []string, watch *stallWatchdog, log Logger) ([]bool, error) {
	deleted := make([]bool, len(paths))
	if cfg.DryRun {
		for i, remotePath := range paths {
			if cfg.LogPerFile() {
				log.Infof("%sdelete %s", planVerb(cfg), remotePath)
			}
			deleted[i] = true
		}
		return deleted, nil
	}

	err := runBounded(ctx, cfg.Concurrency, len(paths), func(groupCtx context.Context, i int) error {
		remotePath := paths[i]
		if cfg.LogPerFile() {
			log.Infof("delete %s", remotePath)
		}
		err := sess.do(groupCtx, watch, func(client *sftp.Client) error {
			// Already-gone counts as deleted: a retried delete may have
			// landed before the connection died.
			done := metrics.Op("sftp_remove")
			err := client.Remove(remotePath)
			done(err)
			watch.tick()
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("deleting %q: %w", remotePath, err)
		}
		deleted[i] = true
		return nil
	})
	return deleted, err
}

func countDeleted(deleted []bool) int {
	n := 0
	for _, ok := range deleted {
		if ok {
			n++
		}
	}
	return n
}

// removeRemoteDirs removes independent directories at the same depth in
// parallel, while keeping the real child-before-parent dependency between
// depths. Directory removal is best effort, matching the previous behavior:
// a non-empty or unreadable directory is left in place.
func removeRemoteDirs(ctx context.Context, cfg *config.Config, sess *session, watch *stallWatchdog, dirs []string) {
	if cfg.DryRun || len(dirs) == 0 {
		return
	}
	byDepth := make(map[int][]string)
	var depths []int
	for _, dir := range dirs {
		depth := strings.Count(path.Clean(dir), "/")
		if _, ok := byDepth[depth]; !ok {
			depths = append(depths, depth)
		}
		byDepth[depth] = append(byDepth[depth], dir)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(depths)))
	workers := cfg.Concurrency
	if workers < 1 {
		workers = 1
	}
	for _, depth := range depths {
		level := byDepth[depth]
		_ = runBounded(ctx, workers, len(level), func(groupCtx context.Context, i int) error {
			dir := level[i]
			_ = sess.do(groupCtx, watch, func(client *sftp.Client) error {
				done := metrics.Op("sftp_rmdir")
				err := client.RemoveDirectory(dir)
				done(err)
				watch.tick()
				return err
			})
			return nil
		})
	}
}
