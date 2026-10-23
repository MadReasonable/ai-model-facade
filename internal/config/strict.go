package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

var nodeType = reflect.TypeOf(yaml.Node{})

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
			if err := checkKnown(v, ft); err != nil {
				return err
			}
		}
	case reflect.Map:
		if n.Kind == yaml.MappingNode {
			for i := 1; i < len(n.Content); i += 2 {
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
