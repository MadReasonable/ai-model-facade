// Package virtualmodel holds the virtual model types. Each type has its own
// schema, validator and handler; an entry picks one with `type`.
package virtualmodel

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// Error is a request failure with the HTTP status and OpenAI error type to report.
type Error struct {
	Status  int
	Type    string
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

// Result describes a handled request. Target, Checked and Reason are filled
// as far as the request got, including when Chat returns an error.
type Result struct {
	Target  string // concrete backend model ID, or alias
	AliasOf string // the base model when Target is an alias
	Backend string // the winner's backend, when it isn't the virtual model's own
	Checked []string
	Reason  string
	Chat    backend.ChatResult
}

// Current is what a request would be routed to right now. Model is empty
// when nothing would be.
type Current struct {
	Model   string
	AliasOf string
	Backend string // set when it isn't the virtual model's own
	Reason  string
}

type Handler interface {
	Description() string
	// Current predicts the routing of a request made now, without making one.
	Current(ctx context.Context) (Current, error)
	Chat(ctx context.Context, endpoint backend.Endpoint, body []byte, client backend.Client) (Result, error)
	// Describe prints the entry's resolved configuration for the check command.
	Describe(w io.Writer)
}

// Env is what types need from the rest of the config while building.
type Env struct {
	Backends map[string]backend.Backend
	// Catalog returns a backend's listed models, fetched once per backend.
	Catalog func(ctx context.Context, backendName string) (*backend.Catalog, error)
	Log     *slog.Logger
}

// Build validates an entry and returns its handler.
func Build(ctx context.Context, entry config.Entry, env Env) (Handler, error) {
	switch entry.Type {
	case "", "candidates":
		return newCandidates(ctx, entry, env)
	}
	return nil, fmt.Errorf("unknown type %q", entry.Type)
}
