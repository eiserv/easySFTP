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

	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/metrics"
)

// conn is one live SSH/SFTP client pair.
type conn struct {
	ssh       *ssh.Client
	sftp      *sftp.Client
	closeJump func() // closes the jump-host transport; no-op for direct connections
	gen       int    // bumped on every successful reconnect of this connection
}

// session holds the run's connections and can transparently redial one when it
// drops mid-run, so per-file retries run against a live client instead of
// burning their backoff on a dead one.
//
// A run opens advanced.connections of them (1 by default). Everything above
// that number is one connection's ceiling: one TCP congestion window and one
// cipher stream carry the whole run, which neither concurrency nor
// request_concurrency can lift (issue #158). Parallel uploads spread over the
// pool. Remote scans and delete sweeps may issue concurrent requests, but keep
// them on the first connection; manifest writes and directory setup do too.
// Their reconnect behavior therefore stays independent of the upload pool.
type session struct {
	cfg *config.Config
	log Logger

	mu    sync.Mutex
	conns []*conn // slot 0 is dialed up front, the rest on first use
	// reconnects spent so far, bounded by cfg.Retries and shared by every
	// connection: the input is a run-wide budget, and a server that drops
	// connections drops them faster with more of them open.
	reconnects int
}

// newSession dials the server and opens the initial SFTP session, retrying
// transient failures with the same exponential backoff and budget (the retries
// input) as per-file uploads: a momentary DNS hiccup or a restarting sshd
// should cost a short wait, not a red pipeline. Permanent failures, most
// importantly a host key mismatch (a security signal) and an authentication
// failure (retrying risks fail2ban-style lockouts), fail immediately. With a
// jump host configured, this covers either hop: a transient failure reaching
// the bastion is retried exactly like one reaching the target.
func newSession(ctx context.Context, cfg *config.Config, log Logger) (*session, error) {
	var lastErr error
	for attempt := 0; attempt <= cfg.Retries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			log.Warningf("could not connect; retrying in %s (attempt %d/%d): %v", backoff, attempt+1, cfg.Retries+1, lastErr)
			if err := sleepCtx(ctx, backoff); err != nil {
				return nil, err
			}
		}
		done := metrics.Op("ssh_connect")
		sshClient, sftpClient, closeJump, err := connect(cfg, log)
		done(err)
		if err == nil {
			metrics.Count("connections_opened", 1)
			metrics.Count("connections_used", 1) // slot 0, dialed up front
			s := &session{cfg: cfg, log: log, conns: make([]*conn, poolSize(cfg, log))}
			s.conns[0] = &conn{ssh: sshClient, sftp: sftpClient, closeJump: closeJump}
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

// poolSize is how many connections the run may open. It never exceeds the
// number of files uploaded in parallel: a connection no worker ever picks is a
// handshake for nothing.
func poolSize(cfg *config.Config, log Logger) int {
	n := cfg.Connections
	if n < 1 {
		n = 1 // a directly constructed config (tests) leaves this at zero
	}
	if cfg.Concurrency > 0 && n > cfg.Concurrency {
		log.Infof("advanced.connections is %d but only %d file(s) upload in parallel; opening at most %d connection(s)",
			n, cfg.Concurrency, cfg.Concurrency)
		n = cfg.Concurrency
	}
	if n > 1 {
		log.Infof("uploads may use up to %d connections", n)
	}
	return n
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
// Files are spread over the pool round-robin. Connections past the first are
// dialed on first use, so a run that uploads three files never pays for four
// handshakes, and a server that refuses one (sshd's MaxSessions/MaxStartups,
// a per-account connection limit on shared hosting) costs a warning rather
// than the run: that slot falls back to the first connection.
func (s *session) acquire(index int) (*sftp.Client, *conn, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := index % len(s.conns)
	if s.conns[slot] == nil {
		c, err := s.dial()
		if err != nil {
			metrics.Count("connections_refused", 1)
			s.log.Warningf("could not open connection %d of %d (%v); this run continues on its first connection",
				slot+1, len(s.conns), err)
			c = s.conns[0]
		} else {
			metrics.Count("connections_opened", 1)
		}
		// Slots the run actually reached, however they resolved: a pool whose
		// tail is never touched (fewer files than connections) is the other
		// way a configured connection can end up unused.
		metrics.Count("connections_used", 1)
		s.conns[slot] = c
	}
	c := s.conns[slot]
	return c.sftp, c, c.gen
}

// dial opens one additional pooled connection. Must be called with s.mu held,
// which also serializes the handshakes of a starting run: the alternative is
// several workers dialing the same slot at once.
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
	sshClient, sftpClient, closeJump, err := connect(s.cfg, log)
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
// The lock is held across the backoff and redial on purpose: workers that
// fail in the meantime block in acquire()/reconnect() until the fresh client
// is up, instead of hammering the dead one.
func (s *session) reconnect(ctx context.Context, c *conn, gen int) (*sftp.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.gen != gen {
		return c.sftp, nil
	}
	if s.reconnects >= s.cfg.Retries {
		return nil, fmt.Errorf("connection lost and the reconnect budget is spent (%d, from the retries input)", s.cfg.Retries)
	}
	s.reconnects++
	metrics.Count("reconnects", 1)
	// The op spans the backoff too: from the run's point of view that wait is
	// exactly what a dropped connection costs.
	doneReconnect := metrics.Op("reconnect")
	backoff := time.Duration(1<<(s.reconnects-1)) * time.Second
	s.log.Warningf("connection to the server was lost; reconnecting in %s (reconnect %d/%d)", backoff, s.reconnects, s.cfg.Retries)
	if err := sleepCtx(ctx, backoff); err != nil {
		doneReconnect(err)
		return nil, err
	}
	c.sftp.Close()
	c.ssh.Close()
	c.closeJump()
	sshClient, sftpClient, closeJump, err := connect(s.cfg, s.log)
	doneReconnect(err)
	if err != nil {
		return nil, fmt.Errorf("reconnecting: %w", err)
	}
	c.ssh, c.sftp, c.closeJump = sshClient, sftpClient, closeJump
	c.gen++
	return c.sftp, nil
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
func (s *session) closeSSH() {
	for _, c := range s.liveSSH() {
		c.Close()
	}
}

// do runs op against the first connection, redialing on a connection-class
// failure so the retried op runs against a fresh client instead of the dead one.
// Everything outside the per-file upload path goes through here, which keeps
// remote scans, delete sweeps and manifest writes on the first connection.
// Several callers may use do concurrently; acquire/reconnect uses the failed
// connection generation to collapse their simultaneous redials into one.
// Reconnects share the run-wide budget (the retries input) with the upload
// path; past that budget the original failure is returned. Non-connection
// errors are returned as-is, untouched.
//
// The op is marked active for the stall watchdog for its duration, so a
// server that hangs during a remote scan, a delete sweep or a manifest write
// trips stall-timeout just like a hung transfer. Ops must be idempotent: a
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
	s.eachConn(func(c *conn) {
		c.sftp.Close()
		c.ssh.Close()
		c.closeJump()
	})
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
