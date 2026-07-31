// Package report renders a measured benchmark into the files a reader gets:
// the CSV a plot plots and the Markdown a pull request shows.
//
// Everything here is a transcription of what scripts/benchmark.sh and
// scripts/benchmark-matrix.sh printed with jq, down to the wording and the
// column order. That is the point of the first Go version (issue #190):
// improving a table is a separate change, and one made deliberately.
package report

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

// buf is a tiny line-oriented builder, so the code below reads like the shell
// it replaces: one call per line printed.
type buf struct {
	sb strings.Builder
}

// line writes one line. An empty call is the shell's bare "echo".
func (b *buf) line(format string, args ...any) {
	if len(args) > 0 {
		fmt.Fprintf(&b.sb, format, args...)
	} else {
		b.sb.WriteString(format)
	}
	b.sb.WriteByte('\n')
}

func (b *buf) String() string { return b.sb.String() }

// num renders a number the way jq interpolates one.
func num(f float64) string { return stats.Format(f) }

// dash renders a nullable number, or "-" when it is null, which is what the
// Markdown tables print where they guard against a null.
func dash(f *float64) string { return stats.FormatOr(f, "-") }

// raw renders a nullable number the way jq's bare string interpolation does,
// which prints an unguarded null as the word "null".
//
// Several tables interpolate a metric without a guard, and a run whose metrics
// file never appeared reaches them. Printing "null" there is deliberate: it is
// what the stored results contain, and a "-" would be this rewrite quietly
// changing a document while claiming to reproduce it.
func raw(f *float64) string { return stats.FormatOr(f, "null") }

// ms renders a nullable duration as jq's "if . == null then \"-\" else \"\(.) ms\"".
func ms(f *float64) string {
	if f == nil {
		return "-"
	}
	return num(*f) + " ms"
}

// percent renders a delta the way both summaries do: an explicit plus sign on a
// regression, and "-" where there is no reference to compare against.
func percent(p *float64) string {
	if p == nil {
		return "-"
	}
	sign := ""
	if *p > 0 {
		sign = "+"
	}
	return sign + num(*p) + "%"
}

// mib renders a byte count as jq's "((x / 1048576) * 10 | round / 10)".
func mib(bytes float64) string { return num(stats.Round1(bytes / 1048576)) }

// mibOf is mib for a nullable byte count. A null renders as "-" rather than as
// a zero, because a metric that did not exist yet is not a measurement of zero.
func mibOf(bytes *float64) string {
	if bytes == nil {
		return "-"
	}
	return mib(*bytes)
}

// csvRow renders one row the way jq's @csv does: strings quoted with inner
// quotes doubled, numbers bare, and null as an empty field.
func csvRow(fields ...any) string {
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		switch value := field.(type) {
		case nil:
			out = append(out, "")
		case string:
			out = append(out, `"`+strings.ReplaceAll(value, `"`, `""`)+`"`)
		case *float64:
			if value == nil {
				out = append(out, "")
			} else {
				out = append(out, num(*value))
			}
		case *int:
			if value == nil {
				out = append(out, "")
			} else {
				out = append(out, strconv.Itoa(*value))
			}
		case float64:
			out = append(out, num(value))
		case int:
			out = append(out, strconv.Itoa(value))
		default:
			out = append(out, fmt.Sprint(value))
		}
	}
	return strings.Join(out, ",")
}
