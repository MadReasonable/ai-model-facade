package gateway

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The live Responses checks serve facades on the real Claude and OpenAI APIs
// in-process, and send them requests the way a Responses client does. Each
// check needs the key of every API it reaches, ANTHROPIC_API_KEY or
// OPENAI_API_KEY, skips without it, and costs a few tokens:
//
//	ANTHROPIC_API_KEY=... OPENAI_API_KEY=... go test ./internal/gateway/ -run Live_Responses

const liveResponsesTools = `[{"type":"function","name":"get_weather","description":"Get the current weather for a city.","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]},"strict":false}]`

// liveAPIs are the hosted APIs the live checks reach: the key each needs, and
// the model each serves, overridable since hosted models are retired.
var liveAPIs = map[string]struct{ keyVar, modelVar, model string }{
	"claude": {"ANTHROPIC_API_KEY", "ANTHROPIC_LIVE_MODEL", "claude-haiku-5-5"},
	"gpt":    {"OPENAI_API_KEY", "OPENAI_LIVE_MODEL", "gpt-5.2"},
}

// startHostedGateway serves facades, a YAML block of models, on backends named
// claude and gpt for the two APIs, skipping t unless the key of each API in
// needs is set. A facade names an API's model as ${CLAUDE_MODEL} or
// ${GPT_MODEL}.
func startHostedGateway(t *testing.T, facades string, needs ...string) *gateway {
	t.Helper()
	vars := map[string]string{}
	for name, api := range liveAPIs {
		key := os.Getenv(api.keyVar)
		if key == "" {
			for _, need := range needs {
				if need == name {
					t.Skipf("set %s to check the real API", api.keyVar)
				}
			}
			key = "unset"
		}
		model := os.Getenv(api.modelVar)
		if model == "" {
			model = api.model
		}
		vars[strings.ToUpper(name)+"_KEY"] = key
		vars[strings.ToUpper(name)+"_MODEL"] = model
	}
	return startGateway(t, `
server: {listen: "127.0.0.1:0"}
backends:
  claude: {type: anthropic, baseUrl: "https://api.anthropic.com", apiKey: "${CLAUDE_KEY}", requestTimeoutSeconds: 120}
  gpt: {type: openAi, baseUrl: "https://api.openai.com", apiKey: "${GPT_KEY}", requestTimeoutSeconds: 120}
models:
`+facades, vars)
}

// responseOfStream returns the response a Responses stream completes with,
// checking each event line against its data's type, and that the stream ends
// at response.completed with no [DONE].
func responseOfStream(t *testing.T, stream string) string {
	t.Helper()
	events := strings.Split(strings.TrimSpace(stream), "\n\n")
	for _, event := range events {
		name, data, ok := strings.Cut(event, "\ndata: ")
		if !ok || !strings.HasPrefix(name, "event: ") {
			t.Fatalf("not Responses framing: %q", event)
		}
		if kind := gjson.Get(data, "type").String(); strings.TrimPrefix(name, "event: ") != kind {
			t.Errorf("event line %q with data of type %q", name, kind)
		}
	}
	last := events[len(events)-1]
	if !strings.HasPrefix(last, "event: response.completed\n") {
		t.Fatalf("stream ends with %q", last)
	}
	return gjson.Get(strings.SplitN(last, "\ndata: ", 2)[1], "response").Raw
}

// liveRespond sends a Responses request, whole or streamed, and returns the
// response, failing t unless it completed.
func liveRespond(t *testing.T, gw *gateway, body string, stream bool) string {
	t.Helper()
	if stream {
		body = strings.Replace(body, "{", `{"stream":true,`, 1)
	}
	r := gw.do(t, http.MethodPost, "/v1/responses", "", body)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("got %d %s", r.StatusCode, r.body)
	}
	response := r.body
	if stream {
		response = responseOfStream(t, r.body)
	}
	if status := gjson.Get(response, "status").String(); status != "completed" {
		t.Fatalf("status %q: %s", status, response)
	}
	return response
}

// liveFunctionLoop runs a Responses tool loop on facade: it asks for a call of
// get_weather until the reply also carries a reasoning item, since a model
// doesn't reason on every turn (the system prompt is there for Claude, as in
// messagesBody), then sends the reply's output back with the call's result,
// and returns the reasoning item. A backend that checks the reasoning it made
// refuses the second turn if anything in it changed.
func liveFunctionLoop(t *testing.T, gw *gateway, facade string, stream bool) gjson.Result {
	t.Helper()
	question := `[{"role":"user","content":` + quoted(liveToolQuestion) + `}]`
	body := `{"model":"` + facade + `","max_output_tokens":1500,"instructions":"You are a travel assistant.","reasoning":{"summary":"auto"},"tools":` + liveResponsesTools + `,"input":%s}`
	for range 8 {
		output := gjson.Get(liveRespond(t, gw, strings.Replace(body, "%s", question, 1), stream), "output")
		reasoning, call := output.Get(`#(type=="reasoning")`), output.Get(`#(type=="function_call")`)
		if !reasoning.Exists() || !call.Exists() {
			continue
		}
		var items []string
		for _, item := range output.Array() {
			items = append(items, item.Raw)
		}
		result := `{"type":"function_call_output","call_id":` + quoted(call.Get("call_id").String()) + `,"output":"Paris: 14C, cloudy."}`
		input := strings.TrimSuffix(question, "]") + "," + strings.Join(items, ",") + "," + result + "]"
		liveRespond(t, gw, strings.Replace(body, "%s", input, 1), false)
		return reasoning
	}
	t.Fatal("no reply held both a reasoning item and a function call")
	return gjson.Result{}
}

// A Responses request for Claude is translated to Messages and back, whole or
// streamed, and in a tool loop the reasoning item carries Claude's signed
// thinking, which Claude accepts back only unchanged.
func TestLive_ResponsesOnClaude(t *testing.T) {
	gw := startHostedGateway(t, `
  ask: {backend: claude, candidates: [{model: "${CLAUDE_MODEL}"}]}
`, "claude")
	for name, stream := range map[string]bool{"whole": false, "streamed": true} {
		t.Run(name, func(t *testing.T) {
			reasoning := liveFunctionLoop(t, gw, "ask", stream)
			if reasoning.Get("encrypted_content").String() == "" || reasoning.Get("summary.0.text").String() == "" {
				t.Errorf("reasoning item %s", reasoning.Raw)
			}
		})
	}
}

// A Responses request for OpenAI is forwarded as it came, and its stream ends
// at response.completed.
func TestLive_ResponsesOnOpenAI(t *testing.T) {
	gw := startHostedGateway(t, `
  ask: {backend: gpt, candidates: [{model: "${GPT_MODEL}"}]}
`, "gpt")
	for name, stream := range map[string]bool{"whole": false, "streamed": true} {
		t.Run(name, func(t *testing.T) {
			response := liveRespond(t, gw, `{"model":"ask","max_output_tokens":200,"reasoning":{"effort":"low"},"input":"Say hi in two words."}`, stream)
			if gjson.Get(response, `output.#(type=="message").content.0.text`).String() == "" {
				t.Errorf("no answer: %s", response)
			}
		})
	}
}

// A candidate sent as Responses serves Chat Completions and Messages clients.
// A Messages client's thinking blocks carry OpenAI's encrypted reasoning in
// their signature, and OpenAI accepts that reasoning back in a tool loop.
func TestLive_ResponsesServeOtherClientsThroughUseProtocol(t *testing.T) {
	gw := startHostedGateway(t, `
  ask:
    backend: gpt
    params: {openAiResponses: {reasoning: {effort: low, summary: auto}}}
    candidates: [{model: "${GPT_MODEL}", useProtocol: openAiResponses}]
`, "gpt")
	user := `{"role":"user","content":` + quoted(liveToolQuestion) + `}`
	r := gw.chat(t, `{"model":"ask","max_completion_tokens":1500,"tools":`+liveChatTools+`,"messages":[`+user+`]}`)
	call := gjson.Get(r.body, "choices.0.message.tool_calls.0")
	if r.StatusCode != http.StatusOK || !call.Exists() {
		t.Fatalf("Chat Completions, first turn: got %d %s", r.StatusCode, r.body)
	}
	assistant := `{"role":"assistant","content":null,"tool_calls":[` + call.Raw + `]}`
	result := `{"role":"tool","tool_call_id":` + quoted(call.Get("id").String()) + `,"content":"Paris: 14C, cloudy."}`
	if r = gw.chat(t, `{"model":"ask","max_completion_tokens":1500,"tools":`+liveChatTools+`,"messages":[`+user+`,`+assistant+`,`+result+`]}`); r.StatusCode != http.StatusOK {
		t.Errorf("Chat Completions, second turn: got %d %s", r.StatusCode, r.body)
	}
	for name, stream := range map[string]bool{"whole": false, "streamed": true} {
		t.Run("Messages "+name, func(t *testing.T) {
			question := `[` + user + `]`
			field := `"thinking":{"type":"adaptive"},`
			for range 8 {
				body := strings.Replace(messagesBody(question, stream), `"max_tokens"`, field+`"max_tokens"`, 1)
				r := messagesRequest(t, gw, liveMessagesPath, "", body, nil)
				if r.StatusCode != http.StatusOK {
					t.Fatalf("got %d %s", r.StatusCode, r.body)
				}
				content := gjson.Get(r.body, "content").Raw
				if stream {
					content = contentOfStream(t, r.body)
				}
				callID := gjson.Get(content, `#(type=="tool_use").id`).String()
				if gjson.Get(content, `#(type=="thinking").signature`).String() == "" || callID == "" {
					continue
				}
				results := `{"role":"user","content":[{"type":"tool_result","tool_use_id":` + quoted(callID) + `,"content":"Paris: 14C, cloudy."}]}`
				messages := `[` + user + `,{"role":"assistant","content":` + content + `},` + results + `]`
				body = strings.Replace(messagesBody(messages, false), `"max_tokens"`, field+`"max_tokens"`, 1)
				if r = messagesRequest(t, gw, liveMessagesPath, "", body, nil); r.StatusCode != http.StatusOK {
					t.Errorf("second turn: got %d %s", r.StatusCode, r.body)
				}
				return
			}
			t.Fatal("no reply held both a thinking block and a tool call")
		})
	}
}

// OpenAI refuses a safety_identifier longer than 128 characters, and Claude
// Code sends a user_id of 150, so a long user_id reaches OpenAI hashed.
func TestLive_ResponsesTakeALongUserIDFromMessages(t *testing.T) {
	gw := startHostedGateway(t, `
  ask: {backend: gpt, candidates: [{model: "${GPT_MODEL}", useProtocol: openAiResponses}]}
`, "gpt")
	body := `{"model":"ask","max_tokens":200,"metadata":{"user_id":"` + strings.Repeat("u", 150) + `"},"messages":[{"role":"user","content":"Say hi in two words."}]}`
	if r := messagesRequest(t, gw, liveMessagesPath, "", body, nil); r.StatusCode != http.StatusOK {
		t.Errorf("got %d %s", r.StatusCode, r.body)
	}
}
