package uploader

import (
	"context"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

// countInfos returns how many recorded info lines contain substr.
func countInfos(log *recordingLogger, substr string) int {
	log.mu.Lock()
	defer log.mu.Unlock()
	n := 0
	for _, i := range log.infos {
		if strings.Contains(i, substr) {
			n++
		}
	}
	return n
}

func TestNormalSuppressesPerFileLines(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1", "b.txt": "2"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategySync}}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 2 {
		t.Errorf("expected 2 uploads, got %d", stats.FilesUploaded)
	}
	if n := countInfos(log, "upload "); n != 0 {
		t.Errorf("expected no per-file upload lines at the default level, got %d: %v", n, log.infos)
	}
	if n := countInfos(log, "sync: "); n != 1 {
		t.Errorf("expected the per-deployment sync summary to survive the default level, got %d", n)
	}
	if n := countInfos(log, "deployment "); n != 1 {
		t.Errorf("expected one deployment summary line, got %d: %v", n, log.infos)
	}
	if n := countInfos(log, "connecting to "); n != 1 {
		t.Errorf("expected the connection line to survive the default level, got %d", n)
	}
}

func TestNormalDryRunStillLogsThePlan(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1"})

	cfg := baseConfig(srv)
	cfg.DryRun = true
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}
	if n := countInfos(log, "would upload "); n != 1 {
		t.Errorf("dry-run must log the plan regardless of log-level, got %d plan lines: %v", n, log.infos)
	}
}

func TestVerboseLogsPerFileLines(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "x", "debug.log": "y"})

	cfg := baseConfig(srv)
	cfg.LogLevel = config.LogVerbose
	cfg.IgnoreLines = []string{"*.log"}
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}
	if n := countInfos(log, "upload "); n != 1 {
		t.Errorf("expected the per-file upload line at verbose level, got %d", n)
	}
	if n := countInfos(log, "ignore pattern"); n != 0 {
		t.Errorf("expected no exclude-decision lines at verbose level, got %d: %v", n, log.infos)
	}
}

func TestDebugExplainsExcludeDecisions(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "x", "debug.log": "y"})

	cfg := baseConfig(srv)
	cfg.LogLevel = config.LogDebug
	cfg.IgnoreLines = []string{"*.log"}
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}
	if n := countInfos(log, `skip debug.log (ignore pattern "*.log")`); n != 1 {
		t.Errorf("expected one exclude-decision line, got %d: %v", n, log.infos)
	}
	if n := countInfos(log, "upload "); n != 1 {
		t.Errorf("expected the per-file upload line at debug level, got %d", n)
	}
}
