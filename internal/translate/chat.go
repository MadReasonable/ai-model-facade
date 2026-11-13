package translate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// This file is the Chat Completions module: what the translators share about
// writing a Chat Completions client's replies, and about reading its
// requests.

// chatWriter assembles a chat completion, or the chunks that stream it, from a
// reply's parts in the order they arrive, so every translator answering a
// Chat Completions client writes the same shapes. Streaming, each part is
// queued as a chunk as it comes; otherwise the parts are collected and body
// returns the completion.
//
// Chat Completions addresses each tool call by its index in every chunk, so
// calls may interleave, and each streams its arguments as they come. For a
// client that called with the deprecated functions, the reply is in that
// shape: one function_call, and finish_reason function_call; the target is
// asked for one call at a time, so a second has nowhere to go and is left
// out.
type chatWriter struct {
	streaming    bool
	includeUsage bool
	legacy       bool
	id, model    string
	created      int64
	chunks       [][]byte

	content, refusal, reasoning strings.Builder
	logprobs                    []json.RawMessage
	calls                       []*chatCall
	finished                    string
	usage                       map[string]any
}

type chatCall struct {
	kind      string // function or custom
	id, name  string
	arguments strings.Builder // a custom call's input
}

func newChatWriter(streaming, includeUsage bool) *chatWriter {
	return &chatWriter{streaming: streaming, includeUsage: includeUsage}
}

// start names the reply; streaming, it sends the first chunk, with the
// assistant role.
func (b *chatWriter) start(id, model string, created int64) {
	b.id, b.model, b.created = id, model, created
	b.send(map[string]any{"role": "assistant", "content": ""}, nil, nil)
}

// text adds content, with the log probabilities of its tokens if the
// backend gave them.
func (b *chatWriter) text(delta string, logprobs []json.RawMessage) {
	if delta == "" && len(logprobs) == 0 {
		return
	}
	b.content.WriteString(delta)
	b.logprobs = append(b.logprobs, logprobs...)
	var choiceLogprobs any
	if len(logprobs) > 0 {
		choiceLogprobs = map[string]any{"content": logprobs}
	}
	b.send(map[string]any{"content": delta}, nil, choiceLogprobs)
}

func (b *chatWriter) refused(delta string) {
	if delta == "" {
		return
	}
	b.refusal.WriteString(delta)
	b.send(map[string]any{"refusal": delta}, nil, nil)
}

// reason adds reasoning, as reasoning_content, the field llama-server uses.
func (b *chatWriter) reason(delta string) {
	if delta == "" {
		return
	}
	b.reasoning.WriteString(delta)
	b.send(map[string]any{"reasoning_content": delta}, nil, nil)
}

// call opens a tool call, function or custom, and returns its index, which
// its argument deltas name.
func (b *chatWriter) call(kind, id, name string) int {
	b.calls = append(b.calls, &chatCall{kind: kind, id: id, name: name})
	index := len(b.calls) - 1
	if b.legacy {
		if index == 0 {
			b.send(map[string]any{"function_call": map[string]any{"name": name, "arguments": ""}}, nil, nil)
		}
		return index
	}
	opened := map[string]any{"index": index, "id": id, "type": kind}
	opened[kind] = b.callFields(kind, name, "")
	b.send(map[string]any{"tool_calls": []any{opened}}, nil, nil)
	return index
}

// callFields is a call's name and arguments, or a custom call's input, in
// the shape of its kind.
func (b *chatWriter) callFields(kind, name, value string) map[string]any {
	if kind == "custom" {
		return map[string]any{"name": name, "input": value}
	}
	return map[string]any{"name": name, "arguments": value}
}

// arguments adds to call index's arguments, or a custom call's input.
func (b *chatWriter) arguments(index int, delta string) {
	if index < 0 || index >= len(b.calls) || delta == "" {
		return
	}
	call := b.calls[index]
	call.arguments.WriteString(delta)
	if b.legacy {
		if index == 0 {
			b.send(map[string]any{"function_call": map[string]any{"arguments": delta}}, nil, nil)
		}
		return
	}
	fields := map[string]any{}
	if call.kind == "custom" {
		fields["input"] = delta
	} else {
		fields["arguments"] = delta
	}
	b.send(map[string]any{"tool_calls": []any{map[string]any{"index": index, call.kind: fields}}}, nil, nil)
}

// finish ends the reply with a finish reason and usage; streaming, it sends
// the finish chunk, then a usage chunk with no choices if the client asked
// for usage, as Chat Completions sends it.
func (b *chatWriter) finish(reason string, usage map[string]any) {
	if b.legacy && reason == "tool_calls" {
		reason = "function_call"
	}
	b.finished, b.usage = reason, usage
	b.send(map[string]any{}, reason, nil)
	if b.streaming && b.includeUsage {
		b.queue(map[string]any{"choices": []any{}, "usage": usage})
	}
}

// send queues a chunk with one choice, streaming.
func (b *chatWriter) send(delta map[string]any, finish, logprobs any) {
	if !b.streaming {
		return
	}
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
	if logprobs != nil {
		choice["logprobs"] = logprobs
	}
	b.queue(map[string]any{"choices": []any{choice}})
}

// queue adds a chunk with the fields every chunk carries.
func (b *chatWriter) queue(chunk map[string]any) {
	chunk["id"], chunk["object"], chunk["created"], chunk["model"] = b.id, "chat.completion.chunk", b.created, b.model
	data, _ := json.Marshal(chunk)
	b.chunks = append(b.chunks, data)
}

// next takes the next queued chunk, if there is one.
func (b *chatWriter) next() ([]byte, bool) {
	if len(b.chunks) == 0 {
		return nil, false
	}
	chunk := b.chunks[0]
	b.chunks = b.chunks[1:]
	return chunk, true
}

// body is the completion the parts make: content null where a reply has only
// tool calls or a refusal, as OpenAI sends it.
func (b *chatWriter) body() []byte {
	message := map[string]any{"role": "assistant", "content": b.content.String()}
	if b.content.Len() == 0 && (len(b.calls) > 0 || b.refusal.Len() > 0) {
		message["content"] = nil
	}
	if b.refusal.Len() > 0 {
		message["refusal"] = b.refusal.String()
	}
	if b.reasoning.Len() > 0 {
		message["reasoning_content"] = b.reasoning.String()
	}
	if len(b.calls) > 0 && b.legacy {
		message["function_call"] = map[string]any{"name": b.calls[0].name, "arguments": b.calls[0].arguments.String()}
	} else if len(b.calls) > 0 {
		var calls []any
		for _, call := range b.calls {
			calls = append(calls, map[string]any{"id": call.id, "type": call.kind, call.kind: b.callFields(call.kind, call.name, call.arguments.String())})
		}
		message["tool_calls"] = calls
	}
	choice := map[string]any{"index": 0, "message": message, "finish_reason": b.finished}
	if b.logprobs != nil {
		choice["logprobs"] = map[string]any{"content": b.logprobs, "refusal": nil}
	}
	data, _ := json.Marshal(map[string]any{
		"id":      b.id,
		"object":  "chat.completion",
		"created": b.created,
		"model":   b.model,
		"choices": []any{choice},
		"usage":   b.usage,
	})
	return data
}

// chatMessage is one message of a Chat Completions request, read for
// translation. A legacy function call and its function message are read as
// a tool call and its result, so the targets see one kind of call.
type chatMessage struct {
	where     string // the message's place, for errors: "messages[2]"
	role      string // system, developer, user, assistant or tool
	content   gjson.Result
	refusal   string
	reasoning bool // the message carries reasoning, which has no signature
	calls     []chatToolCall
	callID    string // a tool message's call
	named     bool   // the message has a participant name
}

// chatToolCall is an assistant message's call of a function or custom tool.
type chatToolCall struct {
	kind  string // function or custom
	id    string
	name  string
	value string // a function's arguments, a custom tool's input
}

// readChatMessages reads a request's messages. A legacy call (an assistant
// message's function_call) has no ID, and its result (a function message)
// names only the function, so each gets an ID made from its place, the
// result paired with the latest call of that function not yet answered.
func readChatMessages(req gjson.Result) ([]chatMessage, error) {
	var messages []chatMessage
	var open []chatToolCall // legacy calls awaiting their function message
	for i, msg := range req.Get("messages").Array() {
		m := chatMessage{where: "messages[" + strconv.Itoa(i) + "]", role: msg.Get("role").String(), content: msg.Get("content"), refusal: msg.Get("refusal").String(), reasoning: reasoningOf(msg) != "", named: msg.Get("name").String() != ""}
		switch m.role {
		case "system", "developer", "user":
		case "assistant":
			for j, call := range msg.Get("tool_calls").Array() {
				kind := call.Get("type").String()
				switch kind {
				case "", "function":
					m.calls = append(m.calls, chatToolCall{kind: "function", id: call.Get("id").String(), name: call.Get("function.name").String(), value: call.Get("function.arguments").String()})
				case "custom":
					m.calls = append(m.calls, chatToolCall{kind: "custom", id: call.Get("id").String(), name: call.Get("custom.name").String(), value: call.Get("custom.input").String()})
				default:
					return nil, refuse("%s.tool_calls[%d]: '%s' is not a tool call type", m.where, j, kind)
				}
			}
			if legacy := msg.Get("function_call"); legacy.IsObject() {
				call := chatToolCall{kind: "function", id: "call_" + strconv.Itoa(i), name: legacy.Get("name").String(), value: legacy.Get("arguments").String()}
				m.calls = append(m.calls, call)
				open = append(open, call)
			}
		case "tool":
			m.callID = msg.Get("tool_call_id").String()
		case "function":
			m.role = "tool"
			name := msg.Get("name").String()
			for j := len(open) - 1; j >= 0; j-- {
				if open[j].name == name {
					m.callID = open[j].id
					open = append(open[:j], open[j+1:]...)
					break
				}
			}
			if m.callID == "" {
				return nil, refuse("%s: function message for '%s' answers no function_call before it", m.where, name)
			}
			m.named = false
		default:
			return nil, refuse("%s: '%s' is not a Chat Completions role", m.where, m.role)
		}
		messages = append(messages, m)
	}
	return messages, nil
}

// chatPart is one part of a message's content.
type chatPart struct {
	kind     string // text, image, file or audio
	text     string
	url      string // an image's URL or data URL
	detail   string
	fileData string // a file's data URL
	fileID   string
	filename string
}

// readChatParts reads a message's content: a string is one text part, and a
// refusal part is text.
func readChatParts(content gjson.Result, where string) ([]chatPart, error) {
	if content.Type == gjson.String {
		return []chatPart{{kind: "text", text: content.String()}}, nil
	}
	var parts []chatPart
	for _, part := range content.Array() {
		switch kind := part.Get("type").String(); kind {
		case "text", "refusal":
			parts = append(parts, chatPart{kind: "text", text: part.Get(kind).String()})
		case "image_url":
			parts = append(parts, chatPart{kind: "image", url: part.Get("image_url.url").String(), detail: part.Get("image_url.detail").String()})
		case "file":
			parts = append(parts, chatPart{kind: "file", fileData: part.Get("file.file_data").String(), fileID: part.Get("file.file_id").String(), filename: part.Get("file.filename").String()})
		case "input_audio":
			parts = append(parts, chatPart{kind: "audio"})
		default:
			return nil, refuse("%s: '%s' is not a Chat Completions content part", where, kind)
		}
	}
	return parts, nil
}

// chatTool is a tool a request offers: a function, from tools or the legacy
// functions, or a custom tool.
type chatTool struct {
	kind        string // function or custom
	name        string
	description gjson.Result
	parameters  gjson.Result
	strict      gjson.Result
	format      gjson.Result // a custom tool's input format
}

// readChatTools reads the tools a request offers, the legacy functions
// included.
func readChatTools(req gjson.Result) ([]chatTool, error) {
	var tools []chatTool
	for i, tool := range req.Get("tools").Array() {
		switch kind := tool.Get("type").String(); kind {
		case "function":
			fn := tool.Get("function")
			tools = append(tools, chatTool{kind: kind, name: fn.Get("name").String(), description: fn.Get("description"), parameters: fn.Get("parameters"), strict: fn.Get("strict")})
		case "custom":
			custom := tool.Get("custom")
			tools = append(tools, chatTool{kind: kind, name: custom.Get("name").String(), description: custom.Get("description"), format: custom.Get("format")})
		default:
			return nil, refuse("tools[%d]: '%s' is not a Chat Completions tool type", i, kind)
		}
	}
	for _, fn := range req.Get("functions").Array() {
		tools = append(tools, chatTool{kind: "function", name: fn.Get("name").String(), description: fn.Get("description"), parameters: fn.Get("parameters")})
	}
	return tools, nil
}

// chatChoice is a request's tool choice: a mode, or one tool named, or the
// tools allowed.
type chatChoice struct {
	mode    string // auto, none, required, tool or allowed
	kind    string // the named tool's kind, function or custom
	name    string
	allowed []chatChoiceTool // with mode allowed: the tools, chosen among as allowedMode says
	// allowedMode is auto or required.
	allowedMode string
	set         bool // the request chose
}

type chatChoiceTool struct{ kind, name string }

// readChatToolChoice reads tool_choice, or the legacy function_call.
func readChatToolChoice(req gjson.Result) (chatChoice, error) {
	choice := req.Get("tool_choice")
	if !choice.Exists() || choice.Type == gjson.Null {
		choice = req.Get("function_call")
		if choice.IsObject() {
			return chatChoice{mode: "tool", kind: "function", name: choice.Get("name").String(), set: true}, nil
		}
	}
	switch {
	case !choice.Exists() || choice.Type == gjson.Null:
		return chatChoice{}, nil
	case choice.Type == gjson.String:
		return chatChoice{mode: choice.String(), set: true}, nil
	}
	switch kind := choice.Get("type").String(); kind {
	case "function", "custom":
		return chatChoice{mode: "tool", kind: kind, name: choice.Get(kind + ".name").String(), set: true}, nil
	case "allowed_tools":
		c := chatChoice{mode: "allowed", allowedMode: choice.Get("allowed_tools.mode").String(), set: true}
		for _, tool := range choice.Get("allowed_tools.tools").Array() {
			kind := tool.Get("type").String()
			c.allowed = append(c.allowed, chatChoiceTool{kind: kind, name: tool.Get(pathKey(kind) + ".name").String()})
		}
		return c, nil
	}
	return chatChoice{}, refuse("tool_choice %s is not a Chat Completions tool choice", choice.Raw)
}

// legacyFunctions reports whether a request uses the deprecated functions
// and function_call, so its reply is written in that shape too.
func legacyFunctions(req gjson.Result) bool {
	return req.Get("functions").IsArray() && !req.Get("tools").IsArray()
}

// chatFormat is a request's response_format.
type chatFormat struct {
	kind   string // "", text, json_object or json_schema
	schema gjson.Result
	name   gjson.Result
	strict gjson.Result
	// description is the schema's description.
	description gjson.Result
}

func readChatFormat(req gjson.Result) chatFormat {
	format := req.Get("response_format")
	return chatFormat{kind: format.Get("type").String(), schema: format.Get("json_schema.schema"), name: format.Get("json_schema.name"), strict: format.Get("json_schema.strict"), description: format.Get("json_schema.description")}
}

// refuse is a request a Chat Completions or Responses client sent that its
// target can't serve as the client means it. The client gets it as a 400 in
// OpenAI's shape, since sending it on would either fail or answer something
// else.
func refuse(format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	return &Refusal{Response: &backend.ChatResponse{Status: http.StatusBadRequest, ContentType: "application/json", Body: openAIErrorBody("invalid_request_error", message)}, message: message}
}

func openAIErrorBody(errType, message string) []byte {
	data, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": errType}})
	return data
}

// reasoningOf reads a model's reasoning, which llama.cpp reports as
// reasoning_content and LocalAI as reasoning (seen on v4.11.0).
func reasoningOf(message gjson.Result) string {
	if reasoning := message.Get("reasoning_content").String(); reasoning != "" {
		return reasoning
	}
	return message.Get("reasoning").String()
}

// chatText joins a message's text parts.
func chatText(content gjson.Result, where string) (string, error) {
	if !content.Exists() || content.Type == gjson.Null {
		return "", nil
	}
	parts, err := readChatParts(content, where)
	if err != nil {
		return "", err
	}
	var texts []string
	for _, part := range parts {
		if part.kind != "text" {
			return "", refuse("%s: a '%s' part can't stand here; only text can", where, part.kind)
		}
		texts = append(texts, part.text)
	}
	return strings.Join(texts, "\n\n"), nil
}
