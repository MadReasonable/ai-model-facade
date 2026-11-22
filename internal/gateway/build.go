// Package gateway wires config into backends and virtual models, and serves
// the front end for every client API.
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/anthropic"
	"github.com/MadReasonable/ai-model-facade/internal/backend/llamaserver"
	"github.com/MadReasonable/ai-model-facade/internal/backend/llamaswap"
	"github.com/MadReasonable/ai-model-facade/internal/backend/lmstudio"
	"github.com/MadReasonable/ai-model-facade/internal/backend/localai"
	"github.com/MadReasonable/ai-model-facade/internal/backend/ollama"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openai"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openaihttp"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openwebui"
	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/history"
	"github.com/MadReasonable/ai-model-facade/internal/pipeline"
	"github.com/MadReasonable/ai-model-facade/internal/protocols"
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
	// served are the protocols the gateway serves, by api.<family>.protocols.
	served map[backend.Protocol]bool
	// history is the Responses conversation store, which a reload hands on
	// to the gateway that replaces this one.
	history *history.Store
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
	return build(ctx, cfg, log, history.NewStore(historyLimits(cfg)))
}

// build is Build with the conversation store to use, so that a reload keeps
// the conversations of the gateway it replaces.
func build(ctx context.Context, cfg *config.Config, log *slog.Logger, store *history.Store) (*Gateway, error) {
	g := &Gateway{Config: cfg, backends: map[string]backend.Backend{}, history: store}
	for _, note := range cfg.Notes {
		log.Warn(note)
	}
	var err error
	if g.served, err = servedProtocols(cfg.API); err != nil {
		return nil, err
	}
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
	access := map[string]config.Access{}
	for _, e := range backendEntries {
		if a, ok := g.backends[e.Name].(accessor); ok {
			access[e.Name] = a.Access()
		}
		if err := cfg.Server.CheckAccess(access[e.Name]); err != nil {
			return nil, fmt.Errorf("backends.%s: line %d: %w", e.Name, e.Node.Line, err)
		}
		if m, ok := g.backends[e.Name].(keyMapper); ok {
			notes, err := cfg.Server.CheckKeyMap(m.KeyMap())
			if err != nil {
				return nil, fmt.Errorf("backends.%s: line %d: %w", e.Name, e.Node.Line, err)
			}
			// Added to the config's notes, which were logged above, so check
			// prints them with the others.
			for _, note := range notes {
				note = "backends." + e.Name + ": " + note
				log.Warn(note)
				cfg.Notes = append(cfg.Notes, note)
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
	run := &pipeline.Pipeline{Hooks: map[backend.Protocol]pipeline.Hook{backend.OpenAIResponses: store}, Backends: g.backends}
	env := virtualmodel.Env{Backends: g.backends, Access: access, Catalogs: g.catalogs, Check: check, Pipeline: run, Log: log}
	modelEntries, err := config.Entries(&cfg.Models, "models")
	if err != nil {
		return nil, err
	}
	// A gateway with no facades serves nothing, unless an admin can add
	// them over HTTP, as a config ai-model-facade init writes expects.
	if len(modelEntries) == 0 && !cfg.Server.AdminAPI.Write.On() {
		return nil, fmt.Errorf("models: at least one facade is required, unless server.adminApi.write lists an admin who can add them over HTTP")
	}
	for _, e := range modelEntries {
		h, err := virtualmodel.Build(e, env)
		if err != nil {
			return nil, fmt.Errorf("models.%s: %w", e.Name, err)
		}
		if err := g.checkServed(h); err != nil {
			return nil, fmt.Errorf("models.%s: line %d: %w", e.Name, e.Node.Line, err)
		}
		if err := cfg.Server.CheckAccess(h.Access()); err != nil {
			return nil, fmt.Errorf("models.%s: line %d: %w", e.Name, e.Node.Line, err)
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

// checkServed refuses a facade that serves only protocols the gateway turns
// off, which no request could reach.
func (g *Gateway) checkServed(h virtualmodel.Handler) error {
	var own []string
	for _, p := range protocols.Names() {
		if h.Serves(p) {
			if g.served[p] {
				return nil
			}
			own = append(own, string(p))
		}
	}
	return fmt.Errorf("it serves only %s, which api.<family>.protocols turns off; add one of them there, or a protocol the gateway serves to its protocols", strings.Join(own, ", "))
}

func historyLimits(cfg *config.Config) history.Limits {
	limits := cfg.Server.Responses
	return history.Limits{MaxBytes: limits.MaxBytes, MaxAge: time.Duration(limits.MaxAgeMinutes * float64(time.Minute))}
}

// accessor is a backend with an access list; every adapter has one, through
// openaihttp.Config. A backend without one allows every caller.
type accessor interface {
	Access() config.Access
}

// keyMapper is a backend with a keyMap; every adapter has one, through
// openaihttp.Config.
type keyMapper interface {
	KeyMap() config.KeyMap
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

// start has every backend that counts the requests it has pending call
// onPending when that count changes, and runs the pending checks in the
// background until stop is called.
func (g *Gateway) start(log *slog.Logger, onPending func()) {
	for _, b := range g.backends {
		if w, ok := b.(backend.PendingWatcher); ok {
			w.WatchPending(onPending)
		}
	}
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
	if e.Type == "" {
		return nil, fmt.Errorf("line %d: type is required", e.Node.Line)
	}
	for _, t := range backendTypes {
		if t.name == e.Type {
			return t.build(e, log)
		}
	}
	return nil, fmt.Errorf("line %d: unknown type '%s'", e.Node.Line, e.Type)
}

// backendTypes are the backend adapters, each with the type that selects it,
// the struct its entry decodes into, for GET /amf/schema, and how it is
// built, in the order the README lists them.
var backendTypes = []struct {
	name   string
	config any
	build  func(config.Entry, *slog.Logger) (backend.Backend, error)
}{
	{"llamaSwap", openaihttp.EntryConfig{}, func(e config.Entry, _ *slog.Logger) (backend.Backend, error) { return llamaswap.New(e) }},
	{"llamaServer", openaihttp.EntryConfig{}, func(e config.Entry, _ *slog.Logger) (backend.Backend, error) { return llamaserver.New(e) }},
	{"lmStudio", openaihttp.EntryConfig{}, func(e config.Entry, _ *slog.Logger) (backend.Backend, error) { return lmstudio.New(e) }},
	{"localAi", openaihttp.EntryConfig{}, func(e config.Entry, _ *slog.Logger) (backend.Backend, error) { return localai.New(e) }},
	{"ollama", openaihttp.EntryConfig{}, func(e config.Entry, _ *slog.Logger) (backend.Backend, error) { return ollama.New(e) }},
	{"openAi", openaihttp.EntryConfig{}, func(e config.Entry, _ *slog.Logger) (backend.Backend, error) { return openai.New(e) }},
	{"anthropic", openaihttp.EntryConfig{}, func(e config.Entry, _ *slog.Logger) (backend.Backend, error) { return anthropic.New(e) }},
	{"openWebUi", openwebui.Config{}, func(e config.Entry, log *slog.Logger) (backend.Backend, error) {
		return openwebui.New(e, log.With("backend", e.Name))
	}},
}
