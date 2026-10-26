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

// ServerModel is one model of a fake llama-server, with its aliases.
type ServerModel struct {
	ID      string
	Aliases []string
}

// LlamaServer is a fake llama.cpp llama-server (b11580), in router or
// single-model mode.
//
//   - /props has `role: router` in router mode; in single-model mode it has
//     no role, and `is_sleeping`.
//   - /v1/models and /models list each model with its aliases. In router
//     mode each entry also has `status.value`: unloaded, loading, loaded or
//     sleeping. In single-model mode the aliases include the model's own ID,
//     as llama-server's do.
//   - In router mode, a completion request for a model, or for one of its
//     aliases, that isn't loaded makes it loading, waits at the load gate if
//     one is set, then makes it loaded; then it answers with the reply. A
//     model it doesn't have is a 400.
//   - In single-model mode, every request answers 503 while the model is
//     loading, and a completion request answers with the reply whatever
//     model it names, waking a sleeping model at once.
type LlamaServer struct {
	*httptest.Server
	Recorder

	mu     sync.Mutex
	router bool
	models []ServerModel
	status map[string]string // model ID -> unloaded, loading, loaded or sleeping
	reply  Reply
	load   *Gate
}

// NewLlamaServerRouter starts a fake llama-server in router mode with these
// models, none loaded, closed when t ends.
func NewLlamaServerRouter(t testing.TB, models ...ServerModel) *LlamaServer {
	f := &LlamaServer{router: true, models: models, status: map[string]string{}}
	for _, m := range models {
		f.status[m.ID] = "unloaded"
	}
	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Close)
	return f
}

// NewLlamaServerSingle starts a fake llama-server in single-model mode with
// model loaded, closed when t ends.
func NewLlamaServerSingle(t testing.TB, model ServerModel) *LlamaServer {
	f := &LlamaServer{models: []ServerModel{model}, status: map[string]string{model.ID: "loaded"}}
	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Close)
	return f
}

// SetStatus puts model id in status: unloaded, loading, loaded or sleeping.
// In single-model mode only loading, loaded and sleeping apply.
func (f *LlamaServer) SetStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[id] = status
}

// Status returns model id's status.
func (f *LlamaServer) Status(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[id]
}

// SetReply sets how completion requests are answered.
func (f *LlamaServer) SetReply(r Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = r
}

// HoldLoads makes every later load in router mode wait until the returned
// gate is opened.
func (f *LlamaServer) HoldLoads() *Gate {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load = NewGate()
	return f.load
}

// modelOf returns the model that id names, a model or one of its aliases.
func (f *LlamaServer) modelOf(id string) (string, bool) {
	for _, m := range f.models {
		if m.ID == id || slices.Contains(m.Aliases, id) {
			return m.ID, true
		}
	}
	return "", false
}

func (f *LlamaServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	f.mu.Lock()
	loadingSingle := !f.router && f.status[f.models[0].ID] == "loading"
	f.mu.Unlock()
	if loadingSingle {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"message":"Loading model","type":"unavailable_error","code":503}}`))
		return
	}
	switch r.URL.Path {
	case "/props":
		f.serveProps(w)
	case "/v1/models", "/models":
		f.serveModels(w)
	case "/v1/chat/completions", "/v1/completions":
		f.serveCompletion(w, r, gjson.GetBytes(body, "model").String())
	default:
		http.NotFound(w, r)
	}
}

func (f *LlamaServer) serveProps(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	props := map[string]any{"build_info": "b11580-4413a1924"}
	if f.router {
		props["role"] = "router"
	} else {
		props["model_alias"] = f.models[0].ID
		props["is_sleeping"] = f.status[f.models[0].ID] == "sleeping"
	}
	json.NewEncoder(w).Encode(props)
}

func (f *LlamaServer) serveModels(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data := []map[string]any{}
	for _, m := range f.models {
		entry := map[string]any{"id": m.ID, "object": "model", "owned_by": "llamacpp"}
		if f.router {
			entry["aliases"] = append([]string{}, m.Aliases...)
			entry["status"] = map[string]any{"value": f.status[m.ID]}
		} else {
			entry["aliases"] = append([]string{m.ID}, m.Aliases...)
		}
		data = append(data, entry)
	}
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (f *LlamaServer) serveCompletion(w http.ResponseWriter, r *http.Request, model string) {
	f.mu.Lock()
	if !f.router {
		f.status[f.models[0].ID] = "loaded"
		reply := f.reply
		f.mu.Unlock()
		reply.write(w)
		return
	}
	id, ok := f.modelOf(model)
	if !ok {
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":400,"message":"model '` + model + `' not found","type":"invalid_request_error"}}`))
		return
	}
	loading := f.status[id] != "loaded"
	if loading {
		f.status[id] = "loading"
	}
	load, reply := f.load, f.reply
	f.mu.Unlock()
	if loading {
		if !load.wait(r.Context()) {
			return
		}
		f.SetStatus(id, "loaded")
	}
	reply.write(w)
}
