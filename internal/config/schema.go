package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Schema describes one config value for GET /amf/schema, in JSON Schema's
// terms where it has them (type, description, default, enum, minimum,
// properties, items, additionalProperties, required), and with x- keys for
// what the admin UI needs beyond them.
//
// It is made from the config's own structs by SchemaOf, from the same
// fields DecodeStrict accepts, so it can't list a key the gateway refuses or
// leave out one it takes. A field's `doc` tag is its description, and its
// `schema` tag, a comma-separated list, the rest:
//
//	default=<v>   the value used when the key is left out
//	min=<n>       the least value allowed
//	enum=a|b      the values allowed; on a list, its items'
//	required      the key must be given
//	secret        a value the admin endpoints show as <redacted>
//	unit=seconds  what a number counts
//	ref=<what>    the names it holds, of: keys (server.apiKeys names),
//	              groups (server.groups names and the built-in ones),
//	              names (either), backends, protocols; on a list, its
//	              items; on a map of lists, the lists' items; on another
//	              map, its keys
//	kind=<what>   for a yaml.Node, what it holds: params (a merge patch),
//	              anchors (vars), backends, models
type Schema struct {
	Type        string   `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Default     any      `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Minimum     *float64 `json:"minimum,omitempty"`
	// Properties are an object's keys, in the order the config writes them.
	Properties           Properties `json:"properties,omitempty"`
	Required             []string   `json:"required,omitempty"`
	Items                *Schema    `json:"items,omitempty"`
	AdditionalProperties *Schema    `json:"additionalProperties,omitempty"`
	Secret               bool       `json:"x-secret,omitempty"`
	Unit                 string     `json:"x-unit,omitempty"`
	Ref                  string     `json:"x-ref,omitempty"`
	Kind                 string     `json:"x-kind,omitempty"`
}

// Property is one key of an object.
type Property struct {
	Name   string
	Schema *Schema
}

// Properties are an object's keys in order, written as a JSON object.
type Properties []Property

func (p Properties) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, prop := range p {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(prop.Name)
		b.Write(name)
		b.WriteByte(':')
		value, err := json.Marshal(prop.Schema)
		if err != nil {
			return nil, err
		}
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Property returns the schema of the key name, nil if s has none.
func (s *Schema) Property(name string) *Schema {
	for _, p := range s.Properties {
		if p.Name == name {
			return p.Schema
		}
	}
	return nil
}

var keyMapType = reflect.TypeOf(KeyMap{})

// SchemaOf returns the schema of the struct v, or of the struct v points
// to. It panics on a field whose tags it can't read, since the tags are
// fixed in the source and a test reads every schema.
func SchemaOf(v any) *Schema {
	return schemaOfType(reflect.TypeOf(v))
}

func schemaOfType(t reflect.Type) *Schema {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == nodeType:
		return &Schema{}
	case t == keyMapType:
		return &Schema{Type: "object", AdditionalProperties: &Schema{Type: "string", Secret: true}}
	}
	switch t.Kind() {
	case reflect.Struct:
		s := &Schema{Type: "object"}
		addProperties(s, t)
		return s
	case reflect.Slice:
		return &Schema{Type: "array", Items: schemaOfType(t.Elem())}
	case reflect.Map:
		return &Schema{Type: "object", AdditionalProperties: schemaOfType(t.Elem())}
	case reflect.String:
		return &Schema{Type: "string"}
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Int, reflect.Int64:
		return &Schema{Type: "integer"}
	case reflect.Float64:
		return &Schema{Type: "number"}
	}
	panic(fmt.Sprintf("config schema: no schema for a %s", t))
}

// addProperties adds t's keys to s as yamlFields reads them, inline
// structs flattened in.
func addProperties(s *Schema, t reflect.Type) {
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
			addProperties(s, f.Type)
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		prop := schemaOfType(f.Type)
		prop.Description = f.Tag.Get("doc")
		if err := applyTag(prop, f.Tag.Get("schema")); err != nil {
			panic(fmt.Sprintf("config schema: %s.%s: %v", t, f.Name, err))
		}
		if hasOption(f.Tag.Get("schema"), "required") {
			s.Required = append(s.Required, name)
		}
		s.Properties = append(s.Properties, Property{name, prop})
	}
}

func hasOption(tag, option string) bool {
	return slices.Contains(strings.Split(tag, ","), option)
}

// applyTag sets on s what a field's schema tag says; see Schema.
func applyTag(s *Schema, tag string) error {
	if tag == "" {
		return nil
	}
	for _, option := range strings.Split(tag, ",") {
		name, value, _ := strings.Cut(option, "=")
		switch name {
		case "required":
		case "secret":
			s.Secret = true
		case "unit":
			s.Unit = value
		case "kind":
			s.Kind = value
		case "ref":
			itemsOf(s).Ref = value
		case "enum":
			itemsOf(s).Enum = strings.Split(value, "|")
		case "min":
			n, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("min '%s' is not a number", value)
			}
			s.Minimum = &n
		case "default":
			d, err := defaultValue(s.Type, value)
			if err != nil {
				return err
			}
			s.Default = d
		default:
			return fmt.Errorf("unknown schema option '%s'", name)
		}
	}
	return nil
}

// itemsOf returns what enum and ref apply to: a list's items, the items of
// a map's lists, else s.
func itemsOf(s *Schema) *Schema {
	switch {
	case s.Type == "array":
		return s.Items
	case s.AdditionalProperties != nil && s.AdditionalProperties.Type == "array":
		return s.AdditionalProperties.Items
	}
	return s
}

func defaultValue(typ, value string) (any, error) {
	switch typ {
	case "string":
		return value, nil
	case "boolean":
		return strconv.ParseBool(value)
	case "integer":
		return strconv.ParseInt(value, 10, 64)
	case "number":
		return strconv.ParseFloat(value, 64)
	}
	return nil, fmt.Errorf("a %s can't have a default", typ)
}
