// Package openaihttp is the HTTP plumbing shared by backend adapters whose
// backend serves the OpenAI completion endpoints at their usual paths: the
// client and its timeouts, auth, chat forwarding, and a cached, shared fetch
// of loaded-model state.
package openaihttp

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
)

// maxResponseBytes caps how much of a backend response body is read into
// memory (streams are relayed and not subject to it).
const maxResponseBytes = 100 << 20

// Config holds the keys every adapter built on Client accepts. Adapters embed
// it with `yaml:",inline"` next to their own `type`.
type Config struct {
	BaseURL               string  `yaml:"baseUrl"`
	APIKey                string  `yaml:"apiKey"`          // startup and state calls; chat too unless passthroughAuth
	PassthroughAuth       bool    `yaml:"passthroughAuth"` // chat requests carry the client's own Authorization
	StateCacheSeconds     float64 `yaml:"stateCacheSeconds"`
	StateTimeoutSeconds   float64 `yaml:"stateTimeoutSeconds"` // state and model-list calls; default 10
	ConnectTimeoutSeconds float64 `yaml:"connectTimeoutSeconds"`
	RequestTimeoutSeconds float64 `yaml:"requestTimeoutSeconds"` // until response headers arrive
}

type Client struct {
	baseURL      string
	apiKey       string
	passthrough  bool
	stateTimeout time.Duration
	client       *http.Client
}

func New(c Config) (*Client, error) {
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
	return &Client{
		baseURL:      strings.TrimRight(c.BaseURL, "/"),
		apiKey:       c.APIKey,
		passthrough:  c.PassthroughAuth,
		stateTimeout: stateTimeout,
		client:       &http.Client{Transport: transport},
	}, nil
}

func seconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

// Chat posts the request to its OpenAI endpoint path on the backend.
func (c *Client) Chat(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	accept := ""
	if req.Stream {
		accept = "text/event-stream"
	}
	resp, err := c.Do(ctx, http.MethodPost, string(req.Endpoint), req.Body, accept, req.Client)
	if err != nil {
		return backend.ChatResult{}, err
	}
	contentType := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if req.Stream && resp.StatusCode/100 == 2 && mediaType == "text/event-stream" {
		return backend.ChatResult{Stream: backend.NewSSEStream(resp.Body)}, nil
	}
	body, err := ReadBody(resp)
	if err != nil {
		return backend.ChatResult{}, err
	}
	return backend.ChatResult{Response: &backend.ChatResponse{Status: resp.StatusCode, ContentType: contentType, Body: body}}, nil
}

// Do sends one request made on a client's behalf, with JSON body if body
// isn't nil. With passthroughAuth it carries the client's own credential, or
// none: a keyless client never borrows the backend's apiKey.
func (c *Client) Do(ctx context.Context, method, path string, body []byte, accept string, client backend.Client) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := c.newRequest(ctx, method, path, reader)
	if err != nil {
		return nil, err
	}
	if c.passthrough {
		req.Header.Del("Authorization")
		if client.Authorization != "" {
			req.Header.Set("Authorization", client.Authorization)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return c.client.Do(req)
}

// ReadBody reads and closes a response body, refusing one over the size cap.
func ReadBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("backend response exceeds %d bytes", maxResponseBytes)
	}
	return body, nil
}

// PassthroughAuth reports whether client requests carry the client's own key.
func (c *Client) PassthroughAuth() bool {
	return c.passthrough
}

// CloseIdleConnections releases pooled connections, e.g. after a config
// reload replaces this backend.
func (c *Client) CloseIdleConnections() {
	c.client.CloseIdleConnections()
}

// GetJSON serves the state and model-list calls, which must fail fast rather
// than wait out a chat-length timeout, so it applies stateTimeout itself.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.stateTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
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

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return req, nil
}

// StateCache implements backend.ModelStateProvider's LoadedModels on top of an
// adapter's fetch function, caching the result for stateCacheSeconds.
type StateCache struct {
	ttl   time.Duration
	fetch func(ctx context.Context) ([]backend.ModelInfo, error)

	mu        sync.Mutex
	loaded    []backend.ModelInfo
	fetchedAt time.Time
	fetched   bool // loaded holds a fetched state, even once expired or invalidated
	inFlight  *stateFetch
	gen       uint64 // bumped by Invalidate; a refresh started earlier isn't cached
}

// stateFetch is one in-flight state refresh. Concurrent requests share it
// instead of queueing their own fetches behind the mutex, so one slow call to
// the backend delays each request at most once.
type stateFetch struct {
	done   chan struct{} // closed when models and err are set
	gen    uint64        // StateCache.gen when the fetch started
	models []backend.ModelInfo
	err    error
}

func NewStateCache(c Config, fetch func(ctx context.Context) ([]backend.ModelInfo, error)) *StateCache {
	return &StateCache{ttl: seconds(c.StateCacheSeconds), fetch: fetch}
}

func (s *StateCache) LoadedModels(ctx context.Context) ([]backend.ModelInfo, error) {
	s.mu.Lock()
	if !s.fetchedAt.IsZero() && time.Since(s.fetchedAt) < s.ttl {
		models := append([]backend.ModelInfo(nil), s.loaded...)
		s.mu.Unlock()
		return models, nil
	}
	f := s.inFlight
	if f != nil && !backend.StateWaitAllowed(ctx) {
		// Possibly our own refresh, looped back through another service.
		defer s.mu.Unlock()
		if !s.fetched {
			return nil, backend.ErrStateBusy
		}
		return append([]backend.ModelInfo(nil), s.loaded...), nil
	}
	if f == nil {
		f = &stateFetch{done: make(chan struct{}), gen: s.gen}
		s.inFlight = f
		go s.refresh(f)
	}
	s.mu.Unlock()
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

// refresh runs the fetch detached from any request context, so a cancelled
// client doesn't fail the requests waiting on the same refresh.
func (s *StateCache) refresh(f *stateFetch) {
	models, err := s.fetch(context.Background())
	s.mu.Lock()
	if err == nil && f.gen == s.gen {
		s.loaded = models
		s.fetchedAt = time.Now()
		s.fetched = true
	}
	if s.inFlight == f {
		s.inFlight = nil
	}
	f.models, f.err = models, err
	s.mu.Unlock()
	close(f.done)
}

// Invalidate drops the cached state, e.g. when the adapter knows a model has
// just finished loading. The next LoadedModels fetches anew rather than join
// a refresh already under way, which may predate the change.
func (s *StateCache) Invalidate() {
	s.mu.Lock()
	s.fetchedAt = time.Time{}
	s.inFlight = nil
	s.gen++
	s.mu.Unlock()
}
