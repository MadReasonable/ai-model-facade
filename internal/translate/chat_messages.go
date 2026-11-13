package translate

import (
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// This file is the Chat Completions and Messages pair: chatToMessages for a
// Chat Completions client on a Messages backend, messagesToChat for the
// reverse, each translating the request one way and its reply, whole or
// streamed, back.

// chatToMessages is the direct translator from Chat Completions to Messages.
type chatToMessages struct{}

func (chatToMessages) Request(body []byte) ([]byte, Reply, error) {
	out, includeUsage, err := toMessages(body)
	if err != nil {
		return nil, nil, err
	}
	return out, &messagesReply{includeUsage: includeUsage, legacy: legacyFunctions(gjson.ParseBytes(body))}, nil
}

// dropped are Chat Completions fields with no Messages counterpart that tune
// sampling or configure OpenAI's own services, so leaving them out changes
// nothing a reply depends on. The API refuses any field it doesn't know
// (verified: "seed: Extra inputs are not permitted").
var dropped = map[string]bool{
	"frequency_penalty":      true,
	"logit_bias":             true,
	"metadata":               true, // OpenAI's stored-completion tags, not Anthropic's metadata
	"moderation":             true,
	"prediction":             true,
	"presence_penalty":       true,
	"prompt_cache_key":       true,
	"prompt_cache_options":   true,
	"prompt_cache_retention": true,
	"seed":                   true,
	"service_tier":           true, // same name, other values
	"store":                  true,
	"top_logprobs":           true,
	"verbosity":              true,
}

// toMessages translates a Chat Completions body into a Messages body, and
// says whether the client asked for usage at the end of a stream.
//
// Top-level keys keep the client's order, each translated in place. A key
// with no translation, such as thinking, cache_control or top_k, is passed on
// unchanged, so a facade's params can set Messages fields directly; where
// one also comes from a translated field, as output_config does from
// reasoning_effort, the native value's own keys win. A missing max_tokens
// stays missing: filling it takes the winning model's output cap, after
// params, which the adapter does for every client of Messages.
func toMessages(body []byte) ([]byte, bool, error) {
	req := gjson.ParseBytes(body)
	if err := checkUnsupported(req); err != nil {
		return nil, false, err
	}
	messages, err := readChatMessages(req)
	if err != nil {
		return nil, false, err
	}
	system, turns, err := messagesTurnsFromChat(messages)
	if err != nil {
		return nil, false, err
	}
	if system != "" && req.Get("system").Exists() {
		return nil, false, refuse("the request has both system messages and a Messages 'system' field; use one")
	}
	choice, err := readChatToolChoice(req)
	if err != nil {
		return nil, false, err
	}
	tools, err := messagesToolsFromChat(req, choice)
	if err != nil {
		return nil, false, err
	}
	toolChoice, err := messagesChoiceFromChat(req, choice)
	if err != nil {
		return nil, false, err
	}

	var out members
	placed := map[string]bool{}
	place := func(key, raw string) {
		if raw != "" && !placed[key] {
			placed[key] = true
			out.add(strconv.Quote(key), raw)
		}
	}
	req.ForEach(func(key, value gjson.Result) bool {
		switch k := key.String(); {
		case k == "messages":
			place("system", system)
			place("messages", turns)
		case k == "max_tokens" || k == "max_completion_tokens":
			place("max_tokens", maxTokensOf(req))
		case k == "output_config" || k == "reasoning_effort" || k == "response_format":
			place("output_config", outputConfigOf(req))
		case k == "tool_choice" || k == "parallel_tool_calls" || k == "function_call":
			place("tool_choice", toolChoice)
		case k == "user" || k == "safety_identifier":
			place("metadata", metadataOf(req))
		case k == "stop":
			place("stop_sequences", stopSequencesOf(value))
		case k == "tools" || k == "functions":
			place("tools", tools)
		case k == "stream_options" || k == "n" || k == "logprobs" || k == "modalities" || k == "audio" || k == "web_search_options":
			// checked by checkUnsupported, so only a harmless value gets here,
			// and stream_options is read for include_usage
		case dropped[k]:
			// left out
		default:
			if !placed[k] {
				placed[k] = true
				out.add(key.Raw, value.Raw)
			}
		}
		return true
	})
	place("tool_choice", toolChoice) // a legacy request may set no tool_choice key
	return out.bytes(), req.Get("stream_options.include_usage").Bool(), nil
}

// checkUnsupported refuses the fields that ask for something Messages can't
// give: several choices, log probabilities, audio, a JSON object without a
// schema, or OpenAI's web search.
func checkUnsupported(req gjson.Result) error {
	if n := req.Get("n"); n.Exists() && n.Type != gjson.Null && n.Int() != 1 {
		return refuse("the Claude API returns one choice per request; 'n' must be 1, not %s", n.Raw)
	}
	if req.Get("logprobs").Bool() {
		return refuse("the Claude API returns no log probabilities; 'logprobs' must be false")
	}
	for _, m := range req.Get("modalities").Array() {
		if m.String() != "text" {
			return refuse("the Claude API answers only in text, not '%s'", m.String())
		}
	}
	if a := req.Get("audio"); a.Exists() && a.Type != gjson.Null {
		return refuse("the Claude API has no audio output; 'audio' must be absent")
	}
	if t := req.Get("response_format.type").String(); t != "" && t != "text" && t != "json_schema" {
		return refuse("the Claude API constrains output only to a JSON schema; response_format '%s' has no equivalent", t)
	}
	if req.Get("web_search_options").Exists() {
		return refuse("'web_search_options' is OpenAI's web search; on the Claude API, add its web search tool through the facade's params")
	}
	return nil
}

func maxTokensOf(req gjson.Result) string {
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if v := req.Get(key); v.Exists() && v.Type != gjson.Null {
			return v.Raw
		}
	}
	return ""
}

// outputConfigOf builds output_config from reasoning_effort, as the nearest
// level Claude has, and a JSON schema response_format, with the request's own
// output_config keys over them.
func outputConfigOf(req gjson.Result) string {
	config := "{}"
	if effort := req.Get("reasoning_effort"); effort.Type == gjson.String {
		config = set(config, "effort", claudeEffort(effort.String()))
	}
	if format := readChatFormat(req); format.kind == "json_schema" {
		config = setRaw(config, "format", setRaw(`{"type":"json_schema"}`, "schema", format.schema.Raw))
	}
	req.Get("output_config").ForEach(func(key, value gjson.Result) bool {
		config = setRaw(config, pathKey(key.String()), value.Raw)
		return true
	})
	if config == "{}" {
		return ""
	}
	return config
}

func metadataOf(req gjson.Result) string {
	for _, key := range []string{"safety_identifier", "user"} {
		if v := req.Get(key); v.Type == gjson.String && v.String() != "" {
			return setRaw("{}", "user_id", v.Raw)
		}
	}
	return ""
}

func stopSequencesOf(stop gjson.Result) string {
	switch {
	case stop.Type == gjson.String:
		return "[" + stop.Raw + "]"
	case stop.IsArray():
		return stop.Raw
	}
	return ""
}

// messagesChoiceFromChat translates tool_choice, the legacy function_call
// included. A list of allowed tools narrows the tools sent (see
// messagesToolsFromChat), so it becomes its mode alone. parallel_tool_calls:
// false, and a legacy request, which takes one call at a time, become
// disable_parallel_tool_use.
func messagesChoiceFromChat(req gjson.Result, choice chatChoice) (string, error) {
	serial := req.Get("parallel_tool_calls").Type == gjson.False || legacyFunctions(req)
	var out string
	mode := choice.mode
	if mode == "allowed" {
		if choice.allowedMode != "auto" && choice.allowedMode != "required" {
			return "", refuse("tool_choice allowed_tools needs a mode of 'auto' or 'required', not '%s'", choice.allowedMode)
		}
		mode = choice.allowedMode
	}
	switch mode {
	case "":
		if !serial || !(req.Get("tools").IsArray() || req.Get("functions").IsArray()) {
			return "", nil
		}
		out = `{"type":"auto"}`
	case "auto":
		out = `{"type":"auto"}`
	case "required":
		out = `{"type":"any"}`
	case "none":
		return `{"type":"none"}`, nil
	case "tool":
		if choice.kind != "function" {
			return "", refuse("tool_choice names a custom tool, and the Claude API has no custom tools")
		}
		out = set(`{"type":"tool"}`, "name", choice.name)
	default:
		return "", refuse("tool_choice '%s' has no Claude API equivalent", mode)
	}
	if serial {
		out = set(out, "disable_parallel_tool_use", true)
	}
	return out, nil
}

// messagesToolsFromChat turns function tools, the legacy functions included,
// into Messages tools. A tool_choice listing allowed tools narrows the tools
// to those, Claude having no such list. Claude has no custom tools.
func messagesToolsFromChat(req gjson.Result, choice chatChoice) (string, error) {
	tools, err := readChatTools(req)
	if err != nil {
		return "", err
	}
	allowed := func(tool chatTool) bool {
		return choice.mode != "allowed" || slices.ContainsFunc(choice.allowed, func(a chatChoiceTool) bool { return a.name == tool.name })
	}
	var out []string
	for i, tool := range tools {
		if tool.kind == "custom" {
			return "", refuse("tools[%d]: the Claude API has no custom tools; '%s' needs to be a function tool", i, tool.name)
		}
		if !allowed(tool) {
			continue
		}
		block := set("{}", "name", tool.name)
		if tool.description.Exists() {
			block = setRaw(block, "description", tool.description.Raw)
		}
		schema := tool.parameters.Raw
		if schema == "" || schema == "null" {
			schema = `{"type":"object"}` // Messages requires a schema; OpenAI's default is no parameters
		}
		block = setRaw(block, "input_schema", schema)
		if tool.strict.Exists() && tool.strict.Type != gjson.Null {
			block = setRaw(block, "strict", tool.strict.Raw)
		}
		out = append(out, block)
	}
	if out == nil {
		return "", nil
	}
	return "[" + strings.Join(out, ",") + "]", nil
}

// messagesTurnsFromChat turns the conversation into Messages' top-level
// system blocks and its user and assistant turns. System and developer
// messages are gathered into system, wherever they stand. A tool result
// becomes a tool_result block of a user turn, and consecutive turns of one
// role become one, so the results of parallel tool calls share a turn, as the
// API asks.
func messagesTurnsFromChat(messages []chatMessage) (system, turns string, err error) {
	systemBlocks := "[]"
	turns = "[]"
	lastRole := ""
	for _, m := range messages {
		role := m.role
		var blocks []string
		switch role {
		case "system", "developer":
			texts, err := messagesTextBlocks(m.content, m.where)
			if err != nil {
				return "", "", err
			}
			for _, b := range texts {
				systemBlocks = setRaw(systemBlocks, "-1", b)
			}
			continue
		case "user":
			blocks, err = messagesUserBlocksFromChat(m.content, m.where)
		case "assistant":
			blocks, err = messagesAssistantBlocksFromChat(m)
		case "tool":
			blocks, err = messagesToolResultFromChat(m)
			role = "user"
		}
		if err != nil {
			return "", "", err
		}
		if len(blocks) == 0 {
			continue
		}
		if role == lastRole {
			last := int(gjson.Get(turns, "#").Int()) - 1
			for _, b := range blocks {
				turns = setRaw(turns, strconv.Itoa(last)+".content.-1", b)
			}
			continue
		}
		turn := set("{}", "role", role)
		turn = setRaw(turn, "content", "["+strings.Join(blocks, ",")+"]")
		turns = setRaw(turns, "-1", turn)
		lastRole = role
	}
	if systemBlocks != "[]" {
		system = systemBlocks
	}
	return system, turns, nil
}

// messagesTextBlocks turns a message's text into text blocks, leaving out
// empty text, which the API refuses.
func messagesTextBlocks(content gjson.Result, where string) ([]string, error) {
	parts, err := readChatParts(content, where)
	if err != nil {
		return nil, err
	}
	var blocks []string
	for _, part := range parts {
		if part.kind != "text" {
			return nil, refuse("%s: a '%s' part can't stand here; only text can", where, part.kind)
		}
		if part.text != "" {
			blocks = append(blocks, set(`{"type":"text"}`, "text", part.text))
		}
	}
	return blocks, nil
}

// messagesUserBlocksFromChat turns a user message's content into text, image and PDF
// document blocks. An image's detail has no Claude counterpart.
func messagesUserBlocksFromChat(content gjson.Result, where string) ([]string, error) {
	parts, err := readChatParts(content, where)
	if err != nil {
		return nil, err
	}
	var blocks []string
	for _, part := range parts {
		switch part.kind {
		case "text":
			if part.text != "" {
				blocks = append(blocks, set(`{"type":"text"}`, "text", part.text))
			}
		case "image":
			block, err := imageBlock(part.url, where)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		case "file":
			block, err := documentBlock(part, where)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		default:
			return nil, refuse("%s: the Claude API takes no '%s' part", where, part.kind)
		}
	}
	return blocks, nil
}

// messagesAssistantBlocksFromChat turns an assistant message's text, refusal and tool
// calls into text and tool_use blocks. Its reasoning_content is left out: the
// API takes thinking back only with the signature it was sent with, which
// Chat Completions has no place for.
func messagesAssistantBlocksFromChat(m chatMessage) ([]string, error) {
	blocks, err := messagesTextBlocks(m.content, m.where)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 && m.refusal != "" {
		blocks = append(blocks, set(`{"type":"text"}`, "text", m.refusal))
	}
	for j, call := range m.calls {
		if call.kind == "custom" {
			return nil, refuse("%s.tool_calls[%d]: the Claude API has no custom tools", m.where, j)
		}
		args := call.value
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		if !gjson.Valid(args) || !gjson.Parse(args).IsObject() {
			return nil, refuse("%s.tool_calls[%d]: arguments must be a JSON object", m.where, j)
		}
		block := set(`{"type":"tool_use"}`, "id", call.id)
		block = set(block, "name", call.name)
		blocks = append(blocks, setRaw(block, "input", args))
	}
	return blocks, nil
}

// messagesToolResultFromChat turns a tool message into the tool_result block of its
// call, its content a string or text blocks.
func messagesToolResultFromChat(m chatMessage) ([]string, error) {
	block := set(`{"type":"tool_result"}`, "tool_use_id", m.callID)
	if m.content.IsArray() {
		texts, err := messagesTextBlocks(m.content, m.where)
		if err != nil {
			return nil, err
		}
		return []string{setRaw(block, "content", "["+strings.Join(texts, ",")+"]")}, nil
	}
	if m.content.Type == gjson.String {
		block = setRaw(block, "content", m.content.Raw)
	}
	return []string{block}, nil
}

// finishReason maps a Messages stop_reason to Chat Completions'. A refusal
// is a content filter's decision in OpenAI's terms; pause_turn, which only
// server tools cause, and any reason added later end the reply as stop.
func finishReason(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

// usageOf converts Messages usage, read through usage, to Chat Completions'.
// Messages counts cache reads and writes apart from input_tokens; OpenAI's
// prompt_tokens includes them, with the reads again as cached_tokens.
func usageOf(usage func(path string) gjson.Result) map[string]any {
	cacheRead := usage("cache_read_input_tokens").Int()
	prompt := usage("input_tokens").Int() + usage("cache_creation_input_tokens").Int() + cacheRead
	completion := usage("output_tokens").Int()
	out := map[string]any{
		"prompt_tokens":         prompt,
		"completion_tokens":     completion,
		"total_tokens":          prompt + completion,
		"prompt_tokens_details": map[string]any{"cached_tokens": cacheRead},
	}
	if thinking := usage("output_tokens_details.thinking_tokens"); thinking.Exists() {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": thinking.Int()}
	}
	return out
}

// completion turns a Messages response into a chat completion: text blocks
// joined as content, thinking as reasoning_content, as llama-server reports
// reasoning, and tool_use blocks as tool calls, each input as its arguments
// string. Other blocks, such as server tool results, have no place in a chat
// completion and are left out.
func completion(msg gjson.Result, legacy bool, created int64) []byte {
	b := newChatWriter(false, false)
	b.legacy = legacy
	b.start(msg.Get("id").String(), msg.Get("model").String(), created)
	for _, block := range msg.Get("content").Array() {
		switch block.Get("type").String() {
		case "text":
			b.text(block.Get("text").String())
		case "thinking":
			b.reason(block.Get("thinking").String())
		case "tool_use":
			b.arguments(b.call(block.Get("id").String(), block.Get("name").String()), block.Get("input").Raw)
		}
	}
	b.finish(finishReason(msg.Get("stop_reason").String()), usageOf(msg.Get("usage").Get))
	return b.body()
}

// openAIError turns a Claude API error body into OpenAI's error shape,
// keeping its message and type. A body that isn't one becomes the message.
func openAIError(body []byte) []byte {
	detail := gjson.GetBytes(body, "error")
	message, errType := detail.Get("message").String(), detail.Get("type").String()
	if message == "" {
		message, errType = strings.TrimSpace(string(body)), "api_error"
	}
	return openAIErrorBody(errType, message)
}

// messagesReply converts what a Messages backend answered back to a chat
// completion: a response or a stream, or an error in OpenAI's shape with the
// backend's status.
type messagesReply struct {
	includeUsage bool
	legacy       bool // the client called with the deprecated functions
}

func (r *messagesReply) Result(result backend.ChatResult) backend.ChatResult {
	created := time.Now().Unix()
	if result.Stream != nil {
		return backend.ChatResult{Stream: newChunkStream(result.Stream, r.includeUsage, r.legacy, created)}
	}
	resp := *result.Response
	resp.ContentType = "application/json"
	if resp.Status/100 == 2 {
		resp.Body = completion(gjson.ParseBytes(resp.Body), r.legacy, created)
		resp.RetryAfter = ""
	} else {
		resp.Body = openAIError(resp.Body)
	}
	return backend.ChatResult{Response: &resp}
}

// chunkStream turns a Messages event stream into Chat Completions chunks, as
// completion does a complete response:
//
//   - message_start, a first chunk with the assistant role;
//   - a text, thinking or tool-input delta, a chunk with content,
//     reasoning_content or the call's arguments;
//   - a tool_use block's start, a chunk opening the tool call, numbered in
//     the order the calls arrive, as OpenAI numbers them;
//   - message_stop, the finish chunk, with the stop reason message_delta gave,
//     then a usage chunk with no choices if the client set
//     stream_options.include_usage.
//
// Blocks of other kinds, such as a server tool's call, open no tool call, so
// their input deltas reach nothing; signature deltas, which Chat Completions
// has no place for, and pings yield nothing. A body that ends before
// message_stop is broken.
type chunkStream struct {
	events  backend.ChatStream
	reply   *chatWriter
	created int64
	done    bool

	startUsage, deltaUsage gjson.Result
	stop                   string
	calls                  map[int64]int // block index -> tool call index
}

func newChunkStream(events backend.ChatStream, includeUsage, legacy bool, created int64) *chunkStream {
	reply := newChatWriter(true, includeUsage)
	reply.legacy = legacy
	return &chunkStream{events: events, reply: reply, created: created, calls: map[int64]int{}}
}

func (s *chunkStream) Next() ([]byte, error) {
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
}

func (s *chunkStream) translate(event gjson.Result) {
	index := event.Get("index").Int()
	switch event.Get("type").String() {
	case "message_start":
		s.reply.start(event.Get("message.id").String(), event.Get("message.model").String(), s.created)
		s.startUsage = event.Get("message.usage")
	case "content_block_start":
		block := event.Get("content_block")
		switch block.Get("type").String() {
		case "tool_use":
			s.calls[index] = s.reply.call(block.Get("id").String(), block.Get("name").String())
		}
	case "content_block_delta":
		delta := event.Get("delta")
		switch delta.Get("type").String() {
		case "text_delta":
			s.reply.text(delta.Get("text").String())
		case "thinking_delta":
			s.reply.reason(delta.Get("thinking").String())
		case "input_json_delta":
			if call, ok := s.calls[index]; ok {
				s.reply.arguments(call, delta.Get("partial_json").String())
			}
		}
	case "message_delta":
		s.deltaUsage = event.Get("usage")
		s.stop = event.Get("delta.stop_reason").String()
	case "message_stop":
		s.reply.finish(finishReason(s.stop), usageOf(s.usage))
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

func (s *chunkStream) Close() error {
	return s.events.Close()
}
