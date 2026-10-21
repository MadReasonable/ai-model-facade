package gateway

import (
	"bytes"
	"context"
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

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/config"
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
	mux.HandleFunc("POST /v1/chat/completions", s.authenticated(s.completions(backend.ChatCompletions)))
	mux.HandleFunc("POST /v1/completions", s.authenticated(s.completions(backend.Completions)))
	return mux
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
		if len(cfg.APIKeys) > 0 && cfg.UnknownKeys != "allow" {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid or missing API key")
			return
		}
		if hasToken {
			sum := sha256.Sum256([]byte(token))
			client.Name = config.UnknownClient
			client.Fingerprint = hex.EncodeToString(sum[:4])
		}
		next(w, r, client)
	}
}

// modelObject is a virtual model in OpenAI's model-object shape, plus
// `current`, an addition clients that don't know it ignore.
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
	AliasOf string `json:"alias_of,omitempty"`
	Reason  string `json:"reason"`
}

// modelObject describes m. If the backend's state can't be read, `current`
// is left out rather than failing the listing.
func (s *server) modelObject(ctx context.Context, m Model) modelObject {
	obj := modelObject{ID: m.Name, Object: "model", Created: s.started, OwnedBy: "ai-model-facade", Description: m.Handler.Description()}
	if cur, err := m.Handler.Current(ctx); err == nil {
		obj.Current = &currentObject{Model: cur.Model, AliasOf: cur.AliasOf, Reason: cur.Reason}
	} else {
		s.log.Warn("current selection unavailable", "model", m.Name, "error", err)
	}
	return obj
}

func (s *server) listModels(w http.ResponseWriter, r *http.Request, client backend.Client) {
	list := struct {
		Object string        `json:"object"`
		Data   []modelObject `json:"data"`
	}{Object: "list", Data: []modelObject{}}
	for _, m := range s.gateway.Models {
		list.Data = append(list.Data, s.modelObject(r.Context(), m))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (s *server) getModel(w http.ResponseWriter, r *http.Request, client backend.Client) {
	id := r.PathValue("id")
	for _, m := range s.gateway.Models {
		if m.Name == id {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s.modelObject(r.Context(), m))
			return
		}
	}
	writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q does not exist", id))
}

// completions serves one completion endpoint. Chat and legacy completions
// share the body shape, selection, params and response framing; only the
// endpoint the backend is called on differs.
func (s *server) completions(endpoint backend.Endpoint) clientHandler {
	return func(w http.ResponseWriter, r *http.Request, client backend.Client) {
		s.complete(w, r, client, endpoint)
	}
}

func (s *server) complete(w http.ResponseWriter, r *http.Request, client backend.Client, endpoint backend.Endpoint) {
	start := time.Now()
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	rc.SetReadDeadline(time.Time{}) // the response may legitimately stream for longer
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "reading request body: "+err.Error())
		return
	}
	if !gjson.ValidBytes(body) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	parsed := gjson.ParseBytes(body)
	if !parsed.IsObject() {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	if key := duplicateKey(parsed); key != "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("request body contains duplicate key %q", key))
		return
	}
	name := gjson.GetBytes(body, "model").String()
	handler := s.models[name]
	if handler == nil {
		s.log.Warn("unknown model", "model", name, "status", http.StatusNotFound)
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q does not exist", name))
		return
	}

	res, err := handler.Chat(r.Context(), endpoint, body, client)
	if res.Target != "" {
		w.Header().Set(s.gateway.Config.Server.TargetHeader, res.Target)
	}
	status := http.StatusOK
	var streamErr error
	switch {
	case err != nil:
		var verr *virtualmodel.Error
		if !errors.As(err, &verr) {
			verr = &virtualmodel.Error{Status: http.StatusInternalServerError, Type: "gateway_error", Message: err.Error()}
		}
		status = verr.Status
		writeError(w, verr.Status, verr.Type, verr.Message)
	case res.Chat.Stream != nil:
		streamErr = s.stream(w, r, res.Chat.Stream)
	default:
		status = res.Chat.Response.Status
		if res.Chat.Response.ContentType != "" {
			w.Header().Set("Content-Type", res.Chat.Response.ContentType)
		}
		w.WriteHeader(status)
		w.Write(res.Chat.Response.Body)
	}

	attrs := []any{"model", name, "checked", res.Checked, "winner", res.Target, "reason", res.Reason, "status", status, "duration", time.Since(start).Round(time.Millisecond)}
	if endpoint != backend.ChatCompletions {
		attrs = append(attrs, "endpoint", endpoint)
	}
	if res.AliasOf != "" {
		attrs = append(attrs, "aliasOf", res.AliasOf)
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

// stream relays a backend stream as SSE. Once the 200 is sent the status
// can't change, so a backend failure ends the stream with an error event and
// no [DONE].
func (s *server) stream(w http.ResponseWriter, r *http.Request, stream backend.ChatStream) error {
	defer stream.Close()
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for {
		data, err := stream.Next()
		if err == io.EOF {
			io.WriteString(w, "data: [DONE]\n\n")
			flush(flusher)
			return nil
		}
		if err != nil {
			if r.Context().Err() != nil {
				return fmt.Errorf("client went away: %w", err)
			}
			fmt.Fprintf(w, "data: %s\n\n", streamErrorEvent(err))
			flush(flusher)
			return err
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		flush(flusher)
	}
}

func flush(f http.Flusher) {
	if f != nil {
		f.Flush()
	}
}

// streamErrorEvent renders a mid-stream failure as one SSE data payload. A
// backend `event: error` whose payload is already an OpenAI-style error object
// is relayed as-is; anything else is wrapped.
func streamErrorEvent(err error) []byte {
	var backendErr *backend.StreamError
	if !errors.As(err, &backendErr) {
		event, _ := json.Marshal(errorBody("gateway_error", "backend stream failed: "+err.Error()))
		return event
	}
	data := backendErr.Data
	if gjson.ValidBytes(data) && gjson.GetBytes(data, "error").Exists() && !bytes.ContainsAny(data, "\r\n") {
		return data
	}
	event, _ := json.Marshal(errorBody("backend_error", string(data)))
	return event
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

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorBody(errType, message))
}
