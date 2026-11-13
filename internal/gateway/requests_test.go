package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// api.openAi.protocols turns protocols off gateway-wide: a protocol left out
// has no route, and the others are served as before.
func TestAPI_ServesOnlyTheProtocolsTurnedOn(t *testing.T) {
	openAI := testkit.NewOpenAI(t, "gpt-a")
	gw := startGateway(t, `
server: {listen: "127.0.0.1:0"}
api: {openAi: {protocols: [openAiChat]}}
backends:
  openAi: {type: openAi, baseUrl: "`+openAI.URL+`", apiKey: k}
models:
  ask: {backend: openAi, protocols: [openAiChat, openAiCompletions], candidates: [{model: gpt-a}]}
`, nil)
	if r := gw.do(t, http.MethodPost, "/v1/completions", "", `{"model":"ask","prompt":"hi"}`); r.StatusCode != http.StatusNotFound {
		t.Errorf("completions: got %d %s", r.StatusCode, r.body)
	}
	if r := gw.do(t, http.MethodPost, "/v1/chat/completions", "", `{"model":"ask","messages":[{"role":"user","content":"hi"}]}`); r.StatusCode != http.StatusOK {
		t.Errorf("chat: got %d %s", r.StatusCode, r.body)
	}
	if got := gjson.Get(gw.get(t, "/v1/models").body, "data.#.id").Raw; got != `["ask"]` {
		t.Errorf("listing: %s", got)
	}
}

// A protocol that isn't the family's, and a facade every protocol of which is
// turned off, are config errors.
func TestAPI_RefusesProtocolsItCantServe(t *testing.T) {
	cases := map[string]string{
		"{openAi: {protocols: [anthropicMessages]}}": "api.openAi.protocols: 'anthropicMessages' is not one of the family's protocols",
		"{openAi: {protocols: [openAiCompletions]}}": "models.chatOnly: line 8: it serves only openAiChat, which api.openAi.protocols turns off",
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
