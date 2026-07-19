package uploader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"github.com/eiserv/easySFTP/internal/config"
)

func TestIsConnError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"sftp connection lost", sftp.ErrSSHFxConnectionLost, true},
		{"wrapped EOF", fmt.Errorf("replacing %q: %w", "/x", io.EOF), true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"permission denied", os.ErrPermission, false},
		{"not exist", os.ErrNotExist, false},
		{"plain sftp failure", errors.New("sftp: \"failure\" (SSH_FX_FAILURE)"), false},
		{"stringly broken pipe", errors.New("write: broken pipe"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isConnError(c.err); got != c.want {
				t.Errorf("isConnError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// A one-off mid-run connection drop must not kill the deploy: the session
// redials, per-file retries re-run against the fresh client, and already
// completed files stay completed.
func TestReconnectResumesAfterMidRunDrop(t *testing.T) {
	// The first connection dies once ~200 KiB have flowed; the redial gets a
	// clean connection.
	srv := startTestServer(t, withDropFirstConnAfter(200*1024))

	local := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 6; i++ {
		files[fmt.Sprintf("f%d.bin", i)] = strings.Repeat(fmt.Sprintf("%d", i), 100*1024)
	}
	writeTree(t, local, files)

	cfg := baseConfig(srv)
	cfg.Concurrency = 2
	cfg.Retries = 2
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("expected the run to survive the drop via reconnect, got %v", err)
	}
	if stats.FilesUploaded != 6 {
		t.Errorf("uploads: got %d, want 6", stats.FilesUploaded)
	}
	for name, content := range files {
		if got := readRemote(t, srv, "/www/"+name); got != content {
			t.Errorf("%s: wrong content after reconnect (len %d, want %d)", name, len(got), len(content))
		}
	}
	reconnected := false
	for _, w := range log.warnings {
		if strings.Contains(w, "reconnecting") {
			reconnected = true
		}
	}
	if !reconnected {
		t.Errorf("expected a reconnect warning, got %v", log.warnings)
	}
}

// When every connection keeps dying, the reconnect budget (the retries input)
// bounds the redials and the run fails instead of looping forever.
func TestReconnectBudgetBoundsRedials(t *testing.T) {
	// Every connection dies after 64 KiB; a 1 MiB file can never finish.
	srv := startTestServer(t, withDropAfter(64*1024))

	local := t.TempDir()
	writeTree(t, local, map[string]string{"big.bin": strings.Repeat("x", 1<<20)})

	cfg := baseConfig(srv)
	cfg.Concurrency = 1
	cfg.Retries = 2
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	start := time.Now()
	stats, err := Run(context.Background(), cfg, log)
	if err == nil {
		t.Fatal("expected the run to fail once the reconnect budget is spent")
	}
	if stats.FilesUploaded != 0 {
		t.Errorf("uploads: got %d, want 0", stats.FilesUploaded)
	}
	reconnects := 0
	for _, w := range log.warnings {
		if strings.Contains(w, "reconnecting") {
			reconnects++
		}
	}
	if reconnects > cfg.Retries {
		t.Errorf("observed %d reconnects, budget is %d", reconnects, cfg.Retries)
	}
	if elapsed := time.Since(start); elapsed > 60*time.Second {
		t.Errorf("run took %s; it should fail fast once the budget is spent", elapsed)
	}
}

// Retries=0 disables reconnects along with per-file retries: a dropped
// connection fails the run on the first error, as before.
func TestReconnectDisabledWithZeroRetries(t *testing.T) {
	srv := startTestServer(t, withDropFirstConnAfter(64*1024))

	local := t.TempDir()
	writeTree(t, local, map[string]string{"big.bin": strings.Repeat("x", 1<<20)})

	cfg := baseConfig(srv) // Retries: 0
	cfg.Concurrency = 1
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err == nil {
		t.Fatal("expected the run to fail without retries/reconnects")
	}
}
