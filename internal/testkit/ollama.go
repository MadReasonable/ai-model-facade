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

// Ollama is a fake Ollama, reached through its OpenAI-compatible endpoints.
//
//   - /v1/models lists the pulled models.
//   - /api/ps lists loaded models only: like Ollama, it never shows a model
//     while it loads.
//   - A completion request for a model that isn't loaded waits at the load
//     gate if one is set, then loads it; then it answers with the reply.
type Ollama struct {
	*httptest.Server
	Recorder

	mu     sync.Mutex
	models []string
	loaded []string
	reply  Reply
	load   *Gate
}

// NewOllama starts a fake Ollama with these pulled models, none loaded,
// closed when t ends.
func NewOllama(t testing.TB, models ...string) *Ollama {
	f := &Ollama{models: models}
	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Close)
	return f
}

// SetLoaded replaces the loaded models.
func (f *Ollama) SetLoaded(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded = slices.Clone(ids)
}

// Loaded returns the loaded models.
func (f *Ollama) Loaded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.loaded)
}

// SetReply sets how completion requests are answered.
func (f *Ollama) SetReply(r Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = r
}

// HoldLoads makes every later load wait until the returned gate is opened.
func (f *Ollama) HoldLoads() *Gate {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load = NewGate()
	return f.load
}

func (f *Ollama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	switch r.URL.Path {
	case "/v1/models":
		f.mu.Lock()
		var data []map[string]any
		for _, id := range f.models {
			data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "library"})
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	case "/api/ps":
		f.mu.Lock()
		models := []map[string]any{}
		for _, id := range f.loaded {
			models = append(models, map[string]any{"name": id, "model": id})
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"models": models})
	case "/v1/chat/completions", "/v1/completions":
		model := gjson.GetBytes(body, "model").String()
		f.mu.Lock()
		loading := !slices.Contains(f.loaded, model)
		load, reply := f.load, f.reply
		f.mu.Unlock()
		if loading {
			if !load.wait(r.Context()) {
				return
			}
			f.mu.Lock()
			if !slices.Contains(f.loaded, model) {
				f.loaded = append(f.loaded, model)
			}
			f.mu.Unlock()
		}
		reply.write(w)
	default:
		http.NotFound(w, r)
	}
}
