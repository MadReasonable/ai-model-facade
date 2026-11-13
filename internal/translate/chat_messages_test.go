package translate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

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
	for _, extra := range []string{`"n":2`, `"logprobs":true`, `"response_format":{"type":"json_object"}`, `"tools":[{"type":"custom","custom":{"name":"x"}}]`, `"tool_choice":{"type":"allowed_tools"}`} {
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
	if n := len(f.Requests()); n != 0 {
		t.Errorf("%d requests reached the API, want none", n)
	}
}

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

// A Messages request becomes a chat completion request with the client's keys
// in their order: the system prompt as the first message, a tool result as a
// tool message right after its call, thinking left out, the settings under
// their Chat Completions names, and a key with no translation unchanged.
func TestMessagesToChat_TranslatesARequest(t *testing.T) {
	body := `{"model":"m","max_tokens":5,"top_k":3,"stream":true,"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],"thinking":{"type":"adaptive"},` +
		`"stop_sequences":["x"],"output_config":{"effort":"low"},"metadata":{"user_id":"u"},` +
		`"tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}],"tool_choice":{"type":"any","disable_parallel_tool_use":true},` +
		`"messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"text","text":"ok"},{"type":"tool_use","id":"c1","name":"f","input":{"a":1}}]},` +
		`{"role":"user","content":[{"type":"text","text":"and"},{"type":"tool_result","tool_use_id":"c1","content":"bad","is_error":true},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}`
	out, _, err := messagesToChat{}.Request([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","max_tokens":5,"top_k":3,"stream":true,"stop":["x"],"reasoning_effort":"low","user":"u",` +
		`"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}],"tool_choice":"required","parallel_tool_calls":false,` +
		`"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"ok","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{\"a\":1}"},"id":"c1"}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"Error: bad"},` +
		`{"role":"user","content":[{"type":"text","text":"and"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}],"stream_options":{"include_usage":true}}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}

// A system message in the conversation joins the system prompt at the front,
// since Claude Code sends one and not every model takes one mid-conversation.
func TestMessagesToChat_GathersSystemMessages(t *testing.T) {
	out, _, err := messagesToChat{}.Request([]byte(`{"system":"first","messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"text","text":"second"}]},{"role":"user","content":"more"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"messages":[{"role":"system","content":"first\n\nsecond"},{"role":"user","content":"hi"},{"role":"user","content":"more"}]}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}

// What only the Claude API provides is refused with a 400 in Anthropic's shape.
func TestMessagesToChat_RefusesWhatChatCompletionsCantServe(t *testing.T) {
	for _, body := range []string{
		`{"tools":[{"type":"web_search_20260209","name":"web_search"}],"messages":[]}`,
		`{"tools":[{"type":"bash_20250124","name":"bash"}],"messages":[]}`,
		`{"mcp_servers":[{}],"messages":[]}`,
		`{"container":"c","messages":[]}`,
		`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"u"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"web_search_tool_result"}]}]}`,
		`{"messages":[{"role":"tool","content":"s"}]}`,
	} {
		_, _, err := messagesToChat{}.Request([]byte(body))
		var refused *Refusal
		if !errors.As(err, &refused) || refused.Response.Status != http.StatusBadRequest || gjson.GetBytes(refused.Response.Body, "error.type").String() != "invalid_request_error" || gjson.GetBytes(refused.Response.Body, "type").String() != "error" {
			t.Errorf("%s: got %v", body, err)
		}
	}
}

// A chat completion becomes a message: reasoning as a thinking block with the
// marker for a signature, tool calls with their arguments as an object, usage
// with cache reads apart. An error keeps its status and Retry-After.
func TestMessagesToChat_TranslatesAResponse(t *testing.T) {
	completion := `{"id":"c","model":"m","choices":[{"finish_reason":"tool_calls","message":{"reasoning_content":"why","content":"ok","tool_calls":[{"id":"t","function":{"name":"f","arguments":"{\"a\":1}"}},{"id":"u","function":{"name":"g","arguments":"nope"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3}}}`
	res := (&chatReply{}).Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, ContentType: "application/json; charset=utf-8", Body: []byte(completion)}})
	got := gjson.ParseBytes(res.Response.Body)
	for path, want := range map[string]string{
		"id": "msg_c", "type": "message", "role": "assistant", "model": "m", "stop_reason": "tool_use",
		"content.0.type": "thinking", "content.0.thinking": "why", "content.0.signature": backend.UnsignedThinking,
		"content.1.text": "ok", "content.2.id": "t", "content.2.input.a": "1", "content.3.input._invalidArguments": "nope",
		"usage.input_tokens": "7", "usage.output_tokens": "2", "usage.cache_read_input_tokens": "3",
	} {
		if v := got.Get(path).String(); v != want {
			t.Errorf("%s: got %q, want %q in %s", path, v, want, res.Response.Body)
		}
	}
	res = (&chatReply{}).Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 429, Body: []byte(`{"error":{"message":"slow down"}}`), RetryAfter: "3"}})
	if r := res.Response; r.Status != 429 || r.RetryAfter != "3" || gjson.GetBytes(r.Body, "error.type").String() != "rate_limit_error" || gjson.GetBytes(r.Body, "error.message").String() != "slow down" {
		t.Errorf("error: got %+v %s", r, r.Body)
	}
}

// Chunks have no blocks, so the stream opens one when the kind of delta changes:
// reasoning closes with the marker as its signature, the first tool call
// streams, a call interleaved with it arrives whole, and the end brings the
// finish reason and usage, which came in a chunk of their own.
func TestMessagesToChat_TranslatesAStream(t *testing.T) {
	result := (&chatReply{}).Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"id":"c","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"th"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"ink"}}]}`,
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t0","function":{"name":"f","arguments":"{\"a\""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"t1","function":{"name":"g","arguments":"{\"b\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":5}}`,
	}}})
	events, err := eventsOf(t, result.Stream)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		line := e.Get("type").String()
		if d := e.Get("delta"); d.Exists() && d.Get("type").Exists() {
			line += ":" + d.Get("type").String() + ":" + d.Get("thinking").String() + d.Get("text").String() + d.Get("signature").String() + d.Get("partial_json").String()
		}
		if b := e.Get("content_block"); b.Exists() {
			line += ":" + b.Get("type").String() + ":" + b.Get("id").String()
		}
		if i := e.Get("index"); i.Exists() {
			line += "@" + i.String()
		}
		got = append(got, line)
	}
	want := []string{
		"message_start",
		"content_block_start:thinking:@0", "content_block_delta:thinking_delta:th@0", "content_block_delta:thinking_delta:ink@0",
		"content_block_delta:signature_delta:" + backend.UnsignedThinking + "@0", "content_block_stop@0",
		"content_block_start:text:@1", "content_block_delta:text_delta:hi@1", "content_block_stop@1",
		"content_block_start:tool_use:t0@2", `content_block_delta:input_json_delta:{"a"@2`, "content_block_delta:input_json_delta::1}@2", "content_block_stop@2",
		"content_block_start:tool_use:t1@3", `content_block_delta:input_json_delta:{"b":2}@3`, "content_block_stop@3",
		"message_delta", "message_stop",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	last := events[len(events)-2]
	if last.Get("delta.stop_reason").String() != "tool_use" || last.Get("usage.input_tokens").Int() != 4 || last.Get("usage.output_tokens").Int() != 5 {
		t.Errorf("message_delta: %s", last.Raw)
	}
}

// Reasoning and content that come while the first call streams are held
// until the end, since its block can't be reopened, so the call keeps every
// argument and nothing is lost.
func TestMessagesToChat_HoldsTextThatComesDuringTheFirstCall(t *testing.T) {
	result := (&chatReply{}).Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"id":"c","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t0","function":{"name":"f","arguments":"{\"a\""}}]}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"hm","content":"wait"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
		`{"choices":[{"delta":{"content":"ed"},"finish_reason":"tool_calls"}]}`,
	}}})
	events, err := eventsOf(t, result.Stream)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		switch e.Get("type").String() {
		case "content_block_start":
			got = append(got, e.Get("content_block.type").String())
		case "content_block_delta":
			d := e.Get("delta")
			got = append(got, d.Get("partial_json").String()+d.Get("thinking").String()+d.Get("text").String())
		}
	}
	want := `tool_use|{"a"|:1}|thinking|hm||text|wait|ed`
	if strings.Join(got, "|") != want {
		t.Errorf("got %s, want %s", strings.Join(got, "|"), want)
	}
}

// A backend's error event becomes Anthropic's shape, and a body that ends early
// stays an error, so the client doesn't take the reply as complete.
func TestMessagesToChat_EndsABrokenStreamWithAnError(t *testing.T) {
	result := (&chatReply{}).Result(backend.ChatResult{Stream: &chunks{end: &backend.StreamError{Data: []byte(`{"error":{"message":"boom"}}`)}, data: []string{`{"choices":[{"delta":{"content":"hi"}}]}`}}})
	_, err := eventsOf(t, result.Stream)
	var streamErr *backend.StreamError
	if !errors.As(err, &streamErr) || gjson.GetBytes(streamErr.Data, "type").String() != "error" || gjson.GetBytes(streamErr.Data, "error.message").String() != "boom" {
		t.Errorf("error event: got %v", err)
	}
	result = (&chatReply{}).Result(backend.ChatResult{Stream: &chunks{end: io.ErrUnexpectedEOF, data: []string{`{"choices":[{"delta":{"content":"hi"}}]}`}}})
	if _, err := eventsOf(t, result.Stream); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("early end: got %v", err)
	}
}

// Fields of a Chat Completions request that have no Messages counterpart are
// left out and reported: a participant's name, an image's detail, and
// OpenAI's prompt caching and moderation settings.
func TestChatToMessages_ReportsWhatItLeavesOut(t *testing.T) {
	body := `{"prompt_cache_key":"k","prompt_cache_retention":"24h","moderation":{},"messages":[{"role":"user","name":"ann","content":[{"type":"image_url","image_url":{"url":"https://x/i.png","detail":"high"}}]}]}`
	out, reply, err := chatToMessages{}.Request([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"prompt_cache_key", "prompt_cache_retention", "moderation", "name", "detail"} {
		if strings.Contains(string(out), `"`+key+`"`) {
			t.Errorf("%s was sent: %s", key, out)
		}
	}
	if got := strings.Join(reply.Dropped(), ","); got != "name,image_url.detail,prompt_cache_key,prompt_cache_retention,moderation" {
		t.Errorf("dropped %s", got)
	}
}

// An effort level crosses as itself where Claude has it, else as the
// nearest: none and minimal become low, and xhigh stays.
func TestChatToMessages_MapsEffortToTheNearestLevel(t *testing.T) {
	for level, want := range map[string]string{"none": "low", "minimal": "low", "low": "low", "high": "high", "xhigh": "xhigh"} {
		out, _, _ := chatToMessages{}.Request([]byte(`{"reasoning_effort":"` + level + `","messages":[]}`))
		if got := gjson.GetBytes(out, "output_config.effort").String(); got != want {
			t.Errorf("%s: got %s", level, got)
		}
	}
}

// A request in the deprecated function calling is sent as tools: functions
// as tools, function_call as the tool choice, a call and its function
// message as a tool_use and its result with an ID made for them, and one call
// at a time, since the reply has room for one.
func TestChatToMessages_TranslatesLegacyFunctionCalling(t *testing.T) {
	body := `{"functions":[{"name":"f","parameters":{"type":"object"}}],"function_call":{"name":"f"},` +
		`"messages":[{"role":"user","content":"go"},{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{\"a\":1}"}},{"role":"function","name":"f","content":"done"}]}`
	out, _, err := chatToMessages{}.Request([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"f","disable_parallel_tool_use":true},` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"go"}]},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"f","input":{"a":1}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}]}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}

// The reply to a legacy request is in its shape: one function_call, and
// finish_reason function_call, whole and streamed.
func TestChatToMessages_RepliesToLegacyFunctionCallingInItsShape(t *testing.T) {
	_, reply, _ := chatToMessages{}.Request([]byte(`{"functions":[{"name":"f"}],"messages":[]}`))
	message := `{"id":"m","model":"c","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}],"usage":{"input_tokens":1,"output_tokens":1}}`
	got := gjson.ParseBytes(reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(message)}}).Response.Body)
	if choice := got.Get("choices.0"); choice.Get("finish_reason").String() != "function_call" || choice.Get("message.function_call.name").String() != "f" || choice.Get("message.function_call.arguments").String() != `{"a":1}` || choice.Get("message.tool_calls").Exists() {
		t.Errorf("completion %s", got.Raw)
	}
	events, err := eventsOf(t, reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"type":"message_start","message":{"id":"m","model":"c","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	for _, e := range events {
		deltas = append(deltas, e.Get("choices.0.delta").Raw+e.Get("choices.0.finish_reason").String())
	}
	if want := []string{`{"content":"","role":"assistant"}`, `{"function_call":{"arguments":"","name":"f"}}`, `{"function_call":{"arguments":"{\"a\":1}"}}`, `{}function_call`}; strings.Join(deltas, "|") != strings.Join(want, "|") {
		t.Errorf("got %v\nwant %v", deltas, want)
	}
}

// A tool choice listing allowed tools narrows the tools sent to those, as
// Claude has no such list, and takes its mode as the choice.
func TestChatToMessages_NarrowsToolsToTheAllowedOnes(t *testing.T) {
	body := `{"tools":[{"type":"function","function":{"name":"a"}},{"type":"function","function":{"name":"b"}}],` +
		`"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"required","tools":[{"type":"function","function":{"name":"b"}}]}},"messages":[]}`
	out, _, err := chatToMessages{}.Request([]byte(body))
	if err != nil || gjson.GetBytes(out, "tools.#.name").Raw != `["b"]` || gjson.GetBytes(out, "tool_choice").Raw != `{"type":"any"}` {
		t.Errorf("got %s, %v", out, err)
	}
}
