package uploader

import (
	"context"
	"io/fs"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

// TestLeafDirs verifies the set of planned directories is reduced to just the
// deepest members, since MkdirAll on those recreates every ancestor.
func TestLeafDirs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, []string{}},
		{"single", []string{"/www"}, []string{"/www"}},
		{
			name: "linear tree keeps only the deepest",
			in:   []string{"a", "a/b", "a/b/c", "a/b/c/d"},
			want: []string{"a/b/c/d"},
		},
		{
			name: "branching tree keeps every branch tip",
			in:   []string{"a", "a/b", "a/b/c", "a/b/c/d", "a/b/c/e"},
			want: []string{"a/b/c/d", "a/b/c/e"},
		},
		{
			// A sibling whose name sorts before '/' must not fool the leaf
			// detection into treating its parent as a leaf.
			name: "sibling sorting before the separator",
			in:   []string{"x", "x/a", "x/a/c", "x/a.b"},
			want: []string{"x/a.b", "x/a/c"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := leafDirs(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("leafDirs(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestFreshTreeCreatesEachDirectoryOnce verifies a first deploy into an empty
// remote creates every directory of a deep tree exactly once, with parents
// created implicitly by MkdirAll on the leaves rather than pre-created level by
// level.
func TestFreshTreeCreatesEachDirectoryOnce(t *testing.T) {
	var ops opCounter
	srv := startTestServer(t, withOpCounter(&ops))

	local := t.TempDir()
	// Two leaf directories under a five-level-deep shared trunk.
	writeTree(t, local, map[string]string{
		"a/b/c/d/one.txt":   "1",
		"a/b/c/d/two.txt":   "2",
		"a/b/c/e/three.txt": "3",
	})

	cfg := baseConfig(srv)
	cfg.Concurrency = 1
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	// /www + a + b + c + d + e, each created exactly once.
	if got := atomic.LoadInt64(&ops.mkdir); got != 6 {
		t.Errorf("first deploy issued %d Mkdir calls; want 6 (one per directory)", got)
	}
	if got := readRemote(t, srv, "/www/a/b/c/e/three.txt"); got != "3" {
		t.Errorf("deep file not uploaded: %q", got)
	}
}

// TestExistingTreeConfirmsOnlyLeaves verifies that when every remote directory
// already exists, the uploader confirms just the leaf directories: it stats one
// path per leaf and creates nothing, instead of stat'ing every ancestor of
// every file as the old level-by-level setup did.
func TestExistingTreeConfirmsOnlyLeaves(t *testing.T) {
	var ops opCounter
	srv := startTestServer(t, withOpCounter(&ops))

	// Pre-create the full directory tree so the deploy finds it already present.
	admin := srv.verifyClient(t)
	for _, d := range []string{"/www/a/b/c/d", "/www/a/b/c/e"} {
		if err := admin.MkdirAll(d); err != nil {
			t.Fatal(err)
		}
	}

	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"a/b/c/d/one.txt": "1",
		"a/b/c/e/two.txt": "2",
	})

	cfg := baseConfig(srv)
	cfg.Concurrency = 1
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	atomic.StoreInt64(&ops.stat, 0)
	atomic.StoreInt64(&ops.mkdir, 0)
	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&ops.mkdir); got != 0 {
		t.Errorf("deploy created %d directories; want 0 (all already exist)", got)
	}
	// One stat per leaf (/www/a/b/c/d and /www/a/b/c/e), not one per ancestor.
	if got := atomic.LoadInt64(&ops.stat); got != 2 {
		t.Errorf("deploy stat'd %d paths for directory setup; want 2 (one per leaf)", got)
	}
}

// TestSyncNoOpIssuesNoDirRoundTrips verifies a sync with nothing to upload
// derives its directory set from the (empty) upload list: zero Mkdir and zero
// directory Stat calls, instead of one round-trip per leaf of the whole plan.
func TestSyncNoOpIssuesNoDirRoundTrips(t *testing.T) {
	var ops opCounter
	srv := startTestServer(t, withOpCounter(&ops))

	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"a/one.txt": "1",
		"b/two.txt": "2",
		"c/tri.txt": "3",
	})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategySync}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	atomic.StoreInt64(&ops.mkdir, 0)
	atomic.StoreInt64(&ops.stat, 0)
	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 0 || stats.FilesSkipped != 3 {
		t.Fatalf("no-op sync: up=%d skip=%d, want 0/3", stats.FilesUploaded, stats.FilesSkipped)
	}
	if got := atomic.LoadInt64(&ops.mkdir); got != 0 {
		t.Errorf("no-op sync issued %d Mkdir calls; want 0", got)
	}
	if got := atomic.LoadInt64(&ops.stat); got != 0 {
		t.Errorf("no-op sync issued %d Stat calls; want 0", got)
	}
}

// TestSyncPartialChangeTouchesOnlyChangedDirs verifies an incremental sync
// confirms only the directories of the files actually being uploaded, not
// every leaf of the whole plan.
func TestSyncPartialChangeTouchesOnlyChangedDirs(t *testing.T) {
	var ops opCounter
	srv := startTestServer(t, withOpCounter(&ops))

	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"a/one.txt": "1",
		"b/two.txt": "2",
		"c/tri.txt": "3",
	})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategySync}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	writeTree(t, local, map[string]string{"a/one.txt": "changed"})
	atomic.StoreInt64(&ops.mkdir, 0)
	atomic.StoreInt64(&ops.stat, 0)
	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 1 {
		t.Fatalf("incremental sync uploads: got %d, want 1", stats.FilesUploaded)
	}
	if got := atomic.LoadInt64(&ops.mkdir); got != 0 {
		t.Errorf("incremental sync created %d directories; want 0 (all exist)", got)
	}
	// MkdirAll confirms just /www/a (the one leaf the changed file needs).
	if got := atomic.LoadInt64(&ops.stat); got != 1 {
		t.Errorf("incremental sync stat'd %d paths for directory setup; want 1", got)
	}
}

// TestSyncDirModeStillCoversWholePlan pins the documented dir-mode semantics:
// with dir-mode set, every directory of the plan is chmod'd (the run "touches"
// them), even when nothing needs uploading. The fake server rejects chmod on
// directories, so the assertion is on the requests received, not the modes.
func TestSyncDirModeStillCoversWholePlan(t *testing.T) {
	rec := &setstatRecorder{}
	srv := startTestServer(t, withSetstatRecorder(rec))

	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"a/one.txt": "1",
		"b/two.txt": "2",
	})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategySync}}
	mode := fs.FileMode(0o755)
	cfg.DirMode = &mode

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	// Second, no-op sync: dir-mode must still request a chmod on every plan
	// directory (/www, /www/a, /www/b).
	rec.mu.Lock()
	rec.calls = nil
	rec.mu.Unlock()
	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	got := map[string]bool{}
	for _, c := range rec.calls {
		got[c.path] = true
	}
	for _, want := range []string{"/www", "/www/a", "/www/b"} {
		if !got[want] {
			t.Errorf("no-op sync with dir-mode did not chmod %s; requests: %v", want, rec.calls)
		}
	}
}
