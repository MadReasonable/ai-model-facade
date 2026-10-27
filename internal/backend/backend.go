// Package backend defines the interfaces backend adapters implement.
package backend

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

type ModelInfo struct {
	ID      string
	Aliases []string // other IDs the backend accepts for this model, e.g. llama-swap presets
	// Starting is set by ModelStateProvider for a model that is loading but
	// not ready yet. A request to it waits for the load to finish.
	Starting bool
	// Unknown is set by ModelStateProvider for a model whose state the
	// backend can't read. It selects like a model that isn't loaded.
	Unknown bool
	// Pending is set by ModelStateProvider on a model the gateway has a
	// request to that hasn't been answered yet, whatever its state.
	Pending bool
	// Preset is set in ListModels for a model that is listed on its own but
	// that the backend treats like an alias, applying params of its own after
	// the gateway's: a llama-swap preset that Open WebUI lists without its
	// base model.
	Preset bool
	// PlainAliases is set in ListModels for a model whose aliases are only
	// other names for it, with no params of their own: llama-server's.
	PlainAliases bool
}

// Catalog indexes a backend's model list for validation and display.
type Catalog struct {
	Models  []ModelInfo       // base models, as listed
	AliasOf map[string]string // alias ID -> base model ID
	// Unavailable marks an empty catalog for a backend that couldn't be
	// listed, so nothing can be checked against it.
	Unavailable bool
	byID        map[string]ModelInfo
}

func NewCatalog(models []ModelInfo) *Catalog {
	c := &Catalog{Models: models, AliasOf: map[string]string{}, byID: map[string]ModelInfo{}}
	for _, m := range models {
		c.byID[m.ID] = m
		for _, a := range m.Aliases {
			c.AliasOf[a] = m.ID
		}
	}
	return c
}

// Contains reports whether id is a listed model or alias.
func (c *Catalog) Contains(id string) bool {
	_, isModel := c.byID[id]
	_, isAlias := c.AliasOf[id]
	return isModel || isAlias
}

// WithTags returns the listed IDs, models and aliases, that are id followed
// by ":" and a tag, sorted: "llama3:8b" and "llama3:latest" for "llama3".
func (c *Catalog) WithTags(id string) []string {
	var ids []string
	for listed := range c.byID {
		if strings.HasPrefix(listed, id+":") {
			ids = append(ids, listed)
		}
	}
	for alias := range c.AliasOf {
		if strings.HasPrefix(alias, id+":") {
			ids = append(ids, alias)
		}
	}
	slices.Sort(ids)
	return ids
}

// Model returns the base model with this ID.
func (c *Catalog) Model(id string) (ModelInfo, bool) {
	m, ok := c.byID[id]
	return m, ok
}

// OwnParams reports whether the backend applies params of its own to
// requests for id, after the gateway's: an alias, unless its base model's
// aliases are plain names, or a model marked Preset.
func (c *Catalog) OwnParams(id string) bool {
	if base, isAlias := c.AliasOf[id]; isAlias {
		return !c.byID[base].PlainAliases
	}
	return c.byID[id].Preset
}

// ListingCheck is a check against the model lists of some backends, such as
// a candidate ID that its backend must list. It depends on those backends
// being up and current, so it runs once every one of them has been listed:
// at startup, or later, once a backend that couldn't be listed answers.
type ListingCheck struct {
	Backends []string
	Run      func(catalogs map[string]*Catalog, warn func(error))
}

// Client identifies the caller.
type Client struct {
	// Name is the matching server.apiKeys entry, "unknown" for an unlisted
	// key, or "" when the request carried no key.
	Name string
	// Fingerprint distinguishes unlisted keys in the log without revealing
	// them: the first 8 hex characters of the key's SHA-256.
	Fingerprint string
	// Authorization is the client's Authorization header, as sent. Only
	// backends with passthroughAuth forward it; never log it.
	Authorization string
	// Route lists the hops the request has made, "<gateway ID>/<virtual
	// model>", ending with this one. Only adapters that forward to something
	// that may be a gateway (Open WebUI) send it on, as RouteField in the body.
	Route []string
}

// RouteField is the request-body key that carries Client.Route between
// gateways, so a gateway that finds its own hop in it can refuse a routing loop.
const RouteField = "ai_model_facade_route"

type noStateWaitKey struct{}

// WithoutStateWait marks ctx so that LoadedModels doesn't wait for a state
// refresh already in flight: it answers from the last cached state, or fails
// with ErrStateBusy. /v1/models uses it, because building `current` can loop
// back into this gateway through Open WebUI, and the nested call would
// otherwise wait on the refresh that is waiting for it.
func WithoutStateWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, noStateWaitKey{}, true)
}

// WithStateWait undoes WithoutStateWait for a state read that can't loop back
// into this gateway, such as llama-swap's.
func WithStateWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, noStateWaitKey{}, nil)
}

// StateWaitAllowed reports whether ctx may wait for a refresh in flight.
func StateWaitAllowed(ctx context.Context) bool {
	return ctx.Value(noStateWaitKey{}) == nil
}

var ErrStateBusy = errors.New("backend state refresh already in flight and nothing cached")

// ErrLoading is wrapped in a ModelStateProvider's error by a backend whose
// state read fails because it is loading a model and serves nothing until it
// has, whoever started the load: single-model llama-server answers 503 then.
// Selection treats the backend as busy: none of its candidates is used, the
// loadIfNone fallback included, since a request sent then would fail, and a
// request that finds nothing else gets 503.
var ErrLoading = errors.New("busy loading a model")

// LoadsOneAtATime is implemented by a backend whose load of one model
// aborts a load of another under way, failing the request that started it,
// and which lists a model as loaded from the moment its load begins (LM
// Studio). So while a model there has a request of the gateway's pending,
// selection uses none of its candidates for another model, whatever their
// state.
type LoadsOneAtATime interface {
	LoadsOneAtATime()
}

type requestStartKey struct{}

// WithRequestStart marks ctx with the time its request arrived. A state
// fetched since then is fresh enough for that request, whatever
// stateCacheSeconds says, so one request never reads a backend's state
// twice. That includes a request whose state read makes Open WebUI call
// this gateway back: the nested request arrives later, and what it fetches
// serves the outer request too.
func WithRequestStart(ctx context.Context, start time.Time) context.Context {
	return context.WithValue(ctx, requestStartKey{}, start)
}

// RequestStart returns the time WithRequestStart set on ctx, or the zero
// time.
func RequestStart(ctx context.Context) time.Time {
	start, _ := ctx.Value(requestStartKey{}).(time.Time)
	return start
}

type stateSnapshotKey struct{}

// stateSnapshot holds the first state each backend reported under one ctx.
type stateSnapshot struct {
	mu    sync.Mutex
	reads map[ModelStateProvider]*stateRead
}

type stateRead struct {
	once   sync.Once
	models []ModelInfo
	err    error
}

// WithStateSnapshot marks ctx so that LoadedModels reads each backend's state
// at most once under it, and later reads get the same answer. /v1/models uses
// it: its virtual models are selected one after another, so without it a
// listing would refresh each backend once per virtual model when the state
// isn't cached, and two entries could be judged against different states.
func WithStateSnapshot(ctx context.Context) context.Context {
	return context.WithValue(ctx, stateSnapshotKey{}, &stateSnapshot{reads: map[ModelStateProvider]*stateRead{}})
}

// LoadedModels returns p's state, from ctx's snapshot if it has one.
func LoadedModels(ctx context.Context, p ModelStateProvider) ([]ModelInfo, error) {
	s, _ := ctx.Value(stateSnapshotKey{}).(*stateSnapshot)
	if s == nil {
		return p.LoadedModels(ctx)
	}
	s.mu.Lock()
	r := s.reads[p]
	if r == nil {
		r = &stateRead{}
		s.reads[p] = r
	}
	s.mu.Unlock()
	r.once.Do(func() { r.models, r.err = p.LoadedModels(ctx) })
	return slices.Clone(r.models), r.err
}

// Endpoint is the OpenAI API endpoint a request was made to.
type Endpoint string

const (
	ChatCompletions Endpoint = "/v1/chat/completions"
	Completions     Endpoint = "/v1/completions" // legacy text completions
)

// ChatRequest is a completion request for either endpoint; both share the
// same body shape (`model`, `stream`) and response framing.
type ChatRequest struct {
	Endpoint Endpoint
	Body     []byte // raw JSON, params applied and model rewritten
	Stream   bool
	Client   Client
	AliasOf  string // the base model when the body's model is an alias
}

// StateModel is the model whose load the request waits for: the base model
// when the body names an alias, since loading an alias loads its base.
func (r ChatRequest) StateModel() string {
	if r.AliasOf != "" {
		return r.AliasOf
	}
	return gjson.GetBytes(r.Body, "model").String()
}

// ChatResponse is a complete, non-streamed response. Its status, content
// type and body are passed on unchanged; of the backend's other headers,
// only Retry-After is kept.
type ChatResponse struct {
	Status      int
	ContentType string
	Body        []byte
	// RetryAfter is the backend's Retry-After header, the one header passed
	// on besides Content-Type: it tells a client refused with 429 or 503
	// when to try again, and reveals nothing about the backend.
	RetryAfter string
}

// ChatStream yields the data payloads of a server-sent event stream.
type ChatStream interface {
	// Next returns the next event's data payload. It returns io.EOF after the
	// stream's normal end and any other error if the stream broke.
	Next() ([]byte, error)
	Close() error
}

// ChatResult carries exactly one of Response or Stream.
type ChatResult struct {
	Response *ChatResponse
	Stream   ChatStream
}

type Backend interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
	Chat(ctx context.Context, req ChatRequest) (ChatResult, error)
}

// ModelStateProvider is an optional backend capability.
type ModelStateProvider interface {
	// LoadedModels returns the models that are ready, plus those still
	// starting with Starting set and those whose state can't be read with
	// Unknown set, each with its aliases. A model that is stopping or
	// stopped is not returned.
	LoadedModels(ctx context.Context) ([]ModelInfo, error)
}

type Capability string

const ModelState Capability = "modelState"

// Has reports whether b provides capability c.
func Has(b Backend, c Capability) bool {
	switch c {
	case ModelState:
		_, ok := b.(ModelStateProvider)
		return ok
	}
	return false
}
