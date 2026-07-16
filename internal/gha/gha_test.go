package gha

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseGithubOutput parses the file-based GITHUB_OUTPUT format (both the
// plain "name=value" and "name<<delimiter" heredoc forms) into a map, the
// same way the actual GitHub Actions runner does.
func parseGithubOutput(t *testing.T, raw string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	lines := strings.Split(raw, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == "" {
			continue
		}
		if idx := strings.Index(line, "<<"); idx != -1 {
			name := line[:idx]
			delimiter := line[idx+2:]
			var value []string
			i++
			for i < len(lines) && lines[i] != delimiter {
				value = append(value, lines[i])
				i++
			}
			out[name] = strings.Join(value, "\n")
			continue
		}
		if idx := strings.Index(line, "="); idx != -1 {
			out[line[:idx]] = line[idx+1:]
		}
	}
	return out
}

func TestSetOutputRoundTripsSpecialCharacters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output")
	t.Setenv("GITHUB_OUTPUT", path)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("failed to create output file: %v", err)
	}

	cases := map[string]string{
		"plain":     "42",
		"percent":   "100% done",
		"multiline": "line one\nline two\nline three",
		"crlf":      "line one\r\nline two",
	}
	for name, value := range cases {
		SetOutput(name, value)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}
	got := parseGithubOutput(t, string(raw))

	for name, want := range cases {
		if got[name] != want {
			t.Errorf("output %q: got %q, want %q", name, got[name], want)
		}
	}
}

func TestSetOutputIsNoopWithoutGithubOutput(t *testing.T) {
	t.Setenv("GITHUB_OUTPUT", "")
	// Must not panic or attempt to open an empty path.
	SetOutput("name", "value")
}
