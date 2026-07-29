// Package linkprobe measures the network path between the benchmark runner and
// the SFTP server (issue #184, phase 1).
//
// The benchmark's "environment" object describes the runner and nothing about
// the path to the server, yet that path is most of what the numbers measure.
// This package supplies the missing half: round-trip time, a throughput control
// that does not go through easySFTP's uploader, and the server's own load where
// it is readable.
//
// It deliberately imports nothing from internal/uploader, internal/session or
// internal/config. A control measurement taken through the code under test
// would not be a control. What it does share with easySFTP is the libraries
// (x/crypto/ssh and pkg/sftp), which is why the report says so in its "note":
// this separates "easySFTP's upload pipeline is slow" from "the line is slow",
// not "pkg/sftp" from the line.
//
// Nothing reported here is allowed to name the host, the user or the password.
// The report is embedded in results.json, which is uploaded as a workflow
// artifact and committed to benchmarks/, and neither masks secrets. The remote
// path is not in that category (it is a plain workflow input with a public
// default), so an error may name it; a message without it would be much harder
// to act on.
package linkprobe

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// schemaVersion of the report below. Bump it when a field changes meaning, not
// when one is added.
const schemaVersion = 1

// Defaults. RTTSamples is odd so the median is a measured value rather than an
// interpolation between two. ControlBytes is a compromise: on the ~0.38 MiB/s
// per-flow ceiling the stored matrix results show, 8 MiB takes about 20 seconds
// per pass, and a probe runs a handful of times per benchmark, not per run.
const (
	DefaultRTTSamples     = 21
	DefaultControlBytes   = 8 << 20
	DefaultControlStreams = 4
	DefaultTimeout        = 5 * time.Minute
)

// Config is everything the probe needs. It carries secrets (Password) and
// identifying data (Host, User), none of which may appear in Report.
type Config struct {
	Host       string
	Port       int
	User       string
	Password   string
	KnownHosts string // verbatim ssh-keyscan output, verified like a real run

	// RemotePath is a directory the probe may create and delete files in. The
	// benchmark points it at the directory it already owns.
	RemotePath string

	RTTSamples     int
	ControlBytes   int64 // total payload per throughput pass, not per stream
	ControlStreams int
	Timeout        time.Duration
}

// withDefaults returns c with the zero values filled in, so a caller may set
// only what it cares about.
func (c Config) withDefaults() Config {
	if c.Port == 0 {
		c.Port = 22
	}
	if c.RTTSamples <= 0 {
		c.RTTSamples = DefaultRTTSamples
	}
	if c.ControlBytes <= 0 {
		c.ControlBytes = DefaultControlBytes
	}
	if c.ControlStreams <= 0 {
		c.ControlStreams = DefaultControlStreams
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	return c
}

// Report is one probe of the link, as stored under "link.probes[]" in a
// benchmark result. Every duration is milliseconds and every rate is MiB/s, to
// match what the rest of the benchmark reports.
//
// A measurement that failed leaves its object nil (JSON null) and adds a line
// to Errors. A partial report is worth far more than no report: a run whose
// throughput control timed out has still recorded its RTT.
type Report struct {
	SchemaVersion int    `json:"schema_version"`
	Note          string `json:"note"`
	MeasuredAt    string `json:"measured_at"`

	// HandshakeMS is dial plus SSH handshake plus SFTP subsystem, i.e. what a
	// deploy pays before it can ask anything. 0 when the connection failed.
	HandshakeMS float64 `json:"handshake_ms"`

	RTT      *RTT      `json:"rtt_ms"`
	Control  *Control  `json:"control"`
	HostLoad *HostLoad `json:"host_load"`

	// Errors is never nil, so a reader can test its length without a null
	// check. Each entry names the measurement that failed.
	Errors []string `json:"errors"`
}

// RTT is the distribution of a no-op round-trip: a Stat of one fixed path,
// issued sequentially so no two calls can overlap.
type RTT struct {
	P50     float64 `json:"p50"`
	P90     float64 `json:"p90"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Samples int     `json:"samples"`
}

// Control is the throughput ceiling of the path, measured without easySFTP.
//
// SingleStreamMiBPerS is the per-flow ceiling: what one TCP connection and one
// SSH channel can carry. NStreamMiBPerS writes the same total payload spread
// over Streams separate connections, so the two numbers are directly
// comparable and their ratio says whether the ceiling is per flow or on the
// line as a whole.
type Control struct {
	Streams             int     `json:"streams"`
	Bytes               int64   `json:"bytes"`
	SingleStreamMiBPerS float64 `json:"single_stream_mib_per_s"`
	NStreamMiBPerS      float64 `json:"n_stream_mib_per_s"`
	Note                string  `json:"note"`
}

// HostLoad is the SFTP host's own load average. A fixed server is not a
// constant server, and a matrix run takes hours.
//
// Unavailable is the normal case rather than a failure: an SFTP-only account,
// which is what a sane benchmark server offers, has neither an exec channel nor
// a /proc to read.
type HostLoad struct {
	Available bool     `json:"available"`
	Method    string   `json:"method,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Load1     *float64 `json:"load1,omitempty"`
	Load5     *float64 `json:"load5,omitempty"`
	Load15    *float64 `json:"load15,omitempty"`
}

// Run probes the link and always returns a Report. The error is non-nil only
// when nothing could be measured at all (the connection failed), and even then
// the Report is valid JSON describing that: the benchmark stores it either way,
// because "the link was unreachable at 14:02" is data.
func Run(ctx context.Context, cfg Config) (Report, error) {
	cfg = cfg.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	rep := Report{
		SchemaVersion: schemaVersion,
		Note: "measured with x/crypto/ssh and pkg/sftp, not with easySFTP's uploader: " +
			"this separates the line from easySFTP, not pkg/sftp from the line. " +
			"rtt is a sequential no-op round-trip; control writes the same total payload once over one connection and once over several",
		MeasuredAt: time.Now().UTC().Format(time.RFC3339),
		Errors:     []string{},
	}

	start := time.Now()
	conn, err := dial(ctx, cfg)
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("connect: %v", err))
		return rep, err
	}
	rep.HandshakeMS = milliseconds(time.Since(start))
	defer conn.Close()

	// The directory is created once, here, so both the RTT target and the
	// control payload can count on it. The benchmark points RemotePath at a
	// directory it owns, which on a fresh server does not exist yet: the runs
	// create it, and the probe measures before the first one.
	statTarget := "/"
	if cfg.RemotePath != "" {
		if err := conn.sftp.MkdirAll(cfg.RemotePath); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("remote path: %v", err))
		} else {
			statTarget = cfg.RemotePath
		}
	}

	if rtt, err := measureRTT(ctx, conn, statTarget, cfg.RTTSamples); err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("rtt: %v", err))
	} else {
		rep.RTT = rtt
	}

	// Host load before the throughput pass: it should describe the server as
	// the benchmark is about to find it, not as the probe just left it.
	rep.HostLoad = measureHostLoad(ctx, conn, cfg)

	if control, err := measureControl(ctx, conn, cfg); err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("control: %v", err))
	} else {
		rep.Control = control
	}

	return rep, nil
}

// percentile returns the p-quantile of an ascending slice by nearest rank, the
// same way internal/metrics does it, so an RTT p90 here and an sftp_stat p90
// there are the same kind of number.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func summarize(samples []time.Duration) *RTT {
	if len(samples) == 0 {
		return nil
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return &RTT{
		P50:     milliseconds(percentile(samples, 0.50)),
		P90:     milliseconds(percentile(samples, 0.90)),
		Min:     milliseconds(samples[0]),
		Max:     milliseconds(samples[len(samples)-1]),
		Samples: len(samples),
	}
}

func milliseconds(d time.Duration) float64 {
	return round2(float64(d) / float64(time.Millisecond))
}

// mibPerSecond of n bytes moved in d. Zero rather than an infinity when the
// clock did not advance, so the JSON stays a number a plot can read.
func mibPerSecond(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return round2((float64(n) / (1 << 20)) / d.Seconds())
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

// errNoRemotePath keeps the two callers that need a writable directory from
// each inventing their own message.
var errNoRemotePath = errors.New("no remote path configured to write the control payload into")
