package uploader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/eiserv/easySFTP/internal/autotune"
	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/metrics"
)

// conn is one live SSH/SFTP client pair. Every field is guarded by
// session.mu, including while a redial of this connection is in flight: the
// dial itself runs with the lock released (see reconnect), so the fields keep
// describing the connection the rest of the run can still see.
type conn struct {
	ssh       *ssh.Client
	sftp      *sftp.Client
	closeJump func() // closes the jump-host transport; no-op for direct connections
	gen       int    // bumped on every successful reconnect of this connection
	// redialing is non-nil while this connection is being redialed, and is
	// closed when that attempt finishes either way. It is what collapses the
	// simultaneous redials of workers that all failed on the same connection
	// into one, now that holding session.mu across the dial no longer does it;
	// see reconnect and issue #224.
	redialing chan struct{}
}

// session holds the run's connections and can transparently redial one when it
// drops mid-run, so per-file retries run against a live client instead of
// burning their backoff on a dead one.
//
// A run opens advanced.connections of them (1 by default). Everything above
// that number is one connection's ceiling: one TCP congestion window and one
// cipher stream carry the whole run, which neither concurrency nor
// request_concurrency can lift (issue #158). Parallel uploads spread over the
// pool. Remote scans, directory setup, stale-temp sweeps and deletes may issue
// concurrent requests, but keep them on the first connection. Their reconnect
// behavior therefore stays independent of the upload pool.
type session struct {
	cfg  *config.Config
	tune *tuning
	log  Logger

	// dialMu serializes the handshakes this session opens, so that at most one
	// is in flight at a time. It is deliberately not mu: a handshake against an
	// unhealthy server can take arbitrarily long, and nothing that only reads
	// the session should wait for it. Never taken while holding mu.
	dialMu sync.Mutex

	mu    sync.Mutex
	conns []*conn // slot 0 is dialed up front, the rest on first use
	// spread is how many of those slots the current deployment hands files
	// out over. conns is allocated to the run's ceiling once; spread is what
	// the policy moves, per deployment and (upwards only) while a transfer
	// runs. A slot past spread is never dialed, which is what makes an
	// adaptive pool cost nothing until it is used.
	spread int
	// refused latches the first connection the server would not give us.
	// After that the pool stops asking: sshd's MaxStartups and the
	// per-account limits of shared hosting do not change mid-run, and one
	// warning is information while eight are noise.
	refused bool
	// reconnects spent so far, bounded by cfg.Retries and shared by every
	// connection: the input is a run-wide budget, and a server that drops
	// connections drops them faster with more of them open.
	reconnects int
	// dialing[slot] is non-nil while that slot's first dial is in flight and is
	// closed when it finishes, so a second worker landing on the same slot
	// waits for the result instead of opening a connection nobody asked for.
	// Same role as conn.redialing, for the other dial path.
	dialing []chan struct{}
}

// newSession dials the server and opens the initial SFTP session, retrying
// transient failures with the same exponential backoff and budget (the retries
// input) as per-file uploads: a momentary DNS hiccup or a restarting sshd
// should cost a short wait, not a red pipeline. Permanent failures, most
// importantly a host key mismatch (a security signal) and an authentication
// failure (retrying risks fail2ban-style lockouts), fail immediately. With a
// jump host configured, this covers either hop: a transient failure reaching
// the bastion is retried exactly like one reaching the target.
func newSession(ctx context.Context, cfg *config.Config, tune *tuning, log Logger) (*session, error) {
	var lastErr error
	for attempt := 0; attempt <= cfg.Retries; attempt++ {
		if attempt > 0 {
			backoff := retryBackoff(attempt)
			log.Warningf("could not connect; retrying in %s (attempt %d/%d): %v", backoff, attempt+1, cfg.Retries+1, lastErr)
			if err := sleepCtx(ctx, backoff); err != nil {
				return nil, err
			}
		}
		done := metrics.Op("ssh_connect")
		// Timed here as well as in the metrics op, because this is stage 2 of
		// the auto policy and not only instrumentation: what one extra
		// connection costs is exactly what this call just paid.
		start := time.Now()
		sshClient, sftpClient, closeJump, err := connect(cfg, tune.requestConcurrency(), log)
		handshake := time.Since(start)
		done(err)
		if err == nil {
			metrics.Count("connections_opened", 1)
			metrics.Count("connections_used", 1) // slot 0, dialed up front
			pool := poolCeiling(tune, log)
			s := &session{
				cfg:     cfg,
				tune:    tune,
				log:     log,
				conns:   make([]*conn, pool),
				dialing: make([]chan struct{}, pool),
				spread:  1,
			}
			s.conns[0] = &conn{ssh: sshClient, sftp: sftpClient, closeJump: closeJump}
			// Stage 2. The handshake is always recorded (it is what an extra
			// connection costs, and it was paid either way); the three extra
			// round-trips of the RTT probe are only spent when they could
			// still change an answer.
			if tune.wantsLinkProbe() {
				tune.setLink(probeLink(sftpClient, handshake))
			} else {
				tune.setLink(autotune.Link{Handshake: handshake})
			}
			// The slots allocated, not the ones the run will use: with the
			// count at auto this is the ceiling the policy may grow into, and
			// connections_opened/connections_used are what actually happened.
			metrics.Set("connections_pool_size", int64(len(s.conns)))
			return s, nil
		}
		lastErr = err
		if !isRetryableConnect(err) {
			break
		}
	}
	return nil, lastErr
}

// poolCeiling is how many connection slots the run allocates, which is the
// most it could ever open. Slots past the first are dialed on first use, so an
// unreached one costs a nil pointer; what this really bounds is how far
// setSpread may go.
//
// A pinned advanced.connections is still checked against a pinned
// advanced.concurrency here and says so: those two numbers are both the user's,
// and a connection no worker will ever pick is a handshake the user asked for
// by accident. When either is adaptive there is nothing to warn about, because
// the policy caps the spread against the workers itself.
func poolCeiling(tune *tuning, log Logger) int {
	n := tune.poolCeiling()
	if tune.fixed.Connections > 0 && tune.fixed.Concurrency > 0 && n > tune.fixed.Concurrency {
		log.Infof("advanced.connections is %d but only %d file(s) upload in parallel; opening at most %d connection(s)",
			n, tune.fixed.Concurrency, tune.fixed.Concurrency)
		n = tune.fixed.Concurrency
	}
	return max(n, 1)
}

// setSpread points the next files at n connections. It is the only way the
// pool grows: slots are dialed lazily, so raising the spread costs nothing
// until a worker lands on a fresh slot, and lowering it simply stops handing
// files to connections that are already open.
//
// The spread never exceeds the allocated slots, never exceeds a ceiling an
// earlier run against this server left behind (issue #212), and never grows
// again once the server has refused one.
//
// The ceiling is applied here rather than only in the plan because this is the
// one place every change goes through, the runtime controller's growth
// included: the pool is allocated to the policy's own maximum before the cache
// has been validated (slots are lazy, so that costs nothing), so the bound has
// to live where the spread moves.
func (s *session) setSpread(n int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n = min(max(n, 1), len(s.conns))
	if s.tune != nil {
		n = s.tune.clampConnections(n)
	}
	if s.refused && n > s.spread {
		return s.spread
	}
	s.spread = n
	return s.spread
}

// currentSpread is how many connections the current deployment is handing
// files to, which is how many streams a throughput measurement has to be
// divided by.
func (s *session) currentSpread() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return max(s.spread, 1)
}

// granted reports how many connections the server actually gave this run, and
// whether it ever said no. Together they are the evidence a connection ceiling
// is written from; see tuning.cacheObservation.
func (s *session) granted() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openSlots(), s.refused
}

// workers is how many of items one phase may have in flight. With
// advanced.concurrency pinned it is that number; with it at auto it is the
// phase's own item count, capped, so a delete sweep of 2000 files and an
// upload of three are both sized for what they are (see tuning.workers).
//
// A directly constructed session (a test that never went through Run) has no
// tuning and falls back to one worker, which is the safe reading of "nothing
// said".
func (s *session) workers(items int) int {
	if s.tune == nil {
		return 1
	}
	return s.tune.workers(items)
}

// refusedConnection reports whether the server has turned an extra connection
// down. The runtime controller reads it and stops growing.
func (s *session) refusedConnection() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

// permanentError marks a connect() failure that must never be retried: local
// configuration problems (unparsable key, bad fingerprint format) and, via the
// host key callback, a host key mismatch. It survives x/crypto/ssh's handshake
// wrapping (%w), so isRetryableConnect can detect it with errors.As.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// isRetryableConnect reports whether an initial-connection failure is worth
// another attempt. Anything tagged permanentError is not; neither is an SSH
// authentication failure, which x/crypto/ssh only reports as a string error.
// Both apply per hop, so a bad jump-host key or password fails fast too.
func isRetryableConnect(err error) bool {
	var pe permanentError
	if errors.As(err, &pe) {
		return false
	}
	return !strings.Contains(err.Error(), "ssh: unable to authenticate")
}

// acquire returns the live SFTP client serving the given file index, the
// connection behind it and that connection's generation. The generation is
// handed back to reconnect so that concurrent workers failing on the same
// dead connection trigger only one redial between them.
//
// Files are spread round-robin over the current spread, not over every
// allocated slot: the pool is sized once for the run and pointed at the number
// of connections this deployment decided on (see setSpread). Connections past
// the first are dialed on first use, so a run that uploads three files never
// pays for four handshakes.
//
// A server that refuses one (sshd's MaxSessions/MaxStartups, a per-account
// connection limit on shared hosting) costs a warning rather than the run:
// that slot falls back to the first connection, and the pool stops asking for
// more. Where the connection count was easySFTP's own choice the run also
// pulls its spread back in, so the refusal is answered once instead of being
// rediscovered slot by slot.
// The handshake itself runs with s.mu released, so a slot whose dial hangs
// against an unhealthy server cannot block the stall watchdog's closeSSH, the
// keepalive loop or any worker on a healthy connection (issue #224). A second
// worker landing on the same undialed slot waits on s.dialing[slot] instead,
// which keeps the "one handshake per slot" property the lock used to give.
func (s *session) acquire(index int) (*sftp.Client, *conn, int) {
	for {
		s.mu.Lock()
		if s.dialing == nil {
			// A session built directly (a test that never went through
			// newSession) has no dial slots; it does now.
			s.dialing = make([]chan struct{}, len(s.conns))
		}
		slot := index % max(s.spread, 1)
		if c := s.conns[slot]; c != nil {
			client, gen := c.sftp, c.gen
			s.mu.Unlock()
			return client, c, gen
		}
		if wait := s.dialing[slot]; wait != nil {
			s.mu.Unlock()
			<-wait
			continue
		}
		wait := make(chan struct{})
		s.dialing[slot] = wait
		s.mu.Unlock()

		c, err := s.dialPooled()

		s.mu.Lock()
		s.dialing[slot] = nil
		if err != nil {
			s.noteDialFailure(slot, err)
			c = s.conns[0]
		} else {
			metrics.Count("connections_opened", 1)
		}
		// Slots the run actually reached, however they resolved: a pool whose
		// tail is never touched (fewer files than connections) is the other
		// way a configured connection can end up unused.
		metrics.Count("connections_used", 1)
		s.conns[slot] = c
		client, gen := c.sftp, c.gen
		close(wait)
		s.mu.Unlock()
		return client, c, gen
	}
}

// connection snapshots the current client and generation of the connection a
// file already acquired. Retries use this instead of routing the file index
// through a possibly changed spread again, so runtime tuning cannot move a
// retry away from the connection it just reconnected.
func (s *session) connection(c *conn) (*sftp.Client, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return c.sftp, c.gen
}

// dialPooled opens one additional pooled connection with s.mu released, but
// with at most one handshake in flight for the whole session. That second half
// is what holding s.mu across the dial used to give and is worth keeping: a
// run that is told "no more connections" then stops asking after one attempt
// instead of after one per slot. What it no longer does is block anything that
// only needs to read the session, the stall watchdog's closeSSH included.
func (s *session) dialPooled() (*conn, error) {
	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	s.mu.Lock()
	refused := s.refused
	s.mu.Unlock()
	if refused {
		// Already told no once. Another handshake attempt would cost a
		// round-trip to be told no again.
		return nil, errPoolRefused
	}
	return s.dial()
}

// noteDialFailure records that the server would not give this run another
// connection and stops the pool asking again. A dial that was skipped because
// the pool had already been refused one is not a new refusal and is silent.
// Must be called with s.mu held.
func (s *session) noteDialFailure(slot int, err error) {
	if errors.Is(err, errPoolRefused) {
		return
	}
	metrics.Count("connections_refused", 1)
	s.refused = true
	if s.tune.autoConnections() {
		// easySFTP picked the number, so easySFTP takes the answer: pull the
		// spread back to what is open and say so once.
		s.spread = s.openSlots()
		s.log.Warningf("the server would not open more than %d SSH connection(s) (%v); easySFTP had chosen more and continues with %d",
			s.spread, err, s.spread)
		return
	}
	s.log.Warningf("could not open connection %d of %d (%v); this run continues on its first connection",
		slot+1, len(s.conns), err)
}

// closeConn tears one connection's transports down.
func closeConn(c *conn) {
	c.sftp.Close()
	c.ssh.Close()
	c.closeJump()
}

// openSlots counts the leading slots that already hold their own connection,
// which is how many the server has actually granted. Must be called with s.mu
// held.
func (s *session) openSlots() int {
	n := 0
	for _, c := range s.conns {
		if c == nil {
			break
		}
		n++
	}
	return max(n, 1)
}

// errPoolRefused marks a dial that was never attempted because the server
// already refused one. It never reaches the user: acquire answers it with the
// first connection, exactly as it answers a real refusal.
var errPoolRefused = errors.New("the server already refused an extra connection")

// dial opens one additional pooled connection. Callers reach it through
// dialPooled, which is what serializes the handshakes of a starting run; s.mu
// must not be held.
//
// Everything connect() logs is a property of the host and the configuration,
// so an extra connection can only repeat what the first one already said (the
// "connecting ..." line, the host key verdict). Outside debug mode those
// repeats are dropped; a dial that actually fails comes back as an error, not
// as a log line, and host key verification itself still runs per connection,
// inside connect().
func (s *session) dial() (*conn, error) {
	log := s.log
	if !s.cfg.Debug() {
		log = quietLogger{s.log}
	}
	sshClient, sftpClient, closeJump, err := connect(s.cfg, s.tune.requestConcurrency(), log)
	if err != nil {
		return nil, err
	}
	return &conn{ssh: sshClient, sftp: sftpClient, closeJump: closeJump}, nil
}

// quietLogger swallows the log output of a repeat dial; see session.dial.
type quietLogger struct{ Logger }

func (quietLogger) Infof(string, ...any)    {}
func (quietLogger) Warningf(string, ...any) {}

// reconnect redials c after a connection-class failure. gen is the generation
// of the caller's failed client: when another worker already reconnected it,
// the fresh client is returned without dialing again. Reconnects are bounded
// by the retries input (one budget for the whole pool); past that budget, or
// when the redial itself fails, an error is returned and the caller gives up.
//
// Workers that fail on the same connection in the meantime do not redial it
// again: the first one publishes c.redialing and the others wait on it, which
// is the collapse the mutex used to provide.
//
// The backoff and the handshake run with s.mu released. Holding it across them
// was a liveness bug: the stall watchdog's kill path needs the same mutex, and
// the case the watchdog exists for (an unhealthy server) is exactly the case
// where the redial it was waiting behind hangs. With advanced.timeout at 0,
// documented as the no-timeout escape hatch, connect() has no deadline of its
// own and the watchdog could be prevented from ever firing (issue #224).
//
// What is left is that a handshake already in flight is not interrupted; a
// kill that lands during one is honored when it returns, by closing the fresh
// connection instead of installing it.
func (s *session) reconnect(ctx context.Context, c *conn, gen int) (*sftp.Client, error) {
	for {
		s.mu.Lock()
		if c.gen != gen {
			// Another worker already redialed this connection.
			client := c.sftp
			s.mu.Unlock()
			return client, nil
		}
		if wait := c.redialing; wait != nil {
			s.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			// Round again: a redial that succeeded bumped the generation, one
			// that failed left it alone and this caller spends the next unit of
			// the budget, exactly as it did when both waited on the mutex.
			continue
		}
		if s.reconnects >= s.cfg.Retries {
			s.mu.Unlock()
			return nil, fmt.Errorf("connection lost and the reconnect budget is spent (%d, from the retries input)", s.cfg.Retries)
		}
		s.reconnects++
		attempt := s.reconnects
		wait := make(chan struct{})
		c.redialing = wait
		dead := *c
		s.mu.Unlock()

		metrics.Count("reconnects", 1)
		// The op spans the backoff too: from the run's point of view that wait
		// is exactly what a dropped connection costs.
		doneReconnect := metrics.Op("reconnect")
		backoff := retryBackoff(attempt)
		s.log.Warningf("connection to the server was lost; reconnecting in %s (reconnect %d/%d)", backoff, attempt, s.cfg.Retries)
		fresh, err := s.redial(ctx, backoff, &dead)
		doneReconnect(err)

		s.mu.Lock()
		c.redialing = nil
		if err != nil {
			close(wait)
			s.mu.Unlock()
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, fmt.Errorf("reconnecting: %w", err)
		}
		c.ssh, c.sftp, c.closeJump = fresh.ssh, fresh.sftp, fresh.closeJump
		c.gen++
		client := c.sftp
		close(wait)
		s.mu.Unlock()
		return client, nil
	}
}

// redial waits out the backoff, drops the dead transports and opens a fresh
// connection. It runs with s.mu released; the dial itself is serialized with
// every other handshake this session opens (see dialPooled), which the backoff
// deliberately is not: a wait nobody is served by should not stall another
// slot's first connection.
func (s *session) redial(ctx context.Context, backoff time.Duration, dead *conn) (*conn, error) {
	if err := sleepCtx(ctx, backoff); err != nil {
		return nil, err
	}
	closeConn(dead)
	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	sshClient, sftpClient, closeJump, err := connect(s.cfg, s.tune.requestConcurrency(), s.log)
	if err != nil {
		return nil, err
	}
	return &conn{ssh: sshClient, sftp: sftpClient, closeJump: closeJump}, nil
}

// eachConn calls fn once per opened connection, skipping slots that were never
// dialed and counting a connection shared by several slots (see acquire) once.
// Must be called with s.mu held.
func (s *session) eachConn(fn func(*conn)) {
	seen := make(map[*conn]bool, len(s.conns))
	for _, c := range s.conns {
		if c == nil || seen[c] {
			continue
		}
		seen[c] = true
		fn(c)
	}
}

// liveSSH returns the SSH client of every opened connection. Snapshotted under
// the lock because reconnect swaps those fields; callers (the keepalive loop
// and the stall watchdog) act on every connection the run has open.
func (s *session) liveSSH() []*ssh.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*ssh.Client
	s.eachConn(func(c *conn) { out = append(out, c.ssh) })
	return out
}

// closeSSH closes every live transport. The stall watchdog fires this: a
// server that stalls one transfer has stalled the run (all connections share
// its answer time), and closing the transports is what unblocks the SFTP
// operations stuck on it.
// It must stay callable at any moment, which is why nothing in this package
// holds s.mu across a network operation; see reconnect and dialPooled. What it
// deliberately does not do is latch the session shut: writeRecoveryManifest
// spends one reconnect after a stall to record partial progress (issue #115),
// so a connection opened after the kill is a documented case, not a leak.
func (s *session) closeSSH() {
	s.mu.Lock()
	var live []*ssh.Client
	s.eachConn(func(c *conn) { live = append(live, c.ssh) })
	s.mu.Unlock()
	for _, c := range live {
		c.Close()
	}
}

// do runs op against the first connection, redialing on a connection-class
// failure so the retried op runs against a fresh client instead of the dead one.
// Everything outside the per-file upload path goes through here, which keeps
// remote metadata phases and manifest writes on the first connection.
// Several callers may use do concurrently; acquire/reconnect uses the failed
// connection generation to collapse their simultaneous redials into one.
// Reconnects share the run-wide budget (the retries input) with the upload
// path; past that budget the original failure is returned. Non-connection
// errors are returned as-is, untouched.
//
// The op is marked active for the stall watchdog for its duration, so a
// server that hangs during a remote metadata phase or a manifest write trips
// stall-timeout just like a hung transfer. Ops must be idempotent: a
// retried op may have partially (or fully) taken effect on the server before
// the connection died.
func (s *session) do(ctx context.Context, watch *stallWatchdog, op func(*sftp.Client) error) error {
	for {
		client, c, gen := s.acquire(0)
		if watch != nil {
			watch.begin()
		}
		err := op(client)
		if watch != nil {
			watch.end()
		}
		if err == nil || !isConnError(err) {
			return err
		}
		// The watchdog closed the connection because the server stopped
		// making progress; redialing would just stall again, so fail fast
		// (mirrors uploadFileWithRetry).
		if watch != nil && watch.fired.Load() {
			return err
		}
		if _, rerr := s.reconnect(ctx, c, gen); rerr != nil {
			return fmt.Errorf("%w (%v)", err, rerr)
		}
	}
}

// close tears every connection of the session down at the end of the run.
func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eachConn(closeConn)
}

// isConnError reports whether err looks like the connection itself died (as
// opposed to a per-file SFTP failure), meaning a retry only helps against a
// fresh connection.
func isConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sftp.ErrSSHFxConnectionLost) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) {
		return true
	}
	var opErr *net.OpError // resets, broken pipes and friends
	if errors.As(err, &opErr) {
		return true
	}
	// The ssh transport reports some transport deaths as plain string errors.
	msg := err.Error()
	return strings.Contains(msg, "connection lost") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset")
}
