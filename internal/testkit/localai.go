package testkit

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

// LocalAI is a fake LocalAI (v4.11.0) with its llama.cpp engine.
//
//   - /v1/models lists the models and the aliases, each as its own entry;
//     /api/aliases maps each alias to its target.
//   - /system lists the loaded models only: like LocalAI, it never shows a
//     model while it loads. /api/settings has max_active_backends.
//   - A completion request for a model, or an alias of it, that isn't
//     loaded waits at the load gate if one is set, then loads it; then it
//     answers with the reply.
//   - With a limit on loaded models, a request for a model that isn't
//     loaded, even the one loading, first evicts what the limit needs, a
//     model still loading first, since it has no last use, then the least
//     recently used. A loading model counts twice, once as loaded and once
//     as pending (v4.11.0 source, ModelLoader.Load and
//     WatchDog.EnforceLRULimit), so a second request evicts a load even under
//     a limit of two. An evicted load still finishes, and its request fails
//     with 500, while the request that evicted it waits and loads its model.
type LocalAI struct {
	*httptest.Server
	Recorder

	mu        sync.Mutex
	models    []string
	aliases   map[string]string // alias -> model
	maxActive int               // 0: no limit
	loaded    []string          // least recently used first
	loading   map[string]*localAILoad
	reply     Reply
	load      *Gate
}

// localAILoad is one load under way: closed when it ends, and evicted if a
// later load evicted it meanwhile.
type localAILoad struct {
	done    chan struct{}
	evicted bool
}

// NewLocalAI starts a fake LocalAI with these models, none loaded, and the
// limit maxActive on loaded models, 0 for none; closed when t ends.
func NewLocalAI(t testing.TB, maxActive int, models ...string) *LocalAI {
	f := &LocalAI{models: models, aliases: map[string]string{}, maxActive: maxActive, loading: map[string]*localAILoad{}}
	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Close)
	return f
}

// SetAlias makes alias another name for model.
func (f *LocalAI) SetAlias(alias, model string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aliases[alias] = model
}

// SetLoaded replaces the loaded models.
func (f *LocalAI) SetLoaded(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded = slices.Clone(ids)
}

// Loaded returns the loaded models.
func (f *LocalAI) Loaded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.loaded)
}

// SetReply sets how completion requests are answered.
func (f *LocalAI) SetReply(r Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = r
}

// HoldLoads makes every later load wait until the returned gate is opened.
func (f *LocalAI) HoldLoads() *Gate {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load = NewGate()
	return f.load
}

func (f *LocalAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	switch r.URL.Path {
	case "/v1/models":
		f.mu.Lock()
		var data []map[string]any
		for _, id := range append(slices.Sorted(maps.Keys(f.aliases)), f.models...) {
			data = append(data, map[string]any{"id": id, "object": "model"})
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	case "/api/aliases":
		f.mu.Lock()
		aliases := []map[string]string{}
		for _, alias := range slices.Sorted(maps.Keys(f.aliases)) {
			aliases = append(aliases, map[string]string{"name": alias, "target": f.aliases[alias]})
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(aliases)
	case "/system":
		f.mu.Lock()
		models := []map[string]any{}
		for _, id := range f.loaded {
			models = append(models, map[string]any{"id": id, "backend": "llama-cpp"})
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"backends": []string{"llama-cpp"}, "loaded_models": models})
	case "/api/settings":
		f.mu.Lock()
		maxActive := f.maxActive
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"max_active_backends": maxActive, "single_backend": maxActive == 1})
	case "/v1/chat/completions", "/v1/completions":
		model := gjson.GetBytes(body, "model").String()
		f.mu.Lock()
		if target, ok := f.aliases[model]; ok {
			model = target
		}
		f.mu.Unlock()
		if !f.ensureLoaded(r, model) {
			http.Error(w, `{"error":{"code":500,"message":"model was evicted while it loaded"}}`, http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		reply := f.reply
		f.mu.Unlock()
		reply.write(w)
	default:
		http.NotFound(w, r)
	}
}

// ensureLoaded loads model unless it is loaded, and reports whether it is
// loaded once its load ends, false if a later load evicted it meanwhile.
func (f *LocalAI) ensureLoaded(r *http.Request, model string) bool {
	f.mu.Lock()
	if i := slices.Index(f.loaded, model); i >= 0 {
		f.loaded = append(slices.Delete(f.loaded, i, i+1), model)
		f.mu.Unlock()
		return true
	}
	if f.maxActive > 0 {
		// The limit applies before a load under way is joined, even one of
		// this model; a load it evicts is stopped once it ends, and this
		// request waits.
		for _, l := range f.evict(len(f.loaded) + 2*len(f.loading) + 1 - f.maxActive) {
			f.mu.Unlock()
			<-l.done
			f.mu.Lock()
		}
	}
	if l, ok := f.loading[model]; ok {
		f.mu.Unlock()
		<-l.done
		return !l.evicted
	}
	l := &localAILoad{done: make(chan struct{})}
	f.loading[model] = l
	load := f.load
	f.mu.Unlock()
	ok := load.wait(r.Context())
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loading[model] == l {
		delete(f.loading, model)
	}
	close(l.done)
	if !ok || l.evicted {
		return false
	}
	f.loaded = append(f.loaded, model)
	return true
}

// evict stops n models, loading ones first, then the least recently used,
// and returns the loads it marked evicted. f.mu is held.
func (f *LocalAI) evict(n int) []*localAILoad {
	var evicted []*localAILoad
	for _, l := range f.loading {
		if n <= 0 {
			return evicted
		}
		if !l.evicted {
			l.evicted = true
			evicted = append(evicted, l)
			n--
		}
	}
	for ; n > 0 && len(f.loaded) > 0; n-- {
		f.loaded = f.loaded[1:]
	}
	return evicted
}
