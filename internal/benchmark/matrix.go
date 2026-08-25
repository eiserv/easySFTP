package benchmark

import (
	"sort"

	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

// matrixNote is stored with every sweep. It says what a cell is, why a cell can
// be missing from the declared grid without having been skipped, and why auto
// is not one.
const matrixNote = "one cell per (link_profile, scenario, build, connections, concurrency, request_concurrency); median_ms is wall clock over the cell's repeats. Each scenario is swept over the axis values its payload can use (axes.per_scenario), so a cell absent from the declared axes was not skipped, it was the same configuration twice. auto[] is off the grid: it is the settings easySFTP picked for itself, scored against the best cell"

// BuildMatrix aggregates a sweep into matrix.json.
func BuildMatrix(m *Manifest, in *Inputs) *schema.Matrix {
	cells := matrixCells(in.Runs)
	canary := in.Canary
	if canary == nil {
		canary = []schema.Canary{}
	}

	return &schema.Matrix{
		SchemaVersion:  schema.MatrixSchemaVersion,
		BenchmarkKind:  schema.BenchmarkMatrix,
		CandidateRef:   m.CandidateRef,
		BaselineRef:    m.BaselineRef,
		ReferenceLabel: m.ReferenceLabel,
		Repeats:        m.Repeats,
		Runner:         m.Runner,
		Environment:    m.Environment,
		Link:           buildLink(m, in.Probes),
		Canary:         canary,
		Settings:       m.Settings,
		Scenarios:      m.ScenarioDocs(),
		Note:           matrixNote,
		Axes:           matrixAxes(m),
		Cells:          cells,
		Deletes:        matrixDeletes(in.Deletes),
		Scaling:        matrixScaling(cells),
		Auto:           matrixAuto(in.Auto, cells),
		Comparison:     matrixComparison(cells, m.ReferenceLabel),
	}
}

// matrixAxes declares the grid explicitly so a heatmap does not have to infer
// it from the cells that happen to be present, and records next to it what each
// scenario was actually measured over (issue #184, phase 5).
func matrixAxes(m *Manifest) schema.Axes {
	axes := schema.Axes{
		LinkProfiles:       m.Link.Profiles,
		Connections:        m.Grid.Connections,
		Concurrency:        m.Grid.Concurrency,
		RequestConcurrency: m.Grid.RequestConcurrency,
		PerScenario:        map[string]schema.ScenarioAxes{},
	}
	if axes.LinkProfiles == nil {
		axes.LinkProfiles = []string{}
	}
	for _, s := range m.Scenarios {
		axes.PerScenario[s.Name] = schema.ScenarioAxes{
			Files:              s.Files,
			Connections:        s.Connections,
			Concurrency:        s.Concurrency,
			RequestConcurrency: s.RequestConcurrency,
		}
	}
	return axes
}

func matrixCells(runs []RunRecord) []schema.Cell {
	groups := stats.GroupBy(runs, func(r RunRecord) stats.Key {
		return stats.Key{r.LinkProfile, r.Label, r.Scenario,
			intKey(r.Connections), intKey(r.Concurrency), intKey(r.RequestConcurrency)}
	})

	out := make([]schema.Cell, 0, len(groups))
	for _, group := range groups {
		ms := metricsOf(group, func(r RunRecord) *schema.Metrics { return r.Metrics })
		durations := durationsOf(group)
		files := maxOf(group, func(r RunRecord) float64 { return r.Files })
		bytes := maxOf(group, func(r RunRecord) float64 { return r.Bytes })
		median := stats.MedianOf(durations)

		process := func(f func(*schema.MetricsProcess) *float64) *float64 {
			return stats.Median(processField(ms, f))
		}

		out = append(out, schema.Cell{
			Scenario:           group[0].Scenario,
			Label:              group[0].Label,
			Ref:                group[0].Ref,
			LinkProfile:        group[0].LinkProfile,
			Connections:        deref(group[0].Connections),
			Concurrency:        deref(group[0].Concurrency),
			RequestConcurrency: group[0].RequestConcurrency,
			Repeats:            len(group),
			FailedRuns:         failedRuns(group),
			Files:              files,
			Bytes:              bytes,
			DurationsMS:        durations,
			MedianMS:           median,
			MinMS:              stats.Or(stats.Min(stats.Nums(durations)), 0),
			MaxMS:              stats.Or(stats.Max(stats.Nums(durations)), 0),
			MadMS:              stats.Mad(durations),
			Retries:            sumOf(group, func(r RunRecord) float64 { return r.Retries }),
			Errors:             sumOf(group, func(r RunRecord) float64 { return r.Errors }),

			UserCPUMS:        process(func(p *schema.MetricsProcess) *float64 { return p.UserCPUMS }),
			SysCPUMS:         process(func(p *schema.MetricsProcess) *float64 { return p.SysCPUMS }),
			CPUPercent:       process(func(p *schema.MetricsProcess) *float64 { return p.CPUPercent }),
			MaxRSSBytes:      process(func(p *schema.MetricsProcess) *float64 { return p.MaxRSSBytes }),
			GoGCCount:        process(func(p *schema.MetricsProcess) *float64 { return p.GCCount }),
			GoPeakGoroutines: stats.Max(processField(ms, func(p *schema.MetricsProcess) *float64 { return p.PeakGoroutines })),
			NetWriteBytes:    process(func(p *schema.MetricsProcess) *float64 { return p.NetWriteBytes }),

			ConnectionsOpened: stats.Median(counterOr(ms, "connections_opened", 0)),
			ConnectionsUsed:   stats.Median(counterOr(ms, "connections_used", 0)),
			// What the run actually ran with, not what the axis asked for: the
			// request axis has a pass that sets nothing, and a null coordinate
			// on its own does not say which value that was.
			RequestConcurrencyUsed: stats.Median(counterValues(ms, "config_request_concurrency")),
			ConnectionsRefused:     stats.Add(counterOr(ms, "connections_refused", 0)),
			Reconnects:             stats.Add(counterOr(ms, "reconnects", 0)),
			UploadPhaseMS:          uploadPhase(ms),

			Phases:     aggregatePhases(ms),
			Operations: aggregateOperations(ms),
			MiBPerS:    stats.MiBPerS(bytes, median),
			FilesPerS:  stats.Ratio(files, median),
		})
	}

	stats.SortByKey(out, func(c schema.Cell) stats.Key {
		return stats.Key{c.LinkProfile, c.Scenario, c.Label,
			c.Connections, c.Concurrency, intKey(c.RequestConcurrency)}
	})
	return out
}

// uploadPhase is the wall clock of the upload phase alone, which is the one
// number a cell kept before it carried its whole phase list (issue #184,
// phase 2). It stays for the CSV and for results stored against the old shape.
func uploadPhase(ms []*schema.Metrics) *float64 {
	var values []*float64
	for _, m := range ms {
		for _, p := range m.Phases {
			if p.Name == "upload" {
				values = append(values, p.WallMS)
			}
		}
	}
	return stats.Median(values)
}

func matrixDeletes(records []DeleteRecord) []schema.MatrixDelete {
	groups := stats.GroupBy(records, func(d DeleteRecord) stats.Key {
		return stats.Key{d.LinkProfile, d.Label, d.Scenario,
			intKey(d.Connections), intKey(d.Concurrency), intKey(d.RequestConcurrency)}
	})
	out := make([]schema.MatrixDelete, 0, len(groups))
	for _, group := range groups {
		agg := aggregateDeletes(group)
		if agg.Sweeps == 0 {
			continue
		}
		out = append(out, schema.MatrixDelete{
			Scenario:           group[0].Scenario,
			Label:              group[0].Label,
			LinkProfile:        group[0].LinkProfile,
			Connections:        deref(group[0].Connections),
			Concurrency:        deref(group[0].Concurrency),
			RequestConcurrency: group[0].RequestConcurrency,
			DeleteStats:        agg,
		})
	}
	stats.SortByKey(out, func(d schema.MatrixDelete) stats.Key {
		return stats.Key{d.LinkProfile, d.Scenario, d.Label,
			d.Connections, d.Concurrency, intKey(d.RequestConcurrency)}
	})
	return out
}

// matrixScaling pre-groups the cells into the curve a reader usually wants, per
// link profile, scenario and build.
func matrixScaling(cells []schema.Cell) []schema.Scaling {
	groups := stats.GroupBy(cells, func(c schema.Cell) stats.Key {
		return stats.Key{c.LinkProfile, c.Scenario, c.Label}
	})

	out := make([]schema.Scaling, 0, len(groups))
	for _, group := range groups {
		fastest := append([]schema.Cell(nil), group...)
		sort.SliceStable(fastest, func(i, j int) bool { return fastest[i].MedianMS < fastest[j].MedianMS })
		best := fastest[0]

		points := append([]schema.Cell(nil), group...)
		stats.SortByKey(points, func(c schema.Cell) stats.Key {
			return stats.Key{c.Connections, c.Concurrency}
		})
		curve := make([]schema.Point, 0, len(points))
		for _, c := range points {
			curve = append(curve, schema.Point{
				Connections:            c.Connections,
				Concurrency:            c.Concurrency,
				RequestConcurrency:     c.RequestConcurrency,
				RequestConcurrencyUsed: c.RequestConcurrencyUsed,
				MedianMS:               c.MedianMS,
				MiBPerS:                c.MiBPerS,
				FilesPerS:              c.FilesPerS,
				ConnectionsUsed:        c.ConnectionsUsed,
				ConnectionsRefused:     c.ConnectionsRefused,
				MaxRSSBytes:            c.MaxRSSBytes,
				UserCPUMS:              c.UserCPUMS,
			})
		}

		out = append(out, schema.Scaling{
			Scenario:      group[0].Scenario,
			Label:         group[0].Label,
			LinkProfile:   group[0].LinkProfile,
			Points:        curve,
			Best:          bestOf(best),
			BestAtAxisMax: bestAtAxisMax(group, best),
		})
	}
	return out
}

func bestOf(c schema.Cell) schema.Best {
	return schema.Best{
		Connections:        c.Connections,
		Concurrency:        c.Concurrency,
		RequestConcurrency: c.RequestConcurrency,
		MedianMS:           c.MedianMS,
		MiBPerS:            c.MiBPerS,
		FilesPerS:          c.FilesPerS,
	}
}

// bestAtAxisMax names the axes whose largest swept value is the best cell.
//
// This is the honesty check the whole sweep rests on: where it is non-empty the
// optimum sits at or beyond the edge of the grid, so it was cut off rather than
// measured, and anything fitted to those numbers extrapolates. An axis with a
// single swept value is not reported, because one value has no edge.
func bestAtAxisMax(group []schema.Cell, best schema.Cell) []string {
	out := []string{}

	connections := map[int]bool{}
	concurrency := map[int]bool{}
	requests := map[int]bool{}
	maxConnections, maxConcurrency, maxRequest := 0, 0, 0
	sweptRequest := false
	for _, c := range group {
		connections[c.Connections] = true
		concurrency[c.Concurrency] = true
		if c.Connections > maxConnections {
			maxConnections = c.Connections
		}
		if c.Concurrency > maxConcurrency {
			maxConcurrency = c.Concurrency
		}
		if c.RequestConcurrency != nil {
			requests[*c.RequestConcurrency] = true
			sweptRequest = true
			if *c.RequestConcurrency > maxRequest {
				maxRequest = *c.RequestConcurrency
			}
		} else {
			// A null coordinate is a distinct value of the axis for the purpose
			// of "was more than one thing swept".
			requests[-1] = true
		}
	}

	if len(connections) > 1 && best.Connections == maxConnections {
		out = append(out, "connections")
	}
	if len(concurrency) > 1 && best.Concurrency == maxConcurrency {
		out = append(out, "concurrency")
	}
	if len(requests) > 1 && sweptRequest && best.RequestConcurrency != nil && *best.RequestConcurrency == maxRequest {
		out = append(out, "request_concurrency")
	}
	return out
}

// matrixAuto is the policy measurement: what easySFTP picked when asked to
// pick, what the grid says was best, and the gap between them (issue #184,
// phase 5).
//
// The regret is per link profile on purpose. A policy that is only good on the
// benchmark host's line is the failure class of #62 all over again.
func matrixAuto(runs []RunRecord, cells []schema.Cell) []schema.Auto {
	// Empty rather than absent: a sweep that measured no auto run still has an
	// auto block, the same way it has an empty comparison without a baseline.
	// Absent is reserved for the sweeps stored before the block existed.
	if len(runs) == 0 {
		return []schema.Auto{}
	}
	groups := stats.GroupBy(runs, func(r RunRecord) stats.Key {
		return stats.Key{r.LinkProfile, r.Scenario}
	})

	out := make([]schema.Auto, 0, len(groups))
	for _, group := range groups {
		ms := metricsOf(group, func(r RunRecord) *schema.Metrics { return r.Metrics })
		durations := durationsOf(group)
		files := maxOf(group, func(r RunRecord) float64 { return r.Files })
		bytes := maxOf(group, func(r RunRecord) float64 { return r.Bytes })
		median := stats.MedianOf(durations)

		// Read back from the run's own counters, so it says what easySFTP did
		// rather than what the harness believes it does.
		chosen := schema.Chosen{
			Connections:        stats.Median(counterValues(ms, "config_connections")),
			Concurrency:        stats.Median(counterValues(ms, "config_concurrency")),
			RequestConcurrency: stats.Median(counterValues(ms, "config_request_concurrency")),

			// What the policy picked up front, next to what the run ended
			// with: the gap between the two is the runtime controller's doing
			// (issue #209).
			InitialConnections:        stats.Median(counterValues(ms, "auto_initial_connections")),
			InitialConcurrency:        stats.Median(counterValues(ms, "auto_initial_concurrency")),
			InitialRequestConcurrency: stats.Median(counterValues(ms, "auto_initial_request_concurrency")),
			Changes:                   stats.Median(counterValues(ms, "auto_changes")),

			// And which way they went: a run that grew and then took the step
			// back reports one of each and settles below its own high-water
			// mark (issue #215).
			SpreadIncreases:  stats.Median(counterValues(ms, "auto_spread_increases")),
			SpreadDecreases:  stats.Median(counterValues(ms, "auto_spread_decreases")),
			FinalConnections: stats.Median(counterValues(ms, "auto_final_connections")),
		}
		workload := autoWorkload(ms)

		auto := schema.Auto{
			Scenario:    group[0].Scenario,
			Label:       "auto",
			Ref:         group[0].Ref,
			LinkProfile: group[0].LinkProfile,
			Repeats:     len(group),
			FailedRuns:  failedRuns(group),
			Files:       files,
			Bytes:       bytes,
			DurationsMS: durations,
			MedianMS:    median,
			MinMS:       stats.Or(stats.Min(stats.Nums(durations)), 0),
			MaxMS:       stats.Or(stats.Max(stats.Nums(durations)), 0),
			MadMS:       stats.Mad(durations),
			Chosen:      chosen,

			// What the pool turned out to be. Chosen.Connections is the
			// spread the policy stood behind; these are the slots a file was
			// handed to and the handshakes paid for them, which is not the
			// same number when the spread moved after the last file had
			// started (issue #217).
			ConnectionsOpened:  stats.Median(counterValues(ms, "connections_opened")),
			ConnectionsUsed:    stats.Median(counterValues(ms, "connections_used")),
			ConnectionsRefused: stats.Median(counterValues(ms, "connections_refused")),

			Workload:  workload,
			MiBPerS:   stats.MiBPerS(bytes, median),
			FilesPerS: stats.Ratio(files, median),
		}

		// Scored against the candidate build only: a regret against a baseline
		// build's grid would compare two different products.
		best := bestCandidateCell(cells, auto.Scenario, auto.LinkProfile)
		if best != nil {
			b := bestOf(*best)
			auto.Best = &b
			auto.RegretMS = stats.Ptr(auto.MedianMS - best.MedianMS)
			auto.RegretPercent = stats.Pct(auto.MedianMS, best.MedianMS)
		}
		if at := chosenCell(cells, auto, chosen); at != nil {
			auto.ChosenInGrid = true
			auto.ChosenCellMedianMS = stats.Ptr(at.MedianMS)
		}
		out = append(out, auto)
	}

	stats.SortByKey(out, func(a schema.Auto) stats.Key {
		return stats.Key{a.LinkProfile, a.Scenario}
	})
	return out
}

// autoWorkload reads back the features the policy saw. A build that does not
// report them (every one before issue #209) leaves the whole block out rather
// than filling it with zeros, which would read as "measured nothing".
func autoWorkload(ms []*schema.Metrics) *schema.AutoWorkload {
	w := schema.AutoWorkload{
		Files:        stats.Median(counterValues(ms, "workload_files")),
		Bytes:        stats.Median(counterValues(ms, "workload_bytes")),
		LargestBytes: stats.Median(counterValues(ms, "workload_largest_bytes")),
		Probes:       stats.Median(counterValues(ms, "workload_probes")),
		P50Bytes:     stats.Median(counterValues(ms, "workload_p50_bytes")),
		P90Bytes:     stats.Median(counterValues(ms, "workload_p90_bytes")),
		SmallFiles:   stats.Median(counterValues(ms, "workload_small_files")),
		RTTMS:        micros(stats.Median(counterValues(ms, "link_rtt_us"))),
		HandshakeMS:  micros(stats.Median(counterValues(ms, "link_handshake_us"))),

		StreamBytesPerSecond: stats.Median(counterValues(ms, "link_stream_bytes_per_second")),
		BDPBytes:             stats.Median(counterValues(ms, "link_bdp_bytes")),
	}
	if w == (schema.AutoWorkload{}) {
		return nil
	}
	return &w
}

// micros converts a counter kept in microseconds (counters are integers, and a
// 13 ms RTT rounds to nothing in whole milliseconds) into the milliseconds
// every other duration in these documents is in.
func micros(v *float64) *float64 {
	if v == nil {
		return nil
	}
	return stats.Ptr(*v / 1000)
}

func bestCandidateCell(cells []schema.Cell, scenario, profile string) *schema.Cell {
	var best *schema.Cell
	for i := range cells {
		c := &cells[i]
		if c.Label != "candidate" || c.Scenario != scenario || c.LinkProfile != profile {
			continue
		}
		if best == nil || c.MedianMS < best.MedianMS {
			best = c
		}
	}
	return best
}

// chosenCell finds the settings the policy picked measured as an ordinary cell.
// It is a control and not a result: a large gap between the two means the runs
// saw different conditions, and then the regret next to it is drift rather than
// policy.
func chosenCell(cells []schema.Cell, auto schema.Auto, chosen schema.Chosen) *schema.Cell {
	if chosen.Connections == nil || chosen.Concurrency == nil {
		return nil
	}
	for i := range cells {
		c := &cells[i]
		if c.Label != "candidate" || c.Scenario != auto.Scenario || c.LinkProfile != auto.LinkProfile {
			continue
		}
		if float64(c.Connections) != *chosen.Connections || float64(c.Concurrency) != *chosen.Concurrency {
			continue
		}
		// Compared against what the cell actually ran with, not against its
		// coordinate: the coordinate is null on the pass that sets nothing.
		if !sameNumber(c.RequestConcurrencyUsed, chosen.RequestConcurrency) {
			continue
		}
		return c
	}
	return nil
}

func matrixComparison(cells []schema.Cell, reference string) []schema.MatrixCompare {
	out := []schema.MatrixCompare{}
	for _, candidate := range cells {
		if candidate.Label == reference {
			continue
		}
		var ref *schema.Cell
		for i := range cells {
			c := &cells[i]
			if c.Label == reference && c.Scenario == candidate.Scenario &&
				c.LinkProfile == candidate.LinkProfile &&
				c.Connections == candidate.Connections && c.Concurrency == candidate.Concurrency &&
				sameInt(c.RequestConcurrency, candidate.RequestConcurrency) {
				ref = c
				break
			}
		}
		if ref == nil {
			continue
		}
		out = append(out, schema.MatrixCompare{
			Scenario:           candidate.Scenario,
			Label:              candidate.Label,
			ReferenceLabel:     reference,
			LinkProfile:        candidate.LinkProfile,
			Connections:        candidate.Connections,
			Concurrency:        candidate.Concurrency,
			RequestConcurrency: candidate.RequestConcurrency,
			MedianMS:           candidate.MedianMS,
			ReferenceMedianMS:  ref.MedianMS,
			DeltaMS:            candidate.MedianMS - ref.MedianMS,
			DeltaPercent:       stats.Pct(candidate.MedianMS, ref.MedianMS),
		})
	}
	return out
}

// intKey turns a nullable coordinate into a grouping key element, keeping null
// distinct from every number the way jq does.
func intKey(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func deref(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func sameInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameNumber(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
