package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/eiserv/easySFTP/internal/benchmark"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
)

// stored is one entry on disk, decoded once so the index and trend.csv can both
// read it.
type stored struct {
	// path is relative to the benchmark directory, which is what an entry
	// links.
	path     string
	archived bool

	envelope schema.Envelope
	kind     string
}

// retain enforces the window and returns the newest kept release.
//
// Releases are kept by version order, not by date: a late benchmark of an old
// release must not push a newer one out of the window.
func retain(opts Options) (string, error) {
	releases, err := stems(filepath.Join(opts.Dir, "releases"), "release-v", ".json")
	if err != nil {
		return "", err
	}
	versions := make([]string, 0, len(releases))
	for _, stem := range releases {
		versions = append(versions, strings.TrimPrefix(stem, "release-"))
	}
	sortVersions(versions)

	if len(versions) > opts.KeepReleases {
		drop := len(versions) - opts.KeepReleases
		for _, version := range versions[:drop] {
			if err := archive(opts.Dir, "releases", "release-"+version); err != nil {
				return "", err
			}
		}
		versions = versions[drop:]
	}
	if len(versions) == 0 {
		return "", nil
	}

	newest := versions[len(versions)-1]
	for _, ext := range []string{".json", ".md"} {
		if err := copyFile(
			filepath.Join(opts.Dir, "releases", "release-"+newest+ext),
			filepath.Join(opts.Dir, "latest"+ext)); err != nil {
			return "", err
		}
	}

	// Manual and matrix runs follow the same window: anything recorded before
	// the oldest kept release is history too. ISO 8601 UTC compares correctly
	// as a string.
	cutoff := ""
	for _, version := range versions {
		recorded, err := recordedAt(filepath.Join(opts.Dir, "releases", "release-"+version+".json"))
		if err != nil {
			return "", err
		}
		if cutoff == "" || recorded < cutoff {
			cutoff = recorded
		}
	}
	if cutoff == "" {
		return newest, nil
	}
	for _, dir := range []string{"manual", "matrix"} {
		names, err := stems(filepath.Join(opts.Dir, dir), dir+"-", ".json")
		if err != nil {
			return "", err
		}
		for _, name := range names {
			recorded, err := recordedAt(filepath.Join(opts.Dir, dir, name+".json"))
			if err != nil {
				return "", err
			}
			if recorded != "" && recorded < cutoff {
				if err := archive(opts.Dir, dir, name); err != nil {
					return "", err
				}
			}
		}
	}
	return newest, nil
}

// stems lists the base names, without the extension, of everything in a
// directory matching a prefix. A directory that does not exist yet holds
// nothing, which is not an error.
func stems(dir, prefix, ext string) ([]string, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, file := range files {
		name := file.Name()
		if file.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) {
			continue
		}
		out = append(out, strings.TrimSuffix(name, ext))
	}
	sort.Strings(out)
	return out, nil
}

// sortVersions orders vMAJOR.MINOR.PATCH numerically, which is what "sort -V"
// did: sorted as text, v1.0.10 would be archived and v1.0.2 kept.
func sortVersions(versions []string) {
	sort.Slice(versions, func(i, j int) bool {
		a, b := parseVersion(versions[i]), parseVersion(versions[j])
		for k := 0; k < 3; k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return versions[i] < versions[j]
	})
}

func parseVersion(version string) [3]int {
	var out [3]int
	for i, part := range strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3) {
		if i > 2 {
			break
		}
		out[i], _ = strconv.Atoi(part)
	}
	return out
}

// archive moves an entry and everything belonging to it into the archive,
// keeping the per-kind subdirectory.
func archive(root, dir, name string) error {
	target := filepath.Join(root, "archive", dir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(root, dir, name+".json"), filepath.Join(target, name+".json")); err != nil {
		return err
	}
	for _, ext := range []string{".md", ".csv"} {
		from := filepath.Join(root, dir, name+ext)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		if err := os.Rename(from, filepath.Join(target, name+ext)); err != nil {
			return err
		}
	}
	fmt.Printf("archived %s/%s\n", dir, name)
	return nil
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o644)
}

func recordedAt(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var probe struct {
		RecordedAt string `json:"recorded_at"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return probe.RecordedAt, nil
}

// list reads every stored entry, live directories before archived ones, in the
// order the index used to glob them.
func list(root string) ([]stored, error) {
	var out []stored
	for _, source := range []struct {
		dir      string
		prefix   string
		archived bool
	}{
		{"releases", "release-v", false},
		{"manual", "manual-", false},
		{"matrix", "matrix-", false},
		{filepath.Join("archive", "releases"), "release-v", true},
		{filepath.Join("archive", "manual"), "manual-", true},
		{filepath.Join("archive", "matrix"), "matrix-", true},
	} {
		names, err := stems(filepath.Join(root, source.dir), source.prefix, ".json")
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			rel := filepath.ToSlash(filepath.Join(source.dir, name+".json"))
			data, err := os.ReadFile(filepath.Join(root, source.dir, name+".json"))
			if err != nil {
				return nil, err
			}
			var envelope schema.Envelope
			if err := json.Unmarshal(data, &envelope); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			kind, err := envelope.BenchmarkKind()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			out = append(out, stored{path: rel, archived: source.archived, envelope: envelope, kind: kind})
		}
	}
	return out, nil
}

// writeIndex rebuilds index.json, the entry point for anything that reads this
// directory without opening every file, agents included. Every entry carries
// the paths of its files, so a consumer never has to reconstruct the layout.
func writeIndex(opts Options, entries []stored, latest string) error {
	index := schema.Index{
		SchemaVersion: schema.IndexSchemaVersion,
		GeneratedAt:   opts.RecordedAt,
		GeneratedBy:   "cmd/easysftp-bench store",
		Documentation: "benchmarks/README.md",
		KeepReleases:  opts.KeepReleases,
		LatestRelease: nullable(latest),
		Entries:       make([]schema.IndexEntry, 0, len(entries)),
	}
	for _, layout := range [][2]string{
		{"releases", "releases/release-vX.Y.Z.{json,md}"},
		{"manual", "manual/manual-<stamp>-<label>.{json,md}"},
		{"matrix", "matrix/matrix-<stamp>-<label>.{json,md,csv}"},
		{"archive", "archive/<kind>/<same names>"},
		{"latest", "latest.{json,md}"},
	} {
		index.Layout.Set(layout[0], layout[1])
	}

	for _, entry := range entries {
		row, err := indexEntry(entry)
		if err != nil {
			return err
		}
		index.Entries = append(index.Entries, row)
	}
	// Newest first, and stable, so two entries recorded in the same second keep
	// the order they were globbed in.
	sort.SliceStable(index.Entries, func(i, j int) bool {
		return index.Entries[i].RecordedAt < index.Entries[j].RecordedAt
	})
	reverse(index.Entries)

	return benchmark.WriteJSON(filepath.Join(opts.Dir, "index.json"), index)
}

func reverse(entries []schema.IndexEntry) {
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
}

func indexEntry(entry stored) (schema.IndexEntry, error) {
	row := schema.IndexEntry{
		Kind:                   entry.envelope.Kind,
		Version:                entry.envelope.Version,
		Label:                  entry.envelope.Label,
		Official:               entry.envelope.Official,
		RecordedAt:             entry.envelope.RecordedAt,
		Commit:                 entry.envelope.Commit,
		RunURL:                 entry.envelope.RunURL,
		BenchmarkKind:          entry.kind,
		BenchmarkSchemaVersion: 1,
		Archived:               entry.archived,
		JSON:                   entry.path,
		Markdown:               strings.TrimSuffix(entry.path, ".json") + ".md",
		LinkProfiles:           []string{},
	}

	// The measurement is read through the type its own kind declares, so a
	// matrix entry reports the best cell of a sweep and a standard one the
	// candidate median. The two are different numbers, and one field for both
	// would invite reading them as one.
	if entry.kind == schema.BenchmarkMatrix {
		m, err := entry.envelope.Matrix()
		if err != nil {
			return row, fmt.Errorf("%s: %w", entry.path, err)
		}
		row.CandidateRef = nullable(m.CandidateRef)
		row.Runner = nullable(m.Runner)
		row.Environment = m.Environment
		if m.SchemaVersion != 0 {
			row.BenchmarkSchemaVersion = m.SchemaVersion
		}
		if m.Link != nil {
			row.LinkProfiles = m.Link.Shaping.Requested
			row.RTTP50MS = startRTT(m.Link.Probes)
		}
		for _, s := range m.Scaling {
			if s.Label == "candidate" {
				row.BestMS.Set(s.Scenario, s.Best.MedianMS)
			}
		}
	} else {
		s, err := entry.envelope.Standard()
		if err != nil {
			return row, fmt.Errorf("%s: %w", entry.path, err)
		}
		row.CandidateRef = nullable(s.CandidateRef)
		row.Runner = nullable(s.Runner)
		row.Environment = s.Environment
		if s.SchemaVersion != 0 {
			row.BenchmarkSchemaVersion = s.SchemaVersion
		}
		if s.Link != nil {
			row.LinkProfiles = s.Link.Shaping.Requested
			row.RTTP50MS = startRTT(s.Link.Probes)
		}
		for _, result := range s.Results {
			if result.Label == "candidate" {
				row.MedianMS.Set(result.Scenario, result.MedianMS)
			}
		}
	}
	if row.LinkProfiles == nil {
		row.LinkProfiles = []string{}
	}
	return row, nil
}

// startRTT is the opening probe of the baseline profile, i.e. the real line as
// the run found it. Here so that "was that release measured on a slower line"
// can be answered without opening every file. Null on everything measured
// before the probe existed.
func startRTT(probes []schema.Probe) *float64 {
	var fallback *float64
	for _, probe := range probes {
		if probe.At != "start" || probe.RTTMS == nil || probe.RTTMS.P50 == nil {
			continue
		}
		if probe.Profile == "baseline" {
			return probe.RTTMS.P50
		}
		if fallback == nil {
			fallback = probe.RTTMS.P50
		}
	}
	return fallback
}
