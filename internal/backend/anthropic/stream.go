package anthropic

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// chunkStream turns a Messages event stream into Chat Completions chunks, as
// completion does a whole response. An event yields zero or more chunks:
//
//   - message_start, a first chunk with the assistant role;
//   - a text, thinking or tool-input delta, a chunk with content,
//     reasoning_content or the call's arguments;
//   - a tool_use block's start, a chunk opening the tool call, numbered in
//     the order the calls arrive, as OpenAI numbers them;
//   - message_delta, the finish chunk, then a usage chunk with no choices
//     if the client set stream_options.include_usage.
//
// The stream ends at message_stop, since Messages sends no [DONE]; signature
// deltas, which Chat Completions has no place for, pings and block stops
// yield nothing.
type chunkStream struct {
	events       *backend.SSEStream
	includeUsage bool
	created      int64
	pending      [][]byte
	done         bool

	id, model              string
	startUsage, deltaUsage gjson.Result
	calls                  map[int64]int // block index -> tool call index
}

func newChunkStream(events *backend.SSEStream, includeUsage bool, created int64) *chunkStream {
	return &chunkStream{events: events, includeUsage: includeUsage, created: created, calls: map[int64]int{}}
}

func (s *chunkStream) Next() ([]byte, error) {
	for len(s.pending) == 0 {
		if s.done {
			return nil, io.EOF
		}
		data, err := s.events.Next()
		var streamErr *backend.StreamError
		if errors.As(err, &streamErr) {
			return nil, &backend.StreamError{Data: openAIError(streamErr.Data)}
		}
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF // the body ended before message_stop
		}
		if err != nil {
			return nil, err
		}
		s.translate(gjson.ParseBytes(data))
	}
	next := s.pending[0]
	s.pending = s.pending[1:]
	return next, nil
}

func (s *chunkStream) translate(event gjson.Result) {
	switch event.Get("type").String() {
	case "message_start":
		s.id, s.model = event.Get("message.id").String(), event.Get("message.model").String()
		s.startUsage = event.Get("message.usage")
		s.send(map[string]any{"role": "assistant", "content": ""}, nil)
	case "content_block_start":
		block := event.Get("content_block")
		if block.Get("type").String() != "tool_use" {
			return
		}
		n := len(s.calls)
		s.calls[event.Get("index").Int()] = n
		s.send(map[string]any{"tool_calls": []any{map[string]any{
			"index":    n,
			"id":       block.Get("id").String(),
			"type":     "function",
			"function": map[string]any{"name": block.Get("name").String(), "arguments": ""},
		}}}, nil)
	case "content_block_delta":
		delta := event.Get("delta")
		switch delta.Get("type").String() {
		case "text_delta":
			s.send(map[string]any{"content": delta.Get("text").String()}, nil)
		case "thinking_delta":
			s.send(map[string]any{"reasoning_content": delta.Get("thinking").String()}, nil)
		case "input_json_delta":
			if partial := delta.Get("partial_json").String(); partial != "" {
				s.send(map[string]any{"tool_calls": []any{map[string]any{
					"index":    s.calls[event.Get("index").Int()],
					"function": map[string]any{"arguments": partial},
				}}}, nil)
			}
		}
	case "message_delta":
		s.deltaUsage = event.Get("usage")
		s.send(map[string]any{}, finishReason(event.Get("delta.stop_reason").String()))
		if s.includeUsage {
			s.queue(map[string]any{"choices": []any{}, "usage": usageOf(s.usage)})
		}
	case "message_stop":
		s.done = true
	}
}

// usage reads a usage count from message_delta, whose counts are
// cumulative and include the input counts (verified), else from
// message_start.
func (s *chunkStream) usage(path string) gjson.Result {
	if v := s.deltaUsage.Get(path); v.Exists() {
		return v
	}
	return s.startUsage.Get(path)
}

func (s *chunkStream) send(delta map[string]any, finish any) {
	s.queue(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
}

// queue adds a chunk with the fields every chunk carries.
func (s *chunkStream) queue(chunk map[string]any) {
	chunk["id"], chunk["object"], chunk["created"], chunk["model"] = s.id, "chat.completion.chunk", s.created, s.model
	data, _ := json.Marshal(chunk)
	s.pending = append(s.pending, data)
}

func (s *chunkStream) Close() error {
	return s.events.Close()
}
