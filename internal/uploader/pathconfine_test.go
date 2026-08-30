package uploader

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

// TestSafeJoinConfinesRemoteSuppliedPaths pins the helper every remote-derived
// delete path goes through. The escaping cases are the ones that matter: they
// are what a poisoned manifest key or a lying directory listing looks like.
func TestSafeJoinConfinesRemoteSuppliedPaths(t *testing.T) {
	const base = "/var/www/html"
	ok := []struct{ rel, want string }{
		{"index.html", "/var/www/html/index.html"},
		{"assets/app.css", "/var/www/html/assets/app.css"},
		{"a/../b.txt", "/var/www/html/b.txt"},
		{"./nested/x", "/var/www/html/nested/x"},
		{"..hidden", "/var/www/html/..hidden"},
		{"dir/..b", "/var/www/html/dir/..b"},
		// A backslash is an ordinary character in a POSIX file name and is
		// not a separator here, so it must not be treated as an escape.
		{`weird\..\name`, `/var/www/html/weird\..\name`},
	}
	for _, tc := range ok {
		got, err := safeJoin(base, tc.rel)
		if err != nil {
			t.Errorf("safeJoin(%q, %q) returned error %v, want %q", base, tc.rel, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("safeJoin(%q, %q) = %q, want %q", base, tc.rel, got, tc.want)
		}
	}

	bad := []string{
		"../etc/passwd",
		"../../../../home/other/public_html/index.php",
		"a/../../escape",
		"/etc/nginx/nginx.conf",
		"/",
		"..",
		".",
		"",
	}
	for _, rel := range bad {
		if got, err := safeJoin(base, rel); err == nil {
			t.Errorf("safeJoin(%q, %q) = %q, want an error", base, rel, got)
		}
	}

	// A relative base is resolved against the session's working directory; a
	// climb out of it is just as much an escape.
	if got, err := safeJoin("www", "../secrets"); err == nil {
		t.Errorf(`safeJoin("www", "../secrets") = %q, want an error`, got)
	}
	if got, err := safeJoin("www", "a.txt"); err != nil || got != "www/a.txt" {
		t.Errorf(`safeJoin("www", "a.txt") = %q, %v; want "www/a.txt", nil`, got, err)
	}

	// safeChild additionally refuses a separator: ReadDir returns names, so a
	// server answering with a path is not describing its own directory.
	if got, err := safeChild("/www", "sub/file.txt"); err == nil {
		t.Errorf(`safeChild("/www", "sub/file.txt") = %q, want an error`, got)
	}
	if got, err := safeChild("/www", "file.txt"); err != nil || got != "/www/file.txt" {
		t.Errorf(`safeChild("/www", "file.txt") = %q, %v; want "/www/file.txt", nil`, got, err)
	}
}

// TestSyncIgnoresManifestEntriesOutsideTheDeployment is the end-to-end case
// issue #223 describes: the manifest lives in the deploy target, so anything
// able to write that one file could name a delete target anywhere the deploy
// account can reach. The run must upload as normal and leave the named file
// alone.
func TestSyncIgnoresManifestEntriesOutsideTheDeployment(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hello"})

	seedRemoteFile(t, srv, "/home/other/public_html", "index.php", "victim")
	seedRemoteFile(t, srv, "/www", manifestName, `{
	  "version": 3,
	  "files": {
	    "gone.txt": {"hash": "abc", "size": 1, "mtime": 1},
	    "../../home/other/public_html/index.php": {"hash": "def", "size": 1, "mtime": 1},
	    "/etc/nginx/nginx.conf": {"hash": "ghi", "size": 1, "mtime": 1}
	  }
	}`)
	seedRemoteFile(t, srv, "/www", "gone.txt", "stale")

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), syncConfig(srv, local), log)
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	if !remoteExists(t, srv, "/home/other/public_html/index.php") {
		t.Fatal("a manifest key that climbs out of the deployment was followed and the file was deleted")
	}
	// The in-deployment entry is still handled normally: the guard must not
	// turn a poisoned manifest into a manifest that does nothing.
	if remoteExists(t, srv, "/www/gone.txt") {
		t.Error("the legitimate manifest entry was not deleted")
	}
	if stats.FilesDeleted != 1 {
		t.Errorf("FilesDeleted = %d, want 1 (only the in-deployment entry)", stats.FilesDeleted)
	}
	if !hasWarning(log, "point outside the deployment") {
		t.Errorf("expected a warning naming the out-of-deployment entries, got %v", log.warnings)
	}
}

// TestCleanIgnoresListingEntriesOutsideTheDirectory covers the same helper on
// the other kind of remote-supplied name: what a server answers a directory
// listing with during the clean-mode scan.
func TestCleanIgnoresListingEntriesOutsideTheDirectory(t *testing.T) {
	srv := startTestServer(t, withLyingList("/www", "sub/..", true))
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "new"})

	seedRemoteFile(t, srv, "/", "victim.txt", "do not delete me")
	seedRemoteFile(t, srv, "/www", "old.txt", "old")

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategyClean}}
	log := &recordingLogger{testLogger: testLogger{t}}

	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatalf("clean deploy failed: %v", err)
	}

	if !remoteExists(t, srv, "/victim.txt") {
		t.Fatal("the clean scan followed a listing entry that pointed out of the directory it listed")
	}
	if remoteExists(t, srv, "/www/old.txt") {
		t.Error("clean did not remove the real remote file")
	}
	if !hasWarning(log, "skipping remote entry while scanning") {
		t.Errorf("expected a warning about the skipped entry, got %v", log.warnings)
	}
}

// TestReadManifestRefusesOversizeManifest: the manifest is a remote file, so
// its size is not this run's to trust. Over the cap it degrades to a first
// sync, exactly like an unreadable one.
func TestReadManifestRefusesOversizeManifest(t *testing.T) {
	srv := startTestServer(t)
	prev := maxManifestBytes
	maxManifestBytes = 512
	t.Cleanup(func() { maxManifestBytes = prev })

	var b strings.Builder
	b.WriteString(`{"version":3,"files":{`)
	for i := 0; i < 50; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"file%02d.txt":{"hash":"h","size":1,"mtime":1}`, i)
	}
	b.WriteString(`}}`)
	seedRemoteFile(t, srv, "/www", manifestName, b.String())

	log := &recordingLogger{testLogger: testLogger{t}}
	got, err := readManifest(srv.verifyClient(t), "/www", manifestName, log)
	if err != nil {
		t.Fatalf("readManifest returned error %v, want nil", err)
	}
	if len(got.Files) != 0 {
		t.Fatalf("readManifest returned %d entries from an over-size manifest, want 0", len(got.Files))
	}
	if !hasWarning(log, "is larger than") {
		t.Errorf("expected an over-size warning, got %v", log.warnings)
	}
}

// hasWarning reports whether the logger recorded a warning containing sub.
func hasWarning(log *recordingLogger, sub string) bool {
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, w := range log.warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
