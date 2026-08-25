package uploader

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"

	"github.com/eiserv/easySFTP/internal/autocache"
	"github.com/eiserv/easySFTP/internal/autotune"
	"github.com/eiserv/easySFTP/internal/config"
	"github.com/eiserv/easySFTP/internal/metrics"
)

// tuning is the run's side of internal/autotune: it holds what the policy was
// told (the pinned settings and the measured link), what it decided, and the
// bookkeeping the log lines and the benchmark counters read back.
//
// The policy itself has no state and no clock; everything mutable about it
// lives here, behind one mutex, because the runtime controller changes the
// connection count while the upload workers are reading it.
type tuning struct {
	// fixed is written once, before anything else runs, and read without the
	// lock from both the upload workers and the controller goroutine. It must
	// stay immutable after newTuning for that to be safe.
	fixed autotune.Fixed

	mu   sync.Mutex
	link autotune.Link
	// requests is resolved once, before the first connection: it is
	// pkg/sftp's MaxConcurrentRequestsPerFile, which is fixed when a client
	// is created, so it cannot follow a deployment the way the other two do.
	requests int

	// probe is set by resolveRunWide when measuring the link could still
	// change an answer; see needsLink.
	probe bool

	// initial is what the policy chose for the first deployment's transfer,
	// before that transfer taught it anything, seen the features behind that
	// choice, and effective the largest the run ended up using. The benchmark
	// reads all three back out of the counters, so a policy run can be scored
	// against the grid it is measured next to and a runtime change is visible
	// as the gap between initial and effective.
	initial     autotune.Settings
	seen        autotune.Workload
	haveInitial bool
	effective   autotune.Settings
	changes     int

	// spreadUp and spreadDown count the runtime changes by direction, and
	// finalSpread is the number of connections the last deployment ended up
	// handing files to. effective stays a high-water mark (it answers "what did
	// this run cost the server"); these answer "where did the policy settle",
	// which is a different question once a step can be taken back
	// (issue #215).
	spreadUp    int
	spreadDown  int
	finalSpread int

	// ceiling is an upper bound on the pool that came from an earlier run
	// against this server: it refused a connection, or a growth step
	// measurably did not pay. Zero is the normal case and means the policy is
	// free up to its own maximum. capped latches when the bound actually held
	// something back, which is what tells the write-back that this run did
	// not put the limit to the test (issue #212, and see
	// autocache.MaxCeilingCarry).
	ceiling int
	capped  bool

	// observed is what this run's own transfers measured about the link, per
	// connection, and observedBytes the size of the transfer that produced it.
	// The largest transfer wins: both are measurements, and the one with more
	// bytes behind it is the better one.
	//
	// It is deliberately not folded back into link. Within a deployment the
	// runtime controller already acts on its own measurement, and re-planning
	// the *next* deployment of the same run against a number the first one
	// produced would change what a multi-deployment run does today for
	// reasons that belong to issue #209, not here. What this is for is the
	// next run.
	observed      float64
	observedBytes int64
}

// newTuning reads which settings the configuration pinned. Everything it does
// not pin is the policy's.
func newTuning(cfg *config.Config) *tuning {
	t := &tuning{}
	if !cfg.Auto.Connections {
		t.fixed.Connections = max(cfg.Connections, 1)
	}
	if !cfg.Auto.Concurrency {
		t.fixed.Concurrency = max(cfg.Concurrency, 1)
	}
	if !cfg.Auto.RequestConcurrency {
		t.fixed.RequestConcurrency = max(cfg.SftpRequestConcurrency, 1)
	}
	t.requests = t.fixed.RequestConcurrency
	// What a run that fails before it plans anything used: one connection and
	// one worker. Left at zero, report() would tell the benchmark the run ran
	// with no settings at all.
	t.effective = autotune.Settings{Connections: 1, Concurrency: 1, RequestConcurrency: max(t.requests, 1)}
	return t
}

// adaptive reports whether the policy has anything to decide at all. A run
// that pinned all three never starts a controller and never logs a decision.
func (t *tuning) adaptive() bool { return !t.fixed.All() }

// wantsLinkProbe reports whether the three extra round-trips of the link probe
// could still change an answer.
func (t *tuning) wantsLinkProbe() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.probe
}

// needsLink reports whether the link measurement can affect anything.
//
// Only the connection count reads it: the worker count follows the item count
// and request_concurrency follows the file size, neither of which the path to
// the server has an opinion about. So the probe is worth its round-trips
// exactly when the pool is still open to argument, and is skipped for a run
// that pinned advanced.connections or that sends at most one file (which can
// never spread over a pool, however slow the link).
func needsLink(w autotune.Workload, f autotune.Fixed) bool {
	return f.Connections == 0 && w.Uploads > 1
}

// resolveRunWide fixes request_concurrency and the pool's ceiling before the
// first connection, from the whole run's plan.
//
// It has to happen here rather than per deployment because both are properties
// of a connection, not of a transfer: request_concurrency is baked into the
// SFTP client at creation, and the pool is a slice the session allocates once.
// Both are ceilings, so taking the run-wide plan (every deployment's planned
// files, before sync narrows anything down) is the safe direction: a
// deployment may use less of them, never more.
func (t *tuning) resolveRunWide(w autotune.Workload) autotune.Settings {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := autotune.Plan(w, t.link, t.fixed)
	t.requests = s.RequestConcurrency
	t.probe = needsLink(w, t.fixed)
	// The other two are per deployment and recorded when they are decided;
	// what a run that never reaches a transfer used is one connection and one
	// worker, which is what it opened.
	t.effective = autotune.Settings{Connections: 1, Concurrency: 1, RequestConcurrency: s.RequestConcurrency}
	return s
}

// requestConcurrency is what a new SFTP client is opened with.
func (t *tuning) requestConcurrency() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.requests < 1 {
		return 1
	}
	return t.requests
}

// setLink records what the first connection measured about the path to the
// server: stage 2 of the policy.
func (t *tuning) setLink(l autotune.Link) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.link = l
}

func (t *tuning) currentLink() autotune.Link {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.link
}

// applyCache hands the policy what an earlier run against this server
// measured, once the link probe has confirmed the record still describes this
// path (see autoCache.confirm).
//
// Only two things come out of a record, and neither is a setting. The
// throughput fills the one input the policy cannot compute for itself, tagged
// as cached so that a runtime measurement of this transfer still outranks it.
// The ceiling bounds the pool at whatever the server was last seen to allow or
// to benefit from, which the cost model cannot derive because it assumes
// perfect scaling. Everything else is planned from scratch, on this run's
// files, which is what keeps a growing dataset from inheriting an old
// deployment's answer (issue #212).
func (t *tuning) applyCache(d autocache.Decision) {
	if !d.Restores() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if d.StreamBytesPerSecond > 0 {
		t.link.StreamBytesPerSecond = d.StreamBytesPerSecond
		t.link.ThroughputSource = autotune.SourceCached
	}
	t.ceiling = d.ConnectionCeiling
}

// clampConnections applies the inherited ceiling to a connection count, and
// notes when it actually held one back. Every path that decides how many
// connections files go over runs through here: the per-deployment plan and,
// via session.setSpread, the runtime controller.
func (t *tuning) clampConnections(n int) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.clampLocked(n)
}

// clampLocked is clampConnections with the lock already held.
//
// A number the user wrote is never clamped. The cache exists to fill in what
// easySFTP would otherwise have to guess, and advanced.connections is not a
// guess: explicit configuration wins over anything remembered, which is the
// first thing issue #212 asks for.
func (t *tuning) clampLocked(n int) int {
	if t.fixed.Connections > 0 || t.ceiling <= 0 || n <= t.ceiling {
		return n
	}
	t.capped = true
	return t.ceiling
}

// observe folds one finished transfer into what this run knows about the link.
// A transfer that is not a measurement (too short, too few bytes, or files too
// small for a byte rate to be a bandwidth) is not one, and autotune.Measure is
// the single place that judgement is made.
func (t *tuning) observe(w autotune.Workload, bytes int64, window time.Duration, streams int) {
	rate, ok := autotune.Measure(w, bytes, window, streams)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if bytes <= t.observedBytes {
		return
	}
	t.observed, t.observedBytes = rate, bytes
}

// cacheObservation is what this run has to tell the next one: the shape it
// deployed, what it measured about the path, what it ran with, and any bound
// the server put on the pool.
//
// The ceiling is worked out here because this is the only place that can see
// all three of its causes at once. A refusal is the server saying no outright,
// and the number it granted is the answer. A step the runtime controller took
// back is the server saying no in slower words: the growth was measured and it
// did not pay. A run that was simply held at a ceiling it inherited learned
// neither, and says so, so that the bound is carried forward as untested
// rather than as fresh evidence.
func (t *tuning) cacheObservation(runWork autotune.Workload, granted int, refused bool) autocache.Observation {
	t.mu.Lock()
	defer t.mu.Unlock()
	link := t.link
	link.StreamBytesPerSecond = t.observed
	obs := autocache.Observation{
		Workload: autocache.WorkloadOf(runWork),
		Link:     autocache.LinkOf(link),
		Settings: autocache.SettingsOf(t.effective),
		Measured: t.observed > 0,
	}
	switch {
	case refused:
		obs.ConnectionCeiling = max(granted, 1)
	case t.spreadDown > 0:
		obs.ConnectionCeiling = max(t.finalSpread, 1)
	case t.capped:
		obs.CeilingUntested = true
	}
	return obs
}

// planFor resolves one deployment's transfer against the work it really has.
// For sync that is the changed set the manifest just produced, not the tree
// that was scanned.
func (t *tuning) planFor(w autotune.Workload) autotune.Settings {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := autotune.Plan(w, t.link, t.fixed)
	s.RequestConcurrency = t.requests // fixed with the connection; see resolveRunWide
	s.Connections = t.clampLocked(s.Connections)
	if !t.haveInitial {
		// The first transfer's choice is the one worth reporting as "what the
		// policy picked": it is the decision taken purely from the plan and
		// the link, with nothing measured yet. Its features go with it, so a
		// stored benchmark result explains the choice as well as recording it.
		t.initial, t.seen, t.haveInitial = s, w, true
	}
	t.finalSpread = s.Connections
	t.record(s)
	return s
}

// record keeps the high-water mark of what the run actually ran with.
func (t *tuning) record(s autotune.Settings) {
	t.effective.Connections = max(t.effective.Connections, s.Connections)
	t.effective.Concurrency = max(t.effective.Concurrency, s.Concurrency)
	t.effective.RequestConcurrency = max(t.effective.RequestConcurrency, s.RequestConcurrency)
}

// applied notes one accepted runtime change, in the direction it went. A step
// back does not un-count the handshakes the step before it paid for, so
// effective keeps its high-water mark either way.
func (t *tuning) applied(s autotune.Settings, shrink bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.changes++
	if shrink {
		t.spreadDown++
	} else {
		t.spreadUp++
	}
	t.finalSpread = s.Connections
	t.record(s)
}

// poolCeiling is how many connection slots the session allocates. Slots are
// dialed on first use, so an unused one costs a nil pointer and nothing else;
// what this number really bounds is how far the runtime controller may grow.
func (t *tuning) poolCeiling() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fixed.Connections > 0 {
		return t.fixed.Connections
	}
	return autotune.MaxConnections
}

// autoConnections reports whether easySFTP chose the connection count, which
// is what decides how a refused connection is phrased and whether the run
// stops asking for more.
func (t *tuning) autoConnections() bool { return t.fixed.Connections == 0 }

// workers is how many of items may be in flight in one phase. A pinned
// concurrency is used verbatim; otherwise it is the item count, capped, which
// is the largest number of workers that can ever be busy.
func (t *tuning) workers(items int) int {
	if t.fixed.Concurrency > 0 {
		return t.fixed.Concurrency
	}
	if items < 1 {
		return 1
	}
	return min(items, autotune.MaxConcurrency)
}

// report writes the policy's inputs and its two answers into the run counters.
// The benchmark reads them back to score auto against the grid it was measured
// next to (benchmarks/README.md), and they are the only record of a decision
// that is otherwise invisible in a non-debug log.
func (t *tuning) report() {
	t.mu.Lock()
	defer t.mu.Unlock()
	metrics.Set("config_connections", int64(t.effective.Connections))
	metrics.Set("config_concurrency", int64(t.effective.Concurrency))
	metrics.Set("config_request_concurrency", int64(t.effective.RequestConcurrency))
	metrics.Set("auto_initial_connections", int64(t.initial.Connections))
	metrics.Set("auto_initial_concurrency", int64(t.initial.Concurrency))
	metrics.Set("auto_initial_request_concurrency", int64(t.initial.RequestConcurrency))
	metrics.Set("auto_changes", int64(t.changes))
	// Direction and destination of those changes: a run that grew and then
	// took the step back is not a run that never moved, and the difference is
	// invisible in a single counter (issue #215, stages 5 and 6).
	metrics.Set("auto_spread_increases", int64(t.spreadUp))
	metrics.Set("auto_spread_decreases", int64(t.spreadDown))
	metrics.Set("auto_final_connections", int64(max(t.finalSpread, 1)))
	metrics.Set("workload_files", int64(t.seen.Uploads))
	metrics.Set("workload_bytes", t.seen.UploadBytes)
	metrics.Set("workload_largest_bytes", t.seen.LargestUpload)
	metrics.Set("workload_p50_bytes", t.seen.P50Upload)
	metrics.Set("workload_p90_bytes", t.seen.P90Upload)
	metrics.Set("workload_small_files", int64(t.seen.SmallUploads))
	metrics.Set("workload_probes", int64(t.seen.Probes))
	metrics.Set("link_rtt_us", t.link.RTT.Microseconds())
	metrics.Set("link_handshake_us", t.link.Handshake.Microseconds())
	// The throughput the pipelining was sized against and the bandwidth-delay
	// product that follows from it. A zero throughput is the normal case and
	// says so explicitly: nothing observes one before the first byte moves, so
	// the run used the built-in prior and link_bdp_bytes is what that prior
	// works out to over the measured RTT.
	metrics.Set("link_stream_bytes_per_second", int64(t.link.StreamBytesPerSecond))
	metrics.Set("link_bdp_bytes", t.link.BDPBytes())
	// What this run's own transfers turned out to carry, per connection. It is
	// a different number from the one above: that is what the pipelining was
	// sized against (a prior, or something remembered), this is what actually
	// happened, and it is zero for a deployment no byte rate could be read
	// from (see autotune.Measure).
	metrics.Set("link_observed_stream_bytes_per_second", int64(t.observed))
}

// planWorkload turns a set of planned files into the features the policy
// reads: the totals and the size distribution stage 1 works on. probes counts
// the remote round-trips that are not uploads (the one stat per file
// advanced.skip_unchanged costs) and unknown marks a set whose members may
// turn out not to be uploaded at all; both are the caller's to say, since
// neither is visible in a list of files.
func planWorkload(files []fileItem, probes int, unknown bool) autotune.Workload {
	w := autotune.SummarizeUploads(uploadSizes(files))
	w.Probes, w.Unknown = probes, unknown
	return w
}

// uploadSizes is the one slice the summary needs. It lives exactly as long as
// the call: what the policy keeps afterwards is the handful of numbers
// autotune.SummarizeUploads reduces it to.
func uploadSizes(files []fileItem) []int64 {
	sizes := make([]int64, 0, len(files))
	for _, f := range files {
		sizes = append(sizes, f.size)
	}
	return sizes
}

// runWorkload is every deployment's plan added together: the ceiling the run
// is sized against before it knows what sync will narrow down. Deployments run
// one after another, so their handshakes are shared and adding them up is what
// the pool actually faces.
//
// The distribution is taken over the union rather than per deployment, for the
// same reason: the connections and the pipelining depth are run-wide, so what
// they have to serve is every file the run may send.
func runWorkload(plans []plan) autotune.Workload {
	var sizes []int64
	for _, p := range plans {
		sizes = append(sizes, uploadSizes(p.files)...)
	}
	return autotune.SummarizeUploads(sizes)
}

// uploadWorkload is the workload of one deployment's transfer phase.
//
// With advanced.skip_unchanged the upload set is not decided yet: every
// planned file costs a stat and only some of them are then sent. The workload
// says so (Unknown) and counts the stats it is certain of rather than uploads
// it is not, which is what keeps a redeploy that changed three files from
// paying for a pool sized for the whole tree. The controller fills the rest in
// once the run has seen the real ratio.
func uploadWorkload(files []fileItem, skipUnchanged bool) autotune.Workload {
	if skipUnchanged {
		// The sizes are the sizes of files that may or may not be sent, so
		// they describe the shape of the work without claiming any of it will
		// happen: the counts stay on the probe side and the byte total stays
		// at zero.
		shape := autotune.SummarizeUploads(uploadSizes(files))
		return autotune.Workload{
			Probes:        len(files),
			Unknown:       true,
			LargestUpload: shape.LargestUpload,
			P50Upload:     shape.P50Upload,
			P90Upload:     shape.P90Upload,
			SmallUploads:  shape.SmallUploads,
		}
	}
	return planWorkload(files, 0, false)
}

// logTuning reports what the policy decided for one deployment.
//
// Two levels, because two readers want different things. A debug run gets the
// whole decision (every feature, the measured link and the answer), which is
// what makes a surprising choice traceable without rerunning anything. A
// normal run only hears about it when the run is about to use more than one
// connection, which is the one part of the decision a user can feel: it is
// visible on the server, it is what a MaxStartups limit reacts to, and it
// replaces the fixed "uploads may use up to N connections" line advanced.
// connections used to print.
func logTuning(cfg *config.Config, sess *session, files []fileItem, skipUnchanged bool, s autotune.Settings, log Logger) {
	if !sess.tune.adaptive() || len(files) == 0 {
		return
	}
	if cfg.Debug() {
		w := uploadWorkload(files, skipUnchanged)
		log.Infof("auto tuning: %s", autotune.Explain(w, sess.tune.currentLink(), sess.tune.fixed, s))
		return
	}
	if s.Connections > 1 {
		log.Infof("auto tuning: spreading %d file(s) over up to %d connections, %d at a time",
			len(files), s.Connections, s.Concurrency)
	}
}

// How many round-trips the RTT is taken over. Three is enough for a median to
// survive one scheduling hiccup and costs about as much as a single stat, and
// is what a link with any latency at all needs.
//
// The batch grows past that only when the median still reads as zero, which
// means the answers are arriving faster than the platform clock ticks (Go's
// clock on Windows resolves to about a millisecond, and a loopback or LAN
// server beats that). Thirty-two of the cheapest request in the protocol still
// costs a couple of milliseconds on such a link, and it turns "too fast to
// time" into a number instead of into a fallback meant for a link nobody could
// measure.
const (
	linkProbeSamples    = 3
	linkProbeMaxSamples = 32
)

// probeLink measures the path to the server: the handshake that has just been
// paid, and the round-trip time of a few of the cheapest requests the protocol
// has. SSH_FXP_REALPATH of "." reads nothing, writes nothing and is answered by
// every server that speaks SFTP at all, so the probe leaves no trace on the
// remote side (issue #209, stage 2).
//
// Two things can leave the RTT at zero: a server that refuses the request, and
// a link that answers faster than the platform's clock resolution (a loopback
// or LAN server on Windows does). Both are handed on as "unmeasured", and the
// policy then reads the handshake as a number of round-trips instead, which is
// consistent with whichever of the two happened.
func probeLink(client *sftp.Client, handshake time.Duration) autotune.Link {
	samples := make([]time.Duration, 0, linkProbeMaxSamples)
	batch := time.Now()
	for len(samples) < linkProbeMaxSamples {
		start := time.Now()
		done := metrics.Op("sftp_realpath")
		_, err := client.Getwd()
		done(err)
		if err != nil {
			return autotune.Link{Handshake: handshake}
		}
		samples = append(samples, time.Since(start))
		if len(samples) >= linkProbeSamples && medianDuration(samples) > 0 {
			break
		}
	}
	rtt := medianDuration(samples)
	if rtt == 0 {
		// Every single answer was below the clock's resolution; the batch as a
		// whole is not, so it is the measurement.
		rtt = time.Since(batch) / time.Duration(len(samples))
	}
	return autotune.Link{RTT: rtt, Handshake: handshake}
}

// medianDuration sorts a copy of samples and returns the middle one.
func medianDuration(samples []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// tuningInterval is how often the runtime controller looks at the transfer.
// The controller ignores anything shorter than its own observation window, so
// this only sets how promptly a decision can be taken once that window has
// passed; a tick costs a handful of atomic loads. A variable only so tests can
// drive it faster than a real deploy would.
var tuningInterval = 250 * time.Millisecond

// uploadProgress is what one deployment's workers report while they run, and
// the only input stage 3 of the policy has. Every field is written from the
// upload workers and read from the controller goroutine, so all of it is
// atomic; a nil *uploadProgress is a run with the controller switched off and
// every method below is a no-op on it.
type uploadProgress struct {
	totalFiles int
	totalBytes int64

	uploaded  atomic.Int64
	skipped   atomic.Int64
	bytesSent atomic.Int64 // bytes that really went over the wire
	bytesDone atomic.Int64 // planned size of every finished file, sent or skipped
	failures  atomic.Int64
}

func newUploadProgress(files []fileItem) *uploadProgress {
	p := &uploadProgress{totalFiles: len(files)}
	for _, f := range files {
		p.totalBytes += f.size
	}
	return p
}

// upload records one finished transfer: sent is what went over the wire,
// planned what the plan expected it to be.
func (p *uploadProgress) upload(sent, planned int64) {
	if p == nil {
		return
	}
	p.uploaded.Add(1)
	p.bytesSent.Add(sent)
	p.bytesDone.Add(planned)
}

// skip records one file the run did not have to send.
func (p *uploadProgress) skip(planned int64) {
	if p == nil {
		return
	}
	p.skipped.Add(1)
	p.bytesDone.Add(planned)
}

// failed records one retried upload. A transfer that is already retrying is
// not one to put more load on.
func (p *uploadProgress) failed() {
	if p == nil {
		return
	}
	p.failures.Add(1)
}

func (p *uploadProgress) snapshot(elapsed time.Duration, refused bool) autotune.Progress {
	uploaded := int(p.uploaded.Load())
	skipped := int(p.skipped.Load())
	return autotune.Progress{
		Elapsed:        elapsed,
		Uploaded:       uploaded,
		Skipped:        skipped,
		Remaining:      max(p.totalFiles-uploaded-skipped, 0),
		RemainingBytes: max(p.totalBytes-p.bytesDone.Load(), 0),
		UploadedBytes:  p.bytesSent.Load(),
		Refused:        refused,
		Failures:       int(p.failures.Load()),
	}
}

// startTuningController runs stage 3 for one deployment: it watches the
// transfer and widens the connection pool while the work that is left still
// justifies another handshake, then narrows it again if that turned out not to
// help. The returned function stops the watcher and must be called before the
// phase ends.
//
// For many deployments there is nothing to watch, and the controller says so
// before a goroutine is started: the settings are pinned, the pool is already
// at its ceiling, or the deployment hands every file to a connection the
// moment it starts (issue #217, and see autotune.NewController).
//
// Only the pool moves; see the comment on autotune.Controller for why the
// other two settings are already where they belong. Both directions go through
// session.setSpread, which points the next files at a different number of
// slots without dialing or closing anything itself, so a decision that turns
// out to be unnecessary (the phase ends first, the step is taken back) costs
// nothing at all.
//
// The workload goes in as well as the settings: what the controller may
// conclude from the bytes it sees depends on what kind of files produced
// them (autotune.Controller.bandwidthBound).
func startTuningController(ctx context.Context, sess *session, w autotune.Workload, start autotune.Settings, prog *uploadProgress, log Logger) func() {
	ctrl := autotune.NewController(w, start, sess.tune.currentLink(), sess.tune.fixed)
	if stopped, why := ctrl.Stopped(); stopped {
		// A stage that never runs and a stage that ran and changed nothing
		// leave the same counters behind, so the difference has to be said out
		// loud somewhere. Debug level, because the answer is "the plan was the
		// whole decision" and that is what the plan line above already says
		// (issue #217).
		if why != "" && sess.cfg.Debug() {
			log.Infof("auto tuning: no runtime changes this deployment (%s)", why)
		}
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		began := time.Now()
		t := time.NewTicker(tuningInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
			}
			change, ok := ctrl.Observe(prog.snapshot(time.Since(began), sess.refusedConnection()))
			if ok {
				// setSpread only ever points the *next* files somewhere else.
				// Nothing in flight moves, and a narrowing spread leaves every
				// connection it stops using open (issue #215, stage 5).
				shrink := change.Shrinks()
				change.To.Connections = sess.setSpread(change.To.Connections)
				sess.tune.applied(change.To, shrink)
				log.Infof("auto tuning: %s", change)
			}
			if stopped, why := ctrl.Stopped(); stopped {
				if why != "" && sess.cfg.Debug() {
					log.Infof("auto tuning: no further changes this deployment (%s)", why)
				}
				return
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}
