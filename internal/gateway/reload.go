package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// reloadTimeout bounds a reload from the files, which reads backends' model
// lists while it builds.
const reloadTimeout = 30 * time.Second

// Live serves the current gateway and can replace it while running.
// Requests already in flight, including streams, finish on the gateway
// that accepted them; new requests use the replacement.
type Live struct {
	log *slog.Logger
	// level is the log level a reload from the files sets.
	level *slog.LevelVar
	// load reads the config files a reload takes effect from.
	load    func() (*config.Config, error)
	mu      sync.Mutex // serializes reloads, from reading the files to the swap
	current atomic.Pointer[live]
	// nextMu guards next.
	nextMu sync.Mutex
	// next is the reload from the files that hasn't read them yet, which a
	// request for one joins; nil when none is waiting.
	next *fileReload
}

type live struct {
	gateway *Gateway
	handler http.Handler
}

// fileReload is one reload from the files, shared by every request for one
// that arrived before it read them.
type fileReload struct {
	by      []string // what asked for it, in order
	done    chan struct{}
	gateway *Gateway
	err     error
}

// NewLive serves g. load reads the config files, as SIGHUP and POST
// /amf/reload reload them, and level is the log level such a reload
// sets.
func NewLive(g *Gateway, log *slog.Logger, level *slog.LevelVar, load func() (*config.Config, error)) *Live {
	l := &Live{log: log, level: level, load: load}
	l.current.Store(&live{gateway: g, handler: l.handler(g)})
	g.start(log)
	return l
}

func (l *Live) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.current.Load().handler.ServeHTTP(w, r)
}

// Gateway returns the gateway currently serving requests.
func (l *Live) Gateway() *Gateway {
	return l.current.Load().gateway
}

func (l *Live) handler(g *Gateway) http.Handler {
	return g.handler(l.log, l.ReloadFiles)
}

// Reload builds a gateway from cfg and, only if that fully succeeds, swaps
// it in, with the Responses conversations of the one it replaces. On any
// error the running gateway is untouched. server.listen can't change without
// a restart, because the port is already bound.
func (l *Live) Reload(ctx context.Context, cfg *config.Config) (*Gateway, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.swap(ctx, cfg)
}

// ReloadFiles reloads from the config files, as Reload does from a config,
// and sets the log level; by names what asked, for the log. A request that
// arrives while another reload is under way waits for it, then shares the
// next one with every request that arrived in the meantime: a reload that had
// already read the files may have read them before an edit the caller made,
// while the next one reads them after every request it serves arrived.
func (l *Live) ReloadFiles(by string) (*Gateway, error) {
	l.nextMu.Lock()
	run := l.next
	owner := run == nil
	if owner {
		run = &fileReload{done: make(chan struct{})}
		l.next = run
	}
	run.by = append(run.by, by)
	l.nextMu.Unlock()
	if owner {
		l.runFileReload(run)
	}
	<-run.done
	return run.gateway, run.err
}

// runFileReload waits for any reload under way, closes run to new requests,
// and runs it. It runs detached from any request, with its own timeout, so
// a caller that leaves doesn't fail the reload the others are waiting on.
func (l *Live) runFileReload(run *fileReload) {
	l.mu.Lock()
	defer l.mu.Unlock()
	defer close(run.done)
	l.nextMu.Lock()
	l.next = nil
	by := run.by
	l.nextMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), reloadTimeout)
	defer cancel()
	run.gateway, run.err = l.reloadFiles(ctx)
	if run.err != nil {
		l.log.Error("config reload failed; keeping the running config", "by", by, "error", run.err)
		return
	}
	l.log.Info("config reloaded", "by", by, "models", len(run.gateway.Models))
}

// reloadFiles reads the files, swaps in a gateway built from them, and sets
// the log level, all or nothing. The caller holds l.mu.
func (l *Live) reloadFiles(ctx context.Context) (*Gateway, error) {
	cfg, err := l.load()
	if err != nil {
		return nil, err
	}
	level, err := cfg.Logging.SlogLevel()
	if err != nil {
		return nil, err
	}
	g, err := l.swap(ctx, cfg)
	if err != nil {
		return nil, err
	}
	l.level.Set(level)
	return g, nil
}

// swap is Reload for a caller that holds l.mu.
func (l *Live) swap(ctx context.Context, cfg *config.Config) (*Gateway, error) {
	old := l.current.Load().gateway
	if cfg.Server.Listen != old.Config.Server.Listen {
		return nil, fmt.Errorf("server.listen changed from '%s' to '%s'; restart to apply", old.Config.Server.Listen, cfg.Server.Listen)
	}
	g, err := build(ctx, cfg, l.log, old.history)
	if err != nil {
		return nil, err
	}
	g.history.SetLimits(historyLimits(cfg))
	l.current.Store(&live{gateway: g, handler: l.handler(g)})
	g.start(l.log)
	old.stop()
	old.closeIdleConnections()
	return g, nil
}

// closeIdleConnections releases pooled connections a replaced gateway no
// longer needs. Connections still serving in-flight requests are untouched.
func (g *Gateway) closeIdleConnections() {
	for _, b := range g.backends {
		if c, ok := b.(interface{ CloseIdleConnections() }); ok {
			c.CloseIdleConnections()
		}
	}
}
