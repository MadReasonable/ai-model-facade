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

// CandidateStatus is one candidate's state now. Match is the model the
// candidate resolves to when that differs from Model: a pattern's match.
// Winner marks the candidate a request made now would go to.
type CandidateStatus struct {
	Model      string
	Backend    string
	LoadIfNone bool
	State      string
	Match      string
	AliasOf    string
	Winner     bool
}

// Inspection is a facade's routing now, with every candidate's state.
type Inspection struct {
	Current    *Current // nil if no backend's state could be read
	Candidates []CandidateStatus
}

type Handler interface {
	Description() string
	// Current predicts the routing of a request made now, without making one.
	Current(ctx context.Context) (Current, error)
	// Inspect is Current with the state of every candidate. A non-nil error
	// means Current is nil; Candidates are still filled.
	Inspect(ctx context.Context) (Inspection, error)
	Chat(ctx context.Context, protocol backend.Protocol, body []byte, client backend.Client) (Result, error)
	// Describe prints the entry's resolved configuration for the check command.
	Describe(w io.Writer)
}

// Env is what types need from the rest of the config while building.
type Env struct {
	Backends map[string]backend.Backend
	// Catalogs holds each backend's listed models, fetched once at startup. A
	// backend that couldn't be listed has an Unavailable catalog, already
	// warned about.
	Catalogs map[string]*backend.Catalog
	// Check runs a check against model lists now, or, if a backend it needs
	// couldn't be listed, once that backend answers. Its warnings are problems
	// that depend on a backend being up and current, such as a candidate ID
	// it doesn't list: serve starts anyway, check fails.
	Check func(backend.ListingCheck)
	Log   *slog.Logger
}

// Build validates an entry and returns its handler.
func Build(entry config.Entry, env Env) (Handler, error) {
	switch entry.Type {
	case "", "candidates":
		return newCandidates(entry, env)
	}
	return nil, fmt.Errorf("unknown type '%s'", entry.Type)
}
