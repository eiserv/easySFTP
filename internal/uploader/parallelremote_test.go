package uploader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/config"
)

func TestRunBoundedEnforcesWorkerLimit(t *testing.T) {
	const limit = 4
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	started := make(chan struct{}, limit+1)
	finished := make(chan error, 1)
	var active, maximum atomic.Int64

	go func() {
		finished <- runBounded(context.Background(), limit, 8, func(context.Context, int) error {
			now := active.Add(1)
			for {
				old := maximum.Load()
				if now <= old || maximum.CompareAndSwap(old, now) {
					break
				}
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			return nil
		})
	}()

	for i := 0; i < limit; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("worker pool did not fill to its limit")
		}
	}
	select {
	case <-started:
		t.Fatal("worker pool started more calls than its limit")
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if got := maximum.Load(); got != limit {
		t.Fatalf("maximum active calls = %d, want %d", got, limit)
	}
}

func TestCleanScansAndPrunesWideDeepTree(t *testing.T) {
	srv := startTestServer(t)
	remote := map[string]string{
		"/www/flat.txt":       "stale",
		"/www/a/b/c.txt":      "stale",
		"/www/a/d.txt":        "stale",
		"/www/e/f/g/h.txt":    "stale",
		"/www/sibling/x.txt":  "stale",
		"/www/sibling2/y.txt": "stale",
	}
	writeRemoteFiles(t, srv, remote)

	cfg := cleanConfig(srv, t.TempDir())
	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesDeleted != len(remote) {
		t.Fatalf("deleted %d files, want %d", stats.FilesDeleted, len(remote))
	}
	for _, dir := range []string{"/www/a/b", "/www/a", "/www/e/f/g", "/www/e/f", "/www/e", "/www/sibling", "/www/sibling2"} {
		if remoteExists(t, srv, dir) {
			t.Errorf("stale directory %s still exists", dir)
		}
	}
}

func TestParallelDeletesTickWatchdogOnProgress(t *testing.T) {
	slow := &slowCmd{method: "Remove", delay: 150 * time.Millisecond}
	srv := startTestServer(t, withSlowCmd(slow))
	remote := make(map[string]string)
	for i := 0; i < 8; i++ {
		remote[fmt.Sprintf("/www/stale-%d.txt", i)] = "stale"
	}
	writeRemoteFiles(t, srv, remote)

	cfg := cleanConfig(srv, t.TempDir())
	cfg.StallTimeout = 400 * time.Millisecond
	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatalf("steady delete progress must not trip stall-timeout: %v", err)
	}
	if stats.FilesDeleted != len(remote) {
		t.Fatalf("deleted %d files, want %d", stats.FilesDeleted, len(remote))
	}
}

// A parallel sync sweep may finish several in-flight deletes after one worker
// fails. The recovery manifest and partial-progress stats must describe the
// paths that actually disappeared, regardless of completion order.
func TestSyncParallelDeleteFailureRecordsExactProgress(t *testing.T) {
	fault := &faultyNthCmd{method: "Remove", failAt: 3}
	srv := startTestServer(t, withFaultyNthCmd(fault))

	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"a.txt": "a",
		"b.txt": "b",
		"c.txt": "c",
		"d.txt": "d",
		"e.txt": "e",
	})
	cfg := syncConfig(srv, local)
	cfg.Concurrency = 4
	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	stale := []string{"b.txt", "c.txt", "d.txt", "e.txt"}
	for _, name := range stale {
		if err := os.Remove(filepath.Join(local, name)); err != nil {
			t.Fatal(err)
		}
	}
	fault.seen.Store(0)
	fault.enabled.Store(true)
	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("expected an injected deletion failure, got %v", err)
	}

	manifest := readRemoteManifest(t, srv)
	actuallyDeleted := 0
	for _, name := range stale {
		exists := remoteExists(t, srv, "/www/"+name)
		_, recorded := manifest.Files[name]
		if exists != recorded {
			t.Errorf("%s: remote exists = %t, recovery manifest contains entry = %t", name, exists, recorded)
		}
		if !exists {
			actuallyDeleted++
		}
	}
	if stats.FilesDeleted != actuallyDeleted {
		t.Errorf("partial deletion stats = %d, actually deleted = %d", stats.FilesDeleted, actuallyDeleted)
	}
	if actuallyDeleted < 2 {
		t.Fatalf("actually deleted %d files before the third request failed, want at least 2", actuallyDeleted)
	}
}

func cleanConfig(srv *testServer, local string) *config.Config {
	cfg := baseConfig(srv)
	cfg.Concurrency = 4
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategyClean}}
	return cfg
}
