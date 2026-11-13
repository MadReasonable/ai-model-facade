package translate

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
)

// What only OpenAI's servers do is refused with a 400 in OpenAI's shape, by
// both translators.
func TestResponses_RefusesWhatOnlyOpenAIServes(t *testing.T) {
	for _, body := range []string{
		`{"tools":[{"type":"file_search"}],"input":"hi"}`,
		`{"background":true,"input":"hi"}`,
		`{"conversation":"conv_1","input":"hi"}`,
		`{"input":[{"type":"item_reference","id":"msg_1"}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file_1"}]}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]}`,
		`{"input":[{"type":"computer_call"}]}`,
	} {
		for name, translator := range map[string]Translator{"chat": responsesToChat{}, "messages": responsesToMessages{}} {
			_, _, err := translator.Request([]byte(body))
			var refused *Refusal
			if !errors.As(err, &refused) || refused.Response.Status != http.StatusBadRequest || gjson.GetBytes(refused.Response.Body, "error.type").String() != "invalid_request_error" {
				t.Errorf("%s: %s: got %v", name, body, err)
			}
		}
	}
}

// A backend's error event ends the stream with a Responses error event, numbered
// after the events before it.
func TestResponses_EndsABrokenStreamWithAnErrorEvent(t *testing.T) {
	_, reply, _ := responsesToChat{}.Request([]byte(`{"input":"hi","stream":true}`))
	result := reply.Result(backend.ChatResult{Stream: &chunks{end: &backend.StreamError{Data: []byte(`{"error":{"message":"boom"}}`)}, data: []string{`{"choices":[{"delta":{"content":"hi"}}]}`}}})
	events, err := eventsOf(t, result.Stream)
	var streamErr *backend.StreamError
	if !errors.As(err, &streamErr) || gjson.GetBytes(streamErr.Data, "type").String() != "error" || gjson.GetBytes(streamErr.Data, "message").String() != "boom" || gjson.GetBytes(streamErr.Data, "sequence_number").Int() != int64(len(events)) {
		t.Errorf("error event: got %v after %d events", err, len(events))
	}
}

// An error event, or a failed response, ends the stream with an error in the
// client's shape, and a body that ends before the response does stays an
// error, in both directions.
func TestToResponses_EndsABrokenStreamWithAnError(t *testing.T) {
	for name, translator := range map[string]Translator{"chat": chatToResponses{}, "messages": messagesToResponses{}} {
		_, reply, _ := translator.Request([]byte(`{"messages":[],"stream":true}`))
		for _, end := range []error{&backend.StreamError{Data: []byte(`{"type":"error","code":"server_error","message":"boom","sequence_number":3}`)}} {
			_, err := eventsOf(t, reply.Result(backend.ChatResult{Stream: &chunks{end: end, data: responsesStreamOf("response.completed")[:2]}}).Stream)
			var streamErr *backend.StreamError
			if !errors.As(err, &streamErr) || gjson.GetBytes(streamErr.Data, "error.message").String() != "boom" {
				t.Errorf("%s: error event: got %v", name, err)
			}
		}
		_, err := eventsOf(t, reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: append(responsesStreamOf("")[:2], `{"type":"response.failed","response":{"status":"failed","error":{"message":"boom"}}}`)}}).Stream)
		var streamErr *backend.StreamError
		if !errors.As(err, &streamErr) || gjson.GetBytes(streamErr.Data, "error.message").String() != "boom" {
			t.Errorf("%s: failed: got %v", name, err)
		}
		if _, err := eventsOf(t, reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: responsesStreamOf("")[:3]}}).Stream); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s: early end: got %v", name, err)
		}
	}
}
