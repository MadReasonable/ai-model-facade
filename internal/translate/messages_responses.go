package translate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// This file is the Messages and Responses pair: messagesToResponses for a
// Messages client on a candidate sent Responses, responsesToMessages for a
// Responses client on the Claude API, each translating the request one way and
// its reply, whole or streamed, back.

// messagesToResponses is the direct translator from Messages to Responses, for
// a candidate sent as Responses, such as a model OpenAI serves only there.
type messagesToResponses struct{}

func (messagesToResponses) Request(body []byte) ([]byte, Reply, error) {
	out, gone, err := responsesFromMessages(body)
	if err != nil {
		return nil, nil, err
	}
	return out, &responsesMessagesReply{dropped: gone}, nil
}

func encodeReasoning(item []byte) string {
	return backend.OpenAIReasoning + base64.StdEncoding.EncodeToString(item)
}

// decodeReasoning returns the OpenAI reasoning item a thinking block carries
// in its signature, or false for a block made anywhere else.
func decodeReasoning(block gjson.Result) (string, bool) {
	encoded, ok := strings.CutPrefix(block.Get("signature").String(), backend.OpenAIReasoning)
	if !ok {
		return "", false
	}
	item, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !gjson.ValidBytes(item) || gjson.GetBytes(item, "type").String() != "reasoning" {
		return "", false
	}
	return string(item), true
}

// responsesFromMessages translates a Messages body into a Responses body.
// Top-level keys keep the client's order, each translated in place, and a key
// with no translation passes on unchanged, so a facade's params can set
// Responses fields directly. The request isn't kept at OpenAI, since a
// Messages client sends its conversation every turn, so the reasoning comes
// back encrypted, to travel in the thinking blocks the client sends back.
func responsesFromMessages(body []byte) ([]byte, droppedFields, error) {
	req := gjson.ParseBytes(body)
	var gone droppedFields
	turns, err := readMessagesTurns(req)
	if err != nil {
		return nil, nil, err
	}
	input, err := responsesInputFromMessages(turns, &gone)
	if err != nil {
		return nil, nil, err
	}
	instructions, err := instructionsOf(req.Get("system"))
	if err != nil {
		return nil, nil, err
	}
	reasoning := reasoningFromMessages(req, &gone)
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
		case k == "system" || k == "messages":
			place("instructions", instructions)
			place("input", input)
		case k == "max_tokens":
			place("max_output_tokens", value.Raw)
		case k == "thinking" || k == "output_config":
			place("reasoning", reasoning)
			if k == "output_config" {
				var text string
				text, failed = textFromOutputConfig(value, &gone)
				place("text", text)
			}
		case k == "stop_sequences":
			if isSet(value) {
				failed = refuseMessages("the Responses API has no stop sequences, and this model is served through it; 'stop_sequences' must be absent")
			}
		case k == "tools":
			var tools string
			tools, failed = responsesToolsFromMessages(value, req.Get("mcp_servers"), &gone)
			place("tools", tools)
		case k == "tool_choice":
			var choice string
			choice, failed = responsesToolChoiceFromMessages(value)
			place("tool_choice", choice)
			if value.Get("disable_parallel_tool_use").Bool() {
				place("parallel_tool_calls", "false")
			}
		case k == "metadata":
			if user := value.Get("user_id"); user.Exists() {
				place("safety_identifier", safetyIdentifier(user.String()))
			}
			value.ForEach(func(setting, _ gjson.Result) bool {
				if s := setting.String(); s != "user_id" {
					gone.add("metadata." + s)
				}
				return true
			})
		case k == "mcp_servers":
			// each server becomes the mcp tool its toolset names
			// (responsesToolsFromMessages); one no toolset names is refused,
			// as the Claude API refuses it
			for i, server := range value.Array() {
				name := server.Get("name").String()
				if !gjson.GetBytes(body, `tools.#(mcp_server_name==`+strconv.Quote(name)+`)`).Exists() {
					failed = refuseMessages("mcp_servers[%d]: server '%s' is named by no mcp_toolset tool", i, name)
				}
			}
		case refusedMessagesFields[k]:
			failed = refuseMessages("'%s' needs the Claude API, and this model is served through the Responses API", k)
		case k == "top_k" || droppedMessagesFieldsEverywhere[k]:
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
		return nil, nil, failed
	}
	reportDroppedBlocks(body, turns, &gone)
	place("instructions", instructions)
	place("input", input)
	place("store", "false")
	include := `["reasoning.encrypted_content"]`
	if strings.Contains(gjson.GetBytes(body, "tools.#.type").Raw, "code_execution_") {
		include = `["reasoning.encrypted_content","code_interpreter_call.outputs"]`
	}
	place("include", include)
	return out.bytes(), gone, nil
}

// instructionsOf joins the system prompt's text.
func instructionsOf(system gjson.Result) (string, error) {
	if !system.Exists() || system.Type == gjson.Null {
		return "", nil
	}
	text, err := textOf(system, "system")
	if err != nil || text == "" {
		return "", err
	}
	quoted, _ := json.Marshal(text)
	return string(quoted), nil
}

// reasoningFromMessages builds reasoning from output_config.effort, and asks
// for a summary when thinking is on and displayed, so it comes back as
// thinking blocks. Thinking can't be turned off through Responses, where a
// reasoning model always reasons, so thinking disabled is reported, as is a
// budget, which Responses has no counterpart for.
func reasoningFromMessages(req gjson.Result, gone *droppedFields) string {
	reasoning := "{}"
	if effort := req.Get("output_config.effort"); effort.Type == gjson.String {
		reasoning = set(reasoning, "effort", openAIEffort(effort.String()))
	}
	thinking := req.Get("thinking")
	switch kind := thinking.Get("type").String(); kind {
	case "enabled", "adaptive":
		if thinking.Get("display").String() != "omitted" {
			reasoning = set(reasoning, "summary", "auto")
		}
	case "disabled":
		gone.add("thinking")
	}
	if thinking.Get("budget_tokens").Exists() {
		gone.add("thinking.budget_tokens")
	}
	if reasoning == "{}" {
		return ""
	}
	return reasoning
}

// textFromOutputConfig turns output_config.format into text.format, and
// reports output_config's other settings but effort, which goes to reasoning.
func textFromOutputConfig(config gjson.Result, gone *droppedFields) (string, error) {
	config.ForEach(func(setting, _ gjson.Result) bool {
		if s := setting.String(); s != "effort" && s != "format" {
			gone.add("output_config." + s)
		}
		return true
	})
	format := config.Get("format")
	if !format.Exists() {
		return "", nil
	}
	if kind := format.Get("type").String(); kind != "json_schema" {
		return "", refuseMessages("output_config.format: '%s' has no Responses API equivalent", kind)
	}
	schema := set(`{"type":"json_schema","name":"response"}`, "strict", true)
	return setRaw("{}", "format", setRaw(schema, "schema", format.Get("schema").Raw)), nil
}

// responsesInputFromMessages turns the conversation into Responses input
// items. A user turn's tool results become function call outputs ahead of the
// rest of the turn, and an assistant turn's text and tool calls become a
// message and function calls in their order. A thinking block made from an
// OpenAI reasoning item becomes that item again; any other is left out, since
// OpenAI can't read Claude's thinking. A server tool's call or result, which
// only the Claude API reads, becomes text the model can read.
func responsesInputFromMessages(turns []messagesTurn, gone *droppedFields) (string, error) {
	var items []string
	shells := map[string]bool{} // IDs of bash calls, whose results are a shell's
	for _, t := range turns {
		var turn []string
		var err error
		switch t.role {
		case "user":
			turn, err = responsesUserFromMessages(t, shells)
		case "assistant":
			turn, err = responsesAssistantFromMessages(t, shells, gone)
		case "system":
			var text string
			if text, err = blocksText(t.blocks, t.where); err == nil {
				turn = []string{set(`{"type":"message","role":"system"}`, "content", text)}
			}
		}
		if err != nil {
			return "", err
		}
		items = append(items, turn...)
	}
	return "[" + strings.Join(items, ",") + "]", nil
}

func responsesUserFromMessages(t messagesTurn, shells map[string]bool) ([]string, error) {
	if len(t.blocks) == 1 && t.blocks[0].raw.Type == gjson.String {
		return []string{set(`{"type":"message","role":"user"}`, "content", t.blocks[0].text)}, nil
	}
	var outputs, parts []string
	for _, b := range t.blocks {
		if b.kind == "tool_result" && shells[b.toolUseID] {
			outputs = append(outputs, shellOutput(b))
			continue
		}
		if b.kind == "tool_result" {
			output, err := responsesToolOutput(b)
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, output)
			continue
		}
		part, err := responsesPartFromBlock(b)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return outputs, nil
	}
	return append(outputs, setRaw(`{"type":"message","role":"user"}`, "content", "["+strings.Join(parts, ",")+"]")), nil
}

// responsesPartFromBlock turns a text, search result, image or document block
// into a Responses input part: a search result as its text, and a document by
// URL as a file by URL.
func responsesPartFromBlock(b messagesBlock) (string, error) {
	switch b.kind {
	case "text", "search_result":
		return set(`{"type":"input_text"}`, "text", b.text), nil
	case "image":
		part, err := chatImagePart(b.source, b.where)
		if err != nil {
			return "", refuseMessages("%s: the Responses API takes an image by URL or base64, not '%s'", b.where, b.source.Get("type").String())
		}
		return set(`{"type":"input_image"}`, "image_url", gjson.Get(part, "image_url.url").String()), nil
	case "document":
		switch kind := b.source.Get("type").String(); {
		case kind == "text":
			return set(`{"type":"input_text"}`, "text", b.source.Get("data").String()), nil
		case kind == "base64" && b.source.Get("media_type").String() == "application/pdf":
			return set(`{"type":"input_file","filename":"document.pdf"}`, "file_data", "data:application/pdf;base64,"+b.source.Get("data").String()), nil
		case kind == "url":
			return set(`{"type":"input_file"}`, "file_url", b.source.Get("url").String()), nil
		}
		return "", refuseMessages("%s: the Responses API takes a document here only as text, an inline PDF or a URL", b.where)
	default:
		return "", refuseMessages("%s: the Responses API can't take a '%s' block", b.where, b.kind)
	}
}

// responsesToolOutput turns a tool result into a function call's output: its
// text, or text and images. Responses has no error flag, so a failed call says
// so in its text, as messagesToChat says it.
func responsesToolOutput(b messagesBlock) (string, error) {
	output := set(`{"type":"function_call_output"}`, "call_id", b.toolUseID)
	prefix := ""
	if b.isError {
		prefix = "Error: "
	}
	var parts []string
	onlyText := true
	for _, c := range b.content {
		part, err := responsesPartFromBlock(c)
		if err != nil {
			return "", err
		}
		onlyText = onlyText && gjson.Get(part, "type").String() == "input_text"
		parts = append(parts, part)
	}
	if onlyText {
		var texts []string
		for _, part := range parts {
			texts = append(texts, gjson.Get(part, "text").String())
		}
		return set(output, "output", prefix+strings.Join(texts, "\n\n")), nil
	}
	if prefix != "" {
		parts = append([]string{set(`{"type":"input_text"}`, "text", "Error:")}, parts...)
	}
	return setRaw(output, "output", "["+strings.Join(parts, ",")+"]"), nil
}

func responsesAssistantFromMessages(t messagesTurn, shells map[string]bool, gone *droppedFields) ([]string, error) {
	var items, text []string
	flush := func() {
		if len(text) > 0 {
			items = append(items, set(`{"type":"message","role":"assistant"}`, "content", strings.Join(text, "")))
			text = nil
		}
	}
	for _, b := range t.blocks {
		switch {
		case b.kind == "text":
			text = append(text, b.text)
		case b.kind == "tool_use" && b.name == "bash":
			flush()
			shells[b.id] = true
			items = append(items, set(set(set(`{"type":"shell_call","status":"completed"}`, "call_id", b.id), "action.commands", []string{gjson.Get(b.input, "command").String()}), "action.timeout_ms", nil))
		case b.kind == "tool_use":
			flush()
			arguments := b.input
			if arguments == "" {
				arguments = "{}"
			}
			item := set(`{"type":"function_call"}`, "call_id", b.id)
			item = set(item, "name", b.name)
			items = append(items, set(item, "arguments", arguments))
		case b.kind == "thinking" || b.kind == "redacted_thinking":
			flush()
			if item, ok := decodeReasoning(b.raw); ok {
				items = append(items, item)
			} else {
				gone.add("thinking")
			}
		case isServerToolBlock(b.kind):
			text = append(text, serverToolText(b))
		default:
			return nil, refuseMessages("%s: the Responses API can't take a '%s' block", t.where, b.kind)
		}
	}
	flush()
	return items, nil
}

// responsesToolsFromMessages turns client tools into function tools. The
// others are tool types only the Claude API defines.
func responsesToolsFromMessages(tools, servers gjson.Result, gone *droppedFields) (string, error) {
	var out []string
	for i, tool := range tools.Array() {
		if tool.Get("type").String() == "mcp_toolset" {
			mcp, err := responsesMCPTool(tool, servers, i)
			if err != nil {
				return "", err
			}
			out = append(out, mcp)
			continue
		}
		if strings.HasPrefix(tool.Get("type").String(), "code_execution_") {
			out = append(out, `{"type":"code_interpreter","container":{"type":"auto"}}`)
			continue
		}
		if strings.HasPrefix(tool.Get("type").String(), "bash_") {
			out = append(out, `{"type":"shell"}`)
			continue
		}
		if strings.HasPrefix(tool.Get("type").String(), "text_editor_") {
			function, err := clientToolFunction(tool, i)
			if err != nil {
				return "", err
			}
			out = append(out, setRaw(setRaw(function, "type", `"function"`), "strict", "false"))
			continue
		}
		if isClaudeWebSearch(tool) {
			search := `{"type":"web_search"}`
			search = setPresent(search, "user_location", claudeSearchLocation(tool))
			search = setPresent(search, "filters.allowed_domains", tool.Get("allowed_domains"))
			reportSearchSettings(tool, gone, "max_uses", "blocked_domains")
			out = append(out, search)
			continue
		}
		if kind := tool.Get("type").String(); kind != "" && kind != "custom" {
			return "", refuseMessages("tools[%d]: only the Claude API defines tool type '%s', and this model is served through the Responses API; a tool sent to it needs its own 'input_schema'", i, kind)
		}
		function := set(`{"type":"function"}`, "name", tool.Get("name").String())
		if description := tool.Get("description"); description.Exists() {
			function = set(function, "description", description.String())
		}
		if schema := tool.Get("input_schema"); schema.Exists() {
			function = setRaw(function, "parameters", schema.Raw)
		}
		// Responses makes a tool strict unless told otherwise (verified
		// 2026-11-09), and strict needs every property required, where Messages
		// defaults to not strict.
		strict := "false"
		if s := tool.Get("strict"); s.Exists() {
			strict = s.Raw
		}
		out = append(out, setRaw(function, "strict", strict))
	}
	return "[" + strings.Join(out, ",") + "]", nil
}

func responsesToolChoiceFromMessages(choice gjson.Result) (string, error) {
	switch kind := choice.Get("type").String(); kind {
	case "auto":
		return strconv.Quote("auto"), nil
	case "any":
		return strconv.Quote("required"), nil
	case "none":
		return strconv.Quote("none"), nil
	case "tool":
		return set(`{"type":"function"}`, "name", choice.Get("name").String()), nil
	default:
		return "", refuseMessages("tool_choice: '%s' has no Responses API equivalent", kind)
	}
}

// responsesMessagesReply converts what a Responses backend answered back to
// Messages: a response or a stream, or an error in Anthropic's shape with the
// backend's status.
type responsesMessagesReply struct {
	dropped droppedFields
}

func (r *responsesMessagesReply) Dropped() []string { return r.dropped }

func (r *responsesMessagesReply) Result(result backend.ChatResult) backend.ChatResult {
	if result.Stream != nil {
		return backend.ChatResult{Stream: newMessagesFromResponsesStream(result.Stream)}
	}
	resp := *result.Response
	resp.ContentType = "application/json"
	switch response := gjson.ParseBytes(resp.Body); {
	case resp.Status/100 != 2:
		resp.Body = anthropicError(resp.Status, resp.Body)
	case response.Get("status").String() == "failed":
		resp.Status = http.StatusBadGateway
		resp.Body = anthropicErrorBody("api_error", gjson.GetBytes(failedError(response), "error.message").String())
	default:
		resp.Body = messageFromResponse(response)
		resp.RetryAfter = ""
	}
	return backend.ChatResult{Response: &resp}
}

// messageFromResponse turns a response into a Messages response: each
// reasoning item a thinking block that carries the item in its signature, text
// and refusals as text, function calls and shell calls as tool_use blocks, and
// the record of a search, a code run or an MCP call as the blocks Claude
// writes for it. Other items have no place in a message and are left out.
func messageFromResponse(response gjson.Result) []byte {
	b := newMessagesWriter(false)
	b.start("msg_"+response.Get("id").String(), response.Get("model").String(), nil)
	refused, calls := false, false
	for _, item := range response.Get("output").Array() {
		switch item.Get("type").String() {
		case "reasoning":
			b.think(responseReasoning(item))
			b.sign(encodeReasoning([]byte(item.Raw)))
			b.closeBlock()
		case "message":
			for _, part := range item.Get("content").Array() {
				switch part.Get("type").String() {
				case "output_text":
					start := utf8.RuneCountInString(b.openText())
					b.text(part.Get("text").String())
					citeAnnotations(b, part.Get("annotations").Array(), start)
				case "refusal":
					refused = true
					b.text(part.Get("refusal").String())
				}
			}
			b.closeBlock()
		case "function_call":
			calls = true
			b.useTool(item.Get("call_id").String(), item.Get("name").String())
			b.input(item.Get("arguments").String())
		case "web_search_call":
			for _, block := range searchBlocks(item) {
				b.block(block)
			}
		case "code_interpreter_call":
			for _, block := range codeBlocks(item) {
				b.block(block)
			}
		case "shell_call":
			calls = true
			b.useTool(item.Get("call_id").String(), "bash")
			b.input(set("{}", "command", shellCommand(item.Get("action.commands"))))
			b.closeBlock()
		case "mcp_call":
			for _, block := range mcpBlocks(item) {
				b.block(block)
			}
		}
	}
	b.finish(stopFromResponse(response, calls, refused), messagesUsageFromResponses(response.Get("usage")))
	return b.body()
}

// responsesMCPTool is a Claude MCP toolset, with the server it names, as a
// Responses MCP tool: the server by label, URL and key, run without asking
// for approval, as Claude runs it, and only the tools the toolset enables when
// it disables the rest.
func responsesMCPTool(toolset, servers gjson.Result, i int) (string, error) {
	name := toolset.Get("mcp_server_name").String()
	var server gjson.Result
	for _, s := range servers.Array() {
		if s.Get("name").String() == name {
			server = s
		}
	}
	if !server.Exists() {
		return "", refuseMessages("tools[%d]: mcp_toolset names server '%s', which mcp_servers doesn't list", i, name)
	}
	tool := set(set(`{"type":"mcp"}`, "server_label", name), "server_url", server.Get("url").String())
	if token := server.Get("authorization_token").String(); token != "" {
		tool = set(tool, "authorization", token)
	}
	tool = set(tool, "require_approval", "never")
	if toolset.Get("default_config.enabled").Type == gjson.False {
		allowed := []string{}
		toolset.Get("configs").ForEach(func(key, config gjson.Result) bool {
			if config.Get("enabled").Type != gjson.False {
				allowed = append(allowed, key.String())
			}
			return true
		})
		tool = set(tool, "allowed_tools", allowed)
	}
	return tool, nil
}

// mcpBlocks is an mcp_call record as the blocks Claude writes an MCP call
// with: the call and its result, their IDs marked as the gateway's, as
// codeBlocks marks them.
func mcpBlocks(item gjson.Result) []map[string]any {
	id := backend.GatewayServerTool + item.Get("id").String()
	output, isError := item.Get("output").String(), false
	if e := item.Get("error"); e.Exists() && e.Type != gjson.Null {
		output, isError = e.String(), true
	}
	return []map[string]any{
		{"type": "mcp_tool_use", "id": id, "name": item.Get("name").String(), "server_name": item.Get("server_label").String(), "input": inputOf(item.Get("arguments").String())},
		{"type": "mcp_tool_result", "tool_use_id": id, "is_error": isError, "content": []any{map[string]any{"type": "text", "text": output}}},
	}
}

// shellOutput is a bash tool's result as the shell_call_output a Responses
// shell's call takes: the text as stdout, its exit an error code where the
// result is an error.
func shellOutput(b messagesBlock) string {
	text, _ := blocksText(b.content, b.where)
	code := 0
	if b.isError {
		code = 1
	}
	output := set(`{"type":"shell_call_output"}`, "call_id", b.toolUseID)
	return set(output, "output", []any{map[string]any{"stdout": text, "stderr": "", "outcome": map[string]any{"type": "exit", "exit_code": code}}})
}

// searchBlocks is a web_search_call record as the blocks Claude writes a
// search with, marked as the gateway's. Their result lists no pages: OpenAI
// lists a search's sources only when the request asks for them in include,
// and its citations reach the client on the answer's text.
func searchBlocks(item gjson.Result) []map[string]any {
	id := backend.GatewayServerTool + item.Get("id").String()
	return []map[string]any{
		{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{"query": item.Get("action.query").String()}},
		{"type": "web_search_tool_result", "tool_use_id": id, "content": []any{}},
	}
}

// codeBlocks is a code_interpreter_call record as the blocks Claude writes a
// code execution with: the call and its result. Only Claude can make real ones,
// so their IDs carry the gateway's marker, by which the anthropic adapter
// removes them before Claude sees the history.
func codeBlocks(item gjson.Result) []map[string]any {
	id := backend.GatewayServerTool + item.Get("id").String()
	var logs []string
	for _, output := range item.Get("outputs").Array() {
		if output.Get("type").String() == "logs" {
			logs = append(logs, output.Get("logs").String())
		}
	}
	return []map[string]any{
		{"type": "server_tool_use", "id": id, "name": "bash_code_execution", "input": map[string]any{"command": item.Get("code").String()}},
		{"type": "bash_code_execution_tool_result", "tool_use_id": id, "content": map[string]any{"type": "bash_code_execution_result", "stdout": strings.Join(logs, "\n"), "stderr": "", "return_code": 0, "content": []any{}}},
	}
}

// stopFromResponse maps a response's status to a Messages stop_reason.
func stopFromResponse(response gjson.Result, calls, refused bool) string {
	switch response.Get("incomplete_details.reason").String() {
	case "max_output_tokens":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	}
	switch {
	case refused:
		return "refusal"
	case calls:
		return "tool_use"
	}
	return "end_turn"
}

// messagesUsageFromResponses reports usage as Messages does, with cache reads
// apart from input_tokens, where Responses counts them within it.
func messagesUsageFromResponses(usage gjson.Result) map[string]any {
	cached := usage.Get("input_tokens_details.cached_tokens").Int()
	return map[string]any{
		"input_tokens":            usage.Get("input_tokens").Int() - cached,
		"output_tokens":           usage.Get("output_tokens").Int(),
		"cache_read_input_tokens": cached,
	}
}

// messagesFromResponsesStream turns Responses events into Messages events, as
// messageFromResponse does a complete response. Each output item it has a
// block for is one: a reasoning item a thinking block, whose signature, the
// item itself, arrives as the item is done, since only then is its encrypted
// content complete; a message a text block; a function call a tool_use block.
// Responses sends one item after another, so each delta belongs to the open
// block. A reasoning item's parts after the first are joined with a blank
// line. response.completed or response.incomplete gives message_delta, with
// the stop reason and usage, then message_stop. response.failed and an error
// event end the stream with an error in Anthropic's shape.
type messagesFromResponsesStream struct {
	events backend.ChatStream
	reply  *messagesWriter
	done   bool

	opened   map[string]bool // output item IDs that opened a block
	reasoned bool            // a reasoning part has been sent in the open block
	calls    bool
	refused  bool
}

func newMessagesFromResponsesStream(events backend.ChatStream) *messagesFromResponsesStream {
	return &messagesFromResponsesStream{events: events, reply: newMessagesWriter(true), opened: map[string]bool{}}
}

func (s *messagesFromResponsesStream) Next() ([]byte, error) {
	for {
		if event, ok := s.reply.next(); ok {
			return event, nil
		}
		if s.done {
			return nil, io.EOF
		}
		data, err := s.events.Next()
		var streamErr *backend.StreamError
		if errors.As(err, &streamErr) {
			return nil, &backend.StreamError{Data: anthropicErrorBody("api_error", streamErrorMessage(streamErr.Data))}
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

func (s *messagesFromResponsesStream) translate(event gjson.Result) error {
	switch event.Get("type").String() {
	case "response.created":
		response := event.Get("response")
		s.reply.start("msg_"+response.Get("id").String(), response.Get("model").String(), map[string]any{"input_tokens": 0, "output_tokens": 0})
	case "response.output_item.added":
		item := event.Get("item")
		s.reasoned = false
		switch item.Get("type").String() {
		case "reasoning":
			s.reply.closeBlock()
			s.reply.think("")
		case "message":
			s.reply.closeBlock()
			s.reply.openBlock(map[string]any{"type": "text", "text": ""})
		case "function_call":
			s.calls = true
			s.reply.useTool(item.Get("call_id").String(), item.Get("name").String())
		default:
			return nil
		}
		s.opened[item.Get("id").String()] = true
	case "response.reasoning_summary_part.added", "response.content_part.added":
		if event.Get("type").String() == "response.content_part.added" && event.Get("part.type").String() != "reasoning_text" {
			return nil
		}
		if s.reasoned {
			s.reply.think("\n\n")
		}
		s.reasoned = true
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		s.reply.think(event.Get("delta").String())
	case "response.output_text.delta":
		s.reply.text(event.Get("delta").String())
	case "response.output_text.annotation.added":
		citeAnnotations(s.reply, []gjson.Result{event.Get("annotation")}, 0)
	case "response.refusal.delta":
		s.refused = true
		s.reply.text(event.Get("delta").String())
	case "response.function_call_arguments.delta":
		s.reply.input(event.Get("delta").String())
	case "response.output_item.done":
		item := event.Get("item")
		switch item.Get("type").String() {
		case "web_search_call":
			for _, block := range searchBlocks(item) {
				s.reply.block(block)
			}
			return nil
		case "code_interpreter_call":
			for _, block := range codeBlocks(item) {
				s.reply.block(block)
			}
			return nil
		case "shell_call":
			s.calls = true
			s.reply.useTool(item.Get("call_id").String(), "bash")
			s.reply.input(set("{}", "command", shellCommand(item.Get("action.commands"))))
			s.reply.closeBlock()
			return nil
		case "mcp_call":
			for _, block := range mcpBlocks(item) {
				s.reply.block(block)
			}
			return nil
		}
		if !s.opened[item.Get("id").String()] {
			return nil
		}
		if item.Get("type").String() == "reasoning" {
			s.reply.sign(encodeReasoning([]byte(item.Raw)))
		}
		s.reply.closeBlock()
	case "response.completed", "response.incomplete":
		response := event.Get("response")
		s.reply.finish(stopFromResponse(response, s.calls, s.refused), messagesUsageFromResponses(response.Get("usage")))
		s.done = true
	case "response.failed":
		return &backend.StreamError{Data: anthropicErrorBody("api_error", gjson.GetBytes(failedError(event.Get("response")), "error.message").String())}
	}
	return nil
}

func (s *messagesFromResponsesStream) Close() error {
	return s.events.Close()
}

// responsesToMessages is the direct translator from Responses to Messages.
type responsesToMessages struct{}

func (responsesToMessages) Request(body []byte) ([]byte, Reply, error) {
	flat, err := withoutNamespaces(body)
	if err != nil {
		return nil, nil, err
	}
	out, gone, err := messagesFromResponses(flat)
	if err != nil {
		return nil, nil, err
	}
	req := gjson.ParseBytes(body)
	return out, &messagesResponsesReply{request: req, records: clientRecordsOf(req), dropped: gone}, nil
}

// safetyIdentifier is a Messages user_id as Responses' safety_identifier, a
// JSON string. OpenAI refuses one longer than 128 characters, and Claude
// Code 2.1.319 sends a user_id of 150 (seen 2026-11-12), so a longer ID is
// sent as its SHA-256 in hex, which still names one user.
func safetyIdentifier(userID string) string {
	if utf8.RuneCountInString(userID) > 128 {
		sum := sha256.Sum256([]byte(userID))
		userID = hex.EncodeToString(sum[:])
	}
	data, _ := json.Marshal(userID)
	return string(data)
}

// claudeReasoning prefixes the encrypted_content of a reasoning item made
// from a Claude thinking block: the block itself, base64-encoded, so that a
// client sending the item back sends Claude the block with the signature it
// was made with. Claude accepts a thinking block only with that signature.
const claudeReasoning = "amf-anthropic:"

func encodeThinking(block []byte) string {
	return claudeReasoning + base64.StdEncoding.EncodeToString(block)
}

// decodeThinking returns the Claude thinking block a reasoning item carries,
// or false for one made anywhere else.
func decodeThinking(item gjson.Result) (string, bool) {
	encoded, ok := strings.CutPrefix(item.Get("encrypted_content").String(), claudeReasoning)
	if !ok {
		return "", false
	}
	block, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !gjson.ValidBytes(block) {
		return "", false
	}
	switch gjson.GetBytes(block, "type").String() {
	case "thinking", "redacted_thinking":
		return string(block), true
	}
	return "", false
}

// messagesFromResponses translates a Responses body into a Messages body.
// Top-level keys keep the client's order, each translated in place, and a key
// with no translation, such as thinking or top_k, passes on unchanged, so a
// facade's params can set Messages fields directly; output_config's own keys
// win over those translated into it. A missing max_output_tokens stays
// missing, for the adapter to fill with the model's output cap.
func messagesFromResponses(body []byte) ([]byte, droppedFields, error) {
	req := gjson.ParseBytes(body)
	var gone droppedFields
	system, messages, err := messagesFromResponsesInput(req, &gone)
	if err != nil {
		return nil, nil, err
	}
	outputConfig, err := responsesOutputConfig(req, &gone)
	if err != nil {
		return nil, nil, err
	}
	toolChoice, err := messagesToolChoiceFromResponses(req)
	if err != nil {
		return nil, nil, err
	}
	thinking := thinkingFromResponses(req)
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
		case k == "input" || k == "instructions":
			place("system", system)
			place("messages", messages)
		case k == "store", k == "previous_response_id":
			// the gateway's
		case k == "max_output_tokens":
			place("max_tokens", value.Raw)
		case k == "reasoning" || k == "text" || k == "output_config":
			place("output_config", outputConfig)
			if k == "reasoning" {
				place("thinking", thinking)
			}
		case k == "thinking":
			place("thinking", thinking)
		case k == "tool_choice" || k == "parallel_tool_calls":
			place("tool_choice", toolChoice)
		case k == "user" || k == "safety_identifier":
			place("metadata", metadataOf(req))
		case k == "tools":
			var tools string
			tools, failed = messagesToolsFromResponses(value, allowedNames(req.Get("tool_choice")), &gone)
			place("tools", tools)
			place("mcp_servers", claudeServers(value))
		case k == "include":
			if wantsLogprobs(req) {
				failed = refuse("the Claude API returns no log probabilities; include must not ask for 'message.output_text.logprobs'")
			} else if isSet(value) {
				gone.add(k)
			}
		case responsesRefused[k] != "":
			if isSet(value) {
				failed = refuseResponsesField(k, "the Claude API")
			}
		case k == "top_logprobs" || responsesOpenAIFields[k] || responsesDropped[k]:
			if isSet(value) && !(k == "truncation" && value.String() == "disabled") {
				gone.add(k)
			}
		default:
			if !placed[k] {
				placed[k] = true
				out.add(key.Raw, value.Raw)
			}
		}
		return failed == nil
	})
	if failed != nil {
		return nil, nil, failed
	}
	place("system", system)
	place("messages", messages)
	return out.bytes(), gone, nil
}

// messagesFromResponsesInput returns the instructions and any system or
// developer message as Messages' system blocks, wherever they stand, and the
// rest as user and assistant turns, consecutive items of one role in one
// turn: a function call joins the assistant's text as a tool_use block, and
// its output is a tool_result block of the next user turn. A reasoning item
// becomes the Claude thinking block it was made from; any other is left out,
// since Claude takes thinking back only with its own signature.
func messagesFromResponsesInput(req gjson.Result, gone *droppedFields) (system, messages string, err error) {
	systemBlocks := "[]"
	if instructions := req.Get("instructions"); instructions.Type == gjson.String && instructions.String() != "" {
		systemBlocks = setRaw(systemBlocks, "-1", setRaw(`{"type":"text"}`, "text", instructions.Raw))
	}
	items := req.Get("input")
	if items.Type == gjson.String {
		items = gjson.Parse(`[` + setRaw(`{"role":"user"}`, "content", items.Raw) + `]`)
	}
	messages = "[]"
	lastRole := ""
	for i, item := range items.Array() {
		where := "input[" + strconv.Itoa(i) + "]"
		var role string
		var blocks []string
		switch kind := responsesItemKind(item); kind {
		case "message":
			switch role = item.Get("role").String(); role {
			case "system", "developer":
				text, err := responsesItemText(item.Get("content"), where)
				if err != nil {
					return "", "", err
				}
				if text != "" {
					systemBlocks = setRaw(systemBlocks, "-1", set(`{"type":"text"}`, "text", text))
				}
				continue
			case "user":
				blocks, err = messagesUserBlocks(item.Get("content"), where)
			case "assistant":
				var text string
				if text, err = responsesItemText(item.Get("content"), where); text != "" {
					blocks = []string{set(`{"type":"text"}`, "text", text)}
				}
			default:
				err = refuse("%s: the Claude API has no '%s' role", where, role)
			}
		case "function_call":
			role = "assistant"
			arguments := item.Get("arguments").String()
			if strings.TrimSpace(arguments) == "" {
				arguments = "{}"
			}
			if !gjson.Valid(arguments) || !gjson.Parse(arguments).IsObject() {
				return "", "", refuse("%s: arguments must be a JSON object", where)
			}
			block := set(`{"type":"tool_use"}`, "id", item.Get("call_id").String())
			block = set(block, "name", item.Get("name").String())
			blocks = []string{setRaw(block, "input", arguments)}
		case "function_call_output":
			role = "user"
			var block string
			block, err = messagesToolResult(item, where)
			blocks = []string{block}
		case "shell_call":
			role = "assistant"
			block := set(`{"type":"tool_use","name":"bash"}`, "id", item.Get("call_id").String())
			blocks = []string{set(block, "input.command", shellCommand(item.Get("action.commands")))}
		case "shell_call_output":
			role = "user"
			blocks = []string{shellResult(item)}
		case "apply_patch_call":
			role = "assistant"
			block := set(`{"type":"tool_use"}`, "id", item.Get("call_id").String())
			block = set(block, "name", patchToolName)
			blocks = []string{setRaw(block, "input", jsonOr(item.Get("operation").Raw, "{}"))}
		case "apply_patch_call_output":
			role = "user"
			block := set(`{"type":"tool_result"}`, "tool_use_id", item.Get("call_id").String())
			block = set(block, "content", item.Get("output").String())
			if item.Get("status").String() == "failed" {
				block = set(block, "is_error", true)
			}
			blocks = []string{block}
		case "reasoning":
			block, ok := decodeThinking(item)
			if !ok {
				gone.add("reasoning")
				continue
			}
			role, blocks = "assistant", []string{block}
		case "item_reference":
			err = refuse("%s: an item_reference names an item kept at OpenAI; send the item itself, or chain the turn with previous_response_id", where)
		default:
			if !responsesServerItems[kind] {
				err = refuse("%s: a '%s' item can't be served through the Claude API", where, kind)
				break
			}
			gone.add(kind)
			role, blocks = "assistant", []string{set(`{"type":"text"}`, "text", serverItemText(item))}
		}
		if err != nil {
			return "", "", err
		}
		if len(blocks) == 0 {
			continue
		}
		if role == lastRole {
			last := strconv.Itoa(int(gjson.Get(messages, "#").Int()) - 1)
			for _, b := range blocks {
				messages = setRaw(messages, last+".content.-1", b)
			}
			continue
		}
		turn := set("{}", "role", role)
		turn = setRaw(turn, "content", "["+strings.Join(blocks, ",")+"]")
		messages = setRaw(messages, "-1", turn)
		lastRole = role
	}
	if systemBlocks != "[]" {
		system = systemBlocks
	}
	return system, messages, nil
}

// messagesUserBlocks turns a user message's content into text, image and
// PDF document blocks.
func messagesUserBlocks(content gjson.Result, where string) ([]string, error) {
	if content.Type == gjson.String {
		if content.String() == "" {
			return nil, nil
		}
		return []string{setRaw(`{"type":"text"}`, "text", content.Raw)}, nil
	}
	var blocks []string
	for _, part := range content.Array() {
		switch kind := part.Get("type").String(); kind {
		case "input_text":
			if part.Get("text").String() != "" {
				blocks = append(blocks, setRaw(`{"type":"text"}`, "text", part.Get("text").Raw))
			}
		case "input_image":
			url := part.Get("image_url").String()
			if url == "" {
				return nil, refuse("%s: an image by file_id is a file kept at OpenAI; send its image_url", where)
			}
			block, err := imageBlock(url, where)
			if err != nil {
				return nil, refuse("%s: an image data URL must be base64", where)
			}
			blocks = append(blocks, block)
		case "input_file":
			if url := part.Get("file_url").String(); url != "" {
				blocks = append(blocks, set(`{"type":"document","source":{"type":"url"}}`, "source.url", url))
				continue
			}
			mediaType, data, ok := base64DataURL(part.Get("file_data").String())
			if !ok || mediaType != "application/pdf" {
				return nil, refuse("%s: the Claude API takes a file only as an inline PDF in file_data, or by file_url", where)
			}
			blocks = append(blocks, set(`{"type":"document","source":{"type":"base64","media_type":"application/pdf"}}`, "source.data", data))
		default:
			return nil, refuse("%s: the Claude API takes no '%s' part", where, kind)
		}
	}
	return blocks, nil
}

// messagesToolResult turns a function call's output into the tool_result
// block of its call: a string, or text and image blocks.
func messagesToolResult(item gjson.Result, where string) (string, error) {
	block := set(`{"type":"tool_result"}`, "tool_use_id", item.Get("call_id").String())
	output := item.Get("output")
	if output.Type == gjson.String {
		return set(block, "content", output.String()), nil
	}
	content, err := messagesUserBlocks(output, where+".output")
	if err != nil {
		return "", err
	}
	return setRaw(block, "content", "["+strings.Join(content, ",")+"]"), nil
}

// responsesOutputConfig builds output_config from reasoning.effort and a JSON
// schema text.format, with the request's own output_config keys over them.
func responsesOutputConfig(req gjson.Result, gone *droppedFields) (string, error) {
	config := "{}"
	if effort := req.Get("reasoning.effort"); effort.Type == gjson.String {
		config = set(config, "effort", claudeEffort(effort.String()))
	}
	reportSettings(gone, "reasoning", req.Get("reasoning"), "effort", "summary")
	switch format := req.Get("text.format"); format.Get("type").String() {
	case "", "text":
	case "json_schema":
		config = setRaw(config, "format", setRaw(`{"type":"json_schema"}`, "schema", format.Get("schema").Raw))
	default:
		return "", refuse("the Claude API constrains output only to a JSON schema; text.format '%s' has no equivalent", format.Get("type").String())
	}
	reportSettings(gone, "text", req.Get("text"), "format")
	req.Get("output_config").ForEach(func(key, value gjson.Result) bool {
		config = setRaw(config, pathKey(key.String()), value.Raw)
		return true
	})
	if config == "{}" {
		return "", nil
	}
	return config, nil
}

// thinkingFromResponses asks for Claude's thinking summarized when the client
// asks for a reasoning summary, since on Claude 5.5 models its text is
// omitted unless display is "summarized" (Claude API docs, read
// 2026-11-09). The request's own thinking keys win; without a summary asked
// for, its thinking passes on unchanged.
func thinkingFromResponses(req gjson.Result) string {
	own := req.Get("thinking")
	if summary := req.Get("reasoning.summary"); !summary.Exists() || summary.Type == gjson.Null || summary.String() == "none" {
		return own.Raw
	}
	thinking := `{"type":"adaptive","display":"summarized"}`
	own.ForEach(func(key, value gjson.Result) bool {
		thinking = setRaw(thinking, pathKey(key.String()), value.Raw)
		return true
	})
	return thinking
}

// messagesToolChoiceFromResponses translates tool_choice, and
// parallel_tool_calls: false as its disable_parallel_tool_use.
func messagesToolChoiceFromResponses(req gjson.Result) (string, error) {
	choice := req.Get("tool_choice")
	serial := req.Get("parallel_tool_calls").Type == gjson.False
	var out string
	switch {
	case !choice.Exists() || choice.Type == gjson.Null:
		if !serial || !req.Get("tools").IsArray() {
			return "", nil
		}
		out = `{"type":"auto"}`
	case choice.String() == "auto":
		out = `{"type":"auto"}`
	case choice.String() == "required":
		out = `{"type":"any"}`
	case choice.String() == "none":
		return `{"type":"none"}`, nil
	case choice.Get("type").String() == "function":
		out = setRaw(`{"type":"tool"}`, "name", choice.Get("name").Raw)
	case choice.Get("type").String() == "allowed_tools":
		// the tools sent are narrowed to these (messagesToolsFromResponses)
		switch mode := choice.Get("mode").String(); mode {
		case "auto":
			out = `{"type":"auto"}`
		case "required":
			out = `{"type":"any"}`
		default:
			return "", refuse("tool_choice allowed_tools needs a mode of 'auto' or 'required', not '%s'", mode)
		}
	case choice.Get("type").String() == "custom":
		return "", refuse("tool_choice names a custom tool, and the Claude API has no custom tools")
	default:
		return "", refuse("tool_choice %s has no Claude API equivalent", choice.Raw)
	}
	if serial {
		out = set(out, "disable_parallel_tool_use", true)
	}
	return out, nil
}

// messagesToolsFromResponses turns function tools into Messages tools. The
// others are OpenAI's hosted tools, which only its servers run. With none
// left, such as a cache-only search dropped, it returns "", for no tools.
func messagesToolsFromResponses(tools gjson.Result, allowed []string, gone *droppedFields) (string, error) {
	if err := clientToolClash(tools, "bash"); err != nil {
		return "", err
	}
	out := "[]"
	for i, tool := range tools.Array() {
		switch kind := tool.Get("type").String(); kind {
		case "function":
		case "web_search", "web_search_preview":
			if tool.Get("external_web_access").Type == gjson.False {
				// the client allows only OpenAI's cached index, and Claude
				// searches the live web
				gone.add("tools.web_search")
				continue
			}
			out = setRaw(out, "-1", claudeWebSearch(tool, gone))
			continue
		case "mcp":
			if approval := tool.Get("require_approval"); approval.String() != "never" {
				return "", refuse("tools[%d]: the Claude API runs MCP tools without asking for approval; require_approval must be 'never', not %s", i, jsonOr(approval.Raw, `"always"`))
			}
			out = setRaw(out, "-1", claudeToolset(tool))
			continue
		case "shell":
			out = setRaw(out, "-1", `{"type":"bash_20250124","name":"bash"}`)
			continue
		case "apply_patch":
			tool := setRaw(set(`{}`, "name", patchToolName), "input_schema", patchSchema)
			out = setRaw(out, "-1", set(tool, "description", patchDescription))
			continue
		case "code_interpreter":
			// Claude runs code in a container of its own; a container OpenAI
			// keeps or sizes has no counterpart
			if container := tool.Get("container"); container.Exists() && container.Get("type").String() != "auto" {
				gone.add("tools.container")
			}
			out = setRaw(out, "-1", `{"type":"code_execution_20260120","name":"code_execution"}`)
			continue
		case "custom":
			return "", refuse("tools[%d]: the Claude API has no custom tools; '%s' needs to be a function tool", i, tool.Get("name").String())
		default:
			return "", refuse("tools[%d]: '%s' is a tool OpenAI's servers run, and this model is served through the Claude API, which has no counterpart", i, kind)
		}
		if allowed != nil && !slices.Contains(allowed, tool.Get("name").String()) {
			continue
		}
		block := setRaw("{}", "name", tool.Get("name").Raw)
		if d := tool.Get("description"); d.Exists() && d.Type != gjson.Null {
			block = setRaw(block, "description", d.Raw)
		}
		schema := tool.Get("parameters").Raw
		if schema == "" || schema == "null" {
			schema = `{"type":"object"}` // Messages requires a schema
		}
		block = setRaw(block, "input_schema", schema)
		if s := tool.Get("strict"); s.Exists() && s.Type != gjson.Null {
			block = setRaw(block, "strict", s.Raw)
		}
		out = setRaw(out, "-1", block)
	}
	if out == "[]" {
		return "", nil
	}
	return out, nil
}

// shellCommand is a shell_call's commands as one bash command, a line each,
// run in order as the shell tool runs them (seen 2026-11-10: gpt-5.4-nano's
// shell_call carries action.commands, a list).
func shellCommand(commands gjson.Result) string {
	var lines []string
	for _, c := range commands.Array() {
		lines = append(lines, c.String())
	}
	return strings.Join(lines, "\n")
}

// shellResult is a shell_call_output as the tool_result Claude's bash tool
// takes: the output as text, an error where a command failed or timed out
// (output shape from openai-python's types, read 2026-11-10).
func shellResult(item gjson.Result) string {
	text, failed := shellOutputText(item)
	block := set(`{"type":"tool_result"}`, "tool_use_id", item.Get("call_id").String())
	block = set(block, "content", text)
	if failed {
		block = set(block, "is_error", true)
	}
	return block
}

// shellCall is a call of Claude's bash tool as the shell_call item a
// Responses client runs. A restart has no shell counterpart; it runs no
// command.
func shellCall(id, input string) map[string]any {
	commands := []string{}
	if command := gjson.Get(input, "command").String(); command != "" {
		commands = append(commands, command)
	}
	return map[string]any{"id": newID("sh_"), "type": "shell_call", "call_id": id, "status": "completed", "action": map[string]any{"commands": commands, "timeout_ms": nil, "max_output_length": nil}}
}

// claudeToolset is a Responses MCP tool as the mcp_toolset that names its
// server, with only the allowed tools enabled when it lists them; the server
// itself goes in mcp_servers (claudeServers).
func claudeToolset(tool gjson.Result) string {
	toolset := set(`{"type":"mcp_toolset"}`, "mcp_server_name", tool.Get("server_label").String())
	allowed := tool.Get("allowed_tools")
	if names := allowed.Get("tool_names"); names.IsArray() {
		allowed = names
	}
	if allowed.IsArray() {
		toolset = setRaw(toolset, "default_config", `{"enabled":false}`)
		for _, name := range allowed.Array() {
			toolset = setRaw(toolset, "configs."+pathKey(name.String()), `{"enabled":true}`)
		}
	}
	return toolset
}

// claudeServers is the mcp_servers list for a request's MCP tools: each
// server by its label, URL and key (MCP connector, beta mcp-client-2025-11-20,
// seen 2026-11-10).
func claudeServers(tools gjson.Result) string {
	var servers []string
	for _, tool := range tools.Array() {
		if tool.Get("type").String() != "mcp" {
			continue
		}
		server := set(set(`{"type":"url"}`, "url", tool.Get("server_url").String()), "name", tool.Get("server_label").String())
		token := tool.Get("authorization").String()
		if token == "" {
			token = strings.TrimPrefix(tool.Get("headers.Authorization").String(), "Bearer ")
		}
		if token != "" {
			server = set(server, "authorization_token", token)
		}
		servers = append(servers, server)
	}
	if servers == nil {
		return ""
	}
	return "[" + strings.Join(servers, ",") + "]"
}

// claudeWebSearch is Responses' web_search tool as Claude's, in the version
// whose results come back with citations (verified 2026-11-10: the later
// versions filter results with code execution and returned none), with the
// location and allowed domains that both have; the context size has no
// counterpart.
func claudeWebSearch(tool gjson.Result, gone *droppedFields) string {
	search := `{"type":"web_search_20250305","name":"web_search"}`
	search = setPresent(search, "user_location", tool.Get("user_location"))
	search = setPresent(search, "allowed_domains", tool.Get("filters.allowed_domains"))
	if tool.Get("search_context_size").Exists() {
		gone.add("tools.search_context_size")
	}
	return search
}

// messagesResponsesReply converts what a Messages backend answered back to
// Responses: a response or a stream, or an error in OpenAI's shape with the
// backend's status.
type messagesResponsesReply struct {
	request gjson.Result
	records clientRecords
	dropped droppedFields
}

// clientRecordsOf is the client tools Claude is given: OpenAI's shell, as
// Claude's bash tool, and apply_patch, as a tool of that name.
func clientRecordsOf(request gjson.Result) clientRecords {
	records := clientRecords{}
	if request.Get(`tools.#(type=="shell")`).Exists() {
		records["bash"] = shellCall
	}
	if request.Get(`tools.#(type=="apply_patch")`).Exists() {
		records[patchToolName] = patchCall
	}
	return records
}

func (r *messagesResponsesReply) Dropped() []string { return r.dropped }

func (r *messagesResponsesReply) Result(result backend.ChatResult) backend.ChatResult {
	if result.Stream != nil {
		return backend.ChatResult{Stream: newMessagesResponsesStream(result.Stream, r.request, r.records)}
	}
	resp := *result.Response
	resp.ContentType = "application/json"
	if resp.Status/100 == 2 {
		resp.Body = responseFromMessage(gjson.ParseBytes(resp.Body), r.request, r.records)
		resp.RetryAfter = ""
	} else {
		resp.Body = openAIError(resp.Body)
	}
	return backend.ChatResult{Response: &resp}
}

// responseFromMessage turns a Messages response into a response: each
// thinking block as a reasoning item, its text as a summary, as Claude's
// thinking is, carrying the block in encrypted_content; text as a message,
// and each tool_use block as a function call. Other blocks, such as server
// tool results, have no place in a response and are left out.
func responseFromMessage(msg, request gjson.Result, records clientRecords) []byte {
	b := newResponseBuilder(request, false)
	b.model = msg.Get("model").String()
	uses := map[string]gjson.Result{} // server tool calls, by ID, for their results
	for _, block := range msg.Get("content").Array() {
		switch block.Get("type").String() {
		case "thinking", "redacted_thinking":
			b.closeItem()
			b.reasoning("summary_text", block.Get("thinking").String())
			b.encrypt(encodeThinking([]byte(block.Raw)))
			b.closeItem()
		case "text":
			start := b.textLength()
			b.text("output_text", block.Get("text").String())
			annotateResponseCitations(b, block.Get("citations").Array(), start)
		case "tool_use":
			if record := records[block.Get("name").String()]; record != nil {
				b.record(record(block.Get("id").String(), block.Get("input").Raw))
				continue
			}
			b.call(block.Get("id").String(), block.Get("name").String())
			b.arguments(block.Get("input").Raw)
		case "server_tool_use":
			uses[block.Get("id").String()] = block
			if record := serverToolRecord(block.Get("name").String(), block.Get("input").Raw); record != nil {
				b.record(record)
			}
		case "bash_code_execution_tool_result", "code_execution_tool_result", "text_editor_code_execution_tool_result":
			b.record(codeRecord(uses[block.Get("tool_use_id").String()], block, msg.Get("container.id").String()))
		case "mcp_tool_use":
			uses[block.Get("id").String()] = block
		case "mcp_tool_result":
			b.record(mcpRecord(uses[block.Get("tool_use_id").String()], block))
		}
	}
	return b.finish(incompleteFromStop(msg.Get("stop_reason").String()), messagesUsageForResponses(msg.Get("usage").Get))
}

// mcpRecord is a Claude MCP call and its result as the mcp_call item
// Responses records one with.
func mcpRecord(use, result gjson.Result) map[string]any {
	var texts []string
	for _, block := range result.Get("content").Array() {
		texts = append(texts, block.Get("text").String())
	}
	record := map[string]any{"id": newID("mcp_"), "type": "mcp_call", "status": "completed", "server_label": use.Get("server_name").String(), "name": use.Get("name").String(), "arguments": jsonOr(use.Get("input").Raw, "{}"), "output": strings.Join(texts, "\n"), "error": nil}
	if result.Get("is_error").Bool() {
		record["output"], record["error"] = nil, strings.Join(texts, "\n")
	}
	return record
}

// jsonOr is text if it is valid JSON, else fallback, for input a stream may
// have sent in part.
func jsonOr(text, fallback string) string {
	if gjson.Valid(text) {
		return text
	}
	return fallback
}

// codeRecord is a Claude code execution, its call and its result, as the
// code_interpreter_call item Responses records one with: the command or code
// run, and its output as logs (Claude's shapes seen 2026-11-10 on Haiku 5.5:
// bash_code_execution with input.command, a result with stdout, stderr and
// return_code).
func codeRecord(use, result gjson.Result, container string) map[string]any {
	input := use.Get("input")
	code := input.Get("command").String()
	if code == "" {
		code = input.Get("code").String()
	}
	if code == "" && input.Exists() {
		code = input.Raw
	}
	content := result.Get("content")
	logs := content.Get("stdout").String() + content.Get("stderr").String()
	if !content.Get("stdout").Exists() && content.Exists() {
		logs = content.Raw
	}
	return map[string]any{"id": newID("ci_"), "type": "code_interpreter_call", "status": "completed", "code": code, "container_id": container, "outputs": []any{map[string]any{"type": "logs", "logs": logs}}}
}

// annotateResponseCitations adds a Claude text block's web citations to the open text
// part as URL citations over the block's text, from start to the part's end.
// Citations of a document the client sent have no Responses counterpart.
func annotateResponseCitations(b *responseBuilder, citations []gjson.Result, start int) {
	for _, c := range citations {
		if url := c.Get("url").String(); url != "" {
			b.annotate(url, c.Get("title").String(), start, b.textLength())
		}
	}
}

// serverToolRecord is a Claude server tool's call as the Responses item that
// records the same call, nil for one Responses has no record of. Claude's
// results stay out: they are encrypted for Claude alone (seen 2026-11-10).
func serverToolRecord(name, input string) map[string]any {
	if name == "web_search" {
		return map[string]any{"id": newID("ws_"), "type": "web_search_call", "status": "completed", "action": map[string]any{"type": "search", "query": gjson.Get(input, "query").String()}}
	}
	return nil
}

// incompleteFromStop maps a Messages stop_reason to Responses'
// incomplete_details.reason, "" for a complete response. A refusal is a
// content filter's decision in OpenAI's terms, as finishReason maps it.
func incompleteFromStop(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "max_output_tokens"
	case "refusal":
		return "content_filter"
	}
	return ""
}

// messagesUsageForResponses converts Messages usage, read through usage.
// Messages counts cache reads and writes apart from input_tokens; Responses
// counts them within it, the reads again as cached_tokens.
func messagesUsageForResponses(usage func(path string) gjson.Result) map[string]any {
	cacheRead := usage("cache_read_input_tokens").Int()
	input := usage("input_tokens").Int() + usage("cache_creation_input_tokens").Int() + cacheRead
	return responsesUsage(input, cacheRead, usage("output_tokens").Int(), usage("output_tokens_details.thinking_tokens").Int())
}

// newMessagesResponsesStream turns Messages events into Responses events, as
// responseFromMessage does a complete one. A thinking block's text streams
// as its summary, and its signature, which arrives last, goes into
// encrypted_content as the item closes. message_stop ends the response.
func newMessagesResponsesStream(events backend.ChatStream, request gjson.Result, records clientRecords) *responsesStream {
	b := newResponseBuilder(request, true)
	var startUsage, deltaUsage gjson.Result
	var stop string
	var thinking, signature, serverInput strings.Builder
	var serverTool, serverID string // the open server tool block's tool and ID, or ""
	var container string            // the code execution container, from message_start
	uses := map[string]gjson.Result{}
	var mcpUse gjson.Result         // the open MCP call block, as it started
	var recordID, recordName string // the open call's ID and tool, when it stands for a record
	var textStart int               // the open text block's start in its part
	var citations []gjson.Result    // the open text block's
	usage := func(path string) gjson.Result {
		if v := deltaUsage.Get(path); v.Exists() {
			return v
		}
		return startUsage.Get(path)
	}
	finish := func() { b.finish(incompleteFromStop(stop), messagesUsageForResponses(usage)) }
	read := func(data []byte) bool {
		event := gjson.ParseBytes(data)
		switch event.Get("type").String() {
		case "message_start":
			b.start(event.Get("message.model").String())
			startUsage = event.Get("message.usage")
			container = event.Get("message.container.id").String()
		case "content_block_start":
			block := event.Get("content_block")
			b.closeItem()
			switch block.Get("type").String() {
			case "thinking":
				thinking.Reset()
				signature.Reset()
			case "redacted_thinking":
				b.encrypt(encodeThinking([]byte(block.Raw)))
			case "tool_use":
				if records[block.Get("name").String()] != nil {
					recordID, recordName = block.Get("id").String(), block.Get("name").String()
					serverInput.Reset()
					break
				}
				b.call(block.Get("id").String(), block.Get("name").String())
			case "server_tool_use":
				serverTool = block.Get("name").String()
				serverID = block.Get("id").String()
				serverInput.Reset()
			case "bash_code_execution_tool_result", "code_execution_tool_result", "text_editor_code_execution_tool_result":
				b.record(codeRecord(uses[block.Get("tool_use_id").String()], block, container))
			case "mcp_tool_use":
				mcpUse = block
				serverInput.Reset()
			case "mcp_tool_result":
				b.record(mcpRecord(uses[block.Get("tool_use_id").String()], block))
			case "text":
				textStart, citations = 0, nil
			}
		case "content_block_delta":
			delta := event.Get("delta")
			switch delta.Get("type").String() {
			case "thinking_delta":
				thinking.WriteString(delta.Get("thinking").String())
				b.reasoning("summary_text", delta.Get("thinking").String())
			case "signature_delta":
				signature.WriteString(delta.Get("signature").String())
			case "text_delta":
				b.text("output_text", delta.Get("text").String())
			case "input_json_delta":
				if serverTool != "" || mcpUse.Exists() || recordID != "" {
					serverInput.WriteString(delta.Get("partial_json").String())
				} else {
					b.arguments(delta.Get("partial_json").String())
				}
			case "citations_delta":
				citations = append(citations, delta.Get("citation"))
			}
		case "content_block_stop":
			if recordID != "" {
				b.record(records[recordName](recordID, serverInput.String()))
				recordID = ""
				return false
			}
			if mcpUse.Exists() {
				input := jsonOr(serverInput.String(), mcpUse.Get("input").Raw)
				uses[mcpUse.Get("id").String()] = gjson.Parse(setRaw(mcpUse.Raw, "input", jsonOr(input, "{}")))
				mcpUse = gjson.Result{}
				return false
			}
			if serverTool != "" {
				uses[serverID] = gjson.Parse(setRaw(set("{}", "name", serverTool), "input", jsonOr(serverInput.String(), "{}")))
				if record := serverToolRecord(serverTool, serverInput.String()); record != nil {
					b.record(record)
				}
				serverTool = ""
				return false
			}
			annotateResponseCitations(b, citations, textStart)
			citations = nil
			if signature.Len() > 0 {
				block, _ := json.Marshal(map[string]string{"type": "thinking", "thinking": thinking.String(), "signature": signature.String()})
				b.encrypt(encodeThinking(block))
				signature.Reset()
			}
			b.closeItem()
		case "message_delta":
			stop = event.Get("delta.stop_reason").String()
			deltaUsage = event.Get("usage")
		case "message_stop":
			finish()
			return true
		}
		return false
	}
	return &responsesStream{events: events, builder: b, read: read, end: finish}
}
