// Package uploader implements the SFTP upload logic of easySFTP:
// connecting, planning uploads, syncing files and optional remote cleanup.
package uploader

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pkg/sftp"
	ignore "github.com/sabhiram/go-gitignore"
	"golang.org/x/crypto/ssh"
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
	DirsCreated   int
	BytesUploaded int64
	Duration      time.Duration
}

// fileItem is a single planned file transfer.
type fileItem struct {
	localPath  string // absolute or workspace-relative OS path
	remotePath string // slash-separated remote path
	size       int64
	mode       fs.FileMode
}

// plan is the complete set of transfers for one upload pair.
type plan struct {
	pair       config.UploadPair
	files      []fileItem
	remoteDirs []string // directories to create, sorted parents-first
}

// Run executes the configured upload and returns transfer statistics.
func Run(ctx context.Context, cfg *config.Config, log Logger) (*Stats, error) {
	start := time.Now()
	stats := &Stats{}

	matcher := ignore.CompileIgnoreLines(cfg.IgnoreLines...)

	// Build the full local plan first so config/path errors surface before
	// we touch the network.
	plans := make([]plan, 0, len(cfg.Uploads))
	for _, pair := range cfg.Uploads {
		p, err := buildPlan(pair, matcher)
		if err != nil {
			return stats, err
		}
		plans = append(plans, p)
	}

	sshClient, sftpClient, err := connect(cfg, log)
	if err != nil {
		return stats, err
	}
	defer sshClient.Close()
	defer sftpClient.Close()

	for _, p := range plans {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if err := executePlan(ctx, cfg, sftpClient, p, stats, log); err != nil {
			return stats, err
		}
	}

	stats.Duration = time.Since(start)
	return stats, nil
}

// buildPlan walks the local side of an upload pair and computes the remote
// file and directory layout, honoring the ignore patterns.
func buildPlan(pair config.UploadPair, matcher *ignore.GitIgnore) (plan, error) {
	p := plan{pair: pair}
	remoteBase := normalizeRemote(pair.Remote)

	info, err := os.Stat(pair.Local)
	if err != nil {
		return p, fmt.Errorf("local path %q: %w", pair.Local, err)
	}

	// A single file maps directly onto the remote path. A trailing slash on
	// the remote side means "into this directory".
	if !info.IsDir() {
		remotePath := remoteBase
		if strings.HasSuffix(pair.Remote, "/") || remoteBase == "." {
			remotePath = path.Join(remoteBase, filepath.Base(pair.Local))
		}
		if matcher.MatchesPath(filepath.Base(pair.Local)) {
			return p, nil
		}
		p.files = append(p.files, fileItem{
			localPath:  pair.Local,
			remotePath: remotePath,
			size:       info.Size(),
			mode:       info.Mode(),
		})
		p.remoteDirs = parentDirs(remotePath)
		return p, nil
	}

	dirSet := map[string]struct{}{}
	err = filepath.WalkDir(pair.Local, func(fpath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(pair.Local, fpath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if matcher.MatchesPath(rel) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			// Symlinks, sockets etc. are skipped; SFTP uploads regular files.
			return nil
		}
		remotePath := path.Join(remoteBase, rel)
		p.files = append(p.files, fileItem{
			localPath:  fpath,
			remotePath: remotePath,
			size:       fi.Size(),
			mode:       fi.Mode(),
		})
		for _, dir := range parentDirs(remotePath) {
			dirSet[dir] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return p, fmt.Errorf("walking local path %q: %w", pair.Local, err)
	}

	for dir := range dirSet {
		p.remoteDirs = append(p.remoteDirs, dir)
	}
	// Parents sort before their children, so creation order is safe.
	sort.Strings(p.remoteDirs)
	return p, nil
}

// executePlan performs (or previews) the deletes, mkdirs and uploads of one plan.
func executePlan(ctx context.Context, cfg *config.Config, client *sftp.Client, p plan, stats *Stats, log Logger) error {
	verb := ""
	if cfg.DryRun {
		verb = "[dry-run] would "
	}
	log.Group(fmt.Sprintf("%s => %s (%d files)", p.pair.Local, p.pair.Remote, len(p.files)))
	defer log.EndGroup()

	if cfg.Delete {
		deleted, err := deleteRemoteContents(client, normalizeRemote(p.pair.Remote), cfg.DryRun, verb, log)
		if err != nil {
			return fmt.Errorf("cleaning remote directory %q: %w", p.pair.Remote, err)
		}
		stats.FilesDeleted += deleted
	}

	for _, dir := range p.remoteDirs {
		if cfg.DryRun {
			continue
		}
		if _, err := client.Stat(dir); err == nil {
			continue
		}
		if err := client.MkdirAll(dir); err != nil {
			return fmt.Errorf("creating remote directory %q: %w", dir, err)
		}
		stats.DirsCreated++
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(cfg.Concurrency)
	results := make([]int64, len(p.files))

	for i, f := range p.files {
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			log.Infof("%supload %s => %s (%s)", verb, f.localPath, f.remotePath, humanSize(f.size))
			if cfg.DryRun {
				return nil
			}
			n, err := uploadFileWithRetry(f, client, cfg.Retries, log)
			if err != nil {
				return err
			}
			results[i] = n
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	stats.FilesUploaded += len(p.files)
	for _, n := range results {
		stats.BytesUploaded += n
	}
	return nil
}

// uploadFileWithRetry uploads one file, retrying transient failures with
// exponential backoff.
func uploadFileWithRetry(f fileItem, client *sftp.Client, retries int, log Logger) (int64, error) {
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			log.Warningf("retrying upload of %s in %s (attempt %d/%d): %v", f.localPath, backoff, attempt+1, retries+1, lastErr)
			time.Sleep(backoff)
		}
		n, err := uploadFile(f, client)
		if err == nil {
			return n, nil
		}
		lastErr = err
	}
	return 0, fmt.Errorf("uploading %q to %q: %w", f.localPath, f.remotePath, lastErr)
}

func uploadFile(f fileItem, client *sftp.Client) (int64, error) {
	src, err := os.Open(f.localPath)
	if err != nil {
		return 0, err
	}
	defer src.Close()

	dst, err := client.OpenFile(f.remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return 0, err
	}

	n, err := io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}

	// Best effort: keep the local permission bits. Some servers reject this.
	_ = client.Chmod(f.remotePath, f.mode.Perm())
	return n, nil
}

// deleteRemoteContents removes everything inside dir (but not dir itself).
// A missing dir is not an error. Returns the number of files removed.
func deleteRemoteContents(client *sftp.Client, dir string, dryRun bool, verb string, log Logger) (int, error) {
	entries, err := client.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	deleted := 0
	for _, entry := range entries {
		full := path.Join(dir, entry.Name())
		if entry.IsDir() {
			n, err := deleteRemoteContents(client, full, dryRun, verb, log)
			deleted += n
			if err != nil {
				return deleted, err
			}
			log.Infof("%sdelete directory %s", verb, full)
			if !dryRun {
				if err := client.RemoveDirectory(full); err != nil {
					return deleted, err
				}
			}
			continue
		}
		log.Infof("%sdelete %s", verb, full)
		if !dryRun {
			if err := client.Remove(full); err != nil {
				return deleted, err
			}
		}
		deleted++
	}
	return deleted, nil
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
		sftp.MaxConcurrentRequestsPerFile(64),
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
	if len(cfg.HostKeyFingerprints) == 0 {
		log.Warningf("no host-key-fingerprint configured — the server's identity will NOT be verified. " +
			"Run 'ssh-keyscan <server> | ssh-keygen -lf -' and set the host-key-fingerprint input to pin it.")
		return ssh.InsecureIgnoreHostKey(), nil
	}
	want := cfg.HostKeyFingerprints
	for _, fp := range want {
		if !strings.HasPrefix(fp, "SHA256:") {
			return nil, fmt.Errorf("host-key-fingerprint must be a SHA256 fingerprint like 'SHA256:...', got %q", fp)
		}
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		got := ssh.FingerprintSHA256(key)
		for _, fp := range want {
			if got == fp {
				return nil
			}
		}
		return fmt.Errorf("host key mismatch for %s: got %s, want one of: %s", hostname, got, strings.Join(want, ", "))
	}, nil
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
