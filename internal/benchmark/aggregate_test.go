package benchmark

import (
	"testing"

	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

// The parity check in scripts/test-benchmark.sh shows that this aggregation
// produces the same documents the jq one did. What it cannot show is *why* any
// of it is the way it is: it compares two implementations, so a rule both got
// wrong would pass it. The tests here state the rules themselves.

func metrics(phases map[string]float64, counters map[string]float64) *schema.Metrics {
	m := &schema.Metrics{Counters: map[string]*float64{}}
	for name, wall := range phases {
		m.Phases = append(m.Phases, schema.MetricsPhase{Name: name, WallMS: stats.Ptr(wall)})
	}
	for name, value := range counters {
		m.Counters[name] = stats.Ptr(value)
	}
	return m
}

func run(scenario, label string, repeat int, duration float64, m *schema.Metrics) RunRecord {
	return RunRecord{
		Label: label, Ref: label + "-ref", Scenario: scenario, LinkProfile: "baseline",
		Repeat: repeat, DurationMS: duration, Files: 10, Bytes: 1048576, Metrics: m,
	}
}

// TestDeletesNeverReachTheUploadNumbers is the invariant issue #184 phase 4
// rests on and issue #190 must not lose: the pre-clean is a pure delete sweep,
// it is aggregated from its own metrics files into its own block, and no upload
// row may contain a delete_sweep phase or an sftp_remove round-trip.
func TestDeletesNeverReachTheUploadNumbers(t *testing.T) {
	in := &Inputs{
		Runs: []RunRecord{
			run("small", "candidate", 1, 100, metrics(map[string]float64{"upload": 90}, nil)),
		},
		Deletes: []DeleteRecord{{
			Label: "candidate", Scenario: "small", LinkProfile: "baseline",
			DurationMS: 50, FilesDeleted: 7,
			Metrics: metrics(map[string]float64{"delete_sweep": 40, "remote_scan": 10}, nil),
		}},
	}
	result := BuildStandard(&Manifest{Kind: schema.BenchmarkStandard}, in)

	for _, row := range result.Results {
		for _, phase := range row.Phases {
			if phase.Name == "delete_sweep" {
				t.Errorf("an upload row grew a delete_sweep phase")
			}
		}
	}
	if len(result.Deletes) != 1 {
		t.Fatalf("got %d delete rows, want 1", len(result.Deletes))
	}
	if result.Deletes[0].FilesDeleted != 7 {
		t.Errorf("files_deleted = %v, want 7", result.Deletes[0].FilesDeleted)
	}
}

// TestSweepsThatFoundNothingAreNotCounted pins the other half of that rule. The
// first pre-clean of a build and scenario runs against an empty remote
// directory and measures the scan alone; a median over that and a real sweep
// describes neither, and a coordinate left with no sweep at all has no row.
func TestSweepsThatFoundNothingAreNotCounted(t *testing.T) {
	in := &Inputs{
		Runs: []RunRecord{run("small", "candidate", 1, 100, nil)},
		Deletes: []DeleteRecord{
			{Label: "candidate", Scenario: "small", LinkProfile: "baseline", DurationMS: 5, FilesDeleted: 0},
			{Label: "candidate", Scenario: "small", LinkProfile: "baseline", DurationMS: 50, FilesDeleted: 4},
			{Label: "candidate", Scenario: "empty-target", LinkProfile: "baseline", DurationMS: 5, FilesDeleted: 0},
		},
	}
	result := BuildStandard(&Manifest{Kind: schema.BenchmarkStandard}, in)

	if len(result.Deletes) != 1 {
		t.Fatalf("got %d delete rows, want 1", len(result.Deletes))
	}
	row := result.Deletes[0]
	if row.Sweeps != 1 {
		t.Errorf("sweeps = %d, want 1: the empty sweep must not be counted", row.Sweeps)
	}
	if row.MedianMS != 50 {
		t.Errorf("median_ms = %v, want 50: the empty sweep must not move it", row.MedianMS)
	}
}

// TestASingleRepeatReportsNoSpread is issue #184 phase 2 in the aggregation: a
// 0 there would read as perfect precision, which is the normal case for a
// matrix run whose REPEATS default is 1.
func TestASingleRepeatReportsNoSpread(t *testing.T) {
	in := &Inputs{Runs: []RunRecord{run("small", "candidate", 1, 100, nil)}}
	result := BuildStandard(&Manifest{Kind: schema.BenchmarkStandard}, in)

	if got := result.Results[0].MadMS; got != nil {
		t.Errorf("mad_ms = %v, want null", *got)
	}
	if got := result.Results[0].DurationMS.Mad; got != nil {
		t.Errorf("duration_ms.mad = %v, want null", *got)
	}
}

// TestWithinNoiseNeedsAMeasuredSpread: a candidate whose delta is smaller than
// the reference's own MAD has not been shown to be faster or slower, and where
// there is no spread at all the question has no answer and must not be given
// one.
func TestWithinNoiseNeedsAMeasuredSpread(t *testing.T) {
	in := &Inputs{Runs: []RunRecord{
		run("small", "baseline", 1, 100, nil),
		run("small", "baseline", 2, 120, nil),
		run("small", "baseline", 3, 110, nil),
		run("small", "candidate", 1, 108, nil),
		run("small", "candidate", 2, 108, nil),
		run("small", "candidate", 3, 108, nil),
	}}
	result := BuildStandard(&Manifest{Kind: schema.BenchmarkStandard, ReferenceLabel: "baseline"}, in)

	if len(result.Comparison) != 1 {
		t.Fatalf("got %d comparisons, want 1", len(result.Comparison))
	}
	c := result.Comparison[0]
	if c.WithinNoise == nil {
		t.Fatal("within_noise is null although the reference has a measured spread")
	}
	if !*c.WithinNoise {
		t.Errorf("a delta of %v against a MAD of %v should be inside the noise",
			c.DeltaMS, stats.Or(c.ReferenceMadMS, 0))
	}

	// The same comparison without a spread to compare against.
	single := &Inputs{Runs: []RunRecord{
		run("small", "baseline", 1, 100, nil),
		run("small", "candidate", 1, 150, nil),
	}}
	result = BuildStandard(&Manifest{Kind: schema.BenchmarkStandard, ReferenceLabel: "baseline"}, single)
	if got := result.Comparison[0].WithinNoise; got != nil {
		t.Errorf("within_noise = %v, want null without a measured spread", *got)
	}
}

// TestAutoIsNotACell: auto chooses a coordinate rather than sitting at one, so
// it must stay out of cells[], scaling[], comparison[] and the CSV, and its
// chosen settings must be read back from the run's own counters rather than
// assumed (issue #184, phase 5).
func TestAutoIsNotACell(t *testing.T) {
	cell := func(conns, conc int, duration float64) RunRecord {
		r := run("small", "candidate", 1, duration, metrics(nil, map[string]float64{
			"config_connections": float64(conns), "config_concurrency": float64(conc),
			"config_request_concurrency": 16,
		}))
		r.Connections, r.Concurrency = &conns, &conc
		return r
	}
	autoRun := run("small", "candidate", 1, 900, metrics(nil, map[string]float64{
		"config_connections": 1, "config_concurrency": 4, "config_request_concurrency": 16,
	}))

	in := &Inputs{Runs: []RunRecord{cell(1, 4, 1000), cell(2, 4, 500)}, Auto: []RunRecord{autoRun}}
	m := &Manifest{Kind: schema.BenchmarkMatrix, ReferenceLabel: "candidate", Grid: &ManifestGrid{}}
	result := BuildMatrix(m, in)

	for _, c := range result.Cells {
		if c.Label == "auto" {
			t.Fatal("an auto run reached cells[]")
		}
	}
	for _, s := range result.Scaling {
		if s.Label == "auto" {
			t.Fatal("an auto run reached scaling[]")
		}
	}
	if len(result.Auto) != 1 {
		t.Fatalf("got %d auto rows, want 1", len(result.Auto))
	}
	a := result.Auto[0]
	if stats.Or(a.Chosen.Connections, 0) != 1 || stats.Or(a.Chosen.Concurrency, 0) != 4 {
		t.Errorf("chosen = %v/%v, want the values the run's own counters report",
			stats.Or(a.Chosen.Connections, 0), stats.Or(a.Chosen.Concurrency, 0))
	}
	if a.Best == nil || a.Best.MedianMS != 500 {
		t.Fatalf("best cell = %v, want the fastest candidate cell", a.Best)
	}
	if got := stats.Or(a.RegretMS, 0); got != 400 {
		t.Errorf("regret_ms = %v, want 400", got)
	}
	// The chosen coordinates are on this grid, so the control is filled in.
	if !a.ChosenInGrid || stats.Or(a.ChosenCellMedianMS, 0) != 1000 {
		t.Errorf("chosen_in_grid = %v, chosen_cell_median_ms = %v; want the 1/4 cell",
			a.ChosenInGrid, stats.Or(a.ChosenCellMedianMS, 0))
	}
}

// TestBestOnTheLargestSweptValueIsReported is the honesty check the auto-config
// work rests on: an optimum sitting on the edge of an axis was cut off, not
// measured. An axis with a single swept value has no edge and is not reported.
func TestBestOnTheLargestSweptValueIsReported(t *testing.T) {
	cell := func(conns, conc int, duration float64) RunRecord {
		r := run("small", "candidate", 1, duration, nil)
		r.Connections, r.Concurrency = &conns, &conc
		return r
	}
	m := &Manifest{Kind: schema.BenchmarkMatrix, ReferenceLabel: "candidate", Grid: &ManifestGrid{}}

	// Fastest at the largest concurrency swept: the sweep stopped before the
	// optimum. connections was swept at one value only, so it has no edge.
	edge := BuildMatrix(m, &Inputs{Runs: []RunRecord{cell(1, 1, 900), cell(1, 8, 400)}})
	if got := edge.Scaling[0].BestAtAxisMax; len(got) != 1 || got[0] != "concurrency" {
		t.Errorf("best_at_axis_max = %v, want [concurrency]", got)
	}

	// Fastest in the interior: the optimum was measured.
	interior := BuildMatrix(m, &Inputs{Runs: []RunRecord{cell(1, 1, 900), cell(1, 4, 400), cell(1, 8, 700)}})
	if got := interior.Scaling[0].BestAtAxisMax; len(got) != 0 {
		t.Errorf("best_at_axis_max = %v, want empty", got)
	}
}
