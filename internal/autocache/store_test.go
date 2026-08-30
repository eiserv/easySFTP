package autocache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRoundTrip is the file contract: what Save writes, Load reads back
// unchanged, including the fields a hit is decided on.
func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto.json")
	want := stored(mediumTree(), measuredLink(), func(r *Record) {
		r.ConnectionCeiling = 2
		r.CeilingCarried = 1
		r.Reused = 3
	})
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 1 {
		t.Fatalf("read %d record(s), want 1", len(got.Records))
	}
	a, b := want.Records[0], got.Records[0]
	if !a.MeasuredAt.Equal(b.MeasuredAt) || !a.LastUsed.Equal(b.LastUsed) {
		t.Errorf("timestamps did not survive: %v / %v", b.MeasuredAt, b.LastUsed)
	}
	a.MeasuredAt, a.LastUsed = time.Time{}, time.Time{}
	b.MeasuredAt, b.LastUsed = time.Time{}, time.Time{}
	if a != b {
		t.Errorf("record changed on the way through the file:\n got %+v\nwant %+v", b, a)
	}
}

func TestConcurrentUpdateFileMergesEveryWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto.json")
	const writers = 6
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := UpdateFile(path, Observation{
				Target:   string(rune('a' + i)),
				Workload: mediumTree(),
				Link:     measuredLink(),
				Measured: true,
			}, time.Date(2026, 8, 30, i, 0, 0, 0, time.UTC))
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Records) != writers {
		t.Fatalf("concurrent writers left %d records, want %d: %+v", len(store.Records), writers, store.Records)
	}
	seen := map[string]bool{}
	for _, record := range store.Records {
		seen[record.Target] = true
	}
	for i := 0; i < writers; i++ {
		target := string(rune('a' + i))
		if !seen[target] {
			t.Errorf("record %q was lost", target)
		}
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock sidecar survived the writers: %v", err)
	}
}

// TestSaveCreatesTheDirectory: a cache path under a directory a CI cache has
// not restored yet is a first run, not a failure.
func TestSaveCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "auto.json")
	if err := Save(path, stored(mediumTree(), measuredLink())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// TestSaveLeavesNoDebris: the write goes through a temporary sibling, and the
// sibling must not survive it. A stray file next to the cache would be
// restored along with it by every CI cache hit from then on.
func TestSaveLeavesNoDebris(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auto.json")
	for i := 0; i < 3; i++ {
		if err := Save(path, stored(mediumTree(), measuredLink())); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "auto.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("the cache directory holds %v, want only auto.json", names)
	}
}

// TestLoadDegradesCleanly. Every one of these is a state a deploy has to
// survive: the cache is an optimisation, and an optimisation that can fail a
// deploy is not one (issue #212).
func TestLoadDegradesCleanly(t *testing.T) {
	newer, err := json.Marshal(Store{Version: FormatVersion + 1})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		write   []byte
		empty   bool
		mention string
	}{
		{name: "missing file", empty: true},
		{name: "empty file", write: []byte{}, empty: true},
		{name: "truncated json", write: []byte(`{"version": 1, "records": [`), mention: "parsing"},
		{name: "not json at all", write: []byte("nope"), mention: "parsing"},
		{name: "a newer format", write: newer, mention: "version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auto.json")
			if tc.write != nil {
				if err := os.WriteFile(path, tc.write, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			s, err := Load(path)
			if s == nil {
				t.Fatal("Load returned no store; every caller would have to nil-check")
			}
			if len(s.Records) != 0 || s.Version != FormatVersion {
				t.Errorf("the fallback store is not empty: %+v", s)
			}
			switch {
			case tc.empty:
				if !errors.Is(err, ErrEmpty) {
					t.Errorf("err = %v, want ErrEmpty so the caller stays quiet about a first run", err)
				}
			case err == nil:
				t.Errorf("a %s was accepted silently", tc.name)
			case !strings.Contains(err.Error(), tc.mention):
				t.Errorf("error %q does not mention %q", err, tc.mention)
			}
		})
	}
}

// TestLoadWithoutAPathIsOff: an unconfigured cache reads nothing and reports
// the same "nothing here" as a missing file, so the caller has one path.
func TestLoadWithoutAPathIsOff(t *testing.T) {
	s, err := Load("")
	if !errors.Is(err, ErrEmpty) || s == nil || len(s.Records) != 0 {
		t.Errorf("Load(\"\") = %+v, %v", s, err)
	}
	if err := Save("", stored(mediumTree(), measuredLink())); err != nil {
		t.Errorf("Save(\"\") = %v, want a no-op", err)
	}
}

// TestSavedFileIsReadable keeps the file something a person can open when a
// deploy picked a surprising number. It is a small document, not a blob.
func TestSavedFileIsReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto.json")
	if err := Save(path, stored(mediumTree(), measuredLink())); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\n  \"version\"", "policy_version", "stream_bytes_per_second", "measured_at"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the stored file does not contain %q:\n%s", want, data)
		}
	}
}
