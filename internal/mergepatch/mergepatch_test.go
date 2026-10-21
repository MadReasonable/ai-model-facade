package mergepatch

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func compile(t *testing.T, src string) []Patch {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	patches, err := Compile(doc.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	return patches
}

func applyAll(t *testing.T, patches []Patch, body string) string {
	t.Helper()
	out := []byte(body)
	for _, p := range patches {
		var err error
		if out, err = p.Apply(out); err != nil {
			t.Fatal(err)
		}
	}
	return string(out)
}

func jsonEqual(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("invalid JSON %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("got %s, want %s", got, want)
	}
}

// RFC 7386 Appendix A, restricted to object targets and object patches (the
// gateway only ever patches request bodies, which are objects).
func TestRFC7386Examples(t *testing.T) {
	cases := []struct{ target, patch, want string }{
		{`{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"b"}`, `{"b":"c"}`, `{"a":"b","b":"c"}`},
		{`{"a":"b"}`, `{"a":null}`, `{}`},
		{`{"a":"b","b":"c"}`, `{"a":null}`, `{"b":"c"}`},
		{`{"a":["b"]}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"c"}`, `{"a":["b"]}`, `{"a":["b"]}`},
		{`{"a":{"b":"c"}}`, `{"a":{"b":"d","c":null}}`, `{"a":{"b":"d"}}`},
		{`{"a":[{"b":"c"}]}`, `{"a":[1]}`, `{"a":[1]}`},
		{`{"e":null}`, `{"a":1}`, `{"e":null,"a":1}`},
		{`{}`, `{"a":{"bb":{"ccc":null}}}`, `{"a":{"bb":{}}}`},
	}
	for _, c := range cases {
		got := applyAll(t, compile(t, c.patch), c.target)
		jsonEqual(t, got, c.want)
	}
}

func TestBodyBytesPreservedExceptPatchedKeys(t *testing.T) {
	body := `{"model":"task","messages":[{"role":"user","content":"hi"}],"max_tokens":50,"think":false,"tools":[{"type":"function"}],"x_vendor":{"n":1.000000000000000001}}`
	patches := compile(t, `{max_tokens: 1000, think: null, chat_template_kwargs: {enable_thinking: false}}`)
	got := applyAll(t, patches, body)
	want := `{"model":"task","messages":[{"role":"user","content":"hi"}],"max_tokens":1000,"tools":[{"type":"function"}],"x_vendor":{"n":1.000000000000000001},"chat_template_kwargs":{"enable_thinking":false}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestNestedMergeKeepsClientKeys(t *testing.T) {
	body := `{"chat_template_kwargs":{"reasoning_effort":"high","enable_thinking":true}}`
	got := applyAll(t, compile(t, `{chat_template_kwargs: {enable_thinking: false}}`), body)
	want := `{"chat_template_kwargs":{"reasoning_effort":"high","enable_thinking":false}}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestAddedKeysFollowConfigOrder(t *testing.T) {
	got := applyAll(t, compile(t, "zeta: 1\nalpha: 2\nmid: {z: 1, a: 2}\n"), `{}`)
	want := `{"zeta":1,"alpha":2,"mid":{"z":1,"a":2}}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestPathSyntaxKeysAreEscaped(t *testing.T) {
	body := `{"qwen3":1,"qwen3.6":{"a":1}}`
	patches := compile(t, `{"qwen3.6": {"b": 2}, "a*b?": 3, "p|q#": 4, "@x": 5, "0": 6}`)
	got := applyAll(t, patches, body)
	want := `{"qwen3":1,"qwen3.6":{"a":1,"b":2},"a*b?":3,"p|q#":4,"@x":5,"0":6}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// sjson reads a leading ':' as its force-key marker, so such keys need extra
// escaping or they patch the wrong field.
func TestLeadingColonKeysAreEscaped(t *testing.T) {
	body := `{":0":1,"0":2,":x":{"a":1}}`
	patches := compile(t, `{":0": null, ":x": {"b": 2}, ":new": 3}`)
	got := applyAll(t, patches, body)
	want := `{"0":2,":x":{"a":1,"b":2},":new":3}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestNumericKeyInsideNewObject(t *testing.T) {
	got := applyAll(t, compile(t, `{n: {"0": 1, "x": null}}`), `{}`)
	want := `{"n":{"0":1}}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestListFormAppliesInOrder(t *testing.T) {
	src := `
- chat_template_kwargs: {enable_thinking: true, reasoning_effort: low}
- thinking_budget_tokens: 2048
  chat_template_kwargs: {reasoning_effort: null}
`
	got := applyAll(t, compile(t, src), `{}`)
	want := `{"chat_template_kwargs":{"enable_thinking":true},"thinking_budget_tokens":2048}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestAnchorsResolve(t *testing.T) {
	src := `
vars:
  noThinking: &noThinking
    chat_template_kwargs: {enable_thinking: false}
params:
  - *noThinking
  - max_tokens: 10
`
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	patches, err := Compile(doc.Content[0].Content[3])
	if err != nil {
		t.Fatal(err)
	}
	got := applyAll(t, patches, `{}`)
	want := `{"chat_template_kwargs":{"enable_thinking":false},"max_tokens":10}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestLiteralsKeepTypesAndOrder(t *testing.T) {
	got := applyAll(t, compile(t, `{s: "1", i: 1, f: 1.5, b: true, arr: [null, {z: 1, a: 2}], html: "<a&b>"}`), `{}`)
	want := `{"s":"1","i":1,"f":1.5,"b":true,"arr":[null,{"z":1,"a":2}],"html":"<a&b>"}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestCompileErrors(t *testing.T) {
	cases := map[string]string{
		"scalar params":   `5`,
		"list of scalars": `[1]`,
		"merge key":       "base: &b {x: 1}\nother: {<<: *b}\n",
		"duplicate key":   "{a: 1, a: 2}",
		"empty key":       `{"": 1}`,
		"nan":             `{a: .nan}`,
	}
	for name, src := range cases {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
			continue // the YAML parser already rejects it
		}
		n := doc.Content[0]
		if name == "merge key" {
			n = n.Content[3]
		}
		if _, err := Compile(n); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestNullAndMissingParamsYieldNothing(t *testing.T) {
	if p, err := Compile(&yaml.Node{}); err != nil || p != nil {
		t.Errorf("missing: %v %v", p, err)
	}
	if p := compile(t, `~`); p != nil {
		t.Errorf("null: %v", p)
	}
}

func TestStringKeepsNulls(t *testing.T) {
	p := compile(t, `{max_tokens: 1000, think: null, k: {a: null}}`)
	if got := p[0].String(); got != `{"max_tokens":1000,"think":null,"k":{"a":null}}` {
		t.Errorf("got %s", got)
	}
}

func TestCompose(t *testing.T) {
	route := compile(t, `{max_tokens: 1000, think: null, chat_template_kwargs: {a: 1}}`)
	cand := compile(t, `{chat_template_kwargs: {enable_thinking: false}, max_tokens: 5}`)
	all := append(append([]Patch{}, route...), cand...)
	c, ok := Compose(all)
	if !ok {
		t.Fatal("expected exact composition")
	}
	if got := c.String(); got != `{"max_tokens":5,"think":null,"chat_template_kwargs":{"a":1,"enable_thinking":false}}` {
		t.Errorf("got %s", got)
	}
	// The composed patch must behave exactly like the sequence.
	for _, body := range []string{`{}`, `{"think":true,"chat_template_kwargs":{"b":2}}`, `{"chat_template_kwargs":7}`} {
		if seq, one := applyAll(t, all, body), applyAll(t, []Patch{c}, body); seq != one {
			t.Errorf("body %s: sequence %s, composed %s", body, seq, one)
		}
	}
}

func TestComposeInexact(t *testing.T) {
	for _, src := range []string{"- {k: 5}\n- {k: {a: 1}}\n", "- {k: null}\n- {k: {a: 1}}\n"} {
		if _, ok := Compose(compile(t, src)); ok {
			t.Errorf("%q: expected inexact composition", strings.TrimSpace(src))
		}
	}
}
