package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
}

// Handler returns the OpenAI API front end: HTTP and SSE framing only.
func (g *Gateway) Handler(log *slog.Logger) http.Handler {
	s := &server{gateway: g, models: map[string]virtualmodel.Handler{}, log: log, started: time.Now().Unix()}
	for _, m := range g.Models {
		s.models[m.Name] = m.Handler
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /v1/models", s.authenticated(s.listModels))
	mux.HandleFunc("GET /v1/models/{id...}", s.authenticated(s.getModel))
	mux.HandleFunc("GET /amf/models", s.authenticated(s.listFacades))
	mux.HandleFunc("GET /amf/models/{id...}", s.authenticated(s.getFacade))
	for _, p := range []backend.Protocol{backend.OpenAIChat, backend.OpenAICompletions} {
		mux.HandleFunc("POST "+protocols.Of(p).Path, s.authenticated(s.completions(p)))
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
func (s *server) authenticated(next clientHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.gateway.Config.Server
		header := r.Header.Get("Authorization")
		client := backend.Client{Authorization: header}
		token, hasToken := strings.CutPrefix(header, "Bearer ")
		if hasToken {
			for _, k := range cfg.APIKeys {
				if subtle.ConstantTimeCompare([]byte(token), []byte(k.Key)) == 1 {
					client.Name = k.Name
					next(w, r, client)
					return
				}
			}
		}
		if hasToken {
			sum := sha256.Sum256([]byte(token))
			client.Fingerprint = hex.EncodeToString(sum[:4])
		}
		if len(cfg.APIKeys) > 0 && cfg.UnknownKeys != "allow" {
			s.reject(w, r, client, http.StatusUnauthorized, "invalid_api_key", "invalid or missing API key")
			return
		}
		if hasToken {
			client.Name = config.UnknownClient
		}
		next(w, r, client)
	}
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
// rather than failing the listing.
func (s *server) currentOf(ctx context.Context, m Model) *currentObject {
	cur, err := m.Handler.Current(ctx)
	if err != nil {
		s.log.Warn("current selection unavailable", "model", m.Name, "error", err)
		return nil
	}
	return newCurrentObject(cur)
}

// modelObject describes m for an OpenAI client. With api.openAi.addCurrent off,
// no backend's state is read.
func (s *server) modelObject(ctx context.Context, m Model) modelObject {
	obj := modelObject{ID: m.Name, Object: "model", Created: s.started, OwnedBy: "ai-model-facade", Description: m.Handler.Description()}
	if s.gateway.Config.API.OpenAI.AddCurrent {
		obj.Current = s.currentOf(ctx, m)
	}
	return obj
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
		list.Data = append(list.Data, s.modelObject(ctx, m))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (s *server) getModel(w http.ResponseWriter, r *http.Request, client backend.Client) {
	id := r.PathValue("id")
	for _, m := range s.gateway.Models {
		if m.Name == id {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s.modelObject(listingContext(r.Context()), m))
			return
		}
	}
	writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model '%s' does not exist", id))
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
// this, so the refresh can't be waiting on this request.
func (s *server) listFacades(w http.ResponseWriter, r *http.Request, client backend.Client) {
	list := struct {
		Models []facadeObject `json:"models"`
	}{Models: []facadeObject{}}
	ctx := backend.WithStateSnapshot(r.Context())
	for _, m := range s.gateway.Models {
		list.Models = append(list.Models, s.facadeObject(ctx, m))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// getFacade serves /amf/models/{id}: one entry of the list.
func (s *server) getFacade(w http.ResponseWriter, r *http.Request, client backend.Client) {
	id := r.PathValue("id")
	for _, m := range s.gateway.Models {
		if m.Name == id {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s.facadeObject(backend.WithStateSnapshot(r.Context()), m))
			return
		}
	}
	writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model '%s' does not exist", id))
}

func (s *server) facadeObject(ctx context.Context, m Model) facadeObject {
	inspection, err := m.Handler.Inspect(ctx)
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
		s.complete(w, r, client, protocol)
	}
}

func (s *server) complete(w http.ResponseWriter, r *http.Request, client backend.Client, protocol backend.Protocol) {
	start := time.Now()
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	rc.SetReadDeadline(time.Time{}) // the response may legitimately stream for longer
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.reject(w, r, client, http.StatusRequestEntityTooLarge, "invalid_request_error", fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit))
			return
		}
		s.reject(w, r, client, http.StatusBadRequest, "invalid_request_error", "reading request body: "+err.Error())
		return
	}
	if !gjson.ValidBytes(body) {
		s.reject(w, r, client, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	parsed := gjson.ParseBytes(body)
	if !parsed.IsObject() {
		s.reject(w, r, client, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	if key := duplicateKey(parsed); key != "" {
		s.reject(w, r, client, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("request body contains duplicate key '%s'", key))
		return
	}
	name := gjson.GetBytes(body, "model").String()
	hop := instanceID + "/" + name
	route, looped := takeRoute(parsed, hop)
	if looped {
		s.log.Warn("routing loop", "model", name, "route", route, "status", http.StatusLoopDetected)
		writeError(w, http.StatusLoopDetected, "loop_detected", fmt.Sprintf("request for '%s' has already passed through this virtual model", name))
		return
	}
	if parsed.Get(backend.RouteField).Exists() {
		// Only an adapter that forwards to another gateway puts it back.
		if body, err = sjson.DeleteBytes(body, backend.RouteField); err != nil {
			writeError(w, http.StatusInternalServerError, "gateway_error", err.Error())
			return
		}
	}
	client.Route = append(route, hop)
	handler := s.models[name]
	if handler == nil {
		s.log.Warn("unknown model", "model", name, "status", http.StatusNotFound)
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model '%s' does not exist", name))
		return
	}

	res, err := handler.Chat(r.Context(), protocol, body, client)
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
		writeError(w, verr.Status, verr.Type, verr.Message)
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

	attrs := []any{"model", name, "checked", res.Checked, "winner", res.Target, "reason", res.Reason, "status", status, "duration", time.Since(start).Round(time.Millisecond)}
	if protocol != backend.OpenAIChat {
		attrs = append(attrs, "endpoint", protocols.Of(protocol).Path)
	}
	if res.Backend != "" {
		attrs = append(attrs, "backend", res.Backend)
	}
	if client.Name != "" {
		attrs = append(attrs, "client", client.Name)
	}
	if client.Fingerprint != "" {
		attrs = append(attrs, "fingerprint", client.Fingerprint)
	}
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
	for {
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
			writeEvent(w, d, d.ErrorEvent(err))
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

// reject answers a request refused before selection and logs it, since no
// selection line follows. The client's key is never logged, only its name or
// fingerprint.
func (s *server) reject(w http.ResponseWriter, r *http.Request, client backend.Client, status int, errType, message string) {
	attrs := []any{"path", r.URL.Path, "status", status}
	if client.Name != "" {
		attrs = append(attrs, "client", client.Name)
	}
	if client.Fingerprint != "" {
		attrs = append(attrs, "fingerprint", client.Fingerprint)
	}
	s.log.Warn("request rejected", append(attrs, "error", message)...)
	writeError(w, status, errType, message)
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorBody(errType, message))
}
