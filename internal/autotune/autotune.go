// Package autotune resolves the transport settings a run left at "auto":
// how many SSH connections it opens, how many files it uploads in parallel
// and how many SFTP requests one file may have in flight.
//
// Nothing in here talks to a server or reads a clock. It is a cost model plus
// the two decisions that fall out of it, so the whole policy can be replayed
// against the stored benchmark grids (see regret_test.go) instead of being
// argued about.
//
// # The three stages
//
// The policy runs in the three stages issue #209 describes, and each of them
// only ever adds information to the same model:
//
//  1. Workload. After the local scan a run knows what it is about to send:
//     how many files, how many bytes, the largest one, and how many pure
//     metadata round-trips (skip stats, deletes) come with them. That is
//     enough to size concurrency exactly and to bound the other two.
//  2. Link. The first connection measures its own handshake, and a few cheap
//     round-trips measure the RTT. Both go into Link and make the cost model
//     concrete: an extra connection costs one handshake, and a round-trip
//     costs one RTT.
//  3. Runtime. What the model cannot know before the transfer is the link's
//     throughput, so Plan starts from a deliberately conservative prior.
//     Controller replaces that prior with the throughput the run is actually
//     achieving and re-plans against the work that is left. See controller.go.
//
// # Why connections are the interesting knob
//
// Concurrency is free: a worker with no file to upload never starts, so the
// useful value is the number of independent items the phase has, capped for
// safety. Nothing about that needs measuring.
//
// A connection is not free. Every one past the first costs a full SSH
// handshake (dialed on first use, and dialed under the session lock, so the
// cost lands in the run's critical path), and it buys a second TCP flow, a
// second cipher stream and a second sftp-server process on the far side. With
// perfect scaling a run that takes W on one connection takes
//
//	T(k) = W/k + (k-1)*H
//
// on k of them, which is smallest at k = sqrt(W/H): open another connection
// exactly while the time it saves is larger than the handshake it costs. That
// single line is the whole connections policy; everything else in this file
// exists to estimate W.
//
// # Where the constants come from
//
// The estimate of W is fitted against the sweeps under benchmarks/matrix/,
// measured over a 13 ms line whose single stream carried about 0.36 MiB/s.
// They are order-of-magnitude constants, not calibration: k depends on the
// square root of W, so being wrong about W by a factor of four moves k by a
// factor of two, and k is clamped hard at both ends anyway.
package autotune

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Hard bounds on what the policy may choose. A user who writes a number gets
// that number; these only ever bound "auto".
const (
	// MaxConnections is as far as the policy will go on its own. The stored
	// sweeps still improve at 8 for the largest payload, so this is a safety
	// stop rather than a measured optimum: sshd's MaxStartups and the
	// per-account connection limits of shared hosting are real, and a policy
	// that trips them for the last few percent is not a good default.
	MaxConnections = 8

	// MaxConcurrency bounds the files in flight. Past this the round-trips of
	// one deploy stop being the limit and the server's own request handling
	// starts to be, and the memory held by the in-flight write buffers grows
	// with it (see maxInFlightPackets).
	MaxConcurrency = 64

	// MinRequestConcurrency is easySFTP's long-standing default and the floor
	// the policy keeps: at 16 packets a file up to 512 KiB is fully
	// pipelined, which covers most of what a web deploy contains.
	MinRequestConcurrency = 16

	// MaxRequestConcurrency is the SSH channel window expressed in packets.
	// x/crypto/ssh (and OpenSSH) open a channel with a 2 MiB window and
	// pkg/sftp writes 32 KiB packets, so 64 requests already fill the window
	// one connection has. Asking for more cannot put more bytes on the wire.
	MaxRequestConcurrency = 64
)

const (
	// packetBytes is pkg/sftp's write packet size, which is what
	// request_concurrency counts.
	packetBytes = 32 * 1024

	// maxInFlightPackets caps concurrency times request_concurrency. Each
	// in-flight packet holds a 32 KiB buffer, so this is a 32 MiB ceiling on
	// what the pipelining may cost in memory on a runner easySFTP does not
	// own.
	maxInFlightPackets = 1024

	// uploadRoundTrips is how many remote round-trips one upload costs beyond
	// its payload: open, write+close, chmod and rename.
	uploadRoundTrips = 4

	// uploadsInFlightPerConnection is how many upload chains one connection
	// overlaps usefully. Fitted from the sweeps: a single connection tops out
	// around 50 small files per second at a 13 ms RTT, which is a handful of
	// chains rather than the dozens the worker count would suggest.
	uploadsInFlightPerConnection = 4

	// probesInFlightPerConnection is the same number for bare metadata
	// round-trips (the skip stats of an overlay redeploy, deletes). They hold
	// no file handle and carry no payload, so one connection pipelines far
	// more of them: the stored redeploy sweep answers 500 stats in about
	// 340 ms over one connection at a 13 ms RTT.
	probesInFlightPerConnection = 16

	// assumedStreamBytesPerSecond is the per-connection throughput Plan
	// assumes while nothing has been measured yet. It is deliberately low.
	// The number is only used to ask "is this run long enough to be worth
	// another handshake", the answer is checked against the throughput the
	// run really achieves as soon as it has one (see Controller), and the
	// damage a wrong guess can do is bounded by MaxConnections handshakes.
	assumedStreamBytesPerSecond = 1 << 20 // 1 MiB/s

	// fallbackRTT and fallbackHandshake stand in when the probe could not
	// measure the link at all (a server that refuses SSH_FXP_REALPATH, say).
	// They describe an unremarkable internet path, so an unmeasurable link
	// lands on a middling choice instead of an extreme one.
	//
	// Deriving the RTT from the handshake instead was tried and dropped: the
	// ratio between the two is not a constant. The stored sweeps put an SSH
	// handshake at 25 to 39 round-trips on one server, where the textbook
	// answer is closer to six, so the derivation would have been a worse guess
	// than the guess it replaced.
	fallbackRTT       = 25 * time.Millisecond
	fallbackHandshake = 300 * time.Millisecond
)

// Workload is what a run knows about the work in front of it. It is filled in
// per deployment, from the plan, immediately before the transfer starts, so a
// sync deployment describes the files it actually changed rather than the tree
// it scanned.
type Workload struct {
	// Uploads is how many files will be sent, and UploadBytes their total
	// size. LargestUpload is the biggest single one, which is the only file
	// size request_concurrency can act on.
	Uploads       int
	UploadBytes   int64
	LargestUpload int64

	// Probes counts remote round-trips that are not uploads: the one stat per
	// file that advanced.skip_unchanged costs, and the removals of a delete
	// sweep. They are cheap individually and pipeline far better than an
	// upload does, which is why they are counted apart.
	Probes int

	// Unknown marks a workload whose upload set is not settled yet: an
	// overlay deployment with advanced.skip_unchanged decides file by file,
	// while it runs, whether a file is sent at all. Plan then sizes the run
	// for the stats it is sure of and leaves the rest to Controller, which
	// sees the real ratio within the first second of the transfer.
	Unknown bool
}

// Items is how many independent units of work the phase has, which is the
// largest number of workers that can ever be busy at once.
func (w Workload) Items() int {
	return max(w.Uploads, w.Probes)
}

// Empty reports whether there is nothing to do.
func (w Workload) Empty() bool { return w.Uploads == 0 && w.Probes == 0 }

// Link is what a run knows about the path to its server. RTT and Handshake are
// measured once, on the first connection; StreamBytesPerSecond is not
// measurable before the transfer and stays zero until the run has moved enough
// bytes to fill it in.
type Link struct {
	RTT       time.Duration
	Handshake time.Duration

	// StreamBytesPerSecond is what one connection has been observed to carry.
	// Zero means "not measured yet" and makes Plan fall back to the
	// conservative prior; see assumedStreamBytesPerSecond.
	StreamBytesPerSecond float64
}

// rtt is the measured RTT, or a plain-internet stand-in when the probe could
// not produce one. A link too fast for the platform clock is not this case:
// the probe grows its batch until the clock can resolve it, so a successful
// probe always reports something positive.
func (l Link) rtt() time.Duration {
	if l.RTT > 0 {
		return l.RTT
	}
	return fallbackRTT
}

// handshake is the measured cost of one extra connection, or a stand-in.
func (l Link) handshake() time.Duration {
	if l.Handshake > 0 {
		return l.Handshake
	}
	return fallbackHandshake
}

// streamBytesPerSecond is the measured per-connection throughput, or the prior.
func (l Link) streamBytesPerSecond() float64 {
	if l.StreamBytesPerSecond > 0 {
		return l.StreamBytesPerSecond
	}
	return assumedStreamBytesPerSecond
}

// Measured reports whether the link carries an observed throughput rather than
// the prior. Callers log the difference, because a decision taken on a guess
// and one taken on a measurement deserve different confidence.
func (l Link) Measured() bool { return l.StreamBytesPerSecond > 0 }

// Fixed holds the values the user pinned in the config file. Zero means the
// setting was left at "auto" and is the policy's to choose. Each field is
// independent: a run may pin connections and let the other two be chosen.
type Fixed struct {
	Connections        int
	Concurrency        int
	RequestConcurrency int
}

// Any reports whether the user pinned anything at all.
func (f Fixed) Any() bool {
	return f.Connections > 0 || f.Concurrency > 0 || f.RequestConcurrency > 0
}

// All reports whether every setting is pinned, in which case there is nothing
// to decide and no reason to probe the link.
func (f Fixed) All() bool {
	return f.Connections > 0 && f.Concurrency > 0 && f.RequestConcurrency > 0
}

// Settings is one resolved transport configuration.
type Settings struct {
	Connections        int
	Concurrency        int
	RequestConcurrency int
}

func (s Settings) String() string {
	return fmt.Sprintf("connections=%d concurrency=%d request_concurrency=%d",
		s.Connections, s.Concurrency, s.RequestConcurrency)
}

// Plan resolves every setting Fixed left at auto, honoring the ones it did
// not: a pinned value is passed through untouched and constrains the rest, so
// "connections: 2, concurrency: auto" chooses only the concurrency and does it
// knowing there are two connections.
func Plan(w Workload, l Link, f Fixed) Settings {
	s := Settings{
		Connections:        f.Connections,
		Concurrency:        f.Concurrency,
		RequestConcurrency: f.RequestConcurrency,
	}
	if s.Concurrency == 0 {
		s.Concurrency = planConcurrency(w)
	}
	if s.RequestConcurrency == 0 {
		s.RequestConcurrency = planRequestConcurrency(w, s.Concurrency)
	}
	if s.Connections == 0 {
		s.Connections = planConnections(w, l, s.Concurrency)
	}
	return s
}

// planConcurrency sizes the parallel workers to the work: as many as there are
// independent items, capped. A worker with nothing to do costs nothing (the
// upload loop simply never starts it), so there is no trade-off to make here
// and nothing for the runtime controller to discover later.
func planConcurrency(w Workload) int {
	items := w.Items()
	if items < 1 {
		return 1
	}
	return min(items, MaxConcurrency)
}

// planRequestConcurrency pipelines one file's writes as far as the file itself
// can use, then as far as the memory budget allows.
//
// The setting is pkg/sftp's MaxConcurrentRequestsPerFile: how many 32 KiB write
// packets of *one* file may be in flight. A 4 KiB file is a single packet and
// cannot use a second request no matter what the value says, so raising it for
// a tree of small files only reserves buffers. It is the large files that can
// use it, and on a high-latency link they need it: with 16 packets in flight a
// file moves at most 512 KiB per round-trip.
func planRequestConcurrency(w Workload, concurrency int) int {
	packets := 1
	if w.LargestUpload > 0 {
		packets = int(min((w.LargestUpload+packetBytes-1)/packetBytes, int64(MaxRequestConcurrency)))
	}
	want := max(packets, MinRequestConcurrency)

	// Never let concurrency times request_concurrency reserve more than the
	// in-flight budget, but never drop below the long-standing default doing
	// so: a run with 64 files in flight is a run of small files, where the
	// setting cannot do anything anyway.
	budget := max(maxInFlightPackets/max(concurrency, 1), MinRequestConcurrency)
	return min(want, budget, MaxRequestConcurrency)
}

// planConnections is the sqrt(W/H) rule from the package comment, clamped by
// everything that makes an extra connection pointless rather than merely
// unprofitable: there is no work for it (fewer files than connections), no
// worker to put on it (concurrency), and no policy room above MaxConnections.
func planConnections(w Workload, l Link, concurrency int) int {
	if w.Uploads <= 1 {
		// Only the per-file upload path spreads over the pool, so a
		// deployment that sends one file (or none) can never use a second
		// connection, however long it takes. This is the "one 32 MiB file"
		// case of the stored sweeps, where every extra connection is a
		// handshake for nothing.
		return 1
	}
	work := SingleConnectionEstimate(w, l, concurrency)
	handshake := l.handshake()
	if work <= 0 || handshake <= 0 {
		return 1
	}
	k := int(math.Round(math.Sqrt(float64(work) / float64(handshake))))
	return min(max(k, 1), MaxConnections, w.Uploads, concurrency)
}

// SingleConnectionEstimate is W: how long this workload would take over one
// connection at the given worker count. It is the only quantity the
// connections decision rests on, so it is exported for the log line and for
// the controller, which recomputes it for the work that is left.
//
// The three terms are the three things a transfer spends time on, and they are
// added rather than maximised: a small-file upload really does pay its
// round-trips and its bytes one after the other inside each chain.
func SingleConnectionEstimate(w Workload, l Link, concurrency int) time.Duration {
	rtt := l.rtt()
	workers := max(concurrency, 1)

	var total time.Duration
	if w.Uploads > 0 {
		chains := min(workers, uploadsInFlightPerConnection)
		total += time.Duration(w.Uploads) * uploadRoundTrips * rtt / time.Duration(chains)
	}
	if w.Probes > 0 {
		chains := min(workers, probesInFlightPerConnection)
		total += time.Duration(w.Probes) * rtt / time.Duration(chains)
	}
	if w.UploadBytes > 0 {
		total += time.Duration(float64(w.UploadBytes) / l.streamBytesPerSecond() * float64(time.Second))
	}
	return total
}

// Explain is the one line a run logs when it resolved something itself: what
// the policy saw and what it decided, in that order, so a surprising choice
// can be traced to the feature that caused it without rerunning anything.
func Explain(w Workload, l Link, f Fixed, s Settings) string {
	unknown := ""
	if w.Unknown {
		unknown = ", upload set not known yet"
	}
	rate := "assumed"
	if l.Measured() {
		rate = "measured"
	}
	return fmt.Sprintf(
		"files=%d bytes=%s largest=%s probes=%d%s rtt=%s handshake=%s throughput=%s %s/s -> %s%s",
		w.Uploads, humanBytes(w.UploadBytes), humanBytes(w.LargestUpload), w.Probes, unknown,
		roundMS(l.rtt()), roundMS(l.handshake()),
		rate, humanBytes(int64(l.streamBytesPerSecond())),
		s, pinned(f))
}

// pinned names the settings the user fixed, so a log line never reads as if
// easySFTP chose a number the workflow chose.
func pinned(f Fixed) string {
	var names []string
	if f.Connections > 0 {
		names = append(names, "connections")
	}
	if f.Concurrency > 0 {
		names = append(names, "concurrency")
	}
	if f.RequestConcurrency > 0 {
		names = append(names, "request_concurrency")
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return " (" + names[0] + " comes from the configuration, it was not chosen)"
	default:
		return " (" + strings.Join(names, ", ") + " come from the configuration, they were not chosen)"
	}
}

func roundMS(d time.Duration) string { return d.Round(time.Millisecond).String() }

// humanBytes renders IEC units the way the uploader's own log lines do. It is
// spelled out here rather than imported because autotune must not depend on
// the uploader: the dependency runs the other way.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
