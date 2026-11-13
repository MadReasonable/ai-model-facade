// Package pipeline runs a request after selection, the same way whatever a
// facade type is: a facade type decides which candidate answers, and the
// pipeline does the rest. In order:
//
//  1. Route: the protocol the backend is sent, the client's own if it accepts
//     it, else one a translator reaches.
//  2. The request's protocol's hook, if it has one: the Responses
//     conversation store expands a conversation the request continues.
//  3. Translation, on a translated route.
//  4. Params in the protocol the backend receives, then the model rewritten
//     to the candidate's ID.
//  5. The backend call, and on a translated route the turn sent back after
//     each pause in Claude's reply, which only a Messages client knows to do.
//  6. The reply translated back, then shown to the hook, which records the
//     turn.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/mergepatch"
	"github.com/MadReasonable/ai-model-facade/internal/translate"
)

// Error is a request failure with the HTTP status and OpenAI error type to
// report; the front end gives it the client's family's shape.
type Error struct {
	Status  int
	Type    string
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

func gatewayError(err error) *Error {
	return &Error{Status: http.StatusInternalServerError, Type: "gateway_error", Message: err.Error()}
}

// Selected is a facade type's answer: the candidate a request goes to, and
// how its request is shaped.
type Selected struct {
	Backend     backend.Backend
	BackendName string
	// Model is the ID the backend is sent, an alias of AliasOf when that is
	// set.
	Model   string
	AliasOf string
	// Accepted are the protocols the backend may be sent for this candidate:
	// every one it accepts, or the one useProtocol names.
	Accepted []backend.Protocol
	// Params returns the patches for a body in protocol, in the order they
	// apply.
	Params func(protocol backend.Protocol) []mergepatch.Patch
	// Service is the backend when the candidate leaves its Responses
	// conversations there (responsesHistory: service), else nil.
	Service backend.ResponsesKeeper
}

// Hook is a protocol's own work around a request. Only Responses has one:
// the conversation store.
type Hook interface {
	// Prepare returns the body the winner is sent and the client's fields it
	// left out, and the Turn that sees the reply.
	Prepare(ctx context.Context, body []byte, winner Winner) ([]byte, Turn, []string, error)
}

// Turn sees a request's reply, in the client's protocol, and returns it as
// the client gets it.
type Turn interface {
	Record(result backend.ChatResult) backend.ChatResult
}

// Winner is what a hook needs to know about the selected candidate.
type Winner struct {
	Backend string // its backend's name
	// Native means the request reaches the backend in its own protocol,
	// without translation.
	Native bool
	// Service is the winner's backend when the candidate keeps its
	// conversations there, else nil.
	Service backend.ResponsesKeeper
	// Backends finds the backend that served an earlier turn.
	Backends map[string]backend.Backend
	Client   backend.Client
}

// Pipeline holds what every request's run after selection shares.
type Pipeline struct {
	Hooks    map[backend.Protocol]Hook
	Backends map[string]backend.Backend
}

// Reply is a request's result as the client gets it, and the client's fields
// the run left out.
type Reply struct {
	Chat    backend.ChatResult
	Dropped []string
}

// Serve runs a request in protocol on the selected candidate.
func (p *Pipeline) Serve(ctx context.Context, protocol backend.Protocol, body []byte, client backend.Client, sel Selected) (Reply, error) {
	to, translator, ok := translate.Route(protocol, sel.Accepted)
	if !ok {
		return Reply{}, &Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("backend '%s' can't be reached from %s", sel.BackendName, protocol)}
	}
	var reply Reply
	var turn Turn
	if hook := p.Hooks[protocol]; hook != nil {
		winner := Winner{Backend: sel.BackendName, Native: to == protocol, Service: sel.Service, Backends: p.Backends, Client: client}
		var dropped []string
		var err error
		if body, turn, dropped, err = hook.Prepare(ctx, body, winner); err != nil {
			var failed *Error
			if errors.As(err, &failed) {
				return Reply{}, failed
			}
			return Reply{}, gatewayError(err)
		}
		reply.drop(dropped)
	}
	var back translate.Reply
	if translator != nil {
		var err error
		if body, back, err = translator.Request(body); err != nil {
			var refused *translate.Refusal
			if errors.As(err, &refused) {
				reply.Chat = backend.ChatResult{Response: refused.Response}
				return reply, nil
			}
			return reply, gatewayError(err)
		}
		reply.drop(back.Dropped())
	}
	body, err := Prepare(body, sel, to)
	if err != nil {
		return reply, gatewayError(err)
	}
	req := backend.ChatRequest{Protocol: to, Body: body, Stream: gjson.GetBytes(body, "stream").Bool(), Client: client, AliasOf: sel.AliasOf}
	if reply.Chat, err = sel.Backend.Chat(ctx, req); err != nil {
		return reply, &Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("backend '%s': %v", sel.BackendName, err)}
	}
	if back != nil {
		send := func(body []byte) (backend.ChatResult, error) {
			req.Body = body
			return sel.Backend.Chat(ctx, req)
		}
		if reply.Chat, err = translate.Resend(to, body, reply.Chat, send); err != nil {
			return reply, &Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("backend '%s': %v", sel.BackendName, err)}
		}
		reply.Chat = back.Result(reply.Chat)
	}
	if turn != nil {
		reply.Chat = turn.Record(reply.Chat)
	}
	return reply, nil
}

func (r *Reply) drop(names []string) {
	for _, name := range names {
		if !slices.Contains(r.Dropped, name) {
			r.Dropped = append(r.Dropped, name)
		}
	}
}

// Prepare applies the selected candidate's params for a body in protocol,
// then rewrites model to its ID, the last step before the backend sees it.
func Prepare(body []byte, sel Selected, protocol backend.Protocol) ([]byte, error) {
	var err error
	for _, patch := range sel.Params(protocol) {
		if body, err = patch.Apply(body); err != nil {
			return nil, err
		}
	}
	return sjson.SetBytes(body, "model", sel.Model)
}
