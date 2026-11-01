// Package anthropic is the backend adapter for Anthropic's Claude API,
// through its Messages API. Clients speak Chat Completions, so the adapter
// translates each request into a Messages request and each response, whole
// or streamed, back into a chat completion; the translations are in
// request.go, response.go and stream.go. Bodies are rebuilt, not patched,
// so this adapter, like the Open WebUI one, is an exception to bodies
// passing through as raw bytes; a key with no translation still passes on
// unchanged, so a facade's params can set Messages fields such as thinking,
// output_config or cache_control.
//
// Model IDs come from GET /v1/models, read a page at a time, which needs an
// anthropic-version header, as /v1/messages does (verified: the API refuses
// either call without one, though its documentation lists it as optional).
// The adapter sends 2023-06-01; 2026-10-21 refuses temperature, top_p and
// top_k on every model, which Chat Completions clients send routinely. Each
// listed model's max_tokens is its output cap, which a request without
// max_tokens is given, since Messages requires one and Chat Completions
// doesn't (verified: a non-streamed request at the full cap is accepted).
// The Claude API has no legacy Completions, so the adapter refuses that
// endpoint.
//
// A hosted model needs no load, so every listed model is ready while the API
// answers, and a state read answers from the latest list without calling the
// API; a failed request, or a list older than stateCacheSeconds, makes the
// next state read list the API again (openaihttp.ListedState).
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openaihttp"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// Backend sends chats to the Messages API through the embedded client's
// Send, translating them both ways.
type Backend struct {
	*openaihttp.Backend
	listed     *openaihttp.ListedState
	outputCaps atomic.Pointer[map[string]int64] // model -> max_tokens, from the latest list
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

// Chat translates a chat completion into a Messages request and sends it,
// and translates what comes back: a response or a stream, or an error in
// OpenAI's shape with the API's status. A request the Messages API can't
// serve as meant gets a 400 without being sent.
func (b *Backend) Chat(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	if req.Endpoint != backend.ChatCompletions {
		return backend.ChatResult{}, fmt.Errorf("the Claude API serves only %s, not %s", backend.ChatCompletions, req.Endpoint)
	}
	body, includeUsage, err := toMessages(req.Body, b.outputCap)
	var refused *requestError
	if errors.As(err, &refused) {
		return jsonResponse(http.StatusBadRequest, errorBody("invalid_request_error", refused.msg), ""), nil
	}
	if err != nil {
		return backend.ChatResult{}, err
	}
	req.Body = body
	resp, err := b.Send(ctx, req, "/v1/messages", apiVersion)
	if err != nil {
		return backend.ChatResult{}, err
	}
	created := time.Now().Unix()
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if req.Stream && resp.StatusCode/100 == 2 && mediaType == "text/event-stream" {
		return backend.ChatResult{Stream: newChunkStream(backend.NewSSEStream(resp.Body), includeUsage, created)}, nil
	}
	data, err := openaihttp.ReadBody(resp)
	if err != nil {
		return backend.ChatResult{}, err
	}
	if resp.StatusCode/100 != 2 {
		return jsonResponse(resp.StatusCode, openAIError(data), resp.Header.Get("Retry-After")), nil
	}
	return jsonResponse(resp.StatusCode, completion(gjson.ParseBytes(data), created), ""), nil
}

func jsonResponse(status int, body []byte, retryAfter string) backend.ChatResult {
	return backend.ChatResult{Response: &backend.ChatResponse{Status: status, ContentType: "application/json", Body: body, RetryAfter: retryAfter}}
}

func errorBody(errType, message string) []byte {
	data, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": errType}})
	return data
}

// outputCap is model's max_tokens in the latest list.
func (b *Backend) outputCap(model string) (int64, bool) {
	caps := b.outputCaps.Load()
	if caps == nil {
		return 0, false
	}
	limit, ok := (*caps)[model]
	return limit, ok && limit > 0
}

// ListModels returns every model the API lists, and keeps them as its state.
func (b *Backend) ListModels(ctx context.Context) ([]backend.ModelInfo, error) {
	return b.listed.List(ctx)
}

// apiVersion is the Claude API version every call is made at; see the
// package comment for why not a later one.
var apiVersion = http.Header{"Anthropic-Version": {"2023-06-01"}}

// list reads every page of /v1/models, the largest the API allows, so most
// accounts need one call, and keeps each model's output cap.
func (b *Backend) list(ctx context.Context) ([]backend.ModelInfo, error) {
	var models []backend.ModelInfo
	caps := map[string]int64{}
	after := ""
	for {
		path := "/v1/models?limit=1000"
		if after != "" {
			path += "&after_id=" + url.QueryEscape(after)
		}
		var page struct {
			Data []struct {
				ID        string `json:"id"`
				MaxTokens int64  `json:"max_tokens"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := b.GetJSONWith(ctx, path, apiVersion, &page); err != nil {
			return nil, err
		}
		for _, m := range page.Data {
			models = append(models, backend.ModelInfo{ID: m.ID})
			caps[m.ID] = m.MaxTokens
		}
		if !page.HasMore || page.LastID == "" {
			b.outputCaps.Store(&caps)
			return models, nil
		}
		after = page.LastID
	}
}
