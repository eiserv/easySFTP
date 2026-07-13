package uploader

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
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
}

const (
	testUser     = "testuser"
	testPassword = "testpass"
)

func startTestServer(t *testing.T) *testServer {
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
