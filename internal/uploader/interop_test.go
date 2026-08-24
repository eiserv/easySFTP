package uploader

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/sftp"

	"github.com/eiserv/easySFTP/internal/config"
)

// The tests here cover servers that answer a missing path with a generic
// status code instead of SSH_FX_NO_SUCH_FILE (issue #152). easySFTP has two
// places where "not there" is the expected, benign state, and reading it as a
// refusal ends the run: the remote scan a clean deployment does before its
// target exists, and deleting a file that is already gone. Both are settled by
// a stat now; see remoteAbsent.
//
// The pairs matter more than the individual cases: each tolerant case has a
// strict sibling proving the tie-break did not turn into blanket tolerance,
// because a server that really is refusing must still fail the run loudly.

// genericStatuses are the codes a server with no specific answer for "not
// there" gives instead, the same two the posix-rename fallback accepts.
var genericStatuses = []struct {
	name string
	code error
}{
	{"SSH_FX_FAILURE", sftp.ErrSSHFxFailure},
	{"SSH_FX_BAD_MESSAGE", sftp.ErrSSHFxBadMessage},
}

// TestCleanIntoMissingTargetOnCoarseServer verifies the first clean deployment
// to a server that does not report a missing directory specifically. The scan
// finds nothing to delete and the upload proceeds, instead of the run dying in
// its remote scan before it has written anything.
func TestCleanIntoMissingTargetOnCoarseServer(t *testing.T) {
	for _, tc := range genericStatuses {
		t.Run(tc.name, func(t *testing.T) {
			srv := startTestServer(t, withCoarseStatus(tc.code))
			local := t.TempDir()
			writeTree(t, local, map[string]string{
				"index.html":       "<h1>hi</h1>",
				"assets/style.css": "body{}",
			})

			cfg := baseConfig(srv)
			cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategyClean}}

			stats, err := Run(context.Background(), cfg, testLogger{t})
			if err != nil {
				t.Fatalf("clean deployment into a target that does not exist yet failed: %v", err)
			}
			if stats.FilesUploaded != 2 {
				t.Errorf("expected 2 uploads, got %d", stats.FilesUploaded)
			}
			if stats.FilesDeleted != 0 {
				t.Errorf("expected nothing to delete in an empty target, got %d", stats.FilesDeleted)
			}
			if got := readRemote(t, srv, "/www/index.html"); got != "<h1>hi</h1>" {
				t.Errorf("unexpected content: %q", got)
			}
		})
	}
}

// TestRefusedScanStillFailsTheRun is the strict half: a target that is there
// and whose listing the server refuses must still fail, because a clean
// deployment that silently skipped its scan would leave the stale files it
// exists to remove.
func TestRefusedScanStillFailsTheRun(t *testing.T) {
	srv := startTestServer(t, withFailList("/www"))
	seedRemoteFile(t, srv, "/www", "stale.html", "stale")

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "<h1>hi</h1>"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategyClean}}

	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "scanning remote directory") {
		t.Fatalf("expected the refused scan to fail the run, got %v", err)
	}
	if !remoteExists(t, srv, "/www/stale.html") {
		t.Error("the stale file was deleted by a run whose scan never succeeded")
	}
}

// TestSyncDeletesAlreadyGoneFileOnCoarseServer verifies the second sync of a
// deployment whose file was removed on the server out of band. The manifest
// still lists it, so sync deletes it; the server answers that there is nothing
// there, which is the outcome sync wanted rather than a failure.
func TestSyncDeletesAlreadyGoneFileOnCoarseServer(t *testing.T) {
	for _, tc := range genericStatuses {
		t.Run(tc.name, func(t *testing.T) {
			srv := startTestServer(t, withCoarseStatus(tc.code))
			local := t.TempDir()
			writeTree(t, local, map[string]string{
				"index.html": "<h1>hi</h1>",
				"old.css":    "body{}",
			})

			cfg := baseConfig(srv)
			cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategySync}}

			if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
				t.Fatalf("first sync: %v", err)
			}

			// Gone locally (so sync deletes it) and gone remotely (so the
			// delete finds nothing), which is what someone tidying up on the
			// server between two deploys leaves behind.
			if err := os.Remove(filepath.Join(local, "old.css")); err != nil {
				t.Fatal(err)
			}
			if err := srv.verifyClient(t).Remove("/www/old.css"); err != nil {
				t.Fatal(err)
			}

			stats, err := Run(context.Background(), cfg, testLogger{t})
			if err != nil {
				t.Fatalf("second sync: %v", err)
			}
			if stats.FilesDeleted != 1 {
				t.Errorf("expected the already-gone file to count as deleted, got %d", stats.FilesDeleted)
			}
		})
	}
}

// TestRefusedDeleteStillFailsTheRun is the strict half of the delete
// tie-break: a file that is still there after the server refused to remove it
// must fail the run, not be reported as deleted.
func TestRefusedDeleteStillFailsTheRun(t *testing.T) {
	faulty := &faultyPathCmd{method: "Remove", path: "/www/stale.html"}
	faulty.enabled.Store(true)
	srv := startTestServer(t, withFaultyPath(faulty))
	seedRemoteFile(t, srv, "/www", "stale.html", "stale")

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "<h1>hi</h1>"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategyClean}}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), `deleting "/www/stale.html"`) {
		t.Fatalf("expected the refused delete to fail the run, got %v", err)
	}
	if stats.FilesDeleted != 0 {
		t.Errorf("a refused delete was counted as deleted: %d", stats.FilesDeleted)
	}
	if !remoteExists(t, srv, "/www/stale.html") {
		t.Error("the file the server refused to remove is gone")
	}
}

// TestFirstSyncIsQuietOnCoarseServer verifies the run says nothing about the
// manifest on the one run where there cannot be one. The behaviour was always
// right (an unreadable manifest degrades to a first sync), but a server that
// does not report a missing file specifically made it warn about the expected
// state on every first sync of every sync deployment.
func TestFirstSyncIsQuietOnCoarseServer(t *testing.T) {
	srv := startTestServer(t, withCoarseStatus(sftp.ErrSSHFxFailure))
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "<h1>hi</h1>"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategySync}}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	for _, w := range log.warnings {
		if strings.Contains(w, "sync manifest") {
			t.Errorf("first sync warned about the manifest that cannot exist yet: %q", w)
		}
	}
}

// TestUnreadableManifestOnCoarseServerStillWarns is the strict half: a
// manifest that is there and cannot be read is a real problem even on a server
// whose status code does not say which of the two it is, and the run has to
// say so before it re-uploads everything and deletes nothing.
func TestUnreadableManifestOnCoarseServerStillWarns(t *testing.T) {
	srv := startTestServer(t,
		withCoarseStatus(sftp.ErrSSHFxFailure),
		withFailOpen("/www/"+manifestName),
	)
	seedRemoteFile(t, srv, "/www", manifestName, `{"version":3,"files":{}}`)

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := readManifest(srv.verifyClient(t), "/www", manifestName, log); err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if len(log.warnings) != 1 || !strings.Contains(log.warnings[0], "could not open sync manifest") {
		t.Fatalf("expected one manifest open warning, got %v", log.warnings)
	}
}
