// Package runner runs one build of easySFTP and reads back what it reported.
//
// It is the Go form of run_easysftp, step_number, count_matches and
// metrics_json from scripts/benchmark-lib.sh (issue #190, step 3). Nothing here
// decides *what* to measure; it only knows how to start a build, where its step
// outputs land and how to read them back.
//
// Nothing here writes into the output directory: run logs and the generated
// config file name the host and the user, and that directory is uploaded as a
// workflow artifact, where nothing is masked.
package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Server is the SFTP server every run connects to. Every field is a secret in
// the benchmark workflow, which is why the generated config file goes to LogDir
// and never to the result directory.
type Server struct {
	Host       string
	Port       int
	Username   string
	Password   string
	KnownHosts string
}

// Runner starts easySFTP builds against one server.
type Runner struct {
	Server Server

	// LogDir holds the per-run logs and the generated config file. It must not
	// be inside the directory that is uploaded as an artifact.
	LogDir string
}

// Run is one invocation of an easySFTP build.
type Run struct {
	Binary string
	Source string
	Remote string
	Mode   string

	// Log is the combined output of the run, Outputs the file the build writes
	// its step outputs into.
	Log     string
	Outputs string

	// Metrics names where the run writes its instrumentation (see
	// internal/metrics). An empty value leaves the run uninstrumented.
	Metrics string

	// Advanced is the advanced.* YAML block, without indentation. Empty means
	// an inline run.
	Advanced string
}

// Do runs one build and returns its exit code.
//
// Without an advanced block this is an inline run, exactly what a workflow with
// source/target inputs does. With one, the run goes through a generated config
// file instead: advanced.* settings have no inputs (every non-secret setting
// has exactly one home in v3), and inline inputs may not be combined with a
// config file.
//
// A non-zero exit code is returned as a code, not as an error: a failed run is
// data the benchmark records rather than a reason to stop. An error means the
// build could not be started or its files could not be written.
func (r *Runner) Do(run Run) (int, error) {
	if err := os.WriteFile(run.Outputs, nil, 0o644); err != nil {
		return 0, err
	}

	env := append(os.Environ(),
		"GITHUB_OUTPUT="+run.Outputs,
		"EASYSFTP_PASSWORD="+r.Server.Password,
	)
	if run.Metrics != "" {
		if err := os.Remove(run.Metrics); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		env = append(env, "EASYSFTP_METRICS_FILE="+run.Metrics)
	}

	if run.Advanced != "" {
		config := filepath.Join(r.LogDir, "config.yml")
		if err := os.WriteFile(config, []byte(r.configFile(run)), 0o600); err != nil {
			return 0, err
		}
		env = append(env, "EASYSFTP_CONFIG="+config)
	} else {
		env = append(env,
			"EASYSFTP_HOST="+r.Server.Host,
			"EASYSFTP_PORT="+strconv.Itoa(r.Server.Port),
			"EASYSFTP_USERNAME="+r.Server.Username,
			"EASYSFTP_KNOWN_HOSTS="+r.Server.KnownHosts,
			"EASYSFTP_SOURCE="+run.Source,
			"EASYSFTP_TARGET="+run.Remote,
			"EASYSFTP_MODE="+run.Mode,
		)
	}

	log, err := os.Create(run.Log)
	if err != nil {
		return 0, err
	}
	defer log.Close()

	cmd := exec.Command(run.Binary)
	cmd.Env = env
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return 0, fmt.Errorf("running %s: %w", run.Binary, err)
	}
	return 0, nil
}

// configFile is the v3 config a run with an advanced block goes through. It
// names the host and the user, which is why it is written to LogDir.
func (r *Runner) configFile(run Run) string {
	var b strings.Builder
	b.WriteString("version: 3\n")
	b.WriteString("connection:\n")
	fmt.Fprintf(&b, "  host: %q\n", r.Server.Host)
	fmt.Fprintf(&b, "  port: %d\n", r.Server.Port)
	fmt.Fprintf(&b, "  username: %q\n", r.Server.Username)
	b.WriteString("  known_hosts: |\n")
	b.WriteString(indent(r.Server.KnownHosts, "    "))
	b.WriteString("deployments:\n")
	b.WriteString("  benchmark:\n")
	fmt.Fprintf(&b, "    source: %q\n", run.Source)
	fmt.Fprintf(&b, "    target: %q\n", run.Remote)
	fmt.Fprintf(&b, "    mode: %s\n", run.Mode)
	b.WriteString("advanced:\n")
	b.WriteString(indent(run.Advanced, "  "))
	return b.String()
}

// indent prefixes every line of text, which is what "sed 's/^/  /'" did. The
// result always ends in a newline, because the block it produces is followed by
// the next YAML key.
func indent(text, prefix string) string {
	text = strings.TrimSuffix(text, "\n")
	lines := strings.Split(text, "\n")
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// StepNumber reads a number written in the GITHUB_OUTPUT heredoc format, 0 when
// the run died before writing it.
func StepNumber(outputs, key string) float64 {
	file, err := os.Open(outputs)
	if err != nil {
		return 0
	}
	defer file.Close()

	prefix := key + "<<"
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if !strings.HasPrefix(scanner.Text(), prefix) {
			continue
		}
		if !scanner.Scan() {
			return 0
		}
		value := scanner.Text()
		if !isDigits(value) {
			return 0
		}
		number, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0
		}
		return number
	}
	return 0
}

func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// CountLines counts the lines of a log the matcher accepts. A missing or
// unreadable log counts as zero, so a run that died before writing one still
// produces a number.
func CountLines(path string, match func(string) bool) float64 {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()

	count := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if match(scanner.Text()) {
			count++
		}
	}
	return float64(count)
}

// ContainsAny is the matcher behind "grep -e a -e b": a line counts once,
// however many of the needles it holds.
func ContainsAny(needles ...string) func(string) bool {
	return func(line string) bool {
		for _, needle := range needles {
			if strings.Contains(line, needle) {
				return true
			}
		}
		return false
	}
}

// HasPrefix is the matcher behind an anchored grep pattern.
func HasPrefix(prefix string) func(string) bool {
	return func(line string) bool { return strings.HasPrefix(line, prefix) }
}

// ReadMetrics is the run's metrics document, or nil (which encodes as JSON
// null) when the run died before writing one, or wrote something unreadable.
// Never an error: an absent metrics file is a run that failed, which the
// benchmark records rather than stops for.
//
// The document travels as the bytes it was written as, not through
// schema.Metrics. The measuring half has no reason to understand it, and a
// counter or a field this repository does not model yet must reach the
// aggregation intact rather than be dropped in passing.
func ReadMetrics(path string) json.RawMessage {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if !json.Valid(data) {
		return nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return nil
	}
	return json.RawMessage(compact.Bytes())
}
