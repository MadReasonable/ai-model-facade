// Package ollama is the backend adapter for Ollama, through its
// OpenAI-compatible endpoints. Loaded-model state comes from GET /api/ps.
//
// Model IDs are Ollama's full `name:tag` form, as /v1/models and /api/ps list
// them. A bare name is not treated as `name:latest`: candidates name exactly
// what Ollama reports.
//
// /api/ps lists a model only once it has finished loading, so the adapter
// tracks loads itself: a model with a request forwarded through this backend
// and still awaiting response headers, but not listed, is reported as
// starting. Ollama sends headers only once the model is loaded. Loads started
// by clients that bypass the gateway stay invisible until they finish.
package ollama

import (
	"context"
	"slices"
	"sync"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openaihttp"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

type Config struct {
	Type              string `yaml:"type"`
	openaihttp.Config `yaml:",inline"`
}

// Backend serves both completion endpoints through the embedded client, since
// Ollama serves them at their OpenAI paths.
type Backend struct {
	*openaihttp.Client
	state *openaihttp.StateCache

	mu      sync.Mutex
	pending map[string]int  // model -> requests forwarded and awaiting response headers
	listed  map[string]bool // models in the last /api/ps
}

// New builds the adapter from its config entry.
func New(entry config.Entry) (*Backend, error) {
	var c Config
	if err := config.DecodeStrict(entry.Node, &c); err != nil {
		return nil, err
	}
	client, err := openaihttp.New(c.Config)
	if err != nil {
		return nil, err
	}
	b := &Backend{Client: client, pending: map[string]int{}, listed: map[string]bool{}}
	b.state = openaihttp.NewStateCache(c.Config, b.fetchState)
	return b, nil
}

// ListModels returns the models Ollama has pulled. Ollama has no aliases.
func (b *Backend) ListModels(ctx context.Context) ([]backend.ModelInfo, error) {
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := b.GetJSON(ctx, "/v1/models", &list); err != nil {
		return nil, err
	}
	models := make([]backend.ModelInfo, 0, len(list.Data))
	for _, m := range list.Data {
		models = append(models, backend.ModelInfo{ID: m.ID})
	}
	return models, nil
}

// LoadedModels returns the models /api/ps lists, as ready, followed by the
// pending models it doesn't list yet, as starting.
func (b *Backend) LoadedModels(ctx context.Context) ([]backend.ModelInfo, error) {
	// Pending first: a load that finishes in between then shows up in one
	// list or the other, since finishing invalidates the cached state.
	b.mu.Lock()
	var pending []string
	for id := range b.pending {
		pending = append(pending, id)
	}
	b.mu.Unlock()
	models, err := b.state.LoadedModels(ctx)
	if err != nil {
		return nil, err
	}
	slices.Sort(pending)
	for _, id := range pending {
		if !slices.ContainsFunc(models, func(m backend.ModelInfo) bool { return m.ID == id }) {
			models = append(models, backend.ModelInfo{ID: id, Starting: true})
		}
	}
	return models, nil
}

// fetchState reads /api/ps. Ollama reports no load state, so every listed
// model is ready.
func (b *Backend) fetchState(ctx context.Context) ([]backend.ModelInfo, error) {
	var ps struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := b.GetJSON(ctx, "/api/ps", &ps); err != nil {
		return nil, err
	}
	var models []backend.ModelInfo
	listed := map[string]bool{}
	for _, m := range ps.Models {
		models = append(models, backend.ModelInfo{ID: m.Name})
		listed[m.Name] = true
	}
	b.mu.Lock()
	b.listed = listed
	b.mu.Unlock()
	return models, nil
}

// Chat forwards the request, counting its model as pending until the
// response headers arrive. If the model wasn't listed, it has just loaded (or
// failed to), so the cached state is dropped for the next request.
func (b *Backend) Chat(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	model := gjson.GetBytes(req.Body, "model").String()
	b.mu.Lock()
	b.pending[model]++
	b.mu.Unlock()
	res, err := b.Client.Chat(ctx, req)
	b.mu.Lock()
	if b.pending[model]--; b.pending[model] == 0 {
		delete(b.pending, model)
	}
	unlisted := !b.listed[model]
	b.mu.Unlock()
	if unlisted {
		b.state.Invalidate()
	}
	return res, err
}
