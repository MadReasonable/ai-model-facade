package llamaserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

func newBackend(t *testing.T, f *testkit.LlamaServer, extra string) *Backend {
	t.Helper()
	b, err := New(testkit.Entry(t, "llamaServer", `{type: llamaServer, baseUrl: "`+f.URL+`"`+extra+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// In router mode each model's status is the state: loaded models are ready
// and loading ones starting, each with its aliases; unloaded and sleeping
// ones don't count, since a request to them loads the model.
func TestLlamaServer_RouterReportsLoadedAndLoadingModels(t *testing.T) {
	f := testkit.NewLlamaServerRouter(t, testkit.ServerModel{ID: "a", Aliases: []string{"a-alt"}}, testkit.ServerModel{ID: "b"}, testkit.ServerModel{ID: "c"}, testkit.ServerModel{ID: "d"})
	f.SetStatus("a", "loaded")
	f.SetStatus("b", "loading")
	f.SetStatus("c", "sleeping")
	got, err := newBackend(t, f, "").LoadedModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[0].Starting || strings.Join(got[0].Aliases, ",") != "a-alt" || got[1].ID != "b" || !got[1].Starting {
		t.Errorf("got %+v", got)
	}
}

// In single-model mode the one model is ready, unless it is sleeping or
// still loading. While it loads, llama-server refuses every request, so the
// read fails with ErrLoading: the server is busy, not starting a model a
// request could wait for.
func TestLlamaServer_SingleReportsItsModelOnlyWhileServing(t *testing.T) {
	for _, c := range []struct {
		status string
		ready  bool
	}{
		{"loaded", true},
		{"sleeping", false},
		{"loading", false},
	} {
		t.Run(c.status, func(t *testing.T) {
			f := testkit.NewLlamaServerSingle(t, testkit.ServerModel{ID: "a"})
			f.SetStatus("a", c.status)
			got, err := newBackend(t, f, "").LoadedModels(context.Background())
			if c.status == "loading" {
				if !errors.Is(err, backend.ErrLoading) {
					t.Errorf("got %+v, %v; want ErrLoading", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ready := len(got) == 1 && got[0].ID == "a" && !got[0].Starting; ready != c.ready || !c.ready && len(got) != 0 {
				t.Errorf("got %+v", got)
			}
		})
	}
}

// The model list is every model the server knows, with its aliases marked as
// plain names; in single-model mode the model's own ID isn't among them.
func TestLlamaServer_ListsModelsWithPlainAliases(t *testing.T) {
	for name, f := range map[string]*testkit.LlamaServer{
		"router": testkit.NewLlamaServerRouter(t, testkit.ServerModel{ID: "a", Aliases: []string{"a-alt"}}),
		"single": testkit.NewLlamaServerSingle(t, testkit.ServerModel{ID: "a", Aliases: []string{"a-alt"}}),
	} {
		got, err := newBackend(t, f, "").ListModels(context.Background())
		if err != nil || len(got) != 1 || got[0].ID != "a" || strings.Join(got[0].Aliases, ",") != "a-alt" || !got[0].PlainAliases {
			t.Errorf("%s: got %+v, %v", name, got, err)
		}
	}
}

// A chat sent through this backend counts as pending, so a load it starts
// shows as starting even while the cached state predates it, and the state
// is fetched again once it has loaded.
func TestLlamaServer_CountsItsChatsAsPending(t *testing.T) {
	f := testkit.NewLlamaServerRouter(t, testkit.ServerModel{ID: "a"})
	b := newBackend(t, f, ", stateCacheSeconds: 60")
	b.LoadedModels(context.Background())
	load := f.HoldLoads()
	done := make(chan error)
	go func() {
		_, err := b.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"a"}`)})
		done <- err
	}()
	f.WaitFor(t, "POST /v1/chat/completions", 1)
	if got, _ := b.LoadedModels(context.Background()); len(got) != 1 || !got[0].Starting || f.Count("GET /props") != 1 {
		t.Errorf("during the load: %+v after %d /props reads", got, f.Count("GET /props"))
	}
	load.Open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, _ := b.LoadedModels(context.Background()); len(got) != 1 || got[0].Starting || f.Count("GET /props") != 2 {
		t.Errorf("after the load: %+v after %d /props reads", got, f.Count("GET /props"))
	}
}

func TestLlamaServer_RejectsAWrongConfig(t *testing.T) {
	for _, src := range []string{"{type: llamaServer}", "{type: llamaServer, baseUrl: x, mode: router}"} {
		if _, err := New(testkit.Entry(t, "llamaServer", src)); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}
