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
	out, includeUsage, gone, err := toMessages(body)
	if err != nil {
		return nil, nil, err
	}
	return out, &messagesReply{includeUsage: includeUsage, legacy: legacyFunctions(gjson.ParseBytes(body)), dropped: gone}, nil
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
func toMessages(body []byte) ([]byte, bool, droppedFields, error) {
	req := gjson.ParseBytes(body)
	if err := checkUnsupported(req); err != nil {
		return nil, false, nil, err
	}
	var gone droppedFields
	messages, err := readChatMessages(req)
	if err != nil {
		return nil, false, nil, err
	}
	system, turns, err := messagesTurnsFromChat(messages, &gone)
	if err != nil {
		return nil, false, nil, err
	}
	if system != "" && req.Get("system").Exists() {
		return nil, false, nil, refuse("the request has both system messages and a Messages 'system' field; use one")
	}
	choice, err := readChatToolChoice(req)
	if err != nil {
		return nil, false, nil, err
	}
	tools, err := messagesToolsFromChat(req, choice, &gone)
	if err != nil {
		return nil, false, nil, err
	}
	toolChoice, err := messagesChoiceFromChat(req, choice)
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
		case k == "tools" || k == "functions" || k == "web_search_options":
			place("tools", tools)
		case k == "stream_options" || k == "n" || k == "logprobs" || k == "modalities" || k == "audio":
			// checked by checkUnsupported, so only a harmless value gets here,
			// and stream_options is read for include_usage
		case dropped[k]:
			gone.add(k)
		default:
			if !placed[k] {
				placed[k] = true
				out.add(key.Raw, value.Raw)
			}
		}
		return true
	})
	place("tool_choice", toolChoice) // a legacy request may set no tool_choice key
	return out.bytes(), req.Get("stream_options.include_usage").Bool(), gone, nil
}

// checkUnsupported refuses the fields that ask for something Messages can't
// give: several choices, log probabilities, audio, or a JSON object without a
// schema.
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
// into Messages tools, and web_search_options into Claude's web search tool,
// in the version whose results come back with citations (verified
// 2026-11-10: the later versions filter results with code execution and
// returned none). A tool_choice listing allowed tools narrows the tools to
// those, Claude having no such list. Claude has no custom tools.
func messagesToolsFromChat(req gjson.Result, choice chatChoice, gone *droppedFields) (string, error) {
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
	if options := req.Get("web_search_options"); options.IsObject() {
		search := `{"type":"web_search_20250305","name":"web_search"}`
		if location := options.Get("user_location"); location.IsObject() {
			flat := set("{}", "type", location.Get("type").String())
			location.Get("approximate").ForEach(func(key, value gjson.Result) bool {
				flat = setRaw(flat, pathKey(key.String()), value.Raw)
				return true
			})
			search = setRaw(search, "user_location", flat)
		}
		if options.Get("search_context_size").Exists() {
			gone.add("web_search_options.search_context_size")
		}
		out = append(out, search)
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
func messagesTurnsFromChat(messages []chatMessage, gone *droppedFields) (system, turns string, err error) {
	systemBlocks := "[]"
	turns = "[]"
	lastRole := ""
	for _, m := range messages {
		if m.named {
			gone.add("name")
		}
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
			blocks, err = messagesUserBlocksFromChat(m.content, m.where, gone)
		case "assistant":
			blocks, err = messagesAssistantBlocksFromChat(m, gone)
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
func messagesUserBlocksFromChat(content gjson.Result, where string, gone *droppedFields) ([]string, error) {
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
			if part.detail != "" {
				gone.add("image_url.detail")
			}
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
func messagesAssistantBlocksFromChat(m chatMessage, gone *droppedFields) ([]string, error) {
	blocks, err := messagesTextBlocks(m.content, m.where)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 && m.refusal != "" {
		blocks = append(blocks, set(`{"type":"text"}`, "text", m.refusal))
	}
	if m.reasoning {
		gone.add("reasoning_content")
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
// is a content filter's decision in OpenAI's terms; pause_turn, which reaches
// a client only once the gateway has sent the turn back maxResends times
// (Resend), and any reason added later end the reply as stop.
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
// joined as content, with a text block's citations as annotations over its
// text, thinking as reasoning_content, as llama-server reports reasoning, and
// tool_use blocks as tool calls, each input as its arguments string. Other
// blocks, such as server tool results, have no place in a chat completion and
// are left out.
func completion(msg gjson.Result, legacy bool, created int64) []byte {
	b := newChatWriter(false, false)
	b.legacy = legacy
	b.start(msg.Get("id").String(), msg.Get("model").String(), created)
	for _, block := range msg.Get("content").Array() {
		switch block.Get("type").String() {
		case "text":
			start := b.sent()
			b.text(block.Get("text").String(), nil)
			annotateCitations(b, block.Get("citations").Array(), start)
		case "thinking":
			b.reason(block.Get("thinking").String())
		case "tool_use":
			b.arguments(b.call("function", block.Get("id").String(), block.Get("name").String()), block.Get("input").Raw)
		}
	}
	b.finish(finishReason(msg.Get("stop_reason").String()), usageOf(msg.Get("usage").Get))
	return b.body()
}

// annotateCitations adds a text block's web citations as annotations over
// its text, from start to what has been sent since. Citations of a document
// the client sent have no Chat Completions counterpart.
func annotateCitations(b *chatWriter, citations []gjson.Result, start int) {
	for _, citation := range citations {
		if url := citation.Get("url").String(); url != "" {
			b.annotate(url, citation.Get("title").String(), start, b.sent())
		}
	}
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
	dropped      droppedFields
}

func (r *messagesReply) Dropped() []string { return r.dropped }

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
//   - a text block's citations, which arrive ahead of its text, a chunk of
//     annotations over its text once the block stops;
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
	calls                  map[int64]int            // block index -> tool call index
	starts                 map[int64]int            // text block index -> content characters before it
	citations              map[int64][]gjson.Result // text block index -> its citations
}

func newChunkStream(events backend.ChatStream, includeUsage, legacy bool, created int64) *chunkStream {
	reply := newChatWriter(true, includeUsage)
	reply.legacy = legacy
	return &chunkStream{events: events, reply: reply, created: created, calls: map[int64]int{}, starts: map[int64]int{}, citations: map[int64][]gjson.Result{}}
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
			s.calls[index] = s.reply.call("function", block.Get("id").String(), block.Get("name").String())
		case "text":
			s.starts[index] = s.reply.sent()
		}
	case "content_block_delta":
		delta := event.Get("delta")
		switch delta.Get("type").String() {
		case "text_delta":
			s.reply.text(delta.Get("text").String(), nil)
		case "thinking_delta":
			s.reply.reason(delta.Get("thinking").String())
		case "input_json_delta":
			if call, ok := s.calls[index]; ok {
				s.reply.arguments(call, delta.Get("partial_json").String())
			}
		case "citations_delta":
			s.citations[index] = append(s.citations[index], delta.Get("citation"))
		}
	case "content_block_stop":
		if citations := s.citations[index]; len(citations) > 0 {
			annotateCitations(s.reply, citations, s.starts[index])
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

// messagesToChat is the direct translator from Messages to Chat Completions.
type messagesToChat struct{}

func (messagesToChat) Request(body []byte) ([]byte, Reply, error) {
	out, gone, err := toChat(body)
	if err != nil {
		return nil, nil, err
	}
	return out, &chatReply{dropped: gone}, nil
}

// refusedMessagesFields ask for what only Claude's servers can do.
var refusedMessagesFields = map[string]bool{
	"container":   true,
	"mcp_servers": true,
}

// toChat translates a Messages body into a Chat Completions body. Top-level
// keys keep the client's order, each translated in place, and a key with no
// translation, such as top_k, passes on unchanged. A stream asks for usage,
// since Messages always reports it. thinking has no Chat Completions
// counterpart, and thinking blocks go the same way: a Chat Completions model
// can't use them.
func toChat(body []byte) ([]byte, droppedFields, error) {
	req := gjson.ParseBytes(body)
	turns, err := readMessagesTurns(req)
	if err != nil {
		return nil, nil, err
	}
	var gone droppedFields
	messages, err := chatMessages(req, turns, &gone)
	if err != nil {
		return nil, nil, err
	}
	var out members
	placedMessages := false
	var failed error
	req.ForEach(func(key, value gjson.Result) bool {
		switch k := key.String(); {
		case k == "system":
			// joined with messages, where it is placed
		case k == "messages":
			out.add(`"messages"`, messages)
			placedMessages = true
		case k == "stop_sequences":
			out.add(`"stop"`, value.Raw)
		case k == "tools":
			var tools string
			var search string
			if tools, search, failed = chatTools(value, &gone); tools != "" {
				out.add(`"tools"`, tools)
			}
			if search != "" {
				out.add(`"web_search_options"`, search)
			}
		case k == "tool_choice":
			var choice, parallel string
			if choice, parallel, failed = chatToolChoice(value); choice != "" {
				out.add(`"tool_choice"`, choice)
			}
			if parallel != "" {
				out.add(`"parallel_tool_calls"`, parallel)
			}
		case k == "output_config":
			if effort := value.Get("effort"); effort.Type == gjson.String {
				out.add(`"reasoning_effort"`, strconv.Quote(openAIEffort(effort.String())))
			}
			value.ForEach(func(setting, _ gjson.Result) bool {
				if s := setting.String(); s != "effort" && s != "format" {
					gone.add("output_config." + s)
				}
				return true
			})
			if format := value.Get("format"); format.Exists() {
				var chatFormat string
				if chatFormat, failed = chatResponseFormat(format); chatFormat != "" {
					out.add(`"response_format"`, chatFormat)
				}
			}
		case k == "metadata":
			if user := value.Get("user_id"); user.Exists() {
				out.add(`"user"`, user.Raw)
			}
			value.ForEach(func(setting, _ gjson.Result) bool {
				if s := setting.String(); s != "user_id" {
					gone.add("metadata." + s)
				}
				return true
			})
		case refusedMessagesFields[k]:
			failed = refuseMessages("'%s' needs the Claude API, and this model is served through Chat Completions", k)
		case k == "thinking" || droppedMessagesFieldsEverywhere[k]:
			gone.add(k)
		default:
			out.add(key.Raw, value.Raw)
		}
		return failed == nil
	})
	if failed != nil {
		return nil, nil, failed
	}
	reportDroppedBlocks(body, turns, &gone)
	if !placedMessages {
		out.add(`"messages"`, messages)
	}
	if req.Get("stream").Bool() {
		out.add(`"stream_options"`, `{"include_usage":true}`)
	}
	return out.bytes(), gone, nil
}

// chatMessages returns the conversation as a Chat Completions messages array.
// The system prompt comes first, joined with any system message in the
// conversation, as Claude Code sends one (seen on 2.1.319), since not every
// model takes a system message mid-conversation.
func chatMessages(req gjson.Result, turns []messagesTurn, gone *droppedFields) (string, error) {
	var system, messages []string
	text, ok, err := readMessagesSystem(req)
	if err != nil {
		return "", err
	}
	if ok {
		system = append(system, text)
	}
	for _, t := range turns {
		switch t.role {
		case "user":
			turn, err := chatUserMessages(t)
			if err != nil {
				return "", err
			}
			messages = append(messages, turn...)
		case "assistant":
			assistant, err := chatAssistantMessage(t, gone)
			if err != nil {
				return "", err
			}
			messages = append(messages, assistant)
		case "system":
			text, err := blocksText(t.blocks, t.where)
			if err != nil {
				return "", err
			}
			system = append(system, text)
		}
	}
	if len(system) > 0 {
		messages = append([]string{set(`{"role":"system"}`, "content", strings.Join(system, "\n\n"))}, messages...)
	}
	return "[" + strings.Join(messages, ",") + "]", nil
}

// chatUserMessages turns a user turn into Chat Completions messages. Each tool
// result becomes a tool message of its own, ahead of the rest, since a tool
// message must directly follow the assistant message that made the call. A
// search result becomes text, its title and source with its content.
func chatUserMessages(t messagesTurn) ([]string, error) {
	var results, parts []string
	onlyText := true
	for _, b := range t.blocks {
		switch b.kind {
		case "text", "search_result":
			parts = append(parts, set(`{"type":"text"}`, "text", b.text))
		case "image":
			part, err := chatImagePart(b.source, t.where)
			if err != nil {
				return nil, err
			}
			parts, onlyText = append(parts, part), false
		case "document":
			part, err := chatDocumentPart(b.source, t.where)
			if err != nil {
				return nil, err
			}
			parts, onlyText = append(parts, part), onlyText && gjson.Get(part, "type").String() == "text"
		case "tool_result":
			result, err := chatToolResult(b)
			if err != nil {
				return nil, err
			}
			results = append(results, result)
		default:
			return nil, refuseMessages("%s: a Chat Completions model can't take a '%s' block", t.where, b.kind)
		}
	}
	if len(parts) == 0 {
		return results, nil
	}
	if !onlyText {
		return append(results, setRaw(`{"role":"user"}`, "content", "["+strings.Join(parts, ",")+"]")), nil
	}
	var text []string
	for _, part := range parts {
		text = append(text, gjson.Get(part, "text").String())
	}
	return append(results, set(`{"role":"user"}`, "content", strings.Join(text, "\n\n"))), nil
}

// chatToolResult turns a tool result into a tool message. A tool message has no
// error flag, so a failed call says so in its text for the model to read.
func chatToolResult(b messagesBlock) (string, error) {
	text, err := blocksText(b.content, b.where)
	if err != nil {
		return "", err
	}
	if b.isError {
		text = "Error: " + text
	}
	return set(set(`{"role":"tool"}`, "tool_call_id", b.toolUseID), "content", text), nil
}

// chatAssistantMessage turns an assistant turn into one message. Thinking
// blocks are left out, since a Chat Completions model has no use for them, and
// a server tool's call or result, which only the Claude API reads, becomes
// text the model can read.
func chatAssistantMessage(t messagesTurn, gone *droppedFields) (string, error) {
	var text, calls []string
	for _, b := range t.blocks {
		switch {
		case b.kind == "text":
			text = append(text, b.text)
		case b.kind == "tool_use":
			call := set(`{"type":"function","function":{}}`, "id", b.id)
			call = set(call, "function.name", b.name)
			arguments := b.input
			if arguments == "" {
				arguments = "{}"
			}
			calls = append(calls, set(call, "function.arguments", arguments))
		case b.kind == "thinking" || b.kind == "redacted_thinking":
			gone.add("thinking")
		case isServerToolBlock(b.kind):
			text = append(text, serverToolText(b))
		default:
			return "", refuseMessages("%s: a Chat Completions model can't take a '%s' block", t.where, b.kind)
		}
	}
	message := `{"role":"assistant"}`
	if len(text) > 0 {
		message = set(message, "content", strings.Join(text, ""))
	} else {
		message = setRaw(message, "content", "null")
	}
	if len(calls) > 0 {
		message = setRaw(message, "tool_calls", "["+strings.Join(calls, ",")+"]")
	}
	return message, nil
}

// chatTools turns client tools into function tools. The others are tool types
// only the Claude API defines. Claude's servers run some, such as web search,
// and the client runs others, such as bash, but none has a schema to send on.
func chatTools(tools gjson.Result, gone *droppedFields) (out, search string, err error) {
	var list []string
	for i, tool := range tools.Array() {
		if isClaudeWebSearch(tool) {
			search = "{}"
			if location := claudeSearchLocation(tool); location.IsObject() {
				nested := set("{}", "type", location.Get("type").String())
				location.ForEach(func(key, value gjson.Result) bool {
					if key.String() != "type" {
						nested = setRaw(nested, "approximate."+pathKey(key.String()), value.Raw)
					}
					return true
				})
				search = setRaw(search, "user_location", nested)
			}
			reportSearchSettings(tool, gone, "max_uses", "allowed_domains", "blocked_domains")
			continue
		}
		client, err := clientToolFunction(tool, i)
		if err != nil {
			return "", "", err
		}
		if client != "" {
			list = append(list, setRaw(`{"type":"function"}`, "function", client))
			continue
		}
		if kind := tool.Get("type").String(); kind != "" && kind != "custom" {
			return "", "", refuseMessages("tools[%d]: only the Claude API defines tool type '%s', and this model is served through Chat Completions; a tool sent to it needs its own 'input_schema'", i, kind)
		}
		function := set(`{}`, "name", tool.Get("name").String())
		if description := tool.Get("description"); description.Exists() {
			function = set(function, "description", description.String())
		}
		if schema := tool.Get("input_schema"); schema.Exists() {
			function = setRaw(function, "parameters", schema.Raw)
		}
		if strict := tool.Get("strict"); strict.Exists() {
			function = setRaw(function, "strict", strict.Raw)
		}
		list = append(list, setRaw(`{"type":"function"}`, "function", function))
	}
	if list != nil {
		out = "[" + strings.Join(list, ",") + "]"
	}
	return out, search, nil
}

// chatToolChoice returns the Chat Completions tool_choice and
// parallel_tool_calls for a Messages tool_choice.
func chatToolChoice(choice gjson.Result) (toolChoice, parallel string, err error) {
	if choice.Get("disable_parallel_tool_use").Bool() {
		parallel = "false"
	}
	switch kind := choice.Get("type").String(); kind {
	case "auto":
		return strconv.Quote("auto"), parallel, nil
	case "any":
		return strconv.Quote("required"), parallel, nil
	case "none":
		return strconv.Quote("none"), parallel, nil
	case "tool":
		return set(`{"type":"function","function":{}}`, "function.name", choice.Get("name").String()), parallel, nil
	default:
		return "", "", refuseMessages("tool_choice: '%s' has no Chat Completions equivalent", kind)
	}
}

// chatResponseFormat turns output_config.format into response_format.
func chatResponseFormat(format gjson.Result) (string, error) {
	if kind := format.Get("type").String(); kind != "json_schema" {
		return "", refuseMessages("output_config.format: '%s' has no Chat Completions equivalent", kind)
	}
	schema := set(`{"name":"response"}`, "strict", true)
	schema = setRaw(schema, "schema", format.Get("schema").Raw)
	return setRaw(`{"type":"json_schema"}`, "json_schema", schema), nil
}

// anthropicError turns a Chat Completions error body into Anthropic's shape.
// A body that isn't one becomes the message.
func anthropicError(status int, body []byte) []byte {
	message := gjson.GetBytes(body, "error.message").String()
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	return anthropicErrorBody(anthropicErrorType(status), message)
}

// stopReason maps a Chat Completions finish_reason to Messages'.
func stopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

// messagesUsage reports usage as Messages does, with cache reads apart from
// input_tokens, where Chat Completions counts them in prompt_tokens.
func messagesUsage(usage gjson.Result) map[string]any {
	cached := usage.Get("prompt_tokens_details.cached_tokens").Int()
	return map[string]any{
		"input_tokens":            usage.Get("prompt_tokens").Int() - cached,
		"output_tokens":           usage.Get("completion_tokens").Int(),
		"cache_read_input_tokens": cached,
	}
}

// chatReply converts what a Chat Completions backend answered back to Messages:
// a response or a stream, or an error in Anthropic's shape with the backend's
// status.
type chatReply struct {
	dropped droppedFields
}

func (r *chatReply) Dropped() []string { return r.dropped }

func (*chatReply) Result(result backend.ChatResult) backend.ChatResult {
	if result.Stream != nil {
		return backend.ChatResult{Stream: newEventStream(result.Stream)}
	}
	resp := *result.Response
	resp.ContentType = "application/json"
	if resp.Status/100 == 2 {
		resp.Body = messageOf(gjson.ParseBytes(resp.Body))
		resp.RetryAfter = ""
	} else {
		resp.Body = anthropicError(resp.Status, resp.Body)
	}
	return backend.ChatResult{Response: &resp}
}

// messageOf turns a chat completion into a Messages response. Reasoning becomes
// a thinking block marked as unsigned, ahead of the text and tool calls.
func messageOf(completion gjson.Result) []byte {
	choice := completion.Get("choices.0")
	message := choice.Get("message")
	b := newMessagesWriter(false)
	b.start("msg_"+completion.Get("id").String(), completion.Get("model").String(), nil)
	if reasoning := reasoningOf(message); reasoning != "" {
		b.think(reasoning)
		b.sign(backend.UnsignedThinking)
	}
	b.text(message.Get("content").String())
	citeAnnotations(b, message.Get("annotations").Array(), 0)
	for _, call := range message.Get("tool_calls").Array() {
		b.useTool(call.Get("id").String(), call.Get("function.name").String())
		b.input(call.Get("function.arguments").String())
	}
	b.finish(stopReason(choice.Get("finish_reason").String()), messagesUsage(completion.Get("usage")))
	return b.body()
}

// citeAnnotations adds URL citations to the open text block, each citing the
// text in its range, which partStart moves to the block's text. A citation
// whose text block has closed can't be added: Messages can't reopen a block.
func citeAnnotations(b *messagesWriter, annotations []gjson.Result, partStart int) {
	text := []rune(b.openText())
	for _, a := range annotations {
		c := a
		if a.Get("url_citation").Exists() {
			c = a.Get("url_citation")
		} else if a.Get("type").String() != "url_citation" {
			continue
		}
		start, end := int(c.Get("start_index").Int())+partStart, int(c.Get("end_index").Int())+partStart
		cited := ""
		if 0 <= start && start <= end && end <= len(text) {
			cited = string(text[start:end])
		}
		b.cite(c.Get("url").String(), c.Get("title").String(), cited)
	}
}

// eventStream turns Chat Completions chunks into Messages events, as messageOf
// does a complete response. Chunks have no blocks, so a delta of another kind
// than the open block opens one of its own:
//
//   - the first chunk, message_start;
//   - a reasoning delta, a thinking block, closed with the unsigned marker as
//     its signature;
//   - a content delta, a text block;
//   - a tool call's first delta, a tool_use block. Messages sends one block at
//     a time and chunks may interleave calls, so only the first call streams
//     its arguments, and the others are sent whole at the end. A block can't
//     be reopened, so reasoning and content that come while the first call's
//     block is open are held until the end too, sent after the call, since a
//     Chat stream never says that a call's arguments are complete.
//
// The stream's end gives the last block's stop, then message_delta, with the
// finish reason and usage, then message_stop.
type eventStream struct {
	chunks  backend.ChatStream
	reply   *messagesWriter
	started bool
	done    bool

	calls  []*toolCall
	held   []gjson.Result // deltas held while the first call streams
	finish string
	usage  gjson.Result
}

// toolCall is one call's arguments, as far as they have been sent.
type toolCall struct {
	chunkIndex int64
	id, name   string
	custom     bool // a custom tool's call, whose arguments are its input
	arguments  string
	opened     bool
}

func newEventStream(chunks backend.ChatStream) *eventStream {
	return &eventStream{chunks: chunks, reply: newMessagesWriter(true)}
}

func (s *eventStream) Next() ([]byte, error) {
	for {
		if event, ok := s.reply.next(); ok {
			return event, nil
		}
		if s.done {
			return nil, io.EOF
		}
		data, err := s.chunks.Next()
		var streamErr *backend.StreamError
		if errors.As(err, &streamErr) {
			return nil, &backend.StreamError{Data: anthropicError(0, streamErr.Data)}
		}
		if err == io.EOF {
			s.end()
			continue
		}
		if err != nil {
			return nil, err
		}
		s.translate(gjson.ParseBytes(data))
	}
}

func (s *eventStream) Close() error {
	return s.chunks.Close()
}

func (s *eventStream) start(chunk gjson.Result) {
	s.started = true
	s.reply.start("msg_"+chunk.Get("id").String(), chunk.Get("model").String(), map[string]any{"input_tokens": 0, "output_tokens": 0})
}

func (s *eventStream) translate(chunk gjson.Result) {
	if !s.started {
		s.start(chunk)
	}
	if usage := chunk.Get("usage"); usage.IsObject() {
		s.usage = usage
	}
	choice := chunk.Get("choices.0")
	delta := choice.Get("delta")
	if len(s.calls) > 0 && s.calls[0].opened && s.reply.isOpen("tool_use") {
		s.held = append(s.held, delta)
	} else {
		s.write(delta)
	}
	for _, call := range delta.Get("tool_calls").Array() {
		s.addToToolCall(call)
	}
	if finish := choice.Get("finish_reason").String(); finish != "" {
		s.finish = finish
	}
}

// write sends a delta's reasoning, content and citations.
func (s *eventStream) write(delta gjson.Result) {
	if reasoning := reasoningOf(delta); reasoning != "" {
		s.reply.think(reasoning)
		s.reply.sign(backend.UnsignedThinking)
	}
	s.reply.text(delta.Get("content").String())
	citeAnnotations(s.reply, delta.Get("annotations").Array(), 0)
}

// addToToolCall sends the first call's arguments as they come, and keeps the
// others' for the end.
func (s *eventStream) addToToolCall(delta gjson.Result) {
	index := delta.Get("index").Int()
	var call *toolCall
	for _, c := range s.calls {
		if c.chunkIndex == index {
			call = c
		}
	}
	if call == nil {
		call = &toolCall{chunkIndex: index}
		s.calls = append(s.calls, call)
	}
	if id := delta.Get("id").String(); id != "" {
		call.id = id
	}
	if name := delta.Get("function.name").String(); name != "" {
		call.name = name
	}
	arguments := delta.Get("function.arguments").String()
	if call == s.calls[0] && call.name != "" && (!call.opened || s.reply.isOpen("tool_use")) {
		s.openToolCall(call)
		s.reply.input(arguments)
		return
	}
	call.arguments += arguments
}

func (s *eventStream) openToolCall(call *toolCall) {
	if call.opened {
		return
	}
	call.opened = true
	s.reply.useTool(call.id, call.name)
	s.reply.input(call.arguments)
	call.arguments = ""
}

// end sends what was held while the first call streamed, then the calls
// still waiting, and ends the message.
func (s *eventStream) end() {
	s.done = true
	if !s.started {
		s.start(gjson.Result{})
	}
	for _, delta := range s.held {
		s.write(delta)
	}
	for _, call := range s.calls {
		s.openToolCall(call)
		s.reply.input(call.arguments)
	}
	s.reply.finish(stopReason(s.finish), messagesUsage(s.usage))
}
