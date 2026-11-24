package gateway

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/pipeline"
	"github.com/MadReasonable/ai-model-facade/internal/protocols"
	"github.com/MadReasonable/ai-model-facade/internal/virtualmodel"
)

// maxBodyBytes caps request bodies: generous enough for several base64
// images. A var so tests can lower it.
var maxBodyBytes int64 = 100 << 20

// bodyReadTimeout bounds how long a client may take to send its body.
const bodyReadTimeout = 2 * time.Minute

type server struct {
	gateway *Gateway
	models  map[string]virtualmodel.Handler
	log     *slog.Logger
	started int64
	// live is the Live serving the gateway, which holds the draft and the
	// files for the admin endpoints; nil where the gateway isn't served by
	// one, and the endpoints are left out.
	live *Live
}

// Handler returns the front end for every client API: HTTP and SSE framing
// only.
func (g *Gateway) Handler(log *slog.Logger) http.Handler {
	return g.handler(log, nil, allEndpoints, "", "")
}

// The sets of endpoints a handler serves. With server.adminApi.listen set,
// server.listen serves the client APIs and that address the admin
// endpoints and the admin UI; without it, server.listen serves them all.
// Both serve /health and /amf/models, which any client may read.
const (
	allEndpoints = iota
	clientEndpoints
	adminEndpoints
)

// handler is Handler for one set of endpoints, with the Live that serves g,
// for the admin endpoints, or nil to leave them out, the path to serve the
// admin UI at, and adminTLS, the HTTPS address the admin routes redirect
// to, or "" to serve them.
func (g *Gateway) handler(log *slog.Logger, live *Live, endpoints int, uiPath, adminTLS string) http.Handler {
	s := &server{gateway: g, models: map[string]virtualmodel.Handler{}, log: log, started: time.Now().Unix(), live: live}
	for _, m := range g.Models {
		s.models[m.Name] = m.Handler
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /amf/models", s.authenticated(amfFamily, s.listFacades))
	mux.HandleFunc("GET /amf/models/{id...}", s.authenticated(amfFamily, s.getFacade))
	if live != nil && endpoints != clientEndpoints {
		s.routeAdmin(mux, uiPath, adminTLS)
	}
	if endpoints != adminEndpoints {
		api := g.Config.API
		s.routeOpenAI(mux, api.OpenAI.BasePath)
		s.routeAnthropic(mux, api.Anthropic.BasePath)
		if api.Default == config.FamilyOpenAI {
			s.routeOpenAI(mux, "")
		} else {
			s.routeAnthropic(mux, "")
		}
	}
	// Every request carries its arrival time, so its state reads reuse any
	// state fetched since then (backend.WithRequestStart).
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(backend.WithRequestStart(r.Context(), time.Now())))
	})
}

// health reports that the gateway is up. It needs no API key, so health
// checkers can call it, and doesn't contact any backend.
func health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"status":"ok"}`+"\n")
}

type clientHandler func(w http.ResponseWriter, r *http.Request, client backend.Client)

// authenticated identifies the caller from its bearer key. A key listed in
// server.apiKeys gets that entry's name. Otherwise, with no keys listed or
// unknownKeys: allow, the request is accepted: an unlisted key is named
// "unknown" with a fingerprint, and no key leaves the name empty. With keys
// listed and unknownKeys: reject, anything else is a 401.
//
// Keys are matched by their SHA-256, the form every entry is indexed in. A
// map lookup's timing can tell an attacker something about a hash, but
// nothing about a key that hashes to it.
func (s *server) authenticated(family *apiFamily, next clientHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client, ok := identify(s.gateway.Config.Server, family.credential(r))
		if !ok {
			s.reject(family, w, r, client, http.StatusUnauthorized, "invalid_api_key", "invalid or missing API key")
			return
		}
		next(w, r, client)
	}
}

// identify finds who header, an Authorization header as sent, belongs to
// under cfg; ok is false if cfg refuses it.
func identify(cfg config.Server, header string) (client backend.Client, ok bool) {
	client = backend.Client{Authorization: header}
	token, hasToken := strings.CutPrefix(header, "Bearer ")
	if hasToken {
		digest := sha256.Sum256([]byte(token))
		if name, ok := cfg.KeyName(digest); ok {
			client.Name, client.Groups = name, cfg.GroupsOf(name)
			return client, true
		}
		client.Fingerprint = hex.EncodeToString(digest[:4])
	}
	if len(cfg.APIKeys) > 0 && cfg.UnknownKeys != "allow" {
		return client, false
	}
	if hasToken {
		client.Name = config.UnknownClient
	}
	client.Groups = []string{config.GroupUnknown}
	return client, true
}

// modelObject is a virtual model in OpenAI's model-object shape, plus
// `current` when api.openAi.addCurrent is on, an addition clients that don't
// know it ignore.
type modelObject struct {
	ID          string         `json:"id"`
	Object      string         `json:"object"`
	Created     int64          `json:"created"`
	OwnedBy     string         `json:"owned_by"`
	Description string         `json:"description,omitempty"`
	Current     *currentObject `json:"current,omitempty"`
}

// currentObject is where a request would be routed right now: a snapshot,
// which can change by the next request.
type currentObject struct {
	Model   string `json:"model,omitempty"`
	AliasOf string `json:"aliasOf,omitempty"`
	Backend string `json:"backend,omitempty"` // when it isn't the virtual model's own
	Reason  string `json:"reason"`
}

func newCurrentObject(cur virtualmodel.Current) *currentObject {
	return &currentObject{Model: cur.Model, AliasOf: cur.AliasOf, Backend: cur.Backend, Reason: cur.Reason}
}

// currentOf returns where a request for m would go now. If the backend's
// state can't be read it returns nil, so the caller leaves `current` out
// rather than failing the listing. A backend with loopsBack whose refresh is
// already in flight, with nothing cached, is the loop through Open WebUI
// broken as designed (backend.ErrStateBusy), so that is logged at INFO; any
// other failure is a WARN.
func (s *server) currentOf(ctx context.Context, m Model, client backend.Client) *currentObject {
	cur, err := m.Handler.Current(ctx, client)
	if errors.Is(err, backend.ErrStateBusy) {
		s.log.Info("current selection unavailable", "model", m.Name, "error", err)
		return nil
	}
	if err != nil {
		s.log.Warn("current selection unavailable", "model", m.Name, "error", err)
		return nil
	}
	return newCurrentObject(cur)
}

// modelObject describes m for an OpenAI client. With api.openAi.addCurrent off,
// no backend's state is read.
func (s *server) modelObject(ctx context.Context, m Model, client backend.Client) modelObject {
	obj := modelObject{ID: m.Name, Object: "model", Created: s.started, OwnedBy: "ai-model-facade", Description: m.Handler.Description()}
	if s.gateway.Config.API.OpenAI.AddCurrent {
		obj.Current = s.currentOf(ctx, m, client)
	}
	return obj
}

// allows reports whether client may ask for facade m. A facade it may not is
// left out of every model list, and a request for one gets the same 404 as a
// facade that doesn't exist, so the lists and the requests agree.
func allows(m Model, client backend.Client) bool {
	return m.Handler.Access().Allows(client.Name, client.Groups)
}

// listingContext is the context for building `current` in an OpenAI listing.
// It never waits on a state refresh already in flight, which may be the one
// asking: Open WebUI's model refresh calls this gateway's /v1/models while
// this gateway fetches it.
func listingContext(ctx context.Context) context.Context {
	return backend.WithoutStateWait(ctx)
}

func (s *server) listModels(w http.ResponseWriter, r *http.Request, client backend.Client) {
	list := struct {
		Object string        `json:"object"`
		Data   []modelObject `json:"data"`
	}{Object: "list", Data: []modelObject{}}
	ctx := listingContext(backend.WithStateSnapshot(r.Context()))
	for _, m := range s.gateway.Models {
		if s.lists(openAIFamily, m) && allows(m, client) {
			list.Data = append(list.Data, s.modelObject(ctx, m, client))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (s *server) getModel(w http.ResponseWriter, r *http.Request, client backend.Client) {
	id := r.PathValue("id")
	for _, m := range s.gateway.Models {
		if m.Name == id && s.lists(openAIFamily, m) && allows(m, client) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s.modelObject(listingContext(r.Context()), m, client))
			return
		}
	}
	writeOpenAIError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model '%s' does not exist", id))
}

// facadeObject is a facade in the gateway's own /amf shape, the same in the
// list and alone. It always reports `current` and its candidates, whatever
// api.openAi.addCurrent says.
type facadeObject struct {
	ID          string            `json:"id"`
	Description string            `json:"description,omitempty"`
	Current     *currentObject    `json:"current,omitempty"`
	Candidates  []candidateObject `json:"candidates,omitempty"`
}

// candidateObject is one candidate of a facade and its state now.
type candidateObject struct {
	Model      string `json:"model"`
	Backend    string `json:"backend,omitempty"`
	LoadIfNone bool   `json:"loadIfNone,omitempty"`
	State      string `json:"state"`
	Match      string `json:"match,omitempty"`
	AliasOf    string `json:"aliasOf,omitempty"`
	Winner     bool   `json:"winner,omitempty"`
}

// listFacades serves /amf/models. Unlike /v1/models it may wait on a state
// refresh in flight: Open WebUI calls /v1/models during its refresh, never
// this, so the refresh can't be waiting on this request. With ?follow, it
// streams the list instead (followFacades).
func (s *server) listFacades(w http.ResponseWriter, r *http.Request, client backend.Client) {
	if r.URL.Query().Has("follow") {
		if s.live == nil {
			// Only a Live keeps the check the streams list under.
			s.reject(amfFamily, w, r, client, http.StatusBadRequest, "invalid_request_error", "?follow needs a gateway run by serve")
			return
		}
		s.followFacades(w, r, client.Authorization)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.facadeList(backend.WithStateSnapshot(r.Context()), s.gateway.Models, client))
}

// facadeList is the body of /amf/models: the facades of models client may
// use, read under ctx.
func (s *server) facadeList(ctx context.Context, models []Model, client backend.Client) facadeList {
	list := facadeList{Models: []facadeObject{}}
	for _, m := range models {
		if allows(m, client) {
			list.Models = append(list.Models, s.facadeObject(ctx, m, client))
		}
	}
	return list
}

type facadeList struct {
	Models []facadeObject `json:"models"`
}

// followFacades streams /amf/models as server-sent events: the list now,
// then the list again after each check of modelsWatch that changes it. A
// stream outlives a reload, so before each list and each keep-alive it finds
// the caller again by the credential it sent, under the running config,
// and lists the running gateway's facades; it ends once that config no
// longer accepts the key.
func (s *server) followFacades(w http.ResponseWriter, r *http.Request, header string) {
	snapshot, next := s.live.followModels()
	defer s.live.unfollowModels()
	ended := s.live.modelsStreamsEnded()
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // so nginx passes each event on as it comes
	w.WriteHeader(http.StatusOK)
	var last []byte
	send := func() bool {
		g := s.live.Gateway()
		client, ok := identify(g.Config.Server, header)
		if !ok {
			endKeyRefused(w, rc)
			return false
		}
		body, err := json.Marshal(s.facadeList(snapshot, g.Models, client))
		if err == nil && !bytes.Equal(body, last) {
			writeSSE(w, "", body)
			last = body
		}
		rc.Flush()
		return true
	}
	if !send() {
		return
	}
	keepAlive := time.NewTicker(keepAliveInterval)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ended:
			writeSSE(w, "end", []byte(`{"reason":"the gateway ended the stream: it is shutting down"}`))
			rc.Flush()
			return
		case <-next:
			snapshot, next = s.live.modelsNow()
			if !send() {
				return
			}
		case <-keepAlive.C:
			if _, ok := identify(s.live.Gateway().Config.Server, header); !ok {
				endKeyRefused(w, rc)
				return
			}
			fmt.Fprint(w, ": keep-alive\n\n")
			rc.Flush()
		}
	}
}

func endKeyRefused(w http.ResponseWriter, rc *http.ResponseController) {
	writeSSE(w, "end", []byte(`{"reason":"the gateway no longer accepts this client's key"}`))
	rc.Flush()
}

// getFacade serves /amf/models/{id}: one entry of the list.
func (s *server) getFacade(w http.ResponseWriter, r *http.Request, client backend.Client) {
	id := r.PathValue("id")
	for _, m := range s.gateway.Models {
		if m.Name == id && allows(m, client) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s.facadeObject(backend.WithStateSnapshot(r.Context()), m, client))
			return
		}
	}
	writeOpenAIError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model '%s' does not exist", id))
}

// reloadResult is the body of a successful POST /amf/reload.
func (s *server) facadeObject(ctx context.Context, m Model, client backend.Client) facadeObject {
	inspection, err := m.Handler.Inspect(ctx, client)
	if err != nil {
		s.log.Warn("current selection unavailable", "model", m.Name, "error", err)
	}
	obj := facadeObject{ID: m.Name, Description: m.Handler.Description()}
	if inspection.Current != nil {
		obj.Current = newCurrentObject(*inspection.Current)
	}
	for _, c := range inspection.Candidates {
		obj.Candidates = append(obj.Candidates, candidateObject{Model: c.Model, Backend: c.Backend, LoadIfNone: c.LoadIfNone, State: c.State, Match: c.Match, AliasOf: c.AliasOf, Winner: c.Winner})
	}
	return obj
}

// completions serves one completion protocol. Chat and legacy completions
// share the body shape, selection, params and response framing; only the
// path the backend is called on differs.
func (s *server) completions(protocol backend.Protocol) clientHandler {
	return func(w http.ResponseWriter, r *http.Request, client backend.Client) {
		s.complete(w, r, client, protocol, func(h virtualmodel.Handler, ctx context.Context, body []byte, client backend.Client) (virtualmodel.Result, error) {
			return h.Chat(ctx, protocol, body, client)
		})
	}
}

func (s *server) complete(w http.ResponseWriter, r *http.Request, client backend.Client, protocol backend.Protocol, call func(virtualmodel.Handler, context.Context, []byte, backend.Client) (virtualmodel.Result, error)) {
	start := time.Now()
	family := familyOf(protocol)
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	rc.SetReadDeadline(time.Time{}) // the response may legitimately stream for longer
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.reject(family, w, r, client, http.StatusRequestEntityTooLarge, "invalid_request_error", fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit))
			return
		}
		s.reject(family, w, r, client, http.StatusBadRequest, "invalid_request_error", "reading request body: "+err.Error())
		return
	}
	if !gjson.ValidBytes(body) {
		s.reject(family, w, r, client, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	parsed := gjson.ParseBytes(body)
	if !parsed.IsObject() {
		s.reject(family, w, r, client, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	if key := duplicateKey(parsed); key != "" {
		s.reject(family, w, r, client, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("request body contains duplicate key '%s'", key))
		return
	}
	name := gjson.GetBytes(body, "model").String()
	hop := instanceID + "/" + name
	route, looped := takeRoute(parsed, hop)
	if looped {
		s.log.Warn("routing loop", append([]any{"model", name, "route", route, "status", http.StatusLoopDetected}, clientAttrs(client)...)...)
		family.writeError(w, http.StatusLoopDetected, "loop_detected", fmt.Sprintf("request for '%s' has already passed through this virtual model", name))
		return
	}
	if parsed.Get(backend.RouteField).Exists() {
		// Only an adapter that forwards to another gateway puts it back.
		if body, err = sjson.DeleteBytes(body, backend.RouteField); err != nil {
			family.writeError(w, http.StatusInternalServerError, "gateway_error", err.Error())
			return
		}
	}
	client.Route = append(route, hop)
	client.MessagesHeader = messagesHeader(r)
	handler := s.models[name]
	if handler == nil {
		s.log.Warn("unknown model", append([]any{"model", name, "status", http.StatusNotFound}, clientAttrs(client)...)...)
		family.writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model '%s' does not exist", name))
		return
	}
	if !handler.Access().Allows(client.Name, client.Groups) {
		attrs := []any{"model", name, "status", http.StatusNotFound}
		attrs = append(attrs, clientAttrs(client)...)
		s.log.Warn("model not allowed for this client", attrs...)
		family.writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model '%s' does not exist", name))
		return
	}

	res, err := call(handler, r.Context(), body, client)
	if res.Estimated {
		w.Header().Set(estimatedHeader, "true")
	}
	if len(res.Dropped) > 0 {
		w.Header().Set(droppedHeader, strings.Join(res.Dropped, ", "))
	}
	if res.Target != "" {
		w.Header().Set(s.gateway.Config.Server.TargetHeader, res.Target)
	}
	status := http.StatusOK
	var streamErr error
	switch {
	case err != nil:
		var verr *pipeline.Error
		if !errors.As(err, &verr) {
			verr = &pipeline.Error{Status: http.StatusInternalServerError, Type: "gateway_error", Message: err.Error()}
		}
		status = verr.Status
		family.writeError(w, verr.Status, verr.Type, verr.Message)
	case res.Chat.Stream != nil:
		streamErr = s.stream(w, r, res.Chat.Stream, protocol)
	default:
		status = res.Chat.Response.Status
		if res.Chat.Response.ContentType != "" {
			w.Header().Set("Content-Type", res.Chat.Response.ContentType)
		}
		if res.Chat.Response.RetryAfter != "" {
			w.Header().Set("Retry-After", res.Chat.Response.RetryAfter)
		}
		w.WriteHeader(status)
		w.Write(res.Chat.Response.Body)
	}

	attrs := []any{"model", name, "checked", res.Checked, "winner", res.Target, "reason", res.Reason, "status", status, "durationMs", time.Since(start).Round(time.Millisecond).Milliseconds(), "path", r.URL.Path}
	if res.Backend != "" {
		attrs = append(attrs, "backend", res.Backend)
	}
	if len(res.Dropped) > 0 {
		attrs = append(attrs, "dropped", res.Dropped)
	}
	attrs = append(attrs, clientAttrs(client)...)
	switch {
	case err != nil:
		s.log.Warn("request failed", append(attrs, "error", err)...)
	case streamErr != nil:
		s.log.Warn("stream failed", append(attrs, "error", streamErr)...)
	case s.gateway.Config.Logging.LogSelection:
		s.log.Info("selection", attrs...)
	}
}

// stream relays a backend stream as SSE in the protocol's framing. Once the
// 200 is sent the status can't change, so a backend failure ends the stream
// with the protocol's error event, and a stream framed with [DONE] gets none.
func (s *server) stream(w http.ResponseWriter, r *http.Request, stream backend.ChatStream, protocol backend.Protocol) error {
	d := protocols.Of(protocol)
	defer stream.Close()
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for sent := 0; ; sent++ {
		data, err := stream.Next()
		if err == io.EOF {
			if !d.Named {
				io.WriteString(w, "data: [DONE]\n\n")
				flush(flusher)
			}
			return nil
		}
		if err != nil {
			if r.Context().Err() != nil {
				return fmt.Errorf("client went away: %w", err)
			}
			writeEvent(w, d, d.ErrorEvent(err, sent))
			flush(flusher)
			return err
		}
		writeEvent(w, d, data)
		flush(flusher)
	}
}

// writeEvent frames one event: with an event line repeating its type, for a
// protocol whose clients dispatch on it, else as a data line alone.
func writeEvent(w io.Writer, d protocols.Descriptor, data []byte) {
	if d.Named {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.GetBytes(data, "type").String(), data)
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func flush(f http.Flusher) {
	if f != nil {
		f.Flush()
	}
}

// estimatedHeader marks a token count as the gateway's estimate, not the
// backend's own, since the two look alike in the body.
const estimatedHeader = "X-Gateway-Estimated"

// droppedHeader names the client's fields a translation left out. A client
// relies on the gateway forwarding what it doesn't patch, so where it can't, it
// says so; a log line alone would be silent to the client.
const droppedHeader = "X-Gateway-Dropped"

// messagesHeader returns the request's headers that reach a backend speaking
// Messages, or nil if it has none.
func messagesHeader(r *http.Request) http.Header {
	var header http.Header
	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta"} {
		if values, ok := r.Header[name]; ok {
			if header == nil {
				header = http.Header{}
			}
			header[name] = values
		}
	}
	return header
}

// instanceID identifies this gateway process in backend.RouteField, where
// each hop is "<instanceID>/<virtual model>". It is per process, not per
// config, so it survives a reload.
var instanceID = func() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}()

// takeRoute returns the hops a request has already made, and whether hop is
// among them. Passing through the same gateway twice is fine (one virtual
// model of it can be a candidate of another, via Open WebUI); reaching the
// same virtual model of the same gateway again is a loop.
func takeRoute(body gjson.Result, hop string) (route []string, looped bool) {
	for _, h := range body.Get(backend.RouteField).Array() {
		route = append(route, h.String())
		if h.String() == hop {
			looped = true
		}
	}
	return route, looped
}

// duplicateKey returns a key that appears more than once in the same object
// anywhere in v, or "". Duplicated keys are rejected because gjson and sjson
// act on the first occurrence while backends typically keep the last, which
// would let a client smuggle values past the gateway's params and the model
// rewrite.
func duplicateKey(v gjson.Result) string {
	if !v.IsObject() && !v.IsArray() {
		return ""
	}
	var dup string
	var seen map[string]bool
	if v.IsObject() {
		seen = map[string]bool{}
	}
	v.ForEach(func(k, child gjson.Result) bool {
		if seen != nil {
			key := k.String()
			if seen[key] {
				dup = key
				return false
			}
			seen[key] = true
		}
		dup = duplicateKey(child)
		return dup == ""
	})
	return dup
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func errorBody(errType, message string) map[string]errorDetail {
	return map[string]errorDetail{"error": {Message: message, Type: errType}}
}

// clientAttrs names the caller in a log line: its key's name, or unknown,
// as the built-in group it is in calls it, for any other caller, with the
// fingerprint of a key server.apiKeys doesn't list, and none for a request
// with no key. Every line about a request then says who sent it.
func clientAttrs(client backend.Client) []any {
	attrs := []any{"client", cmp.Or(client.Name, config.UnknownClient)}
	if client.Fingerprint != "" {
		attrs = append(attrs, "fingerprint", client.Fingerprint)
	}
	return attrs
}

// reject answers a request refused before selection and logs it, since no
// selection line follows. The client's key is never logged, only its name or
// fingerprint.
func (s *server) reject(family *apiFamily, w http.ResponseWriter, r *http.Request, client backend.Client, status int, errType, message string) {
	attrs := []any{"path", r.URL.Path, "status", status}
	attrs = append(attrs, clientAttrs(client)...)
	s.log.Warn("request rejected", append(attrs, "error", message)...)
	family.writeError(w, status, errType, message)
}
