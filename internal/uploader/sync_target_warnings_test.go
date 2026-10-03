package uploader

import (
	"context"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

// TestRunWarnsWhenCleanTargetOverlapsSyncTarget pins the run-level warning
// for the overlap config.validate cannot refuse: a sync deployment whose
// target is inside another deployment's clean target (issue #278). clean is
// documented to wipe everything under its target, so outlawing it would
// break a legitimate "clean the build dir, sync the site" setup; the warning
// makes the order-dependence visible instead. The two runs pin what the
// order actually does when the clean runs first: it deletes the sync
// manifest along with the files, so the sync starts with no manifest to
// compare against and re-uploads the whole site on every run.
func TestRunWarnsWhenCleanTargetOverlapsSyncTarget(t *testing.T) {
	srv := startTestServer(t)
	site := t.TempDir()
	writeTree(t, site, map[string]string{"index.html": "site"})
	empty := t.TempDir()

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{
		{Name: "reset", Local: empty, Remote: "/www", Strategy: config.StrategyClean},
		{Name: "site", Local: site, Remote: "/www", Strategy: config.StrategySync},
	}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	var warned bool
	for _, w := range log.warnings {
		if strings.Contains(w, "site") && strings.Contains(w, "reset") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected a warning naming both deployments, got %v", log.warnings)
	}
	if len(stats.Deployments) != 2 {
		t.Fatalf("both deployments must execute, got %d entries: %+v", len(stats.Deployments), stats.Deployments)
	}
	reset, siteStats := stats.Deployments[0], stats.Deployments[1]
	if reset.Name != "reset" || siteStats.Name != "site" {
		t.Fatalf("deployment order changed: %q then %q", reset.Name, siteStats.Name)
	}
	if reset.FilesDeleted != 0 || siteStats.FilesUploaded != 1 || siteStats.FilesDeleted != 0 {
		t.Fatalf("run 1 against an empty target: reset deleted %d, site uploaded %d deleted %d",
			reset.FilesDeleted, siteStats.FilesUploaded, siteStats.FilesDeleted)
	}

	// Second run, same config: the clean goes first and takes the manifest
	// with it, so the sync starts with no manifest and re-uploads the whole
	// site even though nothing changed locally.
	stats, err = Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	reset, siteStats = stats.Deployments[0], stats.Deployments[1]
	if reset.FilesDeleted != 2 {
		t.Errorf("run 2: the clean should delete run 1's file and the manifest, deleted %d", reset.FilesDeleted)
	}
	if siteStats.FilesUploaded != 1 || siteStats.FilesSkipped != 0 {
		t.Errorf("run 2: with no manifest the sync should re-upload the whole site, uploaded %d skipped %d",
			siteStats.FilesUploaded, siteStats.FilesSkipped)
	}
	if !remoteExists(t, srv, "/www/index.html") {
		t.Error("run 2: the sync should have re-uploaded index.html")
	}
}

// TestRunWarnsWhenCleanTargetIsInsideSyncTarget pins the reverse nesting:
// the clean deletes files the sync uploaded and still lists in its manifest,
// and a manifest-based sync never re-uploads an unchanged file (issue #278).
// The second run pins that claim end to end: the manifest survives a clean
// of a subdirectory, both local files are unchanged, so the sync skips both
// and the file the clean deleted stays missing on the server.
func TestRunWarnsWhenCleanTargetIsInsideSyncTarget(t *testing.T) {
	srv := startTestServer(t)
	site := t.TempDir()
	writeTree(t, site, map[string]string{"index.html": "site", "cache/ttl.txt": "5m"})
	empty := t.TempDir()

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{
		{Name: "site", Local: site, Remote: "/www", Strategy: config.StrategySync},
		{Name: "reset", Local: empty, Remote: "/www/cache", Strategy: config.StrategyClean},
	}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	var warned bool
	for _, w := range log.warnings {
		if strings.Contains(w, "never re-uploads an unchanged file") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected the clean-inside-sync warning, got %v", log.warnings)
	}
	siteStats, reset := stats.Deployments[0], stats.Deployments[1]
	if siteStats.FilesUploaded != 2 || reset.FilesDeleted != 1 {
		t.Fatalf("run 1: site uploaded %d (want 2), reset deleted %d (want 1, the cache file)",
			siteStats.FilesUploaded, reset.FilesDeleted)
	}
	if remoteExists(t, srv, "/www/cache/ttl.txt") {
		t.Fatal("run 1: the clean should have deleted the cache file")
	}

	stats, err = Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	siteStats, reset = stats.Deployments[0], stats.Deployments[1]
	if siteStats.FilesUploaded != 0 || siteStats.FilesSkipped != 2 {
		t.Errorf("run 2: the surviving manifest should skip both unchanged files, uploaded %d skipped %d",
			siteStats.FilesUploaded, siteStats.FilesSkipped)
	}
	if reset.FilesDeleted != 0 {
		t.Errorf("run 2: the clean target is already gone, deleted %d", reset.FilesDeleted)
	}
	if remoteExists(t, srv, "/www/cache/ttl.txt") {
		t.Error("run 2: the deleted file must stay missing until its local content changes")
	}
}

// TestRunIsQuietWithoutTargetOverlaps makes sure the new warnings do not
// fire on an ordinary multi-deployment run. The assertion matches the
// warning wording rather than an issue reference, so rewording the message
// cannot silently stop the test from testing anything; the allow-any-host-key
// warning this config legitimately triggers is not an overlap warning.
func TestRunIsQuietWithoutTargetOverlaps(t *testing.T) {
	srv := startTestServer(t)
	site := t.TempDir()
	empty := t.TempDir()
	writeTree(t, site, map[string]string{"index.html": "site"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{
		{Name: "site", Local: site, Remote: "/www", Strategy: config.StrategySync},
		{Name: "reset", Local: empty, Remote: "/fresh", Strategy: config.StrategyClean},
	}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}
	for _, w := range log.warnings {
		if strings.Contains(w, "overlap") || strings.Contains(w, "inside the sync target") {
			t.Errorf("disjoint targets should not warn, got %v", w)
		}
	}
}
