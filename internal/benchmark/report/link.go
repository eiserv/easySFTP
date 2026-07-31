package report

import (
	"strings"

	"github.com/eiserv/easySFTP/internal/benchmark/schema"
)

// linkSection is link_markdown from scripts/benchmark-link.sh: the section that
// says which path the numbers were measured over.
//
// A result without a link object gets no section at all, which is how a run
// measured before the probe existed reads.
func linkSection(b *buf, link *schema.Link) {
	if link == nil {
		return
	}
	b.line("### The link")
	b.line("")
	b.line("| Profile | When | RTT p50 | RTT p90 | Handshake | Control 1 stream | Control N streams | Host load |")
	b.line("|---|---|---|---|---|---|---|---|")
	for _, p := range link.Probes {
		b.line("| %s | %s | %s ms | %s ms | %s ms | %s MiB/s | %s MiB/s | %s |",
			orDash(p.Profile), orDash(p.At),
			dash(rtt(p, func(r *schema.RTT) *float64 { return r.P50 })),
			dash(rtt(p, func(r *schema.RTT) *float64 { return r.P90 })),
			dash(p.HandshakeMS),
			dash(control(p, func(c *schema.Control) *float64 { return c.SingleStreamMiBPerS })),
			dash(control(p, func(c *schema.Control) *float64 { return c.NStreamMiBPerS })),
			hostLoad(p))
	}
	if len(link.Probes) == 0 {
		b.line("| - | - | - | - | - | - | - | - |")
		b.line("")
		b.line("No link probe ran for this result, so the numbers above cannot be told apart from the line they were measured on.")
	}
	b.line("")
	b.line("%s", shapingSentence(link.Shaping))
	b.line("")
	b.line("The control measurement uses `x/crypto/ssh` and `pkg/sftp` directly, never easySFTP's uploader. It separates \"the line is slow\" from \"easySFTP is slow\", and a single-stream control close to a scenario's own MiB/s means the run was network bound, where a code delta says nothing.")
	b.line("")
}

// shapingSentence distinguishes three states, and conflating the first two is
// exactly the kind of thing that makes a stored result unreadable a month
// later: nothing was asked for, something was asked for and could not be done,
// or it was done.
func shapingSentence(s schema.Shaping) string {
	shaped := 0
	for _, profile := range s.Requested {
		if strings.ContainsAny(profile, "+/") {
			shaped++
		}
	}
	switch {
	case shaped == 0:
		return "No link shaping was requested: every profile here is the real line."
	case s.Available:
		return "Link shaping was available. Requested profiles: " + strings.Join(s.Requested, ", ") +
			"; applied: " + strings.Join(s.Applied, ", ") + "."
	default:
		reason := "unknown"
		if s.Reason != nil && *s.Reason != "" {
			reason = *s.Reason
		}
		return "Link shaping was **not** available (" + reason + "), so every profile was measured on the real line and the profile names say what was asked for, not what happened."
	}
}

func rtt(p schema.Probe, field func(*schema.RTT) *float64) *float64 {
	if p.RTTMS == nil {
		return nil
	}
	return field(p.RTTMS)
}

func control(p schema.Probe, field func(*schema.Control) *float64) *float64 {
	if p.Control == nil {
		return nil
	}
	return field(p.Control)
}

// hostLoad prints "n/a" where the host's load could not be read at all, which
// is a different statement from a load of zero.
//
// A probe that reports the load as available and then carries no value is the
// probe contradicting itself, and this prints that rather than hiding it as
// "n/a": the reader should see the contradiction, not a plausible gap.
func hostLoad(p schema.Probe) string {
	if p.HostLoad == nil || !p.HostLoad.Available {
		return "n/a"
	}
	if p.HostLoad.Load1 == nil {
		return "null"
	}
	return num(*p.HostLoad.Load1)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
