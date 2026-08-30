package uploader

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/autocache"
	"github.com/eiserv/easySFTP/internal/autotune"
	"github.com/eiserv/easySFTP/internal/config"
)

// readCache loads the cache file a run left behind and fails the test if there
// is not exactly one record in it.
func readCache(t *testing.T, path string) autocache.Record {
	t.Helper()
	store, err := autocache.Load(path)
	if err != nil {
		t.Fatalf("reading the cache at %s: %v", path, err)
	}
	if len(store.Records) != 1 {
		t.Fatalf("the cache holds %d record(s), want 1", len(store.Records))
	}
	return store.Records[0]
}

// TestAutoCacheRecordsWhatARunMeasured is the end-to-end wiring: a run with
// advanced.auto_cache set writes what it learned about the server, and a
// second run against the same server reads it back rather than starting a new
// one.
//
// What it cannot assert is a throughput. The in-process server is a socket
// away, so no upload phase here is long enough or large enough to be a
// measurement of a link (autotune.Measure says so, and that is the same rule
// the controller applies to its windows). What is asserted is everything a run
// against a real server would build on: the record exists, it describes this
// target and this deploy, and it carries the link the probe measured.
func TestAutoCacheRecordsWhatARunMeasured(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 40)
	path := filepath.Join(t.TempDir(), "cache", "auto.json")

	cfg := autoConfig(srv)
	cfg.AutoCachePath = path
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	rec := readCache(t, path)
	switch {
	case rec.Target != autocache.Fingerprint(targetIdentity(cfg)):
		t.Errorf("the record is filed under %q, not under this run's target", rec.Target)
	case rec.PolicyVersion != autotune.PolicyVersion:
		t.Errorf("policy version %d, want %d", rec.PolicyVersion, autotune.PolicyVersion)
	case rec.Workload.Files != 40:
		t.Errorf("the anchor describes %d file(s), want the 40 this run deployed", rec.Workload.Files)
	case rec.Link.HandshakeMillis <= 0:
		t.Error("the handshake this run paid was not recorded")
	case rec.Link.RTTMillis <= 0:
		t.Error("the round-trip time the probe measured was not recorded")
	case rec.Settings.Connections < 1 || rec.Settings.Concurrency < 1:
		t.Errorf("the settings this run used were not recorded: %+v", rec.Settings)
	}

	// A second run against the same server updates the one record rather than
	// filing another.
	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}
	readCache(t, path)
}

// TestAutoCacheIsOffWithoutAPath: the default is no cache at all, and a run
// without one must not create files anywhere.
func TestAutoCacheIsOffWithoutAPath(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 4)
	dir := t.TempDir()

	cfg := autoConfig(srv)
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}
	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a run without advanced.auto_cache wrote %d file(s)", len(entries))
	}
}

// TestAutoCacheSurvivesAnUnreadableFile. The cache is an optimisation; one
// that could fail a deploy would be a bad trade at any hit rate. A broken file
// costs one warning and is replaced by what this run learned.
func TestAutoCacheSurvivesAnUnreadableFile(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 4)
	path := filepath.Join(t.TempDir(), "auto.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := autoConfig(srv)
	cfg.AutoCachePath = path
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	log := &recordingLogger{testLogger: testLogger{t}}
	stats, err := Run(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("an unreadable cache failed the deploy: %v", err)
	}
	if stats.FilesUploaded != 4 {
		t.Errorf("uploaded %d file(s), want 4", stats.FilesUploaded)
	}
	if !hasLine(log.warnings, "auto-tuning cache") {
		t.Errorf("no warning about the broken cache: %v", log.warnings)
	}
	if rec := readCache(t, path); rec.Workload.Files != 4 {
		t.Errorf("the broken file was not replaced: %+v", rec)
	}
}

// TestAutoCacheSeedsThePolicy is what a hit is for. A remembered throughput
// fills the one input the policy cannot compute, and it is tagged as
// remembered so that a runtime measurement of this transfer still outranks it.
func TestAutoCacheSeedsThePolicy(t *testing.T) {
	cfg := &config.Config{Auto: config.AutoSettings{Connections: true, Concurrency: true, RequestConcurrency: true}}
	tune := newTuning(cfg)
	tune.setLink(autotune.Link{RTT: 13 * time.Millisecond, Handshake: 360 * time.Millisecond})

	files := make([]fileItem, 400)
	for i := range files {
		files[i] = fileItem{size: 1 << 20}
	}
	work := uploadWorkload(files, false)
	assumed := tune.planFor(work)

	tune.applyCache(autocache.Decision{Hit: true, StreamBytesPerSecond: 32 << 20})
	if got := tune.currentLink().Source(); got != autotune.SourceCached {
		t.Fatalf("the link reports its throughput as %s, want it marked as remembered", got)
	}
	remembered := tune.planFor(work)

	// 400 MiB at 32 MiB/s is a fraction of the work the 1 MiB/s prior implies,
	// and a smaller W wants a smaller pool: that is the whole point of
	// remembering the number.
	if remembered.Connections >= assumed.Connections {
		t.Errorf("planning with a measured 32 MiB/s chose %d connection(s), no fewer than the %d the prior chose",
			remembered.Connections, assumed.Connections)
	}
}

// TestAutoCacheCeilingBoundsThePool: a server that refused a connection, or a
// growth step that measurably did not pay, is evidence the cost model cannot
// derive for itself. It bounds the plan and the runtime controller alike,
// because both go through the same clamp.
func TestAutoCacheCeilingBoundsThePool(t *testing.T) {
	srv := startTestServer(t)
	cfg := autoConfig(srv)
	tune := newTuning(cfg)
	tune.setLink(autotune.Link{RTT: 13 * time.Millisecond, Handshake: 360 * time.Millisecond})
	tune.applyCache(autocache.Decision{Hit: true, ConnectionCeiling: 2})

	files := make([]fileItem, 2000)
	for i := range files {
		files[i] = fileItem{size: 4096}
	}
	if got := tune.planFor(uploadWorkload(files, false)); got.Connections != 2 {
		t.Errorf("a 2000-file deploy over a 13 ms line planned %d connection(s), want the remembered ceiling of 2", got.Connections)
	}

	sess := &session{cfg: cfg, tune: tune, log: testLogger{t}, conns: make([]*conn, autotune.MaxConnections), spread: 1}
	if got := sess.setSpread(autotune.MaxConnections); got != 2 {
		t.Errorf("the runtime controller widened the spread to %d past the remembered ceiling", got)
	}
}

// TestPinnedConnectionsBeatTheCache. "Explicit configuration always overrides
// cached values" is the first line of issue #212's behaviour list, and a
// remembered ceiling is the one thing in a record that could quietly contradict
// a number the user wrote.
func TestPinnedConnectionsBeatTheCache(t *testing.T) {
	cfg := &config.Config{
		Connections: 6,
		Auto:        config.AutoSettings{Concurrency: true, RequestConcurrency: true},
	}
	tune := newTuning(cfg)
	tune.setLink(autotune.Link{RTT: 13 * time.Millisecond, Handshake: 360 * time.Millisecond})
	tune.applyCache(autocache.Decision{Hit: true, ConnectionCeiling: 2, StreamBytesPerSecond: 32 << 20})

	files := make([]fileItem, 2000)
	for i := range files {
		files[i] = fileItem{size: 4096}
	}
	if got := tune.planFor(uploadWorkload(files, false)); got.Connections != 6 {
		t.Errorf("advanced.connections: 6 resolved to %d against a remembered ceiling of 2", got.Connections)
	}
}

// TestCacheObservationReadsTheServersAnswer covers the three ways a run ends up
// with a connection ceiling to record, which is the one piece of a record that
// is not about the measurement.
func TestCacheObservationReadsTheServersAnswer(t *testing.T) {
	work := autotune.SummarizeUploads([]int64{1 << 20, 1 << 20})
	adaptive := func() *config.Config {
		return &config.Config{Auto: config.AutoSettings{Connections: true, Concurrency: true, RequestConcurrency: true}}
	}

	t.Run("the server refused one", func(t *testing.T) {
		tune := newTuning(adaptive())
		obs := tune.cacheObservation(work, 3, true)
		if obs.ConnectionCeiling != 3 || obs.CeilingUntested {
			t.Errorf("obs = %+v, want a ceiling of 3", obs)
		}
	})

	t.Run("a growth step was taken back", func(t *testing.T) {
		tune := newTuning(adaptive())
		tune.applied(autotune.Settings{Connections: 4}, true)
		obs := tune.cacheObservation(work, 8, false)
		if obs.ConnectionCeiling != 4 || obs.CeilingUntested {
			t.Errorf("obs = %+v, want the spread the run settled on", obs)
		}
	})

	t.Run("held at an inherited ceiling", func(t *testing.T) {
		tune := newTuning(adaptive())
		tune.setLink(autotune.Link{RTT: 13 * time.Millisecond, Handshake: 360 * time.Millisecond})
		tune.applyCache(autocache.Decision{Hit: true, ConnectionCeiling: 2})
		files := make([]fileItem, 2000)
		for i := range files {
			files[i] = fileItem{size: 4096}
		}
		tune.planFor(uploadWorkload(files, false))

		obs := tune.cacheObservation(work, 2, false)
		if obs.ConnectionCeiling != 0 || !obs.CeilingUntested {
			t.Errorf("obs = %+v, want an untested ceiling: this run never asked for more", obs)
		}
	})

	t.Run("small deploy still carries an inherited ceiling", func(t *testing.T) {
		tune := newTuning(adaptive())
		tune.setLink(autotune.Link{RTT: 13 * time.Millisecond, Handshake: 360 * time.Millisecond})
		tune.applyCache(autocache.Decision{Hit: true, ConnectionCeiling: 4})
		// One small file plans one connection, so the inherited ceiling never
		// has to clamp it. That makes the ceiling untested, not disproved.
		tune.planFor(autotune.SummarizeUploads([]int64{4096}))

		obs := tune.cacheObservation(work, 1, false)
		if obs.ConnectionCeiling != 0 || !obs.CeilingUntested {
			t.Errorf("obs = %+v, want the unused inherited ceiling carried as untested", obs)
		}
	})

	t.Run("nothing pushed back", func(t *testing.T) {
		tune := newTuning(adaptive())
		obs := tune.cacheObservation(work, 4, false)
		if obs.ConnectionCeiling != 0 || obs.CeilingUntested {
			t.Errorf("obs = %+v, want no ceiling at all", obs)
		}
	})
}

// TestTargetIdentityDistinguishesPaths: a record describes a path to a server,
// so two runs that reach different servers, or the same server through
// different bastions, must not share one.
func TestTargetIdentityDistinguishesPaths(t *testing.T) {
	base := &config.Config{Server: "sftp.example.com", Port: 22, Username: "deploy"}
	same := &config.Config{Server: "sftp.example.com", Port: 22, Username: "deploy"}
	viaJump := &config.Config{Server: "sftp.example.com", Port: 22, Username: "deploy",
		Proxy: &config.Proxy{Server: "bastion.example.com", Port: 22, Username: "jump"}}

	if targetIdentity(base) != targetIdentity(same) {
		t.Error("the same connection produced two identities")
	}
	if targetIdentity(base) == targetIdentity(viaJump) {
		t.Error("a run through a jump host shares an identity with the direct one")
	}
}

func hasLine(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// TestAutoCacheRejectsARecordFromADifferentLink is the validation issue #212
// asks for, end to end: the run measures the link it is on and compares it
// with the one the record was written against. Driven by taking the record a
// real run wrote and moving only its round-trip time, so everything else about
// it still matches exactly.
func TestAutoCacheRejectsARecordFromADifferentLink(t *testing.T) {
	srv := startTestServer(t)
	local := t.TempDir()
	poolTree(t, local, 40)
	path := filepath.Join(t.TempDir(), "auto.json")

	cfg := autoConfig(srv)
	cfg.AutoCachePath = path
	cfg.LogLevel = config.LogDebug
	cfg.Uploads = []config.UploadPair{{Local: local, Remote: "/www"}}

	if _, err := Run(context.Background(), cfg, testLogger{t}); err != nil {
		t.Fatal(err)
	}

	// The same record, from a link a hundred times further away, with a
	// throughput on it so that a hit would have something to restore.
	store, err := autocache.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Records[0].Link.RTTMillis = 100
	store.Records[0].Link.StreamBytesPerSecond = 4 << 20
	if err := autocache.Save(path, store); err != nil {
		t.Fatal(err)
	}

	log := &recordingLogger{testLogger: testLogger{t}}
	if _, err := Run(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}
	if !hasLine(log.infos, "not reusing the cached settings") {
		t.Errorf("the run did not say why it ignored the record: %v", log.infos)
	}
	if hasLine(log.infos, "reusing what an earlier run measured") {
		t.Error("a record from a 100 ms link was reused on a loopback connection")
	}

	// The measurement is kept rather than thrown away: the run that could not
	// use it had nothing better to offer.
	if rec := readCache(t, path); rec.Link.StreamBytesPerSecond != 4<<20 {
		t.Errorf("the stored measurement was discarded by a run that could not use it: %+v", rec.Link)
	}
}

// TestAutoCacheConfirmSaysWhatItDid pins the two sentences a user reads, on a
// link injected rather than measured so the assertion is about the wiring and
// not about how far away a loopback socket happens to be.
func TestAutoCacheConfirmSaysWhatItDid(t *testing.T) {
	work := autocache.WorkloadOf(autotune.SummarizeUploads([]int64{1 << 20, 1 << 20, 1 << 20}))
	store := &autocache.Store{Version: autocache.FormatVersion}
	store.Update(autocache.Observation{
		Target:            "sha256:abc",
		Workload:          work,
		Link:              autocache.Link{RTTMillis: 13, HandshakeMillis: 380, StreamBytesPerSecond: 1 << 20},
		Measured:          true,
		ConnectionCeiling: 2,
	}, time.Now())

	cache := &autoCache{path: "auto.json", store: store, target: "sha256:abc"}
	cache.lookup(work)

	log := &recordingLogger{testLogger: testLogger{t}}
	dec := cache.confirm(14*time.Millisecond, log, false)
	if !dec.Restores() || dec.ConnectionCeiling != 2 || dec.StreamBytesPerSecond != 1<<20 {
		t.Fatalf("decision = %+v, want the throughput and the ceiling restored", dec)
	}
	if !hasLine(log.infos, "reusing what an earlier run measured") ||
		!hasLine(log.infos, "at most 2 connection(s)") {
		t.Errorf("the hit was not reported: %v", log.infos)
	}
}
