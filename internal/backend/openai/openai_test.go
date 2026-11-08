package openai

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

func newBackend(t *testing.T, f *testkit.OpenAI, extra string) *Backend {
	t.Helper()
	b, err := New(testkit.Entry(t, "openAi", `{type: openAi, baseUrl: "`+f.URL+`", apiKey: test-key`+extra+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The model list is every model the API lists, read with the backend's key.
func TestOpenAI_ListsItsModels(t *testing.T) {
	f := testkit.NewOpenAI(t, "gpt-a", "gpt-b")
	got, err := newBackend(t, f, "").ListModels(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "gpt-a" || got[1].ID != "gpt-b" {
		t.Errorf("got %+v, %v", got, err)
	}
	if r, _ := f.Last("GET /v1/models"); r.Header.Get("Authorization") != "Bearer test-key" {
		t.Errorf("Authorization %q", r.Header.Get("Authorization"))
	}
}

// Every listed model is ready, and a state read after the listing calls the
// API no more, even with stateCacheSeconds 0: a hosted model needs no load.
func TestOpenAI_EveryListedModelIsReadyWithoutACall(t *testing.T) {
	f := testkit.NewOpenAI(t, "gpt-a", "gpt-b")
	b := newBackend(t, f, "")
	if _, err := b.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		got, err := b.LoadedModels(context.Background())
		if err != nil || len(got) != 2 || got[0].Starting || got[1].Starting {
			t.Fatalf("got %+v, %v", got, err)
		}
	}
	if n := f.Count("GET /v1/models"); n != 1 {
		t.Errorf("/v1/models read %d times, want 1", n)
	}
}

// A request that fails with a 5xx makes the next state read list the API
// again, and one that gets no answer finds it gone: its models read as
// unavailable rather than ready. A 4xx is the client's, and changes nothing.
func TestOpenAI_AFailedRequestMakesTheStateCheckTheAPI(t *testing.T) {
	f := testkit.NewOpenAI(t, "gpt-a")
	b := newBackend(t, f, "")
	if _, err := b.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	chat := func() {
		b.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"gpt-a"}`)})
	}
	f.SetReply(testkit.Reply{Status: http.StatusBadRequest})
	chat()
	b.LoadedModels(context.Background())
	if n := f.Count("GET /v1/models"); n != 1 {
		t.Errorf("after a 400: /v1/models read %d times, want 1", n)
	}
	f.SetReply(testkit.Reply{Status: http.StatusServiceUnavailable})
	chat()
	if got, err := b.LoadedModels(context.Background()); err != nil || len(got) != 1 || f.Count("GET /v1/models") != 2 {
		t.Errorf("after a 503: %+v, %v, /v1/models read %d times, want 2", got, err, f.Count("GET /v1/models"))
	}
	f.Close()
	chat()
	if got, err := b.LoadedModels(context.Background()); err == nil {
		t.Errorf("with the API gone: %+v, want an error", got)
	}
}

// With stateCacheSeconds set, a state read lists the API again once the
// list is that old, so an outage shows before a request fails.
func TestOpenAI_ChecksTheAPIAgainAfterStateCacheSeconds(t *testing.T) {
	f := testkit.NewOpenAI(t, "gpt-a")
	b := newBackend(t, f, ", stateCacheSeconds: 0.001")
	if _, err := b.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Close()
	deadline := testkit.Timeout
	for start := time.Now(); time.Since(start) < deadline; {
		if _, err := b.LoadedModels(context.Background()); err != nil {
			return
		}
	}
	t.Errorf("the API stayed ready for %v after it went", deadline)
}

// A state read before any listing, such as after a failed one at startup,
// lists the API itself.
func TestOpenAI_StateListsTheAPIIfNotListedYet(t *testing.T) {
	f := testkit.NewOpenAI(t, "gpt-a")
	got, err := newBackend(t, f, "").LoadedModels(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "gpt-a" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestOpenAI_RejectsAWrongConfig(t *testing.T) {
	for _, src := range []string{"{type: openAi}", "{type: openAi, baseUrl: x, organization: o}"} {
		if _, err := New(testkit.Entry(t, "openAi", src)); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}
