package gateway

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// GET /amf/schema describes the config, an entry of each backend type, by
// its type, in the order the README lists them, and each facade type, so
// the admin UI can build its forms from what the gateway decodes. Every key
// has a description.
func TestAdmin_Schema(t *testing.T) {
	a := startAdmin(t, newSwap(t).URL, adminMain, adminSecrets)
	a.expect(t, a.send(t, "GET", "/amf/schema", "secret", ""), http.StatusForbidden, "read by a client")
	r := a.send(t, "GET", "/amf/schema", "bobs-key", "")
	a.expect(t, r, http.StatusOK, "read")
	var types []string
	gjson.Get(r.body, "backendTypes").ForEach(func(name, entry gjson.Result) bool {
		types = append(types, name.String())
		if got := entry.Get("properties.type.enum").String(); got != `["`+name.String()+`"]` {
			t.Errorf("backendTypes.%s.properties.type.enum = %s", name, got)
		}
		return true
	})
	var want []string
	for _, bt := range backendTypes {
		want = append(want, bt.name)
	}
	if !slices.Equal(types, want) {
		t.Errorf("backendTypes: %v, want %v", types, want)
	}
	for path, want := range map[string]string{
		"backendTypes.openWebUi.properties.chats.enum":                                  `["delete","archive","keep"]`,
		"backendTypes.llamaSwap.properties.chats":                                       ``,
		"backendTypes.llamaSwap.properties.apiKey.x-secret":                             `true`,
		"backendTypes.llamaSwap.properties.stallTimeoutSeconds.default":                 `600`,
		"modelTypes.candidates.properties.candidates.items.properties.model.type":       `"string"`,
		"modelTypes.candidates.properties.candidates.items.properties.backend.x-ref":    `"backends"`,
		"config.properties.server.properties.adminApi.properties.configBackups.default": `10`,
		"config.properties.vars.x-kind":                                                 `"anchors"`,
		"protocols.0":                                                                   `"openAiChat"`,
	} {
		if got := gjson.Get(r.body, path).Raw; got != want {
			t.Errorf("%s = %s, want %s", path, got, want)
		}
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(r.body), &schema); err != nil {
		t.Fatal(err)
	}
	var check func(path string, s map[string]any)
	check = func(path string, s map[string]any) {
		props, _ := s["properties"].(map[string]any)
		for name, p := range props {
			p := p.(map[string]any)
			if p["description"] == nil {
				t.Errorf("%s%s has no description", path, name)
			}
			check(path+name+".", p)
		}
		if items, ok := s["items"].(map[string]any); ok {
			check(path, items)
		}
	}
	for _, section := range []string{"backendTypes", "modelTypes"} {
		for name, entry := range schema[section].(map[string]any) {
			check(section+"."+name+".", entry.(map[string]any))
		}
	}
}

// GET /amf/config-anchors lists each anchor with the paths that read it, as
// GET /amf/config shows them, by alias or by merge, so a client can tell
// which values it can't edit in place.
func TestAdmin_Anchors(t *testing.T) {
	main := edit(t, adminMain,
		"backends:\n", "vars:\n  noThinking: &noThinking {chat_template_kwargs: {enable_thinking: false}}\n  common: &common {backend: llamaSwap}\nbackends:\n",
		"  task:\n    backend: llamaSwap\n", "  task:\n    <<: *common\n",
		"        params: {chat_template_kwargs: {enable_thinking: false}}\n      - model: qwen3.8-27b:*", "        params: *noThinking\n      - model: qwen3.8-27b:*",
	)
	a := startAdmin(t, newSwap(t).URL, main, adminSecrets)
	r := a.send(t, "GET", "/amf/config-anchors", "bobs-key", "")
	a.expect(t, r, http.StatusOK, "read")
	if got := gjson.Get(r.body, "anchors.#.name").Raw; got != `["noThinking","common"]` {
		t.Errorf("anchors: %s", r.body)
	}
	if got := gjson.Get(r.body, "anchors.0.users").Raw; got != `[{"path":["models","task","candidates","0","params"],"via":"alias"}]` {
		t.Errorf("noThinking's users: %s", got)
	}
	if got := gjson.Get(r.body, "anchors.1").Raw; got != `{"name":"common","path":["vars","common"],"users":[{"path":["models","task","backend"],"via":"merge"}]}` {
		t.Errorf("common: %s", got)
	}
	if !strings.HasPrefix(r.Header.Get("X-Gateway-Config"), "draft;") {
		t.Errorf("X-Gateway-Config = %s", r.Header.Get("X-Gateway-Config"))
	}
	a.expect(t, a.send(t, "PUT", "/amf/config/vars/noThinking", "alices-key", `{"temperature": 0}`), http.StatusOK, "edit the anchor")
	if got := gjson.Get(a.send(t, "GET", "/amf/config-anchors", "bobs-key", "").body, "anchors.0.users.#").Int(); got != 1 {
		t.Errorf("after an edit of the anchor, it has %d users", got)
	}
	if got := gjson.Get(a.send(t, "GET", "/amf/config/models/task/candidates", "bobs-key", "").body, "0.params.temperature").Raw; got != "0" {
		t.Errorf("the alias doesn't read the edited anchor: temperature = %s", got)
	}
}
