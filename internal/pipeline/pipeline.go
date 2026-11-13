// Package pipeline runs a request after selection, the same way whatever a
// facade type is: a facade type decides which candidate answers, and the
// pipeline does the rest. In order:
//
//  1. Route: the protocol the backend is sent, the client's own if it accepts
//     it, else one a translator reaches.
//  2. Translation, on a translated route.
//  3. Params in the protocol the backend receives, then the model rewritten
//     to the candidate's ID.
//  4. The backend call.
//  5. The reply translated back.
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
	// Accepted are the protocols the backend accepts.
	Accepted []backend.Protocol
	// Params returns the patches for a body in protocol, in the order they
	// apply.
	Params func(protocol backend.Protocol) []mergepatch.Patch
}

// Reply is a request's result as the client gets it, and the client's fields
// the run left out.
type Reply struct {
	Chat    backend.ChatResult
	Dropped []string
}

// Serve runs a request in protocol on the selected candidate.
func Serve(ctx context.Context, protocol backend.Protocol, body []byte, client backend.Client, sel Selected) (Reply, error) {
	to, translator, ok := translate.Route(protocol, sel.Accepted)
	if !ok {
		return Reply{}, &Error{Status: http.StatusBadGateway, Type: "backend_error", Message: fmt.Sprintf("backend '%s' can't be reached from %s", sel.BackendName, protocol)}
	}
	var reply Reply
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
		reply.Chat = back.Result(reply.Chat)
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
