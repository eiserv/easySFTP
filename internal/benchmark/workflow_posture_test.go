package benchmark_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests guard the workflow posture of issue #283: the self-hosted
// measuring jobs run code that may be a pull request's, so they hold a
// read-only token, persist no checkout credentials, and never push; the
// storing jobs that write to main are GitHub-hosted and build the harness
// from main. The split is a security boundary, and a YAML file is exactly the
// kind of file a well-meant edit can quietly regress -- the action.yml inputs
// have drift tests for the same reason.

type workflowYAML struct {
	Jobs map[string]struct {
		RunsOn      any               `yaml:"runs-on"`
		Permissions map[string]string `yaml:"permissions"`
		Steps       []struct {
			Uses string         `yaml:"uses"`
			With map[string]any `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadWorkflow(t *testing.T, name string) workflowYAML {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	var wf workflowYAML
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return wf
}

// checkoutsNeverPersist walks a job's steps and fails on the first
// actions/checkout that would leave its token in the job's git config.
func checkoutsNeverPersist(t *testing.T, wf workflowYAML, file, job string) {
	t.Helper()
	j, ok := wf.Jobs[job]
	if !ok {
		t.Fatalf("%s has no job %q", file, job)
	}
	checked := 0
	for _, step := range j.Steps {
		if !strings.HasPrefix(step.Uses, "actions/checkout") {
			continue
		}
		checked++
		if step.With["persist-credentials"] != false {
			t.Errorf("%s: the %s job's checkout step keeps its token on disk; a job that runs pull request code must set persist-credentials: false (issue #283)", file, job)
		}
	}
	if checked == 0 {
		t.Errorf("%s: the %s job checks out nothing? the test no longer guards anything", file, job)
	}
}

func runsOnSelfHosted(t *testing.T, wf workflowYAML, file, job string) bool {
	t.Helper()
	j, ok := wf.Jobs[job]
	if !ok {
		t.Fatalf("%s has no job %q", file, job)
	}
	switch runs := j.RunsOn.(type) {
	case string:
		return false
	case []any:
		for _, label := range runs {
			if s, ok := label.(string); ok && s == "self-hosted" {
				return true
			}
		}
		return false
	}
	t.Fatalf("%s: the %s job has an unreadable runs-on", file, job)
	return false
}

// TestSelfHostedMeasuringJobsHoldNoWriteToken: the two jobs that run on the
// self-hosted machine measure candidate code, which may be a pull request's.
// A contents: write token next to that code is the most valuable credential
// this repository has (it can push to main, replace release assets and move
// the rolling tags), so the measuring jobs get a read-only one.
func TestSelfHostedMeasuringJobsHoldNoWriteToken(t *testing.T) {
	for _, tc := range []struct{ file, job string }{
		{"benchmark.yml", "benchmark"},
		{"benchmark-matrix.yml", "matrix"},
	} {
		wf := loadWorkflow(t, tc.file)
		j, ok := wf.Jobs[tc.job]
		if !ok {
			t.Fatalf("%s has no job %q", tc.file, tc.job)
		}
		if !runsOnSelfHosted(t, wf, tc.file, tc.job) {
			t.Errorf("%s: the %s job is expected to run on the self-hosted runner; the test and the workflow disagree", tc.file, tc.job)
		}
		if got := j.Permissions["contents"]; got != "read" {
			t.Errorf("%s: the self-hosted %s job holds contents: %q; a job that runs candidate code must not hold contents: write (issue #283)", tc.file, tc.job, got)
		}
	}
}

// TestMeasuringJobsPersistNoCredentials: even a read-only token has no
// business on the self-hosted machine's disk after a checkout.
func TestMeasuringJobsPersistNoCredentials(t *testing.T) {
	checkoutsNeverPersist(t, loadWorkflow(t, "benchmark.yml"), "benchmark.yml", "benchmark")
	checkoutsNeverPersist(t, loadWorkflow(t, "benchmark-matrix.yml"), "benchmark-matrix.yml", "matrix")
}

// TestStoringJobsAreNotSelfHosted: the jobs that write to main run the
// harness built from main on a GitHub-hosted runner, so the write token and
// the push both stay off the self-hosted machine.
func TestStoringJobsAreNotSelfHosted(t *testing.T) {
	for _, tc := range []struct{ file, job string }{
		{"benchmark.yml", "store"},
		{"benchmark-matrix.yml", "store"},
	} {
		wf := loadWorkflow(t, tc.file)
		j, ok := wf.Jobs[tc.job]
		if !ok {
			t.Fatalf("%s has no job %q; the measuring/storing split of issue #283 is gone", tc.file, tc.job)
		}
		if runsOnSelfHosted(t, wf, tc.file, tc.job) {
			t.Errorf("%s: the %s job pushes to main and must not run on the self-hosted machine", tc.file, tc.job)
		}
		if got := j.Permissions["contents"]; got != "write" {
			t.Errorf("%s: the %s job needs contents: write to commit the result; got %q", tc.file, tc.job, got)
		}
	}
}

// TestNonPushingJobsPersistNoCredentials: every other workflow's checkouts
// that never push (ci.yml runs pull request code, pages.yml assembles a site,
// release-binaries.yml validates, tests and cross-compiles, the attach and
// analysis jobs read main) keep no token on disk either (issue #283, item 4).
func TestNonPushingJobsPersistNoCredentials(t *testing.T) {
	for _, tc := range []struct{ file, job string }{
		{"ci.yml", "action-tests"},
		{"ci.yml", "test"},
		{"ci.yml", "analysis-tests"},
		{"ci.yml", "self-test"},
		{"pages.yml", "deploy"},
		{"release-binaries.yml", "validate"},
		{"release-binaries.yml", "unit-tests"},
		{"release-binaries.yml", "build"},
		{"benchmark.yml", "attach"},
		{"benchmark-analysis.yml", "analysis"},
	} {
		checkoutsNeverPersist(t, loadWorkflow(t, tc.file), tc.file, tc.job)
	}
}
