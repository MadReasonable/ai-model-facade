package gateway

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// The live Messages checks serve a facade on the real Claude API in-process,
// and send it requests the way a client built for Claude does. The API has no
// dev-env backend, so they need ANTHROPIC_API_KEY rather than AMF_LIVE, skip
// without it, and cost a few tokens:
//
//	ANTHROPIC_API_KEY=... go test ./internal/gateway/ -run Live_Messages

const (
	liveMessagesPath  = "/anthropic/v1/messages"
	liveMessagesTools = `[{"name":"get_weather","description":"Get the current weather for a city.","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`
	liveChatTools     = `[{"type":"function","function":{"name":"get_weather","description":"Get the current weather for a city.","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`
	liveToolQuestion  = "I can visit either Paris or Rome tomorrow and want the warmer one. Call get_weather for one city at a time, Paris first."
)

// startClaudeGateway serves the facade `ask` on the real API, with thinking
// summarized so a reply carries a signed block when Claude thinks.
func startClaudeGateway(t *testing.T) *gateway {
	t.Helper()
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("set ANTHROPIC_API_KEY to check the real API")
	}
	model := os.Getenv("ANTHROPIC_LIVE_MODEL")
	if model == "" {
		model = "claude-haiku-5-5"
	}
	return startGateway(t, `
server: {listen: "127.0.0.1:0"}
backends:
  claude: {type: anthropic, baseUrl: "https://api.anthropic.com", apiKey: "${LIVE_KEY}", requestTimeoutSeconds: 120}
models:
  ask:
    backend: claude
    params: {anthropicMessages: {thinking: {type: adaptive, display: summarized}}}
    candidates: [{model: "`+model+`"}]
`, map[string]string{"LIVE_KEY": key})
}

// messagesBody is a Messages request for the facade, with its messages as JSON.
// It has a system prompt because Claude Haiku 5.5 returned no thinking for this
// question without one (seen on 4 of 4 requests).
func messagesBody(messages string, stream bool) string {
	field := ""
	if stream {
		field = `"stream":true,`
	}
	return `{"model":"ask",` + field + `"max_tokens":1500,"system":"You are a travel assistant.","tools":` + liveMessagesTools + `,"messages":` + messages + `}`
}

// liveToolTurn asks for a tool call until the reply holds a signed thinking
// block too, since Claude doesn't think on every turn, and returns its content.
func liveToolTurn(t *testing.T, gw *gateway, stream bool) string {
	t.Helper()
	question := `[{"role":"user","content":` + quoted(liveToolQuestion) + `}]`
	for range 8 {
		r := messagesRequest(t, gw, liveMessagesPath, "", messagesBody(question, stream), nil)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("got %d %s", r.StatusCode, r.body)
		}
		content := gjson.Get(r.body, "content").Raw
		if stream {
			content = contentOfStream(t, r.body)
		}
		if gjson.Get(content, `#(type=="thinking").signature`).String() != "" && gjson.Get(content, `#(type=="tool_use").id`).Exists() {
			return content
		}
	}
	t.Fatal("no reply held both a signed thinking block and a tool call")
	return ""
}

func quoted(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// contentOfStream rebuilds a message's content blocks from its stream, as a
// Messages client does, checking each event line against its data's type.
func contentOfStream(t *testing.T, stream string) string {
	t.Helper()
	blocks := map[int64]map[string]any{}
	inputs := map[int64]string{}
	var count int64
	for _, event := range strings.Split(strings.TrimSpace(stream), "\n\n") {
		name, data, ok := strings.Cut(event, "\ndata: ")
		if !ok || !strings.HasPrefix(name, "event: ") {
			t.Fatalf("not Messages framing: %q", event)
		}
		parsed := gjson.Parse(data)
		if kind := parsed.Get("type").String(); strings.TrimPrefix(name, "event: ") != kind {
			t.Errorf("event line %q with data of type %q", name, kind)
		}
		index := parsed.Get("index").Int()
		switch parsed.Get("type").String() {
		case "content_block_start":
			block := map[string]any{}
			if err := json.Unmarshal([]byte(parsed.Get("content_block").Raw), &block); err != nil {
				t.Fatal(err)
			}
			blocks[index] = block
			count = max(count, index+1)
		case "content_block_delta":
			delta := parsed.Get("delta")
			switch delta.Get("type").String() {
			case "text_delta":
				blocks[index]["text"] = blocks[index]["text"].(string) + delta.Get("text").String()
			case "thinking_delta":
				blocks[index]["thinking"] = blocks[index]["thinking"].(string) + delta.Get("thinking").String()
			case "signature_delta":
				blocks[index]["signature"] = delta.Get("signature").String()
			case "input_json_delta":
				inputs[index] += delta.Get("partial_json").String()
			}
		}
	}
	content := make([]map[string]any, count)
	for i := range count {
		content[i] = blocks[i]
		if input, ok := inputs[i]; ok {
			var parsed any
			if err := json.Unmarshal([]byte(input), &parsed); err != nil {
				t.Fatal(err)
			}
			content[i]["input"] = parsed
		}
	}
	data, _ := json.Marshal(content)
	return string(data)
}

// A Messages request gets a Messages response, whole or streamed in its own
// framing, ending at message_stop with no [DONE].
func TestLive_MessagesAnswersWholeOrStreamed(t *testing.T) {
	gw := startClaudeGateway(t)
	question := `[{"role":"user","content":"Say hi in two words."}]`
	r := messagesRequest(t, gw, liveMessagesPath, "", messagesBody(question, false), nil)
	if r.StatusCode != http.StatusOK || gjson.Get(r.body, "type").String() != "message" || r.target() == "" {
		t.Errorf("whole: got %d %s", r.StatusCode, r.body)
	}
	r = messagesRequest(t, gw, liveMessagesPath, "", messagesBody(question, true), nil)
	if r.StatusCode != http.StatusOK || !strings.HasPrefix(r.body, "event: message_start\n") || !strings.Contains(r.body, "event: message_stop\ndata: ") || strings.Contains(r.body, "[DONE]") {
		t.Errorf("stream: got %d %s", r.StatusCode, r.body)
	}
	contentOfStream(t, r.body)
}

// A tool loop's second turn sends the signed thinking block back, which the
// API refuses if the gateway changed anything it signed, so this holds only
// if the bytes reach the client and return intact, whole or streamed.
func TestLive_MessagesToolLoopSendsSignedThinkingBack(t *testing.T) {
	gw := startClaudeGateway(t)
	for name, stream := range map[string]bool{"whole": false, "streamed": true} {
		t.Run(name, func(t *testing.T) {
			content := liveToolTurn(t, gw, stream)
			var results []string
			for _, callID := range gjson.Get(content, `#(type=="tool_use")#.id`).Array() {
				results = append(results, `{"type":"tool_result","tool_use_id":`+quoted(callID.String())+`,"content":"14C, cloudy."}`)
			}
			messages := `[{"role":"user","content":` + quoted(liveToolQuestion) + `},{"role":"assistant","content":` + content + `},{"role":"user","content":[` + strings.Join(results, ",") + `]}]`
			r := messagesRequest(t, gw, liveMessagesPath, "", messagesBody(messages, false), nil)
			if r.StatusCode != http.StatusOK || gjson.Get(r.body, "type").String() != "message" {
				t.Errorf("got %d %s", r.StatusCode, r.body)
			}
		})
	}
}

// A request with no max_tokens gets the model's output cap from the adapter,
// since the API requires one.
func TestLive_MessagesFillsAMissingMaxTokens(t *testing.T) {
	gw := startClaudeGateway(t)
	r := messagesRequest(t, gw, liveMessagesPath, "", `{"model":"ask","messages":[{"role":"user","content":"Say hi in two words."}]}`, nil)
	if r.StatusCode != http.StatusOK {
		t.Errorf("got %d %s", r.StatusCode, r.body)
	}
}

// The API's own error reaches a Messages client unchanged, with its request
// ID, and a client's beta header reaches the API, which refuses a false one.
func TestLive_MessagesPassesOnTheAPIsErrorsAndHeaders(t *testing.T) {
	gw := startClaudeGateway(t)
	r := messagesRequest(t, gw, liveMessagesPath, "", `{"model":"ask","max_tokens":5,"seed":1,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if r.StatusCode != http.StatusBadRequest || gjson.Get(r.body, "type").String() != "error" || gjson.Get(r.body, "error.type").String() != "invalid_request_error" || !gjson.Get(r.body, "request_id").Exists() {
		t.Errorf("unknown field: got %d %s", r.StatusCode, r.body)
	}
	r = messagesRequest(t, gw, liveMessagesPath, "", `{"model":"ask","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"anthropic-beta": "no-such-beta"})
	if r.StatusCode != http.StatusBadRequest || !strings.Contains(r.body, "no-such-beta") {
		t.Errorf("beta header: got %d %s", r.StatusCode, r.body)
	}
}

// A thinking block the gateway made from a local model's reasoning has no real
// signature, which the API refuses, so it must come out on the way to Claude.
// A history that moves from a local model to Claude is answered.
func TestLive_MessagesRemovesUnsignedThinkingForClaude(t *testing.T) {
	gw := startClaudeGateway(t)
	messages := `[{"role":"user","content":"Is Paris a capital city?"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"It is the capital of France.","signature":"` + backend.UnsignedThinking + `"},{"type":"text","text":"Yes."}]},` +
		`{"role":"user","content":"And Rome? Answer in one word."}]`
	r := messagesRequest(t, gw, liveMessagesPath, "", messagesBody(messages, false), nil)
	if r.StatusCode != http.StatusOK || gjson.Get(r.body, "type").String() != "message" {
		t.Errorf("got %d %s", r.StatusCode, r.body)
	}
}

// A Chat Completions client reaches the same facade through the translator,
// with params still in Messages spelling, through a complete tool loop.
func TestLive_MessagesFacadeServesChatCompletionsThroughTheTranslator(t *testing.T) {
	gw := startClaudeGateway(t)
	user := `{"role":"user","content":` + quoted(liveToolQuestion) + `}`
	r := gw.chat(t, `{"model":"ask","max_tokens":1500,"tools":`+liveChatTools+`,"messages":[`+user+`]}`)
	message := gjson.Get(r.body, "choices.0.message")
	call := message.Get("tool_calls.0")
	if r.StatusCode != http.StatusOK || gjson.Get(r.body, "object").String() != "chat.completion" || !call.Exists() {
		t.Fatalf("first turn: got %d %s", r.StatusCode, r.body)
	}
	assistant := `{"role":"assistant","content":null,"tool_calls":[` + call.Raw + `]}`
	result := `{"role":"tool","tool_call_id":` + quoted(call.Get("id").String()) + `,"content":"Paris: 14C, cloudy."}`
	r = gw.chat(t, `{"model":"ask","max_tokens":1500,"tools":`+liveChatTools+`,"messages":[`+user+`,`+assistant+`,`+result+`]}`)
	if r.StatusCode != http.StatusOK || gjson.Get(r.body, "object").String() != "chat.completion" {
		t.Errorf("second turn: got %d %s", r.StatusCode, r.body)
	}
	r = gw.chat(t, `{"model":"ask","max_tokens":50,"seed":1,"messages":[{"role":"user","content":"Say hi in two words."}]}`)
	if got := r.Header.Get("X-Gateway-Dropped"); r.StatusCode != http.StatusOK || got != "seed" {
		t.Errorf("dropped field: got %d, %q: %s", r.StatusCode, got, r.body)
	}
	r = gw.chat(t, `{"model":"ask","max_tokens":1500,"stream":true,"messages":[{"role":"user","content":"Say hi in two words."}]}`)
	if r.StatusCode != http.StatusOK || !strings.HasSuffix(r.body, "data: [DONE]\n\n") || strings.Contains(r.body, "event: ") {
		t.Errorf("stream: got %d %s", r.StatusCode, r.body)
	}
}

// OpenAI's newer models refuse max_tokens, which a Messages request always has
// and a Chat Completions client may send, so both reach it under the name it
// takes. It needs OPENAI_API_KEY, and costs a few tokens.
func TestLive_MessagesReachesOpenAIsNewerModels(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("set OPENAI_API_KEY to check the real API")
	}
	model := os.Getenv("OPENAI_LIVE_MODEL")
	if model == "" {
		model = "gpt-5.2"
	}
	gw := startGateway(t, `
server: {listen: "127.0.0.1:0"}
backends:
  gpt: {type: openAi, baseUrl: "https://api.openai.com", apiKey: "${LIVE_KEY}", requestTimeoutSeconds: 120}
models:
  ask: {backend: gpt, candidates: [{model: "`+model+`"}]}
`, map[string]string{"LIVE_KEY": key})
	r := messagesRequest(t, gw, liveMessagesPath, "", `{"model":"ask","max_tokens":200,"messages":[{"role":"user","content":"Say hi in two words."}]}`, nil)
	if r.StatusCode != http.StatusOK || gjson.Get(r.body, "type").String() != "message" {
		t.Errorf("Messages: got %d %s", r.StatusCode, r.body)
	}
	r = gw.chat(t, `{"model":"ask","max_tokens":200,"messages":[{"role":"user","content":"Say hi in two words."}]}`)
	if r.StatusCode != http.StatusOK {
		t.Errorf("Chat Completions: got %d %s", r.StatusCode, r.body)
	}
}
