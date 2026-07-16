package uploader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

// TestAtomicReplaceLeavesNoTempFile verifies a successful upload swaps the file
// into place and leaves no temporary sibling behind.
func TestAtomicReplaceLeavesNoTempFile(t *testing.T) {
	srv := startTestServer(t)

	// Pre-existing live file that the upload must replace.
	client := srv.verifyClient(t)
	if err := client.MkdirAll("/www"); err != nil {
		t.Fatal(err)
	}
	f, err := client.Create("/www/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("old")); err != nil {
		t.Fatal(err)
	}
	f.Close()

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

	client := srv.verifyClient(t)
	if err := client.MkdirAll("/www"); err != nil {
		t.Fatal(err)
	}
	f, err := client.Create("/www/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("original")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "replacement"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	_, err = Run(context.Background(), cfg, testLogger{t})
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
	}
	for _, c := range cases {
		if got := isRetryable(c.err); got != c.want {
			t.Errorf("%s: isRetryable = %v, want %v", c.name, got, c.want)
		}
	}
}
