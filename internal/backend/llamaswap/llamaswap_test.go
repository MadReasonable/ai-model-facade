package llamaswap

import (
	"context"
	"strings"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

func newBackend(t *testing.T, f *testkit.LlamaSwap, extra string) *Backend {
	t.Helper()
	b, err := New(testkit.Entry(t, "llamaSwap", `{type: llamaSwap, baseUrl: "`+f.URL+`"`+extra+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// /running is the state: ready and starting models count, starting ones
// marked, stopping ones don't; each running model carries its aliases from
// the model list.
func TestLlamaSwap_ReportsReadyAndStartingModels(t *testing.T) {
	f := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "a", Aliases: []string{"a:Fast"}}, testkit.SwapModel{ID: "b"}, testkit.SwapModel{ID: "c"})
	f.SetState("a", "ready")
	f.SetState("b", "starting")
	f.SetState("c", "stopping")
	got, err := newBackend(t, f, "").LoadedModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[0].Starting || strings.Join(got[0].Aliases, ",") != "a:Fast" || got[1].ID != "b" || !got[1].Starting {
		t.Errorf("got %+v", got)
	}
}

// The model list has each base model once, with its aliases, and not the
// aliases' own entries.
func TestLlamaSwap_ListsBaseModelsWithTheirAliases(t *testing.T) {
	f := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "a"}, testkit.SwapModel{ID: "b", Aliases: []string{"b:Fast", "b:Slow"}})
	got, err := newBackend(t, f, "").ListModels(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "a" || len(got[0].Aliases) != 0 || got[1].ID != "b" || strings.Join(got[1].Aliases, ",") != "b:Fast,b:Slow" {
		t.Errorf("got %+v, %v", got, err)
	}
}

// A chat sent through this backend counts as pending, so a load it starts
// shows as starting even while the cached /running predates it, and the
// state is fetched again once it has loaded.
func TestLlamaSwap_CountsItsChatsAsPending(t *testing.T) {
	f := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "a"})
	b := newBackend(t, f, ", stateCacheSeconds: 60")
	b.LoadedModels(context.Background())
	load := f.HoldLoads()
	done := make(chan error)
	go func() {
		_, err := b.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"a"}`)})
		done <- err
	}()
	f.WaitFor(t, "POST /v1/chat/completions", 1)
	if got, _ := b.LoadedModels(context.Background()); len(got) != 1 || !got[0].Starting || f.Count("GET /running") != 1 {
		t.Errorf("during the load: %+v after %d /running reads", got, f.Count("GET /running"))
	}
	load.Open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, _ := b.LoadedModels(context.Background()); len(got) != 1 || got[0].Starting || f.Count("GET /running") != 2 {
		t.Errorf("after the load: %+v after %d /running reads", got, f.Count("GET /running"))
	}
}

func TestLlamaSwap_RejectsAWrongConfig(t *testing.T) {
	for _, src := range []string{"{type: llamaSwap}", "{type: llamaSwap, baseUrl: x, stateCache: 2}"} {
		if _, err := New(testkit.Entry(t, "llamaSwap", src)); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}
