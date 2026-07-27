package uploader

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

// poolTree writes n small files, enough of them that every pool slot is used
// (files are spread over the pool by their plan index).
func poolTree(t *testing.T, root string, n int) {
	t.Helper()
	files := make(map[string]string, n)
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("file%02d.txt", i)] = fmt.Sprintf("content %d", i)
	}
	writeTree(t, root, files)
}

// TestConnectionPoolOpensConfiguredConnections pins the point of issue #158:
// advanced.connections opens that many SSH connections, and the default of one
// keeps the single-connection behavior. The count is read before any
// verifyClient runs, since those dial the same server.
func TestConnectionPoolOpensConfiguredConnections(t *testing.T) {
	for _, tc := range []struct {
		name        string
		connections int
		want        int32
	}{
		{"default is one connection", 0, 1},
		{"explicit single connection", 1, 1},
		{"pool of three", 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startTestServer(t)
			local := t.TempDir()
			poolTree(t, local, 9)

			cfg := baseConfig(srv)
			cfg.Concurrency = 3
			cfg.Connections = tc.connections
			cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

			stats, err := Run(context.Background(), cfg, testLogger{t})
			if err != nil {
				t.Fatal(err)
			}
			if got := atomic.LoadInt32(&srv.accepted); got != tc.want {
				t.Errorf("expected %d connection(s), got %d", tc.want, got)
			}
			if stats.FilesUploaded != 9 {
				t.Fatalf("expected 9 uploads, got %d", stats.FilesUploaded)
			}
			if got := readRemote(t, srv, "/www/file08.txt"); got != "content 8" {
				t.Errorf("unexpected content: %q", got)
			}
		})
	}
}

// TestConnectionPoolNeverExceedsConcurrency: a connection no worker ever picks
// is a handshake for nothing, so the pool is capped at the number of parallel
// uploads and says so.
func TestConnectionPoolNeverExceedsConcurrency(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 6)

	cfg := baseConfig(srv)
	cfg.Concurrency = 2
	cfg.Connections = 8
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&srv.accepted); got != 2 {
		t.Errorf("expected the pool to be capped at concurrency (2), got %d connection(s)", got)
	}
	if !containsSubstring(log.infos, "only 2 file(s) upload in parallel") {
		t.Errorf("expected an info line about the cap, got %v", log.infos)
	}
}

// TestConnectionPoolDegradesWhenServerRefusesConnections: sshd's MaxStartups
// and per-account limits on shared hosting are real, so a refused extra
// connection must cost a warning and not the deploy.
func TestConnectionPoolDegradesWhenServerRefusesConnections(t *testing.T) {
	srv := startTestServer(t, withMaxConns(1))
	local := t.TempDir()
	poolTree(t, local, 6)

	cfg := baseConfig(srv)
	cfg.Concurrency = 3
	cfg.Connections = 3
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("a server refusing extra connections must not fail the run: %v", err)
	}
	if stats.FilesUploaded != 6 {
		t.Errorf("expected 6 uploads, got %d", stats.FilesUploaded)
	}
	if !containsSubstring(log.warnings, "continues on its first connection") {
		t.Errorf("expected a warning about the refused connection, got %v", log.warnings)
	}
}

func containsSubstring(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
