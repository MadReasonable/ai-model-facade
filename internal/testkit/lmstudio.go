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

// LMStudio is a fake LM Studio server (llmster 0.0.25-1) with its default
// of one model loaded by a request at a time.
//
//   - /api/v1/models lists each LLM by key with its loaded_instances, and an
//     embedding model of LM Studio's own, as the real one does.
//   - A completion request for a model that isn't loaded unloads the loaded
//     one and loads the new one, which is listed as loaded from the moment
//     its load begins, as LM Studio lists it. With a load gate set, the load
//     waits there; a request for the same model meanwhile waits for it too,
//     and a request for another model aborts it, failing the requests
//     waiting for it with LM Studio's 400, and loads its own. A request for
//     a model it doesn't have is answered by the loaded model, or refused
//     with 400 when none is.
//   - While it unpacks its engines (SetUnpacking), which LM Studio does for
//     about 25 s after its container starts, the list and every completion
//     request answer 500.
type LMStudio struct {
	*httptest.Server
	Recorder

	mu        sync.Mutex
	models    []string
	loaded    string
	reply     Reply
	load      *Gate
	current   *lmStudioLoad // the load under way, nil if none
	unpacking bool
}

// lmStudioLoad is one load under way: its model, the gate it waits at, and
// a channel closed if a load of another model aborts it.
type lmStudioLoad struct {
	model   string
	gate    *Gate
	aborted chan struct{}
}

// NewLMStudio starts a fake LM Studio with these LLMs, none loaded and its
// engines unpacked, closed when t ends.
func NewLMStudio(t testing.TB, models ...string) *LMStudio {
	f := &LMStudio{models: models}
	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Close)
	return f
}

// SetLoaded makes id the loaded model, or none with "".
func (f *LMStudio) SetLoaded(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded = id
}

// Loaded returns the loaded model, "" if none is.
func (f *LMStudio) Loaded() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loaded
}

// SetReply sets how completion requests are answered.
func (f *LMStudio) SetReply(r Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = r
}

// HoldLoads makes every later load wait until the returned gate is opened.
func (f *LMStudio) HoldLoads() *Gate {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load = NewGate()
	return f.load
}

// SetUnpacking makes the server answer as while LM Studio unpacks its
// engines after starting, or answer normally again.
func (f *LMStudio) SetUnpacking(unpacking bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unpacking = unpacking
}

func (f *LMStudio) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	f.mu.Lock()
	unpacking := f.unpacking
	f.mu.Unlock()
	if unpacking && r.URL.Path != "/v1/models" {
		if r.URL.Path == "/api/v1/models" {
			http.Error(w, `{"error":{"message":"Model does not exist.","type":"internal_error"}}`, http.StatusInternalServerError)
		} else {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}
	switch r.URL.Path {
	case "/api/v1/models":
		f.serveModels(w)
	case "/v1/chat/completions", "/v1/completions":
		f.serveCompletion(w, r, gjson.GetBytes(body, "model").String())
	default:
		http.NotFound(w, r)
	}
}

// serveCompletion answers a completion request for model, loading it first
// if it isn't loaded.
func (f *LMStudio) serveCompletion(w http.ResponseWriter, r *http.Request, model string) {
	f.mu.Lock()
	if !slices.Contains(f.models, model) {
		loaded, reply := f.loaded, f.reply
		f.mu.Unlock()
		if loaded == "" {
			http.Error(w, `{"error":{"message":"No models loaded.","type":"invalid_request_error"}}`, http.StatusBadRequest)
			return
		}
		reply.write(w)
		return
	}
	ld := f.current
	switch {
	case ld != nil && ld.model == model:
		// Waits for the load under way.
	case ld == nil && f.loaded == model:
		reply := f.reply
		f.mu.Unlock()
		reply.write(w)
		return
	default:
		if ld != nil {
			close(ld.aborted)
		}
		ld = &lmStudioLoad{model: model, gate: f.load, aborted: make(chan struct{})}
		f.current, f.loaded = ld, model
	}
	reply := f.reply
	f.mu.Unlock()
	if ld.gate != nil {
		select {
		case <-ld.gate.ch:
		case <-ld.aborted:
			http.Error(w, `{"error":{"message":"Failed to load model \"`+model+`\". Error: Engine protocol startup was aborted.","type":"invalid_request_error","param":"model","code":null}}`, http.StatusBadRequest)
			return
		case <-r.Context().Done():
			return
		}
	}
	f.mu.Lock()
	if f.current == ld {
		f.current = nil
	}
	f.mu.Unlock()
	reply.write(w)
}

func (f *LMStudio) serveModels(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	models := []map[string]any{{"type": "embedding", "key": "text-embedding-nomic-embed-text-v1.5", "loaded_instances": []any{}}}
	for _, key := range f.models {
		instances := []any{}
		if key == f.loaded {
			instances = append(instances, map[string]any{"id": key})
		}
		models = append(models, map[string]any{"type": "llm", "key": key, "loaded_instances": instances})
	}
	json.NewEncoder(w).Encode(map[string]any{"models": models})
}
