package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DecodeStrict decodes a stored document and refuses any field these types do
// not model.
//
// Strictness is the point rather than a precaution: it is what turns "the types
// compile" into "the types cover what is actually on disk". A stored result
// that grew a field nobody declared here fails the fixture test instead of
// being silently dropped on the next rewrite.
func DecodeStrict[T any](data []byte) (*T, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var v T
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// A stored document is exactly one JSON value. Trailing content means the
	// file was concatenated or truncated, which is worth an error and not a
	// shrug.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing content after the JSON document")
	}
	return &v, nil
}

// Validate checks the invariants benchmarks/README.md states about a stored
// result. It reports every problem it finds rather than the first, because a
// file that is wrong is usually wrong in more than one way.
//
// It deliberately does not check numbers. Whether a measurement is plausible is
// not something this package can know, and a validator that rejects a slow run
// would be a threshold, which the benchmarks explicitly do not have.
func (e *Envelope) Validate() error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if e.SchemaVersion < 1 || e.SchemaVersion > EnvelopeSchemaVersion {
		add("envelope schema_version %d is not between 1 and %d", e.SchemaVersion, EnvelopeSchemaVersion)
	}

	switch e.Kind {
	case KindRelease:
		// Only a release is official, and only a release names a version:
		// latest.* is copied from these and from nothing else.
		if !e.Official {
			add("a release must be official")
		}
		if e.Version == nil || *e.Version == "" {
			add("a release must name its version")
		}
		if e.Label != nil {
			add("a release must not carry a label")
		}
	case KindManual, KindMatrix:
		if e.Official {
			add("a %s result must not be official", e.Kind)
		}
		if e.Version != nil {
			add("a %s result must not name a version", e.Kind)
		}
	default:
		add("unknown kind %q", e.Kind)
	}

	if e.RecordedAt == "" {
		add("recorded_at is empty")
	}
	if len(e.Benchmark) == 0 {
		add("the envelope carries no benchmark")
		return errors.Join(problems...)
	}

	kind, err := e.BenchmarkKind()
	if err != nil {
		add("%s", err)
		return errors.Join(problems...)
	}
	switch {
	case e.Kind == KindMatrix && kind != BenchmarkMatrix:
		add("a matrix result must wrap a matrix benchmark, got %q", kind)
	case e.Kind != KindMatrix && kind == BenchmarkMatrix:
		// A sweep measures settings a normal deploy does not use, so it answers
		// a different question than a release number does and can never become
		// one.
		add("a matrix benchmark may only be stored as kind %q, not %q", KindMatrix, e.Kind)
	}

	switch kind {
	case BenchmarkMatrix:
		m, err := DecodeStrict[Matrix](e.Benchmark)
		if err != nil {
			add("decoding the matrix benchmark: %s", err)
			break
		}
		problems = append(problems, m.validate()...)
	default:
		s, err := DecodeStrict[Standard](e.Benchmark)
		if err != nil {
			add("decoding the standard benchmark: %s", err)
			break
		}
		problems = append(problems, s.validate()...)
	}

	return errors.Join(problems...)
}

func (s *Standard) validate() []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	// Absent is version 1, which predates the field entirely.
	if s.SchemaVersion > StandardSchemaVersion {
		add("standard schema_version %d is newer than %d", s.SchemaVersion, StandardSchemaVersion)
	}
	if s.CandidateRef == "" {
		add("candidate_ref is empty")
	}
	if len(s.Results) == 0 {
		add("the result has no results[]")
	}
	for i, r := range s.Results {
		if r.Label == "" || r.Scenario == "" {
			add("results[%d] has no label or no scenario", i)
		}
		if r.Repeats != len(r.DurationsMS) {
			add("results[%d] reports %d repeats but keeps %d durations", i, r.Repeats, len(r.DurationsMS))
		}
	}
	// A single repeat has no measured spread, so mad_ms is null rather than 0
	// (issue #184, phase 2). Results stored before that change carry 0 there,
	// which is why this only looks the other way round: a spread reported for a
	// sample of one is a claim of perfect precision.
	for i, r := range s.Results {
		if len(r.DurationsMS) < 2 && r.MadMS != nil && *r.MadMS != 0 {
			add("results[%d] reports a spread over %d sample(s)", i, len(r.DurationsMS))
		}
	}
	for i, d := range s.Deletes {
		if d.Sweeps == 0 {
			add("deletes[%d] is a row with no sweep", i)
		}
	}
	return problems
}

func (m *Matrix) validate() []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	if m.SchemaVersion > MatrixSchemaVersion {
		add("matrix schema_version %d is newer than %d", m.SchemaVersion, MatrixSchemaVersion)
	}
	if m.CandidateRef == "" {
		add("candidate_ref is empty")
	}
	if len(m.Cells) == 0 {
		add("the sweep has no cells[]")
	}
	for i, c := range m.Cells {
		if c.Connections < 1 || c.Concurrency < 1 {
			add("cells[%d] has a non-positive axis coordinate", i)
		}
		if c.Repeats != len(c.DurationsMS) {
			add("cells[%d] reports %d repeats but keeps %d durations", i, c.Repeats, len(c.DurationsMS))
		}
	}
	for i, d := range m.Deletes {
		if d.Sweeps == 0 {
			add("deletes[%d] is a row with no sweep", i)
		}
	}
	// auto chooses a coordinate rather than sitting at one, so it must not
	// appear as a build label anywhere in the grid (issue #184, phase 5).
	for i, c := range m.Cells {
		if c.Label == "auto" {
			add("cells[%d] is labelled auto, which is not a coordinate of the grid", i)
		}
	}
	return problems
}
