// Package llamaswap is the backend adapter for llama-swap. Loaded-model state
// comes from GET /running, where only models in state "ready" count.
package llamaswap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// maxResponseBytes caps how much of a backend response body is read into
// memory (streams are relayed and not subject to it).
const maxResponseBytes = 100 << 20

type Config struct {
	Type                  string  `yaml:"type"`
	BaseURL               string  `yaml:"baseUrl"`
	APIKey                string  `yaml:"apiKey"`          // startup and state calls; chat too unless passthroughAuth
	PassthroughAuth       bool    `yaml:"passthroughAuth"` // chat requests carry the client's own Authorization
	StateCacheSeconds     float64 `yaml:"stateCacheSeconds"`
	StateTimeoutSeconds   float64 `yaml:"stateTimeoutSeconds"` // /running and /v1/models calls; default 10
	ConnectTimeoutSeconds float64 `yaml:"connectTimeoutSeconds"`
	RequestTimeoutSeconds float64 `yaml:"requestTimeoutSeconds"` // until response headers arrive
}

type Backend struct {
	baseURL      string
	apiKey       string
	passthrough  bool
	cacheTTL     time.Duration
	stateTimeout time.Duration
	client       *http.Client

	mu        sync.Mutex
	loaded    []backend.ModelInfo
	fetchedAt time.Time
	fetch     *stateFetch
}

// stateFetch is one in-flight /running refresh. Concurrent requests share it
// instead of queueing their own fetches behind the mutex, so one slow call to
// llama-swap delays each request at most once.
type stateFetch struct {
	done   chan struct{} // closed when models and err are set
	models []backend.ModelInfo
	err    error
}

// New builds the adapter from its config entry.
func New(entry config.Entry) (*Backend, error) {
	var c Config
	if err := config.DecodeStrict(entry.Node, &c); err != nil {
		return nil, err
	}
	if c.BaseURL == "" {
		return nil, fmt.Errorf("baseUrl is required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: seconds(c.ConnectTimeoutSeconds)}).DialContext
	transport.ResponseHeaderTimeout = seconds(c.RequestTimeoutSeconds)
	transport.MaxIdleConnsPerHost = 8 // keep a few connections warm for parallel requests
	stateTimeout := seconds(c.StateTimeoutSeconds)
	if stateTimeout == 0 {
		stateTimeout = 10 * time.Second
	}
	return &Backend{
		baseURL:      strings.TrimRight(c.BaseURL, "/"),
		apiKey:       c.APIKey,
		passthrough:  c.PassthroughAuth,
		cacheTTL:     seconds(c.StateCacheSeconds),
		stateTimeout: stateTimeout,
		client:       &http.Client{Transport: transport},
	}, nil
}

func seconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
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
	if err := b.getJSON(ctx, "/v1/models", &list); err != nil {
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
	b.mu.Lock()
	if !b.fetchedAt.IsZero() && time.Since(b.fetchedAt) < b.cacheTTL {
		models := append([]backend.ModelInfo(nil), b.loaded...)
		b.mu.Unlock()
		return models, nil
	}
	f := b.fetch
	if f == nil {
		f = &stateFetch{done: make(chan struct{})}
		b.fetch = f
		go b.fetchState(f)
	}
	b.mu.Unlock()
	select {
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		return append([]backend.ModelInfo(nil), f.models...), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetchState refreshes the loaded-model cache, detached from any request
// context so a cancelled client doesn't fail the requests waiting on the
// same refresh. Readiness comes from /running, which lists base models only;
// their aliases come from /v1/models.
func (b *Backend) fetchState(f *stateFetch) {
	var running struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	err := b.getJSON(context.Background(), "/running", &running)
	var listed []backend.ModelInfo
	if err == nil {
		listed, err = b.ListModels(context.Background())
	}
	var models []backend.ModelInfo
	if err == nil {
		aliases := map[string][]string{}
		for _, m := range listed {
			aliases[m.ID] = m.Aliases
		}
		for _, r := range running.Running {
			if r.State == "ready" {
				models = append(models, backend.ModelInfo{ID: r.Model, Aliases: aliases[r.Model]})
			}
		}
	}
	b.mu.Lock()
	if err == nil {
		b.loaded = models
		b.fetchedAt = time.Now()
	}
	b.fetch = nil
	f.models, f.err = models, err
	b.mu.Unlock()
	close(f.done)
}

func (b *Backend) Chat(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	// llama-swap serves both endpoints at their OpenAI paths.
	httpReq, err := b.newRequest(ctx, http.MethodPost, string(req.Endpoint), bytes.NewReader(req.Body))
	if err != nil {
		return backend.ChatResult{}, err
	}
	if b.passthrough {
		// The client's own credential, or none: a keyless client never
		// borrows the backend's apiKey.
		httpReq.Header.Del("Authorization")
		if req.Client.Authorization != "" {
			httpReq.Header.Set("Authorization", req.Client.Authorization)
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return backend.ChatResult{}, err
	}
	contentType := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if req.Stream && resp.StatusCode/100 == 2 && mediaType == "text/event-stream" {
		return backend.ChatResult{Stream: backend.NewSSEStream(resp.Body)}, nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return backend.ChatResult{}, err
	}
	if len(body) > maxResponseBytes {
		return backend.ChatResult{}, fmt.Errorf("backend response exceeds %d bytes", maxResponseBytes)
	}
	return backend.ChatResult{Response: &backend.ChatResponse{Status: resp.StatusCode, ContentType: contentType, Body: body}}, nil
}

// getJSON serves the state and model-list calls, which must fail fast rather
// than wait out a chat-length timeout, so it applies stateTimeout itself.
func (b *Backend) getJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, b.stateTimeout)
	defer cancel()
	req, err := b.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

func (b *Backend) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
	return req, nil
}
