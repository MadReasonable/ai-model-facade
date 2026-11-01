package anthropic

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// requestError is a Chat Completions request the Messages API can't serve as
// the client means it. The client gets it as a 400, since sending it on would
// either fail or answer something else.
type requestError struct{ msg string }

func (e *requestError) Error() string { return e.msg }

func refuse(format string, args ...any) error {
	return &requestError{fmt.Sprintf(format, args...)}
}

// dropped are Chat Completions fields with no Messages counterpart that tune
// sampling or configure OpenAI's own services, so leaving them out changes
// nothing a reply depends on. The API refuses any field it doesn't know
// (verified: "seed: Extra inputs are not permitted").
var dropped = map[string]bool{
	"frequency_penalty": true,
	"logit_bias":        true,
	"metadata":          true, // OpenAI's stored-completion tags, not Anthropic's metadata
	"prediction":        true,
	"presence_penalty":  true,
	"seed":              true,
	"service_tier":      true, // same name, other values
	"store":             true,
	"stream_options":    true, // read for include_usage
	"top_logprobs":      true,
	"verbosity":         true,
}

// toMessages translates a Chat Completions body into a Messages body, and
// says whether the client asked for usage at the end of a stream.
//
// Top-level keys keep the client's order, each translated in place. A key
// with no translation, such as thinking, cache_control or top_k, is passed on
// unchanged, so a facade's params can set Messages fields directly; where
// one also comes from a translated field, as output_config does from
// reasoning_effort, the native value's own keys win. A missing max_tokens,
// which Messages requires, is the model's output cap from outputCap.
func toMessages(body []byte, outputCap func(model string) (int64, bool)) ([]byte, bool, error) {
	req := gjson.ParseBytes(body)
	if err := checkUnsupported(req); err != nil {
		return nil, false, err
	}
	system, messages, err := translateMessages(req.Get("messages"))
	if err != nil {
		return nil, false, err
	}
	if system != "" && req.Get("system").Exists() {
		return nil, false, refuse("the request has both system messages and a Messages 'system' field; use one")
	}
	maxTokens, err := maxTokensOf(req, outputCap)
	if err != nil {
		return nil, false, err
	}
	outputConfig := outputConfigOf(req)
	toolChoice, err := toolChoiceOf(req)
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
	var failed error
	req.ForEach(func(key, value gjson.Result) bool {
		switch k := key.String(); {
		case k == "messages":
			place("system", system)
			place("messages", messages)
		case k == "max_tokens" || k == "max_completion_tokens":
			place("max_tokens", maxTokens)
		case k == "output_config" || k == "reasoning_effort" || k == "response_format":
			place("output_config", outputConfig)
		case k == "tool_choice" || k == "parallel_tool_calls":
			place("tool_choice", toolChoice)
		case k == "user" || k == "safety_identifier":
			place("metadata", metadataOf(req))
		case k == "stop":
			place("stop_sequences", stopSequencesOf(value))
		case k == "tools":
			var tools string
			tools, failed = translateTools(value)
			place("tools", tools)
		case dropped[k] || k == "n" || k == "logprobs" || k == "modalities" || k == "audio" || k == "web_search_options":
			// checked by checkUnsupported; nothing to send
		default:
			if !placed[k] {
				placed[k] = true
				out.add(key.Raw, value.Raw)
			}
		}
		return failed == nil
	})
	if failed != nil {
		return nil, false, failed
	}
	place("max_tokens", maxTokens)
	return out.bytes(), req.Get("stream_options.include_usage").Bool(), nil
}

// members is a JSON object built from raw keys and values, in order.
type members struct{ buf []byte }

func (m *members) add(rawKey, rawValue string) {
	if m.buf == nil {
		m.buf = []byte{'{'}
	} else {
		m.buf = append(m.buf, ',')
	}
	m.buf = append(m.buf, rawKey...)
	m.buf = append(m.buf, ':')
	m.buf = append(m.buf, rawValue...)
}

func (m *members) bytes() []byte {
	if m.buf == nil {
		return []byte("{}")
	}
	return append(m.buf, '}')
}

// setRaw and set build blocks at constant paths, which sjson can't fail on.
func setRaw(dst, path, raw string) string {
	out, _ := sjson.SetRaw(dst, path, raw)
	return out
}

func set(dst, path string, value any) string {
	out, _ := sjson.Set(dst, path, value)
	return out
}

// pathKey escapes a key from the request as one path segment. sjson also
// reads a leading ':' as its force-key marker, which gjson.Escape leaves.
func pathKey(key string) string {
	p := gjson.Escape(key)
	if strings.HasPrefix(p, ":") {
		p = `\` + p
	}
	return p
}

// checkUnsupported refuses the fields that ask for something Messages can't
// give: several choices, log probabilities, audio, or OpenAI's web search.
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
	if w := req.Get("web_search_options"); w.Exists() && w.Type != gjson.Null {
		return refuse("'web_search_options' is OpenAI's web search; on the Claude API, add its web search tool through the facade's params")
	}
	return nil
}

func maxTokensOf(req gjson.Result, outputCap func(model string) (int64, bool)) (string, error) {
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if v := req.Get(key); v.Exists() && v.Type != gjson.Null {
			return v.Raw, nil
		}
	}
	model := req.Get("model").String()
	limit, ok := outputCap(model)
	if !ok {
		return "", refuse("the Claude API needs max_tokens, and its model list gives no output cap for '%s' to use instead; set max_tokens in the request or the facade's params", model)
	}
	return strconv.FormatInt(limit, 10), nil
}

// outputConfigOf builds output_config from reasoning_effort and a JSON schema
// response_format, with the request's own output_config keys over them.
func outputConfigOf(req gjson.Result) string {
	config := "{}"
	if effort := req.Get("reasoning_effort"); effort.Exists() && effort.Type != gjson.Null {
		config = setRaw(config, "effort", effort.Raw)
	}
	if format := req.Get("response_format"); format.Get("type").String() == "json_schema" {
		config = setRaw(config, "format", setRaw(`{"type":"json_schema"}`, "schema", format.Get("json_schema.schema").Raw))
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

// toolChoiceOf translates tool_choice, and parallel_tool_calls: false as its
// disable_parallel_tool_use.
func toolChoiceOf(req gjson.Result) (string, error) {
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
	case choice.Get("type").String() == "function" && choice.Get("function.name").Exists():
		out = setRaw(`{"type":"tool"}`, "name", choice.Get("function.name").Raw)
	default:
		return "", refuse("tool_choice %s has no Claude API equivalent", choice.Raw)
	}
	if serial {
		out = set(out, "disable_parallel_tool_use", true)
	}
	return out, nil
}

// translateTools turns function tools into Messages tools; Messages has no
// other kind of client tool.
func translateTools(tools gjson.Result) (string, error) {
	out := "[]"
	for _, tool := range tools.Array() {
		if t := tool.Get("type").String(); t != "function" {
			return "", refuse("the Claude API takes only function tools, not '%s'", t)
		}
		fn := tool.Get("function")
		block := setRaw("{}", "name", fn.Get("name").Raw)
		if d := fn.Get("description"); d.Exists() {
			block = setRaw(block, "description", d.Raw)
		}
		schema := fn.Get("parameters").Raw
		if schema == "" {
			schema = `{"type":"object"}` // Messages requires a schema; OpenAI's default is no parameters
		}
		block = setRaw(block, "input_schema", schema)
		if s := fn.Get("strict"); s.Exists() && s.Type != gjson.Null {
			block = setRaw(block, "strict", s.Raw)
		}
		out = setRaw(out, "-1", block)
	}
	return out, nil
}

// translateMessages turns the conversation into Messages' top-level system
// blocks and its user and assistant turns. System and developer messages are
// gathered into system, wherever they stand. A tool result becomes a
// tool_result block of a user turn, and consecutive turns of one role
// become one, so the results of parallel tool calls share a turn, as the
// API asks.
func translateMessages(msgs gjson.Result) (system, messages string, err error) {
	systemBlocks := "[]"
	messages = "[]"
	lastRole := ""
	for i, msg := range msgs.Array() {
		role := msg.Get("role").String()
		var blocks []string
		switch role {
		case "system", "developer":
			texts, err := textBlocks(msg.Get("content"), i)
			if err != nil {
				return "", "", err
			}
			for _, b := range texts {
				systemBlocks = setRaw(systemBlocks, "-1", b)
			}
			continue
		case "user":
			blocks, err = userBlocks(msg.Get("content"), i)
		case "assistant":
			blocks, err = assistantBlocks(msg, i)
		case "tool":
			blocks, err = toolResultBlocks(msg, i)
			role = "user"
		default:
			err = refuse("messages[%d]: the Claude API has no '%s' role", i, role)
		}
		if err != nil {
			return "", "", err
		}
		if len(blocks) == 0 {
			continue
		}
		if role == lastRole {
			last := int(gjson.Get(messages, "#").Int()) - 1
			for _, b := range blocks {
				messages = setRaw(messages, strconv.Itoa(last)+".content.-1", b)
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

// textBlocks turns a string or an array of text parts into text blocks,
// leaving out empty text, which the API refuses.
func textBlocks(content gjson.Result, i int) ([]string, error) {
	if content.Type == gjson.String {
		if content.String() == "" {
			return nil, nil
		}
		return []string{setRaw(`{"type":"text"}`, "text", content.Raw)}, nil
	}
	var blocks []string
	for _, part := range content.Array() {
		switch t := part.Get("type").String(); t {
		case "text", "refusal":
			text := part.Get(t)
			if text.String() != "" {
				blocks = append(blocks, setRaw(`{"type":"text"}`, "text", text.Raw))
			}
		default:
			return nil, refuse("messages[%d]: a '%s' part can't stand here; only text can", i, t)
		}
	}
	return blocks, nil
}

// userBlocks turns a user message's content into text, image and PDF
// document blocks.
func userBlocks(content gjson.Result, i int) ([]string, error) {
	if !content.IsArray() {
		return textBlocks(content, i)
	}
	var blocks []string
	for _, part := range content.Array() {
		switch t := part.Get("type").String(); t {
		case "text":
			if part.Get("text").String() != "" {
				blocks = append(blocks, setRaw(`{"type":"text"}`, "text", part.Get("text").Raw))
			}
		case "image_url":
			block, err := imageBlock(part.Get("image_url.url").String(), i)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		case "file":
			block, err := documentBlock(part.Get("file"), i)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		default:
			return nil, refuse("messages[%d]: the Claude API takes no '%s' part", i, t)
		}
	}
	return blocks, nil
}

// imageBlock takes an image by URL, or inline from a base64 data URL.
func imageBlock(url string, i int) (string, error) {
	if !strings.HasPrefix(url, "data:") {
		return set(`{"type":"image","source":{"type":"url"}}`, "source.url", url), nil
	}
	mediaType, data, ok := base64DataURL(url)
	if !ok {
		return "", refuse("messages[%d]: an image data URL must be base64", i)
	}
	block := set(`{"type":"image","source":{"type":"base64"}}`, "source.media_type", mediaType)
	return set(block, "source.data", data), nil
}

// documentBlock takes a file part's inline PDF; a file_id names a file at
// OpenAI, which the Claude API can't reach.
func documentBlock(file gjson.Result, i int) (string, error) {
	mediaType, data, ok := base64DataURL(file.Get("file_data").String())
	if !ok || mediaType != "application/pdf" {
		return "", refuse("messages[%d]: the Claude API takes a file part only as an inline PDF in file_data", i)
	}
	return set(`{"type":"document","source":{"type":"base64","media_type":"application/pdf"}}`, "source.data", data), nil
}

func base64DataURL(url string) (mediaType, data string, ok bool) {
	header, data, found := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	mediaType, encoding, _ := strings.Cut(header, ";")
	if !strings.HasPrefix(url, "data:") || !found || encoding != "base64" {
		return "", "", false
	}
	return mediaType, data, true
}

// assistantBlocks turns an assistant message's text and tool calls into text
// and tool_use blocks. Its reasoning_content is left out: the API takes
// thinking back only with the signature it was sent with, which Chat
// Completions has no place for.
func assistantBlocks(msg gjson.Result, i int) ([]string, error) {
	blocks, err := textBlocks(msg.Get("content"), i)
	if err != nil {
		return nil, err
	}
	for j, call := range msg.Get("tool_calls").Array() {
		args := call.Get("function.arguments").String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		if !gjson.Valid(args) || !gjson.Parse(args).IsObject() {
			return nil, refuse("messages[%d].tool_calls[%d]: arguments must be a JSON object", i, j)
		}
		block := setRaw(`{"type":"tool_use"}`, "id", call.Get("id").Raw)
		block = setRaw(block, "name", call.Get("function.name").Raw)
		blocks = append(blocks, setRaw(block, "input", args))
	}
	return blocks, nil
}

// toolResultBlocks turns a tool message into the tool_result block of its
// call, its content a string or text blocks.
func toolResultBlocks(msg gjson.Result, i int) ([]string, error) {
	block := setRaw(`{"type":"tool_result"}`, "tool_use_id", msg.Get("tool_call_id").Raw)
	content := msg.Get("content")
	if content.IsArray() {
		texts, err := textBlocks(content, i)
		if err != nil {
			return nil, err
		}
		return []string{setRaw(block, "content", "["+strings.Join(texts, ",")+"]")}, nil
	}
	if content.Type == gjson.String {
		block = setRaw(block, "content", content.Raw)
	}
	return []string{block}, nil
}
