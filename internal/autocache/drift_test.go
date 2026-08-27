package autocache

import (
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/autotune"
)

// The failure mode this file exists for.
//
// A deployment that grows a few per cent per run is, at every single step,
// almost identical to the run before it. A cache that asks "does this look
// like last time?" therefore answers yes forever, and a hundred runs later it
// is still handing back something measured on a deploy a fraction of the
// current size. Nothing ever looks wrong from inside such a cache: every
// comparison it makes passes, comfortably.
//
// The rule that prevents it is not a tighter band (there is no band that
// separates 100 files from 103), it is what the comparison is made against.
// A record is anchored to the deploy its measurement was taken on, and a run
// that merely *uses* the record does not move that anchor. So the drift is
// measured from the measurement, accumulates, and eventually leaves the band,
// which is exactly what should happen.
//
// TestDriftLeavesTheBandInsteadOfDraggingItAlong runs both readings side by
// side on the same sequence of deploys.
func TestDriftLeavesTheBandInsteadOfDraggingItAlong(t *testing.T) {
	const (
		runs       = 120
		growth     = 1.03 // three per cent per deploy
		firstFiles = 400
	)

	// growingTree is the deploy on run n: the same kind of files, more of them
	// every time. A quarter of a megabyte each, so the deploy stays the kind
	// of work a throughput can be measured on and the categorical gate never
	// fires; what moves is only the size of it.
	growingTree := func(n int) Workload {
		files := firstFiles
		for i := 0; i < n; i++ {
			files = int(float64(files) * growth)
		}
		return Workload{
			Files:   files,
			Bytes:   int64(files) * 256 * 1024,
			P50:     256 * 1024,
			P90:     256 * 1024,
			Largest: 4 << 20,
		}
	}

	anchored := stored(growingTree(0), measuredLink())
	lastHit := -1
	for n := 1; n < runs; n++ {
		at := now.Add(time.Duration(n) * time.Hour)
		w := growingTree(n)

		// Consecutive deploys are indistinguishable, which is the whole trap:
		// a cache comparing each run with the one before it never sees a
		// reason to stop.
		if why, ok := similar(growingTree(n-1), w); !ok {
			t.Fatalf("run %d already differs from run %d (%s); pick a slower growth for this test", n, n-1, why)
		}

		cand, dec := anchored.Lookup(target, w, at)
		if dec.Reason == "" {
			dec = cand.Validate(13 * time.Millisecond)
		}
		if dec.Hit {
			lastHit = n
		}
		// A run too small to measure anything, which is the case that can
		// carry a record forward at all.
		anchored.Update(Observation{
			Target:   target,
			Workload: w,
			Link:     Link{RTTMillis: 13, HandshakeMillis: 380},
			Reused:   dec.Hit,
		}, at)

		if rec, _ := anchored.find(target); rec.Workload != growingTree(0) {
			t.Fatalf("run %d moved the anchor to %+v without measuring anything", n, rec.Workload)
		}
	}

	switch {
	case lastHit < 0:
		t.Fatal("the cache never hit at all, so this test proves nothing about drift")
	case lastHit >= runs-1:
		t.Fatalf("the cache still hit on the last run, with the deploy %.1fx its original size",
			float64(growingTree(runs-1).Files)/firstFiles)
	}
	grown := float64(growingTree(lastHit).Files) / firstFiles
	if grown > 2.1 {
		t.Errorf("the cache kept hitting until the deploy was %.1fx its original size", grown)
	}
	t.Logf("the anchored cache stopped hitting at run %d, with the deploy %.2fx its original size", lastHit+1, grown)

	// The same sequence read the way an unclean implementation would read it:
	// compare against the previous run and re-anchor every time. It never
	// stops hitting, however far the deploy has travelled.
	naive := stored(growingTree(0), measuredLink())
	for n := 1; n < runs; n++ {
		w := growingTree(n)
		if _, dec := naive.Lookup(target, w, now.Add(time.Duration(n)*time.Hour)); dec.Reason != "" {
			t.Fatalf("comparing against the previous run missed at run %d (%s), which the trap does not do", n, dec.Reason)
		}
		naive.put(Record{
			Target:        target,
			PolicyVersion: 1,
			MeasuredAt:    now.Add(time.Duration(n) * time.Hour),
			Workload:      w,
			Link:          measuredLink(),
		})
	}
	if float64(growingTree(runs-1).Files)/firstFiles < 10 {
		t.Fatal("the deploy did not grow far enough for the contrast to mean anything")
	}
}

// TestDriftOnTheLinkAxis is the same trap one axis over. The round-trip time
// stored in a record is the yardstick the next run is validated against, so a
// record that took today's RTT along while keeping yesterday's throughput
// would let the path move as far as it liked, a per cent at a time.
func TestDriftOnTheLinkAxis(t *testing.T) {
	s := stored(mediumTree(), measuredLink())

	rtt := 13.0
	hits := 0
	for n := 1; n < 200; n++ {
		at := now.Add(time.Duration(n) * time.Hour)
		rtt *= 1.03

		cand, dec := s.Lookup(target, mediumTree(), at)
		if dec.Reason == "" {
			dec = cand.Validate(time.Duration(rtt * float64(time.Millisecond)))
		}
		if !dec.Hit {
			break
		}
		hits++
		s.Update(Observation{
			Target:   target,
			Workload: mediumTree(),
			Link:     Link{RTTMillis: rtt, HandshakeMillis: 380},
			Reused:   true,
		}, at)

		if rec, _ := s.find(target); rec.Link.RTTMillis != 13 {
			t.Fatalf("run %d moved the stored round-trip time to %.1f ms without measuring a throughput", n, rec.Link.RTTMillis)
		}
	}
	if hits == 0 {
		t.Fatal("the cache never hit, so this proves nothing")
	}
	if rtt < 26 {
		t.Errorf("the cache stopped at %.1f ms, before the path had doubled; the band is meant to allow that much", rtt)
	}
	t.Logf("the cache stopped reusing after %d runs, at %.1f ms against the 13.0 ms it was measured at", hits, rtt)
}

// TestAMeasuringRunIsAllowedToMoveTheAnchor is the other half of the rule, and
// the reason it is about evidence rather than about age. A run that measures
// the link afresh describes the deploy it measured, so its anchor is the
// current one and the cache keeps working for a repository that really is
// growing.
func TestAMeasuringRunIsAllowedToMoveTheAnchor(t *testing.T) {
	files := 400
	s := stored(Workload{Files: files, Bytes: int64(files) * 256 * 1024, P50: 256 * 1024, P90: 256 * 1024, Largest: 4 << 20}, measuredLink())

	for n := 1; n < 60; n++ {
		at := now.Add(time.Duration(n) * time.Hour)
		files = int(float64(files) * 1.03)
		w := Workload{Files: files, Bytes: int64(files) * 256 * 1024, P50: 256 * 1024, P90: 256 * 1024, Largest: 4 << 20}

		cand, dec := s.Lookup(target, w, at)
		if dec.Reason == "" {
			dec = cand.Validate(13 * time.Millisecond)
		}
		if !dec.Hit {
			t.Fatalf("run %d missed (%s), although every run measured the link for itself", n, dec.Reason)
		}
		s.Update(Observation{
			Target:   target,
			Workload: w,
			Link:     Link{RTTMillis: 13, HandshakeMillis: 380, StreamBytesPerSecond: 8 << 20},
			Measured: true,
			Reused:   true,
		}, at)
	}
	if rec, _ := s.find(target); rec.Workload.Files != files {
		t.Errorf("the anchor is %d files, want the %d the last measurement was taken on", rec.Workload.Files, files)
	}
}

// TestDriftWithNoStoredThroughput is the same trap through the one door that
// used to be left open (issue #225).
//
// A record whose measurement produced no per-connection throughput is written
// by any deploy that never fills a stream, which is most small deploys, and is
// exactly what the cache is most often pointed at. Update used to require a
// stored throughput before it would protect an anchor, so those records fell
// through to being rebuilt from the current run: they compared each run against
// the one before it, hit forever, and dragged the anchor along. Reused was
// carried in the same skipped branch, so the reuse budget never bit either.
//
// The record still has to work: it carries the round-trip time and the
// connection ceiling, so it must keep hitting for a deploy that has not really
// changed. What it must not do is keep hitting for one that has.
func TestDriftWithNoStoredThroughput(t *testing.T) {
	const (
		runs       = 120
		growth     = 1.03
		firstFiles = 400
	)

	growingTree := func(n int) Workload {
		files := firstFiles
		for i := 0; i < n; i++ {
			files = int(float64(files) * growth)
		}
		return Workload{
			Files:   files,
			Bytes:   int64(files) * 256 * 1024,
			P50:     256 * 1024,
			P90:     256 * 1024,
			Largest: 4 << 20,
		}
	}

	// No throughput, but a ceiling: this is a record worth keeping, which is
	// why the answer to #225 could not simply be to throw such records away.
	// How long the ceiling itself survives is a separate rule with its own
	// lifetime (MaxCeilingCarry); what is under test here is the anchor.
	noThroughput := Link{RTTMillis: 13, HandshakeMillis: 380}
	anchored := stored(growingTree(0), noThroughput, func(r *Record) { r.ConnectionCeiling = 4 })

	lastHit, reuses := -1, 0
	for n := 1; n < runs; n++ {
		at := now.Add(time.Duration(n) * time.Hour)
		w := growingTree(n)

		cand, dec := anchored.Lookup(target, w, at)
		if dec.Reason == "" {
			dec = cand.Validate(13 * time.Millisecond)
		}
		if dec.Hit {
			lastHit = n
			reuses++
		}
		anchored.Update(Observation{
			Target:   target,
			Workload: w,
			Link:     noThroughput,
			Reused:   dec.Hit,
		}, at)

		rec, _ := anchored.find(target)
		if rec.Workload != growingTree(0) {
			t.Fatalf("run %d moved the anchor to %+v without measuring anything", n, rec.Workload)
		}
		if !rec.MeasuredAt.Equal(now) {
			t.Fatalf("run %d moved measured_at to %v without measuring anything", n, rec.MeasuredAt)
		}
		if rec.Reused != reuses {
			t.Fatalf("run %d left the reuse count at %d after %d reuses, so MaxReuse can never bite", n, rec.Reused, reuses)
		}
	}

	switch {
	case lastHit < 0:
		t.Fatal("the cache never hit at all, so this test proves nothing about drift")
	case lastHit >= runs-1:
		t.Fatalf("the cache still hit on the last run, with the deploy %.1fx its original size",
			float64(growingTree(runs-1).Files)/firstFiles)
	}
	grown := float64(growingTree(lastHit).Files) / firstFiles
	if grown > 2.1 {
		t.Errorf("the cache kept hitting until the deploy was %.1fx its original size", grown)
	}
	t.Logf("a throughput-less record stopped hitting at run %d, with the deploy %.2fx its original size", lastHit+1, grown)

	// The pre-#225 reading, reproduced: with no stored throughput the guarded
	// branch was skipped, so the record was rebuilt from the current run every
	// time. It never stops hitting, however far the deploy has travelled.
	naive := stored(growingTree(0), noThroughput, func(r *Record) { r.ConnectionCeiling = 4 })
	for n := 1; n < runs; n++ {
		at := now.Add(time.Duration(n) * time.Hour)
		w := growingTree(n)
		if _, dec := naive.Lookup(target, w, at); dec.Reason != "" {
			t.Fatalf("the pre-fix reading missed at run %d (%s), which it did not do", n, dec.Reason)
		}
		naive.put(Record{
			Target:            target,
			PolicyVersion:     autotune.PolicyVersion,
			MeasuredAt:        at,
			LastUsed:          at,
			Workload:          w,
			Link:              noThroughput,
			ConnectionCeiling: 4,
		})
	}
	if float64(growingTree(runs-1).Files)/firstFiles < 10 {
		t.Fatal("the deploy did not grow far enough for the contrast to mean anything")
	}
}
