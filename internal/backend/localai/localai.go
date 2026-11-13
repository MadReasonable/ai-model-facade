// Package localai is the backend adapter for LocalAI, through its
// OpenAI-compatible endpoints. Model IDs come from GET /v1/models, aliases
// from GET /api/aliases, and loaded-model state from GET /system.
//
// A LocalAI alias redirects every request to its target model and has no
// settings of its own, so it folds into its target with plain aliases
// (backend.ModelInfo.PlainAliases), and /system lists a loaded model under
// the target's ID (verified on v4.11.0).
//
// /system lists a model only once it has finished loading, so a load shows
// as starting only through the shared state cache's pending requests, as
// Ollama's do; LocalAI sends response headers only once the model has
// loaded (v4.11.0 source, ChatEndpoint and processStream).
//
// With a limit on loaded models (LOCALAI_MAX_ACTIVE_BACKENDS), a request
// for a model that isn't loaded, the loading one included, evicts a model
// still loading: the load ends, the model is stopped, and the request that
// started it fails with 500 (verified with limits of one and two). LocalAI
// applies the limit before it checks for a load under way, and counts a
// loading model twice (v4.11.0 source, ModelLoader.Load and
// WatchDog.EnforceLRULimit).
// So while /api/settings says a limit is set and the gateway has a load
// pending there, the adapter reports backend.ErrLoading, and selection sends
// LocalAI nothing until the load ends. Under a limit of one that is exact,
// since nothing else stays loaded; under a higher one, the models still
// loaded are held back too.
//
// POST /v1/tokenize counts raw text with a model's tokenizer, without its
// chat template (v4.11.0 source, TokenizeEndpoint), so the adapter is a
// backend.Tokenizer, not a backend.TokenCounter.
//
// /system, /api/aliases and /api/settings are admin endpoints, so with
// LocalAI's API keys set, apiKey must be an admin key.
package localai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openaihttp"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// Backend serves both completion endpoints through the embedded client, since
// LocalAI serves them at their OpenAI paths.
type Backend struct {
	*openaihttp.Backend
	limited atomic.Bool // the latest state fetch found a limit on loaded models, or couldn't tell
}

// New builds the adapter from its config entry.
func New(entry config.Entry) (*Backend, error) {
	b := &Backend{}
	base, err := openaihttp.NewBackend(entry, b.fetchState)
	if err != nil {
		return nil, err
	}
	b.Backend = base
	b.limited.Store(true)
	return b, nil
}

// Tokenize counts text's tokens on model through /v1/tokenize, counted as a
// pending request while it runs, since it loads the model as a chat would.
func (b *Backend) Tokenize(ctx context.Context, model, text string, client backend.Client) (int, error) {
	body, err := json.Marshal(map[string]string{"model": model, "content": text})
	if err != nil {
		return 0, err
	}
	resp, err := b.Send(ctx, backend.ChatRequest{Protocol: backend.OpenAIChat, Body: body, Client: client}, "/v1/tokenize", nil)
	if err != nil {
		return 0, err
	}
	data, err := openaihttp.ReadBody(resp)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("tokenizing for '%s': status %d: %s", model, resp.StatusCode, data)
	}
	var tokenized struct {
		Tokens []int32 `json:"tokens"`
	}
	if err := json.Unmarshal(data, &tokenized); err != nil {
		return 0, fmt.Errorf("tokenizing for '%s': %w", model, err)
	}
	return len(tokenized.Tokens), nil
}

// ListModels returns every model LocalAI has, loaded or not, with its
// aliases folded in. LocalAI lists aliases as models of their own too.
func (b *Backend) ListModels(ctx context.Context) ([]backend.ModelInfo, error) {
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := b.GetJSON(ctx, "/v1/models", &list); err != nil {
		return nil, err
	}
	aliases, err := b.aliases(ctx)
	if err != nil {
		return nil, err
	}
	var models []backend.ModelInfo
	for _, m := range list.Data {
		if _, isAlias := aliases.target[m.ID]; isAlias {
			continue
		}
		models = append(models, backend.ModelInfo{ID: m.ID, Aliases: aliases.of[m.ID], PlainAliases: true})
	}
	return models, nil
}

// aliasMap is LocalAI's aliases, both ways round.
type aliasMap struct {
	target map[string]string   // alias -> model
	of     map[string][]string // model -> aliases, in LocalAI's order
}

func (b *Backend) aliases(ctx context.Context) (aliasMap, error) {
	var list []struct {
		Name   string `json:"name"`
		Target string `json:"target"`
	}
	if err := b.GetJSON(ctx, "/api/aliases", &list); err != nil {
		return aliasMap{}, err
	}
	aliases := aliasMap{target: map[string]string{}, of: map[string][]string{}}
	for _, a := range list {
		aliases.target[a.Name] = a.Target
		aliases.of[a.Target] = append(aliases.of[a.Target], a.Name)
	}
	return aliases, nil
}

// LoadedModels returns the models LocalAI reports, followed by the pending
// models it doesn't report, as starting; or backend.ErrLoading if LocalAI
// limits its loaded models and one of those pending models is loading,
// since a request sent then would evict it (see the package comment).
func (b *Backend) LoadedModels(ctx context.Context) ([]backend.ModelInfo, error) {
	models, err := b.State.LoadedModels(ctx)
	if err != nil || !b.limited.Load() {
		return models, err
	}
	for _, m := range models {
		if m.Pending && m.Starting {
			return nil, fmt.Errorf("%w: '%s', and LocalAI's limit on loaded models would evict it for any other request for a model that isn't loaded", backend.ErrLoading, m.ID)
		}
	}
	return models, nil
}

// fetchState reads /system for the cache, with every model it lists as
// ready, since LocalAI reports no loading state, and with that model's
// aliases. It also reads whether LocalAI limits its loaded models.
func (b *Backend) fetchState(ctx context.Context) ([]backend.ModelInfo, error) {
	var system struct {
		LoadedModels []struct {
			ID string `json:"id"`
		} `json:"loaded_models"`
	}
	if err := b.GetJSON(ctx, "/system", &system); err != nil {
		return nil, err
	}
	aliases, err := b.aliases(ctx)
	if err != nil {
		return nil, err
	}
	limited, err := b.readLimit(ctx)
	if err != nil {
		return nil, err
	}
	b.limited.Store(limited)
	var models []backend.ModelInfo
	for _, m := range system.LoadedModels {
		models = append(models, backend.ModelInfo{ID: m.ID, Aliases: aliases.of[m.ID]})
	}
	return models, nil
}

// readLimit reads whether /api/settings sets a limit on loaded models. With
// LocalAI's web UI off there is no /api/settings (v4.11.0 source,
// RegisterUIAPIRoutes), and the adapter assumes a limit, since that only
// holds back requests, where assuming none could fail them.
func (b *Backend) readLimit(ctx context.Context) (bool, error) {
	var settings struct {
		MaxActiveBackends int  `json:"max_active_backends"`
		SingleBackend     bool `json:"single_backend"`
	}
	err := b.GetJSON(ctx, "/api/settings", &settings)
	var status *openaihttp.StatusError
	if errors.As(err, &status) && status.Code == http.StatusNotFound {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return settings.MaxActiveBackends > 0 || settings.SingleBackend, nil
}
