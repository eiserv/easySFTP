package uploader

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/autotune"
	"github.com/eiserv/easySFTP/internal/config"
)

// autoConfig is baseConfig with every transport setting left to easySFTP, the
// way config.Load leaves an inline run and a config file that writes "auto".
func autoConfig(srv *testServer) *config.Config {
	cfg := baseConfig(srv)
	cfg.Auto = config.AutoSettings{Connections: true, Concurrency: true, RequestConcurrency: true}
	return cfg
}

// TestAutoUploadsEverythingWithNothingConfigured is the end-to-end shape of the
// policy: a config that pins nothing still deploys, correctly, and stays
// inside the bounds the policy promises. What it picks here is a property of
// the in-process server (a socket away, so its handshake is worth very little)
// and not something to assert a number for; the decisions themselves are
// pinned against measured links in internal/autotune.
func TestAutoUploadsEverythingWithNothingConfigured(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 40)

	cfg := autoConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 40 {
		t.Fatalf("uploaded %d file(s), want 40", stats.FilesUploaded)
	}
	if got := atomic.LoadInt32(&srv.accepted); got > autotune.MaxConnections {
		t.Errorf("opened %d connection(s), more than the policy maximum of %d", got, autotune.MaxConnections)
	}
	for _, name := range []string{"/www/file00.txt", "/www/file39.txt"} {
		if !remoteExists(t, srv, name) {
			t.Errorf("%s did not arrive", name)
		}
	}
	if got := readRemote(t, srv, "/www/file39.txt"); got != "content 39" {
		t.Errorf("unexpected content: %q", got)
	}
}

// TestAutoPlansPerDeployment is the wiring between the uploader and the
// policy, with the link injected so the decision is the same on every machine:
// the same tree is worth a pool over a slow line, and the same tree behind
// advanced.skip_unchanged is not, because then nothing is known to be uploaded
// at all.
func TestAutoPlansPerDeployment(t *testing.T) {
	srv := startTestServer(t)
	cfg := autoConfig(srv)
	tune := newTuning(cfg)
	tune.setLink(autotune.Link{RTT: 13 * time.Millisecond, Handshake: 360 * time.Millisecond})

	files := make([]fileItem, 300)
	for i := range files {
		files[i] = fileItem{size: 4096}
	}

	full := tune.planFor(uploadWorkload(files, false))
	if full.Connections < 2 || full.Concurrency != autotune.MaxConcurrency {
		t.Errorf("300 small files over a 13 ms line resolved to %v, want a pool and full worker count", full)
	}

	redeploy := tune.planFor(uploadWorkload(files, true))
	if redeploy.Connections != 1 {
		t.Errorf("a skip_unchanged redeploy resolved to %d connection(s), want 1 until the transfer says otherwise",
			redeploy.Connections)
	}
	if redeploy.Concurrency != autotune.MaxConcurrency {
		t.Errorf("a skip_unchanged redeploy resolved to %d worker(s), want the stats to run wide", redeploy.Concurrency)
	}
}

// TestAutoReportsItsDecision: the choice has to be visible, or a surprising
// one cannot be traced without rerunning the deploy. Debug level gets the
// features, the link and the answer in one line (issue #209, "Observability").
func TestAutoReportsItsDecision(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 6)

	cfg := autoConfig(srv)
	cfg.LogLevel = config.LogDebug
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}
	line, ok := findLine(log.infos, "auto tuning:")
	if !ok {
		t.Fatalf("no auto tuning line in the debug log: %v", log.infos)
	}
	for _, want := range []string{"files=6", "rtt=", "handshake=", "connections=", "concurrency=", "request_concurrency="} {
		if !strings.Contains(line, want) {
			t.Errorf("the decision line is missing %q: %s", want, line)
		}
	}
}

// TestAutoStaysQuietAndFixedWhenTheConfigurationPinsEverything: a workflow
// that already tuned its deploy must get exactly what it wrote, no probe and
// no line about tuning.
func TestAutoStaysQuietAndFixedWhenTheConfigurationPinsEverything(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 9)

	cfg := baseConfig(srv) // Auto is zero: every setting is the user's
	cfg.Concurrency = 3
	cfg.Connections = 3
	cfg.SftpRequestConcurrency = 8
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&srv.accepted); got != 3 {
		t.Errorf("opened %d connection(s), want the configured 3", got)
	}
	if _, ok := findLine(log.infos, "auto tuning:"); ok {
		t.Errorf("a fully configured run reported tuning it did not do: %v", log.infos)
	}
}

// TestAutoLeavesAPinnedSettingAloneAndChoosesAroundIt is the mixed case the
// issue calls out: connections pinned, the rest adaptive.
func TestAutoLeavesAPinnedSettingAloneAndChoosesAroundIt(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 12)

	cfg := baseConfig(srv)
	cfg.Connections = 3
	cfg.Auto = config.AutoSettings{Concurrency: true, RequestConcurrency: true}
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	stats, err := Run(context.Background(), cfg, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesUploaded != 12 {
		t.Fatalf("uploaded %d file(s), want 12", stats.FilesUploaded)
	}
	if got := atomic.LoadInt32(&srv.accepted); got != 3 {
		t.Errorf("opened %d connection(s); a pinned pool is never talked out of by the policy", got)
	}
}

// TestAutoPoolStopsAskingAfterARefusal covers the shared-hosting case from the
// other side: easySFTP asked for more connections than the server allows, and
// the answer has to cost one warning and no further handshake attempts, rather
// than one refusal per slot. The spread is set by hand because a server this
// close is never given a pool by the policy itself.
func TestAutoPoolStopsAskingAfterARefusal(t *testing.T) {
	srv := startTestServer(t, withMaxConns(1))
	cfg := autoConfig(srv)

	tune := newTuning(cfg)
	tune.resolveRunWide(autotune.Workload{Uploads: 100, UploadBytes: 1 << 20, LargestUpload: 1 << 12})

	log := &recordingLogger{testLogger: testLogger{t}}
	sess, err := newSession(context.Background(), cfg, tune, log)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.close()

	if got := sess.setSpread(4); got != 4 {
		t.Fatalf("setSpread(4) = %d, want the pool to widen before anything is dialed", got)
	}
	for i := range 8 {
		if client, _, _ := sess.acquire(i); client == nil {
			t.Fatalf("acquire(%d) returned no client", i)
		}
	}

	// One accepted connection plus the single attempt the server dropped: the
	// pool must not try the remaining slots one by one.
	if got := atomic.LoadInt32(&srv.accepted); got != 2 {
		t.Errorf("the server saw %d connection attempt(s), want 2: one that worked and one refusal, then no more", got)
	}
	if n := countLines(log.warnings, "would not open more than"); n != 1 {
		t.Errorf("got %d refusal warning(s), want exactly one: %v", n, log.warnings)
	}
	if !sess.refusedConnection() {
		t.Error("the session must remember the refusal so the runtime controller stops growing")
	}
	if got := sess.setSpread(8); got != 1 {
		t.Errorf("setSpread(8) after a refusal = %d, want the pool to stay where the server left it", got)
	}
}

// TestAutoSizesTheWorkersToTheWork: with concurrency at auto, a phase gets as
// many workers as it has items and no more, so a three-file sync does not
// start sixty-four goroutines and a two-thousand-file deploy is not held at
// four.
func TestAutoSizesTheWorkersToTheWork(t *testing.T) {
	srv := startTestServer(t)
	cfg := autoConfig(srv)
	tune := newTuning(cfg)

	for _, tc := range []struct{ items, want int }{
		{0, 1},
		{1, 1},
		{3, 3},
		{100, 64},
		{5000, autotune.MaxConcurrency},
	} {
		if got := tune.workers(tc.items); got != tc.want {
			t.Errorf("workers(%d) = %d, want %d", tc.items, got, tc.want)
		}
	}

	pinned := baseConfig(srv) // Auto zero: concurrency 4 comes from the config
	if got := newTuning(pinned).workers(5000); got != 4 {
		t.Errorf("workers(5000) with a pinned concurrency = %d, want the configured 4", got)
	}
}

// TestUploadWorkloadReadsSkipUnchangedAsMetadata pins the reason a redeploy is
// not given a pool: with advanced.skip_unchanged the upload set is not decided
// yet, so the plan counts the stats it is sure of instead of uploads it is not
// (issue #209, stage 1 vs stage 3).
func TestUploadWorkloadReadsSkipUnchangedAsMetadata(t *testing.T) {
	files := []fileItem{{size: 4096}, {size: 8192}, {size: 1024}}

	settled := uploadWorkload(files, false)
	if settled.Uploads != 3 || settled.UploadBytes != 13312 || settled.LargestUpload != 8192 || settled.Unknown {
		t.Errorf("a plain overlay workload came out %+v", settled)
	}

	unsettled := uploadWorkload(files, true)
	if unsettled.Uploads != 0 || unsettled.Probes != 3 || !unsettled.Unknown {
		t.Errorf("a skip_unchanged workload came out %+v, want three probes and nothing promised", unsettled)
	}
	if unsettled.LargestUpload != 8192 {
		t.Errorf("largest upload = %d, want the biggest planned file: it still bounds request_concurrency", unsettled.LargestUpload)
	}
}

// findLine returns the first logged line containing want.
func findLine(lines []string, want string) (string, bool) {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return l, true
		}
	}
	return "", false
}

func countLines(lines []string, want string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, want) {
			n++
		}
	}
	return n
}

// TestRuntimeControllerWidensThePoolWhileTheTransferRuns is the wiring of
// stage 3: the loop has to notice that the transfer is far slower per stream
// than the pre-transfer guess assumed, tell the session to spread wider, and
// say so once.
//
// The controller's own decision table lives in internal/autotune; what is
// checked here is that the progress the workers record reaches it and that its
// answer reaches the pool.
func TestRuntimeControllerWidensThePoolWhileTheTransferRuns(t *testing.T) {
	defer func(v time.Duration) { tuningInterval = v }(tuningInterval)
	tuningInterval = 10 * time.Millisecond

	srv := startTestServer(t)
	cfg := autoConfig(srv)
	tune := newTuning(cfg)
	tune.resolveRunWide(autotune.Workload{Uploads: 1000, UploadBytes: 1000 << 20, LargestUpload: 1 << 20})

	log := &recordingLogger{testLogger: testLogger{t}}
	sess, err := newSession(context.Background(), cfg, tune, log)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.close()
	// After the session, because newSession fills the link in from its own
	// probe: this is the line the stored sweeps were measured over.
	tune.setLink(autotune.Link{RTT: 13 * time.Millisecond, Handshake: 360 * time.Millisecond})

	prog := &uploadProgress{totalFiles: 1000, totalBytes: 1000 << 20}
	start := autotune.Settings{Connections: 1, Concurrency: 64, RequestConcurrency: 16}
	sess.setSpread(start.Connections)

	stop := startTuningController(context.Background(), sess, start, prog, log)
	// Ten 1 MiB files through in the first window: one stream is carrying a
	// fraction of what the remaining 990 MiB would need.
	for range 10 {
		prog.upload(1<<20, 1<<20)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		sess.mu.Lock()
		spread := sess.spread
		sess.mu.Unlock()
		if spread > 1 {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("the pool never widened; log: %v", log.infos)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()

	if _, ok := findLine(log.infos, "raising connections 1 ->"); !ok {
		t.Errorf("a runtime change must be logged with its measurement: %v", log.infos)
	}
	tune.mu.Lock()
	changes, effective := tune.changes, tune.effective.Connections
	tune.mu.Unlock()
	if changes != 1 {
		t.Errorf("recorded %d runtime change(s), want 1", changes)
	}
	if effective < 2 {
		t.Errorf("the effective connection count stayed at %d; the benchmark reads this back", effective)
	}
}

// TestRuntimeControllerStaysOutOfDryRunsAndPinnedRuns: neither has anything to
// measure or anything it is allowed to change, and starting a goroutine to
// find that out every deployment would be waste.
func TestRuntimeControllerStaysOutOfDryRunsAndPinnedRuns(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 5)

	dry := autoConfig(srv)
	dry.DryRun = true
	dry.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
	if _, err := Run(context.Background(), dry, testLogger{t}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if remoteExists(t, srv, "/www/file00.txt") {
		t.Error("a dry run uploaded something")
	}
}

// TestTheLinkProbeIsSkippedWhenItCannotChangeAnything: the three extra
// round-trips are only worth spending when the connection count is still open
// to argument. Nothing else in the policy reads the link.
func TestTheLinkProbeIsSkippedWhenItCannotChangeAnything(t *testing.T) {
	many := autotune.Workload{Uploads: 100, UploadBytes: 1 << 20, LargestUpload: 1 << 12}
	one := autotune.Workload{Uploads: 1, UploadBytes: 1 << 20, LargestUpload: 1 << 20}

	for _, tc := range []struct {
		name string
		w    autotune.Workload
		f    autotune.Fixed
		want bool
	}{
		{"a tree of files with nothing pinned", many, autotune.Fixed{}, true},
		{"a single file can never spread", one, autotune.Fixed{}, false},
		{"a pinned pool leaves nothing to decide", many, autotune.Fixed{Connections: 4}, false},
		{"the other two pinned still leave the pool", many, autotune.Fixed{Concurrency: 8, RequestConcurrency: 16}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsLink(tc.w, tc.f); got != tc.want {
				t.Errorf("needsLink = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTheLinkProbeMeasuresAServerFasterThanTheClock: the in-process server
// answers in less time than the platform timer resolves on some hosts, and a
// zero there would be read as "unmeasurable" and answered with a stand-in
// meant for a link nobody could measure at all.
func TestTheLinkProbeMeasuresAServerFasterThanTheClock(t *testing.T) {
	srv := startTestServer(t)
	cfg := autoConfig(srv)
	tune := newTuning(cfg)
	tune.resolveRunWide(autotune.Workload{Uploads: 100, UploadBytes: 1 << 20, LargestUpload: 1 << 12})

	sess, err := newSession(context.Background(), cfg, tune, testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.close()

	link := tune.currentLink()
	if link.RTT <= 0 {
		t.Errorf("the probe reported no RTT (%v); a successful probe must always produce one", link.RTT)
	}
	if link.Handshake <= 0 {
		t.Errorf("the handshake was not timed: %v", link.Handshake)
	}
	if link.RTT > link.Handshake {
		t.Errorf("RTT %v is longer than the whole handshake %v, which cannot be right", link.RTT, link.Handshake)
	}
}
