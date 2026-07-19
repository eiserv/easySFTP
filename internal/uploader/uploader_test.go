package uploader

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/config"
	"golang.org/x/crypto/ssh"
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
	if stats.BytesUploaded != 0 {
		t.Errorf("dry-run must not transfer bytes, got %d", stats.BytesUploaded)
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
		sendKeepalives(ctx, sshClient, 10*time.Millisecond)
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
