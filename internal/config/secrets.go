package config

import (
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

// secrets is the schema of the secrets file: the config's shape, holding only
// the fields that are secrets, so the file can't change routing, access or
// anything else. Every other key is an unknown key to DecodeStrict.
type secrets struct {
	Server struct {
		APIKeys []APIKey `yaml:"apiKeys"`
	} `yaml:"server"`
	Backends yaml.Node `yaml:"backends"`
}

// backendSecrets is a backend's entry in the secrets file.
type backendSecrets struct {
	APIKey *string `yaml:"apiKey"`
	KeyMap KeyMap  `yaml:"keyMap"`
}

// SecretsError is an error in the secrets file itself, so Load names that
// file rather than the config.
type SecretsError struct {
	Err error
}

func (e *SecretsError) Error() string { return e.Err.Error() }
func (e *SecretsError) Unwrap() error { return e.Err }

// parseSecrets reads a secrets file, expanding ${NAME} as the config does.
func parseSecrets(data []byte, lookupEnv func(string) (string, bool)) (*secrets, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, &SecretsError{err}
	}
	var s secrets
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return &s, nil // an empty file holds no secrets
	}
	root := doc.Content[0]
	if err := expandEnv(root, lookupEnv); err != nil {
		return nil, &SecretsError{err}
	}
	if err := DecodeStrict(root, &s); err != nil {
		return nil, &SecretsError{err}
	}
	return &s, nil
}

// mergeSecrets puts the secrets file's values into the config, before its
// strict decoding: server.apiKeys entries after the config's own, and each
// backend's apiKey and keyMap into its entry under backends, so adapters
// decode them as if the config held them. Each backend value lives in one of
// the two files: one set in both is an error, rather than one silently
// winning. A key entry adds a key, so a name may have entries in both.
func (cfg *Config) mergeSecrets(s *secrets) error {
	for i, k := range s.Server.APIKeys {
		// Checked here too, so an error names the secrets file and the
		// entry's place in it, not in the merged list.
		if k.Name == "" {
			return &SecretsError{fmt.Errorf("server.apiKeys[%d]: name is required", i)}
		}
		if _, err := k.digest(); err != nil {
			return &SecretsError{fmt.Errorf("server.apiKeys[%d] ('%s'): %w", i, k.Name, err)}
		}
	}
	cfg.Server.APIKeys = append(cfg.Server.APIKeys, s.Server.APIKeys...)
	entries, err := Entries(&s.Backends, "backends")
	if err != nil {
		return &SecretsError{err}
	}
	for _, e := range entries {
		var b backendSecrets
		if err := DecodeStrict(e.Node, &b); err != nil {
			return &SecretsError{fmt.Errorf("backends.%s: %w", e.Name, err)}
		}
		if err := cfg.mergeBackendSecrets(e.Name, e.Node.Line, b); err != nil {
			return err
		}
	}
	return nil
}

// mergeBackendSecrets adds one backend's secrets to its entry in the config.
// The entry, and its keyMap, are copied before anything is added, so an
// anchor they share with other entries is left as it was.
func (cfg *Config) mergeBackendSecrets(name string, line int, b backendSecrets) error {
	section := resolve(&cfg.Backends)
	i := -1
	for j := 0; section.Kind == yaml.MappingNode && j+1 < len(section.Content); j += 2 {
		if section.Content[j].Value == name {
			i = j
			break
		}
	}
	if i < 0 {
		return &SecretsError{fmt.Errorf("line %d: backends.%s: the config defines no backend '%s'", line, name, name)}
	}
	entry := copyMapping(resolve(section.Content[i+1]))
	section.Content[i+1] = entry
	if b.APIKey != nil {
		if _, ok := lookup(entry, "apiKey"); ok {
			return fmt.Errorf("backends.%s.apiKey is in both the config and the secrets file; keep it in one", name)
		}
		entry.Content = append(entry.Content, scalar("apiKey"), scalar(*b.APIKey))
	}
	if b.KeyMap == nil {
		return nil
	}
	keyMap, ok := lookup(entry, "keyMap")
	if !ok {
		keyMap = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		entry.Content = append(entry.Content, scalar("keyMap"), keyMap)
	} else {
		keyMap = copyMapping(resolve(keyMap))
		replace(entry, "keyMap", keyMap)
	}
	// In the secrets file's order, after the config's entries, since a
	// keyMap's order decides between the entries of a key's groups.
	for _, e := range b.KeyMap {
		if _, ok := lookup(keyMap, e.Name); ok {
			return fmt.Errorf("backends.%s.keyMap.%s is in both the config and the secrets file; keep it in one", name, e.Name)
		}
		keyMap.Content = append(keyMap.Content, scalar(e.Name), scalar(e.Key))
	}
	return nil
}

// lookup returns the value of key in mapping n, following `<<:` merges, as
// decoding does.
func lookup(n *yaml.Node, key string) (*yaml.Node, bool) {
	n = resolve(n)
	if n.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key && n.Content[i].Tag != "!!merge" {
			return n.Content[i+1], true
		}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Tag != "!!merge" {
			continue
		}
		merged := resolve(n.Content[i+1])
		sources := []*yaml.Node{merged}
		if merged.Kind == yaml.SequenceNode {
			sources = merged.Content
		}
		for _, src := range sources {
			if v, ok := lookup(src, key); ok {
				return v, true
			}
		}
	}
	return nil, false
}

// replace sets key's value in mapping n, adding the key if n has it only
// through a merge, where it is found by lookup but can't be changed.
func replace(n *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key && n.Content[i].Tag != "!!merge" {
			n.Content[i+1] = value
			return
		}
	}
	n.Content = append(n.Content, scalar(key), value)
}

// copyMapping returns a mapping node with the same pairs as n, so pairs can
// be added without changing n.
func copyMapping(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = slices.Clone(n.Content)
	return &c
}

func scalar(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}
