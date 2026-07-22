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
		// The warning fires per hop: a pinned target behind an unpinned jump
		// host (or vice versa) still deserves a nudge for the open hop.
		log.Warningf("no %[1]shost-key-fingerprint or %[1]sknown-hosts configured; the identity of %[2]s will NOT be verified. "+
			"Run 'ssh-keyscan <server>' and set the %[1]sknown-hosts input (or convert with 'ssh-keygen -lf -' "+
			"and set %[1]shost-key-fingerprint) to pin it.", h.inputPrefix, h.addr)
		return ssh.InsecureIgnoreHostKey(), nil
	}
	for _, fp := range want {
		if !strings.HasPrefix(fp, "SHA256:") {
			return nil, fmt.Errorf("%shost-key-fingerprint must be a SHA256 fingerprint like 'SHA256:...', got %q", h.inputPrefix, fp)
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
