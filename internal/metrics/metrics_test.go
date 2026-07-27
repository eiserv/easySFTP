package metrics

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestDisabledIsInert is the one that matters for production: with metrics off
// every entry point must be a cheap no-op, and Write must not create a file.
func TestDisabledIsInert(t *testing.T) {
	Start("") // explicit: an empty path leaves everything off

	if Enabled() {
		t.Fatal("metrics report as enabled after Start(\"\")")
	}
	Phase("phase")()
	Op("op")(errors.New("boom"))
	Count("counter", 1)
	Set("value", 2)
	Write() // must not panic and must have nowhere to write
}

func TestCollectsPhasesOpsAndCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	Start(path)
	t.Cleanup(func() { active.Store(nil) })

	if !Enabled() {
		t.Fatal("metrics are not enabled after Start")
	}

	endPhase := Phase("upload")
	// Concurrent samples of one operation: the uploader records these from
	// parallel workers, so the recorder has to survive it under -race.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := Op("sftp_open")
			time.Sleep(time.Millisecond)
			done(nil)
		}()
	}
	wg.Wait()
	Op("sftp_open")(errors.New("refused"))
	endPhase()

	Count("retries", 2)
	Count("retries", 1)
	Set("connections_configured", 4)

	Write()

	if Enabled() {
		t.Error("metrics are still enabled after Write")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the report: %v", err)
	}
	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parsing the report: %v", err)
	}

	if got.SchemaVersion != schemaVersion {
		t.Errorf("schema_version = %d, want %d", got.SchemaVersion, schemaVersion)
	}
	if len(got.Phases) != 1 || got.Phases[0].Name != "upload" || got.Phases[0].WallMS <= 0 {
		t.Errorf("phases = %+v, want one positive 'upload' span", got.Phases)
	}
	if len(got.Operations) != 1 {
		t.Fatalf("operations = %+v, want exactly one", got.Operations)
	}
	op := got.Operations[0]
	if op.Count != 9 || op.Errors != 1 {
		t.Errorf("op count/errors = %d/%d, want 9/1", op.Count, op.Errors)
	}
	// The nine samples ran concurrently, so the cumulative total exceeds the
	// wall clock of the phase around them. That relationship is exactly what
	// the report warns about, and it must survive into the JSON.
	if op.TotalMS < op.MaxMS || op.MinMS > op.P50MS || op.P50MS > op.MaxMS {
		t.Errorf("op percentiles are not ordered: %+v", op)
	}
	if got.Counters["retries"] != 3 || got.Counters["connections_configured"] != 4 {
		t.Errorf("counters = %v, want retries 3 and connections_configured 4", got.Counters)
	}
	if got.Process.WallMS <= 0 || got.Process.MaxProcs < 1 {
		t.Errorf("process metrics look unpopulated: %+v", got.Process)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	samples := []time.Duration{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{{0.5, 6}, {0.9, 10}, {0.99, 10}, {0, 1}} {
		if got := percentile(samples, tc.p); got != tc.want {
			t.Errorf("percentile(%v) = %v, want %v", tc.p, got, tc.want)
		}
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile of nothing = %v, want 0", got)
	}
}
