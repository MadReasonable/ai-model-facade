package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/configfile"
	"github.com/MadReasonable/ai-model-facade/internal/loghub"
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
	// listening are the addresses the gateway listens on: those of the
	// config it started with, which only a restart changes.
	listening addresses
	// logs is the gateway's copy of its log, for GET /amf/logs, configured
	// from each config swapped in; nil leaves the endpoint out.
	logs *loghub.Hub
	// nextMu guards next.
	nextMu sync.Mutex
	// next is the reload from the files that hasn't read them yet, which a
	// request for one joins; nil when none is waiting.
	next *fileReload
}

type live struct {
	gateway *Gateway
	handler http.Handler
	// admin serves server.adminApi.listen, when the gateway listens there;
	// nil otherwise.
	admin http.Handler
	// uiPath is where the admin UI is served; see uiPathFor.
	uiPath string
}

// addresses are what a config says the gateway listens on.
type addresses struct {
	main, admin string // server.listen, server.adminApi.listen
}

func addressesOf(cfg *config.Config) addresses {
	return addresses{cfg.Server.Listen, cfg.Server.AdminAPI.Listen}
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
	l := &Live{log: log, level: level, files: files, saved: g.Config.Source, draft: draftCopy{doc: g.Config.Source.Copy(), provenance: fromFiles}, running: fromFiles, listening: addressesOf(g.Config)}
	l.current.Store(l.serving(g))
	g.start(log)
	return l
}

// KeepLogs serves logs, the copy of the log the logger feeds, on GET
// /amf/logs, configured from the running config and from each one swapped
// in after. Call it before serving.
func (l *Live) KeepLogs(logs *loghub.Hub) {
	l.logs = logs
	g := l.Gateway()
	logs.Configure(g.Config)
	l.current.Store(l.serving(g))
}

// ServeHTTP serves server.listen.
func (l *Live) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.current.Load().handler.ServeHTTP(w, r)
}

// Admin serves server.adminApi.listen, which the gateway listens on only if
// the config it started with sets it.
func (l *Live) Admin() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.current.Load().admin.ServeHTTP(w, r)
	})
}

// Gateway returns the gateway currently serving requests.
func (l *Live) Gateway() *Gateway {
	return l.current.Load().gateway
}

// serving returns what serves g: on server.listen, and on
// server.adminApi.listen if the gateway listens there. The split follows
// the addresses listened on, not g's config, which may name others until
// a restart.
func (l *Live) serving(g *Gateway) *live {
	uiPath := l.uiPathFor(g.Config)
	if l.listening.admin == "" {
		return &live{gateway: g, handler: g.handler(l.log, l, allEndpoints, uiPath), uiPath: uiPath}
	}
	return &live{gateway: g, handler: g.handler(l.log, l, clientEndpoints, ""), admin: g.handler(l.log, l, adminEndpoints, uiPath), uiPath: uiPath}
}

// uiPathFor is where cfg's admin UI is served: server.adminApi.uiBasePath,
// except at the root while the gateway listens on server.listen alone,
// which serves the client APIs there, until a restart moves the admin side
// to server.adminApi.listen. Until then the UI stays where it is served now,
// or at the default if that path is now a route of cfg's own.
func (l *Live) uiPathFor(cfg *config.Config) string {
	want := cfg.Server.AdminAPI.UIBasePath
	if want == "" {
		want = config.DefaultUIBasePath // a config not made by Parse
	}
	if want != "/" || l.listening.admin != "" {
		return want
	}
	if now := l.current.Load(); now != nil && now.uiPath != "/" && config.CheckUIBasePath(now.uiPath, cfg.API) == nil {
		return now.uiPath
	}
	return config.DefaultUIBasePath
}

// Reload builds a gateway from cfg and, only if that fully succeeds, swaps
// it in, with the Responses conversations of the one it replaces. On any
// error the running gateway is untouched. A changed server.listen or
// server.adminApi.listen is taken, but the gateway goes on listening where
// it started until a restart, since those ports are bound; Restart reports
// what is pending.
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
	if l.logs != nil {
		l.logs.Configure(cfg)
	}
	return g, nil
}

// swap is Reload for a caller that holds l.mu.
func (l *Live) swap(ctx context.Context, cfg *config.Config) (*Gateway, error) {
	old := l.current.Load().gateway
	g, err := build(ctx, cfg, l.log, old.history)
	if err != nil {
		return nil, err
	}
	g.history.SetLimits(historyLimits(cfg))
	now := l.serving(g)
	l.current.Store(now)
	g.start(l.log)
	old.stop()
	old.closeIdleConnections()
	for _, p := range pendingRestarts(l.listening, now.uiPath, cfg) {
		l.log.Warn("restart needed", "key", p.Key, "listening", p.Listening, "running", p.Running)
	}
	return g, nil
}

// PendingRestart is an address the running config names that the gateway
// doesn't listen on yet, because it started with another, or the admin UI's
// root, which waits for the admin side's own address.
type PendingRestart struct {
	Key string `json:"key"` // server.listen, server.adminApi.listen or server.adminApi.uiBasePath
	// Listening is where the gateway listens, "" for nowhere, or for
	// server.adminApi.uiBasePath, the path the UI is served at.
	Listening string `json:"listening"`
	Running   string `json:"running"` // what the running config says, "" for nowhere
	// Saved is whether the files hold Running too, so a restart takes it,
	// and the health check probes it already.
	Saved bool `json:"saved"`
}

// pendingRestarts compares where the gateway listens, and the path it
// serves the admin UI at, with what cfg says.
func pendingRestarts(listening addresses, uiPath string, cfg *config.Config) []PendingRestart {
	var out []PendingRestart
	running := addressesOf(cfg)
	if running.main != listening.main {
		out = append(out, PendingRestart{Key: "server.listen", Listening: listening.main, Running: running.main})
	}
	if running.admin != listening.admin {
		out = append(out, PendingRestart{Key: "server.adminApi.listen", Listening: listening.admin, Running: running.admin})
	}
	if want := cfg.Server.AdminAPI.UIBasePath; want != "" && want != uiPath {
		out = append(out, PendingRestart{Key: "server.adminApi.uiBasePath", Listening: uiPath, Running: want})
	}
	return out
}

// Restart returns what a restart would change of where the gateway
// listens, and whether the files hold it yet; none if nothing.
func (l *Live) Restart() []PendingRestart {
	now := l.current.Load()
	pending := pendingRestarts(l.listening, now.uiPath, now.gateway.Config)
	if len(pending) == 0 {
		return []PendingRestart{}
	}
	l.stateMu.Lock()
	_, running := l.copies()
	saved := l.saved
	l.stateMu.Unlock()
	files := &config.Config{}
	if running.Saved {
		files = now.gateway.Config
	} else if cfg, err := saved.Parse(os.LookupEnv); err == nil {
		files = cfg
	}
	for i := range pending {
		p := &pending[i]
		switch p.Key {
		case "server.listen":
			p.Saved = files.Server.Listen == p.Running
		case "server.adminApi.listen":
			p.Saved = files.Server.AdminAPI.Listen == p.Running
		default:
			p.Saved = files.Server.AdminAPI.UIBasePath == p.Running
		}
	}
	return pending
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
