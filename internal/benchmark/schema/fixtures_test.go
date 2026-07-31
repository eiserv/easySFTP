package schema_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/benchmark/schema"
)

// benchmarksDir is the repository's own stored results, used here as fixtures.
//
// They are the reason this package exists: "current stored results remain
// readable" is an acceptance criterion of issue #190, and the only way to keep
// it true while the types move is to decode every file that is actually
// committed, strictly, on every test run.
func benchmarksDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "benchmarks"))
	if err != nil {
		t.Fatalf("locating benchmarks/: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("benchmarks/ is not readable: %v", err)
	}
	return dir
}

// storedResults lists every stored result pair, latest.json included. The two
// top-level exports (index.json and trend.csv) are not results and are checked
// separately.
func storedResults(t *testing.T) []string {
	t.Helper()
	root := benchmarksDir(t)
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		if filepath.Base(path) == "index.json" {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walking benchmarks/: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no stored results found; the fixture test would pass vacuously")
	}
	return files
}

func TestStoredResultsDecodeStrictly(t *testing.T) {
	for _, path := range storedResults(t) {
		t.Run(name(t, path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading: %v", err)
			}
			env, err := schema.DecodeStrict[schema.Envelope](data)
			if err != nil {
				t.Fatalf("decoding the envelope: %v", err)
			}
			if err := env.Validate(); err != nil {
				t.Fatalf("validating: %v", err)
			}
		})
	}
}

// TestStoredResultsSurviveARoundTrip is the other half of strict decoding: a
// type may cover every stored field and still lose data, by dropping a null or
// by turning one into a zero. Re-encoding and comparing the decoded values
// catches that, which matters because step 4 of issue #190 will have the store
// rewrite index.json from these types.
func TestStoredResultsSurviveARoundTrip(t *testing.T) {
	for _, path := range storedResults(t) {
		t.Run(name(t, path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading: %v", err)
			}
			env, err := schema.DecodeStrict[schema.Envelope](data)
			if err != nil {
				t.Fatalf("decoding the envelope: %v", err)
			}
			kind, err := env.BenchmarkKind()
			if err != nil {
				t.Fatalf("reading the benchmark kind: %v", err)
			}

			switch kind {
			case schema.BenchmarkMatrix:
				m, err := env.Matrix()
				if err != nil {
					t.Fatalf("decoding: %v", err)
				}
				assertRoundTrip(t, env.Benchmark, m)
			default:
				s, err := env.Standard()
				if err != nil {
					t.Fatalf("decoding: %v", err)
				}
				assertRoundTrip(t, env.Benchmark, s)
			}
		})
	}
}

func TestStoredIndexDecodesStrictly(t *testing.T) {
	path := filepath.Join(benchmarksDir(t), "index.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading index.json: %v", err)
	}
	index, err := schema.DecodeStrict[schema.Index](data)
	if err != nil {
		t.Fatalf("decoding index.json: %v", err)
	}
	if index.SchemaVersion != schema.IndexSchemaVersion {
		t.Errorf("index schema_version = %d, want %d", index.SchemaVersion, schema.IndexSchemaVersion)
	}
	if len(index.Entries) == 0 {
		t.Fatal("index.json lists no entries")
	}

	// Every entry must point at a file that is on disk: the index is
	// regenerated in full on every store precisely so it describes what is
	// there, and a dangling path means it does not.
	root := benchmarksDir(t)
	for _, e := range index.Entries {
		for _, rel := range []string{e.JSON, e.Markdown} {
			if rel == "" {
				t.Errorf("%s: an entry has no path", e.RecordedAt)
				continue
			}
			if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
				t.Errorf("%s: %v", e.RecordedAt, err)
			}
		}
		if e.CSV != nil {
			if _, err := os.Stat(filepath.Join(root, *e.CSV)); err != nil {
				t.Errorf("%s: %v", e.RecordedAt, err)
			}
		}
		// A matrix entry reports a best cell per scenario and a standard one a
		// median per scenario. Neither fills the other's map, because the two
		// are different numbers.
		if e.BenchmarkKind == schema.BenchmarkMatrix && len(e.MedianMS) > 0 {
			t.Errorf("%s: a matrix entry must not report median_ms", e.RecordedAt)
		}
		if e.BenchmarkKind != schema.BenchmarkMatrix && len(e.BestMS) > 0 {
			t.Errorf("%s: a standard entry must not report best_ms", e.RecordedAt)
		}
	}
}

// TestLatestIsCopiedFromARelease pins the invariant the whole store rests on:
// latest.* is only ever a copy of an official release result.
func TestLatestIsCopiedFromARelease(t *testing.T) {
	root := benchmarksDir(t)
	data, err := os.ReadFile(filepath.Join(root, "latest.json"))
	if err != nil {
		t.Fatalf("reading latest.json: %v", err)
	}
	env, err := schema.DecodeStrict[schema.Envelope](data)
	if err != nil {
		t.Fatalf("decoding latest.json: %v", err)
	}
	if env.Kind != schema.KindRelease || !env.Official {
		t.Fatalf("latest.json is kind %q, official %v; want an official release", env.Kind, env.Official)
	}
}

func name(t *testing.T, path string) string {
	t.Helper()
	rel, err := filepath.Rel(benchmarksDir(t), path)
	if err != nil {
		return filepath.Base(path)
	}
	return strings.ReplaceAll(filepath.ToSlash(rel), "/", "_")
}
