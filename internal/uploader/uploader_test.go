package uploader

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		Server:      srv.Host,
		Port:        srv.Port,
		Username:    testUser,
		Password:    testPassword,
		Concurrency: 4,
		Retries:     0,
		Timeout:     10 * time.Second,
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

func TestDeleteMode(t *testing.T) {
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
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
	cfg.Delete = true

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
