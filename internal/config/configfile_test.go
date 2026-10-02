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

// TestConfigFileAutoCache: advanced.auto_cache is a path and nothing else, and
// leaving it out keeps the cache off, which is what every run did before it
// existed (issue #212).
func TestConfigFileAutoCache(t *testing.T) {
	const base = `version: 3
connection:
  host: h
  username: u
  allow_any_host_key: true
deployments:
  web:
    source: a
    target: /b
`
	cfg, err := loadFile(t, base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoCachePath != "" {
		t.Errorf("a file that does not mention the cache enabled it at %q", cfg.AutoCachePath)
	}

	cfg, err = loadFile(t, base+"advanced:\n  auto_cache: \"  .easysftp/auto.json  \"\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoCachePath != ".easysftp/auto.json" {
		t.Errorf("auto_cache = %q, want the trimmed path", cfg.AutoCachePath)
	}

	if _, err := loadFile(t, base+"advanced:\n  auto_cach: x\n"); err == nil ||
		!strings.Contains(err.Error(), `did you mean "auto_cache"?`) {
		t.Errorf("a typo next to auto_cache was not suggested against: %v", err)
	}
}

// TestConfigFileParserGaps covers the six config-file parser gaps from
// issue #286: a valid YAML idiom failing as an unknown option, a natural
// spelling failing with the decoder's raw error, and four silent no-ops.
func TestConfigFileParserGaps(t *testing.T) {
	const base = `version: 3
connection:
  host: h
  username: u
`
	t.Run("merge keys load", func(t *testing.T) {
		cfg, err := loadFile(t, base+`deployments:
  site: &base
    source: dist
    target: /var/www/site
  staging:
    <<: *base
    target: /var/www/staging
`)
		if err != nil {
			t.Fatalf("a merge key was rejected as an option: %v", err)
		}
		if len(cfg.Uploads) != 2 {
			t.Fatalf("expected two deployments, got %d", len(cfg.Uploads))
		}
		if cfg.Uploads[0].Local != "dist" || cfg.Uploads[1].Local != "dist" {
			t.Fatalf("merged source did not resolve through the anchor: %+v", cfg.Uploads)
		}
		if cfg.Uploads[1].Remote != "/var/www/staging" {
			t.Fatalf("the overriding target did not win: %q", cfg.Uploads[1].Remote)
		}
	})
	t.Run("merge sequence of aliases loads", func(t *testing.T) {
		_, err := loadFile(t, base+`defaults: &d
  mode: overlay
deployments:
  site: &s
    source: dist
    target: /a
  staging:
    <<: [*d, *s]
    target: /b
`)
		if err != nil {
			t.Fatalf("a merge sequence was rejected: %v", err)
		}
	})
	t.Run("a typo inside an inline merge mapping is caught", func(t *testing.T) {
		_, err := loadFile(t, base+`deployments:
  web:
    <<: {source: dist, taget: /var/www/html}
    mode: overlay
`)
		if err == nil || !strings.Contains(err.Error(), `unknown option "taget"`) {
			t.Fatalf("expected the typo inside the inline merge value to be caught, got %v", err)
		}
	})
	t.Run("a trailing document separator still loads", func(t *testing.T) {
		_, err := loadFile(t, base+`deployments:
  web:
    source: dist
    target: /b
---
`)
		if err != nil {
			t.Fatalf("a lone trailing --- must not fail the file: %v", err)
		}
	})
	t.Run("a second document with content is still refused", func(t *testing.T) {
		_, err := loadFile(t, base+`deployments:
  web:
    source: a
    target: /b
---
version: 3
`)
		if err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
			t.Fatalf("expected the second document to be refused, got %v", err)
		}
	})
	t.Run("a shared anchor referenced by two siblings is not a cycle", func(t *testing.T) {
		// The diamond: one base, several deployments overriding a field.
		// This is the pattern docs/easysftp.example.yml demonstrates;
		// the cycle guard must not reject it.
		cfg, err := loadFile(t, base+`deployments:
  base: &base
    source: dist
    target: /var/www/base
  website:
    <<: *base
    target: /var/www/html
  staging:
    <<: *base
    target: /var/www/staging
`)
		if err != nil {
			t.Fatalf("a shared anchor across sibling deployments was rejected as cyclic: %v", err)
		}
		if len(cfg.Uploads) != 3 {
			t.Fatalf("expected three deployments, got %d", len(cfg.Uploads))
		}
		for i, want := range []string{"/var/www/base", "/var/www/html", "/var/www/staging"} {
			if cfg.Uploads[i].Remote != want {
				t.Errorf("deployment %d target = %q, want %q", i, cfg.Uploads[i].Remote, want)
			}
		}
	})
	t.Run("cyclic merge key fails cleanly instead of overflowing", func(t *testing.T) {
		_, err := loadFile(t, base+`deployments:
  site: &site
    <<: *site
    source: dist
    target: /a
`)
		if err == nil {
			t.Fatal("a cyclic merge key was accepted")
		}
		if !strings.Contains(err.Error(), "cyclic merge") {
			t.Fatalf("expected a cyclic-merge error, got %v", err)
		}
	})
	// Direct self-merge through a mapping value, the shape the maintainer
	// flagged: a: &a with <<: *a inside it.
	t.Run("self-referential merge key fails cleanly instead of overflowing", func(t *testing.T) {
		_, err := loadFile(t, base+`deployments:
  site: &site
    <<: *site
    source: dist
    target: /a
  staging:
    <<: *site
    target: /b
`)
		if err == nil {
			t.Fatal("a self-referential merge key was accepted")
		}
		if !strings.Contains(err.Error(), "cyclic merge") {
			t.Fatalf("expected a cyclic-merge error, got %v", err)
		}
	})
	t.Run("a typo inside an anchored mapping is still caught", func(t *testing.T) {
		_, err := loadFile(t, base+`deployments:
  site: &base
    source: dist
    target: /a
    taget: /typo
  staging:
    <<: *base
`)
		if err == nil || !strings.Contains(err.Error(), `unknown option "taget"`) {
			t.Fatalf("expected the typo in the anchor to be caught, got %v", err)
		}
	})
	t.Run("host_key list loads like a block scalar", func(t *testing.T) {
		cfg, err := loadFile(t, `version: 3
connection:
  host: h
  username: u
  host_key:
    - SHA256:first
    - SHA256:second
deployments:
  web:
    source: a
    target: /b
`)
		if err != nil {
			t.Fatalf("a host_key list failed to load: %v", err)
		}
		if len(cfg.HostKeyFingerprints) != 2 {
			t.Fatalf("expected both fingerprints, got %v", cfg.HostKeyFingerprints)
		}
		if cfg.HostKeyFingerprints[0] != "SHA256:first" || cfg.HostKeyFingerprints[1] != "SHA256:second" {
			t.Fatalf("fingerprints not preserved in order: %v", cfg.HostKeyFingerprints)
		}
	})
	t.Run("proxy host_key list loads too", func(t *testing.T) {
		cfg, err := loadFile(t, `version: 3
connection:
  host: h
  username: u
  proxy:
    host: jump
    host_key:
      - SHA256:jump
deployments:
  web:
    source: a
    target: /b
`)
		if err != nil {
			t.Fatalf("a proxy host_key list failed to load: %v", err)
		}
		if len(cfg.Proxy.HostKeyFingerprints) != 1 || cfg.Proxy.HostKeyFingerprints[0] != "SHA256:jump" {
			t.Fatalf("proxy fingerprint not preserved: %v", cfg.Proxy.HostKeyFingerprints)
		}
	})
	t.Run("host_key mapping fails clearly", func(t *testing.T) {
		_, err := loadFile(t, base+`  host_key:
    first: SHA256:x
    second: SHA256:y
deployments:
  web:
    source: a
    target: /b
`)
		if err == nil || !strings.Contains(err.Error(), "host_key must be a fingerprint or a list of fingerprints") {
			t.Fatalf("expected a clear host_key error, got %v", err)
		}
	})
	t.Run("fractional concurrency is rejected", func(t *testing.T) {
		_, err := loadFile(t, base+`deployments:
  web:
    source: a
    target: /b
advanced:
  concurrency: 2.5
`)
		if err == nil || !strings.Contains(err.Error(), `must be a number or "auto", got "2.5"`) {
			t.Fatalf("a fractional concurrency was silently truncated: %v", err)
		}
	})
	t.Run("empty deployment name is rejected", func(t *testing.T) {
		_, err := loadFile(t, base+`deployments:
  "":
    source: a
    target: /b
`)
		if err == nil || !strings.Contains(err.Error(), "deployment names must not be empty") {
			t.Fatalf("an empty deployment name was accepted: %v", err)
		}
	})
	t.Run("second YAML document is rejected with its line", func(t *testing.T) {
		_, err := loadFile(t, base+`deployments:
  web:
    source: a
    target: /b
---
version: 3
`)
		if err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
			t.Fatalf("a second document was silently dropped: %v", err)
		}
	})
	t.Run("timeout above a day is rejected", func(t *testing.T) {
		_, err := loadFile(t, base+`deployments:
  web:
    source: a
    target: /b
advanced:
  timeout: 99999999999
`)
		if err == nil || !strings.Contains(err.Error(), "'advanced.timeout' must be at most") {
			t.Fatalf("a giant timeout was silently accepted: %v", err)
		}
	})
	t.Run("stall_timeout above a day is rejected", func(t *testing.T) {
		_, err := loadFile(t, base+`deployments:
  web:
    source: a
    target: /b
advanced:
  stall_timeout: 86401
`)
		if err == nil || !strings.Contains(err.Error(), "'advanced.stall_timeout' must be at most") {
			t.Fatalf("a giant stall_timeout was silently accepted: %v", err)
		}
	})
	t.Run("a day is still allowed", func(t *testing.T) {
		_, err := loadFile(t, base+`deployments:
  web:
    source: a
    target: /b
advanced:
  timeout: 86400
`)
		if err != nil {
			t.Fatalf("a one-day timeout should load, got %v", err)
		}
	})
}
