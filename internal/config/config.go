// Package config loads and validates the action configuration from
// environment variables set by action.yml.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// UploadPair maps a local path to a remote path.
type UploadPair struct {
	Local  string
	Remote string
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

	Delete      bool
	DryRun      bool
	Concurrency int
	Retries     int
	Timeout     time.Duration
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
	if cfg.Retries, err = parseInt(get("RETRIES"), 2); err != nil {
		return nil, fmt.Errorf("invalid retries: %w", err)
	}
	if cfg.Delete, err = parseBool(get("DELETE"), false); err != nil {
		return nil, fmt.Errorf("invalid delete: %w", err)
	}
	if cfg.DryRun, err = parseBool(get("DRY_RUN"), false); err != nil {
		return nil, fmt.Errorf("invalid dry-run: %w", err)
	}

	timeoutSec, err := parseInt(get("TIMEOUT"), 30)
	if err != nil {
		return nil, fmt.Errorf("invalid timeout: %w", err)
	}
	cfg.Timeout = time.Duration(timeoutSec) * time.Second

	if cfg.Uploads, err = ParseUploads(os.Getenv(envPrefix + "UPLOADS")); err != nil {
		return nil, err
	}

	cfg.IgnoreLines = splitLines(os.Getenv(envPrefix + "IGNORE"))
	if ignoreFrom := get("IGNORE_FROM"); ignoreFrom != "" {
		data, err := os.ReadFile(ignoreFrom)
		if err != nil {
			return nil, fmt.Errorf("could not read ignore-from file: %w", err)
		}
		cfg.IgnoreLines = append(cfg.IgnoreLines, splitLines(string(data))...)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
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
	case c.Retries < 0:
		return fmt.Errorf("input 'retries' must not be negative, got %d", c.Retries)
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
