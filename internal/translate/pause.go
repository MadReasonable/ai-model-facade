package translate

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// A Claude turn that runs server tools can stop with stop_reason pause_turn
// when the API's own loop of tool calls reaches its limit, for the client to
// send the turn back as it stands so Claude carries on (Anthropic's server
// tools docs). A Messages client knows to; a Chat or Responses client has no
// such stop, so on a translated route the gateway sends the turn back for it
// and returns the parts as one reply.

// maxResends bounds the resends of one turn, each another run of the API's
// tool loop, so a turn that keeps pausing still ends. The last pause then
// reaches the client as the end of its reply.
const maxResends = 5

// Resend sends the body back to send after each pause in a reply from a
// backend sent protocol to, until the reply ends some other way or
// maxResends is reached, and returns the parts as one reply: the content of
// every part in order, the usage summed, and the rest from the last part.
// A reply in any other protocol, or one that didn't pause, is returned as
// it is.
func Resend(to backend.Protocol, body []byte, result backend.ChatResult, send func(body []byte) (backend.ChatResult, error)) (backend.ChatResult, error) {
	if to != backend.AnthropicMessages {
		return result, nil
	}
	if result.Stream != nil {
		return backend.ChatResult{Stream: &resentStream{events: result.Stream, body: body, send: send, usage: map[string]any{}}}, nil
	}
	return resendWhole(body, result, send)
}

func resendWhole(body []byte, result backend.ChatResult, send func(body []byte) (backend.ChatResult, error)) (backend.ChatResult, error) {
	var content []string
	usage := map[string]any{}
	for resends := 0; ; resends++ {
		response := result.Response
		if response.Status != http.StatusOK {
			return result, nil
		}
		message := gjson.ParseBytes(response.Body)
		for _, block := range message.Get("content").Array() {
			content = append(content, block.Raw)
		}
		addUsage(usage, message.Get("usage"))
		if message.Get("stop_reason").String() != "pause_turn" || resends == maxResends {
			if resends == 0 {
				return result, nil
			}
			joined, _ := sjson.SetRawBytes(response.Body, "content", []byte("["+strings.Join(content, ",")+"]"))
			joined, _ = sjson.SetBytes(joined, "usage", usage)
			return backend.ChatResult{Response: &backend.ChatResponse{Status: response.Status, ContentType: response.ContentType, Body: joined}}, nil
		}
		body = withAssistantContent(body, message.Get("content").Array())
		var err error
		if result, err = send(body); err != nil {
			return backend.ChatResult{}, err
		}
		if result.Stream != nil {
			result.Stream.Close()
			return backend.ChatResult{}, fmt.Errorf("the resent turn came back streamed")
		}
	}
}

// withAssistantContent is a Messages body with blocks added to the end of
// its last turn when that is the assistant's, else as a new assistant turn,
// which is how a paused turn is sent back.
func withAssistantContent(body []byte, blocks []gjson.Result) []byte {
	messages := gjson.GetBytes(body, "messages").Array()
	if len(messages) == 0 || messages[len(messages)-1].Get("role").String() != "assistant" {
		var raws []string
		for _, block := range blocks {
			raws = append(raws, block.Raw)
		}
		out, _ := sjson.SetRawBytes(body, "messages.-1", []byte(`{"role":"assistant","content":[`+strings.Join(raws, ",")+`]}`))
		return out
	}
	path := fmt.Sprintf("messages.%d.content", len(messages)-1)
	if content := messages[len(messages)-1].Get("content"); content.Type == gjson.String {
		body, _ = sjson.SetRawBytes(body, path, []byte("["+set(`{"type":"text"}`, "text", content.String())+"]"))
	}
	for _, block := range blocks {
		body, _ = sjson.SetRawBytes(body, path+".-1", []byte(block.Raw))
	}
	return body
}

// addUsage adds a Messages usage object's counts to total, nested objects
// such as server_tool_use included; a value that isn't a count, such as
// service_tier, takes the latest part's.
func addUsage(total map[string]any, usage gjson.Result) {
	usage.ForEach(func(key, value gjson.Result) bool {
		switch value.Type {
		case gjson.Number:
			count, _ := total[key.String()].(int64)
			total[key.String()] = count + value.Int()
		case gjson.JSON:
			if value.IsObject() {
				nested, _ := total[key.String()].(map[string]any)
				if nested == nil {
					nested = map[string]any{}
					total[key.String()] = nested
				}
				addUsage(nested, value)
				break
			}
			total[key.String()] = value.Value()
		default:
			total[key.String()] = value.Value()
		}
		return true
	})
}

// resentStream is a Messages stream that resends the turn each time it
// pauses and carries on with the stream that comes back, as one message: a
// part's message_start is left out after the first, its blocks are numbered
// on from the parts before, and a pause's message_delta and message_stop are
// left out, so the last part's message_delta, with the usage of every part
// summed, ends it.
type resentStream struct {
	events  backend.ChatStream
	body    []byte
	send    func(body []byte) (backend.ChatResult, error)
	resends int
	offset  int64 // the blocks sent in parts before this one

	// The part being read: its blocks as they are built, its finished blocks,
	// its usage from message_start and message_delta, and whether it paused.
	building  map[int64]*streamedBlock
	finished  []gjson.Result
	blocks    int64
	partUsage string
	paused    bool

	usage map[string]any // the usage of the parts before this one, summed
}

// streamedBlock is a content block put together from its events, for
// sending a paused turn back.
type streamedBlock struct {
	start                gjson.Result
	text, thinking, sign strings.Builder
	input                strings.Builder
	citations            []string
}

func (s *resentStream) Next() ([]byte, error) {
	for {
		data, err := s.events.Next()
		if err == io.EOF && s.paused {
			if err := s.resend(); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if out, ok := s.read(data); ok {
			return out, nil
		}
	}
}

// read takes one event of the part being read, and returns it as the
// client gets it, ok false when it is left out.
func (s *resentStream) read(data []byte) (out []byte, ok bool) {
	event := gjson.ParseBytes(data)
	switch event.Get("type").String() {
	case "message_start":
		s.building = map[int64]*streamedBlock{}
		s.finished = nil
		s.blocks = 0
		s.partUsage = event.Get("message.usage").Raw
		return data, s.resends == 0
	case "content_block_start":
		index := event.Get("index").Int()
		s.building[index] = &streamedBlock{start: event.Get("content_block")}
		s.blocks = max(s.blocks, index+1)
		return s.numbered(data, index), true
	case "content_block_delta":
		index := event.Get("index").Int()
		if b := s.building[index]; b != nil {
			b.add(event.Get("delta"))
		}
		return s.numbered(data, index), true
	case "content_block_stop":
		index := event.Get("index").Int()
		if b := s.building[index]; b != nil {
			s.finished = append(s.finished, gjson.Parse(b.block()))
			delete(s.building, index)
		}
		return s.numbered(data, index), true
	case "message_delta":
		event.Get("usage").ForEach(func(key, value gjson.Result) bool {
			s.partUsage = setRaw(s.partUsage, pathKey(key.String()), value.Raw) // message_delta's counts are cumulative
			return true
		})
		if event.Get("delta.stop_reason").String() == "pause_turn" && s.resends < maxResends {
			s.paused = true
			return nil, false
		}
		if s.resends == 0 {
			return data, true
		}
		addUsage(s.usage, gjson.Parse(s.partUsage))
		out, _ := sjson.SetBytes(data, "usage", s.usage)
		return out, true
	case "message_stop":
		return data, !s.paused
	}
	return data, true
}

// numbered is a block's event with its index counted on from the blocks of
// the parts before.
func (s *resentStream) numbered(data []byte, index int64) []byte {
	if s.offset == 0 {
		return data
	}
	out, _ := sjson.SetBytes(data, "index", s.offset+index)
	return out
}

// resend sends the turn back with the paused part's blocks, and reads on
// from the stream that comes back.
func (s *resentStream) resend() error {
	s.events.Close()
	addUsage(s.usage, gjson.Parse(s.partUsage))
	s.body = withAssistantContent(s.body, s.finished)
	s.offset += s.blocks
	s.resends++
	s.paused = false
	result, err := s.send(s.body)
	if err != nil {
		return err
	}
	if result.Stream == nil {
		return fmt.Errorf("the resent turn came back with status %d: %s", result.Response.Status, result.Response.Body)
	}
	s.events = result.Stream
	return nil
}

func (s *resentStream) Close() error {
	return s.events.Close()
}

func (b *streamedBlock) add(delta gjson.Result) {
	switch delta.Get("type").String() {
	case "text_delta":
		b.text.WriteString(delta.Get("text").String())
	case "thinking_delta":
		b.thinking.WriteString(delta.Get("thinking").String())
	case "signature_delta":
		b.sign.WriteString(delta.Get("signature").String())
	case "input_json_delta":
		b.input.WriteString(delta.Get("partial_json").String())
	case "citations_delta":
		b.citations = append(b.citations, delta.Get("citation").Raw)
	}
}

// block is the content block as a whole message would hold it.
func (b *streamedBlock) block() string {
	block := b.start.Raw
	switch b.start.Get("type").String() {
	case "text":
		block = set(block, "text", b.start.Get("text").String()+b.text.String())
		if len(b.citations) > 0 {
			block = setRaw(block, "citations", "["+strings.Join(b.citations, ",")+"]")
		}
	case "thinking":
		block = set(block, "thinking", b.start.Get("thinking").String()+b.thinking.String())
		block = set(block, "signature", b.start.Get("signature").String()+b.sign.String())
	}
	if b.input.Len() > 0 && gjson.Valid(b.input.String()) {
		block = setRaw(block, "input", b.input.String())
	}
	return block
}
