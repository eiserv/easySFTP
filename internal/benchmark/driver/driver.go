// Package driver measures a benchmark run: it generates the payloads, runs the
// builds, shapes and probes the link, and appends what each run reported to the
// JSONL the aggregation reads back.
//
// It is the Go form of the benchmark.sh and benchmark-matrix.sh shell scripts
// issue #190 replaced (steps 3 and 4; step 6 removed them). What moved is the
// orchestration, not the measurement: the same runs happen in the same order at
// the same settings, and the manifest and the JSONL are the same documents the
// scripts wrote. A rewrite that also changed what is measured could not be
// reviewed, so before the scripts went, both implementations were run against
// the same stubbed inputs and their JSONL and manifests compared (step 5).
//
// Nothing here writes into Options.OutDir except the aggregated result: run
// logs and the generated config file name the host and the user, and OutDir is
// uploaded as a workflow artifact, where nothing is masked.
package driver

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/eiserv/easySFTP/internal/benchmark"
	"github.com/eiserv/easySFTP/internal/benchmark/link"
	"github.com/eiserv/easySFTP/internal/benchmark/runner"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
)

// Options is everything a run needs, already validated. The command that builds
// it reads the environment; nothing below this line does.
type Options struct {
	CandidateBin string
	CandidateRef string
	BaselineBin  string
	BaselineRef  string

	// Connections turns on the standard benchmark's third build label
	// ("poolN"), the same binary with advanced.connections set. Zero is off.
	Connections int

	Repeats    int
	RemoteBase string
	OutDir     string
	DatasetDir string
	LogDir     string

	Server runner.Server

	// LinkProfilesRaw is the requested profile string, verbatim, because the
	// summary prints it. LinkProfiles is the parsed form.
	LinkProfilesRaw string
	LinkProfiles    []string
	LinkProbeBin    string
	LinkIface       string
	LinkSudo        string

	// The matrix grid, as it was asked for. Each scenario's own axes are
	// derived from these against its payload.
	Scenarios          []string
	ConnectionsAxis    []int
	ConcurrencyAxis    []int
	RequestAxis        []*int
	ConnectionsDisplay string
	ConcurrencyDisplay string
	RequestDisplay     string

	// RunnerEnvironment and RunnerName are the two GitHub Actions variables
	// that describe the machine.
	RunnerEnvironment string
	RunnerName        string
}

// run is the state one measuring pass carries around.
type run struct {
	opts   Options
	runner *runner.Runner
	shaper *link.Shaper
	prober *link.Prober

	// profile is the link profile every measured run below is on. Set by the
	// profile loop; a run always carries it, so "baseline" in a stored result
	// means "the real line" and not "unknown".
	profile string

	labels    []string
	binaries  []string
	refs      []string
	advanced  []string
	reference string

	files map[string]*jsonl
}

func logf(format string, args ...any)  { fmt.Printf(format+"\n", args...) }
func warnf(format string, args ...any) { fmt.Printf("::warning::"+format+"\n", args...) }

func newRun(opts Options) (*run, error) {
	if opts.Repeats < 1 {
		return nil, fmt.Errorf("a run needs at least one repeat, got %d", opts.Repeats)
	}
	if err := checkRemoteBase(opts.RemoteBase); err != nil {
		return nil, err
	}
	for _, dir := range []string{opts.OutDir, opts.DatasetDir, opts.LogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if err := checkLogDir(opts.OutDir, opts.LogDir); err != nil {
		return nil, err
	}

	r := &run{
		opts:    opts,
		runner:  &runner.Runner{Server: opts.Server, LogDir: opts.LogDir},
		shaper:  link.NewShaper(opts.LinkIface, opts.LinkSudo, opts.Server.Host, logf, warnf),
		profile: "baseline",
		files:   map[string]*jsonl{},
	}
	r.shaper.Requested = opts.LinkProfiles
	r.prober = &link.Prober{
		Binary:     opts.LinkProbeBin,
		Host:       opts.Server.Host,
		Port:       opts.Server.Port,
		Username:   opts.Server.Username,
		Password:   opts.Server.Password,
		KnownHosts: opts.Server.KnownHosts,
		RemotePath: opts.RemoteBase + "/linkprobe",
		Logf:       logf,
		Warnf:      warnf,
	}
	return r, nil
}

// finish hands the measurement over to the aggregation half and prints the
// summary it wrote.
//
// The manifest is written out as well as passed on: it is the seam the two
// halves meet at, and having it on disk is what let the parity check of issue
// #190 compare two aggregations of one measurement rather than two
// measurements. Keep writing it. A measurement whose inputs are gone the moment
// the aggregation ends cannot be re-aggregated, by a later version or by a
// maintainer holding a failed run's artifact.
func (r *run) finish(m *benchmark.Manifest) error {
	// The aggregation reads these back off disk, so everything measured has to
	// be flushed before it does.
	r.closeFiles()

	manifest := filepath.Join(r.opts.LogDir, "manifest.json")
	if err := benchmark.WriteJSON(manifest, m); err != nil {
		return err
	}
	inputs, err := benchmark.Load(m)
	if err != nil {
		return err
	}
	summary, err := benchmark.Write(m, inputs, r.opts.OutDir)
	if err != nil {
		return err
	}
	cat(filepath.Join(r.opts.OutDir, summary))
	return nil
}

// checkRemoteBase refuses the obvious mistakes. The benchmark wipes everything
// below "<base>/<build>/<scenario>" before every repeat, so a too-broad path is
// a data-loss bug, not a slow benchmark. easySFTP itself refuses "/"; this
// refuses the rest.
func checkRemoteBase(base string) error {
	switch {
	case base == "/", base == ".", base == "..", strings.HasSuffix(base, "/"):
		return fmt.Errorf(
			"REMOTE_BASE ('%s') must be a dedicated directory such as /easysftp-benchmark, without a trailing slash", base)
	}
	return nil
}

// checkLogDir keeps the run logs out of the artifact. They echo the host name
// and the user name, both secrets here, and OutDir is uploaded unmasked.
func checkLogDir(outDir, logDir string) error {
	out, err := filepath.Abs(outDir)
	if err != nil {
		return err
	}
	log, err := filepath.Abs(logDir)
	if err != nil {
		return err
	}
	if log == out || strings.HasPrefix(log, out+string(filepath.Separator)) {
		return fmt.Errorf(
			"LOG_DIR must not be inside OUT_DIR: run logs name the host and user, and OUT_DIR is uploaded as an artifact")
	}
	return nil
}

// jsonl is one of the append-only files a run measures into. They are truncated
// up front, so a re-run in the same log directory measures itself and not its
// predecessor.
type jsonl struct {
	path string
	file *os.File
}

func (r *run) open(name, filename string) (string, error) {
	path := filepath.Join(r.opts.LogDir, filename)
	file, err := os.Create(path)
	if err != nil {
		return "", err
	}
	r.files[name] = &jsonl{path: path, file: file}
	return path, nil
}

// append writes one document as a line. A record that cannot be encoded is a
// bug in this package rather than a measurement problem, so it stops the run.
func (r *run) append(name string, record any) error {
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f := r.files[name]
	if _, err := f.file.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

func (r *run) closeFiles() {
	for _, f := range r.files {
		f.file.Close()
	}
}

func (r *run) path(name string) string {
	if f, ok := r.files[name]; ok {
		return f.path
	}
	return ""
}

// stem is the log file prefix of one measured run, with the profile as a
// filename component: profiles contain "+" and "/", and a log called
// "+50ms/5mbit-small-1.log" is a missing directory, not a log.
func (r *run) stem(parts ...string) string {
	return filepath.Join(r.opts.LogDir, strings.Join(parts, "-"))
}

// builds fills in the build labels every measured run loops over. The
// connection pool is measured as a third build of the *same* binary, not as a
// separate workflow run: the two numbers are only comparable when they are
// interleaved on the same host in the same minutes.
func (r *run) setBuilds(withPool bool) {
	r.labels = []string{"candidate"}
	r.binaries = []string{r.opts.CandidateBin}
	r.refs = []string{r.opts.CandidateRef}
	r.advanced = []string{""}

	if r.opts.BaselineBin != "" {
		ref := r.opts.BaselineRef
		if ref == "" {
			ref = "unknown"
		}
		r.labels = append(r.labels, "baseline")
		r.binaries = append(r.binaries, r.opts.BaselineBin)
		r.refs = append(r.refs, ref)
		r.advanced = append(r.advanced, "")
	}
	if withPool && r.opts.Connections > 0 {
		r.labels = append(r.labels, fmt.Sprintf("pool%d", r.opts.Connections))
		r.binaries = append(r.binaries, r.opts.CandidateBin)
		r.refs = append(r.refs, r.opts.CandidateRef)
		r.advanced = append(r.advanced, fmt.Sprintf("connections: %d", r.opts.Connections))
	}

	// The reference build every delta is measured against: the baseline when
	// one was measured, otherwise the candidate, so a pool run without a
	// baseline is still compared against something.
	r.reference = "candidate"
	if r.opts.BaselineBin != "" {
		r.reference = "baseline"
	}
}

// preClean is the clean deployment that runs before every measured run. It
// wipes the populated tree the last run left behind, so repeat 1 (uploading
// into nothing) measures the same thing as the repeats after it.
//
// It is instrumented where its numbers are wanted, into its own metrics file
// and its own aggregate (issue #184, phase 4): a clean deployment of an empty
// directory is a pure delete sweep, and it is the only place deletions are
// measured at all. Nothing of it ever reaches the upload numbers.
func (r *run) preClean(binary, remote, stem, advanced string, instrument bool) (int, error) {
	metrics := ""
	if instrument {
		metrics = stem + ".clean.metrics.json"
	}
	return r.cleanAt(binary, remote, stem+".clean", advanced, metrics)
}

// cleanAt deploys an empty directory over a remote path, writing its log and
// step outputs next to the given stem.
func (r *run) cleanAt(binary, remote, stem, advanced, metrics string) (int, error) {
	return r.runner.Do(runner.Run{
		Binary:   binary,
		Source:   filepath.Join(r.opts.DatasetDir, "empty"),
		Remote:   remote,
		Mode:     "clean",
		Log:      stem + ".log",
		Outputs:  stem + ".out",
		Metrics:  metrics,
		Advanced: advanced,
	})
}

// cat copies a failed run's log into the job log, which masks secrets. Never
// into the artifact.
func cat(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	os.Stdout.Write(data)
}

// deleteRecord is one pre-clean read back off disk. The coordinates are added
// by the caller, which is what lets a standard sweep and a matrix sweep share
// this shape (issue #184, phase 4).
type deleteRecord struct {
	ExitCode     int             `json:"exit_code"`
	DurationMS   float64         `json:"duration_ms"`
	FilesDeleted float64         `json:"files_deleted"`
	Metrics      json.RawMessage `json:"metrics"`

	Label       string `json:"label"`
	Scenario    string `json:"scenario"`
	LinkProfile string `json:"link_profile"`

	Connections        *int `json:"connections,omitempty"`
	Concurrency        *int `json:"concurrency,omitempty"`
	RequestConcurrency *int `json:"request_concurrency,omitempty"`

	Repeat int `json:"repeat"`
}

func readDelete(stem string, exitCode int) deleteRecord {
	return deleteRecord{
		ExitCode:     exitCode,
		DurationMS:   runner.StepNumber(stem+".clean.out", "duration-ms"),
		FilesDeleted: runner.StepNumber(stem+".clean.out", "files-deleted"),
		Metrics:      runner.ReadMetrics(stem + ".clean.metrics.json"),
	}
}

// runRecord is one measured run, exactly as the scripts appended it.
type runRecord struct {
	Label       string `json:"label"`
	Ref         string `json:"ref"`
	Scenario    string `json:"scenario"`
	LinkProfile string `json:"link_profile"`

	Connections        *int `json:"connections,omitempty"`
	Concurrency        *int `json:"concurrency,omitempty"`
	RequestConcurrency *int `json:"request_concurrency,omitempty"`

	Repeat     int             `json:"repeat"`
	ExitCode   int             `json:"exit_code"`
	DurationMS float64         `json:"duration_ms"`
	Files      float64         `json:"files"`
	Bytes      float64         `json:"bytes"`
	Retries    *float64        `json:"retries,omitempty"`
	Errors     *float64        `json:"errors,omitempty"`
	Refused    *float64        `json:"refused,omitempty"`
	Metrics    json.RawMessage `json:"metrics"`
}

// environment is the machine and toolchain a result was measured on, and the
// comparability key between two results: benchmarks/README.md says two numbers
// may only be compared when this matches.
//
// The three uname fields and the CPU count come from the same commands the
// shell used, so a runner that reports "6.8.0-51-generic" keeps reporting it.
// Where a command is missing, Go's own view of the machine stands in rather
// than the field being invented.
func (r *run) environment() *schema.Environment {
	env := &schema.Environment{
		Runner:     r.opts.RunnerEnvironment,
		OS:         firstLine(exec.Command("uname", "-s"), capitalized(runtime.GOOS)),
		Kernel:     firstLine(exec.Command("uname", "-r"), ""),
		Arch:       firstLine(exec.Command("uname", "-m"), runtime.GOARCH),
		CPUModel:   cpuModel(),
		CPUs:       cpuCount(),
		GoVersion:  goVersion(),
		RunnerName: r.opts.RunnerName,
	}
	return env
}

// runnerLine is the "<environment>, <kernel>, <n> cpu" string a result stores,
// and runnerDisplay the shorter one the summary table prints. The two differ,
// and reconstructing either from the other would be guesswork, so both travel
// through the manifest.
func (r *run) runnerLine() (string, string) {
	display := fmt.Sprintf("%s %s, %d cpu",
		firstLine(exec.Command("uname", "-s"), capitalized(runtime.GOOS)),
		firstLine(exec.Command("uname", "-r"), ""),
		cpuCount())
	return r.opts.RunnerEnvironment + ", " + display, display
}

func firstLine(cmd *exec.Cmd, fallback string) string {
	out, err := cmd.Output()
	if err != nil {
		return fallback
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if line == "" {
		return fallback
	}
	return line
}

func capitalized(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// cpuModel is the first "model name" of /proc/cpuinfo, which is where the shell
// read it. Machines without that file report "unknown", as they did before.
func cpuModel() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, found := strings.Cut(line, ": "); found && strings.HasPrefix(name, "model name") {
			return value
		}
	}
	return "unknown"
}

// cpuCount prefers nproc, which reports the CPUs this process may actually use.
// runtime.NumCPU reports the same thing, and stands in where nproc is missing.
func cpuCount() int {
	if text := firstLine(exec.Command("nproc"), ""); text != "" {
		if n, err := strconv.Atoi(text); err == nil {
			return n
		}
	}
	return runtime.NumCPU()
}

// goVersion is the toolchain on the runner, not the toolchain this binary was
// built with: the benchmark builds easySFTP from source on the same machine.
func goVersion() string {
	fields := strings.Fields(firstLine(exec.Command("go", "version"), ""))
	if len(fields) < 3 {
		return ""
	}
	return fields[2]
}

// linkManifest is the shaping half of the link object.
//
// The probes are deliberately not in here: they are read from the probe file by
// the aggregation, so a run without a probe binary simply has none. This
// carries only what the measuring half knows and the aggregation cannot derive.
func (r *run) linkManifest() benchmark.ManifestLink {
	var iface *string
	if r.shaper.Iface != "" {
		value := r.shaper.Iface
		iface = &value
	}
	var reason *string
	if r.shaper.Reason != "" {
		value := r.shaper.Reason
		reason = &value
	}
	return benchmark.ManifestLink{
		Iface: iface,
		Shaping: schema.Shaping{
			Available: r.shaper.Available,
			Reason:    reason,
			Requested: orEmpty(r.shaper.Requested),
			Applied:   orEmpty(r.shaper.Applied),
		},
		Profiles:  orEmpty(r.opts.LinkProfiles),
		Requested: r.opts.LinkProfilesRaw,
	}
}

// orEmpty keeps a nil slice out of the stored JSON: "[]" is a run that shaped
// nothing, "null" is a document nobody wrote on purpose.
func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// shapeIfNeeded probes for shaping only when a profile actually asks for it: a
// run on the real line must not need tc, sudo or NET_ADMIN.
func (r *run) shapeIfNeeded(what string) {
	if !link.ShapeNeeded(r.opts.LinkProfiles) {
		return
	}
	r.shaper.Probe()
	if !r.shaper.Available {
		warnf("link profiles were requested but shaping is unavailable (%s); every profile is %s on the real line",
			r.shaper.Reason, what)
	}
}

// cleanupLinkProbe removes what a probe killed mid-write left behind. The probe
// removes its own payload, and a leftover would be counted by the next run's
// remote scan.
func (r *run) cleanupLinkProbe() {
	if r.opts.LinkProbeBin == "" {
		return
	}
	if _, err := r.cleanAt(r.opts.CandidateBin, r.opts.RemoteBase+"/linkprobe",
		filepath.Join(r.opts.LogDir, "cleanup-linkprobe"), "", ""); err != nil {
		warnf("cleanup of the link probe directory failed")
	}
}
