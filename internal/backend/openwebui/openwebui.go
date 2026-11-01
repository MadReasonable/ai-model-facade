// Package openwebui is the backend adapter for Open WebUI. A completion runs
// Open WebUI's own chat flow, so its tools and features run server-side:
//
//  1. create a chat holding the prompt and an empty assistant message;
//  2. start the completion with chat_id, the message id and a session_id,
//     which makes Open WebUI treat it as a UI request (built-in tools, the
//     tool loop, partial output while it runs);
//  3. poll the chat for the growing message, and the chat's tasks, until the
//     message is done and no task is left;
//  4. build an OpenAI response or chunk stream from it, with the model's
//     reasoning as reasoning_content, then clean up the chat.
//
// Loaded-model state comes from Open WebUI's own model list; see fetchState.
package openwebui

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/llamaswap"
	"github.com/MadReasonable/ai-model-facade/internal/backend/ollama"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openaihttp"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

type Config struct {
	Type                string `yaml:"type"`
	openaihttp.Config   `yaml:",inline"`
	Chats               string       `yaml:"chats"`               // delete (default) | archive | keep
	PollIntervalSeconds float64      `yaml:"pollIntervalSeconds"` // default 0.25
	Links               []LinkConfig `yaml:"links"`
	LlamaSwap           *LinkConfig  `yaml:"llamaSwap"` // one more link, to a llamaSwap backend only
}

// LinkConfig takes the state of the Open WebUI models that one of its
// connections serves from this gateway's backend for the same server, whose
// state is exact, instead of from what Open WebUI passes on from the server's
// /v1/models: no state at all for most servers, and for llama-swap a `status`
// that reads `loaded` while starting and stopping too.
type LinkConfig struct {
	Backend  string `yaml:"backend"`
	PrefixID string `yaml:"prefixId"` // the Open WebUI connection's prefix_id, if any
}

// linkConfig is a LinkConfig with the key it came from, links or llamaSwap.
type linkConfig struct {
	LinkConfig
	key string
}

type Backend struct {
	*openaihttp.Client        // its Chat is shadowed by the chat flow below
	name               string // its entry under backends
	state              *openaihttp.StateCache
	chats              string
	poll               time.Duration
	linkConfigs        []linkConfig
	links              []*link                       // set by Link
	listed             atomic.Pointer[[]listedModel] // what ListModels last fetched, for checkLinks
	linkList           atomic.Pointer[linkList]      // from the latest-started state fetch, with links
	log                *slog.Logger
}

// New builds the adapter from its config entry. log receives what the
// adapter does in the background, such as a chat it failed to clean up.
func New(entry config.Entry, log *slog.Logger) (*Backend, error) {
	var c Config
	if err := config.DecodeStrict(entry.Node, &c); err != nil {
		return nil, err
	}
	switch c.Chats {
	case "":
		c.Chats = "delete"
	case "delete", "archive", "keep":
	default:
		return nil, fmt.Errorf("chats must be delete, archive or keep, not '%s'", c.Chats)
	}
	if c.PollIntervalSeconds < 0 {
		return nil, fmt.Errorf("pollIntervalSeconds must not be negative")
	}
	poll := time.Duration(c.PollIntervalSeconds * float64(time.Second))
	if poll == 0 {
		poll = 250 * time.Millisecond
	}
	var links []linkConfig
	for _, l := range c.Links {
		links = append(links, linkConfig{l, "links"})
	}
	if c.LlamaSwap != nil {
		links = append(links, linkConfig{*c.LlamaSwap, "llamaSwap"})
	}
	for _, l := range links {
		if l.Backend == "" {
			return nil, fmt.Errorf("%s.backend is required", l.key)
		}
	}
	client, err := openaihttp.NewClient(c.Config)
	if err != nil {
		return nil, err
	}
	b := &Backend{Client: client, name: entry.Name, chats: c.Chats, poll: poll, linkConfigs: links, log: log}
	b.state = openaihttp.NewStateCache(c.Config, b.fetchState)
	return b, nil
}

// link is a resolved LinkConfig.
type link struct {
	name     string // the linked backend's entry under backends
	prefixID string
	target   linkTarget
	origin   string      // what originOf says of the models of target's kind of server
	down     atomic.Bool // the linked backend's last state read failed
}

// linkTarget is a backend a link can name: one that reads its state from its
// server, and in which a request through Open WebUI can count as pending.
// Every adapter built on openaihttp.NewBackend is one; an openWebUi backend
// is not.
type linkTarget interface {
	backend.Backend
	backend.ModelStateProvider
	Pending(model string) (release func())
}

// Link resolves the links once every backend exists. A link fails if it
// names a backend that doesn't exist, can't be linked or is already linked,
// and the llamaSwap block also if it names a backend of another type. It then
// asks for checkLinks, which runs once this backend and the linked ones have
// all been listed.
func (b *Backend) Link(backends map[string]backend.Backend, check func(backend.ListingCheck)) error {
	if len(b.linkConfigs) == 0 {
		return nil
	}
	names := []string{b.name}
	for _, c := range b.linkConfigs {
		other := backends[c.Backend]
		if other == nil {
			return fmt.Errorf("%s.backend '%s' is not defined", c.key, c.Backend)
		}
		if _, isSwap := other.(*llamaswap.Backend); c.key == "llamaSwap" && !isSwap {
			return fmt.Errorf("llamaSwap.backend '%s' is not a llamaSwap backend", c.Backend)
		}
		target, ok := other.(linkTarget)
		if !ok {
			return fmt.Errorf("%s.backend '%s' can't be linked; a link names the backend of a server Open WebUI connects to, such as a llamaSwap or lmStudio backend", c.key, c.Backend)
		}
		if slices.Contains(names[1:], c.Backend) {
			return fmt.Errorf("%s.backend '%s' is linked more than once", c.key, c.Backend)
		}
		names = append(names, c.Backend)
		b.links = append(b.links, &link{name: c.Backend, prefixID: c.PrefixID, target: target, origin: originOfBackend(other)})
	}
	check(backend.ListingCheck{Backends: names, Run: func(catalogs map[string]*backend.Catalog, warn func(error)) {
		// Listing this backend stored what Open WebUI listed, entries the
		// catalog doesn't keep.
		if models := b.listed.Load(); models != nil {
			b.checkLinks(*models, catalogs, func(err error) { warn(fmt.Errorf("backends.%s: %w", b.name, err)) })
		}
	}})
	return nil
}

// checkLinks warns about links that can't be right, each a config error that
// never recovers by itself: a linked backend that lists none of Open WebUI's
// models, a llama-swap model that no linked llamaSwap backend lists, and a
// model that more than one linked backend lists. A model no link matches
// takes the state Open WebUI reports, as without links.
func (b *Backend) checkLinks(models []listedModel, catalogs map[string]*backend.Catalog, warn func(error)) {
	matched := map[*link]bool{}
	bases, _ := group(models)
	for _, m := range bases {
		var names []string
		for _, l := range b.links {
			if _, ok := l.linkedID(m, catalogs[l.name]); ok {
				matched[l] = true
				names = append(names, "'"+l.name+"'")
			}
		}
		if len(names) > 1 {
			warn(fmt.Errorf("Open WebUI model '%s' is listed by more than one linked backend, %s, and takes the state of the first; set prefixId on the links so that one matches", m.ID, strings.Join(names, ", ")))
		}
		if len(names) == 0 && m.isLlamaSwap() && b.linksTo("llamaSwap") {
			warn(fmt.Errorf("Open WebUI model '%s' is a llama-swap model, but no linked llamaSwap backend lists it, so it takes the status Open WebUI reports; check prefixId", m.ID))
		}
	}
	for _, l := range b.links {
		if !matched[l] {
			warn(fmt.Errorf("linked backend '%s' lists none of Open WebUI's models, so the link does nothing; check its prefixId", l.name))
		}
	}
}

// linksTo reports whether a link names a backend whose models have this origin.
func (b *Backend) linksTo(origin string) bool {
	return slices.ContainsFunc(b.links, func(l *link) bool { return l.origin == origin })
}

// linkedID returns the ID that l's backend gives the base model of Open WebUI
// model m, if that backend lists m: m's ID, prefix removed, is one of its
// models or aliases, and Open WebUI doesn't say m came from another kind of
// server (originOf). A facade of a gateway behind Open WebUI is never linked,
// since its `current` is exact already.
func (l *link) linkedID(m listedModel, catalog *backend.Catalog) (string, bool) {
	if catalog == nil || m.Current != nil || originOf(m) != l.origin {
		return "", false
	}
	id := m.ID
	if l.prefixID != "" {
		var ok bool
		if id, ok = strings.CutPrefix(id, l.prefixID+"."); !ok {
			return "", false
		}
	}
	if base, isAlias := catalog.AliasOf[id]; isAlias {
		return base, true
	}
	if _, ok := catalog.Model(id); ok {
		return id, true
	}
	return "", false
}

// linkOf returns the first link whose backend lists Open WebUI model m, with
// the ID that backend gives m's base model, or nil.
func linkOf(m listedModel, links []*link, catalogs map[string]*backend.Catalog) (*link, string) {
	for _, l := range links {
		if id, ok := l.linkedID(m, catalogs[l.name]); ok {
			return l, id
		}
	}
	return nil, ""
}

// originOf is the kind of server Open WebUI's list says model m came from:
// llamaSwap for an entry with meta.llamaswap, ollama for an Ollama
// connection's, "" if the list doesn't say. originOfBackend names a
// backend's kind of server in the same terms.
func originOf(m listedModel) string {
	switch {
	case m.isLlamaSwap():
		return "llamaSwap"
	case m.OwnedBy == "ollama":
		return "ollama"
	}
	return ""
}

func originOfBackend(b backend.Backend) string {
	switch b.(type) {
	case *llamaswap.Backend:
		return "llamaSwap"
	case *ollama.Backend:
		return "ollama"
	}
	return ""
}

// listedModel is the part of an /api/models entry the adapter reads. Open
// WebUI passes each connection's own fields through, which is where the
// state comes from.
type listedModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
	Loaded  *bool  `json:"loaded"` // Ollama connections, set from Ollama's /api/ps
	Status  *struct {
		Value string `json:"value"`
	} `json:"status"` // llama-swap: "loaded" for any process not stopped
	Meta struct {
		LlamaSwap json.RawMessage `json:"llamaswap"`
	} `json:"meta"`
	Current *struct {
		Reason string `json:"reason"`
	} `json:"current"` // an ai-model-facade virtual model
	Info *struct {
		BaseModelID string `json:"base_model_id"`
	} `json:"info"` // a workspace model built on another model
}

func (m listedModel) isLlamaSwap() bool {
	return len(m.Meta.LlamaSwap) > 0 && string(m.Meta.LlamaSwap) != "null"
}

// swapBaseID is the llama-swap base model of a llama-swap preset (alias), as
// llama-swap's own listing names it in meta.llamaswap.modelID, or "" for a
// base model.
func (m listedModel) swapBaseID() string {
	return gjson.GetBytes(m.Meta.LlamaSwap, "modelID").String()
}

func (m listedModel) baseModelID() string {
	if m.Info == nil {
		return ""
	}
	return m.Info.BaseModelID
}

// models fetches Open WebUI's model list. refresh=true makes Open WebUI ask
// every connection again: with its base-models cache on, the plain list can
// stay stale through a whole model swap.
func (b *Backend) models(ctx context.Context) ([]listedModel, error) {
	var list struct {
		Data []listedModel `json:"data"`
	}
	if err := b.GetJSON(ctx, "/api/models?refresh=true", &list); err != nil {
		return nil, err
	}
	return list.Data, nil
}

// group folds models that load with another into it as aliases, the way the
// llamaSwap backend reports presets: a workspace model into the model it's
// built on, and a llama-swap preset into its base model. An alias is loaded
// when its base is, and a glob prefers bases over aliases. A chain, such as
// a workspace model built on a preset, resolves to the first model that
// folds into nothing; a workspace model whose base isn't listed stands
// alone, and so does a preset whose base model Open WebUI doesn't list.
func group(models []listedModel) (bases []listedModel, aliases map[string][]string) {
	byID := map[string]listedModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	aliases = map[string][]string{}
	for _, m := range models {
		if root := rootOf(m, byID); root != m.ID {
			aliases[root] = append(aliases[root], m.ID)
			continue
		}
		bases = append(bases, m)
	}
	return bases, aliases
}

// rootOf follows base_model_id, then a preset's llama-swap base, to the first
// listed model that folds into nothing. It returns m's own ID if a
// workspace model's base isn't listed or the chain loops; a preset whose
// base isn't listed ends the chain.
func rootOf(m listedModel, byID map[string]listedModel) string {
	root := m
	for seen := map[string]bool{m.ID: true}; ; {
		var next listedModel
		if id := root.baseModelID(); id != "" {
			n, ok := byID[id]
			if !ok {
				return m.ID
			}
			next = n
		} else if n, ok := swapBaseOf(root, byID); ok {
			next = n
		} else {
			return root.ID
		}
		if seen[next.ID] {
			return m.ID
		}
		seen[next.ID] = true
		root = next
	}
}

// swapBaseOf returns the listed entry of a llama-swap preset's base model.
// meta.llamaswap.modelID is llama-swap's own ID, and a connection with a
// prefix lists the base as `<prefix>.<ID>`, so it matches that ID exactly or
// after a prefix, and only if exactly one llama-swap base model matches.
func swapBaseOf(m listedModel, byID map[string]listedModel) (listedModel, bool) {
	id := m.swapBaseID()
	if !m.isLlamaSwap() || id == "" {
		return listedModel{}, false
	}
	isBase := func(b listedModel) bool { return b.isLlamaSwap() && b.swapBaseID() == "" }
	if b, ok := byID[id]; ok && isBase(b) {
		return b, true
	}
	var found []listedModel
	for listedID, b := range byID {
		if isBase(b) && strings.HasSuffix(listedID, "."+id) {
			found = append(found, b)
		}
	}
	if len(found) != 1 {
		return listedModel{}, false
	}
	return found[0], true
}

// ListModels lists the models with workspace models and llama-swap presets
// folded in as aliases (see group), and marks a preset left standalone as
// Preset. It keeps what Open WebUI listed for checkLinks, which needs the
// entries' meta.
func (b *Backend) ListModels(ctx context.Context) ([]backend.ModelInfo, error) {
	models, err := b.models(ctx)
	if err != nil {
		return nil, err
	}
	b.listed.Store(&models)
	bases, aliases := group(models)
	infos := make([]backend.ModelInfo, 0, len(bases))
	for _, m := range bases {
		// A preset left standalone, its base not listed, still gets
		// llama-swap's params for the preset after the gateway's.
		preset := m.isLlamaSwap() && m.swapBaseID() != ""
		infos = append(infos, backend.ModelInfo{ID: m.ID, Aliases: aliases[m.ID], Preset: preset})
	}
	return infos, nil
}

func (b *Backend) LoadedModels(ctx context.Context) ([]backend.ModelInfo, error) {
	return b.state.LoadedModelsAdjusted(ctx, b.applyLinkStates)
}

type modelState int

const (
	unloaded modelState = iota
	ready
	starting
	unknown
)

// fetchState reads each model's state from Open WebUI's model list:
//
//   - an Ollama model is ready when `loaded` is true;
//   - a llama-swap model (`meta.llamaswap`) is ready when llama-swap's
//     `status` is `loaded`;
//   - an ai-model-facade virtual model (`current`) has the state its own
//     selection reports: `loaded` is ready, `starting` starting, and
//     `loadIfNone` or `no candidate loaded` unloaded. The chain behind it
//     is never followed;
//   - a workspace model, or a llama-swap preset listed with its base model,
//     has its base model's state (see group);
//   - anything else is unknown.
//
// With links, each read replaces the state of a model a linked backend lists
// with that backend's state (applyLinkStates).
func (b *Backend) fetchState(ctx context.Context) ([]backend.ModelInfo, error) {
	start := time.Now()
	models, err := b.models(ctx)
	if err != nil {
		return nil, err
	}
	catalogs := b.linkCatalogs(ctx)
	bases, aliases := group(models)
	list := &linkList{started: start}
	var infos []backend.ModelInfo
	for _, m := range bases {
		if len(b.links) > 0 {
			base := linkedBase{id: m.ID, aliases: aliases[m.ID]}
			base.link, base.linkedID = linkOf(m, b.links, catalogs)
			list.bases = append(list.bases, base)
		}
		info := backend.ModelInfo{ID: m.ID, Aliases: aliases[m.ID]}
		switch b.stateOf(m) {
		case ready:
		case starting:
			info.Starting = true
		case unknown:
			info.Unknown = true
		default:
			continue
		}
		infos = append(infos, info)
	}
	if len(b.links) > 0 {
		b.storeLinkList(list)
	}
	return infos, nil
}

// linkCatalogs lists each linked backend, to tell which of Open WebUI's
// models it serves. A backend that can't be listed is left out, so its models
// take the state Open WebUI reports until a later fetch lists it.
func (b *Backend) linkCatalogs(ctx context.Context) map[string]*backend.Catalog {
	catalogs := map[string]*backend.Catalog{}
	for _, l := range b.links {
		if models, err := l.target.ListModels(ctx); err == nil {
			catalogs[l.name] = backend.NewCatalog(models)
		}
	}
	return catalogs
}

// linkList is what links need from one Open WebUI fetch: every base model in
// Open WebUI's order, with the link of each model a linked backend lists, so
// a read can apply that backend's state also to models the fetch found
// unloaded.
type linkList struct {
	started time.Time
	bases   []linkedBase
}

type linkedBase struct {
	id       string
	aliases  []string
	link     *link  // nil for a model no linked backend lists
	linkedID string // the ID the linked backend gives this model, a preset's base for a preset
}

// baseOf returns the base model behind Open WebUI model id, which may be a
// model or one of its aliases.
func (l *linkList) baseOf(id string) (linkedBase, bool) {
	for _, base := range l.bases {
		if base.id == id || slices.Contains(base.aliases, id) {
			return base, true
		}
	}
	return linkedBase{}, false
}

// pending counts a request for Open WebUI model id as pending. A model a
// linked backend lists counts in that backend's cache, under the ID it gives
// the model: every read here applies that cache's state, so facades on both
// backends see the load, and its release drops that cache rather than this
// one (a refresh of Open WebUI's list). Any other model counts in this
// backend's cache.
func (b *Backend) pending(id string) (release func()) {
	if list := b.linkList.Load(); list != nil {
		if base, ok := list.baseOf(id); ok && base.link != nil {
			return base.link.target.Pending(base.linkedID)
		}
	}
	return b.state.Pending(id)
}

// storeLinkList keeps list unless a fetch that began later already stored
// its own, so fetches that finish out of order don't bring back an older list.
func (b *Backend) storeLinkList(list *linkList) {
	for {
		old := b.linkList.Load()
		if old != nil && old.started.After(list.started) {
			return
		}
		if b.linkList.CompareAndSwap(old, list) {
			return
		}
	}
}

// applyLinkStates gives each model a linked backend lists the state that
// backend reports at the time of the read, in place of the state the cached
// fetch read. That state comes through the backend's own cache, so its
// pending requests and invalidations show here at once, and under the
// request's own context, so a state fetched since the request arrived is
// reused (backend.WithRequestStart): a gateway that is an Open WebUI
// connection may have read the same backend while Open WebUI answered this
// read. If a linked backend can't be read, its models keep the fetched state.
func (b *Backend) applyLinkStates(ctx context.Context, models []backend.ModelInfo) ([]backend.ModelInfo, error) {
	list := b.linkList.Load()
	if list == nil {
		return models, nil
	}
	// A linked backend reads its state from its server, not through Open
	// WebUI, so this read may wait even under WithoutStateWait.
	states := b.linkStates(backend.WithStateWait(ctx))
	byID := map[string]backend.ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	done := map[string]bool{}
	var out []backend.ModelInfo
	for _, base := range list.bases {
		done[base.id] = true
		linkStates, ok := states[base.link]
		if !ok {
			if m, ok := byID[base.id]; ok {
				out = append(out, m)
			}
			continue
		}
		switch linkStates[base.linkedID] { // not loaded: unloaded
		case ready:
			out = append(out, backend.ModelInfo{ID: base.id, Aliases: base.aliases})
		case starting:
			out = append(out, backend.ModelInfo{ID: base.id, Aliases: base.aliases, Starting: true})
		}
	}
	for _, m := range models {
		if !done[m.ID] { // from a fetch other than the list's
			out = append(out, m)
		}
	}
	return out, nil
}

func (b *Backend) stateOf(m listedModel) modelState {
	switch {
	case m.OwnedBy == "ollama":
		if m.Loaded == nil {
			return unknown
		}
		if *m.Loaded {
			return ready
		}
		return unloaded
	case m.isLlamaSwap():
		if m.Status == nil {
			return unknown
		}
		switch m.Status.Value {
		case "loaded":
			return ready
		case "unloaded":
			return unloaded
		}
		return unknown
	case m.Current != nil:
		switch m.Current.Reason {
		case "loaded":
			return ready
		case "starting":
			return starting
		case "loadIfNone", "no candidate loaded":
			return unloaded
		}
		return unknown
	}
	return unknown
}

// linkStates reads every linked backend at once and returns the states of
// those that answered, by link. A backend left out keeps for its models the
// state Open WebUI reports, as without the link; it going down and coming
// back are each logged once.
func (b *Backend) linkStates(ctx context.Context) map[*link]map[string]modelState {
	read := make([]map[string]modelState, len(b.links))
	var wg sync.WaitGroup
	for i, l := range b.links {
		wg.Go(func() { read[i] = l.states(ctx, b.log) })
	}
	wg.Wait()
	states := map[*link]map[string]modelState{}
	for i, l := range b.links {
		if read[i] != nil {
			states[l] = read[i]
		}
	}
	return states
}

// states maps each model the linked backend has loaded or loading, by the
// ID it gives the model, to its state, or returns nil if the backend can't
// be read. Every linked model of Open WebUI's resolves to its base model
// before the lookup, an alias through the backend's catalog (linkedID).
func (l *link) states(ctx context.Context, log *slog.Logger) map[string]modelState {
	loaded, err := l.target.LoadedModels(ctx)
	if err != nil {
		if !l.down.Swap(true) {
			log.Warn(fmt.Sprintf("linked backend '%s': %v; its models take the state Open WebUI reports until it answers", l.name, err))
		}
		return nil
	}
	if l.down.Swap(false) {
		log.Info(fmt.Sprintf("linked backend '%s' answers again; its models take its state", l.name))
	}
	states := map[string]modelState{}
	for _, m := range loaded {
		state := ready
		if m.Starting {
			state = starting
		}
		states[m.ID] = state
	}
	return states
}

// Chat runs Open WebUI's chat flow for a chat completion. Open WebUI has no
// legacy completions endpoint. The chat is cleaned up in the background once
// the result is known, so the response doesn't wait on that call.
func (b *Backend) Chat(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	if req.Endpoint != backend.ChatCompletions {
		return backend.ChatResult{}, fmt.Errorf("Open WebUI serves only %s, not %s", backend.ChatCompletions, req.Endpoint)
	}
	r := &run{
		b:         b,
		client:    req.Client,
		model:     gjson.GetBytes(req.Body, "model").String(),
		messageID: newID(),
		created:   time.Now().Unix(),
		release:   b.pending(req.StateModel()),
	}
	if err := r.createChat(ctx, req.Body); err != nil {
		r.release()
		return backend.ChatResult{}, err
	}
	if resp, err := r.start(ctx, req); err != nil || resp != nil {
		r.release()
		go r.cleanup()
		return backend.ChatResult{Response: resp}, err
	}
	if req.Stream {
		return backend.ChatResult{Stream: r.stream(ctx, gjson.GetBytes(req.Body, "stream_options.include_usage").Bool())}, nil
	}
	msg, err := r.wait(ctx, nil)
	r.release()
	go r.finish(err != nil && ctx.Err() != nil)
	if err != nil {
		return backend.ChatResult{}, err
	}
	body, _ := json.Marshal(r.completion(msg))
	return backend.ChatResult{Response: &backend.ChatResponse{Status: http.StatusOK, ContentType: "application/json", Body: body}}, nil
}

// run is one completion's trip through Open WebUI.
type run struct {
	b         *Backend
	client    backend.Client
	model     string
	chatID    string
	messageID string // the assistant message Open WebUI fills in
	created   int64
	// release ends the request's pending count in the state cache: at the
	// first output polling sees, since only a loaded model produces any, or
	// when the run ends without output.
	release func()
}

// createChat creates the chat with the client's last user message and an
// empty assistant message after it. The chat is only a record: the request
// names no stored user message, so Open WebUI sends the client's own
// messages to the model, not this history.
func (r *run) createChat(ctx context.Context, body []byte) error {
	prompt := lastUserText(body)
	userID, now := newID(), time.Now().Unix()
	chat := map[string]any{"chat": map[string]any{
		"title":  title(prompt),
		"models": []string{r.model},
		"history": map[string]any{
			"currentId": r.messageID,
			"messages": map[string]any{
				userID:      map[string]any{"id": userID, "role": "user", "content": prompt, "timestamp": now, "models": []string{r.model}, "childrenIds": []string{r.messageID}},
				r.messageID: map[string]any{"id": r.messageID, "role": "assistant", "content": "", "parentId": userID, "childrenIds": []string{}, "model": r.model, "modelName": r.model, "modelIdx": 0, "done": false, "timestamp": now},
			},
		},
	}}
	payload, _ := json.Marshal(chat)
	resp, err := r.b.Do(ctx, http.MethodPost, "/api/v1/chats/new", payload, "", r.client)
	if err != nil {
		return err
	}
	data, err := openaihttp.ReadBody(resp)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("creating chat: %s: %s", resp.Status, snippet(data))
	}
	if r.chatID = gjson.GetBytes(data, "id").String(); r.chatID == "" {
		return fmt.Errorf("creating chat: no chat id in response: %s", snippet(data))
	}
	return nil
}

// start sends the completion: the client's body, edited byte-wise, with the
// chat, message and session that make it a UI request. stream is always on,
// since Open WebUI only runs native tools on its streaming path; whether the
// client gets a stream is decided here, not by Open WebUI. With chat_id and
// session_id set, Open WebUI runs the generation as a background task and
// answers at once with its task_ids, which is why the content is read by
// polling the chat (Open WebUI source, main.py chat_completion; verified on
// 0.12.3). An answer without task_ids is logged as an error and the chat is
// polled anyway: the reply may still arrive, but only once it is finished. A
// refusal from Open WebUI is returned as the response, like any backend's
// error.
func (r *run) start(ctx context.Context, req backend.ChatRequest) (*backend.ChatResponse, error) {
	body := req.Body
	var err error
	edits := []struct {
		path string
		raw  string
	}{
		{"stream", "true"},
		{"chat_id", quote(r.chatID)},
		{"id", quote(r.messageID)},
		{"session_id", quote("ai-model-facade-" + newID())},
		{"background_tasks", `{"title_generation":false,"tags_generation":false,"follow_up_generation":false}`},
	}
	if len(req.Client.Route) > 0 {
		route, _ := json.Marshal(req.Client.Route)
		edits = append(edits, struct{ path, raw string }{backend.RouteField, string(route)})
	}
	for _, e := range edits {
		if body, err = sjson.SetRawBytes(body, e.path, []byte(e.raw)); err != nil {
			return nil, err
		}
	}
	resp, err := r.b.Do(ctx, http.MethodPost, "/api/chat/completions", body, "", r.client)
	if err != nil {
		return nil, err
	}
	data, err := openaihttp.ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return &backend.ChatResponse{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: data, RetryAfter: resp.Header.Get("Retry-After")}, nil
	}
	if !gjson.GetBytes(data, "task_ids").IsArray() {
		r.b.log.Error("Open WebUI started no background task; the reply can't stream and arrives only once finished", "chat", r.chatID, "response", snippet(data))
	}
	return nil, nil
}

// message is the assistant message as a poll sees it.
type message struct {
	content   string
	reasoning string
	done      bool
	usage     gjson.Result
}

// wait polls until the message is done and the chat has no task left,
// passing each new snapshot of the message to onPoll. If ctx ends first, the
// task may still be running; the caller stops it with finish.
func (r *run) wait(ctx context.Context, onPoll func(message) error) (message, error) {
	ticker := time.NewTicker(r.b.poll)
	defer ticker.Stop()
	for {
		msg, err := r.poll(ctx)
		if err == nil && onPoll != nil {
			err = onPoll(msg)
		}
		if err == nil && msg.done {
			var idle bool
			if idle, err = r.idle(ctx); err == nil && idle {
				return msg, nil
			}
		}
		if err != nil {
			return message{}, err
		}
		select {
		case <-ctx.Done():
			return message{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *run) poll(ctx context.Context) (message, error) {
	data, err := r.get(ctx, "/api/v1/chats/"+r.chatID)
	if err != nil {
		return message{}, err
	}
	m := gjson.GetBytes(data, "chat.history.messages."+gjson.Escape(r.messageID))
	if !m.Exists() {
		return message{}, fmt.Errorf("assistant message %s missing from chat %s", r.messageID, r.chatID)
	}
	if e := m.Get("error"); e.Exists() && e.Type != gjson.Null {
		text := e.Get("content").String()
		if text == "" {
			text = e.Raw
		}
		return message{}, fmt.Errorf("Open WebUI: %s", text)
	}
	msg := message{content: m.Get("content").String(), reasoning: reasoningOf(m), done: m.Get("done").Bool(), usage: m.Get("usage")}
	if msg.content != "" || msg.reasoning != "" || msg.done {
		r.release()
	}
	return msg, nil
}

// reasoningOf joins the text of the message's reasoning items. Open WebUI
// keeps the model's reasoning in the message's output list, not in its
// content, and saves it as it grows (verified on 0.12.3). A tool loop has one
// reasoning item per model call, each finished before the next starts, so the
// joined text only grows while the task runs.
func reasoningOf(m gjson.Result) string {
	var items []string
	for _, item := range m.Get("output").Array() {
		if item.Get("type").String() != "reasoning" {
			continue
		}
		var text strings.Builder
		for _, part := range item.Get("content").Array() {
			text.WriteString(part.Get("text").String())
		}
		if text.Len() > 0 {
			items = append(items, text.String())
		}
	}
	return strings.Join(items, "\n\n")
}

// idle reports whether the chat has no task left running.
func (r *run) idle(ctx context.Context) (bool, error) {
	data, err := r.get(ctx, "/api/tasks/chat/"+r.chatID)
	if err != nil {
		return false, err
	}
	return len(gjson.GetBytes(data, "task_ids").Array()) == 0, nil
}

func (r *run) get(ctx context.Context, path string) ([]byte, error) {
	resp, err := r.b.Do(ctx, http.MethodGet, path, nil, "", r.client)
	if err != nil {
		return nil, err
	}
	data, err := openaihttp.ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, snippet(data))
	}
	return data, nil
}

// cleanupTimeout bounds the calls made after the client is gone.
const cleanupTimeout = 10 * time.Second

// stop asks Open WebUI to stop the chat's task, best effort.
func (r *run) stop() {
	r.afterwards(http.MethodPost, "/api/tasks/chat/"+r.chatID+"/stop")
}

// finish makes the calls that follow a chat: stop, if the client left while
// the task may still be running, then cleanup. Callers run it in the
// background, so neither a response nor Close waits on Open WebUI. Stop comes
// first, so the task isn't left running against a chat that no longer exists.
func (r *run) finish(stop bool) {
	if stop {
		r.stop()
	}
	r.cleanup()
}

// cleanup deletes, archives or keeps the chat, as configured.
func (r *run) cleanup() {
	switch r.b.chats {
	case "delete":
		r.afterwards(http.MethodDelete, "/api/v1/chats/"+r.chatID)
	case "archive":
		r.afterwards(http.MethodPost, "/api/v1/chats/"+r.chatID+"/archive")
	}
}

// afterwards makes a call that must happen even when the request's context
// has ended, and logs a failure, since nobody is left to report it to.
func (r *run) afterwards(method, path string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	resp, err := r.b.Do(ctx, method, path, nil, "", r.client)
	if err == nil {
		var data []byte
		if data, err = openaihttp.ReadBody(resp); err == nil && resp.StatusCode/100 != 2 {
			err = fmt.Errorf("%s: %s", resp.Status, snippet(data))
		}
	}
	if err != nil {
		r.b.log.Warn("Open WebUI call failed", "method", method, "path", path, "chat", r.chatID, "error", err)
	}
}

// completion builds the OpenAI response for a finished message.
func (r *run) completion(msg message) map[string]any {
	reply := map[string]any{"role": "assistant", "content": msg.content}
	if msg.reasoning != "" {
		reply["reasoning_content"] = msg.reasoning
	}
	resp := map[string]any{
		"id":      "chatcmpl-" + r.messageID,
		"object":  "chat.completion",
		"created": r.created,
		"model":   r.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       reply,
			"finish_reason": "stop",
		}},
	}
	if usage := usageOf(msg); usage != nil {
		resp["usage"] = usage
	}
	return resp
}

// usageOf converts the message's usage to OpenAI's. The stored usage varies
// by connection: Ollama models carry OpenAI's names, while an OpenAI-type
// connection (seen through a gateway to llama-swap) carried only
// input_tokens and output_tokens.
//
// input_tokens and output_tokens come first because they cover the entire
// reply: since v0.11.0, Open WebUI's merge_usage adds input_tokens,
// output_tokens and total_tokens across the model calls of a tool loop, while
// prompt_tokens and completion_tokens keep only the last call's values
// (Open WebUI source, utils/response.py; verified on 0.12.3 with a three-call
// tool loop; the v0.11.0 start is from the source only). total_tokens is used
// only next to both summed counts, so the three always agree.
func usageOf(msg message) map[string]any {
	tokens := func(names ...string) (int64, bool) {
		for _, n := range names {
			if v := msg.usage.Get(n); v.Exists() {
				return v.Int(), true
			}
		}
		return 0, false
	}
	prompt, okPrompt := tokens("input_tokens", "prompt_tokens")
	completion, okCompletion := tokens("output_tokens", "completion_tokens")
	if !okPrompt && !okCompletion {
		return nil
	}
	total := prompt + completion
	if msg.usage.Get("input_tokens").Exists() && msg.usage.Get("output_tokens").Exists() {
		if t, ok := tokens("total_tokens"); ok {
			total = t
		}
	}
	return map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": total}
}

func (r *run) chunk(delta map[string]any, finish any) []byte {
	data, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-" + r.messageID,
		"object":  "chat.completion.chunk",
		"created": r.created,
		"model":   r.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	return data
}

// stream polls in the background and turns each growth of the message's
// reasoning and content into a chunk, reasoning first, as llama-server streams
// reasoning_content ahead of content. The chat is finished after the stream
// has ended, so neither the client's response nor Close waits on those calls.
func (r *run) stream(ctx context.Context, includeUsage bool) backend.ChatStream {
	ctx, cancel := context.WithCancel(ctx)
	s := &chunkStream{events: make(chan streamEvent), cancel: cancel, done: make(chan struct{})}
	go func() {
		stop := false
		defer func() { r.finish(stop) }()
		defer r.release()
		defer close(s.done)
		defer close(s.events)
		send := func(data []byte) error {
			select {
			case s.events <- streamEvent{data: data}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if send(r.chunk(map[string]any{"role": "assistant", "content": ""}, nil)) != nil {
			stop = true
			return
		}
		var sentReasoning, sentContent string
		msg, err := r.wait(ctx, func(msg message) error {
			if delta, ok := growth(msg.reasoning, &sentReasoning); ok {
				if err := send(r.chunk(map[string]any{"reasoning_content": delta}, nil)); err != nil {
					return err
				}
			}
			if delta, ok := growth(msg.content, &sentContent); ok {
				return send(r.chunk(map[string]any{"content": delta}, nil))
			}
			return nil
		})
		stop = err != nil && ctx.Err() != nil
		if err == nil {
			err = send(r.chunk(map[string]any{}, "stop"))
		}
		if err == nil && includeUsage {
			if usage := usageOf(msg); usage != nil {
				data, _ := json.Marshal(map[string]any{"id": "chatcmpl-" + r.messageID, "object": "chat.completion.chunk", "created": r.created, "model": r.model, "choices": []any{}, "usage": usage})
				err = send(data)
			}
		}
		if err != nil && ctx.Err() == nil {
			select {
			case s.events <- streamEvent{err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return s
}

// growth returns what snapshot adds to *sent and records it as sent. Open
// WebUI only ever appends while a task runs; a snapshot that doesn't extend
// what was sent waits for one that does.
func growth(snapshot string, sent *string) (string, bool) {
	if len(snapshot) <= len(*sent) || !strings.HasPrefix(snapshot, *sent) {
		return "", false
	}
	delta := snapshot[len(*sent):]
	*sent = snapshot
	return delta, true
}

type streamEvent struct {
	data []byte
	err  error
}

// chunkStream is a backend.ChatStream fed by run.stream's goroutine.
type chunkStream struct {
	events chan streamEvent
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *chunkStream) Next() ([]byte, error) {
	ev, ok := <-s.events
	if !ok {
		return nil, io.EOF
	}
	return ev.data, ev.err
}

// Close stops polling. Stopping the task, if it is still running, and the
// chat's cleanup follow in the background; Close doesn't wait for them.
func (s *chunkStream) Close() error {
	s.cancel()
	<-s.done
	return nil
}

// lastUserText is the text of the client's last user message, for the chat
// record: a string, or the text parts of a content array.
func lastUserText(body []byte) string {
	messages := gjson.GetBytes(body, "messages").Array()
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Get("role").String() != "user" {
			continue
		}
		content := m.Get("content")
		if !content.IsArray() {
			return content.String()
		}
		var parts []string
		for _, p := range content.Array() {
			if p.Get("type").String() == "text" {
				parts = append(parts, p.Get("text").String())
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// title names a kept or archived chat after the start of its prompt.
func title(prompt string) string {
	const max = 60
	prompt = strings.Join(strings.Fields(prompt), " ")
	if prompt == "" {
		return "ai-model-facade"
	}
	if utf8.RuneCountInString(prompt) <= max {
		return prompt
	}
	return string([]rune(prompt)[:max]) + "…"
}

// newID returns a random UUID (version 4), the form Open WebUI uses for
// chat and message IDs.
func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func quote(s string) string {
	q, _ := json.Marshal(s)
	return string(q)
}

// snippet shortens a response body for an error message.
func snippet(data []byte) string {
	const max = 300
	if len(data) > max {
		return string(data[:max]) + "…"
	}
	return string(data)
}

var _ backend.ModelStateProvider = (*Backend)(nil)
