package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseUploads(t *testing.T) {
	t.Run("multiple pairs with comments and blank lines", func(t *testing.T) {
		got, err := ParseUploads("./dist/ => /var/www/html/\n\n# comment\nassets => /var/www/assets\r\n")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 pairs, got %d", len(got))
		}
		if got[0].Local != "./dist/" || got[0].Remote != "/var/www/html/" {
			t.Errorf("unexpected first pair: %+v", got[0])
		}
		if got[1].Local != "assets" || got[1].Remote != "/var/www/assets" {
			t.Errorf("unexpected second pair: %+v", got[1])
		}
	})

	t.Run("missing arrow", func(t *testing.T) {
		if _, err := ParseUploads("./dist/ -> /var/www/"); err == nil {
			t.Fatal("expected error for invalid mapping")
		}
	})

	t.Run("empty side", func(t *testing.T) {
		if _, err := ParseUploads("./dist/ =>"); err == nil {
			t.Fatal("expected error for empty remote path")
		}
	})
}

// setBaseEnv sets the minimal valid configuration.
func setBaseEnv(t *testing.T) {
	t.Setenv("EASYSFTP_SERVER", "sftp.example.com")
	t.Setenv("EASYSFTP_USERNAME", "deploy")
	t.Setenv("EASYSFTP_PASSWORD", "hunter2")
	t.Setenv("EASYSFTP_UPLOADS", "./dist/ => /www/")
	for _, name := range []string{"PORT", "PRIVATE_KEY", "PASSPHRASE", "HOST_KEY_FINGERPRINT", "KNOWN_HOSTS",
		"IGNORE", "IGNORE_FROM", "DELETE", "DRY_RUN", "CONCURRENCY", "SFTP_REQUEST_CONCURRENCY", "RETRIES", "TIMEOUT", "STALL_TIMEOUT",
		"SYNC_FAST_PATH", "CONFIG_FILE", "STRATEGY", "MAX_DELETES", "DIR_MODE", "FILE_MODE"} {
		t.Setenv("EASYSFTP_"+name, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	setBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 22 || cfg.Concurrency != 4 || cfg.SftpRequestConcurrency != 16 || cfg.Retries != 2 || cfg.DryRun || cfg.SyncFastPath {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if cfg.Timeout.Seconds() != 30 {
		t.Errorf("expected 30s default timeout, got %s", cfg.Timeout)
	}
}

func TestLoadValidation(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"missing server", map[string]string{"EASYSFTP_SERVER": ""}, "'server' is required"},
		{"missing username", map[string]string{"EASYSFTP_USERNAME": ""}, "'username' is required"},
		{"missing auth", map[string]string{"EASYSFTP_PASSWORD": ""}, "'password' or 'private-key'"},
		{"missing uploads", map[string]string{"EASYSFTP_UPLOADS": ""}, "'uploads' is required"},
		{"bad port", map[string]string{"EASYSFTP_PORT": "99999"}, "'port' must be between"},
		{"bad bool", map[string]string{"EASYSFTP_DRY_RUN": "yes-please"}, "invalid dry-run"},
		{"bad sync-fast-path bool", map[string]string{"EASYSFTP_SYNC_FAST_PATH": "yes-please"}, "invalid sync-fast-path"},
		{"bad max-deletes", map[string]string{"EASYSFTP_MAX_DELETES": "not-a-number"}, "invalid max-deletes"},
		{"bad sftp-request-concurrency", map[string]string{"EASYSFTP_SFTP_REQUEST_CONCURRENCY": "not-a-number"}, "invalid sftp-request-concurrency"},
		{"zero sftp-request-concurrency", map[string]string{"EASYSFTP_SFTP_REQUEST_CONCURRENCY": "0"}, "'sftp-request-concurrency' must be at least 1"},
		{"negative max-deletes", map[string]string{"EASYSFTP_MAX_DELETES": "-1"}, "guards.max_deletes must not be negative"},
		{"negative timeout", map[string]string{"EASYSFTP_TIMEOUT": "-5"}, "'timeout' must not be negative"},
		{"bad stall-timeout", map[string]string{"EASYSFTP_STALL_TIMEOUT": "soon"}, "invalid stall-timeout"},
		{"negative stall-timeout", map[string]string{"EASYSFTP_STALL_TIMEOUT": "-5"}, "'stall-timeout' must not be negative"},
		{"bad dir-mode", map[string]string{"EASYSFTP_DIR_MODE": "not-octal"}, "invalid dir-mode"},
		{"dir-mode out of range", map[string]string{"EASYSFTP_DIR_MODE": "1755"}, "invalid dir-mode"},
		{"bad file-mode", map[string]string{"EASYSFTP_FILE_MODE": "999"}, "invalid file-mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBaseEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoadZeroTimeoutDisablesTimeout(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("EASYSFTP_TIMEOUT", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 0 {
		t.Errorf("expected timeout 0 (disabled), got %s", cfg.Timeout)
	}
}

func TestLoadIgnoreFrom(t *testing.T) {
	setBaseEnv(t)
	ignoreFile := filepath.Join(t.TempDir(), ".sftpignore")
	if err := os.WriteFile(ignoreFile, []byte("*.log\n# comment\nnode_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EASYSFTP_IGNORE", "*.tmp")
	t.Setenv("EASYSFTP_IGNORE_FROM", ignoreFile)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"*.tmp", "*.log", "node_modules/"}
	if len(cfg.IgnoreLines) != len(want) {
		t.Fatalf("expected %v, got %v", want, cfg.IgnoreLines)
	}
	for i := range want {
		if cfg.IgnoreLines[i] != want[i] {
			t.Errorf("ignore line %d: expected %q, got %q", i, want[i], cfg.IgnoreLines[i])
		}
	}
}

func TestLoadMaxDeletes(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("EASYSFTP_MAX_DELETES", "200")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Guards.MaxDeletes != 200 {
		t.Errorf("expected MaxDeletes 200, got %d", cfg.Guards.MaxDeletes)
	}
}

func TestLoadMaxDeletesRejectedWithConfigFile(t *testing.T) {
	setBaseEnv(t)
	configFile := filepath.Join(t.TempDir(), "easysftp.yml")
	if err := os.WriteFile(configFile, []byte("version: 1\ntargets:\n  - local: ./dist/\n    remote: /www/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EASYSFTP_UPLOADS", "")
	t.Setenv("EASYSFTP_CONFIG_FILE", configFile)
	t.Setenv("EASYSFTP_MAX_DELETES", "5")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "do not also set") {
		t.Fatalf("expected rejection error, got %v", err)
	}
}

// TestLoadConfigFileWithActionDefaults simulates a real composite-action run
// that only sets connection settings and config-file: every EASYSFTP_* env
// var wired in action.yml's "Upload via SFTP" step is exported with its
// declared input default (empty when there is none), exactly as the runner
// does. Regression test for the max-deletes default "0" tripping the
// config-file mutual-exclusion check (#62), and a guard against any future
// input default reintroducing the same class of bug.
func TestLoadConfigFileWithActionDefaults(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var action struct {
		Inputs map[string]struct {
			Default string `yaml:"default"`
		} `yaml:"inputs"`
		Runs struct {
			Steps []struct {
				Name string            `yaml:"name"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(data, &action); err != nil {
		t.Fatalf("parse action.yml: %v", err)
	}

	var envBlock map[string]string
	for _, step := range action.Runs.Steps {
		if step.Name == "Upload via SFTP" {
			envBlock = step.Env
		}
	}
	if envBlock == nil {
		t.Fatal("action.yml has no 'Upload via SFTP' step")
	}

	inputRef := regexp.MustCompile(`^\$\{\{ inputs\.([a-z-]+) \}\}$`)
	for name, expr := range envBlock {
		if !strings.HasPrefix(name, envPrefix) {
			continue
		}
		m := inputRef.FindStringSubmatch(expr)
		if m == nil {
			t.Errorf("env %s = %q does not reference an input", name, expr)
			continue
		}
		input, ok := action.Inputs[m[1]]
		if !ok {
			t.Fatalf("env %s references undeclared input %q", name, m[1])
		}
		t.Setenv(name, input.Default)
	}

	// What a user's config-file workflow actually sets.
	t.Setenv("EASYSFTP_SERVER", "sftp.example.com")
	t.Setenv("EASYSFTP_USERNAME", "deploy")
	t.Setenv("EASYSFTP_PASSWORD", "hunter2")
	configFile := filepath.Join(t.TempDir(), "easysftp.yml")
	if err := os.WriteFile(configFile, []byte("version: 1\ntargets:\n  - local: ./dist/\n    remote: /www/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EASYSFTP_CONFIG_FILE", configFile)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("config-file run with action.yml input defaults was rejected: %v", err)
	}
	if len(cfg.Uploads) != 1 || cfg.Uploads[0].Local != "./dist/" {
		t.Errorf("unexpected uploads from config file: %+v", cfg.Uploads)
	}
}

func TestLoadDirFileMode(t *testing.T) {
	setBaseEnv(t)
	if cfg, err := Load(); err != nil {
		t.Fatal(err)
	} else if cfg.DirMode != nil || cfg.FileMode != nil {
		t.Errorf("expected unset dir-mode/file-mode by default, got %v / %v", cfg.DirMode, cfg.FileMode)
	}

	t.Setenv("EASYSFTP_DIR_MODE", "0755")
	t.Setenv("EASYSFTP_FILE_MODE", "644")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DirMode == nil || *cfg.DirMode != 0o755 {
		t.Errorf("expected DirMode 0755, got %v", cfg.DirMode)
	}
	if cfg.FileMode == nil || *cfg.FileMode != 0o644 {
		t.Errorf("expected FileMode 0644, got %v", cfg.FileMode)
	}
}

func TestLoadSyncFastPath(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("EASYSFTP_SYNC_FAST_PATH", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SyncFastPath {
		t.Error("expected SyncFastPath to be true")
	}
}
