package gateway

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// accessConfig has two family keys, one agent key and unlisted keys allowed,
// with llama-swap open only to the family. `shared` has a candidate on each
// backend, `familyOnly` and `agents` limit who may ask for them, and
// `swapOnly` is open to everyone but on a backend only the family may use.
const accessConfig = `
server:
  listen: ":0"
  apiKeys:
    - {name: alice, key: alices-key}
    - {name: bob, key: bobs-key}
    - {name: agent, key: agents-key}
  groups:
    family: [alice, bob]
  unknownKeys: allow
logging: {logSelection: true}
api: {openAi: {addCurrent: true}}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}", allowedGroups: [family]}
  ollama: {type: ollama, baseUrl: "${OLLAMA_URL}"}
models:
  shared:
    backend: llamaSwap
    candidates:
      - model: qwen3.6-35b-a3b:q8_0
      - {model: qwen3:1.7b, backend: ollama}
  familyOnly:
    backend: ollama
    allowedGroups: [family]
    candidates: [{model: qwen3:1.7b}]
  agents:
    backend: ollama
    allowedKeys: [agent]
    allowedGroups: [unknown]
    candidates: [{model: qwen3:1.7b}]
  swapOnly:
    backend: llamaSwap
    candidates: [{model: qwen3.6-35b-a3b:q8_0}]
`

// startAccess serves accessConfig with llama-swap's 35B model ready and
// Ollama's model loaded.
func startAccess(t *testing.T, edits ...string) *gateway {
	t.Helper()
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	ollama := testkit.NewOllama(t, "qwen3:1.7b")
	ollama.SetLoaded("qwen3:1.7b")
	return startGateway(t, edit(t, accessConfig, edits...), map[string]string{"SWAP_URL": swap.URL, "OLLAMA_URL": ollama.URL})
}

// ids returns the values at path in each element of a JSON list.
func ids(body, list, field string) []string {
	var got []string
	for _, v := range gjson.Get(body, list+".#."+field).Array() {
		got = append(got, v.String())
	}
	return got
}

// A facade a caller may not use is in none of that caller's model lists, and
// a request for it gets the 404 of a facade that doesn't exist, so the lists
// and the requests agree and the name isn't confirmed to the caller. With its
// lists left out, a facade is open to everyone, unlisted keys included.
func TestAccess_HidesFacadesACallerMayNotUse(t *testing.T) {
	gw := startAccess(t)
	cases := []struct {
		caller, key string
		want        []string
	}{
		{"family", "alices-key", []string{"shared", "familyOnly", "swapOnly"}},
		{"listed key", "agents-key", []string{"shared", "agents", "swapOnly"}},
		{"unlisted key", "someone-elses-key", []string{"shared", "agents", "swapOnly"}},
		{"no key", "", []string{"shared", "agents", "swapOnly"}},
	}
	for _, c := range cases {
		t.Run(c.caller, func(t *testing.T) {
			lists := map[string]string{"/v1/models": "data", "/anthropic/v1/models": "data", "/amf/models": "models"}
			for path, list := range lists {
				if got := ids(gw.do(t, http.MethodGet, path, c.key, "").body, list, "id"); !slices.Equal(got, c.want) {
					t.Errorf("%s: got %v, want %v", path, got, c.want)
				}
			}
			missing := gw.do(t, http.MethodPost, "/v1/chat/completions", c.key, `{"model":"nope","messages":[]}`)
			for _, name := range []string{"familyOnly", "agents"} {
				if slices.Contains(c.want, name) {
					continue
				}
				r := gw.do(t, http.MethodPost, "/v1/chat/completions", c.key, `{"model":"`+name+`","messages":[]}`)
				if r.StatusCode != http.StatusNotFound || r.body != strings.Replace(missing.body, "nope", name, 1) {
					t.Errorf("chat with %s: got %d %s", name, r.StatusCode, r.body)
				}
				for _, path := range []string{"/v1/models/", "/anthropic/v1/models/", "/amf/models/"} {
					if r := gw.do(t, http.MethodGet, path+name, c.key, ""); r.StatusCode != http.StatusNotFound {
						t.Errorf("%s%s: got %d", path, name, r.StatusCode)
					}
				}
			}
		})
	}
	if out := gw.logs.String(); !strings.Contains(out, `msg="model not allowed for this client" model=familyOnly status=404 client=agent`) {
		t.Errorf("log:\n%s", out)
	}
}

// A candidate on a backend the caller may not use is skipped, and the next
// one serves the caller; the skip is in the log's checked list. With every
// candidate skipped, the request gets 403, which a retry can't change, in
// the shape of the client's API.
func TestAccess_SkipsCandidatesOnBackendsACallerMayNotUse(t *testing.T) {
	gw := startAccess(t)
	if r := gw.do(t, http.MethodPost, "/v1/chat/completions", "alices-key", `{"model":"shared","messages":[]}`); r.target() != "qwen3.6-35b-a3b:q8_0" {
		t.Errorf("family: got %d %q", r.StatusCode, r.target())
	}
	if r := gw.do(t, http.MethodPost, "/v1/chat/completions", "agents-key", `{"model":"shared","messages":[]}`); r.target() != "qwen3:1.7b" {
		t.Errorf("agent: got %d %q %s", r.StatusCode, r.target(), r.body)
	}
	if out := gw.logs.String(); !strings.Contains(out, `checked="[qwen3.6-35b-a3b:q8_0: this client may not use backend 'llamaSwap' qwen3:1.7b on ollama: loaded]"`) {
		t.Errorf("log:\n%s", out)
	}
	r := gw.do(t, http.MethodPost, "/v1/chat/completions", "agents-key", `{"model":"swapOnly","messages":[]}`)
	if r.StatusCode != http.StatusForbidden || gjson.Get(r.body, "error.type").String() != "permission_error" {
		t.Errorf("chat: got %d %s", r.StatusCode, r.body)
	}
	r = gw.do(t, http.MethodPost, "/anthropic/v1/messages", "agents-key", `{"model":"swapOnly","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if r.StatusCode != http.StatusForbidden || gjson.Get(r.body, "type").String() != "error" || gjson.Get(r.body, "error.type").String() != "permission_error" {
		t.Errorf("messages: got %d %s", r.StatusCode, r.body)
	}
}

// `current` and /amf/models describe what this caller's request would do:
// a candidate on a backend the caller may not use is neither the winner nor
// listed, so a caller learns nothing about backends it can't use.
func TestAccess_ListingsShowTheCallersOwnRouting(t *testing.T) {
	gw := startAccess(t)
	cases := []struct {
		key, current string
		candidates   []string
	}{
		{"alices-key", `{"model":"qwen3.6-35b-a3b:q8_0","reason":"loaded"}`, []string{"llamaSwap", "ollama"}},
		{"agents-key", `{"model":"qwen3:1.7b","backend":"ollama","reason":"loaded"}`, []string{"ollama"}},
	}
	for _, c := range cases {
		if got := gjson.Get(gw.do(t, http.MethodGet, "/v1/models/shared", c.key, "").body, "current").Raw; got != c.current {
			t.Errorf("%s: current %s, want %s", c.key, got, c.current)
		}
		amf := gw.do(t, http.MethodGet, "/amf/models/shared", c.key, "").body
		if got := ids(amf, "candidates", "backend"); !slices.Equal(got, c.candidates) || gjson.Get(amf, "current").Raw != c.current {
			t.Errorf("%s: /amf/models %s", c.key, amf)
		}
	}
	if got := gjson.Get(gw.do(t, http.MethodGet, "/v1/models/swapOnly", "agents-key", "").body, "current").Raw; got != `{"reason":"no backend this client may use"}` {
		t.Errorf("no backend the caller may use: current %s", got)
	}
	if amf := gw.do(t, http.MethodGet, "/amf/models/swapOnly", "agents-key", "").body; gjson.Get(amf, "candidates").Exists() || gjson.Get(amf, "current.reason").String() != "no backend this client may use" {
		t.Errorf("no backend the caller may use: /amf/models %s", amf)
	}
}

// An empty list allows no one, unlike a list left out.
func TestAccess_AnEmptyListAllowsNoOne(t *testing.T) {
	gw := startAccess(t, "    allowedGroups: [family]\n", "    allowedGroups: []\n")
	for _, key := range []string{"alices-key", "agents-key", ""} {
		if r := gw.do(t, http.MethodPost, "/v1/chat/completions", key, `{"model":"familyOnly","messages":[]}`); r.StatusCode != http.StatusNotFound {
			t.Errorf("key %q: got %d", key, r.StatusCode)
		}
	}
}

// Access only decides which candidates a caller's request may use; among
// those, selection runs as for everyone. A caller whose allowed candidates
// are all unloaded gets the 503 of no candidate loaded, which may change on a
// retry, not 403, even while a candidate it may not use is ready.
func TestAccess_AllowedCandidatesUnloadedIsNoCandidateLoaded(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	ollama := testkit.NewOllama(t, "qwen3:1.7b")
	gw := startGateway(t, accessConfig, map[string]string{"SWAP_URL": swap.URL, "OLLAMA_URL": ollama.URL})
	swap.Reset()
	r := gw.do(t, http.MethodPost, "/v1/chat/completions", "agents-key", `{"model":"shared","messages":[]}`)
	if r.StatusCode != http.StatusServiceUnavailable || gjson.Get(r.body, "error.type").String() != "no_candidate_loaded" {
		t.Errorf("got %d %s", r.StatusCode, r.body)
	}
	if n := swap.Count("POST /v1/chat/completions"); n != 0 {
		t.Errorf("%d requests reached the backend the caller may not use", n)
	}
}

// A candidate skipped for its protocol and one skipped for access leave the
// same gap: with neither left, the request gets 403, and both reasons are in
// the log. A facade whose candidates can't serve the protocol at all gets 502
// first, as for everyone, whatever the caller may use.
func TestAccess_SkipsForProtocolAndAccessAlike(t *testing.T) {
	claude := testkit.NewAnthropic(t, "claude-a")
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startGateway(t, `
server:
  listen: "127.0.0.1:0"
  apiKeys: [{name: alice, key: alices-key}, {name: agent, key: agents-key}]
  groups: {family: [alice]}
logging: {logSelection: true}
backends:
  claude: {type: anthropic, baseUrl: "`+claude.URL+`", apiKey: k}
  swap: {type: llamaSwap, baseUrl: "`+swap.URL+`", allowedGroups: [family]}
models:
  mixed:
    candidates:
      - {model: claude-a, backend: claude}
      - {model: "qwen3.6-35b-a3b:q8_0", backend: swap}
  onClaude: {backend: claude, candidates: [{model: claude-a}]}
`, nil)
	completion := func(key, model string) response {
		return gw.do(t, http.MethodPost, "/v1/completions", key, `{"model":"`+model+`","prompt":"x"}`)
	}
	if r := completion("agents-key", "mixed"); r.StatusCode != http.StatusForbidden || gjson.Get(r.body, "error.type").String() != "permission_error" {
		t.Errorf("agent, mixed: got %d %s", r.StatusCode, r.body)
	}
	if out := gw.logs.String(); !strings.Contains(out, `checked="[claude-a on claude: its backend can't serve this protocol qwen3.6-35b-a3b:q8_0 on swap: this client may not use backend 'swap']"`) {
		t.Errorf("log:\n%s", out)
	}
	if r := completion("alices-key", "mixed"); r.StatusCode != http.StatusOK || r.target() != "qwen3.6-35b-a3b:q8_0" {
		t.Errorf("alice, mixed: got %d %s", r.StatusCode, r.body)
	}
	if r := completion("agents-key", "onClaude"); r.StatusCode != http.StatusBadGateway {
		t.Errorf("agent, onClaude: got %d %s", r.StatusCode, r.body)
	}
	if r := gw.do(t, http.MethodPost, "/v1/chat/completions", "agents-key", `{"model":"mixed","messages":[{"role":"user","content":"hi"}]}`); r.StatusCode != http.StatusOK || r.target() != "claude-a" {
		t.Errorf("agent, mixed over chat: got %d %s", r.StatusCode, r.body)
	}
}
