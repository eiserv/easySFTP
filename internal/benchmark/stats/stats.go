// Package stats is the JQ_STATS block of scripts/benchmark-lib.sh, in Go.
//
// It exists to be boring and exact. The functions here are not "a median" and
// "a spread": they are the median and the spread the stored benchmark results
// were computed with, down to the edge cases, because a result recomputed with
// a slightly different rule is not comparable to the ones already published
// (issue #190).
//
// Three of those edge cases are load-bearing and are pinned by tests:
//
//   - The median is the *lower* middle value for an even sample count, which is
//     why an odd number of repeats is the better choice.
//   - The median absolute deviation is null below two samples rather than 0: a
//     single repeat has no measured spread at all, and a 0 there reads as
//     perfect precision (issue #184, phase 2).
//   - A value that was never reported is null, and null is not zero. jq sorts
//     null below every number, treats it as the identity of addition, and
//     propagates it out of min and max over an empty list. A median over a
//     field that only half the repeats carried is therefore not the median over
//     the same list with the gaps filled in as zeroes, and this package keeps
//     that difference.
package stats

import (
	"math"
	"sort"
)

// Ptr is the nullable number these functions work in. Nil is JSON null.
func Ptr(f float64) *float64 { return &f }

// Nums lifts plain numbers into the nullable form.
func Nums(xs []float64) []*float64 {
	out := make([]*float64, len(xs))
	for i := range xs {
		out[i] = Ptr(xs[i])
	}
	return out
}

// Median is jq's "median": the lower middle value of the sorted list, and 0 for
// an empty list.
//
// It returns 0 rather than null for the empty case because that is what the
// stored results were computed with. Nulls sort below every number, so a list
// with gaps can have a null median, which is the honest answer: the middle
// sample did not report the value.
func Median(xs []*float64) *float64 {
	if len(xs) == 0 {
		return Ptr(0)
	}
	sorted := sortValues(xs)
	return sorted[(len(sorted)-1)/2]
}

// MedianOf is Median over values that are known to be present.
func MedianOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	return sorted[(len(sorted)-1)/2]
}

// MinRepeatsForAnalysis is the smallest per-cell sample count that can yield a
// non-zero MAD under the lower-middle median used here. With two samples the
// median is the faster run and MAD is structurally 0 for every cell, so a
// sweep below this floor is a best-of-N, not a measurement the acceptance
// tests or analysis tools should trust (issue #227). Analysis gating uses this
// constant; Mad itself stays null only below two samples, matching stored data.
const MinRepeatsForAnalysis = 3

// Mad is the median absolute deviation, and null below two samples.
//
// It is the spread metric to compare a delta against: a single slow repeat,
// which is the normal failure mode of a shared host, moves it far less than it
// moves a standard deviation. With exactly two samples MAD is structurally 0;
// require MinRepeatsForAnalysis rather than reading that zero as precision
// (issue #227).
func Mad(xs []float64) *float64 {
	if len(xs) < 2 {
		return nil
	}
	m := MedianOf(xs)
	deviations := make([]float64, len(xs))
	for i, x := range xs {
		deviations[i] = math.Abs(x - m)
	}
	return Ptr(MedianOf(deviations))
}

// Min is jq's "min": null over an empty list, and null below any number.
func Min(xs []*float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	return sortValues(xs)[0]
}

// Max is jq's "max": null over an empty list.
func Max(xs []*float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	sorted := sortValues(xs)
	return sorted[len(sorted)-1]
}

// Add is jq's "add": null over an empty list and over a list of nothing but
// nulls, and otherwise the sum with the nulls skipped, because null is the
// identity of addition in jq.
func Add(xs []*float64) *float64 {
	var sum float64
	seen := false
	for _, x := range xs {
		if x == nil {
			continue
		}
		sum += *x
		seen = true
	}
	if !seen {
		return nil
	}
	return Ptr(sum)
}

// Or is jq's "// 0" on a nullable number: the value, or 0 when it is null.
func Or(x *float64, fallback float64) float64 {
	if x == nil {
		return fallback
	}
	return *x
}

// Round1 and Round2 are jq's "(. * 10 | round) / 10" and its hundredths
// sibling. jq rounds half away from zero, which is what math.Round does.
func Round1(f float64) float64 { return math.Round(f*10) / 10 }

// Round2 rounds to two decimals, half away from zero.
func Round2(f float64) float64 { return math.Round(f*100) / 100 }

// Pct is jq's "pct(a; b)": the change from b to a in percent, rounded to two
// decimals, and null where b is null or zero and the question has no answer.
func Pct(a, b float64) *float64 {
	if b == 0 {
		return nil
	}
	return Ptr(Round2(((a - b) / b) * 100))
}

// Ratio is the "X per second" both scripts compute the same way: null-safe,
// rounded to two decimals, and 0 where there is no duration to divide by.
func Ratio(count, durationMS float64) float64 {
	if durationMS <= 0 {
		return 0
	}
	return Round2(count / (durationMS / 1000))
}

// MiBPerS is Ratio over bytes, which the scripts write out inline every time.
func MiBPerS(bytes, durationMS float64) float64 {
	return Ratio(bytes/1048576, durationMS)
}

// sortValues sorts nullable numbers the way jq sorts them: null first, then the
// numbers ascending. The sort is stable so equal values keep the order they
// were measured in.
func sortValues(xs []*float64) []*float64 {
	sorted := append([]*float64(nil), xs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		switch {
		case sorted[i] == nil:
			return sorted[j] != nil
		case sorted[j] == nil:
			return false
		default:
			return *sorted[i] < *sorted[j]
		}
	})
	return sorted
}
