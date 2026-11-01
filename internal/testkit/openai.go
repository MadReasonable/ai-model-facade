package testkit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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
type OpenAI struct {
	*httptest.Server
	Recorder

	mu     sync.Mutex
	models []string
	reply  Reply
}

// NewOpenAI starts a fake OpenAI API with these models, closed when t ends.
func NewOpenAI(t testing.TB, models ...string) *OpenAI {
	f := &OpenAI{models: models}
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

func (f *OpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
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
