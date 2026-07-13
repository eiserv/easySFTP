package uploader

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

func syncConfig(srv *testServer, local string) *config.Config {
	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategySync}}
	return cfg
}

func TestSyncIncremental(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"a.txt":    "1",
		"keep.txt": "keep",
		"b/c.txt":  "c",
	})

	// First sync: everything is new.
	stats, err := Run(context.Background(), syncConfig(srv, local), testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 3 || stats.FilesDeleted != 0 || stats.FilesSkipped != 0 {
		t.Fatalf("first sync: up=%d del=%d skip=%d, want 3/0/0", stats.FilesUploaded, stats.FilesDeleted, stats.FilesSkipped)
	}
	if !remoteExists(t, srv, "/www/"+manifestName) {
		t.Fatal("manifest was not written")
	}

	// Change a.txt, delete b/c.txt, leave keep.txt untouched, add new.txt.
	writeTree(t, local, map[string]string{"a.txt": "2", "new.txt": "n"})
	if err := os.Remove(filepath.Join(local, "b", "c.txt")); err != nil {
		t.Fatal(err)
	}

	stats, err = Run(context.Background(), syncConfig(srv, local), testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 2 { // a.txt (changed) + new.txt (new)
		t.Errorf("second sync uploads: got %d, want 2", stats.FilesUploaded)
	}
	if stats.FilesDeleted != 1 { // b/c.txt
		t.Errorf("second sync deletes: got %d, want 1", stats.FilesDeleted)
	}
	if stats.FilesSkipped != 1 { // keep.txt unchanged
		t.Errorf("second sync skipped: got %d, want 1", stats.FilesSkipped)
	}
	if got := readRemote(t, srv, "/www/a.txt"); got != "2" {
		t.Errorf("a.txt not updated: %q", got)
	}
	if got := readRemote(t, srv, "/www/keep.txt"); got != "keep" {
		t.Errorf("keep.txt changed unexpectedly: %q", got)
	}
	if remoteExists(t, srv, "/www/b/c.txt") || remoteExists(t, srv, "/www/b") {
		t.Error("removed file / empty dir was not pruned")
	}
}

// The manifest is the only source of what may be deleted, so files placed on
// the server by someone else are never touched.
func TestSyncLeavesUnmanagedFilesAlone(t *testing.T) {
	srv := startTestServer(t)
	client := srv.verifyClient(t)
	if err := client.MkdirAll("/www"); err != nil {
		t.Fatal(err)
	}
	f, err := client.Create("/www/human.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("do not touch"))
	f.Close()

	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1"})

	if _, err := Run(context.Background(), syncConfig(srv, local), testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if got := readRemote(t, srv, "/www/human.txt"); got != "do not touch" {
		t.Errorf("unmanaged file was modified/deleted: %q", got)
	}
}

func TestSyncMaxDeletesGuard(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"})

	if _, err := Run(context.Background(), syncConfig(srv, local), testLogger{t}); err != nil {
		t.Fatal(err)
	}

	// Remove two files; with max_deletes=1 the second run must refuse.
	if err := os.Remove(filepath.Join(local, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(local, "c.txt")); err != nil {
		t.Fatal(err)
	}
	cfg := syncConfig(srv, local)
	cfg.Guards.MaxDeletes = 1

	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "max_deletes") {
		t.Fatalf("expected max_deletes guard error, got %v", err)
	}
	// Nothing was deleted because the guard fires before any removal.
	if !remoteExists(t, srv, "/www/b.txt") {
		t.Error("guard should have aborted before deleting")
	}
}

func TestCleanRefusesRemoteRoot(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "a"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/", Strategy: config.StrategyClean}}

	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "remote root") {
		t.Fatalf("expected remote-root guard error, got %v", err)
	}
}

func TestSyncSingleFileRejected(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "a"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: filepath.Join(local, "a.txt"), Remote: "/www/a.txt", Strategy: config.StrategySync}}

	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "requires a directory") {
		t.Fatalf("expected single-file rejection, got %v", err)
	}
}

func TestSyncDryRunChangesNothing(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "a", "b.txt": "b"})

	cfg := syncConfig(srv, local)
	cfg.DryRun = true

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 2 {
		t.Errorf("expected 2 planned uploads, got %d", stats.FilesUploaded)
	}
	if remoteExists(t, srv, "/www/"+manifestName) || remoteExists(t, srv, "/www/a.txt") {
		t.Error("dry-run must not write anything to the server")
	}
}
