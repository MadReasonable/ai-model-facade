package testkit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

// SwapModel is one model in a fake llama-swap's config, with its aliases
// (presets).
type SwapModel struct {
	ID      string
	Aliases []string
}

// LlamaSwap is a fake llama-swap (v263) with includeAliasesInList: true.
//
//   - /v1/models lists each model with `meta.llamaswap.type: model` and its
//     `aliases`, and each alias with `type: alias` and its base's `modelID`;
//     its `status` is `loaded` for any model in a state, as llama-swap's is.
//   - /running lists base models in a state (ready, starting or stopping),
//     never aliases.
//   - A completion request for a model, or for one of its aliases, that isn't
//     ready makes it starting, waits at the load gate if one is set, then
//     makes it ready; then it answers with the reply.
type LlamaSwap struct {
	*httptest.Server
	Recorder

	mu        sync.Mutex
	models    []SwapModel
	states    map[string]string // base model ID -> ready, starting or stopping
	reply     Reply
	load      *Gate
	failState bool // /running answers 500
}

// NewLlamaSwap starts a fake llama-swap with these models, none running,
// closed when t ends.
func NewLlamaSwap(t testing.TB, models ...SwapModel) *LlamaSwap {
	f := &LlamaSwap{models: models, states: map[string]string{}}
	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Close)
	return f
}

// SetState puts base model id in state ("ready", "starting" or "stopping"),
// or out of /running with "".
func (f *LlamaSwap) SetState(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if state == "" {
		delete(f.states, id)
		return
	}
	f.states[id] = state
}

// State returns base model id's state, "" if it isn't running.
func (f *LlamaSwap) State(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[id]
}

// SetReply sets how completion requests are answered.
func (f *LlamaSwap) SetReply(r Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = r
}

// HoldLoads makes every later load wait until the returned gate is opened.
func (f *LlamaSwap) HoldLoads() *Gate {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load = NewGate()
	return f.load
}

// FailState makes /running answer 500, or answer normally again.
func (f *LlamaSwap) FailState(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failState = fail
}

// baseOf returns the base model of id, a model or one of its aliases.
func (f *LlamaSwap) baseOf(id string) string {
	for _, m := range f.models {
		if m.ID == id {
			return id
		}
		for _, a := range m.Aliases {
			if a == id {
				return m.ID
			}
		}
	}
	return id
}

func (f *LlamaSwap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	switch r.URL.Path {
	case "/v1/models":
		f.serveModels(w)
	case "/running":
		f.serveRunning(w)
	case "/v1/chat/completions", "/v1/completions":
		f.mu.Lock()
		base := f.baseOf(gjson.GetBytes(body, "model").String())
		loading := f.states[base] != "ready"
		if loading {
			f.states[base] = "starting"
		}
		load, reply := f.load, f.reply
		f.mu.Unlock()
		if loading {
			if !load.wait(r.Context()) {
				return
			}
			f.SetState(base, "ready")
		}
		reply.write(w)
	default:
		http.NotFound(w, r)
	}
}

func (f *LlamaSwap) serveModels(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var data []map[string]any
	for _, m := range f.models {
		status := "unloaded"
		if f.states[m.ID] != "" {
			status = "loaded"
		}
		meta := map[string]any{"type": "model"}
		if len(m.Aliases) > 0 {
			meta["aliases"] = m.Aliases
		}
		data = append(data, map[string]any{"id": m.ID, "object": "model", "owned_by": "llama-swap", "status": map[string]any{"value": status}, "meta": map[string]any{"llamaswap": meta}})
		for _, a := range m.Aliases {
			data = append(data, map[string]any{"id": a, "object": "model", "owned_by": "llama-swap", "status": map[string]any{"value": status}, "meta": map[string]any{"llamaswap": map[string]any{"type": "alias", "modelID": m.ID}}})
		}
	}
	sort.Slice(data, func(i, j int) bool { return data[i]["id"].(string) < data[j]["id"].(string) })
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (f *LlamaSwap) serveRunning(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failState {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	running := []map[string]any{}
	for id, state := range f.states {
		running = append(running, map[string]any{"model": id, "state": state})
	}
	sort.Slice(running, func(i, j int) bool { return running[i]["model"].(string) < running[j]["model"].(string) })
	json.NewEncoder(w).Encode(map[string]any{"running": running})
}
