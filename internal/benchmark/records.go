package benchmark

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/eiserv/easySFTP/internal/benchmark/schema"
)

// The records below are exactly what the measuring scripts already append to
// their JSONL files. They are not a new interface: keeping them byte-for-byte
// what the shell writes today is what lets the aggregation move without the
// measurement path moving with it.

// RunRecord is one measured run. A standard run fills Refused and leaves the
// axis coordinates empty; a matrix cell does the opposite.
type RunRecord struct {
	Label       string `json:"label"`
	Ref         string `json:"ref"`
	Scenario    string `json:"scenario"`
	LinkProfile string `json:"link_profile"`

	Connections        *int `json:"connections"`
	Concurrency        *int `json:"concurrency"`
	RequestConcurrency *int `json:"request_concurrency"`

	Repeat     int             `json:"repeat"`
	ExitCode   int             `json:"exit_code"`
	DurationMS float64         `json:"duration_ms"`
	Files      float64         `json:"files"`
	Bytes      float64         `json:"bytes"`
	Retries    float64         `json:"retries"`
	Errors     float64         `json:"errors"`
	Refused    float64         `json:"refused"`
	Metrics    *schema.Metrics `json:"metrics"`
}

// DeleteRecord is one pre-clean, which is a pure delete sweep (issue #184,
// phase 4). Its coordinates are added by the caller that appended it, so a
// standard sweep and a matrix sweep read through the same type.
type DeleteRecord struct {
	Label       string `json:"label"`
	Scenario    string `json:"scenario"`
	LinkProfile string `json:"link_profile"`

	Connections        *int `json:"connections"`
	Concurrency        *int `json:"concurrency"`
	RequestConcurrency *int `json:"request_concurrency"`

	Repeat       int             `json:"repeat"`
	ExitCode     int             `json:"exit_code"`
	DurationMS   float64         `json:"duration_ms"`
	FilesDeleted float64         `json:"files_deleted"`
	Metrics      *schema.Metrics `json:"metrics"`
}

// CanaryRecord is one measurement of the fixed cell. It is stored verbatim:
// what it measures is the server's steadiness, and aggregating the three would
// hide exactly what they are there to show.
type CanaryRecord = schema.Canary

// ProbeRecord is one link probe, already wrapped with the profile it belongs to
// and whether it was taken before or after that profile's runs.
type ProbeRecord = schema.Probe

// readJSONL decodes one JSON document per line.
//
// A missing file is an empty measurement and not an error: a run without a
// probe binary stores no probes, and a standard run has neither canary nor auto
// runs. An unreadable *line* is an error, because that is a truncated
// measurement and silently dropping it would change the numbers.
func readJSONL[T any](path string) ([]T, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var out []T
	scanner := bufio.NewScanner(file)
	// A metrics document with a long operation list comfortably exceeds the
	// default 64 KiB line limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var value T
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		out = append(out, value)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

// Inputs is everything a run measured, read back off disk.
type Inputs struct {
	Runs    []RunRecord
	Deletes []DeleteRecord
	Canary  []CanaryRecord
	Auto    []RunRecord
	Probes  []ProbeRecord
}

// Load reads every JSONL file the manifest names.
func Load(m *Manifest) (*Inputs, error) {
	var in Inputs
	var err error
	if in.Runs, err = readJSONL[RunRecord](m.Files.Runs); err != nil {
		return nil, err
	}
	if in.Deletes, err = readJSONL[DeleteRecord](m.Files.Deletes); err != nil {
		return nil, err
	}
	if in.Canary, err = readJSONL[CanaryRecord](m.Files.Canary); err != nil {
		return nil, err
	}
	if in.Auto, err = readJSONL[RunRecord](m.Files.Auto); err != nil {
		return nil, err
	}
	if in.Probes, err = readJSONL[ProbeRecord](m.Files.Probes); err != nil {
		return nil, err
	}
	return &in, nil
}
