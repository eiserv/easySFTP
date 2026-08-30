package uploader

import (
	"math/rand/v2"
	"time"
)

// retryBackoff is exponential backoff with up to 25 percent positive jitter.
// The retry number starts at one. Positive jitter keeps the established
// minimum delay while ensuring workers and concurrent jobs which lost the
// same server do not all retry on the same second boundary.
func retryBackoff(retry int) time.Duration {
	retry = max(retry, 1)
	base := time.Duration(1<<(retry-1)) * time.Second
	jitterWindow := base / 4
	return base + time.Duration(rand.Int64N(int64(jitterWindow)+1))
}
