// Package ollama is the backend adapter for Ollama, through its
// OpenAI-compatible endpoints. Loaded-model state comes from GET /api/ps.
//
// Model IDs are Ollama's full `name:tag` form, as /v1/models and /api/ps list
// them. A bare name is not treated as `name:latest`: candidates name exactly
// what Ollama reports.
//
// /api/ps lists a model only once it has finished loading, so a load shows as
// starting only through the shared state cache's pending requests: a model
// with a request forwarded through this backend and still awaiting response
// headers, but not listed, is reported as starting. Ollama sends headers only
// once the model is loaded. Loads started by clients that bypass the gateway
// stay invisible until they finish.
package ollama

import (
	"context"
	"time"

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
	b := &Backend{Client: client}
	b.state = openaihttp.NewStateCache(c.Config, b.fetchState)
	client.TrackPending(b.state)
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
	return b.state.LoadedModels(ctx)
}

// LoadedModelsSince also reuses a state whose fetch began at or after since;
// see openaihttp.StateCache.LoadedModelsSince.
func (b *Backend) LoadedModelsSince(ctx context.Context, since time.Time) ([]backend.ModelInfo, error) {
	return b.state.LoadedModelsSince(ctx, since)
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
	for _, m := range ps.Models {
		models = append(models, backend.ModelInfo{ID: m.Name})
	}
	return models, nil
}
