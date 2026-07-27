// Package metrics collects benchmark instrumentation for one easySFTP run:
// phase wall-clock times, per-operation samples, run counters and process
// resource usage. It writes them as one JSON file at the end of the run.
//
// It is off unless EASYSFTP_METRICS_FILE names a writable path, and off is the
// production case. Everything here is package-level state guarded by one
// atomic pointer, so a disabled run pays one atomic load per call site and
// allocates nothing: threading a recorder through every uploader signature
// would be a far larger diff for instrumentation that only the benchmark ever
// switches on.
//
// Two words that are easy to confuse in the output:
//
//   - a *phase* is wall-clock time. Phases are the sequential steps of a run
//     (connect, scan, hash, upload, delete sweep, ...) and their durations add
//     up to roughly the run's duration.
//   - an *operation* is one remote round-trip, sampled per call. Operation
//     totals are cumulative across parallel workers and are therefore normally
//     larger than the phase they happened in. Never read them as wall clock.
package metrics

import (
	"encoding/json"
	"net"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// schemaVersion of the JSON this package writes. Bump it when a field changes
// meaning or disappears; new fields alone do not need a bump.
const schemaVersion = 1

// maxSamples caps how many per-operation durations are kept for percentiles.
// A run with a million files would otherwise hold a million float64 per
// operation name.
//
// ponytail: fixed cap, first N samples win. Switch to reservoir sampling if a
// benchmark ever uploads enough files for the tail to be unrepresentative.
const maxSamples = 50000

// noop is what every entry point returns while metrics are off.
var noop = func() {}

// active is the enabled recorder, or nil. Loaded on every call site.
var active atomic.Pointer[recorder]

type recorder struct {
	path  string
	start time.Time

	mu       sync.Mutex
	phases   map[string]*phase
	ops      map[string]*op
	counters map[string]int64

	netRead  atomic.Int64
	netWrite atomic.Int64

	peakGoroutines atomic.Int64
	stopSampler    chan struct{}
	samplerDone    chan struct{}
}

type phase struct {
	total time.Duration
	count int64
}

type op struct {
	count   int64
	errors  int64
	total   time.Duration
	min     time.Duration
	max     time.Duration
	samples []time.Duration
}

// Start enables collection and arranges for Write to have somewhere to go.
// An empty path disables everything, which is what a normal run does.
func Start(path string) {
	if path == "" {
		return
	}
	r := &recorder{
		path:        path,
		start:       time.Now(),
		phases:      map[string]*phase{},
		ops:         map[string]*op{},
		counters:    map[string]int64{},
		stopSampler: make(chan struct{}),
		samplerDone: make(chan struct{}),
	}
	active.Store(r)
	go r.sampleGoroutines()
}

// Enabled reports whether anything is being collected. Call sites that would
// have to build data purely for the metrics (the counting net.Conn) check it;
// the cheap ones just call through.
func Enabled() bool { return active.Load() != nil }

// Phase starts a wall-clock span. The returned function ends it and is safe to
// call once; a disabled run gets a shared no-op.
//
//	defer metrics.Phase("upload")()
func Phase(name string) func() {
	r := active.Load()
	if r == nil {
		return noop
	}
	start := time.Now()
	return func() {
		d := time.Since(start)
		r.mu.Lock()
		defer r.mu.Unlock()
		p := r.phases[name]
		if p == nil {
			p = &phase{}
			r.phases[name] = p
		}
		p.total += d
		p.count++
	}
}

// Op samples one operation. The returned function records how long it took and
// whether it failed:
//
//	done := metrics.Op("sftp_open")
//	f, err := client.OpenFile(...)
//	done(err)
//
// Operation totals are cumulative across workers; see the package comment.
func Op(name string) func(error) {
	r := active.Load()
	if r == nil {
		return func(error) {}
	}
	start := time.Now()
	return func(err error) {
		d := time.Since(start)
		r.mu.Lock()
		defer r.mu.Unlock()
		o := r.ops[name]
		if o == nil {
			o = &op{min: d, max: d}
			r.ops[name] = o
		}
		o.count++
		o.total += d
		if d < o.min {
			o.min = d
		}
		if d > o.max {
			o.max = d
		}
		if err != nil {
			o.errors++
		}
		if len(o.samples) < maxSamples {
			o.samples = append(o.samples, d)
		}
	}
}

// Count adds n to a named run counter (connections opened, retries, ...).
func Count(name string, n int64) {
	r := active.Load()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[name] += n
}

// Set records an absolute run value (the configured connection count, the
// concurrency limit, ...), overwriting whatever was there.
func Set(name string, v int64) {
	r := active.Load()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[name] = v
}

// CountConn wraps c so its traffic lands in the network byte counters. It is
// only ever called when Enabled() is true, so a production dial keeps the
// unwrapped connection.
func CountConn(c net.Conn) net.Conn {
	r := active.Load()
	if r == nil {
		return c
	}
	return &countingConn{Conn: c, r: r}
}

type countingConn struct {
	net.Conn
	r *recorder
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.r.netRead.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.r.netWrite.Add(int64(n))
	return n, err
}

// sampleGoroutines keeps the peak goroutine count. runtime.NumGoroutine is a
// plain load, so a 50ms tick costs nothing measurable and still catches the
// peak of a phase that lasts a second.
func (r *recorder) sampleGoroutines() {
	defer close(r.samplerDone)
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if n := int64(runtime.NumGoroutine()); n > r.peakGoroutines.Load() {
			r.peakGoroutines.Store(n)
		}
		select {
		case <-r.stopSampler:
			return
		case <-t.C:
		}
	}
}

// Report is the JSON document this package writes. Callers do not build it;
// Write does.
type Report struct {
	SchemaVersion int              `json:"schema_version"`
	Note          string           `json:"note"`
	Process       ProcessMetrics   `json:"process"`
	Phases        []PhaseMetrics   `json:"phases"`
	Operations    []OpMetrics      `json:"operations"`
	Counters      map[string]int64 `json:"counters"`
}

// ProcessMetrics is what the run cost the machine.
type ProcessMetrics struct {
	WallMS     float64 `json:"wall_ms"`
	UserCPUMS  float64 `json:"user_cpu_ms"`
	SysCPUMS   float64 `json:"sys_cpu_ms"`
	CPUPercent float64 `json:"cpu_percent"` // (user+sys)/wall, so >100 means parallel
	MaxRSSByte int64   `json:"max_rss_bytes"`

	TotalAllocBytes int64   `json:"go_total_alloc_bytes"`
	Mallocs         int64   `json:"go_mallocs"`
	HeapSysBytes    int64   `json:"go_heap_sys_bytes"`
	GCCount         int64   `json:"go_gc_count"`
	GCPauseTotalMS  float64 `json:"go_gc_pause_total_ms"`
	PeakGoroutines  int64   `json:"go_peak_goroutines"`
	MaxProcs        int64   `json:"go_max_procs"`

	DiskReadBytes  int64 `json:"disk_read_bytes"`
	DiskWriteBytes int64 `json:"disk_write_bytes"`
	NetReadBytes   int64 `json:"net_read_bytes"`
	NetWriteBytes  int64 `json:"net_write_bytes"`
}

// PhaseMetrics is one wall-clock span, summed over its occurrences.
type PhaseMetrics struct {
	Name   string  `json:"name"`
	WallMS float64 `json:"wall_ms"`
	Count  int64   `json:"count"`
}

// OpMetrics is one operation name, cumulative across parallel workers.
type OpMetrics struct {
	Name    string  `json:"name"`
	Count   int64   `json:"count"`
	Errors  int64   `json:"errors"`
	TotalMS float64 `json:"total_ms"`
	AvgMS   float64 `json:"avg_ms"`
	MinMS   float64 `json:"min_ms"`
	P50MS   float64 `json:"p50_ms"`
	P90MS   float64 `json:"p90_ms"`
	P99MS   float64 `json:"p99_ms"`
	MaxMS   float64 `json:"max_ms"`
}

// Write flushes the collected metrics to the configured file and disables
// further collection. It is a no-op when metrics were never started, and it
// never returns an error the caller must handle: a benchmark artifact that
// could not be written must not fail a deploy.
func Write() {
	r := active.Swap(nil)
	if r == nil {
		return
	}
	close(r.stopSampler)
	<-r.samplerDone

	report := r.build()
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(r.path, append(data, '\n'), 0o644)
}

func (r *recorder) build() Report {
	wall := time.Since(r.start)

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	usage := readUsage()

	proc := ProcessMetrics{
		WallMS:          ms(wall),
		UserCPUMS:       usage.userMS,
		SysCPUMS:        usage.sysMS,
		MaxRSSByte:      usage.maxRSSBytes,
		TotalAllocBytes: int64(mem.TotalAlloc),
		Mallocs:         int64(mem.Mallocs),
		HeapSysBytes:    int64(mem.HeapSys),
		GCCount:         int64(mem.NumGC),
		GCPauseTotalMS:  float64(mem.PauseTotalNs) / 1e6,
		PeakGoroutines:  r.peakGoroutines.Load(),
		MaxProcs:        int64(runtime.GOMAXPROCS(0)),
		DiskReadBytes:   usage.diskReadBytes,
		DiskWriteBytes:  usage.diskWriteBytes,
		NetReadBytes:    r.netRead.Load(),
		NetWriteBytes:   r.netWrite.Load(),
	}
	if wall > 0 {
		proc.CPUPercent = round2((usage.userMS + usage.sysMS) / ms(wall) * 100)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	phases := make([]PhaseMetrics, 0, len(r.phases))
	for name, p := range r.phases {
		phases = append(phases, PhaseMetrics{Name: name, WallMS: ms(p.total), Count: p.count})
	}
	sort.Slice(phases, func(i, j int) bool { return phases[i].WallMS > phases[j].WallMS })

	ops := make([]OpMetrics, 0, len(r.ops))
	for name, o := range r.ops {
		sort.Slice(o.samples, func(i, j int) bool { return o.samples[i] < o.samples[j] })
		m := OpMetrics{
			Name:    name,
			Count:   o.count,
			Errors:  o.errors,
			TotalMS: ms(o.total),
			MinMS:   ms(o.min),
			MaxMS:   ms(o.max),
			P50MS:   ms(percentile(o.samples, 0.50)),
			P90MS:   ms(percentile(o.samples, 0.90)),
			P99MS:   ms(percentile(o.samples, 0.99)),
		}
		if o.count > 0 {
			m.AvgMS = round2(ms(o.total) / float64(o.count))
		}
		ops = append(ops, m)
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].TotalMS > ops[j].TotalMS })

	counters := make(map[string]int64, len(r.counters))
	for k, v := range r.counters {
		counters[k] = v
	}

	return Report{
		SchemaVersion: schemaVersion,
		Note:          "phases are wall clock; operation totals are cumulative across parallel workers and are normally larger than the phase they belong to",
		Process:       proc,
		Phases:        phases,
		Operations:    ops,
		Counters:      counters,
	}
}

// percentile returns the p-quantile of an ascending slice (nearest rank).
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func ms(d time.Duration) float64 { return round2(float64(d) / float64(time.Millisecond)) }

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }
