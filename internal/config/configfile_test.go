package config

import (
	"strings"
	"testing"
)

// loadFile runs a config file's content through the loader against a fresh
// config with the run-wide defaults, returning the error.
func loadFile(t *testing.T, content string) (*Config, error) {
	t.Helper()
	cfg := &Config{
		Concurrency:            defaultConcurrency,
		SftpRequestConcurrency: defaultRequestConcurrency,
		Retries:                defaultRetries,
		ManifestName:           DefaultManifestName,
	}
	err := loadConfigFile(cfg, writeConfig(t, content))
	return cfg, err
}

func TestConfigFileUnknownKeySuggestions(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr []string
	}{
		{
			"typo in advanced",
			`version: 3
connection:
  host: h
  username: u
deployments:
  web:
    source: a
    target: /b
advanced:
  concurency: 8
`,
			[]string{`unknown option "concurency" at "advanced.concurency"`, `did you mean "concurrency"?`},
		},
		{
			"typo at top level",
			"version: 3\nconection:\n  host: h\n",
			[]string{`unknown option "conection"`, `did you mean "connection"?`},
		},
		{
			"typo in a deployment",
			`version: 3
connection:
  host: h
  username: u
deployments:
  website:
    source: a
    taget: /b
`,
			[]string{`unknown option "taget" at "deployments.website.taget"`, `did you mean "target"?`},
		},
		{
			"typo in proxy",
			`version: 3
connection:
  host: h
  proxy:
    hostt: b
`,
			[]string{`unknown option "hostt" at "connection.proxy.hostt"`, `did you mean "host"?`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFile(t, tc.content)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error should contain %q, got:\n%v", want, err)
				}
			}
		})
	}
	t.Run("no suggestion for a distant name", func(t *testing.T) {
		_, err := loadFile(t, "version: 3\nbanana: true\n")
		if err == nil || !strings.Contains(err.Error(), `unknown option "banana"`) {
			t.Fatalf("expected an unknown-option error, got %v", err)
		}
		if strings.Contains(err.Error(), "did you mean") {
			t.Fatalf("expected no suggestion for a distant key, got %v", err)
		}
	})
}

func TestConfigFileVersionValidation(t *testing.T) {
	t.Run("missing version", func(t *testing.T) {
		_, err := loadFile(t, "connection:\n  host: h\n")
		if err == nil || !strings.Contains(err.Error(), "'version' must be 3") {
			t.Fatalf("expected a version error, got %v", err)
		}
	})
	t.Run("v1 config is rejected with a migration hint", func(t *testing.T) {
		_, err := loadFile(t, "version: 1\nconnection:\n  host: h\n")
		if err == nil || !strings.Contains(err.Error(), "migration-v3") {
			t.Fatalf("expected a v1 migration hint, got %v", err)
		}
	})
}

func TestConfigFileV1TargetsListIsRejected(t *testing.T) {
	// A v1 file's 'targets' list fails the unknown-key check with a hint at
	// the closest v3 concept.
	_, err := loadFile(t, "version: 3\ntargets:\n  - local: ./dist/\n    remote: /www/\n")
	if err == nil || !strings.Contains(err.Error(), `unknown option "targets"`) {
		t.Fatalf("expected an unknown-option error for v1 'targets', got %v", err)
	}
}

func TestConfigFileDeploymentsListIsRejected(t *testing.T) {
	_, err := loadFile(t, `version: 3
connection:
  host: h
  username: u
deployments:
  - source: ./dist/
    target: /www/
`)
	if err == nil || !strings.Contains(err.Error(), "map of named deployments") {
		t.Fatalf("expected a named-deployments error for a list, got %v", err)
	}
}

func TestConfigFileValidation(t *testing.T) {
	base := "version: 3\nconnection:\n  host: h\n  username: u\n"
	deployment := "deployments:\n  web:\n    source: a\n    target: /b\n"
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"no deployments", base, "'deployments' must contain at least one named deployment"},
		{"missing target", base + "deployments:\n  web:\n    source: ./dist/\n", "both 'source' and 'target' are required"},
		{"bad mode", base + "deployments:\n  web:\n    source: a\n    target: /b\n    mode: mirror\n", "'mode' must be overlay, sync or clean"},
		{"bad default mode", base + "defaults:\n  mode: mirror\n" + deployment, "'defaults.mode' must be overlay, sync or clean"},
		{"duplicate deployment", base + "deployments:\n  web:\n    source: a\n    target: /b\n  web:\n    source: c\n    target: /d\n", "defined twice"},
		{"proxy without host", "version: 3\nconnection:\n  host: h\n  username: u\n  proxy:\n    username: j\n" + deployment, "'connection.proxy.host' is required"},
		{"bad manifest name", base + deployment + "sync:\n  manifest: sub/m.json\n", "sync.manifest must be a bare file name"},
		{"bad permissions", base + deployment + "permissions:\n  files: \"999\"\n", "invalid permissions.files"},
		{"bad concurrency value", base + deployment + "advanced:\n  concurrency: fast\n", "must be a number or \"auto\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFile(t, tc.content)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestConfigFileDeploymentOrderIsPreserved(t *testing.T) {
	cfg, err := loadFile(t, `version: 3
connection:
  host: h
  username: u
deployments:
  zeta:
    source: a
    target: /a
  alpha:
    source: b
    target: /b
  mid:
    source: c
    target: /c
`)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{cfg.Uploads[0].Name, cfg.Uploads[1].Name, cfg.Uploads[2].Name}
	want := []string{"zeta", "alpha", "mid"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("deployment order not preserved: got %v, want %v", got, want)
		}
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"concurency", "concurrency", 1},
		{"taget", "target", 1},
		{"banana", "version", 7},
	}
	for _, tc := range cases {
		if got := editDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
