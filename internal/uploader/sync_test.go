package uploader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"

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

// hashPlanFiles hashes files concurrently; its result must be identical to
// hashing each file sequentially with hashFile.
func TestHashPlanFilesMatchesSequentialHash(t *testing.T) {
	dir := t.TempDir()
	files := make([]fileItem, 64)
	for i := range files {
		p := filepath.Join(dir, fmt.Sprintf("f%02d.txt", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("content-%d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		files[i] = fileItem{localPath: p}
	}

	if err := hashPlanFiles(context.Background(), files, 8, nil); err != nil {
		t.Fatal(err)
	}

	for i := range files {
		want, err := hashFile(files[i].localPath)
		if err != nil {
			t.Fatal(err)
		}
		if files[i].hash == "" {
			t.Errorf("file %d: hash was not set", i)
		}
		if files[i].hash != want {
			t.Errorf("file %d: hash = %q, want %q", i, files[i].hash, want)
		}
	}
}

// A read error on any file must surface from the pool rather than being lost.
func TestHashPlanFilesPropagatesError(t *testing.T) {
	files := []fileItem{
		{localPath: filepath.Join(t.TempDir(), "does-not-exist")},
	}
	if err := hashPlanFiles(context.Background(), files, 4, nil); err == nil {
		t.Fatal("expected an error hashing a missing file, got nil")
	}
}

// When a file's size and mtime still match its manifest entry, hashPlanFiles
// must reuse the stored hash instead of reading the file — proven here by
// giving it a wrong-but-matching cached hash and checking it wins.
func TestHashPlanFilesFastPathReusesCachedHash(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	files := []fileItem{{localPath: p, rel: "a.txt", size: fi.Size(), mtime: fi.ModTime().Unix()}}
	cached := map[string]manifestEntry{
		"a.txt": {Hash: "stale-cached-hash", Size: fi.Size(), MTime: fi.ModTime().Unix()},
	}

	if err := hashPlanFiles(context.Background(), files, 4, cached); err != nil {
		t.Fatal(err)
	}
	if files[0].hash != "stale-cached-hash" {
		t.Errorf("fast path did not reuse the cached hash: got %q", files[0].hash)
	}
}

// A manifest entry whose size or mtime differs must trigger a real re-hash,
// not a false-positive fast-path hit.
func TestHashPlanFilesFastPathMissOnMismatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	want, err := hashFile(p)
	if err != nil {
		t.Fatal(err)
	}

	cases := []manifestEntry{
		{Hash: "stale", Size: fi.Size() + 1, MTime: fi.ModTime().Unix()}, // size mismatch
		{Hash: "stale", Size: fi.Size(), MTime: fi.ModTime().Unix() + 1}, // mtime mismatch
		{Hash: "stale", Size: fi.Size(), MTime: 0},                       // v1 upgrade: MTime unknown
	}
	for _, entry := range cases {
		files := []fileItem{{localPath: p, rel: "a.txt", size: fi.Size(), mtime: fi.ModTime().Unix()}}
		cached := map[string]manifestEntry{"a.txt": entry}
		if err := hashPlanFiles(context.Background(), files, 4, cached); err != nil {
			t.Fatal(err)
		}
		if files[0].hash != want {
			t.Errorf("entry %+v: expected a real re-hash, got %q want %q", entry, files[0].hash, want)
		}
	}
}

// A manifest written by an older easySFTP version (v1: hash only, no
// size/mtime) must still be read correctly — an upgrade re-hashes once, then
// starts writing v2 manifests with the fast-path fields populated.
// With sync-fast-path opted in, an unchanged file (same size, same mtime) is
// skipped without a real re-hash — behavior stays identical to the default,
// only the local I/O it takes to get there changes. A changed file with a
// distinct size and a clearly later mtime is still detected and re-uploaded
// (size or mtime differing is all the fast path needs).
func TestSyncFastPathSkipsUnchangedFile(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1", "keep.txt": "keep"})

	cfg := syncConfig(srv, local)
	cfg.SyncFastPath = true

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(local, "a.txt"), []byte("twenty-two"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(local, "a.txt"), future, future); err != nil {
		t.Fatal(err)
	}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 1 || stats.FilesSkipped != 1 {
		t.Fatalf("up=%d skip=%d, want 1/1", stats.FilesUploaded, stats.FilesSkipped)
	}
	if got := readRemote(t, srv, "/www/a.txt"); got != "twenty-two" {
		t.Errorf("changed file was not re-uploaded: %q", got)
	}
}

// Documents the known limitation: with sync-fast-path on, a same-size edit
// that lands within the same mtime second as the file it replaces is
// invisible to the size+mtime check and is missed — exactly the tradeoff
// action.yml and docs/strategies.md describe. This is not a "should" test;
// it pins the documented behavior so a future change to the comparison
// doesn't silently alter it either way.
func TestSyncFastPathMissesSameSecondSameSizeEdit(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1"})

	cfg := syncConfig(srv, local)
	cfg.SyncFastPath = true

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(filepath.Join(local, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	mtime := fi.ModTime()

	// Same size ("1" -> "2"), mtime forced back to exactly what the manifest
	// already recorded — simulating two same-size edits landing in the same
	// mtime second, which second-granularity filesystem clocks make common.
	writeTree(t, local, map[string]string{"a.txt": "2"})
	if err := os.Chtimes(filepath.Join(local, "a.txt"), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if got := readRemote(t, srv, "/www/a.txt"); got != "1" {
		t.Fatalf("expected the fast path to miss the edit and leave the old content, got %q", got)
	}
}

func TestSyncUpgradesV1Manifest(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1", "keep.txt": "keep"})

	hashA, err := hashFile(filepath.Join(local, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	hashKeep, err := hashFile(filepath.Join(local, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}

	client := srv.verifyClient(t)
	if err := client.MkdirAll("/www"); err != nil {
		t.Fatal(err)
	}
	v1 := fmt.Sprintf(`{"version":1,"files":{"a.txt":%q,"keep.txt":%q}}`, hashA, hashKeep)
	f, err := client.Create("/www/" + manifestName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(v1)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// The v1 manifest lists files that were never actually uploaded; put them
	// on the server too so this looks like a real prior sync.
	for name, content := range map[string]string{"a.txt": "1", "keep.txt": "keep"} {
		wf, err := client.Create("/www/" + name)
		if err != nil {
			t.Fatal(err)
		}
		wf.Write([]byte(content))
		wf.Close()
	}

	stats, err := Run(context.Background(), syncConfig(srv, local), testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 0 || stats.FilesDeleted != 0 || stats.FilesSkipped != 2 {
		t.Fatalf("upgrading a v1 manifest: up=%d del=%d skip=%d, want 0/0/2", stats.FilesUploaded, stats.FilesDeleted, stats.FilesSkipped)
	}

	data, err := io.ReadAll(mustOpen(t, client, "/www/"+manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var got manifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("rewritten manifest is not valid v2 JSON: %v", err)
	}
	if got.Version != manifestVersion {
		t.Errorf("manifest version = %d, want %d", got.Version, manifestVersion)
	}
	if e := got.Files["a.txt"]; e.Size == 0 || e.MTime == 0 {
		t.Errorf("rewritten manifest entry missing size/mtime: %+v", e)
	}
}

func mustOpen(t *testing.T, client *sftp.Client, path string) *sftp.File {
	t.Helper()
	f, err := client.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
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
