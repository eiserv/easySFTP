package uploader

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/config"
)

// setLocalMtimes stamps every given relative path under root with mtime.
func setLocalMtimes(t *testing.T, root string, mtime time.Time, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(rel)), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPreserveTimesRequestsLocalMtime(t *testing.T) {
	rec := &chtimesRecorder{}
	srv := startTestServer(t, withChtimesRecorder(rec))
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi", "sub/a.txt": "a"})
	want := time.Date(2020, 5, 4, 3, 2, 1, 0, time.UTC)
	setLocalMtimes(t, local, want, "index.html", "sub/a.txt")

	cfg := baseConfig(srv)
	cfg.PreserveTimes = true
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	got := map[string]int64{}
	rec.mu.Lock()
	for _, c := range rec.calls {
		got[c.path] = c.mtime
	}
	rec.mu.Unlock()
	for _, p := range []string{"/www/index.html", "/www/sub/a.txt"} {
		if got[p] != want.Unix() {
			t.Errorf("expected mtime %d requested for %s, got %d (calls: %+v)", want.Unix(), p, got[p], got)
		}
	}
}

func TestPreserveTimesOffByDefault(t *testing.T) {
	rec := &chtimesRecorder{}
	srv := startTestServer(t, withChtimesRecorder(rec))
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 0 {
		t.Errorf("expected no modification-time requests without preserve-times, got %+v", rec.calls)
	}
}

func TestPreserveTimesFailureWarnsOnceNotPerFile(t *testing.T) {
	srv := startTestServer(t, withFailSetstat())
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1", "b.txt": "2", "c.txt": "3"})

	cfg := baseConfig(srv)
	cfg.PreserveTimes = true
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("a rejected time change must not fail the deploy: %v", err)
	}
	if stats.FilesUploaded != 3 {
		t.Errorf("expected 3 uploads, got %d", stats.FilesUploaded)
	}
	warned := 0
	for _, w := range log.warnings {
		if strings.Contains(w, "preserve the modification time") {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("expected exactly 1 preserve-times warning, got %d: %v", warned, log.warnings)
	}
}
