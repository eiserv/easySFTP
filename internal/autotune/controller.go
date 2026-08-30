package autotune

import (
	"fmt"
	"time"
)

// Stage 3 of the policy: the part that runs while the transfer does.
//
// Plan has to guess one thing it cannot know before any bytes move, the link's
// throughput, and it guesses low on purpose (see assumedStreamBytesPerSecond).
// It also has to plan an overlay redeploy without knowing which of its files
// advanced.skip_unchanged will actually send. Controller closes both gaps with
// the only evidence that settles them: the transfer itself.
//
// What it may change, and what it deliberately may not:
//
//   - The spread grows. This is the whole point. The pool is dialed lazily, so
//     raising the number costs one handshake per connection that a worker
//     then really uses, and the same sqrt(W/H) rule decides whether the work
//     that is *left* justifies it.
//   - The spread comes back down when a step did not pay for itself, and that
//     is not the same as closing a connection. What moves is how many of the
//     open connections the files that are *left* are handed to; every
//     handshake already paid stays paid and every transfer already running
//     stays where it is (issue #215, stage 5). Before that the controller
//     could only widen a pool it had widened too far, and the stored 'small'
//     sweep is what that costs: 4 connections widened to 8 on one thin
//     window, on a workload whose measured optimum was 2.
//   - What counts as evidence is narrower than "a number arrived". A window has
//     to be long enough, see enough files and carry enough bytes, and a byte
//     rate is only read as a bandwidth when the files were large enough to
//     produce one (issue #215, stage 6, and see bandwidthBound).
//   - Neither direction reaches a file that is already on a connection. A
//     worker takes its connection when it picks up its file and keeps it until
//     that file is done, so what a spread change re-points is the *queue*
//     behind the workers. A deployment whose worker count covers every file
//     has no queue: it is decided before the first window closes, and this
//     controller says so rather than reporting a step nobody could cash
//     (issue #217, and see queued).
//   - Concurrency never moves. Plan already sizes it to the number of
//     independent items, which is the largest value that can ever be busy;
//     there is nothing for a hill climb to find.
//   - request_concurrency never moves. It is pkg/sftp's
//     MaxConcurrentRequestsPerFile, fixed when the client is created, so
//     changing it would mean reconnecting.
//
// The controller is deliberately dull. It re-plans, it grows at most to double
// the current pool per step, it takes at most one step back, and it stops for
// good the first time a step fails to pay off or the server pushes back. A
// deploy tool that oscillates its connection count is worse than one that
// settles on a slightly wrong number.
const (
	// minObservation is how long a window has to be before it is a
	// measurement. Short enough that a run of a few seconds still gets one
	// adjustment, long enough that the measurement is not one file's noise.
	//
	// It is the length of the *window*, not the time since the phase started:
	// every decision after the first one compares two windows, and a 250 ms
	// tick against a 750 ms baseline compares two different amounts of
	// evidence (issue #215, stage 6).
	minObservation = 750 * time.Millisecond

	// minSamples is the second half of that: a window that saw a single file
	// finish says nothing about throughput.
	minSamples = 4

	// minWindowBytes is the third. Time and file counts alone let a window of
	// mostly-empty files decide the size of the pool, which is how a 1 MiB
	// deployment of 4 KiB files ended up on eight connections. Half a
	// megabyte is sixteen write packets, one fully pipelined file at the
	// default request_concurrency, and it is deliberately a floor on
	// *evidence* rather than on the workload: a transfer too small to produce
	// it is a transfer too small to be worth a handshake either way.
	minWindowBytes = MinRequestConcurrency * packetBytes

	// improvementBand is how much better a step has to make things before the
	// controller believes it. Below this the two windows are the same
	// measurement, and growing again would be reading noise as a signal.
	improvementBand = 1.05

	// growthFactor bounds one step. Doubling reaches MaxConnections from one
	// connection in three steps, which is fast enough for a long run and slow
	// enough that a wrong first reading cannot commit the whole pool.
	growthFactor = 2
)

// Progress is what the upload phase reports to the controller. Everything is
// cumulative since the phase started, except Elapsed, which is the phase's own
// wall clock.
type Progress struct {
	Elapsed time.Duration

	// Uploaded and Skipped are files that finished, Remaining the ones that
	// have not. RemainingBytes is the planned size of those, which for an
	// unsettled workload (skip_unchanged) is an upper bound rather than a
	// promise.
	//
	// Remaining is the work that is left, not the work a wider pool could be
	// given: most of it is already in flight on the connections it started on.
	// What is still assignable is Remaining minus the workers, which is what
	// Controller.queued computes (issue #217).
	Uploaded       int
	Skipped        int
	Remaining      int
	RemainingBytes int64

	// UploadedBytes is what actually went over the wire. It is the numerator
	// of the throughput measurement, so it must count only real transfers and
	// never a skipped file's size.
	UploadedBytes int64

	// Refused is set once the session could not open an extra connection, and
	// Failures counts upload retries. Either one ends the growth: a server
	// that is already refusing or erroring is not a server to ask for more.
	Refused  bool
	Failures int
}

// Controller refines a Plan while the upload phase runs. The zero value is not
// usable; construct it with NewController. It is not safe for concurrent use,
// and the uploader calls it from a single goroutine.
type Controller struct {
	fixed   Fixed
	link    Link
	current Settings

	// workload is the shape of the transfer being watched. Only its
	// distribution is read: what the observed byte rate is evidence *of*
	// depends on whether the files are big enough for a byte rate to mean
	// anything. See bandwidthBound.
	workload Workload

	// stopped latches once there is nothing left to try.
	stopped bool
	reason  string

	// last is the previous accepted observation. An observation that is not
	// evidence yet (too short a window, too few files, too few bytes) does not
	// replace it, so the window widens until it is one instead of a thin
	// window deciding the size of the pool.
	last     Progress
	haveLast bool

	// pendingRate is the throughput measured just before the most recent
	// growth, and pendingFrom the spread it grew from, so the next window can
	// say whether that step paid for itself and, if it did not, where to put
	// the spread back.
	pendingRate float64
	pendingFrom int
}

// NewController returns the controller for a deployment that Plan resolved to
// start, watching the workload it was resolved for. It reports nothing to
// change while every setting it could move is pinned by the user or already at
// its ceiling.
func NewController(w Workload, start Settings, l Link, f Fixed) *Controller {
	c := &Controller{fixed: f, link: l, current: start, workload: w}
	switch {
	case f.Connections > 0:
		c.stop("connections come from the configuration")
	case start.Connections >= MaxConnections:
		c.stop("the pool is already at the policy maximum")
	case start.Concurrency <= 1:
		c.stop("only one file is in flight, so a second connection has nobody to serve")
	case start.Concurrency >= w.Items():
		// Every item starts at once, so every one of them is bound to a
		// connection before this controller has seen anything. Growing the
		// spread would move a number that no transfer reads (issue #217).
		c.stop("there are as many workers as files, so every file takes its connection at the start and the pool cannot grow into this deployment")
	}
	return c
}

// Change is one accepted runtime adjustment, with the measurement behind it,
// so the log line can say what moved and why in one sentence.
type Change struct {
	From, To Settings
	// Rate is the aggregate throughput of the window that motivated the
	// change, in bytes per second.
	Rate float64
	// Before is the throughput of the window before the step this change is
	// taking back. It is zero for a change that grows the spread, which has no
	// earlier step to compare against.
	Before float64
}

// Shrinks reports whether this change narrows the spread rather than widening
// it. The two are logged differently and counted separately, because "easySFTP
// asked for more connections" and "easySFTP stopped using some of the ones it
// has" are different events on a server.
func (c Change) Shrinks() bool { return c.To.Connections < c.From.Connections }

func (c Change) String() string {
	if c.Shrinks() {
		return fmt.Sprintf("%s is no better than the %s before the spread grew to %d; assigning the remaining files across %d connection(s) instead, without closing any",
			rateString(c.Rate), rateString(c.Before), c.From.Connections, c.To.Connections)
	}
	return fmt.Sprintf("%s over the connections in use; raising connections %d -> %d for the rest of this deployment",
		rateString(c.Rate), c.From.Connections, c.To.Connections)
}

// Settings is the configuration the controller currently stands behind.
func (c *Controller) Settings() Settings { return c.current }

// Stopped reports whether the controller has given up on further changes, and
// why. The reason is a log line, not an error.
func (c *Controller) Stopped() (bool, string) { return c.stopped, c.reason }

func (c *Controller) stop(reason string) {
	c.stopped, c.reason = true, reason
}

// Observe folds one progress report into the controller and reports the new
// settings when it decided to change something. The bool is false whenever
// nothing changed, which is the normal case: a controller that says nothing is
// a controller that is content.
func (c *Controller) Observe(p Progress) (Change, bool) {
	none := Change{From: c.current, To: c.current}
	if c.stopped {
		return none, false
	}
	switch {
	case p.Refused:
		// The pool already hit a server-side limit. Asking again would only
		// produce more refusals and more warnings.
		c.stop("the server refused an extra connection")
		return none, false
	case p.Failures > 0:
		c.stop("the transfer is retrying, so this is not the moment to add load")
		return none, false
	}
	if !c.eligible(p) {
		return none, false
	}

	rate := c.throughput(p)
	c.last, c.haveLast = p, true
	if rate <= 0 {
		// Nothing measurable moved in this window. A metadata-only phase (a
		// redeploy that skips everything) lands here and is left alone, which
		// is what the stored redeploy sweep says is right.
		return none, false
	}

	// Did the previous growth pay for itself? Checked before planning the
	// next one, so a pool that stopped scaling stops growing, and answered by
	// putting the spread back where it came from rather than by living with
	// it (issue #215, stage 5).
	if c.pendingRate > 0 {
		if rate < c.pendingRate*improvementBand {
			return c.reject(rate), true
		}
		c.pendingRate, c.pendingFrom = 0, 0
		if c.current.Connections >= MaxConnections {
			// The step that reached the ceiling paid for itself, so there is
			// nothing left to grow into and nothing left to take back. This is
			// checked *here* rather than when the step was taken: a controller
			// that latched at the maximum could never judge the step that got
			// it there, which is exactly the step most worth judging.
			c.stop("the pool reached the policy maximum")
			return none, false
		}
	}

	queued := c.queued(p)
	if queued <= c.current.Connections {
		// Nothing waiting behind the workers: every file that is left is
		// already on the connection it started on, so a new one would finish
		// its handshake with nothing to carry. Checked after the validation
		// above, so a step that has already been taken is still judged.
		return none, false
	}

	want := Plan(c.remaining(p), c.measuredLink(p, rate), c.fixed)
	grown := min(want.Connections, c.current.Connections*growthFactor, MaxConnections, c.current.Concurrency, queued)
	if grown <= c.current.Connections {
		return none, false
	}
	change := Change{From: c.current, To: c.current, Rate: rate}
	c.pendingRate, c.pendingFrom = rate, c.current.Connections
	c.current.Connections = grown
	change.To = c.current
	return change, true
}

// queued is how much of the work that is left a wider pool could still be
// given: the files that have not been handed to a connection yet.
//
// A worker takes its connection when it picks up its file and keeps it for that
// file's whole transfer, so a growth step re-points the queue and never a
// transfer in flight. The upload loop keeps its workers full while anything is
// left, which puts min(remaining, workers) files in flight and leaves the
// difference queued behind them.
//
// Before issue #217 this read Remaining, the files that have not *finished*,
// and so counted transfers that could not be moved as work a new connection
// might take. That is how the stored 'mixed' sweep came to report a growth step
// it never cashed: 56 files and 56 workers, every one of them bound to one of
// six connections before the first window closed, and a spread raised to eight
// that nothing ever read.
func (c *Controller) queued(p Progress) int {
	return max(p.Remaining-c.current.Concurrency, 0)
}

// eligible reports whether this report closes a window worth deciding on: long
// enough, over enough finished files, and carrying enough bytes that the number
// is a throughput rather than one file's latency.
//
// A report that fails any of the three is not an observation at all: it does
// not replace the baseline, so the window keeps widening until it is evidence.
// That is the difference between "measure every 250 ms and hope" and "decide
// when there is something to decide on" (issue #215, stage 6).
func (c *Controller) eligible(p Progress) bool {
	window, files, bytes := p.Elapsed, p.Uploaded+p.Skipped, p.UploadedBytes
	if c.haveLast {
		window -= c.last.Elapsed
		files -= c.last.Uploaded + c.last.Skipped
		bytes -= c.last.UploadedBytes
	}
	if window < minObservation || files < minSamples {
		return false
	}
	// A phase that is moving no bytes at all (a redeploy skipping everything)
	// is judged on its files: it is never going to reach a byte threshold, and
	// the throughput check below leaves it alone anyway.
	return bytes == 0 || bytes >= minWindowBytes
}

// reject takes back the growth step the previous window motivated. The
// connections opened for it stay open and every transfer running on them runs
// to the end; what changes is where the files that are *left* go. Then the
// controller settles: one step back is a correction, and a second one would be
// the start of an oscillation.
func (c *Controller) reject(rate float64) Change {
	change := Change{From: c.current, To: c.current, Rate: rate, Before: c.pendingRate}
	c.current.Connections = c.pendingFrom
	change.To = c.current
	c.pendingRate, c.pendingFrom = 0, 0
	c.stop(fmt.Sprintf("%s did not improve on the %s measured before the last connection was added, so the remaining files go over %d connection(s)",
		rateString(rate), rateString(change.Before), c.current.Connections))
	return change
}

// throughput is the aggregate bytes per second of the window since the last
// observation, falling back to the whole phase for the first one. A window is
// used rather than the running average so a pool that just grew is judged on
// what it did after growing.
func (c *Controller) throughput(p Progress) float64 {
	bytes, window := p.UploadedBytes, p.Elapsed
	if c.haveLast {
		bytes -= c.last.UploadedBytes
		window -= c.last.Elapsed
	}
	if bytes <= 0 || window <= 0 {
		return 0
	}
	return float64(bytes) / window.Seconds()
}

// measuredLink is the link with the prior replaced by what one connection is
// really carrying. The aggregate is divided by the streams that were actually
// serving files, not by the pool size: a pool whose tail no worker reached
// would make every connection look slower than it is.
//
// For a workload that cannot fill a stream the prior stays, because the
// measurement is not one. See bandwidthBound.
func (c *Controller) measuredLink(p Progress, rate float64) Link {
	l := c.link
	if !c.bandwidthBound() {
		return l
	}
	streams := min(c.current.Connections, c.current.Concurrency, p.Uploaded+p.Remaining)
	l.StreamBytesPerSecond = rate / float64(max(streams, 1))
	l.ThroughputSource = SourceRuntime // the transfer being tuned, which outranks everything else
	return l
}

// bandwidthBound reports whether this transfer's byte rate says anything about
// the link's bandwidth.
//
// A deployment of files that each fit in a single 32 KiB write packet never
// fills a stream: what limits it is four round-trips per file, not bytes per
// second, and its megabytes-per-second reading is a restatement of its
// files-per-second reading. Feeding that number back in as a bandwidth makes
// the link look catastrophically slow, which makes the remaining work look
// enormous, which buys connections that cannot help. That is the stored 'small'
// miss: 300 files of 4 KiB, widened from four connections to eight, measured
// fastest at two.
//
// The round-trip half of the model does not need a runtime correction, because
// nothing about it was guessed: the RTT was measured before the first file and
// the file count is known. So for these workloads the pre-transfer plan already
// had every input it was going to get, and the controller's job is only to
// re-plan the work that is *left* (which is what an unsettled skip_unchanged
// set still needs).
//
// The ninetieth percentile is the test rather than the mean or the largest
// file, so a tree of small files with a few archives in it, where the bytes
// really do come from files that can fill a stream, still gets a runtime
// correction.
func (c *Controller) bandwidthBound() bool { return BandwidthBound(c.workload) }

// BandwidthBound reports whether a transfer of this workload produces a byte
// rate that says something about the link's bandwidth. See the comment on
// Controller.bandwidthBound for why the answer is not simply "it moved bytes",
// and internal/autocache, which asks the same question of a stored shape
// before reusing what was measured on it.
func BandwidthBound(w Workload) bool {
	size := w.P90Upload
	if size == 0 {
		// A workload with no distribution: the largest file is all there is.
		size = w.LargestUpload
	}
	return size > packetBytes
}

// Measure turns one finished transfer into a per-connection throughput, or
// reports that the transfer was not a measurement of the link at all.
//
// It is the same judgement Controller makes on one of its windows, asked of a
// whole phase instead: enough time, enough bytes, and files large enough for a
// byte rate to be a bandwidth. The aggregate is divided by the streams that
// were really carrying files, not by the pool size, for the reason
// Controller.measuredLink gives.
//
// A phase includes what a window deliberately excludes (the ramp-up, the tail
// where most workers have finished), so this is the more conservative of the
// two numbers. That is the right direction for something that will be stored
// and reused: a throughput remembered slightly low costs a handshake, one
// remembered high costs a pool that is too small for the work.
func Measure(w Workload, bytes int64, window time.Duration, streams int) (float64, bool) {
	switch {
	case !BandwidthBound(w):
		return 0, false
	case bytes < minWindowBytes || window < minObservation || streams < 1:
		return 0, false
	}
	return float64(bytes) / window.Seconds() / float64(streams), true
}

// remaining is the workload the run still has in front of it. For a settled
// workload that is simply the files that have not finished. For an unsettled
// one (advanced.skip_unchanged) the ratio observed so far is extrapolated: if
// four in five files were skipped, a fifth of what is left will be uploaded,
// and the bytes with it.
func (c *Controller) remaining(p Progress) Workload {
	done := p.Uploaded + p.Skipped
	uploads, bytes := p.Remaining, p.RemainingBytes
	probes := 0
	if done > 0 && p.Skipped > 0 {
		ratio := float64(p.Uploaded) / float64(done)
		uploads = int(float64(p.Remaining) * ratio)
		bytes = int64(float64(p.RemainingBytes) * ratio)
		// Every remaining file still costs its stat, whether or not it is
		// then sent.
		probes = p.Remaining
	}
	largest := int64(0)
	if uploads > 0 {
		// Progress does not identify which individual files remain. Preserve
		// the plan's largest-file bound instead of silently replacing it with
		// the mean, capped only by all bytes still left to upload.
		largest = min(c.workload.LargestUpload, bytes)
	}
	// The shape carries over from the plan: which files are left is not known,
	// but nothing observed so far suggests they are shaped differently from
	// the ones that were planned.
	return Workload{
		Uploads:       uploads,
		UploadBytes:   bytes,
		LargestUpload: largest,
		P50Upload:     c.workload.P50Upload,
		P90Upload:     c.workload.P90Upload,
		Probes:        probes,
	}
}

func rateString(bytesPerSecond float64) string {
	return humanBytes(int64(bytesPerSecond)) + "/s"
}
