package autotune_test

import (
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/autotune"
)

// TestSummarizeUploadsSeparatesShapesWithEqualTotals is the distinction stage 1
// of issue #215 exists for. Two deployments can carry the same number of bytes
// in the same number of files, with the same largest file, and still be
// completely different work: the policy has to be able to see that.
func TestSummarizeUploadsSeparatesShapesWithEqualTotals(t *testing.T) {
	// A: 99 medium files and one archive.
	a := make([]int64, 0, 100)
	for range 99 {
		a = append(a, 320*KiB)
	}
	a = append(a, 4*MiB)

	// B: the same 100 files carrying the same total, as a heap of tiny ones
	// with a handful of archives among them.
	b := make([]int64, 0, 100)
	for range 90 {
		b = append(b, 4*KiB)
	}
	for range 8 {
		b = append(b, 4*MiB)
	}
	b = append(b, 1324*KiB, 1324*KiB)

	ws, wb := autotune.SummarizeUploads(a), autotune.SummarizeUploads(b)
	if ws.Uploads != wb.Uploads || ws.UploadBytes != wb.UploadBytes || ws.LargestUpload != wb.LargestUpload {
		t.Fatalf("the two shapes were meant to agree on everything the old model saw:\n A %+v\n B %+v", ws, wb)
	}
	if ws.SmallUploads != 0 {
		t.Errorf("no file in A fits in one write packet, got SmallUploads=%d", ws.SmallUploads)
	}
	if wb.SmallUploads != 90 {
		t.Errorf("90 files in B fit in one write packet, got SmallUploads=%d", wb.SmallUploads)
	}
	if ws.P50Upload <= wb.P50Upload {
		t.Errorf("A's median (%d) must be above B's (%d): A has no tiny files at all", ws.P50Upload, wb.P50Upload)
	}
	if ws.P90Upload <= wb.P90Upload {
		t.Errorf("A's ninetieth percentile (%d) must be above B's (%d): nine files in ten of B are 4 KiB",
			ws.P90Upload, wb.P90Upload)
	}
}

// TestSummarizeUploadsIsNearestRank pins the percentile definition, because
// every reader of P50Upload and P90Upload is a threshold comparison and an
// off-by-one bucket is exactly the kind of thing that only shows up as a
// mysterious two percent in a sweep.
func TestSummarizeUploadsIsNearestRank(t *testing.T) {
	var sizes []int64
	for range 40 {
		sizes = append(sizes, 16*KiB)
	}
	for range 12 {
		sizes = append(sizes, 256*KiB)
	}
	for range 4 {
		sizes = append(sizes, 2*MiB)
	}
	w := autotune.SummarizeUploads(sizes)

	switch {
	case w.Uploads != 56:
		t.Errorf("files = %d, want 56", w.Uploads)
	case w.UploadBytes != 40*16*KiB+12*256*KiB+4*2*MiB:
		t.Errorf("bytes = %d", w.UploadBytes)
	case w.LargestUpload != 2*MiB:
		t.Errorf("largest = %d, want 2 MiB", w.LargestUpload)
	case w.P50Upload != 16*KiB:
		t.Errorf("p50 = %d, want 16 KiB: the 28th of 56 files", w.P50Upload)
	case w.P90Upload != 256*KiB:
		t.Errorf("p90 = %d, want 256 KiB: the 51st of 56 files", w.P90Upload)
	case w.SmallUploads != 40:
		t.Errorf("small = %d, want the 40 files that fit in one 32 KiB packet", w.SmallUploads)
	}

	if empty := autotune.SummarizeUploads(nil); empty != (autotune.Workload{}) {
		t.Errorf("summarizing nothing produced %+v", empty)
	}
	if one := autotune.SummarizeUploads([]int64{7}); one.P50Upload != 7 || one.P90Upload != 7 {
		t.Errorf("a single file is its own median and its own p90, got %+v", one)
	}
}

// TestRequestConcurrencySeesTheDistribution is the 'mixed' miss of issue #215.
// The scenario is 40 x 16 KiB + 12 x 256 KiB + 4 x 2 MiB: the memory budget
// used to divide 56 workers into the in-flight ceiling as though all 56 of them
// were the 2 MiB file, and the transfer ran with a request_concurrency of 18.
// Only a tenth of them can be above the ninetieth percentile, which is what
// makes the real bound far looser.
func TestRequestConcurrencySeesTheDistribution(t *testing.T) {
	var sizes []int64
	for range 40 {
		sizes = append(sizes, 16*KiB)
	}
	for range 12 {
		sizes = append(sizes, 256*KiB)
	}
	for range 4 {
		sizes = append(sizes, 2*MiB)
	}
	mixed := autotune.SummarizeUploads(sizes)

	if got := autotune.Plan(mixed, house, autotune.Fixed{}).RequestConcurrency; got != 64 {
		t.Errorf("request_concurrency = %d, want the 2 MiB files fully pipelined at 64: "+
			"the stored 'mixed' sweep is 10%% faster at 4/56/64 than at 4/56/16", got)
	}

	// The same file count where every file really is large: now the budget is
	// the real constraint and it still binds.
	var heavy []int64
	for range 56 {
		heavy = append(heavy, 2*MiB)
	}
	if got := autotune.Plan(autotune.SummarizeUploads(heavy), house, autotune.Fixed{}).RequestConcurrency; got > 16 {
		t.Errorf("request_concurrency = %d: 56 files that are all 2 MiB cannot all hold a full pipeline", got)
	}
}

// TestRequestConcurrencyIsAPowerOfTwo: the value is a pipeline depth, not a
// measurement. 18 invites a reader to believe the eight means something.
func TestRequestConcurrencyIsAPowerOfTwo(t *testing.T) {
	for _, w := range []autotune.Workload{
		{Uploads: 56, UploadBytes: 12 * MiB, LargestUpload: 2 * MiB, P90Upload: 256 * KiB},
		{Uploads: 300, UploadBytes: 300 * 4 * KiB, LargestUpload: 4 * KiB, P90Upload: 4 * KiB},
		{Uploads: 7, UploadBytes: 70 * MiB, LargestUpload: 10 * MiB, P90Upload: 10 * MiB},
	} {
		got := autotune.Plan(w, house, autotune.Fixed{}).RequestConcurrency
		if got&(got-1) != 0 {
			t.Errorf("request_concurrency = %d for %+v, want a power of two", got, w)
		}
	}
}

// TestBDPDrivesRequestConcurrencyOnlyOnceMeasured is stage 3. The number of
// bytes a stream has to keep in flight is bandwidth times delay, but bandwidth
// is the one thing nothing can measure before the transfer starts. So a link
// that has been measured gets a BDP-sized pipeline, and a link that has not
// keeps the conservative pre-#215 answer.
func TestBDPDrivesRequestConcurrencyOnlyOnceMeasured(t *testing.T) {
	// One 8 MiB file, so nothing but the link can decide the answer.
	w := autotune.Workload{Uploads: 4, UploadBytes: 32 * MiB, LargestUpload: 8 * MiB, P90Upload: 8 * MiB}

	prior := autotune.Link{RTT: 100 * time.Millisecond, Handshake: 300 * time.Millisecond}
	if got := autotune.Plan(w, prior, autotune.Fixed{}).RequestConcurrency; got != 64 {
		t.Errorf("an unmeasured link must keep pipelining the file as far as it goes, got %d", got)
	}

	// A long fat path: 40 MiB/s over 100 ms is 4 MiB in flight per stream, so
	// the file wants every packet the channel window has.
	fat := prior
	fat.StreamBytesPerSecond = 40 * MiB
	fat.ThroughputSource = autotune.SourceCached
	if got := autotune.Plan(w, fat, autotune.Fixed{}).RequestConcurrency; got != 64 {
		t.Errorf("a measured long fat path must pipeline fully, got %d", got)
	}

	// A path measured as narrow: 300 KiB/s over 13 ms is four kilobytes in
	// flight. Holding two megabytes of one file open for that buys nothing,
	// and the policy floor is what stops it going lower still.
	narrow := autotune.Link{RTT: 13 * time.Millisecond, Handshake: 300 * time.Millisecond,
		StreamBytesPerSecond: 300 * KiB, ThroughputSource: autotune.SourceRuntime}
	if got := autotune.Plan(w, narrow, autotune.Fixed{}).RequestConcurrency; got != autotune.MinRequestConcurrency {
		t.Errorf("a measured narrow path should fall back to the floor, got %d", got)
	}

	// A LAN: enormous bandwidth, but a fifth of a millisecond of delay holds
	// almost nothing in flight. Bandwidth alone is not the argument for
	// pipelining; bandwidth times delay is.
	lan := autotune.Link{RTT: 200 * time.Microsecond, Handshake: 20 * time.Millisecond,
		StreamBytesPerSecond: 100 * MiB, ThroughputSource: autotune.SourceRuntime}
	if got := autotune.Plan(w, lan, autotune.Fixed{}).RequestConcurrency; got != autotune.MinRequestConcurrency {
		t.Errorf("a fast link with no delay needs no deep pipeline, got %d", got)
	}

	// And whatever the link says, a file that is one packet long cannot use a
	// second request.
	tiny := autotune.Workload{Uploads: 500, UploadBytes: 2 * MiB, LargestUpload: 4 * KiB, P90Upload: 4 * KiB}
	if got := autotune.Plan(tiny, fat, autotune.Fixed{}).RequestConcurrency; got != autotune.MinRequestConcurrency {
		t.Errorf("4 KiB files cannot use a pipeline, got %d", got)
	}
}

// TestLinkReportsWhereItsThroughputCameFrom: the hierarchy of evidence is part
// of the policy, so it has to be readable rather than implied.
func TestLinkReportsWhereItsThroughputCameFrom(t *testing.T) {
	for _, tc := range []struct {
		name string
		link autotune.Link
		want autotune.Source
		text string
	}{
		{"nothing measured", house, autotune.SourcePrior, "assumed"},
		{"a throughput with no provenance is this run's own", autotune.Link{StreamBytesPerSecond: 1 * MiB}, autotune.SourceRuntime, "measured"},
		{"a cached one says so", autotune.Link{StreamBytesPerSecond: 1 * MiB, ThroughputSource: autotune.SourceCached}, autotune.SourceCached, "cached"},
		{"a source without a number is still the prior", autotune.Link{ThroughputSource: autotune.SourceCached}, autotune.SourcePrior, "assumed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.link.Source(); got != tc.want {
				t.Errorf("Source() = %v, want %v", got, tc.want)
			}
			if got := tc.link.Source().String(); got != tc.text {
				t.Errorf("Source().String() = %q, want %q", got, tc.text)
			}
		})
	}

	l := autotune.Link{RTT: 100 * time.Millisecond, StreamBytesPerSecond: 10 * MiB}
	if got := l.BDPBytes(); got != MiB {
		t.Errorf("BDPBytes() = %d, want 1 MiB: 10 MiB/s for a tenth of a second", got)
	}
	if got := (autotune.Link{}).BDPBytes(); got <= 0 {
		t.Errorf("an unmeasured link must still report a usable BDP, got %d", got)
	}
}
