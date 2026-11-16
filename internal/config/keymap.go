package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// KeyMap is a backend's keyMap: by key name or group name, the key sent to
// the backend on a caller's behalf, in the order the entries are written. A
// caller's own entry applies first; among its groups' entries, the first one
// does, so the order decides only for a key in several mapped groups. It is
// a list, decoded by UnmarshalYAML, because a Go map loses the order.
type KeyMap []KeyMapEntry

// KeyMapEntry is one entry of a keyMap: a key name or group name, and the
// key it maps to.
type KeyMapEntry struct {
	Name string
	Key  string
}

// Key returns the key of the entry named name.
func (m KeyMap) Key(name string) (string, bool) {
	for _, e := range m {
		if e.Name == name {
			return e.Key, true
		}
	}
	return "", false
}

// UnmarshalYAML reads a keyMap mapping in order. An entry merged with `<<:`
// takes the merge's place, unless the mapping names it itself, since its own
// entry overrides a merged one, as it would in a decoded map.
func (m *KeyMap) UnmarshalYAML(n *yaml.Node) error {
	var entries KeyMap
	if err := appendKeyMap(n, &entries, map[string]bool{}, map[string]bool{}); err != nil {
		return err
	}
	*m = entries
	return nil
}

// appendKeyMap adds mapping n's entries to entries in order, skipping names in
// skip, which the mapping that merged n names itself, and in seen, which an
// earlier entry or merge already gave.
func appendKeyMap(n *yaml.Node, entries *KeyMap, skip, seen map[string]bool) error {
	n = resolve(n)
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: keyMap must map key and group names to keys", n.Line)
	}
	own := map[string]bool{}
	for k, v := range skip {
		own[k] = v
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if key := n.Content[i]; key.Tag != "!!merge" {
			own[key.Value] = true
		}
	}
	listed := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], resolve(n.Content[i+1])
		if key.Tag == "!!merge" {
			sources := []*yaml.Node{value}
			if value.Kind == yaml.SequenceNode {
				sources = value.Content
			}
			for _, source := range sources {
				if err := appendKeyMap(source, entries, own, seen); err != nil {
					return err
				}
			}
			continue
		}
		if listed[key.Value] {
			return fmt.Errorf("line %d: keyMap.%s is listed twice", key.Line, key.Value)
		}
		listed[key.Value] = true
		if value.Kind != yaml.ScalarNode {
			return fmt.Errorf("line %d: keyMap.%s must be a key", value.Line, key.Value)
		}
		if skip[key.Value] || seen[key.Value] {
			continue
		}
		seen[key.Value] = true
		entry := KeyMapEntry{Name: key.Value, Key: value.Value}
		if value.Tag == "!!null" {
			entry.Key = "" // `~` and `null` are no key, which the client refuses as empty
		}
		*entries = append(*entries, entry)
	}
	return nil
}
