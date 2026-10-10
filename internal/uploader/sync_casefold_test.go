package uploader

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A case-only rename (Readme.md -> README.md) on a case-insensitive server
// is one directory entry: if sync deletes the old spelling after uploading
// the new one, the Remove takes out the file the run just wrote, and the
// run finishes green with the file gone. The old spelling must go before
// the upload, like every other collision (issue #313).
func TestSyncCaseOnlyRenameSurvivesOnCaseInsensitiveServer(t *testing.T) {
	srv := startTestServer(t, withCaseInsensitive())
	local := t.TempDir()
	writeTree(t, local, map[string]string{"Readme.md": "hello"})

	// First sync deploys Readme.md and records it in the manifest.
	if _, err := Run(context.Background(), syncConfig(srv, local), testLogger{t}); err != nil {
		t.Fatal(err)
	}

	// Case-only rename, content unchanged.
	if err := os.Rename(filepath.Join(local, "Readme.md"), filepath.Join(local, "README.md")); err != nil {
		t.Fatal(err)
	}

	stats, err := Run(context.Background(), syncConfig(srv, local), testLogger{t})
	if err != nil {
		t.Fatal(err)
	}

	// The file the run uploaded must still be on the server, readable by
	// its new spelling. On the unpatched code the post-upload delete of
	// Readme.md removed it (both spellings are one entry), the run reported
	// uploaded=1 deleted=1, and every later sync skipped the file as
	// unchanged while it stayed missing.
	if got := readRemote(t, srv, "/www/README.md"); got != "hello" {
		t.Fatalf("case-renamed file lost on a case-insensitive server: read %q, want %q", got, "hello")
	}
	if stats.FilesUploaded != 1 || stats.FilesDeleted != 1 {
		t.Fatalf("second sync: up=%d del=%d, want 1/1 (upload the new spelling, drop the old)", stats.FilesUploaded, stats.FilesDeleted)
	}

	// A third sync must see nothing left to do: the manifest now records the
	// new spelling, so the run settles instead of re-losing the file.
	stats, err = Run(context.Background(), syncConfig(srv, local), testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 0 || stats.FilesDeleted != 0 || stats.FilesSkipped != 1 {
		t.Fatalf("third sync: up=%d del=%d skip=%d, want 0/0/1 (converged)", stats.FilesUploaded, stats.FilesDeleted, stats.FilesSkipped)
	}
	if got := readRemote(t, srv, "/www/README.md"); got != "hello" {
		t.Fatalf("file vanished on the settled run: read %q, want %q", got, "hello")
	}
}

// The directory variant from the issue: Assets/logo.png -> assets/logo.png.
// The re-cased directory must not be taken out by the post-delete prune, and
// the file must survive under the new spelling.
func TestSyncCaseOnlyDirectoryRenameSurvivesOnCaseInsensitiveServer(t *testing.T) {
	srv := startTestServer(t, withCaseInsensitive())
	local := t.TempDir()
	writeTree(t, local, map[string]string{"Assets/logo.png": "logo"})

	if _, err := Run(context.Background(), syncConfig(srv, local), testLogger{t}); err != nil {
		t.Fatal(err)
	}

	// Change the directory's case. On a case-insensitive local filesystem
	// (NTFS, the common CI runner) a one-step rename is a no-op: the
	// directory keeps its original spelling, and the walked plan still
	// reports Assets/logo.png, which is exactly the manifest key. The
	// two-step dance through a neutral name is what makes both kinds of
	// local filesystem record the new spelling, so the plan really carries
	// assets/logo.png against the manifest's Assets/logo.png.
	staging := filepath.Join(local, "dir-case-staging")
	if err := os.Rename(filepath.Join(local, "Assets"), staging); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, filepath.Join(local, "assets")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), syncConfig(srv, local), testLogger{t}); err != nil {
		t.Fatal(err)
	}

	if got := readRemote(t, srv, "/www/assets/logo.png"); got != "logo" {
		t.Fatalf("case-renamed directory's file lost: read %q, want %q", got, "logo")
	}
	if got := readRemote(t, srv, "/www/Assets/logo.png"); got != "logo" {
		t.Fatalf("old directory spelling stopped serving the file: read %q, want %q (same entry)", got, "logo")
	}
}

// The fold must not swallow a real delete: a file that was removed locally
// with no case-relative planned file must still go, even on a
// case-insensitive server.
func TestSyncStillDeletesGenuinelyRemovedFilesOnCaseInsensitiveServer(t *testing.T) {
	srv := startTestServer(t, withCaseInsensitive())
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "a", "b.txt": "b"})

	if _, err := Run(context.Background(), syncConfig(srv, local), testLogger{t}); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(local, "b.txt")); err != nil {
		t.Fatal(err)
	}

	stats, err := Run(context.Background(), syncConfig(srv, local), testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesDeleted != 1 {
		t.Fatalf("second sync deletes: got %d, want 1 (b.txt)", stats.FilesDeleted)
	}
	if remoteExists(t, srv, "/www/b.txt") {
		t.Fatal("removed file b.txt is still on the server")
	}
	if got := readRemote(t, srv, "/www/a.txt"); got != "a" {
		t.Fatalf("a.txt disturbed: %q", got)
	}
}

// On a case-SENSITIVE server the old spelling is a genuinely different file,
// and the pre-upload removal must still clean it up; nothing must be left
// behind under the old name.
func TestSyncCaseOnlyRenameOnCaseSensitiveServerRemovesOldSpelling(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"Readme.md": "hello"})

	if _, err := Run(context.Background(), syncConfig(srv, local), testLogger{t}); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(filepath.Join(local, "Readme.md"), filepath.Join(local, "README.md")); err != nil {
		t.Fatal(err)
	}

	stats, err := Run(context.Background(), syncConfig(srv, local), testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if got := readRemote(t, srv, "/www/README.md"); got != "hello" {
		t.Fatalf("new spelling missing: %q", got)
	}
	if remoteExists(t, srv, "/www/Readme.md") {
		t.Fatal("old spelling left behind on a case-sensitive server")
	}
	if stats.FilesUploaded != 1 || stats.FilesDeleted != 1 {
		t.Fatalf("second sync: up=%d del=%d, want 1/1", stats.FilesUploaded, stats.FilesDeleted)
	}
}
