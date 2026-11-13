package translate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/anthropic"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

func newBackend(t *testing.T, f *testkit.Anthropic, extra string) *anthropic.Backend {
	t.Helper()
	b, err := anthropic.New(testkit.Entry(t, "anthropic", `{type: anthropic, baseUrl: "`+f.URL+`", apiKey: test-key`+extra+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// thinkingAndTools is a message with each kind of block a chat completion
// can carry.
const thinkingAndTools = `[{"type":"thinking","thinking":"Two cities.","signature":"sig"},{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_1","name":"weather","input":{"city":"Paris"}},{"type":"tool_use","id":"toolu_2","name":"weather","input":{"city":"Rome"}}]`

// chat runs a Chat Completions body along the route the gateway would.
func chat(t *testing.T, b *anthropic.Backend, body string) backend.ChatResult {
	t.Helper()
	out, reply, err := chatToMessages{}.Request([]byte(body))
	var refused *Refusal
	if errors.As(err, &refused) {
		return backend.ChatResult{Response: refused.Response}
	}
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Chat(context.Background(), backend.ChatRequest{Protocol: backend.AnthropicMessages, Body: out, Stream: gjson.GetBytes(out, "stream").Bool()})
	if err != nil {
		t.Fatal(err)
	}
	return reply.Result(res)
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

// chunks is a ChatStream of fixed payloads, ending with the given error.
type chunks struct {
	data []string
	end  error
}

func (c *chunks) Next() ([]byte, error) {
	if len(c.data) == 0 {
		return nil, c.end
	}
	next := c.data[0]
	c.data = c.data[1:]
	return []byte(next), nil
}

func (c *chunks) Close() error { return nil }

func eventsOf(t *testing.T, s backend.ChatStream) ([]gjson.Result, error) {
	t.Helper()
	var events []gjson.Result
	for {
		data, err := s.Next()
		if err != nil {
			if err == io.EOF {
				return events, nil
			}
			return events, err
		}
		if !json.Valid(data) {
			t.Fatalf("not JSON: %s", data)
		}
		events = append(events, gjson.ParseBytes(data))
	}
}

// eventLines describes each Responses event by its type and what it carries,
// and checks that the events are numbered in order from 0.
func eventLines(t *testing.T, events []gjson.Result) []string {
	t.Helper()
	var lines []string
	for i, e := range events {
		if e.Get("sequence_number").Int() != int64(i) {
			t.Errorf("event %d numbered %d", i, e.Get("sequence_number").Int())
		}
		line := e.Get("type").String()
		if d := e.Get("delta"); d.Exists() {
			line += ":" + d.String()
		}
		if item := e.Get("item"); item.Exists() {
			line += ":" + item.Get("type").String()
		}
		lines = append(lines, line)
	}
	return lines
}

// responsesStreamOf is the events of a response with reasoning in two
// summary parts, text, and a function call.
func responsesStreamOf(end string) []string {
	return []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-x","created_at":5,"output":[]}}`,
		`{"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","summary_index":0}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"one"}`,
		`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","summary_index":1}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"two"}`,
		`{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"one"},{"type":"summary_text","text":"two"}],"encrypted_content":"gAAA"}}`,
		`{"type":"response.output_item.added","item":{"id":"msg_1","type":"message","content":[]}}`,
		`{"type":"response.content_part.added","item_id":"msg_1","part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":"hi"}`,
		`{"type":"response.output_item.done","item":{"id":"msg_1","type":"message"}}`,
		`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"c1","name":"f","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"a\":1}"}`,
		`{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"}}`,
		`{"type":"` + end + `","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":4,"output_tokens":5}}}`,
	}
}
