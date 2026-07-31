package link

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Shaper applies link profiles with "tc", and is responsible for taking them
// down again.
//
// That last part is the safety-critical half of this file. A run that is
// aborted or times out while a qdisc is in place leaves the runner shaped, and
// every later measurement on that machine is quietly wrong with nothing in its
// output to say so. Callers must therefore install Clear on both the normal and
// the interrupted way out; see Guard.
type Shaper struct {
	// Iface is the interface towards the server. Empty means "resolve it", and
	// only the name is ever printed: the route also reports the server's
	// address, and the host is a secret in this repository.
	Iface string

	// Sudo is "0"/"no"/"none" for never, a command string to override, and
	// empty for "sudo -n when not root".
	Sudo string

	// Host is the SFTP server, used to resolve the interface.
	Host string

	// Logf and Warnf go to the job log, which masks secrets.
	Logf  func(format string, args ...any)
	Warnf func(format string, args ...any)

	// Available and Reason are set by Probe and stored in the result. Shaping
	// that is unavailable is recorded, never fatal.
	Available bool
	Reason    string

	// Requested is every profile the run was asked for, Applied every one that
	// was actually put in place (an unshaped profile counts as applied, because
	// the real line is what it asks for).
	Requested []string
	Applied   []string
}

// NewShaper builds a Shaper in the state a run starts in: nothing probed for
// yet, which is a different thing from "probed and unavailable".
func NewShaper(iface, sudo, host string, logf, warnf func(string, ...any)) *Shaper {
	return &Shaper{
		Iface:  iface,
		Sudo:   sudo,
		Host:   host,
		Logf:   logf,
		Warnf:  warnf,
		Reason: "no profile asked for shaping, so it was never probed for",
	}
}

// tc is the tc invocation, sudo included when one is needed and allowed.
func (s *Shaper) tc(args ...string) *exec.Cmd {
	var argv []string
	switch s.Sudo {
	case "0", "no", "none":
		argv = []string{"tc"}
	case "":
		if os.Geteuid() == 0 {
			argv = []string{"tc"}
		} else {
			argv = []string{"sudo", "-n", "tc"}
		}
	default:
		argv = append(strings.Fields(s.Sudo), "tc")
	}
	argv = append(argv, args...)
	return exec.Command(argv[0], argv[1:]...)
}

func (s *Shaper) tcDisplay() string {
	cmd := s.tc()
	return strings.Join(cmd.Args, " ")
}

// resolveIface is the interface packets to the SFTP server leave through.
func (s *Shaper) resolveIface() (string, error) {
	if s.Iface != "" {
		return s.Iface, nil
	}
	if _, err := exec.LookPath("ip"); err != nil {
		return "", err
	}
	if iface := ifaceFrom(exec.Command("ip", "route", "get", s.Host)); iface != "" {
		return iface, nil
	}
	if iface := ifaceFrom(exec.Command("ip", "route", "show", "default")); iface != "" {
		return iface, nil
	}
	return "", fmt.Errorf("no interface found")
}

// ifaceFrom picks the word after "dev" out of an "ip route" line. Nothing else
// of the output is read, and none of it is printed.
func ifaceFrom(cmd *exec.Cmd) string {
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "dev" {
			return fields[i+1]
		}
	}
	return ""
}

// Probe decides once whether shaping is possible, by doing it.
//
// Checking for "tc" and for root is not enough (a container may have the binary
// and no NET_ADMIN, or CAP_NET_ADMIN and no sudo), so this adds a real qdisc
// and removes it again. It never fails the run, because a benchmark of the real
// line is still a benchmark.
func (s *Shaper) Probe() {
	s.Available = false

	if _, err := exec.LookPath("tc"); err != nil {
		s.Reason = "tc is not installed on this runner (iproute2)"
		return
	}
	iface, err := s.resolveIface()
	if err != nil {
		s.Reason = "could not determine the interface towards the server; set LINK_IFACE"
		return
	}
	s.Iface = iface

	if err := s.tc("qdisc", "show", "dev", s.Iface).Run(); err != nil {
		s.Reason = fmt.Sprintf("cannot run '%s' on %s (NET_ADMIN missing, or sudo needs a password)",
			s.tcDisplay(), s.Iface)
		return
	}
	// The real thing: a netem qdisc with no delay, removed immediately.
	if err := s.tc("qdisc", "add", "dev", s.Iface, "root", "handle", "1:", "netem").Run(); err != nil {
		s.Reason = fmt.Sprintf(
			"adding a qdisc on %s was refused (NET_ADMIN missing, or something else already shapes it)", s.Iface)
		return
	}
	_ = s.tc("qdisc", "del", "dev", s.Iface, "root").Run()

	s.Available = true
	s.Reason = ""
	s.Logf("link shaping is available on %s", s.Iface)
}

// Clear puts the link back the way it was found. Idempotent, and quiet about a
// qdisc that is already gone.
func (s *Shaper) Clear() {
	if s == nil || s.Iface == "" {
		return
	}
	_ = s.tc("qdisc", "del", "dev", s.Iface, "root").Run()
}

// Apply shapes the link for this profile, or reports that it could not.
// Returns an error so the caller can record that the runs were measured
// unshaped instead of pretending they were not.
func (s *Shaper) Apply(profile string) error {
	delay := Delay(profile)
	rate := Rate(profile)

	// An unshaped profile is not a no-op: whatever the previous profile applied
	// has to come off first.
	s.Clear()
	if delay == "" && rate == "" {
		s.Applied = append(s.Applied, profile)
		return nil
	}
	if !s.Available {
		s.Warnf("link profile %s requested but shaping is unavailable (%s); measuring the real line instead",
			profile, s.Reason)
		return fmt.Errorf("shaping unavailable")
	}

	if delay != "" {
		if err := s.tc("qdisc", "add", "dev", s.Iface, "root", "handle", "1:", "netem", "delay", delay).Run(); err != nil {
			s.Warnf("could not add netem delay %s on %s; measuring the real line instead", delay, s.Iface)
			s.Clear()
			return err
		}
		if rate != "" {
			// tbf below netem, so packets are delayed and then metered. burst
			// and latency are tbf's own buffer sizing, not part of the profile:
			// too small a burst throttles below the configured rate.
			if err := s.tc("qdisc", "add", "dev", s.Iface, "parent", "1:", "handle", "2:",
				"tbf", "rate", rate, "burst", "32kbit", "latency", "400ms").Run(); err != nil {
				s.Warnf("could not add a tbf rate of %s on %s; measuring the real line instead", rate, s.Iface)
				s.Clear()
				return err
			}
		}
	} else {
		if err := s.tc("qdisc", "add", "dev", s.Iface, "root", "handle", "1:",
			"tbf", "rate", rate, "burst", "32kbit", "latency", "400ms").Run(); err != nil {
			s.Warnf("could not add a tbf rate of %s on %s; measuring the real line instead", rate, s.Iface)
			s.Clear()
			return err
		}
	}

	s.Applied = append(s.Applied, profile)
	s.Logf("link profile %s applied on %s", profile, s.Iface)
	return nil
}
