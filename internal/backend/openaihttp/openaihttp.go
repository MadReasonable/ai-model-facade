// Package openaihttp is the HTTP plumbing shared by backend adapters whose
// backend serves the OpenAI completion endpoints at their usual paths: the
// client and its timeouts, auth, chat forwarding, and a cached, shared fetch
// of loaded-model state.
package openaihttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/protocols"
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
	LoopsBack             bool    `yaml:"loopsBack"`           // a state read can reach this gateway's /v1/models
	ListRetrySeconds      float64 `yaml:"listRetrySeconds"`    // asking again for the models of a backend that couldn't be listed; default 30
	ConnectTimeoutSeconds float64 `yaml:"connectTimeoutSeconds"`
	RequestTimeoutSeconds float64 `yaml:"requestTimeoutSeconds"` // until response headers arrive
}

type Client struct {
	baseURL      string
	apiKey       string
	passthrough  bool
	stateTimeout time.Duration
	listRetry    time.Duration
	client       *http.Client
	state        *StateCache // set by trackPending
	failed       func()      // set by NewListedBackend: a request got no answer or a 5xx
}

func NewClient(c Config) (*Client, error) {
	if c.BaseURL == "" {
		return nil, fmt.Errorf("baseUrl is required")
	}
	if c.ListRetrySeconds < 0 {
		return nil, fmt.Errorf("listRetrySeconds must not be negative")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: seconds(c.ConnectTimeoutSeconds)}).DialContext
	transport.ResponseHeaderTimeout = seconds(c.RequestTimeoutSeconds)
	transport.MaxIdleConnsPerHost = 8 // keep a few connections warm for parallel requests
	stateTimeout := seconds(c.StateTimeoutSeconds)
	if stateTimeout == 0 {
		stateTimeout = 10 * time.Second
	}
	listRetry := seconds(c.ListRetrySeconds)
	if listRetry == 0 {
		listRetry = 30 * time.Second
	}
	return &Client{
		baseURL:      strings.TrimRight(c.BaseURL, "/"),
		apiKey:       c.APIKey,
		passthrough:  c.PassthroughAuth,
		stateTimeout: stateTimeout,
		listRetry:    listRetry,
		client:       &http.Client{Transport: transport},
	}, nil
}

// ListRetry is how often the gateway asks this backend for its models again
// after it couldn't be listed, to run the checks that need them.
func (c *Client) ListRetry() time.Duration {
	return c.listRetry
}

func seconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

// trackPending makes Chat count each request as pending in s until its
// response headers arrive, so s reports the model as starting meanwhile.
// Ollama, llama-swap, llama-server and LM Studio send headers once the
// model is loaded; a backend that sends them earlier ends the count early,
// and its own state takes over.
func (c *Client) trackPending(s *StateCache) {
	c.state = s
}

// Backend is the base of an adapter that forwards chats unchanged through
// Client and reads loaded-model state through a StateCache. Every chat
// counts as pending in the cache until its response headers arrive, so a
// load the gateway started shows as starting even where the backend doesn't
// report it (see trackPending).
type Backend struct {
	*Client
	State *StateCache
}

// NewBackend builds a Backend from a config entry that takes Config's keys
// and its type, with fetch reading the backend's state for the cache. fetch
// isn't called before NewBackend returns, so it may be a method of the
// adapter that embeds the result.
func NewBackend(entry config.Entry, fetch func(ctx context.Context) ([]backend.ModelInfo, error)) (*Backend, error) {
	c, err := decodeEntry(entry)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(c)
	if err != nil {
		return nil, err
	}
	state := NewStateCache(c, fetch)
	client.trackPending(state)
	return &Backend{Client: client, State: state}, nil
}

// NewListedBackend builds a Backend for an API that loads nothing, such as a
// hosted one, whose state is a ListedState on list. A request to it that
// gets no answer or a 5xx drops the list and the cached state, so the next
// state read finds out whether the API still answers.
func NewListedBackend(entry config.Entry, list func(ctx context.Context) ([]backend.ModelInfo, error)) (*Backend, *ListedState, error) {
	c, err := decodeEntry(entry)
	if err != nil {
		return nil, nil, err
	}
	client, err := NewClient(c)
	if err != nil {
		return nil, nil, err
	}
	listed := &ListedState{maxAge: seconds(c.StateCacheSeconds), list: list}
	state := NewStateCache(c, listed.State)
	client.trackPending(state)
	client.failed = func() {
		listed.Drop()
		state.Invalidate()
	}
	return &Backend{Client: client, State: state}, listed, nil
}

// decodeEntry decodes a config entry that takes Config's keys and its type.
func decodeEntry(entry config.Entry) (Config, error) {
	var c struct {
		Type   string `yaml:"type"`
		Config `yaml:",inline"`
	}
	err := config.DecodeStrict(entry.Node, &c)
	return c.Config, err
}

// LoadedModels returns the models the backend reports, followed by the
// pending models it doesn't report, as starting.
func (b *Backend) LoadedModels(ctx context.Context) ([]backend.ModelInfo, error) {
	return b.State.LoadedModels(ctx)
}

// Pending counts a request for model as pending in the backend's state
// cache, for a request that reaches the backend another way, such as through
// Open WebUI; see StateCache.Pending.
func (b *Backend) Pending(model string) (release func()) {
	return b.State.Pending(model)
}

// Protocols returns what Chat forwards. An adapter that shadows Chat shadows
// this too, since what it accepts is no longer what the backend serves.
func (c *Client) Protocols() []backend.Protocol {
	return []backend.Protocol{backend.OpenAIChat, backend.OpenAICompletions}
}

// Chat posts the request to its protocol's path on the backend.
func (c *Client) Chat(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	resp, err := c.Send(ctx, req, protocols.Of(req.Protocol).Path, nil)
	if err != nil {
		return backend.ChatResult{}, err
	}
	contentType := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if req.Stream && resp.StatusCode/100 == 2 && mediaType == "text/event-stream" {
		return backend.ChatResult{Stream: protocols.Stream(req.Protocol, resp.Body)}, nil
	}
	body, err := ReadBody(resp)
	if err != nil {
		return backend.ChatResult{}, err
	}
	return backend.ChatResult{Response: &backend.ChatResponse{Status: resp.StatusCode, ContentType: contentType, Body: body, RetryAfter: resp.Header.Get("Retry-After")}}, nil
}

// Send posts the request's body to path with these extra headers, as Chat
// does for its own endpoint: counted as pending until the response headers
// arrive, and reported to NewListedBackend's hook if the backend fails it. An
// adapter whose backend has its own chat endpoint, such as the Claude API's
// Messages, sends its translated body through it.
func (c *Client) Send(ctx context.Context, req backend.ChatRequest, path string, header http.Header) (*http.Response, error) {
	accept := ""
	if req.Stream {
		accept = "text/event-stream"
	}
	release := c.state.Pending(req.StateModel())
	resp, err := c.do(ctx, http.MethodPost, path, req.Body, accept, header, req.Client)
	release()
	// A client that left isn't the backend failing.
	if c.failed != nil && (err != nil && !errors.Is(err, context.Canceled) || err == nil && resp.StatusCode >= 500) {
		c.failed()
	}
	return resp, err
}

// Do sends one request made on a client's behalf, with JSON body if body
// isn't nil. With passthroughAuth it carries the client's own credential, or
// none: a keyless client never borrows the backend's apiKey.
func (c *Client) Do(ctx context.Context, method, path string, body []byte, accept string, client backend.Client) (*http.Response, error) {
	return c.do(ctx, method, path, body, accept, nil, client)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, accept string, header http.Header, client backend.Client) (*http.Response, error) {
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
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.client.Do(req)
	return resp, plainURLError(err)
}

// urlError is a *url.Error reworded with the URL in single quotes. Go's own
// formatting double-quotes it, which logs and JSON bodies then escape.
type urlError struct {
	op, url string
	err     error
}

func (e *urlError) Error() string { return e.op + " '" + e.url + "': " + e.err.Error() }
func (e *urlError) Unwrap() error { return e.err }

// plainURLError rewords a *url.Error from the HTTP client; the cause stays
// wrapped, so errors.Is still finds timeouts and cancellations.
func plainURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	return &urlError{op: strings.ToUpper(ue.Op), url: ue.URL, err: ue.Err}
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
	return c.GetJSONWith(ctx, path, nil, out)
}

// GetJSONWith is GetJSON with extra request headers, for a backend whose own
// endpoints need them, such as the Claude API's anthropic-version.
func (c *Client) GetJSONWith(ctx context.Context, path string, header http.Header, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.stateTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return plainURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &StatusError{Path: path, Code: resp.StatusCode, Status: resp.Status}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

// StatusError is GetJSON's error for a response other than 200, so an
// adapter can tell a status that describes the backend's state, such as
// llama-server's 503 while it loads, from a failure.
type StatusError struct {
	Path   string
	Code   int
	Status string // as sent, e.g. "503 Service Unavailable"
}

func (e *StatusError) Error() string { return "GET " + e.Path + ": " + e.Status }

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

// ListedState is the state of a backend that loads nothing, such as a hosted
// API: every model it lists is ready while it answers. A state read answers
// from the latest list, so a request costs a healthy API no call of its own.
// The list is read again when a request to the backend fails (Drop, wired by
// NewListedBackend), and when it is older than stateCacheSeconds, if that is
// set, so a backend that has gone away stops reading as ready. A listing
// that fails leaves no list, so the backend reads as unavailable, and
// selection passes over its candidates, until a listing succeeds; meanwhile
// each state read lists again.
type ListedState struct {
	maxAge time.Duration // 0: never read again because of its age
	list   func(ctx context.Context) ([]backend.ModelInfo, error)
	latest atomic.Pointer[listing]
}

// listing is one successful list and when it was read.
type listing struct {
	models []backend.ModelInfo
	at     time.Time
}

// List calls the backend for its models and keeps them as its state, or
// keeps no list if the call fails.
func (l *ListedState) List(ctx context.Context) ([]backend.ModelInfo, error) {
	models, err := l.list(ctx)
	if err != nil {
		l.latest.Store(nil)
		return nil, err
	}
	l.latest.Store(&listing{models: models, at: time.Now()})
	return models, nil
}

// State returns the latest list, or lists the backend if there is none or it
// is too old.
func (l *ListedState) State(ctx context.Context) ([]backend.ModelInfo, error) {
	if latest := l.latest.Load(); latest != nil && (l.maxAge == 0 || time.Since(latest.at) < l.maxAge) {
		return latest.models, nil
	}
	return l.List(ctx)
}

// Drop forgets the list, so the next state read lists the backend again.
func (l *ListedState) Drop() {
	l.latest.Store(nil)
}

// StateCache implements backend.ModelStateProvider's LoadedModels on top of an
// adapter's fetch function, caching the result for stateCacheSeconds. Each
// read also reports the models of requests still pending (see Pending) as
// starting where the backend doesn't report them: a backend may list a model
// only once loaded, and a cached state can predate a load the gateway began.
type StateCache struct {
	// loopsBack says a fetch can end up calling this gateway's /v1/models, so
	// a read under backend.WithoutStateWait mustn't wait on it. The gateway
	// can't tell what a backend's server calls, so this comes from the config.
	loopsBack bool

	ttl   time.Duration
	fetch func(ctx context.Context) ([]backend.ModelInfo, error)

	mu        sync.Mutex
	pending   map[string]int // model -> requests sent and not yet released
	loaded    []backend.ModelInfo
	fetchedAt time.Time
	startedAt time.Time // when the fetch behind loaded began
	fetched   bool      // loaded holds a fetched state, even once expired or invalidated
	inFlight  *stateFetch
	gen       uint64 // bumped by Invalidate; a refresh started earlier isn't cached

	// waiting, if set, is called by each read that is about to wait for a
	// fetch, so a test can tell when concurrent readers have all joined one.
	waiting func()
}

// stateFetch is one in-flight state refresh. Concurrent requests share it
// instead of queueing their own fetches behind the mutex, so one slow call to
// the backend delays each request at most once.
type stateFetch struct {
	done    chan struct{} // closed when models and err are set
	gen     uint64        // StateCache.gen when the fetch started
	started time.Time
	models  []backend.ModelInfo
	err     error
}

func NewStateCache(c Config, fetch func(ctx context.Context) ([]backend.ModelInfo, error)) *StateCache {
	return &StateCache{loopsBack: c.LoopsBack, ttl: seconds(c.StateCacheSeconds), fetch: fetch, pending: map[string]int{}}
}

// Pending counts a request for model as pending until the returned release
// is called, which may be called more than once. On release, if the cached
// state didn't list model as ready, a load has just finished or failed, and
// may have evicted other models, so the cached state is dropped. On a nil
// StateCache it does nothing.
func (s *StateCache) Pending(model string) (release func()) {
	if s == nil {
		return func() {}
	}
	s.mu.Lock()
	s.pending[model]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.pending[model]--; s.pending[model] == 0 {
				delete(s.pending, model)
			}
			ready := slices.ContainsFunc(s.loaded, func(m backend.ModelInfo) bool {
				return !m.Starting && !m.Unknown && (m.ID == model || slices.Contains(m.Aliases, model))
			})
			s.mu.Unlock()
			if !ready {
				s.Invalidate()
			}
		})
	}
}

// pendingModels lists the models with requests pending.
func (s *StateCache) pendingModels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var models []string
	for id := range s.pending {
		models = append(models, id)
	}
	return models
}

// addPending reports each pending model that models doesn't list, or lists as
// unknown, as starting, and marks every pending model Pending. A model the
// backend reports ready or starting keeps that state. A pending alias counts
// as its base model.
func addPending(models []backend.ModelInfo, pending []string) []backend.ModelInfo {
	slices.Sort(pending)
	for _, id := range slices.Compact(pending) {
		i := slices.IndexFunc(models, func(m backend.ModelInfo) bool { return m.ID == id || slices.Contains(m.Aliases, id) })
		switch {
		case i < 0:
			models = append(models, backend.ModelInfo{ID: id, Starting: true, Pending: true})
		case models[i].Unknown:
			models[i].Unknown, models[i].Starting, models[i].Pending = false, true, true
		default:
			models[i].Pending = true
		}
	}
	return models
}

// LoadedModels returns the cached state if it is younger than
// stateCacheSeconds, or was fetched since ctx's request arrived
// (backend.WithRequestStart), and fetches it otherwise.
func (s *StateCache) LoadedModels(ctx context.Context) ([]backend.ModelInfo, error) {
	return s.LoadedModelsAdjusted(ctx, nil)
}

// Adjust changes a backend's state at read time, before pending requests are
// added.
type Adjust func(ctx context.Context, models []backend.ModelInfo) ([]backend.ModelInfo, error)

// LoadedModelsAdjusted is LoadedModels with adjust, if not nil, applied to
// the backend's state on every read: the links of an openWebUi backend apply
// each linked backend's current state to a cached Open WebUI list.
func (s *StateCache) LoadedModelsAdjusted(ctx context.Context, adjust Adjust) ([]backend.ModelInfo, error) {
	// Pending before and after the read: a load that finishes during it is
	// still in the first set, one that starts during it is in the second.
	pending := s.pendingModels()
	models, err := s.cached(ctx)
	if err != nil {
		return nil, err
	}
	if adjust != nil {
		if models, err = adjust(ctx, models); err != nil {
			return nil, err
		}
	}
	return addPending(models, append(pending, s.pendingModels()...)), nil
}

// cached returns a copy of the backend's own state, cached or fetched.
func (s *StateCache) cached(ctx context.Context) ([]backend.ModelInfo, error) {
	since := backend.RequestStart(ctx)
	s.mu.Lock()
	if !s.fetchedAt.IsZero() && (time.Since(s.fetchedAt) < s.ttl || !since.IsZero() && !s.startedAt.Before(since)) {
		models := append([]backend.ModelInfo(nil), s.loaded...)
		s.mu.Unlock()
		return models, nil
	}
	f := s.inFlight
	if f != nil && s.loopsBack && !backend.StateWaitAllowed(ctx) {
		// Possibly our own refresh, looped back through another service.
		defer s.mu.Unlock()
		if !s.fetched {
			return nil, backend.ErrStateBusy
		}
		return append([]backend.ModelInfo(nil), s.loaded...), nil
	}
	if f == nil {
		f = &stateFetch{done: make(chan struct{}), gen: s.gen, started: time.Now()}
		s.inFlight = f
		go s.refresh(f)
	}
	waiting := s.waiting
	s.mu.Unlock()
	if waiting != nil {
		waiting()
	}
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
		s.startedAt = f.started
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
