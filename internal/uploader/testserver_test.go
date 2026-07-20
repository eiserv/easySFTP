package uploader

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// testServer is an in-process SSH server exposing an in-memory SFTP root,
// shared across all connections so tests can verify uploads with a second
// client session.
type testServer struct {
	Addr          string
	Host          string
	Port          int
	HostKeySHA256 string
	HostPubKey    ssh.PublicKey
	ClientKeyPEM  string
	handlers      sftp.Handlers
	sshConfig     *ssh.ServerConfig
	listener      net.Listener

	// Fault injection (set via options before the accept loop starts).
	failRename    bool  // make every rename fail with a non-transient error
	failSetstat   bool  // make every chmod (Setstat) fail
	dropAfter     int64 // if >0, kill each connection after it reads this many bytes
	dropFirstOnly bool  // with dropAfter: apply the drop to the first accepted connection only
	dropped       int32 // whether the first connection was already wrapped
	stallAfter    int64 // if >0, stop reading (but stay connected) after this many bytes
	refuseFirst   int32 // if >0, close this many first accepted connections immediately
	accepted      int32 // total connections accepted, for asserting attempt counts

	keepalives *int64 // if set, counts "keepalive@openssh.com" global requests received
}

// serverOption tweaks a testServer before it starts serving.
type serverOption func(*testServer)

// withFailRename makes the server reject every (Posix)Rename, simulating a
// server that lets the temp upload through but cannot swap it into place.
func withFailRename() serverOption { return func(s *testServer) { s.failRename = true } }

// withDropAfter closes each connection once it has read n bytes from the
// client, simulating a mid-transfer connection drop.
func withDropAfter(n int64) serverOption { return func(s *testServer) { s.dropAfter = n } }

// withDropFirstConnAfter kills only the first accepted connection after n
// bytes; later connections (reconnects) run clean. Simulates a one-off
// mid-run drop (NAT reset, server restart) that a redial recovers from.
func withDropFirstConnAfter(n int64) serverOption {
	return func(s *testServer) {
		s.dropAfter = n
		s.dropFirstOnly = true
	}
}

// withStallAfter makes each connection stop reading (while staying open) once
// it has read n bytes from the client, simulating a server that is alive but
// makes no progress: the SSH transport stays up, but transfers starve.
func withStallAfter(n int64) serverOption { return func(s *testServer) { s.stallAfter = n } }

// withFailSetstat makes the server reject every chmod (Setstat) request,
// simulating a server that refuses SETSTAT.
func withFailSetstat() serverOption { return func(s *testServer) { s.failSetstat = true } }

// withRefuseFirstConns closes the first n accepted connections before any SSH
// handshake, simulating a transient outage (restarting sshd, network blip)
// that clears after a moment.
func withRefuseFirstConns(n int32) serverOption { return func(s *testServer) { s.refuseFirst = n } }

// withKeepaliveCounter makes the server tally every "keepalive@openssh.com"
// global request it receives into c, instead of just discarding it.
func withKeepaliveCounter(c *int64) serverOption { return func(s *testServer) { s.keepalives = c } }

// setstatCall records one chmod request the server received.
type setstatCall struct {
	path string
	mode uint32
}

// setstatRecorder delegates to the real in-memory handlers while recording
// every Setstat request that carries permission bits, so tests can assert
// which remote paths were chmod'd and to what mode.
type setstatRecorder struct {
	inner sftp.FileCmder
	mu    sync.Mutex
	calls []setstatCall
}

func (r *setstatRecorder) Filecmd(req *sftp.Request) error {
	if req.Method == "Setstat" && req.AttrFlags().Permissions {
		r.mu.Lock()
		r.calls = append(r.calls, setstatCall{path: req.Filepath, mode: req.Attributes().Mode})
		r.mu.Unlock()
	}
	return r.inner.Filecmd(req)
}

func (r *setstatRecorder) PosixRename(req *sftp.Request) error {
	return posixRenamePassthrough(r.inner, req)
}

// posixRenamePassthrough forwards posix-rename to the wrapped handler. Every
// FileCmder wrapper in this file needs a PosixRename method calling this:
// pkg/sftp only serves posix-rename when the outermost FileCmder implements
// PosixRenameFileCmder, and otherwise silently downgrades it to plain
// "Rename", which unlike posix-rename fails when the target exists. Without
// the passthrough, wrapping the server makes every overwriting rename (e.g. a
// manifest rewrite) fail with "file already exists".
func posixRenamePassthrough(inner sftp.FileCmder, req *sftp.Request) error {
	if pr, ok := inner.(sftp.PosixRenameFileCmder); ok {
		return pr.PosixRename(req)
	}
	return inner.Filecmd(req)
}

// chtimesCall records one modification-time (Setstat with Acmodtime) request
// the server received.
type chtimesCall struct {
	path  string
	mtime int64
}

// chtimesRecorder delegates to the real in-memory handlers while recording
// every Setstat request that carries modification times, so tests can assert
// which remote paths got which mtime requested. The in-memory server ignores
// the times themselves (see CLAUDE.md), so recording the request is the only
// way to assert preserve-times behavior.
type chtimesRecorder struct {
	inner sftp.FileCmder
	mu    sync.Mutex
	calls []chtimesCall
}

func (r *chtimesRecorder) Filecmd(req *sftp.Request) error {
	if req.Method == "Setstat" && req.AttrFlags().Acmodtime {
		r.mu.Lock()
		r.calls = append(r.calls, chtimesCall{path: req.Filepath, mtime: int64(req.Attributes().Mtime)})
		r.mu.Unlock()
	}
	return r.inner.Filecmd(req)
}

func (r *chtimesRecorder) PosixRename(req *sftp.Request) error {
	return posixRenamePassthrough(r.inner, req)
}

// withChtimesRecorder records modification-time requests. Like
// withSetstatRecorder, give it before any fault-injecting option.
func withChtimesRecorder(rec *chtimesRecorder) serverOption {
	return func(s *testServer) {
		rec.inner = s.handlers.FileCmd
		s.handlers.FileCmd = rec
	}
}

// withSetstatRecorder records chmod requests. It must be given before any
// fault-injecting option so the recorder observes the request regardless of
// whether a later wrapper then fails it.
func withSetstatRecorder(rec *setstatRecorder) serverOption {
	return func(s *testServer) {
		rec.inner = s.handlers.FileCmd
		s.handlers.FileCmd = rec
	}
}

// withOpCounter wraps the in-memory handlers so the given counter tallies the
// SFTP methods (Mkdir, Stat) a run issues, letting tests assert how many
// directory round-trips it makes.
func withOpCounter(c *opCounter) serverOption {
	return func(s *testServer) {
		c.cmd = s.handlers.FileCmd
		c.list = s.handlers.FileList
		s.handlers.FileCmd = c
		s.handlers.FileList = c
	}
}

// opCounter delegates to the real in-memory handlers while counting the
// directory-related methods that flow through them.
type opCounter struct {
	cmd   sftp.FileCmder
	list  sftp.FileLister
	mkdir int64
	stat  int64
}

func (c *opCounter) Filecmd(r *sftp.Request) error {
	if r.Method == "Mkdir" {
		atomic.AddInt64(&c.mkdir, 1)
	}
	return c.cmd.Filecmd(r)
}

func (c *opCounter) PosixRename(r *sftp.Request) error {
	return posixRenamePassthrough(c.cmd, r)
}

func (c *opCounter) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	if r.Method == "Stat" {
		atomic.AddInt64(&c.stat, 1)
	}
	return c.list.Filelist(r)
}

const (
	testUser     = "testuser"
	testPassword = "testpass"
)

func startTestServer(t *testing.T, opts ...serverOption) *testServer {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}

	clientPub, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientPEM, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	clientSSHPub, err := ssh.NewPublicKey(clientPub)
	if err != nil {
		t.Fatal(err)
	}
	authorizedKey := string(clientSSHPub.Marshal())

	sshConfig := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if conn.User() == testUser && string(password) == testPassword {
				return nil, nil
			}
			return nil, errors.New("access denied")
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if conn.User() == testUser && string(key.Marshal()) == authorizedKey {
				return nil, nil
			}
			return nil, errors.New("access denied")
		},
	}
	sshConfig.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	srv := &testServer{
		Addr:          listener.Addr().String(),
		HostKeySHA256: ssh.FingerprintSHA256(hostSigner.PublicKey()),
		HostPubKey:    hostSigner.PublicKey(),
		ClientKeyPEM:  string(pem.EncodeToMemory(clientPEM)),
		handlers:      sftp.InMemHandler(),
		sshConfig:     sshConfig,
		listener:      listener,
	}
	for _, opt := range opts {
		opt(srv)
	}
	if srv.failRename {
		srv.handlers.FileCmd = &faultyRename{inner: srv.handlers.FileCmd}
	}
	if srv.failSetstat {
		srv.handlers.FileCmd = &faultySetstat{inner: srv.handlers.FileCmd}
	}
	tcpAddr := listener.Addr().(*net.TCPAddr)
	srv.Host = tcpAddr.IP.String()
	srv.Port = tcpAddr.Port

	go srv.acceptLoop()
	return srv
}

// faultyRename wraps a FileCmder and fails every rename, delegating everything
// else to the real in-memory handler.
type faultyRename struct{ inner sftp.FileCmder }

func (f *faultyRename) Filecmd(r *sftp.Request) error {
	if r.Method == "Rename" || r.Method == "PosixRename" {
		return errors.New("injected rename failure")
	}
	return f.inner.Filecmd(r)
}

func (f *faultyRename) PosixRename(r *sftp.Request) error {
	return errors.New("injected rename failure")
}

// faultySetstat wraps a FileCmder and fails every chmod (Setstat) request,
// simulating a server that rejects SETSTAT, delegating everything else to the
// real in-memory handler.
type faultySetstat struct{ inner sftp.FileCmder }

func (f *faultySetstat) Filecmd(r *sftp.Request) error {
	if r.Method == "Setstat" {
		return errors.New("injected setstat failure")
	}
	return f.inner.Filecmd(r)
}

func (f *faultySetstat) PosixRename(r *sftp.Request) error {
	return posixRenamePassthrough(f.inner, r)
}

// faultyPathCmd fails one SFTP method on one exact path while enabled,
// delegating everything else to the real in-memory handler. Tests toggle
// enabled between runs to simulate a fault that later clears.
type faultyPathCmd struct {
	inner   sftp.FileCmder
	method  string // e.g. "PosixRename", "Remove"
	path    string // the request Target for renames, the Filepath otherwise
	enabled atomic.Bool
}

// withFaultyPath installs f as the FileCmd handler (wrapping the real one).
func withFaultyPath(f *faultyPathCmd) serverOption {
	return func(s *testServer) {
		f.inner = s.handlers.FileCmd
		s.handlers.FileCmd = f
	}
}

func (f *faultyPathCmd) match(method, path string) bool {
	return f.enabled.Load() && method == f.method && path == f.path
}

func (f *faultyPathCmd) Filecmd(r *sftp.Request) error {
	p := r.Filepath
	if r.Method == "Rename" || r.Method == "PosixRename" {
		p = r.Target
	}
	if f.match(r.Method, p) {
		return fmt.Errorf("injected %s failure on %s", f.method, f.path)
	}
	return f.inner.Filecmd(r)
}

// PosixRename must be forwarded explicitly: pkg/sftp only serves posix-rename
// when the outermost FileCmder implements PosixRenameFileCmder, and otherwise
// downgrades it to plain Rename, which fails whenever the target exists.
func (f *faultyPathCmd) PosixRename(r *sftp.Request) error {
	if f.match("PosixRename", r.Target) {
		return fmt.Errorf("injected %s failure on %s", f.method, f.path)
	}
	return posixRenamePassthrough(f.inner, r)
}

// dropConn closes the connection once it has read limit bytes, simulating a
// network drop partway through a transfer.
type dropConn struct {
	net.Conn
	limit int64
	read  int64
}

func (c *dropConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if atomic.AddInt64(&c.read, int64(n)) > c.limit {
		c.Conn.Close()
	}
	return n, err
}

// stallConn stops reading (but keeps the connection open) once it has read
// limit bytes, simulating a server that is alive but stops making progress.
// A blocked read is released when the connection closes, or after a generous
// safety timeout so an abandoned server goroutine cannot outlive the test
// binary for long.
type stallConn struct {
	net.Conn
	limit int64
	read  int64
	stall chan struct{}
	once  sync.Once
}

func (c *stallConn) Read(p []byte) (int, error) {
	if atomic.LoadInt64(&c.read) > c.limit {
		select {
		case <-c.stall:
		case <-time.After(30 * time.Second):
		}
		return 0, net.ErrClosed
	}
	n, err := c.Conn.Read(p)
	atomic.AddInt64(&c.read, int64(n))
	return n, err
}

func (c *stallConn) Close() error {
	c.once.Do(func() { close(c.stall) })
	return c.Conn.Close()
}

func (s *testServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		if atomic.AddInt32(&s.accepted, 1) <= s.refuseFirst {
			conn.Close()
			continue
		}
		if s.dropAfter > 0 &&
			(!s.dropFirstOnly || atomic.CompareAndSwapInt32(&s.dropped, 0, 1)) {
			conn = &dropConn{Conn: conn, limit: s.dropAfter}
		}
		if s.stallAfter > 0 {
			conn = &stallConn{Conn: conn, limit: s.stallAfter, stall: make(chan struct{})}
		}
		go s.handleConn(conn)
	}
}

func (s *testServer) handleConn(conn net.Conn) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, s.sshConfig)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go func() {
		for req := range reqs {
			if s.keepalives != nil && req.Type == "keepalive@openssh.com" {
				atomic.AddInt64(s.keepalives, 1)
			}
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}()

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range requests {
				// Only the "sftp" subsystem is served; payload is an SSH
				// string: 4 length bytes followed by the subsystem name.
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				req.Reply(ok, nil)
				if ok {
					server := sftp.NewRequestServer(channel, s.handlers)
					serr := server.Serve()
					fmt.Printf("DEBUG server.Serve on %v returned: %v\n", conn.RemoteAddr(), serr)
					server.Close()
					return
				}
			}
		}()
	}
}

// verifyClient opens an independent SFTP session for inspecting server state.
func (s *testServer) verifyClient(t *testing.T) *sftp.Client {
	t.Helper()
	sshClient, err := ssh.Dial("tcp", s.Addr, &ssh.ClientConfig{
		User:            testUser,
		Auth:            []ssh.AuthMethod{ssh.Password(testPassword)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sshClient.Close() })
	client, err := sftp.NewClient(sshClient)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// testLogger routes uploader logs into the test log.
type testLogger struct{ t *testing.T }

func (l testLogger) Infof(format string, args ...any)    { l.t.Logf("INFO  "+format, args...) }
func (l testLogger) Warningf(format string, args ...any) { l.t.Logf("WARN  "+format, args...) }
func (l testLogger) Group(name string)                   { l.t.Logf("GROUP %s", name) }
func (l testLogger) EndGroup()                           {}

// recordingLogger wraps testLogger and additionally records every info and
// warning message, so tests can assert what a run logged. Safe for concurrent
// use since uploads happen in parallel workers.
type recordingLogger struct {
	testLogger
	mu       sync.Mutex
	infos    []string
	warnings []string
}

func (l *recordingLogger) Infof(format string, args ...any) {
	l.testLogger.Infof(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, fmt.Sprintf(format, args...))
}

func (l *recordingLogger) Warningf(format string, args ...any) {
	l.testLogger.Warningf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}
