package schema

// IndexSchemaVersion is the current version of benchmarks/index.json.
const IndexSchemaVersion = 2

// Index is benchmarks/index.json: every stored result, newest first, with
// enough of each one that a reader does not have to open every file.
//
// It is regenerated in full on every store, so it always describes what is
// actually on disk.
type Index struct {
	SchemaVersion int             `json:"schema_version"`
	GeneratedAt   string          `json:"generated_at"`
	GeneratedBy   string          `json:"generated_by"`
	Documentation string          `json:"documentation"`
	Layout        Ordered[string] `json:"layout"`
	KeepReleases  int             `json:"keep_releases"`
	LatestRelease *string         `json:"latest_release"`
	Entries       []IndexEntry    `json:"entries"`
}

// IndexEntry is one stored result as the index lists it.
//
// MedianMS carries the candidate's median per scenario for a standard result
// and BestMS the best cell per scenario for a matrix one. Each kind fills its
// own and leaves the other empty, because the two are not the same number and a
// single field would invite reading them as one.
// Every field a stored entry can carry as JSON null is a pointer here. The
// store rewrites this file from these types, so a null that came back as an
// empty string would be a change to a committed document made in passing.
type IndexEntry struct {
	Kind         string  `json:"kind"`
	Version      *string `json:"version"`
	Label        *string `json:"label"`
	Official     bool    `json:"official"`
	RecordedAt   string  `json:"recorded_at"`
	Commit       *string `json:"commit"`
	RunURL       *string `json:"run_url"`
	CandidateRef *string `json:"candidate_ref"`

	BenchmarkKind          string       `json:"benchmark_kind"`
	BenchmarkSchemaVersion int          `json:"benchmark_schema_version"`
	Runner                 *string      `json:"runner"`
	Environment            *Environment `json:"environment"`

	// LinkProfiles and RTTP50MS keep a chart from reading a slower line as a
	// slower release. RTTP50MS is the baseline profile's probed value and is
	// null for results measured before the probe existed.
	LinkProfiles []string `json:"link_profiles"`
	RTTP50MS     *float64 `json:"rtt_p50_ms"`

	Archived bool    `json:"archived"`
	JSON     string  `json:"json"`
	Markdown string  `json:"markdown"`
	CSV      *string `json:"csv,omitzero"`

	// Repeats is the per-cell sample count of a matrix sweep. It is omitted for
	// standard results, which answer a different question.
	Repeats *int `json:"repeats,omitzero"`
	// BelowAnalysisThreshold marks a matrix sweep whose repeats are too few for
	// MAD or a median-of-best cell to be meaningful under the lower-middle
	// median (issue #227). Stored results are never rewritten; the mark lives
	// only in the regenerated index.
	BelowAnalysisThreshold bool `json:"below_analysis_threshold,omitzero"`

	MedianMS Ordered[float64] `json:"median_ms"`
	BestMS   Ordered[float64] `json:"best_ms"`
}
