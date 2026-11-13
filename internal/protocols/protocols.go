// Package protocols describes each protocol the gateway serves and forwards:
// where its endpoint is, how its stream is framed and ended, and what a
// broken stream's error event looks like. Code
// that needs one of these reads the protocol's Descriptor rather than
// switching on the protocol, so adding a protocol is adding a descriptor.
package protocols

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// Descriptor is what the gateway knows about one protocol.
type Descriptor struct {
	Protocol backend.Protocol
	// Path is where the protocol is served, on the gateway, and on a backend
	// that forwards it.
	Path string
	// Named means each stream event's type is on an event line ahead of its
	// data, and the stream sends no [DONE], ending instead with an event of
	// one of the LastEvents types, as Messages does. Otherwise
	// events are data lines and [DONE] ends the stream.
	Named      bool
	LastEvents []string
	// ErrorEvent is a broken stream as the protocol's error event: a
	// backend's own error event where it already is one, else the failure in
	// the protocol's shape. A protocol the gateway only sends has none.
	ErrorEvent func(err error) []byte
}

// All are the protocols the gateway serves, in the order a client sees them
// listed.
var All = []Descriptor{
	{Protocol: backend.OpenAIChat, Path: "/v1/chat/completions", ErrorEvent: openAIErrorEvent},
	{Protocol: backend.OpenAICompletions, Path: "/v1/completions", ErrorEvent: openAIErrorEvent},
	{Protocol: backend.AnthropicMessages, Path: "/v1/messages", Named: true, LastEvents: []string{"message_stop"}},
}

// Of returns protocol p's descriptor. Every protocol a config can name has
// one, since config loading checks names against All.
func Of(p backend.Protocol) Descriptor {
	for _, d := range All {
		if d.Protocol == p {
			return d
		}
	}
	panic("protocol '" + string(p) + "' has no descriptor")
}

// Stream reads a backend's event stream in protocol p, ending it where p
// ends one: at [DONE], or at one of p's last events. A body that ends before
// is a broken stream (io.ErrUnexpectedEOF), as SSEStream reports.
func Stream(p backend.Protocol, body io.ReadCloser) backend.ChatStream {
	events := backend.NewSSEStream(body)
	if d := Of(p); d.Named {
		return backend.NewEndingStream(events, d.LastEvents...)
	}
	return events
}

// backendEvent returns a backend's error event if err is one and its payload
// can be sent on as an event as it is: valid JSON on one line, passing check.
func backendEvent(err error, check func(gjson.Result) bool) ([]byte, bool) {
	var backendErr *backend.StreamError
	if !errors.As(err, &backendErr) {
		return nil, false
	}
	data := backendErr.Data
	if !gjson.ValidBytes(data) || bytes.ContainsAny(data, "\r\n") || !check(gjson.ParseBytes(data)) {
		return nil, false
	}
	return data, true
}

func errorObject(errType, message string) map[string]any {
	return map[string]any{"message": message, "type": errType}
}

// openAIErrorEvent is a broken Chat Completions or legacy Completions stream
// as one data payload: a backend's OpenAI-style error object as it is, else
// the failure wrapped in one.
func openAIErrorEvent(err error) []byte {
	if data, ok := backendEvent(err, func(e gjson.Result) bool { return e.Get("error").Exists() }); ok {
		return data
	}
	var backendErr *backend.StreamError
	if errors.As(err, &backendErr) {
		event, _ := json.Marshal(map[string]any{"error": errorObject("backend_error", string(backendErr.Data))})
		return event
	}
	event, _ := json.Marshal(map[string]any{"error": errorObject("gateway_error", "backend stream failed: "+err.Error())})
	return event
}
