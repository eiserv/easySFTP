package benchmark

import (
	"sort"

	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

// The helpers here are the pieces both aggregations share, and each one mirrors
// one jq expression from scripts/benchmark-lib.sh or the two scripts. Where a
// comment says "jq does X", that is the reason the Go looks the way it does and
// not an observation about jq.

// metricsOf is jq's "(map(select(.metrics != null)) | map(.metrics)) as $m":
// the metrics documents of the runs that wrote one. Runs that died before
// writing are not represented at all, rather than as an empty document.
func metricsOf[T any](items []T, metrics func(T) *schema.Metrics) []*schema.Metrics {
	var out []*schema.Metrics
	for _, item := range items {
		if m := metrics(item); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// processField reads one process value from every metrics document, keeping the
// gaps as nulls. A document that never reported the field contributes a null,
// which can then reach the middle of the sorted list and make the median null.
func processField(ms []*schema.Metrics, field func(*schema.MetricsProcess) *float64) []*float64 {
	out := make([]*float64, 0, len(ms))
	for _, m := range ms {
		if m == nil || m.Process == nil {
			out = append(out, nil)
			continue
		}
		out = append(out, field(m.Process))
	}
	return out
}

// counterValues reads one counter from every metrics document, keeping the gaps
// as nulls. Used where the jq reads ".counters.x" without a "// 0".
func counterValues(ms []*schema.Metrics, name string) []*float64 {
	out := make([]*float64, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Counter(name))
	}
	return out
}

// counterOr reads one counter from every metrics document with a default, which
// is what "(.counters.x // 0)" does: a run that never reported the counter
// contributes the default and not a gap.
func counterOr(ms []*schema.Metrics, name string, fallback float64) []*float64 {
	out := make([]*float64, 0, len(ms))
	for _, m := range ms {
		out = append(out, stats.Ptr(stats.Or(m.Counter(name), fallback)))
	}
	return out
}

// aggregateProcess is the process block of a standard result: the median of
// every value across the repeats, except the goroutine peak, which is a peak
// and is therefore taken as a maximum.
func aggregateProcess(ms []*schema.Metrics) *schema.Process {
	field := func(f func(*schema.MetricsProcess) *float64) *float64 {
		return stats.Median(processField(ms, f))
	}
	return &schema.Process{
		UserCPUMS:         field(func(p *schema.MetricsProcess) *float64 { return p.UserCPUMS }),
		SysCPUMS:          field(func(p *schema.MetricsProcess) *float64 { return p.SysCPUMS }),
		CPUPercent:        field(func(p *schema.MetricsProcess) *float64 { return p.CPUPercent }),
		MaxRSSBytes:       field(func(p *schema.MetricsProcess) *float64 { return p.MaxRSSBytes }),
		GoTotalAllocBytes: field(func(p *schema.MetricsProcess) *float64 { return p.TotalAllocBytes }),
		GoMallocs:         field(func(p *schema.MetricsProcess) *float64 { return p.Mallocs }),
		GoGCCount:         field(func(p *schema.MetricsProcess) *float64 { return p.GCCount }),
		GoGCPauseTotalMS:  field(func(p *schema.MetricsProcess) *float64 { return p.GCPauseTotalMS }),
		GoPeakGoroutines:  stats.Max(processField(ms, func(p *schema.MetricsProcess) *float64 { return p.PeakGoroutines })),
		DiskReadBytes:     field(func(p *schema.MetricsProcess) *float64 { return p.DiskReadBytes }),
		NetReadBytes:      field(func(p *schema.MetricsProcess) *float64 { return p.NetReadBytes }),
		NetWriteBytes:     field(func(p *schema.MetricsProcess) *float64 { return p.NetWriteBytes }),
	}
}

// aggregateCounters is the counters object: every counter any repeat reported,
// each one the median of the repeats that reported it.
//
// A counter a repeat did not report contributes nothing here rather than a
// null, because the jq concatenates the counter objects and groups the entries
// that exist. That is deliberately different from the process block above,
// where the field is read from every document and a gap stays a gap.
func aggregateCounters(ms []*schema.Metrics) schema.Counters {
	values := map[string][]*float64{}
	for _, m := range ms {
		for name, value := range m.Counters {
			values[name] = append(values[name], value)
		}
	}
	if len(values) == 0 {
		return schema.Counters{}
	}
	out := make(schema.Counters, len(values))
	for name, list := range values {
		out[name] = stats.Median(list)
	}
	return out
}

// aggregatePhases is the phases block: wall clock per phase, median over the
// repeats, ordered by cost. Phases add up to roughly the run's duration.
func aggregatePhases(ms []*schema.Metrics) []schema.Phase {
	var all []schema.MetricsPhase
	for _, m := range ms {
		all = append(all, m.Phases...)
	}
	groups := stats.GroupBy(all, func(p schema.MetricsPhase) stats.Key {
		return stats.Key{p.Name}
	})
	out := make([]schema.Phase, 0, len(groups))
	for _, group := range groups {
		wall := make([]*float64, 0, len(group))
		for _, p := range group {
			wall = append(wall, p.WallMS)
		}
		out = append(out, schema.Phase{
			Name:     group[0].Name,
			MedianMS: stats.Or(stats.Median(wall), 0),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].MedianMS > out[j].MedianMS })
	return out
}

// aggregateOperations is the round-trip block: per operation name, the median
// of every percentile across the repeats and the sum of the errors.
//
// The totals are cumulative across the parallel upload workers and are normally
// larger than the phase they happened in. They are a share of the work and a
// per-call cost, never elapsed time.
func aggregateOperations(ms []*schema.Metrics) []schema.Operation {
	var all []schema.MetricsOp
	for _, m := range ms {
		all = append(all, m.Operations...)
	}
	groups := stats.GroupBy(all, func(o schema.MetricsOp) stats.Key {
		return stats.Key{o.Name}
	})
	out := make([]schema.Operation, 0, len(groups))
	for _, group := range groups {
		median := func(f func(schema.MetricsOp) *float64) *float64 {
			values := make([]*float64, 0, len(group))
			for _, o := range group {
				values = append(values, f(o))
			}
			return stats.Median(values)
		}
		errors := make([]*float64, 0, len(group))
		for _, o := range group {
			errors = append(errors, o.Errors)
		}
		out = append(out, schema.Operation{
			Name:          group[0].Name,
			Count:         median(func(o schema.MetricsOp) *float64 { return o.Count }),
			MedianTotalMS: median(func(o schema.MetricsOp) *float64 { return o.TotalMS }),
			AvgMS:         median(func(o schema.MetricsOp) *float64 { return o.AvgMS }),
			P50MS:         median(func(o schema.MetricsOp) *float64 { return o.P50MS }),
			P90MS:         median(func(o schema.MetricsOp) *float64 { return o.P90MS }),
			P99MS:         median(func(o schema.MetricsOp) *float64 { return o.P99MS }),
			MaxMS:         median(func(o schema.MetricsOp) *float64 { return o.MaxMS }),
			Errors:        stats.Add(errors),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return stats.Or(out[i].MedianTotalMS, 0) > stats.Or(out[j].MedianTotalMS, 0)
	})
	return out
}

// aggregateDeletes is JQ_DELETE: one group of pre-cleans reduced to the row
// both scripts store under deletes[].
//
// Sweeps that deleted nothing are dropped rather than averaged in. The first
// pre-clean of a build and scenario runs against an empty remote directory and
// measures the scan alone, and a median over that and a real sweep describes
// neither.
func aggregateDeletes(rows []DeleteRecord) schema.DeleteStats {
	var swept []DeleteRecord
	for _, row := range rows {
		if row.FilesDeleted > 0 {
			swept = append(swept, row)
		}
	}
	ms := metricsOf(swept, func(d DeleteRecord) *schema.Metrics { return d.Metrics })

	durations := make([]float64, 0, len(swept))
	deleted := make([]*float64, 0, len(swept))
	failed := 0
	for _, row := range swept {
		durations = append(durations, row.DurationMS)
		deleted = append(deleted, stats.Ptr(row.FilesDeleted))
		if row.ExitCode != 0 {
			failed++
		}
	}

	out := schema.DeleteStats{
		Sweeps:       len(swept),
		FailedSweeps: failed,
		FilesDeleted: stats.Or(stats.Max(deleted), 0),
		DurationsMS:  durations,
		MedianMS:     stats.MedianOf(durations),
		MinMS:        stats.Or(stats.Min(stats.Nums(durations)), 0),
		MaxMS:        stats.Or(stats.Max(stats.Nums(durations)), 0),
		MadMS:        stats.Mad(durations),
		Phases:       aggregatePhases(ms),
		Operations:   aggregateOperations(ms),
	}
	out.DeletesPerS = stats.Ratio(out.FilesDeleted, out.MedianMS)
	if out.DurationsMS == nil {
		out.DurationsMS = []float64{}
	}
	return out
}

// durationsOf and the two helpers below exist so the two aggregations state the
// same thing the same way.
func durationsOf(runs []RunRecord) []float64 {
	out := make([]float64, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.DurationMS)
	}
	return out
}

func maxOf(runs []RunRecord, field func(RunRecord) float64) float64 {
	values := make([]*float64, 0, len(runs))
	for _, r := range runs {
		values = append(values, stats.Ptr(field(r)))
	}
	return stats.Or(stats.Max(values), 0)
}

func sumOf(runs []RunRecord, field func(RunRecord) float64) float64 {
	var total float64
	for _, r := range runs {
		total += field(r)
	}
	return total
}

func failedRuns(runs []RunRecord) int {
	failed := 0
	for _, r := range runs {
		if r.ExitCode != 0 {
			failed++
		}
	}
	return failed
}
