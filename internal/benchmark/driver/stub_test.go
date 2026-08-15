// The stub the tests in this package measure instead of a real easySFTP build.
//
// The driver does not care what the binary it runs is: it sets EASYSFTP_*, plus
// GITHUB_OUTPUT and EASYSFTP_METRICS_FILE, and runs it. So this writes
// plausible step outputs and a plausible metrics file, derived from the two
// things the driver varies (the source tree and, from the generated config
// file, advanced.*).
//
// It is the Go form of the stub the shell self-check wrote, and it answers the
// same way. That is what made the two comparable during the parity check of
// issue #190 step 5, when both implementations were driven against this very
// binary and their JSONL diffed.
//
// It is a *mode of the test binary* rather than a program of its own: TestMain
// re-executes this binary with stubMarker set (see driver_test.go), so nothing
// has to be compiled at test time and there is no second main package to keep
// building.
package driver_test

import (
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// stubMarker turns this binary into the stub. Its value is read before any test
// runs, so a child process never re-enters the test suite.
const stubMarker = "EASYSFTP_BENCH_STUB"

func stubMain() {

	source := os.Getenv("EASYSFTP_SOURCE")
	target := os.Getenv("EASYSFTP_TARGET")
	mode := os.Getenv("EASYSFTP_MODE")
	connections, concurrency, requests := 1, 4, 16
	skipUnchanged := false

	if config := os.Getenv("EASYSFTP_CONFIG"); config != "" {
		data, err := os.ReadFile(config)
		if err != nil {
			fmt.Fprintf(os.Stderr, "::error::%v\n", err)
			os.Exit(1)
		}
		text := string(data)
		source = stubQuoted(text, "    source:")
		target = stubQuoted(text, "    target:")
		mode = stubBare(text, "    mode:")
		// "auto" resolves to easySFTP's own defaults here, exactly as autoInt.or
		// does in internal/config/configfile.go: an auto run has to come out
		// somewhere on the grid, otherwise the regret arithmetic has nothing to
		// compare.
		connections = stubSetting(text, "  connections:", 1)
		concurrency = stubSetting(text, "  concurrency:", 4)
		requests = stubSetting(text, "  request_concurrency:", 16)
		skipUnchanged = strings.Contains(text, "\n  skip_unchanged: true")
	}

	// Into the run log, which is what the checks on the deploy shapes read: the
	// mode and skip_unchanged are chosen per scenario, and nothing else in the
	// output would show which ones a cell actually ran with.
	if mode == "" {
		mode = "none"
	}
	fmt.Printf("stub: mode=%s skip_unchanged=%v source=%s\n", mode, skipUnchanged, source)

	files, bytes := stubWalk(source)

	// Just enough remote state to make a "clean" run delete what an earlier run
	// put there: one file per remote target holding its file count. Without it
	// every pre-clean would report zero deletions, and the delete aggregation
	// (issue #184, phase 4) would have nothing to aggregate.
	deleted := 0
	if state := os.Getenv("EASYSFTP_STUB_STATE"); state != "" && target != "" {
		os.MkdirAll(state, 0o755)
		path := filepath.Join(state, strings.ReplaceAll(strings.TrimPrefix(target, "/"), "/", "_"))
		if mode == "clean" {
			if data, err := os.ReadFile(path); err == nil {
				deleted, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			}
			os.Remove(path)
		} else {
			os.WriteFile(path, []byte(strconv.Itoa(files)), 0o644)
		}
	}

	// More connections and more workers finish sooner, with a floor: exactly
	// the shape a scaling curve should have, so the matrix output can be
	// checked against a known answer.
	parallel := min(connections, concurrency)
	duration := 200 + (files+deleted)*4/parallel + rand.Intn(7)

	if outputs := os.Getenv("GITHUB_OUTPUT"); outputs != "" {
		file, err := os.OpenFile(outputs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "::error::%v\n", err)
			os.Exit(1)
		}
		for _, out := range [][2]any{
			{"files-uploaded", files}, {"bytes-uploaded", bytes},
			{"files-deleted", deleted}, {"duration-ms", duration},
		} {
			fmt.Fprintf(file, "%s<<EOF\n%v\nEOF\n", out[0], out[1])
		}
		file.Close()
	}

	if metrics := os.Getenv("EASYSFTP_METRICS_FILE"); metrics != "" {
		os.WriteFile(metrics, []byte(stubReport(mode, duration, files, bytes, deleted,
			connections, concurrency, requests, parallel)), 0o644)
	}
}

// A clean deployment runs different phases and different round-trips than an
// upload, and the delete aggregation reads exactly those.
func stubReport(mode string, duration, files int, bytes int64, deleted, connections, concurrency, requests, parallel int) string {
	phases := fmt.Sprintf(`[{"name": "upload", "wall_ms": %d, "count": 1},
    {"name": "connect", "wall_ms": 40, "count": 1},
    {"name": "local_scan", "wall_ms": 15, "count": 1},
    {"name": "create_dirs", "wall_ms": 5, "count": 1}]`, duration-60)
	operations := fmt.Sprintf(`[{"name": "file_upload", "count": %d, "errors": 0, "total_ms": %d,
     "avg_ms": 3.5, "min_ms": 1, "p50_ms": 3, "p90_ms": 6, "p99_ms": 9, "max_ms": 11},
    {"name": "sftp_open", "count": %d, "errors": 0, "total_ms": %d,
     "avg_ms": 1.7, "min_ms": 1, "p50_ms": 1.5, "p90_ms": 3, "p99_ms": 5, "max_ms": 6}]`,
		files, duration*2, files, duration)
	if mode == "clean" {
		phases = fmt.Sprintf(`[{"name": "remote_scan", "wall_ms": %d, "count": 1},
    {"name": "delete_sweep", "wall_ms": %d, "count": 1},
    {"name": "connect", "wall_ms": 40, "count": 1}]`, duration/3, duration*2/3)
		operations = fmt.Sprintf(`[{"name": "sftp_remove", "count": %d, "errors": 0, "total_ms": %d,
     "avg_ms": 2.1, "min_ms": 1, "p50_ms": 2, "p90_ms": 4, "p99_ms": 7, "max_ms": 8},
    {"name": "sftp_rmdir", "count": 8, "errors": 0, "total_ms": 24,
     "avg_ms": 3, "min_ms": 2, "p50_ms": 3, "p90_ms": 4, "p99_ms": 5, "max_ms": 5}]`,
			deleted, duration/2)
	}
	return fmt.Sprintf(`{
  "schema_version": 1,
  "note": "stub",
  "process": {
    "wall_ms": %d, "user_cpu_ms": 40, "sys_cpu_ms": 12, "cpu_percent": 26,
    "max_rss_bytes": 41943040, "go_total_alloc_bytes": 10485760, "go_mallocs": 5000,
    "go_heap_sys_bytes": 20971520, "go_gc_count": 3, "go_gc_pause_total_ms": 0.4,
    "go_peak_goroutines": %d, "go_max_procs": 4,
    "disk_read_bytes": %d, "disk_write_bytes": 0,
    "net_read_bytes": 4096, "net_write_bytes": %d
  },
  "phases": %s,
  "operations": %s,
  "counters": {
    "connections_opened": %d, "connections_used": %d,
    "connections_refused": 0, "reconnects": 0, "retries": 0, "stalls": 0, "errors": 0,
    "config_connections": %d, "config_concurrency": %d,
    "config_request_concurrency": %d,
    "files_uploaded": %d, "bytes_uploaded": %d, "files_deleted": %d
  }
}
`, duration, parallel+5, bytes, bytes, phases, operations,
		connections, connections, connections, concurrency, requests, files, bytes, deleted)
}

func stubWalk(dir string) (int, int64) {
	files, bytes := 0, int64(0)
	if dir == "" {
		return 0, 0
	}
	filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil //nolint:nilerr // a missing tree is an empty one here
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	return files, bytes
}

// quoted reads a `key: "value"` line, bare a `key: value` one.
func stubQuoted(text, key string) string {
	value := stubBare(text, key)
	return strings.Trim(value, `"`)
}

func stubBare(text, key string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, key) {
			return strings.TrimSpace(strings.TrimPrefix(line, key))
		}
	}
	return ""
}

func stubSetting(text, key string, fallback int) int {
	value := stubBare(text, key)
	if value == "" || value == "auto" {
		return fallback
	}
	if number, err := strconv.Atoi(value); err == nil {
		return number
	}
	return fallback
}
