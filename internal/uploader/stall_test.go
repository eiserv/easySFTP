package uploader

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/config"
)

// The watchdog must fire only when transfers are active and silent: not while
// idle, not while progress ticks arrive.
func TestStallWatchdogFiresOnlyWhenActiveAndSilent(t *testing.T) {
	killed := make(chan struct{})
	w := startStallWatchdog(300*time.Millisecond, func() { close(killed) }, testLogger{t})
	defer w.stop()

	// Idle (no active transfers): must not fire, no matter how long.
	select {
	case <-killed:
		t.Fatal("watchdog fired while no transfer was active")
	case <-time.After(600 * time.Millisecond):
	}

	// Active with steady progress: must not fire.
	w.begin()
	stopTicks := make(chan struct{})
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopTicks:
				return
			case <-tick.C:
				w.ticks.Add(1)
			}
		}
	}()
	select {
	case <-killed:
		t.Fatal("watchdog fired despite steady progress")
	case <-time.After(600 * time.Millisecond):
	}

	// Active and silent: must fire within a few check intervals.
	close(stopTicks)
	select {
	case <-killed:
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not fire on a silent active transfer")
	}
	if !w.fired.Load() {
		t.Error("fired flag not set after the watchdog killed the connection")
	}
	w.end()
}

// A server that accepts the connection and then stops making progress
// mid-transfer must fail the run quickly with a clear stall error, instead of
// blocking until an outer timeout.
func TestStallTimeoutFailsFast(t *testing.T) {
	// The server stops reading after 256 KiB but keeps the connection open.
	srv := startTestServer(t, withStallAfter(256*1024))

	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"big.bin": strings.Repeat("x", 2<<20), // 2 MiB, far beyond the stall point
	})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
	cfg.Concurrency = 1
	// One in-flight request per file: reads stop promptly once the server
	// stops acknowledging, making the stall visible to the watchdog fast.
	cfg.SftpRequestConcurrency = 1
	cfg.StallTimeout = 1 * time.Second

	log := &recordingLogger{testLogger: testLogger{t}}
	start := time.Now()
	_, err := Run(context.Background(), cfg, log)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("expected a transfer-stalled error, got %v", err)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("run took %s; the stall-timeout should have failed it fast", elapsed)
	}
	found := false
	for _, w := range log.warnings {
		if strings.Contains(w, "no transfer progress") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a stall warning, got %v", log.warnings)
	}
}

// With stall-timeout unset (the default) nothing changes: a normal deploy
// against a healthy server runs without a watchdog.
func TestStallTimeoutOffByDefault(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 1 {
		t.Fatalf("expected 1 upload, got %d", stats.FilesUploaded)
	}
}
