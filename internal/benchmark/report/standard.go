package report

import (
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

// Standard is what the summary of a standard run needs beyond the measurement
// itself: the display strings the script owns and the order it loops in.
type Standard struct {
	Result *schema.Standard

	CandidateRef  string
	BaselineRef   string
	Repeats       int
	RunnerDisplay string
	LinkRequested string
	Settings      string

	Labels       []string
	Scenarios    []string
	LinkProfiles []string
}

// CSV is results.csv: one flat row per (build, scenario), so a spreadsheet or a
// plot can read a stored result without a JSON parser.
//
// link_profile, rtt_p50_ms and control_single_mib_per_s make a row readable on
// its own: without them a throughput number cannot be told apart from the line
// it was measured on. The two link numbers come from the profile's own start
// probe, which is the one taken right before these runs.
func (s Standard) CSV() string {
	var b buf
	b.line("%s", csvRow(
		"scenario", "build", "ref", "link_profile", "rtt_p50_ms", "control_single_mib_per_s",
		"repeats", "files", "bytes", "median_ms", "min_ms", "max_ms", "mad_ms",
		"mib_per_s", "files_per_s", "user_cpu_ms", "sys_cpu_ms", "cpu_percent", "max_rss_bytes",
		"go_gc_count", "go_peak_goroutines", "net_write_bytes", "connections_opened", "connections_refused",
		"retries", "errors", "failed_runs"))

	for _, row := range s.Result.Results {
		probe := startProbe(s.Result.Link, row.LinkProfile)
		p := row.Process
		if p == nil {
			p = &schema.Process{}
		}
		b.line("%s", csvRow(
			row.Scenario, row.Label, row.Ref, row.LinkProfile,
			rttP50(probe), controlSingle(probe),
			row.Repeats, row.Files, row.Bytes, row.MedianMS, row.MinMS, row.MaxMS, row.MadMS,
			row.MiBPerS, row.FilesPerS, p.UserCPUMS, p.SysCPUMS, p.CPUPercent,
			p.MaxRSSBytes, p.GoGCCount, p.GoPeakGoroutines, p.NetWriteBytes,
			counter(row.Counters, "connections_opened"), counter(row.Counters, "connections_refused"),
			row.Retries, row.Errors, row.FailedRuns))
	}
	return b.String()
}

// Markdown is summary.md: the same numbers, readable.
func (s Standard) Markdown() string {
	var b buf
	result := s.Result

	b.line("## easySFTP benchmark")
	b.line("")
	b.line("| Setting | Value |")
	b.line("|---|---|")
	b.line("| Candidate | `%s` |", s.CandidateRef)
	b.line("| Baseline | `%s` |", orNone(s.BaselineRef))
	b.line("| Repeats per scenario | %d |", s.Repeats)
	b.line("| Runner | %s |", s.RunnerDisplay)
	b.line("| Link profiles | %s |", orRealLine(s.LinkRequested))
	b.line("| Settings | %s |", s.Settings)
	b.line("")
	linkSection(&b, result.Link)

	b.line("### Throughput")
	b.line("")
	b.line("| Scenario | Build | Profile | Files | Size | Median | Min | Max | MAD | MiB/s | files/s | Retries | Errors | Failed runs | Delta |")
	b.line("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
	// Looped in the script's own order rather than in the aggregation's, so the
	// table reads scenario by scenario. A combination that was not measured
	// prints no row at all.
	s.each(func(row *schema.Result) {
		b.line("| %s | %s | %s | %s | %s MiB | %s ms | %s ms | %s ms | %s | %s | %s | %s | %s | %d | %s |",
			row.Scenario, row.Label, row.LinkProfile, num(row.Files), mib(row.Bytes),
			num(row.MedianMS), num(row.MinMS), num(row.MaxMS), ms(row.MadMS),
			num(row.MiBPerS), num(row.FilesPerS), num(row.Retries), num(row.Errors), row.FailedRuns,
			percent(deltaOf(result.Comparison, row)))
	})
	b.line("")
	b.line("Delta compares each build's median against the `%s` build **on the same link profile**; negative is faster. MAD is the median absolute deviation of the repeats: a delta smaller than it is inside this host's own noise.", result.ReferenceLabel)
	b.line("")
	b.line("### Resources (median per run)")
	b.line("")
	b.line("%s", "| Scenario | Build | Profile | User CPU | Sys CPU | CPU % | Peak RSS | Go allocs | GCs | GC pause | Peak goroutines | Net sent |")
	b.line("|---|---|---|---|---|---|---|---|---|---|---|---|")
	s.each(func(row *schema.Result) {
		p := row.Process
		if p == nil {
			p = &schema.Process{}
		}
		b.line("| %s | %s | %s | %s ms | %s ms | %s%% | %s MiB | %s MiB | %s | %s ms | %s | %s MiB |",
			row.Scenario, row.Label, row.LinkProfile,
			raw(p.UserCPUMS), raw(p.SysCPUMS), raw(p.CPUPercent),
			mibOf(p.MaxRSSBytes), mibOf(p.GoTotalAllocBytes), raw(p.GoGCCount),
			raw(p.GoGCPauseTotalMS), raw(p.GoPeakGoroutines), mibOf(p.NetWriteBytes))
	})

	b.line("")
	b.line("### Where the time goes")
	b.line("")
	b.line("Phases are wall clock and add up to roughly the run's duration. Operation totals are **cumulative across parallel workers** and are normally larger than the phase they belong to; read them for their share and their per-call cost, never as wall clock.")
	b.line("")
	for _, scenario := range s.Scenarios {
		b.line("<details><summary><code>%s</code> phases and round-trips</summary>", scenario)
		b.line("")
		b.line("| Build | Profile | Phase | Wall |")
		b.line("|---|---|---|---|")
		for _, row := range result.Results {
			if row.Scenario != scenario {
				continue
			}
			for _, phase := range row.Phases {
				if phase.MedianMS <= 0 {
					continue
				}
				b.line("| %s | %s | %s | %s ms |", row.Label, row.LinkProfile, phase.Name, num(phase.MedianMS))
			}
		}
		b.line("")
		b.line("| Build | Profile | Operation | Count | Cumulative | Avg | p50 | p90 | p99 | Max |")
		b.line("|---|---|---|---|---|---|---|---|---|---|")
		for _, row := range result.Results {
			if row.Scenario != scenario {
				continue
			}
			for _, op := range row.Operations {
				if stats.Or(op.Count, 0) <= 0 {
					continue
				}
				b.line("| %s | %s | %s | %s | %s ms | %s ms | %s ms | %s ms | %s ms | %s ms |",
					row.Label, row.LinkProfile, op.Name, raw(op.Count), raw(op.MedianTotalMS),
					raw(op.AvgMS), raw(op.P50MS), raw(op.P90MS), raw(op.P99MS), raw(op.MaxMS))
			}
		}
		b.line("")
		b.line("</details>")
		b.line("")
	}

	b.line("### Delete sweeps")
	b.line("")
	b.line("The pre-clean before every measured run wipes the tree the previous repeat left behind, which makes it a pure delete sweep. It costs no extra time (it has always run) and its numbers never enter the upload tables above. Sweeps that found an empty directory are not counted.")
	b.line("")
	b.line("| Scenario | Build | Profile | Sweeps | Files deleted | Median | files/s | remote_scan | delete_sweep |")
	b.line("|---|---|---|---|---|---|---|---|---|")
	for _, d := range result.Deletes {
		b.line("| %s | %s | %s | %d | %s | %s ms | %s | %s | %s |",
			d.Scenario, d.Label, d.LinkProfile, d.Sweeps, num(d.FilesDeleted),
			num(d.MedianMS), num(d.DeletesPerS),
			ms(phaseOf(d.Phases, "remote_scan")), ms(phaseOf(d.Phases, "delete_sweep")))
	}
	b.line("")
	b.line("| Scenario | Build | Profile | Operation | Count | Cumulative | p50 | p90 | p99 | Max |")
	b.line("|---|---|---|---|---|---|---|---|---|---|")
	// sftp_remove and sftp_rmdir are the numbers issue #157 is about: one
	// round-trip per entry, strictly sequential, no concurrency anywhere near
	// them.
	for _, d := range result.Deletes {
		for _, op := range d.Operations {
			if stats.Or(op.Count, 0) <= 0 {
				continue
			}
			b.line("| %s | %s | %s | %s | %s | %s ms | %s ms | %s ms | %s ms | %s ms |",
				d.Scenario, d.Label, d.LinkProfile, op.Name, raw(op.Count), raw(op.MedianTotalMS),
				raw(op.P50MS), raw(op.P90MS), raw(op.P99MS), raw(op.MaxMS))
		}
	}
	b.line("")

	// Without this line a pool run whose extra connections the server refused
	// reads as "the pool did nothing", which is the wrong conclusion entirely.
	var refused float64
	for _, row := range result.Results {
		refused += stats.Or(row.RefusedConnections, 0)
	}
	if refused != 0 {
		b.line("**%s connection(s) were refused by the server** and fell back to the run's first connection, so a pool build measured here had fewer connections than configured.", num(refused))
		b.line("")
	}
	b.line("Data only: these numbers set no threshold and fail no build. Collected to evaluate the single-connection ceiling discussed in issue #158 and to show where a run spends its time.")
	return b.String()
}

// each walks the results in the script's loop order: scenario, then build, then
// link profile.
func (s Standard) each(fn func(*schema.Result)) {
	for _, scenario := range s.Scenarios {
		for _, label := range s.Labels {
			for _, profile := range s.LinkProfiles {
				for i := range s.Result.Results {
					row := &s.Result.Results[i]
					if row.Scenario == scenario && row.Label == label && row.LinkProfile == profile {
						fn(row)
					}
				}
			}
		}
	}
}

// deltaOf finds a row's entry in comparison[]. The reference build has none,
// and its Delta column is then "-".
func deltaOf(comparisons []schema.Comparison, row *schema.Result) *float64 {
	for _, c := range comparisons {
		if c.Scenario == row.Scenario && c.Label == row.Label && c.LinkProfile == row.LinkProfile {
			return c.DeltaPercent
		}
	}
	return nil
}

func phaseOf(phases []schema.Phase, name string) *float64 {
	for _, p := range phases {
		if p.Name == name {
			value := p.MedianMS
			return &value
		}
	}
	return nil
}

func opOf(ops []schema.Operation, name string) *float64 {
	for _, o := range ops {
		if o.Name == name {
			return o.P50MS
		}
	}
	return nil
}

func counter(c schema.Counters, name string) float64 {
	if c == nil {
		return 0
	}
	return stats.Or(c[name], 0)
}

func startProbe(link *schema.Link, profile string) *schema.Probe {
	if link == nil {
		return nil
	}
	for i := range link.Probes {
		if link.Probes[i].Profile == profile && link.Probes[i].At == "start" {
			return &link.Probes[i]
		}
	}
	return nil
}

func rttP50(p *schema.Probe) *float64 {
	if p == nil || p.RTTMS == nil {
		return nil
	}
	return p.RTTMS.P50
}

func controlSingle(p *schema.Probe) *float64 {
	if p == nil || p.Control == nil {
		return nil
	}
	return p.Control.SingleStreamMiBPerS
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func orRealLine(s string) string {
	if s == "" {
		return "the real line"
	}
	return s
}
