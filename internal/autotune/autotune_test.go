package autotune_test

import (
	"strings"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/autotune"
)

const (
	KiB = 1 << 10
	MiB = 1 << 20
)

// house is a round-numbered stand-in for the line the stored sweeps were
// measured over: a 10 ms RTT and a 300 ms handshake. The real numbers are
// 13 ms and ~360 ms; rounding them keeps the expectations below readable
// without moving any of the decisions (regret_test.go replays the policy
// against the measured values).
var house = autotune.Link{RTT: 10 * time.Millisecond, Handshake: 300 * time.Millisecond}

// TestPlanChoosesPerWorkload walks the shapes the benchmark scenarios are made
// of. Each case names the measured optimum it is aiming at, so a constant that
// drifts fails against the evidence rather than against a number somebody once
// typed here.
func TestPlanChoosesPerWorkload(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    autotune.Workload
		want autotune.Settings
		why  string
	}{
		{
			name: "one large file cannot use a pool",
			w:    autotune.Workload{Uploads: 1, UploadBytes: 32 * MiB, LargestUpload: 32 * MiB},
			want: autotune.Settings{Connections: 1, Concurrency: 1, RequestConcurrency: 64},
			why:  "only the per-file path spreads, so 32 MiB in one file is one connection however long it takes",
		},
		{
			name: "two large files use one connection each",
			w:    autotune.Workload{Uploads: 2, UploadBytes: 32 * MiB, LargestUpload: 16 * MiB},
			want: autotune.Settings{Connections: 2, Concurrency: 2, RequestConcurrency: 64},
			why:  "the stored 'large' sweep is fastest at 2/2 and cannot go above the file count",
		},
		{
			name: "many small files",
			w:    autotune.Workload{Uploads: 300, UploadBytes: 300 * 4 * KiB, LargestUpload: 4 * KiB},
			want: autotune.Settings{Connections: 5, Concurrency: 64, RequestConcurrency: 16},
			why: "the stored 'small' sweep is fastest at 4 connections and flat past 32 workers; " +
				"5 is between two swept columns (4 and 8) and the replay scores it at 4, " +
				"the same 5.0/4.5/4.9 second medians, so refitting the throughput prior " +
				"in #230 moved this by one connection into a gap the grid cannot separate",
		},
		{
			name: "very many small files",
			w:    autotune.Workload{Uploads: 2000, UploadBytes: 2000 * 4 * KiB, LargestUpload: 4 * KiB},
			want: autotune.Settings{Connections: 8, Concurrency: 64, RequestConcurrency: 16},
			why:  "the stored 'bulk' sweep still improves at the largest values swept",
		},
		{
			name: "a redeploy that may skip everything",
			w:    autotune.Workload{Probes: 500, LargestUpload: 4 * KiB, Unknown: true},
			want: autotune.Settings{Connections: 1, Concurrency: 64, RequestConcurrency: 16},
			why:  "500 stats are worth 64 workers and no second handshake; the stored 'redeploy' sweep is fastest at 1/64",
		},
		{
			name: "a sync that changed three files",
			w:    autotune.Workload{Uploads: 3, UploadBytes: 3 * 4 * KiB, LargestUpload: 4 * KiB},
			want: autotune.Settings{Connections: 1, Concurrency: 3, RequestConcurrency: 16},
			why:  "the whole transfer is shorter than one handshake",
		},
		{
			name: "nothing to do",
			w:    autotune.Workload{},
			want: autotune.Settings{Connections: 1, Concurrency: 1, RequestConcurrency: 16},
			why:  "an empty deployment must still resolve to something usable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := autotune.Plan(tc.w, house, autotune.Fixed{}); got != tc.want {
				t.Errorf("Plan() = %v, want %v\n%s", got, tc.want, tc.why)
			}
		})
	}
}

// TestPinnedSettingsAreNeverChanged is the promise the issue makes to anyone
// who already tuned their deploy: a number in the config file is the number
// the run uses, and the settings around it are chosen inside that constraint.
func TestPinnedSettingsAreNeverChanged(t *testing.T) {
	w := autotune.Workload{Uploads: 2000, UploadBytes: 2000 * 4 * KiB, LargestUpload: 4 * KiB}

	got := autotune.Plan(w, house, autotune.Fixed{Connections: 2})
	if got.Connections != 2 {
		t.Errorf("a pinned connections=2 became %d", got.Connections)
	}
	if got.Concurrency != autotune.MaxConcurrency {
		t.Errorf("concurrency = %d, want the workload's own value even next to a pinned pool", got.Concurrency)
	}

	got = autotune.Plan(w, house, autotune.Fixed{Concurrency: 3})
	if got.Concurrency != 3 {
		t.Errorf("a pinned concurrency=3 became %d", got.Concurrency)
	}
	if got.Connections > 3 {
		t.Errorf("connections = %d, want at most the pinned worker count: a connection no worker picks is a handshake for nothing", got.Connections)
	}

	got = autotune.Plan(w, house, autotune.Fixed{RequestConcurrency: 7})
	if got.RequestConcurrency != 7 {
		t.Errorf("a pinned request_concurrency=7 became %d", got.RequestConcurrency)
	}

	all := autotune.Fixed{Connections: 5, Concurrency: 6, RequestConcurrency: 7}
	if got := autotune.Plan(w, house, all); got != (autotune.Settings{Connections: 5, Concurrency: 6, RequestConcurrency: 7}) {
		t.Errorf("a fully pinned configuration was rewritten to %v", got)
	}
	if !all.All() {
		t.Error("Fixed.All() must report a fully pinned configuration, which is what switches the probe off")
	}
}

// TestRequestConcurrencyFollowsFileSizeAndBudget: the setting is per-file
// in-flight write packets, so only a file with several packets can use it, and
// concurrency times request_concurrency is what it costs in buffers.
func TestRequestConcurrencyFollowsFileSizeAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    autotune.Workload
		want int
	}{
		{"a tree of 4 KiB files keeps the default", autotune.Workload{Uploads: 50, LargestUpload: 4 * KiB}, 16},
		{"64 KiB files still keep the default", autotune.Workload{Uploads: 50, LargestUpload: 64 * KiB}, 16},
		{"a 4 MiB file among few files pipelines fully", autotune.Workload{Uploads: 4, LargestUpload: 4 * MiB}, 64},
		{"a 4 MiB file among many gives way to the budget", autotune.Workload{Uploads: 500, LargestUpload: 4 * MiB}, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := autotune.Plan(tc.w, house, autotune.Fixed{}).RequestConcurrency; got != tc.want {
				t.Errorf("request_concurrency = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestPlanStaysInsideItsBounds is the safety net: whatever is thrown at the
// policy, it must not ask for a value a server or a runner would choke on, and
// it must never return zero, which every caller would read as "no limit".
func TestPlanStaysInsideItsBounds(t *testing.T) {
	links := []autotune.Link{
		{}, // nothing measured
		{RTT: time.Microsecond, Handshake: time.Microsecond},            // a local socket
		{RTT: 2 * time.Second, Handshake: 20 * time.Second},             // a very bad path
		{RTT: 10 * time.Millisecond, StreamBytesPerSecond: 1},           // a link measured as almost dead
		{Handshake: 300 * time.Millisecond, StreamBytesPerSecond: 1e12}, // and one measured as absurdly fast
	}
	workloads := []autotune.Workload{
		{},
		{Uploads: 1_000_000, UploadBytes: 1 << 42, LargestUpload: 1 << 34},
		{Probes: 1_000_000},
		{Uploads: 1, UploadBytes: -1, LargestUpload: -1},
	}
	for _, l := range links {
		for _, w := range workloads {
			s := autotune.Plan(w, l, autotune.Fixed{})
			switch {
			case s.Connections < 1 || s.Connections > autotune.MaxConnections:
				t.Errorf("connections %d out of bounds for %+v / %+v", s.Connections, w, l)
			case s.Concurrency < 1 || s.Concurrency > autotune.MaxConcurrency:
				t.Errorf("concurrency %d out of bounds for %+v / %+v", s.Concurrency, w, l)
			case s.RequestConcurrency < autotune.MinRequestConcurrency || s.RequestConcurrency > autotune.MaxRequestConcurrency:
				t.Errorf("request_concurrency %d out of bounds for %+v / %+v", s.RequestConcurrency, w, l)
			case s.Connections > s.Concurrency:
				t.Errorf("connections %d exceed concurrency %d for %+v / %+v", s.Connections, s.Concurrency, w, l)
			}
		}
	}
}

// TestAnUnmeasuredLinkFallsBackInsteadOfDividingByZero: a server that refuses
// SSH_FXP_REALPATH leaves the RTT at zero, and a Config built directly (a test,
// or a caller that never probed) leaves the whole Link empty.
func TestAnUnmeasuredLinkFallsBackInsteadOfDividingByZero(t *testing.T) {
	w := autotune.Workload{Uploads: 200, UploadBytes: 200 * 8 * KiB, LargestUpload: 8 * KiB}
	got := autotune.Plan(w, autotune.Link{}, autotune.Fixed{})
	if got.Connections < 1 || got.Concurrency != 64 {
		t.Fatalf("an unmeasured link produced %v", got)
	}
	if (autotune.Link{}).Measured() {
		t.Error("an empty link must not claim a measured throughput")
	}
	if !(autotune.Link{StreamBytesPerSecond: 1}).Measured() {
		t.Error("a link with an observed throughput must say so")
	}
}

// TestMeasuredThroughputMovesTheDecision is the difference stage 3 makes: the
// same files over a link that has been measured as slow are worth more
// connections than the same files over the conservative prior are, and a link
// measured as fast is worth fewer.
func TestMeasuredThroughputMovesTheDecision(t *testing.T) {
	// Both fixtures moved when #230 refitted the throughput prior from 1 MiB/s
	// to the measured 0.35 MiB/s, and both moved because the refit did what it
	// was meant to do.
	//
	// The workload is smaller because 12 MiB now puts the *prior* at
	// MaxConnections, where a slower measurement has nowhere left to go; this
	// one leaves the prior at 6, off the ceiling, so the ordering can be read.
	// And "slow" has to be genuinely below the prior: 300 KiB/s is no longer
	// slower than what Plan already assumes.
	w := autotune.Workload{Uploads: 60, UploadBytes: 4 * MiB, LargestUpload: 2 * MiB}

	slow := house
	slow.StreamBytesPerSecond = 100 * KiB
	fast := house
	fast.StreamBytesPerSecond = 50 * MiB

	slowChoice := autotune.Plan(w, slow, autotune.Fixed{}).Connections
	priorChoice := autotune.Plan(w, house, autotune.Fixed{}).Connections
	fastChoice := autotune.Plan(w, fast, autotune.Fixed{}).Connections

	if !(slowChoice > priorChoice && priorChoice > fastChoice) {
		t.Errorf("connections should fall as the measured link gets faster, got slow=%d prior=%d fast=%d",
			slowChoice, priorChoice, fastChoice)
	}
}

// TestExplainNamesWhatWasNotChosen: the log line must never read as if
// easySFTP picked a number the workflow picked.
func TestExplainNamesWhatWasNotChosen(t *testing.T) {
	w := autotune.Workload{Uploads: 10, UploadBytes: 40 * KiB, LargestUpload: 4 * KiB, Probes: 3}
	f := autotune.Fixed{Concurrency: 4}
	line := autotune.Explain(w, house, f, autotune.Plan(w, house, f))

	for _, want := range []string{"files=10", "probes=3", "rtt=10ms", "handshake=300ms", "assumed", "concurrency=4", "concurrency comes from the configuration"} {
		if !strings.Contains(line, want) {
			t.Errorf("the explanation is missing %q:\n%s", want, line)
		}
	}
	if plain := autotune.Explain(w, house, autotune.Fixed{}, autotune.Plan(w, house, autotune.Fixed{})); strings.Contains(plain, "configuration") {
		t.Errorf("a fully adaptive run must not claim anything came from the configuration:\n%s", plain)
	}
	unsettled := w
	unsettled.Unknown = true
	if line := autotune.Explain(unsettled, house, autotune.Fixed{}, autotune.Settings{}); !strings.Contains(line, "not known yet") {
		t.Errorf("an unsettled upload set must say so:\n%s", line)
	}
}
