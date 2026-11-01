package anthropic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

func newBackend(t *testing.T, f *testkit.Anthropic, extra string) *Backend {
	t.Helper()
	b, err := New(testkit.Entry(t, "anthropic", `{type: anthropic, baseUrl: "`+f.URL+`", apiKey: test-key`+extra+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The model list is every page of the API's list, read with the backend's
// key as a bearer token and the anthropic-version header the API requires.
func TestAnthropic_ListsEveryPage(t *testing.T) {
	var ids []string
	for i := range 1001 {
		ids = append(ids, fmt.Sprintf("claude-%d", i))
	}
	f := testkit.NewAnthropic(t, ids...)
	got, err := newBackend(t, f, "").ListModels(context.Background())
	if err != nil || len(got) != 1001 || got[0].ID != "claude-0" || got[1000].ID != "claude-1000" {
		t.Fatalf("got %d models, %v", len(got), err)
	}
	if n := f.Count("GET /v1/models"); n != 2 {
		t.Errorf("/v1/models read %d times, want 2 pages", n)
	}
	if r, _ := f.Last("GET /v1/models"); r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Anthropic-Version") == "" {
		t.Errorf("Authorization %q, anthropic-version %q", r.Header.Get("Authorization"), r.Header.Get("Anthropic-Version"))
	}
}

// Every listed model is ready, and a state read after the listing calls the
// API no more.
func TestAnthropic_EveryListedModelIsReadyWithoutACall(t *testing.T) {
	f := testkit.NewAnthropic(t, "claude-a", "claude-b")
	b := newBackend(t, f, "")
	if _, err := b.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := b.LoadedModels(context.Background())
	if err != nil || len(got) != 2 || got[0].Starting || got[1].Starting {
		t.Fatalf("got %+v, %v", got, err)
	}
	if n := f.Count("GET /v1/models"); n != 1 {
		t.Errorf("/v1/models read %d times, want 1", n)
	}
}

func chat(t *testing.T, b *Backend, body string) backend.ChatResult {
	t.Helper()
	res, err := b.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(body), Stream: gjson.Get(body, "stream").Bool()})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A chat completion becomes a Messages request with the client's keys in
// their order, each translated in place: system and developer messages
// gathered into system, an image by data URL or URL, tool calls as tool_use
// blocks, the results of parallel calls in one user turn, and the request's
// settings under their Messages names. Fields with no counterpart that only
// tune sampling are dropped, and a key with no translation passes on
// unchanged.
func TestAnthropic_TranslatesARequest(t *testing.T) {
	f := testkit.NewAnthropic(t, "claude-a")
	b := newBackend(t, f, "")
	body := `{"model":"claude-a","messages":[` +
		`{"role":"system","content":"Be brief."},` +
		`{"role":"user","content":[{"type":"text","text":"Look:"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBO"}},{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"low"}}]},` +
		`{"role":"developer","content":[{"type":"text","text":"Use tools."}]},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call_2","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Rome\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"sunny"},` +
		`{"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"rain"}]},` +
		`{"role":"user","content":"Thanks."}],` +
		`"max_completion_tokens":512,"seed":7,"stop":"END","temperature":0.5,` +
		`"tools":[{"type":"function","function":{"name":"weather","description":"Weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],` +
		`"tool_choice":"required","parallel_tool_calls":false,"reasoning_effort":"low",` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object"}}},` +
		`"user":"u-1","top_k":40,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"high"}}`
	if res := chat(t, b, body); res.Response.Status != 200 {
		t.Fatalf("chat: %d %s", res.Response.Status, res.Response.Body)
	}
	want := `{"model":"claude-a",` +
		`"system":[{"type":"text","text":"Be brief."},{"type":"text","text":"Use tools."}],` +
		`"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"Look:"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}},{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"weather","input":{"city":"Paris"}},{"type":"tool_use","id":"call_2","name":"weather","input":{"city":"Rome"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"sunny"},{"type":"tool_result","tool_use_id":"call_2","content":[{"type":"text","text":"rain"}]},{"type":"text","text":"Thanks."}]}],` +
		`"max_tokens":512,"stop_sequences":["END"],"temperature":0.5,` +
		`"tools":[{"name":"weather","description":"Weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],` +
		`"tool_choice":{"type":"any","disable_parallel_tool_use":true},` +
		`"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object"}}},` +
		`"metadata":{"user_id":"u-1"},"top_k":40,"thinking":{"type":"adaptive","display":"summarized"}}`
	if r, _ := f.Last("POST /v1/messages"); r.Body != want {
		t.Errorf("sent\n%s\nwant\n%s", r.Body, want)
	}
}

// A request without max_tokens gets the model's output cap from the latest
// list; before the model is listed, it is refused, since Messages requires
// one.
func TestAnthropic_DefaultsMaxTokensToTheOutputCap(t *testing.T) {
	f := testkit.NewAnthropic(t, "claude-a")
	b := newBackend(t, f, "")
	body := `{"model":"claude-a","messages":[{"role":"user","content":"hi"}]}`
	if res := chat(t, b, body); res.Response.Status != 400 || !strings.Contains(string(res.Response.Body), "max_tokens") {
		t.Errorf("before the list: %d %s", res.Response.Status, res.Response.Body)
	}
	if _, err := b.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	chat(t, b, body)
	if r, _ := f.Last("POST /v1/messages"); gjson.Get(r.Body, "max_tokens").Int() != testkit.OutputCap {
		t.Errorf("sent %s", r.Body)
	}
}

// A request the Messages API can't serve as the client means it is refused
// with 400 in OpenAI's error shape, and so is a legacy completion, before
// anything is sent.
func TestAnthropic_RefusesWhatMessagesCantServe(t *testing.T) {
	f := testkit.NewAnthropic(t, "claude-a")
	b := newBackend(t, f, "")
	for _, extra := range []string{`"n":2`, `"logprobs":true`, `"response_format":{"type":"json_object"}`, `"web_search_options":{}`, `"tools":[{"type":"custom","custom":{"name":"x"}}]`, `"tool_choice":{"type":"allowed_tools"}`} {
		res := chat(t, b, `{"model":"claude-a","max_tokens":5,"messages":[{"role":"user","content":"hi"}],`+extra+`}`)
		if res.Response.Status != 400 || gjson.GetBytes(res.Response.Body, "error.type").String() != "invalid_request_error" {
			t.Errorf("%s: %d %s", extra, res.Response.Status, res.Response.Body)
		}
	}
	for _, msg := range []string{`{"role":"function","name":"f","content":"x"}`, `{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}`, `{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"f","arguments":"not json"}}]}`} {
		res := chat(t, b, `{"model":"claude-a","max_tokens":5,"messages":[`+msg+`]}`)
		if res.Response.Status != 400 || !strings.Contains(string(res.Response.Body), "messages[0]") {
			t.Errorf("%s: %d %s", msg, res.Response.Status, res.Response.Body)
		}
	}
	_, err := b.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.Completions, Body: []byte(`{"model":"claude-a","prompt":"hi"}`)})
	if err == nil || !strings.Contains(err.Error(), "serves only") {
		t.Errorf("legacy completion: %v", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Errorf("%d requests reached the API, want none", n)
	}
}

// thinkingAndTools is a message with each kind of block a chat completion
// can carry.
const thinkingAndTools = `[{"type":"thinking","thinking":"Two cities.","signature":"sig"},{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_1","name":"weather","input":{"city":"Paris"}},{"type":"tool_use","id":"toolu_2","name":"weather","input":{"city":"Rome"}}]`

// A response becomes a chat completion: text as content, thinking as
// reasoning_content, tool_use blocks as tool calls, the stop reason as the
// finish reason, and cache reads counted into the prompt tokens.
func TestAnthropic_TranslatesAResponse(t *testing.T) {
	f := testkit.NewAnthropic(t, "claude-a")
	f.SetMessage(thinkingAndTools, "tool_use")
	res := chat(t, newBackend(t, f, ""), `{"model":"claude-a","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	got := gjson.ParseBytes(res.Response.Body)
	msg := got.Get("choices.0.message")
	if res.Response.Status != 200 || got.Get("object").String() != "chat.completion" || got.Get("model").String() != "claude-a" ||
		msg.Get("content").String() != "Checking." || msg.Get("reasoning_content").String() != "Two cities." ||
		msg.Get("tool_calls.1.id").String() != "toolu_2" || msg.Get("tool_calls.1.function.arguments").String() != `{"city":"Rome"}` ||
		got.Get("choices.0.finish_reason").String() != "tool_calls" ||
		got.Get("usage.prompt_tokens").Int() != 12 || got.Get("usage.prompt_tokens_details.cached_tokens").Int() != 2 || got.Get("usage.total_tokens").Int() != 17 {
		t.Errorf("%d %s", res.Response.Status, res.Response.Body)
	}
}

func drain(t *testing.T, s backend.ChatStream) ([]gjson.Result, error) {
	t.Helper()
	defer s.Close()
	var chunks []gjson.Result
	for {
		data, err := s.Next()
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return chunks, err
		}
		chunks = append(chunks, gjson.ParseBytes(data))
	}
}

// A stream becomes chat completion chunks: the role first, reasoning and
// content as they arrive, each tool call opened with its ID and name and
// numbered in order, its arguments in pieces that join to its input, the
// finish reason, and usage last when the client asked for it.
func TestAnthropic_TranslatesAStream(t *testing.T) {
	f := testkit.NewAnthropic(t, "claude-a")
	f.SetMessage(thinkingAndTools, "tool_use")
	res := chat(t, newBackend(t, f, ""), `{"model":"claude-a","max_tokens":5,"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	chunks, err := drain(t, res.Stream)
	if err != nil {
		t.Fatal(err)
	}
	var reasoning, content string
	args := map[int64]string{}
	ids := map[int64]string{}
	for _, c := range chunks {
		delta := c.Get("choices.0.delta")
		reasoning += delta.Get("reasoning_content").String()
		content += delta.Get("content").String()
		for _, call := range delta.Get("tool_calls").Array() {
			i := call.Get("index").Int()
			args[i] += call.Get("function.arguments").String()
			if id := call.Get("id").String(); id != "" {
				ids[i] = id
			}
		}
	}
	n := len(chunks)
	if n < 3 || chunks[0].Get("choices.0.delta.role").String() != "assistant" || chunks[0].Get("object").String() != "chat.completion.chunk" ||
		reasoning != "Two cities." || content != "Checking." ||
		ids[0] != "toolu_1" || ids[1] != "toolu_2" || args[0] != `{"city":"Paris"}` || args[1] != `{"city":"Rome"}` ||
		chunks[n-2].Get("choices.0.finish_reason").String() != "tool_calls" ||
		chunks[n-1].Get("choices.#").Int() != 0 || chunks[n-1].Get("usage.prompt_tokens").Int() != 12 || chunks[n-1].Get("usage.completion_tokens").Int() != 5 {
		t.Errorf("chunks: %v", chunks)
	}
}

// An error event mid-stream ends the stream with a stream error in OpenAI's
// shape, and a stream that ends before message_stop is an unexpected end.
func TestAnthropic_EndsABrokenStreamWithAnError(t *testing.T) {
	f := testkit.NewAnthropic(t, "claude-a")
	b := newBackend(t, f, "")
	body := `{"model":"claude-a","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	f.SetStreamError(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	_, err := drain(t, chat(t, b, body).Stream)
	var streamErr *backend.StreamError
	if !errors.As(err, &streamErr) || gjson.GetBytes(streamErr.Data, "error.type").String() != "overloaded_error" || gjson.GetBytes(streamErr.Data, "error.message").String() != "Overloaded" {
		t.Errorf("error event: %v", err)
	}
	f.SetReply(testkit.Reply{Stream: []string{`{"type":"message_start","message":{"id":"m","model":"claude-a","usage":{}}}`}, Truncate: true})
	if _, err := drain(t, chat(t, b, body).Stream); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated stream: %v", err)
	}
}

// An API error is passed on with its status and Retry-After, in OpenAI's
// error shape; a 5xx also makes the next state read list the API again.
func TestAnthropic_PassesOnAnAPIError(t *testing.T) {
	f := testkit.NewAnthropic(t, "claude-a")
	b := newBackend(t, f, "")
	if _, err := b.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.SetReply(testkit.Reply{Status: 429, Header: map[string]string{"Retry-After": "7"}, Body: `{"type":"error","error":{"type":"rate_limit_error","message":"Slow down"}}`})
	res := chat(t, b, `{"model":"claude-a","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if res.Response.Status != 429 || res.Response.RetryAfter != "7" || string(res.Response.Body) != `{"error":{"message":"Slow down","type":"rate_limit_error"}}` {
		t.Errorf("429: %d %q %s", res.Response.Status, res.Response.RetryAfter, res.Response.Body)
	}
	f.SetReply(testkit.Reply{Status: 500, Body: `{"type":"error","error":{"type":"api_error","message":"Internal"}}`})
	chat(t, b, `{"model":"claude-a","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := b.LoadedModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.Count("GET /v1/models"); n != 2 {
		t.Errorf("/v1/models read %d times, want 2: once at the start, once after the 500", n)
	}
}

func TestAnthropic_RejectsAWrongConfig(t *testing.T) {
	for _, src := range []string{"{type: anthropic}", "{type: anthropic, baseUrl: x, workspaceId: w}"} {
		if _, err := New(testkit.Entry(t, "anthropic", src)); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}
