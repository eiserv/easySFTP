// Command easysftp-bench aggregates a benchmark run into the documents it
// stores.
//
// It is the Go half of the split issue #190 describes. The measuring scripts
// keep generating payloads, running easySFTP and shaping the link; they hand
// this command one manifest describing the run plus the JSONL they appended to,
// and it writes results.json/results.csv/summary.md or
// matrix.json/matrix.csv/matrix.md.
//
// It performs no measurement, opens no connection and reads no environment
// variable. Given the same inputs it writes the same bytes, which is what makes
// it comparable against the jq implementation it replaces.
//
// Usage:
//
//	easysftp-bench aggregate --manifest <path> --out <dir>
//	easysftp-bench validate <stored-result.json>...
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/eiserv/easySFTP/internal/benchmark"
	"github.com/eiserv/easySFTP/internal/benchmark/report"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "aggregate":
		err = aggregate(os.Args[2:])
	case "validate":
		err = validate(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		// The GitHub Actions annotation format, like every other error the
		// benchmark scripts produce.
		fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `easysftp-bench aggregates a benchmark run into its stored documents.

  aggregate --manifest <path> --out <dir>   write the result, the CSV and the summary
  validate <file>...                        check stored results against the schema
`)
}

func aggregate(args []string) error {
	flags := flag.NewFlagSet("aggregate", flag.ExitOnError)
	manifestPath := flags.String("manifest", "", "the run manifest the measuring script wrote")
	outDir := flags.String("out", "", "directory the result, CSV and summary are written to")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *manifestPath == "" || *outDir == "" {
		return fmt.Errorf("aggregate needs both --manifest and --out")
	}

	manifest, err := benchmark.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	inputs, err := benchmark.Load(manifest)
	if err != nil {
		return err
	}

	switch manifest.Kind {
	case schema.BenchmarkMatrix:
		return writeMatrix(manifest, inputs, *outDir)
	default:
		return writeStandard(manifest, inputs, *outDir)
	}
}

func writeStandard(m *benchmark.Manifest, in *benchmark.Inputs, outDir string) error {
	result := benchmark.BuildStandard(m, in)
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
	if err := writeJSON(filepath.Join(outDir, "results.json"), result); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(outDir, "results.csv"), summary.CSV()); err != nil {
		return err
	}
	return writeFile(filepath.Join(outDir, "summary.md"), summary.Markdown())
}

func writeMatrix(m *benchmark.Manifest, in *benchmark.Inputs, outDir string) error {
	result := benchmark.BuildMatrix(m, in)
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
	if err := writeJSON(filepath.Join(outDir, "matrix.json"), result); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(outDir, "matrix.csv"), summary.CSV()); err != nil {
		return err
	}
	return writeFile(filepath.Join(outDir, "matrix.md"), summary.Markdown())
}

func scenarioNames(m *benchmark.Manifest) []string {
	names := make([]string, 0, len(m.Scenarios))
	for _, s := range m.Scenarios {
		names = append(names, s.Name)
	}
	return names
}

func matrixScenarios(m *benchmark.Manifest) []report.MatrixScenario {
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

// validate checks stored results against the schema. It is what keeps "current
// stored results remain readable" from being a claim nobody runs.
func validate(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("validate needs at least one file")
	}
	failures := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			failures++
			continue
		}
		env, err := schema.DecodeStrict[schema.Envelope](data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			failures++
			continue
		}
		if err := env.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			failures++
			continue
		}
		fmt.Printf("%s: ok\n", path)
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d stored result(s) did not validate", failures, len(paths))
	}
	return nil
}

// writeJSON writes a document the way jq does: two-space indentation, a
// trailing newline, and no HTML escaping.
//
// That last part is the reason this does not use json.MarshalIndent, which
// rewrites the three HTML-significant characters as unicode escapes. jq leaves
// them alone, and a ref or a label carrying one of them would otherwise make a
// Go-written result differ from the jq-written results already stored, for no
// reason a reader could make sense of.
func writeJSON(path string, value any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return err
	}
	// Encode already ends the document with a newline.
	return writeFile(path, buf.String())
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
