// Command easysftp is the entry point of the easySFTP GitHub Action.
// It reads its configuration from EASYSFTP_* environment variables
// (set by action.yml) and uploads files to an SFTP server.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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
	if err := run(); err != nil {
		gha.Errorf("%v", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stats, err := uploader.Run(ctx, cfg, ghaLogger{})
	if err != nil {
		return err
	}

	mode := "uploaded"
	if cfg.DryRun {
		mode = "would upload (dry-run)"
	}
	gha.Infof("done: %s %d file(s), %d byte(s), deleted %d file(s), skipped %d unchanged, took %s",
		mode, stats.FilesUploaded, stats.BytesUploaded, stats.FilesDeleted, stats.FilesSkipped, stats.Duration.Round(1e6))

	gha.SetOutput("files-uploaded", fmt.Sprintf("%d", stats.FilesUploaded))
	gha.SetOutput("files-deleted", fmt.Sprintf("%d", stats.FilesDeleted))
	gha.SetOutput("files-skipped", fmt.Sprintf("%d", stats.FilesSkipped))
	gha.SetOutput("bytes-uploaded", fmt.Sprintf("%d", stats.BytesUploaded))
	gha.SetOutput("duration-ms", fmt.Sprintf("%d", stats.Duration.Milliseconds()))

	gha.AppendSummary(fmt.Sprintf(
		"### easySFTP\n\n| Metric | Value |\n|---|---|\n| Files %s | %d |\n| Files deleted | %d |\n| Files skipped (unchanged) | %d |\n| Bytes transferred | %d |\n| Duration | %s |\n",
		mode, stats.FilesUploaded, stats.FilesDeleted, stats.FilesSkipped, stats.BytesUploaded, stats.Duration.Round(1e6)))

	return nil
}
