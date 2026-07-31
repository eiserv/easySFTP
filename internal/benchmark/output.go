package benchmark

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/eiserv/easySFTP/internal/benchmark/report"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
)

// Write turns one measured run into the three documents it stores, choosing the
// pair of aggregations by the manifest's kind. It returns the name of the
// summary the caller prints.
func Write(m *Manifest, in *Inputs, outDir string) (string, error) {
	if m.Kind == schema.BenchmarkMatrix {
		return "matrix.md", WriteMatrix(m, in, outDir)
	}
	return "summary.md", WriteStandard(m, in, outDir)
}

// WriteStandard writes results.json, results.csv and summary.md.
func WriteStandard(m *Manifest, in *Inputs, outDir string) error {
	result := BuildStandard(m, in)
	summary := report.Standard{
		Result:        result,
		CandidateRef:  m.CandidateRef,
		BaselineRef:   m.BaselineRef,
		Repeats:       m.Repeats,
		RunnerDisplay: m.RunnerDisplay,
		LinkRequested: m.Link.Requested,
		Settings:      m.Settings,
		Labels:        m.Labels,
		Scenarios:     scenarioNames(m),
		LinkProfiles:  m.Link.Profiles,
	}
	if err := WriteJSON(filepath.Join(outDir, "results.json"), result); err != nil {
		return err
	}
	if err := WriteFile(filepath.Join(outDir, "results.csv"), summary.CSV()); err != nil {
		return err
	}
	return WriteFile(filepath.Join(outDir, "summary.md"), summary.Markdown())
}

// WriteMatrix writes matrix.json, matrix.csv and matrix.md.
func WriteMatrix(m *Manifest, in *Inputs, outDir string) error {
	result := BuildMatrix(m, in)
	summary := report.Matrix{
		Result:             result,
		CandidateRef:       m.CandidateRef,
		BaselineRef:        m.BaselineRef,
		Repeats:            m.Repeats,
		RunnerDisplay:      m.RunnerDisplay,
		LinkRequested:      m.Link.Requested,
		ConnectionsDisplay: m.Grid.ConnectionsDisplay,
		ConcurrencyDisplay: m.Grid.ConcurrencyDisplay,
		RequestDisplay:     m.Grid.RequestDisplay,
		Labels:             m.Labels,
		LinkProfiles:       m.Link.Profiles,
		Scenarios:          matrixScenarios(m),
		Canary: schema.Canary{
			Scenario:    m.Grid.Canary.Scenario,
			Connections: m.Grid.Canary.Connections,
			Concurrency: m.Grid.Canary.Concurrency,
		},
		HasBaseline: m.BaselineRef != "",
	}
	if err := WriteJSON(filepath.Join(outDir, "matrix.json"), result); err != nil {
		return err
	}
	if err := WriteFile(filepath.Join(outDir, "matrix.csv"), summary.CSV()); err != nil {
		return err
	}
	return WriteFile(filepath.Join(outDir, "matrix.md"), summary.Markdown())
}

func scenarioNames(m *Manifest) []string {
	names := make([]string, 0, len(m.Scenarios))
	for _, s := range m.Scenarios {
		names = append(names, s.Name)
	}
	return names
}

func matrixScenarios(m *Manifest) []report.MatrixScenario {
	out := make([]report.MatrixScenario, 0, len(m.Scenarios))
	for _, s := range m.Scenarios {
		out = append(out, report.MatrixScenario{
			Name:               s.Name,
			Description:        s.Description,
			Mode:               s.Mode,
			ConnectionsDisplay: s.ConnectionsDisplay,
			ConcurrencyDisplay: s.ConcurrencyDisplay,
			RequestDisplay:     s.RequestDisplay,
			Connections:        s.Connections,
			Concurrency:        s.Concurrency,
			RequestConcurrency: s.RequestConcurrency,
		})
	}
	return out
}

// WriteJSON writes a document the way jq does: two-space indentation, a
// trailing newline, and no HTML escaping.
//
// That last part is the reason this does not use json.MarshalIndent, which
// rewrites the three HTML-significant characters as unicode escapes. jq leaves
// them alone, and a ref or a label carrying one of them would otherwise make a
// Go-written result differ from the jq-written results already stored, for no
// reason a reader could make sense of.
func WriteJSON(path string, value any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return err
	}
	// Encode already ends the document with a newline.
	return WriteFile(path, buf.String())
}

// WriteFile writes a text document, creating the directory it goes in.
func WriteFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
