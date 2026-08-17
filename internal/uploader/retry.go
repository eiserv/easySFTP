package uploader

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/pkg/sftp"

	"github.com/eiserv/easySFTP/internal/metrics"
)

// uploadFileWithRetry uploads one file, retrying transient failures with
// exponential backoff. It stops early when the context is cancelled or the
// error is permanent (see isRetryable), so a doomed transfer fails fast.
// When a failure looks connection-class, the session is asked to reconnect
// first, so the retry runs against a live client instead of the dead one.
//
// index is the file's position in the plan. It is folded into the temp path
// (see uploadFile) so two planned transfers never race over the same temporary
// name, even if one target's path happens to literally be another's plus
// tmpSuffix, and it picks the file's connection out of the pool.
// The results are named so the metrics sample below can be recorded from a
// defer, covering every one of this function's exits.
func uploadFileWithRetry(ctx context.Context, env *transferEnv, f fileItem, index int, mode fs.FileMode) (uploaded int64, err error) {
	sess, watch, log, retries := env.sess, env.watch, env.log, env.cfg.Retries
	// One sample per file, covering every attempt and its backoff: the sum of
	// the individual round-trips below it is what this splits into.
	doneFile := metrics.Op("file_upload")
	defer func() { doneFile(err) }()
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			metrics.Count("retries", 1)
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			log.Warningf("retrying upload of %s in %s (attempt %d/%d): %v", f.localPath, backoff, attempt+1, retries+1, lastErr)
			if err := sleepCtx(ctx, backoff); err != nil {
				return 0, err
			}
		}
		client, c, gen := sess.acquire(index)
		if attempt > 0 {
			// A previous attempt may have left its temp file behind (a dead
			// connection cannot run the normal cleanup). Clear it so the
			// fresh attempt starts from a clean slate; harmless when absent.
			_ = client.Remove(fmt.Sprintf("%s%s.%d", f.remotePath, tmpSuffix, index))
		}
		n, err := uploadFile(ctx, env, f, index, mode, client)
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
			if _, rerr := sess.reconnect(ctx, c, gen); rerr != nil {
				return 0, fmt.Errorf("uploading %q to %q: %w (%v)", f.localPath, f.remotePath, lastErr, rerr)
			}
		}
	}
	// A status the server will repeat verbatim ended the loop early. Say so:
	// otherwise a run configured with retries looks like it silently skipped
	// them, and the reader has to guess whether the file was given up on too
	// soon. The server's own status line stays in front of the clause.
	if isPermanentStatus(lastErr) {
		return 0, fmt.Errorf("uploading %q to %q: %w (the server rejected this outright; retrying would not have helped)",
			f.localPath, f.remotePath, lastErr)
	}
	return 0, fmt.Errorf("uploading %q to %q: %w", f.localPath, f.remotePath, lastErr)
}

// isRetryable reports whether an error is worth another attempt. Permanent
// failures (bad permissions, missing paths, a status code the server will
// repeat) and a cancelled/expired context are not retried; transient ones
// (dropped connections, timeouts, EOF) are.
//
// Anything unrecognised stays retryable on purpose. For a momentary network
// condition another attempt is what saves the run, and mistaking a transient
// failure for a permanent one costs more than the reverse. The point of
// isPermanentStatus is to name the failures that are known-permanent, not to
// flip that bias.
func isRetryable(err error) bool {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, os.ErrPermission), errors.Is(err, os.ErrNotExist):
		return false
	case isPermanentStatus(err):
		return false
	default:
		return true
	}
}

// isPermanentStatus reports whether err carries an SFTP status code the server
// has already made up its mind about: a retry sends the identical request over
// a healthy connection and gets the identical answer back, so it only costs
// the backoff.
//
// Only unambiguous codes are listed. SSH_FX_FAILURE deliberately is not. It is
// the catch-all servers use for a disk quota, a full or read-only filesystem
// and a path-length limit (all permanent), but also for genuinely transient
// conditions, and only the human-readable message tells the two apart.
// Matching on that text is fragile across server implementations and locales,
// so SSH_FX_FAILURE keeps its retry; the wrapped error carries the server's
// own status line, which is what the user reads either way.
//
// SSH_FX_NO_SUCH_FILE and SSH_FX_PERMISSION_DENIED are listed even though
// pkg/sftp normalises both into os.ErrNotExist and os.ErrPermission before
// they reach here, so the errors.Is arms above catch them first: the
// classification should not quietly change if that normalisation does.
// SSH_FX_NO_CONNECTION and SSH_FX_CONNECTION_LOST are absent on purpose, being
// exactly the failures a reconnect fixes.
func isPermanentStatus(err error) bool {
	var se *sftp.StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.FxCode() {
	case sftp.ErrSSHFxOpUnsupported, // the server does not implement this operation
		sftp.ErrSSHFxBadMessage, // the request itself is malformed for this server
		sftp.ErrSSHFxNoSuchFile,
		sftp.ErrSSHFxPermissionDenied:
		return true
	}
	return false
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
