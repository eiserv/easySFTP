package driver

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/eiserv/easySFTP/internal/benchmark"
	"github.com/eiserv/easySFTP/internal/benchmark/link"
	"github.com/eiserv/easySFTP/internal/benchmark/runner"
	"github.com/eiserv/easySFTP/internal/benchmark/scenario"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/stats"
	"github.com/eiserv/easySFTP/internal/config"
)

// StandardScenarios is the standard benchmark's scenario set, and it is fixed:
// adding one here would make every stored result before it incomparable. The
// matrix benchmark is where new scenarios such as "single" live.
var StandardScenarios = []string{"small", "mixed", "large"}

// Standard measures one or two builds of easySFTP at the default settings and
// writes results.json, results.csv and summary.md (issue #169).
//
// It collects data only: nothing here fails on a slow number, because
// run-to-run variance against an external host is not understood yet.
//
// Candidate and baseline are interleaved repeat by repeat, not run as two
// blocks: a shared host's throughput drifts over minutes, and a block layout
// would charge that drift to whichever build ran second.
func Standard(opts Options) error {
	r, err := newRun(opts)
	if err != nil {
		return err
	}
	defer r.closeFiles()
	defer r.shaper.Clear()

	for _, f := range []struct{ name, filename string }{
		{"runs", "runs.jsonl"},
		{"probes", "link-probes.jsonl"},
		{"deletes", "deletes.jsonl"},
	} {
		if _, err := r.open(f.name, f.filename); err != nil {
			return err
		}
	}

	r.setBuilds(true)
	if err := scenario.Generate(opts.DatasetDir, StandardScenarios, logf, warnf); err != nil {
		return err
	}
	r.shapeIfNeeded("measured")

	// The profile loop is the outermost one: re-applying tc per measured run
	// would itself be noise. The price is that drift over the hours falls onto
	// this axis instead of spreading across it, which is why each profile is
	// probed twice, before and after its own runs, and still shaped for both. A
	// start and an end probe of the same profile are comparable; two probes of
	// different profiles are not, which is what makes drift visible here at
	// all.
	for _, profile := range opts.LinkProfiles {
		r.profile = profile
		if err := r.shaper.Apply(profile); err == nil {
			r.shaper.Guard()
		}
		if err := r.probe(profile, "start"); err != nil {
			return err
		}
		for _, name := range StandardScenarios {
			for repeat := 1; repeat <= opts.Repeats; repeat++ {
				for i := range r.labels {
					if err := r.measure(i, name, repeat); err != nil {
						return err
					}
				}
			}
		}
		if err := r.probe(profile, "end"); err != nil {
			return err
		}
	}

	// The deferred Clear does this too, but only on the way out: doing it here
	// keeps the cleanup runs below on the same unshaped line every run ends on.
	r.shaper.Clear()

	// Leave the benchmark directories empty so the payload does not linger on
	// the server. Best effort: a cleanup hiccup must not hide the results.
	for _, name := range StandardScenarios {
		for i, label := range r.labels {
			stem := filepath.Join(opts.LogDir, "cleanup-"+label+"-"+name)
			code, err := r.cleanAt(r.binaries[i], opts.RemoteBase+"/"+label+"/"+name, stem, r.advanced[i], "")
			if err != nil || code != 0 {
				warnf("cleanup of %s/%s failed", label, name)
			}
		}
	}
	r.cleanupLinkProbe()

	return r.aggregateStandard()
}

// probe records one link probe, or nothing when there is no probe binary.
func (r *run) probe(profile, at string) error {
	document := r.prober.Probe(profile, at)
	if document == nil {
		return nil
	}
	return r.append("probes", document)
}

// measure runs one build against one scenario once, and appends what it
// reported.
func (r *run) measure(build int, name string, repeat int) error {
	label, binary, ref, advanced := r.labels[build], r.binaries[build], r.refs[build], r.advanced[build]
	remote := r.opts.RemoteBase + "/" + label + "/" + name
	stem := r.stem(link.Slug(r.profile), label, name, fmt.Sprint(repeat))

	cleanCode, err := r.preClean(binary, remote, stem, "", true)
	if err != nil {
		return err
	}
	if cleanCode != 0 {
		warnf("pre-clean of %s/%s repeat %d exited %d", label, name, repeat, cleanCode)
		cat(stem + ".clean.log")
	}
	sweep := readDelete(stem, cleanCode)
	sweep.Label, sweep.Scenario, sweep.LinkProfile, sweep.Repeat = label, name, r.profile, repeat
	if err := r.append("deletes", sweep); err != nil {
		return err
	}

	code, err := r.runner.Do(runner.Run{
		Binary:   binary,
		Source:   filepath.Join(r.opts.DatasetDir, name),
		Remote:   remote,
		Mode:     "overlay",
		Log:      stem + ".log",
		Outputs:  stem + ".out",
		Metrics:  stem + ".metrics.json",
		Advanced: advanced,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		// Into the job log, which masks secrets. Never into the artifact.
		warnf("%s/%s repeat %d exited %d", label, name, repeat, code)
		cat(stem + ".log")
	}

	duration := runner.StepNumber(stem+".out", "duration-ms")
	retries := runner.CountLines(stem+".log", runner.ContainsAny("retrying", "reconnecting"))
	errorLines := runner.CountLines(stem+".log", runner.HasPrefix("::error::"))
	refused := runner.CountLines(stem+".log", runner.ContainsAny("could not open connection"))
	if err := r.append("runs", runRecord{
		Label:       label,
		Ref:         ref,
		Scenario:    name,
		LinkProfile: r.profile,
		Repeat:      repeat,
		ExitCode:    code,
		DurationMS:  duration,
		Files:       runner.StepNumber(stem+".out", "files-uploaded"),
		Bytes:       runner.StepNumber(stem+".out", "bytes-uploaded"),
		Retries:     &retries,
		Errors:      &errorLines,
		Refused:     &refused,
		Metrics:     runner.ReadMetrics(stem + ".metrics.json"),
	}); err != nil {
		return err
	}

	logf("%s %s/%s repeat %d: %s ms, exit %d", r.profile, label, name, repeat, stats.Format(duration), code)
	return nil
}

// defaultsSentence describes what a run at the defaults is actually configured
// with, read from internal/config rather than restated.
//
// It used to be a literal, and it went stale the day #211 made auto the real
// default: two permanent documents (v3.6.0 and v3.7.0) say "concurrency 4,
// request_concurrency 16" for runs that in fact resolved all three transport
// settings through the auto policy, per scenario (issue #229). The three store
// invariants mean those documents stay as they are; benchmarks/README.md
// carries the correction.
//
// The resolved numbers deliberately stay out of this sentence. They are not
// run-wide any more, so the honest run-wide statement is that the run left them
// to easySFTP; what the policy then chose is per scenario and already lives in
// the auto[] block, read back from the run's own config_* counters.
//
// One caveat worth naming: this reads the defaults of the tree the harness was
// built from, which for a release sweep is the tree of the build being
// measured. A comparison run against an older baseline ref describes the
// candidate's defaults, which is the run the sentence is about.
func defaultsSentence() string {
	cfg := config.Defaults()
	autoOr := func(isAuto bool, n int) string {
		if isAuto {
			return "auto"
		}
		return strconv.Itoa(n)
	}
	return fmt.Sprintf(
		"easySFTP defaults (no advanced.* overrides): connections %s, concurrency %s, request_concurrency %s, retries %d, timeout %s, mode overlay",
		autoOr(cfg.Auto.Connections, cfg.Connections),
		autoOr(cfg.Auto.Concurrency, cfg.Concurrency),
		autoOr(cfg.Auto.RequestConcurrency, cfg.SftpRequestConcurrency),
		cfg.Retries,
		cfg.Timeout,
	)
}

// aggregateStandard hands the run over to the aggregation half.
//
// The display strings travel verbatim rather than being rebuilt on the other
// side. This half already has them, and a summary table that reformats itself
// during a rewrite is a change to a stored document made by accident.
func (r *run) aggregateStandard() error {
	settings := defaultsSentence()
	if r.opts.Connections > 0 {
		settings += fmt.Sprintf("; the pool%d build is the same binary with advanced.connections: %d",
			r.opts.Connections, r.opts.Connections)
	}
	if r.opts.LinkProfilesRaw != "" {
		settings += "; measured over the link profiles " + join(r.opts.LinkProfiles)
	}

	scenarios := make([]benchmark.ManifestScenario, 0, len(StandardScenarios))
	for _, name := range StandardScenarios {
		scenarios = append(scenarios, benchmark.ManifestScenario{
			Name:        name,
			Description: scenario.Description(name),
		})
	}

	runnerLine, runnerDisplay := r.runnerLine()
	manifest := &benchmark.Manifest{
		Kind:           schema.BenchmarkStandard,
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
		Files: benchmark.ManifestFiles{
			Runs:    r.path("runs"),
			Deletes: r.path("deletes"),
			Probes:  r.path("probes"),
		},
	}
	return r.finish(manifest)
}
