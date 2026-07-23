package uploader

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ignore "github.com/sabhiram/go-gitignore"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/eiserv/easySFTP/internal/config"
)

// writeTree creates files under root; keys are slash-separated relative paths.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func baseConfig(srv *testServer) *config.Config {
	return &config.Config{
		Server:                 srv.Host,
		Port:                   srv.Port,
		Username:               testUser,
		Password:               testPassword,
		Concurrency:            4,
		SftpRequestConcurrency: 16,
		Retries:                0,
		Timeout:                10 * time.Second,
		// The in-process test server's host key is not pinned; opt out of
		// verification like a directly constructed config must in v3.
		AllowAnyHostKey: true,
	}
}

func readRemote(t *testing.T, srv *testServer, path string) string {
	t.Helper()
	client := srv.verifyClient(t)
	f, err := client.Open(path)
	if err != nil {
		t.Fatalf("opening remote file %s: %v", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func remoteExists(t *testing.T, srv *testServer, path string) bool {
	t.Helper()
	client := srv.verifyClient(t)
	_, err := client.Stat(path)
	return err == nil
}

// remoteHasTmpFile reports whether dir contains any leftover temp upload file
// for base, i.e. an entry named "<base><tmpSuffix>" or "<base><tmpSuffix>.N".
func remoteHasTmpFile(t *testing.T, srv *testServer, dir, base string) bool {
	t.Helper()
	client := srv.verifyClient(t)
	entries, err := client.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	prefix := base + tmpSuffix
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			return true
		}
	}
	return false
}

func TestUploadDirectoryWithIgnore(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"index.html":          "<h1>hello</h1>",
		"assets/style.css":    "body{}",
		"assets/deep/app.js":  "console.log(1)",
		"debug.log":           "ignore me",
		"node_modules/x/y.js": "ignore me too",
	})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
	cfg.IgnoreLines = []string{"*.log", "node_modules/"}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 3 {
		t.Errorf("expected 3 uploads, got %d", stats.FilesUploaded)
	}
	if got := readRemote(t, srv, "/www/index.html"); got != "<h1>hello</h1>" {
		t.Errorf("unexpected content: %q", got)
	}
	if got := readRemote(t, srv, "/www/assets/deep/app.js"); got != "console.log(1)" {
		t.Errorf("unexpected content: %q", got)
	}
	if remoteExists(t, srv, "/www/debug.log") {
		t.Error("ignored file debug.log was uploaded")
	}
	if remoteExists(t, srv, "/www/node_modules/x/y.js") {
		t.Error("ignored directory node_modules was uploaded")
	}
}

func TestMultiTargetStatsBreakdown(t *testing.T) {
	srv := startTestServer(t)

	siteLocal := t.TempDir()
	writeTree(t, siteLocal, map[string]string{
		"index.html": "<h1>hi</h1>",
		"style.css":  "body{}",
	})
	docsLocal := t.TempDir()
	writeTree(t, docsLocal, map[string]string{
		"readme.md": "# docs",
	})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{
		{Local: siteLocal, Remote: "/www/site", Strategy: config.StrategyOverlay},
		{Local: docsLocal, Remote: "/www/docs", Strategy: config.StrategyOverlay},
	}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}

	if len(stats.Targets) != 2 {
		t.Fatalf("expected 2 target entries, got %d: %+v", len(stats.Targets), stats.Targets)
	}

	site, docs := stats.Targets[0], stats.Targets[1]
	if site.Local != siteLocal || site.Remote != "/www/site" || site.Strategy != config.StrategyOverlay {
		t.Errorf("unexpected site target stats: %+v", site)
	}
	if site.FilesUploaded != 2 {
		t.Errorf("expected 2 files uploaded for site, got %d", site.FilesUploaded)
	}
	if docs.Local != docsLocal || docs.Remote != "/www/docs" {
		t.Errorf("unexpected docs target stats: %+v", docs)
	}
	if docs.FilesUploaded != 1 {
		t.Errorf("expected 1 file uploaded for docs, got %d", docs.FilesUploaded)
	}

	// Per-target totals must sum to the run-wide totals.
	var sumUploaded int
	var sumBytes int64
	for _, ts := range stats.Targets {
		sumUploaded += ts.FilesUploaded
		sumBytes += ts.BytesUploaded
	}
	if sumUploaded != stats.FilesUploaded {
		t.Errorf("target FilesUploaded sum %d != total %d", sumUploaded, stats.FilesUploaded)
	}
	if sumBytes != stats.BytesUploaded {
		t.Errorf("target BytesUploaded sum %d != total %d", sumBytes, stats.BytesUploaded)
	}
}

func TestSingleTargetStatsAreRecorded(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "hi"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Name: "website", Local: local, Remote: "/www"}}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Targets) != 1 || stats.Targets[0].Name != "website" || stats.Targets[0].FilesUploaded != 1 {
		t.Errorf("expected one named target entry with 1 upload, got %+v", stats.Targets)
	}
}

func TestUploadDirectoryFailsWhenRemoteDirIsFile(t *testing.T) {
	srv := startTestServer(t)
	client := srv.verifyClient(t)
	f, err := client.Create("/www")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("not a directory")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"index.html": "fresh",
	})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	_, err = Run(context.Background(), cfg, testLogger{t})
	if err == nil {
		t.Fatal("expected remote file conflict error")
	}
	if got, want := err.Error(), `remote path "/www" exists but is not a directory`; !strings.Contains(got, want) {
		t.Fatalf("expected error containing %q, got %q", want, got)
	}
}

func TestUploadSingleFile(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"config.json": `{"a":1}`})
	src := filepath.Join(local, "config.json")

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{
		{Local: src, Remote: "/etc/app/renamed.json"}, // exact target path
		{Local: src, Remote: "/etc/dir/"},             // into directory
	}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if got := readRemote(t, srv, "/etc/app/renamed.json"); got != `{"a":1}` {
		t.Errorf("unexpected content: %q", got)
	}
	if got := readRemote(t, srv, "/etc/dir/config.json"); got != `{"a":1}` {
		t.Errorf("unexpected content: %q", got)
	}
}

func TestCleanStrategy(t *testing.T) {
	srv := startTestServer(t)

	// Pre-populate the remote target with stale content.
	client := srv.verifyClient(t)
	if err := client.MkdirAll("/www/old"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/www/stale.html", "/www/old/stale.js"} {
		f, err := client.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("stale")); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	local := t.TempDir()
	writeTree(t, local, map[string]string{"index.html": "fresh"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategyClean}}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesDeleted != 2 {
		t.Errorf("expected 2 deleted files, got %d", stats.FilesDeleted)
	}
	if remoteExists(t, srv, "/www/stale.html") || remoteExists(t, srv, "/www/old") {
		t.Error("stale remote content was not deleted")
	}
	if got := readRemote(t, srv, "/www/index.html"); got != "fresh" {
		t.Errorf("unexpected content: %q", got)
	}
}

func TestDryRun(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "a", "b/c.txt": "c"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
	cfg.DryRun = true

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 2 {
		t.Errorf("expected 2 planned uploads, got %d", stats.FilesUploaded)
	}
	if stats.BytesUploaded != 2 {
		t.Errorf("expected 2 planned bytes, got %d", stats.BytesUploaded)
	}
	if remoteExists(t, srv, "/www") {
		t.Error("dry-run must not create remote directories")
	}
}

func TestPrivateKeyAuth(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "key-auth"})

	cfg := baseConfig(srv)
	cfg.Password = ""
	cfg.PrivateKey = srv.ClientKeyPEM
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	if got := readRemote(t, srv, "/www/a.txt"); got != "key-auth" {
		t.Errorf("unexpected content: %q", got)
	}
}

func TestHostKeyFingerprint(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "pinned"})

	t.Run("matching fingerprint succeeds", func(t *testing.T) {
		cfg := baseConfig(srv)
		cfg.HostKeyFingerprints = []string{srv.HostKeySHA256}
		cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/pinned"}}
		if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("any of multiple fingerprints matches", func(t *testing.T) {
		cfg := baseConfig(srv)
		cfg.HostKeyFingerprints = []string{
			"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			srv.HostKeySHA256,
		}
		cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/pinned"}}
		if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("wrong fingerprint fails", func(t *testing.T) {
		cfg := baseConfig(srv)
		cfg.HostKeyFingerprints = []string{"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
		cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/pinned"}}
		_, err := Run(context.Background(), cfg, testLogger{t})
		if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
			t.Fatalf("expected host key mismatch error, got %v", err)
		}
	})

	t.Run("malformed fingerprint fails", func(t *testing.T) {
		cfg := baseConfig(srv)
		cfg.HostKeyFingerprints = []string{"md5:abcdef"}
		cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/pinned"}}
		_, err := Run(context.Background(), cfg, testLogger{t})
		if err == nil || !strings.Contains(err.Error(), "SHA256") {
			t.Fatalf("expected SHA256 format error, got %v", err)
		}
	})
}

func TestUnverifiedHostKeyRequiresOptIn(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "x"})

	t.Run("no pins and no opt-in fails", func(t *testing.T) {
		cfg := baseConfig(srv)
		cfg.AllowAnyHostKey = false
		cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
		_, err := Run(context.Background(), cfg, testLogger{t})
		if err == nil || !strings.Contains(err.Error(), "allow-any-host-key") {
			t.Fatalf("expected an unverified-host-key error naming the opt-in, got %v", err)
		}
	})

	t.Run("opt-in connects but warns", func(t *testing.T) {
		cfg := baseConfig(srv)
		cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
		log := &recordingLogger{testLogger: testLogger{t}}
		if _, err := Run(context.Background(), cfg, log); err != nil {
			t.Fatal(err)
		}
		found := false
		log.mu.Lock()
		for _, w := range log.warnings {
			if strings.Contains(w, "allow-any-host-key") {
				found = true
			}
		}
		log.mu.Unlock()
		if !found {
			t.Errorf("expected an allow-any-host-key warning, got %v", log.warnings)
		}
	})

	t.Run("config-mode error names the config options", func(t *testing.T) {
		cfg := baseConfig(srv)
		cfg.AllowAnyHostKey = false
		cfg.ConfigPath = ".github/easysftp.yml"
		cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
		_, err := Run(context.Background(), cfg, testLogger{t})
		if err == nil || !strings.Contains(err.Error(), "connection.allow_any_host_key") {
			t.Fatalf("expected the config-mode option name in the error, got %v", err)
		}
	})
}

func TestKnownHosts(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "known"})

	addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))
	// What "ssh-keyscan -p <port> <host>" would print for this server.
	keyscanLine := knownhosts.Line([]string{addr}, srv.HostPubKey)

	run := func(t *testing.T, cfg *config.Config) error {
		t.Helper()
		cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/known"}}
		_, err := Run(context.Background(), cfg, testLogger{t})
		return err
	}

	t.Run("matching keyscan line succeeds", func(t *testing.T) {
		cfg := baseConfig(srv)
		cfg.KnownHosts = keyscanLine
		if err := run(t, cfg); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("hashed entry succeeds", func(t *testing.T) {
		cfg := baseConfig(srv)
		hashed := knownhosts.HashHostname(knownhosts.Normalize(addr))
		cfg.KnownHosts = hashed + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(srv.HostPubKey)))
		if err := run(t, cfg); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("entry for another key fails", func(t *testing.T) {
		_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		otherSigner, err := ssh.NewSignerFromKey(otherPriv)
		if err != nil {
			t.Fatal(err)
		}
		cfg := baseConfig(srv)
		cfg.KnownHosts = knownhosts.Line([]string{addr}, otherSigner.PublicKey())
		err = run(t, cfg)
		if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
			t.Fatalf("expected host key mismatch error, got %v", err)
		}
	})

	t.Run("entry for another host fails", func(t *testing.T) {
		cfg := baseConfig(srv)
		cfg.KnownHosts = knownhosts.Line([]string{"other.example.com:22"}, srv.HostPubKey)
		err := run(t, cfg)
		if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
			t.Fatalf("expected host key mismatch error, got %v", err)
		}
	})

	t.Run("wrong fingerprint plus matching known-hosts succeeds", func(t *testing.T) {
		cfg := baseConfig(srv)
		cfg.HostKeyFingerprints = []string{"SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
		cfg.KnownHosts = keyscanLine
		if err := run(t, cfg); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("matching fingerprint plus non-matching known-hosts succeeds", func(t *testing.T) {
		cfg := baseConfig(srv)
		cfg.HostKeyFingerprints = []string{srv.HostKeySHA256}
		cfg.KnownHosts = knownhosts.Line([]string{"other.example.com:22"}, srv.HostPubKey)
		if err := run(t, cfg); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("garbage known-hosts fails to parse", func(t *testing.T) {
		cfg := baseConfig(srv)
		cfg.KnownHosts = "not a known_hosts line"
		err := run(t, cfg)
		if err == nil || !strings.Contains(err.Error(), "known-hosts") {
			t.Fatalf("expected a known-hosts parse error, got %v", err)
		}
	})
}

func TestDirModeChmodsEveryRemoteDirectory(t *testing.T) {
	var rec setstatRecorder
	srv := startTestServer(t, withSetstatRecorder(&rec))
	local := t.TempDir()
	writeTree(t, local, map[string]string{"assets/deep/app.js": "x"})

	mode := fs.FileMode(0o700)
	cfg := baseConfig(srv)
	cfg.DirMode = &mode
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	want := map[string]uint32{"/www": 0o700, "/www/assets": 0o700, "/www/assets/deep": 0o700}
	got := map[string]uint32{}
	rec.mu.Lock()
	for _, c := range rec.calls {
		got[c.path] = c.mode & 0o777
	}
	rec.mu.Unlock()
	for path, mode := range want {
		if got[path] != mode {
			t.Errorf("expected %s chmod'd to %04o, got %04o (all calls: %+v)", path, mode, got[path], got)
		}
	}
}

func TestFileModeOverridesLocalPermissionBits(t *testing.T) {
	var rec setstatRecorder
	srv := startTestServer(t, withSetstatRecorder(&rec))
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "content"})

	mode := fs.FileMode(0o600)
	cfg := baseConfig(srv)
	cfg.FileMode = &mode
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 1 || rec.calls[0].mode&0o777 != 0o600 {
		t.Errorf("expected exactly one chmod to 0600, got %+v", rec.calls)
	}
}

func TestDirModeFailureWarnsOnceNotPerDirectory(t *testing.T) {
	srv := startTestServer(t, withFailSetstat())
	local := t.TempDir()
	writeTree(t, local, map[string]string{"assets/deep/app.js": "x", "assets/other.js": "y"})

	mode := fs.FileMode(0o700)
	cfg := baseConfig(srv)
	cfg.DirMode = &mode
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatalf("a rejected dir-mode chmod must not fail the run: %v", err)
	}

	log.mu.Lock()
	defer log.mu.Unlock()
	n := 0
	for _, w := range log.warnings {
		if strings.Contains(w, "dir-mode") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly 1 dir-mode warning, got %d: %v", n, log.warnings)
	}
}

func TestFileModeFailureWarnsOnceNotPerFile(t *testing.T) {
	srv := startTestServer(t, withFailSetstat())
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1", "b.txt": "2", "c.txt": "3"})

	mode := fs.FileMode(0o600)
	cfg := baseConfig(srv)
	cfg.FileMode = &mode
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("a rejected file-mode chmod must not fail the run: %v", err)
	}
	if stats.FilesUploaded != 3 {
		t.Errorf("expected all 3 files to still upload, got %d", stats.FilesUploaded)
	}

	log.mu.Lock()
	defer log.mu.Unlock()
	n := 0
	for _, w := range log.warnings {
		if strings.Contains(w, "file-mode") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly 1 file-mode warning, got %d: %v", n, log.warnings)
	}
}

func TestDefaultModeMirrorsLocalBitsSilentlyOnFailure(t *testing.T) {
	srv := startTestServer(t, withFailSetstat())
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1"})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatalf("a rejected chmod must not fail the run: %v", err)
	}

	log.mu.Lock()
	defer log.mu.Unlock()
	for _, w := range log.warnings {
		if strings.Contains(w, "SETSTAT") {
			t.Errorf("expected no chmod warning when mirroring local mode (no explicit override), got %v", log.warnings)
		}
	}
}

func TestSymlinkSkipWarnsOncePerTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1", "b.txt": "2"})
	if err := os.Symlink(filepath.Join(local, "a.txt"), filepath.Join(local, "link1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(local, "b.txt"), filepath.Join(local, "link2")); err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("symlinks must be skipped, not fail the run: %v", err)
	}
	if stats.FilesUploaded != 2 {
		t.Errorf("expected the 2 regular files to upload, got %d", stats.FilesUploaded)
	}

	log.mu.Lock()
	defer log.mu.Unlock()
	n := 0
	for _, w := range log.warnings {
		if strings.Contains(w, "non-regular") {
			n++
			if !strings.Contains(w, "2") {
				t.Errorf("expected the warning to mention the count of 2, got %q", w)
			}
		}
	}
	if n != 1 {
		t.Errorf("expected exactly 1 non-regular-file warning, got %d: %v", n, log.warnings)
	}
}

func TestMissingLocalPathFailsBeforeConnecting(t *testing.T) {
	cfg := &config.Config{
		Server:      "unreachable.invalid",
		Port:        22,
		Username:    "u",
		Password:    "p",
		Concurrency: 1,
		Timeout:     time.Second,
		Uploads:     []config.UploadPair{{Local: filepath.Join(os.TempDir(), "easysftp-does-not-exist-xyz"), Remote: "/www"}},
	}
	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "local path") {
		t.Fatalf("expected local path error, got %v", err)
	}
}

func TestSendKeepalivesPingsUntilCanceled(t *testing.T) {
	var received int64
	srv := startTestServer(t, withKeepaliveCounter(&received))

	sshClient, err := ssh.Dial("tcp", srv.Addr, &ssh.ClientConfig{
		User:            testUser,
		Auth:            []ssh.AuthMethod{ssh.Password(testPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sshClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sendKeepalives(ctx, func() *ssh.Client { return sshClient }, 10*time.Millisecond)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for atomic.LoadInt64(&received) < 3 {
		select {
		case <-deadline:
			t.Fatalf("expected at least 3 keepalives, got %d", atomic.LoadInt64(&received))
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sendKeepalives did not stop after context cancellation")
	}
}

// writeRemoteFiles pre-creates files on the server, so tests can shape the
// remote side an overlay run will encounter.
func writeRemoteFiles(t *testing.T, srv *testServer, files map[string]string) {
	t.Helper()
	client := srv.verifyClient(t)
	for name, content := range files {
		if err := client.MkdirAll(path.Dir(name)); err != nil {
			t.Fatal(err)
		}
		f, err := client.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}

// With skip-unchanged, overlay skips a file whose remote counterpart has the
// same size, uploads size mismatches and missing files. The same-size skip
// deliberately ignores content: that is the documented size-only trade-off.
func TestOverlaySkipUnchanged(t *testing.T) {
	srv := startTestServer(t)
	writeRemoteFiles(t, srv, map[string]string{
		"/www/same.txt": "AAA",
		"/www/diff.txt": "AAAA",
	})

	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"same.txt": "BBB", // same size as remote: skipped
		"diff.txt": "BB",  // different size: uploaded
		"new.txt":  "n",   // missing remotely: uploaded
	})

	cfg := baseConfig(srv)
	cfg.SkipUnchanged = true
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 2 || stats.FilesSkipped != 1 {
		t.Fatalf("up=%d skip=%d, want 2/1", stats.FilesUploaded, stats.FilesSkipped)
	}
	if got := readRemote(t, srv, "/www/same.txt"); got != "AAA" {
		t.Errorf("same-size file was touched: %q", got)
	}
	if got := readRemote(t, srv, "/www/diff.txt"); got != "BB" {
		t.Errorf("size-changed file not uploaded: %q", got)
	}
	if got := readRemote(t, srv, "/www/new.txt"); got != "n" {
		t.Errorf("new file not uploaded: %q", got)
	}
}

// The skip stat is read-only, so dry-run performs it too and previews the
// same skips the real run would, without changing anything.
func TestOverlaySkipUnchangedDryRun(t *testing.T) {
	srv := startTestServer(t)
	writeRemoteFiles(t, srv, map[string]string{"/www/same.txt": "AAA"})

	local := t.TempDir()
	writeTree(t, local, map[string]string{"same.txt": "BBB", "new.txt": "n"})

	cfg := baseConfig(srv)
	cfg.SkipUnchanged = true
	cfg.DryRun = true
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 1 || stats.FilesSkipped != 1 {
		t.Fatalf("dry-run up=%d skip=%d, want 1/1", stats.FilesUploaded, stats.FilesSkipped)
	}
	if remoteExists(t, srv, "/www/new.txt") {
		t.Error("dry-run must not upload anything")
	}
}

// skip_unchanged only applies to overlay; other modes warn and ignore it.
func TestSkipUnchangedWarnsOnNonOverlay(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "1"})

	cfg := baseConfig(srv)
	cfg.SkipUnchanged = true
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www", Strategy: config.StrategySync}}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range log.warnings {
		if strings.Contains(w, "skip_unchanged only applies to the overlay mode") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning that skip_unchanged is ignored for sync, got %v", log.warnings)
	}
}

func TestIgnoredDirectoryIsPruned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 does not restrict directory reads on Windows")
	}
	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"index.html":          "x",
		"node_modules/x/y.js": "y",
	})
	// An unreadable ignored directory: descending into it would fail the walk,
	// so a successful plan proves it was pruned, not just filtered per file.
	if err := os.Chmod(filepath.Join(local, "node_modules"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(local, "node_modules"), 0o755) })

	matcher := ignore.CompileIgnoreLines("node_modules/")
	p, err := buildPlan(config.UploadPair{Local: local, Remote: "/www"}, config.StrategyOverlay, planOptions{matcher: matcher, pruneDirs: true, manifestName: manifestName})
	if err != nil {
		t.Fatalf("walk descended into the pruned directory: %v", err)
	}
	if len(p.files) != 1 || p.files[0].rel != "index.html" {
		t.Errorf("unexpected plan files: %+v", p.files)
	}
}

func TestIgnoreNegationReincludesBelowIgnoredDirectory(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{
		"index.html":            "x",
		"node_modules/keep.js":  "keep",
		"node_modules/other.js": "drop",
	})

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
	cfg.IgnoreLines = []string{"node_modules/", "!node_modules/keep.js"}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 2 {
		t.Errorf("expected 2 uploads, got %d", stats.FilesUploaded)
	}
	if got := readRemote(t, srv, "/www/node_modules/keep.js"); got != "keep" {
		t.Errorf("re-included file missing or wrong: %q", got)
	}
	if remoteExists(t, srv, "/www/node_modules/other.js") {
		t.Error("ignored file other.js was uploaded")
	}
}

func TestHasNegation(t *testing.T) {
	if hasNegation([]string{"*.log", "node_modules/"}) {
		t.Error("no negation expected")
	}
	if !hasNegation([]string{"*.log", "!important.log"}) {
		t.Error("negation expected")
	}
}

// BenchmarkBuildPlanIgnoredTree compares planning over a tree whose bulk sits
// in an ignored node_modules/ directory, with and without directory pruning.
func BenchmarkBuildPlanIgnoredTree(b *testing.B) {
	local := b.TempDir()
	for i := range 200 {
		dir := filepath.Join(local, "node_modules", "pkg"+strconv.Itoa(i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		for j := range 10 {
			if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(j)+".js"), []byte("x"), 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(local, "index.html"), []byte("x"), 0o644); err != nil {
		b.Fatal(err)
	}
	matcher := ignore.CompileIgnoreLines("node_modules/")
	pair := config.UploadPair{Local: local, Remote: "/www"}
	for _, bench := range []struct {
		name  string
		prune bool
	}{{"pruned", true}, {"unpruned", false}} {
		b.Run(bench.name, func(b *testing.B) {
			for b.Loop() {
				if _, err := buildPlan(pair, config.StrategyOverlay, planOptions{matcher: matcher, pruneDirs: bench.prune, manifestName: manifestName}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
