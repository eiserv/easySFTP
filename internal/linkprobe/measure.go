package linkprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// controlFilePrefix names the throughput payload. It starts with a dot and says
// what wrote it, so a leftover after a killed probe is recognisable rather than
// mysterious.
const controlFilePrefix = ".linkprobe-control-"

// measureRTT times n sequential Stat calls on one fixed path. Sequential on
// purpose: two overlapping requests would measure the pipeline's throughput,
// not the path's latency.
func measureRTT(ctx context.Context, c *conn, target string, n int) (*RTT, error) {
	// A warm-up call, deliberately not counted: the first request after the
	// handshake pays for state both sides set up lazily, and folding that into
	// the distribution would put a 100 ms outlier in every p90.
	if _, err := c.sftp.Stat(target); err != nil {
		return nil, fmt.Errorf("stat %q: %w", target, err)
	}

	samples := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			// Partial is fine: what was measured is real. Only an empty sample
			// set is a failure.
			break
		}
		start := time.Now()
		if _, err := c.sftp.Stat(target); err != nil {
			return nil, fmt.Errorf("stat %q after %d sample(s): %w", target, len(samples), err)
		}
		samples = append(samples, time.Since(start))
	}
	if len(samples) == 0 {
		return nil, errors.New("no round-trip completed")
	}
	return summarize(samples), nil
}

// measureControl writes the same total payload twice: once over the connection
// it was given, once spread over cfg.ControlStreams separate connections. The
// ratio of the two is the answer to the question the stored matrix data cannot
// answer, namely whether the ceiling sits on one flow or on the whole line.
func measureControl(ctx context.Context, c *conn, cfg Config) (*Control, error) {
	// The directory itself is Run's job, so both measurements agree on what
	// exists before either of them starts.
	if cfg.RemotePath == "" {
		return nil, errNoRemotePath
	}

	block := payloadBlock()

	// Stream 0 goes through the caller's connection, so the single-stream pass
	// measures exactly the connection whose handshake and RTT are reported
	// alongside it.
	single, err := writeStream(ctx, c, cfg, block, 0, cfg.ControlBytes)
	if err != nil {
		return nil, fmt.Errorf("single stream: %w", err)
	}

	nStream, err := writeStreams(ctx, c, cfg, block)
	if err != nil {
		return nil, fmt.Errorf("%d streams: %w", cfg.ControlStreams, err)
	}

	return &Control{
		Streams:             cfg.ControlStreams,
		Bytes:               cfg.ControlBytes,
		SingleStreamMiBPerS: mibPerSecond(cfg.ControlBytes, single),
		NStreamMiBPerS:      mibPerSecond(cfg.ControlBytes, nStream),
		Note:                "both passes write the same total payload; the multi-stream pass splits it over separate SSH connections",
	}, nil
}

// writeStreams runs cfg.ControlStreams parallel writes over their own
// connections and returns the wall clock of the whole pass. The extra
// connections are dialled before the clock starts: a handshake is not
// throughput, and counting it would make more streams look slower than they
// are.
func writeStreams(ctx context.Context, first *conn, cfg Config, block []byte) (time.Duration, error) {
	streams := cfg.ControlStreams
	if streams < 1 {
		streams = 1
	}
	conns := make([]*conn, streams)
	conns[0] = first
	for i := 1; i < streams; i++ {
		extra, err := dial(ctx, cfg)
		if err != nil {
			// Close what was opened, then report: a server that caps
			// connections is a finding, not a crash.
			for _, c := range conns[1:i] {
				c.Close()
			}
			return 0, fmt.Errorf("opening connection %d: %w", i+1, err)
		}
		conns[i] = extra
	}
	defer func() {
		for _, c := range conns[1:] {
			c.Close()
		}
	}()

	// Split the total so both passes move the same number of bytes. The
	// remainder goes to the first stream rather than being dropped.
	per := cfg.ControlBytes / int64(streams)
	sizes := make([]int64, streams)
	for i := range sizes {
		sizes[i] = per
	}
	sizes[0] += cfg.ControlBytes - per*int64(streams)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	start := time.Now()
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := writeStream(ctx, conns[i], cfg, block, i+1, sizes[i]); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("stream %d: %w", i+1, err))
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	if len(errs) > 0 {
		return 0, errors.Join(errs...)
	}
	return elapsed, nil
}

// writeStream writes n bytes to its own file and removes it again, returning
// the wall clock of the write alone. The file is named per stream index so
// parallel streams never contend for one path.
func writeStream(ctx context.Context, c *conn, cfg Config, block []byte, index int, n int64) (time.Duration, error) {
	name := path.Join(cfg.RemotePath, controlFilePrefix+strconv.Itoa(index)+".bin")
	f, err := c.sftp.Create(name)
	if err != nil {
		return 0, fmt.Errorf("creating %q: %w", name, err)
	}
	// Removed even when the write fails: a half-written control payload left on
	// the server would show up in the next run's remote scan.
	defer func() {
		_ = c.sftp.Remove(name)
	}()

	start := time.Now()
	written, err := f.ReadFrom(&blockReader{block: block, remaining: n})
	elapsed := time.Since(start)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, fmt.Errorf("writing %q: %w", name, err)
	}
	if written != n {
		return 0, fmt.Errorf("writing %q: wrote %d of %d bytes", name, written, n)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return elapsed, nil
}

// payloadBlock is one megabyte of deterministic pseudo-random bytes, repeated
// to fill a stream. Pseudo-random rather than zeros because a compressing
// transport would carry zeros for free; x/crypto/ssh negotiates no compression,
// so this is belt and braces, and deterministic so two probes move identical
// bytes.
func payloadBlock() []byte {
	const size = 1 << 20
	block := make([]byte, size)
	r := rand.New(rand.NewPCG(0x5eed, 0xf1de))
	for i := 0; i+8 <= size; i += 8 {
		binary.LittleEndian.PutUint64(block[i:], r.Uint64())
	}
	return block
}

// blockReader hands out the same block over and over until remaining bytes are
// gone. It exists so a multi-megabyte payload costs one megabyte of memory per
// probe rather than one per stream.
type blockReader struct {
	block     []byte
	remaining int64
	off       int
}

func (b *blockReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n := copy(p, b.block[b.off:])
	b.off += n
	if b.off >= len(b.block) {
		b.off = 0
	}
	b.remaining -= int64(n)
	return n, nil
}

// measureHostLoad reads the server's load average, in the order of least
// privilege: an SFTP read first, an exec channel only if that failed.
//
// Unavailable is the expected outcome on a properly locked down server
// (internal-sftp in a chroot has no /proc and no exec channel), which is why it
// is recorded as a reason rather than as an error.
func measureHostLoad(ctx context.Context, c *conn, cfg Config) *HostLoad {
	if load := loadFromProc(c); load != nil {
		return load
	}
	if load := loadFromExec(ctx, c); load != nil {
		return load
	}
	return &HostLoad{
		Available: false,
		Reason:    "neither /proc/loadavg nor an exec channel is available; expected on an SFTP-only account",
	}
}

func loadFromProc(c *conn) *HostLoad {
	f, err := c.sftp.Open("/proc/loadavg")
	if err != nil {
		return nil
	}
	defer f.Close()
	// /proc/loadavg reports size 0, so io.ReadAll (which pkg/sftp lets stream)
	// is the only way to get its contents; the cap keeps a lying server from
	// filling memory.
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil || len(data) == 0 {
		return nil
	}
	load := parseLoadFields(strings.Fields(string(data)))
	if load == nil {
		return nil
	}
	load.Method = "sftp:/proc/loadavg"
	return load
}

func loadFromExec(ctx context.Context, c *conn) *HostLoad {
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		session, err := c.ssh.NewSession()
		if err != nil {
			done <- result{err: err}
			return
		}
		defer session.Close()
		out, err := session.Output("uptime")
		done <- result{out: out, err: err}
	}()

	// A separate, short deadline: an SFTP-only server may accept the session
	// and then simply never answer, and the whole probe must not wait out its
	// own timeout for a best-effort extra.
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(10 * time.Second):
		return nil
	case res := <-done:
		if res.err != nil {
			return nil
		}
		load := parseUptime(string(res.out))
		if load == nil {
			return nil
		}
		load.Method = "exec:uptime"
		return load
	}
}

// parseUptime reads the load averages out of uptime(1). Linux prints
// "load average: 0.1, 0.2, 0.3", the BSDs "load averages: 0.1 0.2 0.3", so the
// separators are normalised before the fields are split.
func parseUptime(out string) *HostLoad {
	lower := strings.ToLower(out)
	i := strings.Index(lower, "load average")
	if i < 0 {
		return nil
	}
	tail := out[i:]
	if j := strings.Index(tail, ":"); j >= 0 {
		tail = tail[j+1:]
	}
	return parseLoadFields(strings.Fields(strings.ReplaceAll(tail, ",", " ")))
}

// parseLoadFields turns the first three numeric fields into a HostLoad. Fewer
// than three means the format was not what we expected, and a guess is worse
// than an absent measurement.
func parseLoadFields(fields []string) *HostLoad {
	if len(fields) < 3 {
		return nil
	}
	values := make([]float64, 0, 3)
	for _, field := range fields[:3] {
		v, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return nil
		}
		values = append(values, v)
	}
	return &HostLoad{
		Available: true,
		Load1:     &values[0],
		Load5:     &values[1],
		Load15:    &values[2],
	}
}
