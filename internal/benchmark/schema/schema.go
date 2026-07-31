// Package schema describes what a stored benchmark result looks like on disk.
//
// It is deliberately separate from the rest of internal/benchmark: this package
// is about files that already exist under benchmarks/ and must stay readable
// forever, while the packages around it are about what a live run produces.
// Nothing here may gain a required field, because a result stored before that
// field existed would stop decoding, and benchmarks/README.md promises the
// opposite (issue #190).
//
// Two conventions run through every type:
//
//   - A pointer means the value is genuinely nullable in the stored JSON and
//     null means something other than zero. mad_ms is the worked example: a
//     single repeat has no measured spread, and a 0 there would read as perfect
//     precision (issue #184, phase 2).
//   - A plain field means "absent decodes as the zero value and that is
//     harmless". Everything a schema_version 1 result simply does not have,
//     such as link_profile, is of that kind.
package schema

import (
	"encoding/json"
	"fmt"
)

// Kinds a stored result can have. The kind decides the subdirectory, whether
// the result is official, and whether latest.* may ever be copied from it.
const (
	KindRelease = "release"
	KindManual  = "manual"
	KindMatrix  = "matrix"
)

// Benchmark kinds, i.e. which script produced the measurement.
const (
	BenchmarkStandard = "standard"
	BenchmarkMatrix   = "matrix"
)

// EnvelopeSchemaVersion is the current version of the stored wrapper. Version 1
// predates the kind subdirectories; its files are otherwise identical.
const EnvelopeSchemaVersion = 2

// Envelope is the stored file: provenance around a measurement kept verbatim.
type Envelope struct {
	SchemaVersion int     `json:"schema_version"`
	Kind          string  `json:"kind"`
	Version       *string `json:"version"`
	Label         *string `json:"label"`
	Official      bool    `json:"official"`
	RecordedAt    string  `json:"recorded_at"`
	Commit        string  `json:"commit"`
	RunURL        string  `json:"run_url"`

	// Benchmark is results.json or matrix.json exactly as the measuring script
	// wrote it. Kept raw so an envelope can be read, listed and rewritten
	// without the measurement having to round-trip through these types.
	Benchmark json.RawMessage `json:"benchmark"`
}

// BenchmarkKind reports which script produced the wrapped measurement.
//
// It reads the wrapped document rather than the envelope's own kind: a matrix
// measurement is always stored under kind "matrix", but a standard measurement
// is stored under both "release" and "manual". Results from before the field
// existed carry no benchmark_kind and are standard runs.
func (e *Envelope) BenchmarkKind() (string, error) {
	var probe struct {
		BenchmarkKind string `json:"benchmark_kind"`
	}
	if err := json.Unmarshal(e.Benchmark, &probe); err != nil {
		return "", fmt.Errorf("reading benchmark_kind: %w", err)
	}
	if probe.BenchmarkKind == "" {
		return BenchmarkStandard, nil
	}
	return probe.BenchmarkKind, nil
}

// Standard decodes the wrapped measurement as a standard run.
func (e *Envelope) Standard() (*Standard, error) {
	var s Standard
	if err := json.Unmarshal(e.Benchmark, &s); err != nil {
		return nil, fmt.Errorf("decoding standard benchmark: %w", err)
	}
	return &s, nil
}

// Matrix decodes the wrapped measurement as a matrix sweep.
func (e *Envelope) Matrix() (*Matrix, error) {
	var m Matrix
	if err := json.Unmarshal(e.Benchmark, &m); err != nil {
		return nil, fmt.Errorf("decoding matrix benchmark: %w", err)
	}
	return &m, nil
}

// Environment is the machine and toolchain a result was measured on, and the
// comparability key between two results: benchmarks/README.md says two numbers
// may only be compared when this matches. Empty fields are dropped when it is
// written, so every field is optional here.
//
// The link a result was measured over is deliberately not part of this. It is a
// measurement and varies per run, which is exactly why it must not sit in the
// key that decides comparability (issue #184, phase 1).
type Environment struct {
	Runner     string `json:"runner,omitzero"`
	OS         string `json:"os,omitzero"`
	Kernel     string `json:"kernel,omitzero"`
	Arch       string `json:"arch,omitzero"`
	CPUModel   string `json:"cpu_model,omitzero"`
	CPUs       int    `json:"cpus,omitzero"`
	GoVersion  string `json:"go_version,omitzero"`
	RunnerName string `json:"runner_name,omitzero"`
}

// Stats is the duration_ms sub-object of a standard result: every repeat's
// value alongside the summary. Mad is null below two samples.
type Stats struct {
	Values  []float64 `json:"values"`
	Median  float64   `json:"median"`
	Min     float64   `json:"min"`
	Max     float64   `json:"max"`
	Mad     *float64  `json:"mad"`
	Samples int       `json:"samples"`
}

// Phase is wall clock spent in one phase of a run. Phases add up to roughly the
// run's duration; operations do not (see Operation).
type Phase struct {
	Name     string  `json:"name"`
	MedianMS float64 `json:"median_ms"`
}

// Operation is one kind of round-trip, summarised over the repeats.
//
// MedianTotalMS is cumulative across the parallel upload workers and is
// normally larger than the phase it happened in. It is a share of the work and
// a per-call cost, never elapsed time.
type Operation struct {
	Name          string   `json:"name"`
	Count         *float64 `json:"count"`
	MedianTotalMS *float64 `json:"median_total_ms"`
	AvgMS         *float64 `json:"avg_ms"`
	P50MS         *float64 `json:"p50_ms"`
	P90MS         *float64 `json:"p90_ms"`
	P99MS         *float64 `json:"p99_ms"`
	MaxMS         *float64 `json:"max_ms"`
	Errors        *float64 `json:"errors"`
}

// Counters is the run's own bookkeeping (connections opened/used/refused,
// reconnects, retries, the resolved config_* values). An open map on purpose:
// internal/metrics gains counters without this package being a place they have
// to be declared a second time.
type Counters map[string]*float64

// DeleteStats is one aggregated delete sweep without its coordinates. The
// pre-clean that runs before every measured run is a pure delete sweep, and
// these are the only deletion numbers there are (issue #184, phase 4).
//
// Sweeps that found nothing are not counted, and a coordinate left with no
// sweep at all has no row. The coordinates themselves differ by benchmark kind,
// so they sit in StandardDelete and MatrixDelete rather than here.
type DeleteStats struct {
	Sweeps       int       `json:"sweeps"`
	FailedSweeps int       `json:"failed_sweeps"`
	FilesDeleted float64   `json:"files_deleted"`
	DurationsMS  []float64 `json:"durations_ms"`
	MedianMS     float64   `json:"median_ms"`
	MinMS        float64   `json:"min_ms"`
	MaxMS        float64   `json:"max_ms"`
	MadMS        *float64  `json:"mad_ms"`

	Phases     []Phase     `json:"phases"`
	Operations []Operation `json:"operations"`

	// DeletesPerS is appended after the block above, which is why it sits
	// behind the nested arrays rather than next to the other statistics.
	DeletesPerS float64 `json:"deletes_per_s"`
}
