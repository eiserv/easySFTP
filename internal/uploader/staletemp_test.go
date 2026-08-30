package uploader

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"github.com/eiserv/easySFTP/internal/config"
)

// shrinkStaleTempMaxAge makes every existing temp file immediately eligible
// for the sweep, restoring the production margin when the test ends. The
// in-memory test server stamps files with their creation time and ignores
// time changes in Setstat (see CLAUDE.md), so aging a file for real is not
// possible; shrinking the threshold is.
func shrinkStaleTempMaxAge(t *testing.T) {
	t.Helper()
	old := staleTempMaxAge
	staleTempMaxAge = 0
	t.Cleanup(func() { staleTempMaxAge = old })
}

// writeRemoteFile creates a remote file with the given content, so tests can
// plant orphaned temp files as a killed earlier run would have left them.
func writeRemoteFile(t *testing.T, client *sftp.Client, path, content string) {
	t.Helper()
	f, err := client.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestIsTempFileName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"app.js" + tmpSuffix + ".0", true},           // file upload temp
		{"app.js" + tmpSuffix + ".417", true},         // higher plan index
		{manifestName + tmpSuffix, true},              // manifest write temp (no index)
		{"app.js", false},                             // ordinary file
		{".easysftp-manifest.json", false},            // the manifest itself
		{tmpSuffix, false},                            // bare suffix, no name before it
		{"app.js" + tmpSuffix + ".backup", false},     // non-numeric suffix
		{"app.js" + tmpSuffix + ".", false},           // dot but no index
		{"app.js" + tmpSuffix + ".1a", false},         // trailing garbage
		{"app.js" + tmpSuffix + ".3" + ".txt", false}, // renamed by a user
	}
	for _, c := range cases {
		if got := isTempFileName(c.name); got != c.want {
			t.Errorf("isTempFileName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// The core of issue #160: temp files a killed run left behind are removed by
// the next deploy touching their directories, on both non-destructive modes,
// and each removal is logged so the debris is explained.
func TestSweepRemovesStaleTempFiles(t *testing.T) {
	shrinkStaleTempMaxAge(t)

	srv := startTestServer(t)
	client := srv.verifyClient(t)
	if err := client.MkdirAll("/www/sub"); err != nil {
		t.Fatal(err)
	}
	// What a SIGKILLed earlier run leaves: an indexed file-upload temp in two
	// directories, a manifest-write temp (no index), and an unrelated file
	// that must survive the sweep.
	writeRemoteFile(t, client, "/www/index.html"+tmpSuffix+".417", "partial")
	writeRemoteFile(t, client, "/www/sub/app.js"+tmpSuffix+".3", "partial")
	writeRemoteFile(t, client, "/www/"+manifestName+tmpSuffix, "partial")
	writeRemoteFile(t, client, "/www/human.txt", "do not touch")

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi", "sub/app.js": "x"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}

	for _, orphan := range []string{
		"/www/index.html" + tmpSuffix + ".417",
		"/www/sub/app.js" + tmpSuffix + ".3",
		"/www/" + manifestName + tmpSuffix,
	} {
		if remoteExists(t, srv, orphan) {
			t.Errorf("stale temp file %s survived the sweep", orphan)
		}
	}
	if got := readRemote(t, srv, "/www/human.txt"); got != "do not touch" {
		t.Errorf("unrelated file was touched by the sweep: %q", got)
	}
	if got := readRemote(t, srv, "/www/index.html"); got != "hi" {
		t.Errorf("deploy did not complete normally: %q", got)
	}
	removals := 0
	for _, msg := range log.infos {
		if strings.Contains(msg, "removed stale temporary file") {
			removals++
		}
	}
	if removals != 3 {
		t.Errorf("expected 3 removal log lines, got %d: %v", removals, log.infos)
	}
}

// A temp file younger than staleTempMaxAge is plausibly another live run's
// in-progress upload; deleting it would turn an (unsupported but real)
// concurrent deploy race into a corrupted deploy, so it must survive.
func TestSweepKeepsFreshTempFiles(t *testing.T) {
	srv := startTestServer(t)
	client := srv.verifyClient(t)
	if err := client.MkdirAll("/www"); err != nil {
		t.Fatal(err)
	}
	writeRemoteFile(t, client, "/www/index.html"+tmpSuffix+".9", "in progress")

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if !remoteExists(t, srv, "/www/index.html"+tmpSuffix+".9") {
		t.Error("a fresh temp file (possibly a live concurrent upload) was swept")
	}
}

// A real deployed file whose name happens to match the temp pattern (see the
// literal-name coverage in atomic_test.go) is a planned target of the run and
// must never be swept, even once it is old enough.
func TestSweepKeepsPlannedTargetsNamedLikeTempFiles(t *testing.T) {
	shrinkStaleTempMaxAge(t)

	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"app.js" + tmpSuffix + ".3": "real content"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	// First deploy puts the literal-named file on the server; the second
	// deploy's sweep then sees it as an existing, age-eligible temp name.
	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}

	if got := readRemote(t, srv, "/www/app.js"+tmpSuffix+".3"); got != "real content" {
		t.Errorf("planned literal-named file was lost: %q", got)
	}
	for _, msg := range log.infos {
		if strings.Contains(msg, "removed stale temporary file") {
			t.Errorf("a planned target was swept as a stale temp file: %q", msg)
		}
	}
}

// The sweep's scope guarantee: a deploy to a target several levels deep must
// not list (let alone delete in) any ancestor of that target. Age-eligible
// temp debris planted above the base survives untouched, the only directories
// ever listed are the base and the directories receiving files this run, and
// the debris inside those is removed.
func TestSweepStaysWithinDeploymentBase(t *testing.T) {
	shrinkStaleTempMaxAge(t)

	rec := &listRecorder{}
	srv := startTestServer(t, withListRecorder(rec))
	client := srv.verifyClient(t)
	if err := client.MkdirAll("/var/www/html/blog/sub"); err != nil {
		t.Fatal(err)
	}
	// Temp debris in every ancestor of the base; the sweep has no business
	// there, so all of it must survive.
	writeRemoteFile(t, client, "/var/a"+tmpSuffix+".1", "above")
	writeRemoteFile(t, client, "/var/www/b"+tmpSuffix+".2", "above")
	writeRemoteFile(t, client, "/var/www/html/c"+tmpSuffix+".3", "above")
	// Debris inside the deployment: in the base (where a manifest temp would
	// live) and in a directory receiving a file this run.
	writeRemoteFile(t, client, "/var/www/html/blog/index.html"+tmpSuffix+".4", "stale")
	writeRemoteFile(t, client, "/var/www/html/blog/sub/app.js"+tmpSuffix+".5", "stale")

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi", "sub/app.js": "x"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/var/www/html/blog"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	for _, kept := range []string{
		"/var/a" + tmpSuffix + ".1",
		"/var/www/b" + tmpSuffix + ".2",
		"/var/www/html/c" + tmpSuffix + ".3",
	} {
		if !remoteExists(t, srv, kept) {
			t.Errorf("temp file above the deployment base was swept: %s", kept)
		}
	}
	for _, swept := range []string{
		"/var/www/html/blog/index.html" + tmpSuffix + ".4",
		"/var/www/html/blog/sub/app.js" + tmpSuffix + ".5",
	} {
		if remoteExists(t, srv, swept) {
			t.Errorf("stale temp file inside the deployment survived: %s", swept)
		}
	}
	for _, dir := range rec.listed() {
		if dir != "/var/www/html/blog" && dir != "/var/www/html/blog/sub" {
			t.Errorf("a directory outside the deployment was listed: %s", dir)
		}
	}
}

// Issue #186: sync narrows the upload to the changed files, but the sweep's
// keep set must still cover the full plan. A literal-named target that is
// unchanged this run is not in the upload subset, yet its directory is swept
// because a sibling changed; removing it would be silent and permanent, since
// the manifest still records it as up to date.
func TestSyncSweepKeepsUnchangedTargetsNamedLikeTempFiles(t *testing.T) {
	shrinkStaleTempMaxAge(t)

	srv := startTestServer(t)
	local := t.TempDir()
	literal := "app.js" + tmpSuffix + ".3"
	writeTree(t, local, map[string]string{literal: "real content", "other.txt": "1"})

	cfg := syncConfig(srv, local)
	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	// Only the sibling changes, so the second sync uploads just other.txt
	// while still sweeping /www, where the unchanged literal-named file lives.
	writeTree(t, local, map[string]string{literal: "real content", "other.txt": "2"})

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}

	if got := readRemote(t, srv, "/www/"+literal); got != "real content" {
		t.Errorf("unchanged literal-named file was lost: %q", got)
	}
	for _, msg := range log.infos {
		if strings.Contains(msg, "removed stale temporary file") {
			t.Errorf("an unchanged planned target was swept as a stale temp file: %q", msg)
		}
	}

	// A third run must find it unchanged as well: had the second run deleted
	// it, the manifest hash would keep sync from restoring it.
	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if got := readRemote(t, srv, "/www/"+literal); got != "real content" {
		t.Errorf("sync did not leave the literal-named file in place: %q", got)
	}
}

// The sync strategy uploads through the same path, so a directory a sync run
// touches is swept too, and the orphan does not linger despite sync's
// manifest never having known about it.
func TestSweepRunsForSyncUploads(t *testing.T) {
	shrinkStaleTempMaxAge(t)

	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1"})

	cfg := syncConfig(srv, local)
	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	writeRemoteFile(t, srv.verifyClient(t), "/www/a.txt"+tmpSuffix+".42", "partial")
	// a.txt changes, so the next sync uploads into /www and sweeps it.
	writeTree(t, local, map[string]string{"a.txt": "2"})

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if remoteExists(t, srv, "/www/a.txt"+tmpSuffix+".42") {
		t.Error("stale temp file survived a sync deploy touching its directory")
	}
	if got := readRemote(t, srv, "/www/a.txt"); got != "2" {
		t.Errorf("sync deploy did not complete normally: %q", got)
	}
}

func TestSweepReachesPlannedDirectoryWithNoChangedFiles(t *testing.T) {
	shrinkStaleTempMaxAge(t)

	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"index.html":       "1",
		"assets/deep/a.js": "unchanged",
	})
	cfg := syncConfig(srv, local)
	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	orphan := "/www/assets/deep/old.js" + tmpSuffix + ".42"
	writeRemoteFile(t, srv.verifyClient(t), orphan, "partial")
	// No local file changed. The upload subset is empty, but the deployment's
	// full planned directory set still reaches assets/deep.
	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if remoteExists(t, srv, orphan) {
		t.Error("stale temp file survived in a planned directory with no changed files")
	}
}

// Dry runs must not delete anything, orphaned temp files included.
func TestSweepSkippedInDryRun(t *testing.T) {
	shrinkStaleTempMaxAge(t)

	srv := startTestServer(t)
	client := srv.verifyClient(t)
	if err := client.MkdirAll("/www"); err != nil {
		t.Fatal(err)
	}
	writeRemoteFile(t, client, "/www/index.html"+tmpSuffix+".7", "partial")

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
	cfg.DryRun = true

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if !remoteExists(t, srv, "/www/index.html"+tmpSuffix+".7") {
		t.Error("dry-run swept a temp file")
	}
}

// A directory listing failure must degrade to "sweep nothing there", never
// fail the deploy: the sweep is a cleanup courtesy, not part of the upload.
func TestSweepListFailureDoesNotFailDeploy(t *testing.T) {
	shrinkStaleTempMaxAge(t)

	srv := startTestServer(t, withFailList("/www"))
	client := srv.verifyClient(t)
	if err := client.MkdirAll("/www"); err != nil {
		t.Fatal(err)
	}
	writeRemoteFile(t, client, "/www/index.html"+tmpSuffix+".2", "partial")

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatalf("an unlistable directory must not fail the deploy: %v", err)
	}
	if got := readRemote(t, srv, "/www/index.html"); got != "hi" {
		t.Errorf("deploy did not complete: %q", got)
	}
}

// The sweep must not double as a stall: a healthy but slow listing pass ticks
// the watchdog per directory, mirroring createRemoteDirs. This is a cheap
// guard that the wiring passes the watchdog through (a nil watchdog is the
// common case and is covered by every other test in this file).
func TestSweepTicksWatchdog(t *testing.T) {
	shrinkStaleTempMaxAge(t)

	srv := startTestServer(t)
	client := srv.verifyClient(t)
	if err := client.MkdirAll("/www"); err != nil {
		t.Fatal(err)
	}
	writeRemoteFile(t, client, "/www/index.html"+tmpSuffix+".1", "partial")

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
	cfg.StallTimeout = 10 * time.Second

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if remoteExists(t, srv, "/www/index.html"+tmpSuffix+".1") {
		t.Error("sweep did not run with the stall watchdog armed")
	}
}
