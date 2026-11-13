package history

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/pipeline"
)

var generous = Limits{MaxBytes: 1 << 20, MaxAge: time.Hour}

// keeper is a service that keeps conversations, answering ResponsesHistory
// with history.
type keeper struct {
	backend.Backend
	history string
	reads   []string
}

func (k *keeper) ResponsesHistory(ctx context.Context, id string, client backend.Client) ([]byte, error) {
	k.reads = append(k.reads, id)
	return []byte(k.history), nil
}

func response(id, output string) backend.ChatResult {
	return backend.ChatResult{Response: &backend.ChatResponse{Status: http.StatusOK, Body: []byte(`{"id":"` + id + `","object":"response","output":` + output + `}`)}}
}

// serve prepares body for winner, then records the response with this ID and
// output, and returns the body the winner was sent.
func serve(t *testing.T, s *Store, winner pipeline.Winner, body, id, output string) string {
	t.Helper()
	sent, turn, _, err := s.Prepare(context.Background(), []byte(body), winner)
	if err != nil {
		t.Fatal(err)
	}
	turn.Record(response(id, output))
	return string(sent)
}

var local = pipeline.Winner{Backend: "llama"}

// A request naming an earlier response is sent the conversation up to it as
// input, earlier input and output first, without previous_response_id, and
// its other keys keep their places.
func TestHistory_ExpandsTheConversationIntoInput(t *testing.T) {
	s := NewStore(generous)
	serve(t, s, local, `{"model":"m","input":"one"}`, "r1", `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"1"}]}]`)
	serve(t, s, local, `{"model":"m","previous_response_id":"r1","input":[{"role":"user","content":"two"}]}`, "r2", `[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}]`)
	sent := serve(t, s, local, `{"model":"m","previous_response_id":"r2","input":[{"type":"function_call_output","call_id":"c","output":"x"}],"stream":true}`, "r3", `[]`)
	want := `{"model":"m","input":[{"role":"user","content":"one"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"1"}]},{"role":"user","content":"two"},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"x"}],"stream":true}`
	if sent != want {
		t.Errorf("sent\n%s\nwant\n%s", sent, want)
	}
}

// A request without previous_response_id is sent unchanged.
func TestHistory_LeavesAFirstTurnAlone(t *testing.T) {
	s := NewStore(generous)
	body := `{"model":"m","input":"one","store":true}`
	if sent := serve(t, s, local, body, "r1", `[]`); sent != body {
		t.Errorf("sent %s", sent)
	}
}

// An unknown response, and one made with store false, are refused as OpenAI
// refuses them: a 400 naming the ID.
func TestHistory_RefusesAnUnknownResponse(t *testing.T) {
	s := NewStore(generous)
	serve(t, s, local, `{"model":"m","input":"one","store":false}`, "r1", `[]`)
	for _, id := range []string{"r1", "nothing"} {
		_, _, _, err := s.Prepare(context.Background(), []byte(`{"previous_response_id":"`+id+`"}`), local)
		var refused *pipeline.Error
		if !errors.As(err, &refused) || refused.Status != http.StatusBadRequest || refused.Message != "Previous response with id '"+id+"' not found." {
			t.Errorf("%s: %v", id, err)
		}
	}
}

// Only a successful response is a turn.
func TestHistory_DoesNotRecordAFailure(t *testing.T) {
	s := NewStore(generous)
	_, tr, _, _ := s.Prepare(context.Background(), []byte(`{"input":"one"}`), local)
	tr.Record(backend.ChatResult{Response: &backend.ChatResponse{Status: http.StatusBadRequest, Body: []byte(`{"id":"r1"}`)}})
	if _, _, _, err := s.Prepare(context.Background(), []byte(`{"previous_response_id":"r1"}`), local); err == nil {
		t.Error("a failed response was recorded")
	}
}

type events struct {
	data []string
}

func (e *events) Next() ([]byte, error) {
	if len(e.data) == 0 {
		return nil, io.EOF
	}
	next := e.data[0]
	e.data = e.data[1:]
	return []byte(next), nil
}

func (e *events) Close() error { return nil }

// A stream's turn is recorded from the event that ends it, completed or
// incomplete, and the events reach the client unchanged.
func TestHistory_RecordsAStreamFromItsLastEvent(t *testing.T) {
	for _, end := range []string{"response.completed", "response.incomplete"} {
		s := NewStore(generous)
		_, tr, _, _ := s.Prepare(context.Background(), []byte(`{"input":"one"}`), local)
		stream := tr.Record(backend.ChatResult{Stream: &events{data: []string{
			`{"type":"response.created","response":{"id":"r1","output":[]}}`,
			`{"type":"` + end + `","response":{"id":"r1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"1"}]}]}}`,
		}}}).Stream
		for {
			if _, err := stream.Next(); err != nil {
				break
			}
		}
		sent, _, _, err := s.Prepare(context.Background(), []byte(`{"previous_response_id":"r1","input":"two"}`), local)
		if err != nil || gjson.GetBytes(sent, "input.1.content.0.text").String() != "1" {
			t.Errorf("%s: %s, %v", end, sent, err)
		}
	}
}

// A candidate that keeps its conversations at the service is passed the ID
// of a response that service made, unexpanded. Any other winner gets the
// conversation, read from the service up to that turn on every request, and
// from the store after it.
func TestHistory_ReadsATurnKeptByTheService(t *testing.T) {
	s := NewStore(generous)
	service := &keeper{history: `[{"role":"user","content":"one"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"1"}]}]`}
	hosted := pipeline.Winner{Backend: "openAi", Native: true, Service: service, Backends: map[string]backend.Backend{"openAi": service}}
	serve(t, s, hosted, `{"input":"one"}`, "o1", `[{"type":"message"}]`)

	body := `{"previous_response_id":"o1","input":"two"}`
	if sent := serve(t, s, hosted, body, "o2", `[]`); sent != body || len(service.reads) != 0 {
		t.Errorf("same service: sent %s, read %v", sent, service.reads)
	}
	elsewhere := pipeline.Winner{Backend: "llama", Backends: hosted.Backends}
	serve(t, s, elsewhere, `{"previous_response_id":"o1","input":"three"}`, "l3", `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"3"}]}]`)
	sent := serve(t, s, elsewhere, `{"previous_response_id":"l3","input":"four"}`, "l4", `[]`)
	if texts := gjson.Get(sent, "input.#.content").Raw; texts != `["one",[{"type":"output_text","text":"1"}],"three",[{"type":"output_text","text":"3"}],"four"]` || len(service.reads) != 2 || service.reads[1] != "o1" {
		t.Errorf("elsewhere: sent %s, read %v", sent, service.reads)
	}
}

// A conversation kept by a backend that a reload removed can't be read.
func TestHistory_FailsWhenTheServiceIsGone(t *testing.T) {
	s := NewStore(generous)
	service := &keeper{}
	serve(t, s, pipeline.Winner{Backend: "openAi", Native: true, Service: service}, `{"input":"one"}`, "o1", `[]`)
	_, _, _, err := s.Prepare(context.Background(), []byte(`{"previous_response_id":"o1"}`), pipeline.Winner{Backend: "llama", Backends: map[string]backend.Backend{}})
	var failed *pipeline.Error
	if !errors.As(err, &failed) || failed.Status != http.StatusBadGateway {
		t.Errorf("got %v", err)
	}
}

// Reasoning items made on another backend are left out and reported, since
// only the backend that made one can read it; the winner's own are kept.
func TestHistory_DropsReasoningFromAnotherBackend(t *testing.T) {
	s := NewStore(generous)
	serve(t, s, pipeline.Winner{Backend: "claude"}, `{"input":"one"}`, "c1", `[{"type":"reasoning","encrypted_content":"x"},{"type":"message"}]`)
	sent, _, dropped, _ := s.Prepare(context.Background(), []byte(`{"previous_response_id":"c1"}`), pipeline.Winner{Backend: "llama"})
	if gjson.GetBytes(sent, "input.#").Int() != 2 || len(dropped) != 1 || dropped[0] != "reasoning" {
		t.Errorf("another backend: %s, %v", sent, dropped)
	}
	sent, _, dropped, _ = s.Prepare(context.Background(), []byte(`{"previous_response_id":"c1"}`), pipeline.Winner{Backend: "claude"})
	if gjson.GetBytes(sent, "input.#").Int() != 3 || len(dropped) != 0 {
		t.Errorf("the same backend: %s, %v", sent, dropped)
	}
}

func known(s *Store, id string) bool {
	_, err := s.chain(id)
	return err == nil
}

// Over its byte limit, the store evicts the least recently used
// conversations, each with all its turns.
func TestHistory_EvictsTheLeastRecentlyUsedConversation(t *testing.T) {
	s := NewStore(Limits{MaxBytes: 3 * (turnOverhead + 100), MaxAge: time.Hour})
	output := `["` + strings.Repeat("x", 40) + `"]`
	serve(t, s, local, `{"input":"a"}`, "a1", output)
	serve(t, s, local, `{"input":"b"}`, "b1", output)
	serve(t, s, local, `{"previous_response_id":"a1"}`, "a2", output)
	serve(t, s, local, `{"input":"c"}`, "c1", output)
	if known(s, "b1") || !known(s, "a1") || !known(s, "a2") || !known(s, "c1") {
		t.Errorf("b should have been evicted: a1 %v, a2 %v, b1 %v, c1 %v", known(s, "a1"), known(s, "a2"), known(s, "b1"), known(s, "c1"))
	}
}

// A conversation unused for longer than the age limit is evicted.
func TestHistory_EvictsAnOldConversation(t *testing.T) {
	s := NewStore(Limits{MaxBytes: 1 << 20, MaxAge: time.Minute})
	clock := time.Unix(0, 0)
	s.now = func() time.Time { return clock }
	serve(t, s, local, `{"input":"a"}`, "a1", `[]`)
	clock = clock.Add(2 * time.Minute)
	serve(t, s, local, `{"input":"b"}`, "b1", `[]`)
	if known(s, "a1") || !known(s, "b1") {
		t.Errorf("a1 %v, b1 %v", known(s, "a1"), known(s, "b1"))
	}
}
