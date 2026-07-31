package driver

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eiserv/easySFTP/internal/benchmark"
	"github.com/eiserv/easySFTP/internal/benchmark/link"
	"github.com/eiserv/easySFTP/internal/benchmark/runner"
	"github.com/eiserv/easySFTP/internal/benchmark/scenario"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/stats"
)

// The canary cell. Deliberately constants and not options: three canaries of
// one run are compared against each other, and two runs' canaries against each
// other, which only works while the cell stays the same everywhere.
const (
	CanaryScenario    = "small"
	CanaryConnections = 1
	CanaryConcurrency = 4
)

// axes is one scenario's own grid, after its payload capped what was asked for.
type axes struct {
	connections []int
	concurrency []int
	request     []*int
	files       int
}

// cells is how many measured runs this scenario contributes to one profile.
func (a axes) cells(labels, repeats int) int {
	return len(a.connections) * len(a.concurrency) * len(a.request) * labels * repeats
}

// Matrix sweeps advanced.connections against advanced.concurrency (optionally
// advanced.request_concurrency as a third axis) and records one cell per
// combination, so the result can be read as a heatmap or as a scaling curve
// rather than as a single number.
//
// Candidate and baseline are measured back to back *within* each cell, not as
// two separate sweeps: a shared host drifts over the hour such a sweep takes,
// and a per-sweep layout would charge all of that drift to whichever build ran
// second.
func Matrix(opts Options) error {
	r, err := newRun(opts)
	if err != nil {
		return err
	}
	defer r.closeFiles()
	defer r.shaper.Clear()

	for _, f := range []struct{ name, filename string }{
		{"runs", "matrix-runs.jsonl"},
		{"probes", "link-probes.jsonl"},
		{"canary", "canary.jsonl"},
		{"deletes", "deletes.jsonl"},
		{"auto", "auto.jsonl"},
	} {
		if _, err := r.open(f.name, f.filename); err != nil {
			return err
		}
	}
	r.setBuilds(false)

	// The grid is per scenario, not global (issue #184, phase 5). Two payload
	// facts decide it, and both are properties of the payload rather than of
	// the code: a value above the file count cannot be used by anything, and
	// request_concurrency is per file, so a payload of 4 KiB files cannot show
	// it at all. Computed once, up front, because the run count printed below
	// has to be the count that is actually measured.
	perScenario := map[string]axes{}
	cellsPerProfile := 0
	for _, name := range opts.Scenarios {
		a, err := axesFor(name, opts)
		if err != nil {
			return err
		}
		perScenario[name] = a
		cellsPerProfile += a.cells(len(r.labels), opts.Repeats)
		logf("matrix: %s (%d file(s)) sweeps connections %s, concurrency %s, request_concurrency %s",
			name, a.files, joinInts(a.connections), joinInts(a.concurrency), joinRequests(a.request))
	}
	total := cellsPerProfile * len(opts.LinkProfiles)
	logf("matrix: %d scenario(s) x %d link profile(s) x %d build(s) x %d repeat(s) = %d measured run(s), "+
		"plus up to %d canary run(s) and %d auto run(s)",
		len(opts.Scenarios), len(opts.LinkProfiles), len(r.labels), opts.Repeats, total,
		3*len(opts.LinkProfiles), len(opts.Scenarios)*opts.Repeats*len(opts.LinkProfiles))

	// A prepopulated scenario deploys its tree twice per cell, and only the
	// second one is measured. Worth saying before the hours start rather than
	// after.
	var prepopulated []string
	for _, name := range opts.Scenarios {
		if scenario.ShapeOf(name).Prepopulate {
			prepopulated = append(prepopulated, name)
		}
	}
	if len(prepopulated) > 0 {
		logf("matrix: %s are redeploy scenarios; each of their cells runs an unmeasured full deploy first, "+
			"so those cells cost roughly twice their measured time", join(prepopulated))
	}

	// The canary payload has to exist even when its scenario is not swept.
	datasetScenarios := append([]string(nil), opts.Scenarios...)
	if !contains(opts.Scenarios, CanaryScenario) {
		datasetScenarios = append(datasetScenarios, CanaryScenario)
	}
	if err := scenario.Generate(opts.DatasetDir, datasetScenarios, logf, warnf); err != nil {
		return err
	}
	r.shapeIfNeeded("swept")

	// The profile loop is the outermost one: re-applying tc per cell would
	// itself be noise, and a sweep already takes hours. Each profile is probed
	// before and after its own grid, so drift inside a profile is visible; two
	// probes of different profiles are not comparable and are not meant to be.
	for _, profile := range opts.LinkProfiles {
		r.profile = profile
		if err := r.shaper.Apply(profile); err == nil {
			r.shaper.Guard()
		}
		if err := r.probe(profile, "start"); err != nil {
			return err
		}
		if err := r.measureCanary("start"); err != nil {
			return err
		}

		// Counted in measured runs rather than in wall clock, so the middle
		// canary sits in the middle of the work and not of an estimate. A grid
		// of one cell has no middle, and then there are two canaries instead of
		// three.
		done := 0
		for _, name := range opts.Scenarios {
			a := perScenario[name]
			for repeat := 1; repeat <= opts.Repeats; repeat++ {
				// Before this repeat's cells, so the policy run and the grid it
				// is scored against sit inside the same stretch of the sweep.
				if err := r.measureAuto(name, repeat); err != nil {
					return err
				}
				for _, conns := range a.connections {
					for _, conc := range a.concurrency {
						for _, request := range a.request {
							// Innermost, so candidate and baseline of one cell
							// are measured within seconds of each other.
							for i := range r.labels {
								if err := r.measureCell(i, name, conns, conc, request, repeat); err != nil {
									return err
								}
								done++
								if done == cellsPerProfile/2 {
									if err := r.measureCanary("mid"); err != nil {
										return err
									}
								}
							}
						}
					}
				}
			}
		}
		if err := r.measureCanary("end"); err != nil {
			return err
		}
		if err := r.probe(profile, "end"); err != nil {
			return err
		}
	}

	// The deferred Clear does this too, but only on the way out: doing it here
	// keeps the cleanup runs below on the unshaped line.
	r.shaper.Clear()

	// Leave the server as we found it. Best effort: a cleanup hiccup must not
	// hide the results.
	for _, name := range opts.Scenarios {
		for i, label := range r.labels {
			stem := filepath.Join(opts.LogDir, "cleanup-"+label+"-"+name)
			if code, err := r.cleanAt(r.binaries[i],
				opts.RemoteBase+"/matrix/"+label+"/"+name, stem, "", ""); err != nil || code != 0 {
				warnf("cleanup of %s/%s failed", label, name)
			}
		}
		stem := filepath.Join(opts.LogDir, "cleanup-auto-"+name)
		if code, err := r.cleanAt(opts.CandidateBin,
			opts.RemoteBase+"/matrix/auto/"+name, stem, "", ""); err != nil || code != 0 {
			warnf("cleanup of auto/%s failed", name)
		}
	}
	if code, err := r.cleanAt(opts.CandidateBin, opts.RemoteBase+"/matrix/canary",
		filepath.Join(opts.LogDir, "cleanup-canary"), "", ""); err != nil || code != 0 {
		warnf("cleanup of the canary directory failed")
	}
	r.cleanupLinkProbe()

	return r.aggregateMatrix(perScenario)
}

// axesFor caps a scenario's axes against its payload and decides whether the
// request axis applies to it at all.
func axesFor(name string, opts Options) (axes, error) {
	files, err := scenario.Files(name)
	if err != nil {
		return axes{}, err
	}
	connections, err := scenario.AxisFor(name, opts.ConnectionsAxis)
	if err != nil {
		return axes{}, err
	}
	concurrency, err := scenario.AxisFor(name, opts.ConcurrencyAxis)
	if err != nil {
		return axes{}, err
	}
	sweeps, err := scenario.SweepsRequests(name)
	if err != nil {
		return axes{}, err
	}
	request := opts.RequestAxis
	if !sweeps {
		// The one pass that sets nothing and leaves easySFTP its own value,
		// stored as a null coordinate.
		request = []*int{nil}
	}
	return axes{connections: connections, concurrency: concurrency, request: request, files: files}, nil
}

// deploy runs one deployment of a scenario exactly the way its shape says: a
// pre-clean, the unmeasured base deploy plus mutation a redeploy scenario
// needs, then the measured run. Shared by the cells and the auto runs, which
// differ only in the advanced block they pass and in what they do with the
// result.
//
// The pre-clean is only instrumented where its numbers are wanted, which is the
// cells: an auto run's coordinates are not a coordinate of the grid, so a
// delete row of it would sit in deletes[] under settings nobody asked for.
func (r *run) deploy(binary, name, remote, stem, advanced string, instrumentClean bool, what string) (code, cleanCode int, err error) {
	shape := scenario.ShapeOf(name)

	// Kept off the pre-clean: skip_unchanged applies to overlay only, and a
	// clean deployment carrying it just logs a warning about being ignored.
	deployAdvanced := advanced
	if shape.Prepopulate && shape.Mode == "overlay" {
		deployAdvanced = advanced + "\nskip_unchanged: true"
	}

	cleanCode, err = r.preClean(binary, remote, stem, advanced, instrumentClean)
	if err != nil {
		return 0, 0, err
	}
	if cleanCode != 0 {
		warnf("pre-clean of %s exited %d", what, cleanCode)
		cat(stem + ".clean.log")
	}

	// The deploy the measured run redeploys over. Unmeasured, with the same
	// settings: what is under test is the second run, and a base laid down at
	// other settings would put a different remote tree under each cell.
	if shape.Prepopulate {
		baseCode, err := r.runner.Do(runner.Run{
			Binary:   binary,
			Source:   filepath.Join(r.opts.DatasetDir, name),
			Remote:   remote,
			Mode:     shape.Mode,
			Log:      stem + ".base.log",
			Outputs:  stem + ".base.out",
			Advanced: deployAdvanced,
		})
		if err != nil {
			return 0, 0, err
		}
		if baseCode != 0 {
			warnf("base deploy of %s failed", what)
			cat(stem + ".base.log")
		}
		if err := scenario.Mutate(filepath.Join(r.opts.DatasetDir, name), scenario.ChangedFiles); err != nil {
			return 0, 0, err
		}
	}

	code, err = r.runner.Do(runner.Run{
		Binary:   binary,
		Source:   filepath.Join(r.opts.DatasetDir, name),
		Remote:   remote,
		Mode:     shape.Mode,
		Log:      stem + ".log",
		Outputs:  stem + ".out",
		Metrics:  stem + ".metrics.json",
		Advanced: deployAdvanced,
	})
	if err != nil {
		return 0, 0, err
	}
	if code != 0 {
		// Into the job log, which masks secrets. Never into the artifact.
		warnf("%s exited %d", what, code)
		cat(stem + ".log")
	}
	return code, cleanCode, nil
}

// measureCell measures one coordinate of the grid.
//
// The remote path is per (build, scenario) rather than per cell: every run is
// preceded by an unmeasured clean anyway, so a path per cell would only leave
// more empty directories behind on the server.
func (r *run) measureCell(build int, name string, conns, conc int, request *int, repeat int) error {
	label, binary, ref := r.labels[build], r.binaries[build], r.refs[build]
	remote := r.opts.RemoteBase + "/matrix/" + label + "/" + name
	stem := r.stem(link.Slug(r.profile), label, name,
		fmt.Sprintf("c%d", conns), fmt.Sprintf("w%d", conc), "r"+requestToken(request), fmt.Sprint(repeat))
	what := fmt.Sprintf("%s/%s c%d/w%d/r%s repeat %d", label, name, conns, conc, requestToken(request), repeat)

	advanced := fmt.Sprintf("connections: %d\nconcurrency: %d", conns, conc)
	if request != nil {
		advanced += fmt.Sprintf("\nrequest_concurrency: %d", *request)
	}

	code, cleanCode, err := r.deploy(binary, name, remote, stem, advanced, true, what)
	if err != nil {
		return err
	}

	sweep := readDelete(stem, cleanCode)
	sweep.Label, sweep.Scenario, sweep.LinkProfile, sweep.Repeat = label, name, r.profile, repeat
	sweep.Connections, sweep.Concurrency, sweep.RequestConcurrency = &conns, &conc, request
	if err := r.append("deletes", sweep); err != nil {
		return err
	}

	duration := runner.StepNumber(stem+".out", "duration-ms")
	retries := runner.CountLines(stem+".log", runner.ContainsAny("retrying", "reconnecting"))
	errorLines := runner.CountLines(stem+".log", runner.HasPrefix("::error::"))
	if err := r.append("runs", runRecord{
		Label:              label,
		Ref:                ref,
		Scenario:           name,
		LinkProfile:        r.profile,
		Connections:        &conns,
		Concurrency:        &conc,
		RequestConcurrency: request,
		Repeat:             repeat,
		ExitCode:           code,
		DurationMS:         duration,
		Files:              runner.StepNumber(stem+".out", "files-uploaded"),
		Bytes:              runner.StepNumber(stem+".out", "bytes-uploaded"),
		Retries:            &retries,
		Errors:             &errorLines,
		Metrics:            runner.ReadMetrics(stem + ".metrics.json"),
	}); err != nil {
		return err
	}

	logf("%s %s/%s connections=%d concurrency=%d request=%s repeat %d: %s ms, exit %d",
		r.profile, label, name, conns, conc, requestToken(request), repeat, stats.Format(duration), code)
	return nil
}

// measureAuto measures the candidate build with connections, concurrency and
// request_concurrency all left at "auto" (issue #184, phase 5).
//
// This is the policy under test, not a coordinate: auto picks its own settings,
// so what the run is measured for is the gap to the best cell of the same
// scenario and profile. Which settings it picked is read back out of the run's
// own counters (config_connections and friends, see internal/uploader) rather
// than assumed here, because the point of the measurement is what easySFTP did,
// not what this package believes it does.
//
// It runs inside the repeat loop, next to the cells it is compared against, and
// on the candidate build only: a regret against a baseline build's grid would
// compare two different products.
func (r *run) measureAuto(name string, repeat int) error {
	stem := r.stem("auto", link.Slug(r.profile), name, fmt.Sprint(repeat))
	remote := r.opts.RemoteBase + "/matrix/auto/" + name
	what := fmt.Sprintf("auto/%s repeat %d", name, repeat)
	advanced := "connections: auto\nconcurrency: auto\nrequest_concurrency: auto"

	code, _, err := r.deploy(r.opts.CandidateBin, name, remote, stem, advanced, false, what)
	if err != nil {
		return err
	}

	duration := runner.StepNumber(stem+".out", "duration-ms")
	if err := r.append("auto", runRecord{
		Label:       "auto",
		Ref:         r.opts.CandidateRef,
		Scenario:    name,
		LinkProfile: r.profile,
		Repeat:      repeat,
		ExitCode:    code,
		DurationMS:  duration,
		Files:       runner.StepNumber(stem+".out", "files-uploaded"),
		Bytes:       runner.StepNumber(stem+".out", "bytes-uploaded"),
		Metrics:     runner.ReadMetrics(stem + ".metrics.json"),
	}); err != nil {
		return err
	}

	logf("%s auto/%s repeat %d: %s ms, exit %d", r.profile, name, repeat, stats.Format(duration), code)
	return nil
}

// measureCanary measures the fixed cell, always the candidate build, three
// times per profile ("start", "mid", "end").
//
// Its numbers are kept apart from cells[] on purpose: they measure the server's
// steadiness, not a coordinate of the grid, and mixing them into an aggregate
// would hide exactly what they are there to show.
func (r *run) measureCanary(at string) error {
	stem := r.stem("canary", link.Slug(r.profile), at)
	remote := r.opts.RemoteBase + "/matrix/canary"
	advanced := fmt.Sprintf("connections: %d\nconcurrency: %d", CanaryConnections, CanaryConcurrency)

	if code, err := r.cleanAt(r.opts.CandidateBin, remote, stem+".clean", advanced, ""); err != nil || code != 0 {
		warnf("pre-clean of the %s canary on %s failed", at, r.profile)
	}

	code, err := r.runner.Do(runner.Run{
		Binary:   r.opts.CandidateBin,
		Source:   filepath.Join(r.opts.DatasetDir, CanaryScenario),
		Remote:   remote,
		Mode:     "overlay",
		Log:      stem + ".log",
		Outputs:  stem + ".out",
		Advanced: advanced,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		warnf("the %s canary on %s exited %d", at, r.profile, code)
	}

	duration := runner.StepNumber(stem+".out", "duration-ms")
	if err := r.append("canary", schema.Canary{
		LinkProfile: r.profile,
		At:          at,
		Scenario:    CanaryScenario,
		Connections: CanaryConnections,
		Concurrency: CanaryConcurrency,
		ExitCode:    code,
		DurationMS:  duration,
		Files:       runner.StepNumber(stem+".out", "files-uploaded"),
		Bytes:       runner.StepNumber(stem+".out", "bytes-uploaded"),
	}); err != nil {
		return err
	}

	logf("%s canary %s: %s ms, exit %d", r.profile, at, stats.Format(duration), code)
	return nil
}

// aggregateMatrix hands the sweep over to the aggregation half.
//
// One scenario entry each, carrying both what it is and the axes its payload
// allowed. The display strings are the ones the summary prints, kept verbatim
// so a rewrite cannot reformat a stored document by accident.
func (r *run) aggregateMatrix(perScenario map[string]axes) error {
	settings := fmt.Sprintf("matrix sweep; every other advanced.* setting stays at easySFTP's defaults "+
		"(retries 2, timeout 30s). The mode belongs to the scenario, see scenarios below: a redeploy scenario "+
		"is deployed once unmeasured and then measured over itself with %d file(s) changed. Each scenario is "+
		"swept over the axis values its payload can use, see axes.per_scenario", scenario.ChangedFiles)
	if r.opts.LinkProfilesRaw != "" {
		settings += "; swept over the link profiles " + join(r.opts.LinkProfiles)
	}

	scenarios := make([]benchmark.ManifestScenario, 0, len(r.opts.Scenarios))
	for _, name := range r.opts.Scenarios {
		a := perScenario[name]
		shape := scenario.ShapeOf(name)
		mode := shape.Mode
		if shape.Prepopulate {
			mode += ", redeployed"
		}
		scenarios = append(scenarios, benchmark.ManifestScenario{
			Name:               name,
			Description:        scenario.Description(name),
			Mode:               mode,
			Files:              a.files,
			ConnectionsDisplay: joinInts(a.connections) + " ",
			ConcurrencyDisplay: joinInts(a.concurrency) + " ",
			RequestDisplay:     joinRequests(a.request),
			Connections:        a.connections,
			Concurrency:        a.concurrency,
			RequestConcurrency: a.request,
		})
	}

	runnerLine, runnerDisplay := r.runnerLine()
	manifest := &benchmark.Manifest{
		Kind:           schema.BenchmarkMatrix,
		CandidateRef:   r.opts.CandidateRef,
		BaselineRef:    r.opts.BaselineRef,
		ReferenceLabel: r.reference,
		Repeats:        r.opts.Repeats,
		Runner:         runnerLine,
		RunnerDisplay:  runnerDisplay,
		Environment:    r.environment(),
		Settings:       settings,
		Labels:         r.labels,
		Scenarios:      scenarios,
		Link:           r.linkManifest(),
		Grid: &benchmark.ManifestGrid{
			Connections:        r.opts.ConnectionsAxis,
			Concurrency:        r.opts.ConcurrencyAxis,
			RequestConcurrency: r.opts.RequestAxis,
			ConnectionsDisplay: r.opts.ConnectionsDisplay,
			ConcurrencyDisplay: r.opts.ConcurrencyDisplay,
			RequestDisplay:     r.opts.RequestDisplay,
			Canary: benchmark.ManifestCanary{
				Scenario:    CanaryScenario,
				Connections: CanaryConnections,
				Concurrency: CanaryConcurrency,
			},
		},
		Files: benchmark.ManifestFiles{
			Runs:    r.path("runs"),
			Deletes: r.path("deletes"),
			Probes:  r.path("probes"),
			Canary:  r.path("canary"),
			Auto:    r.path("auto"),
		},
	}
	return r.finish(manifest)
}

// requestToken is how a request_concurrency coordinate appears in a log file
// name and in the job log. The token "default" is the pass that sets nothing
// and leaves easySFTP its own value; it travels as that token rather than as an
// empty string because it has to survive being stored in the per-scenario axis
// strings.
func requestToken(request *int) string {
	if request == nil {
		return "default"
	}
	return fmt.Sprint(*request)
}

func joinRequests(values []*int) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, requestToken(v))
	}
	return strings.Join(parts, " ")
}

func joinInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, fmt.Sprint(v))
	}
	return strings.Join(parts, " ")
}

func join(values []string) string { return strings.Join(values, " ") }

func contains(values []string, needle string) bool {
	for _, v := range values {
		if v == needle {
			return true
		}
	}
	return false
}
