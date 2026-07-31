package schema

// Metrics is the instrumentation document one run wrote, as a *reader* sees it.
//
// It deliberately does not reuse metrics.Report from internal/metrics, for two
// reasons. Stored results embed metrics documents written by older easySFTP
// versions, and a reader must keep decoding one that is missing half of today's
// fields. And the aggregation has to tell "the run never reported this" from
// "the run reported zero": a median over a field that only half the repeats
// carry is not the same number as a median over zeroes, so every value here is
// nullable and the aggregation propagates that null.
//
// The fields easySFTP writes today live in metrics.Report; that type stays the
// producer and this one stays the consumer.
type Metrics struct {
	SchemaVersion int                 `json:"schema_version,omitzero"`
	Note          string              `json:"note,omitzero"`
	Process       *MetricsProcess     `json:"process,omitzero"`
	Phases        []MetricsPhase      `json:"phases,omitzero"`
	Operations    []MetricsOp         `json:"operations,omitzero"`
	Counters      map[string]*float64 `json:"counters,omitzero"`
}

// MetricsProcess is what the run cost the machine.
type MetricsProcess struct {
	WallMS          *float64 `json:"wall_ms"`
	UserCPUMS       *float64 `json:"user_cpu_ms"`
	SysCPUMS        *float64 `json:"sys_cpu_ms"`
	CPUPercent      *float64 `json:"cpu_percent"`
	MaxRSSBytes     *float64 `json:"max_rss_bytes"`
	TotalAllocBytes *float64 `json:"go_total_alloc_bytes"`
	Mallocs         *float64 `json:"go_mallocs"`
	HeapSysBytes    *float64 `json:"go_heap_sys_bytes"`
	GCCount         *float64 `json:"go_gc_count"`
	GCPauseTotalMS  *float64 `json:"go_gc_pause_total_ms"`
	PeakGoroutines  *float64 `json:"go_peak_goroutines"`
	MaxProcs        *float64 `json:"go_max_procs"`
	DiskReadBytes   *float64 `json:"disk_read_bytes"`
	DiskWriteBytes  *float64 `json:"disk_write_bytes"`
	NetReadBytes    *float64 `json:"net_read_bytes"`
	NetWriteBytes   *float64 `json:"net_write_bytes"`
}

// MetricsPhase is one wall-clock span, summed over its occurrences.
type MetricsPhase struct {
	Name   string   `json:"name"`
	WallMS *float64 `json:"wall_ms"`
	Count  *float64 `json:"count"`
}

// MetricsOp is one operation name, cumulative across the parallel workers.
type MetricsOp struct {
	Name    string   `json:"name"`
	Count   *float64 `json:"count"`
	Errors  *float64 `json:"errors"`
	TotalMS *float64 `json:"total_ms"`
	AvgMS   *float64 `json:"avg_ms"`
	MinMS   *float64 `json:"min_ms"`
	P50MS   *float64 `json:"p50_ms"`
	P90MS   *float64 `json:"p90_ms"`
	P99MS   *float64 `json:"p99_ms"`
	MaxMS   *float64 `json:"max_ms"`
}

// Counter returns the named counter, or nil when the run did not report it.
// Nil is the value the aggregation then carries forward, which is what keeps a
// missing counter from being averaged in as a zero.
func (m *Metrics) Counter(name string) *float64 {
	if m == nil || m.Counters == nil {
		return nil
	}
	return m.Counters[name]
}
