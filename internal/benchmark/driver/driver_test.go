package driver_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eiserv/easySFTP/internal/benchmark/driver"
	"github.com/eiserv/easySFTP/internal/benchmark/runner"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
)

// These tests are the harness's own self-check, and since issue #190 step 6 the
// only one: the driver does not care what the "easySFTP binary" it runs is, so
// a stub that writes plausible step outputs and a plausible metrics file is
// enough to check the measurement order, the deploy shapes, the delete sweeps
// and the documents that come out, without an SFTP server, a network or a real
// build.
//
// What they deliberately do not cover is whether easySFTP itself is fast; that
// is what the real benchmark is for.

// stub is the "easySFTP build" these tests measure: this very test binary, run
// again with stubMarker set so it takes the stub path in stub_test.go instead
// of running the suite.
//
// Re-execution rather than a compiled helper, because nothing then has to be
// built while the tests run, and the binary the driver starts is one that
// already exists.
var stub string

func TestMain(m *testing.M) {
	// The probe check comes first and keys off LINKPROBE_HOST, not off a marker
	// of its own: the driver hands its whole environment to everything it
	// starts, so both children see stubMarker and only the probe sees the
	// variables the prober adds.
	if os.Getenv("LINKPROBE_HOST") != "" {
		probeStubMain()
		return
	}
	if os.Getenv(stubMarker) != "" {
		stubMain()
		return
	}
	binary, err := os.Executable()
	if err != nil {
		panic("locating the test binary: " + err.Error())
	}
	stub = binary
	os.Exit(m.Run())
}

// options is the smallest complete configuration: one work directory per test,
// with the log directory outside the output directory, as the driver insists.
func options(t *testing.T) driver.Options {
	t.Helper()
	work := t.TempDir()
	// The driver passes its own environment on to every build it starts, which
	// is how the stub learns both that it is the stub and where to keep its
	// stand-in for the remote server.
	t.Setenv(stubMarker, "1")
	t.Setenv("EASYSFTP_STUB_STATE", filepath.Join(work, "remote"))
	return driver.Options{
		CandidateBin: stub,
		CandidateRef: "candidate-ref",
		BaselineBin:  stub,
		BaselineRef:  "baseline-ref",
		RemoteBase:   "/easysftp-benchmark-test",
		OutDir:       filepath.Join(work, "out"),
		DatasetDir:   filepath.Join(work, "data"),
		LogDir:       filepath.Join(work, "logs"),
		Server: runner.Server{
			Host: "example.invalid", Port: 22, Username: "tester",
			Password: "secret", KnownHosts: "example.invalid ssh-ed25519 AAAA",
		},
		LinkProfiles:      []string{"baseline"},
		RunnerEnvironment: "local",
		Repeats:           1,
	}
}

func readJSON[T any](t *testing.T, path string) T {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	value, err := schema.DecodeStrict[T](data)
	if err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return *value
}

func TestStandard(t *testing.T) {
	opts := options(t)
	opts.Repeats = 3
	opts.Connections = 2
	if err := driver.Standard(opts); err != nil {
		t.Fatalf("measuring: %v", err)
	}

	result := readJSON[schema.Standard](t, filepath.Join(opts.OutDir, "results.json"))
	if result.SchemaVersion != schema.StandardSchemaVersion {
		t.Errorf("schema_version = %d, want %d", result.SchemaVersion, schema.StandardSchemaVersion)
	}
	// Three builds (candidate, baseline, pool2) x three scenarios.
	if len(result.Results) != 9 {
		t.Errorf("aggregated %d rows, want 9", len(result.Results))
	}
	if len(result.Runs) != 27 {
		t.Errorf("kept %d raw repeats, want 27", len(result.Runs))
	}
	for _, row := range result.Results {
		if row.FailedRuns != 0 {
			t.Errorf("%s/%s: %d failed run(s)", row.Label, row.Scenario, row.FailedRuns)
		}
		if row.LinkProfile != "baseline" {
			t.Errorf("%s/%s: link profile %q, want the implicit baseline", row.Label, row.Scenario, row.LinkProfile)
		}
	}
	if result.Environment == nil || result.Environment.OS == "" {
		t.Error("the environment block is empty")
	}

	// The stub gives the pool build twice the parallelism, so it must come out
	// ahead of the single-connection baseline. This is the check that the delta
	// arithmetic has its sign the right way round.
	delta := comparison(t, result, "pool2", "small")
	if delta == nil || *delta >= 0 {
		t.Errorf("the pool build should be faster than the baseline, got delta %v", delta)
	}

	// The pre-clean is a delete sweep and is stored as one (issue #184, phase
	// 4). Three repeats mean two sweeps that found something: the first
	// pre-clean of a build and scenario runs against an empty directory and is
	// dropped.
	if len(result.Deletes) != 9 {
		t.Errorf("stored %d delete rows, want one per build and scenario", len(result.Deletes))
	}
	sweep := deleteRow(t, result, "candidate", "small")
	if sweep.Sweeps != 2 {
		t.Errorf("counted %d sweeps, want the two that found something", sweep.Sweeps)
	}
	if sweep.FilesDeleted != 300 {
		t.Errorf("deleted %v files, want 300", sweep.FilesDeleted)
	}
	if got := names(sweep.Phases); got != "connect delete_sweep remote_scan" {
		t.Errorf("the delete sweep kept phases %q", got)
	}

	// The whole point of a separate block: an upload aggregate must not have
	// grown a delete phase, and the delete numbers must not have moved an
	// upload median.
	for _, row := range result.Results {
		for _, phase := range row.Phases {
			if phase.Name == "delete_sweep" {
				t.Errorf("%s/%s picked up a delete phase", row.Label, row.Scenario)
			}
		}
		for _, op := range row.Operations {
			if op.Name == "sftp_remove" {
				t.Errorf("%s/%s picked up a delete round-trip", row.Label, row.Scenario)
			}
		}
	}

	// Without a probe binary the result still has to carry a valid link object,
	// with an empty probe list rather than an invented entry.
	if result.Link == nil || len(result.Link.Probes) != 0 || result.Link.Shaping.Available {
		t.Errorf("an unprobed run reports link %+v", result.Link)
	}

	csv := lines(t, filepath.Join(opts.OutDir, "results.csv"))
	if len(csv) != 10 {
		t.Errorf("the CSV has %d lines, want a header plus one row per build and scenario", len(csv))
	}
	summary := read(t, filepath.Join(opts.OutDir, "summary.md"))
	for _, needle := range []string{
		"## easySFTP benchmark", "### Throughput", "### Resources",
		"### Where the time goes", "### Delete sweeps", "No link probe ran for this result",
	} {
		if !strings.Contains(summary, needle) {
			t.Errorf("summary.md is missing %q", needle)
		}
	}
}

func TestMatrix(t *testing.T) {
	opts := options(t)
	opts.Scenarios = []string{"small", "single"}
	opts.ConnectionsAxis = []int{1, 2}
	opts.ConcurrencyAxis = []int{1, 4}
	opts.RequestAxis = []*int{intp(1), intp(16), intp(64)}
	opts.ConnectionsDisplay, opts.ConcurrencyDisplay, opts.RequestDisplay = "1 2", "1 4", "1 16 64"
	if err := driver.Matrix(opts); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	result := readJSON[schema.Matrix](t, filepath.Join(opts.OutDir, "matrix.json"))
	if result.BenchmarkKind != schema.BenchmarkMatrix {
		t.Errorf("benchmark_kind = %q", result.BenchmarkKind)
	}

	// The grid is per scenario (issue #184, phase 5): "small" (300 files) is
	// swept over 2 connections x 2 concurrency at easySFTP's own
	// request_concurrency, and "single" (one 32 MiB file) collapses to a single
	// connections/concurrency cell but is the one scenario the request axis
	// applies to. 2 builds each: 8 + 6.
	if len(result.Cells) != 14 {
		t.Errorf("measured %d cells, want 14", len(result.Cells))
	}
	if got := len(cells(result, "single", "candidate")); got != 3 {
		t.Errorf("a one-file scenario was measured at %d coordinates, want the 3 request values", got)
	}
	for _, cell := range cells(result, "small", "candidate") {
		if cell.RequestConcurrency != nil {
			t.Errorf("the request axis was swept on a payload of 4 KiB files: %v", *cell.RequestConcurrency)
		}
	}
	axes, ok := result.Axes.PerScenario["single"]
	if !ok || axes.Files != 1 {
		t.Errorf("per-scenario axes for single: %+v", axes)
	}

	// A matrix run has no runs[], so a cell is the finest grain there is: the
	// phase and round-trip detail has to survive into it (issue #184, phase 2).
	for _, cell := range result.Cells {
		if len(cell.Phases) == 0 || len(cell.Operations) == 0 {
			t.Errorf("%s c%d/w%d lost its detail", cell.Scenario, cell.Connections, cell.Concurrency)
		}
		if cell.MadMS != nil {
			t.Errorf("a one-repeat cell reports a MAD of %v; 0 there would read as precision", *cell.MadMS)
		}
		for _, phase := range cell.Phases {
			if phase.Name == "delete_sweep" {
				t.Errorf("%s picked up a delete phase", cell.Scenario)
			}
		}
	}

	// The policy measurement of issue #184 phase 5. The stub resolves "auto" to
	// easySFTP's own defaults (1/4/16) and gets faster with more parallelism,
	// so the best cell of "small" is 2/4 and auto must come out behind it.
	if len(result.Auto) != 2 {
		t.Errorf("measured auto %d time(s), want once per scenario and profile", len(result.Auto))
	}
	for _, cell := range result.Cells {
		if cell.Label == "auto" {
			t.Error("auto reached cells[]; it chooses a coordinate rather than sitting at one")
		}
	}
	auto := autoRow(t, result, "small")
	if auto.Chosen.Connections == nil || *auto.Chosen.Connections != 1 ||
		auto.Chosen.Concurrency == nil || *auto.Chosen.Concurrency != 4 {
		t.Errorf("auto reported chosen settings %+v, want the stub's 1/4", auto.Chosen)
	}
	if auto.RegretPercent == nil || *auto.RegretPercent <= 0 {
		t.Errorf("auto picked the slower cell, so the regret should be positive, got %v", auto.RegretPercent)
	}

	// Start, middle and end of the grid: 2 connections x 2 concurrency x 2
	// scenarios x 2 builds leaves the middle canary a middle to sit in.
	if got := canaryOrder(result); got != "start mid end" {
		t.Errorf("the canary ran %q, want start mid end", got)
	}

	// One delete row per cell, minus the first cell of each (build, scenario):
	// its pre-clean has an empty directory in front of it. The auto runs
	// contribute none, because their coordinates are not a coordinate of the
	// grid.
	if len(result.Deletes) != 10 {
		t.Errorf("stored %d delete rows, want 10", len(result.Deletes))
	}
	for _, row := range result.Deletes {
		if row.Label == "auto" {
			t.Error("an auto run contributed a delete row")
		}
	}

	csv := lines(t, filepath.Join(opts.OutDir, "matrix.csv"))
	if len(csv) != 15 {
		t.Errorf("the matrix CSV has %d lines, want a header plus one row per cell", len(csv))
	}
	summary := read(t, filepath.Join(opts.OutDir, "matrix.md"))
	for _, needle := range []string{
		"## easySFTP connections/concurrency matrix", "### Delete sweeps", "### Canary",
		"costs (policy regret)", "**The optimum sits on the edge of the grid**",
	} {
		if !strings.Contains(summary, needle) {
			t.Errorf("matrix.md is missing %q", needle)
		}
	}
}

// TestMatrixDeployShapes is the payload side: a scenario carries a mode and a
// base deploy, not only a payload (issue #184, phase 3).
func TestMatrixDeployShapes(t *testing.T) {
	opts := options(t)
	opts.BaselineBin, opts.BaselineRef = "", ""
	opts.Scenarios = []string{"redeploy", "sync", "calib-10x64k"}
	opts.ConnectionsAxis = []int{1}
	opts.ConcurrencyAxis = []int{2}
	opts.RequestAxis = []*int{nil}
	opts.ConnectionsDisplay, opts.ConcurrencyDisplay, opts.RequestDisplay = "1", "2", "default"
	if err := driver.Matrix(opts); err != nil {
		t.Fatalf("sweeping: %v", err)
	}

	result := readJSON[schema.Matrix](t, filepath.Join(opts.OutDir, "matrix.json"))
	if got := scenarios(result); got != "calib-10x64k redeploy sync" {
		t.Errorf("measured %q", got)
	}
	if got := result.Scenarios["calib-10x64k"]; got != "10 files x 64 KiB, uniform (calibration)" {
		t.Errorf("the calibration scenario is documented as %q", got)
	}

	logs := read(t, filepath.Join(opts.LogDir, "baseline-candidate-sync-c1-w2-rdefault-1.log"))
	if !strings.Contains(logs, "stub: mode=sync") {
		t.Errorf("the sync scenario was not measured in mode sync: %q", logs)
	}
	logs = read(t, filepath.Join(opts.LogDir, "baseline-candidate-redeploy-c1-w2-rdefault-1.log"))
	if !strings.Contains(logs, "stub: mode=overlay skip_unchanged=true") {
		t.Errorf("the redeploy scenario was not measured with skip_unchanged: %q", logs)
	}

	// The unmeasured deploy the measured run redeploys over. Two cells plus the
	// two auto runs of the same scenarios: the policy run is a deployment of
	// that scenario like any other, so it gets the base deploy too. A scenario
	// without a base deploy runs one deploy only.
	if got := len(glob(t, opts.LogDir, "*.base.log")); got != 4 {
		t.Errorf("found %d base deploys, want 4", got)
	}
	if got := len(glob(t, opts.LogDir, "*calib-10x64k*.base.log")); got != 0 {
		t.Errorf("a scenario without a base deploy laid down %d", got)
	}
}

// linkOptions asks for a second link profile and a probe binary.
//
// The interface name belongs to nothing and sudo is refused, so every tc call
// fails and the run takes the degradation path. That is deliberate and must
// stay: this check may run as root on a maintainer's box or on the self-hosted
// runner, and a netem qdisc left on a real interface by a test is a broken
// machine, not a failed assertion.
func linkOptions(t *testing.T) driver.Options {
	t.Helper()
	opts := options(t)
	opts.LinkProfiles = []string{"baseline", "+50ms"}
	opts.LinkProfilesRaw = "baseline +50ms"
	opts.LinkProbeBin = stub
	opts.LinkIface = "easysftp-selftest0"
	opts.LinkSudo = "0"
	return opts
}

// TestStandardOverLinkProfiles covers the phase 1 axis of issue #184: the
// profile loop, the probe either side of each profile's own runs, and shaping
// that is unavailable being recorded rather than fatal.
func TestStandardOverLinkProfiles(t *testing.T) {
	opts := linkOptions(t)
	if err := driver.Standard(opts); err != nil {
		t.Fatalf("measuring: %v", err)
	}
	result := readJSON[schema.Standard](t, filepath.Join(opts.OutDir, "results.json"))

	// The axis multiplies the run rather than replacing it: every build and
	// scenario is measured on every profile.
	measured := map[string]int{}
	for _, row := range result.Results {
		measured[row.LinkProfile]++
	}
	if measured["baseline"] != 6 || measured["+50ms"] != 6 {
		t.Errorf("aggregated %v, want 2 builds x 3 scenarios on each profile", measured)
	}

	// Start and end of each profile's own runs, and no more. Two probes of one
	// profile are comparable and are what makes drift visible; two probes of
	// different profiles are not, which is why they are labelled at all.
	probes := map[string]string{}
	for _, probe := range result.Link.Probes {
		probes[probe.Profile] += probe.At + " "
		if probe.Note != "stub" || probe.RTTMS == nil || probe.RTTMS.P50 == nil {
			t.Errorf("the probe document for %s (%s) was not kept whole", probe.Profile, probe.At)
		}
	}
	if probes["baseline"] != "start end " || probes["+50ms"] != "start end " {
		t.Errorf("probed %v, want a start and an end per profile", probes)
	}

	// Unavailable shaping is data, not a failure. The profile names then say
	// what was asked for, never what happened, so the record of both has to
	// survive into the result.
	if result.Link.Shaping.Available {
		t.Error("shaping reported itself available on an interface that does not exist")
	}
	if result.Link.Shaping.Reason == nil || *result.Link.Shaping.Reason == "" {
		t.Error("unavailable shaping carries no reason")
	}
	// Requested is every profile the run was asked for and Applied only the
	// ones tc really put in place. Keeping both is the whole point: with
	// shaping unavailable the two disagree, and a reader who only saw the
	// profile names would believe a "+50ms" row was measured at +50 ms.
	if got := strings.Join(result.Link.Shaping.Requested, " "); got != "baseline +50ms" {
		t.Errorf("requested shaping %q, want every profile asked for", got)
	}
	if got := strings.Join(result.Link.Shaping.Applied, " "); got != "baseline" {
		t.Errorf("applied shaping %q, want only the profile that needed no tc", got)
	}

	// A delta compares two builds on one profile. Across profiles it would be
	// comparing two different networks and calling the difference a code change.
	for _, row := range result.Comparison {
		if row.LinkProfile == "" {
			t.Error("a comparison row does not say which profile it stayed inside")
		}
	}

	// The CSV columns of phase 1, filled from the profile's start probe.
	csv := lines(t, filepath.Join(opts.OutDir, "results.csv"))
	if !strings.Contains(csv[0], `"link_profile","rtt_p50_ms","control_single_mib_per_s"`) {
		t.Errorf("the CSV does not name the link columns: %s", csv[0])
	}
	for _, row := range csv[1:] {
		if !strings.Contains(row, ",18.4,0.41,") {
			t.Errorf("a CSV row carries no probed link: %s", row)
		}
	}
	if summary := read(t, filepath.Join(opts.OutDir, "summary.md")); !strings.Contains(summary, "### The link") {
		t.Error("summary.md is missing its link section")
	}
}

// TestMatrixOverLinkProfiles is the same axis over a grid, where it multiplies
// everything: the cells, the canary triple and the scaling view.
func TestMatrixOverLinkProfiles(t *testing.T) {
	opts := linkOptions(t)
	opts.ConnectionsAxis = []int{1, 2}
	opts.ConcurrencyAxis = []int{1, 4}
	opts.RequestAxis = []*int{nil}
	opts.Scenarios = []string{"small"}
	if err := driver.Matrix(opts); err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	result := readJSON[schema.Matrix](t, filepath.Join(opts.OutDir, "matrix.json"))

	perProfile := map[string]int{}
	for _, cell := range result.Cells {
		perProfile[cell.LinkProfile]++
	}
	if perProfile["baseline"] != 8 || perProfile["+50ms"] != 8 {
		t.Errorf("measured %v cells, want the 2x2 grid x 2 builds on each profile", perProfile)
	}

	// One canary triple per profile, not one triple spread over both: a canary
	// says whether the host drifted during a profile's own runs, and comparing
	// a value from one profile against another answers a different question.
	canaries := map[string]string{}
	for _, row := range result.Canary {
		canaries[row.LinkProfile] += row.At + " "
	}
	if canaries["baseline"] != "start mid end " || canaries["+50ms"] != "start mid end " {
		t.Errorf("canaries %v, want a triple per profile", canaries)
	}

	// The scaling view and the policy measurement are grouped by profile too.
	for _, view := range result.Scaling {
		if view.LinkProfile == "" {
			t.Error("a scaling row does not say which profile it describes")
		}
	}
	if len(result.Auto) != 2 {
		t.Errorf("measured auto %d time(s), want once per scenario and profile", len(result.Auto))
	}

	if !strings.Contains(read(t, filepath.Join(opts.OutDir, "matrix.md")), "+50ms") {
		t.Error("matrix.md does not report the profile its grids were measured on")
	}
}

// TestRemoteBaseIsRefused pins the guard that keeps a typo from wiping
// something else: the benchmark deletes everything below its remote base.
func TestRemoteBaseIsRefused(t *testing.T) {
	for _, base := range []string{"/", ".", "..", "/easysftp-benchmark/"} {
		opts := options(t)
		opts.RemoteBase = base
		if err := driver.Standard(opts); err == nil {
			t.Errorf("REMOTE_BASE %q was accepted", base)
		}
	}
}

// TestLogDirInsideOutDirIsRefused pins the other guard: run logs name the host
// and the user, and the output directory is uploaded as an artifact, where
// nothing is masked.
func TestLogDirInsideOutDirIsRefused(t *testing.T) {
	opts := options(t)
	opts.LogDir = filepath.Join(opts.OutDir, "logs")
	if err := driver.Standard(opts); err == nil {
		t.Error("a log directory inside the artifact directory was accepted")
	}
}

func intp(v int) *int { return &v }

func comparison(t *testing.T, result schema.Standard, label, name string) *float64 {
	t.Helper()
	for _, row := range result.Comparison {
		if row.Label == label && row.Scenario == name {
			return row.DeltaPercent
		}
	}
	t.Fatalf("no comparison for %s/%s", label, name)
	return nil
}

func deleteRow(t *testing.T, result schema.Standard, label, name string) schema.StandardDelete {
	t.Helper()
	for _, row := range result.Deletes {
		if row.Label == label && row.Scenario == name {
			return row
		}
	}
	t.Fatalf("no delete row for %s/%s", label, name)
	return schema.StandardDelete{}
}

func autoRow(t *testing.T, result schema.Matrix, name string) schema.Auto {
	t.Helper()
	for _, row := range result.Auto {
		if row.Scenario == name {
			return row
		}
	}
	t.Fatalf("no auto row for %s", name)
	return schema.Auto{}
}

func cells(result schema.Matrix, name, label string) []schema.Cell {
	var out []schema.Cell
	for _, cell := range result.Cells {
		if cell.Scenario == name && cell.Label == label {
			out = append(out, cell)
		}
	}
	return out
}

func canaryOrder(result schema.Matrix) string {
	var at []string
	for _, row := range result.Canary {
		at = append(at, row.At)
	}
	return strings.Join(at, " ")
}

func names(phases []schema.Phase) string {
	var out []string
	for _, phase := range phases {
		out = append(out, phase.Name)
	}
	sortStrings(out)
	return strings.Join(out, " ")
}

func scenarios(result schema.Matrix) string {
	seen := map[string]bool{}
	var out []string
	for _, cell := range result.Cells {
		if !seen[cell.Scenario] {
			seen[cell.Scenario] = true
			out = append(out, cell.Scenario)
		}
	}
	sortStrings(out)
	return strings.Join(out, " ")
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	return strings.Split(strings.TrimRight(read(t, path), "\n"), "\n")
}

func glob(t *testing.T, dir, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatalf("globbing %s: %v", pattern, err)
	}
	return matches
}
