package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// responsesConfig is a facade that prefers a local model on llama-swap and
// falls back to a model on OpenAI's API, which keeps its own conversations
// when the candidate says so.
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

// A conversation continues on whichever candidate is selected: a turn served
// by a local model through Chat Completions is sent to OpenAI as input, with
// no previous_response_id OpenAI couldn't resolve, and back on the local
// model the entire conversation is its messages.
func TestResponses_ContinueAConversationOnAnotherCandidate(t *testing.T) {
	gw, swap, openAI := startResponses(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	first := gw.respond(t, `{"model":"ask","input":"one"}`)
	if req, _ := swap.Last("POST /v1/chat/completions"); gjson.Get(req.Body, "messages").Raw != `[{"role":"user","content":"one"}]` {
		t.Errorf("first turn sent %s", req.Body)
	}
	swap.SetState("qwen3.6-35b-a3b:q8_0", "")
	second := gw.respond(t, `{"model":"ask","previous_response_id":"`+gjson.Get(first.body, "id").String()+`","input":"two"}`)
	req, _ := openAI.Last("POST /v1/responses")
	if gjson.Get(req.Body, "previous_response_id").Exists() || gjson.Get(req.Body, "input.#.role").Raw != `["user","assistant","user"]` || gjson.Get(req.Body, "input.1.content.0.text").String() != "ok" {
		t.Errorf("second turn sent %s", req.Body)
	}
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw.respond(t, `{"model":"ask","previous_response_id":"`+gjson.Get(second.body, "id").String()+`","input":"three"}`)
	req, _ = swap.Last("POST /v1/chat/completions")
	if got := gjson.Get(req.Body, "messages.#.content").Raw; got != `["one","ok","two","ok","three"]` {
		t.Errorf("third turn sent %s", req.Body)
	}
}

// A candidate whose service keeps its conversations is passed the service's
// own response ID, and the gateway reads the conversation from the service
// only when another candidate continues it.
func TestResponses_LetTheServiceKeepItsConversations(t *testing.T) {
	gw, swap, openAI := startResponses(t, "{model: gpt-a, backend: openAi}", "{model: gpt-a, backend: openAi, responsesHistory: service}")
	first := gw.respond(t, `{"model":"ask","input":"one"}`)
	id := gjson.Get(first.body, "id").String()
	second := gw.respond(t, `{"model":"ask","previous_response_id":"`+id+`","input":"two"}`)
	if req, _ := openAI.Last("POST /v1/responses"); gjson.Get(req.Body, "previous_response_id").String() != id || gjson.Get(req.Body, "input").String() != "two" {
		t.Errorf("second turn sent %s", req.Body)
	}
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	gw.respond(t, `{"model":"ask","previous_response_id":"`+gjson.Get(second.body, "id").String()+`","input":"three"}`)
	if openAI.Count("GET /v1/responses/"+gjson.Get(second.body, "id").String()+"/input_items") != 1 {
		t.Errorf("input_items not read: %v", openAI.Keys())
	}
	req, _ := swap.Last("POST /v1/chat/completions")
	if got := gjson.Get(req.Body, "messages.#.content").Raw; got != `["one","ok","two","ok","three"]` {
		t.Errorf("third turn sent %s", req.Body)
	}
}

// Only a backend that keeps conversations can be told to.
func TestResponses_RefuseAServiceThatKeepsNoConversations(t *testing.T) {
	cfg, err := parseConfigErr(edit(t, responsesConfig, "{model: qwen3.6-35b-a3b:q8_0, backend: llamaSwap}", "{model: qwen3.6-35b-a3b:q8_0, backend: llamaSwap, responsesHistory: service}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), cfg, discard); err == nil || !strings.Contains(err.Error(), "backend 'llamaSwap' keeps no Responses conversations") {
		t.Errorf("got %v", err)
	}
}

// An unknown previous_response_id is refused as OpenAI refuses it, and a
// response made with store false can't be continued.
func TestResponses_RefuseAnUnknownPreviousResponse(t *testing.T) {
	gw, swap, _ := startResponses(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	unstored := gw.respond(t, `{"model":"ask","input":"one","store":false}`)
	for _, id := range []string{"resp_unknown", gjson.Get(unstored.body, "id").String()} {
		r := gw.do(t, http.MethodPost, "/v1/responses", "", `{"model":"ask","previous_response_id":"`+id+`","input":"two"}`)
		if r.StatusCode != http.StatusBadRequest || gjson.Get(r.body, "error.type").String() != "invalid_request_error" || !strings.Contains(gjson.Get(r.body, "error.message").String(), id) {
			t.Errorf("%s: got %d %s", id, r.StatusCode, r.body)
		}
	}
}

// A translated stream keeps Responses' framing, each event with its event
// line and no [DONE], and its turn can be continued.
func TestResponses_StreamInTheirOwnFraming(t *testing.T) {
	gw, swap, _ := startResponses(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	swap.SetReply(testkit.Reply{Stream: []string{`{"model":"m","choices":[{"delta":{"content":"hi"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`}})
	r := gw.do(t, http.MethodPost, "/v1/responses", "", `{"model":"ask","input":"one","stream":true}`)
	if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "text/event-stream" || !strings.HasPrefix(r.body, "event: response.created\ndata: {") || strings.Contains(r.body, "[DONE]") {
		t.Fatalf("got %d %s", r.StatusCode, r.body)
	}
	events := strings.Split(strings.TrimSpace(r.body), "\n\n")
	last := strings.TrimPrefix(strings.SplitN(events[len(events)-1], "\n", 2)[1], "data: ")
	if !strings.HasPrefix(events[len(events)-1], "event: response.completed\n") {
		t.Fatalf("last event %s", events[len(events)-1])
	}
	swap.SetReply(testkit.Reply{})
	gw.respond(t, `{"model":"ask","previous_response_id":"`+gjson.Get(last, "response.id").String()+`","input":"two"}`)
	if req, _ := swap.Last("POST /v1/chat/completions"); gjson.Get(req.Body, "messages.#.content").Raw != `["one","hi","two"]` {
		t.Errorf("next turn sent %s", req.Body)
	}
}

// A reload keeps the conversations, even with every facade rebuilt.
func TestResponses_SurviveAReload(t *testing.T) {
	swap, openAI := newSwap(t), testkit.NewOpenAI(t, "gpt-a")
	vars := map[string]string{"SWAP_URL": swap.URL, "OPENAI_URL": openAI.URL}
	g, err := Build(context.Background(), parseConfig(t, responsesConfig, vars), discard)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(g, discard)
	srv := httptest.NewServer(live)
	t.Cleanup(srv.Close)
	gw := &gateway{Server: srv, g: g}
	first := gw.respond(t, `{"model":"ask","input":"one"}`)
	if _, err := live.Reload(context.Background(), parseConfig(t, edit(t, responsesConfig, "  ask:", "  asked:"), vars)); err != nil {
		t.Fatal(err)
	}
	gw.respond(t, `{"model":"asked","previous_response_id":"`+gjson.Get(first.body, "id").String()+`","input":"two"}`)
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
