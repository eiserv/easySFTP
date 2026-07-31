// Package link is the link half of the benchmark measurement (issue #184,
// phase 1), moved off scripts/benchmark-link.sh (issue #190, step 4).
//
// internal/benchmark/runner measures easySFTP. This package measures the path
// easySFTP runs over, and optionally shapes it, so RTT and bandwidth become
// variables instead of invisible constants. Two things live here:
//
//	Prober   run cmd/linkprobe once and record its document
//	Shaper   apply a "tc" profile, and take it down again
//
// Profile grammar, validated before a single byte is measured:
//
//	baseline | unshaped        the real line, untouched
//	+<N>ms                     netem delay, so +50ms means +50 ms of RTT
//	<delay>/<rate>             the same plus a tbf rate, e.g. +50ms/5mbit
//	baseline/<rate>            rate only, e.g. baseline/5mbit
//
// Shaping is egress only, which is the direction a deploy sends bytes in. The
// return path stays untouched, so a delay of N adds N to the round-trip rather
// than 2N.
package link

import (
	"fmt"
	"regexp"
	"strings"
)

// profilePattern is the grammar above. Kept as one expression so a typo is
// refused in one place, before the measuring starts rather than after hours of
// it.
var profilePattern = regexp.MustCompile(`^(baseline|unshaped|\+[0-9]+ms)(/[0-9]+(kbit|mbit|gbit))?$`)

// ParseProfiles validates the requested profiles, defaulting to the real line
// when none were asked for.
func ParseProfiles(raw string) ([]string, error) {
	profiles := strings.Fields(raw)
	if len(profiles) == 0 {
		return []string{"baseline"}, nil
	}
	for _, profile := range profiles {
		if !profilePattern.MatchString(profile) {
			return nil, fmt.Errorf(
				"link profile '%s' is not understood. Use baseline, +50ms, +50ms/5mbit or baseline/5mbit", profile)
		}
	}
	return profiles, nil
}

// Delay is the netem delay of a profile, empty when it has none.
func Delay(profile string) string {
	head, _, _ := strings.Cut(profile, "/")
	if strings.HasPrefix(head, "+") {
		return strings.TrimPrefix(head, "+")
	}
	return ""
}

// Rate is the tbf rate of a profile, empty when it has none.
func Rate(profile string) string {
	if _, rate, found := strings.Cut(profile, "/"); found {
		return rate
	}
	return ""
}

// Slug is a profile as a filename component. Profiles contain "+" and "/", and
// a log file called "+50ms/5mbit-small-1.log" is a missing directory, not a
// log.
func Slug(profile string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, profile)
}

// ShapeNeeded reports whether any of these profiles asks for shaping. A sweep
// that only wants the real line must not need tc, sudo or NET_ADMIN.
func ShapeNeeded(profiles []string) bool {
	for _, profile := range profiles {
		if Delay(profile) != "" || Rate(profile) != "" {
			return true
		}
	}
	return false
}
