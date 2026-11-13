// Package openai is the backend adapter for OpenAI's API, through its Chat
// Completions and legacy Completions endpoints, and for any hosted API that
// serves them the same way, such as OpenRouter's. Model IDs come from GET
// /v1/models.
//
// A hosted model needs no load, so every listed model is ready while the API
// answers, and a state read answers from the latest list without calling the
// API; a failed request, or a list older than stateCacheSeconds, makes the
// next state read list the API again (openaihttp.ListedState). The list
// holds OpenAI's embedding, image and audio models too, since it doesn't say
// what each one does.
package openai

import (
	"context"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openaihttp"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// Backend serves both completion endpoints through the embedded client, since
// OpenAI serves them at their own paths.
type Backend struct {
	*openaihttp.Backend
	listed *openaihttp.ListedState
}

// New builds the adapter from its config entry.
func New(entry config.Entry) (*Backend, error) {
	b := &Backend{}
	base, listed, err := openaihttp.NewListedBackend(entry, b.list)
	if err != nil {
		return nil, err
	}
	b.Backend, b.listed = base, listed
	return b, nil
}

// Chat forwards a request with its token limit named max_completion_tokens,
// since OpenAI's newer models refuse max_tokens, and a client may send either.
// Groq's and OpenRouter's references also take that name and deprecate the
// other (read 2026-11-08, not run). Legacy Completions has only max_tokens.
func (b *Backend) Chat(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	if req.Protocol == backend.OpenAIChat {
		body, err := openaihttp.UseMaxCompletionTokens(req.Body)
		if err != nil {
			return backend.ChatResult{}, err
		}
		req.Body = body
	}
	return b.Backend.Chat(ctx, req)
}

// ListModels returns every model the API lists, and keeps them as its state.
func (b *Backend) ListModels(ctx context.Context) ([]backend.ModelInfo, error) {
	return b.listed.List(ctx)
}

func (b *Backend) list(ctx context.Context) ([]backend.ModelInfo, error) {
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
