// Command easysftp is the entry point of the easySFTP GitHub Action.
// It reads its configuration from EASYSFTP_* environment variables
// (set by action.yml) and uploads files to an SFTP server.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/gha"
	"github.com/eiserv/easySFTP/internal/metrics"
	"github.com/eiserv/easySFTP/internal/uploader"
)

type ghaLogger struct{}

var buildVersion string

func (ghaLogger) Infof(format string, args ...any)    { gha.Infof(format, args...) }
func (ghaLogger) Warningf(format string, args ...any) { gha.Warningf(format, args...) }
func (ghaLogger) Group(name string)                   { gha.Group(name) }
func (ghaLogger) EndGroup()                           { gha.EndGroup() }

func main() {
	if helpRequested(os.Args[1:]) {
		fmt.Print("easySFTP uploads files to an SFTP server using EASYSFTP_* environment variables.\n\nUsage:\n  easysftp\n  easysftp --help\n")
		return
	}
	if err := run(); err != nil {
		gha.Errorf("%v", err)
		os.Exit(1)
	}
}

func helpRequested(args []string) bool {
	return len(args) == 1 && (args[0] == "--help" || args[0] == "-h")
}

func run() error {
	logBuildInfo()

	// Benchmark instrumentation, off unless the variable names a file. It is
	// deliberately not an action.yml input: it is a measurement hook for
	// scripts/benchmark*.sh, not a feature of the action, and the file it
	// writes is a workflow artifact, never part of a deploy. See
	// internal/metrics and benchmarks/README.md.
	metrics.Start(os.Getenv("EASYSFTP_METRICS_FILE"))
	defer metrics.Write()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stats, runErr := uploader.Run(ctx, cfg, ghaLogger{})
	metrics.Set("files_uploaded", int64(stats.FilesUploaded))
	metrics.Set("files_deleted", int64(stats.FilesDeleted))
	metrics.Set("files_skipped", int64(stats.FilesSkipped))
	metrics.Set("bytes_uploaded", stats.BytesUploaded)
	if runErr != nil {
		metrics.Count("errors", 1)
	}
	mode := "uploaded"
	if cfg.DryRun {
		mode = "would upload (dry-run)"
	}
	if runErr == nil {
		gha.Infof("done: %s %d file(s), %s, deleted %d file(s), skipped %d unchanged, took %s",
			mode, stats.FilesUploaded, humanBytes(stats.BytesUploaded), stats.FilesDeleted, stats.FilesSkipped, stats.Duration.Round(time.Millisecond))
	}

	reportStats(cfg, stats, mode, runErr)
	return runErr
}

// hostKeyStatus describes how the run verified the server's identity, for
// the job summary.
func hostKeyStatus(cfg *config.Config) string {
	status := "❌ NOT verified (allow-any-host-key)"
	if cfg.HostKeyPinned() {
		status = "✅ pinned"
	}
	if p := cfg.Proxy; p != nil {
		proxyStatus := "❌ NOT verified (allow_any_host_key)"
		if len(p.HostKeyFingerprints) > 0 || p.KnownHosts != "" {
			proxyStatus = "✅ pinned"
		}
		status += ", proxy: " + proxyStatus
	}
	return status
}

func logBuildInfo() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if line := buildInfoLine(info, buildVersion); line != "" {
		gha.Infof("%s", line)
	}
}

func buildInfoLine(info *debug.BuildInfo, version string) string {
	if info == nil {
		return ""
	}

	for _, setting := range info.Settings {
		if setting.Key != "vcs.revision" || setting.Value == "" {
			continue
		}

		revision := setting.Value
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if version == "" {
			version = info.Main.Version
		}
		if version == "" {
			version = "(devel)"
		}
		return fmt.Sprintf("easySFTP %s (%s)", version, revision)
	}

	return ""
}

// humanBytes renders a byte count readably without losing the exact figure,
// e.g. "70.0 MiB (73,400,320 bytes)". Job summaries get copy-pasted into
// reports, so the precise number stays available (issue #16). Below one KiB
// the raw count is already readable and stands alone.
func humanBytes(n int64) string {
	if n < 1024 {
		return uploader.HumanSize(n)
	}
	return fmt.Sprintf("%s (%s bytes)", uploader.HumanSize(n), groupDigits(n))
}

// groupDigits inserts thousands separators: 73400320 -> "73,400,320".
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 && s[i-1] != '-' {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func reportStats(cfg *config.Config, stats *uploader.Stats, mode string, runErr error) {
	status := "✅ Succeeded"
	if runErr != nil {
		status = fmt.Sprintf("❌ Failed after %d file(s), %s", stats.FilesUploaded, humanBytes(stats.BytesUploaded))
	}

	gha.SetOutput("files-uploaded", fmt.Sprintf("%d", stats.FilesUploaded))
	gha.SetOutput("files-deleted", fmt.Sprintf("%d", stats.FilesDeleted))
	gha.SetOutput("files-skipped", fmt.Sprintf("%d", stats.FilesSkipped))
	gha.SetOutput("bytes-uploaded", fmt.Sprintf("%d", stats.BytesUploaded))
	gha.SetOutput("duration-ms", fmt.Sprintf("%d", stats.Duration.Milliseconds()))

	configSource := "inline inputs"
	if cfg.ConfigPath != "" {
		configSource = fmt.Sprintf("`%s` (version 3)", cfg.ConfigPath)
	}
	summary := fmt.Sprintf(
		"### easySFTP\n\n| Metric | Value |\n|---|---|\n| Status | %s |\n| Host key | %s |\n| Configuration | %s |\n| Files %s | %d |\n| Files deleted | %d |\n| Files skipped (unchanged) | %d |\n| Bytes transferred | %s |\n| Duration | %s |\n",
		status, hostKeyStatus(cfg), configSource, mode, stats.FilesUploaded, stats.FilesDeleted, stats.FilesSkipped, humanBytes(stats.BytesUploaded), stats.Duration.Round(time.Millisecond))
	summary += deploymentBreakdown(stats.Deployments)
	gha.AppendSummary(summary)
}

// deploymentBreakdown renders a per-deployment table, or "" when there is
// only one unnamed deployment (its row would just repeat the totals above).
func deploymentBreakdown(deployments []uploader.DeploymentStats) string {
	if len(deployments) < 2 && (len(deployments) == 0 || deployments[0].Name == "") {
		return ""
	}

	var b strings.Builder
	// The per-deployment rows use the compact size only: the exact byte count
	// belongs in the totals above, and repeating it in every row would make
	// the table unreadably wide.
	b.WriteString("\n#### Deployments\n\n| Deployment | Source | Target | Mode | Uploaded | Deleted | Skipped | Size | Duration |\n|---|---|---|---|---|---|---|---|---|\n")

	var totalUploaded, totalDeleted, totalSkipped int
	var totalBytes int64
	for _, d := range deployments {
		name := d.Name
		if name == "" {
			name = "(inline)"
		}
		fmt.Fprintf(&b, "| %s | `%s` | `%s` | %s | %d | %d | %d | %s | %s |\n",
			name, d.Local, d.Remote, d.Strategy, d.FilesUploaded, d.FilesDeleted, d.FilesSkipped,
			uploader.HumanSize(d.BytesUploaded), d.Duration.Round(time.Millisecond))
		totalUploaded += d.FilesUploaded
		totalDeleted += d.FilesDeleted
		totalSkipped += d.FilesSkipped
		totalBytes += d.BytesUploaded
	}
	if len(deployments) > 1 {
		fmt.Fprintf(&b, "| **Total** | | | | **%d** | **%d** | **%d** | **%s** | |\n",
			totalUploaded, totalDeleted, totalSkipped, uploader.HumanSize(totalBytes))
	}
	return b.String()
}
