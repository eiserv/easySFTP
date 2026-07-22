package uploader

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync/atomic"
	"time"
)

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
