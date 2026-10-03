package uploader

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/config"
)

// The timeout input must bound the whole of the initial connection, not just
// the TCP dial. A server that accepts and then stays silent (an overloaded
// host, a firewall that completes the TCP handshake and drops the rest, a
// tarpit in front of port 22) used to hang the SSH handshake -- the version
// exchange, key exchange and authentication -- until the job-level timeout,
// because ssh.Dial applies ClientConfig.Timeout to the dial alone (issue #277).
func TestConnectTimeoutBoundsTheInitialHandshake(t *testing.T) {
	// Hangs from the very first accept: the server takes the TCP connection
	// and never answers the version exchange.
	srv := startTestServer(t, withHangHandshakeFrom(1))
	cfg := baseConfig(srv)
	cfg.Timeout = 1 * time.Second
	cfg.Uploads = []config.UploadPair{{Local: t.TempDir(), Remote: "/www"}}
	defer srv.closeLiveConns() // release any accepted socket the run leaves

	start := time.Now()
	_, err := Run(context.Background(), cfg, testLogger{t})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the run to fail against a server that never handshakes")
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("expected an i/o timeout error, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("run took %s; the 1s connect timeout should have failed it fast", elapsed)
	}
	if got := atomic.LoadInt32(&srv.accepted); got < 1 {
		t.Errorf("the server accepted %d connections; the run never dialed it", got)
	}
}

// The expiry must be retryable like any transient network condition, spending
// the retries budget exactly as a refused connection does, instead of being
// classified permanent (issue #277).
func TestConnectTimeoutIsRetryable(t *testing.T) {
	srv := startTestServer(t, withHangHandshakeFrom(1))
	cfg := baseConfig(srv)
	cfg.Timeout = 1 * time.Second
	cfg.Retries = 1
	cfg.Uploads = []config.UploadPair{{Local: t.TempDir(), Remote: "/www"}}
	defer srv.closeLiveConns()

	log := &recordingLogger{testLogger: testLogger{t}}
	_, err := Run(context.Background(), cfg, log)
	if err == nil {
		t.Fatal("expected the run to fail once the retry budget is spent")
	}
	if got := atomic.LoadInt32(&srv.accepted); got != 2 {
		t.Errorf("expected 2 connection attempts (1 + 1 retry), got %d", got)
	}
	retries := 0
	for _, w := range log.warnings {
		if strings.Contains(w, "could not connect; retrying") {
			retries++
		}
	}
	if retries != 1 {
		t.Errorf("expected 1 connect-retry warning, got %d: %v", retries, log.warnings)
	}
}

// A pooled connection's dial must be bounded too. acquire runs the handshake
// with the session mutex released (issue #224), so a slot whose dial hangs
// cannot block the run's other machinery -- but the dial itself used to have
// no deadline of its own, so the worker was stuck forever all the same
// (issue #277). It must resolve within the timeout, falling back to the first
// connection like any other refusal the server answers with.
func TestConnectTimeoutBoundsAPooledDial(t *testing.T) {
	// The first connection is served; every later one hangs.
	srv := startTestServer(t, withHangHandshakeAfter(1))
	cfg := baseConfig(srv)
	cfg.Connections = 2
	cfg.Concurrency = 2
	cfg.Timeout = 1 * time.Second
	cfg.Uploads = []config.UploadPair{{Local: t.TempDir(), Remote: "/www"}}
	defer srv.closeLiveConns()

	sess, err := newSession(context.Background(), cfg, newTuning(cfg), testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.close()

	sess.setSpread(2)
	acquired := make(chan error, 1)
	go func() {
		client, _, _ := sess.acquire(1)
		_ = client
		acquired <- nil
	}()
	select {
	case <-acquired:
		// The dial resolved (bounded by the timeout) and fell back to the
		// first connection, which is the refusal path's documented answer.
	case <-time.After(5 * time.Second):
		t.Fatal("a pooled dial hung past the timeout; acquire(1) never returned")
	}
	if got := atomic.LoadInt32(&srv.accepted); got < 2 {
		t.Errorf("the server accepted %d connections; the pooled dial never reached it", got)
	}
}

// A redial after a mid-run drop must be bounded as well: the same connect()
// runs underneath it. It used to inherit no deadline at all, so an unhealthy
// server pinned the run at the reconnect step (issue #277).
func TestConnectTimeoutBoundsARedial(t *testing.T) {
	// The first connection is served; every later one hangs.
	srv := startTestServer(t, withHangHandshakeAfter(1))
	cfg := baseConfig(srv)
	cfg.Timeout = 1 * time.Second
	cfg.Retries = 1
	cfg.Uploads = []config.UploadPair{{Local: t.TempDir(), Remote: "/www"}}
	defer srv.closeLiveConns()

	sess, err := newSession(context.Background(), cfg, newTuning(cfg), testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.close()

	c := sess.conns[0]
	c.ssh.Close() // the drop a worker would react to

	start := time.Now()
	_, err = sess.reconnect(context.Background(), c, c.gen, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the redial to fail against a server that no longer handshakes")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("reconnect took %s; the 1s connect timeout should have bounded it", elapsed)
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("expected the redial error to carry the timeout, got: %v", err)
	}
}

// Through a jump host the tunnel's net.Conn is an SSH channel, which rejects
// deadlines, so the bound is the timer that closes the jump client. The target
// that accepts the tunnel and then stays silent must fail the run within the
// timeout instead of hanging the handshake over the tunnel (issue #277).
func TestConnectTimeoutBoundsTheJumpHostTunnel(t *testing.T) {
	// The jump host is healthy; the target behind it never handshakes.
	target := startTestServer(t, withHangHandshakeFrom(1))
	jump := startTestJumpServer(t)
	cfg := baseConfig(target)
	cfg.HostKeyFingerprints = []string{target.HostKeySHA256}
	cfg.Proxy = proxyFor(jump)
	cfg.Timeout = 1 * time.Second
	cfg.Uploads = []config.UploadPair{{Local: t.TempDir(), Remote: "/www"}}
	defer target.closeLiveConns()

	start := time.Now()
	_, err := Run(context.Background(), cfg, testLogger{t})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the run to fail when the target never answers over the tunnel")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("run took %s; the 1s connect timeout should have failed it fast", elapsed)
	}
	if !strings.Contains(err.Error(), "jump host") {
		t.Errorf("the error should name the jump host hop, got: %v", err)
	}
}

// The skip_unchanged stat must run inside the watchdog's active window: a
// server healthy enough to connect but dead the moment a request lands on it
// must be caught by stall_timeout, which used to never fire because no
// transfer was active yet (issue #277).
func TestSkipUnchangedStatIsWatchedByTheStallWatchdog(t *testing.T) {
	// Stat on the upload's file never returns; everything else is healthy.
	srv := startTestServer(t, withStallOnStat("/www/same.txt"))
	writeRemoteFiles(t, srv, map[string]string{"/www/same.txt": "AAA"})

	local := t.TempDir()
	writeTree(t, local, map[string]string{"same.txt": "BBB"}) // same size: the stat decides

	cfg := baseConfig(srv)
	cfg.SkipUnchanged = true
	cfg.StallTimeout = 1 * time.Second
	cfg.Concurrency = 1
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	start := time.Now()
	_, err := Run(context.Background(), cfg, log)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("expected a transfer-stalled error, got %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("run took %s; the 1s stall timeout should have failed it fast", elapsed)
	}
	found := false
	log.mu.Lock()
	for _, w := range log.warnings {
		if strings.Contains(w, "no transfer progress") {
			found = true
		}
	}
	log.mu.Unlock()
	if !found {
		t.Errorf("expected a stall warning, got %v", log.warnings)
	}
}

// A connection-class failure during the skip stat must not be read as "the
// remote file has a different size": the file would be re-uploaded although it
// may be unchanged, and a dry run would report "would upload". It takes the
// existing reconnect path instead (issue #277).
func TestSkipUnchangedStatSurvivesAConnectionDrop(t *testing.T) {
	// The stat of the payload file kills the connection; the reconnect's
	// fresh connection serves it clean, so the skip decision is made on a
	// healthy stat.
	srv := startTestServer(t, withDropOnRequest("Stat", "/www/same.txt"))
	writeRemoteFiles(t, srv, map[string]string{"/www/same.txt": "AAA"})

	local := t.TempDir()
	writeTree(t, local, map[string]string{"same.txt": "BBB", "new.txt": "n"})

	cfg := baseConfig(srv)
	cfg.SkipUnchanged = true
	cfg.Retries = 2
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	// The file was not re-uploaded: the stat survived the drop, and the
	// same-size skip was reached on the fresh connection.
	if stats.FilesSkipped != 1 || stats.FilesUploaded != 1 {
		t.Fatalf("up=%d skip=%d, want 1/1", stats.FilesUploaded, stats.FilesSkipped)
	}
	if got := readRemote(t, srv, "/www/same.txt"); got != "AAA" {
		t.Errorf("the unchanged file was re-uploaded: %q", got)
	}
}
