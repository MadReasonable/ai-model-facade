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
		`{"tools":[{"type":"web_fetch_20260209","name":"web_fetch"}],"messages":[]}`,
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
// Claude has, its tools narrowed to those a tool choice allows, a file by URL
// as a URL document, web search as Claude's, and a web search's record in the
// history as text; OpenAI's service settings, which Claude lacks, are
// reported, and asking for log probabilities is refused.
func TestResponsesToMessages_TranslatesWhatClaudeHas(t *testing.T) {
	body := `{"reasoning":{"effort":"minimal"},"moderation":{},"client_metadata":{},"prompt_cache_options":{},"metadata":{"a":"b"},` +
		`"tools":[{"type":"function","name":"a"},{"type":"function","name":"b"},{"type":"web_search","user_location":{"type":"approximate","country":"NO"},"filters":{"allowed_domains":["x.no"]}}],` +
		`"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"b"}]},` +
		`"input":[{"role":"user","content":[{"type":"input_file","file_url":"https://x/d.pdf"}]},{"type":"web_search_call","action":{"query":"q"}}]}`
	out, reply, err := responsesToMessages{}.Request([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	got := gjson.ParseBytes(out)
	checks := map[string]string{
		"output_config.effort":      `"low"`,
		"tools.#.name":              `["b","web_search"]`,
		"tools.1":                   `{"type":"web_search_20250305","name":"web_search","user_location":{"type":"approximate","country":"NO"},"allowed_domains":["x.no"]}`,
		"tool_choice":               `{"type":"any"}`,
		"messages.0.content.0":      `{"type":"document","source":{"type":"url","url":"https://x/d.pdf"}}`,
		"messages.1.content.0.text": `"[web search: q]"`,
	}
	for path, want := range checks {
		if v := got.Get(path).Raw; v != want {
			t.Errorf("%s: got %s, want %s", path, v, want)
		}
	}
	dropped := strings.Join(reply.Dropped(), ",")
	for _, want := range []string{"moderation", "client_metadata", "prompt_cache_options", "metadata", "web_search_call"} {
		if !strings.Contains(dropped, want) {
			t.Errorf("%s not reported in %s", want, dropped)
		}
	}
	if _, _, err := (responsesToMessages{}).Request([]byte(`{"include":["message.output_text.logprobs"],"input":"hi"}`)); err == nil {
		t.Error("log probabilities were asked of Claude")
	}
}

// Claude's web search reaches a Responses client as a web_search_call item
// and its citations as url_citation annotations over the text they cite,
// whole and streamed; Claude's encrypted results stay out.
func TestResponsesToMessages_RepliesWithSearchesAndCitations(t *testing.T) {
	_, reply, _ := responsesToMessages{}.Request([]byte(`{"model":"facade","input":"hi"}`))
	citation := `{"type":"web_search_result_location","url":"https://x","title":"X","cited_text":"...","encrypted_index":"e"}`
	message := `{"id":"m","model":"c","stop_reason":"end_turn","content":[{"type":"server_tool_use","id":"s","name":"web_search","input":{"query":"oslo"}},{"type":"web_search_tool_result","tool_use_id":"s","content":[]},` +
		`{"type":"text","text":"Oslé "},{"type":"text","text":"is big.","citations":[` + citation + `]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	got := gjson.ParseBytes(reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(message)}}).Response.Body)
	if search := got.Get("output.0"); search.Get("type").String() != "web_search_call" || search.Get("action.query").String() != "oslo" {
		t.Errorf("search %s", search.Raw)
	}
	if a := got.Get("output.1.content.0.annotations.0"); a.Get("start_index").Int() != 5 || a.Get("end_index").Int() != 12 || a.Get("url").String() != "https://x" {
		t.Errorf("annotation %s", got.Get("output.1").Raw)
	}
	events, err := eventsOf(t, reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"type":"message_start","message":{"id":"m","model":"c","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"s","name":"web_search","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"oslo\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":` + citation + `}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"is big."}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	completed := events[len(events)-1].Get("response")
	if completed.Get("output.0.type").String() != "web_search_call" || completed.Get("output.0.action.query").String() != "oslo" || completed.Get("output.1.content.0.annotations.0.end_index").Int() != 7 {
		t.Errorf("completed %s", completed.Raw)
	}
}

// OpenAI's URL citations reach a Claude client as citations on the text
// block, citing the text in their range, marked as the gateway's, whole and
// streamed, from either OpenAI API.
func TestMessages_RepliesWithOpenAICitations(t *testing.T) {
	_, fromChat, _ := messagesToChat{}.Request([]byte(`{"messages":[]}`))
	completion := `{"id":"c","model":"m","choices":[{"finish_reason":"stop","message":{"content":"Big city.","annotations":[{"type":"url_citation","url_citation":{"start_index":0,"end_index":3,"url":"https://x","title":"X"}}]}}]}`
	_, fromResponses, _ := messagesToResponses{}.Request([]byte(`{"messages":[]}`))
	response := `{"id":"r","model":"m","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Big city.","annotations":[{"type":"url_citation","start_index":0,"end_index":3,"url":"https://x","title":"X"}]}]}]}`
	for name, body := range map[string][]byte{
		"chat":      fromChat.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(completion)}}).Response.Body,
		"responses": fromResponses.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(response)}}).Response.Body,
	} {
		c := gjson.GetBytes(body, "content.0.citations.0")
		if c.Get("url").String() != "https://x" || c.Get("cited_text").String() != "Big" || c.Get("encrypted_index").String() != backend.GatewayCitation {
			t.Errorf("%s: %s", name, body)
		}
	}
	events, err := eventsOf(t, fromChat.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"id":"c","model":"m","choices":[{"delta":{"content":"Big city."}}]}`,
		`{"choices":[{"delta":{"annotations":[{"type":"url_citation","url_citation":{"start_index":0,"end_index":3,"url":"https://x","title":"X"}}]},"finish_reason":"stop"}]}`,
	}}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	cited := false
	for _, e := range events {
		if e.Get("delta.type").String() == "citations_delta" && e.Get("delta.citation.cited_text").String() == "Big" {
			cited = true
		}
	}
	if !cited {
		t.Errorf("no citation in the stream")
	}
}

// Claude's web search tool reaches each OpenAI API as its web search, with
// the location and, toward Responses, the allowed domains; what neither has
// is reported.
func TestMessages_AsksEitherOpenAIAPIToSearchTheWeb(t *testing.T) {
	body := `{"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":3,"allowed_domains":["x.no"],"user_location":{"type":"approximate","country":"NO"}}],"messages":[]}`
	out, reply, err := messagesToChat{}.Request([]byte(body))
	if err != nil || gjson.GetBytes(out, "web_search_options").Raw != `{"user_location":{"type":"approximate","approximate":{"country":"NO"}}}` || gjson.GetBytes(out, "tools").Exists() || strings.Join(reply.Dropped(), ",") != "tools.max_uses,tools.allowed_domains" {
		t.Errorf("chat: %s, %v, %v", out, reply.Dropped(), err)
	}
	out, reply, err = messagesToResponses{}.Request([]byte(body))
	if err != nil || gjson.GetBytes(out, "tools.0").Raw != `{"type":"web_search","user_location":{"type":"approximate","country":"NO"},"filters":{"allowed_domains":["x.no"]}}` || !strings.Contains(strings.Join(reply.Dropped(), ","), "tools.max_uses") {
		t.Errorf("responses: %s, %v, %v", out, reply.Dropped(), err)
	}
}

// Code execution crosses between Responses and Messages: the tool as the
// other's, and the record of a run as the other's, whole and streamed. Toward
// a Claude client the blocks carry the gateway's ID marker.
func TestMessagesAndResponses_TranslateCodeExecution(t *testing.T) {
	out, _, err := responsesToMessages{}.Request([]byte(`{"tools":[{"type":"code_interpreter","container":{"type":"auto"}}],"input":"hi"}`))
	if err != nil || gjson.GetBytes(out, "tools").Raw != `[{"type":"code_execution_20260120","name":"code_execution"}]` {
		t.Errorf("toward Claude: %s, %v", out, err)
	}
	out, _, err = messagesToResponses{}.Request([]byte(`{"tools":[{"type":"code_execution_20260120","name":"code_execution"}],"messages":[]}`))
	if err != nil || gjson.GetBytes(out, "tools").Raw != `[{"type":"code_interpreter","container":{"type":"auto"}}]` || !strings.Contains(gjson.GetBytes(out, "include").Raw, "code_interpreter_call.outputs") {
		t.Errorf("toward OpenAI: %s, %v", out, err)
	}

	_, toClaude, _ := responsesToMessages{}.Request([]byte(`{"input":"hi"}`))
	message := `{"id":"m","model":"c","stop_reason":"end_turn","container":{"id":"container_1"},"content":[{"type":"server_tool_use","id":"s","name":"bash_code_execution","input":{"command":"python3 -c 'print(2**10)'"}},` +
		`{"type":"bash_code_execution_tool_result","tool_use_id":"s","content":{"type":"bash_code_execution_result","stdout":"1024\n","stderr":"","return_code":0}},{"type":"text","text":"1024"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	got := gjson.ParseBytes(toClaude.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(message)}}).Response.Body)
	if r := got.Get("output.0"); r.Get("type").String() != "code_interpreter_call" || r.Get("code").String() != "python3 -c 'print(2**10)'" || r.Get("outputs.0.logs").String() != "1024\n" || r.Get("container_id").String() != "container_1" {
		t.Errorf("record %s", got.Raw)
	}

	_, toOpenAI, _ := messagesToResponses{}.Request([]byte(`{"messages":[]}`))
	response := `{"id":"r","model":"m","status":"completed","output":[{"id":"ci_1","type":"code_interpreter_call","status":"completed","code":"2**10","outputs":[{"type":"logs","logs":"1024"}]},{"type":"message","content":[{"type":"output_text","text":"1024"}]}]}`
	blocks := gjson.GetBytes(toOpenAI.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(response)}}).Response.Body, "content")
	if blocks.Get("0.type").String() != "server_tool_use" || blocks.Get("0.id").String() != backend.GatewayServerTool+"ci_1" || blocks.Get("0.input.command").String() != "2**10" ||
		blocks.Get("1.tool_use_id").String() != backend.GatewayServerTool+"ci_1" || blocks.Get("1.content.stdout").String() != "1024" {
		t.Errorf("blocks %s", blocks.Raw)
	}
	events, err := eventsOf(t, toOpenAI.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"type":"response.created","response":{"id":"r","model":"m"}}`,
		`{"type":"response.output_item.added","item":{"id":"ci_1","type":"code_interpreter_call","status":"in_progress","code":""}}`,
		`{"type":"response.output_item.done","item":{"id":"ci_1","type":"code_interpreter_call","status":"completed","code":"2**10","outputs":[{"type":"logs","logs":"1024"}]}}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	}}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	var started []string
	for _, e := range events {
		if e.Get("type").String() == "content_block_start" {
			started = append(started, e.Get("content_block.type").String())
		}
	}
	if strings.Join(started, ",") != "server_tool_use,bash_code_execution_tool_result" {
		t.Errorf("streamed blocks %v", started)
	}
}

// OpenAI's record of a search reaches a Messages client as the blocks Claude
// writes a search with, marked as the gateway's, whole and streamed.
func TestMessagesToResponses_RepliesWithSearchRecords(t *testing.T) {
	_, reply, _ := messagesToResponses{}.Request([]byte(`{"messages":[]}`))
	search := `{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"oslo"}}`
	response := `{"id":"r","model":"m","status":"completed","output":[` + search + `,{"type":"message","content":[{"type":"output_text","text":"Big city."}]}]}`
	blocks := gjson.GetBytes(reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(response)}}).Response.Body, "content")
	if blocks.Get("0.type").String() != "server_tool_use" || blocks.Get("0.name").String() != "web_search" || blocks.Get("0.id").String() != backend.GatewayServerTool+"ws_1" || blocks.Get("0.input.query").String() != "oslo" ||
		blocks.Get("1.type").String() != "web_search_tool_result" || blocks.Get("1.tool_use_id").String() != backend.GatewayServerTool+"ws_1" || blocks.Get("2.text").String() != "Big city." {
		t.Errorf("blocks %s", blocks.Raw)
	}
	events, err := eventsOf(t, reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"type":"response.created","response":{"id":"r","model":"m"}}`,
		`{"type":"response.output_item.added","item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}`,
		`{"type":"response.output_item.done","item":` + search + `}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	}}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	var started []string
	for _, e := range events {
		if e.Get("type").String() == "content_block_start" {
			started = append(started, e.Get("content_block.type").String())
		}
	}
	if strings.Join(started, ",") != "server_tool_use,web_search_tool_result" {
		t.Errorf("streamed blocks %v", started)
	}
}

// Remote MCP crosses between Responses and Messages: a server and the tools
// allowed on it as the other's, approval required refused toward Claude,
// which runs MCP tools without asking, and the record of a call as the
// other's, its Claude blocks marked as the gateway's.
func TestMessagesAndResponses_TranslateMCP(t *testing.T) {
	out, _, err := responsesToMessages{}.Request([]byte(`{"tools":[{"type":"mcp","server_label":"wiki","server_url":"https://m","authorization":"k","allowed_tools":["read"],"require_approval":"never"}],"input":"hi"}`))
	if err != nil || gjson.GetBytes(out, "mcp_servers").Raw != `[{"type":"url","url":"https://m","name":"wiki","authorization_token":"k"}]` ||
		gjson.GetBytes(out, "tools").Raw != `[{"type":"mcp_toolset","mcp_server_name":"wiki","default_config":{"enabled":false},"configs":{"read":{"enabled":true}}}]` {
		t.Errorf("toward Claude: %s, %v", out, err)
	}
	if _, _, err := (responsesToMessages{}).Request([]byte(`{"tools":[{"type":"mcp","server_label":"wiki","server_url":"https://m"}],"input":"hi"}`)); err == nil {
		t.Error("an MCP tool that requires approval went to Claude")
	}
	out, _, err = messagesToResponses{}.Request([]byte(`{"mcp_servers":[{"type":"url","url":"https://m","name":"wiki","authorization_token":"k"}],"tools":[{"type":"mcp_toolset","mcp_server_name":"wiki","default_config":{"enabled":false},"configs":{"read":{"enabled":true}}}],"messages":[]}`))
	if err != nil || gjson.GetBytes(out, "tools").Raw != `[{"type":"mcp","server_label":"wiki","server_url":"https://m","authorization":"k","require_approval":"never","allowed_tools":["read"]}]` || gjson.GetBytes(out, "mcp_servers").Exists() {
		t.Errorf("toward OpenAI: %s, %v", out, err)
	}

	_, toClaude, _ := responsesToMessages{}.Request([]byte(`{"input":"hi"}`))
	message := `{"id":"m","model":"c","stop_reason":"end_turn","content":[{"type":"mcp_tool_use","id":"u","name":"read","server_name":"wiki","input":{"repo":"x"}},{"type":"mcp_tool_result","tool_use_id":"u","is_error":false,"content":[{"type":"text","text":"pages"}]},{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	record := gjson.GetBytes(toClaude.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(message)}}).Response.Body, "output.0")
	if record.Get("type").String() != "mcp_call" || record.Get("server_label").String() != "wiki" || record.Get("arguments").String() != `{"repo":"x"}` || record.Get("output").String() != "pages" {
		t.Errorf("record %s", record.Raw)
	}
	_, toOpenAI, _ := messagesToResponses{}.Request([]byte(`{"messages":[]}`))
	response := `{"id":"r","model":"m","status":"completed","output":[{"id":"mcp_1","type":"mcp_call","name":"read","server_label":"wiki","arguments":"{\"repo\":\"x\"}","output":"pages"},{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`
	blocks := gjson.GetBytes(toOpenAI.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(response)}}).Response.Body, "content")
	if blocks.Get("0.type").String() != "mcp_tool_use" || blocks.Get("0.id").String() != backend.GatewayServerTool+"mcp_1" || blocks.Get("0.input.repo").String() != "x" || blocks.Get("1.content.0.text").String() != "pages" {
		t.Errorf("blocks %s", blocks.Raw)
	}
}

// OpenAI's shell tool and Claude's bash tool cross as each other: the tool,
// a call and its result in history, and a call in the reply, whole and
// streamed.
func TestMessagesAndResponses_TranslateShell(t *testing.T) {
	request := `{"tools":[{"type":"shell"}],"input":[` +
		`{"type":"shell_call","call_id":"c1","status":"completed","action":{"commands":["ls","pwd"]}},` +
		`{"type":"shell_call_output","call_id":"c1","output":[{"stdout":"a\n","stderr":"","outcome":{"type":"exit","exit_code":0}},{"stdout":"","stderr":"no\n","outcome":{"type":"exit","exit_code":2}}]}]}`
	out, toClaude, err := responsesToMessages{}.Request([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "tools").Raw; got != `[{"type":"bash_20250124","name":"bash"}]` {
		t.Errorf("tools %s", got)
	}
	use := gjson.GetBytes(out, `messages.#(role=="assistant").content.0`)
	if use.Get("type").String() != "tool_use" || use.Get("name").String() != "bash" || use.Get("input.command").String() != "ls\npwd" {
		t.Errorf("tool_use %s", use.Raw)
	}
	result := gjson.GetBytes(out, `messages.#(role=="user").content.0`)
	if result.Get("tool_use_id").String() != "c1" || !result.Get("is_error").Bool() || !strings.Contains(result.Get("content").String(), "[exit code 2]") {
		t.Errorf("tool_result %s", result.Raw)
	}

	message := `{"id":"m","model":"c","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"command":"uname -a"}}],"usage":{"input_tokens":1,"output_tokens":1}}`
	got := gjson.ParseBytes(toClaude.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(message)}}).Response.Body)
	if call := got.Get("output.0"); call.Get("type").String() != "shell_call" || call.Get("call_id").String() != "t1" || call.Get("action.commands").Raw != `["uname -a"]` {
		t.Errorf("reply %s", got.Raw)
	}
	events, err := eventsOf(t, toClaude.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"type":"message_start","message":{"id":"m","model":"c","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"bash","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"uname -a\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	var streamed gjson.Result
	for _, e := range events {
		if e.Get("type").String() == "response.output_item.done" {
			streamed = e.Get("item")
		}
	}
	if streamed.Get("type").String() != "shell_call" || streamed.Get("action.commands").Raw != `["uname -a"]` {
		t.Errorf("streamed %s", streamed.Raw)
	}

	request = `{"tools":[{"type":"bash_20250124","name":"bash"}],"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"command":"ls"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a\n","is_error":true}]}]}`
	out, toOpenAI, err := messagesToResponses{}.Request([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "tools").Raw; got != `[{"type":"shell"}]` {
		t.Errorf("tools %s", got)
	}
	call := gjson.GetBytes(out, `input.#(type=="shell_call")`)
	if call.Get("call_id").String() != "t1" || call.Get("action.commands").Raw != `["ls"]` {
		t.Errorf("shell_call %s", call.Raw)
	}
	output := gjson.GetBytes(out, `input.#(type=="shell_call_output")`)
	if output.Get("call_id").String() != "t1" || output.Get("output.0.stdout").String() != "a\n" || output.Get("output.0.outcome.exit_code").Int() != 1 {
		t.Errorf("shell_call_output %s", output.Raw)
	}

	response := `{"id":"r","model":"m","status":"completed","output":[{"id":"sh_1","type":"shell_call","call_id":"c2","status":"completed","action":{"commands":["ls","pwd"]}}]}`
	reply := gjson.ParseBytes(toOpenAI.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(response)}}).Response.Body)
	if use := reply.Get("content.0"); use.Get("type").String() != "tool_use" || use.Get("id").String() != "c2" || use.Get("name").String() != "bash" || use.Get("input.command").String() != "ls\npwd" || reply.Get("stop_reason").String() != "tool_use" {
		t.Errorf("reply %s", reply.Raw)
	}
	events, err = eventsOf(t, toOpenAI.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"type":"response.created","response":{"id":"r","model":"m"}}`,
		`{"type":"response.output_item.added","item":{"id":"sh_1","type":"shell_call","call_id":"c2","status":"in_progress","action":{"commands":[]}}}`,
		`{"type":"response.output_item.done","item":{"id":"sh_1","type":"shell_call","call_id":"c2","status":"completed","action":{"commands":["ls"]}}}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	}}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, e := range events {
		switch e.Get("type").String() {
		case "content_block_start":
			lines = append(lines, e.Get("content_block.type").String()+":"+e.Get("content_block.name").String())
		case "content_block_delta":
			lines = append(lines, e.Get("delta.partial_json").String())
		case "message_delta":
			lines = append(lines, e.Get("delta.stop_reason").String())
		}
	}
	if strings.Join(lines, "|") != `tool_use:bash|{"command":"ls"}|tool_use` {
		t.Errorf("streamed %v", lines)
	}
}

// File editing crosses as a function tool: apply_patch reaches Claude as a
// tool of that name taking the operation, with its call and output in
// history and its call in the reply, whole and streamed; Claude's text
// editor reaches OpenAI as a function with its commands.
func TestMessagesAndResponses_TranslateFileEditing(t *testing.T) {
	request := `{"tools":[{"type":"apply_patch"}],"input":[` +
		`{"type":"apply_patch_call","call_id":"c1","status":"completed","operation":{"type":"delete_file","path":"old.txt"}},` +
		`{"type":"apply_patch_call_output","call_id":"c1","status":"failed","output":"no such file"}]}`
	out, toClaude, err := responsesToMessages{}.Request([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	tool := gjson.GetBytes(out, "tools.0")
	if !gjson.ValidBytes(out) || tool.Get("name").String() != "apply_patch" || tool.Get("type").Exists() || tool.Get("input_schema.properties.diff.type").String() != "string" || !strings.Contains(tool.Get("description").String(), "@@") {
		t.Errorf("tool %s", tool.Raw)
	}
	use := gjson.GetBytes(out, `messages.#(role=="assistant").content.0`)
	if use.Get("name").String() != "apply_patch" || use.Get("id").String() != "c1" || use.Get("input.path").String() != "old.txt" {
		t.Errorf("tool_use %s", use.Raw)
	}
	result := gjson.GetBytes(out, `messages.#(role=="user").content.0`)
	if result.Get("tool_use_id").String() != "c1" || !result.Get("is_error").Bool() || result.Get("content").String() != "no such file" {
		t.Errorf("tool_result %s", result.Raw)
	}

	operation := `{"type":"update_file","path":"app.py","diff":"@@\n-a\n+b"}`
	message := `{"id":"m","model":"c","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"apply_patch","input":` + operation + `}],"usage":{"input_tokens":1,"output_tokens":1}}`
	got := gjson.ParseBytes(toClaude.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(message)}}).Response.Body)
	if call := got.Get("output.0"); call.Get("type").String() != "apply_patch_call" || call.Get("call_id").String() != "t1" || call.Get("operation").Raw != operation || !strings.HasPrefix(call.Get("id").String(), "apc_") {
		t.Errorf("reply %s", got.Raw)
	}
	events, err := eventsOf(t, toClaude.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"type":"message_start","message":{"id":"m","model":"c","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"apply_patch","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"type\":\"update_file\","}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"path\":\"app.py\",\"diff\":\"@@\\n-a\\n+b\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	var streamed gjson.Result
	for _, e := range events {
		if e.Get("type").String() == "response.output_item.done" {
			streamed = e.Get("item")
		}
	}
	if streamed.Get("type").String() != "apply_patch_call" || streamed.Get("operation.diff").String() != "@@\n-a\n+b" {
		t.Errorf("streamed %s", streamed.Raw)
	}

	out, _, err = messagesToResponses{}.Request([]byte(`{"tools":[{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"}],"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	function := gjson.GetBytes(out, "tools.0")
	if function.Get("type").String() != "function" || function.Get("name").String() != "str_replace_based_edit_tool" || function.Get("strict").Bool() ||
		function.Get("parameters.properties.command.enum").Raw != `["view","create","str_replace","insert"]` || !strings.Contains(function.Get("description").String(), "str_replace") {
		t.Errorf("function %s", function.Raw)
	}
}

// An earlier text editor, and a function named like the tool that stands
// for a client's shell or apply_patch, are refused.
func TestMessagesAndResponses_RefuseEditingToolsThatCantCross(t *testing.T) {
	if _, _, err := (messagesToResponses{}).Request([]byte(`{"tools":[{"type":"text_editor_20250124","name":"str_replace_editor"}],"messages":[]}`)); err == nil || !strings.Contains(err.Error(), "use 'text_editor_20250728'") {
		t.Errorf("earlier editor: %v", err)
	}
	for _, tools := range []string{
		`[{"type":"apply_patch"},{"type":"function","name":"apply_patch","parameters":{}}]`,
		`[{"type":"shell"},{"type":"function","name":"bash","parameters":{}}]`,
	} {
		if _, _, err := (responsesToMessages{}).Request([]byte(`{"tools":` + tools + `,"input":"hi"}`)); err == nil || !strings.Contains(err.Error(), "can't be sent beside") {
			t.Errorf("%s: %v", tools, err)
		}
	}
}
