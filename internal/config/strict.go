package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	nodeType   = reflect.TypeOf(yaml.Node{})
	accessType = reflect.TypeOf(Access{})
)

// DecodeStrict decodes n into v and fails on mapping keys that v's struct
// types don't declare. yaml.Node.Decode ignores Decoder.KnownFields, and the
// config is decoded from an env-expanded node tree, so the check is done here
// against the tree, which also keeps file positions in the errors.
// yaml.Node fields are left for their owner to decode.
func DecodeStrict(n *yaml.Node, v any) error {
	if err := checkKnown(n, reflect.TypeOf(v)); err != nil {
		return err
	}
	return n.Decode(v)
}

func checkKnown(n *yaml.Node, t reflect.Type) error {
	n = resolve(n)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nodeType {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			return nil // Decode reports the type mismatch
		}
		fields := yamlFields(t)
		for i := 0; i < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Tag == "!!merge" {
				if err := checkMerge(v, t); err != nil {
					return err
				}
				continue
			}
			ft, ok := fields.fields[k.Value]
			if !ok {
				if fields.inlineMap == nil {
					return fmt.Errorf("line %d: unknown key '%s'", k.Line, k.Value)
				}
				ft = fields.inlineMap // an inline map catches any leftover key
			}
			if err := checkShape(k, v, ft); err != nil {
				return err
			}
			if err := checkKnown(v, ft); err != nil {
				return err
			}
		}
	case reflect.Map:
		if n.Kind == yaml.MappingNode {
			for i := 1; i < len(n.Content); i += 2 {
				if err := checkShape(n.Content[i-1], n.Content[i], t.Elem()); err != nil {
					return err
				}
				if err := checkKnown(n.Content[i], t.Elem()); err != nil {
					return err
				}
			}
		}
	case reflect.Slice:
		if n.Kind == yaml.SequenceNode {
			for _, c := range n.Content {
				if err := checkKnown(c, t.Elem()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkShape refuses a value whose shape is wrong for its field, where
// Decode's own message, such as "cannot unmarshal !!str into []string",
// would name neither the key nor what is expected: a single name where a
// list of names goes, as in allowedKeys: alice, and an access list given as
// anything but a map, as in read: [alice].
func checkShape(k, v *yaml.Node, t reflect.Type) error {
	v = resolve(v)
	if t == accessType && v.Kind != yaml.MappingNode && v.Tag != "!!null" {
		example := "alice"
		if v.Kind == yaml.ScalarNode && v.Value != "" {
			example = v.Value
		} else if v.Kind == yaml.SequenceNode && len(v.Content) > 0 && v.Content[0].Kind == yaml.ScalarNode {
			example = v.Content[0].Value
		}
		return fmt.Errorf("line %d: %s must be an access list, a map with allowedKeys or allowedGroups, such as {allowedKeys: [%s]}", v.Line, k.Value, example)
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.String && v.Kind == yaml.ScalarNode && v.Tag != "!!null" {
		return fmt.Errorf("line %d: %s must be a list, such as [%s]", v.Line, k.Value, v.Value)
	}
	return nil
}

func checkMerge(v *yaml.Node, t reflect.Type) error {
	v = resolve(v)
	if v.Kind == yaml.SequenceNode {
		for _, c := range v.Content {
			if err := checkKnown(c, t); err != nil {
				return err
			}
		}
		return nil
	}
	return checkKnown(v, t)
}

// fieldSet is the set of keys one struct accepts, with `,inline` struct
// fields flattened in.
type fieldSet struct {
	fields    map[string]reflect.Type
	inlineMap reflect.Type // element type of an inline map, which accepts any key
}

func yamlFields(t reflect.Type) fieldSet {
	fs := fieldSet{fields: map[string]reflect.Type{}}
	addYamlFields(t, &fs)
	return fs
}

func addYamlFields(t reflect.Type, fs *fieldSet) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if slices.Contains(strings.Split(opts, ","), "inline") {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch ft.Kind() {
			case reflect.Struct:
				addYamlFields(ft, fs)
			case reflect.Map:
				fs.inlineMap = ft.Elem()
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		fs.fields[name] = f.Type
	}
}
