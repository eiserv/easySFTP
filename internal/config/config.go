// Package config loads and validates the action configuration from
// environment variables set by action.yml, optionally from a YAML config file.
//
// v3 knows exactly two configuration modes and never mixes them:
//
//   - Inline mode: the connection and one deployment come from the action
//     inputs (host, username, source, target, ...).
//   - Config mode: the 'config' input points at a YAML file that holds every
//     non-secret setting (connection, deployments, safety, advanced tuning).
//     Only credentials (password, private-key, passphrase and their proxy-*
//     counterparts) and run-wide switches (dry-run, log-level) remain inputs.
package config

import (
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// Strategy selects how a deployment's remote directory is reconciled with the
// local files. Its user-facing input name in v3 is "mode".
type Strategy string

const (
	// StrategyOverlay uploads files on top of whatever is already there and
	// never deletes anything (the default).
	StrategyOverlay Strategy = "overlay"
	// StrategySync uploads new/changed files and deletes remote files that are
	// no longer present locally, tracked via a per-deployment manifest.
	StrategySync Strategy = "sync"
	// StrategyClean wipes the remote target directory, then uploads.
	StrategyClean Strategy = "clean"
)

func (s Strategy) valid() bool {
	switch s {
	case StrategyOverlay, StrategySync, StrategyClean:
		return true
	}
	return false
}

// LogLevel selects how chatty the run's log output is.
type LogLevel string

const (
	// LogNormal is the default: connection status, one summary per
	// deployment, warnings and errors. No per-file lines, so a 20,000-file
	// deploy produces a readable log.
	LogNormal LogLevel = "normal"
	// LogVerbose additionally logs one line per uploaded, deleted or skipped
	// file (v2's "normal").
	LogVerbose LogLevel = "verbose"
	// LogDebug additionally explains internal decisions, most importantly
	// which exclude pattern excluded which file during planning.
	LogDebug LogLevel = "debug"
)

// resolveLogLevel maps the log-level input to a concrete level.
func resolveLogLevel(input string) (LogLevel, error) {
	switch l := LogLevel(input); l {
	case "":
		return LogNormal, nil
	case LogNormal, LogVerbose, LogDebug:
		return l, nil
	}
	return "", fmt.Errorf("input 'log-level' must be normal, verbose or debug, got %q", input)
}

// UploadPair maps a local path to a remote path with its own strategy. In
// config mode every pair carries its deployment name; inline mode's single
// pair has an empty name.
type UploadPair struct {
	Name     string
	Local    string
	Remote   string
	Strategy Strategy
	Ignore   []string // deployment-specific exclude patterns, additive to the global ones
}

// Label names the pair in logs and summaries: the deployment name from the
// config file, or the mapping itself in inline mode.
func (p UploadPair) Label() string {
	if p.Name != "" {
		return p.Name
	}
	return fmt.Sprintf("%s => %s", p.Local, p.Remote)
}

// Proxy holds the connection settings of an optional jump host (bastion)
// through which the SFTP server is reached. In v3 the non-secret parts come
// exclusively from the config file (connection.proxy); the credentials come
// from the proxy-password / proxy-private-key / proxy-passphrase inputs.
type Proxy struct {
	Server              string
	Port                int
	Username            string
	Password            string
	PrivateKey          string
	Passphrase          string
	HostKeyFingerprints []string
	// KnownHosts holds raw OpenSSH known_hosts lines for the jump host, an
	// alternative to fingerprints.
	KnownHosts string
	// AllowAnyHostKey explicitly opts out of host key verification for this
	// hop. Without pinned keys and without this opt-in, the run fails.
	AllowAnyHostKey bool
}

// Guards holds the safety limits applied before any destructive operation.
type Guards struct {
	// MaxDeletes refuses a run that would delete more than this many files.
	// 0 means unlimited. The refusal to delete the remote root is always on.
	MaxDeletes int
}

// Config holds the fully parsed action configuration.
type Config struct {
	Server              string
	Port                int
	Username            string
	Password            string
	PrivateKey          string
	Passphrase          string
	HostKeyFingerprints []string
	// KnownHosts holds raw OpenSSH known_hosts lines (e.g. ssh-keyscan
	// output), an alternative to SHA256 fingerprints. When both are set, a
	// key matching either is accepted.
	KnownHosts string
	// AllowAnyHostKey explicitly opts out of host key verification. Without
	// pinned keys and without this opt-in, the run fails instead of talking
	// to an unverified server.
	AllowAnyHostKey bool

	// Proxy, if non-nil, routes the connection through a jump host.
	Proxy *Proxy

	Uploads     []UploadPair
	IgnoreLines []string
	Guards      Guards

	// ConfigPath is the path of the loaded YAML config file, or "" in inline
	// mode. It drives config-mode error message wording and the job summary.
	ConfigPath string

	// DirMode, if set, chmods every remote directory the run creates or
	// touches to this permission, overriding the server's umask default.
	// FileMode, if set, chmods every uploaded file to this permission instead
	// of mirroring the local file's mode bits. Both are best-effort: a server
	// that rejects the chmod produces one warning per run, not a failure.
	DirMode  *fs.FileMode
	FileMode *fs.FileMode

	DryRun                 bool
	Concurrency            int
	SftpRequestConcurrency int
	Retries                int
	Timeout                time.Duration
	SyncFastPath           bool

	// StallTimeout, if positive, aborts the run when active transfers make
	// no progress for this long, instead of hanging until the job-level
	// timeout. 0 (the default) disables the check.
	StallTimeout time.Duration

	// SkipUnchanged makes the overlay strategy skip a file whose remote
	// counterpart already exists with the same size (one stat per file).
	// Deliberately coarse; sync's content hashes are the exact alternative.
	SkipUnchanged bool

	// LogLevel controls per-file log output; see the LogLevel constants.
	// The zero value behaves like LogNormal.
	LogLevel LogLevel

	// ManifestName is the file name the sync strategy uses for its manifest
	// in each remote target. Defaults to DefaultManifestName; a custom
	// (unguessable) name mitigates the manifest being publicly downloadable
	// from web-root deployments. Always a bare file name, never a path.
	ManifestName string

	// PreserveTimes keeps each uploaded file's local modification time on the
	// server instead of "now". Best-effort: a server that rejects the request
	// produces one warning per run, not a failure.
	PreserveTimes bool
}

// LogPerFile reports whether per-file operation lines (upload/delete/skip)
// should be logged. A dry run always logs them: inspecting the plan is its
// whole point, regardless of log-level.
func (c *Config) LogPerFile() bool {
	return c.LogLevel == LogVerbose || c.LogLevel == LogDebug || c.DryRun
}

// Debug reports whether debug-only diagnostics (exclude decisions) should be
// logged.
func (c *Config) Debug() bool {
	return c.LogLevel == LogDebug
}

// HostKeyPinned reports whether the primary connection verifies the server's
// host key against pinned keys.
func (c *Config) HostKeyPinned() bool {
	return len(c.HostKeyFingerprints) > 0 || c.KnownHosts != ""
}

// DefaultManifestName is the sync manifest file name used when sync.manifest
// is not configured.
const DefaultManifestName = ".easysftp-manifest.json"

// SyncManifestName returns the effective sync manifest file name, falling
// back to the default for directly constructed configs.
func (c *Config) SyncManifestName() string {
	if c.ManifestName == "" {
		return DefaultManifestName
	}
	return c.ManifestName
}

const envPrefix = "EASYSFTP_"

// Run-wide defaults, applied in inline mode and overridable per config file.
const (
	defaultConcurrency        = 4
	defaultRequestConcurrency = 16
	defaultRetries            = 2
	defaultTimeoutSec         = 30
)

// removedInput maps a v2 input's environment variable to its migration hint.
// The inputs stay declared in action.yml (without defaults) so a workflow
// still passing them fails loudly here instead of being silently ignored by
// the runner.
type removedInput struct{ env, hint string }

var removedInputs = []removedInput{
	{"SERVER", "the 'server' input was renamed to 'host'"},
	{"HOST_KEY_FINGERPRINT", "the 'host-key-fingerprint' input was renamed to 'host-key'"},
	{"UPLOADS", "the 'uploads' input was removed; replace a single 'local => remote' mapping with the 'source' and 'target' inputs, and move multiple mappings into a config file ('config' input)"},
	{"CONFIG_FILE", "the 'config-file' input was renamed to 'config', and the file format changed (version: 3, named deployments, connection settings in the file)"},
	{"STRATEGY", "the 'strategy' input was renamed to 'mode'"},
	{"IGNORE", "the 'ignore' input was renamed to 'exclude'"},
	{"IGNORE_FROM", "the 'ignore-from' input was removed; put the patterns in the 'exclude' input or in the config file"},
	{"MAX_DELETES", "the 'max-deletes' input moved to 'safety.max_deletes' in the config file"},
	{"DELETE", "the 'delete' input was removed in v2 already; use 'mode: clean'"},
	{"CONCURRENCY", "the 'concurrency' input moved to 'advanced.concurrency' in the config file"},
	{"SFTP_REQUEST_CONCURRENCY", "the 'sftp-request-concurrency' input moved to 'advanced.request_concurrency' in the config file"},
	{"RETRIES", "the 'retries' input moved to 'advanced.retries' in the config file"},
	{"TIMEOUT", "the 'timeout' input moved to 'advanced.timeout' in the config file"},
	{"STALL_TIMEOUT", "the 'stall-timeout' input moved to 'advanced.stall_timeout' in the config file"},
	{"SYNC_FAST_PATH", "the 'sync-fast-path' input moved to 'sync.fast_path' in the config file"},
	{"MANIFEST_NAME", "the 'manifest-name' input moved to 'sync.manifest' in the config file"},
	{"SKIP_UNCHANGED", "the 'skip-unchanged' input moved to 'advanced.skip_unchanged' in the config file"},
	{"DIR_MODE", "the 'dir-mode' input moved to 'permissions.directories' in the config file"},
	{"FILE_MODE", "the 'file-mode' input moved to 'permissions.files' in the config file"},
	{"PRESERVE_TIMES", "the 'preserve-times' input moved to 'permissions.preserve_times' in the config file"},
	{"PROXY_SERVER", "the 'proxy-server' input moved to 'connection.proxy.host' in the config file"},
	{"PROXY_PORT", "the 'proxy-port' input moved to 'connection.proxy.port' in the config file"},
	{"PROXY_USERNAME", "the 'proxy-username' input moved to 'connection.proxy.username' in the config file"},
	{"PROXY_HOST_KEY_FINGERPRINT", "the 'proxy-host-key-fingerprint' input moved to 'connection.proxy.host_key' in the config file"},
	{"PROXY_KNOWN_HOSTS", "the 'proxy-known-hosts' input moved to 'connection.proxy.known_hosts' in the config file"},
}

// checkRemovedInputs fails the run with a migration hint when a v2 input is
// still set.
func checkRemovedInputs() error {
	for _, r := range removedInputs {
		if strings.TrimSpace(os.Getenv(envPrefix+r.env)) != "" {
			return fmt.Errorf("%s in easySFTP v3; see docs/migration-v3.md", r.hint)
		}
	}
	return nil
}

// inlineOnlyInputs are the non-secret inputs that must stay empty in config
// mode: in v3 every non-secret setting has exactly one home, so there is no
// override or precedence between the workflow and the config file.
var inlineOnlyInputs = []struct{ env, input string }{
	{"HOST", "host"},
	{"PORT", "port"},
	{"USERNAME", "username"},
	{"HOST_KEY", "host-key"},
	{"KNOWN_HOSTS", "known-hosts"},
	{"ALLOW_ANY_HOST_KEY", "allow-any-host-key"},
	{"SOURCE", "source"},
	{"TARGET", "target"},
	{"MODE", "mode"},
	{"EXCLUDE", "exclude"},
}

// Load reads the configuration from the environment and validates it.
func Load() (*Config, error) {
	if err := checkRemovedInputs(); err != nil {
		return nil, err
	}

	get := func(name string) string {
		return strings.TrimSpace(os.Getenv(envPrefix + name))
	}

	cfg := &Config{
		Password:               os.Getenv(envPrefix + "PASSWORD"),
		PrivateKey:             os.Getenv(envPrefix + "PRIVATE_KEY"),
		Passphrase:             os.Getenv(envPrefix + "PASSPHRASE"),
		Concurrency:            defaultConcurrency,
		SftpRequestConcurrency: defaultRequestConcurrency,
		Retries:                defaultRetries,
		Timeout:                defaultTimeoutSec * time.Second,
		ManifestName:           DefaultManifestName,
	}

	var err error
	if cfg.DryRun, err = parseBool(get("DRY_RUN"), false); err != nil {
		return nil, fmt.Errorf("invalid dry-run: %w", err)
	}
	if cfg.LogLevel, err = resolveLogLevel(get("LOG_LEVEL")); err != nil {
		return nil, err
	}

	proxyPassword := os.Getenv(envPrefix + "PROXY_PASSWORD")
	proxyPrivateKey := os.Getenv(envPrefix + "PROXY_PRIVATE_KEY")
	proxyPassphrase := os.Getenv(envPrefix + "PROXY_PASSPHRASE")
	haveProxyCreds := proxyPassword != "" || strings.TrimSpace(proxyPrivateKey) != "" || proxyPassphrase != ""

	if configPath := get("CONFIG"); configPath != "" {
		var set []string
		for _, in := range inlineOnlyInputs {
			if strings.TrimSpace(os.Getenv(envPrefix+in.env)) != "" {
				set = append(set, "'"+in.input+"'")
			}
		}
		if len(set) > 0 {
			return nil, fmt.Errorf("when 'config' is set, all non-secret settings come from the config file; "+
				"remove the %s input(s) from the workflow (only credentials, dry-run and log-level may be combined with a config file)",
				strings.Join(set, ", "))
		}
		if err := loadConfigFile(cfg, configPath); err != nil {
			return nil, err
		}
		cfg.ConfigPath = configPath
		if cfg.Proxy != nil {
			cfg.Proxy.Password = proxyPassword
			cfg.Proxy.PrivateKey = proxyPrivateKey
			cfg.Proxy.Passphrase = proxyPassphrase
		} else if haveProxyCreds {
			return nil, fmt.Errorf("proxy credential inputs are set but the config file defines no 'connection.proxy'; " +
				"add the proxy connection there or remove the proxy-* inputs")
		}
	} else {
		if haveProxyCreds {
			return nil, fmt.Errorf("proxy connections are configured in the config file in easySFTP v3 " +
				"(connection.proxy, with proxy-password / proxy-private-key as inputs); see docs/migration-v3.md")
		}
		if err := loadInline(cfg, get); err != nil {
			return nil, err
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadInline reads the single-deployment inline configuration from the
// action inputs.
func loadInline(cfg *Config, get func(string) string) error {
	cfg.Server = get("HOST")
	cfg.Username = get("USERNAME")
	cfg.HostKeyFingerprints = splitLines(os.Getenv(envPrefix + "HOST_KEY"))
	cfg.KnownHosts = strings.TrimSpace(os.Getenv(envPrefix + "KNOWN_HOSTS"))

	var err error
	if cfg.Port, err = parseInt(get("PORT"), 22); err != nil {
		return fmt.Errorf("invalid port: %w", err)
	}
	if cfg.AllowAnyHostKey, err = parseBool(get("ALLOW_ANY_HOST_KEY"), false); err != nil {
		return fmt.Errorf("invalid allow-any-host-key: %w", err)
	}

	strategy := StrategyOverlay
	if mode := get("MODE"); mode != "" {
		strategy = Strategy(mode)
		if !strategy.valid() {
			return fmt.Errorf("input 'mode' must be overlay, sync or clean, got %q", mode)
		}
	}

	source, target := get("SOURCE"), get("TARGET")
	if source == "" || target == "" {
		return fmt.Errorf("easySFTP could not determine what to deploy.\n\nAdd both:\n\n  source: dist\n  target: /var/www/html\n\nFor multiple deployments, use a config file:\n\n  config: .github/easysftp.yml")
	}
	cfg.Uploads = []UploadPair{{Local: source, Remote: target, Strategy: strategy}}
	cfg.IgnoreLines = splitLines(os.Getenv(envPrefix + "EXCLUDE"))
	return nil
}

func (c *Config) validate() error {
	// requiredIn phrases a missing required setting for the active mode.
	requiredIn := func(input, field string) error {
		if c.ConfigPath != "" {
			return fmt.Errorf("config %q: '%s' is required", c.ConfigPath, field)
		}
		return fmt.Errorf("input '%s' is required", input)
	}
	switch {
	case c.Server == "":
		return requiredIn("host", "connection.host")
	case c.Username == "":
		return requiredIn("username", "connection.username")
	case c.Password == "" && strings.TrimSpace(c.PrivateKey) == "":
		return fmt.Errorf("either input 'password' or 'private-key' is required")
	case len(c.Uploads) == 0:
		return fmt.Errorf("no deployments configured")
	case c.Port < 1 || c.Port > 65535:
		return fmt.Errorf("port must be between 1 and 65535, got %d", c.Port)
	case c.Concurrency < 1:
		return fmt.Errorf("advanced.concurrency must be at least 1, got %d", c.Concurrency)
	case c.SftpRequestConcurrency < 1:
		return fmt.Errorf("advanced.request_concurrency must be at least 1, got %d", c.SftpRequestConcurrency)
	case c.Retries < 0:
		return fmt.Errorf("advanced.retries must not be negative, got %d", c.Retries)
	case c.Timeout < 0:
		return fmt.Errorf("advanced.timeout must not be negative (use 0 to disable the timeout), got %d", int(c.Timeout/time.Second))
	case c.StallTimeout < 0:
		return fmt.Errorf("advanced.stall_timeout must not be negative (use 0 to disable the check), got %d", int(c.StallTimeout/time.Second))
	case c.Guards.MaxDeletes < 0:
		return fmt.Errorf("safety.max_deletes must not be negative, got %d", c.Guards.MaxDeletes)
	}
	if p := c.Proxy; p != nil {
		switch {
		case p.Username == "":
			return fmt.Errorf("config %q: 'connection.proxy.username' is required", c.ConfigPath)
		case p.Password == "" && strings.TrimSpace(p.PrivateKey) == "":
			return fmt.Errorf("either input 'proxy-password' or 'proxy-private-key' is required when the config file defines connection.proxy")
		case p.Port < 1 || p.Port > 65535:
			return fmt.Errorf("connection.proxy.port must be between 1 and 65535, got %d", p.Port)
		}
	}
	return nil
}

// splitLines splits s into trimmed, non-empty, non-comment lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func parseInt(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	return strconv.Atoi(s)
}

func parseBool(s string, def bool) (bool, error) {
	if s == "" {
		return def, nil
	}
	return strconv.ParseBool(s)
}

// parseManifestName validates the sync manifest file name. It must be a bare
// file name (the manifest always lives directly in each sync target), so path
// separators and the "."/".." components are rejected.
func parseManifestName(s string) (string, error) {
	if s == "" {
		return DefaultManifestName, nil
	}
	if strings.ContainsAny(s, "/\\") || s == "." || s == ".." {
		return "", fmt.Errorf("sync.manifest must be a bare file name (no path separators), got %q", s)
	}
	return s, nil
}

// parseMode parses an octal permission string like "755" or "0755". An empty
// string means "unset" (nil), keeping the current default behavior.
func parseMode(s, name string) (*fs.FileMode, error) {
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || v > 0o777 {
		return nil, fmt.Errorf("invalid %s: must be an octal permission like \"755\", got %q", name, s)
	}
	m := fs.FileMode(v)
	return &m, nil
}
