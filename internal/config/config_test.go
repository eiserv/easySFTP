package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// v3EnvVars is every EASYSFTP_* variable action.yml wires into the upload
// step: live inputs and the removed-input tombstones. setBaseEnv clears all
// of them so config tests stay hermetic when the ambient environment sets
// EASYSFTP_* variables.
var v3EnvVars = []string{
	// live v3 inputs
	"HOST", "PORT", "USERNAME", "PASSWORD", "PRIVATE_KEY", "PASSPHRASE",
	"HOST_KEY", "KNOWN_HOSTS", "ALLOW_ANY_HOST_KEY",
	"SOURCE", "TARGET", "MODE", "EXCLUDE", "CONFIG",
	"DRY_RUN", "LOG_LEVEL",
	"PROXY_PASSWORD", "PROXY_PRIVATE_KEY", "PROXY_PASSPHRASE",
	// removed-input tombstones
	"SERVER", "HOST_KEY_FINGERPRINT", "UPLOADS", "CONFIG_FILE", "STRATEGY",
	"IGNORE", "IGNORE_FROM", "MAX_DELETES", "DELETE",
	"CONCURRENCY", "SFTP_REQUEST_CONCURRENCY", "RETRIES", "TIMEOUT", "STALL_TIMEOUT",
	"SYNC_FAST_PATH", "MANIFEST_NAME", "SKIP_UNCHANGED",
	"DIR_MODE", "FILE_MODE", "PRESERVE_TIMES",
	"PROXY_SERVER", "PROXY_PORT", "PROXY_USERNAME",
	"PROXY_HOST_KEY_FINGERPRINT", "PROXY_KNOWN_HOSTS",
}

// setBaseEnv sets the minimal valid inline configuration.
func setBaseEnv(t *testing.T) {
	for _, name := range v3EnvVars {
		t.Setenv("EASYSFTP_"+name, "")
	}
	t.Setenv("EASYSFTP_HOST", "sftp.example.com")
	t.Setenv("EASYSFTP_USERNAME", "deploy")
	t.Setenv("EASYSFTP_PASSWORD", "hunter2")
	t.Setenv("EASYSFTP_SOURCE", "./dist/")
	t.Setenv("EASYSFTP_TARGET", "/www/")
	t.Setenv("EASYSFTP_HOST_KEY", "SHA256:abc")
}

// writeConfig writes a config file into a temp dir and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "easysftp.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalConfigFile = `version: 3
connection:
  host: sftp.example.com
  username: deploy
  host_key: SHA256:abc
deployments:
  website:
    source: ./dist/
    target: /www/
`

// setConfigModeEnv sets the minimal valid config-mode environment.
func setConfigModeEnv(t *testing.T, fileContent string) string {
	t.Helper()
	for _, name := range v3EnvVars {
		t.Setenv("EASYSFTP_"+name, "")
	}
	t.Setenv("EASYSFTP_PASSWORD", "hunter2")
	path := writeConfig(t, fileContent)
	t.Setenv("EASYSFTP_CONFIG", path)
	return path
}

func TestLoadInlineDefaults(t *testing.T) {
	setBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 22 || cfg.Concurrency != 4 || cfg.SftpRequestConcurrency != 16 || cfg.Retries != 2 ||
		cfg.DryRun || cfg.SyncFastPath || cfg.SkipUnchanged || cfg.AllowAnyHostKey || cfg.LogLevel != LogNormal {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if cfg.Timeout != 30*time.Second {
		t.Errorf("expected 30s default timeout, got %s", cfg.Timeout)
	}
	if cfg.Guards.MaxDeletes != 0 {
		t.Errorf("expected unlimited max deletes by default, got %d", cfg.Guards.MaxDeletes)
	}
	if cfg.ManifestName != DefaultManifestName {
		t.Errorf("expected default manifest name %q, got %q", DefaultManifestName, cfg.ManifestName)
	}
	if len(cfg.Uploads) != 1 {
		t.Fatalf("expected one deployment, got %+v", cfg.Uploads)
	}
	up := cfg.Uploads[0]
	if up.Name != "" || up.Local != "./dist/" || up.Remote != "/www/" || up.Strategy != StrategyOverlay {
		t.Errorf("unexpected inline deployment: %+v", up)
	}
	if cfg.ConfigPath != "" {
		t.Errorf("expected empty ConfigPath in inline mode, got %q", cfg.ConfigPath)
	}
}

func TestLoadInlineModeAndExclude(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("EASYSFTP_MODE", "sync")
	t.Setenv("EASYSFTP_EXCLUDE", "*.map\n# comment\nnode_modules/\n")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Uploads[0].Strategy != StrategySync {
		t.Errorf("expected sync mode, got %q", cfg.Uploads[0].Strategy)
	}
	want := []string{"*.map", "node_modules/"}
	if len(cfg.IgnoreLines) != len(want) || cfg.IgnoreLines[0] != want[0] || cfg.IgnoreLines[1] != want[1] {
		t.Errorf("expected exclude lines %v, got %v", want, cfg.IgnoreLines)
	}
}

func TestLoadInlineValidation(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"missing host", map[string]string{"EASYSFTP_HOST": ""}, "'host' is required"},
		{"missing username", map[string]string{"EASYSFTP_USERNAME": ""}, "'username' is required"},
		{"missing auth", map[string]string{"EASYSFTP_PASSWORD": ""}, "'password' or 'private-key'"},
		{"missing source", map[string]string{"EASYSFTP_SOURCE": ""}, "could not determine what to deploy"},
		{"missing target", map[string]string{"EASYSFTP_TARGET": ""}, "could not determine what to deploy"},
		{"bad port", map[string]string{"EASYSFTP_PORT": "99999"}, "port must be between"},
		{"bad mode", map[string]string{"EASYSFTP_MODE": "mirror"}, "'mode' must be overlay, sync or clean"},
		{"bad dry-run", map[string]string{"EASYSFTP_DRY_RUN": "yes-please"}, "invalid dry-run"},
		{"bad allow-any-host-key", map[string]string{"EASYSFTP_ALLOW_ANY_HOST_KEY": "maybe"}, "invalid allow-any-host-key"},
		{"bad log-level", map[string]string{"EASYSFTP_LOG_LEVEL": "quiet"}, "'log-level' must be normal, verbose or debug"},
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

func TestLoadMissingSourceErrorExplainsTheFix(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("EASYSFTP_SOURCE", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"source: dist", "target: /var/www/html", "config: .github/easysftp.yml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got:\n%v", want, err)
		}
	}
}

func TestLoadRemovedInputsFailWithMigrationHint(t *testing.T) {
	cases := []struct {
		env      string
		wantHint string
	}{
		{"SERVER", "'server' input was renamed to 'host'"},
		{"HOST_KEY_FINGERPRINT", "renamed to 'host-key'"},
		{"UPLOADS", "'source' and 'target'"},
		{"CONFIG_FILE", "renamed to 'config'"},
		{"STRATEGY", "renamed to 'mode'"},
		{"IGNORE", "renamed to 'exclude'"},
		{"IGNORE_FROM", "'ignore-from' input was removed"},
		{"MAX_DELETES", "safety.max_deletes"},
		{"DELETE", "use 'mode: clean'"},
		{"CONCURRENCY", "advanced.concurrency"},
		{"SFTP_REQUEST_CONCURRENCY", "advanced.request_concurrency"},
		{"RETRIES", "advanced.retries"},
		{"TIMEOUT", "advanced.timeout"},
		{"STALL_TIMEOUT", "advanced.stall_timeout"},
		{"SYNC_FAST_PATH", "sync.fast_path"},
		{"MANIFEST_NAME", "sync.manifest"},
		{"SKIP_UNCHANGED", "advanced.skip_unchanged"},
		{"DIR_MODE", "permissions.directories"},
		{"FILE_MODE", "permissions.files"},
		{"PRESERVE_TIMES", "permissions.preserve_times"},
		{"PROXY_SERVER", "connection.proxy.host"},
		{"PROXY_PORT", "connection.proxy.port"},
		{"PROXY_USERNAME", "connection.proxy.username"},
		{"PROXY_HOST_KEY_FINGERPRINT", "connection.proxy.host_key"},
		{"PROXY_KNOWN_HOSTS", "connection.proxy.known_hosts"},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			setBaseEnv(t)
			t.Setenv("EASYSFTP_"+tc.env, "something")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.wantHint) {
				t.Fatalf("expected migration hint containing %q, got %v", tc.wantHint, err)
			}
			if !strings.Contains(err.Error(), "docs/migration-v3.md") {
				t.Errorf("migration error should point at docs/migration-v3.md, got %v", err)
			}
		})
	}
}

func TestLoadAllowAnyHostKey(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("EASYSFTP_HOST_KEY", "")
	t.Setenv("EASYSFTP_ALLOW_ANY_HOST_KEY", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowAnyHostKey {
		t.Error("expected AllowAnyHostKey to be true")
	}
	if cfg.HostKeyPinned() {
		t.Error("expected no pinned host keys")
	}
}

func TestLoadInlineRejectsProxyCredentials(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("EASYSFTP_PROXY_PASSWORD", "pw")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "connection.proxy") {
		t.Fatalf("expected a proxy-requires-config error, got %v", err)
	}
}

func TestLoadConfigMode(t *testing.T) {
	path := setConfigModeEnv(t, `version: 3
connection:
  host: sftp.example.com
  port: 2222
  username: deploy
  host_key: |
    SHA256:abc
    SHA256:def
defaults:
  mode: sync
  exclude:
    - "*.map"
deployments:
  website:
    source: ./dist/
    target: /var/www/html/
  documentation:
    source: ./docs/
    target: /var/www/docs/
    mode: clean
    exclude:
      - "*.tmp"
safety:
  max_deletes: 500
advanced:
  retries: 4
  timeout: 60
  stall_timeout: 120
  concurrency: 8
  request_concurrency: 4
  skip_unchanged: true
permissions:
  files: "0644"
  directories: "0755"
  preserve_times: true
sync:
  fast_path: true
  manifest: .deploy-manifest.json
`)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "sftp.example.com" || cfg.Port != 2222 || cfg.Username != "deploy" {
		t.Errorf("unexpected connection: %+v", cfg)
	}
	if cfg.Password != "hunter2" {
		t.Errorf("expected the password credential input to apply, got %q", cfg.Password)
	}
	if len(cfg.HostKeyFingerprints) != 2 {
		t.Errorf("expected 2 fingerprints, got %v", cfg.HostKeyFingerprints)
	}
	if cfg.ConfigPath != path {
		t.Errorf("expected ConfigPath %q, got %q", path, cfg.ConfigPath)
	}
	if len(cfg.Uploads) != 2 {
		t.Fatalf("expected 2 deployments, got %+v", cfg.Uploads)
	}
	website, docs := cfg.Uploads[0], cfg.Uploads[1]
	if website.Name != "website" || website.Local != "./dist/" || website.Remote != "/var/www/html/" || website.Strategy != StrategySync {
		t.Errorf("unexpected website deployment: %+v", website)
	}
	if docs.Name != "documentation" || docs.Strategy != StrategyClean || len(docs.Ignore) != 1 {
		t.Errorf("unexpected documentation deployment: %+v", docs)
	}
	if len(cfg.IgnoreLines) != 1 || cfg.IgnoreLines[0] != "*.map" {
		t.Errorf("unexpected global excludes: %v", cfg.IgnoreLines)
	}
	if cfg.Guards.MaxDeletes != 500 {
		t.Errorf("expected max_deletes 500, got %d", cfg.Guards.MaxDeletes)
	}
	if cfg.Retries != 4 || cfg.Timeout != 60*time.Second || cfg.StallTimeout != 120*time.Second ||
		cfg.Concurrency != 8 || cfg.SftpRequestConcurrency != 4 || !cfg.SkipUnchanged {
		t.Errorf("unexpected advanced settings: %+v", cfg)
	}
	if cfg.FileMode == nil || *cfg.FileMode != 0o644 || cfg.DirMode == nil || *cfg.DirMode != 0o755 || !cfg.PreserveTimes {
		t.Errorf("unexpected permissions: %v %v %v", cfg.FileMode, cfg.DirMode, cfg.PreserveTimes)
	}
	if !cfg.SyncFastPath || cfg.ManifestName != ".deploy-manifest.json" {
		t.Errorf("unexpected sync settings: %v %q", cfg.SyncFastPath, cfg.ManifestName)
	}
}

func TestLoadConfigModeDefaults(t *testing.T) {
	setConfigModeEnv(t, minimalConfigFile)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 22 || cfg.Concurrency != 4 || cfg.SftpRequestConcurrency != 16 || cfg.Retries != 2 ||
		cfg.Timeout != 30*time.Second || cfg.StallTimeout != 0 || cfg.Guards.MaxDeletes != 0 {
		t.Errorf("unexpected config-mode defaults: %+v", cfg)
	}
	if cfg.Uploads[0].Strategy != StrategyOverlay {
		t.Errorf("expected overlay default mode, got %q", cfg.Uploads[0].Strategy)
	}
}

func TestLoadConfigModeExplicitZeros(t *testing.T) {
	setConfigModeEnv(t, `version: 3
connection:
  host: sftp.example.com
  username: deploy
  host_key: SHA256:abc
deployments:
  website:
    source: ./dist/
    target: /www/
advanced:
  retries: 0
  timeout: 0
`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retries != 0 || cfg.Timeout != 0 {
		t.Errorf("explicit zeros must win over the defaults, got retries=%d timeout=%s", cfg.Retries, cfg.Timeout)
	}
}

func TestLoadConfigModeConcurrencyAuto(t *testing.T) {
	setConfigModeEnv(t, `version: 3
connection:
  host: sftp.example.com
  username: deploy
  host_key: SHA256:abc
deployments:
  website:
    source: ./dist/
    target: /www/
advanced:
  concurrency: auto
`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency != 4 {
		t.Errorf("expected 'auto' to resolve to the default concurrency, got %d", cfg.Concurrency)
	}
}

func TestLoadConfigModeRejectsInlineInputs(t *testing.T) {
	for _, in := range []struct{ env, name string }{
		{"EASYSFTP_HOST", "host"},
		{"EASYSFTP_SOURCE", "source"},
		{"EASYSFTP_TARGET", "target"},
		{"EASYSFTP_MODE", "mode"},
		{"EASYSFTP_EXCLUDE", "exclude"},
		{"EASYSFTP_HOST_KEY", "host-key"},
		{"EASYSFTP_KNOWN_HOSTS", "known-hosts"},
		{"EASYSFTP_ALLOW_ANY_HOST_KEY", "allow-any-host-key"},
		{"EASYSFTP_USERNAME", "username"},
		{"EASYSFTP_PORT", "port"},
	} {
		t.Run(in.name, func(t *testing.T) {
			setConfigModeEnv(t, minimalConfigFile)
			t.Setenv(in.env, "something")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "'"+in.name+"'") {
				t.Fatalf("expected a mixed-mode rejection naming %q, got %v", in.name, err)
			}
		})
	}
}

func TestLoadConfigModeAllowsCredentialsAndRunSwitches(t *testing.T) {
	setConfigModeEnv(t, minimalConfigFile)
	t.Setenv("EASYSFTP_PRIVATE_KEY", "-----BEGIN OPENSSH PRIVATE KEY-----")
	t.Setenv("EASYSFTP_PASSPHRASE", "secret")
	t.Setenv("EASYSFTP_DRY_RUN", "true")
	t.Setenv("EASYSFTP_LOG_LEVEL", "verbose")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DryRun || cfg.LogLevel != LogVerbose || cfg.Passphrase != "secret" {
		t.Errorf("credentials and run switches must combine with config mode: %+v", cfg)
	}
}

func TestLoadConfigModeProxy(t *testing.T) {
	setConfigModeEnv(t, `version: 3
connection:
  host: sftp.internal.example.com
  username: deploy
  host_key: SHA256:abc
  proxy:
    host: bastion.example.com
    username: jumper
    host_key: SHA256:jump
deployments:
  website:
    source: ./dist/
    target: /www/
`)
	t.Setenv("EASYSFTP_PROXY_PASSWORD", "jump-pw")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Proxy
	if p == nil {
		t.Fatal("expected a proxy config")
	}
	if p.Server != "bastion.example.com" || p.Port != 22 || p.Username != "jumper" || p.Password != "jump-pw" {
		t.Errorf("unexpected proxy config: %+v", p)
	}
	if len(p.HostKeyFingerprints) != 1 || p.HostKeyFingerprints[0] != "SHA256:jump" {
		t.Errorf("unexpected proxy fingerprints: %v", p.HostKeyFingerprints)
	}
}

func TestLoadConfigModeProxyWithoutCredentialsFails(t *testing.T) {
	setConfigModeEnv(t, `version: 3
connection:
  host: sftp.internal.example.com
  username: deploy
  host_key: SHA256:abc
  proxy:
    host: bastion.example.com
    username: jumper
    host_key: SHA256:jump
deployments:
  website:
    source: ./dist/
    target: /www/
`)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "'proxy-password' or 'proxy-private-key'") {
		t.Fatalf("expected a missing proxy credential error, got %v", err)
	}
}

func TestLoadConfigModeProxyCredentialsWithoutProxyFails(t *testing.T) {
	setConfigModeEnv(t, minimalConfigFile)
	t.Setenv("EASYSFTP_PROXY_PASSWORD", "pw")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "no 'connection.proxy'") {
		t.Fatalf("expected a proxy-credentials-without-proxy error, got %v", err)
	}
}

// TestLoadConfigModeWithActionDefaults simulates a real composite-action run
// that only sets credentials and config: every EASYSFTP_* env var wired in
// action.yml's "Upload via SFTP" step is exported with its declared input
// default (empty when there is none), exactly as the runner does. Regression
// test for the input-default class of bug (#62): a declared default on a
// mode-specific input would wrongly trip the mixed-mode check on every
// config-mode run.
func TestLoadConfigModeWithActionDefaults(t *testing.T) {
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

	// What a user's config-mode workflow actually sets.
	t.Setenv("EASYSFTP_PASSWORD", "hunter2")
	t.Setenv("EASYSFTP_CONFIG", writeConfig(t, minimalConfigFile))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("config-mode run with action.yml input defaults was rejected: %v", err)
	}
	if len(cfg.Uploads) != 1 || cfg.Uploads[0].Name != "website" {
		t.Errorf("unexpected deployments from config file: %+v", cfg.Uploads)
	}
}
