package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	for _, name := range []string{"PORT", "PRIVATE_KEY", "PASSPHRASE", "HOST_KEY_FINGERPRINT",
		"IGNORE", "IGNORE_FROM", "DELETE", "DRY_RUN", "CONCURRENCY", "RETRIES", "TIMEOUT",
		"CONFIG_FILE", "STRATEGY"} {
		t.Setenv("EASYSFTP_"+name, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	setBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 22 || cfg.Concurrency != 4 || cfg.Retries != 2 || cfg.DryRun {
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
