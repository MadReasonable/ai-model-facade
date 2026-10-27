package lmstudio

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

func newBackend(t *testing.T, f *testkit.LMStudio, extra string) *Backend {
	t.Helper()
	b, err := New(testkit.Entry(t, "lmStudio", `{type: lmStudio, baseUrl: "`+f.URL+`"`+extra+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The model list is LM Studio's LLMs by key; its embedding model is left
// out, since the gateway serves completions only.
func TestLMStudio_ListsItsLLMs(t *testing.T) {
	got, err := newBackend(t, testkit.NewLMStudio(t, "a", "b"), "").ListModels(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("got %+v, %v", got, err)
	}
}

// A model with a loaded instance is ready, and only that one.
func TestLMStudio_ReportsTheLoadedModelAsReady(t *testing.T) {
	f := testkit.NewLMStudio(t, "a", "b")
	f.SetLoaded("b")
	got, err := newBackend(t, f, "").LoadedModels(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "b" || got[0].Starting {
		t.Errorf("got %+v, %v", got, err)
	}
}

// A chat sent through this backend counts as pending, so the model it loads
// shows as starting while a cached state predates the load, and the state
// is fetched again once it has loaded, when LM Studio lists it.
func TestLMStudio_CountsItsChatsAsPending(t *testing.T) {
	f := testkit.NewLMStudio(t, "a")
	b := newBackend(t, f, ", stateCacheSeconds: 60")
	b.LoadedModels(context.Background())
	load := f.HoldLoads()
	done := make(chan error)
	go func() {
		_, err := b.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"a"}`)})
		done <- err
	}()
	f.WaitFor(t, "POST /v1/chat/completions", 1)
	if got, _ := b.LoadedModels(context.Background()); len(got) != 1 || !got[0].Starting {
		t.Errorf("during the load: %+v", got)
	}
	load.Open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, _ := b.LoadedModels(context.Background()); len(got) != 1 || got[0].Starting || f.Count("GET /api/v1/models") != 2 {
		t.Errorf("after the load: %+v after %d list reads", got, f.Count("GET /api/v1/models"))
	}
}

// LM Studio's list answers 500 while it unpacks its engines after starting,
// so a state read then wraps ErrLoading; and the backend says a load of one
// model aborts a load of another.
func TestLMStudio_ReadWhileUnpackingIsLoading(t *testing.T) {
	f := testkit.NewLMStudio(t, "a")
	f.SetUnpacking(true)
	b := newBackend(t, f, "")
	if got, err := b.LoadedModels(context.Background()); !errors.Is(err, backend.ErrLoading) {
		t.Errorf("while unpacking: %+v, %v; want ErrLoading", got, err)
	}
	if _, ok := any(b).(backend.LoadsOneAtATime); !ok {
		t.Error("doesn't implement backend.LoadsOneAtATime")
	}
}

// A list read that fails with another status than 500, such as a wrong
// apiKey or baseUrl, is a failure of its own and not ErrLoading, so the
// backend isn't reported busy.
func TestLMStudio_OtherFailuresAreNotLoading(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusNotFound} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, http.StatusText(code), code)
		}))
		defer srv.Close()
		b, err := New(testkit.Entry(t, "lmStudio", `{type: lmStudio, baseUrl: "`+srv.URL+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.LoadedModels(context.Background()); err == nil || errors.Is(err, backend.ErrLoading) {
			t.Errorf("%d: got %v; want an error other than ErrLoading", code, err)
		}
	}
}

func TestLMStudio_RejectsAWrongConfig(t *testing.T) {
	for _, src := range []string{"{type: lmStudio}", "{type: lmStudio, baseUrl: x, ttl: 60}"} {
		if _, err := New(testkit.Entry(t, "lmStudio", src)); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}
