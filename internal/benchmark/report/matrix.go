package report

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

// Matrix is what the sweep's summary needs beyond the measurement: the display
// strings the script owns and the order it loops in.
type Matrix struct {
	Result *schema.Matrix

	CandidateRef  string
	BaselineRef   string
	Repeats       int
	RunnerDisplay string
	LinkRequested string

	ConnectionsDisplay string
	ConcurrencyDisplay string
	RequestDisplay     string

	Labels       []string
	LinkProfiles []string
	Scenarios    []MatrixScenario

	Canary      schema.Canary
	HasBaseline bool
}

// MatrixScenario is one scenario as the summary prints it: what it is, how it
// was deployed, and the axis values it was actually swept over.
type MatrixScenario struct {
	Name               string
	Description        string
	Mode               string
	ConnectionsDisplay string
	ConcurrencyDisplay string
	RequestDisplay     string
	Connections        []int
	Concurrency        []int
	RequestConcurrency []*int
}

// CSV is matrix.csv: one flat row per cell, which is the file a heatmap or a
// scaling plot reads. The nested phases and operations stay in the JSON.
func (m Matrix) CSV() string {
	var b buf
	b.line("%s", csvRow(
		"scenario", "build", "ref", "link_profile", "rtt_p50_ms", "control_single_mib_per_s",
		"connections", "concurrency", "request_concurrency", "request_concurrency_used", "repeats",
		"files", "bytes", "median_ms", "min_ms", "max_ms", "mad_ms", "mib_per_s", "files_per_s",
		"upload_phase_ms", "net_write_bytes", "user_cpu_ms", "sys_cpu_ms", "cpu_percent", "max_rss_bytes", "go_gc_count",
		"go_peak_goroutines", "connections_opened", "connections_used", "connections_refused",
		"reconnects", "retries", "errors", "failed_runs"))

	for _, c := range m.Result.Cells {
		probe := startProbe(m.Result.Link, c.LinkProfile)
		b.line("%s", csvRow(
			c.Scenario, c.Label, c.Ref, c.LinkProfile,
			rttP50(probe), controlSingle(probe),
			c.Connections, c.Concurrency, c.RequestConcurrency, c.RequestConcurrencyUsed, c.Repeats,
			c.Files, c.Bytes, c.MedianMS, c.MinMS, c.MaxMS, c.MadMS, c.MiBPerS, c.FilesPerS,
			c.UploadPhaseMS, c.NetWriteBytes, c.UserCPUMS, c.SysCPUMS, c.CPUPercent, c.MaxRSSBytes, c.GoGCCount,
			c.GoPeakGoroutines, c.ConnectionsOpened, c.ConnectionsUsed, c.ConnectionsRefused,
			c.Reconnects, c.Retries, c.Errors, c.FailedRuns))
	}
	return b.String()
}

// Markdown is matrix.md.
func (m Matrix) Markdown() string {
	var b buf
	result := m.Result

	b.line("## easySFTP connections/concurrency matrix")
	b.line("")
	b.line("| Setting | Value |")
	b.line("|---|---|")
	b.line("| Candidate | `%s` |", m.CandidateRef)
	b.line("| Baseline | `%s` |", orNone(m.BaselineRef))
	b.line("| Repeats per cell | %d |", m.Repeats)
	b.line("| Runner | %s |", m.RunnerDisplay)
	b.line("| connections | %s |", m.ConnectionsDisplay)
	b.line("| concurrency | %s |", m.ConcurrencyDisplay)
	b.line("| request_concurrency | %s |", orDefault(m.RequestDisplay))
	b.line("| Link profiles | %s |", orRealLine(m.LinkRequested))
	b.line("| Scenarios | %s |", strings.Join(m.scenarioNames(), " "))
	b.line("")
	// What each scenario is, next to the numbers: "sync" and "redeploy" are a
	// deploy shape and not only a payload, and a MiB/s of theirs is over the few
	// changed files rather than over the tree.
	b.line("| Scenario | Mode | Payload | connections | concurrency | request_concurrency |")
	b.line("|---|---|---|---|---|---|")
	for _, s := range m.Scenarios {
		b.line("| `%s` | %s | %s | %s | %s | %s |",
			s.Name, s.Mode, s.Description, s.ConnectionsDisplay, s.ConcurrencyDisplay, s.RequestDisplay)
	}
	b.line("")
	b.line("The last three columns are the axis values this scenario was swept over, which are not always the ones requested above. A value larger than the payload's file count is the same configuration under another name (only the per-file upload path spreads over connections and workers), and `request_concurrency` is a per-file setting that a payload of small files cannot use at all, so both are capped against the payload rather than measured twice.")
	b.line("")
	linkSection(&b, result.Link)
	b.line("Each grid below is median wall-clock milliseconds: rows are `advanced.connections`, columns are `advanced.concurrency`. Lower is better. `connections > concurrency` is measured, not skipped; easySFTP caps the pool at the concurrency (a connection no worker picks is a handshake for nothing), so those cells are expected to flatten out rather than improve.")
	b.line("")

	m.grids(&b)
	m.bestCells(&b)
	if m.HasBaseline {
		m.candidateAgainstBaseline(&b)
	}
	m.autoCost(&b)
	m.canary(&b)
	m.deletes(&b)

	var refused float64
	for _, c := range result.Cells {
		refused += stats.Or(c.ConnectionsRefused, 0)
	}
	if refused != 0 {
		b.line("")
		b.line("**%s connection(s) were refused by the server** across the sweep and fell back to the run's first connection. Those cells had fewer connections than configured, which is the server's limit showing up in the data, not easySFTP's.", num(refused))
	}
	b.line("")
	b.line("Raw data: `matrix.json` (every cell with its own phases and round-trip percentiles, plus the pre-grouped `scaling` view, the `auto` policy runs, the `canary` runs and the `deletes` sweeps), `matrix.csv` (one flat row per cell). Data only: nothing here fails a build.")
	return b.String()
}

// grids prints one heatmap per scenario, build, link profile and swept
// request_concurrency value.
func (m Matrix) grids(b *buf) {
	for _, s := range m.Scenarios {
		for _, label := range m.Labels {
			for _, profile := range m.LinkProfiles {
				for _, request := range s.RequestConcurrency {
					title := "`" + s.Name + "` / " + label + " / " + profile
					if request != nil {
						title += " / request_concurrency " + num(float64(*request))
					}
					b.line("#### %s", title)
					b.line("")

					header := "| connections \\ concurrency |"
					separator := "|---|"
					for _, conc := range s.Concurrency {
						header += " " + num(float64(conc)) + " |"
						separator += "---|"
					}
					b.line("%s", header)
					b.line("%s", separator)

					for _, conns := range s.Connections {
						row := "| " + num(float64(conns)) + " |"
						for _, conc := range s.Concurrency {
							row += " " + m.cellText(s.Name, label, profile, conns, conc, request) + " |"
						}
						b.line("%s", row)
					}
					b.line("")
				}
			}
		}
	}
}

// cellText is one square of a grid: the median, the throughput, and the refused
// connections where the server turned any down.
func (m Matrix) cellText(scenario, label, profile string, conns, conc int, request *int) string {
	for i := range m.Result.Cells {
		c := &m.Result.Cells[i]
		if c.Scenario != scenario || c.Label != label || c.LinkProfile != profile {
			continue
		}
		if c.Connections != conns || c.Concurrency != conc {
			continue
		}
		if !sameInt(c.RequestConcurrency, request) {
			continue
		}
		text := num(c.MedianMS) + " ms<br>" + num(c.MiBPerS) + " MiB/s"
		if stats.Or(c.ConnectionsRefused, 0) > 0 {
			text += "<br>" + num(*c.ConnectionsRefused) + " refused"
		}
		return text
	}
	return "-"
}

func (m Matrix) bestCells(b *buf) {
	b.line("### Best cell per scenario, build and link profile")
	b.line("")
	b.line("| Scenario | Build | Profile | connections | concurrency | request_concurrency | Median | MiB/s | files/s | On an axis edge |")
	b.line("|---|---|---|---|---|---|---|---|---|---|")
	for _, s := range m.Result.Scaling {
		edge := "no"
		if len(s.BestAtAxisMax) > 0 {
			edge = strings.Join(s.BestAtAxisMax, ", ")
		}
		b.line("| %s | %s | %s | %d | %d | %s | %s ms | %s | %s | %s |",
			s.Scenario, s.Label, s.LinkProfile, s.Best.Connections, s.Best.Concurrency,
			requestOrDefault(s.Best.RequestConcurrency), num(s.Best.MedianMS),
			num(s.Best.MiBPerS), num(s.Best.FilesPerS), edge)
	}
	b.line("")

	// The check the auto-config work rests on: a best cell sitting on the
	// largest value of an axis is a cut-off, not an optimum, and anything fitted
	// to it extrapolates. Only the upper edge is reported; the lower one is 1
	// and there is nothing below it to sweep.
	var cutoff []string
	for _, s := range m.Result.Scaling {
		if len(s.BestAtAxisMax) == 0 {
			continue
		}
		cutoff = append(cutoff, s.Scenario+"/"+s.Label+"/"+s.LinkProfile+": "+strings.Join(s.BestAtAxisMax, ", "))
	}
	if len(cutoff) > 0 {
		b.line("**The optimum sits on the edge of the grid** for %s. The best value measured is the largest one swept, so the real optimum is at or beyond it and this sweep does not contain it. Extend that axis before fitting anything to these numbers.", strings.Join(cutoff, "; "))
	} else {
		b.line("Every best cell is interior to its axes, so each optimum was measured rather than cut off.")
	}
}

// candidateAgainstBaseline prints the worst and the best cell of each scenario
// and profile.
//
// Grouped per profile as well: the worst cell of one profile and the best of
// another are not two ends of one distribution.
func (m Matrix) candidateAgainstBaseline(b *buf) {
	b.line("")
	b.line("### Candidate against baseline, worst and best cell")
	b.line("")
	b.line("| Scenario | Profile | connections | concurrency | Candidate | Baseline | Delta |")
	b.line("|---|---|---|---|---|---|---|")

	groups := stats.GroupBy(m.Result.Comparison, func(c schema.MatrixCompare) stats.Key {
		return stats.Key{c.LinkProfile, c.Scenario}
	})
	var picked []schema.MatrixCompare
	for _, group := range groups {
		sorted := append([]schema.MatrixCompare(nil), group...)
		sort.SliceStable(sorted, func(i, j int) bool {
			return stats.Or(sorted[i].DeltaPercent, 0) < stats.Or(sorted[j].DeltaPercent, 0)
		})
		picked = append(picked, sorted[0], sorted[len(sorted)-1])
	}
	// jq's unique_by both deduplicates and sorts, so the rows come out ordered
	// by their coordinates rather than by which end of a group they came from.
	stats.SortByKey(picked, func(c schema.MatrixCompare) stats.Key {
		return stats.Key{c.LinkProfile, c.Scenario, c.Connections, c.Concurrency}
	})
	// A group with a single cell contributes that cell as both its worst and its
	// best, which is where the deduplication earns its keep.
	seen := map[string]bool{}
	for _, c := range picked {
		key := csvRow(c.LinkProfile, c.Scenario, c.Connections, c.Concurrency)
		if seen[key] {
			continue
		}
		seen[key] = true
		b.line("| %s | %s | %d | %d | %s ms | %s ms | %s |",
			c.Scenario, c.LinkProfile, c.Connections, c.Concurrency,
			num(c.MedianMS), num(c.ReferenceMedianMS), percent(c.DeltaPercent))
	}
}

// changeSummary renders the runtime changes as "total (+up/-down)", so a run
// that grew once and took the step back reads differently from one that never
// moved and from one that grew twice (issue #215, stage 5). Builds that report
// only the total keep the plain number.
func changeSummary(c schema.Chosen) string {
	total := raw(c.Changes)
	if c.SpreadIncreases == nil && c.SpreadDecreases == nil {
		return total
	}
	return fmt.Sprintf("%s (+%s/-%s)", total, raw(c.SpreadIncreases), raw(c.SpreadDecreases))
}

func (m Matrix) autoCost(b *buf) {
	b.line("")
	b.line("### What `auto` costs (policy regret)")
	b.line("")
	// Through "%s": the sentence carries a literal per cent sign, and vet reads
	// a bare string here as a format.
	b.line("%s", "One run per scenario and profile with `connections`, `concurrency` and `request_concurrency` all set to `auto`, on the candidate build, measured next to the cells it is scored against. `Picked` is read out of the run's own counters, so it is what easySFTP did and not what this script assumes; `Best` is the fastest cell of the same scenario and profile. `Regret` is the gap between them: how much slower the policy is than the settings a sweep would have chosen. A policy within ~15% on every profile is defensible, one that only wins on the house line is not (issue #184, phase 5; the policy itself is #209).")
	b.line("")
	b.line("| Scenario | Profile | Picked (conn/conc/req) | Started at | Changes | auto | Best cell | Best | Regret | Same cell in grid |")
	b.line("|---|---|---|---|---|---|---|---|---|---|")
	for _, a := range m.Result.Auto {
		best := "-"
		bestMS := "-"
		if a.Best != nil {
			best = num(float64(a.Best.Connections)) + "/" + num(float64(a.Best.Concurrency)) + "/" +
				requestOrDefault(a.Best.RequestConcurrency)
			bestMS = num(a.Best.MedianMS) + " ms"
		}
		inGrid := "not swept"
		if a.ChosenInGrid {
			inGrid = raw(a.ChosenCellMedianMS) + " ms"
		}
		b.line("| %s | %s | %s/%s/%s | %s/%s/%s | %s | %s ms | %s | %s | %s | %s |",
			a.Scenario, a.LinkProfile,
			raw(a.Chosen.Connections), raw(a.Chosen.Concurrency), raw(a.Chosen.RequestConcurrency),
			raw(a.Chosen.InitialConnections), raw(a.Chosen.InitialConcurrency), raw(a.Chosen.InitialRequestConcurrency),
			changeSummary(a.Chosen),
			num(a.MedianMS), best, bestMS, percent(a.RegretPercent), inGrid)
	}
	b.line("")
	b.line("`Started at` is what the policy chose before the transfer taught it anything, and `Changes` how many times it moved the connection spread while the transfer ran, as `total (+up/-down)`: a step back leaves the connections open and hands the remaining files to fewer of them (issue #215). When the two coordinate columns agree, the run was decided entirely up front. The last column is the same coordinates measured as an ordinary cell. It is a control, not a result: a large gap between it and the `auto` column means the two runs saw different conditions, and then the regret next to it is drift rather than policy. Where it says \"not swept\", the settings easySFTP picked are not on this grid at all.")
	m.autoWorkload(b)
}

// autoWorkload prints the input side of each decision. The regret table says
// what the policy chose; this says what it was looking at, which is what makes
// a surprising choice readable without rerunning the sweep (issue #209).
func (m Matrix) autoWorkload(b *buf) {
	rows := 0
	for _, a := range m.Result.Auto {
		if a.Workload != nil {
			rows++
		}
	}
	if rows == 0 {
		return
	}
	b.line("")
	b.line("<details><summary>What the policy was looking at</summary>")
	b.line("")
	b.line("| Scenario | Profile | Files | Bytes | p50 | p90 | Largest file | One-packet files | Probes | RTT | Handshake | BDP |")
	b.line("|---|---|---|---|---|---|---|---|---|---|---|---|")
	for _, a := range m.Result.Auto {
		w := a.Workload
		if w == nil {
			continue
		}
		b.line("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s ms | %s ms | %s |",
			a.Scenario, a.LinkProfile, raw(w.Files), raw(w.Bytes),
			raw(w.P50Bytes), raw(w.P90Bytes), raw(w.LargestBytes), raw(w.SmallFiles), raw(w.Probes),
			raw(w.RTTMS), raw(w.HandshakeMS), raw(w.BDPBytes))
	}
	b.line("")
	b.line("`p50` and `p90` are the median and ninetieth-percentile file size and `One-packet files` how many of them fit in a single 32 KiB SFTP write, which is what tells a tree of tiny files with one archive in it from a tree of large ones; the pipelining depth is sized against the distribution rather than against the largest file alone (issue #215). `BDP` is bandwidth times delay: how many bytes one stream has to keep in flight to fill this path. `Probes` are the remote round-trips that are not uploads, which is what an `advanced.skip_unchanged` redeploy is made of. The RTT and handshake here are easySFTP's own measurement, taken on its first connection; `The link` above is the same path measured by `cmd/linkprobe`, which does not go through the uploader. The two should agree, and where they do not, one of them is measuring something else.")
	b.line("")
	b.line("</details>")
}

func (m Matrix) canary(b *buf) {
	b.line("")
	b.line("### Canary")
	b.line("")
	b.line("One fixed cell (`%s`, connections %d, concurrency %d, candidate build) measured at the start, the middle and the end of each profile's grid. Spread is the gap between the fastest and the slowest of them. A spread larger than the deltas below it means the server or the line moved during the sweep, and the whole run is a poor comparison basis.",
		m.Canary.Scenario, m.Canary.Connections, m.Canary.Concurrency)
	b.line("")
	b.line("| Profile | Start | Middle | End | Spread |")
	b.line("|---|---|---|---|---|")

	groups := stats.GroupBy(m.Result.Canary, func(c schema.Canary) stats.Key {
		return stats.Key{c.LinkProfile}
	})
	for _, group := range groups {
		durations := make([]float64, 0, len(group))
		for _, c := range group {
			durations = append(durations, c.DurationMS)
		}
		low := stats.Or(stats.Min(stats.Nums(durations)), 0)
		high := stats.Or(stats.Max(stats.Nums(durations)), 0)
		spread := "-"
		if low > 0 {
			spread = num(math.Round((high-low)/low*100)) + "%"
		}
		b.line("| %s | %s | %s | %s | %s |", group[0].LinkProfile,
			canaryAt(group, "start"), canaryAt(group, "mid"), canaryAt(group, "end"), spread)
	}
}

func canaryAt(group []schema.Canary, at string) string {
	for _, c := range group {
		if c.At == at {
			return num(c.DurationMS) + " ms"
		}
	}
	// The middle canary needs a middle to sit in; a grid of a single measured
	// run per profile only gets a start and an end.
	return "-"
}

func (m Matrix) deletes(b *buf) {
	b.line("")
	b.line("### Delete sweeps")
	b.line("")
	b.line("Every cell's pre-clean wipes the tree the cell before it left behind, at that cell's own `connections`/`concurrency`. It has always run and cost nothing extra; what is new is that it is measured (issue #184, phase 4). Cells whose pre-clean found an empty directory are not listed. Read the two columns on the right against #157: deletions are one round-trip per entry and the pool has nowhere to spread them.")
	b.line("")
	b.line("<details><summary>Per-cell delete sweeps</summary>")
	b.line("")
	b.line("| Scenario | Build | Profile | conn | conc | Files deleted | Median | files/s | remote_scan | delete_sweep | sftp_remove p50 | sftp_rmdir p50 |")
	b.line("|---|---|---|---|---|---|---|---|---|---|---|---|")
	for _, d := range m.Result.Deletes {
		b.line("| %s | %s | %s | %d | %d | %s | %s ms | %s | %s | %s | %s | %s |",
			d.Scenario, d.Label, d.LinkProfile, d.Connections, d.Concurrency,
			num(d.FilesDeleted), num(d.MedianMS), num(d.DeletesPerS),
			ms(phaseOf(d.Phases, "remote_scan")), ms(phaseOf(d.Phases, "delete_sweep")),
			ms(opOf(d.Operations, "sftp_remove")), ms(opOf(d.Operations, "sftp_rmdir")))
	}
	b.line("")
	b.line("</details>")
}

func (m Matrix) scenarioNames() []string {
	names := make([]string, 0, len(m.Scenarios))
	for _, s := range m.Scenarios {
		names = append(names, s.Name)
	}
	return names
}

// requestOrDefault prints the pass that sets nothing as "default", which is what
// it is: easySFTP's own value rather than an axis coordinate.
func requestOrDefault(v *int) string {
	if v == nil {
		return "default"
	}
	return num(float64(*v))
}

func orDefault(s string) string {
	if s == "" {
		return "easySFTP default"
	}
	return s
}

func sameInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
