package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/protocols"
)

// apiFamily is what differs between the APIs the gateway presents: where a
// client's key arrives, and what the gateway's own errors look like. A
// backend's error on a route with no translation passes through unchanged.
// A family's protocols are those whose descriptor names it.
type apiFamily struct {
	name       string
	credential func(r *http.Request) string // the client's key as an Authorization value, or ""
	writeError func(w http.ResponseWriter, status int, errType, message string)
}

var openAIFamily = &apiFamily{
	name:       config.FamilyOpenAI,
	credential: func(r *http.Request) string { return r.Header.Get("Authorization") },
	writeError: writeOpenAIError,
}

// anthropicFamily takes the key from x-api-key, as Claude's clients send it,
// or from Authorization, as they send a token.
var anthropicFamily = &apiFamily{
	name:       config.FamilyAnthropic,
	credential: eitherKeyHeader,
	writeError: writeAnthropicError,
}

// amfFamily is the gateway's own endpoints. Their schema is the gateway's,
// borrowing OpenAI's error shape, and clients of either API call them, so the
// key may come in either API's header.
var amfFamily = &apiFamily{
	name:       "amf",
	credential: eitherKeyHeader,
	writeError: writeOpenAIError,
}

func eitherKeyHeader(r *http.Request) string {
	if key := strings.TrimSpace(r.Header.Get("x-api-key")); key != "" {
		return "Bearer " + key
	}
	return r.Header.Get("Authorization")
}

// protocols returns the descriptors of the family's protocols.
func (f *apiFamily) protocols() []protocols.Descriptor {
	var own []protocols.Descriptor
	for _, d := range protocols.All {
		if d.Family == f.name {
			own = append(own, d)
		}
	}
	return own
}

// servedProtocols returns the protocols the gateway serves: each family's
// that api.<family>.protocols names, or all of them when it is left out.
func servedProtocols(api config.API) (map[backend.Protocol]bool, error) {
	served := map[backend.Protocol]bool{}
	for _, c := range []struct {
		family *apiFamily
		names  []string
	}{{openAIFamily, api.OpenAI.Protocols}, {anthropicFamily, api.Anthropic.Protocols}} {
		own := c.family.protocols()
		if c.names == nil {
			for _, d := range own {
				served[d.Protocol] = true
			}
			continue
		}
		for _, name := range c.names {
			if !slices.ContainsFunc(own, func(d protocols.Descriptor) bool { return string(d.Protocol) == name }) {
				var names []string
				for _, d := range own {
					names = append(names, string(d.Protocol))
				}
				return nil, fmt.Errorf("api.%s.protocols: '%s' is not one of the family's protocols; use %s", c.family.name, name, strings.Join(names, ", "))
			}
			served[backend.Protocol(name)] = true
		}
	}
	return served, nil
}

// served returns the descriptors of the family's protocols that the gateway
// serves.
func (s *server) served(f *apiFamily) []protocols.Descriptor {
	var on []protocols.Descriptor
	for _, d := range f.protocols() {
		if s.gateway.served[d.Protocol] {
			on = append(on, d)
		}
	}
	return on
}

// lists reports whether facade m takes requests in any of the family's
// protocols the gateway serves, so a facade narrowed away from them isn't
// listed there.
func (s *server) lists(f *apiFamily, m Model) bool {
	return slices.ContainsFunc(s.served(f), func(d protocols.Descriptor) bool { return m.Handler.Serves(d.Protocol) })
}

// familyOf returns the family a protocol belongs to.
func familyOf(protocol backend.Protocol) *apiFamily {
	if protocols.Of(protocol).Family == config.FamilyAnthropic {
		return anthropicFamily
	}
	return openAIFamily
}

func writeOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorBody(errType, message))
}

// anthropicErrorTypes maps the gateway's error types to the ones Claude's
// clients know, which they use to decide what to do with an error. Anything
// else is the gateway's own failure, an api_error.
var anthropicErrorTypes = map[string]string{
	"invalid_request_error": "invalid_request_error",
	"invalid_api_key":       "authentication_error",
	"model_not_found":       "not_found_error",
	"not_found":             "not_found_error",
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	mapped, ok := anthropicErrorTypes[errType]
	if !ok {
		mapped = "api_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(struct {
		Type  string      `json:"type"`
		Error errorDetail `json:"error"`
	}{Type: "error", Error: errorDetail{Message: message, Type: mapped}})
}

// routeOpenAI serves the OpenAI family under base, which is "" at the root:
// the protocols the gateway serves, and the model list when there are any.
func (s *server) routeOpenAI(mux *http.ServeMux, base string) {
	if len(s.served(openAIFamily)) == 0 {
		return
	}
	mux.HandleFunc("GET "+base+"/v1/models", s.authenticated(openAIFamily, s.listModels))
	mux.HandleFunc("GET "+base+"/v1/models/{id...}", s.authenticated(openAIFamily, s.getModel))
	for _, d := range s.served(openAIFamily) {
		mux.HandleFunc("POST "+base+d.Path, s.authenticated(openAIFamily, s.completions(d.Protocol)))
	}
}

// routeAnthropic serves the Anthropic family under base, which is "" at the
// root, unless the gateway serves none of its protocols.
func (s *server) routeAnthropic(mux *http.ServeMux, base string) {
	if len(s.served(anthropicFamily)) == 0 {
		return
	}
	mux.HandleFunc("GET "+base+"/v1/models", s.authenticated(anthropicFamily, s.listAnthropicModels))
	mux.HandleFunc("GET "+base+"/v1/models/{id...}", s.authenticated(anthropicFamily, s.getAnthropicModel))
	messages := protocols.Of(backend.AnthropicMessages).Path
	mux.HandleFunc("POST "+base+messages, s.authenticated(anthropicFamily, s.completions(backend.AnthropicMessages)))
	routeAnthropicMisses(mux, base)
}

// routeAnthropicMisses answers a wrong path or method in Anthropic's error
// shape and without a key, as its API does under /v1. The patterns name no
// method, so they catch what the routes above don't. At the root only /v1 is
// the family's, since the root also serves /health and /amf.
func routeAnthropicMisses(mux *http.ServeMux, base string) {
	for _, route := range []struct{ path, allow string }{
		{base + "/v1/models", "GET, HEAD"},
		{base + "/v1/models/{id...}", "GET, HEAD"},
		{base + protocols.Of(backend.AnthropicMessages).Path, http.MethodPost},
	} {
		mux.HandleFunc(route.path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", route.allow)
			writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", fmt.Sprintf("method '%s' is not allowed on '%s'; use %s", r.Method, r.URL.Path, route.allow))
		})
	}
	notFound := func(w http.ResponseWriter, r *http.Request) {
		writeAnthropicError(w, http.StatusNotFound, "not_found", fmt.Sprintf("path '%s' does not exist", r.URL.Path))
	}
	prefix := base
	if prefix == "" {
		prefix = "/v1"
	}
	mux.HandleFunc(prefix, notFound)
	mux.HandleFunc(prefix+"/", notFound)
}

// anthropicModel is a facade in the shape of Anthropic's model objects. Its
// max_tokens is the facade's OutputCap, which every candidate accepts, and is
// left out when no candidate's backend lists one.
type anthropicModel struct {
	Type        string         `json:"type"`
	ID          string         `json:"id"`
	DisplayName string         `json:"display_name"`
	CreatedAt   string         `json:"created_at"`
	MaxTokens   int64          `json:"max_tokens,omitempty"`
	Current     *currentObject `json:"current,omitempty"`
}

func (s *server) anthropicModel(ctx context.Context, m Model) anthropicModel {
	obj := anthropicModel{Type: "model", ID: m.Name, DisplayName: m.Name, CreatedAt: time.Unix(s.started, 0).UTC().Format(time.RFC3339)}
	if limit, ok := m.Handler.OutputCap(); ok {
		obj.MaxTokens = limit
	}
	if s.gateway.Config.API.Anthropic.AddCurrent {
		obj.Current = s.currentOf(ctx, m)
	}
	return obj
}

// listAnthropicModels returns every facade on one page, so a client's paging
// parameters have nothing to do.
func (s *server) listAnthropicModels(w http.ResponseWriter, r *http.Request, client backend.Client) {
	list := struct {
		Data    []anthropicModel `json:"data"`
		HasMore bool             `json:"has_more"`
		FirstID *string          `json:"first_id"`
		LastID  *string          `json:"last_id"`
	}{Data: []anthropicModel{}}
	ctx := listingContext(backend.WithStateSnapshot(r.Context()))
	for _, m := range s.gateway.Models {
		if s.lists(anthropicFamily, m) {
			list.Data = append(list.Data, s.anthropicModel(ctx, m))
		}
	}
	if len(list.Data) > 0 {
		list.FirstID, list.LastID = &list.Data[0].ID, &list.Data[len(list.Data)-1].ID
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (s *server) getAnthropicModel(w http.ResponseWriter, r *http.Request, client backend.Client) {
	id := r.PathValue("id")
	for _, m := range s.gateway.Models {
		if m.Name == id && s.lists(anthropicFamily, m) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s.anthropicModel(listingContext(r.Context()), m))
			return
		}
	}
	writeAnthropicError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model '%s' does not exist", id))
}
