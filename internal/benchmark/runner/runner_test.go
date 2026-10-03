package runner_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/benchmark/runner"
)

// TestMain is two binaries in one (the same trick the driver tests use): with
// EASYSFTP_RUNNER_DUMP set, this process is the "measured build" the tests
// below start, and it answers by writing the environment it received to the
// file the variable names and exiting. Without it, the test suite runs.
//
// The dump marker travels through the childEnv allowlist like any other
// EASYSFTP_* variable, which is itself part of what the tests assert.
func TestMain(m *testing.M) {
	if dump := os.Getenv("EASYSFTP_RUNNER_DUMP"); dump != "" {
		var b strings.Builder
		env := os.Environ()
		sort.Strings(env)
		for _, kv := range env {
			b.WriteString(kv)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(dump, []byte(b.String()), 0o644); err != nil {
			panic("dumping the child environment: " + err.Error())
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

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

// childEnvOf runs one build of this very test binary and returns the
// environment it received, as a map. The build exits through TestMain before
// any test runs, so this measures exactly what runner.Do hands a candidate.
func childEnvOf(t *testing.T, r *runner.Runner, run runner.Run) map[string]string {
	t.Helper()
	dump := filepath.Join(t.TempDir(), "child-env.txt")
	t.Setenv("EASYSFTP_RUNNER_DUMP", dump)

	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	run.Binary = binary

	if _, err := r.Do(run); err != nil {
		t.Fatalf("running the child: %v", err)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the child wrote no environment dump: %v", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		name, value, _ := strings.Cut(line, "=")
		if _, dup := env[name]; dup {
			t.Errorf("the child received %s twice", name)
		}
		env[name] = value
	}
	return env
}

// The measured build may be a candidate pull request, so it gets an
// allowlisted environment, not this process's: the benchmark job's own
// variables (the server's credentials, the sweep axes, whatever else the step
// exports) must stay with the parent (issue #283).
func TestChildEnvironmentIsAnAllowlist(t *testing.T) {
	// Parent variables a build has no business seeing.
	t.Setenv("BENCH_HOST", "bench.example.invalid")
	t.Setenv("BENCH_USERNAME", "bench-user")
	t.Setenv("BENCH_PASSWORD", "bench-password")
	t.Setenv("MATRIX_SCENARIOS", "small large")
	t.Setenv("REMOTE_BASE", "/easysftp-benchmark")
	t.Setenv("RUNNER_TEMP", "/tmp/runner")

	dir := t.TempDir()
	r := &runner.Runner{
		LogDir: dir,
		Server: runner.Server{
			Host: "sftp.example.invalid", Port: 2222, Username: "deployer",
			Password: "secret", KnownHosts: "sftp.example.invalid ssh-ed25519 AAAA",
		},
	}
	env := childEnvOf(t, r, runner.Run{
		Source:  "/payload",
		Remote:  "/target",
		Mode:    "overlay",
		Log:     filepath.Join(dir, "run.log"),
		Outputs: filepath.Join(dir, "run.out"),
	})

	for _, name := range []string{
		"BENCH_HOST", "BENCH_USERNAME", "BENCH_PASSWORD",
		"MATRIX_SCENARIOS", "REMOTE_BASE", "RUNNER_TEMP",
	} {
		if _, leaked := env[name]; leaked {
			t.Errorf("the benchmark's own %s reached the measured build", name)
		}
	}

	// What a build does get: the inline inputs the Runner set, and
	// GITHUB_OUTPUT pointing at the file its step outputs belong in.
	for name, want := range map[string]string{
		"EASYSFTP_HOST":        "sftp.example.invalid",
		"EASYSFTP_PORT":        "2222",
		"EASYSFTP_USERNAME":    "deployer",
		"EASYSFTP_PASSWORD":    "secret",
		"EASYSFTP_KNOWN_HOSTS": "sftp.example.invalid ssh-ed25519 AAAA",
		"EASYSFTP_SOURCE":      "/payload",
		"EASYSFTP_TARGET":      "/target",
		"EASYSFTP_MODE":        "overlay",
		"GITHUB_OUTPUT":        filepath.Join(dir, "run.out"),
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}

	// The dump marker is an EASYSFTP_* variable of the parent, so its arrival
	// proves the prefix passes through the allowlist at all -- which is what
	// keeps the driver tests' stub re-execution working.
	if env["EASYSFTP_RUNNER_DUMP"] == "" {
		t.Error("the EASYSFTP_* prefix did not pass through to the child")
	}
}

// The allowlist passes every EASYSFTP_* of the parent through, because the
// driver tests' stub marker travels that way -- but the Runner owns the
// deploy's own names: a hostile parent export of EASYSFTP_SOURCE can neither
// shadow the harness-provided value on an inline run nor turn a config-file
// run into the env-plus-config combination easySFTP refuses on purpose.
func TestTheHarnessOwnsTheDeployVariables(t *testing.T) {
	t.Setenv("EASYSFTP_SOURCE", "/hostile-parent-source")
	t.Setenv("EASYSFTP_HOST", "hostile-parent-host.example.invalid")

	// An inline run: the Runner's values win.
	dir := t.TempDir()
	r := &runner.Runner{
		LogDir: dir,
		Server: runner.Server{Host: "sftp.example.invalid", Port: 22, Username: "u", Password: "p", KnownHosts: "k"},
	}
	env := childEnvOf(t, r, runner.Run{
		Source:  "/payload",
		Remote:  "/target",
		Mode:    "overlay",
		Log:     filepath.Join(dir, "run.log"),
		Outputs: filepath.Join(dir, "run.out"),
	})
	if env["EASYSFTP_SOURCE"] != "/payload" {
		t.Errorf("EASYSFTP_SOURCE = %q, want the harness-provided /payload", env["EASYSFTP_SOURCE"])
	}
	if env["EASYSFTP_HOST"] != "sftp.example.invalid" {
		t.Errorf("EASYSFTP_HOST = %q, want the harness-provided host", env["EASYSFTP_HOST"])
	}

	// A config-file run: the inline names are absent entirely, not stale.
	env = childEnvOf(t, r, runner.Run{
		Source:   "/payload",
		Remote:   "/target",
		Mode:     "overlay",
		Log:      filepath.Join(dir, "advanced.log"),
		Outputs:  filepath.Join(dir, "advanced.out"),
		Advanced: "connections: 2\nconcurrency: 8",
	})
	for _, name := range []string{"EASYSFTP_SOURCE", "EASYSFTP_HOST", "EASYSFTP_TARGET", "EASYSFTP_MODE"} {
		if _, present := env[name]; present {
			t.Errorf("%s reached a config-file run (value %q)", name, env[name])
		}
	}
	if env["EASYSFTP_CONFIG"] == "" {
		t.Error("the config-file run received no EASYSFTP_CONFIG")
	}
}
