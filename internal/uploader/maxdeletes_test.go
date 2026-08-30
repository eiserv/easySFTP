package uploader

import (
	"context"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

// TestMaxDeletesCountsDirectories: safety.max_deletes used to be handed only
// the file count, so a clean run against a tree of empty directories walked
// straight past the limit (issue #237).
func TestMaxDeletesCountsDirectories(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "new"})

	// One file and four directories on the server: five removals in total.
	seedRemoteFile(t, srv, "/www", "old.txt", "old")
	for _, dir := range []string{"/www/a", "/www/b", "/www/c", "/www/d"} {
		if err := srv.verifyClient(t).MkdirAll(dir); err != nil {
			t.Fatal(err)
		}
	}

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategyClean}}
	cfg.Safety.MaxDeletes = 3

	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "safety.max_deletes") {
		t.Fatalf("expected the guard to refuse 5 removals against a limit of 3, got %v", err)
	}
	// Refused before it removed anything, which is what "guard" has to mean.
	if !remoteExists(t, srv, "/www/old.txt") || !remoteExists(t, srv, "/www/a") {
		t.Error("the run removed entries before refusing")
	}
}

// TestMaxDeletesReportsDirectoriesRemoved is the reporting half of the same
// issue: directory removals were absent from the run's statistics, so the job
// summary and the outputs under-reported what the run did.
func TestMaxDeletesReportsDirectoriesRemoved(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "new"})

	seedRemoteFile(t, srv, "/www", "old.txt", "old")
	seedRemoteFile(t, srv, "/www/gone", "stale.txt", "stale")

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategyClean}}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesDeleted != 2 {
		t.Errorf("FilesDeleted = %d, want 2", stats.FilesDeleted)
	}
	if stats.DirsDeleted != 1 {
		t.Errorf("DirsDeleted = %d, want 1 (/www/gone)", stats.DirsDeleted)
	}
}

// TestMaxDeletesIsRunWide: the check used to sit inside the per-deployment
// execute functions, so a config file with N deployments allowed N times the
// limit. The key lives under a run-wide safety: section.
func TestMaxDeletesIsRunWide(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "new"})

	seedRemoteFile(t, srv, "/one", "a.txt", "a")
	seedRemoteFile(t, srv, "/one", "b.txt", "b")
	seedRemoteFile(t, srv, "/two", "c.txt", "c")
	seedRemoteFile(t, srv, "/two", "d.txt", "d")

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{
		{Name: "one", Local: local, Remote: "/one", Strategy: config.StrategyClean},
		{Name: "two", Local: local, Remote: "/two", Strategy: config.StrategyClean},
	}
	// Each deployment deletes two files: within the limit on its own, over it
	// for the run.
	cfg.Safety.MaxDeletes = 3

	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "safety.max_deletes") {
		t.Fatalf("expected the second deployment to exhaust the run-wide budget, got %v", err)
	}
	if !strings.Contains(err.Error(), "already deleted earlier in this run") {
		t.Errorf("the error should say the budget was already partly spent, got %q", err)
	}
	// The first deployment ran; the second was refused before deleting.
	if remoteExists(t, srv, "/one/a.txt") {
		t.Error("the first deployment did not run")
	}
	if !remoteExists(t, srv, "/two/c.txt") {
		t.Error("the second deployment deleted before the guard refused it")
	}
}

// TestDeleteBudgetUnlimitedByDefault pins the documented default: 0 is
// unlimited, and that is still what a config that says nothing gets.
func TestDeleteBudgetUnlimitedByDefault(t *testing.T) {
	b := newDeleteBudget(&config.Config{})
	if err := b.reserve(1_000_000); err != nil {
		t.Fatalf("an unset limit must not refuse anything, got %v", err)
	}
	if !b.take() {
		t.Fatal("an unset limit must not run out")
	}
}

// TestDeleteBudgetTakeAndRefund covers the sync prune's accounting: a
// directory that could not be removed must not spend budget, or a prune over a
// deep tree would stop far short of the limit the user set.
func TestDeleteBudgetTakeAndRefund(t *testing.T) {
	b := newDeleteBudget(&config.Config{Safety: config.Safety{MaxDeletes: 2}})
	if !b.take() {
		t.Fatal("first take must succeed")
	}
	b.refund()
	if !b.take() || !b.take() {
		t.Fatal("a refunded charge must be available again")
	}
	if b.take() {
		t.Fatal("the budget must run out after its limit")
	}
}
