package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

// streamTask sends a streamed request for task to a ready backend that
// answers with reply, and returns the response's events.
func streamTask(t *testing.T, reply testkit.Reply) (response, []string) {
	t.Helper()
	swap := newSwap(t)
	swap.SetState("qwen3.6-35b-a3b:q8_0", "ready")
	swap.SetReply(reply)
	r := startTask(t, swap).chat(t, `{"model":"task","stream":true}`)
	return r, strings.Split(strings.TrimSpace(r.body), "\n\n")
}

// A stream is relayed event by event, ending with [DONE].
func TestStreams_RelayEventsAndEndWithDone(t *testing.T) {
	r, events := streamTask(t, testkit.Reply{Stream: []string{`{"choices":[{"delta":{"content":"hi"}}]}`}})
	if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("got %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if strings.Join(events, "|") != `data: {"choices":[{"delta":{"content":"hi"}}]}|data: [DONE]` {
		t.Errorf("got %q", r.body)
	}
}

// A stream the backend breaks off ends with an error event and no [DONE], so
// a client shows an error instead of a silently cut-off reply.
func TestStreams_EndABrokenStreamWithAnErrorEvent(t *testing.T) {
	r, events := streamTask(t, testkit.Reply{Stream: []string{`{"choices":[{"delta":{"content":"hi"}}]}`}, Truncate: true})
	if len(events) != 2 || strings.Contains(r.body, "[DONE]") {
		t.Fatalf("got %q", r.body)
	}
	var last struct {
		Error struct{ Message, Type string }
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(events[1], "data: ")), &last); err != nil || last.Error.Type != "gateway_error" {
		t.Errorf("got %q, %v", events[1], err)
	}
}

// A backend's own error event is relayed verbatim as the last event.
func TestStreams_RelayABackendErrorEvent(t *testing.T) {
	r, events := streamTask(t, testkit.Reply{Stream: []string{`{"choices":[{"delta":{"content":"hi"}}]}`}, ErrorEvent: `{"error":{"message":"context full","type":"server_error"}}`})
	if len(events) != 2 || strings.Contains(r.body, "[DONE]") || events[1] != `data: {"error":{"message":"context full","type":"server_error"}}` {
		t.Errorf("got %q", r.body)
	}
}
