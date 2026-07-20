package uploader

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

// TestSyncCustomManifestName runs a sync round-trip with a non-default
// manifest name: the manifest must be written and read back under that name,
// the default name must not appear, and a local file that happens to share
// the custom name must not be uploaded over it.
func TestSyncCustomManifestName(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"index.html":         "one",
		"gone.txt":           "temporary",
		"manifest-8f3a.json": "local decoy, must not be uploaded",
	})

	cfg := baseConfig(srv)
	cfg.ManifestName = "manifest-8f3a.json"
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategySync}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if remoteExists(t, srv, "/www/"+manifestName) {
		t.Errorf("default manifest %s must not exist with a custom manifest-name", manifestName)
	}
	got := readRemote(t, srv, "/www/manifest-8f3a.json")
	if !strings.Contains(got, `"version"`) || !strings.Contains(got, "index.html") {
		t.Errorf("custom-name manifest is not the sync manifest (local decoy uploaded over it?): %q", got)
	}

	// Second sync with a file removed: only a manifest read back under the
	// custom name can know the file must be deleted remotely.
	if err := os.Remove(filepath.Join(local, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesDeleted != 1 {
		t.Errorf("expected 1 deletion tracked via the custom-name manifest, got %d", stats.FilesDeleted)
	}
	if remoteExists(t, srv, "/www/gone.txt") {
		t.Error("gone.txt should have been deleted by the second sync")
	}
	if stats.FilesSkipped == 0 {
		t.Error("expected unchanged files to be skipped, so the manifest was read back")
	}
}
