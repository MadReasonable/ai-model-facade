// Package lmstudio is the backend adapter for LM Studio's server, through
// its OpenAI-compatible endpoints. Model IDs and state come from LM Studio's
// own model list, GET /api/v1/models, where each model is listed by its key
// with the instances of it that are loaded.
//
// A model with a loaded instance is ready. LM Studio reports no loading
// state, so a load shows as starting only through the shared state cache's
// pending requests, as Ollama's do; LM Studio sends response headers only
// once the model has loaded (verified on llmster 0.0.25-1). It lists the
// model it is loading as loaded from the moment the load begins, so the model
// reads as ready for the whole load, and a request sent to it then waits for
// the load rather than starting another. A request for another model aborts
// the load, failing the request that started it ("Engine protocol startup
// was aborted"; verified), so the adapter implements backend.LoadsOneAtATime,
// and selection sends LM Studio no request for another model while one has a
// request pending.
//
// For about 25 s after its container starts, LM Studio unpacks its engines,
// and meanwhile its list answers 500, and so does every other request
// (verified). So the adapter wraps backend.ErrLoading in a list read that
// fails with 500, and selection sends LM Studio nothing until it is done.
//
// LM Studio answers a request for a model it doesn't have with whichever
// model is loaded (verified), so only the startup check catches a candidate
// with the wrong ID; selection treats it as unlisted, never loaded.
package lmstudio

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openaihttp"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// Backend serves both completion endpoints through the embedded client, since
// LM Studio serves them at their OpenAI paths.
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

// listedModel is one model in GET /api/v1/models.
type listedModel struct {
	Key             string `json:"key"`
	Type            string `json:"type"` // llm, embedding or decision
	LoadedInstances []struct {
		ID string `json:"id"`
	} `json:"loaded_instances"`
}

// listed returns LM Studio's LLMs, the models the completion endpoints
// serve; its embedding and decision models are left out.
func (b *Backend) listed(ctx context.Context) ([]listedModel, error) {
	var list struct {
		Models []listedModel `json:"models"`
	}
	if err := b.GetJSON(ctx, "/api/v1/models", &list); err != nil {
		return nil, err
	}
	var llms []listedModel
	for _, m := range list.Models {
		if m.Type == "llm" {
			llms = append(llms, m)
		}
	}
	return llms, nil
}

// ListModels returns every LLM LM Studio has, loaded or not, by key.
func (b *Backend) ListModels(ctx context.Context) ([]backend.ModelInfo, error) {
	listed, err := b.listed(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]backend.ModelInfo, 0, len(listed))
	for _, m := range listed {
		models = append(models, backend.ModelInfo{ID: m.Key})
	}
	return models, nil
}

// LoadsOneAtATime marks LM Studio as aborting a load for a request for
// another model.
func (b *Backend) LoadsOneAtATime() {}

// fetchState reads /api/v1/models for the cache, with each model that has a
// loaded instance as ready. A 500 means a load is under way, since that is
// how LM Studio's list fails then; any other failure, such as a 401 for a
// wrong apiKey or a 404 for a wrong baseUrl, is an error of its own.
func (b *Backend) fetchState(ctx context.Context) ([]backend.ModelInfo, error) {
	listed, err := b.listed(ctx)
	var status *openaihttp.StatusError
	if errors.As(err, &status) && status.Code == http.StatusInternalServerError {
		return nil, fmt.Errorf("%w: %w", backend.ErrLoading, err)
	}
	if err != nil {
		return nil, err
	}
	var models []backend.ModelInfo
	for _, m := range listed {
		if len(m.LoadedInstances) > 0 {
			models = append(models, backend.ModelInfo{ID: m.Key})
		}
	}
	return models, nil
}
