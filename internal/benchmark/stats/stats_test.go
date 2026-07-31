package stats_test

import (
	"testing"

	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

func TestMedianTakesTheLowerMiddle(t *testing.T) {
	// The rule an odd repeat count exists to avoid: with an even sample count
	// the median is a measured value and not the average of two, so it never
	// invents a number that was not observed.
	if got := stats.MedianOf([]float64{10, 20, 30, 40}); got != 20 {
		t.Errorf("median = %v, want 20", got)
	}
	if got := stats.MedianOf([]float64{30, 10, 20}); got != 20 {
		t.Errorf("median = %v, want 20", got)
	}
}

func TestMedianOfNothingIsZero(t *testing.T) {
	// Not null: this is what the stored results were computed with, and a group
	// with no metrics documents at all reaches it.
	got := stats.Median(nil)
	if got == nil || *got != 0 {
		t.Errorf("median of an empty list = %v, want 0", got)
	}
}

func TestMedianSortsNullBelowEveryNumber(t *testing.T) {
	// A field only some repeats reported. Nulls sort below every number, so they
	// take the low half of the list and can reach the middle themselves, and
	// then the median is null: the middle sample did not report the value. That
	// is deliberately not the median of the values that happen to be there.
	half := []*float64{stats.Ptr(5), nil, nil, stats.Ptr(9)}
	if got := stats.Median(half); got != nil {
		t.Errorf("median = %v, want null", *got)
	}

	// One missing value out of three does not reach the middle, so the median is
	// still a measured number.
	one := []*float64{stats.Ptr(5), nil, stats.Ptr(9)}
	if got := stats.Median(one); got == nil || *got != 5 {
		t.Errorf("median = %v, want 5", got)
	}
}

func TestMadIsNullBelowTwoSamples(t *testing.T) {
	// Issue #184, phase 2: a single repeat has no measured spread, and a 0 here
	// would read as perfect precision. That is the normal case for a matrix run,
	// whose REPEATS default is 1.
	if got := stats.Mad([]float64{42}); got != nil {
		t.Errorf("mad of one sample = %v, want null", *got)
	}
	if got := stats.Mad(nil); got != nil {
		t.Errorf("mad of no samples = %v, want null", *got)
	}
}

func TestMadIsTheMedianDeviation(t *testing.T) {
	// Deviations from the median 20 are 10, 0, 10, 20; their lower-middle value
	// is 10. A standard deviation would have been dragged up by the outlier,
	// which is the whole reason this is the spread metric here.
	got := stats.Mad([]float64{10, 20, 30, 40})
	if got == nil || *got != 10 {
		t.Errorf("mad = %v, want 10", got)
	}
}

func TestMinAndMaxAreNullOverNothing(t *testing.T) {
	if got := stats.Min(nil); got != nil {
		t.Errorf("min = %v, want null", *got)
	}
	if got := stats.Max(nil); got != nil {
		t.Errorf("max = %v, want null", *got)
	}
}

func TestAddSkipsNullsAndIsNullOverNothing(t *testing.T) {
	// null is the identity of addition in jq, so a counter half the repeats did
	// not report still sums to what the others reported.
	got := stats.Add([]*float64{nil, stats.Ptr(2), stats.Ptr(3)})
	if got == nil || *got != 5 {
		t.Errorf("add = %v, want 5", got)
	}
	if got := stats.Add([]*float64{nil, nil}); got != nil {
		t.Errorf("add over nulls = %v, want null", *got)
	}
	if got := stats.Add(nil); got != nil {
		t.Errorf("add over nothing = %v, want null", *got)
	}
}

func TestPctIsNullWithoutAReference(t *testing.T) {
	// A delta against a reference of zero has no answer, and 0 or infinity would
	// both be an invented one.
	if got := stats.Pct(10, 0); got != nil {
		t.Errorf("pct against 0 = %v, want null", *got)
	}
	got := stats.Pct(110, 100)
	if got == nil || *got != 10 {
		t.Errorf("pct = %v, want 10", got)
	}
}

func TestRoundingIsHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0.125, 0.13},
		{0.005, 0.01},
		{-0.005, -0.01},
	}
	for _, c := range cases {
		if got := stats.Round2(c.in); got != c.want {
			t.Errorf("round2(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRatioIsZeroWithoutADuration(t *testing.T) {
	// The scripts guard every throughput division with "if median > 0", because
	// a run that died before writing its duration must not produce an infinity.
	if got := stats.Ratio(100, 0); got != 0 {
		t.Errorf("ratio = %v, want 0", got)
	}
	if got := stats.MiBPerS(1048576*2, 1000); got != 2 {
		t.Errorf("mib_per_s = %v, want 2", got)
	}
}

func TestFormatDropsTheTrailingZero(t *testing.T) {
	if got := stats.Format(624); got != "624" {
		t.Errorf("format = %q, want %q", got, "624")
	}
	if got := stats.Format(0.38); got != "0.38" {
		t.Errorf("format = %q, want %q", got, "0.38")
	}
	if got := stats.Format(33554432); got != "33554432" {
		t.Errorf("format = %q, want %q", got, "33554432")
	}
}

func TestGroupByOrdersTheWayJqDoes(t *testing.T) {
	type row struct {
		profile string
		conns   int
		request *int
	}
	four := 4
	rows := []row{
		{"baseline", 2, &four},
		{"+50ms", 1, nil},
		{"baseline", 1, nil},
		{"baseline", 1, &four},
		{"baseline", 2, &four},
	}
	groups := stats.GroupBy(rows, func(r row) stats.Key {
		var request any
		if r.request != nil {
			request = *r.request
		}
		return stats.Key{r.profile, r.conns, request}
	})

	var got []string
	for _, g := range groups {
		got = append(got, g[0].profile)
	}
	// "+" sorts below "b" by codepoint, and within a profile the null
	// request_concurrency sorts below the number, which is the order the stored
	// results are in.
	want := []string{"+50ms", "baseline", "baseline", "baseline"}
	if len(got) != len(want) {
		t.Fatalf("got %d groups, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("group order = %v, want %v", got, want)
		}
	}
	if len(groups[len(groups)-1]) != 2 {
		t.Errorf("the two identical coordinates did not end up in one group")
	}
	if groups[1][0].request != nil {
		t.Errorf("a null coordinate must sort below a number")
	}
}
