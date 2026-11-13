package protocols

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

func readAll(t *testing.T, s backend.ChatStream) ([]string, error) {
	t.Helper()
	var events []string
	for {
		data, err := s.Next()
		if err != nil {
			return events, err
		}
		events = append(events, string(data))
	}
}

// A stream framed with [DONE] ends there; one with named events ends at its
// protocol's last event, whatever follows; either is broken if the body ends
// first.
func TestStream_EndsWhereItsProtocolEndsOne(t *testing.T) {
	cases := []struct {
		protocol backend.Protocol
		body     string
		want     []string
	}{
		{backend.OpenAIChat, "data: {\"a\":1}\n\ndata: [DONE]\n\n", []string{`{"a":1}`}},
		{backend.AnthropicMessages, "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\nevent: ping\ndata: {\"type\":\"ping\"}\n\n", []string{`{"type":"message_start"}`, `{"type":"message_stop"}`}},
		{backend.OpenAIResponses, "event: response.incomplete\ndata: {\"type\":\"response.incomplete\"}\n\n", []string{`{"type":"response.incomplete"}`}},
	}
	for _, c := range cases {
		events, err := readAll(t, Stream(c.protocol, io.NopCloser(strings.NewReader(c.body))))
		if err != io.EOF || strings.Join(events, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %v, %v", c.protocol, events, err)
		}
	}
	for _, p := range []backend.Protocol{backend.OpenAIChat, backend.AnthropicMessages, backend.OpenAIResponses} {
		if _, err := readAll(t, Stream(p, io.NopCloser(strings.NewReader("data: {\"type\":\"x\"}\n\n")))); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s: an early end gave %v", p, err)
		}
	}
}

// A backend's own error event passes on unchanged where it is already in the
// protocol's shape; anything else becomes the protocol's error event, a
// Responses one numbered after the events sent.
func TestErrorEvent_IsInTheProtocolsShape(t *testing.T) {
	failed := errors.New("connection reset")
	cases := []struct {
		protocol backend.Protocol
		err      error
		check    func(gjson.Result) bool
	}{
		{backend.OpenAIChat, &backend.StreamError{Data: []byte(`{"error":{"message":"boom"}}`)}, func(e gjson.Result) bool { return e.Raw == `{"error":{"message":"boom"}}` }},
		{backend.OpenAIChat, &backend.StreamError{Data: []byte(`not json`)}, func(e gjson.Result) bool {
			return e.Get("error.type").String() == "backend_error" && e.Get("error.message").String() == "not json"
		}},
		{backend.OpenAIChat, failed, func(e gjson.Result) bool { return e.Get("error.type").String() == "gateway_error" }},
		{backend.AnthropicMessages, &backend.StreamError{Data: []byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)}, func(e gjson.Result) bool { return e.Get("error.type").String() == "overloaded_error" }},
		{backend.AnthropicMessages, failed, func(e gjson.Result) bool {
			return e.Get("type").String() == "error" && e.Get("error.type").String() == "api_error"
		}},
		{backend.OpenAIResponses, &backend.StreamError{Data: []byte(`{"type":"error","code":"server_error","message":"boom","sequence_number":3}`)}, func(e gjson.Result) bool { return e.Get("code").String() == "server_error" }},
		{backend.OpenAIResponses, failed, func(e gjson.Result) bool {
			return e.Get("type").String() == "error" && e.Get("sequence_number").Int() == 7
		}},
	}
	for _, c := range cases {
		if event := gjson.ParseBytes(Of(c.protocol).ErrorEvent(c.err, 7)); !c.check(event) {
			t.Errorf("%s, %v: got %s", c.protocol, c.err, event.Raw)
		}
	}
}
