// Package backend defines the interfaces backend adapters implement.
package backend

import "context"

type ModelInfo struct {
	ID      string
	Aliases []string // other IDs the backend accepts for this model, e.g. llama-swap presets
	// Starting is set by ModelStateProvider for a model that is loading but
	// not ready yet. A request to it waits for the load to finish.
	Starting bool
}

// Catalog indexes a backend's model list for validation and display.
type Catalog struct {
	Models  []ModelInfo       // base models, as listed
	AliasOf map[string]string // alias ID -> base model ID
	byID    map[string]ModelInfo
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

// Model returns the base model with this ID.
func (c *Catalog) Model(id string) (ModelInfo, bool) {
	m, ok := c.byID[id]
	return m, ok
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
}

// ChatResponse is a complete, non-streamed response, passed through as-is.
type ChatResponse struct {
	Status      int
	ContentType string
	Body        []byte
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
	// starting with Starting set, each with its aliases. A model that is
	// stopping or stopped is not returned.
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
