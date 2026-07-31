package stats

import "strconv"

// Format renders a number the way jq's string interpolation does, which is what
// the Markdown tables and the CSV columns of a stored result contain: no
// trailing ".0" on a whole number, and no exponent for anything a benchmark
// produces.
//
// Everything these reports print is either a whole number (milliseconds, byte
// counts, round-trip counts) or has already been rounded to one or two decimals,
// so the shortest representation is always short.
func Format(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// FormatOr renders a nullable number, or the given placeholder when it is null.
// The placeholder is "-" in every Markdown table and the empty string in every
// CSV: an empty column says "this metric did not exist yet", and a 0 there would
// be a measurement that was never taken.
func FormatOr(f *float64, placeholder string) string {
	if f == nil {
		return placeholder
	}
	return Format(*f)
}

func formatNumber(f float64) string { return Format(f) }
