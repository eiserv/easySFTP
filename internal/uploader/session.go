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
)

// session holds the run's SSH/SFTP client pair and can transparently redial
// it when the connection drops mid-run, so per-file retries run against a
// live client instead of burning their backoff on a dead one.
type session struct {
	cfg *config.Config
	log Logger

	mu         sync.Mutex
	sshClient  *ssh.Client
	sftpClient *sftp.Client
	gen        int // bumped on every successful reconnect
	reconnects int // spent so far; bounded by cfg.Retries
}

// newSession dials the server and opens the initial SFTP session, retrying
// transient failures with the same exponential backoff and budget (the retries
// input) as per-file uploads: a momentary DNS hiccup or a restarting sshd
// should cost a short wait, not a red pipeline. Permanent failures, most
// importantly a host key mismatch (a security signal) and an authentication
// failure (retrying risks fail2ban-style lockouts), fail immediately.
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
		sshClient, sftpClient, err := connect(cfg, log)
		if err == nil {
			return &session{cfg: cfg, log: log, sshClient: sshClient, sftpClient: sftpClient}, nil
		}
		lastErr = err
		if !isRetryableConnect(err) {
			break
		}
	}
	return nil, lastErr
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
func isRetryableConnect(err error) bool {
	var pe permanentError
	if errors.As(err, &pe) {
		return false
	}
	return !strings.Contains(err.Error(), "ssh: unable to authenticate")
}

// current returns the live SFTP client and its generation. The generation is
// handed back to reconnect so that concurrent workers failing on the same
// dead connection trigger only one redial between them.
func (s *session) current() (*sftp.Client, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sftpClient, s.gen
}

// currentSSH returns the live SSH client (used by the keepalive loop, which
// must follow reconnects to the fresh transport).
func (s *session) currentSSH() *ssh.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sshClient
}

// reconnect redials after a connection-class failure. gen is the generation
// of the caller's failed client: when another worker already reconnected, the
// fresh client is returned without dialing again. Reconnects are bounded by
// the retries input; past that budget, or when the redial itself fails, an
// error is returned and the caller gives up.
//
// The lock is held across the backoff and redial on purpose: workers that
// fail in the meantime block in current()/reconnect() until the fresh client
// is up, instead of hammering the dead one.
func (s *session) reconnect(ctx context.Context, gen int) (*sftp.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen {
		return s.sftpClient, nil
	}
	if s.reconnects >= s.cfg.Retries {
		return nil, fmt.Errorf("connection lost and the reconnect budget is spent (%d, from the retries input)", s.cfg.Retries)
	}
	s.reconnects++
	backoff := time.Duration(1<<(s.reconnects-1)) * time.Second
	s.log.Warningf("connection to the server was lost; reconnecting in %s (reconnect %d/%d)", backoff, s.reconnects, s.cfg.Retries)
	if err := sleepCtx(ctx, backoff); err != nil {
		return nil, err
	}
	s.sftpClient.Close()
	s.sshClient.Close()
	sshClient, sftpClient, err := connect(s.cfg, s.log)
	if err != nil {
		return nil, fmt.Errorf("reconnecting: %w", err)
	}
	s.sshClient, s.sftpClient = sshClient, sftpClient
	s.gen++
	return s.sftpClient, nil
}

// close tears the session down at the end of the run.
func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sftpClient.Close()
	s.sshClient.Close()
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
