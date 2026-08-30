package uploader

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCloseSSHRunsDuringAnInFlightReconnect is the liveness property the stall
// watchdog depends on (issue #224).
//
// The scenario is the one the watchdog exists for: the server stops being
// healthy, a worker's connection drops, its redial hangs in the handshake. The
// kill path must still be able to close the run's transports. It used to block
// on the session mutex, which reconnect held across its backoff and the whole
// SSH plus SFTP handshake, so the watchdog was prevented from firing exactly
// when it was needed. With advanced.timeout at 0 that hold is unbounded.
//
// go test -race does not catch this: it is a lock-hold-duration and liveness
// problem, not a data race.
func TestCloseSSHRunsDuringAnInFlightReconnect(t *testing.T) {
	srv := startTestServer(t, withHangHandshakeAfter(1))
	cfg := baseConfig(srv)
	cfg.Retries = 3

	sess, err := newSession(context.Background(), cfg, newTuning(cfg), testLogger{t})
	if err != nil {
		t.Fatal(err)
	}

	c := sess.conns[0]
	c.ssh.Close() // the drop the worker is reacting to

	reconnectDone := make(chan struct{})
	go func() {
		defer close(reconnectDone)
		// Hangs in the handshake until the test closes the socket.
		_, _ = sess.reconnect(context.Background(), c, c.gen)
	}()
	waitFor(t, "the redial to reach the server", func() bool {
		return atomic.LoadInt32(&srv.accepted) > 1
	})

	closed := make(chan struct{})
	go func() {
		sess.closeSSH()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("closeSSH blocked behind an in-flight reconnect; the stall watchdog cannot fire")
	}

	srv.closeLiveConns() // unblock the hung handshake
	<-reconnectDone
	sess.close()
}

func TestSessionReadsRunDuringAnInFlightPooledDial(t *testing.T) {
	srv := startTestServer(t, withHangHandshakeAfter(1))
	cfg := baseConfig(srv)
	cfg.Connections = 2
	cfg.Concurrency = 2

	sess, err := newSession(context.Background(), cfg, newTuning(cfg), testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	sess.setSpread(2)
	acquired := make(chan struct{})
	go func() {
		_, _, _ = sess.acquire(1)
		close(acquired)
	}()
	waitFor(t, "the pooled dial to reach the server", func() bool {
		return atomic.LoadInt32(&srv.accepted) > 1
	})

	read := make(chan struct{})
	go func() {
		_ = sess.liveSSH()
		close(read)
	}()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("a session read blocked behind an in-flight pooled handshake")
	}

	srv.closeLiveConns()
	<-acquired
	sess.close()
}

// TestConcurrentReconnectsCollapseIntoOne pins the behaviour session.do
// documents and that used to fall out of holding the mutex across the redial:
// workers that all fail on the same connection cost one reconnect between
// them, not one each.
func TestConcurrentReconnectsCollapseIntoOne(t *testing.T) {
	srv := startTestServer(t)
	cfg := baseConfig(srv)
	cfg.Retries = 5

	sess, err := newSession(context.Background(), cfg, newTuning(cfg), testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.close()

	c := sess.conns[0]
	gen := c.gen
	c.ssh.Close()

	const workers = 8
	done := make(chan error, workers)
	for range workers {
		go func() {
			_, err := sess.reconnect(context.Background(), c, gen)
			done <- err
		}()
	}
	for range workers {
		if err := <-done; err != nil {
			t.Fatalf("reconnect returned %v, want nil", err)
		}
	}

	sess.mu.Lock()
	spent, newGen := sess.reconnects, c.gen
	sess.mu.Unlock()
	if spent != 1 {
		t.Errorf("%d reconnects spent for one dead connection, want 1", spent)
	}
	if newGen != gen+1 {
		t.Errorf("connection generation = %d, want %d", newGen, gen+1)
	}
}
