// Package gateway wires config into backends and virtual models, and serves
// the OpenAI API front end.
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/llamaswap"
	"github.com/MadReasonable/ai-model-facade/internal/backend/ollama"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openwebui"
	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/virtualmodel"
)

type Gateway struct {
	Config *config.Config
	Models []Model // in config order
	// Warnings are checks that depend on a backend being up and current,
	// such as candidate IDs it doesn't list. serve starts with them (they are
	// logged); check reports them as errors.
	Warnings []error
	backends map[string]backend.Backend
	catalogs map[string]*backend.Catalog
	// pending are the checks waiting for a backend that couldn't be listed;
	// see runPending.
	pending []backend.ListingCheck
	stop    context.CancelFunc
}

type Model struct {
	Name    string
	Handler virtualmodel.Handler
}

// Build constructs and validates every backend and virtual model. A config
// that is wrong fails. What depends on a backend being up and current, such
// as listed model IDs, is checked now too, but only warned about (logged and
// kept in Warnings): a backend that is down or not ready yet shouldn't stop
// the gateway, nor its other backends' models. Each backend is listed once,
// and a check that needs one that couldn't be listed is kept in pending.
// Startup notes, such as candidates that resolve to aliases, go to log.
func Build(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Gateway, error) {
	g := &Gateway{Config: cfg, backends: map[string]backend.Backend{}}
	warn := func(err error) {
		g.Warnings = append(g.Warnings, err)
		log.Warn(err.Error())
	}
	backendEntries, err := config.Entries(&cfg.Backends, "backends")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range backendEntries {
		b, err := newBackend(e, log)
		if err != nil {
			return nil, fmt.Errorf("backends.%s: %w", e.Name, err)
		}
		g.backends[e.Name] = b
		names = append(names, e.Name)
	}
	var checks []backend.ListingCheck
	check := func(c backend.ListingCheck) { checks = append(checks, c) }
	for _, e := range backendEntries {
		if l, ok := g.backends[e.Name].(linker); ok {
			if err := l.Link(g.backends, check); err != nil {
				return nil, fmt.Errorf("backends.%s: %w", e.Name, err)
			}
		}
	}
	var errs map[string]error
	g.catalogs, errs = listModels(ctx, g.backends, names)
	for _, name := range names {
		if err := errs[name]; err != nil {
			warn(fmt.Errorf("backends.%s: listing models: %w; checks against its model list are skipped until it answers", name, err))
		}
	}
	env := virtualmodel.Env{Backends: g.backends, Catalogs: g.catalogs, Check: check, Log: log}
	modelEntries, err := config.Entries(&cfg.Models, "models")
	if err != nil {
		return nil, err
	}
	if len(modelEntries) == 0 {
		return nil, fmt.Errorf("models: at least one virtual model is required")
	}
	for _, e := range modelEntries {
		h, err := virtualmodel.Build(e, env)
		if err != nil {
			return nil, fmt.Errorf("models.%s: %w", e.Name, err)
		}
		g.Models = append(g.Models, Model{Name: e.Name, Handler: h})
	}
	for _, c := range checks {
		if !listed(c, g.catalogs) {
			g.pending = append(g.pending, c)
			continue
		}
		c.Run(g.catalogs, warn)
	}
	return g, nil
}

// linker is a backend that refers to other backends. Link resolves those
// references once every backend exists, failing on a wrong one; checks that
// need a backend to be up go to check.
type linker interface {
	Link(backends map[string]backend.Backend, check func(backend.ListingCheck)) error
}

// listModels lists the named backends at the same time, so that backends that
// don't answer delay startup by one stateTimeout, not one each, and can't use
// up the time left for the others. A backend that fails gets an Unavailable
// catalog and its error.
func listModels(ctx context.Context, backends map[string]backend.Backend, names []string) (map[string]*backend.Catalog, map[string]error) {
	type listing struct {
		name   string
		models []backend.ModelInfo
		err    error
	}
	results := make(chan listing, len(names))
	for _, name := range names {
		go func() {
			models, err := backends[name].ListModels(ctx)
			results <- listing{name, models, err}
		}()
	}
	catalogs, errs := map[string]*backend.Catalog{}, map[string]error{}
	for range names {
		l := <-results
		catalogs[l.name] = backend.NewCatalog(l.models)
		if l.err != nil {
			catalogs[l.name].Unavailable = true
			errs[l.name] = l.err
		}
	}
	return catalogs, errs
}

// listed reports whether every backend a check needs has been listed.
func listed(c backend.ListingCheck, catalogs map[string]*backend.Catalog) bool {
	for _, name := range c.Backends {
		if catalogs[name].Unavailable {
			return false
		}
	}
	return true
}

// start runs the pending checks in the background until stop is called.
func (g *Gateway) start(log *slog.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	g.stop = cancel
	go g.runPending(ctx, log)
}

// runPending runs the checks that Build put off because a backend couldn't be
// listed. Each of those backends is asked again on its own interval (see
// relist), and each check runs once all of its backends have answered;
// warnings only go to log, since serve is already running. It returns once no
// check is left, or when ctx ends, as it does when a reload replaces this
// gateway.
func (g *Gateway) runPending(ctx context.Context, log *slog.Logger) {
	catalogs := maps.Clone(g.catalogs)
	pending := g.pending
	var names []string
	for _, c := range pending {
		for _, name := range c.Backends {
			if catalogs[name].Unavailable && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	answered := make(chan relisted)
	for _, name := range names {
		go g.relist(ctx, name, answered)
	}
	for len(pending) > 0 {
		var r relisted
		select {
		case <-ctx.Done():
			return
		case r = <-answered:
		}
		catalogs[r.name] = r.catalog
		log.Info(fmt.Sprintf("backends.%s answers; running the checks against its model list", r.name))
		var waiting []backend.ListingCheck
		for _, c := range pending {
			if !listed(c, catalogs) {
				waiting = append(waiting, c)
				continue
			}
			c.Run(catalogs, func(err error) { log.Warn(err.Error()) })
		}
		pending = waiting
	}
}

type relisted struct {
	name    string
	catalog *backend.Catalog
}

// relist asks a backend for its models every listRetrySeconds until it
// answers, and sends the result to answered. Failures aren't logged: Build
// already warned that the backend couldn't be listed.
func (g *Gateway) relist(ctx context.Context, name string, answered chan<- relisted) {
	b := g.backends[name]
	retry, ok := b.(interface{ ListRetry() time.Duration })
	if !ok {
		return // every adapter has it, through openaihttp.Client
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry.ListRetry()):
		}
		if models, err := b.ListModels(ctx); err == nil {
			select {
			case answered <- relisted{name, backend.NewCatalog(models)}:
			case <-ctx.Done():
			}
			return
		}
	}
}

func newBackend(e config.Entry, log *slog.Logger) (backend.Backend, error) {
	switch e.Type {
	case "llamaSwap":
		return llamaswap.New(e)
	case "ollama":
		return ollama.New(e)
	case "openWebUi":
		return openwebui.New(e, log.With("backend", e.Name))
	case "":
		return nil, fmt.Errorf("line %d: type is required", e.Node.Line)
	}
	return nil, fmt.Errorf("line %d: unknown type '%s'", e.Node.Line, e.Type)
}
