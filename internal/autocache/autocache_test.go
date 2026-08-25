package autocache

import (
	"strings"
	"testing"
	"time"

	"github.com/eiserv/easySFTP/internal/autotune"
)

// now is the fixed clock every test here reads. Nothing in this package calls
// time.Now, which is what lets the age and reuse rules be tested by arithmetic
// instead of by waiting.
var now = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

const target = "sha256:0123456789abcdef"

// mediumTree is the anchor most of these tests use: a thousand files of a
// quarter megabyte each, which is bandwidth-bound (its files can fill a
// stream) and therefore the kind of deploy a throughput can be measured on.
func mediumTree() Workload {
	return Workload{Files: 1000, Bytes: 1000 * 256 * 1024, P50: 256 * 1024, P90: 256 * 1024, Largest: 4 << 20}
}

// stored builds a store holding one record for target, measured just now.
func stored(w Workload, l Link, mutate ...func(*Record)) *Store {
	rec := Record{
		Target:        target,
		PolicyVersion: autotune.PolicyVersion,
		MeasuredAt:    now,
		LastUsed:      now,
		Workload:      w,
		Link:          l,
		Settings:      Settings{Connections: 4, Concurrency: 64, RequestConcurrency: 32},
	}
	for _, m := range mutate {
		m(&rec)
	}
	return &Store{Version: FormatVersion, Records: []Record{rec}}
}

func measuredLink() Link {
	return Link{RTTMillis: 13, HandshakeMillis: 380, StreamBytesPerSecond: 8 << 20}
}

// TestLookupRestoresAMeasurement is the whole point of the package: a run that
// looks like the one that was measured, over a link that still measures the
// same, gets the throughput and the ceiling handed to it.
func TestLookupRestoresAMeasurement(t *testing.T) {
	s := stored(mediumTree(), measuredLink(), func(r *Record) { r.ConnectionCeiling = 4 })

	cand, dec := s.Lookup(target, mediumTree(), now)
	if dec.Reason != "" {
		t.Fatalf("pre-connection gates rejected an identical workload: %s", dec.Reason)
	}
	dec = cand.Validate(13 * time.Millisecond)
	switch {
	case !dec.Hit:
		t.Fatalf("an unchanged link was rejected: %s", dec.Reason)
	case !dec.Restores():
		t.Fatalf("a hit carrying a throughput and a ceiling restores nothing: %+v", dec)
	case dec.StreamBytesPerSecond != 8<<20:
		t.Errorf("restored %.0f B/s, want %d", dec.StreamBytesPerSecond, 8<<20)
	case dec.ConnectionCeiling != 4:
		t.Errorf("restored ceiling %d, want 4", dec.ConnectionCeiling)
	}
}

// TestLookupAcceptsTheIssuesOwnExample: issue #212 asks for similarity, not
// equality, and names the case. 980 files must still reuse a result measured
// on 1,000.
func TestLookupAcceptsTheIssuesOwnExample(t *testing.T) {
	anchor := mediumTree()
	s := stored(anchor, measuredLink())

	nearly := anchor
	nearly.Files = 980
	nearly.Bytes = int64(980) * 256 * 1024

	if _, dec := s.Lookup(target, nearly, now); dec.Reason != "" {
		t.Fatalf("980 files did not match an anchor of 1,000: %s", dec.Reason)
	}
}

// TestLookupGates walks the reasons a record is not reused. Each one is a
// separate sentence in the log, because "the cache never helps" is a question
// with several different answers.
func TestLookupGates(t *testing.T) {
	anchor := mediumTree()

	doubled := anchor
	doubled.Files = 2500
	doubled.Bytes = int64(2500) * 256 * 1024

	tiny := Workload{Files: 1000, Bytes: 1000 * 4096, P50: 4096, P90: 4096, Largest: 4096, SmallFiles: 1000}

	mostlyTiny := anchor
	mostlyTiny.SmallFiles = 900

	for _, tc := range []struct {
		name   string
		store  *Store
		now    Workload
		at     time.Time
		expect string
	}{
		{
			name:   "unknown target",
			store:  &Store{Version: FormatVersion},
			now:    anchor,
			at:     now,
			expect: "not in the cache yet",
		},
		{
			name:   "a different policy generation",
			store:  stored(anchor, measuredLink(), func(r *Record) { r.PolicyVersion = autotune.PolicyVersion + 1 }),
			now:    anchor,
			at:     now,
			expect: "auto-policy version",
		},
		{
			name:   "older than the trust window",
			store:  stored(anchor, measuredLink()),
			now:    anchor,
			at:     now.Add(MaxAge + time.Hour),
			expect: "day(s) old",
		},
		{
			name:   "leaned on too often",
			store:  stored(anchor, measuredLink(), func(r *Record) { r.Reused = MaxReuse }),
			now:    anchor,
			at:     now,
			expect: "has been reused",
		},
		{
			name:   "the deploy more than doubled",
			store:  stored(anchor, measuredLink()),
			now:    doubled,
			at:     now,
			expect: "file count moved",
		},
		{
			name:   "the files are now too small to fill a stream",
			store:  stored(anchor, measuredLink()),
			now:    tiny,
			at:     now,
			expect: "too small to fill a stream",
		},
		{
			name:   "the same files, mostly tiny ones now",
			store:  stored(anchor, measuredLink()),
			now:    mostlyTiny,
			at:     now,
			expect: "share of very small files",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cand, dec := tc.store.Lookup(target, tc.now, tc.at)
			if dec.Reason == "" {
				t.Fatalf("the record was accepted, want a miss mentioning %q", tc.expect)
			}
			if !strings.Contains(dec.Reason, tc.expect) {
				t.Errorf("miss reason %q does not mention %q", dec.Reason, tc.expect)
			}
			if dec := cand.Validate(13 * time.Millisecond); dec.Hit {
				t.Errorf("a rejected candidate validated anyway: %+v", dec)
			}
		})
	}
}

// TestValidateChecksTheLinkAgainstTodaysMeasurement is the cheap validation
// issue #212 asks for. It is the strongest gate here, because unlike every
// other one it compares the record against a measurement taken now.
func TestValidateChecksTheLinkAgainstTodaysMeasurement(t *testing.T) {
	s := stored(mediumTree(), measuredLink())

	for _, tc := range []struct {
		name string
		rtt  time.Duration
		hit  bool
	}{
		{"the same link", 13 * time.Millisecond, true},
		{"a little slower", 20 * time.Millisecond, true},
		{"four times as far away", 52 * time.Millisecond, false},
		{"a different, much closer path", 1 * time.Millisecond, false},
		{"unmeasurable", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cand, dec := s.Lookup(target, mediumTree(), now)
			if dec.Reason != "" {
				t.Fatalf("unexpected pre-connection miss: %s", dec.Reason)
			}
			if got := cand.Validate(tc.rtt); got.Hit != tc.hit {
				t.Errorf("hit = %v, want %v (%s)", got.Hit, tc.hit, got.Reason)
			}
		})
	}
}

// TestHitWithNothingToRestore: a record written by a run that could measure
// nothing is still a valid record (it carries the round-trip time the next run
// checks against), but it must not be announced as if it changed a decision.
func TestHitWithNothingToRestore(t *testing.T) {
	s := stored(mediumTree(), Link{RTTMillis: 13, HandshakeMillis: 380})

	cand, dec := s.Lookup(target, mediumTree(), now)
	if dec.Reason != "" {
		t.Fatalf("unexpected miss: %s", dec.Reason)
	}
	dec = cand.Validate(13 * time.Millisecond)
	if !dec.Hit {
		t.Fatalf("the record was rejected: %s", dec.Reason)
	}
	if dec.Restores() {
		t.Errorf("a record with no throughput and no ceiling claims to restore something: %+v", dec)
	}
}

// TestUpdateReanchorsOnlyOnAMeasurement is the anti-drift rule, stated
// directly: a run that used a stored measurement does not get to say what that
// measurement describes.
func TestUpdateReanchorsOnlyOnAMeasurement(t *testing.T) {
	anchor := mediumTree()
	s := stored(anchor, measuredLink())
	later := now.Add(24 * time.Hour)

	grown := anchor
	grown.Files = 1400
	grown.Bytes = int64(1400) * 256 * 1024

	// A run that reused the record and measured nothing of its own.
	s.Update(Observation{
		Target:   target,
		Workload: grown,
		Link:     Link{RTTMillis: 15, HandshakeMillis: 390},
		Settings: Settings{Connections: 6, Concurrency: 64, RequestConcurrency: 32},
		Reused:   true,
	}, later)

	rec, _ := s.find(target)
	switch {
	case rec.Workload != anchor:
		t.Errorf("the anchor followed the run that only used it: %+v", rec.Workload)
	case rec.Link != measuredLink():
		t.Errorf("the stored link was overwritten by a run that measured nothing: %+v", rec.Link)
	case !rec.MeasuredAt.Equal(now):
		t.Errorf("measured_at moved to %v without a measurement", rec.MeasuredAt)
	case rec.Reused != 1:
		t.Errorf("reuse count is %d, want 1", rec.Reused)
	case rec.Settings.Connections != 6:
		t.Errorf("the recorded settings did not follow the run that ran them: %+v", rec.Settings)
	}

	// A run that measured the link replaces all of it, anchor included.
	s.Update(Observation{
		Target:   target,
		Workload: grown,
		Link:     Link{RTTMillis: 15, HandshakeMillis: 390, StreamBytesPerSecond: 3 << 20},
		Measured: true,
		Reused:   true,
	}, later)

	rec, _ = s.find(target)
	switch {
	case rec.Workload != grown:
		t.Errorf("a measurement did not re-anchor the record: %+v", rec.Workload)
	case rec.Link.StreamBytesPerSecond != 3<<20:
		t.Errorf("the new measurement was not stored: %+v", rec.Link)
	case !rec.MeasuredAt.Equal(later):
		t.Errorf("measured_at is %v, want the time of the measurement", rec.MeasuredAt)
	case rec.Reused != 0:
		t.Errorf("reuse count is %d, want it reset by the new measurement", rec.Reused)
	}
}

// TestUpdateWritesARecordWithNoMeasurementToProtect: a record that never held
// a throughput has no anchor worth freezing, so a later run may replace it
// outright. This is what keeps a first run's round-trip time from ageing out
// while the deploys keep coming.
func TestUpdateWritesARecordWithNoMeasurementToProtect(t *testing.T) {
	s := stored(mediumTree(), Link{RTTMillis: 13, HandshakeMillis: 380})
	later := now.Add(48 * time.Hour)

	grown := mediumTree()
	grown.Files = 1400

	s.Update(Observation{
		Target:   target,
		Workload: grown,
		Link:     Link{RTTMillis: 14, HandshakeMillis: 400},
		Reused:   true,
	}, later)

	rec, _ := s.find(target)
	if rec.Workload != grown || !rec.MeasuredAt.Equal(later) {
		t.Errorf("a record with nothing measured in it was preserved anyway: %+v", rec)
	}
}

// TestUpdateKeepsAMeasurementItCouldNotUse: a run whose workload walked out of
// the band has nothing better to offer, so the stored measurement stays. A
// cache that threw evidence away every time it did not apply would be
// permanently empty for anyone with two different deploys.
func TestUpdateKeepsAMeasurementItCouldNotUse(t *testing.T) {
	anchor := mediumTree()
	s := stored(anchor, measuredLink())

	other := Workload{Files: 20, Bytes: 20 * 4096, P50: 4096, P90: 4096, Largest: 4096, SmallFiles: 20}
	s.Update(Observation{Target: target, Workload: other, Link: Link{RTTMillis: 13}}, now.Add(time.Hour))

	rec, _ := s.find(target)
	if rec.Workload != anchor || rec.Link.StreamBytesPerSecond != 8<<20 {
		t.Errorf("a run that could not use the record threw it away: %+v", rec)
	}
	if rec.Reused != 0 {
		t.Errorf("a run that did not use the record counted as a reuse (%d)", rec.Reused)
	}
}

// TestUpdateSkipsAWriteWhenNothingChanged keeps a cache file from being
// rewritten (and, in CI, re-uploaded) by runs that learned nothing new.
func TestUpdateSkipsAWriteWhenNothingChanged(t *testing.T) {
	s := stored(mediumTree(), measuredLink())
	obs := Observation{
		Target:   target,
		Workload: mediumTree(),
		Link:     measuredLink(),
		Settings: Settings{Connections: 4, Concurrency: 64, RequestConcurrency: 32},
	}
	if s.Update(obs, now.Add(time.Hour)) {
		t.Error("a run that repeated what the record already said asked for a write")
	}
}

// TestCeilingIsCarriedButNotForever. A ceiling caps the pool, so a run that
// inherits one never tests it; carried without a bound that is a cache that
// has turned into configuration, which issue #212 explicitly rules out.
func TestCeilingIsCarriedButNotForever(t *testing.T) {
	s := stored(mediumTree(), measuredLink(), func(r *Record) { r.ConnectionCeiling = 2 })

	for i := 1; i <= MaxCeilingCarry; i++ {
		s.Update(Observation{
			Target:          target,
			Workload:        mediumTree(),
			Link:            measuredLink(),
			Reused:          true,
			CeilingUntested: true,
		}, now.Add(time.Duration(i)*time.Hour))
		rec, _ := s.find(target)
		if rec.ConnectionCeiling != 2 {
			t.Fatalf("run %d: the untested ceiling was dropped early", i)
		}
		if rec.CeilingCarried != i {
			t.Fatalf("run %d: carried count is %d", i, rec.CeilingCarried)
		}
	}

	s.Update(Observation{
		Target:          target,
		Workload:        mediumTree(),
		Link:            measuredLink(),
		Reused:          true,
		CeilingUntested: true,
	}, now.Add(time.Duration(MaxCeilingCarry+1)*time.Hour))
	if rec, _ := s.find(target); rec.ConnectionCeiling != 0 {
		t.Errorf("the ceiling survived %d runs without ever being tested", MaxCeilingCarry+1)
	}
}

// TestCeilingFollowsWhatTheServerJustDid: a run that met pushback replaces the
// stored bound and resets the carry count, and a run that asked for everything
// and met none drops it.
func TestCeilingFollowsWhatTheServerJustDid(t *testing.T) {
	s := stored(mediumTree(), measuredLink(), func(r *Record) {
		r.ConnectionCeiling = 4
		r.CeilingCarried = 3
	})

	s.Update(Observation{
		Target: target, Workload: mediumTree(), Link: measuredLink(),
		Reused: true, ConnectionCeiling: 2,
	}, now.Add(time.Hour))
	rec, _ := s.find(target)
	if rec.ConnectionCeiling != 2 || rec.CeilingCarried != 0 {
		t.Errorf("a fresh refusal did not replace the stored ceiling: %+v", rec)
	}

	s.Update(Observation{
		Target: target, Workload: mediumTree(), Link: measuredLink(), Reused: true,
	}, now.Add(2*time.Hour))
	if rec, _ := s.find(target); rec.ConnectionCeiling != 0 {
		t.Errorf("a run that met no pushback kept a ceiling of %d", rec.ConnectionCeiling)
	}
}

// TestStoreKeepsSeveralTargets: one cache file has to serve a repository that
// deploys to staging and production, and must not grow without bound.
func TestStoreKeepsSeveralTargets(t *testing.T) {
	s := &Store{Version: FormatVersion}
	for i := 0; i < MaxRecords+3; i++ {
		s.Update(Observation{
			Target:   Fingerprint(string(rune('a' + i))),
			Workload: mediumTree(),
			Link:     measuredLink(),
			Measured: true,
		}, now.Add(time.Duration(i)*time.Minute))
	}
	if len(s.Records) != MaxRecords {
		t.Fatalf("stored %d record(s), want the file capped at %d", len(s.Records), MaxRecords)
	}
	if s.Records[0].Target != Fingerprint(string(rune('a'+MaxRecords+2))) {
		t.Errorf("the most recent record is not first: %s", s.Records[0].Target)
	}
	if _, ok := s.find(Fingerprint("a")); ok {
		t.Error("the oldest record survived the cap")
	}
}

// TestFingerprintNeverNamesTheHost. The file can end up in a CI cache that
// outlives the job and is restored into other workflows.
func TestFingerprintNeverNamesTheHost(t *testing.T) {
	fp := Fingerprint("sftp.example.com:22:deploy")
	switch {
	case strings.Contains(fp, "example.com") || strings.Contains(fp, "deploy"):
		t.Fatalf("the fingerprint carries the identity: %s", fp)
	case fp == Fingerprint("sftp.example.com:2222:deploy"):
		t.Error("a different port fingerprints the same")
	case fp == Fingerprint("sftp.example.com:22:other"):
		t.Error("a different user fingerprints the same")
	}
}

// TestWorkloadOfReadsThePlan pins the one conversion between the policy's
// workload and the stored one, which is what a similarity judgement is made
// on.
func TestWorkloadOfReadsThePlan(t *testing.T) {
	w := autotune.SummarizeUploads([]int64{4096, 4096, 1 << 20, 8 << 20})
	got := WorkloadOf(w)
	want := Workload{Files: 4, Bytes: 4096 + 4096 + (1 << 20) + (8 << 20), P50: 4096, P90: 8 << 20, Largest: 8 << 20, SmallFiles: 2}
	if got != want {
		t.Errorf("WorkloadOf = %+v, want %+v", got, want)
	}
}
