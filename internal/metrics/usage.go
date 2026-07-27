package metrics

// usage is the per-OS part of the process metrics: what only the kernel knows.
// Zero values mean "not available on this platform", never "the run used none".
type usage struct {
	userMS         float64
	sysMS          float64
	maxRSSBytes    int64
	diskReadBytes  int64
	diskWriteBytes int64
}
