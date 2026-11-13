package localai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

func newBackend(t *testing.T, url, extra string) *Backend {
	t.Helper()
	b, err := New(testkit.Entry(t, "localAi", `{type: localAi, baseUrl: "`+url+`"`+extra+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The model list is LocalAI's models, each alias folded into its target as
// a plain alias rather than listed on its own.
func TestLocalAI_ListsModelsWithTheirAliases(t *testing.T) {
	f := testkit.NewLocalAI(t, 0, "a", "b")
	f.SetAlias("a-fast", "a")
	got, err := newBackend(t, f.URL, "").ListModels(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "a" || strings.Join(got[0].Aliases, ",") != "a-fast" || !got[0].PlainAliases || got[1].ID != "b" {
		t.Errorf("got %+v, %v", got, err)
	}
}

// /system is the state: every model it lists is ready, with its aliases.
func TestLocalAI_ReportsLoadedModelsAsReady(t *testing.T) {
	f := testkit.NewLocalAI(t, 0, "a", "b")
	f.SetAlias("a-fast", "a")
	f.SetLoaded("a")
	got, err := newBackend(t, f.URL, "").LoadedModels(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "a" || got[0].Starting || strings.Join(got[0].Aliases, ",") != "a-fast" {
		t.Errorf("got %+v, %v", got, err)
	}
}

// LocalAI doesn't list a model while it loads, so a load the gateway started
// through an alias shows as starting under its target, through the pending
// chat, and as ready once LocalAI lists it.
func TestLocalAI_ShowsItsChatsLoadAsStarting(t *testing.T) {
	f := testkit.NewLocalAI(t, 0, "a")
	f.SetAlias("a-fast", "a")
	b := newBackend(t, f.URL, "")
	load := f.HoldLoads()
	done := make(chan error)
	go func() {
		_, err := b.Chat(context.Background(), backend.ChatRequest{Protocol: backend.OpenAIChat, Body: []byte(`{"model":"a-fast"}`)})
		done <- err
	}()
	f.WaitFor(t, "POST /v1/chat/completions", 1)
	if got, _ := b.LoadedModels(context.Background()); len(got) != 1 || got[0].ID != "a-fast" || !got[0].Starting {
		t.Errorf("during the load: %+v", got)
	}
	load.Open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, _ := b.LoadedModels(context.Background()); len(got) != 1 || got[0].ID != "a" || got[0].Starting {
		t.Errorf("after the load: %+v", got)
	}
}

// While LocalAI limits its loaded models, a load the gateway started makes
// it busy, since any request for a model that isn't loaded would evict the
// load: with a limit, and when it has no /api/settings to say; without a
// limit the load shows as starting, as on any backend.
func TestLocalAI_IsBusyDuringALoadWithALimit(t *testing.T) {
	for _, c := range []struct {
		name     string
		start    func(t *testing.T) (*testkit.LocalAI, string)
		wantBusy bool
	}{
		{"limit", func(t *testing.T) (*testkit.LocalAI, string) { f := testkit.NewLocalAI(t, 1, "a"); return f, f.URL }, true},
		{"no limit", func(t *testing.T) (*testkit.LocalAI, string) { f := testkit.NewLocalAI(t, 0, "a"); return f, f.URL }, false},
		{"no settings", func(t *testing.T) (*testkit.LocalAI, string) {
			f := testkit.NewLocalAI(t, 0, "a")
			return f, withoutSettings(t, f)
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, url := c.start(t)
			b := newBackend(t, url, "")
			if got, err := b.LoadedModels(context.Background()); err != nil || len(got) != 0 {
				t.Fatalf("before the load: %+v, %v", got, err)
			}
			load := f.HoldLoads()
			done := make(chan error)
			go func() {
				_, err := b.Chat(context.Background(), backend.ChatRequest{Protocol: backend.OpenAIChat, Body: []byte(`{"model":"a"}`)})
				done <- err
			}()
			f.WaitFor(t, "POST /v1/chat/completions", 1)
			got, err := b.LoadedModels(context.Background())
			if busy := errors.Is(err, backend.ErrLoading); busy != c.wantBusy || !busy && (len(got) != 1 || !got[0].Starting) {
				t.Errorf("during the load: %+v, %v; want busy %v", got, err, c.wantBusy)
			}
			load.Open()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got, err := b.LoadedModels(context.Background()); err != nil || len(got) != 1 || got[0].Starting {
				t.Errorf("after the load: %+v, %v", got, err)
			}
		})
	}
}

// withoutSettings serves f without /api/settings, as LocalAI does with its
// web UI off.
func withoutSettings(t *testing.T, f *testkit.LocalAI) string {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/settings" {
			http.NotFound(w, r)
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func TestLocalAI_RejectsAWrongConfig(t *testing.T) {
	for _, src := range []string{"{type: localAi}", "{type: localAi, baseUrl: x, maxActiveBackends: 1}"} {
		if _, err := New(testkit.Entry(t, "localAi", src)); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}
