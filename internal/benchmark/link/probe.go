package link

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"

	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

// guardOnce keeps the interrupt handler to one installation per process, the
// way link_shape_trap kept it to one trap per shell.
var guardOnce sync.Once

// Guard makes sure an interrupted run still takes its shaping down.
//
// Without it an aborted or timed out run leaves the runner shaped, and every
// later measurement on that machine is quietly wrong with nothing in its output
// to say so. Callers pair it with a deferred Clear for the normal way out; this
// covers the ways out that skip defers.
//
// Installed on the first Apply rather than at startup, so a run that shapes
// nothing keeps the default signal behaviour.
func (s *Shaper) Guard() {
	guardOnce.Do(func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		go func() {
			received := <-signals
			s.Clear()
			if received == syscall.SIGTERM {
				os.Exit(143)
			}
			os.Exit(130)
		}()
	})
}

// Prober runs cmd/linkprobe and records what it reported.
type Prober struct {
	// Binary is the built cmd/linkprobe. Empty means "do not probe", so a local
	// run without it still works and stores an empty probe list.
	Binary string

	Host       string
	Port       int
	Username   string
	Password   string
	KnownHosts string
	RemotePath string

	Logf  func(format string, args ...any)
	Warnf func(format string, args ...any)
}

// Probe runs the probe once and returns the document, wrapped with the profile
// it belongs to and whether it was taken before or after the measured runs.
//
// It returns nil rather than an error when there is nothing to record: an empty
// probe list is honest, an invented entry is not.
func (p *Prober) Probe(profile, at string) json.RawMessage {
	if p.Binary == "" {
		return nil
	}
	if !executable(p.Binary) {
		p.Warnf("LINKPROBE_BIN ('%s') is not executable; the link is not being probed", p.Binary)
		return nil
	}

	// The probe's own stderr goes to the job log, which masks secrets. Its
	// stdout is the document and is stored, which is why cmd/linkprobe keeps
	// the host and the user out of it.
	cmd := exec.Command(p.Binary)
	cmd.Env = append(os.Environ(),
		"LINKPROBE_HOST="+p.Host,
		fmt.Sprintf("LINKPROBE_PORT=%d", p.Port),
		"LINKPROBE_USERNAME="+p.Username,
		"LINKPROBE_PASSWORD="+p.Password,
		"LINKPROBE_KNOWN_HOSTS="+p.KnownHosts,
		"LINKPROBE_REMOTE_PATH="+p.RemotePath,
	)
	cmd.Stderr = os.Stderr
	document, err := cmd.Output()
	if err != nil {
		p.Warnf("the link probe for profile %s (%s) failed; no document stored", profile, at)
		return nil
	}
	if !json.Valid(document) {
		p.Warnf("the link probe for profile %s (%s) produced no JSON; no document stored", profile, at)
		return nil
	}

	wrapped, err := wrap(profile, at, document)
	if err != nil {
		p.Warnf("the link probe for profile %s (%s) produced no JSON object; no document stored", profile, at)
		return nil
	}
	p.Logf("%s", summarize(wrapped))
	return wrapped
}

// executable is the "-x" test the shell did before running the probe, so a
// binary that was never built gets the message that says so rather than a
// generic failure. Windows has no executable bit, so there it is a file test.
func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

// wrap prepends the coordinates to the probe document without re-encoding what
// the probe measured.
//
// The document is stored verbatim (see schema.Probe.MarshalJSON): re-encoding
// it from the fields this repository models would silently drop anything
// cmd/linkprobe reports that they do not cover, and would rewrite the numbers
// they do cover, so a load of "1.0" would come back as "1".
func wrap(profile, at string, document []byte) (json.RawMessage, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, document); err != nil {
		return nil, err
	}
	body := compact.Bytes()
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return nil, fmt.Errorf("the probe document is not a JSON object")
	}

	head, err := json.Marshal(struct {
		Profile string `json:"profile"`
		At      string `json:"at"`
	}{profile, at})
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.Write(head[:len(head)-1]) // everything but the closing brace
	if len(body) > 2 {
		out.WriteByte(',')
		out.Write(body[1:])
	} else {
		out.WriteByte('}')
	}
	return out.Bytes(), nil
}

// summarize is the one line a probe prints into the job log.
func summarize(wrapped []byte) string {
	var probe schema.Probe
	if err := json.Unmarshal(wrapped, &probe); err != nil {
		return "link probe: unreadable"
	}
	rtt := "n/a"
	if probe.RTTMS != nil && probe.RTTMS.P50 != nil {
		rtt = stats.Format(*probe.RTTMS.P50)
	}
	single, multi := "n/a", "n/a"
	if probe.Control != nil {
		if probe.Control.SingleStreamMiBPerS != nil {
			single = stats.Format(*probe.Control.SingleStreamMiBPerS)
		}
		if probe.Control.NStreamMiBPerS != nil {
			multi = stats.Format(*probe.Control.NStreamMiBPerS)
		}
	}
	return fmt.Sprintf("link probe %s (%s): rtt p50 %s ms, control %s MiB/s single / %s MiB/s multi",
		probe.Profile, probe.At, rtt, single, multi)
}
