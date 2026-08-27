package uploader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/pkg/sftp"

	"github.com/eiserv/easySFTP/internal/config"
)

// TestAtomicReplaceLeavesNoTempFile verifies a successful upload swaps the file
// into place and leaves no temporary sibling behind.
func TestAtomicReplaceLeavesNoTempFile(t *testing.T) {
	srv := startTestServer(t)

	// Pre-existing live file that the upload must replace.
	seedRemoteFile(t, srv, "/www", "index.html", "old")

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "new"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if got := readRemote(t, srv, "/www/index.html"); got != "new" {
		t.Errorf("content not replaced: %q", got)
	}
	if remoteHasTmpFile(t, srv, "/www", "index.html") {
		t.Error("temporary upload file was left behind")
	}
}

// TestRenameFailureCleansUpAndKeepsOriginal verifies that when the final rename
// fails, the run errors, the temporary file is removed, and the live file is
// left untouched (never replaced by a half-swapped upload).
func TestRenameFailureCleansUpAndKeepsOriginal(t *testing.T) {
	srv := startTestServer(t, withFailRename())
	seedRemoteFile(t, srv, "/www", "index.html", "original")

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "replacement"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "replacing") {
		t.Fatalf("expected a rename/replace error, got %v", err)
	}
	if got := readRemote(t, srv, "/www/index.html"); got != "original" {
		t.Errorf("live file was clobbered by a failed upload: %q", got)
	}
	if remoteHasTmpFile(t, srv, "/www", "index.html") {
		t.Error("temporary file was not cleaned up after the failed rename")
	}
}

// seedRemoteFile writes content to a remote path (creating its directory),
// standing in for a file an earlier deploy left on the server.
func seedRemoteFile(t *testing.T, srv *testServer, dir, name, content string) {
	t.Helper()
	client := srv.verifyClient(t)
	if err := client.MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	f, err := client.Create(path.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// TestPosixRenameUnsupportedFallsBack verifies the remove+rename fallback is
// taken on a server that lacks posix-rename@openssh.com, for both answers such
// a server gives: the protocol's SSH_FX_OP_UNSUPPORTED, and the generic
// SSH_FX_FAILURE that non-OpenSSH implementations answer an unknown extended
// request with. In the generic case the fallback is justified only by the
// server not announcing the extension, so that server does not announce it
// (issue #152).
func TestPosixRenameUnsupportedFallsBack(t *testing.T) {
	cases := []struct {
		name       string
		code       error
		unannounce bool
	}{
		{"op unsupported", sftp.ErrSSHFxOpUnsupported, false},
		{"generic failure, extension not announced", sftp.ErrSSHFxFailure, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.unannounce {
				unadvertisePosixRename(t)
			}
			srv := startTestServer(t, withFailPosixRename(c.code))
			seedRemoteFile(t, srv, "/www", "index.html", "old")

			local := t.TempDir()
			writeTree(t, local, map[string]string{"index.html": "new"})

			cfg := baseConfig(srv)
			cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

			if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
				t.Fatalf("upload failed without posix-rename: %v", err)
			}
			if got := readRemote(t, srv, "/www/index.html"); got != "new" {
				t.Errorf("content not replaced: %q", got)
			}
			if remoteHasTmpFile(t, srv, "/www", "index.html") {
				t.Error("temporary upload file was left behind")
			}
		})
	}
}

// TestPosixRenameRefusalIsNotOverridden verifies the opposite half of the same
// rule: a server that announces posix-rename@openssh.com and then refuses one
// rename is refusing it, not missing the extension. Falling back there would
// remove a live file the server was never going to let us replace, so the run
// must fail with the target untouched. SSH_FX_FAILURE is the ambiguous code
// (SSH_FX_PERMISSION_DENIED is unambiguous, and the client library rewrites it
// into os.ErrPermission before renameReplace ever sees a status).
func TestPosixRenameRefusalIsNotOverridden(t *testing.T) {
	srv := startTestServer(t, withFailPosixRename(sftp.ErrSSHFxFailure))
	seedRemoteFile(t, srv, "/www", "index.html", "original")

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "replacement"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err == nil {
		t.Fatal("expected the refused rename to fail the run, got nil")
	}
	if got := readRemote(t, srv, "/www/index.html"); got != "original" {
		t.Errorf("live file was removed by a fallback that should not have run: %q", got)
	}
	if remoteHasTmpFile(t, srv, "/www", "index.html") {
		t.Error("temporary file was not cleaned up after the refused rename")
	}
}

// statusErr builds the error a pkg/sftp client surfaces for one SFTP status
// code, which is the shape renameReplace has to classify.
func statusErr(code uint32) error { return &sftp.StatusError{Code: code} }

// TestPosixRenameUnsupportedClassification pins the decision itself, over
// every status code that can reach it and both announcement states.
func TestPosixRenameUnsupportedClassification(t *testing.T) {
	var (
		unsupported = statusErr(uint32(sftp.ErrSSHFxOpUnsupported))
		failure     = statusErr(uint32(sftp.ErrSSHFxFailure))
		badMessage  = statusErr(uint32(sftp.ErrSSHFxBadMessage))
		connLost    = statusErr(uint32(sftp.ErrSSHFxConnectionLost))
		noConn      = statusErr(uint32(sftp.ErrSSHFxNoConnection))
	)

	cases := []struct {
		name      string
		err       error
		announced bool
		want      bool
	}{
		{"op unsupported, announced", unsupported, true, true},
		{"op unsupported, not announced", unsupported, false, true},
		{"failure, announced", failure, true, false},
		{"failure, not announced", failure, false, true},
		{"bad message, announced", badMessage, true, false},
		{"bad message, not announced", badMessage, false, true},
		{"connection lost, not announced", connLost, false, false},
		{"no connection, not announced", noConn, false, false},
		// The client translates these two away before renameReplace sees them.
		{"permission denied", os.ErrPermission, false, false},
		{"not exist", os.ErrNotExist, false, false},
		{"transport error", errors.New("connection lost"), false, false},
		{"wrapped op unsupported", fmt.Errorf("rename: %w", unsupported), true, true},
	}
	for _, c := range cases {
		if got := posixRenameUnsupported(c.err, c.announced); got != c.want {
			t.Errorf("%s: posixRenameUnsupported = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTempNameCollisionAvoided verifies that a deployment containing both
// "app.js" and a file literally named "app.js.easysftp-tmp" uploads both
// correctly: the temp path used while streaming "app.js" must not collide
// with the real target path of the other file (issue #42).
func TestTempNameCollisionAvoided(t *testing.T) {
	srv := startTestServer(t)

	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"app.js":             "real-app-content",
		"app.js" + tmpSuffix: "literal-file-named-like-a-temp",
	})

	cfg := baseConfig(srv)
	cfg.Concurrency = 4
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if got := readRemote(t, srv, "/www/app.js"); got != "real-app-content" {
		t.Errorf("/www/app.js = %q, want unclobbered content", got)
	}
	if got := readRemote(t, srv, "/www/app.js"+tmpSuffix); got != "literal-file-named-like-a-temp" {
		t.Errorf("/www/app.js%s = %q, want unclobbered content", tmpSuffix, got)
	}
}

// TestConnectionDropFailsCleanly verifies that a mid-transfer connection drop
// surfaces as an error instead of hanging or being reported as success.
func TestConnectionDropFailsCleanly(t *testing.T) {
	srv := startTestServer(t, withDropAfter(64*1024))

	local := t.TempDir()
	writeTree(t, local, map[string]string{"big.bin": strings.Repeat("x", 4*1024*1024)})

	cfg := baseConfig(srv)
	cfg.Concurrency = 1
	cfg.Retries = 0 // a dropped single connection cannot recover; fail fast.
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err == nil {
		t.Fatal("expected an error after the connection was dropped, got nil")
	}
}

// TestFailedBatchReturnsPartialStats verifies successful files in a batch are
// still reported when a later file fails.
func TestFailedBatchReturnsPartialStats(t *testing.T) {
	srv := startTestServer(t)

	client := srv.verifyClient(t)
	if err := client.MkdirAll("/www/z.txt"); err != nil {
		t.Fatal(err)
	}

	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "a", "z.txt": "z"})

	cfg := baseConfig(srv)
	cfg.Concurrency = 1
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil {
		t.Fatal("expected the second upload to fail")
	}
	if stats.FilesUploaded != 1 || stats.BytesUploaded != 1 {
		t.Errorf("partial stats = %d file(s), %d byte(s); want 1 file, 1 byte",
			stats.FilesUploaded, stats.BytesUploaded)
	}
	if stats.Duration <= 0 {
		t.Errorf("partial duration = %s; want a positive duration", stats.Duration)
	}
}

// TestAbortedDeploymentStops verifies that a cancelled context aborts the
// deployment with the context error and uploads nothing.
func TestAbortedDeploymentStops(t *testing.T) {
	srv := startTestServer(t)

	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "a", "b/c.txt": "c"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // deployment aborted before any transfer

	stats, err := Run(ctx, cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("expected a context-cancelled error, got %v", err)
	}
	if stats.BytesUploaded != 0 {
		t.Errorf("aborted deployment transferred %d bytes", stats.BytesUploaded)
	}
	if remoteExists(t, srv, "/www") {
		t.Error("aborted deployment created remote files")
	}
}

// TestIsRetryable checks the error classification that drives retry behaviour.
func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"permission denied", os.ErrPermission, false},
		{"not exist", os.ErrNotExist, false},
		{"context cancelled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"wrapped permission", fmt.Errorf("open: %w", os.ErrPermission), false},
		{"transient network error", errors.New("connection lost"), true},
		{"wrapped context cancel", fmt.Errorf("copy: %w", context.Canceled), false},
		// Server status codes. The permanent ones are named explicitly; the
		// ambiguous and the connection-class ones keep their retry.
		{"op unsupported", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxOpUnsupported)}, false},
		{"bad message", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxBadMessage)}, false},
		{"status no such file", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxNoSuchFile)}, false},
		{"status permission denied", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxPermissionDenied)}, false},
		{"wrapped op unsupported", fmt.Errorf("replacing %q: %w", "/www/a.txt",
			&sftp.StatusError{Code: uint32(sftp.ErrSSHFxOpUnsupported)}), false},
		// Quota, no space and read-only filesystems arrive as this one, but so
		// do transient server-side failures; it stays retryable.
		{"generic failure", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxFailure)}, true},
		{"connection lost status", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxConnectionLost)}, true},
		{"no connection status", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxNoConnection)}, true},
	}
	for _, c := range cases {
		if got := isRetryable(c.err); got != c.want {
			t.Errorf("%s: isRetryable = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestNonAtomicPublishKeepsTheLiveFileWhenTheRenameFails is the guarantee the
// atomic-upload feature exists for, arriving through the fallback that servers
// without posix-rename@openssh.com take.
//
// The old spelling was remove(final) then rename(tmp, final). If the remove
// succeeded and the rename then failed, the live file was gone and nothing had
// replaced it, which is precisely the outcome the feature promises cannot
// happen. The causes are ordinary on the appliance and managed-hosting servers
// this path exists for: a full filesystem, a quota, a permission change, a
// dropped connection, another process taking the name (issue #242).
func TestNonAtomicPublishKeepsTheLiveFileWhenTheRenameFails(t *testing.T) {
	srv := startTestServer(t,
		withFailPosixRename(sftp.ErrSSHFxOpUnsupported),
		withFailPublishRename("/www/index.html"))
	seedRemoteFile(t, srv, "/www", "index.html", "original")

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "replacement"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err == nil {
		t.Fatal("expected the failed publish to fail the run")
	}
	if got := readRemote(t, srv, "/www/index.html"); got != "original" {
		t.Errorf("the live file reads %q; a failed publish must not cost the file it was replacing", got)
	}
	if remoteHasTmpFile(t, srv, "/www", "index.html") {
		t.Error("a temporary or backup file was left behind after the rollback")
	}
}

// TestNonAtomicPublishOfANewFile covers the branch where there is nothing to
// park, on both kinds of server: one that reports a missing file specifically,
// and one that answers every absence with a generic status, where the tie-break
// from issue #152 is the only thing that can tell "nothing there" from "the
// server refused to move it".
func TestNonAtomicPublishOfANewFile(t *testing.T) {
	cases := []struct {
		name string
		opts []serverOption
	}{
		{"server reports a missing file", []serverOption{withFailPosixRename(sftp.ErrSSHFxOpUnsupported)}},
		{"server answers absence generically", []serverOption{withCoarseStatus(sftp.ErrSSHFxFailure), withFailPosixRename(sftp.ErrSSHFxOpUnsupported)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startTestServer(t, tc.opts...)
			local := t.TempDir()
			writeTree(t, local, map[string]string{"index.html": "fresh"})

			cfg := baseConfig(srv)
			cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

			if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
				t.Fatalf("first upload without posix-rename failed: %v", err)
			}
			if got := readRemote(t, srv, "/www/index.html"); got != "fresh" {
				t.Errorf("unexpected content: %q", got)
			}
			if remoteHasTmpFile(t, srv, "/www", "index.html") {
				t.Error("a temporary or backup file was left behind")
			}
		})
	}
}

// The backup has to be swept if a run is killed between parking the live file
// and putting it back, which is why it lives inside the tmpSuffix family
// rather than under a name of its own.
func TestBackupNameIsSweptAsATempFile(t *testing.T) {
	if !isTempFileName("index.html" + bakSuffix) {
		t.Errorf("%q is not recognised as a temp file, so an interrupted publish would leave it behind forever", "index.html"+bakSuffix)
	}
	for _, name := range []string{".easysftp-tmp.bak", "index.html.bak", "index.html.easysftp-tmp.baked"} {
		if isTempFileName(name) {
			t.Errorf("%q is swept but is not a file this action writes", name)
		}
	}
}
