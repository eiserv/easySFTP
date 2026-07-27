//go:build !linux

package metrics

// readUsage has no portable implementation. The benchmark runs on Linux, and
// the Go-level numbers (allocations, GC, goroutines, network bytes) are
// collected everywhere regardless; the process-level ones stay zero here
// rather than pulling in a per-OS dependency for a platform nothing measures on.
func readUsage() usage { return usage{} }
