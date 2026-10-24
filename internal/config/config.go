// Package config loads the gateway config: parse to a yaml.Node tree, expand
// ${NAME} in scalar string values, then decode strictly so unknown keys are
// startup errors. Backend and virtual model entries are kept as nodes; each
// type decodes its own schema with DecodeStrict.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   Server    `yaml:"server"`
	Logging  Logging   `yaml:"logging"`
	Vars     yaml.Node `yaml:"vars"` // ignored; a home for anchors
	Backends yaml.Node `yaml:"backends"`
	Models   yaml.Node `yaml:"models"`
}

type Server struct {
	Listen       string   `yaml:"listen"`
	APIKeys      []APIKey `yaml:"apiKeys"`
	UnknownKeys  string   `yaml:"unknownKeys"` // reject (default) | allow
	TargetHeader string   `yaml:"targetHeader"`
}

type APIKey struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

type Logging struct {
	Level        string `yaml:"level"`
	LogSelection bool   `yaml:"logSelection"`
}

// UnknownClient is the client name logged for a key not in server.apiKeys.
const UnknownClient = "unknown"

// Entry is one named item under `backends:` or `models:`.
type Entry struct {
	Name string
	Type string // the entry's `type` key; "" when absent
	Node *yaml.Node
}

// Load reads, expands and strictly decodes the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data, os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse is Load without the file read, with an injectable environment.
func Parse(data []byte, lookupEnv func(string) (string, bool)) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("config is empty")
	}
	root := doc.Content[0]
	if err := expandEnv(root, lookupEnv); err != nil {
		return nil, err
	}
	var cfg Config
	if err := DecodeStrict(root, &cfg); err != nil {
		return nil, err
	}
	if cfg.Server.Listen == "" {
		return nil, fmt.Errorf("server.listen is required")
	}
	if cfg.Server.TargetHeader == "" {
		cfg.Server.TargetHeader = "X-Gateway-Target"
	}
	for i, k := range cfg.Server.APIKeys {
		if k.Name == "" || k.Key == "" {
			return nil, fmt.Errorf("server.apiKeys[%d]: name and key are required", i)
		}
		if k.Name == UnknownClient {
			return nil, fmt.Errorf("server.apiKeys[%d]: name '%s' is reserved for unlisted keys", i, UnknownClient)
		}
	}
	switch cfg.Server.UnknownKeys {
	case "":
		cfg.Server.UnknownKeys = "reject"
	case "reject", "allow":
	default:
		return nil, fmt.Errorf("server.unknownKeys: '%s' is not reject or allow", cfg.Server.UnknownKeys)
	}
	return &cfg, nil
}

// Entries lists a section's named entries in file order. A repeated name is an
// error: callers keep entries both in order and in a map by name, and the two
// would disagree on which definition the name means.
func Entries(section *yaml.Node, sectionName string) ([]Entry, error) {
	section = resolve(section)
	if section.Kind == 0 || isNull(section) {
		return nil, nil
	}
	if section.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: %s must be a map", section.Line, sectionName)
	}
	var entries []Entry
	seen := map[string]int{}
	for i := 0; i < len(section.Content); i += 2 {
		k, v := section.Content[i], resolve(section.Content[i+1])
		if first, ok := seen[k.Value]; ok {
			return nil, fmt.Errorf("line %d: %s.%s is defined again (first at line %d)", k.Line, sectionName, k.Value, first)
		}
		seen[k.Value] = k.Line
		if v.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("line %d: %s.%s must be a map", v.Line, sectionName, k.Value)
		}
		e := Entry{Name: k.Value, Node: v}
		for j := 0; j < len(v.Content); j += 2 {
			if v.Content[j].Value == "type" {
				e.Type = resolve(v.Content[j+1]).Value
			}
		}
		entries = append(entries, e)
	}
	return entries, nil
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${NAME} in scalar string values. Alias nodes are skipped:
// their targets are expanded where the anchor is defined.
func expandEnv(n *yaml.Node, lookupEnv func(string) (string, bool)) error {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			if err := expandEnv(n.Content[i], lookupEnv); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if err := expandEnv(c, lookupEnv); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if n.Tag != "!!str" || !strings.Contains(n.Value, "${") {
			return nil
		}
		var missing string
		n.Value = envPattern.ReplaceAllStringFunc(n.Value, func(m string) string {
			name := m[2 : len(m)-1]
			v, ok := lookupEnv(name)
			if !ok && missing == "" {
				missing = name
			}
			return v
		})
		if missing != "" {
			return fmt.Errorf("line %d: environment variable %s is not defined", n.Line, missing)
		}
	}
	return nil
}

func resolve(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}
