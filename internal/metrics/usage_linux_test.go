//go:build linux

package metrics

import (
	"crypto/sha256"
	"testing"
)

// TestReadUsageOnLinux pins the one part of the metrics that is not portable
// Go: getrusage(2) and /proc/self/io. Without it, a broken conversion here
// would only show up as a zero in a stored benchmark, which reads like "the
// run used no CPU" rather than like a bug.
func TestReadUsageOnLinux(t *testing.T) {
	// Burn a measurable slice of CPU first: a test binary that has barely
	// started can legitimately report 0 ms of user time.
	h := sha256.New()
	buf := make([]byte, 4096)
	for range 20000 {
		h.Write(buf)
	}
	_ = h.Sum(nil)

	u := readUsage()
	if u.userMS <= 0 {
		t.Errorf("user CPU time = %v ms, want a positive value", u.userMS)
	}
	if u.sysMS < 0 {
		t.Errorf("system CPU time = %v ms, want a non-negative value", u.sysMS)
	}
	// A Go process holds several MiB before it runs a line of test code, so a
	// plausible peak RSS is the check that the kilobyte conversion is applied.
	if u.maxRSSBytes < 1<<20 {
		t.Errorf("peak RSS = %d bytes, want at least 1 MiB", u.maxRSSBytes)
	}
	if u.diskReadBytes < 0 || u.diskWriteBytes < 0 {
		t.Errorf("disk I/O counters are negative: %+v", u)
	}
}

func TestProcIOUnknownKeyIsZero(t *testing.T) {
	if got := procIO("no_such_counter"); got != 0 {
		t.Errorf("procIO of an unknown key = %d, want 0", got)
	}
}
