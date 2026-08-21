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
//   - Connections grow. This is the whole point. The pool is dialed lazily, so
//     raising the number costs one handshake per connection that a worker
//     then really uses, and the same sqrt(W/H) rule decides whether the work
//     that is *left* justifies it.
//   - Connections never shrink. A handshake already paid is sunk, and closing
//     a connection mid-transfer would abort the files on it.
//   - Concurrency never moves. Plan already sizes it to the number of
//     independent items, which is the largest value that can ever be busy;
//     there is nothing for a hill climb to find.
//   - request_concurrency never moves. It is pkg/sftp's
//     MaxConcurrentRequestsPerFile, fixed when the client is created, so
//     changing it would mean reconnecting.
//
// The controller is deliberately dull. It re-plans, it grows at most to double
// the current pool per step, and it stops for good the first time a step fails
// to pay off or the server pushes back. A deploy tool that oscillates its
// connection count is worse than one that settles on a slightly wrong number.
const (
	// minObservation is how much transfer has to have happened before the
	// first decision. Short enough that a run of a few seconds still gets one
	// adjustment, long enough that the measurement is not one file's noise.
	minObservation = 750 * time.Millisecond

	// minSamples is the other half of that: a window that saw a single file
	// finish says nothing about throughput.
	minSamples = 4

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

	// stopped latches once there is nothing left to try.
	stopped bool
	reason  string

	// last is the previous accepted observation, and pending records the
	// throughput measured just before the most recent growth so the next
	// observation can tell whether that growth paid off.
	last        Progress
	haveLast    bool
	pendingRate float64
}

// NewController returns the controller for a deployment that Plan resolved to
// start. It reports nothing to change while every setting it could move is
// pinned by the user or already at its ceiling.
func NewController(start Settings, l Link, f Fixed) *Controller {
	c := &Controller{fixed: f, link: l, current: start}
	switch {
	case f.Connections > 0:
		c.stop("connections come from the configuration")
	case start.Connections >= MaxConnections:
		c.stop("the pool is already at the policy maximum")
	case start.Concurrency <= 1:
		c.stop("only one file is in flight, so a second connection has nobody to serve")
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
}

func (c Change) String() string {
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
	case p.Remaining <= c.current.Connections:
		// Fewer files left than connections open: a new one would finish its
		// handshake with nothing to carry.
		return none, false
	}
	if p.Elapsed < minObservation || p.Uploaded+p.Skipped < minSamples {
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
	// next one, so a pool that stopped scaling stops growing.
	if c.pendingRate > 0 {
		if rate < c.pendingRate*improvementBand {
			c.stop(fmt.Sprintf("%s is no faster than the %s before the last connection was added, so the pool stays at %d",
				rateString(rate), rateString(c.pendingRate), c.current.Connections))
			return none, false
		}
		c.pendingRate = 0
	}

	want := Plan(c.remaining(p), c.measuredLink(p, rate), c.fixed)
	grown := min(want.Connections, c.current.Connections*growthFactor, MaxConnections, c.current.Concurrency, p.Remaining)
	if grown <= c.current.Connections {
		return none, false
	}
	change := Change{From: c.current, To: c.current, Rate: rate}
	c.pendingRate = rate
	c.current.Connections = grown
	change.To = c.current
	if c.current.Connections >= MaxConnections {
		c.stop("the pool reached the policy maximum")
	}
	return change, true
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
func (c *Controller) measuredLink(p Progress, rate float64) Link {
	streams := min(c.current.Connections, c.current.Concurrency, p.Uploaded+p.Remaining)
	l := c.link
	l.StreamBytesPerSecond = rate / float64(max(streams, 1))
	return l
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
		largest = bytes / int64(uploads)
	}
	return Workload{Uploads: uploads, UploadBytes: bytes, LargestUpload: largest, Probes: probes}
}

func rateString(bytesPerSecond float64) string {
	return humanBytes(int64(bytesPerSecond)) + "/s"
}
