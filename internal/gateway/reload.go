package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
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
	// watch publishes the status to GET /amf/config-status?follow.
	watch statusWatch
	// models tells the streams of GET /amf/models?follow when to list again.
	models modelsWatch
	// version is the gateway's, for the status; see SetVersion.
	version string
}

type live struct {
	gateway *Gateway
	// slots are what each address the gateway listens on serves.
	slots map[string]*slot
	// uiPath is where the admin UI is served; see uiPathFor.
	uiPath string
	// https is where each side's HTTPS is served now; see servedTLS.
	https servedHTTPS
}

// slot is what one address serves: plain HTTP, and HTTPS with its
// certificate, nil where the address serves none.
type slot struct {
	plain, secure http.Handler
	cert          *tls.Certificate
}

// addresses are what a config says the gateway listens on.
type addresses struct {
	main, admin       string // server.listen, server.adminApi.listen
	mainTLS, adminTLS string // where each side serves HTTPS; "" where it doesn't
}

func addressesOf(cfg *config.Config) addresses {
	a := cfg.Server.Addresses()
	return addresses{a.Main, a.Admin, a.MainTLS, a.AdminTLS}
}

// bound reports whether the gateway listens on addr.
func (a addresses) bound(addr string) bool {
	return addr != "" && (addr == a.main || addr == a.admin || addr == a.mainTLS || addr == a.adminTLS)
}

// servedHTTPS is where each side serves HTTPS, "" where it serves none.
type servedHTTPS struct {
	main, admin string
	// wantMain and wantAdmin are where the running config asks for it,
	// which differ from main and admin until a restart binds them.
	wantMain, wantAdmin string
}

// servedTLS is where each side of cfg serves HTTPS, given the addresses the
// gateway listens on. A tls block without listen shares its side's address,
// which is bound; one whose listen isn't bound goes on where its side's
// HTTPS started, if it had any, until a restart, so where HTTPS is served
// changes only then.
func servedTLS(listening addresses, cfg *config.Config) servedHTTPS {
	var h servedHTTPS
	side := func(t config.TLS, want, started string) (served, wanted string) {
		if !t.On() {
			return "", ""
		}
		if listening.bound(want) {
			return want, want
		}
		return started, want
	}
	server, admin := cfg.Server.TLS, cfg.Server.AdminAPI.TLS
	h.main, h.wantMain = side(server, firstSet(server.Listen, listening.main), listening.mainTLS)
	h.admin, h.wantAdmin = side(admin, firstSet(admin.Listen, listening.admin, listening.main), listening.adminTLS)
	if h.admin != "" && h.admin == h.main {
		h.admin = "" // one address offers one certificate; server.tls keeps it until a restart sorts them apart
	}
	return h
}

func firstSet(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// fileReload is one reload from the files, shared by every request for one
// that arrived before it read them.
type fileReload struct {
	asked  int // how many requests share it
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
	// Dir is the config file's folder, which a relative TLS path is from.
	Dir() string
}

// NewLive serves g. files are the config files, which SIGHUP and POST
// /amf/reload reload and POST /amf/config-save writes, and level is
// the log level a reload sets. g's config is taken to be what the files
// hold, and is the draft's first content.
func NewLive(g *Gateway, log *slog.Logger, level *slog.LevelVar, files ConfigFiles) *Live {
	fromFiles := provenance{source: sourceFiles}
	l := &Live{log: log, level: level, files: files, saved: g.Config.Source, draft: draftCopy{doc: g.Config.Source.Copy(), provenance: fromFiles}, running: fromFiles, listening: addressesOf(g.Config)}
	l.current.Store(l.serving(g))
	g.start(log, l.modelsChanged)
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

// SetVersion gives the version the status reports, as ai-model-facade
// version prints it. Call it before serving.
func (l *Live) SetVersion(version string) {
	l.version = version
}

// ServeHTTP serves server.listen's plain HTTP.
func (l *Live) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.Plain(l.listening.main).ServeHTTP(w, r)
}

// Admin serves server.adminApi.listen's plain HTTP, which the gateway
// listens on only if the config it started with sets it.
func (l *Live) Admin() http.Handler {
	return l.Plain(l.listening.admin)
}

// Addresses are the addresses the gateway listens on, each once: those of
// the config it started with, which only a restart changes. Each serves
// Plain and, through Split, Secure.
func (l *Live) Addresses() []string {
	var out []string
	for _, a := range []string{l.listening.main, l.listening.admin, l.listening.mainTLS, l.listening.adminTLS} {
		if a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// Plain serves addr's plain HTTP, as the running config says.
func (l *Live) Plain(addr string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slot := l.current.Load().slots[addr]
		if slot == nil || slot.plain == nil {
			http.NotFound(w, r)
			return
		}
		slot.plain.ServeHTTP(w, r)
	})
}

// Secure serves addr's HTTPS, with TLSConfig, as the running config says. A
// request on a connection whose handshake an earlier config accepted, on an
// address the running one serves no HTTPS on, gets 421.
func (l *Live) Secure(addr string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slot := l.current.Load().slots[addr]
		if slot == nil || slot.secure == nil {
			http.Error(w, errNoHTTPS.Error(), http.StatusMisdirectedRequest)
			return
		}
		slot.secure.ServeHTTP(w, r)
	})
}

// Gateway returns the gateway currently serving requests.
func (l *Live) Gateway() *Gateway {
	return l.current.Load().gateway
}

// serving returns what serves g on each address the gateway listens on:
// server.listen's side, plain and HTTPS, and the admin side's where it has
// addresses of its own. The split follows the addresses listened on, not
// g's config, which may name others until a restart. A side whose tls block
// serves https only redirects its plain HTTP there; an admin side with no
// address of its own does that for the admin paths of server.listen's
// plain HTTP. A separate HTTPS address redirects its plain HTTP to itself.
func (l *Live) serving(g *Gateway) *live {
	uiPath := l.uiPathFor(g.Config)
	cfg := g.Config.Server
	https := servedTLS(l.listening, g.Config)
	now := &live{gateway: g, slots: map[string]*slot{}, uiPath: uiPath, https: https}
	at := func(addr string) *slot {
		if now.slots[addr] == nil {
			now.slots[addr] = &slot{}
		}
		return now.slots[addr]
	}
	main, mainUI := allEndpoints, uiPath
	if l.listening.admin != "" {
		main, mainUI = clientEndpoints, ""
	}
	adminRedirect := ""
	if l.listening.admin == "" && cfg.AdminAPI.TLS.Only() {
		adminRedirect = https.admin
	}
	if cfg.TLS.Only() && https.main != "" {
		at(l.listening.main).plain = redirectToTLS(https.main)
	} else {
		at(l.listening.main).plain = g.handler(l.log, l, main, mainUI, adminRedirect)
	}
	if l.listening.admin != "" {
		if cfg.AdminAPI.TLS.Only() && https.admin != "" {
			at(l.listening.admin).plain = redirectToTLS(https.admin)
		} else {
			at(l.listening.admin).plain = g.handler(l.log, l, adminEndpoints, uiPath, "")
		}
	}
	if https.main != "" {
		s := at(https.main)
		s.secure, s.cert = g.handler(l.log, l, main, mainUI, ""), cfg.TLS.Certificate()
	}
	if https.admin != "" {
		s := at(https.admin)
		s.secure, s.cert = g.handler(l.log, l, adminEndpoints, uiPath, ""), cfg.AdminAPI.TLS.Certificate()
	}
	for _, addr := range l.Addresses() {
		if s := at(addr); s.plain == nil {
			s.plain = redirectToTLS(addr)
		}
	}
	return now
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

// SIGHUPCaller is the caller of a reload asked for by SIGHUP, as the log
// names it. A name starts with a letter, so no key can have this one.
var SIGHUPCaller = backend.Client{Name: "<SIGHUP>"}

// ReloadFiles reloads from the config files, as Reload does from a config,
// sets the log level, and replaces the draft with the files' config. It logs
// each request with its caller, by, and each reload once with its outcome,
// since several requests can share one. A request that arrives while another reload
// is under way waits for it, then shares the next one with every request
// that arrived in the meantime: a reload that had already read the files may
// have read them before an edit the caller made, while the next one reads
// them after every request it serves arrived.
func (l *Live) ReloadFiles(by backend.Client) (*Reloaded, error) {
	l.log.Info("reload requested", clientAttrs(by)...)
	l.nextMu.Lock()
	run := l.next
	owner := run == nil
	if owner {
		run = &fileReload{done: make(chan struct{})}
		l.next = run
	}
	run.asked++
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
	l.nextMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), reloadTimeout)
	defer cancel()
	run.result, run.err = l.reloadFiles(ctx)
	if run.err != nil {
		l.log.Error("config reload failed; keeping the running config", "error", run.err)
		return
	}
	attrs := []any{"models", len(run.result.Gateway.Models)}
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
	l.changed()
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
	g.start(l.log, l.modelsChanged)
	old.stop()
	old.closeIdleConnections()
	l.changed()
	l.modelsChanged()
	for _, p := range pendingRestarts(l.listening, now.uiPath, cfg) {
		l.log.Warn("restart needed", "key", p.Key, "listening", p.Listening, "running", p.Running)
	}
	return g, nil
}

// PendingRestart is an address the running config names that the gateway
// doesn't listen on yet, because it started with another, or the admin UI's
// root, which waits for the admin side's own address.
type PendingRestart struct {
	// Key is the setting that waits: server.listen, server.tls.listen,
	// server.adminApi.listen, server.adminApi.tls.listen or
	// server.adminApi.uiBasePath.
	Key string `json:"key"`
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
	https := servedTLS(listening, cfg)
	if https.wantMain != https.main {
		out = append(out, PendingRestart{Key: "server.tls.listen", Listening: https.main, Running: https.wantMain})
	}
	if https.wantAdmin != https.admin {
		out = append(out, PendingRestart{Key: "server.adminApi.tls.listen", Listening: https.admin, Running: https.wantAdmin})
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
		case "server.tls.listen":
			p.Saved = files.Server.TLS.Listen == p.Running
		case "server.adminApi.tls.listen":
			p.Saved = files.Server.AdminAPI.TLS.Listen == p.Running
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
