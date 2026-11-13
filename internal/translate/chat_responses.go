package translate

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// This file is the Chat Completions and Responses pair: chatToResponses for a
// Chat Completions client on a candidate sent Responses, responsesToChat for a
// Responses client on a Chat Completions backend, each translating the request
// one way and its reply, whole or streamed, back.

// chatToResponses is the direct translator from Chat Completions to
// Responses, for a candidate sent as Responses, such as a model OpenAI serves
// only there.
type chatToResponses struct{}

func (chatToResponses) Request(body []byte) ([]byte, Reply, error) {
	out, includeUsage, gone, err := responsesFromChat(body)
	if err != nil {
		return nil, nil, err
	}
	return out, &responsesChatReply{includeUsage: includeUsage, legacy: legacyFunctions(gjson.ParseBytes(body)), dropped: gone}, nil
}

// droppedChatForResponses are Chat Completions fields with no Responses
// counterpart that only tune sampling or OpenAI's stored completions.
var droppedChatForResponses = map[string]bool{
	"frequency_penalty": true,
	"logit_bias":        true,
	"prediction":        true,
	"presence_penalty":  true,
	"seed":              true,
}

// responsesFromChat translates a Chat Completions body into a Responses body,
// and says whether the client asked for usage at the end of a stream. Top-level
// keys keep the client's order, each translated in place, and a key with no
// translation passes on unchanged, so a facade's params can set Responses
// fields directly. The request isn't kept at OpenAI unless the client set
// store, since a Chat Completions client sends its conversation every turn and
// Responses keeps one by default.
func responsesFromChat(body []byte) ([]byte, bool, droppedFields, error) {
	req := gjson.ParseBytes(body)
	if err := checkChatForResponses(req); err != nil {
		return nil, false, nil, err
	}
	var gone droppedFields
	messages, err := readChatMessages(req)
	if err != nil {
		return nil, false, nil, err
	}
	input, err := responsesInputFromChat(messages, &gone)
	if err != nil {
		return nil, false, nil, err
	}
	choice, err := readChatToolChoice(req)
	if err != nil {
		return nil, false, nil, err
	}
	var out members
	placed := map[string]bool{}
	place := func(key, raw string) {
		if raw != "" && !placed[key] {
			placed[key] = true
			out.add(strconv.Quote(key), raw)
		}
	}
	var failed error
	req.ForEach(func(key, value gjson.Result) bool {
		switch k := key.String(); {
		case k == "messages":
			place("input", input)
		case k == "max_tokens" || k == "max_completion_tokens":
			place("max_output_tokens", maxTokensOf(req))
		case k == "reasoning_effort":
			place("reasoning", setRaw("{}", "effort", value.Raw))
		case k == "response_format" || k == "verbosity":
			var text string
			text, failed = responsesTextFromChat(req)
			place("text", text)
		case k == "tools" || k == "functions":
			var tools string
			tools, failed = responsesToolsFromChat(req)
			place("tools", tools)
		case k == "tool_choice" || k == "function_call":
			place("tool_choice", responsesToolChoiceFromChat(choice))
		case k == "store":
			place("store", value.Raw)
		case k == "logprobs" || k == "top_logprobs":
			// Responses returns them when its include asks, each part's in the
			// shape Chat Completions gives (verified 2026-11-09)
			if req.Get("logprobs").Bool() {
				place("include", `["message.output_text.logprobs"]`)
				if top := req.Get("top_logprobs"); top.Exists() && top.Type != gjson.Null {
					place("top_logprobs", top.Raw)
				}
			}
		case k == "stream_options" || k == "n" || k == "modalities" || k == "audio" || k == "stop":
			// checked by checkChatForResponses, so only a harmless value gets
			// here, and stream_options is read for include_usage
		case droppedChatForResponses[k]:
			gone.add(k)
		default:
			if !placed[k] {
				placed[k] = true
				out.add(key.Raw, value.Raw)
			}
		}
		return failed == nil
	})
	if failed != nil {
		return nil, false, nil, failed
	}
	place("input", input)
	if legacyFunctions(req) {
		place("parallel_tool_calls", "false") // the reply has room for one call
	}
	place("store", "false")
	return out.bytes(), req.Get("stream_options.include_usage").Bool(), gone, nil
}

// checkChatForResponses refuses the fields that ask for something Responses
// can't give here: several choices, audio, stop sequences, or web search.
func checkChatForResponses(req gjson.Result) error {
	if n := req.Get("n"); n.Exists() && n.Type != gjson.Null && n.Int() != 1 {
		return refuse("the Responses API returns one choice per request; 'n' must be 1, not %s", n.Raw)
	}
	for _, m := range req.Get("modalities").Array() {
		if m.String() != "text" {
			return refuse("this model is served through the Responses API, which answers here only in text, not '%s'", m.String())
		}
	}
	if a := req.Get("audio"); a.Exists() && a.Type != gjson.Null {
		return refuse("this model is served through the Responses API, which has no audio output; 'audio' must be absent")
	}
	if stop := req.Get("stop"); isSet(stop) {
		return refuse("the Responses API has no stop sequences, and this model is served through it; 'stop' must be absent")
	}
	if req.Get("web_search_options").Exists() {
		return refuse("this model is served through the Responses API, and the gateway doesn't translate 'web_search_options' to it")
	}
	return nil
}

// responsesInputFromChat turns the conversation into Responses input items.
// Each message keeps its place and role, system and developer included, since
// Responses takes them anywhere in its input. An assistant message's tool
// calls become function or custom calls after its text, and a tool message
// that call's output. Its reasoning_content is left out: Responses takes
// reasoning back only as the item it was sent as. A participant's name has no
// Responses counterpart.
func responsesInputFromChat(messages []chatMessage, gone *droppedFields) (string, error) {
	var items []string
	custom := map[string]bool{} // IDs of custom tool calls, whose outputs are custom too
	for _, m := range messages {
		if m.named {
			gone.add("name")
		}
		switch m.role {
		case "system", "developer":
			text, err := chatText(m.content, m.where)
			if err != nil {
				return "", err
			}
			items = append(items, set(set(`{"type":"message"}`, "role", m.role), "content", text))
		case "user":
			item, err := responsesUserFromChat(m.content, m.where)
			if err != nil {
				return "", err
			}
			items = append(items, item)
		case "assistant":
			text, err := chatText(m.content, m.where)
			if err != nil {
				return "", err
			}
			if text == "" {
				text = m.refusal
			}
			if text != "" {
				items = append(items, set(`{"type":"message","role":"assistant"}`, "content", text))
			}
			if m.reasoning {
				gone.add("reasoning_content")
			}
			for _, call := range m.calls {
				if call.kind == "custom" {
					custom[call.id] = true
					item := set(`{"type":"custom_tool_call"}`, "call_id", call.id)
					item = set(item, "name", call.name)
					items = append(items, set(item, "input", call.value))
					continue
				}
				item := set(`{"type":"function_call"}`, "call_id", call.id)
				item = set(item, "name", call.name)
				items = append(items, set(item, "arguments", call.value))
			}
		case "tool":
			output, err := chatText(m.content, m.where)
			if err != nil {
				return "", err
			}
			kind := `{"type":"function_call_output"}`
			if custom[m.callID] {
				kind = `{"type":"custom_tool_call_output"}`
			}
			items = append(items, set(set(kind, "call_id", m.callID), "output", output))
		}
	}
	return "[" + strings.Join(items, ",") + "]", nil
}

// responsesUserFromChat turns a user message into a Responses message: text,
// images by URL or data URL, and files by data or by ID, since the target is
// OpenAI's, where the ID's file is.
func responsesUserFromChat(content gjson.Result, where string) (string, error) {
	if content.Type == gjson.String {
		return set(`{"type":"message","role":"user"}`, "content", content.String()), nil
	}
	parts, err := readChatParts(content, where)
	if err != nil {
		return "", err
	}
	var out []string
	for _, part := range parts {
		switch part.kind {
		case "text":
			out = append(out, set(`{"type":"input_text"}`, "text", part.text))
		case "image":
			image := set(`{"type":"input_image"}`, "image_url", part.url)
			if part.detail != "" {
				image = set(image, "detail", part.detail)
			}
			out = append(out, image)
		case "file":
			file := `{"type":"input_file"}`
			for _, field := range [][2]string{{"file_data", part.fileData}, {"file_id", part.fileID}, {"filename", part.filename}} {
				if field[1] != "" {
					file = set(file, field[0], field[1])
				}
			}
			out = append(out, file)
		default:
			return "", refuse("%s: the Responses API takes no '%s' part here", where, part.kind)
		}
	}
	return setRaw(`{"type":"message","role":"user"}`, "content", "["+strings.Join(out, ",")+"]"), nil
}

// responsesTextFromChat builds text from response_format and verbosity.
func responsesTextFromChat(req gjson.Result) (string, error) {
	text := "{}"
	switch format := req.Get("response_format"); format.Get("type").String() {
	case "", "text":
	case "json_object":
		text = setRaw(text, "format", `{"type":"json_object"}`)
	case "json_schema":
		schema := `{"type":"json_schema"}`
		for _, key := range []string{"name", "description", "schema", "strict"} {
			if v := format.Get("json_schema." + key); v.Exists() && v.Type != gjson.Null {
				schema = setRaw(schema, key, v.Raw)
			}
		}
		text = setRaw(text, "format", schema)
	default:
		return "", refuse("response_format '%s' has no Responses API equivalent", format.Get("type").String())
	}
	if verbosity := req.Get("verbosity"); verbosity.Exists() && verbosity.Type != gjson.Null {
		text = setRaw(text, "verbosity", verbosity.Raw)
	}
	if text == "{}" {
		return "", nil
	}
	return text, nil
}

// responsesToolsFromChat turns function and custom tools, the legacy
// functions included, into Responses' flatter shape.
func responsesToolsFromChat(req gjson.Result) (string, error) {
	tools, err := readChatTools(req)
	if err != nil {
		return "", err
	}
	var out []string
	for _, tool := range tools {
		if tool.kind == "custom" {
			custom := set(`{"type":"custom"}`, "name", tool.name)
			custom = setPresent(custom, "description", tool.description)
			custom = setPresent(custom, "format", tool.format)
			out = append(out, custom)
			continue
		}
		// Responses makes a tool strict unless told otherwise (verified
		// 2026-11-09), and strict needs every property required, where Chat
		// Completions defaults to not strict.
		function := set(`{"type":"function","strict":false}`, "name", tool.name)
		function = setPresent(function, "description", tool.description)
		function = setPresent(function, "parameters", tool.parameters)
		function = setPresent(function, "strict", tool.strict)
		out = append(out, function)
	}
	if len(out) == 0 {
		return "", nil
	}
	return "[" + strings.Join(out, ",") + "]", nil
}

// responsesToolChoiceFromChat translates a tool choice, the legacy
// function_call included.
func responsesToolChoiceFromChat(choice chatChoice) string {
	switch choice.mode {
	case "":
		return ""
	case "tool":
		return set(set("{}", "type", choice.kind), "name", choice.name)
	case "allowed":
		allowed := set(`{"type":"allowed_tools"}`, "mode", choice.allowedMode)
		var tools []string
		for _, tool := range choice.allowed {
			tools = append(tools, set(set("{}", "type", tool.kind), "name", tool.name))
		}
		return setRaw(allowed, "tools", "["+strings.Join(tools, ",")+"]")
	}
	return strconv.Quote(choice.mode)
}

// responsesChatReply converts what a Responses backend answered back to a chat
// completion: a response or a stream, or an error with the backend's status,
// already in OpenAI's shape.
type responsesChatReply struct {
	includeUsage bool
	legacy       bool // the client called with the deprecated functions
	dropped      droppedFields
}

func (r *responsesChatReply) Dropped() []string { return r.dropped }

func (r *responsesChatReply) Result(result backend.ChatResult) backend.ChatResult {
	if result.Stream != nil {
		return backend.ChatResult{Stream: newChatFromResponsesStream(result.Stream, r.includeUsage, r.legacy)}
	}
	resp := *result.Response
	resp.ContentType = "application/json"
	switch response := gjson.ParseBytes(resp.Body); {
	case resp.Status/100 != 2:
		if !gjson.GetBytes(resp.Body, "error.message").Exists() {
			resp.Body = openAIErrorBody("api_error", errorMessage(resp.Body))
		}
	case response.Get("status").String() == "failed":
		resp.Status = http.StatusBadGateway
		resp.Body = failedError(response)
	default:
		resp.Body = completionFromResponse(response, r.legacy)
		resp.RetryAfter = ""
	}
	return backend.ChatResult{Response: &resp}
}

// completionFromResponse turns a response into a chat completion: messages'
// text joined as content, with its log probabilities, refusals as refusal,
// reasoning as reasoning_content, as llama-server reports reasoning, and
// function and custom tool calls as tool calls. Other items have no place in
// a chat completion and are left out.
func completionFromResponse(response gjson.Result, legacy bool) []byte {
	b := newChatWriter(false, false)
	b.legacy = legacy
	b.start("chatcmpl-"+response.Get("id").String(), response.Get("model").String(), createdOf(response))
	for _, item := range response.Get("output").Array() {
		switch item.Get("type").String() {
		case "message":
			for _, part := range item.Get("content").Array() {
				switch part.Get("type").String() {
				case "output_text":
					b.text(part.Get("text").String(), rawItems(part.Get("logprobs")))
				case "refusal":
					b.refused(part.Get("refusal").String())
				}
			}
		case "reasoning":
			b.reason(responseReasoning(item))
		case "function_call":
			b.arguments(b.call("function", item.Get("call_id").String(), item.Get("name").String()), item.Get("arguments").String())
		case "custom_tool_call":
			b.arguments(b.call("custom", item.Get("call_id").String(), item.Get("name").String()), item.Get("input").String())
		}
	}
	b.finish(finishFromResponse(response, len(b.calls) > 0), chatUsageFromResponses(response.Get("usage")))
	return b.body()
}

// finishFromResponse maps a response's status to a Chat Completions
// finish_reason.
func finishFromResponse(response gjson.Result, calls bool) string {
	switch response.Get("incomplete_details.reason").String() {
	case "max_output_tokens":
		return "length"
	case "content_filter":
		return "content_filter"
	}
	if calls {
		return "tool_calls"
	}
	return "stop"
}

// chatUsageFromResponses converts Responses usage, which counts cache reads
// within input_tokens as Chat Completions does within prompt_tokens.
func chatUsageFromResponses(usage gjson.Result) map[string]any {
	prompt, completion := usage.Get("input_tokens").Int(), usage.Get("output_tokens").Int()
	return map[string]any{
		"prompt_tokens":             prompt,
		"completion_tokens":         completion,
		"total_tokens":              prompt + completion,
		"prompt_tokens_details":     map[string]any{"cached_tokens": usage.Get("input_tokens_details.cached_tokens").Int()},
		"completion_tokens_details": map[string]any{"reasoning_tokens": usage.Get("output_tokens_details.reasoning_tokens").Int()},
	}
}

// chatFromResponsesStream turns Responses events into Chat Completions chunks,
// as completionFromResponse does a complete response:
//
//   - response.created, a first chunk with the assistant role;
//   - a text, refusal or reasoning delta, a chunk with content, refusal or
//     reasoning_content, a text delta with its log probabilities; a reasoning
//     item's parts after the first are joined with a blank line;
//   - a function or custom tool call added, a chunk opening the tool call,
//     numbered in the order the calls arrive, and its argument or input
//     deltas, chunks with arguments or input;
//   - response.completed or response.incomplete, the finish chunk, then a
//     usage chunk with no choices if the client set
//     stream_options.include_usage.
//
// response.failed and an error event end the stream with an error in OpenAI's
// shape. The stream ends at the response's last event, as the Responses
// descriptor ends it; one that ends before is broken.
type chatFromResponsesStream struct {
	events backend.ChatStream
	reply  *chatWriter
	done   bool

	calls    map[string]int // tool call item ID -> tool call index
	reasoned bool           // a reasoning part has been sent in the open item
}

func newChatFromResponsesStream(events backend.ChatStream, includeUsage, legacy bool) *chatFromResponsesStream {
	reply := newChatWriter(true, includeUsage)
	reply.legacy = legacy
	return &chatFromResponsesStream{events: events, reply: reply, calls: map[string]int{}}
}

func (s *chatFromResponsesStream) Next() ([]byte, error) {
	for {
		if chunk, ok := s.reply.next(); ok {
			return chunk, nil
		}
		if s.done {
			return nil, io.EOF
		}
		data, err := s.events.Next()
		var streamErr *backend.StreamError
		if errors.As(err, &streamErr) {
			return nil, &backend.StreamError{Data: openAIErrorBody("api_error", streamErrorMessage(streamErr.Data))}
		}
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF // the body ended before the response did
		}
		if err != nil {
			return nil, err
		}
		if err := s.translate(gjson.ParseBytes(data)); err != nil {
			return nil, err
		}
	}
}

func (s *chatFromResponsesStream) translate(event gjson.Result) error {
	switch event.Get("type").String() {
	case "response.created":
		response := event.Get("response")
		s.reply.start("chatcmpl-"+response.Get("id").String(), response.Get("model").String(), createdOf(response))
	case "response.output_item.added":
		item := event.Get("item")
		s.reasoned = false
		switch item.Get("type").String() {
		case "function_call":
			s.calls[item.Get("id").String()] = s.reply.call("function", item.Get("call_id").String(), item.Get("name").String())
		case "custom_tool_call":
			s.calls[item.Get("id").String()] = s.reply.call("custom", item.Get("call_id").String(), item.Get("name").String())
		}
	case "response.content_part.added":
		switch event.Get("part.type").String() {
		case "reasoning_text":
			s.separateReasoning()
		}
	case "response.reasoning_summary_part.added":
		s.separateReasoning()
	case "response.output_text.delta":
		s.reply.text(event.Get("delta").String(), rawItems(event.Get("logprobs")))
	case "response.refusal.delta":
		s.reply.refused(event.Get("delta").String())
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		s.reply.reason(event.Get("delta").String())
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		if index, ok := s.calls[event.Get("item_id").String()]; ok {
			s.reply.arguments(index, event.Get("delta").String())
		}
	case "response.completed", "response.incomplete":
		response := event.Get("response")
		s.reply.finish(finishFromResponse(response, len(s.calls) > 0), chatUsageFromResponses(response.Get("usage")))
		s.done = true
	case "response.failed":
		return &backend.StreamError{Data: failedError(event.Get("response"))}
	}
	return nil
}

// separateReasoning puts a blank line between a reasoning item's parts.
func (s *chatFromResponsesStream) separateReasoning() {
	if s.reasoned {
		s.reply.reason("\n\n")
	}
	s.reasoned = true
}

func (s *chatFromResponsesStream) Close() error {
	return s.events.Close()
}

// responsesToChat is the direct translator from Responses to Chat
// Completions.
type responsesToChat struct{}

func (responsesToChat) Request(body []byte) ([]byte, Reply, error) {
	out, gone, err := chatFromResponses(body)
	if err != nil {
		return nil, nil, err
	}
	return out, &chatResponsesReply{request: gjson.ParseBytes(body), dropped: gone}, nil
}

// chatFromResponses translates a Responses body into a Chat Completions
// body. Top-level keys keep the client's order, each translated in place, and
// a key with no translation passes on unchanged, so a facade's params can set
// Chat Completions fields directly. A stream asks for usage, since Responses
// always reports it.
func chatFromResponses(body []byte) ([]byte, droppedFields, error) {
	req := gjson.ParseBytes(body)
	var gone droppedFields
	messages, err := chatMessagesFromResponses(req, &gone)
	if err != nil {
		return nil, nil, err
	}
	var out members
	placedMessages := false
	var failed error
	req.ForEach(func(key, value gjson.Result) bool {
		switch k := key.String(); {
		case k == "input":
			out.add(`"messages"`, messages)
			placedMessages = true
		case k == "instructions":
			// in messages
		case k == "max_output_tokens":
			out.add(`"max_tokens"`, value.Raw)
		case k == "reasoning":
			if effort := value.Get("effort"); effort.Exists() && effort.Type != gjson.Null {
				out.add(`"reasoning_effort"`, effort.Raw)
			}
			reportSettings(&gone, k, value, "effort")
		case k == "text":
			var format string
			if format, failed = chatFormatFromResponses(value.Get("format")); format != "" {
				out.add(`"response_format"`, format)
			}
			if verbosity := value.Get("verbosity"); verbosity.Exists() && verbosity.Type != gjson.Null {
				out.add(`"verbosity"`, verbosity.Raw)
			}
			reportSettings(&gone, k, value, "format", "verbosity")
		case k == "tools":
			var tools string
			tools, failed = chatToolsFromResponses(value)
			if tools != "" {
				out.add(`"tools"`, tools)
			}
		case k == "tool_choice":
			var choice string
			if choice, failed = chatToolChoiceFromResponses(value); choice != "" {
				out.add(`"tool_choice"`, choice)
			}
		case k == "include" || k == "top_logprobs":
			// Chat Completions returns them in the same shape (verified
			// 2026-11-09); include asks for nothing else Chat has
			if k == "top_logprobs" {
				break
			}
			if wantsLogprobs(req) {
				out.add(`"logprobs"`, "true")
				if top := req.Get("top_logprobs"); top.Exists() && top.Type != gjson.Null {
					out.add(`"top_logprobs"`, top.Raw)
				}
			}
			for _, v := range value.Array() {
				if v.String() != "message.output_text.logprobs" {
					gone.add("include")
				}
			}
		case k == "safety_identifier":
			if !req.Get("user").Exists() {
				out.add(`"user"`, value.Raw)
			}
		case responsesRefused[k] != "":
			if isSet(value) {
				failed = refuseResponsesField(k, "Chat Completions")
			}
		case responsesDropped[k]:
			if isSet(value) && !(k == "truncation" && value.String() == "disabled") {
				gone.add(k)
			}
		default:
			out.add(key.Raw, value.Raw)
		}
		return failed == nil
	})
	if failed != nil {
		return nil, nil, failed
	}
	if !placedMessages {
		out.add(`"messages"`, messages)
	}
	if req.Get("stream").Bool() {
		out.add(`"stream_options"`, `{"include_usage":true}`)
	}
	return out.bytes(), gone, nil
}

// chatMessagesFromResponses returns the conversation as a Chat Completions
// messages array. The instructions come first, joined with any system or
// developer message in the input, as messagesToChat joins Messages' system
// prompt, since not every model takes a system message mid-conversation. An
// assistant's text and the function calls after it are one message, and each
// call's output a tool message. Reasoning items are left out: a Chat
// Completions request has no field for them.
func chatMessagesFromResponses(req gjson.Result, gone *droppedFields) (string, error) {
	var system, messages []string
	if instructions := req.Get("instructions"); instructions.Type == gjson.String && instructions.String() != "" {
		system = append(system, instructions.String())
	}
	items := req.Get("input")
	if items.Type == gjson.String {
		items = gjson.Parse(`[` + set(`{"role":"user"}`, "content", items.String()) + `]`)
	}
	var assistant string // the assistant message being built, or ""
	flush := func() {
		if assistant != "" {
			messages = append(messages, assistant)
			assistant = ""
		}
	}
	for i, item := range items.Array() {
		where := "input[" + strconv.Itoa(i) + "]"
		switch kind := responsesItemKind(item); kind {
		case "message":
			switch role := item.Get("role").String(); role {
			case "system", "developer":
				text, err := responsesItemText(item.Get("content"), where)
				if err != nil {
					return "", err
				}
				system = append(system, text)
			case "user":
				flush()
				message, err := chatUserFromResponses(item.Get("content"), where)
				if err != nil {
					return "", err
				}
				messages = append(messages, message)
			case "assistant":
				text, err := responsesItemText(item.Get("content"), where)
				if err != nil {
					return "", err
				}
				if gjson.Get(assistant, "tool_calls").Exists() {
					flush()
				}
				if assistant == "" {
					assistant = set(`{"role":"assistant"}`, "content", text)
				} else {
					assistant = set(assistant, "content", gjson.Get(assistant, "content").String()+text)
				}
			default:
				return "", refuse("%s: Chat Completions has no '%s' role", where, role)
			}
		case "function_call", "custom_tool_call":
			if assistant == "" {
				assistant = `{"role":"assistant","content":null}`
			}
			call := set(`{"type":"function","function":{}}`, "id", item.Get("call_id").String())
			call = set(call, "function.name", item.Get("name").String())
			call = set(call, "function.arguments", item.Get("arguments").String())
			if kind == "custom_tool_call" {
				call = set(`{"type":"custom","custom":{}}`, "id", item.Get("call_id").String())
				call = set(call, "custom.name", item.Get("name").String())
				call = set(call, "custom.input", item.Get("input").String())
			}
			assistant = setRaw(assistant, "tool_calls.-1", call)
		case "function_call_output", "custom_tool_call_output":
			flush()
			output, err := responsesItemText(item.Get("output"), where+".output")
			if err != nil {
				return "", err
			}
			messages = append(messages, set(set(`{"role":"tool"}`, "tool_call_id", item.Get("call_id").String()), "content", output))
		case "reasoning":
			gone.add("reasoning")
		case "item_reference":
			return "", refuse("%s: an item_reference names an item kept at OpenAI; send the item itself, or chain the turn with previous_response_id", where)
		default:
			return "", refuse("%s: a '%s' item can't be served through Chat Completions", where, kind)
		}
	}
	flush()
	if len(system) > 0 {
		messages = append([]string{set(`{"role":"system"}`, "content", strings.Join(system, "\n\n"))}, messages...)
	}
	return "[" + strings.Join(messages, ",") + "]", nil
}

// chatUserFromResponses turns a user message into a Chat Completions one:
// text alone as a string, as messagesToChat sends it, else text, image and
// file parts.
func chatUserFromResponses(content gjson.Result, where string) (string, error) {
	if content.Type == gjson.String {
		return set(`{"role":"user"}`, "content", content.String()), nil
	}
	var parts, texts []string
	for _, part := range content.Array() {
		switch kind := part.Get("type").String(); kind {
		case "input_text":
			texts = append(texts, part.Get("text").String())
			parts = append(parts, set(`{"type":"text"}`, "text", part.Get("text").String()))
		case "input_image":
			url := part.Get("image_url").String()
			if url == "" {
				return "", refuse("%s: an image by file_id is a file kept at OpenAI; send its image_url", where)
			}
			image := set(`{"type":"image_url","image_url":{}}`, "image_url.url", url)
			if detail := part.Get("detail"); detail.Exists() && detail.Type != gjson.Null {
				image = setRaw(image, "image_url.detail", detail.Raw)
			}
			parts = append(parts, image)
		case "input_file":
			if !part.Get("file_data").Exists() {
				return "", refuse("%s: a file by file_id or file_url is fetched by OpenAI; send its file_data", where)
			}
			file := set(`{"type":"file","file":{}}`, "file.file_data", part.Get("file_data").String())
			if name := part.Get("filename"); name.Exists() {
				file = set(file, "file.filename", name.String())
			}
			parts = append(parts, file)
		default:
			return "", refuse("%s: a '%s' part can't be served through Chat Completions", where, kind)
		}
	}
	if len(texts) == len(parts) {
		return set(`{"role":"user"}`, "content", strings.Join(texts, "\n\n")), nil
	}
	return setRaw(`{"role":"user"}`, "content", "["+strings.Join(parts, ",")+"]"), nil
}

// chatToolsFromResponses turns function and custom tools into Chat
// Completions tools. The other tools only OpenAI's servers run.
func chatToolsFromResponses(tools gjson.Result) (out string, err error) {
	var list []string
	for i, tool := range tools.Array() {
		switch kind := tool.Get("type").String(); kind {
		case "function":
			function := set(`{}`, "name", tool.Get("name").String())
			for _, key := range []string{"description", "parameters", "strict"} {
				function = setPresent(function, key, tool.Get(key))
			}
			list = append(list, setRaw(`{"type":"function"}`, "function", function))
		case "custom":
			custom := set(`{}`, "name", tool.Get("name").String())
			for _, key := range []string{"description", "format"} {
				custom = setPresent(custom, key, tool.Get(key))
			}
			list = append(list, setRaw(`{"type":"custom"}`, "custom", custom))
		default:
			return "", refuse("tools[%d]: '%s' is a tool OpenAI's servers run, and this model is served through Chat Completions, which can't run it", i, kind)
		}
	}
	if list != nil {
		out = "[" + strings.Join(list, ",") + "]"
	}
	return out, nil
}

// chatToolChoiceFromResponses translates a tool choice: a function or custom
// tool named, or the tools allowed.
func chatToolChoiceFromResponses(choice gjson.Result) (string, error) {
	switch {
	case choice.Type == gjson.Null:
		return "", nil
	case choice.Type == gjson.String:
		return choice.Raw, nil
	}
	switch kind := choice.Get("type").String(); kind {
	case "function", "custom":
		return set(set("{}", "type", kind), kind+".name", choice.Get("name").String()), nil
	case "allowed_tools":
		allowed := set(`{"type":"allowed_tools","allowed_tools":{}}`, "allowed_tools.mode", choice.Get("mode").String())
		var tools []string
		for _, tool := range choice.Get("tools").Array() {
			kind := tool.Get("type").String()
			tools = append(tools, set(set("{}", "type", kind), pathKey(kind)+".name", tool.Get("name").String()))
		}
		return setRaw(allowed, "allowed_tools.tools", "["+strings.Join(tools, ",")+"]"), nil
	}
	return "", refuse("tool_choice %s has no Chat Completions equivalent", choice.Raw)
}

// chatFormatFromResponses turns text.format into response_format; plain text
// needs none.
func chatFormatFromResponses(format gjson.Result) (string, error) {
	switch kind := format.Get("type").String(); kind {
	case "", "text":
		return "", nil
	case "json_object":
		return `{"type":"json_object"}`, nil
	case "json_schema":
		schema := "{}"
		for _, key := range []string{"name", "description", "schema", "strict"} {
			if v := format.Get(key); v.Exists() && v.Type != gjson.Null {
				schema = setRaw(schema, key, v.Raw)
			}
		}
		return setRaw(`{"type":"json_schema"}`, "json_schema", schema), nil
	default:
		return "", refuse("text.format '%s' has no Chat Completions equivalent", kind)
	}
}

// chatResponsesReply converts what a Chat Completions backend answered back
// to Responses: a response or a stream, or an error in OpenAI's shape with
// the backend's status.
type chatResponsesReply struct {
	request gjson.Result
	dropped droppedFields
}

func (r *chatResponsesReply) Dropped() []string { return r.dropped }

func (r *chatResponsesReply) Result(result backend.ChatResult) backend.ChatResult {
	if result.Stream != nil {
		return backend.ChatResult{Stream: newChatResponsesStream(result.Stream, r.request)}
	}
	resp := *result.Response
	resp.ContentType = "application/json"
	if resp.Status/100 == 2 {
		resp.Body = responseFromChat(gjson.ParseBytes(resp.Body), r.request)
		resp.RetryAfter = ""
	} else if !gjson.GetBytes(resp.Body, "error.message").Exists() {
		resp.Body = openAIErrorBody("api_error", errorMessage(resp.Body))
	}
	return backend.ChatResult{Response: &resp}
}

// responseFromChat turns a chat completion into a response: reasoning as a
// reasoning item, the text and any refusal as a message, and each tool call
// as a function call.
func responseFromChat(completion, request gjson.Result) []byte {
	b := newResponseBuilder(request, false)
	b.model = completion.Get("model").String()
	choice := completion.Get("choices.0")
	message := choice.Get("message")
	b.reasoning("reasoning_text", reasoningOf(message))
	b.text("output_text", message.Get("content").String())
	b.text("refusal", message.Get("refusal").String())
	for _, call := range message.Get("tool_calls").Array() {
		b.call(call.Get("id").String(), call.Get("function.name").String())
		b.arguments(call.Get("function.arguments").String())
	}
	return b.finish(incompleteReason(choice.Get("finish_reason").String()), chatUsageForResponses(completion.Get("usage")))
}

// incompleteReason maps a Chat Completions finish_reason to Responses'
// incomplete_details.reason, "" for a complete response.
func incompleteReason(finish string) string {
	switch finish {
	case "length":
		return "max_output_tokens"
	case "content_filter":
		return "content_filter"
	}
	return ""
}

// chatUsageForResponses converts Chat Completions usage, which counts cache
// reads within prompt_tokens as Responses does within input_tokens.
func chatUsageForResponses(usage gjson.Result) map[string]any {
	return responsesUsage(usage.Get("prompt_tokens").Int(), usage.Get("prompt_tokens_details.cached_tokens").Int(), usage.Get("completion_tokens").Int(), usage.Get("completion_tokens_details.reasoning_tokens").Int())
}

// newChatResponsesStream turns Chat Completions chunks into Responses events,
// as responseFromChat does a complete one. Chunks may interleave tool
// calls, while Responses sends one item after another, so the first call
// streams its arguments and the others are sent complete at the end, as
// messagesToChat's stream does.
func newChatResponsesStream(chunks backend.ChatStream, request gjson.Result) *responsesStream {
	b := newResponseBuilder(request, true)
	var calls []*toolCall
	var finish string
	var usage gjson.Result
	read := func(data []byte) bool {
		chunk := gjson.ParseBytes(data)
		b.start(chunk.Get("model").String())
		if u := chunk.Get("usage"); u.IsObject() {
			usage = u
		}
		choice := chunk.Get("choices.0")
		delta := choice.Get("delta")
		b.reasoning("reasoning_text", reasoningOf(delta))
		b.text("output_text", delta.Get("content").String())
		b.text("refusal", delta.Get("refusal").String())
		for _, d := range delta.Get("tool_calls").Array() {
			index := d.Get("index").Int()
			var call *toolCall
			for _, c := range calls {
				if c.chunkIndex == index {
					call = c
				}
			}
			if call == nil {
				call = &toolCall{chunkIndex: index}
				calls = append(calls, call)
			}
			if id := d.Get("id").String(); id != "" {
				call.id = id
			}
			if name := d.Get("function.name").String(); name != "" {
				call.name = name
			}
			arguments := d.Get("function.arguments").String()
			if call != calls[0] || call.name == "" || call.opened && (b.open == nil || b.open.kind != "function_call") {
				call.arguments += arguments
				continue
			}
			if !call.opened {
				call.opened = true
				b.call(call.id, call.name)
				b.arguments(call.arguments)
				call.arguments = ""
			}
			b.arguments(arguments)
		}
		if f := choice.Get("finish_reason").String(); f != "" {
			finish = f
		}
		return false
	}
	end := func() {
		for _, call := range calls {
			if !call.opened {
				b.call(call.id, call.name)
			}
			b.arguments(call.arguments)
		}
		b.finish(incompleteReason(finish), chatUsageForResponses(usage))
	}
	return &responsesStream{events: chunks, builder: b, read: read, end: end}
}
