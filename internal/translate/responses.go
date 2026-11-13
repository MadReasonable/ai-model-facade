package translate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// This file holds what both translators from Responses share: the request
// fields neither backend protocol has, and the builder that assembles a
// Responses response, and the events that stream it, from a reply's parts.
// The shapes are those of OpenAI's API (verified 2026-11-09 on gpt-5.4-nano)
// and its Python SDK's types.

// responsesRefused are request fields that ask for what only OpenAI's servers
// do.
var responsesRefused = map[string]string{
	"background":   "background mode needs work the gateway owns after the request; send the request without it",
	"conversation": "the gateway keeps no Conversations API objects; chain turns with previous_response_id",
	"prompt":       "a prompt template is kept at OpenAI",
}

// responsesDropped are request fields with no counterpart in either other
// API that only tune OpenAI's own services or what it returns beyond the
// response, so leaving them out changes nothing the reply depends on. store
// and previous_response_id are the gateway's (history), so they aren't
// reported.
var responsesDropped = map[string]bool{
	"access_programs":    true,
	"client_metadata":    true,
	"context_management": true,
	"image":              true,
	"max_tool_calls":     true,
	"stream_options":     true,
	"truncation":         true,
}

// responsesOpenAIFields are request fields Chat Completions has too, with the
// same meaning, and Messages hasn't: passed on toward Chat Completions,
// dropped toward Messages.
var responsesOpenAIFields = map[string]bool{
	"metadata":               true,
	"moderation":             true,
	"prompt_cache_key":       true,
	"prompt_cache_options":   true,
	"prompt_cache_retention": true,
	"service_tier":           true,
}

// wantsLogprobs reports whether include asks for the log probabilities of the
// output's tokens.
func wantsLogprobs(req gjson.Result) bool {
	for _, v := range req.Get("include").Array() {
		if v.String() == "message.output_text.logprobs" {
			return true
		}
	}
	return false
}

// responsesServerItems are input items that record a call of a tool OpenAI's
// servers ran, which only OpenAI reads; toward another API each becomes text
// the model can read.
var responsesServerItems = map[string]bool{
	"code_interpreter_call": true,
	"file_search_call":      true,
	"image_generation_call": true,
	"mcp_call":              true,
	"mcp_list_tools":        true,
	"mcp_approval_request":  true,
	"mcp_approval_response": true,
	"web_search_call":       true,
	"tool_search_call":      true,
	"compaction_trigger":    true,
	"program":               true,
	"program_output":        true,
	"additional_tools":      true,
}

// serverItemText is a server tool's record from a conversation's history as
// text: what was searched, run or called, and what came back.
func serverItemText(item gjson.Result) string {
	kind := item.Get("type").String()
	switch kind {
	case "web_search_call":
		return "[web search: " + item.Get("action.query").String() + "]"
	case "code_interpreter_call":
		var outputs []string
		for _, o := range item.Get("outputs").Array() {
			outputs = append(outputs, o.Get("logs").String())
		}
		return "[code run: " + item.Get("code").String() + " -> " + strings.Join(outputs, "\n") + "]"
	case "mcp_call":
		return "[" + item.Get("name").String() + " called with " + item.Get("arguments").String() + " -> " + item.Get("output").String() + "]"
	}
	return "[" + kind + "]"
}

// allowedNames is the names of the tools a Responses tool choice allows, nil
// for a choice that doesn't list them.
func allowedNames(choice gjson.Result) []string {
	if choice.Get("type").String() != "allowed_tools" {
		return nil
	}
	names := []string{}
	for _, tool := range choice.Get("tools").Array() {
		names = append(names, tool.Get("name").String())
	}
	return names
}

// refuseResponsesField refuses a top-level field responsesRefused names.
func refuseResponsesField(key, protocol string) error {
	return refuse("'%s' can't be served: %s, and this model is served through %s", key, responsesRefused[key], protocol)
}

// responsesItemText returns the text of a message item's content, a string or
// parts of text, for a role that takes only text.
func responsesItemText(content gjson.Result, where string) (string, error) {
	if content.Type == gjson.String {
		return content.String(), nil
	}
	var texts []string
	for _, part := range content.Array() {
		switch kind := part.Get("type").String(); kind {
		case "input_text", "output_text":
			texts = append(texts, part.Get("text").String())
		case "refusal":
			texts = append(texts, part.Get("refusal").String())
		default:
			return "", refuse("%s: a '%s' part can't stand here; only text can", where, kind)
		}
	}
	return strings.Join(texts, "\n\n"), nil
}

// newID returns an ID with prefix, unique enough to name a response or item.
func newID(prefix string) string {
	b := make([]byte, 24)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// responseBuilder assembles a Responses response from a reply's parts in the
// order they arrive. Streaming, it also queues the events that send each part
// as it comes, numbered as Responses numbers them.
//
// Output items follow one another: a part of another kind than the open item
// closes it. A reasoning item holds reasoning_text parts, a model's own
// reasoning, or summary_text, a summary of it such as Claude's; a message
// holds output_text and refusal parts.
type responseBuilder struct {
	request   gjson.Result
	streaming bool
	id, model string
	created   int64
	started   bool
	events    [][]byte
	sequence  int
	output    []*outputItem
	open      *outputItem
}

type outputItem struct {
	kind      string         // reasoning, message, function_call or record
	raw       map[string]any // a record's object
	id        string
	index     int
	content   []*contentPart // message content, or reasoning_text
	summary   []*contentPart // summary_text
	callID    string
	name      string
	namespace string // the namespace holding a called tool, if any
	arguments strings.Builder
	encrypted string
}

type contentPart struct {
	kind        string // output_text, refusal, reasoning_text or summary_text
	text        strings.Builder
	length      int // characters of text, which an annotation's range counts in
	annotations []any
	logprobs    []json.RawMessage
}

func newResponseBuilder(request gjson.Result, streaming bool) *responseBuilder {
	return &responseBuilder{request: request, streaming: streaming, id: newID("resp_"), created: time.Now().Unix()}
}

// start sends response.created and response.in_progress, once.
func (b *responseBuilder) start(model string) {
	if b.started {
		return
	}
	b.started = true
	if model != "" {
		b.model = model
	}
	response := b.response("in_progress", "", nil)
	b.event("response.created", map[string]any{"response": response})
	b.event("response.in_progress", map[string]any{"response": response})
}

// event queues a streaming event with its type and number.
func (b *responseBuilder) event(kind string, fields map[string]any) {
	if !b.streaming {
		return
	}
	fields["type"] = kind
	fields["sequence_number"] = b.sequence
	b.sequence++
	data, _ := json.Marshal(fields)
	b.events = append(b.events, data)
}

func (b *responseBuilder) openItem(kind string, fields func(*outputItem)) *outputItem {
	if b.open != nil && b.open.kind == kind && kind != "function_call" && kind != "custom_tool_call" {
		return b.open
	}
	b.closeItem()
	b.start("")
	prefixes := map[string]string{"reasoning": "rs_", "message": "msg_", "function_call": "fc_", "custom_tool_call": "ctc_"}
	item := &outputItem{kind: kind, id: newID(prefixes[kind]), index: len(b.output)}
	if fields != nil {
		fields(item)
	}
	b.output = append(b.output, item)
	b.open = item
	b.event("response.output_item.added", map[string]any{"output_index": item.index, "item": item.object("in_progress")})
	return item
}

// text adds a delta to the open message, in a part of this kind: output_text
// or refusal.
func (b *responseBuilder) text(kind, delta string) {
	b.textWithLogprobs(kind, delta, nil)
}

// textWithLogprobs is text with the log probabilities of its tokens, which a
// text part carries as Responses reports them (verified 2026-11-09, the same
// shape as Chat Completions').
func (b *responseBuilder) textWithLogprobs(kind, delta string, logprobs []json.RawMessage) {
	if delta == "" {
		return
	}
	item := b.openItem("message", nil)
	part, index := b.part(item, &item.content, kind, "")
	part.text.WriteString(delta)
	part.length += utf8.RuneCountInString(delta)
	part.logprobs = append(part.logprobs, logprobs...)
	fields := map[string]any{"item_id": item.id, "output_index": item.index, "content_index": index, "delta": delta}
	if kind == "output_text" {
		fields["logprobs"] = logprobs
		if logprobs == nil {
			fields["logprobs"] = []any{}
		}
		b.event("response.output_text.delta", fields)
		return
	}
	b.event("response.refusal.delta", fields)
}

// textLength is how many characters the open text part holds, 0 if none is
// open, for an annotation whose range starts after text written so far.
func (b *responseBuilder) textLength() int {
	if b.open == nil || b.open.kind != "message" || len(b.open.content) == 0 {
		return 0
	}
	if part := b.open.content[len(b.open.content)-1]; part.kind == "output_text" {
		return part.length
	}
	return 0
}

// annotate adds a URL citation to the open text part, its range counted in
// that part's characters, as Responses counts it.
func (b *responseBuilder) annotate(url, title string, start, end int) {
	if b.open == nil || b.open.kind != "message" || len(b.open.content) == 0 {
		return
	}
	index := len(b.open.content) - 1
	part := b.open.content[index]
	annotation := map[string]any{"type": "url_citation", "url": url, "title": title, "start_index": start, "end_index": end}
	part.annotations = append(part.annotations, annotation)
	b.event("response.output_text.annotation.added", map[string]any{"item_id": b.open.id, "output_index": b.open.index, "content_index": index, "annotation_index": len(part.annotations) - 1, "annotation": annotation})
}

// record adds a complete output item that records a server tool's call, such
// as a web search, sent added and done at once.
func (b *responseBuilder) record(object map[string]any) {
	b.closeItem()
	b.start("")
	item := &outputItem{kind: "record", index: len(b.output), raw: object}
	b.output = append(b.output, item)
	b.event("response.output_item.added", map[string]any{"output_index": item.index, "item": object})
	b.event("response.output_item.done", map[string]any{"output_index": item.index, "item": object})
}

// reasoning adds a delta to the open reasoning item, as reasoning_text or
// summary_text.
func (b *responseBuilder) reasoning(kind, delta string) {
	if delta == "" {
		return
	}
	item := b.openItem("reasoning", nil)
	if kind == "summary_text" {
		part, index := b.part(item, &item.summary, kind, "summary")
		part.text.WriteString(delta)
		b.event("response.reasoning_summary_text.delta", map[string]any{"item_id": item.id, "output_index": item.index, "summary_index": index, "delta": delta})
		return
	}
	part, index := b.part(item, &item.content, kind, "")
	part.text.WriteString(delta)
	b.event("response.reasoning_text.delta", map[string]any{"item_id": item.id, "output_index": item.index, "content_index": index, "delta": delta})
}

// part returns the last part of parts if it is of this kind, else closes it
// and opens one. A summary's parts have their own events.
func (b *responseBuilder) part(item *outputItem, parts *[]*contentPart, kind, summary string) (*contentPart, int) {
	if n := len(*parts); n > 0 && (*parts)[n-1].kind == kind {
		return (*parts)[n-1], n - 1
	}
	b.closePart(item, *parts, summary)
	part := &contentPart{kind: kind}
	*parts = append(*parts, part)
	index := len(*parts) - 1
	if summary != "" {
		b.event("response.reasoning_summary_part.added", map[string]any{"item_id": item.id, "output_index": item.index, "summary_index": index, "part": part.object()})
	} else {
		b.event("response.content_part.added", map[string]any{"item_id": item.id, "output_index": item.index, "content_index": index, "part": part.object()})
	}
	return part, index
}

// closePart sends the done events of the last part.
func (b *responseBuilder) closePart(item *outputItem, parts []*contentPart, summary string) {
	if len(parts) == 0 {
		return
	}
	index := len(parts) - 1
	part := parts[index]
	text := part.text.String()
	switch {
	case summary != "":
		b.event("response.reasoning_summary_text.done", map[string]any{"item_id": item.id, "output_index": item.index, "summary_index": index, "text": text})
		b.event("response.reasoning_summary_part.done", map[string]any{"item_id": item.id, "output_index": item.index, "summary_index": index, "part": part.object()})
		return
	case part.kind == "output_text":
		b.event("response.output_text.done", map[string]any{"item_id": item.id, "output_index": item.index, "content_index": index, "text": text, "logprobs": []any{}})
	case part.kind == "refusal":
		b.event("response.refusal.done", map[string]any{"item_id": item.id, "output_index": item.index, "content_index": index, "refusal": text})
	case part.kind == "reasoning_text":
		b.event("response.reasoning_text.done", map[string]any{"item_id": item.id, "output_index": item.index, "content_index": index, "text": text})
	}
	b.event("response.content_part.done", map[string]any{"item_id": item.id, "output_index": item.index, "content_index": index, "part": part.object()})
}

// call opens a function call.
func (b *responseBuilder) call(callID, name string) {
	namespace, own := b.namespaceOf(name)
	b.openItem("function_call", func(item *outputItem) { item.callID, item.name, item.namespace = callID, own, namespace })
}

// callCustom opens a custom tool's call, whose input is free text.
func (b *responseBuilder) callCustom(callID, name string) {
	namespace, own := b.namespaceOf(name)
	b.openItem("custom_tool_call", func(item *outputItem) { item.callID, item.name, item.namespace = callID, own, namespace })
}

// arguments adds a delta to the open function call's arguments.
func (b *responseBuilder) arguments(delta string) {
	item := b.open
	if item == nil || (item.kind != "function_call" && item.kind != "custom_tool_call") || delta == "" {
		return
	}
	item.arguments.WriteString(delta)
	if item.kind == "custom_tool_call" {
		b.event("response.custom_tool_call_input.delta", map[string]any{"item_id": item.id, "output_index": item.index, "delta": delta})
		return
	}
	b.event("response.function_call_arguments.delta", map[string]any{"item_id": item.id, "output_index": item.index, "delta": delta})
}

// encrypt sets the open reasoning item's encrypted_content, opening one if
// none is open, for reasoning whose text the client doesn't see.
func (b *responseBuilder) encrypt(content string) {
	b.openItem("reasoning", nil).encrypted = content
}

// closeItem sends the open item's done events.
func (b *responseBuilder) closeItem() {
	item := b.open
	if item == nil {
		return
	}
	b.open = nil
	switch item.kind {
	case "reasoning":
		b.closePart(item, item.summary, "summary")
		b.closePart(item, item.content, "")
	case "message":
		b.closePart(item, item.content, "")
	case "function_call":
		b.event("response.function_call_arguments.done", map[string]any{"item_id": item.id, "output_index": item.index, "arguments": item.arguments.String()})
	case "custom_tool_call":
		b.event("response.custom_tool_call_input.done", map[string]any{"item_id": item.id, "output_index": item.index, "input": item.arguments.String()})
	}
	b.event("response.output_item.done", map[string]any{"output_index": item.index, "item": item.object("completed")})
}

// finish closes the open item and returns the response, sending it as
// response.completed, or response.incomplete with a reason.
func (b *responseBuilder) finish(incomplete string, usage map[string]any) []byte {
	b.start("")
	b.closeItem()
	status, kind := "completed", "response.completed"
	if incomplete != "" {
		status, kind = "incomplete", "response.incomplete"
	}
	response := b.response(status, incomplete, usage)
	b.event(kind, map[string]any{"response": response})
	data, _ := json.Marshal(response)
	return data
}

// response is the response object with the output so far. It repeats the
// request's settings, with Responses' defaults for those it left out, as
// OpenAI's does; clients such as OpenAI's SDKs require tools, tool_choice and
// parallel_tool_calls.
func (b *responseBuilder) response(status, incomplete string, usage map[string]any) map[string]any {
	output := []any{}
	for _, item := range b.output {
		output = append(output, item.object(status))
	}
	setting := func(key string, fallback any) any {
		if v := b.request.Get(key); v.Exists() && v.Type != gjson.Null {
			return json.RawMessage(v.Raw)
		}
		return fallback
	}
	var details any
	if incomplete != "" {
		details = map[string]any{"reason": incomplete}
	}
	model := b.model
	if model == "" {
		model = b.request.Get("model").String()
	}
	return map[string]any{
		"id":                   b.id,
		"object":               "response",
		"created_at":           b.created,
		"status":               status,
		"error":                nil,
		"incomplete_details":   details,
		"instructions":         setting("instructions", nil),
		"max_output_tokens":    setting("max_output_tokens", nil),
		"model":                model,
		"output":               output,
		"parallel_tool_calls":  setting("parallel_tool_calls", true),
		"previous_response_id": nil,
		"reasoning":            setting("reasoning", map[string]any{"effort": nil, "summary": nil}),
		"store":                setting("store", true),
		"temperature":          setting("temperature", nil),
		"text":                 setting("text", map[string]any{"format": map[string]any{"type": "text"}}),
		"tool_choice":          setting("tool_choice", "auto"),
		"tools":                setting("tools", []any{}),
		"top_p":                setting("top_p", nil),
		"truncation":           "disabled",
		"usage":                usage,
		"metadata":             setting("metadata", map[string]any{}),
	}
}

// object is the item as a Responses output item, with this status once done.
func (item *outputItem) object(status string) map[string]any {
	if item.kind == "record" {
		return item.raw
	}
	if status == "in_progress" {
		// a newly added item has no parts yet
		switch item.kind {
		case "reasoning":
			return map[string]any{"id": item.id, "type": "reasoning", "summary": []any{}}
		case "message":
			return map[string]any{"id": item.id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
		}
		if item.kind == "custom_tool_call" {
			return item.withNamespace(map[string]any{"id": item.id, "type": "custom_tool_call", "status": "in_progress", "call_id": item.callID, "name": item.name, "input": ""})
		}
		return item.withNamespace(map[string]any{"id": item.id, "type": "function_call", "status": "in_progress", "call_id": item.callID, "name": item.name, "arguments": ""})
	}
	switch item.kind {
	case "reasoning":
		object := map[string]any{"id": item.id, "type": "reasoning", "summary": parts(item.summary)}
		if len(item.content) > 0 {
			object["content"] = parts(item.content)
		}
		if item.encrypted != "" {
			object["encrypted_content"] = item.encrypted
		}
		return object
	case "message":
		return map[string]any{"id": item.id, "type": "message", "status": status, "role": "assistant", "content": parts(item.content)}
	}
	if item.kind == "custom_tool_call" {
		return item.withNamespace(map[string]any{"id": item.id, "type": "custom_tool_call", "status": status, "call_id": item.callID, "name": item.name, "input": item.arguments.String()})
	}
	return item.withNamespace(map[string]any{"id": item.id, "type": "function_call", "status": status, "call_id": item.callID, "name": item.name, "arguments": item.arguments.String()})
}

// withNamespace adds a call's namespace to its object, where it has one.
func (item *outputItem) withNamespace(object map[string]any) map[string]any {
	if item.namespace != "" {
		object["namespace"] = item.namespace
	}
	return object
}

func parts(list []*contentPart) []any {
	out := []any{}
	for _, part := range list {
		out = append(out, part.object())
	}
	return out
}

func (part *contentPart) object() map[string]any {
	text := part.text.String()
	switch part.kind {
	case "output_text":
		var annotations, logprobs any = part.annotations, part.logprobs
		if part.annotations == nil {
			annotations = []any{}
		}
		if part.logprobs == nil {
			logprobs = []any{}
		}
		return map[string]any{"type": "output_text", "text": text, "annotations": annotations, "logprobs": logprobs}
	case "refusal":
		return map[string]any{"type": "refusal", "refusal": text}
	}
	return map[string]any{"type": part.kind, "text": text}
}

// errorEvent is a backend's broken stream as a Responses error event,
// numbered after the events sent.
func (b *responseBuilder) errorEvent(message string) []byte {
	data, _ := json.Marshal(map[string]any{"type": "error", "code": nil, "message": message, "param": nil, "sequence_number": b.sequence})
	return data
}

// responsesUsage is usage as Responses reports it.
func responsesUsage(input, cached, output, reasoning int64) map[string]any {
	return map[string]any{
		"input_tokens":          input,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         output,
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          input + output,
	}
}

// responsesStream is a backend stream translated into Responses events: read
// turns each of the backend's events into calls on the builder, and reports
// whether the reply has ended; end finishes the response when the backend's
// stream ends before that. A broken stream ends with a Responses error event.
type responsesStream struct {
	events  backend.ChatStream
	builder *responseBuilder
	read    func(data []byte) (done bool)
	end     func()
	done    bool
}

func (s *responsesStream) Next() ([]byte, error) {
	b := s.builder
	for len(b.events) == 0 {
		if s.done {
			return nil, io.EOF
		}
		data, err := s.events.Next()
		var streamErr *backend.StreamError
		if errors.As(err, &streamErr) {
			s.done = true
			return nil, &backend.StreamError{Data: b.errorEvent(errorMessage(streamErr.Data))}
		}
		if err == io.EOF {
			s.done = true
			s.end()
			continue
		}
		if err != nil {
			return nil, err
		}
		s.done = s.read(data)
	}
	next := b.events[0]
	b.events = b.events[1:]
	return next, nil
}

func (s *responsesStream) Close() error {
	return s.events.Close()
}

// errorMessage reads an error body's message, OpenAI's or Anthropic's shape
// alike, or takes the body as the message.
func errorMessage(body []byte) string {
	if message := gjson.GetBytes(body, "error.message").String(); message != "" {
		return message
	}
	return strings.TrimSpace(string(body))
}

// responsesItemKind is an input item's type; one with a role and no type is
// a message, as Responses reads it.
func responsesItemKind(item gjson.Result) string {
	if kind := item.Get("type").String(); kind != "" {
		return kind
	}
	if item.Get("role").Exists() {
		return "message"
	}
	return ""
}

// failedError is a response that failed as an error in OpenAI's shape.
func failedError(response gjson.Result) []byte {
	message := response.Get("error.message").String()
	if message == "" {
		message = "the response failed"
	}
	data, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "api_error", "code": response.Get("error.code").Value()}})
	return data
}

// responseReasoning joins a reasoning item's text: its reasoning_text, else its
// summary.
func responseReasoning(item gjson.Result) string {
	var texts []string
	for _, part := range item.Get("content").Array() {
		texts = append(texts, part.Get("text").String())
	}
	if len(texts) == 0 {
		for _, part := range item.Get("summary").Array() {
			texts = append(texts, part.Get("text").String())
		}
	}
	return strings.Join(texts, "\n\n")
}

func createdOf(response gjson.Result) int64 {
	if created := response.Get("created_at").Int(); created != 0 {
		return created
	}
	return time.Now().Unix()
}

// streamErrorMessage reads a Responses error event's message, or an error
// object's, or takes the payload as the message.
func streamErrorMessage(data []byte) string {
	if message := gjson.GetBytes(data, "message").String(); message != "" {
		return message
	}
	return errorMessage(data)
}

// A Responses client's shell and apply_patch, which it runs itself, reach a
// backend without them as tools of their names: patchSchema takes the
// operation apply_patch_call carries, and patchDescription gives the diff
// format OpenAI's Agents SDK applies (apply_diff.py, read 2026-11-10), in
// which Claude Sonnet 5.5 and Haiku 5.5 wrote diffs that applied, creating,
// updating in several places and deleting (2026-11-10); shellSchema takes
// the commands shell_call carries.
const (
	patchToolName = "apply_patch"
	shellToolName = "shell"
)

const patchSchema = `{"type":"object","properties":{"type":{"type":"string","enum":["create_file","update_file","delete_file"]},"path":{"type":"string"},"diff":{"type":"string"}},"required":["type","path"]}`

const patchDescription = "Create, update or delete one file.\n" +
	"- create_file: diff is the file's full contents, every line prefixed with \"+\" (an empty line is a lone \"+\").\n" +
	"- update_file: diff is one or more hunks. A hunk starts with a line \"@@\", or \"@@ \" followed by one line of the file that comes before the change, such as a function's signature, to find the place. Then each line of the hunk starts with \" \" for an unchanged line, \"-\" for a line removed or \"+\" for a line added. Give about three unchanged lines before and after each change, so the place is found once. Unchanged and removed lines must match the file exactly. A hunk at the end of the file can end with a line \"*** End of File\".\n" +
	"- delete_file: no diff."

const shellSchema = `{"type":"object","properties":{"commands":{"type":"array","items":{"type":"string"}},"timeout_ms":{"type":"integer"}},"required":["commands"]}`

const shellDescription = "Run shell commands, one after another, and return each one's output and exit status. timeout_ms limits how long each may run."

// namespaceSeparator joins a namespace's name and the name of a tool it
// holds, in the name a backend is given: only Responses groups tools in
// namespaces, and the Claude API and Chat Completions take only letters,
// digits, _ and - in a tool's name.
const namespaceSeparator = "__"

// withoutNamespaces rewrites a Responses request for a backend with no
// namespace tools, which group a client's own tools under one name, as Codex
// groups its sub-agent functions. Each namespace becomes the tools it holds,
// each named its namespace's name and its own, joined by namespaceSeparator,
// and described by the namespace's description ahead of its own. A call in
// the input that names a namespace is renamed the same way, and so is a tool
// a tool choice names, which names no namespace, where only one namespace
// holds it. responseBuilder.call gives the reply's calls their namespace back.
func withoutNamespaces(body []byte) ([]byte, error) {
	req := gjson.ParseBytes(body)
	tools := req.Get("tools").Array()
	top := map[string]bool{}
	for _, tool := range tools {
		if tool.Get("type").String() != "namespace" {
			top[tool.Get("name").String()] = true
		}
	}
	out := string(body)
	if len(top) < len(tools) {
		var flat []string
		held := map[string][]string{} // a held tool's own name: the names it is given
		for i, tool := range tools {
			if tool.Get("type").String() != "namespace" {
				flat = append(flat, tool.Raw)
				continue
			}
			namespace := tool.Get("name").String()
			for _, inner := range tool.Get("tools").Array() {
				own, name := inner.Get("name").String(), namespace+namespaceSeparator+inner.Get("name").String()
				if top[name] {
					return nil, refuse("tools[%d]: namespace '%s' holds '%s', which a backend without namespaces is given as '%s', the name of another tool", i, namespace, own, name)
				}
				description := strings.TrimSpace(tool.Get("description").String() + "\n\n" + inner.Get("description").String())
				flat = append(flat, set(set(inner.Raw, "name", name), "description", description))
				held[own] = append(held[own], name)
			}
		}
		out = setRaw(out, "tools", "["+strings.Join(flat, ",")+"]")
		named := func(path string) error {
			own := req.Get(path).String()
			if top[own] || len(held[own]) == 0 {
				return nil
			}
			if len(held[own]) > 1 {
				return refuse("%s: '%s' is in more than one namespace, and a tool choice names no namespace", path, own)
			}
			out = set(out, path, held[own][0])
			return nil
		}
		choice := req.Get("tool_choice")
		switch choice.Get("type").String() {
		case "function", "custom":
			if err := named("tool_choice.name"); err != nil {
				return nil, err
			}
		case "allowed_tools":
			for i := range choice.Get("tools").Array() {
				if err := named(fmt.Sprintf("tool_choice.tools.%d.name", i)); err != nil {
					return nil, err
				}
			}
		}
	}
	for i, item := range req.Get("input").Array() {
		if namespace := item.Get("namespace").String(); namespace != "" {
			if kind := item.Get("type").String(); kind == "function_call" || kind == "custom_tool_call" {
				out = set(out, fmt.Sprintf("input.%d.name", i), namespace+namespaceSeparator+item.Get("name").String())
			}
		}
	}
	return []byte(out), nil
}

// namespaceOf splits the name a backend called a tool by into the namespace
// holding the tool and the tool's own name, as withoutNamespaces named it,
// or returns no namespace for a tool that isn't in one.
func (b *responseBuilder) namespaceOf(name string) (namespace, own string) {
	for _, tool := range b.request.Get("tools").Array() {
		if tool.Get("type").String() != "namespace" {
			continue
		}
		prefix := tool.Get("name").String() + namespaceSeparator
		for _, inner := range tool.Get("tools").Array() {
			if name == prefix+inner.Get("name").String() {
				return tool.Get("name").String(), inner.Get("name").String()
			}
		}
	}
	return "", name
}

// clientRecords holds, by the name of the tool a backend was given, the
// Responses item that a call of that tool stands for: a call of a tool the
// client runs that has no function form on the backend, such as shell or
// apply_patch.
type clientRecords map[string]func(id, input string) map[string]any

// patchCall is a call of the tool that stands for apply_patch as the
// apply_patch_call a Responses client runs: its input is the operation.
func patchCall(id, input string) map[string]any {
	return map[string]any{"id": newID("apc_"), "type": "apply_patch_call", "call_id": id, "status": "completed", "operation": json.RawMessage(jsonOr(input, "{}"))}
}

// shellOutputText is a shell_call_output as text, each command's output
// with how it ended where it failed, and whether any failed or timed out
// (output shape from openai-python's types, read 2026-11-10).
func shellOutputText(item gjson.Result) (text string, failed bool) {
	var texts []string
	for _, o := range item.Get("output").Array() {
		texts = append(texts, o.Get("stdout").String()+o.Get("stderr").String())
		if outcome := o.Get("outcome"); outcome.Get("type").String() == "timeout" {
			texts = append(texts, "[timed out]")
			failed = true
		} else if outcome.Get("exit_code").Int() != 0 {
			texts = append(texts, "[exit code "+outcome.Get("exit_code").String()+"]")
			failed = true
		}
	}
	return strings.Join(texts, "\n"), failed
}

// clientToolClash refuses a function or custom tool named like the tool that stands for a
// client's shell or apply_patch beside that tool, since a call of it would
// be read as the client tool's.
func clientToolClash(tools gjson.Result, shellName string) error {
	for _, c := range [][2]string{{"shell", shellName}, {"apply_patch", patchToolName}} {
		if tools.Get(`#(type=="`+c[0]+`")`).Exists() && tools.Get(`#(name=="`+c[1]+`")`).Exists() {
			return refuse("tools: a function named '%s' can't be sent beside the %s tool, which reaches the backend as a tool of that name", c[1], c[0])
		}
	}
	return nil
}
