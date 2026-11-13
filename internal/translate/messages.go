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

// This file is the Messages module: what the translators share about writing
// a Messages client's replies, and about reading its requests.

// messagesWriter assembles a Messages response, or the events that stream
// it, from a reply's parts in the order they arrive, so every translator
// answering a Messages client writes the same shapes. Streaming, each part is
// queued as an event as it comes; otherwise the blocks are collected and body
// returns the message.
//
// Messages streams one content block at a time, so a part of another kind
// than the open block closes it and opens one of its own kind. A thinking
// block takes its signature as it closes, since the signature arrives last.
type messagesWriter struct {
	streaming bool
	events    [][]byte
	id, model string
	blocks    []map[string]any
	open      map[string]any // the open block, also the last of blocks
	signature string         // the open thinking block's, sent as it closes
	inputs    []byte         // the open tool_use block's input, as streamed
	stop      string
	usage     map[string]any
}

func newMessagesWriter(streaming bool) *messagesWriter {
	return &messagesWriter{streaming: streaming}
}

// start names the message; streaming, it sends message_start, with no
// content and the usage known so far.
func (b *messagesWriter) start(id, model string, usage map[string]any) {
	b.id, b.model = id, model
	b.event(map[string]any{"type": "message_start", "message": map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []any{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage":         usage,
	}})
}

func (b *messagesWriter) event(event map[string]any) {
	if !b.streaming {
		return
	}
	data, _ := json.Marshal(event)
	b.events = append(b.events, data)
}

// index is the open block's index.
func (b *messagesWriter) index() int {
	return len(b.blocks) - 1
}

// openBlock opens a block, closing the one before; streaming, it sends
// content_block_start with the block as it starts.
func (b *messagesWriter) openBlock(block map[string]any) {
	b.closeBlock()
	b.blocks = append(b.blocks, block)
	b.open = block
	started := map[string]any{}
	for k, v := range block {
		started[k] = v
	}
	switch block["type"] {
	case "text":
		started["text"] = ""
	case "thinking":
		started["thinking"], started["signature"] = "", ""
	case "tool_use":
		started["input"] = map[string]any{}
	}
	b.event(map[string]any{"type": "content_block_start", "index": b.index(), "content_block": started})
}

func (b *messagesWriter) delta(delta map[string]any) {
	b.event(map[string]any{"type": "content_block_delta", "index": b.index(), "delta": delta})
}

// closeBlock closes the open block, a thinking block with its signature.
func (b *messagesWriter) closeBlock() {
	block := b.open
	if block == nil {
		return
	}
	b.open = nil
	switch block["type"] {
	case "thinking":
		block["signature"] = b.signature
		b.delta(map[string]any{"type": "signature_delta", "signature": b.signature})
		b.signature = ""
	case "tool_use":
		block["input"] = inputOf(string(b.inputs))
		b.inputs = nil
	}
	b.event(map[string]any{"type": "content_block_stop", "index": b.index()})
}

func (b *messagesWriter) isOpen(kind string) bool {
	return b.open != nil && b.open["type"] == kind
}

// text adds to the open text block, opening one if none is.
func (b *messagesWriter) text(delta string) {
	if delta == "" {
		return
	}
	if !b.isOpen("text") {
		b.openBlock(map[string]any{"type": "text", "text": ""})
	}
	b.open["text"] = b.open["text"].(string) + delta
	b.delta(map[string]any{"type": "text_delta", "text": delta})
}

// think adds to the open thinking block, opening one if none is.
func (b *messagesWriter) think(delta string) {
	if !b.isOpen("thinking") {
		b.openBlock(map[string]any{"type": "thinking", "thinking": ""})
	}
	if delta == "" {
		return
	}
	b.open["thinking"] = b.open["thinking"].(string) + delta
	b.delta(map[string]any{"type": "thinking_delta", "thinking": delta})
}

// sign sets the open thinking block's signature, opening a thinking block if
// none is open, for thinking whose text the client doesn't see.
func (b *messagesWriter) sign(signature string) {
	if !b.isOpen("thinking") {
		b.think("")
	}
	b.signature = signature
}

// cite adds a web citation to the open text block. Only Claude can write a
// citation's encrypted_index, so the gateway's carries its marker, by which
// the anthropic adapter removes it before Claude sees the history.
func (b *messagesWriter) cite(url, title, citedText string) {
	if !b.isOpen("text") {
		return
	}
	citation := map[string]any{"type": "web_search_result_location", "url": url, "title": title, "cited_text": citedText, "encrypted_index": backend.GatewayCitation}
	citations, _ := b.open["citations"].([]any)
	b.open["citations"] = append(citations, citation)
	b.delta(map[string]any{"type": "citations_delta", "citation": citation})
}

// block adds a complete block, such as a server tool's call or result, sent
// started and stopped at once.
func (b *messagesWriter) block(block map[string]any) {
	b.closeBlock()
	b.blocks = append(b.blocks, block)
	b.event(map[string]any{"type": "content_block_start", "index": b.index(), "content_block": block})
	b.event(map[string]any{"type": "content_block_stop", "index": b.index()})
}

// openText is the open text block's text so far, "" if none is open.
func (b *messagesWriter) openText() string {
	if !b.isOpen("text") {
		return ""
	}
	return b.open["text"].(string)
}

// useTool opens a tool_use block.
func (b *messagesWriter) useTool(id, name string) {
	b.openBlock(map[string]any{"type": "tool_use", "id": id, "name": name})
}

// input adds to the open tool_use block's input, as JSON text.
func (b *messagesWriter) input(delta string) {
	if !b.isOpen("tool_use") || delta == "" {
		return
	}
	b.inputs = append(b.inputs, delta...)
	b.delta(map[string]any{"type": "input_json_delta", "partial_json": delta})
}

// finish closes the open block and ends the message with a stop reason and
// usage; streaming, it sends message_delta and message_stop.
func (b *messagesWriter) finish(stop string, usage map[string]any) {
	b.closeBlock()
	b.stop, b.usage = stop, usage
	b.event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": usage})
	b.event(map[string]any{"type": "message_stop"})
}

// next takes the next queued event, if there is one.
func (b *messagesWriter) next() ([]byte, bool) {
	if len(b.events) == 0 {
		return nil, false
	}
	event := b.events[0]
	b.events = b.events[1:]
	return event, true
}

// body is the message the parts make.
func (b *messagesWriter) body() []byte {
	content := b.blocks
	if content == nil {
		content = []map[string]any{}
	}
	data, _ := json.Marshal(map[string]any{
		"id":            b.id,
		"type":          "message",
		"role":          "assistant",
		"model":         b.model,
		"content":       content,
		"stop_reason":   b.stop,
		"stop_sequence": nil,
		"usage":         b.usage,
	})
	return data
}

// messagesTurn is one turn of a Messages request, read for translation.
type messagesTurn struct {
	where  string // the turn's place, for errors: "messages[2]"
	role   string // user or assistant
	blocks []messagesBlock
}

// messagesBlock is one content block. A string content is one text block.
type messagesBlock struct {
	where string
	kind  string // the block's type
	raw   gjson.Result
	text  string // a text block's text, a search result's text
	// citations are a text block's.
	citations []gjson.Result
	// source is an image's or document's source.
	source gjson.Result
	// id, name and input are a tool_use's, or a server tool's call.
	id, name string
	input    string
	// toolUseID, isError and content are a tool result's.
	toolUseID string
	isError   bool
	content   []messagesBlock
}

// readMessagesTurns reads a request's turns.
func readMessagesTurns(req gjson.Result) ([]messagesTurn, error) {
	var turns []messagesTurn
	for i, msg := range req.Get("messages").Array() {
		t := messagesTurn{where: "messages[" + strconv.Itoa(i) + "]", role: msg.Get("role").String()}
		if t.role != "user" && t.role != "assistant" && t.role != "system" {
			return nil, refuseMessages("%s: role '%s' is not a Messages role", t.where, t.role)
		}
		t.blocks = readMessagesBlocks(msg.Get("content"), t.where)
		turns = append(turns, t)
	}
	return turns, nil
}

// readMessagesBlocks reads a content array, or a string as one text block.
func readMessagesBlocks(content gjson.Result, where string) []messagesBlock {
	if content.Type == gjson.String {
		return []messagesBlock{{where: where, kind: "text", text: content.String(), raw: content}}
	}
	var blocks []messagesBlock
	for j, block := range content.Array() {
		b := messagesBlock{where: where + ".content[" + strconv.Itoa(j) + "]", kind: block.Get("type").String(), raw: block, source: block.Get("source")}
		switch b.kind {
		case "text":
			b.text, b.citations = block.Get("text").String(), block.Get("citations").Array()
		case "search_result":
			b.text = searchResultText(block)
		case "tool_use", "server_tool_use", "mcp_tool_use":
			b.id, b.name, b.input = block.Get("id").String(), block.Get("name").String(), block.Get("input").Raw
		case "tool_result":
			b.toolUseID, b.isError = block.Get("tool_use_id").String(), block.Get("is_error").Bool()
			b.content = readMessagesBlocks(block.Get("content"), b.where)
		}
		blocks = append(blocks, b)
	}
	return blocks
}

// searchResultText is a search result block as text a model without search
// results can read: its title, source and the text it holds.
func searchResultText(block gjson.Result) string {
	var texts []string
	for _, part := range block.Get("content").Array() {
		texts = append(texts, part.Get("text").String())
	}
	return block.Get("title").String() + " (" + block.Get("source").String() + ")\n" + strings.Join(texts, "\n")
}

// serverToolText is a server tool's call or result from a conversation's
// history as text, for a model that can't take the block itself: what was
// called with what input, or what came back.
func serverToolText(block messagesBlock) string {
	switch block.kind {
	case "server_tool_use", "mcp_tool_use":
		return "[" + block.name + " called with " + block.input + "]"
	case "web_search_tool_result":
		var results []string
		for _, r := range block.raw.Get("content").Array() {
			if url := r.Get("url").String(); url != "" {
				results = append(results, r.Get("title").String()+" <"+url+">")
			}
		}
		return "[web search results: " + strings.Join(results, "; ") + "]"
	}
	if content := block.raw.Get("content"); content.Exists() {
		return "[" + block.kind + ": " + content.Raw + "]"
	}
	return "[" + block.kind + "]"
}

// isServerToolBlock reports whether a block is a server tool's call or
// result, which only the Claude API reads.
func isServerToolBlock(kind string) bool {
	switch kind {
	case "server_tool_use", "mcp_tool_use", "mcp_tool_result", "web_search_tool_result", "web_fetch_tool_result", "code_execution_tool_result", "bash_code_execution_tool_result", "text_editor_code_execution_tool_result", "tool_search_tool_result", "advisor_tool_result", "container_upload":
		return true
	}
	return false
}

// readMessagesSystem joins the system prompt's text.
func readMessagesSystem(req gjson.Result) (string, bool, error) {
	system := req.Get("system")
	if !system.Exists() || system.Type == gjson.Null {
		return "", false, nil
	}
	text, err := textOf(system, "system")
	return text, true, err
}

// droppedMessagesFieldsEverywhere have no counterpart in either OpenAI API
// and only tune how Claude's service works, so leaving them out changes
// nothing a reply depends on.
var droppedMessagesFieldsEverywhere = map[string]bool{
	"cache_control":      true,
	"context_management": true,
	"diagnostics":        true,
	"inference_geo":      true,
	"service_tier":       true, // same name, other values
	"speed":              true,
	"user_profile_id":    true,
	"workspace_id":       true,
}

// reportDroppedBlocks reports what translating a Messages conversation leaves
// out of its blocks: cache markers, eager input streaming, document
// citations, and server tool blocks, which become text. A quoted key can only
// match a real key, since quotes inside a JSON string are escaped.
func reportDroppedBlocks(body []byte, turns []messagesTurn, gone *droppedFields) {
	if bytesContain(body, "cache_control") {
		gone.add("cache_control")
	}
	if bytesContain(body, "eager_input_streaming") {
		gone.add("eager_input_streaming")
	}
	for _, t := range turns {
		for _, b := range t.blocks {
			switch {
			case b.kind == "document" && b.raw.Get("citations.enabled").Bool():
				gone.add("citations")
			case isServerToolBlock(b.kind):
				gone.add(b.kind)
			}
		}
	}
}

// refuseMessages is a Messages request a Chat Completions backend can't serve
// as the client means it. The client gets it as a 400 in Anthropic's shape.
func refuseMessages(format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	return &Refusal{Response: &backend.ChatResponse{Status: http.StatusBadRequest, ContentType: "application/json", Body: anthropicErrorBody("invalid_request_error", message)}, message: message}
}

// textOf joins a string or an array of text blocks.
func textOf(content gjson.Result, where string) (string, error) {
	return blocksText(readMessagesBlocks(content, where), where)
}

// blocksText joins text blocks, refusing any other kind.
func blocksText(blocks []messagesBlock, where string) (string, error) {
	var parts []string
	for _, b := range blocks {
		if b.kind != "text" {
			return "", refuseMessages("%s: a Chat Completions model takes only text here, not a '%s' block", where, b.kind)
		}
		parts = append(parts, b.text)
	}
	return strings.Join(parts, "\n\n"), nil
}

func chatImagePart(source gjson.Result, where string) (string, error) {
	var url string
	switch kind := source.Get("type").String(); kind {
	case "base64":
		url = "data:" + source.Get("media_type").String() + ";base64," + source.Get("data").String()
	case "url":
		url = source.Get("url").String()
	default:
		return "", refuseMessages("%s: a Chat Completions model takes an image by URL or base64, not '%s'", where, kind)
	}
	return set(`{"type":"image_url","image_url":{}}`, "image_url.url", url), nil
}

func chatDocumentPart(source gjson.Result, where string) (string, error) {
	switch kind := source.Get("type").String(); {
	case kind == "text":
		return set(`{"type":"text"}`, "text", source.Get("data").String()), nil
	case kind == "base64" && source.Get("media_type").String() == "application/pdf":
		return set(`{"type":"file","file":{"filename":"document.pdf"}}`, "file.file_data", "data:application/pdf;base64,"+source.Get("data").String()), nil
	}
	return "", refuseMessages("%s: a Chat Completions model takes a document only as text or an inline PDF", where)
}

func anthropicErrorBody(errType, message string) []byte {
	data, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": errType, "message": message}})
	return data
}

// anthropicErrorType is the error type Anthropic's API gives a status, which
// Claude's clients use to decide whether to retry.
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	return "api_error"
}

// MessagesError returns an error body in Anthropic's shape: one already in it
// unchanged, and any other as anthropicError converts it.
func MessagesError(status int, body []byte) []byte {
	if gjson.GetBytes(body, "type").String() == "error" {
		return body
	}
	return anthropicError(status, body)
}

// inputOf parses a tool call's arguments, which a model can get wrong. Messages
// requires an object, so anything else is kept as text the client can see.
func inputOf(arguments string) json.RawMessage {
	if gjson.Valid(arguments) && gjson.Parse(arguments).IsObject() {
		return json.RawMessage(arguments)
	}
	if strings.TrimSpace(arguments) == "" {
		return json.RawMessage("{}")
	}
	data, _ := json.Marshal(map[string]string{"_invalidArguments": arguments})
	return data
}

// imageBlock takes an image by URL, or inline from a base64 data URL.
func imageBlock(url, where string) (string, error) {
	if !strings.HasPrefix(url, "data:") {
		return set(`{"type":"image","source":{"type":"url"}}`, "source.url", url), nil
	}
	mediaType, data, ok := base64DataURL(url)
	if !ok {
		return "", refuse("%s: an image data URL must be base64", where)
	}
	block := set(`{"type":"image","source":{"type":"base64"}}`, "source.media_type", mediaType)
	return set(block, "source.data", data), nil
}

// documentBlock takes a file part's inline PDF; a file_id names a file at
// OpenAI, which the Claude API can't reach.
func documentBlock(part chatPart, where string) (string, error) {
	mediaType, data, ok := base64DataURL(part.fileData)
	if !ok || mediaType != "application/pdf" {
		return "", refuse("%s: the Claude API takes a file part only as an inline PDF in file_data", where)
	}
	return set(`{"type":"document","source":{"type":"base64","media_type":"application/pdf"}}`, "source.data", data), nil
}

// isClaudeWebSearch reports whether a Messages tool is Claude's web search,
// in any of its versions (web_search_20250305, 20260209, 20260318 seen).
func isClaudeWebSearch(tool gjson.Result) bool {
	return strings.HasPrefix(tool.Get("type").String(), "web_search_")
}

// claudeSearchLocation is a web search tool's user location, which Claude
// and Responses both spell flat: type, city, region, country, timezone.
func claudeSearchLocation(tool gjson.Result) gjson.Result {
	return tool.Get("user_location")
}

// reportSearchSettings reports a Claude web search tool's settings that the
// target has no counterpart for.
func reportSearchSettings(tool gjson.Result, gone *droppedFields, unmatched ...string) {
	for _, key := range unmatched {
		if tool.Get(key).Exists() {
			gone.add("tools." + key)
		}
	}
}

// editorToolType is the text editor of Claude 4 and later, the one a backend
// without it is given as a function: editorDescription and editorSchema
// describe its commands as the Claude API documents them, and gpt-5.4-nano
// used each of them through that function as Claude does (2026-11-10).
const editorToolType = "text_editor_20250728"

const editorDescription = "View, create and edit files. Commands:\n" +
	"- view: show a file with line numbers, or list a directory; view_range [start, end] limits a file to those lines (end -1 for the end).\n" +
	"- create: write file_text to a new file at path.\n" +
	"- str_replace: replace old_str, which must occur exactly once in the file, with new_str.\n" +
	"- insert: insert insert_text after line insert_line (0 for the start of the file)."

const editorSchema = `{"type":"object","properties":{"command":{"type":"string","enum":["view","create","str_replace","insert"]},"path":{"type":"string"},"view_range":{"type":"array","items":{"type":"integer"}},"file_text":{"type":"string"},"old_str":{"type":"string"},"new_str":{"type":"string"},"insert_line":{"type":"integer"},"insert_text":{"type":"string"}},"required":["command","path"]}`

// bashDescription and bashSchema describe Claude's bash tool, as the Claude
// API documents it, for a backend without it.
const bashDescription = "Run a command in a bash shell that keeps its state between calls, and return its output. restart: true starts a new shell."

const bashSchema = `{"type":"object","properties":{"command":{"type":"string"},"restart":{"type":"boolean"}}}`

// clientToolFunction is a tool the Claude API defines and the client runs,
// bash or the text editor, as a function of its name with its commands, for
// a backend without it; "" for any other tool. An earlier text editor is
// refused.
func clientToolFunction(tool gjson.Result, i int) (string, error) {
	kind := tool.Get("type").String()
	var description, schema string
	switch {
	case strings.HasPrefix(kind, "bash_"):
		description, schema = bashDescription, bashSchema
	case kind == editorToolType:
		description, schema = editorDescription, editorSchema
	case strings.HasPrefix(kind, "text_editor_"):
		return "", refuseMessages("tools[%d]: '%s' is the text editor of Claude models before Claude 4; use '%s'", i, kind, editorToolType)
	default:
		return "", nil
	}
	function := set(set(`{}`, "name", tool.Get("name").String()), "description", description)
	return setRaw(function, "parameters", schema), nil
}
