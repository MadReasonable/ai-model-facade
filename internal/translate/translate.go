// Package translate converts a request and its reply between two protocols,
// for a request whose protocol its backend doesn't accept. A route is found
// by Route: the identity route when the backend accepts the protocol, with
// nothing parsed or changed, otherwise a direct translator for the pair.
//
// A translator runs before params are applied, since params are written in
// the protocol the adapter receives, so it never sees them.
package translate

import (
	"slices"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// Translator converts requests from one protocol to another.
type Translator interface {
	// Request converts a body and returns the Reply that converts what comes
	// back, since a reply can depend on what the request asked for. A *Refusal
	// means the request can't be served as the client means it.
	Request(body []byte) ([]byte, Reply, error)
}

// Reply converts a response, an error or a stream back to the client's protocol.
type Reply interface {
	Result(result backend.ChatResult) backend.ChatResult
	// Dropped names the client's fields the translation left out, in the order
	// they were found, so the client can be told. A field converted to a
	// counterpart isn't one of them.
	Dropped() []string
}

// droppedFields collects field names without repeats.
type droppedFields []string

func (d *droppedFields) add(name string) {
	if !slices.Contains(*d, name) {
		*d = append(*d, name)
	}
}

// Refusal is a request a translator won't send, with the response in the
// client's protocol that says why.
type Refusal struct {
	Response *backend.ChatResponse
	message  string
}

func (r *Refusal) Error() string { return r.message }

type pair struct{ from, to backend.Protocol }

// direct holds the translators written for one pair of protocols.
var direct = map[pair]Translator{
	{backend.OpenAIChat, backend.AnthropicMessages}:      chatToMessages{},
	{backend.AnthropicMessages, backend.OpenAIChat}:      messagesToChat{},
	{backend.OpenAIResponses, backend.OpenAIChat}:        responsesToChat{},
	{backend.OpenAIResponses, backend.AnthropicMessages}: responsesToMessages{},
	{backend.OpenAIChat, backend.OpenAIResponses}:        chatToResponses{},
	{backend.AnthropicMessages, backend.OpenAIResponses}: messagesToResponses{},
}

// Route picks the protocol to send a request in to a backend that accepts
// these, and the translator to reach it, nil on the identity route. ok is
// false if the backend can't be reached from the protocol.
func Route(from backend.Protocol, accepted []backend.Protocol) (to backend.Protocol, translator Translator, ok bool) {
	if slices.Contains(accepted, from) {
		return from, nil, true
	}
	for _, to := range accepted {
		if translator, ok := direct[pair{from, to}]; ok {
			return to, translator, true
		}
	}
	return "", nil, false
}
