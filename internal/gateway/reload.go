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
	"github.com/MadReasonable/ai-model-facade/internal/configfile"
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
	// files are the config files a reload takes effect from and a save
	// writes.
	files ConfigFiles
	// mu serializes what changes the running config or the files: a reload,
	// from reading the files to the swap, an apply, and a save. A build
	// under it can take as long as a slow backend's model list, so nothing
	// that only reads the copies below takes it.
	mu      sync.Mutex
	current atomic.Pointer[live]
	// stateMu guards the three copies of the config the admin endpoints
	// report: the draft, the provenance of what is running (its Document is
	// the running gateway's Config.Source), and saved, the files as the
	// gateway last read or wrote them. It is held only to read them or to
	// commit a change, never across a build or file I/O, so a read never
	// waits on a reload. Taken after mu, never before.
	stateMu sync.Mutex
	draft   draftCopy
	running provenance
	saved   *config.Document
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
	by     []string // what asked for it, in order
	done   chan struct{}
	result *Reloaded
	err    error
}

// Reloaded is the outcome of a reload from the files.
type Reloaded struct {
	Gateway *Gateway
	// DiscardedDraft is whether the draft held edits not applied, which the
	// reload replaced with the files' config.
	DiscardedDraft bool
	// DiscardedUnsaved is whether the running config held changes not
	// saved, which the reload replaced.
	DiscardedUnsaved bool
}

// ConfigFiles are the files a Live reloads from and saves to;
// configfile.Files outside tests.
type ConfigFiles interface {
	Load() (*config.Config, error)
	Read() (*config.Document, error)
	Save(running, saved *config.Document, force bool, keep int) (*configfile.Saved, error)
	Status(saved *config.Document, keep int) ([]configfile.FileStatus, configfile.BackupsStatus)
	Backups() ([]configfile.Backup, error)
	ReadBackup(ref string) (id string, doc *config.Document, err error)
}

// NewLive serves g. files are the config files, which SIGHUP and POST
// /amf/reload reload and POST /amf/config-save writes, and level is
// the log level a reload sets. g's config is taken to be what the files
// hold, and is the draft's first content.
func NewLive(g *Gateway, log *slog.Logger, level *slog.LevelVar, files ConfigFiles) *Live {
	fromFiles := provenance{source: sourceFiles}
	l := &Live{log: log, level: level, files: files, saved: g.Config.Source, draft: draftCopy{doc: g.Config.Source.Copy(), provenance: fromFiles}, running: fromFiles}
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
	return g.handler(l.log, l)
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
// sets the log level, and replaces the draft with the files' config; by
// names what asked, for the log. A request that arrives while another reload
// is under way waits for it, then shares the next one with every request
// that arrived in the meantime: a reload that had already read the files may
// have read them before an edit the caller made, while the next one reads
// them after every request it serves arrived.
func (l *Live) ReloadFiles(by string) (*Reloaded, error) {
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
	return run.result, run.err
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
	run.result, run.err = l.reloadFiles(ctx)
	if run.err != nil {
		l.log.Error("config reload failed; keeping the running config", "by", by, "error", run.err)
		return
	}
	attrs := []any{"by", by, "models", len(run.result.Gateway.Models)}
	if run.result.DiscardedDraft {
		attrs = append(attrs, "discardedDraft", true)
	}
	if run.result.DiscardedUnsaved {
		attrs = append(attrs, "discardedUnsaved", true)
	}
	l.log.Info("config reloaded", attrs...)
}

// reloadFiles reads the files, swaps in a gateway built from them, sets the
// log level and replaces the draft, all or nothing. The caller holds l.mu.
func (l *Live) reloadFiles(ctx context.Context) (*Reloaded, error) {
	if l.files == nil {
		return nil, fmt.Errorf("the gateway has no config files to reload")
	}
	cfg, err := l.files.Load()
	if err != nil {
		return nil, err
	}
	replaced := l.Gateway().Config.Source
	g, err := l.apply(ctx, cfg)
	if err != nil {
		return nil, err
	}
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	fromFiles := provenance{source: sourceFiles}
	result := &Reloaded{Gateway: g, DiscardedDraft: !l.draft.doc.Equal(replaced), DiscardedUnsaved: !replaced.Equal(l.saved)}
	l.saved, l.running = cfg.Source, fromFiles
	l.draft = draftCopy{doc: cfg.Source.Copy(), provenance: fromFiles}
	return result, nil
}

// apply swaps in a gateway built from cfg and sets its log level, all or
// nothing. The caller holds l.mu.
func (l *Live) apply(ctx context.Context, cfg *config.Config) (*Gateway, error) {
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
