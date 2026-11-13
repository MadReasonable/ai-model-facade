package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// responsesConfig is a facade that prefers a local model on llama-swap and
// falls back to a model on OpenAI's API.
const responsesConfig = `
server: {listen: "127.0.0.1:0"}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}"}
  openAi: {type: openAi, baseUrl: "${OPENAI_URL}", apiKey: k}
models:
  ask:
    candidates:
      - {model: qwen3.6-35b-a3b:q8_0, backend: llamaSwap}
      - {model: gpt-a, backend: openAi}
`

func startResponses(t *testing.T, edits ...string) (*gateway, *testkit.LlamaSwap, *testkit.OpenAI) {
	t.Helper()
	swap, openAI := newSwap(t), testkit.NewOpenAI(t, "gpt-a")
	gw := startGateway(t, edit(t, responsesConfig, edits...), map[string]string{"SWAP_URL": swap.URL, "OPENAI_URL": openAI.URL})
	return gw, swap, openAI
}

func (gw *gateway) respond(t *testing.T, body string) response {
	t.Helper()
	r := gw.do(t, http.MethodPost, "/v1/responses", "", body)
	if r.StatusCode != http.StatusOK || gjson.Get(r.body, "object").String() != "response" {
		t.Fatalf("got %d %s", r.StatusCode, r.body)
	}
	return r
}

// A translated stream keeps Responses' framing, each event with its event
// line and no [DONE].
func TestResponses_StreamInTheirOwnFraming(t *testing.T) {
	gw, swap, _ := startResponses(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	swap.SetReply(testkit.Reply{Stream: []string{`{"model":"m","choices":[{"delta":{"content":"hi"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`}})
	r := gw.do(t, http.MethodPost, "/v1/responses", "", `{"model":"ask","input":"one","stream":true}`)
	if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "text/event-stream" || !strings.HasPrefix(r.body, "event: response.created\ndata: {") || strings.Contains(r.body, "[DONE]") {
		t.Fatalf("got %d %s", r.StatusCode, r.body)
	}
	events := strings.Split(strings.TrimSpace(r.body), "\n\n")
	if !strings.HasPrefix(events[len(events)-1], "event: response.completed\n") {
		t.Fatalf("last event %s", events[len(events)-1])
	}
}

// A stream from a backend that speaks Responses ends at its last event, since
// Responses sends no [DONE]: nothing follows response.completed.
func TestResponses_EndAStreamAtItsLastEvent(t *testing.T) {
	gw, _, _ := startResponses(t)
	r := gw.do(t, http.MethodPost, "/v1/responses", "", `{"model":"ask","input":"one","stream":true}`)
	events := strings.Split(strings.TrimSpace(r.body), "\n\n")
	if r.StatusCode != http.StatusOK || !strings.HasPrefix(events[len(events)-1], "event: response.completed\n") || strings.Contains(r.body, "event: error") {
		t.Errorf("got %d %s", r.StatusCode, r.body)
	}
}

// A Responses client's reasoning summary asks Claude for summarized thinking,
// and a facade whose params turn thinking off still reaches Claude, without
// the display setting it refuses there.
func TestResponses_AskClaudeForSummariesUnlessThinkingIsOff(t *testing.T) {
	claude := testkit.NewAnthropic(t, "claude-a")
	gw := startGateway(t, `
server: {listen: "127.0.0.1:0"}
backends:
  claude: {type: anthropic, baseUrl: "`+claude.URL+`", apiKey: k}
models:
  thinks: {backend: claude, candidates: [{model: claude-a}]}
  quick:
    backend: claude
    params: {anthropicMessages: {thinking: {type: disabled}}}
    candidates: [{model: claude-a}]
`, nil)
	for model, want := range map[string]string{"thinks": `{"type":"adaptive","display":"summarized"}`, "quick": `{"type":"disabled"}`} {
		gw.respond(t, `{"model":"`+model+`","reasoning":{"summary":"auto"},"input":"hi"}`)
		if req, _ := claude.Last("POST /v1/messages"); gjson.Get(req.Body, "thinking").Raw != want {
			t.Errorf("%s sent %s", model, req.Body)
		}
	}
}

// A candidate sent as Responses gets a Chat Completions or a Messages client's
// request translated to Responses, and the reply translated back.
func TestResponses_ServeOtherClientsThroughUseProtocol(t *testing.T) {
	gw, _, openAI := startResponses(t, "{model: gpt-a, backend: openAi}", "{model: gpt-a, backend: openAi, useProtocol: openAiResponses}")
	r := gw.chat(t, `{"model":"ask","messages":[{"role":"user","content":"hi"}]}`)
	if r.StatusCode != http.StatusOK || gjson.Get(r.body, "choices.0.message.content").String() != "ok" {
		t.Errorf("chat: %d %s", r.StatusCode, r.body)
	}
	if req, ok := openAI.Last("POST /v1/responses"); !ok || gjson.Get(req.Body, "input.0.content").String() != "hi" || gjson.Get(req.Body, "store").Raw != "false" {
		t.Errorf("chat sent %+v", req)
	}
	m := messagesRequest(t, gw, "/anthropic/v1/messages", "", `{"model":"ask","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if m.StatusCode != http.StatusOK || gjson.Get(m.body, "content.0.text").String() != "ok" {
		t.Errorf("messages: %d %s", m.StatusCode, m.body)
	}
	if openAI.Count("POST /v1/chat/completions") != 0 {
		t.Errorf("a request went as Chat Completions: %v", openAI.Keys())
	}
}

// useProtocol names a protocol the backend accepts.
func TestResponses_RefuseAUseProtocolTheBackendDoesntAccept(t *testing.T) {
	cfg, err := parseConfigErr(edit(t, responsesConfig, "{model: qwen3.6-35b-a3b:q8_0, backend: llamaSwap}", "{model: qwen3.6-35b-a3b:q8_0, backend: llamaSwap, useProtocol: openAiResponses}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), cfg, discard); err == nil || !strings.Contains(err.Error(), "useProtocol 'openAiResponses' is not a protocol backend 'llamaSwap' accepts") {
		t.Errorf("got %v", err)
	}
}
