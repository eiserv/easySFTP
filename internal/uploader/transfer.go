package uploader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/sync/errgroup"

	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/metrics"
)

// tmpSuffix is appended to the remote path while a file is still uploading.
// It keeps the temp file in the same directory as its target so the final
// rename stays on one filesystem and is atomic.
const tmpSuffix = ".easysftp-tmp"

// transferEnv carries the invariants that every level of the upload call chain
// re-threads to the next (the session, the stall watchdog, the logger and the
// two warn-once flags). It is built once in uploadFiles and passed down, so
// per-file positions stay short and, in particular, the two adjacent
// *atomic.Bool flags can no longer be transposed at a call site: they are
// named struct fields instead of interchangeable positional arguments.
//
// uploadFiles runs once per deployment, so the two warn-once flags are scoped
// to a deployment, not to the whole run: a multi-deployment run against a
// server that rejects SETSTAT warns once per affected deployment, which is
// what tells the user how far the problem reaches. The warning texts say so
// (see issue #121).
type transferEnv struct {
	cfg   *config.Config
	sess  *session
	watch *stallWatchdog
	log   Logger
	// modeWarned is armed only when file-mode is an explicit override; a
	// mirrored local mode (the default) stays nil and warns silently. See
	// uploadFile.
	modeWarned *atomic.Bool
	// timesWarned doubles as the preserve-times switch: nil means off.
	timesWarned *atomic.Bool
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
func uploadFiles(ctx context.Context, cfg *config.Config, sess *session, files []fileItem, dirs []string, base string, stats *Stats, verb string, watch *stallWatchdog, skipUnchanged bool, log Logger) ([]bool, error) {
	// Declared before the first failure point: callers index the returned
	// slice by file, so it must be sized even when nothing was uploaded.
	completed := make([]bool, len(files))
	skipped := make([]bool, len(files))

	if !cfg.DryRun {
		// Through sess.do so a connection drop during directory setup redials
		// instead of failing the run; MkdirAll and chmod are idempotent, so
		// rerunning the whole pass on a fresh client is safe.
		endDirs := metrics.Phase("create_dirs")
		err := sess.do(ctx, watch, func(client *sftp.Client) error {
			return createRemoteDirs(client, dirs, cfg.DirMode, watch, log)
		})
		endDirs()
		if err != nil {
			return completed, err
		}

		// Before any upload starts, remove temp files that an earlier killed
		// run left in the directories this run uploads into; see
		// sweepStaleTemps.
		endSweep := metrics.Phase("sweep_stale_temps")
		sweepStaleTemps(ctx, sess, watch, base, files, log)
		endSweep()
	}

	defer metrics.Phase("upload")()
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(cfg.Concurrency)
	results := make([]int64, len(files))
	env := &transferEnv{cfg: cfg, sess: sess, watch: watch, log: log}
	// modeWarned is only armed when file-mode is an explicit override: a
	// mirrored local mode (the default) stays silent on failure, as before.
	if cfg.FileMode != nil {
		env.modeWarned = new(atomic.Bool)
	}
	// timesWarned doubles as the preserve-times switch: nil means off. The
	// user explicitly asked for preserved times, so a refusing server warns
	// (once per deployment); staying silent would defeat the point of the
	// input.
	if cfg.PreserveTimes {
		env.timesWarned = new(atomic.Bool)
	}

	for i, f := range files {
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			// The stat is read-only, so it also runs in dry-run mode: the
			// preview then reports the same skips the real run would.
			if skipUnchanged {
				client, _, _ := sess.acquire(i)
				done := metrics.Op("sftp_stat")
				fi, err := client.Stat(f.remotePath)
				done(err)
				if err == nil && fi.Mode().IsRegular() && fi.Size() == f.size {
					if cfg.LogPerFile() {
						log.Infof("%sskip %s (remote file has the same size)", verb, f.remotePath)
					}
					skipped[i] = true
					return nil
				}
			}
			if cfg.LogPerFile() {
				log.Infof("%supload %s => %s (%s)", verb, f.localPath, f.remotePath, HumanSize(f.size))
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
			n, err := uploadFileWithRetry(ctx, env, f, i, mode)
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
func uploadFile(ctx context.Context, env *transferEnv, f fileItem, index int, mode fs.FileMode, client *sftp.Client) (int64, error) {
	watch, log := env.watch, env.log
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
	//
	// The four round-trips below (open, write, chmod, rename) are sampled
	// separately: in the small-file scenario the payload is a rounding error
	// and the question is which of them the run is really spending its time in.
	tmpPath := fmt.Sprintf("%s%s.%d", f.remotePath, tmpSuffix, index)
	doneOpen := metrics.Op("sftp_open")
	dst, err := client.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	doneOpen(err)
	if err != nil {
		return 0, err
	}

	// ctxReader aborts the copy promptly when the deployment is cancelled,
	// instead of streaming a whole large file first.
	var reader io.Reader = &ctxReader{ctx: ctx, r: src}
	if watch != nil {
		reader = watch.reader(reader)
	}
	doneWrite := metrics.Op("sftp_write")
	n, err := io.Copy(dst, reader)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	doneWrite(err)
	metrics.Count("local_read_bytes", n)
	if err != nil {
		cleanupTmp(client, tmpPath, log)
		return n, err
	}

	// Best effort: mirrors the local permission bits, or the file-mode
	// override when set. Some servers reject SETSTAT; an explicit override
	// warns once per deployment so the user knows it isn't taking effect, but
	// a mirrored local mode stays silent as before.
	doneChmod := metrics.Op("sftp_chmod")
	cerr := client.Chmod(tmpPath, mode)
	doneChmod(cerr)
	if cerr != nil && env.modeWarned != nil && !env.modeWarned.Swap(true) {
		log.Warningf("could not set file-mode %04o on %s (server may reject SETSTAT); not warning again for this deployment: %v", mode, f.remotePath, cerr)
	}

	doneRename := metrics.Op("sftp_rename")
	rerr := renameReplace(client, tmpPath, f.remotePath)
	doneRename(rerr)
	if rerr != nil {
		cleanupTmp(client, tmpPath, log)
		return n, fmt.Errorf("replacing %q: %w", f.remotePath, rerr)
	}

	// preserve-times (timesWarned non-nil): keep the local modification time
	// instead of "now". After the rename, so the request targets the final
	// path; a failure warns once per deployment and never fails the deploy.
	// f.mtime is unix nanoseconds; the SFTP SETSTAT request carries whole
	// seconds, so the sub-second part is truncated on the wire either way.
	if env.timesWarned != nil {
		mtime := time.Unix(0, f.mtime)
		doneTimes := metrics.Op("sftp_chtimes")
		cerr := client.Chtimes(f.remotePath, mtime, mtime)
		doneTimes(cerr)
		if cerr != nil && !env.timesWarned.Swap(true) {
			log.Warningf("could not preserve the modification time on %s (server may reject SETSTAT); not warning again for this deployment: %v", f.remotePath, cerr)
		}
	}
	return n, nil
}

// posixRenameExt is the OpenSSH extension that makes a rename replace an
// existing target atomically. Servers announce the extensions they implement
// in their SSH_FXP_VERSION packet, which pkg/sftp records per connection.
const posixRenameExt = "posix-rename@openssh.com"

// renameReplace atomically moves tmp onto final. It prefers the
// posix-rename@openssh.com extension (a true atomic overwrite) and falls back
// to a plain remove+rename for servers that do not support it.
func renameReplace(client *sftp.Client, tmp, final string) error {
	err := client.PosixRename(tmp, final)
	if err == nil {
		return nil
	}
	_, announced := client.HasExtension(posixRenameExt)
	if !posixRenameUnsupported(err, announced) {
		return err
	}
	// note: non-atomic fallback, a brief window where final is missing.
	// Only reached on servers lacking posix-rename; unavoidable there.
	_ = client.Remove(final)
	return client.Rename(tmp, final)
}

// posixRenameUnsupported reports whether a failed posix-rename@openssh.com
// request means "this server does not implement the extension" rather than
// "the server refused this particular rename". Only the first justifies the
// non-atomic remove+rename fallback: falling back on a refusal would remove a
// live file the server was never going to let us replace (issue #152).
//
// SSH_FX_OP_UNSUPPORTED is the answer the protocol asks for, and OpenSSH gives
// it. Other implementations answer an unknown extended request with the
// generic SSH_FX_FAILURE (or, when they reject the packet itself,
// SSH_FX_BAD_MESSAGE), which on its own is indistinguishable from a policy or
// permission rejection. The server's own extension announcement breaks the
// tie: a server that never announced posix-rename was never asked a question
// it advertised being able to answer, so a generic failure there is the
// extension missing. A server that did announce it and then fails generically
// is refusing the rename, and that error is returned unchanged.
//
// Codes the client library translates away (SSH_FX_NO_SUCH_FILE and
// SSH_FX_PERMISSION_DENIED become os.ErrNotExist / os.ErrPermission and carry
// no *sftp.StatusError) therefore never reach the fallback either, which is
// what we want: both name a rename the server understood and declined.
func posixRenameUnsupported(err error, announced bool) bool {
	var se *sftp.StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.FxCode() {
	case sftp.ErrSSHFxOpUnsupported:
		return true
	case sftp.ErrSSHFxFailure, sftp.ErrSSHFxBadMessage:
		return !announced
	default:
		return false
	}
}

// cleanupTmp best-effort removes a leftover temp file, warning (but not
// failing) if the server refuses, so an orphan is at least visible in the log.
func cleanupTmp(client *sftp.Client, tmpPath string, log Logger) {
	if err := client.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warningf("could not remove temporary file %s: %v", tmpPath, err)
	}
}

// staleTempMaxAge is how old (by remote modification time) a leftover temp
// upload file must be before sweepStaleTemps removes it. Concurrent deploys
// to one target are unsupported, but a sweep that deleted another live run's
// in-progress temp file would turn that race into a corrupted deploy; the
// age margin keeps any plausibly-live temp file safe without locking. A
// variable only so tests can shrink it.
var staleTempMaxAge = time.Hour

// isTempFileName reports whether name looks like a temporary upload file this
// action writes: "<name>.easysftp-tmp.<n>" for a file upload (n being the
// plan index, see uploadFile) or "<name>.easysftp-tmp" for a manifest write
// (see writeManifest). The suffix is always appended to an existing name, so
// a file named exactly ".easysftp-tmp" (i == 0) is not one of ours.
func isTempFileName(name string) bool {
	i := strings.LastIndex(name, tmpSuffix)
	if i <= 0 {
		return false
	}
	rest := name[i+len(tmpSuffix):]
	if rest == "" {
		return true
	}
	if rest[0] != '.' || len(rest) < 2 {
		return false
	}
	for _, c := range rest[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// sweepStaleTemps removes temp upload files that an earlier killed run left
// behind (issue #160). Every in-process failure path already cleans its temp
// file up, but SIGKILL, a cancelled workflow past its grace period or a
// reclaimed runner skip all deferred code, and neither overlay nor sync ever
// deletes a file it did not just write, so the orphans would otherwise sit in
// the target forever, publicly served when the target is a web root.
//
// The swept set is exactly the directories that receive files this run (the
// parent directory of every planned remote path) plus the deployment's remote
// base, where the sync manifest's temp file lives even when no changed file
// does. Nothing above the base is ever listed or touched: a deploy to
// /var/www/html/blog has no business reading /var. That keeps the added
// round-trips proportional to the run and the sweep's reach inside the
// deployment.
//
// Three guards bound what the sweep may remove: the name must match the temp
// pattern (isTempFileName), the entry must be older than staleTempMaxAge (a
// younger one is plausibly another live run's in-progress upload), and it
// must not be a planned target of this run (a real deployed file can be named
// like a temp file; see the literal-name test in atomic_test.go). Staleness
// compares the runner's clock against the mtime the server reports
// (time.Since of the entry's ModTime), so a server clock running behind the
// runner's can make a fresh temp file look older than it is; concurrent
// deploys to one target are unsupported anyway, so such skew weakens a
// mitigation rather than creating a new hazard.
//
// The sweep deliberately runs for every strategy, the non-destructive overlay
// included, and has no opt-out: it only ever removes this action's own
// "*.easysftp-tmp" and "*.easysftp-tmp.<n>" debris, never a file it did not
// name itself. For the same reason its removals deliberately do not count
// against safety.max_deletes (which budgets deletions of deployed files) and
// are not reported in the run's delete stats.
//
// Everything is best-effort: each directory runs through sess.do so a dropped
// connection redials, but a listing or removal failure only warns and never
// fails the deploy. Removals are logged, so a user who wondered what those
// files were gets an answer.
func sweepStaleTemps(ctx context.Context, sess *session, watch *stallWatchdog, base string, files []fileItem, log Logger) {
	keep := make(map[string]struct{}, len(files))
	for _, f := range files {
		keep[f.remotePath] = struct{}{}
	}
	touched := make(map[string]struct{}, len(files)+1)
	// A single-file overlay deploy without a trailing slash resolves base to
	// the planned file itself, not a directory; its parent joins the set via
	// path.Dir below, so the file path is skipped here.
	if _, isPlannedFile := keep[base]; !isPlannedFile {
		touched[base] = struct{}{}
	}
	for _, f := range files {
		touched[path.Dir(f.remotePath)] = struct{}{}
	}
	dirs := make([]string, 0, len(touched))
	for dir := range touched {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		_ = sess.do(ctx, watch, func(client *sftp.Client) error {
			return sweepDirStaleTemps(client, dir, keep, watch, log)
		})
	}
}

// sweepDirStaleTemps sweeps a single remote directory; see sweepStaleTemps.
// Connection-class errors are returned so sess.do redials and reruns the
// directory (removals are idempotent); anything else is swallowed after a
// warning.
func sweepDirStaleTemps(client *sftp.Client, dir string, keep map[string]struct{}, watch *stallWatchdog, log Logger) error {
	doneList := metrics.Op("sftp_readdir")
	entries, err := client.ReadDir(dir)
	doneList(err)
	if err != nil {
		if isConnError(err) {
			return err
		}
		// Best-effort: a directory that cannot be listed is left alone. Not
		// worth a warning; the run itself has not lost anything.
		return nil
	}
	watch.tick()
	for _, e := range entries {
		if e.IsDir() || !isTempFileName(e.Name()) {
			continue
		}
		if time.Since(e.ModTime()) < staleTempMaxAge {
			continue
		}
		full := path.Join(dir, e.Name())
		if _, ok := keep[full]; ok {
			continue
		}
		done := metrics.Op("sftp_remove")
		err := client.Remove(full)
		done(err)
		if err != nil {
			if isConnError(err) {
				return err
			}
			if !errors.Is(err, os.ErrNotExist) {
				log.Warningf("could not remove stale temporary file %s: %v", full, err)
			}
			continue
		}
		log.Infof("removed stale temporary file %s (left behind by an earlier interrupted run)", full)
		watch.tick()
	}
	return nil
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
