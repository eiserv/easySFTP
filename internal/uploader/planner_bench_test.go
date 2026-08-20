package uploader

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// What the local hashing pool costs at a given worker count, which is the
// question issue #155 asks: advanced.concurrency bounds the network upload pool
// and, through hashPlanFiles, the local hashing pool as well, so a user who
// sets "concurrency: 1" because their server dislikes parallel writes also
// hashes their tree one file at a time.
//
// A local benchmark and not an extension of the SFTP matrix, because hashing
// never touches the server: mixing it into a measurement that does would bury
// the effect under network variance. The SFTP side of the same question is the
// "sync" scenario of cmd/easysftp-bench, whose stored runs report a "hash"
// phase next to the rest of a deployment.
//
//	go test ./internal/uploader -run '^$' -bench BenchmarkHashPlanFiles -benchtime 1x
//
// Two things to know before reading a number out of this. The payload is
// written immediately before it is hashed, so on a machine with room to cache
// it this measures CPU and not storage, which is the friendliest case
// parallelism can get; and SHA-256 runs on dedicated instructions on most
// current CPUs, so the per-core throughput is high enough that a small payload
// is dominated by the walk around it rather than by the hashing.
func BenchmarkHashPlanFiles(b *testing.B) {
	payloads := []struct {
		name  string
		count int
		size  int64
	}{
		// The CI case, and the payload of the "sync" benchmark scenario.
		{"500x4KiB", 500, 4 << 10},
		// A built site with assets.
		{"200x1MiB", 200, 1 << 20},
		// Large enough for the hashing itself to be the whole cost, which is
		// the shape issue #155 is about (a media or artifact deploy).
		{"32x32MiB", 32, 32 << 20},
	}

	for _, payload := range payloads {
		dir := b.TempDir()
		files := writeHashPayload(b, dir, payload.count, payload.size)
		total := payload.size * int64(payload.count)

		// GOMAXPROCS is the value issue #155 suggests hashing should use
		// instead of the transfer setting; it is measured alongside the fixed
		// counts rather than instead of them.
		workers := []int{1, 2, 4, 8}
		if procs := runtime.GOMAXPROCS(0); procs > 0 {
			workers = append(workers, procs)
		}
		seen := map[int]bool{}
		for _, count := range workers {
			if seen[count] {
				continue
			}
			seen[count] = true
			name := fmt.Sprintf("%s/workers-%d", payload.name, count)
			b.Run(name, func(b *testing.B) {
				b.SetBytes(total)
				b.ReportAllocs()
				for b.Loop() {
					if err := hashPlanFiles(context.Background(), files, count, nil); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// writeHashPayload lays out count files of size bytes and returns them as the
// planner would. The content is pseudo-random from a fixed seed: identical on
// every machine and every run, and not compressible by a filesystem that might
// otherwise make one payload cheaper to read than another.
func writeHashPayload(b *testing.B, dir string, count int, size int64) []fileItem {
	b.Helper()

	block := make([]byte, 1<<20)
	source := rand.New(rand.NewSource(1))
	for i := range block {
		block[i] = byte(source.Intn(256))
	}

	files := make([]fileItem, count)
	for i := range files {
		path := filepath.Join(dir, fmt.Sprintf("file-%04d.bin", i))
		f, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		for written := int64(0); written < size; {
			chunk := int64(len(block))
			if remaining := size - written; remaining < chunk {
				chunk = remaining
			}
			n, err := f.Write(block[:chunk])
			if err != nil {
				f.Close()
				b.Fatal(err)
			}
			written += int64(n)
		}
		if err := f.Close(); err != nil {
			b.Fatal(err)
		}
		files[i] = fileItem{
			localPath: path,
			rel:       fmt.Sprintf("file-%04d.bin", i),
			size:      size,
		}
	}
	return files
}
