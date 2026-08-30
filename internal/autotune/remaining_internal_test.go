package autotune

import "testing"

func TestRemainingPreservesLargestUploadInsteadOfMean(t *testing.T) {
	const mib = int64(1024 * 1024)
	c := &Controller{workload: Workload{
		Uploads: 100, UploadBytes: 107 * mib,
		LargestUpload: 8 * mib, P50Upload: mib, P90Upload: mib,
	}}

	remaining := c.remaining(Progress{Uploaded: 90, Remaining: 10, RemainingBytes: 17 * mib})
	if remaining.LargestUpload != 8*mib {
		t.Fatalf("LargestUpload = %d, want the planned maximum %d instead of the mean %d", remaining.LargestUpload, 8*mib, remaining.UploadBytes/int64(remaining.Uploads))
	}

	remaining = c.remaining(Progress{Uploaded: 99, Remaining: 1, RemainingBytes: 2 * mib})
	if remaining.LargestUpload != 2*mib {
		t.Fatalf("LargestUpload = %d, want it capped by the %d remaining bytes", remaining.LargestUpload, 2*mib)
	}
}
