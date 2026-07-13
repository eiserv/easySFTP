package uploader

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"sync/atomic"
	"testing"

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
	ClientKeyPEM  string
	handlers      sftp.Handlers
	sshConfig     *ssh.ServerConfig
	listener      net.Listener

	// Fault injection (set via options before the accept loop starts).
	failRename bool  // make every rename fail with a non-transient error
	dropAfter  int64 // if >0, kill each connection after it reads this many bytes
}

// serverOption tweaks a testServer before it starts serving.
type serverOption func(*testServer)

// withFailRename makes the server reject every (Posix)Rename, simulating a
// server that lets the temp upload through but cannot swap it into place.
func withFailRename() serverOption { return func(s *testServer) { s.failRename = true } }

// withDropAfter closes each connection once it has read n bytes from the
// client, simulating a mid-transfer connection drop.
func withDropAfter(n int64) serverOption { return func(s *testServer) { s.dropAfter = n } }

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

func (s *testServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		if s.dropAfter > 0 {
			conn = &dropConn{Conn: conn, limit: s.dropAfter}
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
	go ssh.DiscardRequests(reqs)

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
					server.Serve()
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
