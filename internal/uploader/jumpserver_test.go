package uploader

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testJumpServer is an in-process SSH server that only serves direct-tcpip
// channel forwarding, the mechanism a ProxyJump tunnel uses. It hosts no SFTP
// subsystem at all, so a test passing through it proves the SFTP traffic
// really flowed through the tunnel.
type testJumpServer struct {
	Addr          string
	Host          string
	Port          int
	HostKeySHA256 string
	forwarded     int64 // direct-tcpip channels opened, for asserting the tunnel was used
}

func startTestJumpServer(t *testing.T) *testJumpServer {
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

	tcpAddr := listener.Addr().(*net.TCPAddr)
	srv := &testJumpServer{
		Addr:          listener.Addr().String(),
		Host:          tcpAddr.IP.String(),
		Port:          tcpAddr.Port,
		HostKeySHA256: ssh.FingerprintSHA256(hostSigner.PublicKey()),
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go srv.handleConn(conn, sshConfig)
		}
	}()
	return srv
}

func (s *testJumpServer) handleConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "direct-tcpip" {
			newChannel.Reject(ssh.UnknownChannelType, "only direct-tcpip is served")
			continue
		}
		// See RFC 4254, section 7.2 for the payload layout.
		var d struct {
			DestAddr string
			DestPort uint32
			SrcAddr  string
			SrcPort  uint32
		}
		if err := ssh.Unmarshal(newChannel.ExtraData(), &d); err != nil {
			newChannel.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
			continue
		}
		dst, err := net.Dial("tcp", net.JoinHostPort(d.DestAddr, fmt.Sprintf("%d", d.DestPort)))
		if err != nil {
			newChannel.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		channel, channelReqs, err := newChannel.Accept()
		if err != nil {
			dst.Close()
			continue
		}
		atomic.AddInt64(&s.forwarded, 1)
		go ssh.DiscardRequests(channelReqs)
		go func() {
			defer channel.Close()
			defer dst.Close()
			_, _ = io.Copy(channel, dst)
		}()
		go func() {
			defer channel.Close()
			defer dst.Close()
			_, _ = io.Copy(dst, channel)
		}()
	}
}
