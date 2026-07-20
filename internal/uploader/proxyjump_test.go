package uploader

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

// proxyFor returns proxy settings pointing at the jump server, pinned to its
// host key.
func proxyFor(jump *testJumpServer) *config.Proxy {
	return &config.Proxy{
		Server:              jump.Host,
		Port:                jump.Port,
		Username:            testUser,
		Password:            testPassword,
		HostKeyFingerprints: []string{jump.HostKeySHA256},
	}
}

func TestProxyJumpUpload(t *testing.T) {
	target := startTestServer(t)
	jump := startTestJumpServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "via bastion", "sub/a.txt": "a"})

	cfg := baseConfig(target)
	cfg.HostKeyFingerprints = []string{target.HostKeySHA256}
	cfg.Proxy = proxyFor(jump)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 2 {
		t.Errorf("expected 2 uploads, got %d", stats.FilesUploaded)
	}
	if got := readRemote(t, target, "/www/index.html"); got != "via bastion" {
		t.Errorf("unexpected content: %q", got)
	}
	if atomic.LoadInt64(&jump.forwarded) == 0 {
		t.Error("the jump server forwarded no connection; the run must have bypassed the tunnel")
	}
}

func TestProxyJumpVerifiesJumpHostKey(t *testing.T) {
	target := startTestServer(t)
	jump := startTestJumpServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "x"})

	cfg := baseConfig(target)
	cfg.HostKeyFingerprints = []string{target.HostKeySHA256}
	cfg.Proxy = proxyFor(jump)
	cfg.Proxy.HostKeyFingerprints = []string{"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("expected a jump-host key mismatch error, got %v", err)
	}
	if !strings.Contains(err.Error(), "jump host") {
		t.Errorf("the error should name the jump host hop, got %v", err)
	}
}

func TestProxyJumpVerifiesTargetHostKey(t *testing.T) {
	target := startTestServer(t)
	jump := startTestJumpServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "x"})

	cfg := baseConfig(target)
	cfg.HostKeyFingerprints = []string{"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	cfg.Proxy = proxyFor(jump)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("expected a target host key mismatch error through the tunnel, got %v", err)
	}
}

func TestProxyJumpWarnsPerUnpinnedHop(t *testing.T) {
	target := startTestServer(t)
	jump := startTestJumpServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "x"})

	cfg := baseConfig(target)
	cfg.HostKeyFingerprints = []string{target.HostKeySHA256} // target pinned, jump not
	cfg.Proxy = proxyFor(jump)
	cfg.Proxy.HostKeyFingerprints = nil
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}
	found := false
	log.mu.Lock()
	for _, w := range log.warnings {
		if strings.Contains(w, "proxy-host-key-fingerprint") {
			found = true
		}
	}
	log.mu.Unlock()
	if !found {
		t.Errorf("expected an unverified-host-key warning naming the proxy inputs, got %v", log.warnings)
	}
}
