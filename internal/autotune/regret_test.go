package autotune_test

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/autotune"
	"github.com/eiserv/easySFTP/internal/benchmark/scenario"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

// This file is the acceptance test issue #209 asks for: replay the policy over
// the settings sweeps that are committed under benchmarks/matrix/, and score
// what it picks against the fastest cell those sweeps measured.
//
//	regret = auto_duration / best_matrix_duration - 1
//
// It is a real test rather than a note in a report because the alternative is
// a policy whose constants nobody may touch without rerunning a multi-hour
// sweep. Two things keep it honest and two keep it from being a tripwire:
//
//   - Honest: the workload comes from internal/benchmark/scenario, the same
//     tables that built the payloads, so the features are the ones a run would
//     really compute. The link comes from the sweep's own probes, so the RTT
//     and the handshake are the ones that sweep saw.
//   - Not a tripwire: only sweeps with at least three repeats per cell are
//     read (a single-repeat grid is one sample per cell and its "best" is
//     often noise), and a gap of a few hundred milliseconds on a run of one or
//     two seconds passes on its absolute size. The issue allows exactly that:
//     "accepting larger gaps only where setup cost/noise makes the absolute
//     difference trivial".
//
// The replay can only score stages 1 and 2. A grid cell is a fixed
// configuration measured from its first byte, so there is nothing in it for
// the runtime controller to react to; what the controller adds is on top of
// the numbers below, not in them.
//
// It also cannot score a coordinate the grid does not contain. scoreOf
// interpolates between the measured cells that bracket the choice, so a choice
// between two swept values is estimated from both rather than credited with
// the better one; snapNote prints the span, and where that span is wide the
// percentage next to it is an estimate and not a measurement of the policy
// (issues #217 and #228).
//
// What it cannot see at all is a sweep's own recorded auto[] regret, which is
// a measurement rather than a replay. TestRecordedAutoRegretOfStoredSweeps in
// recorded_regret_test.go reads those, including from the sweeps this file
// skips.
//
// When this fails after a policy change, read the per-scenario table it
// prints: it names the cell the policy chose, the cell that won, and the two
// durations. When it fails after a *new sweep* is stored, the numbers moved
// and the policy has to be refitted, which is the point.
const (
	// regretTarget is the issue's own acceptance threshold.
	regretTarget = 0.15

	// trivialGap is how small an absolute difference has to be before the
	// percentage stops meaning anything. The stored sweeps' own canary spread
	// (the same cell measured at the start, the middle and the end of a grid)
	// is around 9%, and the scenarios that land here are 1.3 second runs.
	trivialGap = 300 * time.Millisecond

	// minRepeats matches stats.MinRepeatsForAnalysis: below that, MAD is
	// structurally zero and a best cell is a sample (issue #227).
	minRepeats = stats.MinRepeatsForAnalysis
)

func TestPolicyRegretAgainstStoredSweeps(t *testing.T) {
	sweeps := storedSweeps(t)
	if len(sweeps) == 0 {
		t.Fatal("no sweep with enough repeats found under benchmarks/matrix; this test would pass vacuously")
	}
	for _, sweep := range sweeps {
		t.Run(sweep.name, func(t *testing.T) {
			link := sweep.link()
			t.Logf("replaying against a %s / %s link", link.RTT.Round(time.Millisecond), link.Handshake.Round(time.Millisecond))
			for _, group := range sweep.groups() {
				w, err := workloadOf(group.scenario)
				if err != nil {
					t.Fatalf("%s: %v", group.scenario, err)
				}
				chosen := autotune.Plan(w, link, autotune.Fixed{})
				at, ok := group.scoreOf(chosen)
				if !ok {
					// The policy picked coordinates this grid did not measure
					// at all. Nothing to score, and silently dropping it would
					// make the test look more complete than it is.
					t.Logf("%-16s chose %v: not on this grid, skipped", group.scenario, chosen)
					continue
				}
				best := group.best()
				gap := time.Duration(at-best.MedianMS) * time.Millisecond
				regret := at/best.MedianMS - 1

				t.Logf("%-16s chose %d/%d/%s -> %s, best %d/%d/%s -> %s, regret %+.1f%%%s",
					group.scenario, chosen.Connections, chosen.Concurrency, request(chosen.RequestConcurrency),
					ms(at), best.Connections, best.Concurrency, requestOf(best.RequestConcurrency),
					ms(best.MedianMS), regret*100, group.snapNote(chosen))

				if regret > regretTarget && gap > trivialGap {
					t.Errorf("%s: the policy is %.1f%% (%s) behind the fastest cell, over the %.0f%% target",
						group.scenario, regret*100, gap.Round(time.Millisecond), regretTarget*100)
				}
			}
		})
	}
}

// TestTheFixedDefaultsWouldFailThisTest is the control. 1/4/16 is what "auto"
// resolved to before this package existed, and the point of the exercise is
// that it is not defensible: if the replay above ever stopped separating the
// two, it would have stopped measuring anything.
func TestTheFixedDefaultsWouldFailThisTest(t *testing.T) {
	old := autotune.Settings{Connections: 1, Concurrency: 4, RequestConcurrency: 16}
	worst := 0.0
	for _, sweep := range storedSweeps(t) {
		for _, group := range sweep.groups() {
			at, ok := group.scoreOf(old)
			if !ok {
				continue
			}
			worst = math.Max(worst, at/group.best().MedianMS-1)
		}
	}
	if worst <= regretTarget {
		t.Errorf("the pre-adaptive defaults come out %.1f%% behind the best cell, which is inside the %.0f%% target: "+
			"this replay is no longer telling a good policy from a bad one", worst*100, regretTarget*100)
	}
	t.Logf("the pre-adaptive fixed 1/4/16 is up to %.0f%% behind the best cell", worst*100)
}

// workloadOf rebuilds the features a run would compute for one benchmark
// scenario, from the tables that generated its payload.
//
// The shape matters as much as the payload. A prepopulated overlay scenario is
// measured with advanced.skip_unchanged, so its upload set is not settled when
// the policy runs and every file costs a stat; a prepopulated sync scenario has
// already read its manifest, so it knows it is sending scenario.ChangedFiles
// files and nothing else.
func workloadOf(name string) (autotune.Workload, error) {
	groups, err := scenario.Spec(name)
	if err != nil {
		return autotune.Workload{}, err
	}
	shape := scenario.ShapeOf(name)

	// The sizes the payload really has, in the order a scan would produce
	// them, so the distribution features of the workload (issue #215, stage 1)
	// are the ones a run would compute rather than a summary of a summary.
	var sizes []int64
	for _, g := range groups {
		for range g.Count {
			sizes = append(sizes, int64(g.KiB)*KiB)
		}
	}
	files := len(sizes)

	switch {
	case shape.Prepopulate && shape.Mode == "sync":
		// A sync knows what changed before it uploads: the manifest already
		// told it. The harness changes the first ChangedFiles files of the
		// tree, which is what those sizes describe.
		changed := min(scenario.ChangedFiles, files)
		return autotune.SummarizeUploads(sizes[:changed]), nil
	case shape.Prepopulate:
		// An overlay redeploy with skip_unchanged has not decided anything
		// yet: every file costs a stat and only some are then sent.
		w := autotune.SummarizeUploads(sizes)
		return autotune.Workload{
			Probes:        files,
			Unknown:       true,
			LargestUpload: w.LargestUpload,
			P50Upload:     w.P50Upload,
			P90Upload:     w.P90Upload,
			SmallUploads:  w.SmallUploads,
		}, nil
	default:
		return autotune.SummarizeUploads(sizes), nil
	}
}

// sweep is one stored matrix result, reduced to what the replay needs.
type sweep struct {
	name   string
	matrix schema.Matrix
}

// storedSweeps reads every committed matrix result with enough repeats to be
// worth scoring against.
func storedSweeps(t *testing.T) []sweep {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "benchmarks", "matrix"))
	if err != nil {
		t.Fatalf("locating benchmarks/matrix: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatalf("listing benchmarks/matrix: %v", err)
	}
	var out []sweep
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		var env schema.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		var m schema.Matrix
		if err := json.Unmarshal(env.Benchmark, &m); err != nil {
			t.Fatalf("decoding the measurement in %s: %v", path, err)
		}
		if m.Repeats < minRepeats || len(m.Cells) == 0 {
			continue
		}
		if m.Link == nil || len(m.Link.Probes) == 0 {
			// Without the sweep's own probes there is no link to replay
			// against, and inventing one would score the policy on a path
			// nobody measured.
			continue
		}
		out = append(out, sweep{name: strings.TrimSuffix(filepath.Base(path), ".json"), matrix: m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// link is the path that sweep was measured over: the median of its own probes.
// Those come from cmd/linkprobe, which never goes through the uploader, so
// they are exactly the numbers a run's own probe would try to reproduce.
func (s sweep) link() autotune.Link {
	var rtts, handshakes []float64
	if s.matrix.Link != nil {
		for _, p := range s.matrix.Link.Probes {
			if p.RTTMS != nil && p.RTTMS.P50 != nil {
				rtts = append(rtts, *p.RTTMS.P50)
			}
			if p.HandshakeMS != nil {
				handshakes = append(handshakes, *p.HandshakeMS)
			}
		}
	}
	return autotune.Link{
		RTT:       millis(median(rtts)),
		Handshake: millis(median(handshakes)),
	}
}

// cellGroup is every cell of one scenario on one link profile, which is the
// set a single decision is scored inside.
type cellGroup struct {
	scenario, profile string
	cells             []schema.Cell
}

// groups collects the candidate build's cells. A baseline build's grid belongs
// to a different product and a regret against it would compare two things.
func (s sweep) groups() []cellGroup {
	byKey := map[string]*cellGroup{}
	var order []string
	for _, c := range s.matrix.Cells {
		if c.Label != "candidate" {
			continue
		}
		key := c.Scenario + "/" + c.LinkProfile
		g, ok := byKey[key]
		if !ok {
			g = &cellGroup{scenario: c.Scenario, profile: c.LinkProfile}
			byKey[key] = g
			order = append(order, key)
		}
		g.cells = append(g.cells, c)
	}
	sort.Strings(order)
	out := make([]cellGroup, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	return out
}

func (g cellGroup) best() schema.Cell {
	best := g.cells[0]
	for _, c := range g.cells[1:] {
		if c.MedianMS < best.MedianMS {
			best = c
		}
	}
	return best
}

// scoreOf is the duration a choice is scored at. A grid is a handful of powers
// of two, so the exact choice usually is not on it and the replay has to
// estimate.
//
// It interpolates between the measured cells that bracket the choice, weighted
// per axis, instead of snapping to the closest measured neighbour. Snapping
// was optimistic in the one direction that matters for an acceptance test,
// because ties went up and up is usually the faster column: for the stored
// "mixed" sweep the policy chose 6 connections, whose measured brackets are 8
// (12,995 ms) and 4 (17,447 ms), and rounding to 8 scored it at +1.3% where the
// run really measured at those settings came out +24.8% (issue #228).
// Interpolating puts it at 15,221 ms, +17%, which is an estimate rather than a
// measurement but is not one that flatters the policy by construction.
//
// A choice that is on the grid is unaffected: it is scored at its own cell.
// Where a corner of the bracket was not measured, the weights of the corners
// that were are renormalized, which is the same approximation one axis at a
// time.
func (g cellGroup) scoreOf(s autotune.Settings) (float64, bool) {
	corners := g.bracketCells(s)
	if len(corners) == 0 {
		return 0, false
	}
	var sum, weight float64
	for _, c := range corners {
		sum += c.weight * c.cell.MedianMS
		weight += c.weight
	}
	if weight == 0 {
		return 0, false
	}
	return sum / weight, true
}

// corner is one measured cell of a bracket, with the weight interpolation
// gives it.
type corner struct {
	cell   schema.Cell
	weight float64
}

// bracketCells is every measured cell whose coordinates bracket the choice: on
// each axis either the chosen value itself, when the grid measured it, or the
// neighbours immediately below and above.
func (g cellGroup) bracketCells(s autotune.Settings) []corner {
	conns := axis(g.cells, func(c schema.Cell) int { return c.Connections })
	concs := axis(g.cells, func(c schema.Cell) int { return c.Concurrency })
	var requests []int
	for _, c := range g.cells {
		if c.RequestConcurrency != nil {
			requests = append(requests, *c.RequestConcurrency)
		}
	}
	requests = dedup(requests)

	// A grid pass that set nothing is "default", which is not the same as any
	// measured number; where the grid swept the axis at all, only its measured
	// values are candidates.
	wantRequests := map[*int]float64{}
	if len(requests) == 0 {
		wantRequests[nil] = 1
	} else {
		for value, w := range bracket(requests, s.RequestConcurrency) {
			v := value
			wantRequests[&v] = w
		}
	}

	var out []corner
	for wantConn, wConn := range bracket(conns, s.Connections) {
		for wantConc, wConc := range bracket(concs, s.Concurrency) {
			for wantRequest, wRequest := range wantRequests {
				if c, ok := g.cellExact(wantConn, wantConc, wantRequest); ok {
					out = append(out, corner{cell: c, weight: wConn * wConc * wRequest})
				}
			}
		}
	}
	return out
}

// bracket is the measured values on one axis that a choice sits between, each
// with its interpolation weight: the value itself at weight 1 when the grid has
// it, otherwise the neighbours below and above weighted by how close the
// choice is to each. A choice outside the swept range has only one neighbour
// and is scored at it.
func bracket(values []int, want int) map[int]float64 {
	if slices.Contains(values, want) {
		return map[int]float64{want: 1}
	}
	lo, hasLo := largestBelow(values, want)
	hi, hasHi := smallestAbove(values, want)
	switch {
	case hasLo && hasHi:
		span := float64(hi - lo)
		return map[int]float64{lo: float64(hi-want) / span, hi: float64(want-lo) / span}
	case hasLo:
		return map[int]float64{lo: 1}
	case hasHi:
		return map[int]float64{hi: 1}
	default:
		return map[int]float64{snap(values, want): 1}
	}
}

// snapNote says what a scored row is standing in for when the choice is not on
// the grid: how many measured cells bracket it and how far apart they are. A
// reader has to be able to see that the number next to it is a bound rather
// than a measurement of that coordinate.
func (g cellGroup) snapNote(s autotune.Settings) string {
	corners := g.bracketCells(s)
	if len(corners) < 2 {
		return ""
	}
	lo, hi := corners[0].cell.MedianMS, corners[0].cell.MedianMS
	for _, c := range corners[1:] {
		lo = math.Min(lo, c.cell.MedianMS)
		hi = math.Max(hi, c.cell.MedianMS)
	}
	return fmt.Sprintf("  [%d/%d/%s was not swept; interpolated between %d measured cells spanning %s to %s]",
		s.Connections, s.Concurrency, request(s.RequestConcurrency), len(corners), ms(lo), ms(hi))
}

// cellExact is the measured cell at exactly these coordinates, if the grid has
// one. A nil request concurrency is the grid pass that set nothing, which is
// not the same as any measured number.
func (g cellGroup) cellExact(connections, concurrency int, requestConcurrency *int) (schema.Cell, bool) {
	for _, c := range g.cells {
		if c.Connections != connections || c.Concurrency != concurrency {
			continue
		}
		if (c.RequestConcurrency == nil) != (requestConcurrency == nil) {
			continue
		}
		if requestConcurrency != nil && *c.RequestConcurrency != *requestConcurrency {
			continue
		}
		return c, true
	}
	return schema.Cell{}, false
}

func axis(cells []schema.Cell, of func(schema.Cell) int) []int {
	values := make([]int, 0, len(cells))
	for _, c := range cells {
		values = append(values, of(c))
	}
	return dedup(values)
}

func dedup(values []int) []int {
	seen := map[int]bool{}
	out := values[:0:0]
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

// largestBelow is the largest measured value strictly under want, and
// smallestAbove its counterpart: together they are the two neighbours an
// off-grid choice sits between.
func largestBelow(values []int, want int) (int, bool) {
	best, found := 0, false
	for _, v := range values {
		if v < want && (!found || v > best) {
			best, found = v, true
		}
	}
	return best, found
}

func smallestAbove(values []int, want int) (int, bool) {
	best, found := 0, false
	for _, v := range values {
		if v > want && (!found || v < best) {
			best, found = v, true
		}
	}
	return best, found
}

func snap(values []int, want int) int {
	best := values[0]
	for _, v := range values[1:] {
		switch d, bd := abs(v-want), abs(best-want); {
		case d < bd, d == bd && v > best:
			best = v
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	return values[len(values)/2]
}

func millis(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }

func ms(v float64) string { return fmt.Sprintf("%.0f ms", v) }

// request renders a chosen request_concurrency, and requestOf the one a cell
// was measured at: a grid pass that set nothing is "default", which is not the
// same as any measured number.
func request(v int) string { return fmt.Sprint(v) }

func requestOf(p *int) string {
	if p == nil {
		return "default"
	}
	return fmt.Sprint(*p)
}
