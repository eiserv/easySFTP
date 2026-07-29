package linkprobe

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// testServer is an in-process SSH server exposing an in-memory SFTP root, cut
// down from internal/uploader/testserver_test.go to what this package needs: no
// fault injection, but an optional exec channel, because the host-load probe
// falls back to running "uptime".
type testServer struct {
	Addr       string
	Host       string
	Port       int
	HostPubKey ssh.PublicKey

	handlers  sftp.Handlers
	sshConfig *ssh.ServerConfig
	listener  net.Listener

	// execOutput, when set, makes the server serve exec requests and answer
	// every command with this text. Unset means an SFTP-only account, which is
	// what a properly locked down benchmark server looks like.
	execOutput *string
}

type serverOption func(*testServer)

// withProcLoadavg makes the in-memory root serve /proc/loadavg, i.e. a server
// whose SFTP subsystem sees a real filesystem rather than a chroot.
func withProcLoadavg(contents string) serverOption {
	return func(s *testServer) {
		s.handlers.FileGet = &procLoadavg{inner: s.handlers.FileGet, contents: contents}
	}
}

// withExec makes the server answer exec requests, so the host-load probe's
// second choice can be exercised.
func withExec(output string) serverOption {
	return func(s *testServer) { s.execOutput = &output }
}

// procLoadavg serves one fixed path and delegates every other read to the
// in-memory handler. Not a FileCmder, so the PosixRename rule that applies to
// FileCmd wrappers does not apply here.
type procLoadavg struct {
	inner    sftp.FileReader
	contents string
}

func (p *procLoadavg) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	if r.Filepath == "/proc/loadavg" {
		return strings.NewReader(p.contents), nil
	}
	return p.inner.Fileread(r)
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

	sshConfig := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if conn.User() == testUser && string(password) == testPassword {
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
		Addr:       listener.Addr().String(),
		HostPubKey: hostSigner.PublicKey(),
		handlers:   sftp.InMemHandler(),
		sshConfig:  sshConfig,
		listener:   listener,
	}
	for _, opt := range opts {
		opt(srv)
	}
	tcpAddr := listener.Addr().(*net.TCPAddr)
	srv.Host = tcpAddr.IP.String()
	srv.Port = tcpAddr.Port

	go srv.acceptLoop()
	return srv
}

func (s *testServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
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
		go s.serveSession(channel, requests)
	}
}

func (s *testServer) serveSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	for req := range requests {
		switch {
		// Payload is an SSH string: four length bytes, then the name.
		case req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp":
			req.Reply(true, nil)
			server := sftp.NewRequestServer(channel, s.handlers)
			_ = server.Serve()
			server.Close()
			return
		case req.Type == "exec" && s.execOutput != nil:
			req.Reply(true, nil)
			_, _ = channel.Write([]byte(*s.execOutput))
			// Without an exit-status the client's Wait fails with
			// ExitMissingError, which would look like an unreachable exec
			// channel rather than a successful one.
			status := make([]byte, 4)
			binary.BigEndian.PutUint32(status, 0)
			_, _ = channel.SendRequest("exit-status", false, status)
			channel.Close()
			return
		default:
			req.Reply(false, nil)
		}
	}
}

// knownHosts renders the server's host key the way "ssh-keyscan" would, which
// is exactly the material the probe verifies against.
func (s *testServer) knownHosts(t *testing.T) string {
	t.Helper()
	return knownhosts.Line([]string{knownhosts.Normalize(s.Addr)}, s.HostPubKey)
}

// config is a probe config pointed at this server, with the measurements kept
// small so the tests stay fast.
func (s *testServer) config(t *testing.T) Config {
	t.Helper()
	return Config{
		Host:           s.Host,
		Port:           s.Port,
		User:           testUser,
		Password:       testPassword,
		KnownHosts:     s.knownHosts(t),
		RemotePath:     "/probe",
		RTTSamples:     5,
		ControlBytes:   64 << 10,
		ControlStreams: 2,
	}
}
