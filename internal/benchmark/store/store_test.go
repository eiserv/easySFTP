package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/store"
)

// This is the Go form of scripts/test-benchmark-store.sh: the retention window,
// the archiving, the latest.* pointer and the refusal to rewrite a stored
// result, against a temporary directory and never against the repository's own
// benchmarks/.

const standardResult = `{
  "schema_version": 2,
  "benchmark_kind": "standard",
  "candidate_ref": "test-ref",
  "baseline_ref": "",
  "repeats": 1,
  "runner": "self-hosted, Linux 6.1.0, 4 cpu",
  "environment": { "os": "Linux", "cpus": 4 },
  "link": {
    "iface": "eth0",
    "shaping": {
      "available": false,
      "reason": "no profile asked for shaping, so it was never probed for",
      "requested": ["baseline"],
      "applied": ["baseline"]
    },
    "probes": [
      {
        "profile": "baseline", "at": "start", "handshake_ms": 41.2,
        "rtt_ms": { "p50": 18.4, "p90": 21, "min": 17.1, "max": 44.2, "samples": 21 },
        "control": { "streams": 4, "bytes": 8388608,
                     "single_stream_mib_per_s": 0.41, "n_stream_mib_per_s": 1.6 },
        "host_load": { "available": false, "reason": "SFTP-only account" },
        "errors": []
      }
    ]
  },
  "results": [
    { "label": "candidate", "scenario": "small", "link_profile": "baseline", "median_ms": 1000 },
    { "label": "baseline", "scenario": "small", "link_profile": "baseline", "median_ms": 1200 }
  ]
}
`

const matrixResult = `{
  "schema_version": 2,
  "benchmark_kind": "matrix",
  "candidate_ref": "test-ref",
  "axes": { "connections": [1, 2], "concurrency": [1, 4] },
  "cells": [
    { "scenario": "small", "label": "candidate", "connections": 1, "concurrency": 1, "median_ms": 1400 },
    { "scenario": "small", "label": "candidate", "connections": 2, "concurrency": 4, "median_ms": 800 }
  ],
  "scaling": [
    { "scenario": "small", "label": "candidate", "best": { "connections": 2, "concurrency": 4, "median_ms": 800 } }
  ]
}
`

type fixture struct {
	dir            string
	standard       string
	matrix         string
	matrixCSV      string
	summary        string
	t              *testing.T
	benchmarksPath string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	work := t.TempDir()
	f := &fixture{
		t:              t,
		dir:            work,
		benchmarksPath: filepath.Join(work, "benchmarks"),
		standard:       filepath.Join(work, "results.json"),
		matrix:         filepath.Join(work, "matrix.json"),
		matrixCSV:      filepath.Join(work, "matrix.csv"),
		summary:        filepath.Join(work, "summary.md"),
	}
	for path, content := range map[string]string{
		f.standard:  standardResult,
		f.matrix:    matrixResult,
		f.matrixCSV: "scenario,build,median_ms\nsmall,candidate,800\n",
		f.summary:   "## easySFTP benchmark\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	return f
}

func (f *fixture) store(kind, name, recordedAt string) error {
	f.t.Helper()
	opts := store.Options{
		Kind: kind, SummaryMD: f.summary, ResultsJSON: f.standard,
		Label: name, RecordedAt: recordedAt,
		Dir: f.benchmarksPath, KeepReleases: 5,
	}
	if kind == schema.KindRelease {
		opts.Version = name
	}
	if kind == schema.KindMatrix {
		opts.ResultsJSON, opts.ResultsCSV = f.matrix, f.matrixCSV
	}
	return store.Run(opts)
}

func (f *fixture) mustStore(kind, name, recordedAt string) {
	f.t.Helper()
	if err := f.store(kind, name, recordedAt); err != nil {
		f.t.Fatalf("storing %s %s: %v", kind, name, err)
	}
}

func (f *fixture) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(f.benchmarksPath, rel))
	return err == nil
}

func (f *fixture) read(rel string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.benchmarksPath, rel))
	if err != nil {
		f.t.Fatalf("reading %s: %v", rel, err)
	}
	return string(data)
}

func (f *fixture) index() schema.Index {
	f.t.Helper()
	index, err := schema.DecodeStrict[schema.Index]([]byte(f.read("index.json")))
	if err != nil {
		f.t.Fatalf("decoding index.json: %v", err)
	}
	return *index
}

func TestRetentionAndIndex(t *testing.T) {
	f := setup(t)

	// Eight releases plus two manual runs and a matrix sweep, oldest first.
	// Three releases fall out of the five-wide window, which is where version
	// order and plain string order disagree: sorted as text, v1.0.10 would be
	// archived and v1.0.2 kept.
	for _, entry := range [][3]string{
		{schema.KindRelease, "v1.0.0", "2026-01-01T00:00:00Z"},
		{schema.KindManual, "old-run", "2026-01-15T00:00:00Z"},
		{schema.KindRelease, "v1.0.1", "2026-02-01T00:00:00Z"},
		{schema.KindRelease, "v1.0.2", "2026-03-01T00:00:00Z"},
		{schema.KindRelease, "v1.0.9", "2026-04-01T00:00:00Z"},
		{schema.KindRelease, "v1.0.10", "2026-05-01T00:00:00Z"},
		{schema.KindRelease, "v1.1.0", "2026-06-01T00:00:00Z"},
		{schema.KindManual, "new-run", "2026-06-15T00:00:00Z"},
		{schema.KindRelease, "v1.1.1", "2026-07-01T00:00:00Z"},
		{schema.KindMatrix, "sweep", "2026-07-20T00:00:00Z"},
		{schema.KindRelease, "v1.2.0", "2026-08-01T00:00:00Z"},
	} {
		f.mustStore(entry[0], entry[1], entry[2])
	}

	for _, version := range []string{"v1.0.0", "v1.0.1", "v1.0.2"} {
		if f.exists("releases/release-" + version + ".json") {
			t.Errorf("%s is outside the window and should have been archived", version)
		}
		for _, ext := range []string{".json", ".md"} {
			if !f.exists("archive/releases/release-" + version + ext) {
				t.Errorf("archive/releases/release-%s%s is missing", version, ext)
			}
		}
	}
	for _, version := range []string{"v1.0.9", "v1.0.10", "v1.1.0", "v1.1.1", "v1.2.0"} {
		if !f.exists("releases/release-" + version + ".json") {
			t.Errorf("%s should be inside the window", version)
		}
	}

	// latest.* is only ever a copy of a release entry, so a manual or matrix
	// run can never overwrite an official number.
	latest, err := schema.DecodeStrict[schema.Envelope]([]byte(f.read("latest.json")))
	if err != nil {
		t.Fatalf("decoding latest.json: %v", err)
	}
	if latest.Version == nil || *latest.Version != "v1.2.0" || !latest.Official {
		t.Errorf("latest.json is %+v, want the official v1.2.0", latest.Version)
	}
	if latest.SchemaVersion != schema.EnvelopeSchemaVersion {
		t.Errorf("the envelope is schema_version %d", latest.SchemaVersion)
	}
	if !strings.Contains(f.read("latest.md"), "release v1.2.0") {
		t.Error("latest.md does not carry the newest release")
	}
	// The store wraps a measurement, it does not summarise it.
	if !strings.Contains(f.read("latest.json"), `"candidate_ref": "test-ref"`) ||
		!strings.Contains(f.read("latest.json"), `"single_stream_mib_per_s": 0.41`) {
		t.Error("the raw measurement was not kept verbatim")
	}

	// Manual and matrix runs follow the same window.
	if f.exists("manual/manual-20260115T000000Z-old-run.json") ||
		!f.exists("archive/manual/manual-20260115T000000Z-old-run.json") {
		t.Error("a manual run older than the window was not archived")
	}
	if !f.exists("manual/manual-20260615T000000Z-new-run.json") {
		t.Error("a manual run inside the window was archived")
	}
	// A matrix run is filed under its own kind and keeps its CSV.
	for _, ext := range []string{".json", ".md", ".csv"} {
		if !f.exists("matrix/matrix-20260720T000000Z-sweep" + ext) {
			t.Errorf("matrix/matrix-20260720T000000Z-sweep%s is missing", ext)
		}
	}

	index := f.index()
	if len(index.Entries) != 11 {
		t.Errorf("the index lists %d entries, want 11", len(index.Entries))
	}
	if index.LatestRelease == nil || *index.LatestRelease != "v1.2.0" {
		t.Errorf("the index names %v as the latest release", index.LatestRelease)
	}
	archived := 0
	for _, entry := range index.Entries {
		if entry.Archived {
			archived++
		}
	}
	if archived != 4 {
		t.Errorf("the index marks %d entries archived, want 4", archived)
	}

	// Newest first, and the newest is the release just stored.
	newest := index.Entries[0]
	if newest.JSON != "releases/release-v1.2.0.json" {
		t.Errorf("the newest entry links %q", newest.JSON)
	}
	if median, ok := newest.MedianMS.Get("small"); !ok || median != 1000 {
		t.Errorf("the index carries median %v for small", median)
	}
	if newest.Environment == nil || newest.Environment.OS != "Linux" {
		t.Error("the index does not carry the environment forward")
	}
	// environment says which machine, the link fields say which path. Both have
	// to survive into the index, or a reader has to open every file to find out
	// whether two results are comparable at all.
	if newest.RTTP50MS == nil || *newest.RTTP50MS != 18.4 {
		t.Errorf("the index carries RTT %v", newest.RTTP50MS)
	}
	if strings.Join(newest.LinkProfiles, " ") != "baseline" {
		t.Errorf("the index names link profiles %v", newest.LinkProfiles)
	}

	// A matrix entry has no single median per scenario (that is the point of a
	// sweep), so it reports its best cell instead.
	matrix := entryOfKind(t, index, schema.BenchmarkMatrix)
	if best, ok := matrix.BestMS.Get("small"); !ok || best != 800 {
		t.Errorf("the index reports best cell %v for the matrix run", best)
	}
	if matrix.MedianMS.Len() != 0 {
		t.Error("a matrix entry must not report median_ms")
	}

	// trend.csv: a header plus one row per candidate scenario and link profile
	// of every stored non-matrix result.
	trend := strings.Split(strings.TrimRight(f.read("trend.csv"), "\n"), "\n")
	if len(trend) != 11 {
		t.Errorf("trend.csv has %d lines, want a header plus 10 rows", len(trend))
	}
	if !strings.Contains(trend[0], `"link_profile","rtt_p50_ms","control_single_mib_per_s"`) {
		t.Errorf("trend.csv header: %s", trend[0])
	}
	filled := 0
	for _, row := range trend[1:] {
		if strings.Contains(row, `,"baseline",18.4,0.41,`) {
			filled++
		}
		if strings.Contains(row, `"matrix"`) {
			t.Errorf("trend.csv includes a matrix run: %s", row)
		}
	}
	if filled != 10 {
		t.Errorf("trend.csv filled the link columns on %d rows, want 10", filled)
	}
}

// A stored file is never rewritten, in the live directory and in the archive
// alike, and the kinds and versions are validated before anything is written.
func TestRefusals(t *testing.T) {
	f := setup(t)
	f.mustStore(schema.KindRelease, "v1.0.0", "2026-01-01T00:00:00Z")
	f.mustStore(schema.KindRelease, "v1.2.0", "2026-08-01T00:00:00Z")

	for _, tc := range []struct {
		what              string
		kind, name, stamp string
	}{
		{"storing a release twice", schema.KindRelease, "v1.2.0", "2026-08-02T00:00:00Z"},
		{"a non-release version", schema.KindRelease, "1.2.0", "2026-08-02T00:00:00Z"},
		{"an unknown kind", "nightly", "whatever", "2026-08-02T00:00:00Z"},
		{"a malformed timestamp", schema.KindManual, "run", "yesterday"},
	} {
		if err := f.store(tc.kind, tc.name, tc.stamp); err == nil {
			t.Errorf("%s was accepted", tc.what)
		}
	}

	// Storing an archived release again must fail too, or history could be
	// overwritten by a rerun.
	f.mustStore(schema.KindRelease, "v1.0.1", "2026-02-01T00:00:00Z")
	f.mustStore(schema.KindRelease, "v1.0.2", "2026-03-01T00:00:00Z")
	f.mustStore(schema.KindRelease, "v1.0.3", "2026-04-01T00:00:00Z")
	f.mustStore(schema.KindRelease, "v1.0.4", "2026-05-01T00:00:00Z")
	if !f.exists("archive/releases/release-v1.0.0.json") {
		t.Fatal("v1.0.0 should be archived by now")
	}
	if err := f.store(schema.KindRelease, "v1.0.0", "2026-08-02T00:00:00Z"); err == nil {
		t.Error("an archived release was stored again")
	}
}

// A manual run must not be able to write outside the benchmark directory,
// whatever the workflow input says.
func TestLabelIsConfined(t *testing.T) {
	f := setup(t)
	f.mustStore(schema.KindManual, "../../escaped run", "2026-08-16T00:00:00Z")
	if !f.exists("manual/manual-20260816T000000Z-..-..-escaped-run.json") {
		t.Error("the label was not slugified into the manual directory")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "..", "..", "escaped run.json")); err == nil {
		t.Error("a label escaped the benchmark directory")
	}
	if err := f.store(schema.KindManual, "///", "2026-08-17T00:00:00Z"); err == nil {
		t.Error("a label with no letter or digit was accepted")
	}
}

// Reindexing rebuilds index.json, trend.csv and latest.* from what is on disk,
// which is how results moved by hand are picked up.
func TestReindex(t *testing.T) {
	f := setup(t)
	f.mustStore(schema.KindRelease, "v1.0.0", "2026-01-01T00:00:00Z")
	if err := os.Remove(filepath.Join(f.benchmarksPath, "index.json")); err != nil {
		t.Fatalf("removing the index: %v", err)
	}
	if err := store.Run(store.Options{
		Kind: store.KindReindex, RecordedAt: "2026-08-01T00:00:00Z",
		Dir: f.benchmarksPath, KeepReleases: 5,
	}); err != nil {
		t.Fatalf("reindexing: %v", err)
	}
	if len(f.index().Entries) != 1 {
		t.Error("the reindex did not rebuild the index from disk")
	}
}

func entryOfKind(t *testing.T, index schema.Index, kind string) schema.IndexEntry {
	t.Helper()
	for _, entry := range index.Entries {
		if entry.BenchmarkKind == kind {
			return entry
		}
	}
	t.Fatalf("no %s entry in the index", kind)
	return schema.IndexEntry{}
}
