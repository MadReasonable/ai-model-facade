// Package llamaswap is the backend adapter for llama-swap. Loaded-model state
// comes from GET /running, where only models in state "ready" count.
package llamaswap

import (
	"context"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openaihttp"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// Backend serves both completion endpoints through the embedded client, since
// llama-swap serves them at their OpenAI paths.
type Backend struct {
	*openaihttp.Backend
}

// New builds the adapter from its config entry.
func New(entry config.Entry) (*Backend, error) {
	b := &Backend{}
	base, err := openaihttp.NewBackend(entry, b.fetchState)
	if err != nil {
		return nil, err
	}
	b.Backend = base
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

// Pending counts a request for model as pending in this backend's state
// cache, for a request that reaches llama-swap another way, such as through
// Open WebUI; see openaihttp.StateCache.Pending.
func (b *Backend) Pending(model string) (release func()) {
	return b.State.Pending(model)
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
