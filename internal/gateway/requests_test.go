package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// A request is forwarded with the facade's and then the winning candidate's
// params merged in and the model set to the winner, keys in their original
// order; the response comes back unchanged, naming the winner in
// X-Gateway-Target.
func TestRequests_RewriteTheBodyAndForward(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startTask(t, swap)
	r := gw.chat(t, `{"model":"task","messages":[{"role":"user","content":"title this"}],"stream":false,"think":false,"chat_template_kwargs":{"enable_thinking":true,"x":1},"response_format":{"type":"json_object"}}`)
	if r.StatusCode != http.StatusOK || r.body != testkit.DefaultBody || r.target() != "qwen3.6-35b-a3b:q8_0" {
		t.Fatalf("got %d %q %s", r.StatusCode, r.target(), r.body)
	}
	want := `{"model":"qwen3.6-35b-a3b:q8_0","messages":[{"role":"user","content":"title this"}],"stream":false,"chat_template_kwargs":{"enable_thinking":false,"x":1},"response_format":{"type":"json_object"},"max_tokens":1000}`
	if req, _ := swap.Last("POST /v1/chat/completions"); req.Body != want {
		t.Errorf("forwarded\n got  %s\n want %s", req.Body, want)
	}
}

// Legacy completions go through the same selection and params, to the
// backend's /v1/completions.
func TestRequests_ForwardLegacyCompletionsToTheirEndpoint(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startTask(t, swap)
	r := gw.do(t, http.MethodPost, "/v1/completions", "secret", `{"model":"task","prompt":"Title: "}`)
	if r.StatusCode != http.StatusOK || r.target() != "qwen3.6-35b-a3b:q8_0" {
		t.Fatalf("got %d %s", r.StatusCode, r.body)
	}
	if req, ok := swap.Last("POST /v1/completions"); !ok || req.Body != `{"model":"qwen3.6-35b-a3b:q8_0","prompt":"Title: ","max_tokens":1000,"chat_template_kwargs":{"enable_thinking":false}}` {
		t.Errorf("forwarded %+v", req)
	}
}

// A candidate whose backend can't serve the request's protocol is skipped,
// even if it is ready, and the skip is logged at error level. With no candidate
// left the request gets 502 naming the protocols, and reaches no backend.
func TestRequests_SkipACandidateThatCantServeTheProtocol(t *testing.T) {
	claude := testkit.NewAnthropic(t, "claude-a")
	webui := testkit.NewOpenWebUI(t, testkit.OllamaEntry("qwen3:1.7b", true))
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startGateway(t, `
server: {listen: "127.0.0.1:0"}
backends:
  claude: {type: anthropic, baseUrl: "`+claude.URL+`", apiKey: k}
  webui: {type: openWebUi, baseUrl: "`+webui.URL+`", apiKey: k}
  swap: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
models:
  mixed:
    candidates:
      - {model: claude-a, backend: claude}
      - {model: "qwen3.6-35b-a3b:q8_0", backend: swap}
  onClaude: {backend: claude, candidates: [{model: claude-a}]}
  onWebui: {backend: webui, candidates: [{model: "qwen3:1.7b"}]}
`, nil)
	r := gw.do(t, http.MethodPost, "/v1/completions", "", `{"model":"mixed","prompt":"x"}`)
	if r.StatusCode != http.StatusOK || r.target() != "qwen3.6-35b-a3b:q8_0" {
		t.Errorf("mixed: got %d %s", r.StatusCode, r.body)
	}
	gw.logs.waitFor(t, "level=ERROR msg=\"candidate skipped; its backend can't serve this protocol\" model=mixed candidate=claude-a backend=claude protocol=openAiCompletions")
	for _, facade := range []string{"onClaude", "onWebui"} {
		r := gw.do(t, http.MethodPost, "/v1/completions", "", `{"model":"`+facade+`","prompt":"x"}`)
		if r.StatusCode != http.StatusBadGateway || !strings.Contains(r.body, "no candidate of this model can serve openAiCompletions") {
			t.Errorf("%s: got %d %s", facade, r.StatusCode, r.body)
		}
	}
	if n := claude.Count("POST /v1/messages"); n != 0 {
		t.Errorf("%d requests reached the Claude API, want none", n)
	}
	if n := webui.Count("POST /api/v1/chats/new"); n != 0 {
		t.Errorf("%d chats created on Open WebUI, want none", n)
	}
}

// The OpenAI family answers under its base path and, as the default family,
// at the root.
func TestRequests_ServeTheOpenAIFamilyUnderItsBasePathAndTheRoot(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startGateway(t, `
server: {listen: "127.0.0.1:0"}
api: {openAi: {basePath: /oa}}
backends:
  swap: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
models:
  task: {backend: swap, candidates: [{model: "qwen3.6-35b-a3b:q8_0"}]}
`, nil)
	for _, base := range []string{"", "/oa"} {
		if r := gw.do(t, http.MethodPost, base+"/v1/chat/completions", "", `{"model":"task"}`); r.StatusCode != http.StatusOK {
			t.Errorf("%s chat: got %d %s", base, r.StatusCode, r.body)
		}
		if r := gw.get(t, base+"/v1/models/task"); r.StatusCode != http.StatusOK {
			t.Errorf("%s model: got %d %s", base, r.StatusCode, r.body)
		}
	}
	if r := gw.get(t, "/openai/v1/models"); r.StatusCode != http.StatusNotFound {
		t.Errorf("the default base path, renamed: got %d", r.StatusCode)
	}
}

// A Chat Completions client reaches a Claude candidate through the direct
// translator, and the params for it are in Messages spelling, patching the
// translated body: thinking stays as written, and a Chat Completions-only key
// in params would reach the API raw, so it is warned about at load.
func TestRequests_TranslateForABackendOnAnotherProtocol(t *testing.T) {
	claude := testkit.NewAnthropic(t, "claude-a")
	gw := startGateway(t, `
server: {listen: "127.0.0.1:0"}
backends:
  claude: {type: anthropic, baseUrl: "`+claude.URL+`", apiKey: k}
models:
  ask:
    backend: claude
    params:
      openAiChat: {ignored: true}
      anthropicMessages: {thinking: {type: adaptive}, stop: [x]}
    candidates: [{model: claude-a}]
`, nil)
	r := gw.chat(t, `{"model":"ask","max_tokens":5,"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`)
	if r.StatusCode == http.StatusOK || !strings.Contains(r.body, "stop") {
		t.Errorf("a Chat Completions key in Messages params: got %d %s", r.StatusCode, r.body)
	}
	if !strings.Contains(gw.logs.String(), "params set a Chat Completions key on a candidate that receives Messages") {
		t.Errorf("no warning at load: %s", gw.logs.String())
	}
	req, ok := claude.Last("POST /v1/messages")
	if want := `{"model":"claude-a","max_tokens":5,"system":[{"type":"text","text":"s"}],"messages":[{"role":"user","content":[{"type":"text","text":"u"}]}],"thinking":{"type":"adaptive"},"stop":["x"]}`; !ok || req.Body != want {
		t.Errorf("sent %+v, want %s", req, want)
	}
}

// claudeGateway serves a facade on a fake Claude API, with this api block.
func claudeGateway(t *testing.T, api string) (*gateway, *testkit.Anthropic) {
	t.Helper()
	claude := testkit.NewAnthropic(t, "claude-a")
	return startGateway(t, `
server:
  listen: "127.0.0.1:0"
  apiKeys: [{name: alice, key: secret}]
api: `+api+`
backends:
  claude: {type: anthropic, baseUrl: "`+claude.URL+`", apiKey: k}
models:
  ask:
    backend: claude
    params: {anthropicMessages: {thinking: {type: adaptive}}}
    candidates: [{model: claude-a}]
  chatOnly: {backend: claude, protocols: [openAiChat], candidates: [{model: claude-a}]}
`, nil), claude
}

// messagesRequest posts to the Messages endpoint as Claude's clients do, with
// the key in x-api-key.
func messagesRequest(t *testing.T, gw *gateway, path, key, body string, header map[string]string) response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, gw.URL+path, strings.NewReader(body))
	req.Header.Set("x-api-key", key)
	for name, value := range header {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return response{resp, string(data)}
}

// A Messages client reaches a Messages backend with nothing parsed: its keys
// and numbers stay as sent, only params and the model change, its version and
// beta headers go with it, and the response comes back unchanged.
func TestMessages_ForwardsARequestAndItsHeaders(t *testing.T) {
	gw, claude := claudeGateway(t, "{}")
	body := `{"max_tokens":5,"temperature":0.50,"model":"ask","messages":[{"role":"user","content":"hi"}]}`
	r := messagesRequest(t, gw, "/anthropic/v1/messages", "secret", body, map[string]string{"anthropic-version": "2099-01-01", "anthropic-beta": "some-beta", "x-other": "dropped"})
	if r.StatusCode != http.StatusOK || gjson.Get(r.body, "type").String() != "message" {
		t.Fatalf("got %d %s", r.StatusCode, r.body)
	}
	req, ok := claude.Last("POST /v1/messages")
	if want := `{"max_tokens":5,"temperature":0.50,"model":"claude-a","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"}}`; !ok || req.Body != want {
		t.Errorf("sent %+v, want %s", req, want)
	}
	if h := req.Header; h.Get("Anthropic-Version") != "2099-01-01" || h.Get("Anthropic-Beta") != "some-beta" || h.Get("X-Other") != "" || h.Get("X-Api-Key") != "" {
		t.Errorf("headers %v", h)
	}
}

// A stream keeps Messages' framing: each event with its event line, and no
// [DONE] after message_stop.
func TestMessages_StreamsInItsOwnFraming(t *testing.T) {
	gw, _ := claudeGateway(t, "{}")
	r := messagesRequest(t, gw, "/anthropic/v1/messages", "secret", `{"model":"ask","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("got %d %s", r.StatusCode, r.body)
	}
	if !strings.HasPrefix(r.body, "event: message_start\ndata: {") || !strings.HasSuffix(r.body, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") || strings.Contains(r.body, "[DONE]") {
		t.Errorf("got %s", r.body)
	}
}

// A stream that breaks ends with a Messages error event, the API's own.
func TestMessages_EndsABrokenStreamWithAnErrorEvent(t *testing.T) {
	gw, claude := claudeGateway(t, "{}")
	claude.SetStreamError(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
	r := messagesRequest(t, gw, "/anthropic/v1/messages", "secret", `{"model":"ask","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if !strings.HasSuffix(r.body, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n") {
		t.Errorf("got %s", r.body)
	}
}

// The gateway's own errors reach a Messages client in Anthropic's shape.
func TestMessages_ErrorsAreInAnthropicsShape(t *testing.T) {
	gw, _ := claudeGateway(t, "{}")
	cases := []struct {
		name, key, body string
		status          int
		errType         string
	}{
		{"wrong key", "wrong", `{"model":"ask"}`, http.StatusUnauthorized, "authentication_error"},
		{"unknown facade", "secret", `{"model":"nope"}`, http.StatusNotFound, "not_found_error"},
		{"facade narrowed away", "secret", `{"model":"chatOnly"}`, http.StatusNotFound, "not_found_error"},
		{"malformed body", "secret", `[1]`, http.StatusBadRequest, "invalid_request_error"},
	}
	for _, c := range cases {
		r := messagesRequest(t, gw, "/anthropic/v1/messages", c.key, c.body, nil)
		if r.StatusCode != c.status || gjson.Get(r.body, "type").String() != "error" || gjson.Get(r.body, "error.type").String() != c.errType {
			t.Errorf("%s: got %d %s", c.name, r.StatusCode, r.body)
		}
	}
}

// A wrong path or method gets Anthropic's error shape with no key, as its API
// does, and the family's routes still work beside those answers.
func TestMessages_WrongPathsAndMethodsAreInAnthropicsShape(t *testing.T) {
	cases := []struct {
		config, method, path string
		status               int
		errType, allow       string
	}{
		{"{}", http.MethodGet, "/anthropic/v1/nothing", http.StatusNotFound, "not_found_error", ""},
		{"{}", http.MethodPost, "/anthropic/v1/chat/completions", http.StatusNotFound, "not_found_error", ""},
		{"{}", http.MethodPost, "/anthropic/messages", http.StatusNotFound, "not_found_error", ""},
		{"{}", http.MethodGet, "/anthropic", http.StatusNotFound, "not_found_error", ""},
		{"{}", http.MethodGet, "/anthropic/v1/messages", http.StatusMethodNotAllowed, "invalid_request_error", "POST"},
		{"{}", http.MethodGet, "/anthropic/v1/messages/count_tokens", http.StatusMethodNotAllowed, "invalid_request_error", "POST"},
		{"{}", http.MethodPost, "/anthropic/v1/models", http.StatusMethodNotAllowed, "invalid_request_error", "GET, HEAD"},
		{"{}", http.MethodDelete, "/anthropic/v1/models/ask", http.StatusMethodNotAllowed, "invalid_request_error", "GET, HEAD"},
		{"{default: anthropic}", http.MethodPost, "/v1/chat/completions", http.StatusNotFound, "not_found_error", ""},
		{"{default: anthropic}", http.MethodGet, "/v1/messages", http.StatusMethodNotAllowed, "invalid_request_error", "POST"},
	}
	for _, c := range cases {
		gw, _ := claudeGateway(t, c.config)
		r := gw.do(t, c.method, c.path, "", "")
		if r.StatusCode != c.status || gjson.Get(r.body, "type").String() != "error" || gjson.Get(r.body, "error.type").String() != c.errType || r.Header.Get("Allow") != c.allow {
			t.Errorf("%s %s with %s: got %d, Allow '%s', %s", c.method, c.path, c.config, r.StatusCode, r.Header.Get("Allow"), r.body)
		}
	}
	gw, _ := claudeGateway(t, "{default: anthropic}")
	if r := gw.do(t, http.MethodGet, "/nothing", "", ""); r.StatusCode != http.StatusNotFound || gjson.Get(r.body, "type").Exists() {
		t.Errorf("path outside /v1 at the root: got %d %s", r.StatusCode, r.body)
	}
	if r := gw.do(t, http.MethodGet, "/health", "", ""); r.StatusCode != http.StatusOK {
		t.Errorf("health: got %d %s", r.StatusCode, r.body)
	}
	if r := gw.do(t, http.MethodGet, "/anthropic/v1/models", "secret", ""); r.StatusCode != http.StatusOK {
		t.Errorf("listing: got %d %s", r.StatusCode, r.body)
	}
}

// Anthropic's listing has its own shape: every facade the family serves, on one
// page, with the facade's name for its ID and display name, and its
// candidate's output cap as max_tokens.
func TestMessages_ListsFacadesInAnthropicsShape(t *testing.T) {
	gw, _ := claudeGateway(t, "{}")
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/anthropic/v1/models", nil)
	req.Header.Set("x-api-key", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	list := gjson.ParseBytes(data)
	if list.Get("has_more").Bool() || list.Get("first_id").String() != "ask" || list.Get("last_id").String() != "ask" || list.Get("data.#").Int() != 1 {
		t.Fatalf("got %s", data)
	}
	one := list.Get("data.0")
	if one.Get("type").String() != "model" || one.Get("display_name").String() != "ask" || !one.Get("created_at").Exists() || one.Get("max_tokens").Int() != testkit.OutputCap || one.Get("current").Exists() {
		t.Errorf("got %s", one.Raw)
	}
}

// With anthropic as the default family, the root serves it instead of OpenAI's.
func TestMessages_ServesTheRootAsTheDefaultFamily(t *testing.T) {
	gw, _ := claudeGateway(t, "{default: anthropic}")
	body := `{"model":"ask","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
	if r := messagesRequest(t, gw, "/v1/messages", "secret", body, nil); r.StatusCode != http.StatusOK {
		t.Errorf("root messages: got %d %s", r.StatusCode, r.body)
	}
	if r := gw.do(t, http.MethodPost, "/v1/chat/completions", "secret", body); r.StatusCode != http.StatusNotFound {
		t.Errorf("root chat: got %d", r.StatusCode)
	}
	if r := gw.do(t, http.MethodPost, "/openai/v1/chat/completions", "secret", body); r.StatusCode != http.StatusOK {
		t.Errorf("based chat: got %d %s", r.StatusCode, r.body)
	}
}

// A Messages client reaches a Chat Completions backend through the translator:
// params for that candidate patch the chat body, in their Chat Completions
// spelling, and the reply comes back as a message, whole or streamed.
func TestMessages_ReachesAChatCompletionsBackend(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startGateway(t, `
server: {listen: "127.0.0.1:0"}
backends:
  swap: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
models:
  ask:
    backend: swap
    params: {openAiChat: {chat_template_kwargs: {enable_thinking: false}}, anthropicMessages: {ignored: true}}
    candidates: [{model: "qwen3.6-35b-a3b:q8_0"}]
`, nil)
	body := `{"model":"ask","max_tokens":5,"system":"s","messages":[{"role":"user","content":"hi"}]}`
	r := messagesRequest(t, gw, "/anthropic/v1/messages", "", body, nil)
	if r.StatusCode != http.StatusOK || gjson.Get(r.body, "type").String() != "message" || r.target() != "qwen3.6-35b-a3b:q8_0" {
		t.Fatalf("whole: got %d %s", r.StatusCode, r.body)
	}
	req, ok := swap.Last("POST /v1/chat/completions")
	if want := `{"model":"qwen3.6-35b-a3b:q8_0","max_tokens":5,"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}],"chat_template_kwargs":{"enable_thinking":false}}`; !ok || req.Body != want {
		t.Errorf("sent %+v, want %s", req, want)
	}
	swap.SetReply(testkit.Reply{Stream: []string{`{"id":"c","model":"m","choices":[{"delta":{"content":"hi"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`}})
	r = messagesRequest(t, gw, "/anthropic/v1/messages", "", `{"model":"ask","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if r.StatusCode != http.StatusOK || !strings.HasPrefix(r.body, "event: message_start\n") || !strings.Contains(r.body, "event: message_stop\n") || strings.Contains(r.body, "[DONE]") {
		t.Errorf("stream: got %d %s", r.StatusCode, r.body)
	}
}

// A count goes through selection like a chat, to the backend's own count where
// it has one, with generation-only fields out, since the Claude API refuses
// them. A backend that can only tokenize text has the text counted there and
// the rest estimated, and one that can't count gets an estimate; both are
// marked as estimates.
func TestMessages_CountsTokensOnTheSelectedBackend(t *testing.T) {
	claude := testkit.NewAnthropic(t, "claude-a")
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	server := testkit.NewOllama(t, "a:1")
	local := testkit.NewLocalAI(t, 0, "m")
	gw := startGateway(t, `
server: {listen: "127.0.0.1:0"}
backends:
  claude: {type: anthropic, baseUrl: "`+claude.URL+`", apiKey: k}
  swap: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
  ollama: {type: ollama, baseUrl: "`+server.URL+`"}
  localAi: {type: localAi, baseUrl: "`+local.URL+`"}
models:
  onClaude:
    backend: claude
    params: {all: {max_tokens: 9}, anthropicMessages: {thinking: {type: adaptive}}}
    candidates: [{model: claude-a}]
  onSwap: {backend: swap, candidates: [{model: "qwen3.6-35b-a3b:q8_0"}]}
  onOllama: {backend: ollama, candidates: [{model: "a:1", loadIfNone: true}]}
  onLocalAi: {backend: localAi, candidates: [{model: m, loadIfNone: true}]}
`, nil)
	body := func(facade string) string {
		return `{"model":"` + facade + `","system":"` + strings.Repeat("s", 400) + `","messages":[{"role":"user","content":"hi"}]}`
	}
	want := `{"input_tokens":42}`
	r := messagesRequest(t, gw, "/anthropic/v1/messages/count_tokens", "", body("onClaude"), nil)
	if r.StatusCode != http.StatusOK || r.body != want || r.Header.Get("X-Gateway-Estimated") != "" {
		t.Errorf("Claude: got %d %s", r.StatusCode, r.body)
	}
	sent, ok := claude.Last("POST /v1/messages/count_tokens")
	if !ok || gjson.Get(sent.Body, "model").String() != "claude-a" || gjson.Get(sent.Body, "thinking.type").String() != "adaptive" || gjson.Get(sent.Body, "max_tokens").Exists() {
		t.Errorf("Claude was sent %+v", sent)
	}
	if n := claude.Count("POST /v1/messages"); n != 0 {
		t.Errorf("%d chats reached the API, want none", n)
	}
	r = messagesRequest(t, gw, "/anthropic/v1/messages/count_tokens", "", body("onSwap"), nil)
	if r.StatusCode != http.StatusOK || r.body != want || r.target() != "qwen3.6-35b-a3b:q8_0" {
		t.Errorf("llama-swap: got %d %s", r.StatusCode, r.body)
	}
	r = messagesRequest(t, gw, "/anthropic/v1/messages/count_tokens", "", body("onOllama"), nil)
	if r.StatusCode != http.StatusOK || gjson.Get(r.body, "input_tokens").Int() != 113 || r.Header.Get("X-Gateway-Estimated") != "true" {
		t.Errorf("Ollama: got %d %s", r.StatusCode, r.body)
	}
	if n := server.Count("POST /v1/messages/count_tokens") + server.Count("POST /v1/chat/completions"); n != 0 {
		t.Errorf("an estimate made %d requests to Ollama, want none", n)
	}
	r = messagesRequest(t, gw, "/anthropic/v1/messages/count_tokens", "", body("onLocalAi"), nil)
	if r.StatusCode != http.StatusOK || gjson.Get(r.body, "input_tokens").Int() != 15 || r.Header.Get("X-Gateway-Estimated") != "true" {
		t.Errorf("LocalAI: got %d %s", r.StatusCode, r.body)
	}
	if sent, ok := local.Last("POST /v1/tokenize"); !ok || gjson.Get(sent.Body, "model").String() != "m" || gjson.Get(sent.Body, "content").String() != strings.Repeat("s", 400)+"\nhi" {
		t.Errorf("LocalAI was sent %+v", sent)
	}
}

// A translation names the client's fields it left out, in a header and in the
// log. A route with no translation leaves nothing out, so it has no header, and
// a field converted to a counterpart isn't reported.
func TestRequests_ReportWhatATranslationDropped(t *testing.T) {
	claude := testkit.NewAnthropic(t, "claude-a")
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startGateway(t, `
server: {listen: "127.0.0.1:0"}
logging: {logSelection: true}
backends:
  claude: {type: anthropic, baseUrl: "`+claude.URL+`", apiKey: k}
  swap: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
models:
  onClaude: {backend: claude, candidates: [{model: claude-a}]}
  onSwap: {backend: swap, candidates: [{model: "qwen3.6-35b-a3b:q8_0"}]}
`, nil)
	const header = "X-Gateway-Dropped"
	r := messagesRequest(t, gw, "/anthropic/v1/messages", "", `{"model":"onSwap","max_tokens":5,"thinking":{"type":"adaptive"},"output_config":{"effort":"low","task_budget":1},"metadata":{"user_id":"u","other":1},"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`, nil)
	if got := r.Header.Get(header); r.StatusCode != http.StatusOK || got != "thinking, output_config.task_budget, metadata.other, cache_control" {
		t.Errorf("Messages to Chat Completions: got %d, %q", r.StatusCode, got)
	}
	gw.logs.waitFor(t, "dropped=")
	r = gw.chat(t, `{"model":"onClaude","max_tokens":5,"seed":1,"presence_penalty":0.5,"stop":["x"],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a","reasoning_content":"r"},{"role":"user","content":"more"}]}`)
	if got := r.Header.Get(header); r.StatusCode != http.StatusOK || got != "reasoning_content, seed, presence_penalty" {
		t.Errorf("Chat Completions to Messages: got %d, %q: %s", r.StatusCode, got, r.body)
	}
	r = gw.chat(t, `{"model":"onSwap","seed":1,"messages":[]}`)
	if got := r.Header.Get(header); r.StatusCode != http.StatusOK || got != "" {
		t.Errorf("no translation: got %d, %q", r.StatusCode, got)
	}
	r = messagesRequest(t, gw, "/anthropic/v1/messages", "", `{"model":"onClaude","max_tokens":5,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`, nil)
	if got := r.Header.Get(header); r.StatusCode != http.StatusOK || got != "" {
		t.Errorf("Messages to Claude: got %d, %q", r.StatusCode, got)
	}
}

// A params map of protocol blocks patches each protocol in its own spelling:
// `all` first, then the protocol's block, at both levels, then the model.
func TestRequests_ApplyTheParamsBlockForTheProtocol(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startGateway(t, `
server: {listen: "127.0.0.1:0"}
backends:
  swap: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
models:
  blocks:
    backend: swap
    params:
      all: {max_tokens: 1}
      openAiChat: {facadeChat: true}
      openAiCompletions: {facadeLegacy: true}
    candidates:
      - model: qwen3.6-35b-a3b:q8_0
        params:
          all: {max_tokens: 2}
          openAiChat: [{candidateChat: true}]
`, nil)
	for path, want := range map[string]string{
		"/v1/chat/completions": `{"model":"qwen3.6-35b-a3b:q8_0","max_tokens":2,"facadeChat":true,"candidateChat":true}`,
		"/v1/completions":      `{"model":"qwen3.6-35b-a3b:q8_0","max_tokens":2,"facadeLegacy":true}`,
	} {
		if r := gw.do(t, http.MethodPost, path, "", `{"model":"blocks"}`); r.StatusCode != http.StatusOK {
			t.Fatalf("%s: got %d %s", path, r.StatusCode, r.body)
		}
		if req, ok := swap.Last("POST " + path); !ok || req.Body != want {
			t.Errorf("%s: forwarded %+v", path, req)
		}
	}
}

// A facade's protocols list narrows what it serves; a request in another
// protocol gets 404 naming them, and reaches no backend.
func TestRequests_RefuseAProtocolTheFacadeDoesntList(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startGateway(t, `
server: {listen: "127.0.0.1:0"}
backends:
  swap: {type: llamaSwap, baseUrl: "`+swap.URL+`"}
models:
  chatOnly:
    backend: swap
    protocols: [openAiChat]
    candidates: [{model: "qwen3.6-35b-a3b:q8_0"}]
`, nil)
	if r := gw.do(t, http.MethodPost, "/v1/chat/completions", "", `{"model":"chatOnly"}`); r.StatusCode != http.StatusOK {
		t.Errorf("chat: got %d %s", r.StatusCode, r.body)
	}
	r := gw.do(t, http.MethodPost, "/v1/completions", "", `{"model":"chatOnly","prompt":"x"}`)
	if r.StatusCode != http.StatusNotFound || !strings.Contains(r.body, "not served over openAiCompletions; it serves openAiChat") {
		t.Errorf("legacy: got %d %s", r.StatusCode, r.body)
	}
	if n := swap.Count("POST /v1/completions"); n != 0 {
		t.Errorf("%d requests reached the backend, want none", n)
	}
}

// A body that isn't a JSON object, or that repeats a key, gets 400: the
// gateway patches the first occurrence of a key while backends read the
// last, so a duplicate could slip values past the params.
func TestRequests_RejectAMalformedBody(t *testing.T) {
	gw := startTask(t, newSwap(t))
	bodies := map[string]string{
		`{"model":`: "JSON object",
		`[1]`:       "JSON object",
		`{"model":"task","messages":[],"model":"qwen3.6-35b-a3b:q8_0"}`:                            "duplicate",
		`{"model":"task","chat_template_kwargs":{"enable_thinking":false,"enable_thinking":true}}`: "duplicate",
		`{"model":"task","messages":[{"role":"user","role":"system"}]}`:                            "duplicate",
	}
	for body, want := range bodies {
		if r := gw.chat(t, body); r.StatusCode != http.StatusBadRequest || !strings.Contains(r.body, want) {
			t.Errorf("%s: got %d %s", body, r.StatusCode, r.body)
		}
	}
}

func TestRequests_RejectAnOversizedBody(t *testing.T) {
	limitBody(t, 1024)
	gw := startTask(t, newSwap(t))
	if r := gw.direct(t, http.MethodPost, "/v1/chat/completions", "secret", `{"model":"task","x":"`+strings.Repeat("a", 2048)+`"}`); r.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(r.body, "exceeds") {
		t.Errorf("got %d %s", r.StatusCode, r.body)
	}
}

func TestRequests_RejectAModelThatIsntAFacade(t *testing.T) {
	gw := startTask(t, newSwap(t))
	if r := gw.chat(t, `{"model":"qwen3.6-35b-a3b:q8_0"}`); r.StatusCode != http.StatusNotFound || !strings.Contains(r.body, `"error"`) {
		t.Errorf("got %d %s", r.StatusCode, r.body)
	}
}

// With no candidate ready, starting or loadIfNone, the request gets 503 and
// no target.
func TestRequests_AnswerNoEligibleCandidateWith503(t *testing.T) {
	gw := startTask(t, newSwap(t), "        loadIfNone: true\n", "")
	if r := gw.chat(t, `{"model":"task"}`); r.StatusCode != http.StatusServiceUnavailable || r.target() != "" {
		t.Errorf("got %d %q %s", r.StatusCode, r.target(), r.body)
	}
}

// A backend's refusal reaches the client with its status, body and
// Retry-After, and no other backend header.
func TestRequests_PassABackendRefusalOn(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	swap.SetReply(testkit.Reply{Status: http.StatusTooManyRequests, Header: map[string]string{"Retry-After": "7", "Server": "llama-swap"}, Body: `{"error":"concurrency limit reached"}`})
	r := startTask(t, swap).chat(t, `{"model":"task"}`)
	if r.StatusCode != http.StatusTooManyRequests || !strings.Contains(r.body, "concurrency limit reached") || r.Header.Get("Retry-After") != "7" || r.Header.Get("Server") != "" {
		t.Errorf("got %d, Retry-After %q, Server %q: %s", r.StatusCode, r.Header.Get("Retry-After"), r.Header.Get("Server"), r.body)
	}
}

// Requests refused before selection get a log line of their own, naming the
// client or the fingerprint of an unlisted key, never the key.
func TestRequests_LogRejections(t *testing.T) {
	limitBody(t, 1024)
	gw := startTask(t, newSwap(t))
	sum := sha256.Sum256([]byte("wrong-key"))
	cases := []struct{ key, body, want string }{
		{"", `{"model":"task"}`, `path=/v1/chat/completions status=401 error="invalid or missing API key"`},
		{"wrong-key", `{"model":"task"}`, "status=401 fingerprint=" + hex.EncodeToString(sum[:4]) + ` error="invalid or missing API key"`},
		{"secret", `{"model":`, `status=400 client=openWebUi error="request body must be a JSON object"`},
		{"secret", `{"model":"task","model":"x"}`, `status=400 client=openWebUi error="request body contains duplicate key 'model'"`},
		{"secret", `{"model":"task","x":"` + strings.Repeat("a", 2048) + `"}`, `status=413 client=openWebUi error="request body exceeds 1024 bytes"`},
	}
	for _, c := range cases {
		gw.direct(t, http.MethodPost, "/v1/chat/completions", c.key, c.body)
		if out := gw.logs.String(); !strings.Contains(out, `msg="request rejected" `) || !strings.Contains(out, c.want) {
			t.Errorf("want %s in log:\n%s", c.want, out)
		}
	}
	if out := gw.logs.String(); strings.Contains(out, "wrong-key") || strings.Contains(out, "secret") {
		t.Errorf("log leaks a key:\n%s", out)
	}
}

// A request that has already passed through this facade of this gateway is
// refused with 508 and never reaches a backend; this gateway's other facade
// on the route is not a loop, and the route never reaches a backend that
// isn't Open WebUI.
func TestRequests_RefuseARoutingLoop(t *testing.T) {
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw := startTask(t, swap)
	r := gw.chat(t, `{"model":"task","messages":[],"ai_model_facade_route":["elsewhere/x","`+instanceID+`/task"]}`)
	if r.StatusCode != http.StatusLoopDetected || !strings.Contains(r.body, "loop_detected") || swap.Count("POST /v1/chat/completions") != 0 {
		t.Errorf("loop: got %d %s; backend saw %v", r.StatusCode, r.body, swap.Keys())
	}
	r = gw.chat(t, `{"model":"task","ai_model_facade_route":["elsewhere/x","`+instanceID+`/other"],"messages":[]}`)
	req, _ := swap.Last("POST /v1/chat/completions")
	if r.StatusCode != http.StatusOK || strings.Contains(req.Body, "ai_model_facade_route") || !strings.HasPrefix(req.Body, `{"model":"qwen3.6-35b-a3b:q8_0","messages":[]`) {
		t.Errorf("another facade: got %d %s; backend saw %s", r.StatusCode, r.body, req.Body)
	}
}

// limitBody lowers the request body limit for one test.
func limitBody(t *testing.T, n int64) {
	old := maxBodyBytes
	maxBodyBytes = n
	t.Cleanup(func() { maxBodyBytes = old })
}

// api.<family>.protocols turns protocols off gateway-wide: a protocol left
// out has no route, a facade is listed by a family only for a protocol still
// served, and a family with none is not served at all.
func TestAPI_ServesOnlyTheProtocolsTurnedOn(t *testing.T) {
	claude := testkit.NewAnthropic(t, "claude-a")
	gw := startGateway(t, `
server: {listen: "127.0.0.1:0"}
api: {openAi: {protocols: [openAiResponses]}, anthropic: {protocols: []}}
backends:
  claude: {type: anthropic, baseUrl: "`+claude.URL+`", apiKey: k}
models:
  ask: {backend: claude, candidates: [{model: claude-a}]}
  chatOrClaude: {backend: claude, protocols: [openAiChat, anthropicMessages, openAiResponses], candidates: [{model: claude-a}]}
`, nil)
	body := `{"model":"ask","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
	for _, path := range []string{"/v1/chat/completions", "/openai/v1/chat/completions", "/anthropic/v1/messages"} {
		if r := gw.do(t, http.MethodPost, path, "", body); r.StatusCode != http.StatusNotFound {
			t.Errorf("%s: got %d %s", path, r.StatusCode, r.body)
		}
	}
	if r := gw.do(t, http.MethodGet, "/anthropic/v1/models", "", ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("Anthropic listing: got %d %s", r.StatusCode, r.body)
	}
	if r := gw.do(t, http.MethodPost, "/v1/responses", "", `{"model":"ask","input":"hi"}`); r.StatusCode != http.StatusOK {
		t.Errorf("responses: got %d %s", r.StatusCode, r.body)
	}
	if got := gjson.Get(gw.get(t, "/v1/models").body, "data.#.id").Raw; got != `["ask","chatOrClaude"]` {
		t.Errorf("OpenAI listing: %s", got)
	}
}

// A protocol that isn't the family's, and a facade every protocol of which is
// turned off, are config errors.
func TestAPI_RefusesProtocolsItCantServe(t *testing.T) {
	cases := map[string]string{
		"{openAi: {protocols: [anthropicMessages]}}": "api.openAi.protocols: 'anthropicMessages' is not one of the family's protocols",
		"{openAi: {protocols: [openAiResponses]}}":   "models.chatOnly: line 8: it serves only openAiChat, which api.<family>.protocols turns off",
	}
	for api, want := range cases {
		cfg := parseConfig(t, `
server: {listen: "127.0.0.1:0"}
api: `+api+`
backends:
  claude: {type: anthropic, baseUrl: "http://127.0.0.1:1", apiKey: k}
models:
  ask: {backend: claude, candidates: [{model: claude-a}]}
  chatOnly: {backend: claude, protocols: [openAiChat], candidates: [{model: claude-a}]}
`, nil)
		if _, err := Build(context.Background(), cfg, discard); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", api, err, want)
		}
	}
}
