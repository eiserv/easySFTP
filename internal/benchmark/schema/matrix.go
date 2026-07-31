package schema

// MatrixSchemaVersion is the current version of a matrix measurement.
const MatrixSchemaVersion = 2

// Matrix is matrix.json: the sweep scripts/benchmark-matrix.sh produces.
//
// A matrix run has no runs[]. Its cell is the finest grain it keeps, which is
// why the cell itself carries phases and round-trip percentiles rather than
// only the upload_phase_ms it used to (issue #184, phase 2).
type Matrix struct {
	SchemaVersion  int               `json:"schema_version,omitzero"`
	BenchmarkKind  string            `json:"benchmark_kind,omitzero"`
	CandidateRef   string            `json:"candidate_ref"`
	BaselineRef    string            `json:"baseline_ref"`
	ReferenceLabel string            `json:"reference_label"`
	Repeats        int               `json:"repeats"`
	Runner         string            `json:"runner"`
	Environment    *Environment      `json:"environment,omitzero"`
	Link           *Link             `json:"link,omitzero"`
	Canary         []Canary          `json:"canary,omitzero"`
	Settings       string            `json:"settings"`
	Scenarios      map[string]string `json:"scenarios"`
	Note           string            `json:"note,omitzero"`

	Axes       Axes            `json:"axes"`
	Cells      []Cell          `json:"cells"`
	Deletes    []MatrixDelete  `json:"deletes,omitzero"`
	Scaling    []Scaling       `json:"scaling"`
	Auto       []Auto          `json:"auto,omitzero"`
	Comparison []MatrixCompare `json:"comparison"`
}

// Axes is the grid as requested, plus what each scenario was actually measured
// over. The two are kept apart on purpose: a cell missing from the declared
// grid was not skipped, it would have been the same configuration twice
// (issue #184, phase 5).
type Axes struct {
	LinkProfiles       []string                `json:"link_profiles"`
	Connections        []int                   `json:"connections"`
	Concurrency        []int                   `json:"concurrency"`
	RequestConcurrency []*int                  `json:"request_concurrency"`
	PerScenario        map[string]ScenarioAxes `json:"per_scenario,omitzero"`
}

// ScenarioAxes is one scenario's own grid, and the file count both axes were
// capped against.
//
// A null in RequestConcurrency is the pass that sets nothing and leaves
// easySFTP its own value.
type ScenarioAxes struct {
	Files              int    `json:"files"`
	Connections        []int  `json:"connections"`
	Concurrency        []int  `json:"concurrency"`
	RequestConcurrency []*int `json:"request_concurrency"`
}

// Cell is one row per (link profile, scenario, build, connections, concurrency,
// request_concurrency).
type Cell struct {
	Scenario    string `json:"scenario"`
	Label       string `json:"label"`
	Ref         string `json:"ref"`
	LinkProfile string `json:"link_profile,omitzero"`
	Connections int    `json:"connections"`
	Concurrency int    `json:"concurrency"`

	// RequestConcurrency is the coordinate that was asked for, null on the pass
	// that sets nothing. RequestConcurrencyUsed is what the run actually ran
	// with, read back from its own counters, because that null alone does not
	// say which value it was.
	RequestConcurrency *int `json:"request_concurrency"`

	Repeats     int       `json:"repeats"`
	FailedRuns  int       `json:"failed_runs"`
	Files       float64   `json:"files"`
	Bytes       float64   `json:"bytes"`
	DurationsMS []float64 `json:"durations_ms"`
	MedianMS    float64   `json:"median_ms"`
	MinMS       float64   `json:"min_ms"`
	MaxMS       float64   `json:"max_ms"`
	MadMS       *float64  `json:"mad_ms"`
	Retries     float64   `json:"retries"`
	Errors      float64   `json:"errors"`

	UserCPUMS              *float64 `json:"user_cpu_ms"`
	SysCPUMS               *float64 `json:"sys_cpu_ms"`
	CPUPercent             *float64 `json:"cpu_percent"`
	MaxRSSBytes            *float64 `json:"max_rss_bytes"`
	GoGCCount              *float64 `json:"go_gc_count"`
	GoPeakGoroutines       *float64 `json:"go_peak_goroutines"`
	NetWriteBytes          *float64 `json:"net_write_bytes"`
	ConnectionsOpened      *float64 `json:"connections_opened"`
	ConnectionsUsed        *float64 `json:"connections_used"`
	RequestConcurrencyUsed *float64 `json:"request_concurrency_used"`
	ConnectionsRefused     *float64 `json:"connections_refused"`
	Reconnects             *float64 `json:"reconnects"`
	UploadPhaseMS          *float64 `json:"upload_phase_ms"`

	Phases     []Phase     `json:"phases"`
	Operations []Operation `json:"operations"`

	MiBPerS   float64 `json:"mib_per_s"`
	FilesPerS float64 `json:"files_per_s"`
}

// MatrixDelete is a delete sweep at a cell's coordinates.
type MatrixDelete struct {
	Scenario           string `json:"scenario"`
	Label              string `json:"label"`
	LinkProfile        string `json:"link_profile"`
	Connections        int    `json:"connections"`
	Concurrency        int    `json:"concurrency"`
	RequestConcurrency *int   `json:"request_concurrency"`
	DeleteStats
}

// Scaling is the cells of one (link profile, scenario, build) pre-grouped into
// the curve a reader usually wants, ordered along the axes.
type Scaling struct {
	Scenario    string  `json:"scenario"`
	Label       string  `json:"label"`
	LinkProfile string  `json:"link_profile,omitzero"`
	Points      []Point `json:"points"`
	Best        Best    `json:"best"`

	// BestAtAxisMax names the axes whose largest swept value is the best cell.
	// Where it is non-empty the optimum was cut off rather than measured, and
	// anything fitted to those numbers extrapolates.
	BestAtAxisMax []string `json:"best_at_axis_max,omitzero"`
}

// Point is one cell reduced to what a scaling curve plots.
type Point struct {
	Connections            int      `json:"connections"`
	Concurrency            int      `json:"concurrency"`
	RequestConcurrency     *int     `json:"request_concurrency"`
	RequestConcurrencyUsed *float64 `json:"request_concurrency_used"`
	MedianMS               float64  `json:"median_ms"`
	MiBPerS                float64  `json:"mib_per_s"`
	FilesPerS              float64  `json:"files_per_s"`
	ConnectionsUsed        *float64 `json:"connections_used"`
	ConnectionsRefused     *float64 `json:"connections_refused"`
	MaxRSSBytes            *float64 `json:"max_rss_bytes"`
	UserCPUMS              *float64 `json:"user_cpu_ms"`
}

// Best is the fastest cell of a group.
type Best struct {
	Connections        int     `json:"connections"`
	Concurrency        int     `json:"concurrency"`
	RequestConcurrency *int    `json:"request_concurrency"`
	MedianMS           float64 `json:"median_ms"`
	MiBPerS            float64 `json:"mib_per_s"`
	FilesPerS          float64 `json:"files_per_s"`
}

// Auto is the settings easySFTP picked for itself, scored against the best cell
// of the same scenario and link profile (issue #184, phase 5).
//
// It is not a cell: auto chooses a coordinate rather than sitting at one, which
// is why it stays out of cells[], scaling[], comparison[] and the CSV.
type Auto struct {
	Scenario    string `json:"scenario"`
	Label       string `json:"label"`
	Ref         string `json:"ref"`
	LinkProfile string `json:"link_profile"`

	Repeats     int       `json:"repeats"`
	FailedRuns  int       `json:"failed_runs"`
	Files       float64   `json:"files"`
	Bytes       float64   `json:"bytes"`
	DurationsMS []float64 `json:"durations_ms"`
	MedianMS    float64   `json:"median_ms"`
	MinMS       float64   `json:"min_ms"`
	MaxMS       float64   `json:"max_ms"`
	MadMS       *float64  `json:"mad_ms"`

	// Chosen is read back from the run's own counters, so it says what easySFTP
	// did rather than what the script believes it does.
	Chosen Chosen `json:"chosen"`

	MiBPerS   float64 `json:"mib_per_s"`
	FilesPerS float64 `json:"files_per_s"`

	Best *Best `json:"best"`

	// ChosenInGrid and ChosenCellMedianMS are a control, not a result: the same
	// settings measured a second time as an ordinary cell. A large gap between
	// the two means the runs saw different conditions, and the regret next to it
	// is then drift rather than policy.
	ChosenInGrid       bool     `json:"chosen_in_grid"`
	ChosenCellMedianMS *float64 `json:"chosen_cell_median_ms"`
	RegretMS           *float64 `json:"regret_ms"`
	RegretPercent      *float64 `json:"regret_percent"`
}

// Chosen is what an auto run resolved its three knobs to.
type Chosen struct {
	Connections        *float64 `json:"connections"`
	Concurrency        *float64 `json:"concurrency"`
	RequestConcurrency *float64 `json:"request_concurrency"`
}

// MatrixCompare pairs a build's cell with the reference build's cell at
// identical coordinates, including the same link profile.
type MatrixCompare struct {
	Scenario           string   `json:"scenario"`
	Label              string   `json:"label"`
	ReferenceLabel     string   `json:"reference_label"`
	LinkProfile        string   `json:"link_profile,omitzero"`
	Connections        int      `json:"connections"`
	Concurrency        int      `json:"concurrency"`
	RequestConcurrency *int     `json:"request_concurrency"`
	MedianMS           float64  `json:"median_ms"`
	ReferenceMedianMS  float64  `json:"reference_median_ms"`
	DeltaMS            float64  `json:"delta_ms"`
	DeltaPercent       *float64 `json:"delta_percent"`
}

// Canary is the fixed cell, measured at the start, the middle and the end of
// each profile's grid. Read it before reading the grid: when the spread between
// the three is larger than the deltas in the grid, the run measured drift and
// not settings.
type Canary struct {
	LinkProfile string  `json:"link_profile"`
	At          string  `json:"at"`
	Scenario    string  `json:"scenario"`
	Connections int     `json:"connections"`
	Concurrency int     `json:"concurrency"`
	ExitCode    int     `json:"exit_code"`
	DurationMS  float64 `json:"duration_ms"`
	Files       float64 `json:"files"`
	Bytes       float64 `json:"bytes"`
}
