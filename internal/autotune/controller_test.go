package autotune_test

import (
	"strings"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/autotune"
)

// start is the configuration a byte-heavy deployment begins at: one
// connection, because Plan had to assume the conservative throughput prior,
// and enough workers for the files it has.
var start = autotune.Settings{Connections: 1, Concurrency: 64, RequestConcurrency: 16}

// TestControllerGrowsOnMeasuredThroughputThenStops is the whole of stage 3 in
// one run: the transfer turns out to be far slower per stream than the prior
// assumed, so the pool grows; it grows one doubling at a time; and the moment
// a step stops paying for itself the controller settles instead of climbing on.
func TestControllerGrowsOnMeasuredThroughputThenStops(t *testing.T) {
	c := autotune.NewController(start, house, autotune.Fixed{})
	if stopped, why := c.Stopped(); stopped {
		t.Fatalf("a fresh controller with room to grow reported stopped: %s", why)
	}

	// One second in: 10 of 1000 files of 1 MiB each are through, so one stream
	// carries about 10 MiB/s and the 990 MiB left would take a minute and a
	// half on it. That is worth several handshakes.
	first, ok := c.Observe(autotune.Progress{
		Elapsed: time.Second, Uploaded: 10, Remaining: 990,
		RemainingBytes: 990 * MiB, UploadedBytes: 10 * MiB,
	})
	if !ok {
		t.Fatal("a stream measured far slower than the prior must widen the pool")
	}
	if first.From.Connections != 1 || first.To.Connections != 2 {
		t.Fatalf("first step went %d -> %d, want one doubling from 1", first.From.Connections, first.To.Connections)
	}
	if !strings.Contains(first.String(), "raising connections 1 -> 2") {
		t.Errorf("the change does not describe itself usefully: %s", first)
	}

	// The second connection tripled the throughput of the window, so the
	// controller believes the step and takes the next one.
	second, ok := c.Observe(autotune.Progress{
		Elapsed: 2 * time.Second, Uploaded: 40, Remaining: 960,
		RemainingBytes: 960 * MiB, UploadedBytes: 40 * MiB,
	})
	if !ok || second.To.Connections != 4 {
		t.Fatalf("second step = %+v (changed: %v), want a doubling to 4", second, ok)
	}

	// This window is slower than the one before the growth, so the pool has
	// stopped scaling and the controller stops with it.
	if _, ok := c.Observe(autotune.Progress{
		Elapsed: 3 * time.Second, Uploaded: 45, Remaining: 955,
		RemainingBytes: 955 * MiB, UploadedBytes: 45 * MiB,
	}); ok {
		t.Fatal("a step that did not pay off must not be followed by another")
	}
	stopped, why := c.Stopped()
	if !stopped {
		t.Fatal("the controller must settle once growing stops helping")
	}
	if !strings.Contains(why, "no faster") {
		t.Errorf("the reason should say the measurement did not improve, got %q", why)
	}
	if c.Settings().Connections != 4 {
		t.Errorf("settled at %d connections, want the last accepted 4", c.Settings().Connections)
	}
}

// TestControllerLeavesShortAndMetadataOnlyPhasesAlone: the two cases the
// stored sweeps say must not grow a pool are a run that finishes before a
// handshake would pay for itself, and a redeploy whose files are all skipped.
func TestControllerLeavesShortAndMetadataOnlyPhasesAlone(t *testing.T) {
	t.Run("too early to say anything", func(t *testing.T) {
		c := autotune.NewController(start, house, autotune.Fixed{})
		if _, ok := c.Observe(autotune.Progress{
			Elapsed: 100 * time.Millisecond, Uploaded: 1, Remaining: 999,
			RemainingBytes: 999 * MiB, UploadedBytes: MiB,
		}); ok {
			t.Error("a 100 ms window is not a measurement")
		}
	})

	t.Run("a redeploy that skips everything", func(t *testing.T) {
		c := autotune.NewController(autotune.Settings{Connections: 1, Concurrency: 64, RequestConcurrency: 16}, house, autotune.Fixed{})
		for i := 1; i <= 5; i++ {
			p := autotune.Progress{
				Elapsed:   time.Duration(i) * time.Second,
				Skipped:   i * 100,
				Remaining: 500 - i*100,
			}
			if _, ok := c.Observe(p); ok {
				t.Fatalf("a phase that moved no bytes must not open connections (observation %d)", i)
			}
		}
		if c.Settings().Connections != 1 {
			t.Errorf("connections = %d, want to stay at 1", c.Settings().Connections)
		}
	})

	t.Run("fewer files left than connections", func(t *testing.T) {
		c := autotune.NewController(autotune.Settings{Connections: 4, Concurrency: 64, RequestConcurrency: 16}, house, autotune.Fixed{})
		if _, ok := c.Observe(autotune.Progress{
			Elapsed: 5 * time.Second, Uploaded: 996, Remaining: 4,
			RemainingBytes: 4 * MiB, UploadedBytes: 996 * MiB,
		}); ok {
			t.Error("a fifth connection cannot help four remaining files")
		}
	})
}

// TestControllerStandsDownWhenTheServerPushesBack: a refused connection or a
// retrying transfer means the far side is already at a limit. Adding load then
// is the wrong instinct, and the reason has to be legible in the log.
func TestControllerStandsDownWhenTheServerPushesBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    autotune.Progress
		why  string
	}{
		{"a refused connection", autotune.Progress{Refused: true}, "refused"},
		{"a retrying transfer", autotune.Progress{Failures: 1}, "retrying"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := autotune.NewController(start, house, autotune.Fixed{})
			p := tc.p
			p.Elapsed, p.Uploaded, p.Remaining = 2*time.Second, 100, 900
			p.RemainingBytes, p.UploadedBytes = 900*MiB, 100*MiB
			if _, ok := c.Observe(p); ok {
				t.Fatal("the pool must not grow while the server is pushing back")
			}
			stopped, why := c.Stopped()
			if !stopped || !strings.Contains(why, tc.why) {
				t.Errorf("stopped=%v reason=%q, want a stop mentioning %q", stopped, why, tc.why)
			}
		})
	}
}

// TestControllerNeverOverridesTheConfiguration: with advanced.connections
// pinned there is nothing the runtime half is allowed to do, and it must not
// even start.
func TestControllerNeverOverridesTheConfiguration(t *testing.T) {
	c := autotune.NewController(autotune.Settings{Connections: 2, Concurrency: 64, RequestConcurrency: 16},
		house, autotune.Fixed{Connections: 2})
	stopped, why := c.Stopped()
	if !stopped || !strings.Contains(why, "configuration") {
		t.Fatalf("a pinned pool must switch the controller off, got stopped=%v %q", stopped, why)
	}
	if _, ok := c.Observe(autotune.Progress{
		Elapsed: 10 * time.Second, Uploaded: 10, Remaining: 990,
		RemainingBytes: 990 * MiB, UploadedBytes: MiB,
	}); ok {
		t.Error("a stopped controller changed something anyway")
	}
}

// TestControllerStopsAtTheCeiling: once the pool is as wide as the policy
// allows there is nothing left to decide, and the loop should not keep asking.
func TestControllerStopsAtTheCeiling(t *testing.T) {
	c := autotune.NewController(autotune.Settings{Connections: autotune.MaxConnections, Concurrency: 64}, house, autotune.Fixed{})
	if stopped, why := c.Stopped(); !stopped || !strings.Contains(why, "maximum") {
		t.Errorf("stopped=%v reason=%q, want a stop at the ceiling", stopped, why)
	}
}

// TestControllerExtrapolatesAnUnsettledUploadSet is the case Plan cannot see:
// an overlay redeploy with advanced.skip_unchanged starts sized for its stats,
// and only the ratio of real uploads to skips, observed while it runs, says
// whether it is a metadata sweep or a full deploy in disguise.
func TestControllerExtrapolatesAnUnsettledUploadSet(t *testing.T) {
	c := autotune.NewController(start, house, autotune.Fixed{})
	// Half of the first 200 files turned out to be changed, at about 4 MiB/s
	// on the one stream: the 800 that are left are another 400 uploads.
	change, ok := c.Observe(autotune.Progress{
		Elapsed: time.Second, Uploaded: 100, Skipped: 100, Remaining: 800,
		RemainingBytes: 800 * MiB, UploadedBytes: 4 * MiB,
	})
	if !ok || change.To.Connections <= change.From.Connections {
		t.Fatalf("a redeploy that is really uploading must widen the pool, got %+v (changed: %v)", change, ok)
	}
}
