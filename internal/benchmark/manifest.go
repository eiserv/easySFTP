// Package benchmark turns what a benchmark run measured into the documents it
// stores.
//
// It is the second half of the split issue #190 describes: the shell scripts
// generate payloads, run easySFTP and shape the link, and everything downstream
// of the JSONL they append to lives here. The measurement path is deliberately
// untouched by that move, because a rewrite that also changes what is measured
// cannot be reviewed.
//
// The scripts hand over one manifest describing the run, and the JSONL files
// they wrote. Nothing here runs a process, opens a connection or reads the
// environment: given the same inputs it produces the same bytes, which is what
// makes the parity check against the jq implementation meaningful at all.
package benchmark

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/eiserv/easySFTP/internal/benchmark/schema"
)

// Manifest is what a measuring script knows and the aggregation cannot derive:
// which refs were built, which axes were asked for, which display strings the
// summary tables print, and where the JSONL it appended to lives.
//
// Display strings travel verbatim rather than being rebuilt here. The scripts
// already have them, and a reformatted axis list in a summary would be a change
// to a stored document made by accident (issue #190: behavioural rewrite first,
// improvements separately).
type Manifest struct {
	// Kind is "standard" or "matrix" and decides which aggregation runs.
	Kind string `json:"kind"`

	CandidateRef   string `json:"candidate_ref"`
	BaselineRef    string `json:"baseline_ref"`
	ReferenceLabel string `json:"reference_label"`
	Repeats        int    `json:"repeats"`

	// Runner is the value stored in the result; RunnerDisplay is the shorter
	// one the summary table prints. The two differ, and reconstructing either
	// from the other would be guesswork.
	Runner        string `json:"runner"`
	RunnerDisplay string `json:"runner_display"`

	Environment *schema.Environment `json:"environment"`
	Settings    string              `json:"settings"`
	Note        string              `json:"note"`

	// Labels is the build order the summary tables loop in, and Scenarios the
	// scenario order. Both are the script's own order, which is not the order
	// the aggregated rows come out in.
	Labels    []string           `json:"labels"`
	Scenarios []ManifestScenario `json:"scenarios"`

	Link  ManifestLink  `json:"link"`
	Files ManifestFiles `json:"files"`
	Grid  *ManifestGrid `json:"grid,omitzero"`
}

// ManifestScenario is one scenario as the run measured it: what it is, how it
// was deployed, and which axis values its payload could actually use.
type ManifestScenario struct {
	Name        string `json:"name"`
	Description string `json:"description"`

	// Mode is the display string of the scenario's deploy shape, already
	// carrying the ", redeployed" a prepopulated scenario adds.
	Mode string `json:"mode,omitzero"`

	Files int `json:"files,omitzero"`

	// The axes as the summary prints them, and as the result stores them. A
	// matrix run fills these; a standard run has no grid.
	ConnectionsDisplay string `json:"connections_display,omitzero"`
	ConcurrencyDisplay string `json:"concurrency_display,omitzero"`
	RequestDisplay     string `json:"request_display,omitzero"`
	Connections        []int  `json:"connections,omitzero"`
	Concurrency        []int  `json:"concurrency,omitzero"`
	RequestConcurrency []*int `json:"request_concurrency,omitzero"`
}

// ManifestLink is the shaping state the script owns. The probes themselves are
// read from the probe file, so a run without a probe binary simply has none.
type ManifestLink struct {
	Iface    *string        `json:"iface"`
	Shaping  schema.Shaping `json:"shaping"`
	Profiles []string       `json:"profiles"`
	// Requested is the raw input string the summary prints, empty when the run
	// measured the real line only.
	Requested string `json:"requested,omitzero"`
}

// ManifestFiles names the JSONL the scripts appended to. Absent paths are an
// empty measurement rather than an error: a standard run has no canary file and
// no auto file, and a run without a probe binary has no probes.
type ManifestFiles struct {
	Runs    string `json:"runs"`
	Deletes string `json:"deletes,omitzero"`
	Probes  string `json:"probes,omitzero"`
	Canary  string `json:"canary,omitzero"`
	Auto    string `json:"auto,omitzero"`
}

// ManifestGrid is the declared grid of a matrix run, i.e. what was asked for
// before each scenario's payload capped it.
type ManifestGrid struct {
	Connections        []int  `json:"connections"`
	Concurrency        []int  `json:"concurrency"`
	RequestConcurrency []*int `json:"request_concurrency"`

	// The raw input strings, for the settings table.
	ConnectionsDisplay string `json:"connections_display"`
	ConcurrencyDisplay string `json:"concurrency_display"`
	RequestDisplay     string `json:"request_display"`

	Canary ManifestCanary `json:"canary"`
}

// ManifestCanary is the fixed cell, which is a constant in the script and has
// to be printed by the summary that explains it.
type ManifestCanary struct {
	Scenario    string `json:"scenario"`
	Connections int    `json:"connections"`
	Concurrency int    `json:"concurrency"`
}

// LoadManifest reads and validates a manifest.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	switch m.Kind {
	case schema.BenchmarkStandard, schema.BenchmarkMatrix:
	default:
		return nil, fmt.Errorf("%s: unknown benchmark kind %q", path, m.Kind)
	}
	if m.Files.Runs == "" {
		return nil, fmt.Errorf("%s: the manifest names no runs file", path)
	}
	if m.Kind == schema.BenchmarkMatrix && m.Grid == nil {
		return nil, fmt.Errorf("%s: a matrix run must declare its grid", path)
	}
	return &m, nil
}

// ScenarioDocs is the scenarios map a stored result carries.
func (m *Manifest) ScenarioDocs() map[string]string {
	docs := make(map[string]string, len(m.Scenarios))
	for _, s := range m.Scenarios {
		docs[s.Name] = s.Description
	}
	return docs
}
