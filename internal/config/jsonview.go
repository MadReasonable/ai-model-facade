package config

import (
	"bytes"
	"encoding/json"
	"math"

	"gopkg.in/yaml.v3"
)

// MarshalJSON returns n as JSON, which has no anchors or merges: an alias is
// written as the value of its anchor, and a merge ('<<') as the pairs it
// brings, so a JSON client sees each value as the gateway reads it. Key
// order is kept; comments are left out, since JSON has none.
func MarshalJSON(n *yaml.Node) ([]byte, error) {
	data, err := json.Marshal(jsonValue(n))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// orderedMap is a JSON object with its keys in a given order.
type orderedMap struct {
	keys   []string
	values map[string]any
}

func (m orderedMap) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteByte(':')
		v, err := json.Marshal(m.values[k])
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func jsonValue(n *yaml.Node) any {
	n = resolve(n)
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil
		}
		return jsonValue(n.Content[0])
	case yaml.MappingNode:
		m := orderedMap{values: map[string]any{}}
		for _, pair := range mergedPairs(n) {
			m.keys = append(m.keys, pair[0].Value)
			m.values[pair[0].Value] = jsonValue(pair[1])
		}
		return m
	case yaml.SequenceNode:
		items := []any{}
		for _, c := range n.Content {
			items = append(items, jsonValue(c))
		}
		return items
	}
	switch n.Tag {
	case "!!null":
		return nil
	case "!!bool":
		var b bool
		if n.Decode(&b) == nil {
			return b
		}
	case "!!int":
		var i int64
		if n.Decode(&i) == nil {
			return i
		}
	case "!!float":
		var f float64
		if n.Decode(&f) == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return f
		}
	}
	return n.Value
}

// mergedPairs returns a mapping's pairs as the gateway reads them: each
// merge replaced by the pairs it brings, in place, without those a key of
// the mapping's own or an earlier source already gives.
func mergedPairs(n *yaml.Node) [][2]*yaml.Node {
	own := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Tag != "!!merge" {
			own[n.Content[i].Value] = true
		}
	}
	seen := map[string]bool{}
	var out [][2]*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Tag != "!!merge" {
			out = append(out, [2]*yaml.Node{k, v})
			seen[k.Value] = true
			continue
		}
		sources := []*yaml.Node{resolve(v)}
		if resolve(v).Kind == yaml.SequenceNode {
			sources = resolve(v).Content
		}
		for _, src := range sources {
			src = resolve(src)
			if src.Kind != yaml.MappingNode {
				continue
			}
			for _, pair := range mergedPairs(src) {
				if !own[pair[0].Value] && !seen[pair[0].Value] {
					out = append(out, pair)
					seen[pair[0].Value] = true
				}
			}
		}
	}
	return out
}

// MarshalYAML returns n as YAML, as the files would hold it, comments,
// anchors and aliases included.
func MarshalYAML(n *yaml.Node) ([]byte, error) {
	return encodeYAML(n)
}
