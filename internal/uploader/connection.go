package uploader

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/eiserv/easySFTP/internal/config"
)

// keepaliveInterval is how often sendKeepalives pings the connection. It is
// deliberately not configurable: it's cheap, harmless to send more often than
// strictly needed, and there's no evidence yet that any user needs a
// different value.
const keepaliveInterval = 30 * time.Second

// sendKeepalives periodically sends an SSH keepalive request until ctx is
// canceled. This keeps long or idle-looking transfers alive across NAT
// gateways and firewalls that drop idle TCP connections, and answers sshd's
// own ClientAliveInterval probes so the server doesn't disconnect us first.
// client is a getter (not a fixed *ssh.Client) so one loop follows the
// session across reconnects; interval is a parameter so tests can drive it
// with a short tick instead of waiting 30s.
func sendKeepalives(ctx context.Context, client func() *ssh.Client, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _, _ = client().SendRequest("keepalive@openssh.com", true, nil)
		}
	}
}

// hopNames holds the user-facing option names of one hop's host key knobs,
// so warnings and errors point at what the user actually writes: inline
// input names for the primary hop in inline mode, config-file paths in
// config mode, and connection.proxy.* paths for the jump host (which only
// exists in config mode).
type hopNames struct {
	hostKey    string
	knownHosts string
	allowAny   string
	privateKey string
}

// hop carries the connection parameters of one SSH hop: the primary server,
// or the optional jump host in front of it.
type hop struct {
	addr            string
	user            string
	password        string
	privateKey      string
	passphrase      string
	fingerprints    []string
	knownHosts      string
	allowAnyHostKey bool
	names           hopNames
}

// clientConfig builds the ssh.ClientConfig for this hop, including its own
// host key verification.
func (h hop) clientConfig(timeout time.Duration, log Logger) (*ssh.ClientConfig, error) {
	// Auth and host key setup errors are local configuration problems, never
	// fixed by dialing again; tag them so the connect retry loop fails fast.
	auth, err := authMethods(h)
	if err != nil {
		return nil, permanentError{err}
	}
	cb, err := hostKeyCallback(h, log)
	if err != nil {
		return nil, permanentError{err}
	}
	return &ssh.ClientConfig{
		User:            h.user,
		Auth:            auth,
		HostKeyCallback: cb,
		Timeout:         timeout,
	}, nil
}

// targetHop maps the primary connection settings onto a hop.
func targetHop(cfg *config.Config) hop {
	names := hopNames{hostKey: "host-key", knownHosts: "known-hosts", allowAny: "allow-any-host-key", privateKey: "private-key"}
	if cfg.ConfigPath != "" {
		names = hopNames{hostKey: "connection.host_key", knownHosts: "connection.known_hosts",
			allowAny: "connection.allow_any_host_key", privateKey: "private-key"}
	}
	return hop{
		addr:            net.JoinHostPort(cfg.Server, fmt.Sprintf("%d", cfg.Port)),
		user:            cfg.Username,
		password:        cfg.Password,
		privateKey:      cfg.PrivateKey,
		passphrase:      cfg.Passphrase,
		fingerprints:    cfg.HostKeyFingerprints,
		knownHosts:      cfg.KnownHosts,
		allowAnyHostKey: cfg.AllowAnyHostKey,
		names:           names,
	}
}

// jumpHop maps the connection.proxy settings onto a hop.
func jumpHop(p *config.Proxy) hop {
	return hop{
		addr:            net.JoinHostPort(p.Server, fmt.Sprintf("%d", p.Port)),
		user:            p.Username,
		password:        p.Password,
		privateKey:      p.PrivateKey,
		passphrase:      p.Passphrase,
		fingerprints:    p.HostKeyFingerprints,
		knownHosts:      p.KnownHosts,
		allowAnyHostKey: p.AllowAnyHostKey,
		names: hopNames{hostKey: "connection.proxy.host_key", knownHosts: "connection.proxy.known_hosts",
			allowAny: "connection.proxy.allow_any_host_key", privateKey: "proxy-private-key"},
	}
}

// logHostKeyStatus reports one hop's successful verification. The unverified
// case already warned in hostKeyCallback; this line is the positive signal
// the compact default log promises ("Host key verified").
func logHostKeyStatus(h hop, log Logger) {
	if len(h.fingerprints) > 0 || h.knownHosts != "" {
		log.Infof("host key of %s verified against the pinned key(s)", h.addr)
	}
}

// connect dials the server, optionally through the configured jump host, and
// opens an SFTP session on top of SSH. The returned cleanup function closes
// the jump-host transport (it is a no-op for direct connections) and must be
// called after the SSH client is closed.
func connect(cfg *config.Config, log Logger) (*ssh.Client, *sftp.Client, func(), error) {
	noop := func() {}
	target := targetHop(cfg)
	targetConfig, err := target.clientConfig(cfg.Timeout, log)
	if err != nil {
		return nil, nil, noop, err
	}

	var sshClient *ssh.Client
	cleanup := noop
	if cfg.Proxy == nil {
		log.Infof("connecting to %s as %s ...", target.addr, target.user)
		sshClient, err = ssh.Dial("tcp", target.addr, targetConfig)
		if err != nil {
			return nil, nil, noop, fmt.Errorf("connecting to %s: %w", target.addr, err)
		}
		logHostKeyStatus(target, log)
	} else {
		sshClient, cleanup, err = dialViaJump(cfg, target.addr, targetConfig, log)
		if err != nil {
			return nil, nil, noop, err
		}
	}

	sftpClient, err := sftp.NewClient(sshClient,
		sftp.UseConcurrentWrites(true),
		sftp.MaxConcurrentRequestsPerFile(cfg.SftpRequestConcurrency),
	)
	if err != nil {
		sshClient.Close()
		cleanup()
		return nil, nil, noop, fmt.Errorf("opening SFTP session: %w", err)
	}
	return sshClient, sftpClient, cleanup, nil
}

// dialViaJump connects to the jump host with its own auth and host key
// verification, opens a TCP tunnel to the target through it, and runs the
// target's SSH handshake (with the target's own host key verification) over
// that tunnel. The returned cleanup closes the jump transport.
func dialViaJump(cfg *config.Config, targetAddr string, targetConfig *ssh.ClientConfig, log Logger) (*ssh.Client, func(), error) {
	jump := jumpHop(cfg.Proxy)
	jumpConfig, err := jump.clientConfig(cfg.Timeout, log)
	if err != nil {
		return nil, nil, err
	}
	log.Infof("connecting to jump host %s as %s ...", jump.addr, jump.user)
	jumpClient, err := ssh.Dial("tcp", jump.addr, jumpConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to jump host %s: %w", jump.addr, err)
	}
	logHostKeyStatus(jump, log)
	log.Infof("connecting to %s as %s through the jump host ...", targetAddr, targetConfig.User)
	conn, err := jumpClient.Dial("tcp", targetAddr)
	if err != nil {
		jumpClient.Close()
		return nil, nil, fmt.Errorf("dialing %s through jump host %s: %w", targetAddr, jump.addr, err)
	}
	ncc, chans, reqs, err := ssh.NewClientConn(conn, targetAddr, targetConfig)
	if err != nil {
		conn.Close()
		jumpClient.Close()
		return nil, nil, fmt.Errorf("connecting to %s through jump host %s: %w", targetAddr, jump.addr, err)
	}
	logHostKeyStatus(targetHop(cfg), log)
	return ssh.NewClient(ncc, chans, reqs), func() { jumpClient.Close() }, nil
}

func authMethods(h hop) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if key := strings.TrimSpace(h.privateKey); key != "" {
		var signer ssh.Signer
		var err error
		if h.passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key+"\n"), []byte(h.passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(key + "\n"))
		}
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", h.names.privateKey, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if h.password != "" {
		methods = append(methods, ssh.Password(h.password))
	}
	return methods, nil
}
