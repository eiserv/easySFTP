package uploader

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func TestRetryBackoffUsesExponentialRangeWithJitter(t *testing.T) {
	for retry := 1; retry <= 5; retry++ {
		base := time.Duration(1<<(retry-1)) * time.Second
		for range 20 {
			got := retryBackoff(retry)
			if got < base || got > base+base/4 {
				t.Fatalf("retry %d backoff %s is outside [%s, %s]", retry, got, base, base+base/4)
			}
		}
	}
}

func TestUploadTempPathHasOneDefinition(t *testing.T) {
	if got := uploadTempPath("/www/app.js", 42); got != "/www/app.js"+tmpSuffix+".42" {
		t.Fatalf("upload temp path = %q", got)
	}
}

func TestRetryKeepsItsAcquiredConnectionWhenSpreadChanges(t *testing.T) {
	first := &conn{sftp: &sftp.Client{}}
	second := &conn{sftp: &sftp.Client{}}
	sess := &session{conns: []*conn{first, second}, spread: 2}

	_, acquired, _ := sess.acquire(1)
	if acquired != second {
		t.Fatal("test setup did not acquire the second connection")
	}
	sess.setSpread(1)
	client, _ := sess.connection(acquired)
	if client != second.sftp {
		t.Fatal("captured connection changed after the spread changed")
	}
	_, rerouted, _ := sess.acquire(1)
	if rerouted != first {
		t.Fatal("test setup did not demonstrate that re-acquiring by index reroutes")
	}
}

type keepaliveProbe struct {
	called  chan bool
	release chan struct{}
}

func (p *keepaliveProbe) SendRequest(_ string, wantReply bool, _ []byte) (bool, []byte, error) {
	p.called <- wantReply
	if p.release != nil {
		<-p.release
	}
	return false, nil, nil
}

func TestKeepaliveLoopBoundsBlockedRequestsWithoutDelayingHealthyConnections(t *testing.T) {
	blocked := &keepaliveProbe{called: make(chan bool, 2), release: make(chan struct{})}
	healthy := &keepaliveProbe{called: make(chan bool, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		keepaliveLoop(ctx, func() []keepaliveSender { return []keepaliveSender{blocked, healthy} }, 10*time.Millisecond)
		close(done)
	}()

	select {
	case wantReply := <-healthy.called:
		if wantReply {
			t.Error("keepalive unnecessarily waited for a reply")
		}
	case <-time.After(time.Second):
		t.Fatal("healthy connection was blocked by another connection's keepalive")
	}
	if wantReply := <-blocked.called; wantReply {
		t.Error("blocked keepalive unnecessarily waited for a reply")
	}
	select {
	case <-blocked.called:
		t.Error("a second keepalive started while the first was blocked")
	case <-time.After(40 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("keepalive loop did not stop after cancellation")
	}
	close(blocked.release)
}

func TestWriteProgressTicksAfterThePreviousPacket(t *testing.T) {
	watch := &stallWatchdog{}
	reader := watch.writeProgress(strings.NewReader("abc"))
	buf := make([]byte, 1)
	if _, err := reader.Read(buf); err != nil {
		t.Fatal(err)
	}
	if got := watch.ticks.Load(); got != 0 {
		t.Fatalf("first local read produced %d progress ticks, want none", got)
	}
	if _, err := reader.Read(buf); err != nil {
		t.Fatal(err)
	}
	if got := watch.ticks.Load(); got != 1 {
		t.Fatalf("read after the previous write round produced %d ticks, want 1", got)
	}
}

func TestSleepCtxStillCancelsJitteredWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, retryBackoff(1)); err != context.Canceled {
		t.Fatalf("cancelled backoff returned %v", err)
	}
}
