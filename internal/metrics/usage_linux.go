//go:build linux

package metrics

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// readUsage reads the kernel's own accounting for this process: CPU time and
// peak resident set from getrusage(2), block-layer I/O from /proc/self/io.
// Both are cheap one-shot reads at the end of a run and need no dependency
// beyond the standard library.
//
// /proc/self/io's read_bytes/write_bytes count what actually reached the block
// device, so a payload served from the page cache reads as 0. That is the
// honest number for "did this run touch the disk"; the bytes easySFTP moved
// are counted separately, at the application level.
func readUsage() usage {
	u := usage{}
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		u.userMS = round2(float64(ru.Utime.Nano()) / 1e6)
		u.sysMS = round2(float64(ru.Stime.Nano()) / 1e6)
		// Linux reports ru_maxrss in kilobytes. The conversion is explicit
		// because Rusage.Maxrss is int32 on 32-bit Linux and int64 elsewhere.
		u.maxRSSBytes = int64(ru.Maxrss) * 1024
	}
	u.diskReadBytes = procIO("read_bytes")
	u.diskWriteBytes = procIO("write_bytes")
	return u
}

func procIO(key string) int64 {
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || name != key {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}
