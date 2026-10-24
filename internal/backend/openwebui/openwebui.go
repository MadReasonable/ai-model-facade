// Package openwebui is the backend adapter for Open WebUI. A completion runs
// Open WebUI's own chat flow, so its tools and features run server-side:
//
//  1. create a chat holding the prompt and an empty assistant message;
//  2. start the completion with chat_id, the message id and a session_id,
//     which makes Open WebUI treat it as a UI request (built-in tools, the
//     tool loop, partial output while it runs);
//  3. poll the chat for the growing message, and the chat's tasks, until the
//     message is done and no task is left;
//  4. build an OpenAI response or chunk stream from it, then clean up the chat.
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
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/llamaswap"
	"github.com/MadReasonable/ai-model-facade/internal/backend/openaihttp"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

type Config struct {
	Type                string `yaml:"type"`
	openaihttp.Config   `yaml:",inline"`
	Chats               string           `yaml:"chats"`               // delete (default) | archive | keep
	PollIntervalSeconds float64          `yaml:"pollIntervalSeconds"` // default 0.25
	LlamaSwap           *LlamaSwapConfig `yaml:"llamaSwap"`
}

// LlamaSwapConfig takes the state of Open WebUI's llama-swap models from a
// llamaSwap backend of this gateway, whose /running is exact, instead of from
// llama-swap's `status`, which reads `loaded` while starting and stopping too.
type LlamaSwapConfig struct {
	Backend  string `yaml:"backend"`
	PrefixID string `yaml:"prefixId"` // the Open WebUI connection's prefix_id, if any
}

type Backend struct {
	*openaihttp.Client        // its Chat is shadowed by the chat flow below
	name               string // its entry under backends
	state              *openaihttp.StateCache
	chats              string
	poll               time.Duration
	llamaSwapConfig    *LlamaSwapConfig
	llamaSwap          *llamaswap.Backend            // set by Link
	listed             atomic.Pointer[[]listedModel] // what ListModels last fetched, for checkPrefixes
	swapDown           atomic.Bool                   // the llamaSwap backend's last state read failed
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
	if c.LlamaSwap != nil && c.LlamaSwap.Backend == "" {
		return nil, fmt.Errorf("llamaSwap.backend is required")
	}
	client, err := openaihttp.New(c.Config)
	if err != nil {
		return nil, err
	}
	b := &Backend{Client: client, name: entry.Name, chats: c.Chats, poll: poll, llamaSwapConfig: c.LlamaSwap, log: log}
	b.state = openaihttp.NewStateCache(c.Config, b.fetchState)
	return b, nil
}

// Link resolves the llamaSwap block once every backend exists; naming a
// backend that isn't a llamaSwap one fails. It then asks for the prefix check
// (see checkPrefixes), which runs once both backends have been listed.
func (b *Backend) Link(backends map[string]backend.Backend, check func(backend.ListingCheck)) error {
	if b.llamaSwapConfig == nil {
		return nil
	}
	name := b.llamaSwapConfig.Backend
	ls, ok := backends[name].(*llamaswap.Backend)
	if !ok {
		if backends[name] == nil {
			return fmt.Errorf("llamaSwap.backend '%s' is not defined", name)
		}
		return fmt.Errorf("llamaSwap.backend '%s' is not a llamaSwap backend", name)
	}
	b.llamaSwap = ls
	check(backend.ListingCheck{Backends: []string{b.name, name}, Run: func(catalogs map[string]*backend.Catalog, warn func(error)) {
		// Listing this backend stored what Open WebUI listed, entries the
		// catalog doesn't keep.
		if models := b.listed.Load(); models != nil {
			b.checkPrefixes(*models, catalogs[name], func(err error) { warn(fmt.Errorf("backends.%s: %w", b.name, err)) })
		}
	}})
	return nil
}

// checkPrefixes checks that each llama-swap model Open WebUI lists, prefix
// removed, is listed by the llamaSwap backend. A mismatch is a config error
// that never recovers by itself: those models are never found among
// llama-swap's running models, so they read as unloaded.
func (b *Backend) checkPrefixes(models []listedModel, catalog *backend.Catalog, warn func(error)) {
	for _, m := range models {
		if !m.isLlamaSwap() {
			continue
		}
		if id, ok := b.llamaSwapID(m.ID); !ok || !catalog.Contains(id) {
			warn(fmt.Errorf("Open WebUI model '%s' is a llama-swap model, but '%s' is not listed by llamaSwap.backend '%s' and won't count as loaded; check llamaSwap.prefixId", m.ID, id, b.llamaSwapConfig.Backend))
		}
	}
}

// llamaSwapID turns an Open WebUI model ID into the llama-swap ID it stands
// for, by removing the connection's prefix.
func (b *Backend) llamaSwapID(id string) (string, bool) {
	if b.llamaSwapConfig.PrefixID == "" {
		return id, true
	}
	return strings.CutPrefix(id, b.llamaSwapConfig.PrefixID+".")
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

// group folds workspace models into their base model as aliases, like
// llama-swap presets: a workspace model is loaded when its base is. A chain
// of workspace models resolves to the first model that isn't one; a
// workspace model whose base isn't listed stands alone.
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

// rootOf follows base_model_id to the first listed model that has none, and
// returns m's own ID if its base isn't listed or the chain loops.
func rootOf(m listedModel, byID map[string]listedModel) string {
	root := m
	for seen := map[string]bool{m.ID: true}; root.baseModelID() != ""; {
		next, ok := byID[root.baseModelID()]
		if !ok || seen[next.ID] {
			return m.ID
		}
		seen[next.ID] = true
		root = next
	}
	return root.ID
}

// ListModels lists the models with workspace models folded in as aliases. It
// keeps what Open WebUI listed for checkPrefixes, which needs the entries'
// meta.
func (b *Backend) ListModels(ctx context.Context) ([]backend.ModelInfo, error) {
	models, err := b.models(ctx)
	if err != nil {
		return nil, err
	}
	b.listed.Store(&models)
	bases, aliases := group(models)
	infos := make([]backend.ModelInfo, 0, len(bases))
	for _, m := range bases {
		infos = append(infos, backend.ModelInfo{ID: m.ID, Aliases: aliases[m.ID]})
	}
	return infos, nil
}

func (b *Backend) LoadedModels(ctx context.Context) ([]backend.ModelInfo, error) {
	return b.state.LoadedModels(ctx)
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
//     `status` is `loaded`, or, with the llamaSwap block, has the state the
//     named backend reports;
//   - an ai-model-facade virtual model (`current`) has the state its own
//     selection reports: `loaded` is ready, `starting` starting, and
//     `loadIfNone` or `no candidate loaded` unloaded. The chain behind it
//     is never followed;
//   - a workspace model has its base model's state;
//   - anything else is unknown.
func (b *Backend) fetchState(ctx context.Context) ([]backend.ModelInfo, error) {
	models, err := b.models(ctx)
	if err != nil {
		return nil, err
	}
	swapStates := b.swapStates(ctx)
	bases, aliases := group(models)
	var infos []backend.ModelInfo
	for _, m := range bases {
		info := backend.ModelInfo{ID: m.ID, Aliases: aliases[m.ID]}
		switch b.stateOf(m, swapStates) {
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
	return infos, nil
}

func (b *Backend) stateOf(m listedModel, swapStates map[string]modelState) modelState {
	switch {
	case m.OwnedBy == "ollama":
		if m.Loaded == nil {
			return unknown
		}
		if *m.Loaded {
			return ready
		}
		return unloaded
	case m.isLlamaSwap() && swapStates != nil:
		id, _ := b.llamaSwapID(m.ID)
		return swapStates[id] // unlisted means not running: unloaded
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

// swapStates returns the llamaSwap backend's states, or nil to read
// llama-swap's own `status` from Open WebUI's list instead, as if the
// llamaSwap block weren't set: when the block isn't set, or the backend can't
// be reached. Going down and coming back are each logged once.
func (b *Backend) swapStates(ctx context.Context) map[string]modelState {
	if b.llamaSwap == nil {
		return nil
	}
	states, err := b.llamaSwapStates(ctx)
	if err != nil {
		if !b.swapDown.Swap(true) {
			b.log.Warn(fmt.Sprintf("%v; using llama-swap's status as Open WebUI reports it until it answers", err))
		}
		return nil
	}
	if b.swapDown.Swap(false) {
		b.log.Info(fmt.Sprintf("llamaSwap.backend '%s' answers again; using its state", b.llamaSwapConfig.Backend))
	}
	return states
}

// llamaSwapStates maps each running llama-swap model and alias to its state.
func (b *Backend) llamaSwapStates(ctx context.Context) (map[string]modelState, error) {
	loaded, err := b.llamaSwap.LoadedModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("llamaSwap.backend '%s': %w", b.llamaSwapConfig.Backend, err)
	}
	states := map[string]modelState{}
	for _, m := range loaded {
		state := ready
		if m.Starting {
			state = starting
		}
		for _, id := range append([]string{m.ID}, m.Aliases...) {
			states[id] = state
		}
	}
	return states, nil
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
	}
	if err := r.createChat(ctx, req.Body); err != nil {
		return backend.ChatResult{}, err
	}
	if resp, err := r.start(ctx, req); err != nil || resp != nil {
		go r.cleanup()
		return backend.ChatResult{Response: resp}, err
	}
	if req.Stream {
		return backend.ChatResult{Stream: r.stream(ctx, gjson.GetBytes(req.Body, "stream_options.include_usage").Bool())}, nil
	}
	msg, err := r.wait(ctx, nil)
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
// client gets a stream is decided here, not by Open WebUI. A refusal from Open
// WebUI is returned as the response, like any backend's error.
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
		return &backend.ChatResponse{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: data}, nil
	}
	return nil, nil
}

// message is the assistant message as a poll sees it.
type message struct {
	content string
	done    bool
	usage   gjson.Result
}

// wait polls until the message is done and the chat has no task left,
// passing each new snapshot of the content to onContent. If ctx ends first,
// the task may still be running; the caller stops it with finish.
func (r *run) wait(ctx context.Context, onContent func(string) error) (message, error) {
	ticker := time.NewTicker(r.b.poll)
	defer ticker.Stop()
	for {
		msg, err := r.poll(ctx)
		if err == nil && onContent != nil {
			err = onContent(msg.content)
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
	return message{content: m.Get("content").String(), done: m.Get("done").Bool(), usage: m.Get("usage")}, nil
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
	resp := map[string]any{
		"id":      "chatcmpl-" + r.messageID,
		"object":  "chat.completion",
		"created": r.created,
		"model":   r.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": msg.content},
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
// (Open WebUI source, utils/response.py; not run). total_tokens is used only
// next to both summed counts, so the three always agree.
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

// stream polls in the background and turns each growth of the message into
// a chunk. Open WebUI only ever appends to the content while a task runs; a
// snapshot that doesn't extend what was sent waits for one that does. The
// chat is finished after the stream has ended, so neither the client's
// response nor Close waits on those calls.
func (r *run) stream(ctx context.Context, includeUsage bool) backend.ChatStream {
	ctx, cancel := context.WithCancel(ctx)
	s := &chunkStream{events: make(chan streamEvent), cancel: cancel, done: make(chan struct{})}
	go func() {
		stop := false
		defer func() { r.finish(stop) }()
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
		sent := ""
		msg, err := r.wait(ctx, func(content string) error {
			if len(content) <= len(sent) || !strings.HasPrefix(content, sent) {
				return nil
			}
			delta := content[len(sent):]
			sent = content
			return send(r.chunk(map[string]any{"content": delta}, nil))
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
