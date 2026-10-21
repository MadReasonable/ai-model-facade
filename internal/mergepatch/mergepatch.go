// Package mergepatch compiles JSON Merge Patches (RFC 7386) from YAML config
// nodes and applies them to raw JSON bodies as targeted byte edits.
//
// The walker iterates the patch, not the body: keys the patch doesn't mention
// are never touched, existing keys keep their position, and keys the patch
// adds are appended in config order.
package mergepatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gopkg.in/yaml.v3"
)

type kind int

const (
	kindNull    kind = iota // delete the key
	kindObject              // merge recursively
	kindLiteral             // scalar or array: replace
)

type value struct {
	kind kind
	// raw is the pre-encoded JSON written when this value replaces whatever is
	// in the body. For objects it is the patch applied to {}, so nulls are dropped.
	raw     []byte
	members []member // objects only, in config order
}

type member struct {
	key   string
	path  string // key escaped as one gjson/sjson path segment
	value *value
}

// Patch is one compiled merge patch. Its root is always an object.
type Patch struct {
	root *value
}

// Compile turns a params node into patches in application order. The node is
// a mapping (one patch) or a sequence of mappings (applied in order). A
// missing or null node yields no patches.
func Compile(n *yaml.Node) ([]Patch, error) {
	n = resolve(n)
	switch {
	case n.Kind == 0 || isNull(n):
		return nil, nil
	case n.Kind == yaml.MappingNode:
		root, err := compileObject(n)
		if err != nil {
			return nil, err
		}
		return []Patch{{root: root}}, nil
	case n.Kind == yaml.SequenceNode:
		var patches []Patch
		for _, item := range n.Content {
			item = resolve(item)
			if item.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("line %d: each params list item must be a map", item.Line)
			}
			root, err := compileObject(item)
			if err != nil {
				return nil, err
			}
			patches = append(patches, Patch{root: root})
		}
		return patches, nil
	default:
		return nil, fmt.Errorf("line %d: params must be a map or a list of maps", n.Line)
	}
}

func compileObject(n *yaml.Node) (*value, error) {
	obj := &value{kind: kindObject}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		k := resolve(n.Content[i])
		if k.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("line %d: params keys must be scalars", k.Line)
		}
		if k.Tag == "!!merge" {
			return nil, fmt.Errorf("line %d: YAML merge keys (<<) are shallow and not supported in params; use list-form params", k.Line)
		}
		if k.Value == "" {
			return nil, fmt.Errorf("line %d: empty params key", k.Line)
		}
		if seen[k.Value] {
			return nil, fmt.Errorf("line %d: duplicate params key %q", k.Line, k.Value)
		}
		seen[k.Value] = true
		v, err := compileValue(n.Content[i+1])
		if err != nil {
			return nil, err
		}
		obj.members = append(obj.members, member{key: k.Value, path: escapeKey(k.Value), value: v})
	}
	obj.raw = appendStripped(nil, obj)
	return obj, nil
}

func compileValue(n *yaml.Node) (*value, error) {
	n = resolve(n)
	switch {
	case n.Kind == yaml.MappingNode:
		return compileObject(n)
	case isNull(n):
		return &value{kind: kindNull}, nil
	default:
		raw, err := appendJSON(nil, n)
		if err != nil {
			return nil, err
		}
		return &value{kind: kindLiteral, raw: raw}, nil
	}
}

// Apply applies the patch to body, which must be a JSON object.
func (p Patch) Apply(body []byte) ([]byte, error) {
	return applyObject(body, "", p.root)
}

func applyObject(body []byte, prefix string, obj *value) ([]byte, error) {
	var err error
	for _, m := range obj.members {
		path := prefix + m.path
		switch m.value.kind {
		case kindNull:
			body, err = sjson.DeleteBytes(body, path)
		case kindObject:
			if gjson.GetBytes(body, path).IsObject() {
				body, err = applyObject(body, path+".", m.value)
			} else {
				body, err = sjson.SetRawBytes(body, path, m.value.raw)
			}
		default:
			body, err = sjson.SetRawBytes(body, path, m.value.raw)
		}
		if err != nil {
			return nil, fmt.Errorf("applying params at %s: %w", path, err)
		}
	}
	return body, nil
}

// String renders the patch as compact JSON in config order, nulls included.
func (p Patch) String() string {
	return string(appendPatch(nil, p.root))
}

// Compose folds patches applied in order into one patch. ok is false when no
// single merge patch is equivalent: a later object meeting an earlier
// non-object value for the same key replaces it when applied in sequence, but
// would merge into the body's value if folded into one patch.
func Compose(patches []Patch) (composed Patch, ok bool) {
	acc := &value{kind: kindObject}
	ok = true
	for _, p := range patches {
		var exact bool
		acc, exact = composeObject(acc, p.root)
		ok = ok && exact
	}
	return Patch{root: acc}, ok
}

func composeObject(acc, next *value) (*value, bool) {
	out := &value{kind: kindObject, members: append([]member(nil), acc.members...)}
	ok := true
	for _, m := range next.members {
		i := indexOf(out.members, m.key)
		if i < 0 {
			out.members = append(out.members, m)
			continue
		}
		prev := out.members[i].value
		if m.value.kind == kindObject {
			if prev.kind == kindObject {
				merged, exact := composeObject(prev, m.value)
				ok = ok && exact
				out.members[i].value = merged
				continue
			}
			ok = false
		}
		out.members[i] = m
	}
	out.raw = appendStripped(nil, out)
	return out, ok
}

func indexOf(members []member, key string) int {
	for i, m := range members {
		if m.key == key {
			return i
		}
	}
	return -1
}

// appendStripped encodes v as it is written into a body: nested nulls dropped.
func appendStripped(buf []byte, v *value) []byte {
	if v.kind != kindObject {
		return append(buf, v.raw...)
	}
	buf = append(buf, '{')
	first := true
	for _, m := range v.members {
		if m.value.kind == kindNull {
			continue
		}
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = appendString(buf, m.key)
		buf = append(buf, ':')
		buf = appendStripped(buf, m.value)
	}
	return append(buf, '}')
}

// appendPatch encodes v as a patch: nulls kept.
func appendPatch(buf []byte, v *value) []byte {
	switch v.kind {
	case kindNull:
		return append(buf, "null"...)
	case kindLiteral:
		return append(buf, v.raw...)
	}
	buf = append(buf, '{')
	for i, m := range v.members {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendString(buf, m.key)
		buf = append(buf, ':')
		buf = appendPatch(buf, m.value)
	}
	return append(buf, '}')
}

// appendJSON encodes a literal YAML value, preserving mapping key order.
func appendJSON(buf []byte, n *yaml.Node) ([]byte, error) {
	n = resolve(n)
	var err error
	switch n.Kind {
	case yaml.MappingNode:
		buf = append(buf, '{')
		for i := 0; i < len(n.Content); i += 2 {
			k := resolve(n.Content[i])
			if k.Kind != yaml.ScalarNode || k.Tag == "!!merge" {
				return nil, fmt.Errorf("line %d: unsupported key in params value", k.Line)
			}
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = appendString(buf, k.Value)
			buf = append(buf, ':')
			if buf, err = appendJSON(buf, n.Content[i+1]); err != nil {
				return nil, err
			}
		}
		return append(buf, '}'), nil
	case yaml.SequenceNode:
		buf = append(buf, '[')
		for i, item := range n.Content {
			if i > 0 {
				buf = append(buf, ',')
			}
			if buf, err = appendJSON(buf, item); err != nil {
				return nil, err
			}
		}
		return append(buf, ']'), nil
	case yaml.ScalarNode:
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		b, err := marshal(v)
		if err != nil {
			return nil, fmt.Errorf("line %d: value %q can't be encoded as JSON: %w", n.Line, n.Value, err)
		}
		return append(buf, b...), nil
	default:
		return nil, fmt.Errorf("line %d: unsupported YAML node in params", n.Line)
	}
}

func appendString(buf []byte, s string) []byte {
	b, _ := marshal(s)
	return append(buf, b...)
}

// marshal encodes like json.Marshal but leaves <, > and & readable.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// escapeKey escapes one object key as a gjson/sjson path segment. gjson.Escape
// covers the shared path syntax, but sjson additionally reads a leading ':' as
// its force-key marker and strips it, so it is escaped here as well.
func escapeKey(key string) string {
	p := gjson.Escape(key)
	if strings.HasPrefix(p, ":") {
		p = `\` + p
	}
	return p
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
