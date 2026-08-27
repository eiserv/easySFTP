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
//     how many files, how many bytes, how they are distributed (the median,
//     the ninetieth percentile and how many of them fit in a single write
//     packet), and how many pure metadata round-trips (skip stats, deletes)
//     come with them. Totals alone cannot tell a tree of 4 KiB files with one
//     archive in it from a tree of megabyte files, and the two want different
//     pipelining; the distribution can (issue #215, stage 1).
//  2. Link. The first connection measures its own handshake, and a few cheap
//     round-trips measure the RTT. Both go into Link and make the cost model
//     concrete: an extra connection costs one handshake, and a round-trip
//     costs one RTT.
//  3. Runtime. What the model cannot know before the transfer is the link's
//     throughput, so Plan starts from a deliberately conservative prior.
//     Controller replaces that prior with the throughput the run is actually
//     achieving and re-plans against the work that is left. A step that does
//     not pay for itself is taken back: the connections stay open, the files
//     that are left simply stop being handed to them. See controller.go.
//
// Stage 3 only reaches work that has not started. A worker takes its connection
// when it picks up its file and keeps it until that file is done, so a wider
// pool is offered to the queue behind the workers and never to a transfer
// already running. The worker count is the item count (capped), which means a
// deployment of at most MaxConcurrency files starts every one of them at once
// and has no queue at all: stage 1 is the whole decision there, and Controller
// stands down instead of moving a number nothing reads (issue #217).
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
	"slices"
	"strings"
	"time"
)

// PolicyVersion is the generation of this policy. Bump it whenever a change
// here makes an observation taken by an older easySFTP no longer comparable:
// a different definition of what counts as a measurement, a different meaning
// for one of the Workload features, a different cost model.
//
// It exists for internal/autocache, which stores measurements taken under this
// policy and must refuse the ones taken under another (issue #212). Adding a
// constant, refitting one against a new sweep or changing a clamp does not
// need a bump: those change what the policy decides, not what a stored
// measurement means.
const PolicyVersion = 1

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

	// bdpSafetyFactor is how many bandwidth-delay products of data a stream
	// is asked to keep in flight. One BDP is the textbook amount that fills a
	// pipe exactly and leaves nothing over for the server's own write latency,
	// for an ack that arrives late, or for an RTT that was measured on a good
	// moment. Four is the usual engineering answer and it is cheap to be
	// generous with here: the result is clamped by the file, by the memory
	// budget and by MaxRequestConcurrency anyway.
	bdpSafetyFactor = 4

	// tailShare is the share of the files in flight that the memory model
	// treats as possibly the big ones. The rest are bounded by the ninetieth
	// percentile, which is exactly what a percentile promises: no more than a
	// tenth of the files are above it. See inFlightPackets.
	tailShare = 0.1

	// assumedStreamBytesPerSecond is the per-connection throughput Plan
	// assumes while nothing has been measured yet.
	//
	// Fitted, like every other constant here, rather than guessed: it was set
	// to 1 MiB/s and called conservative, while the only link the project has
	// ever measured reports a single-stream control of 0.36 to 0.39 MiB/s,
	// stable across four releases (issue #230). W is divided by this number,
	// so a prior 2.8x too high understates the work, understates
	// k = sqrt(W/H), and opens fewer connections than the link would reward.
	//
	// Replaying the policy over every stored sweep, varying only this value:
	//
	//	assumed        mean regret   worst regret
	//	1 MiB/s              3.91%         24.40%
	//	0.75 MiB/s           3.62%         14.44%
	//	0.5 MiB/s            3.38%         14.44%
	//	0.375 MiB/s          3.36%         14.44%
	//	0.35 MiB/s           3.36%         14.44%
	//	0.25 MiB/s           3.42%         14.44%
	//	0.1875 MiB/s         4.61%         22.12%
	//	0.125 MiB/s          5.77%         32.08%
	//
	// The whole 0.25 to 0.75 MiB/s range holds the worst case inside the 15%
	// budget, so the choice is not balanced on one number; the flat optimum is
	// 0.35 to 0.45. This sits just under the lowest measured control, which is
	// the conservative side to be on: under-estimating throughput errs toward
	// one more connection, and the scaling data says that is the cheaper
	// direction to be wrong in.
	//
	// Caveat worth carrying with the number: one host, one 13 ms path, and
	// shaping.available is false in all 15 stored results. A prior fitted to a
	// single measured link beats one fitted to nothing and is not a general
	// answer.
	//
	// The number is only used to ask "is this run long enough to be worth
	// another handshake", the answer is checked against the throughput the
	// run really achieves as soon as it has one (see Controller), and the
	// damage a wrong guess can do is bounded by MaxConnections handshakes.
	assumedStreamBytesPerSecond = 360 << 10 // 0.35 MiB/s

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
	// size. LargestUpload is the biggest single one.
	Uploads       int
	UploadBytes   int64
	LargestUpload int64

	// P50Upload and P90Upload are the median and the ninetieth percentile of
	// the file sizes, and SmallUploads counts the files that fit in a single
	// SFTP write packet, which is the size below which no amount of
	// pipelining can do anything for a file.
	//
	// They are here so that two transfers with the same totals and the same
	// largest file stop looking alike: 99 medium files plus one archive and a
	// thousand tiny files plus a few archives cost very different amounts of
	// memory to pipeline, and only the distribution can tell them apart
	// (issue #215, stage 1). SummarizeUploads fills all three; a workload
	// built by hand may leave them at zero, and every reader below falls back
	// to LargestUpload when they are.
	P50Upload    int64
	P90Upload    int64
	SmallUploads int

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

// SummarizeUploads is how a caller turns the file sizes it is about to send
// into the upload side of a Workload: the totals, the largest file and the
// distribution features stage 1 of the policy reads.
//
// The sizes are only needed here. Nothing downstream keeps them, which is the
// point of reducing them to a handful of numbers: a deployment of a hundred
// thousand files is summarised once, in one pass plus a sort, and the policy
// then works on a plain struct. The caller's slice is not modified.
func SummarizeUploads(sizes []int64) Workload {
	var w Workload
	if len(sizes) == 0 {
		return w
	}
	sorted := make([]int64, 0, len(sizes))
	for _, size := range sizes {
		size = max(size, 0)
		sorted = append(sorted, size)
		w.Uploads++
		w.UploadBytes += size
		if size <= packetBytes {
			w.SmallUploads++
		}
	}
	slices.Sort(sorted)
	w.LargestUpload = sorted[len(sorted)-1]
	w.P50Upload = percentile(sorted, 0.5)
	w.P90Upload = percentile(sorted, 0.9)
	return w
}

// percentile is the nearest-rank percentile of an ascending slice: the
// smallest value at or above which the given share of the files sit. Exact,
// deterministic and without interpolation, because the readers of these
// numbers ask "how big is a file up here" rather than "what is the continuous
// quantile of the distribution".
func percentile(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(q * float64(len(sorted))))
	return sorted[min(max(rank, 1), len(sorted))-1]
}

// Items is how many independent units of work the phase has, which is the
// largest number of workers that can ever be busy at once.
func (w Workload) Items() int {
	return max(w.Uploads, w.Probes)
}

// Empty reports whether there is nothing to do.
func (w Workload) Empty() bool { return w.Uploads == 0 && w.Probes == 0 }

// Source says where a throughput number came from, because a decision taken on
// a guess and one taken on a measurement do not deserve the same confidence.
// New evidence always outranks old evidence: the runtime measurement of the
// current transfer wins over a cached one, which wins over the built-in prior.
type Source uint8

const (
	// SourcePrior is the built-in conservative guess: nothing about this link
	// has been observed. It is what the whole policy starts from.
	SourcePrior Source = iota
	// SourceCached is an observation carried over from an earlier run with a
	// similar workload and link (issue #212). easySFTP does not produce these
	// yet; Link accepts them so the policy that consumes one is written and
	// tested before the cache exists.
	SourceCached
	// SourceRuntime is what this transfer is achieving right now, which is the
	// only evidence that describes the run being tuned.
	SourceRuntime
)

func (s Source) String() string {
	switch s {
	case SourceCached:
		return "cached"
	case SourceRuntime:
		return "measured"
	default:
		return "assumed"
	}
}

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

	// ThroughputSource records where that number came from. It only means
	// anything while StreamBytesPerSecond is positive, and a caller that fills
	// in a throughput without saying where it found it is taken at its word:
	// an observation of the run in front of it.
	ThroughputSource Source
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
// the prior.
func (l Link) Measured() bool { return l.StreamBytesPerSecond > 0 }

// Source is where the throughput this link reports came from.
func (l Link) Source() Source {
	if !l.Measured() {
		return SourcePrior
	}
	if l.ThroughputSource == SourcePrior {
		// A throughput with no provenance is the caller's own measurement:
		// nothing else could have produced one.
		return SourceRuntime
	}
	return l.ThroughputSource
}

// BDPBytes is the bandwidth-delay product of this path: how many bytes one
// stream has to keep in flight for the pipe to stay full. It is what stage 3
// of the policy sizes request_concurrency against, and it is exported so the
// number behind a surprising choice can be read back out of the run's counters
// instead of recomputed by hand.
func (l Link) BDPBytes() int64 {
	return int64(l.streamBytesPerSecond() * l.rtt().Seconds())
}

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
		s.RequestConcurrency = planRequestConcurrency(w, l, s.Concurrency)
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
//
// Issue #215 asks whether an *active* worker is free too, since it holds a
// remote file handle, a slot in the server's request queue and its share of
// the in-flight buffers. It is not, and the answer is still this one: across
// the seven sweeps under benchmarks/matrix/ the fastest concurrency at the
// connection count the policy picks is 32 or 64 for every payload-heavy
// scenario, and the two cases that disagree (small at 4 connections, mixed at
// 8) disagree by two to fourteen per cent in opposite directions on grids
// whose own canary spread is around seven. There is no rule in that, and
// fitting one to it would be fitting noise. The other two costs the issue
// names are bounded elsewhere: the buffers by inFlightPackets, which is a real
// constraint and does now read the distribution, and the pool by
// planConnections, which is where the measured cost of over-provisioning
// actually lives.
func planConcurrency(w Workload) int {
	items := w.Items()
	if items < 1 {
		return 1
	}
	return min(items, MaxConcurrency)
}

// planRequestConcurrency sizes one file's pipeline: enough in-flight bytes to
// keep the path busy, never more than the file itself or the memory budget can
// use.
//
// The setting is pkg/sftp's MaxConcurrentRequestsPerFile: how many 32 KiB write
// packets of *one* file may be in flight. A 4 KiB file is a single packet and
// cannot use a second request no matter what the value says, so raising it for
// a tree of small files only reserves buffers. It is the large files that can
// use it, and how many of them they need is a property of the path rather than
// of the file: a stream keeps a pipe full by holding one bandwidth-delay
// product of data in it, so that (times a safety factor) is the target, and
// the file only ever caps it (issue #215, stage 3).
//
// The BDP is only spent when somebody measured the throughput. While the
// number is the built-in prior, a BDP computed from it would be a guess
// dressed as an argument, so the conservative pre-#215 rule stands: pipeline
// the file as far as it goes. That direction is the safe one. Under-pipelining
// a long fat path costs throughput on every large file, while over-pipelining a
// slow one costs buffers the budget below already bounds.
func planRequestConcurrency(w Workload, l Link, concurrency int) int {
	file := packetsFor(w.LargestUpload)
	want := max(file, MinRequestConcurrency)
	if l.Measured() {
		// A measured path may argue the file down as well as up: on a link
		// that carries 5 KiB per round-trip, holding two megabytes of one file
		// in flight buys nothing that the floor does not already buy.
		bdp := int(min((int64(bdpSafetyFactor)*l.BDPBytes()+packetBytes-1)/packetBytes, int64(MaxRequestConcurrency)))
		want = min(max(bdp, MinRequestConcurrency), max(file, MinRequestConcurrency))
	}
	want = quantize(want)

	// Never let the files in flight reserve more than the in-flight budget,
	// but never drop below the long-standing default doing so: at 64 workers
	// the floor and the budget meet exactly, so this can only ever bind
	// request_concurrency and never the worker count.
	for want > MinRequestConcurrency && inFlightPackets(w, concurrency, want) > maxInFlightPackets {
		want = quantize(want / 2)
	}
	return min(want, MaxRequestConcurrency)
}

// packetsFor is how many write packets a file of this size occupies, capped at
// the most one file may have in flight. Zero and negative sizes are one packet:
// every file costs at least one write.
func packetsFor(size int64) int {
	if size <= 0 {
		return 1
	}
	return int(min((size+packetBytes-1)/packetBytes, int64(MaxRequestConcurrency)))
}

// quantize snaps a request count to a power of two. The value is a pipeline
// depth, not a measurement: 18 and 16 cannot behave differently, and a policy
// that reports 18 invites the reader to believe the second digit means
// something. Snapping down also keeps the choice on the coordinates the stored
// sweeps actually measured, which is what makes the replay in regret_test.go
// able to score it.
func quantize(n int) int {
	if n < 1 {
		return 1
	}
	p := 1
	for p*2 <= n {
		p *= 2
	}
	return p
}

// inFlightPackets is what a transfer holds in write buffers at once, in 32 KiB
// packets. It is an upper bound rather than an estimate, and the distribution
// is what makes it one worth having.
//
// Before issue #215 the bound was concurrency times request_concurrency: every
// file in flight assumed to be large enough to use the whole pipeline. For a
// deployment of 4 KiB files with a couple of archives in it that is off by
// more than an order of magnitude, and it was the reason the stored 'mixed'
// sweep ran with a request_concurrency of 18: 56 workers divided into the
// budget, as if all 56 files were the 2 MiB one.
//
// With a percentile the same bound gets much tighter without getting wrong. At
// most a tenth of the files are above P90Upload, so a tenth of the workers may
// be holding a full pipeline each and the rest are holding no more than the
// percentile needs.
func inFlightPackets(w Workload, concurrency, requests int) int {
	workers := float64(max(concurrency, 1))
	tail := workers * tailShare
	body := workers - tail

	// Without a distribution (a workload built by hand, or the controller's
	// extrapolation of what is left) the largest file is all there is to go
	// on, which is the pre-#215 bound.
	perFile := float64(min(packetsFor(w.LargestUpload), requests))
	if w.P90Upload > 0 {
		perFile = float64(min(packetsFor(w.P90Upload), requests))
	}
	return int(math.Ceil(body*perFile + tail*float64(requests)))
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
	return fmt.Sprintf(
		"files=%d bytes=%s p50=%s p90=%s small=%d largest=%s probes=%d%s rtt=%s handshake=%s throughput=%s %s/s bdp=%s -> %s%s",
		w.Uploads, humanBytes(w.UploadBytes), humanBytes(w.P50Upload), humanBytes(w.P90Upload),
		w.SmallUploads, humanBytes(w.LargestUpload), w.Probes, unknown,
		roundMS(l.rtt()), roundMS(l.handshake()),
		l.Source(), humanBytes(int64(l.streamBytesPerSecond())), humanBytes(l.BDPBytes()),
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
