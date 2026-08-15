package uploader

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/metrics"
)

// metricsReport is the shape cmd/easysftp-bench reads back. It is redeclared
// here (instead of importing metrics.Report) so that a field rename in the
// metrics package fails this test rather than silently changing the JSON the
// benchmark harness parses.
type metricsReport struct {
	SchemaVersion int `json:"schema_version"`
	Process       struct {
		WallMS         float64 `json:"wall_ms"`
		PeakGoroutines int64   `json:"go_peak_goroutines"`
		NetReadBytes   int64   `json:"net_read_bytes"`
		NetWriteBytes  int64   `json:"net_write_bytes"`
	} `json:"process"`
	Phases []struct {
		Name   string  `json:"name"`
		WallMS float64 `json:"wall_ms"`
	} `json:"phases"`
	Operations []struct {
		Name  string `json:"name"`
		Count int64  `json:"count"`
	} `json:"operations"`
	Counters map[string]int64 `json:"counters"`
}

func runWithMetrics(t *testing.T, cfg *config.Config) metricsReport {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.json")
	metrics.Start(path)
	// Write clears the global recorder; the cleanup covers a failed run that
	// never reached it, so one test can never leak metrics into the next.
	t.Cleanup(metrics.Write)

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	metrics.Write()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the metrics file: %v", err)
	}
	var report metricsReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("parsing the metrics file: %v", err)
	}
	return report
}

func phaseMS(r metricsReport, name string) (float64, bool) {
	for _, p := range r.Phases {
		if p.Name == name {
			return p.WallMS, true
		}
	}
	return 0, false
}

func opCount(r metricsReport, name string) (int64, bool) {
	for _, o := range r.Operations {
		if o.Name == name {
			return o.Count, true
		}
	}
	return 0, false
}

// TestMetricsRecordsPhasesAndRoundTrips is the end-to-end check of the
// instrumentation: a real (in-process) upload must produce the phases and the
// per-round-trip operation counts the benchmark reports, with the counts
// matching the file count exactly. Those numbers are what tells a reader of a
// small-file benchmark which round-trip dominates, so a miscount is a wrong
// conclusion, not a cosmetic bug.
func TestMetricsRecordsPhasesAndRoundTrips(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 6)

	cfg := baseConfig(srv)
	cfg.Concurrency = 2
	cfg.Connections = 2
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	report := runWithMetrics(t, cfg)

	if report.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", report.SchemaVersion)
	}
	for _, name := range []string{"local_scan", "connect", "create_dirs", "upload"} {
		ms, ok := phaseMS(report, name)
		if !ok {
			t.Errorf("phase %q is missing", name)
			continue
		}
		if ms < 0 {
			t.Errorf("phase %q has a negative duration %v", name, ms)
		}
	}

	// One open, one write and one rename per file. Chmod runs per file too;
	// this fake server ignores the bits (see CLAUDE.md) but still answers.
	for _, name := range []string{"sftp_open", "sftp_write", "sftp_rename", "file_upload"} {
		if got, ok := opCount(report, name); !ok || got != 6 {
			t.Errorf("operation %q count = %d (present: %v), want 6", name, got, ok)
		}
	}

	if report.Counters["files_uploaded"] != 0 {
		// Set by cmd/easysftp, not by the uploader: this test drives Run
		// directly, so the run-level totals are deliberately absent here.
		t.Errorf("files_uploaded = %d, want 0 when Run is called directly", report.Counters["files_uploaded"])
	}
	if got := report.Counters["connections_opened"]; got != 2 {
		t.Errorf("connections_opened = %d, want 2", got)
	}
	if got := report.Counters["config_concurrency"]; got != 2 {
		t.Errorf("config_concurrency = %d, want 2", got)
	}
	if report.Counters["retries"] != 0 || report.Counters["reconnects"] != 0 {
		t.Errorf("a clean run recorded retries/reconnects: %v", report.Counters)
	}

	// The counting transport is only wired in when metrics are on, so this is
	// also the check that dialSSH's instrumented path really carried the run.
	if report.Process.NetReadBytes <= 0 || report.Process.NetWriteBytes <= 0 {
		t.Errorf("network byte counters look unpopulated: %+v", report.Process)
	}
	if report.Process.PeakGoroutines < 1 {
		t.Errorf("peak goroutines = %d, want at least 1", report.Process.PeakGoroutines)
	}
}

// TestMetricsRecordsSyncPhases covers the phases only the sync strategy has;
// they are what shows whether a sync run spends its time hashing locally or
// talking to the server.
func TestMetricsRecordsSyncPhases(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 4)

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategySync}}

	report := runWithMetrics(t, cfg)

	for _, name := range []string{"manifest_read", "hash", "manifest_write"} {
		if _, ok := phaseMS(report, name); !ok {
			t.Errorf("phase %q is missing from a sync run", name)
		}
	}
}

// TestMetricsDisabledByDefault: the production path must record nothing. A
// stray Start somewhere in the call chain would silently put the counting
// transport into every real deploy.
func TestMetricsDisabledByDefault(t *testing.T) {
	if metrics.Enabled() {
		t.Fatal("metrics are enabled without EASYSFTP_METRICS_FILE")
	}
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 2)

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if metrics.Enabled() {
		t.Error("a run turned metrics on by itself")
	}
}
