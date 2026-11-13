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

// A Responses request becomes a chat completion request with the client's keys
// in their order: the instructions and a developer message as the first
// message, an assistant's text and its calls as one message, each output as a
// tool message, the settings under their Chat Completions names, and what has
// no counterpart left out and reported.
func TestResponsesToChat_TranslatesARequest(t *testing.T) {
	body := `{"model":"m","instructions":"be brief","top_k":3,"stream":true,"max_output_tokens":9,"store":true,"metadata":{"a":"b"},` +
		`"reasoning":{"effort":"low","summary":"auto"},"text":{"format":{"type":"json_schema","name":"r","schema":{"type":"object"},"strict":true},"verbosity":"low"},` +
		`"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"},"strict":true}],"tool_choice":{"type":"function","name":"f"},"parallel_tool_calls":false,` +
		`"input":[{"role":"developer","content":"no fluff"},{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"low"}]},` +
		`{"type":"reasoning","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"done"},{"role":"user","content":"and?"}]}`
	out, reply, err := responsesToChat{}.Request([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","top_k":3,"stream":true,"max_tokens":9,"metadata":{"a":"b"},"reasoning_effort":"low",` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object"},"strict":true}},"verbosity":"low",` +
		`"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"},"strict":true}}],"tool_choice":{"type":"function","function":{"name":"f"}},"parallel_tool_calls":false,` +
		`"messages":[{"role":"system","content":"be brief\n\nno fluff"},{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA==","detail":"low"}}]},` +
		`{"role":"assistant","content":"ok","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{\"a\":1}"},"id":"c1"}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"done"},{"role":"user","content":"and?"}],"stream_options":{"include_usage":true}}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
	if got := strings.Join(reply.Dropped(), ","); got != "reasoning,reasoning.summary" {
		t.Errorf("dropped %s", got)
	}
}

// A chat completion becomes a response: reasoning as a reasoning item, the
// text as a message, each tool call as a function call, a cut-off reply as
// incomplete, and usage under Responses' names. The request's settings are
// repeated, as OpenAI repeats them.
func TestResponsesToChat_TranslatesAResponse(t *testing.T) {
	completion := `{"id":"c","model":"m","choices":[{"finish_reason":"length","message":{"reasoning_content":"why","content":"ok","tool_calls":[{"id":"t","function":{"name":"f","arguments":"{\"a\":1}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":1}}}`
	_, reply, _ := responsesToChat{}.Request([]byte(`{"model":"facade","input":"hi","tools":[{"type":"function","name":"f"}]}`))
	res := reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(completion)}})
	got := gjson.ParseBytes(res.Response.Body)
	if got.Get("object").String() != "response" || !strings.HasPrefix(got.Get("id").String(), "resp_") || got.Get("model").String() != "m" || got.Get("status").String() != "incomplete" || got.Get("incomplete_details.reason").String() != "max_output_tokens" {
		t.Errorf("response: %s", got.Raw)
	}
	if kinds := got.Get("output.#.type").Raw; kinds != `["reasoning","message","function_call"]` {
		t.Errorf("output %s", got.Get("output").Raw)
	}
	if got.Get("output.0.content.0.text").String() != "why" || got.Get("output.1.content.0.text").String() != "ok" || got.Get("output.2.call_id").String() != "t" || got.Get("output.2.arguments").String() != `{"a":1}` {
		t.Errorf("output %s", got.Get("output").Raw)
	}
	if u := got.Get("usage"); u.Get("input_tokens").Int() != 10 || u.Get("input_tokens_details.cached_tokens").Int() != 3 || u.Get("output_tokens").Int() != 2 || u.Get("output_tokens_details.reasoning_tokens").Int() != 1 || u.Get("total_tokens").Int() != 12 {
		t.Errorf("usage %s", got.Get("usage").Raw)
	}
	if got.Get("tools.0.name").String() != "f" || got.Get("tool_choice").String() != "auto" || !got.Get("parallel_tool_calls").Bool() {
		t.Errorf("settings: %s", got.Raw)
	}

	failed := reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 503, Body: []byte(`Loading model`), RetryAfter: "5"}})
	if failed.Response.Status != 503 || failed.Response.RetryAfter != "5" || gjson.GetBytes(failed.Response.Body, "error.message").String() != "Loading model" {
		t.Errorf("error: %+v %s", failed.Response, failed.Response.Body)
	}
}

// Chunks stream as Responses events: each item added, its parts and deltas,
// then done; the first tool call streams its arguments, a call interleaved
// with it arrives complete at the end, and the response ends completed, with the
// usage of the last chunk.
func TestResponsesToChat_TranslatesAStream(t *testing.T) {
	_, reply, _ := responsesToChat{}.Request([]byte(`{"model":"facade","input":"hi","stream":true}`))
	result := reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"id":"c","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"th"}}]}`,
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
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added:reasoning", "response.content_part.added", "response.reasoning_text.delta:th",
		"response.reasoning_text.done", "response.content_part.done", "response.output_item.done:reasoning",
		"response.output_item.added:message", "response.content_part.added", "response.output_text.delta:hi",
		"response.output_text.done", "response.content_part.done", "response.output_item.done:message",
		"response.output_item.added:function_call", `response.function_call_arguments.delta:{"a"`, "response.function_call_arguments.delta::1}",
		"response.function_call_arguments.done", "response.output_item.done:function_call",
		"response.output_item.added:function_call", `response.function_call_arguments.delta:{"b":2}`,
		"response.function_call_arguments.done", "response.output_item.done:function_call",
		"response.completed",
	}
	if got := eventLines(t, events); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	last := events[len(events)-1].Get("response")
	if last.Get("status").String() != "completed" || last.Get("model").String() != "m" || last.Get("output.2.arguments").String() != `{"a":1}` || last.Get("output.3.call_id").String() != "t1" || last.Get("usage.input_tokens").Int() != 4 {
		t.Errorf("response.completed: %s", last.Raw)
	}
	if created := events[0].Get("response"); created.Get("id").String() != last.Get("id").String() || created.Get("status").String() != "in_progress" {
		t.Errorf("response.created: %s", created.Raw)
	}
}

// A chat completion request becomes a Responses request with the client's keys
// in their order: each message an input item in its place, system messages
// included, an assistant's tool calls as function calls after its text, a tool
// message as a call's output, the settings under their Responses names, tools
// not strict unless asked, and the request not kept at OpenAI.
func TestChatToResponses_TranslatesARequest(t *testing.T) {
	body := `{"model":"m","max_completion_tokens":9,"reasoning_effort":"low","seed":1,"stream":true,"stream_options":{"include_usage":true},` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object"},"strict":true}},` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"f"}},` +
		`"messages":[{"role":"system","content":"be brief"},{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://x/i.png","detail":"low"}}]},` +
		`{"role":"assistant","content":"ok","reasoning_content":"why","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"done"}]}`
	out, reply, err := chatToResponses{}.Request([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","max_output_tokens":9,"reasoning":{"effort":"low"},"stream":true,` +
		`"text":{"format":{"type":"json_schema","name":"r","schema":{"type":"object"},"strict":true}},` +
		`"tools":[{"type":"function","strict":false,"name":"f","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"f"},` +
		`"input":[{"type":"message","role":"system","content":"be brief"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"https://x/i.png","detail":"low"}]},` +
		`{"type":"message","role":"assistant","content":"ok"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"done"}],"store":false}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
	if got := strings.Join(reply.Dropped(), ","); got != "reasoning_content,seed" {
		t.Errorf("dropped %s", got)
	}
}

// Log probabilities, web search and custom tools have Responses
// counterparts: logprobs as an include and top_logprobs, web_search_options as
// the web_search tool with its location flattened, and custom tools, their
// calls and outputs, and a tool choice naming one, as custom ones there.
func TestChatToResponses_TranslatesLogprobsWebSearchAndCustomTools(t *testing.T) {
	body := `{"logprobs":true,"top_logprobs":2,"web_search_options":{"search_context_size":"low","user_location":{"type":"approximate","approximate":{"country":"NO","city":"Oslo"}}},` +
		`"tools":[{"type":"custom","custom":{"name":"sql","description":"d","format":{"type":"text"}}}],"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[{"type":"custom","custom":{"name":"sql"}}]}},` +
		`"messages":[{"role":"user","content":"count"},{"role":"assistant","tool_calls":[{"id":"c1","type":"custom","custom":{"name":"sql","input":"SELECT 1"}}]},{"role":"tool","tool_call_id":"c1","content":"1"}]}`
	out, _, err := chatToResponses{}.Request([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"include":["message.output_text.logprobs"],"top_logprobs":2,` +
		`"tools":[{"type":"custom","name":"sql","description":"d","format":{"type":"text"}},{"type":"web_search","search_context_size":"low","user_location":{"type":"approximate","country":"NO","city":"Oslo"}}],` +
		`"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"custom","name":"sql"}]},` +
		`"input":[{"type":"message","role":"user","content":"count"},{"type":"custom_tool_call","call_id":"c1","name":"sql","input":"SELECT 1"},{"type":"custom_tool_call_output","call_id":"c1","output":"1"}],"store":false}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
	if out, _, _ := (chatToResponses{}).Request([]byte(`{"logprobs":false,"top_logprobs":2,"messages":[]}`)); gjson.GetBytes(out, "include").Exists() || gjson.GetBytes(out, "top_logprobs").Exists() {
		t.Errorf("logprobs false: %s", out)
	}
}

// A response's citations come back as Chat Completions annotations, their
// ranges counted in the joined content, its log probabilities as the choice's,
// and a custom tool call as one.
func TestChatToResponses_TranslatesCitationsLogprobsAndCustomCalls(t *testing.T) {
	response := `{"status":"completed","output":[{"type":"web_search_call","status":"completed"},{"type":"message","content":[` +
		`{"type":"output_text","text":"Oslé.","annotations":[],"logprobs":[{"token":"Oslé.","logprob":-1,"bytes":[1],"top_logprobs":[]}]},` +
		`{"type":"output_text","text":" Big.","annotations":[{"type":"url_citation","start_index":1,"end_index":4,"url":"https://x","title":"X"}],"logprobs":[{"token":" Big.","logprob":-2,"bytes":[2],"top_logprobs":[]}]}]},` +
		`{"type":"custom_tool_call","call_id":"c1","name":"sql","input":"SELECT 1"}]}`
	_, reply, _ := chatToResponses{}.Request([]byte(`{"messages":[]}`))
	got := gjson.ParseBytes(reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(response)}}).Response.Body)
	choice := got.Get("choices.0")
	if a := choice.Get("message.annotations.0.url_citation"); a.Get("start_index").Int() != 6 || a.Get("end_index").Int() != 9 || a.Get("url").String() != "https://x" {
		t.Errorf("annotation %s", choice.Get("message.annotations").Raw)
	}
	if choice.Get("logprobs.content.#.logprob").Raw != "[-1,-2]" {
		t.Errorf("logprobs %s", choice.Get("logprobs").Raw)
	}
	if call := choice.Get("message.tool_calls.0"); call.Get("type").String() != "custom" || call.Get("custom.input").String() != "SELECT 1" || choice.Get("finish_reason").String() != "tool_calls" {
		t.Errorf("custom call %s", choice.Raw)
	}
}

// Streamed, log probabilities ride on their text chunk's choice, a citation is
// a chunk of annotations counted in the content streamed, and a custom tool
// call opens with its name and streams its input.
func TestChatToResponses_StreamsCitationsLogprobsAndCustomCalls(t *testing.T) {
	_, reply, _ := chatToResponses{}.Request([]byte(`{"messages":[],"stream":true}`))
	events, err := eventsOf(t, reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_item.added","item":{"id":"msg_1","type":"message"}}`,
		`{"type":"response.content_part.added","item_id":"msg_1","part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":"Oslé.","logprobs":[{"token":"Oslé.","logprob":-1}]}`,
		`{"type":"response.content_part.added","item_id":"msg_1","part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":" Big.","logprobs":[]}`,
		`{"type":"response.output_text.annotation.added","item_id":"msg_1","annotation":{"type":"url_citation","start_index":1,"end_index":4,"url":"https://x","title":"X"}}`,
		`{"type":"response.output_item.added","item":{"id":"ctc_1","type":"custom_tool_call","call_id":"c1","name":"sql","input":""}}`,
		`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","delta":"SELECT 1"}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	}}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.Get("choices.0.delta").Raw+" "+e.Get("choices.0.logprobs").Raw)
	}
	want := []string{
		`{"content":"","role":"assistant"} `,
		`{"content":"Oslé."} {"content":[{"token":"Oslé.","logprob":-1}]}`,
		`{"content":" Big."} `,
		`{"annotations":[{"type":"url_citation","url_citation":{"end_index":9,"start_index":6,"title":"X","url":"https://x"}}]} `,
		`{"tool_calls":[{"custom":{"input":"","name":"sql"},"id":"c1","index":0,"type":"custom"}]} `,
		`{"tool_calls":[{"custom":{"input":"SELECT 1"},"index":0}]} `,
		`{} `,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// What Responses can't give is refused with a 400 in OpenAI's shape.
func TestChatToResponses_RefusesWhatResponsesCantServe(t *testing.T) {
	for _, body := range []string{
		`{"n":2,"messages":[]}`,
		`{"stop":["x"],"messages":[]}`,
		`{"audio":{"voice":"x"},"messages":[]}`,
		`{"tools":[{"type":"other"}],"messages":[]}`,
		`{"messages":[{"role":"function","content":"x"}]}`,
	} {
		_, _, err := chatToResponses{}.Request([]byte(body))
		var refused *Refusal
		if !errors.As(err, &refused) || refused.Response.Status != http.StatusBadRequest || gjson.GetBytes(refused.Response.Body, "error.type").String() != "invalid_request_error" {
			t.Errorf("%s: got %v", body, err)
		}
	}
}

// A response becomes a chat completion: reasoning as reasoning_content, text
// as content, a refusal as refusal, function calls as tool calls, usage under
// Chat Completions' names. A failed response is an error.
func TestChatToResponses_TranslatesAResponse(t *testing.T) {
	response := `{"id":"resp_1","created_at":5,"model":"gpt-x","status":"completed","output":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"why"}]},` +
		`{"type":"message","content":[{"type":"output_text","text":"ok"}]},` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"}],` +
		`"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":3},"output_tokens":4,"output_tokens_details":{"reasoning_tokens":2}}}`
	_, reply, _ := chatToResponses{}.Request([]byte(`{"messages":[]}`))
	got := gjson.ParseBytes(reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(response)}}).Response.Body)
	message := got.Get("choices.0.message")
	if got.Get("object").String() != "chat.completion" || got.Get("created").Int() != 5 || message.Get("content").String() != "ok" || message.Get("reasoning_content").String() != "why" ||
		message.Get("tool_calls.0.id").String() != "c1" || message.Get("tool_calls.0.function.arguments").String() != `{"a":1}` || got.Get("choices.0.finish_reason").String() != "tool_calls" {
		t.Errorf("completion %s", got.Raw)
	}
	if u := got.Get("usage"); u.Get("prompt_tokens").Int() != 10 || u.Get("prompt_tokens_details.cached_tokens").Int() != 3 || u.Get("completion_tokens").Int() != 4 || u.Get("completion_tokens_details.reasoning_tokens").Int() != 2 {
		t.Errorf("usage %s", got.Get("usage").Raw)
	}
	cut := reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`)}})
	if gjson.GetBytes(cut.Response.Body, "choices.0.finish_reason").String() != "length" {
		t.Errorf("incomplete: %s", cut.Response.Body)
	}
	failed := reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(`{"status":"failed","error":{"code":"server_error","message":"boom"}}`)}})
	if failed.Response.Status != http.StatusBadGateway || gjson.GetBytes(failed.Response.Body, "error.message").String() != "boom" {
		t.Errorf("failed: %d %s", failed.Response.Status, failed.Response.Body)
	}
}

// Events stream as chunks: the role first, reasoning with its parts joined,
// text, the call opened then its arguments, the finish, then usage.
func TestChatToResponses_TranslatesAStream(t *testing.T) {
	_, reply, _ := chatToResponses{}.Request([]byte(`{"messages":[],"stream":true,"stream_options":{"include_usage":true}}`))
	events, err := eventsOf(t, reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: responsesStreamOf("response.completed")}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.Get("choices.0.delta").Raw+" "+e.Get("choices.0.finish_reason").Raw+e.Get("usage.prompt_tokens").Raw)
	}
	want := []string{
		`{"content":"","role":"assistant"} null`,
		`{"reasoning_content":"one"} null`, `{"reasoning_content":"\n\n"} null`, `{"reasoning_content":"two"} null`,
		`{"content":"hi"} null`,
		`{"tool_calls":[{"function":{"arguments":"","name":"f"},"id":"c1","index":0,"type":"function"}]} null`,
		`{"tool_calls":[{"function":{"arguments":"{\"a\":1}"},"index":0}]} null`,
		`{} "tool_calls"`,
		` 4`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if events[0].Get("id").String() != "chatcmpl-resp_1" || events[0].Get("model").String() != "gpt-x" {
		t.Errorf("first chunk %s", events[0].Raw)
	}
}

// Toward Responses, the deprecated function calling is sent as tools, one
// call at a time, a participant's name is reported, and the reply comes back
// in the legacy shape.
func TestChatToResponses_TranslatesLegacyFunctionCalling(t *testing.T) {
	body := `{"functions":[{"name":"f","parameters":{"type":"object"}}],"function_call":"auto",` +
		`"messages":[{"role":"user","name":"ann","content":"go"},{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{}"}},{"role":"function","name":"f","content":"done"}]}`
	out, reply, err := chatToResponses{}.Request([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tools":[{"type":"function","strict":false,"name":"f","parameters":{"type":"object"}}],"tool_choice":"auto",` +
		`"input":[{"type":"message","role":"user","content":"go"},{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"done"}],` +
		`"parallel_tool_calls":false,"store":false}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
	if got := strings.Join(reply.Dropped(), ","); got != "name" {
		t.Errorf("dropped %s", got)
	}
	response := `{"id":"r","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"}]}`
	got := gjson.ParseBytes(reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(response)}}).Response.Body)
	if choice := got.Get("choices.0"); choice.Get("finish_reason").String() != "function_call" || choice.Get("message.function_call.arguments").String() != `{"a":1}` {
		t.Errorf("completion %s", got.Raw)
	}
}

// Toward Chat Completions, a Responses request keeps what Chat Completions
// has too: custom tools, their calls and outputs, a tool choice naming one or
// listing allowed tools, log probabilities, verbosity, web search with its
// location, and OpenAI's service settings; a web search's record in the
// history becomes text, reported.
func TestResponsesToChat_KeepsWhatChatCompletionsHasToo(t *testing.T) {
	body := `{"include":["message.output_text.logprobs"],"top_logprobs":2,"text":{"verbosity":"low"},"metadata":{"a":"b"},"service_tier":"flex","prompt_cache_key":"k","moderation":{},` +
		`"tools":[{"type":"custom","name":"sql","format":{"type":"text"}},{"type":"web_search","search_context_size":"low","user_location":{"type":"approximate","country":"NO"}}],` +
		`"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"custom","name":"sql"}]},` +
		`"input":[{"role":"user","content":"count"},{"type":"web_search_call","action":{"query":"q"}},{"type":"custom_tool_call","call_id":"c1","name":"sql","input":"SELECT 1"},{"type":"custom_tool_call_output","call_id":"c1","output":"1"}]}`
	out, reply, err := responsesToChat{}.Request([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	got := gjson.ParseBytes(out)
	checks := map[string]string{
		"logprobs":                "true",
		"top_logprobs":            "2",
		"verbosity":               `"low"`,
		"metadata":                `{"a":"b"}`,
		"service_tier":            `"flex"`,
		"prompt_cache_key":        `"k"`,
		"moderation":              "{}",
		"tools":                   `[{"type":"custom","custom":{"name":"sql","format":{"type":"text"}}}]`,
		"web_search_options":      `{"search_context_size":"low","user_location":{"type":"approximate","approximate":{"country":"NO"}}}`,
		"tool_choice":             `{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[{"type":"custom","custom":{"name":"sql"}}]}}`,
		"messages.1.content":      `"[web search: q]"`,
		"messages.1.tool_calls.0": `{"type":"custom","custom":{"name":"sql","input":"SELECT 1"},"id":"c1"}`,
		"messages.2":              `{"role":"tool","tool_call_id":"c1","content":"1"}`,
	}
	for path, want := range checks {
		if v := got.Get(path).Raw; v != want {
			t.Errorf("%s: got %s, want %s", path, v, want)
		}
	}
	if dropped := strings.Join(reply.Dropped(), ","); dropped != "web_search_call" {
		t.Errorf("dropped %s", dropped)
	}
}

// A Chat Completions reply's citations, custom tool calls and log
// probabilities reach a Responses client as its own: url_citation
// annotations, custom_tool_call items, and the text part's logprobs.
func TestResponsesToChat_RepliesWithCitationsCustomCallsAndLogprobs(t *testing.T) {
	_, reply, _ := responsesToChat{}.Request([]byte(`{"model":"facade","input":"hi"}`))
	completion := `{"model":"m","choices":[{"finish_reason":"tool_calls","logprobs":{"content":[{"token":"Big","logprob":-1}]},"message":{"content":"Big city.","annotations":[{"type":"url_citation","url_citation":{"start_index":0,"end_index":3,"url":"https://x","title":"X"}}],` +
		`"tool_calls":[{"id":"c1","type":"custom","custom":{"name":"sql","input":"SELECT 1"}}]}}]}`
	got := gjson.ParseBytes(reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(completion)}}).Response.Body)
	part := got.Get("output.0.content.0")
	if part.Get("annotations.0").Raw != `{"end_index":3,"start_index":0,"title":"X","type":"url_citation","url":"https://x"}` || part.Get("logprobs.0.token").String() != "Big" {
		t.Errorf("text part %s", part.Raw)
	}
	if call := got.Get("output.1"); call.Get("type").String() != "custom_tool_call" || call.Get("input").String() != "SELECT 1" || call.Get("call_id").String() != "c1" {
		t.Errorf("call %s", call.Raw)
	}
	events, err := eventsOf(t, reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"model":"m","choices":[{"delta":{"content":"Big city."}}]}`,
		`{"choices":[{"delta":{"annotations":[{"type":"url_citation","url_citation":{"start_index":0,"end_index":3,"url":"https://x","title":"X"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"custom","custom":{"name":"sql","input":"SELECT"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"custom":{"input":" 1"}}]},"finish_reason":"tool_calls"}]}`,
	}}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Get("type").String())
	}
	all := strings.Join(kinds, ",")
	for _, want := range []string{"response.output_text.annotation.added", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done"} {
		if !strings.Contains(all, want) {
			t.Errorf("%s missing from %s", want, all)
		}
	}
	if last := events[len(events)-1].Get("response.output.1"); last.Get("input").String() != "SELECT 1" {
		t.Errorf("completed call %s", last.Raw)
	}
}

// A Responses client's shell and apply_patch reach a Chat backend as
// functions of their names: their calls and outputs in history as tool calls
// and tool messages, and a call in the reply as the client tool's record,
// whole and streamed, held in a stream until its arguments are complete.
func TestResponsesToChat_OffersClientToolsAsFunctions(t *testing.T) {
	request := `{"tools":[{"type":"shell"},{"type":"apply_patch"}],"input":[` +
		`{"type":"shell_call","call_id":"s1","status":"completed","action":{"commands":["ls"]}},` +
		`{"type":"shell_call_output","call_id":"s1","output":[{"stdout":"a\n","stderr":"","outcome":{"type":"exit","exit_code":1}}]},` +
		`{"type":"apply_patch_call","call_id":"p1","status":"completed","operation":{"type":"delete_file","path":"x"}},` +
		`{"type":"apply_patch_call_output","call_id":"p1","status":"failed","output":"no file"}]}`
	out, reply, err := responsesToChat{}.Request([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	tools := gjson.GetBytes(out, "tools")
	if tools.Get("0.function.name").String() != "shell" || tools.Get("0.function.parameters.required.0").String() != "commands" ||
		tools.Get("1.function.name").String() != "apply_patch" || !strings.Contains(tools.Get("1.function.description").String(), "@@") {
		t.Errorf("tools %s", tools.Raw)
	}
	messages := gjson.GetBytes(out, "messages")
	if call := messages.Get("0.tool_calls.0"); call.Get("function.name").String() != "shell" || call.Get("function.arguments").String() != `{"commands":["ls"]}` {
		t.Errorf("shell call %s", messages.Raw)
	}
	if output := messages.Get("1"); output.Get("role").String() != "tool" || output.Get("tool_call_id").String() != "s1" || output.Get("content").String() != "a\n\n[exit code 1]" {
		t.Errorf("shell output %s", output.Raw)
	}
	if call := messages.Get("2.tool_calls.0"); call.Get("function.name").String() != "apply_patch" || gjson.Get(call.Get("function.arguments").String(), "path").String() != "x" {
		t.Errorf("patch call %s", messages.Raw)
	}
	if output := messages.Get("3"); output.Get("content").String() != "The patch failed: no file" {
		t.Errorf("patch output %s", output.Raw)
	}

	completion := `{"id":"c","model":"m","choices":[{"message":{"role":"assistant","tool_calls":[` +
		`{"id":"t1","type":"function","function":{"name":"shell","arguments":"{\"commands\":[\"pwd\"],\"timeout_ms\":500}"}},` +
		`{"id":"t2","type":"function","function":{"name":"apply_patch","arguments":"{\"type\":\"create_file\",\"path\":\"n\",\"diff\":\"+a\"}"}}]},"finish_reason":"tool_calls"}]}`
	got := gjson.ParseBytes(reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(completion)}}).Response.Body)
	if shell := got.Get("output.0"); shell.Get("type").String() != "shell_call" || shell.Get("call_id").String() != "t1" || shell.Get("action.commands").Raw != `["pwd"]` || shell.Get("action.timeout_ms").Int() != 500 {
		t.Errorf("shell %s", got.Raw)
	}
	if patch := got.Get("output.1"); patch.Get("type").String() != "apply_patch_call" || patch.Get("operation.path").String() != "n" {
		t.Errorf("patch %s", got.Raw)
	}
	events, err := eventsOf(t, reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"id":"c","model":"m","choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"shell","arguments":"{\"comm"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ands\":[\"pwd\"]}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	var done []string
	for _, e := range events {
		if e.Get("type").String() == "response.output_item.done" {
			done = append(done, e.Get("item.type").String()+":"+e.Get("item.action.commands").Raw)
		}
		if e.Get("type").String() == "response.function_call_arguments.delta" {
			t.Errorf("streamed a function call: %s", e.Raw)
		}
	}
	if strings.Join(done, ",") != `shell_call:["pwd"]` {
		t.Errorf("streamed %v", done)
	}
	if _, _, err := (responsesToChat{}).Request([]byte(`{"tools":[{"type":"shell"},{"type":"function","name":"shell","parameters":{}}],"input":"hi"}`)); err == nil || !strings.Contains(err.Error(), "can't be sent beside") {
		t.Errorf("clash: %v", err)
	}
}
