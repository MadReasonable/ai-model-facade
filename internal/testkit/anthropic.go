package testkit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

// Anthropic is a fake of Anthropic's Claude API, through the endpoints the
// gateway uses (API documentation, read 2026-11-01, and live runs).
//
//   - Both endpoints need an anthropic-version header, and refuse a call
//     without one with 400 (verified; the documentation lists it as
//     optional).
//   - /v1/models lists the models a page at a time, `limit` to a page
//     (default 20, at most 1000), the next page after the model `after_id`
//     names; each page has `has_more`, `first_id` and `last_id`, and each
//     model its `type`, `id`, `display_name`, `created_at` and `max_tokens`,
//     its output cap.
//   - /v1/messages refuses a body field the Messages API doesn't know, or a
//     missing max_tokens, with 400, and a model it doesn't list with 404, each
//     in the API's error shape (verified). Otherwise it answers with the
//     message SetMessage set, whole, or as the API streams it when the body
//     sets stream: message_start, then each content block's start, deltas
//     and stop, then message_delta with the stop reason and message_stop.
//     A Reply set with SetReply answers instead, for errors.
type Anthropic struct {
	*httptest.Server
	Recorder

	mu          sync.Mutex
	models      []string
	content     string // the message's content blocks, a JSON array
	stopReason  string
	streamError string
	reply       *Reply
}

// OutputCap is the max_tokens the fake lists for every model.
const OutputCap = 64000

// messagesFields are the Messages API's request fields (API documentation,
// read 2026-11-01).
var messagesFields = []string{"model", "messages", "max_tokens", "system", "stop_sequences", "stream", "temperature", "top_p", "top_k", "tools", "tool_choice", "metadata", "thinking", "output_config", "cache_control", "service_tier", "container", "mcp_servers", "context_management", "inference_geo"}

// NewAnthropic starts a fake Claude API with these models, closed when t
// ends. Its message is the text "ok".
func NewAnthropic(t testing.TB, models ...string) *Anthropic {
	f := &Anthropic{models: models, content: `[{"type":"text","text":"ok"}]`, stopReason: "end_turn"}
	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Close)
	return f
}

// SetMessage sets the content blocks, a JSON array, and stop reason of the
// message every chat is answered with.
func (f *Anthropic) SetMessage(content, stopReason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.content, f.stopReason = content, stopReason
}

// SetStreamError makes a stream end, after its content blocks, with this
// error event's data instead of message_delta and message_stop, as the API
// reports a failure mid-stream.
func (f *Anthropic) SetStreamError(data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streamError = data
}

// SetReply makes every chat answered with r instead of the message.
func (f *Anthropic) SetReply(r Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = &r
}

func (f *Anthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	if r.Header.Get("Anthropic-Version") == "" {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "anthropic-version: header is required")
		return
	}
	switch r.URL.Path {
	case "/v1/models":
		f.serveModels(w, r)
	case "/v1/messages":
		f.serveMessages(w, body)
	default:
		http.NotFound(w, r)
	}
}

func anthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": errType, "message": message}})
}

func (f *Anthropic) serveMessages(w http.ResponseWriter, body []byte) {
	req := gjson.ParseBytes(body)
	unknown := ""
	req.ForEach(func(key, _ gjson.Result) bool {
		if !slices.Contains(messagesFields, key.String()) {
			unknown = key.String()
		}
		return unknown == ""
	})
	if unknown != "" {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", unknown+": Extra inputs are not permitted")
		return
	}
	if !req.Get("max_tokens").Exists() {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "max_tokens: Field required")
		return
	}
	model := req.Get("model").String()
	f.mu.Lock()
	listed, content, stopReason, streamError, reply := slices.Contains(f.models, model), f.content, f.stopReason, f.streamError, f.reply
	f.mu.Unlock()
	if !listed {
		anthropicError(w, http.StatusNotFound, "not_found_error", "model: "+model)
		return
	}
	if reply != nil {
		reply.write(w)
		return
	}
	usage := map[string]any{"input_tokens": 10, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 2, "output_tokens": 5}
	if !req.Get("stream").Bool() {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "msg_fake", "type": "message", "role": "assistant", "model": model, "content": json.RawMessage(content), "stop_reason": stopReason, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(data map[string]any) {
		payload, _ := json.Marshal(data)
		io.WriteString(w, "event: "+data["type"].(string)+"\ndata: "+string(payload)+"\n\n")
	}
	event(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_fake", "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 10, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 2, "output_tokens": 1}}})
	event(map[string]any{"type": "ping"})
	for i, block := range gjson.Parse(content).Array() {
		delta := func(d map[string]any) { event(map[string]any{"type": "content_block_delta", "index": i, "delta": d}) }
		switch block.Get("type").String() {
		case "text":
			event(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			delta(map[string]any{"type": "text_delta", "text": block.Get("text").String()})
		case "thinking":
			event(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
			delta(map[string]any{"type": "thinking_delta", "thinking": block.Get("thinking").String()})
			delta(map[string]any{"type": "signature_delta", "signature": "sig"})
		case "tool_use":
			event(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": block.Get("id").String(), "name": block.Get("name").String(), "input": map[string]any{}}})
			input := block.Get("input").Raw
			delta(map[string]any{"type": "input_json_delta", "partial_json": input[:len(input)/2]})
			delta(map[string]any{"type": "input_json_delta", "partial_json": input[len(input)/2:]})
		}
		event(map[string]any{"type": "content_block_stop", "index": i})
	}
	if streamError != "" {
		io.WriteString(w, "event: error\ndata: "+streamError+"\n\n")
		return
	}
	event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason}, "usage": usage})
	event(map[string]any{"type": "message_stop"})
}

func (f *Anthropic) serveModels(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n >= 1 && n <= 1000 {
		limit = n
	}
	f.mu.Lock()
	models := slices.Clone(f.models)
	f.mu.Unlock()
	if after := r.URL.Query().Get("after_id"); after != "" {
		models = models[slices.Index(models, after)+1:]
	}
	page := models[:min(limit, len(models))]
	data := []map[string]any{}
	for _, id := range page {
		data = append(data, map[string]any{"type": "model", "id": id, "display_name": id, "created_at": "2026-01-01T00:00:00Z", "max_tokens": OutputCap})
	}
	answer := map[string]any{"data": data, "has_more": len(page) < len(models), "first_id": nil, "last_id": nil}
	if len(page) > 0 {
		answer["first_id"], answer["last_id"] = page[0], page[len(page)-1]
	}
	json.NewEncoder(w).Encode(answer)
}
