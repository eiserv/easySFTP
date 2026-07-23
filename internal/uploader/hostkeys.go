package uploader

import (
	"fmt"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func hostKeyCallback(h hop, log Logger) (ssh.HostKeyCallback, error) {
	want := h.fingerprints
	if len(want) == 0 && h.knownHosts == "" {
		// Unverified connections are an explicit opt-in in v3, per hop: a
		// pinned target behind an unpinned jump host (or vice versa) still
		// fails for the open hop unless that hop opts out too.
		if !h.allowAnyHostKey {
			return nil, fmt.Errorf("the identity of %[1]s cannot be verified: no %[2]s or %[3]s configured. "+
				"Pin the server's keys (run 'ssh-keyscan <server>' and set %[3]s, or convert with 'ssh-keygen -lf -' and set %[2]s), "+
				"or explicitly accept any host key with %[4]s (NOT recommended: allows man-in-the-middle attacks)",
				h.addr, h.names.hostKey, h.names.knownHosts, h.names.allowAny)
		}
		log.Warningf("%s is set; the identity of %s will NOT be verified and man-in-the-middle attacks are possible. "+
			"Pin the server's keys via %s or %s and remove the opt-out.",
			h.names.allowAny, h.addr, h.names.hostKey, h.names.knownHosts)
		return ssh.InsecureIgnoreHostKey(), nil
	}
	for _, fp := range want {
		if !strings.HasPrefix(fp, "SHA256:") {
			return nil, fmt.Errorf("%s must be a SHA256 fingerprint like 'SHA256:...', got %q", h.names.hostKey, fp)
		}
	}
	var khCallback ssh.HostKeyCallback
	if h.knownHosts != "" {
		var err error
		if khCallback, err = knownHostsCallback(h.knownHosts); err != nil {
			return nil, err
		}
	}
	// A key matching either input is accepted, mirroring how multiple
	// fingerprints already OR together: users can pin every key their server
	// presents, in whichever format they have at hand.
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		got := ssh.FingerprintSHA256(key)
		for _, fp := range want {
			if got == fp {
				return nil
			}
		}
		if khCallback != nil && khCallback(hostname, remote, key) == nil {
			return nil
		}
		accepted := append([]string{}, want...)
		if khCallback != nil {
			accepted = append(accepted, "the known-hosts entries")
		}
		// permanentError: a mismatch is a security signal (or a config error),
		// and retrying the connection would present the same key again.
		return permanentError{fmt.Errorf("host key mismatch for %s: got %s, want one of: %s", hostname, got, strings.Join(accepted, ", "))}
	}, nil
}

// knownHostsCallback builds a host key verifier from raw OpenSSH known_hosts
// lines (e.g. ssh-keyscan output). x/crypto's knownhosts parser only reads
// files, so the lines are staged in a temp file that is removed again right
// after parsing.
func knownHostsCallback(data string) (ssh.HostKeyCallback, error) {
	f, err := os.CreateTemp("", "easysftp-known-hosts-*")
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
