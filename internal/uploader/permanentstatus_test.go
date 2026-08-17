package uploader

import (
	"context"
	"strings"
	"testing"

	"github.com/pkg/sftp"

	"github.com/eiserv/easySFTP/internal/config"
)

// countRetryWarnings counts the per-file retry warnings a run logged, i.e. how
// many extra attempts uploadFileWithRetry actually spent.
func countRetryWarnings(log *recordingLogger) int {
	log.mu.Lock()
	defer log.mu.Unlock()
	n := 0
	for _, w := range log.warnings {
		if strings.HasPrefix(w, "retrying upload of ") {
			n++
		}
	}
	return n
}

// A server that refuses the upload with a status code it will repeat verbatim
// (SSH_FX_OP_UNSUPPORTED here) is not worth a second attempt: the run must
// fail after the first one, without spending any backoff, and must say why it
// did not retry.
func TestPermanentServerStatusIsNotRetried(t *testing.T) {
	srv := startTestServer(t, withStatusOnPut("/www/a.txt", sftp.ErrSSHFxOpUnsupported))

	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "a"})

	cfg := baseConfig(srv)
	cfg.Concurrency = 1
	cfg.Retries = 2
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	_, err := Run(context.Background(), cfg, log)
	if err == nil {
		t.Fatal("expected the upload to fail")
	}
	if n := countRetryWarnings(log); n != 0 {
		t.Errorf("a permanent server status was retried %d time(s); want 0", n)
	}
	// The server's own status line stays in the failure, and the reason the
	// configured retries did not run is spelled out next to it.
	msg := err.Error()
	if !strings.Contains(msg, "SSH_FX_OP_UNSUPPORTED") {
		t.Errorf("failure does not carry the server's status: %v", err)
	}
	if !strings.Contains(msg, "retrying would not have helped") {
		t.Errorf("failure does not explain why it was not retried: %v", err)
	}
}

// The other half of the same rule: SSH_FX_FAILURE is the catch-all servers
// also use for transient conditions, so it keeps its retries. Losing them
// would trade a slow doomed deploy for a fast broken one, which is the worse
// mistake (see isPermanentStatus).
func TestAmbiguousServerStatusStillRetries(t *testing.T) {
	srv := startTestServer(t, withStatusOnPut("/www/a.txt", sftp.ErrSSHFxFailure))

	local := t.TempDir()
	writeTree(t, local, map[string]string{"a.txt": "a"})

	cfg := baseConfig(srv)
	cfg.Concurrency = 1
	cfg.Retries = 1 // one retry, so the test pays a single 1s backoff
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	_, err := Run(context.Background(), cfg, log)
	if err == nil {
		t.Fatal("expected the upload to fail")
	}
	if n := countRetryWarnings(log); n != 1 {
		t.Errorf("SSH_FX_FAILURE produced %d retry warning(s); want 1", n)
	}
	if !strings.Contains(err.Error(), "SSH_FX_FAILURE") {
		t.Errorf("failure does not carry the server's status: %v", err)
	}
	if strings.Contains(err.Error(), "retrying would not have helped") {
		t.Errorf("an ambiguous status was reported as permanent: %v", err)
	}
}
