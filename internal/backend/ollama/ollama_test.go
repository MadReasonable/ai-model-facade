package ollama

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

func newBackend(t *testing.T, src string) *Backend {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	b, err := New(config.Entry{Name: "ollama", Type: "ollama", Node: doc.Content[0]})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		io.WriteString(w, `{"object":"list","data":[{"id":"llama3:latest","object":"model","created":1,"owned_by":"library"},{"id":"qwen2.5:7b","object":"model"}]}`)
	}))
	defer srv.Close()
	got, err := newBackend(t, "{type: ollama, baseUrl: \""+srv.URL+"\"}").ListModels(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "llama3:latest" || got[1].ID != "qwen2.5:7b" || len(got[0].Aliases) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// Every model /api/ps lists counts as ready: it has no load state.
func TestLoadedModelsFromPsAndCached(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("unexpected request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/ps" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		calls.Add(1)
		io.WriteString(w, `{"models":[{"name":"llama3:latest","model":"llama3:latest","size":5137025024,"expires_at":"2026-10-22T10:00:00Z","size_vram":5137025024},{"name":"qwen2.5:7b","model":"qwen2.5:7b"}]}`)
	}))
	defer srv.Close()
	b := newBackend(t, "{type: ollama, baseUrl: \""+srv.URL+"/\", apiKey: k, stateCacheSeconds: 60}")
	for range 3 {
		got, err := b.LoadedModels(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].ID != "llama3:latest" || got[1].ID != "qwen2.5:7b" || got[0].Starting || got[1].Starting {
			t.Fatalf("got %+v", got)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("expected one /api/ps call, got %d", calls.Load())
	}
}

func TestLoadedModelsNoneLoaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"models":[]}`)
	}))
	defer srv.Close()
	got, err := newBackend(t, "{type: ollama, baseUrl: \""+srv.URL+"\"}").LoadedModels(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("got %+v, %v", got, err)
	}
}

// A model with a request awaiting headers is starting until /api/ps lists it;
// once headers arrive the cached state is dropped and the model shows as ready.
func TestPendingModelIsStarting(t *testing.T) {
	var loaded atomic.Bool
	var psCalls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			psCalls.Add(1)
			if loaded.Load() {
				io.WriteString(w, `{"models":[{"name":"a:1"}]}`)
				return
			}
			io.WriteString(w, `{"models":[]}`)
		case "/v1/chat/completions":
			<-release // the load
			loaded.Store(true)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	b := newBackend(t, "{type: ollama, baseUrl: \""+srv.URL+"\", stateCacheSeconds: 60}")
	if got, _ := b.LoadedModels(context.Background()); len(got) != 0 {
		t.Fatalf("before: %+v", got)
	}
	done := make(chan error)
	go func() {
		_, err := b.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"a:1"}`)})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the request reach the backend
	got, _ := b.LoadedModels(context.Background())
	if len(got) != 1 || got[0].ID != "a:1" || !got[0].Starting {
		t.Errorf("during load: %+v", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ = b.LoadedModels(context.Background())
	if len(got) != 1 || got[0].ID != "a:1" || got[0].Starting {
		t.Errorf("after load: %+v", got)
	}
	if psCalls.Load() != 2 {
		t.Errorf("expected the cache to be refetched once, got %d /api/ps calls", psCalls.Load())
	}
}

// A request to a listed model leaves the cache alone.
func TestListedModelKeepsCache(t *testing.T) {
	var psCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			psCalls.Add(1)
			io.WriteString(w, `{"models":[{"name":"a:1"}]}`)
			return
		}
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	b := newBackend(t, "{type: ollama, baseUrl: \""+srv.URL+"\", stateCacheSeconds: 60}")
	b.LoadedModels(context.Background())
	b.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"a:1"}`)})
	b.LoadedModels(context.Background())
	if psCalls.Load() != 1 {
		t.Errorf("got %d /api/ps calls", psCalls.Load())
	}
}

func TestChatPaths(t *testing.T) {
	for _, endpoint := range []backend.Endpoint{backend.ChatCompletions, backend.Completions} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if r.URL.Path != string(endpoint) || string(body) != `{"model":"llama3:latest"}` {
				t.Errorf("got %s %s", r.URL.Path, body)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[]}`)
		}))
		res, err := newBackend(t, "{type: ollama, baseUrl: \""+srv.URL+"\"}").Chat(context.Background(), backend.ChatRequest{Endpoint: endpoint, Body: []byte(`{"model":"llama3:latest"}`)})
		if err != nil || res.Response == nil || string(res.Response.Body) != `{"choices":[]}` {
			t.Errorf("%s: got %+v, %v", endpoint, res, err)
		}
		srv.Close()
	}
}

func TestConfigErrors(t *testing.T) {
	for _, src := range []string{"{type: ollama}", "{type: ollama, baseUrl: x, keepAlive: 5m}"} {
		var doc yaml.Node
		yaml.Unmarshal([]byte(src), &doc)
		if _, err := New(config.Entry{Node: doc.Content[0]}); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}
