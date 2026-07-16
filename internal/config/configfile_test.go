package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes a YAML config file into a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "easysftp.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigFile(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("EASYSFTP_UPLOADS", "") // provided by the file instead
	t.Setenv("EASYSFTP_CONFIG_FILE", writeConfig(t, `
version: 1
strategy: sync
ignore:
  - "*.map"
guards:
  max_deletes: 5
targets:
  - local: ./dist/
    remote: /var/www/html/
    ignore:
      - node_modules/
  - local: ./docs/
    remote: /var/www/docs/
    strategy: clean
`))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Uploads) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(cfg.Uploads))
	}
	if cfg.Uploads[0].Strategy != StrategySync {
		t.Errorf("target 0 should inherit sync, got %q", cfg.Uploads[0].Strategy)
	}
	if cfg.Uploads[1].Strategy != StrategyClean {
		t.Errorf("target 1 should override to clean, got %q", cfg.Uploads[1].Strategy)
	}
	if cfg.Guards.MaxDeletes != 5 {
		t.Errorf("expected max_deletes 5, got %d", cfg.Guards.MaxDeletes)
	}
	if len(cfg.Uploads[0].Ignore) != 1 || cfg.Uploads[0].Ignore[0] != "node_modules/" {
		t.Errorf("unexpected target ignore: %v", cfg.Uploads[0].Ignore)
	}
	if len(cfg.IgnoreLines) != 1 || cfg.IgnoreLines[0] != "*.map" {
		t.Errorf("unexpected global ignore: %v", cfg.IgnoreLines)
	}
}

func TestConfigFileErrors(t *testing.T) {
	cases := []struct {
		name, body, wantErr string
	}{
		{"wrong version", "version: 2\ntargets:\n  - {local: a, remote: /b}", "'version' must be 1"},
		{"unknown key", "version: 1\nstartegy: sync\ntargets:\n  - {local: a, remote: /b}", "not valid"},
		{"bad strategy", "version: 1\nstrategy: mirror\ntargets:\n  - {local: a, remote: /b}", "overlay, sync or clean"},
		{"bad target strategy", "version: 1\ntargets:\n  - {local: a, remote: /b, strategy: nope}", "overlay, sync or clean"},
		{"no targets", "version: 1\ntargets: []", "at least one entry"},
		{"missing remote", "version: 1\ntargets:\n  - {local: a}", "both 'local' and 'remote'"},
		{"negative max_deletes", "version: 1\nguards: {max_deletes: -1}\ntargets:\n  - {local: a, remote: /b}", "max_deletes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBaseEnv(t)
			t.Setenv("EASYSFTP_UPLOADS", "")
			t.Setenv("EASYSFTP_CONFIG_FILE", writeConfig(t, tc.body))
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestConfigFileRejectsMixingWithInputs(t *testing.T) {
	setBaseEnv(t) // sets EASYSFTP_UPLOADS
	t.Setenv("EASYSFTP_CONFIG_FILE", writeConfig(t, "version: 1\ntargets:\n  - {local: a, remote: /b}"))
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "config-file") {
		t.Fatalf("expected mixing error, got %v", err)
	}
}

func TestStrategyInput(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("EASYSFTP_STRATEGY", "sync")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Uploads[0].Strategy != StrategySync {
		t.Errorf("expected sync strategy, got %q", cfg.Uploads[0].Strategy)
	}
}

func TestDeleteInputIsRejected(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("EASYSFTP_DELETE", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "strategy: clean") {
		t.Fatalf("expected the delete tombstone error, got %v", err)
	}
}

func TestDeleteFalseIsAccepted(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("EASYSFTP_DELETE", "false")
	if _, err := Load(); err != nil {
		t.Fatalf("delete=false must stay accepted (it is the action.yml default): %v", err)
	}
}
