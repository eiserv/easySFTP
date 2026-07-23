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
	"strings"
	"syscall"
	"time"

	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/gha"
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

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stats, runErr := uploader.Run(ctx, cfg, ghaLogger{})
	mode := "uploaded"
	if cfg.DryRun {
		mode = "would upload (dry-run)"
	}
	if runErr == nil {
		gha.Infof("done: %s %d file(s), %d byte(s), deleted %d file(s), skipped %d unchanged, took %s",
			mode, stats.FilesUploaded, stats.BytesUploaded, stats.FilesDeleted, stats.FilesSkipped, stats.Duration.Round(time.Millisecond))
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

func reportStats(cfg *config.Config, stats *uploader.Stats, mode string, runErr error) {
	status := "✅ Succeeded"
	if runErr != nil {
		status = fmt.Sprintf("❌ Failed after %d file(s), %d byte(s)", stats.FilesUploaded, stats.BytesUploaded)
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
		"### easySFTP\n\n| Metric | Value |\n|---|---|\n| Status | %s |\n| Host key | %s |\n| Configuration | %s |\n| Files %s | %d |\n| Files deleted | %d |\n| Files skipped (unchanged) | %d |\n| Bytes transferred | %d |\n| Duration | %s |\n",
		status, hostKeyStatus(cfg), configSource, mode, stats.FilesUploaded, stats.FilesDeleted, stats.FilesSkipped, stats.BytesUploaded, stats.Duration.Round(time.Millisecond))
	summary += deploymentBreakdown(stats.Targets)
	gha.AppendSummary(summary)
}

// deploymentBreakdown renders a per-deployment table, or "" when there is
// only one unnamed deployment (its row would just repeat the totals above).
func deploymentBreakdown(targets []uploader.TargetStats) string {
	if len(targets) < 2 && (len(targets) == 0 || targets[0].Name == "") {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n#### Deployments\n\n| Deployment | Source | Target | Mode | Uploaded | Deleted | Skipped | Bytes | Duration |\n|---|---|---|---|---|---|---|---|---|\n")

	var totalUploaded, totalDeleted, totalSkipped int
	var totalBytes int64
	for _, t := range targets {
		name := t.Name
		if name == "" {
			name = "(inline)"
		}
		fmt.Fprintf(&b, "| %s | `%s` | `%s` | %s | %d | %d | %d | %d | %s |\n",
			name, t.Local, t.Remote, t.Strategy, t.FilesUploaded, t.FilesDeleted, t.FilesSkipped, t.BytesUploaded,
			t.Duration.Round(time.Millisecond))
		totalUploaded += t.FilesUploaded
		totalDeleted += t.FilesDeleted
		totalSkipped += t.FilesSkipped
		totalBytes += t.BytesUploaded
	}
	if len(targets) > 1 {
		fmt.Fprintf(&b, "| **Total** | | | | **%d** | **%d** | **%d** | **%d** | |\n",
			totalUploaded, totalDeleted, totalSkipped, totalBytes)
	}
	return b.String()
}
