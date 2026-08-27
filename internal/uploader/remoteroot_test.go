package uploader

import (
	"context"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/config"
)

// checkRemoteRoot is the one delete guard that is always on and cannot be
// configured away, and until issue #222 nothing exercised it directly. The
// table covers both directions: what it must refuse, and what it must keep
// allowing, since a guard that grew too strict would break every workflow
// deploying into a relative directory.
func TestCheckRemoteRoot(t *testing.T) {
	refused := []struct {
		name   string
		remote string
	}{
		{"filesystem root", "/"},
		{"working directory", "."},
		{"empty target", ""},
		{"home shorthand", "~"},
		{"absolute path that climbs back to the root", "/var/www/../.."},
		{"home shorthand that climbs to itself", "~/.."},

		// The rows below all passed before #222: path.Clean leaves ".." as
		// "..", which is not the root, so the switch never matched it.
		{"bare parent", ".."},
		{"grandparent", "../.."},
		{"parent reached through the current directory", "./.."},
		{"parent reached by climbing out of a subdirectory", "dist/../.."},
		{"escape below a deeper prefix", "../../etc"},
		{"backslashes normalize to the same escape", `..\..`},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			err := checkRemoteRoot(tc.remote)
			if err == nil {
				t.Fatalf("checkRemoteRoot(%q) allowed a destructive mode on %q", tc.remote, normalizeRemote(tc.remote))
			}
			if !strings.Contains(err.Error(), "remote root") {
				t.Errorf("checkRemoteRoot(%q) = %v, want an error naming the remote root", tc.remote, err)
			}
		})
	}

	allowed := []struct {
		name   string
		remote string
	}{
		{"absolute subdirectory", "/var/www/public_html"},
		{"absolute subdirectory with a trailing slash", "/var/www/"},
		{"relative subdirectory", "www/public_html"},
		{"relative subdirectory below the current directory", "./dist"},
		{"path that climbs and comes back down", "/var/www/../html"},
		{"directory whose name merely starts with two dots", "..config"},
		{"home subdirectory", "~/public_html"},
	}
	for _, tc := range allowed {
		t.Run("allowed/"+tc.name, func(t *testing.T) {
			if err := checkRemoteRoot(tc.remote); err != nil {
				t.Errorf("checkRemoteRoot(%q) refused a legitimate target: %v", tc.remote, err)
			}
		})
	}
}

// The guard has to stop the run, not merely return an error to a caller that
// ignores it. This is the shape a workflow lands in when an expression leaves
// a trailing "/.." on the target.
func TestCleanRefusesTargetAboveTheLoginDirectory(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "a"})

	client := srv.verifyClient(t)
	if err := client.MkdirAll("/www/keep"); err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "dist/../..", Strategy: config.StrategyClean}}

	_, err := Run(context.Background(), cfg, testLogger{t})
	if err == nil || !strings.Contains(err.Error(), "remote root") {
		t.Fatalf("expected the remote-root guard to stop the run, got %v", err)
	}
	if !remoteExists(t, srv, "/www/keep") {
		t.Error("the run deleted remote content before the guard refused it")
	}
}
