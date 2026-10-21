package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

const minimal = `
server:
  listen: 0.0.0.0:8090
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "http://x"}
models:
  task: {backend: llamaSwap}
`

func TestParseMinimal(t *testing.T) {
	cfg, err := Parse([]byte(minimal), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.TargetHeader != "X-Gateway-Target" {
		t.Errorf("default target header: %q", cfg.Server.TargetHeader)
	}
	entries, err := Entries(&cfg.Backends, "backends")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "llamaSwap" || entries[0].Type != "llamaSwap" {
		t.Errorf("entries: %+v", entries)
	}
}

func TestEnvExpansion(t *testing.T) {
	src := minimal + `
  other:
    backend: llamaSwap
    params:
      key: "${SECRET}"
      port: ${PORT}
      mixed: a-${PORT}-b
`
	cfg, err := Parse([]byte(src), env(map[string]string{"SECRET": "s3", "PORT": "8090"}))
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := Entries(&cfg.Models, "models")
	var params struct {
		Params map[string]any `yaml:"params"`
	}
	if err := entries[1].Node.Decode(&params); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"key": "s3", "port": "8090", "mixed": "a-8090-b"}
	for k, v := range want {
		if params.Params[k] != v {
			t.Errorf("%s: got %#v, want %#v", k, params.Params[k], v)
		}
	}
}

func TestUndefinedEnvIsAnErrorWithPosition(t *testing.T) {
	src := minimal + "  other: {backend: \"${NOPE}\"}\n"
	_, err := Parse([]byte(src), env(nil))
	if err == nil || !strings.Contains(err.Error(), "NOPE") || !strings.Contains(err.Error(), "line 8") {
		t.Errorf("got %v", err)
	}
}

func TestUnknownKeysAreErrors(t *testing.T) {
	cases := map[string]string{
		"top level": minimal + "extra: 1\n",
		"server":    strings.Replace(minimal, "listen:", "listn: x\n  listen:", 1),
		"api key":   strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: a, key: b, scope: c}]", 1),
		"logging":   minimal + "logging: {levl: info}\n",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src), env(nil)); err == nil || !strings.Contains(err.Error(), "unknown key") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestDecodeStrictNested(t *testing.T) {
	type candidate struct {
		Model string `yaml:"model"`
	}
	type entry struct {
		Backend    string      `yaml:"backend"`
		Params     yaml.Node   `yaml:"params"`
		Candidates []candidate `yaml:"candidates"`
	}
	var doc yaml.Node
	src := "backend: b\nparams: {anything: [1, {goes: true}]}\ncandidates:\n  - model: a\n  - modl: b\n"
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	var e entry
	err := DecodeStrict(doc.Content[0], &e)
	if err == nil || !strings.Contains(err.Error(), `line 5: unknown key "modl"`) {
		t.Errorf("got %v", err)
	}
}

func TestDecodeStrictFollowsAnchorsAndMerges(t *testing.T) {
	type limits struct {
		A int `yaml:"a"`
	}
	type root struct {
		Vars yaml.Node `yaml:"vars"`
		X    limits    `yaml:"x"`
		Y    limits    `yaml:"y"`
	}
	src := "vars:\n  base: &base {a: 1, b: 2}\nx: *base\ny: {<<: *base}\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	var r root
	err := DecodeStrict(doc.Content[0], &r)
	if err == nil || !strings.Contains(err.Error(), `unknown key "b"`) {
		t.Errorf("got %v", err)
	}
}

func TestDecodeStrictInlineStruct(t *testing.T) {
	type common struct {
		A int `yaml:"a"`
	}
	type entry struct {
		Common common `yaml:",inline"`
		B      int    `yaml:"b"`
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("a: 1\nb: 2"), &doc); err != nil {
		t.Fatal(err)
	}
	var e entry
	if err := DecodeStrict(doc.Content[0], &e); err != nil {
		t.Errorf("inlined keys rejected: %v", err)
	}
	if err := yaml.Unmarshal([]byte("a: 1\nc: 3"), &doc); err != nil {
		t.Fatal(err)
	}
	if err := DecodeStrict(doc.Content[0], &e); err == nil || !strings.Contains(err.Error(), `unknown key "c"`) {
		t.Errorf("got %v", err)
	}
	// The inline field's own name must not be accepted as a key.
	if err := yaml.Unmarshal([]byte("common: {a: 1}"), &doc); err != nil {
		t.Fatal(err)
	}
	if err := DecodeStrict(doc.Content[0], &e); err == nil || !strings.Contains(err.Error(), `unknown key "common"`) {
		t.Errorf("got %v", err)
	}
}

func TestDecodeStrictInlineMap(t *testing.T) {
	type inner struct {
		X int `yaml:"x"`
	}
	type entry struct {
		B     int              `yaml:"b"`
		Extra map[string]inner `yaml:",inline"`
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("b: 1\nanything: {x: 2}"), &doc); err != nil {
		t.Fatal(err)
	}
	var e entry
	if err := DecodeStrict(doc.Content[0], &e); err != nil {
		t.Errorf("inline map keys rejected: %v", err)
	}
	// Values caught by the inline map are still checked against its type.
	if err := yaml.Unmarshal([]byte("b: 1\nanything: {y: 2}"), &doc); err != nil {
		t.Fatal(err)
	}
	if err := DecodeStrict(doc.Content[0], &e); err == nil || !strings.Contains(err.Error(), `unknown key "y"`) {
		t.Errorf("got %v", err)
	}
}

func TestAPIKeysRequireNameAndKey(t *testing.T) {
	src := strings.Replace(minimal, "listen: 0.0.0.0:8090", "listen: x\n  apiKeys: [{name: a}]", 1)
	if _, err := Parse([]byte(src), env(nil)); err == nil {
		t.Error("expected an error")
	}
}

func TestExampleConfigParses(t *testing.T) {
	if _, err := Load("../../config.example.yaml"); err != nil {
		t.Fatal(err)
	}
}
