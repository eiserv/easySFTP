package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// yamlConfig mirrors the v3 YAML config file. Its JSON Schema lives in
// schema/easysftp.schema.json (used for editor validation); checkKeys and
// applyYAML enforce the same rules at runtime with friendly messages.
type yamlConfig struct {
	Version    int            `yaml:"version"`
	Connection yamlConnection `yaml:"connection"`
	Defaults   yamlDefaults   `yaml:"defaults"`
	// Deployments stays a raw node so the file's own ordering is preserved
	// (a Go map would shuffle it) and duplicate names can be detected.
	Deployments yaml.Node       `yaml:"deployments"`
	Safety      yamlSafety      `yaml:"safety"`
	Advanced    yamlAdvanced    `yaml:"advanced"`
	Permissions yamlPermissions `yaml:"permissions"`
	Sync        yamlSync        `yaml:"sync"`
}

type yamlConnection struct {
	Host            string     `yaml:"host"`
	Port            int        `yaml:"port"`
	Username        string     `yaml:"username"`
	HostKey         string     `yaml:"host_key"`
	KnownHosts      string     `yaml:"known_hosts"`
	AllowAnyHostKey bool       `yaml:"allow_any_host_key"`
	Proxy           *yamlProxy `yaml:"proxy"`
}

type yamlProxy struct {
	Host            string `yaml:"host"`
	Port            int    `yaml:"port"`
	Username        string `yaml:"username"`
	HostKey         string `yaml:"host_key"`
	KnownHosts      string `yaml:"known_hosts"`
	AllowAnyHostKey bool   `yaml:"allow_any_host_key"`
}

type yamlDefaults struct {
	Mode    string   `yaml:"mode"`
	Exclude []string `yaml:"exclude"`
}

type yamlDeployment struct {
	Source  string   `yaml:"source"`
	Target  string   `yaml:"target"`
	Mode    string   `yaml:"mode"`
	Exclude []string `yaml:"exclude"`
}

type yamlSafety struct {
	MaxDeletes int `yaml:"max_deletes"`
}

// yamlAdvanced uses pointers so "not set" keeps the run-wide default while an
// explicit 0 (e.g. retries: 0, timeout: 0) means what it says.
type yamlAdvanced struct {
	Retries            *int    `yaml:"retries"`
	Timeout            *int    `yaml:"timeout"`
	StallTimeout       *int    `yaml:"stall_timeout"`
	Concurrency        autoInt `yaml:"concurrency"`
	RequestConcurrency autoInt `yaml:"request_concurrency"`
	SkipUnchanged      bool    `yaml:"skip_unchanged"`
}

type yamlPermissions struct {
	Files         string `yaml:"files"`
	Directories   string `yaml:"directories"`
	PreserveTimes bool   `yaml:"preserve_times"`
}

type yamlSync struct {
	FastPath bool   `yaml:"fast_path"`
	Manifest string `yaml:"manifest"`
}

// autoInt is an integer that also accepts the literal "auto" (or being
// absent), both meaning "let easySFTP pick the default".
type autoInt struct {
	set bool
	v   int
}

func (a *autoInt) UnmarshalYAML(node *yaml.Node) error {
	if node.Value == "auto" {
		return nil
	}
	if err := node.Decode(&a.v); err != nil {
		return fmt.Errorf("must be a number or \"auto\", got %q", node.Value)
	}
	a.set = true
	return nil
}

// or returns the configured value, or def when unset/"auto".
func (a autoInt) or(def int) int {
	if a.set {
		return a.v
	}
	return def
}

// allowedKeys lists the valid option names per config-file section, keyed by
// the section's dotted path ("" is the file's top level, "deployments.*" any
// named deployment). checkKeys walks the raw YAML against it so a typo fails
// with a suggestion instead of a silent no-op or a cryptic decoder error.
var allowedKeys = map[string][]string{
	"":                 {"version", "connection", "defaults", "deployments", "safety", "advanced", "permissions", "sync"},
	"connection":       {"host", "port", "username", "host_key", "known_hosts", "allow_any_host_key", "proxy"},
	"connection.proxy": {"host", "port", "username", "host_key", "known_hosts", "allow_any_host_key"},
	"defaults":         {"mode", "exclude"},
	"deployments.*":    {"source", "target", "mode", "exclude"},
	"safety":           {"max_deletes"},
	"advanced":         {"retries", "timeout", "stall_timeout", "concurrency", "request_concurrency", "skip_unchanged"},
	"permissions":      {"files", "directories", "preserve_times"},
	"sync":             {"fast_path", "manifest"},
}

// checkKeys validates every mapping key in the file against allowedKeys and
// reports the first unknown one with its location and, when a known key is
// close enough, a "did you mean" suggestion.
func checkKeys(node *yaml.Node, section, location string) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	allowed := allowedKeys[section]
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		at := key
		if location != "" {
			at = location + "." + key
		}
		if !contains(allowed, key) {
			msg := fmt.Sprintf("unknown option %q at %q", key, at)
			if s := closestKey(key, allowed); s != "" {
				msg += fmt.Sprintf("; did you mean %q?", s)
			}
			return fmt.Errorf("%s", msg)
		}
		// Recurse into the sections that have their own key set.
		sub := section
		if section == "" {
			sub = key
		} else {
			sub = section + "." + key
		}
		if _, ok := allowedKeys[sub]; ok {
			if err := checkKeys(value, sub, at); err != nil {
				return err
			}
		}
		if (section == "" && key == "deployments") && value.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(value.Content); j += 2 {
				name, dep := value.Content[j].Value, value.Content[j+1]
				if err := checkKeys(dep, "deployments.*", "deployments."+name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, l := range list {
		if l == s {
			return true
		}
	}
	return false
}

// closestKey returns the allowed key closest to got (edit distance at most
// 2), or "" when nothing is close enough to suggest.
func closestKey(got string, allowed []string) string {
	best, bestDist := "", 3
	for _, a := range allowed {
		if d := editDistance(got, a); d < bestDist {
			best, bestDist = a, d
		}
	}
	return best
}

// editDistance is the classic Levenshtein distance.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = minInt(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func minInt(vals ...int) int {
	m := vals[0]
	for _, v := range vals[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

// loadConfigFile reads, parses and applies the v3 YAML config file onto cfg.
func loadConfigFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("could not read config %q: %w", path, err)
	}

	fail := func(err error) error {
		return fmt.Errorf("config %q: %w", path, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fail(err)
	}
	if len(root.Content) > 0 {
		if err := checkKeys(root.Content[0], "", ""); err != nil {
			return fail(err)
		}
	}

	var yc yamlConfig
	if err := root.Decode(&yc); err != nil {
		return fail(err)
	}
	if err := applyYAML(cfg, &yc); err != nil {
		return fail(err)
	}
	return nil
}

func applyYAML(cfg *Config, yc *yamlConfig) error {
	if yc.Version != 3 {
		return fmt.Errorf("'version' must be 3, got %d (v1 config files are not supported by easySFTP v3; see docs/migration-v3.md)", yc.Version)
	}

	// Connection.
	conn := yc.Connection
	cfg.Server = strings.TrimSpace(conn.Host)
	cfg.Username = strings.TrimSpace(conn.Username)
	cfg.Port = 22
	if conn.Port != 0 {
		cfg.Port = conn.Port
	}
	cfg.HostKeyFingerprints = splitLines(conn.HostKey)
	cfg.KnownHosts = strings.TrimSpace(conn.KnownHosts)
	cfg.AllowAnyHostKey = conn.AllowAnyHostKey
	if p := conn.Proxy; p != nil {
		proxy := &Proxy{
			Server:              strings.TrimSpace(p.Host),
			Port:                22,
			Username:            strings.TrimSpace(p.Username),
			HostKeyFingerprints: splitLines(p.HostKey),
			KnownHosts:          strings.TrimSpace(p.KnownHosts),
			AllowAnyHostKey:     p.AllowAnyHostKey,
		}
		if p.Port != 0 {
			proxy.Port = p.Port
		}
		if proxy.Server == "" {
			return fmt.Errorf("'connection.proxy.host' is required when connection.proxy is set")
		}
		cfg.Proxy = proxy
	}

	// Defaults and deployments.
	def := StrategyOverlay
	if yc.Defaults.Mode != "" {
		def = Strategy(yc.Defaults.Mode)
		if !def.valid() {
			return fmt.Errorf("'defaults.mode' must be overlay, sync or clean, got %q", yc.Defaults.Mode)
		}
	}
	cfg.IgnoreLines = append(cfg.IgnoreLines, yc.Defaults.Exclude...)

	deps := yc.Deployments
	if deps.Kind == 0 || len(deps.Content) == 0 {
		return fmt.Errorf("'deployments' must contain at least one named deployment, e.g.\n\ndeployments:\n  website:\n    source: dist\n    target: /var/www/html")
	}
	if deps.Kind != yaml.MappingNode {
		return fmt.Errorf("'deployments' must be a map of named deployments (v1's 'targets' list is not supported; see docs/migration-v3.md)")
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(deps.Content); i += 2 {
		name := deps.Content[i].Value
		if seen[name] {
			return fmt.Errorf("deployment %q is defined twice", name)
		}
		seen[name] = true
		var d yamlDeployment
		if err := deps.Content[i+1].Decode(&d); err != nil {
			return fmt.Errorf("deployment %q: %w", name, err)
		}
		if d.Source == "" || d.Target == "" {
			return fmt.Errorf("deployment %q: both 'source' and 'target' are required", name)
		}
		mode := def
		if d.Mode != "" {
			mode = Strategy(d.Mode)
			if !mode.valid() {
				return fmt.Errorf("deployment %q: 'mode' must be overlay, sync or clean, got %q", name, d.Mode)
			}
		}
		cfg.Uploads = append(cfg.Uploads, UploadPair{
			Name:     name,
			Local:    d.Source,
			Remote:   d.Target,
			Strategy: mode,
			Ignore:   d.Exclude,
		})
	}

	// Safety, advanced tuning, permissions, sync.
	cfg.Guards.MaxDeletes = yc.Safety.MaxDeletes
	if yc.Advanced.Retries != nil {
		cfg.Retries = *yc.Advanced.Retries
	}
	if yc.Advanced.Timeout != nil {
		cfg.Timeout = time.Duration(*yc.Advanced.Timeout) * time.Second
	}
	if yc.Advanced.StallTimeout != nil {
		cfg.StallTimeout = time.Duration(*yc.Advanced.StallTimeout) * time.Second
	}
	cfg.Concurrency = yc.Advanced.Concurrency.or(defaultConcurrency)
	cfg.SftpRequestConcurrency = yc.Advanced.RequestConcurrency.or(defaultRequestConcurrency)
	cfg.SkipUnchanged = yc.Advanced.SkipUnchanged

	var err error
	if cfg.FileMode, err = parseMode(yc.Permissions.Files, "permissions.files"); err != nil {
		return err
	}
	if cfg.DirMode, err = parseMode(yc.Permissions.Directories, "permissions.directories"); err != nil {
		return err
	}
	cfg.PreserveTimes = yc.Permissions.PreserveTimes

	cfg.SyncFastPath = yc.Sync.FastPath
	if cfg.ManifestName, err = parseManifestName(yc.Sync.Manifest); err != nil {
		return err
	}
	return nil
}
