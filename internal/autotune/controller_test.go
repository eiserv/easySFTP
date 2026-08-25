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

// byteHeavy is the workload behind that configuration: a thousand files of a
// megabyte each. The size matters as much as the count, because a byte rate is
// only evidence about a link when the files are big enough to fill a stream
// (see the controller's bandwidthBound).
var byteHeavy = autotune.Workload{
	Uploads: 1000, UploadBytes: 1000 * MiB,
	LargestUpload: MiB, P50Upload: MiB, P90Upload: MiB,
}

// TestControllerGrowsOnMeasuredThroughputThenStops is the whole of stage 3 in
// one run: the transfer turns out to be far slower per stream than the prior
// assumed, so the pool grows; it grows one doubling at a time; and the moment
// a step stops paying for itself the controller settles instead of climbing on.
func TestControllerGrowsOnMeasuredThroughputThenStops(t *testing.T) {
	c := autotune.NewController(byteHeavy, start, house, autotune.Fixed{})
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
	// stopped scaling. The step is taken back rather than merely stopped at:
	// the four connections stay open and the files that are left go over the
	// two that were carrying them before (issue #215, stage 5).
	back, ok := c.Observe(autotune.Progress{
		Elapsed: 3 * time.Second, Uploaded: 45, Remaining: 955,
		RemainingBytes: 955 * MiB, UploadedBytes: 45 * MiB,
	})
	if !ok {
		t.Fatal("a step that did not pay off must be taken back")
	}
	if !back.Shrinks() || back.From.Connections != 4 || back.To.Connections != 2 {
		t.Fatalf("step back went %d -> %d (shrinks: %v), want the spread returned to 2",
			back.From.Connections, back.To.Connections, back.Shrinks())
	}
	if !strings.Contains(back.String(), "without closing any") {
		t.Errorf("a step back must say the connections stay open: %s", back)
	}
	stopped, why := c.Stopped()
	if !stopped {
		t.Fatal("the controller must settle once growing stops helping")
	}
	if !strings.Contains(why, "did not improve") {
		t.Errorf("the reason should say the measurement did not improve, got %q", why)
	}
	if c.Settings().Connections != 2 {
		t.Errorf("settled at %d connections, want the spread back at 2", c.Settings().Connections)
	}
}

// TestControllerLeavesShortAndMetadataOnlyPhasesAlone: the two cases the
// stored sweeps say must not grow a pool are a run that finishes before a
// handshake would pay for itself, and a redeploy whose files are all skipped.
func TestControllerLeavesShortAndMetadataOnlyPhasesAlone(t *testing.T) {
	t.Run("too early to say anything", func(t *testing.T) {
		c := autotune.NewController(byteHeavy, start, house, autotune.Fixed{})
		if _, ok := c.Observe(autotune.Progress{
			Elapsed: 100 * time.Millisecond, Uploaded: 1, Remaining: 999,
			RemainingBytes: 999 * MiB, UploadedBytes: MiB,
		}); ok {
			t.Error("a 100 ms window is not a measurement")
		}
	})

	t.Run("a redeploy that skips everything", func(t *testing.T) {
		c := autotune.NewController(byteHeavy, autotune.Settings{Connections: 1, Concurrency: 64, RequestConcurrency: 16}, house, autotune.Fixed{})
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
		c := autotune.NewController(byteHeavy, autotune.Settings{Connections: 4, Concurrency: 64, RequestConcurrency: 16}, house, autotune.Fixed{})
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
			c := autotune.NewController(byteHeavy, start, house, autotune.Fixed{})
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
	c := autotune.NewController(byteHeavy, autotune.Settings{Connections: 2, Concurrency: 64, RequestConcurrency: 16},
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
	c := autotune.NewController(byteHeavy, autotune.Settings{Connections: autotune.MaxConnections, Concurrency: 64}, house, autotune.Fixed{})
	if stopped, why := c.Stopped(); !stopped || !strings.Contains(why, "maximum") {
		t.Errorf("stopped=%v reason=%q, want a stop at the ceiling", stopped, why)
	}
}

// TestControllerExtrapolatesAnUnsettledUploadSet is the case Plan cannot see:
// an overlay redeploy with advanced.skip_unchanged starts sized for its stats,
// and only the ratio of real uploads to skips, observed while it runs, says
// whether it is a metadata sweep or a full deploy in disguise.
func TestControllerExtrapolatesAnUnsettledUploadSet(t *testing.T) {
	c := autotune.NewController(byteHeavy, start, house, autotune.Fixed{})
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

// TestControllerNeedsBytesBeforeItDecides is stage 6 of issue #215. The window
// that grew the stored 'small' sweep from four connections to eight was
// 250 ms long and carried a few dozen kilobytes: it satisfied "enough time" and
// "enough files" while saying nothing at all about throughput. A window has to
// carry real bytes before it may size a pool.
func TestControllerNeedsBytesBeforeItDecides(t *testing.T) {
	c := autotune.NewController(byteHeavy, start, house, autotune.Fixed{})

	// 4 KiB files, one second in: long enough, four files, 16 KiB moved.
	if _, ok := c.Observe(autotune.Progress{
		Elapsed: time.Second, Uploaded: 4, Remaining: 296,
		RemainingBytes: 296 * 4 * KiB, UploadedBytes: 4 * 4 * KiB,
	}); ok {
		t.Fatal("16 KiB is not a throughput measurement, whatever the clock says")
	}

	// A window that never becomes evidence must not become the baseline
	// either: the next report is still measured from the start of the phase,
	// so the evidence accumulates instead of being reset every tick.
	if _, ok := c.Observe(autotune.Progress{
		Elapsed: 2 * time.Second, Uploaded: 8, Remaining: 292,
		RemainingBytes: 292 * 4 * KiB, UploadedBytes: 8 * 4 * KiB,
	}); ok {
		t.Fatal("two thin windows in a row are not one thick one")
	}

	// The same phase once it has really moved something.
	if _, ok := c.Observe(autotune.Progress{
		Elapsed: 3 * time.Second, Uploaded: 200, Remaining: 800,
		RemainingBytes: 800 * MiB, UploadedBytes: 8 * MiB,
	}); !ok {
		t.Error("a window carrying megabytes over three seconds is a measurement")
	}
}

// TestControllerSettlesAfterASingleStepBack: taking one step back is a
// correction, taking a second would be the start of an oscillation. Whatever
// arrives after the reversal, the spread stays where the reversal put it.
func TestControllerSettlesAfterASingleStepBack(t *testing.T) {
	c := autotune.NewController(byteHeavy, start, house, autotune.Fixed{})

	if _, ok := c.Observe(autotune.Progress{
		Elapsed: time.Second, Uploaded: 10, Remaining: 990,
		RemainingBytes: 990 * MiB, UploadedBytes: 10 * MiB,
	}); !ok {
		t.Fatal("a stream measured far slower than the prior must widen the pool")
	}
	back, ok := c.Observe(autotune.Progress{
		Elapsed: 2 * time.Second, Uploaded: 18, Remaining: 982,
		RemainingBytes: 982 * MiB, UploadedBytes: 18 * MiB,
	})
	if !ok || !back.Shrinks() || back.To.Connections != 1 {
		t.Fatalf("a step that bought nothing must go back to 1, got %+v (changed: %v)", back, ok)
	}
	for i := range 3 {
		if _, ok := c.Observe(autotune.Progress{
			Elapsed: time.Duration(3+i) * time.Second, Uploaded: 100 * (i + 1), Remaining: 900 - 100*i,
			RemainingBytes: int64(900-100*i) * MiB, UploadedBytes: int64(100*(i+1)) * MiB,
		}); ok {
			t.Fatalf("the controller moved again after settling (observation %d)", i)
		}
	}
	if c.Settings().Connections != 1 {
		t.Errorf("settled at %d connections, want the spread left where the step back put it", c.Settings().Connections)
	}
}

// TestControllerJudgesAStepEvenAtTheEndOfAPhase: "fewer files left than
// connections" stops the controller opening more, but it must not stop it
// noticing that the last step it took was a mistake, or a pool that grew on the
// second-to-last window would never be corrected.
func TestControllerJudgesAStepEvenAtTheEndOfAPhase(t *testing.T) {
	c := autotune.NewController(byteHeavy, start, house, autotune.Fixed{})
	if _, ok := c.Observe(autotune.Progress{
		Elapsed: time.Second, Uploaded: 10, Remaining: 990,
		RemainingBytes: 990 * MiB, UploadedBytes: 10 * MiB,
	}); !ok {
		t.Fatal("the pool must widen first for there to be anything to judge")
	}
	back, ok := c.Observe(autotune.Progress{
		Elapsed: 2 * time.Second, Uploaded: 999, Remaining: 1,
		RemainingBytes: MiB, UploadedBytes: 20 * MiB,
	})
	if !ok || !back.Shrinks() {
		t.Errorf("a phase running out of files must still take back a step that did not pay: %+v (changed: %v)", back, ok)
	}
}

// TestTheStepThatReachesTheCeilingIsStillJudged: the pool's maximum used to
// latch the controller the moment a step reached it, so the one step that could
// no longer be undone was also the one step nobody checked. That is the shape
// of the stored 'small' miss, which widened from four connections to eight and
// stayed there.
func TestTheStepThatReachesTheCeilingIsStillJudged(t *testing.T) {
	near := autotune.Settings{Connections: autotune.MaxConnections / 2, Concurrency: 64, RequestConcurrency: 16}
	c := autotune.NewController(byteHeavy, near, house, autotune.Fixed{})

	up, ok := c.Observe(autotune.Progress{
		Elapsed: time.Second, Uploaded: 10, Remaining: 990,
		RemainingBytes: 990 * MiB, UploadedBytes: 10 * MiB,
	})
	if !ok || up.To.Connections != autotune.MaxConnections {
		t.Fatalf("step = %+v (changed: %v), want a doubling to the ceiling", up, ok)
	}
	if stopped, why := c.Stopped(); stopped {
		t.Fatalf("reaching the ceiling must not stop the controller before it judged the step: %s", why)
	}

	back, ok := c.Observe(autotune.Progress{
		Elapsed: 2 * time.Second, Uploaded: 18, Remaining: 982,
		RemainingBytes: 982 * MiB, UploadedBytes: 18 * MiB,
	})
	if !ok || !back.Shrinks() || back.To.Connections != near.Connections {
		t.Fatalf("step back = %+v (changed: %v), want the spread returned to %d", back, ok, near.Connections)
	}

	// And when the step does pay for itself, the controller settles at the
	// ceiling instead of observing forever.
	c = autotune.NewController(byteHeavy, near, house, autotune.Fixed{})
	c.Observe(autotune.Progress{
		Elapsed: time.Second, Uploaded: 10, Remaining: 990,
		RemainingBytes: 990 * MiB, UploadedBytes: 10 * MiB,
	})
	if _, ok := c.Observe(autotune.Progress{
		Elapsed: 2 * time.Second, Uploaded: 60, Remaining: 940,
		RemainingBytes: 940 * MiB, UploadedBytes: 60 * MiB,
	}); ok {
		t.Error("there is nothing above the ceiling to change to")
	}
	stopped, why := c.Stopped()
	if !stopped || !strings.Contains(why, "policy maximum") {
		t.Errorf("a validated step to the ceiling must settle there, got stopped=%v (%s)", stopped, why)
	}
}

// TestAByteRateIsOnlyEvidenceWhenTheFilesCanFillAStream is the stored 'small'
// miss stated as a rule. 300 files of 4 KiB are limited by four round-trips
// each, not by bandwidth, so their megabytes-per-second is a restatement of
// their files-per-second. Read as a bandwidth it makes the link look dead, the
// work that is left look enormous, and buys connections that measured slower
// than the two the sweep preferred.
func TestAByteRateIsOnlyEvidenceWhenTheFilesCanFillAStream(t *testing.T) {
	tiny := autotune.Workload{
		Uploads: 300, UploadBytes: 300 * 4 * KiB,
		LargestUpload: 4 * KiB, P50Upload: 4 * KiB, P90Upload: 4 * KiB,
	}
	c := autotune.NewController(tiny, autotune.Settings{Connections: 4, Concurrency: 64, RequestConcurrency: 16},
		house, autotune.Fixed{})

	// A window that satisfies every eligibility rule and reads as 0.7 MiB/s
	// over four connections. The old model divided that by the streams,
	// concluded each was carrying 180 KiB/s, and asked for more of them.
	if change, ok := c.Observe(autotune.Progress{
		Elapsed: 2 * time.Second, Uploaded: 150, Remaining: 150,
		RemainingBytes: 150 * 4 * KiB, UploadedBytes: MiB + MiB/2,
	}); ok {
		t.Errorf("a tree of one-packet files must not buy connections with its byte rate: %+v", change)
	}
	if c.Settings().Connections != 4 {
		t.Errorf("connections = %d, want the pre-transfer choice left alone", c.Settings().Connections)
	}

	// The same numbers where the bytes really do come from files that can fill
	// a stream: now the measurement means what it says and the pool grows.
	fat := tiny
	fat.LargestUpload, fat.P50Upload, fat.P90Upload = 8*MiB, 4*MiB, 8*MiB
	fat.UploadBytes = 300 * 4 * MiB
	c = autotune.NewController(fat, autotune.Settings{Connections: 4, Concurrency: 64, RequestConcurrency: 16},
		house, autotune.Fixed{})
	if _, ok := c.Observe(autotune.Progress{
		Elapsed: 2 * time.Second, Uploaded: 150, Remaining: 150,
		RemainingBytes: 150 * 4 * MiB, UploadedBytes: MiB + MiB/2,
	}); !ok {
		t.Error("600 MiB left over a link measured this slow is worth another connection")
	}
}

// TestTheControllerStandsDownWhereItCannotBeCashed is issue #217. A spread
// change re-points the files that have not been handed to a connection yet, so
// a deployment with as many workers as files has nothing for it to re-point:
// every file takes its connection when its worker starts, which is before the
// first window can close.
//
// The stored 'mixed' sweep is the case. 56 files, 56 workers, six connections
// chosen up front and a runtime step to eight that no transfer ever read: the
// run reported connections=8 and took what six connections take.
func TestTheControllerStandsDownWhereItCannotBeCashed(t *testing.T) {
	mixed := autotune.Workload{
		Uploads: 56, UploadBytes: 12 * MiB,
		LargestUpload: 2 * MiB, P50Upload: 64 * KiB, P90Upload: MiB,
	}

	t.Run("as many workers as files", func(t *testing.T) {
		c := autotune.NewController(mixed,
			autotune.Settings{Connections: 6, Concurrency: 56, RequestConcurrency: 16},
			house, autotune.Fixed{})
		stopped, why := c.Stopped()
		if !stopped {
			t.Fatal("a deployment that starts every file at once has no queue for a wider pool to serve")
		}
		if !strings.Contains(why, "as many workers as files") {
			t.Errorf("reason = %q, want it to name the worker count", why)
		}
		if _, ok := c.Observe(autotune.Progress{
			Elapsed: 2 * time.Second, Uploaded: 20, Remaining: 36,
			RemainingBytes: 8 * MiB, UploadedBytes: 4 * MiB,
		}); ok {
			t.Error("a stopped controller reported a change the transfer could not use")
		}
	})

	// The same payload behind fewer workers does have a queue, and the runtime
	// stage is worth running: this is what keeps the stop above from being a
	// blanket switch-off.
	t.Run("a queue behind the workers", func(t *testing.T) {
		c := autotune.NewController(mixed,
			autotune.Settings{Connections: 1, Concurrency: 8, RequestConcurrency: 16},
			house, autotune.Fixed{})
		if stopped, why := c.Stopped(); stopped {
			t.Fatalf("48 files queued behind 8 workers is work a wider pool can take: %s", why)
		}
		if _, ok := c.Observe(autotune.Progress{
			Elapsed: 2 * time.Second, Uploaded: 8, Remaining: 48,
			RemainingBytes: 10 * MiB, UploadedBytes: 2 * MiB,
		}); !ok {
			t.Error("a stream this slow with 40 files still queued must widen the pool")
		}
	})
}

// TestGrowthNeedsFilesThatHaveNotStarted is the same rule inside a running
// phase. A pool may not grow into the tail of a deployment whose remaining
// files are all already in flight, however many of them there are: they are
// bound to the connections they started on and cannot be moved.
func TestGrowthNeedsFilesThatHaveNotStarted(t *testing.T) {
	c := autotune.NewController(byteHeavy, start, house, autotune.Fixed{})

	// 64 workers and 64 files left: the queue is empty even though the phase
	// still has a tenth of its bytes to move.
	if change, ok := c.Observe(autotune.Progress{
		Elapsed: 2 * time.Second, Uploaded: 936, Remaining: 64,
		RemainingBytes: 64 * MiB, UploadedBytes: 100 * MiB,
	}); ok {
		t.Errorf("the pool grew into files that are all already on a connection: %+v", change)
	}
	if c.Settings().Connections != start.Connections {
		t.Errorf("connections = %d, want the pre-transfer choice left alone", c.Settings().Connections)
	}

	// One more file than there are workers, and the same window is worth
	// acting on again.
	if _, ok := c.Observe(autotune.Progress{
		Elapsed: 4 * time.Second, Uploaded: 940, Remaining: 500,
		RemainingBytes: 500 * MiB, UploadedBytes: 200 * MiB,
	}); !ok {
		t.Error("with files queued behind the workers the pool must still be allowed to grow")
	}
}
