package scenario

import (
	"crypto/rand"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Logf is where dataset generation reports what it is doing. The measuring
// scripts print these lines into the job log, so they stay plain text.
type Logf func(format string, args ...any)

// Generate writes each scenario's payload under datasetDir, plus the empty
// directory used for the pre-clean and cleanup runs.
func Generate(datasetDir string, names []string, logf Logf, warnf Logf) error {
	if err := os.MkdirAll(filepath.Join(datasetDir, "empty"), 0o755); err != nil {
		return err
	}
	for _, name := range names {
		if err := generateOne(datasetDir, name, logf, warnf); err != nil {
			return err
		}
	}
	return nil
}

func generateOne(datasetDir, name string, logf, warnf Logf) error {
	groups, err := Spec(name)
	if err != nil {
		return err
	}
	dir := filepath.Join(datasetDir, name)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	layout := ShapeOf(name).Layout

	// Before writing, not after: the calibration family takes a count and a
	// size from whoever typed it, and "calib-1000x16m" is 16 GiB of local disk
	// plus the hours to upload it. Said early, and not refused, because a
	// deliberate large run is a legitimate thing to ask this for.
	plannedKiB := 0
	for _, g := range groups {
		plannedKiB += g.Count * g.KiB
	}
	logf("dataset %s: generating %d MiB", name, plannedKiB/1024)
	if plannedKiB > 2*1024*1024 {
		warnf("the %s payload is %d GiB; that is local disk on the runner and upload time in every cell that uses it",
			name, plannedKiB/1024/1024)
	}

	index := 0
	for _, g := range groups {
		for i := 0; i < g.Count; i++ {
			sub := filepath.Join(dir, subdirFor(layout, index))
			if err := os.MkdirAll(sub, 0o755); err != nil {
				return err
			}
			path := filepath.Join(sub, fmt.Sprintf("file%d.bin", index))
			if err := writeRandom(path, int64(g.KiB)*1024, os.O_CREATE|os.O_TRUNC|os.O_WRONLY); err != nil {
				return err
			}
			index++
		}
	}

	size, err := treeSize(dir)
	if err != nil {
		return err
	}
	logf("dataset %s: %d file(s), %s", name, index, humanSize(size))
	return nil
}

// subdirFor is where file <index> of a payload goes, relative to the scenario
// directory.
//
// The deep layout puts one directory per 7-bit pattern of the index: 128 leaf
// directories holding a handful of files each, which is the node_modules shape.
// It separates create_dirs and sftp_mkdirall cost from transfer cost, and at
// this RTT that is a large share of a real deploy.
//
// The flat one spreads over 8 subdirectories so remote directory creation is
// part of the measurement, as it is in a real site upload.
func subdirFor(layout string, index int) string {
	if layout != LayoutDeep {
		return fmt.Sprintf("part%d", index%8)
	}
	parts := make([]string, 0, 7)
	rest := index
	for level := 0; level < 7; level++ {
		parts = append(parts, fmt.Sprintf("d%d", rest%2))
		rest /= 2
	}
	return filepath.Join(parts...)
}

func writeRandom(path string, size int64, flags int) error {
	file, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(file, rand.Reader, size); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// Mutate changes the first <count> files of a payload, in sorted order, so a
// prepopulated scenario's measured run has something to deploy. Called between
// the unmeasured deploy and the measured one.
//
// The change appends rather than rewriting in place, because it has to be
// visible to both detectors: the sync manifest compares content hashes, but
// advanced.skip_unchanged compares the remote *size* only (see uploadFiles in
// internal/uploader/transfer.go), and a same-size rewrite would be skipped. The
// files therefore grow by a few hundred bytes per repeat, which is deliberate
// and negligible against the payload.
func Mutate(dir string, count int) error {
	files, err := listFiles(dir)
	if err != nil {
		return err
	}
	sort.Strings(files)
	if count > len(files) {
		count = len(files)
	}
	for _, path := range files[:count] {
		if err := writeRandom(path, 512, os.O_APPEND|os.O_WRONLY); err != nil {
			return err
		}
	}
	return nil
}

func listFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

func treeSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// humanSize is the "du -sh" style figure the generation log used to print.
func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	suffixes := []string{"K", "M", "G", "T"}
	value := float64(bytes) / unit
	i := 0
	for value >= unit && i < len(suffixes)-1 {
		value /= unit
		i++
	}
	if value < 10 {
		return fmt.Sprintf("%.1f%s", value, suffixes[i])
	}
	return fmt.Sprintf("%.0f%s", value, suffixes[i])
}
