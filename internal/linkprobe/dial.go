package linkprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// conn is one SSH connection with its SFTP session on top. The SSH client is
// kept because the host-load probe may need an exec channel, which an SFTP
// client cannot open.
type conn struct {
	ssh  *ssh.Client
	sftp *sftp.Client
}

func (c *conn) Close() error {
	if c == nil {
		return nil
	}
	// The SFTP session first: closing the SSH client under it would make the
	// close look like a transport error in the server's log.
	var err error
	if c.sftp != nil {
		err = c.sftp.Close()
	}
	if c.ssh != nil {
		if cerr := c.ssh.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// dial opens one connection with the SFTP client tuned as high as pkg/sftp
// allows. That is on purpose: the control measurement is meant to be an upper
// bound for the path, so anything easySFTP could conceivably reach must not be
// held back by a conservative client setting here.
func dial(ctx context.Context, cfg Config) (*conn, error) {
	cb, err := hostKeyCallback(cfg.KnownHosts)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	// DialContext rather than ssh.Dial: the probe's whole point is to run
	// against a path that may be slow or shaped, and a probe that cannot be
	// cancelled would outlive the benchmark that started it.
	var d net.Dialer
	tcp, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dialing the server: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		// Covers the handshake only; cleared again below, since a deadline left
		// on the connection would abort the measurements themselves.
		_ = tcp.SetDeadline(deadline)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(tcp, addr, &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.Password(cfg.Password)},
		HostKeyCallback: cb,
		Timeout:         30 * time.Second,
	})
	if err != nil {
		tcp.Close()
		return nil, fmt.Errorf("SSH handshake: %w", err)
	}
	_ = tcp.SetDeadline(time.Time{})
	client := ssh.NewClient(sshConn, chans, reqs)

	sftpClient, err := sftp.NewClient(client,
		sftp.MaxPacket(32768),
		sftp.UseConcurrentWrites(true),
		sftp.MaxConcurrentRequestsPerFile(64),
	)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("opening the SFTP subsystem: %w", err)
	}
	return &conn{ssh: client, sftp: sftpClient}, nil
}

// hostKeyCallback verifies the server against raw known_hosts lines, the same
// material a benchmark run passes to easySFTP.
//
// There is deliberately no opt-out here. easySFTP itself has one
// (allow-any-host-key) because a user's server may be unpinnable; the benchmark
// always has the keyscan output, so an unverified probe would only be a
// security hole nobody needs. x/crypto's parser reads files only, so the lines
// are staged in a temp file that is removed again right after parsing, the same
// way internal/uploader does it.
func hostKeyCallback(data string) (ssh.HostKeyCallback, error) {
	if data == "" {
		return nil, errors.New("no known-hosts material configured; the probe verifies the server like a real run does")
	}
	f, err := os.CreateTemp("", "linkprobe-known-hosts-*")
	if err != nil {
		return nil, fmt.Errorf("staging known-hosts: %w", err)
	}
	defer os.Remove(f.Name())
	_, werr := f.WriteString(data + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, fmt.Errorf("staging known-hosts: %w", werr)
	}
	cb, err := knownhosts.New(f.Name())
	if err != nil {
		return nil, fmt.Errorf("parsing known-hosts: %w", err)
	}
	return cb, nil
}
