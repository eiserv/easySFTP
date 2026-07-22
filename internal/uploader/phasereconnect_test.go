package uploader

// Tests for issue #107: the non-upload phases (remote scans, delete sweeps,
// manifest handling) share the reconnect budget and the stall watchdog with
// the upload path, instead of running on a one-time client snapshot.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/config"
)

// A connection drop during the clean strategy's delete sweep must redial and
// finish the sweep, not fail the run.
func TestCleanReconnectsDuringDeletePhase(t *testing.T) {
	srv := startTestServer(t, withDropOnRequest("Remove", "/www/stale.txt"))
	writeRemoteFiles(t, srv, map[string]string{
		"/www/stale.txt":     "old",
		"/www/sub/stale.txt": "old",
	})

	local := t.TempDir()
	writeTree(t, local, map[string]string{"new.txt": "fresh"})

	cfg := baseConfig(srv)
	cfg.Retries = 2
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategyClean}}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("expected the clean run to survive the mid-delete drop, got %v", err)
	}
	if stats.FilesDeleted != 2 {
		t.Errorf("deletes: got %d, want 2", stats.FilesDeleted)
	}
	for _, p := range []string{"/www/stale.txt", "/www/sub/stale.txt"} {
		if remoteExists(t, srv, p) {
			t.Errorf("%s still exists after the clean sweep", p)
		}
	}
	if got := readRemote(t, srv, "/www/new.txt"); got != "fresh" {
		t.Errorf("new.txt content: got %q, want %q", got, "fresh")
	}
	if !loggedReconnect(log) {
		t.Errorf("expected a reconnect warning, got %v", log.warnings)
	}
}

// A connection drop during the clean strategy's remote scan must redial and
// rescan; the scan is idempotent.
func TestCleanReconnectsDuringRemoteScan(t *testing.T) {
	srv := startTestServer(t, withDropOnRequest("List", "/www/sub"))
	writeRemoteFiles(t, srv, map[string]string{
		"/www/stale.txt":     "old",
		"/www/sub/stale.txt": "old",
	})

	local := t.TempDir()
	writeTree(t, local, map[string]string{"new.txt": "fresh"})

	cfg := baseConfig(srv)
	cfg.Retries = 2
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategyClean}}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("expected the clean run to survive the mid-scan drop, got %v", err)
	}
	if stats.FilesDeleted != 2 {
		t.Errorf("deletes: got %d, want 2", stats.FilesDeleted)
	}
	if !loggedReconnect(log) {
		t.Errorf("expected a reconnect warning, got %v", log.warnings)
	}
}

// A connection drop during the sync strategy's delete phase must redial,
// finish the deletions and leave a manifest matching the local tree.
func TestSyncReconnectsDuringDeletePhase(t *testing.T) {
	srv := startTestServer(t, withDropOnRequest("Remove", "/www/b.txt"))

	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "A", "b.txt": "B"})

	cfg := baseConfig(srv)
	cfg.Retries = 2
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategySync}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatalf("first sync failed: %v", err)
	}

	if err := os.Remove(filepath.Join(local, "b.txt")); err != nil {
		t.Fatal(err)
	}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("expected the second sync to survive the mid-delete drop, got %v", err)
	}
	if stats.FilesDeleted != 1 {
		t.Errorf("deletes: got %d, want 1", stats.FilesDeleted)
	}
	if remoteExists(t, srv, "/www/b.txt") {
		t.Error("b.txt still exists after the sync deleted it")
	}
	m := readRemoteManifest(t, srv)
	if _, ok := m.Files["b.txt"]; ok {
		t.Error("manifest still lists b.txt after its deletion")
	}
	if _, ok := m.Files["a.txt"]; !ok {
		t.Error("manifest lost a.txt")
	}
	if !loggedReconnect(log) {
		t.Errorf("expected a reconnect warning, got %v", log.warnings)
	}
}

// A server that hangs during the delete sweep must trip stall-timeout and
// fail the run fast, instead of hanging until the job-level timeout.
func TestStallTimeoutCoversDeletePhase(t *testing.T) {
	srv := startTestServer(t, withStallOnRequest("Remove", "/www/stale.txt"))
	writeRemoteFiles(t, srv, map[string]string{"/www/stale.txt": "old"})

	local := t.TempDir()
	writeTree(t, local, map[string]string{"new.txt": "fresh"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategyClean}}
	cfg.StallTimeout = 1 * time.Second

	log := &recordingLogger{testLogger: testLogger{t}}
	start := time.Now()
	_, err := Run(context.Background(), cfg, log)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("expected a transfer-stalled error, got %v", err)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("run took %s; the stall-timeout should have failed it fast", elapsed)
	}
}

// loggedReconnect reports whether the run logged a reconnect warning.
func loggedReconnect(log *recordingLogger) bool {
	for _, w := range log.warnings {
		if strings.Contains(w, "reconnecting") {
			return true
		}
	}
	return false
}
