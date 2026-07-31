package link_test

import (
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/benchmark/link"
)

// The grammar is validated before a single byte is measured, which is the whole
// point of doing it up front rather than after hours of uploading.
func TestParseProfiles(t *testing.T) {
	if got, err := link.ParseProfiles(""); err != nil || len(got) != 1 || got[0] != "baseline" {
		t.Errorf("an empty request parsed as %v (%v), want the real line", got, err)
	}
	good := "baseline unshaped +50ms +50ms/5mbit baseline/500kbit +150ms/1gbit"
	got, err := link.ParseProfiles(good)
	if err != nil {
		t.Fatalf("parsing %q: %v", good, err)
	}
	if strings.Join(got, " ") != good {
		t.Errorf("parsed %q, want the input back in order", strings.Join(got, " "))
	}
	for _, bad := range []string{"+50", "50ms", "baseline/5mb", "+50ms/", "fast", "+50ms/5mbit/3"} {
		if _, err := link.ParseProfiles(bad); err == nil {
			t.Errorf("profile %q was accepted", bad)
		}
	}
}

func TestProfileParts(t *testing.T) {
	for _, tc := range []struct {
		profile, delay, rate, slug string
		shapes                     bool
	}{
		{"baseline", "", "", "baseline", false},
		{"unshaped", "", "", "unshaped", false},
		{"+50ms", "50ms", "", "_50ms", true},
		{"+50ms/5mbit", "50ms", "5mbit", "_50ms_5mbit", true},
		{"baseline/5mbit", "", "5mbit", "baseline_5mbit", true},
	} {
		if got := link.Delay(tc.profile); got != tc.delay {
			t.Errorf("%s: delay %q, want %q", tc.profile, got, tc.delay)
		}
		if got := link.Rate(tc.profile); got != tc.rate {
			t.Errorf("%s: rate %q, want %q", tc.profile, got, tc.rate)
		}
		// Profiles contain "+" and "/", and a log file called
		// "+50ms/5mbit-small-1.log" is a missing directory, not a log.
		if got := link.Slug(tc.profile); got != tc.slug {
			t.Errorf("%s: slug %q, want %q", tc.profile, got, tc.slug)
		}
		if got := link.ShapeNeeded([]string{tc.profile}); got != tc.shapes {
			t.Errorf("%s: needs shaping %v, want %v", tc.profile, got, tc.shapes)
		}
	}

	// A sweep that only wants the real line must not need tc, sudo or
	// NET_ADMIN, and one profile that does is enough to need them.
	if link.ShapeNeeded([]string{"baseline", "unshaped"}) {
		t.Error("the real line alone asked for shaping")
	}
	if !link.ShapeNeeded([]string{"baseline", "+50ms"}) {
		t.Error("a shaped profile in the list did not ask for shaping")
	}
}
