package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
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
	Host            string         `yaml:"host"`
	Port            int            `yaml:"port"`
	Username        string         `yaml:"username"`
	HostKey         fingerprints   `yaml:"host_key"`
	KnownHosts      string         `yaml:"known_hosts"`
	AllowAnyHostKey bool           `yaml:"allow_any_host_key"`
	Algorithms      yamlAlgorithms `yaml:"algorithms"`
	Proxy           *yamlProxy     `yaml:"proxy"`
}

type yamlAlgorithms struct {
	KeyExchanges      []string `yaml:"key_exchanges"`
	Ciphers           []string `yaml:"ciphers"`
	MACs              []string `yaml:"macs"`
	HostKeyAlgorithms []string `yaml:"host_key_algorithms"`
}

type yamlProxy struct {
	Host            string       `yaml:"host"`
	Port            int          `yaml:"port"`
	Username        string       `yaml:"username"`
	HostKey         fingerprints `yaml:"host_key"`
	KnownHosts      string       `yaml:"known_hosts"`
	AllowAnyHostKey bool         `yaml:"allow_any_host_key"`
}

// fingerprints is a host_key value: either a block scalar with one
// fingerprint per line (the documented spelling) or a YAML list of them
// (the natural "one or more" spelling, and how exclude is written). Both
// decode to the same newline-joined string that splitLines consumes.
type fingerprints string

func (f *fingerprints) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*f = fingerprints(node.Value)
	case yaml.SequenceNode:
		var list []string
		if err := node.Decode(&list); err != nil {
			return fmt.Errorf("host_key must be a fingerprint or a list of fingerprints, one per entry")
		}
		*f = fingerprints(strings.Join(list, "\n"))
	default:
		return fmt.Errorf("host_key must be a fingerprint or a list of fingerprints, one per entry")
	}
	return nil
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
	Connections        autoInt `yaml:"connections"`
	SkipUnchanged      bool    `yaml:"skip_unchanged"`
	AutoCache          string  `yaml:"auto_cache"`
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
// absent), both meaning "let easySFTP work the value out for itself". What
// that resolves to is internal/autotune's job, per deployment and per link;
// this type only records which of the two the user wrote.
type autoInt struct {
	set bool
	v   int
}

func (a *autoInt) UnmarshalYAML(node *yaml.Node) error {
	if node.Value == "auto" {
		return nil
	}
	// A float node decodes into an int without complaint (2.5 loads as 2),
	// so require an integer node up front: the schema says integer and the
	// parser should not silently truncate what the user wrote.
	if node.Tag != "!!int" {
		return fmt.Errorf("must be a number or \"auto\", got %q", node.Value)
	}
	if err := node.Decode(&a.v); err != nil {
		return fmt.Errorf("must be a number or \"auto\", got %q", node.Value)
	}
	a.set = true
	return nil
}

// or returns the configured value, or def when unset/"auto". def is the
// pre-adaptive fixed default, which the run carries only until the policy
// replaces it; auto reports which of the two happened.
func (a autoInt) or(def int) int {
	if a.set {
		return a.v
	}
	return def
}

// auto reports whether easySFTP chooses this value.
func (a autoInt) auto() bool { return !a.set }

// allowedKeys lists the valid option names per config-file section, keyed by
// the section's dotted path ("" is the file's top level, "deployments.*" any
// named deployment). checkKeys walks the raw YAML against it so a typo fails
// with a suggestion instead of a silent no-op or a cryptic decoder error.
var allowedKeys = map[string][]string{
	"":                      {"version", "connection", "defaults", "deployments", "safety", "advanced", "permissions", "sync"},
	"connection":            {"host", "port", "username", "host_key", "known_hosts", "allow_any_host_key", "algorithms", "proxy"},
	"connection.algorithms": {"key_exchanges", "ciphers", "macs", "host_key_algorithms"},
	"connection.proxy":      {"host", "port", "username", "host_key", "known_hosts", "allow_any_host_key"},
	"defaults":              {"mode", "exclude"},
	"deployments.*":         {"source", "target", "mode", "exclude"},
	"safety":                {"max_deletes"},
	"advanced":              {"retries", "timeout", "stall_timeout", "concurrency", "request_concurrency", "connections", "skip_unchanged", "auto_cache"},
	"permissions":           {"files", "directories", "preserve_times"},
	"sync":                  {"fast_path", "manifest"},
}

// checkKeys validates every mapping key in the file against allowedKeys and
// reports the first unknown one with its location and, when a known key is
// close enough, a "did you mean" suggestion.
//
// A "<<" merge key is not an option: yaml.v3 resolves merge keys when decoding
// into a struct, so the keys that matter are the ones in the aliased mapping.
// Walk those instead of reporting "<<" as unknown, and keep walking when the
// merge value is a sequence of aliases, which YAML also allows.
func checkKeys(node *yaml.Node, section, location string) error {
	return checkKeysVisited(node, section, location, map[*yaml.Node]bool{})
}

// checkKeysVisited is checkKeys carrying the set of alias targets already
// walked, so a self-referential merge key (a: &a with <<: *a inside it)
// fails with a cyclic-merge error instead of recursing until the stack
// overflows. yaml.v3 itself resolves such loops only when decoding, and the
// walk here predates that, so the guard belongs here.
func checkKeysVisited(node *yaml.Node, section, location string, visited map[*yaml.Node]bool) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	allowed := allowedKeys[section]
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		at := key.Value
		if location != "" {
			at = location + "." + key.Value
		}
		if key.Value == "<<" {
			// A merge key's value is an alias node or a sequence of alias
			// nodes; each alias target is a mapping whose keys must be
			// checked like the enclosing section's own keys.
			targets := []*yaml.Node{}
			if value.Kind == yaml.AliasNode {
				targets = append(targets, value.Alias)
			} else if value.Kind == yaml.MappingNode {
				// An inline mapping merges just like an alias target, so
				// its keys need the same check - a typo inside it is
				// otherwise a silent no-op, the class of bug this walk
				// exists to close.
				targets = append(targets, value)
			} else if value.Kind == yaml.SequenceNode {
				for _, item := range value.Content {
					if item.Kind == yaml.AliasNode {
						targets = append(targets, item.Alias)
					} else if item.Kind == yaml.MappingNode {
						targets = append(targets, item)
					}
				}
			}
			for _, target := range targets {
				if visited[target] {
					return fmt.Errorf("cyclic merge at %q: a merge key refers back to the mapping that contains it", at)
				}
				visited[target] = true
				err := checkKeysVisited(target, section, location, visited)
				// Back-track: the mark means "on this merge chain", not
				// "ever seen". A shared anchor referenced by two siblings
				// is a diamond, not a cycle - the one-base-many-targets
				// pattern the example config itself demonstrates - while
				// a real self-cycle still fails, because its mark stays
				// set for as long as the chain containing it is walked.
				delete(visited, target)
				if err != nil {
					return err
				}
			}
			continue
		}
		if !contains(allowed, key.Value) {
			msg := fmt.Sprintf("unknown option %q at %q", key.Value, at)
			if s := closestKey(key.Value, allowed); s != "" {
				msg += fmt.Sprintf("; did you mean %q?", s)
			}
			return fmt.Errorf("%s", msg)
		}
		// Recurse into the sections that have their own key set.
		sub := section
		if section == "" {
			sub = key.Value
		} else {
			sub = section + "." + key.Value
		}
		if _, ok := allowedKeys[sub]; ok {
			if err := checkKeysVisited(value, sub, at, visited); err != nil {
				return err
			}
		}
		if (section == "" && key.Value == "deployments") && value.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(value.Content); j += 2 {
				name, dep := value.Content[j].Value, value.Content[j+1]
				if err := checkKeysVisited(dep, "deployments.*", "deployments."+name, visited); err != nil {
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

// maxConfiguredSeconds caps timeout and stall_timeout at a day: a value
// beyond that overflows neither the int nor the duration in any harmful way,
// but the error-free run should not carry a value the user did not write
// because YAML's int parsed something enormous.
const maxConfiguredSeconds = 24 * 60 * 60

// isEmptySecondDocument reports whether a decoded second document carries
// no content: a document node whose single scalar is null (a trailing ---
// or one followed by comments only). Files ending that way loaded fine
// before the decoder swap and must keep loading.
func isEmptySecondDocument(node *yaml.Node) bool {
	if len(node.Content) != 1 {
		return false
	}
	doc := node.Content[0]
	if doc.Kind != yaml.ScalarNode || doc.Tag != "!!null" {
		return false
	}
	return doc.Value == ""
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

	// A decoder, not yaml.Unmarshal: Unmarshal reads the first document and
	// silently drops the rest, so a user who pasted one config under another
	// (a "---" separator between them) gets a run that uses half of what
	// they wrote. Reading a second document is the error to name.
	var root yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&root); err != nil && err != io.EOF {
		return fail(err)
	}
	var second yaml.Node
	if err := dec.Decode(&second); err != io.EOF {
		if err != nil {
			return fail(err)
		}
		// A lone '---' at the end of a file is a document node holding
		// a single null scalar. Nothing was pasted under it, so the file
		// is what it always was; refuse only a second document with
		// content, naming the line it starts on.
		if !isEmptySecondDocument(&second) {
			line := 0
			if len(second.Content) > 0 {
				line = second.Content[0].Line
			}
			return fail(fmt.Errorf("the file contains more than one YAML document (document 2 starts at line %d); easySFTP reads a single configuration, so combine the documents into one", line))
		}
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
	cfg.HostKeyFingerprints = splitLines(string(conn.HostKey))
	cfg.KnownHosts = strings.TrimSpace(conn.KnownHosts)
	cfg.AllowAnyHostKey = conn.AllowAnyHostKey
	var err error
	if cfg.Algorithms, err = parseSSHAlgorithms(conn.Algorithms); err != nil {
		return err
	}
	if p := conn.Proxy; p != nil {
		proxy := &Proxy{
			Server:              strings.TrimSpace(p.Host),
			Port:                22,
			Username:            strings.TrimSpace(p.Username),
			HostKeyFingerprints: splitLines(string(p.HostKey)),
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
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("deployment names must not be empty or whitespace; the inline deployment is the one configured without a config file, so a named file needs a name for every entry")
		}
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
	cfg.Safety.MaxDeletes = yc.Safety.MaxDeletes
	if yc.Advanced.Retries != nil {
		cfg.Retries = *yc.Advanced.Retries
	}
	if yc.Advanced.Timeout != nil {
		if *yc.Advanced.Timeout > maxConfiguredSeconds {
			return fmt.Errorf("'advanced.timeout' must be at most %d seconds (a day); a connection that outlives that is not a timeout anymore, got %d", maxConfiguredSeconds, *yc.Advanced.Timeout)
		}
		cfg.Timeout = time.Duration(*yc.Advanced.Timeout) * time.Second
	}
	if yc.Advanced.StallTimeout != nil {
		if *yc.Advanced.StallTimeout > maxConfiguredSeconds {
			return fmt.Errorf("'advanced.stall_timeout' must be at most %d seconds (a day); got %d", maxConfiguredSeconds, *yc.Advanced.StallTimeout)
		}
		cfg.StallTimeout = time.Duration(*yc.Advanced.StallTimeout) * time.Second
	}
	cfg.Concurrency = yc.Advanced.Concurrency.or(defaultConcurrency)
	cfg.SftpRequestConcurrency = yc.Advanced.RequestConcurrency.or(defaultRequestConcurrency)
	cfg.Connections = yc.Advanced.Connections.or(defaultConnections)
	// Each key is resolved on its own: a file that pins connections and
	// leaves concurrency at auto gets exactly that, and the policy then
	// chooses the concurrency knowing the connection count it has to live
	// with (issue #209).
	cfg.Auto = AutoSettings{
		Connections:        yc.Advanced.Connections.auto(),
		Concurrency:        yc.Advanced.Concurrency.auto(),
		RequestConcurrency: yc.Advanced.RequestConcurrency.auto(),
	}
	cfg.SkipUnchanged = yc.Advanced.SkipUnchanged
	cfg.AutoCachePath = strings.TrimSpace(yc.Advanced.AutoCache)

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

func parseSSHAlgorithms(y yamlAlgorithms) (SSHAlgorithms, error) {
	supported := ssh.SupportedAlgorithms()
	insecure := ssh.InsecureAlgorithms()
	var out SSHAlgorithms

	categories := []struct {
		name        string
		requested   []string
		supported   []string
		insecure    []string
		destination *[]string
	}{
		{"key_exchanges", y.KeyExchanges, supported.KeyExchanges, insecure.KeyExchanges, &out.KeyExchanges},
		{"ciphers", y.Ciphers, supported.Ciphers, insecure.Ciphers, &out.Ciphers},
		{"macs", y.MACs, supported.MACs, insecure.MACs, &out.MACs},
		{"host_key_algorithms", y.HostKeyAlgorithms, supported.HostKeys, insecure.HostKeys, &out.HostKeyAlgorithms},
	}

	for _, category := range categories {
		seen := map[string]bool{}
		for _, raw := range category.requested {
			name := strings.TrimSpace(raw)
			path := "connection.algorithms." + category.name
			if name == "" {
				return SSHAlgorithms{}, fmt.Errorf("'%s' contains an empty algorithm name", path)
			}
			if !slices.Contains(category.supported, name) && !slices.Contains(category.insecure, name) {
				return SSHAlgorithms{}, fmt.Errorf("'%s' contains unsupported SSH algorithm %q", path, name)
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			*category.destination = append(*category.destination, name)
			if slices.Contains(category.insecure, name) && !slices.Contains(out.Insecure, name) {
				out.Insecure = append(out.Insecure, name)
			}
		}
	}
	return out, nil
}
