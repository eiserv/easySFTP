package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// yamlConfig mirrors the YAML config file. Its JSON Schema lives in
// schema/easysftp.schema.json (used for editor validation); the checks in
// applyYAML enforce the same rules at runtime with friendly messages.
type yamlConfig struct {
	Version  int          `yaml:"version"`
	Strategy string       `yaml:"strategy"`
	Ignore   []string     `yaml:"ignore"`
	Guards   yamlGuards   `yaml:"guards"`
	Targets  []yamlTarget `yaml:"targets"`
}

type yamlGuards struct {
	MaxDeletes int `yaml:"max_deletes"`
}

type yamlTarget struct {
	Local    string   `yaml:"local"`
	Remote   string   `yaml:"remote"`
	Strategy string   `yaml:"strategy"`
	Ignore   []string `yaml:"ignore"`
}

// loadConfigFile reads, parses and applies the YAML config file onto cfg.
func loadConfigFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("could not read config-file %q: %w", path, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // unknown keys become a clear error instead of silent no-ops
	var yc yamlConfig
	if err := dec.Decode(&yc); err != nil {
		return fmt.Errorf("config-file %q is not valid: %w", path, err)
	}
	return applyYAML(cfg, &yc)
}

func applyYAML(cfg *Config, yc *yamlConfig) error {
	if yc.Version != 1 {
		return fmt.Errorf("config-file: 'version' must be 1, got %d", yc.Version)
	}

	def := StrategyOverlay
	if yc.Strategy != "" {
		def = Strategy(yc.Strategy)
		if !def.valid() {
			return fmt.Errorf("config-file: 'strategy' must be overlay, sync or clean, got %q", yc.Strategy)
		}
	}
	cfg.Guards.MaxDeletes = yc.Guards.MaxDeletes

	if len(yc.Targets) == 0 {
		return fmt.Errorf("config-file: 'targets' must contain at least one entry")
	}
	for i, t := range yc.Targets {
		if t.Local == "" || t.Remote == "" {
			return fmt.Errorf("config-file: target %d: both 'local' and 'remote' are required", i+1)
		}
		st := def
		if t.Strategy != "" {
			st = Strategy(t.Strategy)
			if !st.valid() {
				return fmt.Errorf("config-file: target %d (%s): 'strategy' must be overlay, sync or clean, got %q", i+1, t.Remote, t.Strategy)
			}
		}
		cfg.Uploads = append(cfg.Uploads, UploadPair{
			Local:    t.Local,
			Remote:   t.Remote,
			Strategy: st,
			Ignore:   t.Ignore,
		})
	}
	cfg.IgnoreLines = append(cfg.IgnoreLines, yc.Ignore...)
	return nil
}
