package testkit

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

// OpenAI is a fake of OpenAI's API, or of another hosted API that serves
// Chat Completions the same way.
//
//   - /v1/models lists the models, each with its object, created and
//     owned_by fields.
//   - A completion request for a listed model answers with the reply; one
//     for a model it doesn't list answers 404 with the error code
//     model_not_found. A hosted model needs no load.
//   - A Responses request answers with a response whose output is the
//     Responses output, streamed as events if it asks. Unless it sets store
//     to false the response is kept: a later request can name it in
//     previous_response_id, and GET /v1/responses/{id} and its input_items,
//     in pages, return it and the entire conversation's input, as OpenAI's do
//     (verified 2026-11-09 on gpt-5.4-nano). An unknown previous_response_id
//     answers 400 with the code previous_response_not_found.
type OpenAI struct {
	*httptest.Server
	Recorder

	mu        sync.Mutex
	models    []string
	reply     Reply
	output    string                    // Responses output items; default one message saying "ok"
	responses map[string]storedResponse // by ID
	answered  int                       // Responses requests answered, for unique IDs
}

// storedResponse is a response the fake keeps: the conversation's input up to
// it, and the response object.
type storedResponse struct {
	input    []string // raw items
	response string
}

// DefaultResponsesOutput is the output a fake OpenAI's Responses requests get
// unless SetResponsesOutput changes it.
const DefaultResponsesOutput = `[{"id":"msg_fake","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","annotations":[],"text":"ok"}]}]`

// NewOpenAI starts a fake OpenAI API with these models, closed when t ends.
func NewOpenAI(t testing.TB, models ...string) *OpenAI {
	f := &OpenAI{models: models, output: DefaultResponsesOutput, responses: map[string]storedResponse{}}
	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Close)
	return f
}

// SetReply sets how completion requests are answered.
func (f *OpenAI) SetReply(r Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = r
}

// SetResponsesOutput sets the output items of every later Responses request,
// as a JSON array.
func (f *OpenAI) SetResponsesOutput(output string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.output = output
}

func (f *OpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	if strings.HasPrefix(r.URL.Path, "/v1/responses") {
		f.serveResponses(w, r, body)
		return
	}
	switch r.URL.Path {
	case "/v1/models":
		f.mu.Lock()
		data := []map[string]any{}
		for _, id := range f.models {
			data = append(data, map[string]any{"id": id, "object": "model", "created": 1700000000, "owned_by": "system"})
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	case "/v1/chat/completions", "/v1/completions":
		model := gjson.GetBytes(body, "model").String()
		f.mu.Lock()
		listed, reply := slices.Contains(f.models, model), f.reply
		f.mu.Unlock()
		if !listed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"message": "The model `" + model + "` does not exist or you do not have access to it.",
				"type":    "invalid_request_error",
				"param":   nil,
				"code":    "model_not_found",
			}})
			return
		}
		reply.write(w)
	default:
		http.NotFound(w, r)
	}
}

func openAIError(w http.ResponseWriter, status int, code, param, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error", "param": param, "code": code}})
}

func (f *OpenAI) serveResponses(w http.ResponseWriter, r *http.Request, body []byte) {
	id, _ := strings.CutPrefix(r.URL.Path, "/v1/responses/")
	id, items := strings.CutSuffix(id, "/input_items")
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		stored, ok := f.responses[id]
		if !ok {
			openAIError(w, http.StatusNotFound, "", "", "Response with id '"+id+"' not found.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !items {
			io.WriteString(w, stored.response)
			return
		}
		writeInputItems(w, r, stored.input)
		return
	}
	req := gjson.ParseBytes(body)
	model := req.Get("model").String()
	if !slices.Contains(f.models, model) {
		openAIError(w, http.StatusBadRequest, "model_not_found", "model", "The requested model '"+model+"' does not exist.")
		return
	}
	var input []string
	if previous := req.Get("previous_response_id").String(); previous != "" {
		stored, ok := f.responses[previous]
		if !ok {
			openAIError(w, http.StatusBadRequest, "previous_response_not_found", "previous_response_id", "Previous response with id '"+previous+"' not found.")
			return
		}
		input = slices.Clone(stored.input)
		for _, item := range gjson.Get(stored.response, "output").Array() {
			input = append(input, item.Raw)
		}
	}
	// The API keeps a message given as role and text as a message item with an
	// input_text part, and gives every input item an ID, which input_items
	// pages by.
	keep := func(item gjson.Result) string {
		raw := item.Raw
		if content := item.Get("content"); content.Type == gjson.String {
			raw = `{"type":"message","role":` + item.Get("role").Raw + `,"content":[{"type":"input_text","text":` + content.Raw + `}]}`
		}
		if item.Get("id").Exists() {
			return raw
		}
		return strings.Replace(raw, "{", `{"id":"in_fake`+strconv.Itoa(len(input)+1)+`",`, 1)
	}
	if text := req.Get("input"); text.Type == gjson.String {
		input = append(input, keep(gjson.Parse(`{"role":"user","content":`+text.Raw+`}`)))
	} else {
		for _, item := range text.Array() {
			input = append(input, keep(item))
		}
	}
	f.answered++
	id = fmt.Sprintf("resp_fake%d", f.answered)
	response := `{"id":"` + id + `","object":"response","created_at":1700000000,"status":"completed","model":` + strconv.Quote(model) + `,"output":` + f.output + `,"parallel_tool_calls":true,"tool_choice":"auto","tools":[],"usage":{"input_tokens":5,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":6}}`
	if req.Get("store").Type != gjson.False {
		f.responses[id] = storedResponse{input: input, response: response}
	}
	if !req.Get("stream").Bool() {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range responsesEvents(response) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.Get(event, "type").String(), event)
	}
}

// writeInputItems answers an input_items request with a page of the items,
// newest first unless order=asc, as OpenAI pages them.
func writeInputItems(w io.Writer, r *http.Request, input []string) {
	items := slices.Clone(input)
	if r.URL.Query().Get("order") != "asc" {
		slices.Reverse(items)
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit < 1 || limit > 100 {
		limit = 20
	}
	start := 0
	if after := r.URL.Query().Get("after"); after != "" {
		for i, item := range items {
			if gjson.Get(item, "id").String() == after {
				start = i + 1
			}
		}
	}
	page := items[start:min(start+limit, len(items))]
	lastID := "null"
	if len(page) > 0 {
		lastID = strconv.Quote(gjson.Get(page[len(page)-1], "id").String())
	}
	fmt.Fprintf(w, `{"object":"list","data":[%s],"has_more":%t,"last_id":%s}`, strings.Join(page, ","), start+limit < len(items), lastID)
}

// responsesEvents is a response as the events OpenAI streams it in: each output
// item added and done, a message's text as one delta, then
// response.completed.
func responsesEvents(response string) []string {
	var events []string
	add := func(event string) {
		event = strings.Replace(event, "{", `{"sequence_number":`+strconv.Itoa(len(events))+",", 1)
		events = append(events, event)
	}
	inProgress := strings.Replace(strings.Replace(response, `"status":"completed"`, `"status":"in_progress"`, 1), `"output":`+gjson.Get(response, "output").Raw, `"output":[]`, 1)
	add(`{"type":"response.created","response":` + inProgress + `}`)
	add(`{"type":"response.in_progress","response":` + inProgress + `}`)
	for i, item := range gjson.Get(response, "output").Array() {
		index := strconv.Itoa(i)
		itemID := strconv.Quote(item.Get("id").String())
		if item.Get("type").String() != "message" {
			add(`{"type":"response.output_item.added","output_index":` + index + `,"item":` + item.Raw + `}`)
			add(`{"type":"response.output_item.done","output_index":` + index + `,"item":` + item.Raw + `}`)
			continue
		}
		text := item.Get("content.0.text").Raw
		add(`{"type":"response.output_item.added","output_index":` + index + `,"item":{"id":` + itemID + `,"type":"message","status":"in_progress","role":"assistant","content":[]}}`)
		add(`{"type":"response.content_part.added","item_id":` + itemID + `,"output_index":` + index + `,"content_index":0,"part":{"type":"output_text","annotations":[],"text":""}}`)
		add(`{"type":"response.output_text.delta","item_id":` + itemID + `,"output_index":` + index + `,"content_index":0,"delta":` + text + `}`)
		add(`{"type":"response.output_text.done","item_id":` + itemID + `,"output_index":` + index + `,"content_index":0,"text":` + text + `}`)
		add(`{"type":"response.content_part.done","item_id":` + itemID + `,"output_index":` + index + `,"content_index":0,"part":` + item.Get("content.0").Raw + `}`)
		add(`{"type":"response.output_item.done","output_index":` + index + `,"item":` + item.Raw + `}`)
	}
	add(`{"type":"response.completed","response":` + response + `}`)
	return events
}
