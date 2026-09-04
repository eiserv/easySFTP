// Package store files one benchmark result set under benchmarks/.
//
// It is the Go form of scripts/benchmark-store.sh (issue #190, step 4). The
// measuring half writes its results into a throwaway directory; this turns such
// a set into a permanent entry, filed by kind:
//
//	benchmarks/releases/release-vX.Y.Z.{json,md}            official reference
//	benchmarks/manual/manual-<stamp>-<label>.{json,md}      manual or experimental
//	benchmarks/matrix/matrix-<stamp>-<label>.{json,md,csv}  matrix sweep
//	benchmarks/latest.{json,md}                             copy of the newest release
//	benchmarks/index.json                                   machine readable listing
//	benchmarks/archive/<kind>/                              everything past the window
//
// Three rules the callers depend on:
//
//   - A stored file is never rewritten. Storing a name that already exists
//     fails, in the live directory and in the archive alike.
//   - latest.* is only ever a copy of a release entry, so a manual or matrix run
//     can never overwrite an official number. It stays at the top of benchmarks/
//     so its link never moves.
//   - A matrix run is never official: it sweeps settings that a normal deploy
//     does not use, so it answers a different question than a release number.
//
// Only latest.*, index.json and the archive moves change on a later run; the
// historical files themselves are moved, never edited.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/eiserv/easySFTP/internal/benchmark"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

// KindReindex rebuilds index.json, trend.csv and latest.* from what is already
// on disk, which is how results moved by hand are picked up.
const KindReindex = "reindex"

// Options is one store, already validated by the command that built it.
type Options struct {
	// Kind is "release", "manual", "matrix" or "reindex".
	Kind string

	// ResultsJSON and SummaryMD come from the measuring script and are required
	// unless Kind is "reindex". ResultsCSV is an optional flat export stored
	// next to the pair.
	ResultsJSON string
	SummaryMD   string
	ResultsCSV  string

	// Version is vMAJOR.MINOR.PATCH and is required for a release.
	Version string
	// Label is the short slug of a manual or matrix run.
	Label string

	RecordedAt string
	Commit     string
	RunURL     string

	// Dir is the target directory, KeepReleases how many releases stay outside
	// the archive (the current one plus four, by default).
	Dir          string
	KeepReleases int
}

var (
	versionPattern    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	recordedAtPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
	unsafeInSlug      = regexp.MustCompile(`[^A-Za-z0-9._-]`)
)

// Run stores one result set and rebuilds the bookkeeping around it.
func Run(opts Options) error {
	if opts.KeepReleases < 1 {
		return fmt.Errorf("KEEP_RELEASES must be a positive integer, got '%d'", opts.KeepReleases)
	}
	if !recordedAtPattern.MatchString(opts.RecordedAt) {
		return fmt.Errorf("RECORDED_AT must look like 2026-07-27T12:00:00Z, got '%s'", opts.RecordedAt)
	}

	entry, err := plan(opts)
	if err != nil {
		return err
	}
	if entry != nil {
		if err := write(opts, *entry); err != nil {
			return err
		}
	}

	latest, err := retain(opts)
	if err != nil {
		return err
	}
	stored, err := list(opts.Dir)
	if err != nil {
		return err
	}
	if err := writeIndex(opts, stored, latest); err != nil {
		return err
	}
	if err := writeTrend(opts.Dir, stored); err != nil {
		return err
	}

	if entry != nil {
		fmt.Printf("stored %s/%s.json and %s/%s.md in %s\n", entry.subdir, entry.stem, entry.subdir, entry.stem, opts.Dir)
	} else {
		fmt.Printf("reindexed %s: %d entr(y|ies)\n", opts.Dir, len(stored))
	}
	return nil
}

// newEntry is where one stored result goes and how its own page describes
// itself.
type newEntry struct {
	subdir   string
	stem     string
	slug     string
	version  string
	title    string
	kindNote string
}

func plan(opts Options) (*newEntry, error) {
	switch opts.Kind {
	case KindReindex:
		// Store nothing; only the bookkeeping runs.
		return nil, nil
	case schema.KindRelease:
		// Exactly the tag release-please creates: anything else sorts wrong
		// here and does not line up with .easysftp-version.
		if !versionPattern.MatchString(opts.Version) {
			return nil, fmt.Errorf(
				"VERSION must be vMAJOR.MINOR.PATCH for a release benchmark, got '%s'", opts.Version)
		}
		return &newEntry{
			subdir:   "releases",
			stem:     "release-" + opts.Version,
			version:  opts.Version,
			title:    "release " + opts.Version,
			kindNote: "official reference",
		}, nil
	case schema.KindManual:
		slug, err := slugify(opts.Label)
		if err != nil {
			return nil, err
		}
		return &newEntry{
			subdir:   "manual",
			stem:     "manual-" + stamp(opts.RecordedAt) + "-" + slug,
			slug:     slug,
			title:    "manual run " + slug,
			kindNote: "not an official reference",
		}, nil
	case schema.KindMatrix:
		slug, err := slugify(opts.Label)
		if err != nil {
			return nil, err
		}
		return &newEntry{
			subdir:   "matrix",
			stem:     "matrix-" + stamp(opts.RecordedAt) + "-" + slug,
			slug:     slug,
			title:    "connections/concurrency matrix " + slug,
			kindNote: "a settings sweep, never an official reference",
		}, nil
	default:
		return nil, fmt.Errorf("KIND must be 'release', 'manual', 'matrix' or 'reindex', got '%s'", opts.Kind)
	}
}

// slugify keeps a workflow input inside the benchmark directory and inside what
// a Windows checkout can write (no colons, so the timestamp loses them too).
func slugify(label string) (string, error) {
	slug := unsafeInSlug.ReplaceAllString(label, "-")
	if strings.Trim(slug, "-") == "" {
		return "", fmt.Errorf("LABEL must contain at least one letter or digit")
	}
	return slug, nil
}

func stamp(recordedAt string) string {
	return strings.NewReplacer(":", "", "-", "").Replace(recordedAt)
}

// write files the new entry, refusing to touch a name that already exists.
func write(opts Options, entry newEntry) error {
	dir := filepath.Join(opts.Dir, entry.subdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, base := range []string{dir, filepath.Join(opts.Dir, "archive", entry.subdir)} {
		for _, ext := range []string{".json", ".md", ".csv"} {
			path := filepath.Join(base, entry.stem+ext)
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists: stored benchmarks are never rewritten", path)
			}
		}
	}

	measurement, err := os.ReadFile(opts.ResultsJSON)
	if err != nil {
		return err
	}
	if !json.Valid(measurement) {
		return fmt.Errorf("RESULTS_JSON ('%s') is not valid JSON", opts.ResultsJSON)
	}
	summary, err := os.ReadFile(opts.SummaryMD)
	if err != nil {
		return err
	}

	// The envelope keeps the measurement verbatim under .benchmark and adds
	// only provenance around it, so a reader never has to guess which run a
	// number is.
	//
	// schema_version 2 is the envelope's own version: the directory layout
	// gained a level and the entry now records which measuring script produced
	// it. Envelopes already stored as version 1 keep their number and stay
	// readable; .benchmark carries its own schema_version.
	envelope := schema.Envelope{
		SchemaVersion: schema.EnvelopeSchemaVersion,
		Kind:          opts.Kind,
		Version:       nullable(entry.version),
		Label:         nullable(entry.slug),
		Official:      opts.Kind == schema.KindRelease,
		RecordedAt:    opts.RecordedAt,
		Commit:        nullable(opts.Commit),
		RunURL:        nullable(opts.RunURL),
		Benchmark:     json.RawMessage(measurement),
	}
	if err := benchmark.WriteJSON(filepath.Join(dir, entry.stem+".json"), envelope); err != nil {
		return err
	}
	if opts.Kind == schema.KindMatrix {
		warnThinMatrix(measurement)
	}

	var page strings.Builder
	fmt.Fprintf(&page, "# easySFTP benchmark: %s\n\n", entry.title)
	page.WriteString("| Field | Value |\n|---|---|\n")
	fmt.Fprintf(&page, "| Kind | %s (%s) |\n", opts.Kind, entry.kindNote)
	if entry.version != "" {
		fmt.Fprintf(&page, "| Version | `%s` |\n", entry.version)
	}
	fmt.Fprintf(&page, "| Recorded | %s |\n", opts.RecordedAt)
	if opts.Commit != "" {
		fmt.Fprintf(&page, "| Commit | `%s` |\n", opts.Commit)
	}
	if opts.RunURL != "" {
		fmt.Fprintf(&page, "| Workflow run | %s |\n", opts.RunURL)
	}
	fmt.Fprintf(&page, "| Raw data | [%s.json](%s.json) |\n", entry.stem, entry.stem)
	if opts.ResultsCSV != "" {
		fmt.Fprintf(&page, "| Flat export | [%s.csv](%s.csv) |\n", entry.stem, entry.stem)
	}
	page.WriteString("\n")
	page.Write(summary)
	if err := benchmark.WriteFile(filepath.Join(dir, entry.stem+".md"), page.String()); err != nil {
		return err
	}

	if opts.ResultsCSV != "" {
		data, err := os.ReadFile(opts.ResultsCSV)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, entry.stem+".csv"), data, 0o644)
	}
	return nil
}

// warnThinMatrix prints when a matrix sweep is stored with fewer repeats than
// the acceptance tests will read. The file is still filed — store never
// refuses a valid measurement — but the regenerated index marks it
// below_analysis_threshold so consumers can skip it (issue #227).
func warnThinMatrix(measurement []byte) {
	var probe struct {
		Repeats int `json:"repeats"`
	}
	if err := json.Unmarshal(measurement, &probe); err != nil {
		return
	}
	if probe.Repeats > 0 && probe.Repeats < stats.MinRepeatsForAnalysis {
		fmt.Printf("warning: matrix sweep stored with repeats=%d; below analysis threshold of %d (mad_ms is structurally 0 and acceptance tests will skip it; issue #227)\n",
			probe.Repeats, stats.MinRepeatsForAnalysis)
	}
}

func nullable(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
