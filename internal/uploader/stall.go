package uploader

import (
	"io"
	"sync/atomic"
	"time"
)

// stallWatchdog fails a run fast when its active transfers stop making
// progress. Transfers mark themselves active for the duration of each upload
// attempt and tick the watchdog on every read that moved bytes. A monitor
// goroutine fires when transfers are active but no tick arrived for the
// configured timeout; firing closes the SSH connection, which unblocks every
// SFTP operation stuck on the stalled server with an error, so the run fails
// in minutes with a clear message instead of hanging until the job-level
// timeout.
//
// Closing the whole connection (rather than aborting just the stalled file)
// is deliberate: all transfers share one SSH session, and a write blocked on
// an exhausted SSH channel window cannot be interrupted any other way. A
// server that stalls one transfer has stalled the session.
type stallWatchdog struct {
	timeout time.Duration
	kill    func() // closes the SSH connection
	log     Logger
	done    chan struct{}

	active atomic.Int64 // transfers currently inside an upload attempt
	ticks  atomic.Int64 // bumped whenever any transfer moves bytes
	fired  atomic.Bool
}

// startStallWatchdog launches the monitor goroutine; stop it with stop().
func startStallWatchdog(timeout time.Duration, kill func(), log Logger) *stallWatchdog {
	w := &stallWatchdog{timeout: timeout, kill: kill, log: log, done: make(chan struct{})}
	go w.monitor()
	return w
}

func (w *stallWatchdog) stop() { close(w.done) }

// begin marks one transfer attempt as active. The tick resets the silence
// clock, so a transfer that hangs in its very first operation (e.g. opening
// the remote file on a stalled server) still gets the full timeout window.
func (w *stallWatchdog) begin() { w.tick(); w.active.Add(1) }
func (w *stallWatchdog) end()   { w.active.Add(-1) }

// tick counts one unit of remote progress. Safe on a nil watchdog, so helpers
// inside multi-operation phases (remote scans, directory creation) can tick
// after every completed round-trip without threading nil checks around: a
// long but healthy phase must not read as silence.
func (w *stallWatchdog) tick() {
	if w != nil {
		w.ticks.Add(1)
	}
}

func (w *stallWatchdog) monitor() {
	interval := w.timeout / 4
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	last := w.ticks.Load()
	lastChange := time.Now()
	for {
		select {
		case <-w.done:
			return
		case <-t.C:
		}
		now := w.ticks.Load()
		if w.active.Load() == 0 || now != last {
			last = now
			lastChange = time.Now()
			continue
		}
		if time.Since(lastChange) >= w.timeout {
			w.fired.Store(true)
			w.log.Warningf("no transfer progress for %s; closing the connection so the run fails fast (stall-timeout)", w.timeout)
			w.kill()
			return
		}
	}
}

// reader wraps r so that every read that moved bytes counts as progress.
func (w *stallWatchdog) reader(r io.Reader) io.Reader { return &tickReader{w: w, r: r} }

type tickReader struct {
	w *stallWatchdog
	r io.Reader
}

func (t *tickReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.w.ticks.Add(1)
	}
	return n, err
}
