package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type schemaSample struct {
	Name    string              `yaml:"name" doc:"A name." schema:"required"`
	Mode    string              `yaml:"mode" doc:"A mode." schema:"enum=on|off,default=on"`
	Seconds *float64            `yaml:"seconds" doc:"A wait." schema:"default=0.5,min=0,unit=seconds"`
	Keys    []string            `yaml:"keys" doc:"Some keys." schema:"ref=keys"`
	Groups  map[string][]string `yaml:"groups" doc:"Some groups." schema:"ref=keys"`
	KeyMap  KeyMap              `yaml:"keyMap" doc:"A key map." schema:"ref=names"`
	Params  yaml.Node           `yaml:"params" doc:"A patch." schema:"kind=params"`
	Access  `yaml:",inline"`
	Hidden  string `yaml:"-"`
}

// The schema has the keys DecodeStrict accepts, in order, inline structs
// flattened in, and each tag's options where they apply: enum and ref on a
// list's items or a map's lists' items, and on another map its keys.
func TestSchemaOf_ReadsTheTags(t *testing.T) {
	s := SchemaOf(schemaSample{})
	var names []string
	for _, p := range s.Properties {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, " "); got != "name mode seconds keys groups keyMap params allowedKeys allowedGroups" {
		t.Errorf("properties: %s", got)
	}
	if strings.Join(s.Required, " ") != "name" {
		t.Errorf("required: %v", s.Required)
	}
	got, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"mode":{"type":"string","description":"A mode.","default":"on","enum":["on","off"]}`,
		`"seconds":{"type":"number","description":"A wait.","default":0.5,"minimum":0,"x-unit":"seconds"}`,
		`"keys":{"type":"array","description":"Some keys.","items":{"type":"string","x-ref":"keys"}}`,
		`"groups":{"type":"object","description":"Some groups.","additionalProperties":{"type":"array","items":{"type":"string","x-ref":"keys"}}}`,
		`"keyMap":{"type":"object","description":"A key map.","additionalProperties":{"type":"string","x-secret":true},"x-ref":"names"}`,
		`"params":{"description":"A patch.","x-kind":"params"}`,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("schema lacks %s:\n%s", want, got)
		}
	}
}

// Every key of the config has a description, so the admin UI can say what
// each one does; the gateway's own test covers the backend and facade
// entries.
func TestSchemaOf_EveryConfigKeyIsDescribed(t *testing.T) {
	var check func(path string, s *Schema)
	check = func(path string, s *Schema) {
		for _, p := range s.Properties {
			if p.Schema.Description == "" {
				t.Errorf("%s%s has no doc tag", path, p.Name)
			}
			check(path+p.Name+".", p.Schema)
		}
		if s.Items != nil {
			check(path, s.Items)
		}
	}
	check("", SchemaOf(Config{}))
}
