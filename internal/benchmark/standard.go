package benchmark

import (
	"math"

	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

// standardNote is stored with every standard result. It is the sentence that
// keeps a phase and an operation total from being read as the same kind of
// number.
const standardNote = "phases are wall clock and add up to the duration; operations are cumulative across parallel workers and do not"

// BuildStandard aggregates a standard run into results.json.
//
// The layers are the order a reader needs them in: what was measured, the
// aggregate per build and scenario, the deltas, the delete sweeps, and finally
// every individual repeat verbatim. Every key schema_version 1 had still exists
// and still means the same thing.
func BuildStandard(m *Manifest, in *Inputs) *schema.Standard {
	results := standardResults(in.Runs)

	out := &schema.Standard{
		SchemaVersion:  schema.StandardSchemaVersion,
		BenchmarkKind:  schema.BenchmarkStandard,
		CandidateRef:   m.CandidateRef,
		BaselineRef:    m.BaselineRef,
		Repeats:        m.Repeats,
		Runner:         m.Runner,
		Environment:    m.Environment,
		Link:           buildLink(m, in.Probes),
		Settings:       m.Settings,
		ReferenceLabel: m.ReferenceLabel,
		Scenarios:      m.ScenarioDocs(),
		Note:           standardNote,
		Results:        results,
		Deletes:        standardDeletes(in.Deletes),
		Comparison:     standardComparison(results, m.ReferenceLabel),
		Runs:           standardRuns(in.Runs),
	}
	return out
}

// standardResults is one aggregate row per (link profile, build, scenario).
//
// With no profiles requested there is exactly one profile, "baseline", and the
// row count is what it always was.
func standardResults(runs []RunRecord) []schema.Result {
	groups := stats.GroupBy(runs, func(r RunRecord) stats.Key {
		return stats.Key{r.LinkProfile, r.Label, r.Scenario}
	})

	out := make([]schema.Result, 0, len(groups))
	for _, group := range groups {
		ms := metricsOf(group, func(r RunRecord) *schema.Metrics { return r.Metrics })
		durations := durationsOf(group)
		files := maxOf(group, func(r RunRecord) float64 { return r.Files })
		bytes := maxOf(group, func(r RunRecord) float64 { return r.Bytes })
		median := stats.MedianOf(durations)

		out = append(out, schema.Result{
			Label:              group[0].Label,
			Ref:                group[0].Ref,
			Scenario:           group[0].Scenario,
			LinkProfile:        group[0].LinkProfile,
			Repeats:            len(group),
			FailedRuns:         failedRuns(group),
			Files:              files,
			Bytes:              bytes,
			DurationsMS:        durations,
			MedianMS:           median,
			MinMS:              stats.Or(stats.Min(stats.Nums(durations)), 0),
			MaxMS:              stats.Or(stats.Max(stats.Nums(durations)), 0),
			MadMS:              stats.Mad(durations),
			DurationMS:         durationStats(durations),
			Retries:            sumOf(group, func(r RunRecord) float64 { return r.Retries }),
			Errors:             sumOf(group, func(r RunRecord) float64 { return r.Errors }),
			RefusedConnections: stats.Ptr(sumOf(group, func(r RunRecord) float64 { return r.Refused })),
			Process:            aggregateProcess(ms),
			Counters:           aggregateCounters(ms),
			Phases:             aggregatePhases(ms),
			Operations:         aggregateOperations(ms),
			MiBPerS:            stats.MiBPerS(bytes, median),
			FilesPerS:          stats.Ratio(files, median),
		})
	}
	return out
}

// durationStats is the duration_ms sub-object: every repeat's value next to the
// summary, so a reader never has to recompute one from the other.
func durationStats(durations []float64) *schema.Stats {
	values := durations
	if values == nil {
		values = []float64{}
	}
	return &schema.Stats{
		Values:  values,
		Median:  stats.MedianOf(durations),
		Min:     stats.Or(stats.Min(stats.Nums(durations)), 0),
		Max:     stats.Or(stats.Max(stats.Nums(durations)), 0),
		Mad:     stats.Mad(durations),
		Samples: len(durations),
	}
}

func standardDeletes(records []DeleteRecord) []schema.StandardDelete {
	groups := stats.GroupBy(records, func(d DeleteRecord) stats.Key {
		return stats.Key{d.LinkProfile, d.Label, d.Scenario}
	})
	out := make([]schema.StandardDelete, 0, len(groups))
	for _, group := range groups {
		agg := aggregateDeletes(group)
		// A coordinate whose every pre-clean found an empty directory has no
		// row at all, rather than a row of zeroes.
		if agg.Sweeps == 0 {
			continue
		}
		out = append(out, schema.StandardDelete{
			Label:       group[0].Label,
			Scenario:    group[0].Scenario,
			LinkProfile: group[0].LinkProfile,
			DeleteStats: agg,
		})
	}
	return out
}

// standardComparison measures every build against the reference build, on the
// same scenario and the same link profile.
//
// A row whose reference was never measured is dropped rather than compared
// against nothing, which is what the jq's "as $b" over an empty generator does.
func standardComparison(results []schema.Result, reference string) []schema.Comparison {
	out := []schema.Comparison{}
	for _, candidate := range results {
		if candidate.Label == reference {
			continue
		}
		var ref *schema.Result
		for i := range results {
			r := &results[i]
			if r.Label == reference && r.Scenario == candidate.Scenario && r.LinkProfile == candidate.LinkProfile {
				ref = r
				break
			}
		}
		if ref == nil {
			continue
		}
		delta := candidate.MedianMS - ref.MedianMS
		out = append(out, schema.Comparison{
			Scenario:          candidate.Scenario,
			Label:             candidate.Label,
			LinkProfile:       candidate.LinkProfile,
			ReferenceLabel:    reference,
			MedianMS:          candidate.MedianMS,
			ReferenceMedianMS: ref.MedianMS,
			DeltaMS:           delta,
			DeltaPercent:      stats.Pct(candidate.MedianMS, ref.MedianMS),
			ReferenceMadMS:    ref.MadMS,
			WithinNoise:       withinNoise(delta, ref.MadMS),
		})
	}
	return out
}

// withinNoise answers whether a delta is smaller than the reference's own
// median absolute deviation, and null where there is no measured spread to
// compare against. A candidate inside the noise has not been shown to be faster
// or slower, which is the only honest reading of a delta on a shared host.
func withinNoise(delta float64, mad *float64) *bool {
	if stats.Or(mad, 0) == 0 {
		return nil
	}
	within := math.Abs(delta) <= *mad
	return &within
}

// standardRuns keeps every individual repeat verbatim, metrics document
// included. It is what makes a stored result re-aggregatable later.
func standardRuns(runs []RunRecord) []schema.Run {
	out := make([]schema.Run, 0, len(runs))
	for _, r := range runs {
		out = append(out, schema.Run{
			Label:       r.Label,
			Ref:         r.Ref,
			Scenario:    r.Scenario,
			LinkProfile: r.LinkProfile,
			Repeat:      r.Repeat,
			ExitCode:    r.ExitCode,
			DurationMS:  r.DurationMS,
			Files:       r.Files,
			Bytes:       r.Bytes,
			Retries:     r.Retries,
			Errors:      r.Errors,
			Refused:     r.Refused,
			Metrics:     r.Metrics,
		})
	}
	return out
}
