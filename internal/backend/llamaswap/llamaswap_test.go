package llamaswap

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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
	b, err := New(config.Entry{Name: "llamaSwap", Type: "llamaSwap", Node: doc.Content[0]})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLoadedModelsOnlyReadyAndCached(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("unexpected request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/running":
			calls.Add(1)
			io.WriteString(w, `{"running":[{"model":"a","state":"ready"},{"model":"b","state":"starting"},{"model":"c","state":"ready","cmd":"x"},{"model":"d","state":"stopping"}]}`)
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"a","meta":{"llamaswap":{"type":"model","aliases":["a:Slacker"]}}},{"id":"a:Slacker","meta":{"llamaswap":{"type":"alias","modelID":"a"}}},{"id":"b"},{"id":"c"}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	b := newBackend(t, "{type: llamaSwap, baseUrl: \""+srv.URL+"/\", apiKey: k, stateCacheSeconds: 60}")
	for range 3 {
		got, err := b.LoadedModels(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Ready and starting models are returned, starting ones marked; stopping ones are not.
		if len(got) != 3 || got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
			t.Fatalf("got %+v", got)
		}
		if got[0].Starting || !got[1].Starting || got[2].Starting {
			t.Errorf("starting flags: %+v", got)
		}
		if len(got[0].Aliases) != 1 || got[0].Aliases[0] != "a:Slacker" || len(got[2].Aliases) != 0 {
			t.Errorf("aliases: %+v", got)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("expected one /running call, got %d", calls.Load())
	}
}

// A request the gateway sent shows its model as starting even while the
// cached /running predates the load, under the base model when the request
// names an alias; once headers arrive the cache is dropped and the model
// shows as ready.
func TestPendingModelIsStarting(t *testing.T) {
	var loaded atomic.Bool
	var runningCalls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/running":
			runningCalls.Add(1)
			if loaded.Load() {
				io.WriteString(w, `{"running":[{"model":"a","state":"ready"}]}`)
				return
			}
			io.WriteString(w, `{"running":[]}`)
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"a","meta":{"llamaswap":{"type":"model","aliases":["a:Slacker"]}}},{"id":"a:Slacker","meta":{"llamaswap":{"type":"alias","modelID":"a"}}}]}`)
		case "/v1/chat/completions":
			<-release // the load
			loaded.Store(true)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	b := newBackend(t, "{type: llamaSwap, baseUrl: \""+srv.URL+"\", stateCacheSeconds: 60}")
	if got, _ := b.LoadedModels(context.Background()); len(got) != 0 {
		t.Fatalf("before: %+v", got)
	}
	done := make(chan error)
	go func() {
		_, err := b.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"a:Slacker"}`), AliasOf: "a"})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the request reach the backend
	got, _ := b.LoadedModels(context.Background())
	if len(got) != 1 || got[0].ID != "a" || !got[0].Starting {
		t.Errorf("during load: %+v", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ = b.LoadedModels(context.Background())
	if len(got) != 1 || got[0].ID != "a" || got[0].Starting {
		t.Errorf("after load: %+v", got)
	}
	if runningCalls.Load() != 2 {
		t.Errorf("expected the cache to be refetched once, got %d /running calls", runningCalls.Load())
	}
}

func TestNoCacheWithoutStateCacheSeconds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/running" {
			calls.Add(1)
		}
		io.WriteString(w, `{"running":[],"data":[]}`)
	}))
	defer srv.Close()
	b := newBackend(t, "{type: llamaSwap, baseUrl: \""+srv.URL+"\"}")
	b.LoadedModels(context.Background())
	b.LoadedModels(context.Background())
	if calls.Load() != 2 {
		t.Errorf("got %d calls", calls.Load())
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"a","object":"model"},{"id":"b","meta":{"llamaswap":{"type":"model","aliases":["b:Slacker","b:Thinker"]}}},{"id":"b:Slacker","meta":{"llamaswap":{"type":"alias","modelID":"b"}}},{"id":"b:Thinker","meta":{"llamaswap":{"type":"alias","modelID":"b"}}}]}`)
	}))
	defer srv.Close()
	got, err := newBackend(t, "{type: llamaSwap, baseUrl: \""+srv.URL+"\"}").ListModels(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("got %+v, %v", got, err)
	}
	// Alias entries fold into their base model instead of being listed.
	if len(got[0].Aliases) != 0 || len(got[1].Aliases) != 2 || got[1].Aliases[1] != "b:Thinker" {
		t.Errorf("aliases: %+v", got)
	}
}

func TestChatPassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" || string(body) != `{"model":"a"}` {
			t.Errorf("got %s %s", r.URL.Path, body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, `{"error":"x"}`)
	}))
	defer srv.Close()
	res, err := newBackend(t, "{type: llamaSwap, baseUrl: \""+srv.URL+"\"}").Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"a"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stream != nil || res.Response.Status != http.StatusTeapot || string(res.Response.Body) != `{"error":"x"}` || res.Response.ContentType != "application/json" {
		t.Errorf("got %+v", res.Response)
	}
}

func TestChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		io.WriteString(w, "data: {\"n\":1}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	res, err := newBackend(t, "{type: llamaSwap, baseUrl: \""+srv.URL+"\"}").Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{}`), Stream: true})
	if err != nil || res.Stream == nil {
		t.Fatalf("got %+v, %v", res, err)
	}
	defer res.Stream.Close()
	data, err := res.Stream.Next()
	if err != nil || string(data) != `{"n":1}` {
		t.Errorf("got %s, %v", data, err)
	}
	if _, err := res.Stream.Next(); err != io.EOF {
		t.Errorf("got %v", err)
	}
}

// Concurrent requests share one /running fetch instead of queueing their own.
func TestLoadedModelsSingleFlight(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[]}`)
			return
		}
		calls.Add(1)
		<-release
		io.WriteString(w, `{"running":[{"model":"a","state":"ready"}]}`)
	}))
	defer srv.Close()
	b := newBackend(t, "{type: llamaSwap, baseUrl: \""+srv.URL+"\", stateCacheSeconds: 60}")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = b.LoadedModels(context.Background())
		}()
	}
	time.Sleep(50 * time.Millisecond) // let the goroutines queue on the fetch
	close(release)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("expected one /running fetch, got %d", calls.Load())
	}
}

// A slow /running is bounded by stateTimeoutSeconds, not the chat timeout.
func TestLoadedModelsStateTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	b := newBackend(t, "{type: llamaSwap, baseUrl: \""+srv.URL+"\", stateTimeoutSeconds: 0.1, requestTimeoutSeconds: 180}")
	start := time.Now()
	_, err := b.LoadedModels(context.Background())
	if err == nil || time.Since(start) > time.Second {
		t.Errorf("got %v after %v", err, time.Since(start))
	}
}

// A caller whose context ends stops waiting; the shared fetch carries on for
// the others.
func TestLoadedModelsCallerCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	b := newBackend(t, "{type: llamaSwap, baseUrl: \""+srv.URL+"\", stateTimeoutSeconds: 1}")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := b.LoadedModels(ctx)
	if err != context.DeadlineExceeded || time.Since(start) > 500*time.Millisecond {
		t.Errorf("got %v after %v", err, time.Since(start))
	}
}

func TestLegacyCompletionsPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			t.Errorf("got path %s", r.URL.Path)
		}
		io.WriteString(w, `{"choices":[{"text":"ok"}]}`)
	}))
	defer srv.Close()
	res, err := newBackend(t, "{type: llamaSwap, baseUrl: \""+srv.URL+"\"}").Chat(context.Background(), backend.ChatRequest{Endpoint: backend.Completions, Body: []byte(`{"model":"a","prompt":"x"}`)})
	if err != nil || string(res.Response.Body) != `{"choices":[{"text":"ok"}]}` {
		t.Errorf("got %+v, %v", res.Response, err)
	}
}

func TestConfigErrors(t *testing.T) {
	for _, src := range []string{"{type: llamaSwap}", "{type: llamaSwap, baseUrl: x, stateCache: 2}"} {
		var doc yaml.Node
		yaml.Unmarshal([]byte(src), &doc)
		if _, err := New(config.Entry{Node: doc.Content[0]}); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}
