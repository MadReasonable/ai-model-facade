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
