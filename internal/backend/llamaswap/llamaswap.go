// Package llamaswap is the backend adapter for llama-swap. Loaded-model state
// comes from GET /running, where only models in state "ready" count.
package llamaswap

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
// llama-swap serves them at their OpenAI paths.
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

// ListModels returns llama-swap's base models with their aliases. llama-swap
// lists each alias as its own entry too (meta.llamaswap.type "alias"); those
// are folded into their base model.
func (b *Backend) ListModels(ctx context.Context) ([]backend.ModelInfo, error) {
	var list struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				LlamaSwap struct {
					Type    string   `json:"type"`
					Aliases []string `json:"aliases"`
				} `json:"llamaswap"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := b.GetJSON(ctx, "/v1/models", &list); err != nil {
		return nil, err
	}
	models := make([]backend.ModelInfo, 0, len(list.Data))
	for _, m := range list.Data {
		if m.Meta.LlamaSwap.Type == "alias" {
			continue
		}
		models = append(models, backend.ModelInfo{ID: m.ID, Aliases: m.Meta.LlamaSwap.Aliases})
	}
	return models, nil
}

func (b *Backend) LoadedModels(ctx context.Context) ([]backend.ModelInfo, error) {
	return b.state.LoadedModels(ctx)
}

// LoadedModelsSince also reuses a state whose fetch began at or after since;
// see openaihttp.StateCache.LoadedModelsSince.
func (b *Backend) LoadedModelsSince(ctx context.Context, since time.Time) ([]backend.ModelInfo, error) {
	return b.state.LoadedModelsSince(ctx, since)
}

// fetchState reads loaded-model state for the cache. Readiness comes from
// /running, which lists base models only; their aliases come from /v1/models.
func (b *Backend) fetchState(ctx context.Context) ([]backend.ModelInfo, error) {
	var running struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if err := b.GetJSON(ctx, "/running", &running); err != nil {
		return nil, err
	}
	listed, err := b.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	aliases := map[string][]string{}
	for _, m := range listed {
		aliases[m.ID] = m.Aliases
	}
	var models []backend.ModelInfo
	for _, r := range running.Running {
		if r.State == "ready" || r.State == "starting" {
			models = append(models, backend.ModelInfo{ID: r.Model, Aliases: aliases[r.Model], Starting: r.State == "starting"})
		}
	}
	return models, nil
}
