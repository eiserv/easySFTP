package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	reportStats(stats, "uploaded", errors.New("upload failed"))

	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"files-uploaded=3",
		"files-deleted=1",
		"files-skipped=4",
		"bytes-uploaded=2048",
		"duration-ms=1500",
	} {
		if !strings.Contains(string(output), want) {
			t.Errorf("output does not contain %q:\n%s", want, output)
		}
	}

	summary, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| Status | ❌ Failed after 3 file(s), 2048 byte(s) |",
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
