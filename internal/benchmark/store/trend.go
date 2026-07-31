package store

import (
	"path/filepath"
	"strings"

	"github.com/eiserv/easySFTP/internal/benchmark"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

// trendColumns is one row per (stored standard result, scenario, link profile),
// so a plot of runtime and throughput across releases needs neither a JSON
// parser nor a pass over every file. index.json carries the medians but not the
// throughput, which is exactly the second axis such a plot wants. Matrix
// results are left out: their point is that they have no single number per
// scenario.
//
// link_profile, rtt_p50_ms and control_single_mib_per_s ride along because a
// trend line across months is otherwise a trend of the line as much as of the
// code. A result measured before the probe existed has one row per scenario
// with the profile empty, exactly as before.
var trendColumns = []string{
	"recorded_at", "kind", "version", "label", "candidate_ref", "runner", "scenario",
	"link_profile", "rtt_p50_ms", "control_single_mib_per_s", "files", "bytes",
	"median_ms", "min_ms", "max_ms", "mad_ms", "mib_per_s", "files_per_s",
	"max_rss_bytes", "user_cpu_ms", "archived", "json",
}

func writeTrend(root string, entries []stored) error {
	var out strings.Builder
	out.WriteString(quoteAll(trendColumns))
	out.WriteByte('\n')

	for _, entry := range entries {
		if entry.kind == schema.BenchmarkMatrix {
			continue
		}
		s, err := entry.envelope.Standard()
		if err != nil {
			return err
		}
		var probes []schema.Probe
		if s.Link != nil {
			probes = s.Link.Probes
		}
		for _, result := range s.Results {
			if result.Label != "candidate" {
				continue
			}
			profile := result.LinkProfile
			if profile == "" {
				profile = "baseline"
			}
			probe := startProbe(probes, profile)

			var rtt, control *float64
			if probe != nil {
				if probe.RTTMS != nil {
					rtt = probe.RTTMS.P50
				}
				if probe.Control != nil {
					control = probe.Control.SingleStreamMiBPerS
				}
			}
			var maxRSS, userCPU *float64
			if result.Process != nil {
				maxRSS, userCPU = result.Process.MaxRSSBytes, result.Process.UserCPUMS
			}

			out.WriteString(strings.Join([]string{
				quote(entry.envelope.RecordedAt),
				quote(entry.envelope.Kind),
				quoteOrNull(entry.envelope.Version),
				quoteOrNull(entry.envelope.Label),
				quoteOrNull(nullable(s.CandidateRef)),
				quoteOrNull(nullable(s.Runner)),
				quote(result.Scenario),
				quoteOrNull(nullable(result.LinkProfile)),
				number(rtt),
				number(control),
				stats.Format(result.Files),
				stats.Format(result.Bytes),
				stats.Format(result.MedianMS),
				stats.Format(result.MinMS),
				stats.Format(result.MaxMS),
				number(result.MadMS),
				stats.Format(result.MiBPerS),
				stats.Format(result.FilesPerS),
				number(maxRSS),
				number(userCPU),
				boolean(entry.archived),
				quote(entry.path),
			}, ","))
			out.WriteByte('\n')
		}
	}
	return benchmark.WriteFile(filepath.Join(root, "trend.csv"), out.String())
}

// startProbe is the opening probe of the profile a row was measured on. A row
// whose profile was never probed keeps its link columns empty rather than
// borrowing another profile's numbers.
func startProbe(probes []schema.Probe, profile string) *schema.Probe {
	for i := range probes {
		if probes[i].Profile == profile && probes[i].At == "start" {
			return &probes[i]
		}
	}
	return nil
}

// The four renderers below are jq's "@csv": strings are quoted with inner
// quotes doubled, numbers and booleans are bare, and null is an empty column.
// An empty column says "this metric did not exist yet", and a 0 there would be
// a measurement that was never taken.
func quote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quoteOrNull(value *string) string {
	if value == nil {
		return ""
	}
	return quote(*value)
}

func number(value *float64) string {
	if value == nil {
		return ""
	}
	return stats.Format(*value)
}

func boolean(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func quoteAll(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, quote(value))
	}
	return strings.Join(quoted, ",")
}
