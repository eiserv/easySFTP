package runner_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/benchmark/runner"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// StepNumber reads the GITHUB_OUTPUT heredoc format, and answers 0 rather than
// failing where a run died before writing the value.
func TestStepNumber(t *testing.T) {
	dir := t.TempDir()
	outputs := write(t, dir, "run.out", strings.Join([]string{
		"files-uploaded<<EOF", "300", "EOF",
		"bytes-uploaded<<EOF", "1228800", "EOF",
		"duration-ms<<EOF", "412", "EOF",
	}, "\n")+"\n")

	for _, tc := range []struct {
		key  string
		want float64
	}{
		{"files-uploaded", 300},
		{"bytes-uploaded", 1228800},
		{"duration-ms", 412},
		{"files-deleted", 0}, // never written
	} {
		if got := runner.StepNumber(outputs, tc.key); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.key, got, tc.want)
		}
	}

	if got := runner.StepNumber(filepath.Join(dir, "missing.out"), "duration-ms"); got != 0 {
		t.Errorf("a run that wrote no outputs reported %v", got)
	}
	// Anything that is not a plain number is a run that failed halfway, not a
	// value: a partial heredoc must not become a measurement.
	broken := write(t, dir, "broken.out", "duration-ms<<EOF\nnot-a-number\nEOF\n")
	if got := runner.StepNumber(broken, "duration-ms"); got != 0 {
		t.Errorf("a non-numeric output reported %v", got)
	}
}

func TestCountLines(t *testing.T) {
	dir := t.TempDir()
	log := write(t, dir, "run.log", strings.Join([]string{
		"uploading 300 files",
		"::warning::retrying after a dropped connection",
		"reconnecting to the server",
		"a line that says retrying and reconnecting at once",
		"::error::the deploy failed",
		"note: ::error:: is not at the start here",
		"could not open connection 3 of 4",
	}, "\n")+"\n")

	// A line counts once, however many of the needles it holds.
	if got := runner.CountLines(log, runner.ContainsAny("retrying", "reconnecting")); got != 3 {
		t.Errorf("counted %v retry lines, want 3", got)
	}
	// The error pattern is anchored: a mention mid-line is not an error.
	if got := runner.CountLines(log, runner.HasPrefix("::error::")); got != 1 {
		t.Errorf("counted %v error lines, want 1", got)
	}
	if got := runner.CountLines(log, runner.ContainsAny("could not open connection")); got != 1 {
		t.Errorf("counted %v refused connections, want 1", got)
	}
	if got := runner.CountLines(filepath.Join(dir, "missing.log"), runner.HasPrefix("::error::")); got != 0 {
		t.Errorf("a run that wrote no log reported %v", got)
	}
}

// ReadMetrics keeps the document as the run wrote it: the measuring half has no
// reason to understand it, and a counter this repository does not model yet
// must reach the aggregation intact.
func TestReadMetrics(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "metrics.json", `{"schema_version": 1, "counters": {"something_new": 7}}`)
	got := string(runner.ReadMetrics(path))
	if !strings.Contains(got, `"something_new":7`) {
		t.Errorf("metrics came back as %q", got)
	}

	if runner.ReadMetrics(filepath.Join(dir, "missing.json")) != nil {
		t.Error("a run that wrote no metrics reported some")
	}
	broken := write(t, dir, "broken.json", "{not json")
	if runner.ReadMetrics(broken) != nil {
		t.Error("an unreadable metrics document was passed on")
	}
}

// A run with an advanced block goes through a generated config file, because
// advanced.* settings have no inputs and inline inputs may not be combined with
// a config file. That file names the host and the user, so it lands in the log
// directory and never in the artifact.
func TestConfigFileIsWrittenToTheLogDir(t *testing.T) {
	dir := t.TempDir()
	r := &runner.Runner{
		LogDir: dir,
		Server: runner.Server{
			Host: "sftp.example.invalid", Port: 2222, Username: "deployer",
			Password: "secret", KnownHosts: "sftp.example.invalid ssh-ed25519 AAAA\nsecond line",
		},
	}
	// A binary that does not exist fails to start, which is an error rather
	// than an exit code; the config file is written before that happens.
	_, _ = r.Do(runner.Run{
		Binary:   filepath.Join(dir, "no-such-binary"),
		Source:   "/payload",
		Remote:   "/target",
		Mode:     "overlay",
		Log:      filepath.Join(dir, "run.log"),
		Outputs:  filepath.Join(dir, "run.out"),
		Advanced: "connections: 2\nconcurrency: 8",
	})

	data, err := os.ReadFile(filepath.Join(dir, "config.yml"))
	if err != nil {
		t.Fatalf("the config file was not written: %v", err)
	}
	config := string(data)
	for _, needle := range []string{
		"version: 3\n",
		"  host: \"sftp.example.invalid\"\n",
		"  port: 2222\n",
		"  known_hosts: |\n    sftp.example.invalid ssh-ed25519 AAAA\n    second line\n",
		"    source: \"/payload\"\n",
		"    mode: overlay\n",
		"advanced:\n  connections: 2\n  concurrency: 8\n",
	} {
		if !strings.Contains(config, needle) {
			t.Errorf("the config file is missing %q:\n%s", needle, config)
		}
	}
}
