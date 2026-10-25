package ollama

import (
	"context"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

func newBackend(t *testing.T, f *testkit.Ollama, extra string) *Backend {
	t.Helper()
	b, err := New(testkit.Entry(t, "ollama", `{type: ollama, baseUrl: "`+f.URL+`"`+extra+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// /api/ps is the state, and every model it lists is ready: Ollama reports no
// load state of its own.
func TestOllama_ReportsLoadedModelsAsReady(t *testing.T) {
	f := testkit.NewOllama(t, "a:1", "b:1")
	f.SetLoaded("b:1")
	got, err := newBackend(t, f, "").LoadedModels(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "b:1" || got[0].Starting {
		t.Errorf("got %+v, %v", got, err)
	}
}

// The model list is the pulled models, by their full name:tag IDs.
func TestOllama_ListsPulledModels(t *testing.T) {
	got, err := newBackend(t, testkit.NewOllama(t, "llama3:latest", "qwen2.5:7b"), "").ListModels(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "llama3:latest" || got[1].ID != "qwen2.5:7b" {
		t.Errorf("got %+v, %v", got, err)
	}
}

// Ollama doesn't list a model while it loads, so a load the gateway started
// shows as starting only through the pending chat, and as ready once Ollama
// lists it.
func TestOllama_ShowsItsChatsLoadAsStarting(t *testing.T) {
	f := testkit.NewOllama(t, "a:1")
	b := newBackend(t, f, "")
	load := f.HoldLoads()
	done := make(chan error)
	go func() {
		_, err := b.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"a:1"}`)})
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
	if got, _ := b.LoadedModels(context.Background()); len(got) != 1 || got[0].Starting {
		t.Errorf("after the load: %+v", got)
	}
}

func TestOllama_RejectsAWrongConfig(t *testing.T) {
	for _, src := range []string{"{type: ollama}", "{type: ollama, baseUrl: x, keepAlive: 5m}"} {
		if _, err := New(testkit.Entry(t, "ollama", src)); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}
