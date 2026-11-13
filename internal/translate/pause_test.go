package translate

import (
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

func whole(body string) backend.ChatResult {
	return backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(body)}}
}

const (
	pausedMessage = `{"id":"m1","type":"message","stop_reason":"pause_turn","content":[` +
		`{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"q"}},` +
		`{"type":"web_search_tool_result","tool_use_id":"s1","content":[]}],` +
		`"usage":{"input_tokens":10,"output_tokens":2,"server_tool_use":{"web_search_requests":1},"service_tier":"standard"}}`
	endedMessage = `{"id":"m2","type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],` +
		`"usage":{"input_tokens":20,"output_tokens":3,"server_tool_use":{"web_search_requests":2},"service_tier":"standard"}}`
)

// A paused whole reply is sent back with its blocks as the assistant's turn,
// and the parts come back as one message.
func TestResend_JoinsAPausedReply(t *testing.T) {
	var sent []string
	send := func(body []byte) (backend.ChatResult, error) {
		sent = append(sent, string(body))
		return whole(endedMessage), nil
	}
	got, err := Resend(backend.AnthropicMessages, []byte(`{"model":"c","messages":[{"role":"user","content":"hi"}]}`), whole(pausedMessage), send)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 {
		t.Fatalf("sent %d times", len(sent))
	}
	if turn := gjson.Get(sent[0], "messages.1"); turn.Get("role").String() != "assistant" || turn.Get("content.#").Int() != 2 || turn.Get("content.0.id").String() != "s1" {
		t.Errorf("sent %s", sent[0])
	}
	message := gjson.ParseBytes(got.Response.Body)
	if message.Get("id").String() != "m2" || message.Get("stop_reason").String() != "end_turn" || message.Get("content.#").Int() != 3 || message.Get("content.2.text").String() != "done" {
		t.Errorf("message %s", message.Raw)
	}
	if usage := message.Get("usage"); usage.Get("input_tokens").Int() != 30 || usage.Get("output_tokens").Int() != 5 || usage.Get("server_tool_use.web_search_requests").Int() != 3 || usage.Get("service_tier").String() != "standard" {
		t.Errorf("usage %s", usage.Raw)
	}
}

// A paused turn that ends with the assistant's own prefill is sent back as
// that turn carried on, its text a block before the paused part's.
func TestResend_CarriesOnAPrefilledTurn(t *testing.T) {
	var sent string
	send := func(body []byte) (backend.ChatResult, error) {
		sent = string(body)
		return whole(endedMessage), nil
	}
	if _, err := Resend(backend.AnthropicMessages, []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"Sure:"}]}`), whole(pausedMessage), send); err != nil {
		t.Fatal(err)
	}
	if turns := gjson.Get(sent, "messages.#").Int(); turns != 2 {
		t.Errorf("%d turns in %s", turns, sent)
	}
	if content := gjson.Get(sent, "messages.1.content"); content.Get("#").Int() != 3 || content.Get("0.text").String() != "Sure:" || content.Get("1.type").String() != "server_tool_use" {
		t.Errorf("sent %s", sent)
	}
}

// A turn that keeps pausing is sent back maxResends times, and its last
// pause ends the reply.
func TestResend_StopsAtTheLimit(t *testing.T) {
	sends := 0
	send := func(body []byte) (backend.ChatResult, error) {
		sends++
		return whole(pausedMessage), nil
	}
	got, err := Resend(backend.AnthropicMessages, []byte(`{"messages":[]}`), whole(pausedMessage), send)
	if err != nil {
		t.Fatal(err)
	}
	message := gjson.ParseBytes(got.Response.Body)
	if sends != maxResends || message.Get("stop_reason").String() != "pause_turn" || message.Get("content.#").Int() != 2*(maxResends+1) {
		t.Errorf("%d sends, message %s", sends, message.Raw)
	}
}

// A reply that didn't pause, a failed resend's error response, and a reply
// in another protocol come back as they are.
func TestResend_LeavesOtherRepliesAlone(t *testing.T) {
	never := func(body []byte) (backend.ChatResult, error) {
		t.Fatal("sent")
		return backend.ChatResult{}, nil
	}
	for _, c := range []struct {
		to   backend.Protocol
		body string
	}{{backend.AnthropicMessages, endedMessage}, {backend.OpenAIResponses, pausedMessage}} {
		got, err := Resend(c.to, []byte(`{"messages":[]}`), whole(c.body), never)
		if err != nil || string(got.Response.Body) != c.body {
			t.Errorf("%s: %s, %v", c.to, got.Response.Body, err)
		}
	}
	failed := func(body []byte) (backend.ChatResult, error) {
		return backend.ChatResult{Response: &backend.ChatResponse{Status: 529, Body: []byte(`{"type":"error"}`)}}, nil
	}
	got, err := Resend(backend.AnthropicMessages, []byte(`{"messages":[]}`), whole(pausedMessage), failed)
	if err != nil || got.Response.Status != 529 {
		t.Errorf("%+v, %v", got.Response, err)
	}
}

// A paused stream is sent back with the blocks its events built, and goes on
// with the stream that comes back as one message: one message_start, blocks
// numbered on, and one message_delta with the usage of both parts.
func TestResend_JoinsAPausedStream(t *testing.T) {
	paused := &chunks{end: io.EOF, data: []string{
		`{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"look"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"s1","name":"web_search","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"q\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"web_search_tool_result","tool_use_id":"s1","content":[]}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"pause_turn"},"usage":{"input_tokens":10,"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	}}
	var sent string
	send := func(body []byte) (backend.ChatResult, error) {
		sent = string(body)
		return backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
			`{"type":"message_start","message":{"id":"m2","usage":{"input_tokens":20,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":20,"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		}}}, nil
	}
	got, err := Resend(backend.AnthropicMessages, []byte(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`), backend.ChatResult{Stream: paused}, send)
	if err != nil {
		t.Fatal(err)
	}
	events, err := eventsOf(t, got.Stream)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, e := range events {
		line := e.Get("type").String()
		if index := e.Get("index"); index.Exists() {
			line += ":" + index.String()
		}
		if usage := e.Get("usage"); usage.Exists() {
			line += ":" + usage.Get("input_tokens").String() + "/" + usage.Get("output_tokens").String()
		}
		lines = append(lines, line)
	}
	want := "message_start,content_block_start:0,content_block_delta:0,content_block_delta:0,content_block_stop:0," +
		"content_block_start:1,content_block_delta:1,content_block_delta:1,content_block_stop:1,content_block_start:2,content_block_stop:2," +
		"content_block_start:3,content_block_delta:3,content_block_stop:3,message_delta:30/5,message_stop"
	if strings.Join(lines, ",") != want {
		t.Errorf("events\n%s\nwant\n%s", strings.Join(lines, ","), want)
	}
	content := gjson.Get(sent, "messages.1.content")
	if content.Get("0.thinking").String() != "look" || content.Get("0.signature").String() != "sig" || content.Get("1.input.query").String() != "q" || content.Get("2.type").String() != "web_search_tool_result" {
		t.Errorf("sent %s", sent)
	}
}
