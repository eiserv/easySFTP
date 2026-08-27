// Package autocache carries what one run measured about its server over to
// the next one.
//
// internal/autotune is a cost model with exactly one input it cannot compute.
// The files are known before the transfer, the round-trip time and the
// handshake are measured a moment later, and everything else falls out of
// arithmetic. Throughput does not: nothing observes a throughput before bytes
// move. So the policy starts from a deliberately conservative prior and
// corrects it while the transfer runs, which costs the first window of every
// run that gets a correction at all, and which never reaches a deployment
// whose files all start at once (issue #217).
//
// The thing worth remembering between runs is therefore the measurement, not
// the decision. This package stores measurements.
//
// # Why not the settings
//
// Plan is a pure function of the workload, the link and whatever the user
// pinned. It performs no I/O, reads no clock and costs microseconds, so
// storing its output and replaying it would save nothing at all. It would only
// buy the one failure mode a configuration cache really has: a stored answer
// that outlives the question it answered.
//
// That failure mode is not hypothetical, and it does not need a sudden change
// to appear. A dataset that grows by one per cent per deploy is similar to the
// deploy before it every single time, and a hundred deploys later it is
// nothing like the one the settings were chosen for. A cache that compares
// each run against the run before it hits forever and answers with a decision
// nobody would make today.
//
// Two rules rule that out here, and both are load-bearing:
//
//  1. Settings are never restored. What a hit supplies is an *input*; the
//     policy then runs again, on today's files, and decides for itself. A
//     deployment that grew from 1,000 files to 5,000 gets a 5,000-file plan
//     whatever the record says, because the record was never consulted about
//     the answer.
//  2. Every record is anchored to the workload its measurement was taken on,
//     and a run that merely *uses* a record does not rewrite that anchor. Only
//     a run that measures the link afresh does, because only that run has new
//     evidence. Similarity is therefore always judged against the deploy that
//     was actually measured and never against yesterday's deploy, so a slow
//     drift walks out of the band instead of dragging it along. See
//     drift_test.go, which pins exactly that difference.
//
// # What a hit restores
//
// Two things, and both are evidence rather than opinion:
//
//   - The per-stream throughput, handed to the policy as autotune.SourceCached.
//     A runtime measurement of the transfer being tuned still outranks it, so a
//     stale number survives only until the run has something better to say.
//   - A connection ceiling, when the previous run found one: a server that
//     refused an extra connection, or a growth step that measurably did not pay
//     for itself. The cost model assumes perfect scaling (T(k) = W/k + (k-1)*H)
//     and cannot derive either of those; the only way to know is to have been
//     told, and a run that was told should not have to be told again. A ceiling
//     only ever lowers the pool, which is the direction that costs a server
//     nothing.
//
// # What invalidates a record
//
// The format or policy version changed, the target changed, the record is too
// old or has been leaned on too many times without being re-measured, the
// workload no longer looks like the anchor, or the round-trip time measured
// right now disagrees with the one that was measured then. The last of those
// is the cheap validation issue #212 asks for, and it is free: the link probe
// runs anyway, so comparing its answer with the stored one costs nothing and
// is a direct measurement of the same path taken today.
package autocache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/eiserv/easySFTP/internal/autotune"
)

const (
	// FormatVersion is the layout of the file on disk. A file written by a
	// newer easySFTP is ignored rather than guessed at; see Load.
	FormatVersion = 1

	// MaxRecords is how many targets one file keeps. A repository that
	// deploys to staging and production from one cache path should not have
	// the two evict each other, and a file that grows without bound is a file
	// nobody notices going wrong. Records past this are dropped least
	// recently used first.
	MaxRecords = 8

	// MaxAge is how long a measurement is allowed to stand without being
	// taken again. It is a safety stop rather than a measured half-life: a
	// server that was re-provisioned, moved or resized announces none of
	// that, and the only honest assumption about a month-old number is that
	// nobody has checked it.
	MaxAge = 30 * 24 * time.Hour

	// MaxReuse is the same bound on the other clock. Age in days and age in
	// runs are different things: a repository that deploys two hundred times
	// a day burns a month of wall-clock freshness in a few hours, and every
	// one of those runs may be too small to measure anything. Past this the
	// record stops being restored and the run falls back to the prior, which
	// is the state easySFTP shipped in before this cache existed.
	MaxReuse = 32

	// MaxCeilingCarry bounds a connection ceiling that keeps being inherited
	// without ever being tested again.
	//
	// A ceiling caps the pool, so a run that inherits one never asks for more
	// than it and therefore never finds out whether the limit is still there.
	// Left alone that is a cache turning into permanent configuration, which
	// is the one thing issue #212 asks it not to become. So an untested
	// ceiling is carried a bounded number of runs and then dropped: the next
	// run asks for what the policy wants, and either the server says no again
	// (and the ceiling comes straight back) or it does not, and the cap was
	// worth losing.
	MaxCeilingCarry = 16
)

// The similarity bands. They are deliberately generous, because of what a hit
// actually restores: a throughput is a property of the path, not of the files,
// so a workload that is merely a bit larger than the anchor does not make a
// measured bandwidth wrong. What the bands are there to catch is a run that is
// no longer the same *kind* of work, where the stored number was never
// evidence about this link in the first place.
//
// They are ratios rather than absolute tolerances so that they mean the same
// thing at every scale, and the issue's own example (a 980-file deploy reusing
// a 1,000-file result) is comfortably inside all of them.
const (
	// countBand covers the file count and the byte total. A deploy that
	// doubled or halved is a different deploy.
	countBand = 2.0

	// sizeBand covers the median and the ninetieth percentile. The
	// distribution is read for one thing (can a file fill a stream), so it is
	// judged an order of magnitude at a time rather than by the digit.
	sizeBand = 4.0

	// smallShareBand is how far the share of files that fit in a single write
	// packet may move, in absolute terms. A tree that went from a tenth tiny
	// files to nine tenths tiny files is a different tree even when every
	// other number survived.
	smallShareBand = 0.25

	// rttBand is how far the round-trip time measured now may be from the one
	// measured then. A path whose latency doubled is not the path the
	// throughput was measured over. It is the only gate that compares a
	// measurement taken today against the record, which is what makes it the
	// strongest one here.
	rttBand = 2.0
)

// Store is the file on disk: a format version and a handful of records, most
// recently used first.
//
// It is a slice rather than a map keyed by target because the file is a
// document. A Go map re-encodes its keys in sorted order, so a run that
// changed nothing would still rewrite the file in a different shape, and "most
// recently used first" is information a sorted key order would throw away.
type Store struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}

// Record is one target's remembered measurement.
//
// Every field here is either the measurement, the context that says what the
// measurement is evidence about, or provenance. Settings is the exception and
// is written for the reader rather than for the policy: it records what the
// run this measurement came from ended up using, so a stored record explains
// itself. Nothing restores it. See the package comment.
type Record struct {
	// Target is a fingerprint of host, port, user and jump host, never the
	// host itself. The file may end up in a CI cache that outlives the job
	// and is restored into other workflows; a deployment target is not a
	// secret, but it is not this file's to publish either.
	Target string `json:"target"`

	// PolicyVersion is the generation of internal/autotune that produced the
	// measurement. Settings are not restored, but the evidence rules that
	// decide what counts as a measurement are the policy's, so a record from
	// a different generation is not comparable.
	PolicyVersion int `json:"policy_version"`

	// MeasuredAt is when the anchor below was measured, and Reused how many
	// runs have leaned on it since. Neither moves when a run merely uses the
	// record: that is the whole anti-drift rule, and LastUsed is what moves
	// instead.
	MeasuredAt time.Time `json:"measured_at"`
	LastUsed   time.Time `json:"last_used"`
	Reused     int       `json:"reused"`

	// Workload is the anchor: the shape of the run whose transfer produced
	// Link.StreamBytesPerSecond.
	Workload Workload `json:"workload"`

	// Link is what was measured about the path. StreamBytesPerSecond is zero
	// for a record written by a run that had nothing to measure (a redeploy
	// that sent almost nothing, a tree of files too small to fill a stream);
	// such a record still carries the round-trip time and the ceiling, which
	// are worth remembering on their own.
	Link Link `json:"link"`

	// Settings is what that run ran with, for the reader. See the type
	// comment above.
	Settings Settings `json:"settings"`

	// ConnectionCeiling is the largest spread the server was observed to
	// actually give, or to actually benefit from. Zero means nothing was
	// observed and the policy is free up to its own maximum.
	ConnectionCeiling int `json:"connection_ceiling,omitempty"`

	// CeilingCarried counts the runs that inherited the ceiling above without
	// putting it to the test, and is what MaxCeilingCarry bounds.
	CeilingCarried int `json:"ceiling_carried,omitempty"`
}

// Workload is the shape of a deploy, reduced to the features a similarity
// judgement can be made on. It mirrors autotune.Workload's upload side rather
// than embedding it: this is a stored document, and a field added to an
// internal struct must not silently change what is on disk.
type Workload struct {
	Files      int   `json:"files"`
	Bytes      int64 `json:"bytes"`
	P50        int64 `json:"size_p50"`
	P90        int64 `json:"size_p90"`
	Largest    int64 `json:"size_largest"`
	SmallFiles int   `json:"small_files"`
}

// Link is what was measured about the path to the server. Milliseconds and
// bytes per second, because this file is read by people as well as by
// easySFTP.
type Link struct {
	RTTMillis            float64 `json:"rtt_ms"`
	HandshakeMillis      float64 `json:"handshake_ms"`
	StreamBytesPerSecond float64 `json:"stream_bytes_per_second"`
}

// Settings is one resolved transport configuration, recorded for the reader.
type Settings struct {
	Connections        int `json:"connections"`
	Concurrency        int `json:"concurrency"`
	RequestConcurrency int `json:"request_concurrency"`
}

// WorkloadOf reduces a planned workload to the stored features.
func WorkloadOf(w autotune.Workload) Workload {
	return Workload{
		Files:      w.Uploads,
		Bytes:      w.UploadBytes,
		P50:        w.P50Upload,
		P90:        w.P90Upload,
		Largest:    w.LargestUpload,
		SmallFiles: w.SmallUploads,
	}
}

// LinkOf reduces a measured link to the stored features.
func LinkOf(l autotune.Link) Link {
	return Link{
		RTTMillis:            millis(l.RTT),
		HandshakeMillis:      millis(l.Handshake),
		StreamBytesPerSecond: l.StreamBytesPerSecond,
	}
}

// SettingsOf records what a run ran with.
func SettingsOf(s autotune.Settings) Settings {
	return Settings{
		Connections:        s.Connections,
		Concurrency:        s.Concurrency,
		RequestConcurrency: s.RequestConcurrency,
	}
}

func millis(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(d) / float64(time.Millisecond)
}

// Fingerprint hashes a target identity into the value stored as Record.Target.
// Truncated to 64 bits: this is a lookup key among at most MaxRecords entries,
// not a signature.
func Fingerprint(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// Candidate is a record that passed every gate a run can check before it has
// touched the network. It still has to survive Validate, which compares the
// stored round-trip time against the one this run just measured.
type Candidate struct {
	record Record
	found  bool
}

// Decision is the outcome of a lookup: what may be restored, and the sentence
// that explains why. Reason is always set, for a hit as well as for a miss,
// because "the cache did nothing again" is the question a user actually asks.
type Decision struct {
	Hit    bool
	Reason string

	// StreamBytesPerSecond is the remembered per-connection throughput, or
	// zero when the record carries none.
	StreamBytesPerSecond float64

	// ConnectionCeiling is the remembered upper bound on the pool, or zero.
	ConnectionCeiling int

	// Reused is how many runs had already leaned on this record before this
	// one, which is what the run reports and what MaxReuse bounds.
	Reused int
}

// Restores reports whether a hit actually carries anything the policy will
// read. A record written by a run that could measure nothing and met no
// pushback is a valid hit that changes no decision, and saying so is more
// useful than announcing a hit that does nothing.
func (d Decision) Restores() bool {
	return d.Hit && (d.StreamBytesPerSecond > 0 || d.ConnectionCeiling > 0)
}

// Lookup finds the record for target and checks everything that can be checked
// before the first connection: the format, the policy generation, the age, the
// reuse budget and the workload anchor.
//
// The round-trip time cannot be checked here, because measuring it needs the
// connection this decision helps to size. That is what Validate is for, and it
// is why a miss at this stage and a miss at that stage are two separate
// sentences.
func (s *Store) Lookup(target string, now Workload, at time.Time) (Candidate, Decision) {
	if s == nil {
		return Candidate{}, Decision{Reason: "no cache file was read"}
	}
	rec, ok := s.find(target)
	if !ok {
		return Candidate{}, Decision{Reason: "this server is not in the cache yet"}
	}
	if rec.PolicyVersion != autotune.PolicyVersion {
		return Candidate{}, Decision{Reason: fmt.Sprintf(
			"the record was written by auto-policy version %d and this easySFTP runs version %d",
			rec.PolicyVersion, autotune.PolicyVersion)}
	}
	if age := at.Sub(rec.MeasuredAt); age > MaxAge {
		return Candidate{}, Decision{Reason: fmt.Sprintf(
			"the measurement is %d day(s) old and is only trusted for %d",
			int(age.Hours()/24), int(MaxAge.Hours()/24))}
	}
	if rec.Reused >= MaxReuse {
		return Candidate{}, Decision{Reason: fmt.Sprintf(
			"the measurement has been reused %d time(s) without being taken again, which is the limit",
			rec.Reused)}
	}
	if why, ok := similar(rec.Workload, now); !ok {
		return Candidate{}, Decision{Reason: fmt.Sprintf(
			"this deploy no longer looks like the one that was measured (%s)", why)}
	}
	return Candidate{record: rec, found: true}, Decision{}
}

// Validate is the cheap check the run makes once it has a connection: the
// round-trip time it just measured, against the one the record was written
// with. A path whose latency moved materially is not the path the throughput
// came from, whatever the files look like.
//
// A run that did not measure an RTT (the probe is skipped when it cannot
// change an answer, and a server may refuse it) gets no hit. That is
// deliberate: the alternative is to restore a measurement with nothing at all
// standing behind it, and the fallback is the prior easySFTP used before this
// cache existed.
func (c Candidate) Validate(rtt time.Duration) Decision {
	if !c.found {
		return Decision{Reason: "no usable record"}
	}
	stored := c.record.Link.RTTMillis
	if rtt <= 0 || stored <= 0 {
		return Decision{Reason: "the round-trip time could not be measured, so the record could not be checked against this link"}
	}
	now := millis(rtt)
	if !withinBand(now, stored, rttBand) {
		return Decision{Reason: fmt.Sprintf(
			"the link measures %.1f ms now and measured %.1f ms when it was recorded", now, stored)}
	}
	return Decision{
		Hit:                  true,
		Reason:               fmt.Sprintf("%.1f ms now against %.1f ms recorded", now, stored),
		StreamBytesPerSecond: c.record.Link.StreamBytesPerSecond,
		ConnectionCeiling:    c.record.ConnectionCeiling,
		Reused:               c.record.Reused,
	}
}

// Observation is what a finished run has to say about its server.
type Observation struct {
	Target   string
	Workload Workload
	Link     Link
	Settings Settings

	// Measured reports whether Link.StreamBytesPerSecond is this run's own
	// measurement. Only a run that says so may re-anchor a record: that is
	// the rule the whole package rests on.
	Measured bool

	// Reused reports whether this run's plan actually stood on the stored
	// record. A run that was offered one and rejected it did not.
	Reused bool

	// ConnectionCeiling is the bound this run found for itself: the server
	// refused an extra connection, or a growth step was measurably not worth
	// it. Zero means this run found none.
	ConnectionCeiling int

	// CeilingUntested says the run was held at a ceiling it inherited and met
	// no pushback of its own, so it learned nothing about the limit either
	// way. Such a ceiling is carried forward, but only MaxCeilingCarry times.
	CeilingUntested bool
}

// Update folds one finished run into the store and reports whether anything
// changed, so a run that learned nothing does not rewrite the file.
//
// The rule is one sentence: a stored measurement is replaced only by a new
// measurement, and never merely by a newer run.
//
//   - This run measured the link, or there is no record Lookup would still
//     consider: the record is written from this run, anchor and all. New
//     evidence, new anchor, reuse count back to zero.
//   - This run did not measure, and a record Lookup would still consider is
//     standing: the anchor, the whole stored link and MeasuredAt are left
//     exactly as they were. Only the provenance, the recorded settings and the
//     ceiling move. This is the case that would otherwise let a dataset (or a
//     round-trip time) drift a per cent at a time until the record described
//     nothing that was ever measured.
//
// The second bullet used to require the stored record to carry a throughput,
// which quietly exempted every record written by a run that had nothing to
// measure (issue #225). Those records are legitimate: they still carry the
// round-trip time the next run is validated against and the connection
// ceiling, which is why Record documents them. Exempting them meant they
// re-anchored on every run, compared each run against the one before it, and
// so hit forever on a dataset that grows a per cent at a time, which is the
// one failure mode this package is shaped around. Reused was carried in the
// same skipped branch, so MaxReuse could never evict them either, and
// MeasuredAt kept moving, so MaxAge never could. A record has one life,
// whichever of its fields happen to be populated.
//
// What the throughput test was reaching for is in stillTrusted instead, and
// stated as the thing it actually is: an anchor is worth protecting only while
// it is one a run could still be given. A spent record (too old, reused too
// often, written by another policy generation) restores nothing, and only a
// measuring run rewrites an anchor, so freezing a spent one would leave a
// deploy that is too small to ever measure anything with a permanently dead
// cache entry.
//
// The link is kept whole rather than field by field on purpose. The stored
// round-trip time is what the next run validates against, so letting today's
// RTT overwrite it while the throughput stays would move the yardstick and
// leave the measurement: the same slow drift, one axis over.
func (s *Store) Update(obs Observation, at time.Time) bool {
	if s == nil || obs.Target == "" {
		return false
	}
	s.Version = FormatVersion
	old, existed := s.find(obs.Target)

	rec := Record{
		Target:        obs.Target,
		PolicyVersion: autotune.PolicyVersion,
		MeasuredAt:    at,
		LastUsed:      at,
		Workload:      obs.Workload,
		Link:          obs.Link,
		Settings:      obs.Settings,
	}
	if existed && !obs.Measured && old.stillTrusted(at) {
		rec.MeasuredAt = old.MeasuredAt
		rec.Workload = old.Workload
		rec.Link = old.Link
		rec.Reused = old.Reused
		if obs.Reused {
			rec.Reused++
		}
	}

	// The ceiling is the one part of a record that is not about the
	// measurement, so it has its own life: it is current evidence about the
	// server, it is refreshed by every run that meets pushback, and it is let
	// go when nothing has tested it for long enough. See MaxCeilingCarry.
	switch {
	case obs.ConnectionCeiling > 0:
		rec.ConnectionCeiling = obs.ConnectionCeiling
	case existed && obs.CeilingUntested && old.ConnectionCeiling > 0 && old.CeilingCarried < MaxCeilingCarry:
		rec.ConnectionCeiling = old.ConnectionCeiling
		rec.CeilingCarried = old.CeilingCarried + 1
	}

	if existed && rec.equal(old) {
		return false
	}
	s.put(rec)
	return true
}

// stillTrusted reports whether Lookup would still consider this record at time
// at, on the gates that do not depend on the run being planned. It is
// deliberately the same three constants Lookup checks (MaxAge, MaxReuse and
// the policy generation); the workload band is not one of them, because a
// deploy that walked out of the band is exactly the case where the stored
// anchor still has to be protected.
func (r Record) stillTrusted(at time.Time) bool {
	return r.PolicyVersion == autotune.PolicyVersion &&
		at.Sub(r.MeasuredAt) <= MaxAge &&
		r.Reused < MaxReuse
}

// equal reports whether two records say the same thing. LastUsed is ignored:
// rewriting the file to move a timestamp nobody reads is not a change worth
// the write.
func (r Record) equal(other Record) bool {
	a, b := r, other
	a.LastUsed, b.LastUsed = time.Time{}, time.Time{}
	return a == b
}

func (s *Store) find(target string) (Record, bool) {
	for _, r := range s.Records {
		if r.Target == target {
			return r, true
		}
	}
	return Record{}, false
}

// put stores rec at the front (most recently used) and drops the tail past
// MaxRecords.
func (s *Store) put(rec Record) {
	kept := make([]Record, 0, len(s.Records)+1)
	kept = append(kept, rec)
	for _, r := range s.Records {
		if r.Target == rec.Target {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) > MaxRecords {
		kept = kept[:MaxRecords]
	}
	s.Records = kept
}

// similar reports whether now is close enough to the anchor for the anchor's
// measurement to still be evidence about this run, and names the feature that
// disagreed when it is not.
//
// Every feature has to pass. A workload is a shape, and a shape that matches
// on three features out of four is not the same shape; taking a majority would
// mean deciding which feature to be wrong about.
func similar(anchor, now Workload) (string, bool) {
	// The categorical one first, because it is the only feature that changes
	// what the stored number *is* rather than how far off it might be. A byte
	// rate measured on files that each fit in a single write packet is a file
	// rate in other units (see autotune.BandwidthBound); a run on the other
	// side of that line is not describable by it.
	if bandwidthBound(anchor) != bandwidthBound(now) {
		return "one of the two is made of files too small to fill a stream and the other is not", false
	}
	for _, f := range []struct {
		name       string
		then, this float64
		band       float64
	}{
		{"file count", float64(anchor.Files), float64(now.Files), countBand},
		{"total bytes", float64(anchor.Bytes), float64(now.Bytes), countBand},
		{"median file size", float64(anchor.P50), float64(now.P50), sizeBand},
		{"ninetieth percentile file size", float64(anchor.P90), float64(now.P90), sizeBand},
	} {
		if !withinBand(f.this, f.then, f.band) {
			return fmt.Sprintf("%s moved from %s to %s", f.name, number(f.then), number(f.this)), false
		}
	}
	if a, b := smallShare(anchor), smallShare(now); math.Abs(a-b) > smallShareBand {
		return fmt.Sprintf("the share of very small files moved from %.0f%% to %.0f%%", a*100, b*100), false
	}
	return "", true
}

// bandwidthBound is autotune's question asked of a stored shape.
func bandwidthBound(w Workload) bool {
	return autotune.BandwidthBound(autotune.Workload{
		Uploads:       w.Files,
		LargestUpload: w.Largest,
		P90Upload:     w.P90,
	})
}

func smallShare(w Workload) float64 {
	if w.Files <= 0 {
		return 0
	}
	return float64(w.SmallFiles) / float64(w.Files)
}

// withinBand reports whether a and b are within a multiplicative band of each
// other. Two zeroes match; a zero against a non-zero never does, because a
// feature that appeared or vanished is a categorical change and no ratio
// describes it.
func withinBand(a, b, band float64) bool {
	switch {
	case a == b:
		return true
	case a <= 0 || b <= 0:
		return false
	}
	ratio := a / b
	return ratio <= band && ratio >= 1/band
}

// number renders a feature value for the miss message without pretending to a
// precision the comparison does not have.
func number(v float64) string {
	if v >= 1024 {
		return fmt.Sprintf("%.3g", v)
	}
	return fmt.Sprintf("%.0f", v)
}
