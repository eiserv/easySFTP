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
	"syscall"

	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/gha"
	"github.com/eiserv/easySFTP/internal/uploader"
)

type ghaLogger struct{}

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
			mode, stats.FilesUploaded, stats.BytesUploaded, stats.FilesDeleted, stats.FilesSkipped, stats.Duration.Round(1e6))
	}

	reportStats(stats, mode, runErr)
	return runErr
}

func logBuildInfo() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if line := buildInfoLine(info); line != "" {
		gha.Infof("%s", line)
	}
}

func buildInfoLine(info *debug.BuildInfo) string {
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
		version := info.Main.Version
		if version == "" {
			version = "(devel)"
		}
		return fmt.Sprintf("easySFTP %s (%s)", version, revision)
	}

	return ""
}

func reportStats(stats *uploader.Stats, mode string, runErr error) {
	status := "✅ Succeeded"
	if runErr != nil {
		status = fmt.Sprintf("❌ Failed after %d file(s), %d byte(s)", stats.FilesUploaded, stats.BytesUploaded)
	}

	gha.SetOutput("files-uploaded", fmt.Sprintf("%d", stats.FilesUploaded))
	gha.SetOutput("files-deleted", fmt.Sprintf("%d", stats.FilesDeleted))
	gha.SetOutput("files-skipped", fmt.Sprintf("%d", stats.FilesSkipped))
	gha.SetOutput("bytes-uploaded", fmt.Sprintf("%d", stats.BytesUploaded))
	gha.SetOutput("duration-ms", fmt.Sprintf("%d", stats.Duration.Milliseconds()))

	gha.AppendSummary(fmt.Sprintf(
		"### easySFTP\n\n| Metric | Value |\n|---|---|\n| Status | %s |\n| Files %s | %d |\n| Files deleted | %d |\n| Files skipped (unchanged) | %d |\n| Bytes transferred | %d |\n| Duration | %s |\n",
		status, mode, stats.FilesUploaded, stats.FilesDeleted, stats.FilesSkipped, stats.BytesUploaded, stats.Duration.Round(1e6)))
}
