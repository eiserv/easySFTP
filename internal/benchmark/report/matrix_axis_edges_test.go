package report_test

import (
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/benchmark/report"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
)

// The payload-clamped edge of issue #240: a best cell sitting on the largest
// swept value of an axis is only a cut-off when the sweep chose that edge. An
// axis the payload clamped (connections and concurrency at the scenario's file
// count, request_concurrency at the largest file's packet count) has nothing
// distinct beyond it, so "extend that axis" is not advice for it, and the
// honesty check must not present it as one.
//
// cappedFixture is a sweep whose requested axes were clamped for its one
// scenario: the run asked to sweep 1, 2, 4 and 8 connections, but "twofile"
// has two files, so 4 and 8 folded onto 2; the run asked to sweep 1, 16 and 64
// requests, but the largest file is 64 KiB, so 64 clamped to the 3 requests
// its 32 KiB packets can hold open. The best cell sits on all three edges and
// every one of them is the payload's own bound.
func cappedFixture() report.Matrix {
	rc := 64
	result := &schema.Matrix{
		CandidateRef:   "cand (abcdef1)",
		ReferenceLabel: "baseline",
		Repeats:        2,
		Link:           linkFixture(),
		Axes: schema.Axes{
			LinkProfiles:       []string{"baseline"},
			Connections:        []int{1, 2, 4, 8},
			Concurrency:        []int{1, 2, 4},
			RequestConcurrency: []*int{intp(1), intp(16), &rc},
			PerScenario: map[string]schema.ScenarioAxes{
				"twofile": {
					Files:              2,
					Connections:        []int{1, 2},
					Concurrency:        []int{1, 2},
					RequestConcurrency: []*int{intp(1), intp(3)},
				},
			},
		},
		Cells: []schema.Cell{
			{
				Scenario: "twofile", Label: "candidate", Ref: "cand (abcdef1)", LinkProfile: "baseline",
				Connections: 1, Concurrency: 1, Repeats: 2, Files: 2, Bytes: 131072,
				MedianMS: 900, MiBPerS: 0.14, FilesPerS: 2.2, RequestConcurrencyUsed: ptr(1),
			},
			{
				Scenario: "twofile", Label: "candidate", Ref: "cand (abcdef1)", LinkProfile: "baseline",
				Connections: 1, Concurrency: 1, Repeats: 2, Files: 2, Bytes: 131072,
				MedianMS: 850, MiBPerS: 0.15, FilesPerS: 2.3, RequestConcurrency: intp(1),
			},
			{
				Scenario: "twofile", Label: "candidate", Ref: "cand (abcdef1)", LinkProfile: "baseline",
				Connections: 1, Concurrency: 1, Repeats: 2, Files: 2, Bytes: 131072,
				MedianMS: 800, MiBPerS: 0.16, FilesPerS: 2.5, RequestConcurrency: intp(3),
			},
			{
				Scenario: "twofile", Label: "candidate", Ref: "cand (abcdef1)", LinkProfile: "baseline",
				Connections: 1, Concurrency: 2, Repeats: 2, Files: 2, Bytes: 131072,
				MedianMS: 950, MiBPerS: 0.13, FilesPerS: 2.1, RequestConcurrency: intp(3),
			},
			{
				Scenario: "twofile", Label: "candidate", Ref: "cand (abcdef1)", LinkProfile: "baseline",
				Connections: 2, Concurrency: 1, Repeats: 2, Files: 2, Bytes: 131072,
				MedianMS: 1000, MiBPerS: 0.12, FilesPerS: 2, RequestConcurrency: intp(3),
			},
			{
				Scenario: "twofile", Label: "candidate", Ref: "cand (abcdef1)", LinkProfile: "baseline",
				Connections: 2, Concurrency: 2, Repeats: 2, Files: 2, Bytes: 131072,
				MedianMS: 700, MiBPerS: 0.18, FilesPerS: 2.8, RequestConcurrency: intp(3),
			},
		},
		Scaling: []schema.Scaling{
			{
				Scenario: "twofile", Label: "candidate", LinkProfile: "baseline",
				Points: []schema.Point{
					{Connections: 1, Concurrency: 1, RequestConcurrency: intp(1), MedianMS: 850, MiBPerS: 0.15},
					{Connections: 1, Concurrency: 1, RequestConcurrency: intp(3), MedianMS: 800, MiBPerS: 0.16},
					{Connections: 2, Concurrency: 2, RequestConcurrency: intp(3), MedianMS: 700, MiBPerS: 0.18},
				},
				Best:          schema.Best{Connections: 2, Concurrency: 2, RequestConcurrency: intp(3), MedianMS: 700, MiBPerS: 0.18, FilesPerS: 2.8},
				BestAtAxisMax: []string{"connections", "concurrency", "request_concurrency"},
			},
		},
		Auto: []schema.Auto{
			{
				Scenario: "twofile", Label: "auto", Ref: "cand (abcdef1)", LinkProfile: "baseline",
				Repeats: 2, MedianMS: 720, Files: 2, Bytes: 131072, MiBPerS: 0.18, FilesPerS: 2.7,
				Chosen: schema.Chosen{
					Connections: ptr(2), Concurrency: ptr(2), RequestConcurrency: ptr(3),
					InitialConnections: ptr(1), InitialConcurrency: ptr(1), InitialRequestConcurrency: ptr(3),
				},
				Best:          &schema.Best{Connections: 2, Concurrency: 2, RequestConcurrency: intp(3), MedianMS: 700, MiBPerS: 0.18, FilesPerS: 2.8},
				RegretPercent: ptr(2.8),
			},
		},
	}
	return report.Matrix{Result: result, CandidateRef: "cand (abcdef1)", Repeats: 2}
}

// TestPayloadClampedEdgesAreNamedAsBounds pins the report against issue #240's
// false positive: a best cell on a payload-clamped axis is not presented as an
// extendable cut-off, in the edge column or in the verdict paragraph.
func TestPayloadClampedEdgesAreNamedAsBounds(t *testing.T) {
	md := cappedFixture().Markdown()
	t.Log(md)
	row := "| twofile | candidate | baseline | 2 | 2 | 3 | 700 ms | 0.18 | 2.8 | "
	if !strings.Contains(md, row) {
		t.Errorf("the best-cell row is missing or its columns moved: \n%s", md)
	}
	for _, axis := range []string{"connections", "concurrency", "request_concurrency"} {
		want := axis + " (at the payload's bound)"
		if !strings.Contains(md, want) {
			t.Errorf("the clamped %s edge is not named as the payload's bound", axis)
		}
	}

	// The verdict paragraph does not tell the reader to extend them.
	if strings.Contains(md, "**The optimum sits on the edge of the grid**") {
		t.Error("a payload-clamped edge was presented as an extendable cut-off")
	}
	if !strings.Contains(md, "No optimum sits on an extendable edge of its grid.") {
		t.Error("the all-bounded verdict is missing")
	}
	if !strings.Contains(md, "sits on an axis edge that is the payload's own bound") {
		t.Error("the payload-bound sentence is missing")
	}

	// The regret row's best cell carries the same qualifier, because the
	// number it sits next to is not a lower bound the way an extendable
	// edge's is.
	if !strings.Contains(md, "2/2/3 (at the payload's bound: connections, concurrency, request_concurrency)") {
		t.Errorf("the regret row's best cell does not name the bound: \n%s", md)
	}
}

// TestExtendableEdgesKeepTheCutoffVerdict is the other half: an edge the sweep
// chose, with nothing clamped about it, keeps the cut-off warning exactly as it
// was, so the new distinction cannot quietly swallow the honesty check.
func TestExtendableEdgesKeepTheCutoffVerdict(t *testing.T) {
	// matrixFixture's "single" row: the best cell sits on the largest swept
	// concurrency, and there is no per_scenario axes block to clamp anything
	// (the pre-#184 stored shape, which reads as uncapped).
	md := matrixFixture().Markdown()
	if !strings.Contains(md, "**The optimum sits on the edge of the grid** for single/candidate/baseline: concurrency.") {
		t.Error("the extendable cut-off verdict is missing")
	}
	if strings.Contains(md, "at the payload's bound") {
		t.Error("an extendable edge was named as a payload bound")
	}
	// The same sweep with no per-scenario axes at all (the stored shape before
	// issue #184): every edge the sweep chose is an ordinary cut-off, and the
	// report must read exactly as it did before the distinction existed.
	m := cappedFixture()
	m.Result.Axes.PerScenario = nil
	md = m.Markdown()
	if !strings.Contains(md, "**The optimum sits on the edge of the grid** for twofile/candidate/baseline: connections, concurrency, request_concurrency.") {
		t.Errorf("without clamps the three-edge cut-off verdict is missing: \n%s", md)
	}
	if strings.Contains(md, "at the payload's bound") {
		t.Error("an unclamped edge was named as a payload bound")
	}
	if !strings.Contains(md, "2/2/3 (edge of the grid: connections, concurrency, request_concurrency)") {
		t.Errorf("without clamps the regret row does not name the extendable edges: \n%s", md)
	}
}

// TestMixedEdgesSplitTheirVerdict pins the half-and-half case: one axis
// extendable, one clamped, in the same row. The cut-off list keeps the
// extendable axis and the bound sentence names the clamped one; neither is
// silent and neither is presented as the other.
func TestMixedEdgesSplitTheirVerdict(t *testing.T) {
	m := cappedFixture()
	// Give the request axis back its headroom: the payload can no longer be
	// what clamped it, so its edge becomes an ordinary cut-off again. The
	// other two axes keep the clamps above, so the row is half and half.
	rc := 64
	m.Result.Axes.PerScenario["twofile"] = schema.ScenarioAxes{
		Files:              2,
		Connections:        []int{1, 2},
		Concurrency:        []int{1, 2},
		RequestConcurrency: []*int{intp(1), &rc},
	}
	md := m.Markdown()

	if !strings.Contains(md, "**The optimum sits on the edge of the grid** for twofile/candidate/baseline: request_concurrency.") {
		t.Errorf("the extendable half of the row lost its cut-off verdict: \n%s", md)
	}
	if !strings.Contains(md, "sits on an axis edge that is the payload's own bound**, not a place the sweep stopped") {
		t.Error("the clamped half of the row lost its bound sentence")
	}
	if !strings.Contains(md, "2/2/3 (edge of the grid: request_concurrency; at the payload's bound: connections, concurrency)") {
		t.Errorf("the regret row does not split its edge kinds: \n%s", md)
	}
}

// intp is ptr for ints, spelled for the reader of a fixture.
func intp(v int) *int { return &v }
