package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/eiserv/easySFTP/internal/benchmark/driver"
	"github.com/eiserv/easySFTP/internal/benchmark/link"
	"github.com/eiserv/easySFTP/internal/benchmark/runner"
	"github.com/eiserv/easySFTP/internal/benchmark/scenario"
	"github.com/eiserv/easySFTP/internal/benchmark/schema"
	"github.com/eiserv/easySFTP/internal/benchmark/store"
)

// runStandard measures one or two builds at the default settings.
func runStandard() error {
	opts, err := commonOptions()
	if err != nil {
		return err
	}
	if opts.Repeats, err = envPositive("REPEATS", 3); err != nil {
		return err
	}
	if opts.Connections, err = envPositive("BENCH_CONNECTIONS", 0); err != nil {
		return err
	}
	// Parsed before anything is measured: a typo in a profile must not surface
	// after minutes of uploading.
	opts.LinkProfilesRaw = os.Getenv("BENCH_LINK_PROFILES")
	if opts.LinkProfiles, err = link.ParseProfiles(opts.LinkProfilesRaw); err != nil {
		return err
	}
	return driver.Standard(opts)
}

// runMatrix sweeps advanced.connections against advanced.concurrency.
func runMatrix() error {
	opts, err := commonOptions()
	if err != nil {
		return err
	}
	if opts.Repeats, err = envPositive("REPEATS", 1); err != nil {
		return err
	}
	if opts.ConnectionsAxis, opts.ConnectionsDisplay, err = envAxis("MATRIX_CONNECTIONS", "1 2 4 8"); err != nil {
		return err
	}
	if opts.ConcurrencyAxis, opts.ConcurrencyDisplay, err = envAxis("MATRIX_CONCURRENCY", "1 2 4 8 16 32 64"); err != nil {
		return err
	}
	if opts.RequestAxis, opts.RequestDisplay, err = envRequestAxis("MATRIX_REQUEST_CONCURRENCY", "1 16 64"); err != nil {
		return err
	}

	opts.Scenarios = strings.Fields(envOr("MATRIX_SCENARIOS", "small large single"))
	for _, name := range opts.Scenarios {
		if _, err := scenario.Spec(name); err != nil {
			return err
		}
	}

	opts.LinkProfilesRaw = os.Getenv("MATRIX_LINK_PROFILES")
	if opts.LinkProfiles, err = link.ParseProfiles(opts.LinkProfilesRaw); err != nil {
		return err
	}
	return driver.Matrix(opts)
}

// commonOptions is everything both measuring commands read.
func commonOptions() (driver.Options, error) {
	if err := requireEnv("CANDIDATE_BIN", "CANDIDATE_REF", "OUT_DIR", "DATASET_DIR", "LOG_DIR",
		"REMOTE_BASE", "BENCH_HOST", "BENCH_USERNAME", "BENCH_PASSWORD", "BENCH_KNOWN_HOSTS"); err != nil {
		return driver.Options{}, err
	}
	port, err := envPositive("BENCH_PORT", 22)
	if err != nil {
		return driver.Options{}, err
	}
	return driver.Options{
		CandidateBin: os.Getenv("CANDIDATE_BIN"),
		CandidateRef: os.Getenv("CANDIDATE_REF"),
		BaselineBin:  os.Getenv("BASELINE_BIN"),
		BaselineRef:  os.Getenv("BASELINE_REF"),
		RemoteBase:   os.Getenv("REMOTE_BASE"),
		OutDir:       os.Getenv("OUT_DIR"),
		DatasetDir:   os.Getenv("DATASET_DIR"),
		LogDir:       os.Getenv("LOG_DIR"),
		Server: runner.Server{
			Host:       os.Getenv("BENCH_HOST"),
			Port:       port,
			Username:   os.Getenv("BENCH_USERNAME"),
			Password:   os.Getenv("BENCH_PASSWORD"),
			KnownHosts: os.Getenv("BENCH_KNOWN_HOSTS"),
		},
		LinkProbeBin:      os.Getenv("LINKPROBE_BIN"),
		LinkIface:         os.Getenv("LINK_IFACE"),
		LinkSudo:          os.Getenv("LINK_SUDO"),
		RunnerEnvironment: envOr("RUNNER_ENVIRONMENT", "local"),
		RunnerName:        os.Getenv("RUNNER_NAME"),
	}, nil
}

// runStore files a measured result set under benchmarks/.
func runStore() error {
	keep, err := envPositive("KEEP_RELEASES", 5)
	if err != nil {
		return err
	}
	opts := store.Options{
		Kind:         os.Getenv("KIND"),
		ResultsJSON:  os.Getenv("RESULTS_JSON"),
		SummaryMD:    os.Getenv("SUMMARY_MD"),
		ResultsCSV:   os.Getenv("RESULTS_CSV"),
		Version:      os.Getenv("VERSION"),
		Label:        envOr("LABEL", "manual"),
		RecordedAt:   envOr("RECORDED_AT", time.Now().UTC().Format("2006-01-02T15:04:05Z")),
		Commit:       os.Getenv("COMMIT"),
		RunURL:       os.Getenv("RUN_URL"),
		Dir:          envOr("BENCH_DIR", "benchmarks"),
		KeepReleases: keep,
	}
	if opts.Kind != store.KindReindex {
		if opts.ResultsJSON == "" {
			return fmt.Errorf("RESULTS_JSON is required")
		}
		if opts.SummaryMD == "" {
			return fmt.Errorf("SUMMARY_MD is required")
		}
		for _, path := range []string{opts.ResultsJSON, opts.SummaryMD} {
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("%s does not exist", path)
			}
		}
		if opts.ResultsCSV != "" {
			if _, err := os.Stat(opts.ResultsCSV); err != nil {
				return fmt.Errorf("RESULTS_CSV ('%s') does not exist", opts.ResultsCSV)
			}
		}
	}
	// A release is filed under its version and a sweep under its label, so the
	// two never share a name; the store refuses to reuse one either way.
	if opts.Kind != schema.KindRelease {
		opts.Version = ""
	}
	return store.Run(opts)
}
