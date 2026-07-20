package uploader

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

func TestInitialConnectRetriesTransientFailure(t *testing.T) {
	srv := startTestServer(t, withRefuseFirstConns(2))
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi"})

	cfg := baseConfig(srv)
	cfg.Retries = 2
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("expected the run to succeed within the retry budget: %v", err)
	}
	if stats.FilesUploaded != 1 {
		t.Errorf("expected 1 upload, got %d", stats.FilesUploaded)
	}
	retries := 0
	for _, w := range log.warnings {
		if strings.Contains(w, "could not connect; retrying") {
			retries++
		}
	}
	if retries != 2 {
		t.Errorf("expected 2 connect-retry warnings, got %d: %v", retries, log.warnings)
	}
}

func TestInitialConnectGivesUpAfterRetryBudget(t *testing.T) {
	srv := startTestServer(t, withRefuseFirstConns(100))
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi"})

	cfg := baseConfig(srv)
	cfg.Retries = 1
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err == nil {
		t.Fatal("expected the run to fail once the retry budget is spent")
	}
	if got := atomic.LoadInt32(&srv.accepted); got != 2 {
		t.Errorf("expected 2 connection attempts (1 + 1 retry), got %d", got)
	}
}

func TestAuthFailureIsNotRetried(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi"})

	cfg := baseConfig(srv)
	cfg.Retries = 3
	cfg.Password = "wrong-password"
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("expected an authentication error, got %v", err)
	}
	if got := atomic.LoadInt32(&srv.accepted); got != 1 {
		t.Errorf("auth failure must not be retried; got %d connection attempts", got)
	}
}

func TestHostKeyMismatchIsNotRetried(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi"})

	cfg := baseConfig(srv)
	cfg.Retries = 3
	cfg.HostKeyFingerprints = []string{"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("expected a host key mismatch error, got %v", err)
	}
	if got := atomic.LoadInt32(&srv.accepted); got != 1 {
		t.Errorf("host key mismatch must not be retried; got %d connection attempts", got)
	}
}
