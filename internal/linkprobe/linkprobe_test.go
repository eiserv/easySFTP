package linkprobe

import (
	"context"
	"strings"
	"testing"
)

func TestRunMeasuresRTTAndControl(t *testing.T) {
	srv := startTestServer(t)
	cfg := srv.config(t)

	rep, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Errors) != 0 {
		t.Fatalf("errors: %v", rep.Errors)
	}
	if rep.SchemaVersion != schemaVersion {
		t.Errorf("schema_version = %d, want %d", rep.SchemaVersion, schemaVersion)
	}
	if rep.MeasuredAt == "" {
		t.Error("measured_at is empty")
	}
	if rep.HandshakeMS <= 0 {
		t.Errorf("handshake_ms = %v, want a positive duration", rep.HandshakeMS)
	}

	if rep.RTT == nil {
		t.Fatal("rtt_ms is null")
	}
	if rep.RTT.Samples != cfg.RTTSamples {
		t.Errorf("rtt samples = %d, want %d", rep.RTT.Samples, cfg.RTTSamples)
	}
	// Ordering is the only claim that holds on a loopback server, where every
	// round-trip is fast enough to round to the same number of milliseconds.
	if rep.RTT.Min > rep.RTT.P50 || rep.RTT.P50 > rep.RTT.P90 || rep.RTT.P90 > rep.RTT.Max {
		t.Errorf("rtt percentiles are not ordered: %+v", rep.RTT)
	}

	if rep.Control == nil {
		t.Fatal("control is null")
	}
	if rep.Control.Bytes != cfg.ControlBytes {
		t.Errorf("control bytes = %d, want %d", rep.Control.Bytes, cfg.ControlBytes)
	}
	if rep.Control.Streams != cfg.ControlStreams {
		t.Errorf("control streams = %d, want %d", rep.Control.Streams, cfg.ControlStreams)
	}
	if rep.Control.SingleStreamMiBPerS <= 0 || rep.Control.NStreamMiBPerS <= 0 {
		t.Errorf("control rates should be positive, got %+v", rep.Control)
	}
}

// The payload must not survive the probe: a leftover would show up in the next
// benchmark run's remote scan and be counted as a file to delete.
func TestRunRemovesItsControlPayload(t *testing.T) {
	srv := startTestServer(t)
	cfg := srv.config(t)

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	c, err := dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dial for verification: %v", err)
	}
	defer c.Close()
	entries, err := c.sftp.ReadDir(cfg.RemotePath)
	if err != nil {
		t.Fatalf("reading %q: %v", cfg.RemotePath, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), controlFilePrefix) {
			t.Errorf("control payload %q was left behind", entry.Name())
		}
	}
}

// Without a writable directory the throughput control cannot run, and that must
// cost the RTT measurement nothing: a partial report is worth far more than no
// report.
func TestRunWithoutRemotePathStillMeasuresRTT(t *testing.T) {
	srv := startTestServer(t)
	cfg := srv.config(t)
	cfg.RemotePath = ""

	rep, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.RTT == nil {
		t.Error("rtt_ms is null, but RTT does not need a writable path")
	}
	if rep.Control != nil {
		t.Errorf("control should be null without a remote path, got %+v", rep.Control)
	}
	if len(rep.Errors) != 1 || !strings.HasPrefix(rep.Errors[0], "control:") {
		t.Errorf("errors = %v, want one entry naming the control measurement", rep.Errors)
	}
}

// The expected outcome on a locked down server. Recorded as a reason, not as an
// error: an SFTP-only account has neither /proc nor an exec channel by design.
func TestHostLoadUnavailableOnSFTPOnlyServer(t *testing.T) {
	srv := startTestServer(t)

	rep, err := Run(context.Background(), srv.config(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.HostLoad == nil {
		t.Fatal("host_load is null; it should always say something")
	}
	if rep.HostLoad.Available {
		t.Errorf("host_load should be unavailable, got %+v", rep.HostLoad)
	}
	if rep.HostLoad.Reason == "" {
		t.Error("an unavailable host load must carry a reason")
	}
	if len(rep.Errors) != 0 {
		t.Errorf("an unreadable host load is not an error: %v", rep.Errors)
	}
}

func TestHostLoadFromProc(t *testing.T) {
	srv := startTestServer(t, withProcLoadavg("0.52 1.03 0.98 2/431 12345\n"))

	rep, err := Run(context.Background(), srv.config(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	load := rep.HostLoad
	if load == nil || !load.Available {
		t.Fatalf("host_load = %+v, want an available reading", load)
	}
	if load.Method != "sftp:/proc/loadavg" {
		t.Errorf("method = %q, want sftp:/proc/loadavg", load.Method)
	}
	if load.Load1 == nil || *load.Load1 != 0.52 {
		t.Errorf("load1 = %v, want 0.52", load.Load1)
	}
	if load.Load15 == nil || *load.Load15 != 0.98 {
		t.Errorf("load15 = %v, want 0.98", load.Load15)
	}
}

// The fallback: no /proc, but the account may run a command.
func TestHostLoadFromExec(t *testing.T) {
	srv := startTestServer(t, withExec(
		" 14:02:11 up 9 days,  3:11,  2 users,  load average: 1.25, 0.80, 0.42\n"))

	rep, err := Run(context.Background(), srv.config(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	load := rep.HostLoad
	if load == nil || !load.Available {
		t.Fatalf("host_load = %+v, want an available reading", load)
	}
	if load.Method != "exec:uptime" {
		t.Errorf("method = %q, want exec:uptime", load.Method)
	}
	if load.Load1 == nil || *load.Load1 != 1.25 {
		t.Errorf("load1 = %v, want 1.25", load.Load1)
	}
}

// The probe verifies the server like a real run does, and a report is produced
// even when it cannot connect: "the link was unreachable" is data too.
func TestRunRefusesAMismatchedHostKey(t *testing.T) {
	srv := startTestServer(t)
	other := startTestServer(t)
	cfg := srv.config(t)
	cfg.KnownHosts = strings.Replace(other.knownHosts(t), other.Addr, srv.Addr, 1)

	rep, err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run accepted a host key that is not the server's")
	}
	if len(rep.Errors) != 1 || !strings.HasPrefix(rep.Errors[0], "connect:") {
		t.Errorf("errors = %v, want one entry naming the connection", rep.Errors)
	}
	if rep.RTT != nil || rep.Control != nil {
		t.Error("nothing may be reported as measured when the connection failed")
	}
}

func TestNoKnownHostsIsRefused(t *testing.T) {
	srv := startTestServer(t)
	cfg := srv.config(t)
	cfg.KnownHosts = ""

	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("Run connected without any known-hosts material")
	}
}

func TestParseUptime(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want [3]float64
	}{
		{
			name: "linux",
			out:  " 14:02:11 up 9 days,  3:11,  2 users,  load average: 1.25, 0.80, 0.42",
			want: [3]float64{1.25, 0.80, 0.42},
		},
		{
			// The BSDs pluralise and separate with spaces, macOS included.
			name: "bsd",
			out:  "14:02  up 3 days, 22:15, 4 users, load averages: 2.11 1.90 1.55",
			want: [3]float64{2.11, 1.90, 1.55},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			load := parseUptime(tc.out)
			if load == nil {
				t.Fatal("parseUptime returned nothing")
			}
			got := [3]float64{*load.Load1, *load.Load5, *load.Load15}
			if got != tc.want {
				t.Errorf("loads = %v, want %v", got, tc.want)
			}
		})
	}

	// A guess is worse than an absent measurement, so anything unrecognised
	// must come back empty rather than partially parsed.
	for _, out := range []string{"", "up 3 days", "load average: nope, 1.0, 2.0"} {
		if load := parseUptime(out); load != nil {
			t.Errorf("parseUptime(%q) = %+v, want nothing", out, load)
		}
	}
}

// blockReader is what keeps a multi-megabyte payload from costing a megabyte
// per stream, so it has to hand out exactly the requested total.
func TestBlockReaderYieldsExactlyItsBudget(t *testing.T) {
	block := []byte("0123456789")
	r := &blockReader{block: block, remaining: 25}
	buf := make([]byte, 7)
	total := 0
	for {
		n, err := r.Read(buf)
		total += n
		if err != nil {
			if total != 25 {
				t.Fatalf("read %d bytes, want 25", total)
			}
			break
		}
		if total > 25 {
			t.Fatalf("read past the budget: %d bytes", total)
		}
	}
}
