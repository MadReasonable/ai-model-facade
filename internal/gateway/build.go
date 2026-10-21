// Package gateway wires config into backends and virtual models, and serves
// the OpenAI API front end.
package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/llamaswap"
	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/virtualmodel"
)

type Gateway struct {
	Config *config.Config
	Models []Model // in config order
}

type Model struct {
	Name    string
	Handler virtualmodel.Handler
}

// Build constructs and validates every backend and virtual model. Validation
// that needs a backend (listed model IDs) contacts it now. Startup notes,
// such as candidates that resolve to aliases, go to log.
func Build(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Gateway, error) {
	backendEntries, err := config.Entries(&cfg.Backends, "backends")
	if err != nil {
		return nil, err
	}
	backends := map[string]backend.Backend{}
	for _, e := range backendEntries {
		b, err := newBackend(e)
		if err != nil {
			return nil, fmt.Errorf("backends.%s: %w", e.Name, err)
		}
		backends[e.Name] = b
	}
	catalogs := map[string]*backend.Catalog{}
	env := virtualmodel.Env{
		Backends: backends,
		Catalog: func(ctx context.Context, name string) (*backend.Catalog, error) {
			if c, ok := catalogs[name]; ok {
				return c, nil
			}
			models, err := backends[name].ListModels(ctx)
			if err != nil {
				return nil, err
			}
			catalogs[name] = backend.NewCatalog(models)
			return catalogs[name], nil
		},
		Log: log,
	}
	modelEntries, err := config.Entries(&cfg.Models, "models")
	if err != nil {
		return nil, err
	}
	if len(modelEntries) == 0 {
		return nil, fmt.Errorf("models: at least one virtual model is required")
	}
	g := &Gateway{Config: cfg}
	for _, e := range modelEntries {
		h, err := virtualmodel.Build(ctx, e, env)
		if err != nil {
			return nil, fmt.Errorf("models.%s: %w", e.Name, err)
		}
		g.Models = append(g.Models, Model{Name: e.Name, Handler: h})
	}
	return g, nil
}

func newBackend(e config.Entry) (backend.Backend, error) {
	switch e.Type {
	case "llamaSwap":
		return llamaswap.New(e)
	case "":
		return nil, fmt.Errorf("line %d: type is required", e.Node.Line)
	}
	return nil, fmt.Errorf("line %d: unknown type %q", e.Node.Line, e.Type)
}
