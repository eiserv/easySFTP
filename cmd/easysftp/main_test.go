package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/uploader"
)

func TestHelpRequested(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "long", args: []string{"--help"}, want: true},
		{name: "short", args: []string{"-h"}, want: true},
		{name: "none", args: nil, want: false},
		{name: "extra", args: []string{"--help", "unexpected"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := helpRequested(tt.args); got != tt.want {
				t.Fatalf("helpRequested(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestBuildInfoLine(t *testing.T) {
	tests := []struct {
		name    string
		info    *debug.BuildInfo
		version string
		want    string
	}{
		{
			name: "version and revision",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v1.2.3"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
				},
			},
			want: "easySFTP v1.2.3 (0123456789ab)",
		},
		{
			name: "injected release version",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "0123456789abcdef"},
				},
			},
			version: "v1.2.3",
			want:    "easySFTP v1.2.3 (0123456789ab)",
		},
		{
			name: "short revision and unknown version",
			info: &debug.BuildInfo{
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "abc123"},
				},
			},
			want: "easySFTP (devel) (abc123)",
		},
		{
			name: "missing revision",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v1.2.3"},
			},
		},
		{
			name: "build info unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildInfoLine(tt.info, tt.version); got != tt.want {
				t.Fatalf("buildInfoLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReportStatsOnFailure(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "output")
	summaryPath := filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_OUTPUT", outputPath)
	t.Setenv("GITHUB_STEP_SUMMARY", summaryPath)

	stats := &uploader.Stats{
		FilesUploaded: 3,
		FilesDeleted:  1,
		FilesSkipped:  4,
		BytesUploaded: 2048,
		Duration:      1500 * time.Millisecond,
	}
	cfg := &config.Config{HostKeyFingerprints: []string{"SHA256:abc"}}
	reportStats(cfg, stats, "uploaded", errors.New("upload failed"))

	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	got := parseGithubOutput(t, string(output))
	for name, want := range map[string]string{
		"files-uploaded": "3",
		"files-deleted":  "1",
		"files-skipped":  "4",
		"bytes-uploaded": "2048",
		"duration-ms":    "1500",
	} {
		if got[name] != want {
			t.Errorf("output %q = %q, want %q:\n%s", name, got[name], want, output)
		}
	}

	summary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| Status | ❌ Failed after 3 file(s), 2048 byte(s) |",
		"| Host key | ✅ pinned |",
		"| Configuration | inline inputs |",
		"| Files uploaded | 3 |",
		"| Files deleted | 1 |",
		"| Files skipped (unchanged) | 4 |",
		"| Bytes transferred | 2048 |",
	} {
		if !strings.Contains(string(summary), want) {
			t.Errorf("summary does not contain %q:\n%s", want, summary)
		}
	}
}

func TestReportStatsMultiTargetBreakdown(t *testing.T) {
	summaryPath := filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))
	t.Setenv("GITHUB_STEP_SUMMARY", summaryPath)

	stats := &uploader.Stats{
		FilesUploaded: 252,
		FilesDeleted:  217,
		FilesSkipped:  1988,
		BytesUploaded: 17_825_792,
		Duration:      2*time.Minute + 13*time.Second,
		Targets: []uploader.TargetStats{
			{Name: "website", Local: "./dist/", Remote: "/var/www/html/", Strategy: "sync", FilesUploaded: 12, FilesDeleted: 3, FilesSkipped: 1988, BytesUploaded: 4_297_523, Duration: time.Second},
			{Name: "documentation", Local: "./docs/", Remote: "/var/www/docs/", Strategy: "clean", FilesUploaded: 240, FilesDeleted: 214, FilesSkipped: 0, BytesUploaded: 13_528_269, Duration: 2 * time.Second},
		},
	}
	cfg := &config.Config{ConfigPath: ".github/easysftp.yml", KnownHosts: "line"}
	reportStats(cfg, stats, "uploaded", nil)

	summary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| Configuration | `.github/easysftp.yml` (version 3) |",
		"#### Deployments",
		"| Deployment | Source | Target | Mode | Uploaded | Deleted | Skipped | Bytes | Duration |",
		"| website | `./dist/` | `/var/www/html/` | sync | 12 | 3 | 1988 | 4297523 | 1s |",
		"| documentation | `./docs/` | `/var/www/docs/` | clean | 240 | 214 | 0 | 13528269 | 2s |",
		"| **Total** | | | | **252** | **217** | **1988** | **17825792** | |",
	} {
		if !strings.Contains(string(summary), want) {
			t.Errorf("summary does not contain %q:\n%s", want, summary)
		}
	}
}

func TestReportStatsSingleInlineTargetHasNoBreakdown(t *testing.T) {
	summaryPath := filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))
	t.Setenv("GITHUB_STEP_SUMMARY", summaryPath)

	stats := &uploader.Stats{
		FilesUploaded: 3, BytesUploaded: 2048, Duration: time.Second,
		Targets: []uploader.TargetStats{{Local: "./dist/", Remote: "/www/", Strategy: "overlay", FilesUploaded: 3, BytesUploaded: 2048}},
	}
	reportStats(&config.Config{AllowAnyHostKey: true}, stats, "uploaded", nil)

	summary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(summary), "#### Deployments") {
		t.Errorf("expected no per-deployment breakdown for a single inline target:\n%s", summary)
	}
	if !strings.Contains(string(summary), "| Host key | ❌ NOT verified (allow-any-host-key) |") {
		t.Errorf("expected the unverified host key status in the summary:\n%s", summary)
	}
}

func TestReportStatsSingleNamedDeploymentGetsBreakdown(t *testing.T) {
	summaryPath := filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "output"))
	t.Setenv("GITHUB_STEP_SUMMARY", summaryPath)

	stats := &uploader.Stats{
		FilesUploaded: 3, BytesUploaded: 2048, Duration: time.Second,
		Targets: []uploader.TargetStats{{Name: "website", Local: "./dist/", Remote: "/www/", Strategy: "sync", FilesUploaded: 3, BytesUploaded: 2048}},
	}
	reportStats(&config.Config{ConfigPath: "x.yml", KnownHosts: "line"}, stats, "uploaded", nil)

	summary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), "| website | `./dist/` | `/www/` | sync | 3 | 0 | 0 | 2048 | 0s |") {
		t.Errorf("expected the named deployment row in the summary:\n%s", summary)
	}
}

// parseGithubOutput parses the file-based GITHUB_OUTPUT "name<<delimiter"
// heredoc format into a map, the same way the GitHub Actions runner does.
func parseGithubOutput(t *testing.T, raw string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	lines := strings.Split(raw, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		idx := strings.Index(line, "<<")
		if idx == -1 {
			continue
		}
		name := line[:idx]
		delimiter := line[idx+2:]
		var value []string
		i++
		for i < len(lines) && lines[i] != delimiter {
			value = append(value, lines[i])
			i++
		}
		out[name] = strings.Join(value, "\n")
	}
	return out
}
