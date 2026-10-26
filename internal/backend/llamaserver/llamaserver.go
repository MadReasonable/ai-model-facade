// Package llamaserver is the backend adapter for llama.cpp's llama-server, in
// either of its modes. Every state read takes the mode from GET /props, where
// a router has `role: router`, so a server restarted in the other mode is
// read correctly without a reload.
//
//   - In router mode the server loads and unloads models on demand. Each
//     model's status in /v1/models is its state: loaded is ready, loading is
//     starting. Unloaded, sleeping, downloading and incomplete models don't
//     count, since a request to one loads it. A model being unloaded still
//     reads as loaded until it is gone (b11580 reports no stopping state).
//   - In single-model mode the server holds its one model for as long as it
//     runs, and that model is ready, unless /props reports it sleeping. While
//     the model loads, the server answers every request with 503, a chat
//     included, so a request sent then would fail, not wait: the adapter
//     reports the 503 as backend.ErrLoading, and selection sends the server
//     nothing until its model has loaded.
//
// A model's aliases are other names for the same model instance, with no
// params of their own (backend.ModelInfo.PlainAliases).
package llamaserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openaihttp"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// Backend serves both completion endpoints through the embedded client, since
// llama-server serves them at their OpenAI paths.
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

// listedModel is a /v1/models entry. Status is set in router mode only.
type listedModel struct {
	ID      string   `json:"id"`
	Aliases []string `json:"aliases"`
	Status  struct {
		Value string `json:"value"`
	} `json:"status"`
}

// info returns the model with its aliases. In single-model mode llama-server
// lists the model's own ID among its aliases, so that one is dropped.
func (m listedModel) info() backend.ModelInfo {
	aliases := slices.DeleteFunc(slices.Clone(m.Aliases), func(a string) bool { return a == m.ID })
	return backend.ModelInfo{ID: m.ID, Aliases: aliases, PlainAliases: true}
}

func (b *Backend) listed(ctx context.Context) ([]listedModel, error) {
	var list struct {
		Data []listedModel `json:"data"`
	}
	if err := b.GetJSON(ctx, "/v1/models", &list); err != nil {
		return nil, err
	}
	return list.Data, nil
}

// ListModels returns the models llama-server can serve, with their aliases:
// in router mode every model it knows of, loaded or not.
func (b *Backend) ListModels(ctx context.Context) ([]backend.ModelInfo, error) {
	listed, err := b.listed(ctx)
	if err != nil {
		return nil, err
	}
	return infos(listed), nil
}

// fetchState reads loaded-model state for the cache, in whichever mode
// /props reports.
func (b *Backend) fetchState(ctx context.Context) ([]backend.ModelInfo, error) {
	var props struct {
		Role       string `json:"role"`
		IsSleeping bool   `json:"is_sleeping"`
	}
	err := b.GetJSON(ctx, "/props", &props)
	var status *openaihttp.StatusError
	if errors.As(err, &status) && status.Code == http.StatusServiceUnavailable {
		return nil, fmt.Errorf("%w: %w", backend.ErrLoading, err) // a single model loading; see the package comment
	}
	if err != nil {
		return nil, err
	}
	if props.Role != "router" && props.IsSleeping {
		return nil, nil
	}
	listed, err := b.listed(ctx)
	if err != nil {
		return nil, err
	}
	if props.Role != "router" {
		return infos(listed), nil
	}
	return routerState(listed), nil
}

// infos returns every listed model, as ready where it is state.
func infos(listed []listedModel) []backend.ModelInfo {
	models := make([]backend.ModelInfo, 0, len(listed))
	for _, m := range listed {
		models = append(models, m.info())
	}
	return models
}

// routerState returns the loaded models as ready and the loading ones as
// starting.
func routerState(listed []listedModel) []backend.ModelInfo {
	var models []backend.ModelInfo
	for _, m := range listed {
		if m.Status.Value != "loaded" && m.Status.Value != "loading" {
			continue
		}
		info := m.info()
		info.Starting = m.Status.Value == "loading"
		models = append(models, info)
	}
	return models
}
