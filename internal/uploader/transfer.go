package uploader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/sync/errgroup"

	"github.com/eiserv/easySFTP/internal/config"
)

// tmpSuffix is appended to the remote path while a file is still uploading.
// It keeps the temp file in the same directory as its target so the final
// rename stays on one filesystem and is atomic.
const tmpSuffix = ".easysftp-tmp"

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
		// Through sess.do so a connection drop during directory setup redials
		// instead of failing the run; MkdirAll and chmod are idempotent, so
		// rerunning the whole pass on a fresh client is safe.
		err := sess.do(ctx, watch, func(client *sftp.Client) error {
			return createRemoteDirs(client, dirs, cfg.DirMode, watch, log)
		})
		if err != nil {
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
