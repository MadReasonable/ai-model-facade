package translate

import (
	"errors"
	"io"
	"net/http"
	"slices"
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

// A namespace groups a client's functions, which a backend without
// namespaces gets as functions named for their namespace too. The history's
// calls and a tool choice name them that way, and a call comes back with its
// namespace and its own name, whole and streamed, through both translators.
func TestResponses_FlattenNamespacesForOtherBackends(t *testing.T) {
	request := `{"tool_choice":{"type":"function","name":"close"},"tools":[{"type":"function","name":"top"},` +
		`{"type":"namespace","name":"agents","description":"Manage agents.","tools":[{"type":"function","name":"close","description":"Close one.","parameters":{"type":"object"}}]}],` +
		`"input":[{"type":"function_call","call_id":"c1","namespace":"agents","name":"close","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"closed"}]}`
	out, reply, err := responsesToMessages{}.Request([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "tools.#.name").Raw; got != `["top","agents__close"]` || gjson.GetBytes(out, "tools.1.description").String() != "Manage agents.\n\nClose one." ||
		gjson.GetBytes(out, "messages.0.content.0.name").String() != "agents__close" || gjson.GetBytes(out, "tool_choice.name").String() != "agents__close" {
		t.Errorf("to Messages: %s", out)
	}
	message := `{"id":"m","model":"c","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"agents__close","input":{}}],"usage":{"input_tokens":1,"output_tokens":1}}`
	call := gjson.GetBytes(reply.Result(backend.ChatResult{Response: &backend.ChatResponse{Status: 200, Body: []byte(message)}}).Response.Body, "output.0")
	if call.Get("name").String() != "close" || call.Get("namespace").String() != "agents" {
		t.Errorf("from Messages: %s", call.Raw)
	}

	out, reply, err = responsesToChat{}.Request([]byte(request))
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "tools.#.function.name").Raw; got != `["top","agents__close"]` || gjson.GetBytes(out, "messages.0.tool_calls.0.function.name").String() != "agents__close" {
		t.Errorf("to Chat: %s", out)
	}
	events, err := eventsOf(t, reply.Result(backend.ChatResult{Stream: &chunks{end: io.EOF, data: []string{
		`{"model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"agents__close","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}}}).Stream)
	if err != nil {
		t.Fatal(err)
	}
	if call := events[len(events)-1].Get("response.output.0"); call.Get("name").String() != "close" || call.Get("namespace").String() != "agents" {
		t.Errorf("from Chat, streamed: %s", call.Raw)
	}
}

// A namespaced name that is another tool's name, and a tool choice naming a
// tool two namespaces hold, can't be told apart, and are refused.
func TestResponses_RefuseNamespacesThatClash(t *testing.T) {
	agents := `{"type":"namespace","name":"agents","description":"d","tools":[{"type":"function","name":"close"}]}`
	for _, body := range []string{
		`{"tools":[{"type":"function","name":"agents__close"},` + agents + `],"input":"hi"}`,
		`{"tool_choice":{"type":"function","name":"close"},"tools":[` + agents + `,{"type":"namespace","name":"jobs","description":"d","tools":[{"type":"function","name":"close"}]}],"input":"hi"}`,
	} {
		for name, translator := range map[string]Translator{"chat": responsesToChat{}, "messages": responsesToMessages{}} {
			if _, _, err := translator.Request([]byte(body)); err == nil {
				t.Errorf("%s: %s: not refused", name, body)
			}
		}
	}
}

// A search limited to OpenAI's cached index is dropped and reported toward a
// backend whose search is live, rather than searching more widely than the
// client allowed.
func TestResponses_DropACacheOnlySearch(t *testing.T) {
	body := `{"tools":[{"type":"web_search","external_web_access":false}],"input":"hi"}`
	for name, translator := range map[string]Translator{"chat": responsesToChat{}, "messages": responsesToMessages{}} {
		out, reply, err := translator.Request([]byte(body))
		if err != nil || gjson.GetBytes(out, "tools").Exists() || gjson.GetBytes(out, "web_search_options").Exists() || !slices.Contains(reply.Dropped(), "tools.web_search") {
			t.Errorf("%s: %s, dropped %v, %v", name, out, reply.Dropped(), err)
		}
	}
}
