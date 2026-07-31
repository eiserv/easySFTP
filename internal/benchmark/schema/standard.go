package schema

// StandardSchemaVersion is the current version of a standard measurement.
//
// Version 1 (up to and including v3.3.2) carries only candidate_ref,
// baseline_ref, repeats, runner, settings, scenarios and a smaller results[].
// Every key it had still exists and still means the same thing in version 2,
// which is why one set of types reads both.
const StandardSchemaVersion = 2

// Standard is results.json: the measurement scripts/benchmark.sh produces.
//
// Field order matches what the script writes, so a re-encoded document diffs
// against the original by text and not only by value.
type Standard struct {
	SchemaVersion  int               `json:"schema_version,omitzero"`
	BenchmarkKind  string            `json:"benchmark_kind,omitzero"`
	CandidateRef   string            `json:"candidate_ref"`
	BaselineRef    string            `json:"baseline_ref"`
	Repeats        int               `json:"repeats"`
	Runner         string            `json:"runner"`
	Environment    *Environment      `json:"environment,omitzero"`
	Link           *Link             `json:"link,omitzero"`
	Settings       string            `json:"settings"`
	ReferenceLabel string            `json:"reference_label,omitzero"`
	Scenarios      map[string]string `json:"scenarios"`
	Note           string            `json:"note,omitzero"`

	Results    []Result         `json:"results"`
	Deletes    []StandardDelete `json:"deletes,omitzero"`
	Comparison []Comparison     `json:"comparison,omitzero"`
	Runs       []Run            `json:"runs,omitzero"`
}

// Result is one aggregate row per (link profile, build, scenario).
//
// The timing fields at the top are exactly the ones version 1 had, so anything
// already reading a stored benchmark keeps working; everything version 2 added
// sits in its own sub-object.
type Result struct {
	Label       string `json:"label"`
	Ref         string `json:"ref"`
	Scenario    string `json:"scenario"`
	LinkProfile string `json:"link_profile,omitzero"`

	Repeats     int       `json:"repeats"`
	FailedRuns  int       `json:"failed_runs"`
	Files       float64   `json:"files"`
	Bytes       float64   `json:"bytes"`
	DurationsMS []float64 `json:"durations_ms"`
	MedianMS    float64   `json:"median_ms"`
	MinMS       float64   `json:"min_ms"`
	MaxMS       float64   `json:"max_ms"`

	// MadMS is absent in version 1 results and null where a single repeat left
	// no spread to measure.
	MadMS      *float64 `json:"mad_ms"`
	DurationMS *Stats   `json:"duration_ms"`

	Retries            float64  `json:"retries"`
	Errors             float64  `json:"errors"`
	RefusedConnections *float64 `json:"refused_connections"`

	Process    *Process    `json:"process"`
	Counters   Counters    `json:"counters"`
	Phases     []Phase     `json:"phases"`
	Operations []Operation `json:"operations"`

	MiBPerS   float64 `json:"mib_per_s"`
	FilesPerS float64 `json:"files_per_s"`
}

// Process is what a run cost the machine, median over the repeats.
type Process struct {
	UserCPUMS         *float64 `json:"user_cpu_ms"`
	SysCPUMS          *float64 `json:"sys_cpu_ms"`
	CPUPercent        *float64 `json:"cpu_percent"`
	MaxRSSBytes       *float64 `json:"max_rss_bytes"`
	GoTotalAllocBytes *float64 `json:"go_total_alloc_bytes"`
	GoMallocs         *float64 `json:"go_mallocs"`
	GoGCCount         *float64 `json:"go_gc_count"`
	GoGCPauseTotalMS  *float64 `json:"go_gc_pause_total_ms"`
	GoPeakGoroutines  *float64 `json:"go_peak_goroutines"`
	DiskReadBytes     *float64 `json:"disk_read_bytes"`
	NetReadBytes      *float64 `json:"net_read_bytes"`
	NetWriteBytes     *float64 `json:"net_write_bytes"`
}

// StandardDelete is a delete sweep keyed the way a standard run keys one.
type StandardDelete struct {
	Label       string `json:"label"`
	Scenario    string `json:"scenario"`
	LinkProfile string `json:"link_profile"`
	DeleteStats
}

// Comparison is one build against the reference build, at the same scenario and
// on the same link profile.
//
// WithinNoise answers the only question a delta can honestly answer on a shared
// host: is it larger than the reference's own median absolute deviation. It is
// null where there is no measured spread to compare against.
type Comparison struct {
	Scenario          string   `json:"scenario"`
	Label             string   `json:"label"`
	LinkProfile       string   `json:"link_profile,omitzero"`
	ReferenceLabel    string   `json:"reference_label"`
	MedianMS          float64  `json:"median_ms"`
	ReferenceMedianMS float64  `json:"reference_median_ms"`
	DeltaMS           float64  `json:"delta_ms"`
	DeltaPercent      *float64 `json:"delta_percent"`
	ReferenceMadMS    *float64 `json:"reference_mad_ms"`
	WithinNoise       *bool    `json:"within_noise"`
}

// Run is one individual repeat, verbatim, including the metrics document the
// run itself wrote. A matrix result has no equivalent: its cell is the finest
// grain it keeps.
type Run struct {
	Label       string   `json:"label"`
	Ref         string   `json:"ref"`
	Scenario    string   `json:"scenario"`
	LinkProfile string   `json:"link_profile,omitzero"`
	Repeat      int      `json:"repeat"`
	ExitCode    int      `json:"exit_code"`
	DurationMS  float64  `json:"duration_ms"`
	Files       float64  `json:"files"`
	Bytes       float64  `json:"bytes"`
	Retries     float64  `json:"retries"`
	Errors      float64  `json:"errors"`
	Refused     float64  `json:"refused"`
	Metrics     *Metrics `json:"metrics"`
}
