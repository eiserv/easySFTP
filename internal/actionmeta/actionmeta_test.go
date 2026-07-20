package actionmeta_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type metadata struct {
	Name    string                 `yaml:"name"`
	Inputs  map[string]actionInput `yaml:"inputs"`
	Outputs map[string]any         `yaml:"outputs"`
	Runs    struct {
		Using string       `yaml:"using"`
		Steps []actionStep `yaml:"steps"`
	} `yaml:"runs"`
}

type actionInput struct {
	Default string `yaml:"default"`
}

type actionStep struct {
	Name string            `yaml:"name"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Env  map[string]string `yaml:"env"`
}

func TestActionMetadata(t *testing.T) {
	path := filepath.Join("..", "..", "action.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var action metadata
	if err := yaml.Unmarshal(data, &action); err != nil {
		t.Fatalf("parse action.yml: %v", err)
	}
	if action.Name != "easySFTP" {
		t.Errorf("name = %q, want easySFTP", action.Name)
	}
	if action.Runs.Using != "composite" {
		t.Errorf("runs.using = %q, want composite", action.Runs.Using)
	}
	if len(action.Runs.Steps) == 0 {
		t.Fatal("action has no steps")
	}
	input, ok := action.Inputs["build-mode"]
	if !ok {
		t.Fatal("build-mode input is missing")
	}
	if input.Default != "prebuilt" {
		t.Errorf("build-mode default = %q, want prebuilt", input.Default)
	}

	wantInputs := []string{
		"build-mode", "server", "port", "username", "password", "private-key",
		"passphrase", "host-key-fingerprint", "known-hosts", "uploads", "config-file", "strategy",
		"ignore", "ignore-from", "max-deletes", "delete", "dry-run", "concurrency",
		"sftp-request-concurrency", "sync-fast-path", "skip-unchanged", "retries",
		"timeout", "stall-timeout", "dir-mode", "file-mode", "log-level", "manifest-name",
	}
	for _, name := range wantInputs {
		if _, ok := action.Inputs[name]; !ok {
			t.Errorf("input %q is missing", name)
		}
	}

	// Structural drift check: every EASYSFTP_* env var wired in the upload
	// step must map to a declared input and to an entry in wantInputs, so a
	// future input can't be forgotten in either place again.
	var uploadStep *actionStep
	for i, step := range action.Runs.Steps {
		if step.Name == "Upload via SFTP" {
			uploadStep = &action.Runs.Steps[i]
		}
	}
	if uploadStep == nil {
		t.Fatal("step \"Upload via SFTP\" is missing")
	}
	for envName := range uploadStep.Env {
		if !strings.HasPrefix(envName, "EASYSFTP_") {
			continue
		}
		inputName := strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(envName, "EASYSFTP_")), "_", "-")
		if _, ok := action.Inputs[inputName]; !ok {
			t.Errorf("env %s is wired but input %q is not declared", envName, inputName)
		}
		if !slices.Contains(wantInputs, inputName) {
			t.Errorf("env %s is wired but input %q is missing from wantInputs", envName, inputName)
		}
	}
	for _, name := range []string{"files-uploaded", "files-deleted", "files-skipped", "bytes-uploaded", "duration-ms"} {
		if _, ok := action.Outputs[name]; !ok {
			t.Errorf("output %q is missing", name)
		}
	}

	conditions := map[string]string{
		"Set up Go":      "steps.prepare.outputs.build-mode == 'source'",
		"Build easySFTP": "steps.prepare.outputs.build-mode == 'source'",
	}
	for _, step := range action.Runs.Steps {
		if want, ok := conditions[step.Name]; ok {
			if step.If != want {
				t.Errorf("%s condition = %q, want %q", step.Name, step.If, want)
			}
			delete(conditions, step.Name)
		}
	}
	for name := range conditions {
		t.Errorf("step %q is missing", name)
	}
}
