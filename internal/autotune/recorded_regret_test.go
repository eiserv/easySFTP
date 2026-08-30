package autotune_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/autotune"
	"github.com/eiserv/easySFTP/internal/benchmark/scenario"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
)

// This file is the other half of the acceptance test in regret_test.go, and it
// exists because that half could not see the only data that measured the
// shipped policy end to end (issue #228).
//
// The replay next door reconstructs a decision and scores it against a grid.
// This one reads what a sweep recorded about itself: every matrix run measures
// `auto` once per scenario and profile and stores the regret it achieved in
// that run's own auto[] block. That number needs no replay and no
// interpolation. It is what the policy did, on that host, that day.
//
// Reading it was the gap. regret_test.go only reads sweeps with at least three
// repeats, and the two release sweeps that ran the shipped policy end to end
// (v3.6.0 and v3.7.0) were stored at two, so the flagship policy was accepted
// against replayed pre-policy grids while its only live measurements sat in
// the repository unexamined, two of nine scenarios over the budget. Every row
// is now printed by the suite, whether or not it is enforced.
//
// Two conditions decide whether a row is also *enforced*, and both are about
// what the number means rather than about how convenient it is:
//
//   - The sweep needs at least minRepeats repeats. A recorded regret is
//     median/best-1, and its denominator is the fastest of the sweep's own
//     cells: at two repeats that is the minimum of a few hundred best-of-two
//     medians, which is biased low for the same reason regret_test.go will not
//     read such a grid. The stored 'small' rows are what that looks like: at
//     concurrency 64 the best connection count is 2 in one sweep (3,893 ms,
//     with 4 at 4,455 ms) and 4 in the next (3,907 ms, with 2 at 4,148 ms),
//     a flip inside the sweeps' own drift. Fixing the repeat count at the
//     source is the companion issue #227, and doing so turns every row here
//     into an enforced one.
//   - Today's policy has to still choose what the row measured. A recorded
//     regret describes the settings that run picked, and the policy has been
//     refitted since (issue #230); holding today's policy to a number produced
//     by a decision it would no longer make would be scoring the wrong thing.
func TestRecordedAutoRegretOfStoredSweeps(t *testing.T) {
	rows := storedAutoRows(t)
	if len(rows) == 0 {
		// The rows this test exists for all carry a workload block. Losing
		// them to a schema change or a broken reader has to fail here rather
		// than turn the test green by finding nothing.
		t.Fatal("no sweep under benchmarks/matrix recorded an auto[] regret with the workload behind it; this test would pass vacuously")
	}

	enforced := 0
	for _, row := range rows {
		recorded, ok := row.chosenSettings()
		if !ok {
			t.Logf("%-34s %-14s no resolved settings recorded", row.sweep, row.auto.Scenario)
			continue
		}
		w, link, err := row.replayInputs()
		if err != nil {
			t.Fatalf("%s/%s: rebuilding the recorded workload: %v", row.sweep, row.auto.Scenario, err)
		}
		today := autotune.Plan(w, link, autotune.Fixed{})
		regret := *row.auto.RegretPercent / 100
		gap := recordedGap(row.auto)

		var why string
		switch {
		case row.repeats < minRepeats:
			why = fmt.Sprintf("not enforced: %d repeats, the best cell it is measured against is a sample (issue #227)", row.repeats)
		case today != recorded:
			why = fmt.Sprintf("not enforced: today's policy chooses %s for this workload", settings(today))
		default:
			why = "enforced"
			enforced++
		}
		t.Logf("%-34s %-14s measured %+6.1f%% (%8s) at %-9s  %s",
			row.sweep, row.auto.Scenario, regret*100, gap.Round(time.Millisecond), settings(recorded), why)

		if why != "enforced" {
			continue
		}
		if regret > regretTarget && gap > trivialGap {
			t.Errorf("%s/%s: the policy measured %.1f%% (%s) behind the best cell of that sweep, over the %.0f%% target. "+
				"This is a measurement of the shipped policy at settings it still chooses, not a replay, so the policy needs refitting",
				row.sweep, row.auto.Scenario, regret*100, gap.Round(time.Millisecond), regretTarget*100)
		}
	}
	if enforced == 0 {
		t.Logf("no stored row is enforceable yet: the %d rows above were measured either below %d repeats or at settings "+
			"the policy no longer chooses. The first release sweep stored after issue #227 changes that.", len(rows), minRepeats)
	}
}

// autoRow is one auto[] entry together with the sweep it came from.
type autoRow struct {
	sweep   string
	repeats int
	auto    schema.Auto
}

// storedAutoRows reads every auto[] row under benchmarks/matrix that carries
// both a recorded regret and the workload the policy saw.
//
// Rows without a workload are the sweeps stored before the policy existed,
// where "auto" was the fixed 1/4/16; there is nothing of today's policy in
// them, and TestTheFixedDefaultsWouldFailThisTest already owns that
// comparison. Unlike the replay next door this reader does not drop a sweep
// for its repeat count: every row is worth printing, and whether it is also
// enforced is decided per row, next to the reason.
func storedAutoRows(t *testing.T) []autoRow {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "benchmarks", "matrix"))
	if err != nil {
		t.Fatalf("locating benchmarks/matrix: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatalf("listing benchmarks/matrix: %v", err)
	}
	var out []autoRow
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		var env schema.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		var m schema.Matrix
		if err := json.Unmarshal(env.Benchmark, &m); err != nil {
			t.Fatalf("decoding the measurement in %s: %v", path, err)
		}
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		for _, a := range m.Auto {
			if a.Workload == nil || a.RegretPercent == nil || a.Best == nil {
				continue
			}
			// The row's own repeat count, falling back to the sweep's: they
			// are the same today, and the row is the more specific answer.
			repeats := a.Repeats
			if repeats == 0 {
				repeats = m.Repeats
			}
			out = append(out, autoRow{sweep: name, repeats: repeats, auto: a})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].sweep != out[j].sweep {
			return out[i].sweep < out[j].sweep
		}
		return out[i].auto.Scenario < out[j].auto.Scenario
	})
	return out
}

// chosenSettings is what the run resolved its three knobs to before the
// runtime controller touched anything, which is what Plan returns and
// therefore the only thing comparable to it. The plain fields are the
// high-water mark including the controller's growth, so Initial* is preferred
// where the run recorded it.
func (r autoRow) chosenSettings() (autotune.Settings, bool) {
	c := r.auto.Chosen
	conns := firstOf(c.InitialConnections, c.Connections)
	concs := firstOf(c.InitialConcurrency, c.Concurrency)
	reqs := firstOf(c.InitialRequestConcurrency, c.RequestConcurrency)
	if conns == nil || concs == nil || reqs == nil {
		return autotune.Settings{}, false
	}
	return autotune.Settings{
		Connections:        int(*conns),
		Concurrency:        int(*concs),
		RequestConcurrency: int(*reqs),
	}, true
}

// replayInputs rebuilds what the policy was looking at when this row was
// recorded. Every feature comes from the row's own workload block, so nothing
// here is reconstructed from the scenario tables except Unknown, which the
// block does not carry and which follows from the deployment's shape.
func (r autoRow) replayInputs() (autotune.Workload, autotune.Link, error) {
	w := r.auto.Workload
	if w.RTTMS == nil || w.HandshakeMS == nil {
		return autotune.Workload{}, autotune.Link{}, fmt.Errorf("the row recorded no link")
	}
	shape := scenario.ShapeOf(r.auto.Scenario)
	out := autotune.Workload{
		Uploads:       int(value(w.Files)),
		UploadBytes:   int64(value(w.Bytes)),
		LargestUpload: int64(value(w.LargestBytes)),
		P50Upload:     int64(value(w.P50Bytes)),
		P90Upload:     int64(value(w.P90Bytes)),
		SmallUploads:  int(value(w.SmallFiles)),
		Probes:        int(value(w.Probes)),
		Unknown:       shape.Prepopulate && shape.Mode != "sync",
	}
	link := autotune.Link{
		RTT:       millis(*w.RTTMS),
		Handshake: millis(*w.HandshakeMS),
	}
	return out, link, nil
}

// recordedGap is the absolute distance to the best cell the run itself
// measured, which is what the trivial-gap allowance is applied to.
func recordedGap(a schema.Auto) time.Duration {
	if a.RegretMS != nil {
		return millis(*a.RegretMS)
	}
	return millis(a.MedianMS - a.Best.MedianMS)
}

func firstOf(values ...*float64) *float64 {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func value(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func settings(s autotune.Settings) string {
	return fmt.Sprintf("%d/%d/%d", s.Connections, s.Concurrency, s.RequestConcurrency)
}
