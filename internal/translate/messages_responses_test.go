package translate

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// A Responses request becomes a Messages request: the instructions and a
// developer message as system blocks, an assistant's text and calls as one
// turn, an output as a tool_result block of the next user turn, effort and a
// schema in output_config, and a reasoning item made from a Claude thinking
// block as that block; any other reasoning item is left out and reported.
func TestResponsesToMessages_TranslatesARequest(t *testing.T) {
	thinking := encodeThinking([]byte(`{"type":"thinking","thinking":"t","signature":"sig"}`))
	body := `{"model":"m","instructions":"be brief","max_output_tokens":9,"reasoning":{"effort":"low"},"text":{"format":{"type":"json_schema","name":"r","schema":{"type":"object"}}},"top_k":3,` +
		`"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"}}],"tool_choice":"required","parallel_tool_calls":false,"user":"u",` +
		`"input":[{"role":"user","content":"hi"},{"role":"developer","content":"no fluff"},{"type":"reasoning","summary":[],"encrypted_content":"gAAA"},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"t"}],"encrypted_content":"` + thinking + `"},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"done"},{"role":"user","content":[{"type":"input_text","text":"and?"}]}]}`
	out, reply, err := responsesToMessages{}.Request([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","system":[{"type":"text","text":"be brief"},{"type":"text","text":"no fluff"}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"sig"},{"type":"text","text":"ok"},{"type":"tool_use","id":"c1","name":"f","input":{"a":1}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"done"},{"type":"text","text":"and?"}]}],` +
		`"max_tokens":9,"output_config":{"effort":"low","format":{"type":"json_schema","schema":{"type":"object"}}},"top_k":3,` +
		`"tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}],"tool_choice":{"type":"any","disable_parallel_tool_use":true},"metadata":{"user_id":"u"}}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
	if got := strings.Join(reply.Dropped(), ","); got != "reasoning" {
		t.Errorf("dropped %s", got)
	}
}

// A Messages response becomes a response: a thinking block as a reasoning
// item whose summary is the thinking and whose encrypted_content carries the
// block, so that sent back it is the block again; text as a message, a tool
// call as a function call, cache counts within input_tokens.
func TestResponsesToMessages_TranslatesAResponse(t *testing.T) {
	message := `{"id":"msg_1","model":"claude-x","stop_reason":"tool_use","content":[{"type":"thinking","thinking":"why","signature":"sig"},{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}],` +
		`"usage":{"input_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":2,"output_tokens":7}}`
	_, reply, _ := responsesToMessages{}.Request([]byte(`{"model":"facade","input":"hi"}`))
	res := reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(message)}})
	got := gjson.ParseBytes(res.Response.Body)
	if kinds := got.Get("output.#.type").Raw; kinds != `["reasoning","message","function_call"]` || got.Get("status").String() != "completed" || got.Get("model").String() != "claude-x" {
		t.Fatalf("response %s", got.Raw)
	}
	reasoning := got.Get("output.0")
	if reasoning.Get("summary.0.type").String() != "summary_text" || reasoning.Get("summary.0.text").String() != "why" {
		t.Errorf("reasoning %s", reasoning.Raw)
	}
	if block, ok := decodeThinking(reasoning); !ok || block != `{"type":"thinking","thinking":"why","signature":"sig"}` {
		t.Errorf("encrypted_content carries %q, %v", block, ok)
	}
	if got.Get("output.2.call_id").String() != "toolu_1" || got.Get("output.2.arguments").String() != `{"a":1}` {
		t.Errorf("function call %s", got.Get("output.2").Raw)
	}
	if u := got.Get("usage"); u.Get("input_tokens").Int() != 10 || u.Get("input_tokens_details.cached_tokens").Int() != 3 || u.Get("output_tokens").Int() != 7 {
		t.Errorf("usage %s", got.Get("usage").Raw)
	}

	failed := reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 429, Body: []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)}})
	if failed.Response.Status != 429 || gjson.GetBytes(failed.Response.Body, "error.type").String() != "rate_limit_error" || gjson.GetBytes(failed.Response.Body, "error.message").String() != "slow down" {
		t.Errorf("error: %s", failed.Response.Body)
	}
}

// Messages events stream as Responses events: thinking deltas as the summary,
// its signature into encrypted_content as the item closes, text, and a tool
// call's input as its arguments; message_stop ends the response, with the
// stop reason and usage of message_delta.
func TestResponsesToMessages_TranslatesAStream(t *testing.T) {
	_, reply, _ := responsesToMessages{}.Request([]byte(`{"model":"facade","input":"hi","stream":true}`))
	result := reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":4,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"why"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"input_tokens":4,"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	}}})
	events, err := eventsOf(t, result.Stream)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added:reasoning", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta:why",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done:reasoning",
		"response.output_item.added:message", "response.content_part.added", "response.output_text.delta:ok",
		"response.output_text.done", "response.content_part.done", "response.output_item.done:message",
		"response.output_item.added:function_call", `response.function_call_arguments.delta:{"a":1}`,
		"response.function_call_arguments.done", "response.output_item.done:function_call",
		"response.incomplete",
	}
	if got := eventLines(t, events); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if block, ok := decodeThinking(events[7].Get("item")); !ok || gjson.Get(block, "signature").String() != "sig" || gjson.Get(block, "thinking").String() != "why" {
		t.Errorf("reasoning item done: %s", events[7].Raw)
	}
	last := events[len(events)-1].Get("response")
	if last.Get("incomplete_details.reason").String() != "max_output_tokens" || last.Get("usage.output_tokens").Int() != 9 || last.Get("usage.input_tokens").Int() != 4 {
		t.Errorf("response.incomplete: %s", last.Raw)
	}
}

// A reasoning summary asked for is Claude's thinking displayed summarized, the
// request's own thinking keys winning; without one, thinking passes on as
// sent.
func TestResponsesToMessages_AsksForSummarizedThinking(t *testing.T) {
	for body, want := range map[string]string{
		`{"reasoning":{"summary":"auto"},"input":"hi"}`:                                   `{"thinking":{"type":"adaptive","display":"summarized"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		`{"reasoning":{"summary":"detailed"},"thinking":{"type":"enabled"},"input":"hi"}`: `{"thinking":{"type":"enabled","display":"summarized"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		`{"reasoning":{"summary":null},"thinking":{"type":"adaptive"},"input":"hi"}`:      `{"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
	} {
		out, reply, err := responsesToMessages{}.Request([]byte(body))
		if err != nil || string(out) != want || len(reply.Dropped()) != 0 {
			t.Errorf("%s:\ngot  %s %v %v\nwant %s", body, out, reply.Dropped(), err, want)
		}
	}
}

// A Messages request becomes a Responses request: the system prompt as
// instructions, tool results as call outputs ahead of the user's text,
// tool_use blocks as function calls, a thinking block made from an OpenAI
// reasoning item as that item, any other left out and reported, thinking on as
// a summary asked for, effort and a schema translated, tools not strict, and
// the request not kept but its reasoning returned encrypted.
func TestMessagesToResponses_TranslatesARequest(t *testing.T) {
	item := `{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"gAAA"}`
	body := `{"model":"m","max_tokens":9,"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],"thinking":{"type":"adaptive"},"top_k":3,` +
		`"output_config":{"effort":"low","format":{"type":"json_schema","schema":{"type":"object"}}},"metadata":{"user_id":"u"},` +
		`"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"any","disable_parallel_tool_use":true},` +
		`"messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"` + encodeReasoning([]byte(item)) + `"},{"type":"thinking","thinking":"c","signature":"claude-sig"},{"type":"text","text":"ok"},{"type":"tool_use","id":"c1","name":"f","input":{"a":1}}]},` +
		`{"role":"user","content":[{"type":"text","text":"and"},{"type":"tool_result","tool_use_id":"c1","content":"bad","is_error":true}]}]}`
	out, reply, err := messagesToResponses{}.Request([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","max_output_tokens":9,"instructions":"be brief",` +
		`"input":[{"type":"message","role":"user","content":"hi"},` + item + `,{"type":"message","role":"assistant","content":"ok"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"Error: bad"},{"type":"message","role":"user","content":[{"type":"input_text","text":"and"}]}],` +
		`"reasoning":{"effort":"low","summary":"auto"},"text":{"format":{"type":"json_schema","name":"response","strict":true,"schema":{"type":"object"}}},` +
		`"safety_identifier":"u","tools":[{"type":"function","name":"f","parameters":{"type":"object"},"strict":false}],"tool_choice":"required","parallel_tool_calls":false,` +
		`"store":false,"include":["reasoning.encrypted_content"]}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
	if got := strings.Join(reply.Dropped(), ","); got != "thinking,top_k,cache_control" {
		t.Errorf("dropped %s", got)
	}
}

// What only the Claude API provides, and stop sequences, which Responses lacks,
// are refused with a 400 in Anthropic's shape.
// OpenAI takes a safety_identifier of at most 128 characters, so a longer
// user_id is sent as its SHA-256, the same for every request from that user.
func TestMessagesToResponses_HashesALongUserID(t *testing.T) {
	for id, want := range map[string]string{
		"user-1":                 "user-1",
		strings.Repeat("u", 150): "816752062318c8f216b0d347163d40e917a8c522ce07be3d0ebc80db6132fc5b",
	} {
		out, _, err := messagesToResponses{}.Request([]byte(`{"metadata":{"user_id":"` + id + `"},"messages":[]}`))
		if got := gjson.GetBytes(out, "safety_identifier").String(); err != nil || got != want {
			t.Errorf("%.20s...: got %q, %v", id, got, err)
		}
	}
}

func TestMessagesToResponses_RefusesWhatResponsesCantServe(t *testing.T) {
	for _, body := range []string{
		`{"stop_sequences":["x"],"messages":[]}`,
		`{"tools":[{"type":"web_search_20260209","name":"web_search"}],"messages":[]}`,
		`{"mcp_servers":[{}],"messages":[]}`,
		`{"output_config":{"format":{"type":"other"}},"messages":[]}`,
	} {
		_, _, err := messagesToResponses{}.Request([]byte(body))
		var refused *Refusal
		if !errors.As(err, &refused) || refused.Response.Status != http.StatusBadRequest || gjson.GetBytes(refused.Response.Body, "type").String() != "error" {
			t.Errorf("%s: got %v", body, err)
		}
	}
}

// A response becomes a message: a reasoning item as a thinking block carrying
// the item, which the next request turns back into the same item; text, and a
// function call as tool_use with its input as an object; usage with cache
// reads apart.
func TestMessagesToResponses_TranslatesAResponseAndCarriesReasoningBack(t *testing.T) {
	item := `{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"why"}],"encrypted_content":"gAAA"}`
	response := `{"id":"resp_1","model":"gpt-x","status":"completed","output":[` + item + `,` +
		`{"type":"message","content":[{"type":"output_text","text":"ok"}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"}],` +
		`"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":3},"output_tokens":4}}`
	_, reply, _ := messagesToResponses{}.Request([]byte(`{"messages":[]}`))
	got := gjson.ParseBytes(reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(response)}}).Response.Body)
	if got.Get("content.#.type").Raw != `["thinking","text","tool_use"]` || got.Get("content.0.thinking").String() != "why" || got.Get("content.2.input.a").Int() != 1 || got.Get("stop_reason").String() != "tool_use" {
		t.Fatalf("message %s", got.Raw)
	}
	if u := got.Get("usage"); u.Get("input_tokens").Int() != 7 || u.Get("cache_read_input_tokens").Int() != 3 || u.Get("output_tokens").Int() != 4 {
		t.Errorf("usage %s", got.Get("usage").Raw)
	}
	next := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":` + got.Get("content").Raw + `},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"x"}]}]}`
	out, _, err := messagesToResponses{}.Request([]byte(next))
	if err != nil || gjson.GetBytes(out, "input.1").Raw != item {
		t.Errorf("sent back as %s, %v; want %s", gjson.GetBytes(out, "input.1").Raw, err, item)
	}
	failed := reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 429, Body: []byte(`{"error":{"message":"slow down","type":"rate_limit_error"}}`)}})
	if failed.Response.Status != 429 || gjson.GetBytes(failed.Response.Body, "error.type").String() != "rate_limit_error" {
		t.Errorf("error: %s", failed.Response.Body)
	}
}

// Events stream as Messages events: a block per item, reasoning parts joined,
// the reasoning item as its block's signature once done, then message_delta
// and message_stop.
func TestMessagesToResponses_TranslatesAStream(t *testing.T) {
	_, reply, _ := messagesToResponses{}.Request([]byte(`{"messages":[],"stream":true}`))
	events, err := eventsOf(t, reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: responsesStreamOf("response.completed")}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		line := e.Get("type").String()
		if d := e.Get("delta"); d.Get("type").Exists() {
			line += ":" + d.Get("type").String() + ":" + d.Get("thinking").String() + d.Get("text").String() + d.Get("partial_json").String()
		}
		if b := e.Get("content_block"); b.Exists() {
			line += ":" + b.Get("type").String()
		}
		got = append(got, line)
	}
	want := []string{
		"message_start",
		"content_block_start:thinking", "content_block_delta:thinking_delta:one", "content_block_delta:thinking_delta:\n\n", "content_block_delta:thinking_delta:two",
		"content_block_delta:signature_delta:", "content_block_stop",
		"content_block_start:text", "content_block_delta:text_delta:hi", "content_block_stop",
		"content_block_start:tool_use", `content_block_delta:input_json_delta:{"a":1}`, "content_block_stop",
		"message_delta", "message_stop",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if item, ok := decodeReasoning(gjson.Parse(`{"signature":` + events[5].Get("delta.signature").Raw + `}`)); !ok || gjson.Get(item, "encrypted_content").String() != "gAAA" {
		t.Errorf("signature carries %s", item)
	}
	if events[len(events)-2].Get("delta.stop_reason").String() != "tool_use" {
		t.Errorf("message_delta %s", events[len(events)-2].Raw)
	}
}

// Toward Messages, a Responses request gets its effort as the nearest level
// Claude has, its tools narrowed to those a tool choice allows, and a file by
// URL as a URL document; OpenAI's service settings, which Claude lacks, are
// reported, and asking for log probabilities is refused.
func TestResponsesToMessages_TranslatesWhatClaudeHas(t *testing.T) {
	body := `{"reasoning":{"effort":"minimal"},"moderation":{},"client_metadata":{},"prompt_cache_options":{},"metadata":{"a":"b"},` +
		`"tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}],` +
		`"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"b"}]},` +
		`"input":[{"role":"user","content":[{"type":"input_file","file_url":"https://x/d.pdf"}]}]}`
	out, reply, err := responsesToMessages{}.Request([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	got := gjson.ParseBytes(out)
	checks := map[string]string{
		"output_config.effort": `"low"`,
		"tools.#.name":         `["b"]`,
		"tool_choice":          `{"type":"any"}`,
		"messages.0.content.0": `{"type":"document","source":{"type":"url","url":"https://x/d.pdf"}}`,
	}
	for path, want := range checks {
		if v := got.Get(path).Raw; v != want {
			t.Errorf("%s: got %s, want %s", path, v, want)
		}
	}
	dropped := strings.Join(reply.Dropped(), ",")
	for _, want := range []string{"moderation", "client_metadata", "prompt_cache_options", "metadata"} {
		if !strings.Contains(dropped, want) {
			t.Errorf("%s not reported in %s", want, dropped)
		}
	}
	if _, _, err := (responsesToMessages{}).Request([]byte(`{"include":["message.output_text.logprobs"],"input":"hi"}`)); err == nil {
		t.Error("log probabilities were asked of Claude")
	}
}
