// Package config loads and validates the action configuration from
// environment variables set by action.yml, optionally from a YAML config file.
package config

import (
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// Strategy selects how a target's remote directory is reconciled with the
// local files.
type Strategy string

const (
	// StrategyOverlay uploads files on top of whatever is already there and
	// never deletes anything (the default, backwards-compatible behavior).
	StrategyOverlay Strategy = "overlay"
	// StrategySync uploads new/changed files and deletes remote files that are
	// no longer present locally, tracked via a per-target manifest.
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

// UploadPair maps a local path to a remote path with its own strategy.
type UploadPair struct {
	Local    string
	Remote   string
	Strategy Strategy
	Ignore   []string // target-specific ignore patterns, additive to the global ones
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

	Uploads     []UploadPair
	IgnoreLines []string
	Guards      Guards

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
}

const envPrefix = "EASYSFTP_"

// Load reads the configuration from the environment and validates it.
func Load() (*Config, error) {
	get := func(name string) string {
		return strings.TrimSpace(os.Getenv(envPrefix + name))
	}

	cfg := &Config{
		Server:              get("SERVER"),
		Username:            get("USERNAME"),
		Password:            os.Getenv(envPrefix + "PASSWORD"),
		PrivateKey:          os.Getenv(envPrefix + "PRIVATE_KEY"),
		Passphrase:          os.Getenv(envPrefix + "PASSPHRASE"),
		HostKeyFingerprints: splitLines(os.Getenv(envPrefix + "HOST_KEY_FINGERPRINT")),
	}

	var err error
	if cfg.Port, err = parseInt(get("PORT"), 22); err != nil {
		return nil, fmt.Errorf("invalid port: %w", err)
	}
	if cfg.Concurrency, err = parseInt(get("CONCURRENCY"), 4); err != nil {
		return nil, fmt.Errorf("invalid concurrency: %w", err)
	}
	if cfg.SftpRequestConcurrency, err = parseInt(get("SFTP_REQUEST_CONCURRENCY"), 16); err != nil {
		return nil, fmt.Errorf("invalid sftp-request-concurrency: %w", err)
	}
	if cfg.Retries, err = parseInt(get("RETRIES"), 2); err != nil {
		return nil, fmt.Errorf("invalid retries: %w", err)
	}
	// Tombstone: 'delete' is still declared in action.yml so that a workflow
	// passing it fails loudly here instead of silently falling back to overlay.
	if deleted, err := parseBool(get("DELETE"), false); err != nil {
		return nil, fmt.Errorf("invalid delete: %w", err)
	} else if deleted {
		return nil, fmt.Errorf("the 'delete' input was removed in v2 — use 'strategy: clean' instead")
	}
	if cfg.DryRun, err = parseBool(get("DRY_RUN"), false); err != nil {
		return nil, fmt.Errorf("invalid dry-run: %w", err)
	}
	if cfg.SyncFastPath, err = parseBool(get("SYNC_FAST_PATH"), false); err != nil {
		return nil, fmt.Errorf("invalid sync-fast-path: %w", err)
	}
	if cfg.DirMode, err = parseMode(get("DIR_MODE"), "dir-mode"); err != nil {
		return nil, err
	}
	if cfg.FileMode, err = parseMode(get("FILE_MODE"), "file-mode"); err != nil {
		return nil, err
	}

	timeoutSec, err := parseInt(get("TIMEOUT"), 30)
	if err != nil {
		return nil, fmt.Errorf("invalid timeout: %w", err)
	}
	cfg.Timeout = time.Duration(timeoutSec) * time.Second

	// The deployment (targets, strategy, ignore, guards) comes either from a
	// YAML config file or from the plain action inputs, never a mix of both.
	configFile := get("CONFIG_FILE")
	strategyInput := get("STRATEGY")
	uploadsInput := os.Getenv(envPrefix + "UPLOADS")
	ignoreInput := os.Getenv(envPrefix + "IGNORE")
	ignoreFrom := get("IGNORE_FROM")
	maxDeletesInput := get("MAX_DELETES")

	if configFile != "" {
		if strings.TrimSpace(uploadsInput) != "" || strategyInput != "" ||
			strings.TrimSpace(ignoreInput) != "" || ignoreFrom != "" || maxDeletesInput != "" {
			return nil, fmt.Errorf("when 'config-file' is set, put targets/strategy/ignore/guards " +
				"in the file — do not also set the uploads, strategy, ignore, ignore-from or max-deletes inputs")
		}
		if err := loadConfigFile(cfg, configFile); err != nil {
			return nil, err
		}
	} else {
		strategy, err := resolveStrategy(strategyInput)
		if err != nil {
			return nil, err
		}
		if cfg.Uploads, err = ParseUploads(uploadsInput); err != nil {
			return nil, err
		}
		for i := range cfg.Uploads {
			cfg.Uploads[i].Strategy = strategy
		}
		cfg.IgnoreLines = splitLines(ignoreInput)
		if ignoreFrom != "" {
			data, err := os.ReadFile(ignoreFrom)
			if err != nil {
				return nil, fmt.Errorf("could not read ignore-from file: %w", err)
			}
			cfg.IgnoreLines = append(cfg.IgnoreLines, splitLines(string(data))...)
		}
		if cfg.Guards.MaxDeletes, err = parseInt(maxDeletesInput, 0); err != nil {
			return nil, fmt.Errorf("invalid max-deletes: %w", err)
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// resolveStrategy maps the strategy input to a concrete strategy.
func resolveStrategy(input string) (Strategy, error) {
	if input == "" {
		return StrategyOverlay, nil
	}
	s := Strategy(input)
	if !s.valid() {
		return "", fmt.Errorf("input 'strategy' must be overlay, sync or clean, got %q", input)
	}
	return s, nil
}

func (c *Config) validate() error {
	switch {
	case c.Server == "":
		return fmt.Errorf("input 'server' is required")
	case c.Username == "":
		return fmt.Errorf("input 'username' is required")
	case c.Password == "" && strings.TrimSpace(c.PrivateKey) == "":
		return fmt.Errorf("either input 'password' or 'private-key' is required")
	case len(c.Uploads) == 0:
		return fmt.Errorf("input 'uploads' is required and must contain at least one 'local => remote' mapping")
	case c.Port < 1 || c.Port > 65535:
		return fmt.Errorf("input 'port' must be between 1 and 65535, got %d", c.Port)
	case c.Concurrency < 1:
		return fmt.Errorf("input 'concurrency' must be at least 1, got %d", c.Concurrency)
	case c.SftpRequestConcurrency < 1:
		return fmt.Errorf("input 'sftp-request-concurrency' must be at least 1, got %d", c.SftpRequestConcurrency)
	case c.Retries < 0:
		return fmt.Errorf("input 'retries' must not be negative, got %d", c.Retries)
	case c.Timeout < 0:
		return fmt.Errorf("input 'timeout' must not be negative (use 0 to disable the timeout), got %d", int(c.Timeout/time.Second))
	case c.Guards.MaxDeletes < 0:
		return fmt.Errorf("guards.max_deletes must not be negative, got %d", c.Guards.MaxDeletes)
	}
	return nil
}

// ParseUploads parses the multiline "local => remote" upload mapping.
// Empty lines and lines starting with '#' are skipped.
func ParseUploads(s string) ([]UploadPair, error) {
	var pairs []UploadPair
	for _, line := range splitLines(s) {
		parts := strings.SplitN(line, "=>", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid upload mapping %q: expected format 'local/path => remote/path'", line)
		}
		local, remote := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if local == "" || remote == "" {
			return nil, fmt.Errorf("invalid upload mapping %q: local and remote path must not be empty", line)
		}
		pairs = append(pairs, UploadPair{Local: local, Remote: remote})
	}
	return pairs, nil
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
