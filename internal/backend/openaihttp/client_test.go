package openaihttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

func newClient(t *testing.T, c Config) *Client {
	t.Helper()
	client, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestClient_ChatPostsToTheEndpointPath(t *testing.T) {
	f := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
	f.SetState("m", "ready")
	c := newClient(t, Config{BaseURL: f.URL})
	for _, endpoint := range []backend.Endpoint{backend.ChatCompletions, backend.Completions} {
		if _, err := c.Chat(context.Background(), backend.ChatRequest{Endpoint: endpoint, Body: []byte(`{"model":"m"}`)}); err != nil {
			t.Fatal(err)
		}
		if req, ok := f.Last("POST " + string(endpoint)); !ok || req.Body != `{"model":"m"}` {
			t.Errorf("%s: got %+v", endpoint, req)
		}
	}
}

// A baseUrl ending in a slash doesn't double the slash in request paths.
func TestClient_TrimsTheBaseURLsTrailingSlash(t *testing.T) {
	f := testkit.NewLlamaSwap(t)
	newClient(t, Config{BaseURL: f.URL + "/"}).GetJSON(context.Background(), "/running", &struct{}{})
	if f.Count("GET /running") != 1 {
		t.Errorf("got %v", f.Keys())
	}
}

// A response is passed on with its status, content type and body, and of
// its other headers only Retry-After.
func TestClient_ChatPassesTheResponseOn(t *testing.T) {
	f := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
	f.SetState("m", "ready")
	f.SetReply(testkit.Reply{Status: http.StatusTooManyRequests, Header: map[string]string{"Retry-After": "7", "Server": "llama-swap"}, Body: `{"error":"busy"}`})
	res, err := newClient(t, Config{BaseURL: f.URL}).Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatal(err)
	}
	want := backend.ChatResponse{Status: http.StatusTooManyRequests, ContentType: "application/json", Body: []byte(`{"error":"busy"}`), RetryAfter: "7"}
	if r := res.Response; res.Stream != nil || r.Status != want.Status || r.ContentType != want.ContentType || string(r.Body) != string(want.Body) || r.RetryAfter != want.RetryAfter {
		t.Errorf("got %+v", res.Response)
	}
}

func TestClient_ChatRelaysAStream(t *testing.T) {
	f := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
	f.SetState("m", "ready")
	f.SetReply(testkit.Reply{Stream: []string{`{"n":1}`}})
	res, err := newClient(t, Config{BaseURL: f.URL}).Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"m"}`), Stream: true})
	if err != nil || res.Stream == nil {
		t.Fatalf("got %+v, %v", res, err)
	}
	defer res.Stream.Close()
	if data, err := res.Stream.Next(); err != nil || string(data) != `{"n":1}` {
		t.Errorf("first event: %s, %v", data, err)
	}
	if _, err := res.Stream.Next(); err != io.EOF {
		t.Errorf("after [DONE]: %v", err)
	}
}

// With TrackPending, a chat counts as pending, under its alias's base model,
// until the response headers arrive; then the cache is dropped, since the
// state it holds predates the load.
func TestClient_ChatCountsAsPendingUntilHeaders(t *testing.T) {
	f := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "base", Aliases: []string{"base:alias"}})
	load := f.HoldLoads()
	c := newClient(t, Config{BaseURL: f.URL})
	state := newFetcher()
	s := NewStateCache(Config{StateCacheSeconds: 60}, state.fetch)
	c.TrackPending(s)
	s.LoadedModels(context.Background())
	done := make(chan error)
	go func() {
		_, err := c.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"base:alias"}`), AliasOf: "base"})
		done <- err
	}()
	f.WaitFor(t, "POST /v1/chat/completions", 1)
	if got, _ := s.LoadedModels(context.Background()); len(got) != 1 || got[0].ID != "base" || !got[0].Starting {
		t.Errorf("during the load: %+v", got)
	}
	load.Open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LoadedModels(context.Background()); len(got) != 0 || state.calls.Load() != 2 {
		t.Errorf("after the headers: %+v after %d fetches, want none after 2", got, state.calls.Load())
	}
}

// State and model-list calls are bounded by stateTimeoutSeconds, not by the
// much longer requestTimeoutSeconds meant for a model load.
func TestClient_StateCallsHaveTheirOwnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	c := newClient(t, Config{BaseURL: srv.URL, StateTimeoutSeconds: 0.05, RequestTimeoutSeconds: 180})
	start := time.Now()
	if err := c.GetJSON(context.Background(), "/running", &struct{}{}); err == nil || time.Since(start) > time.Second {
		t.Errorf("got %v after %v", err, time.Since(start))
	}
}

// The backend's apiKey goes on every call, unless passthroughAuth puts the
// client's own key, or none, on chat requests; the gateway's own calls keep
// the apiKey either way.
func TestClient_Auth(t *testing.T) {
	cases := []struct {
		name        string
		passthrough bool
		client      string
		chat, state string
	}{
		{"apiKey", false, "Bearer client", "Bearer svc", "Bearer svc"},
		{"passthrough", true, "Bearer client", "Bearer client", "Bearer svc"},
		{"passthrough without a client key", true, "", "", "Bearer svc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
			f.SetState("m", "ready")
			client := newClient(t, Config{BaseURL: f.URL, APIKey: "svc", PassthroughAuth: c.passthrough})
			client.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(`{"model":"m"}`), Client: backend.Client{Authorization: c.client}})
			client.GetJSON(context.Background(), "/running", &struct{}{})
			chat, _ := f.Last("POST /v1/chat/completions")
			state, _ := f.Last("GET /running")
			if got := chat.Header.Get("Authorization"); got != c.chat {
				t.Errorf("chat: got %q, want %q", got, c.chat)
			}
			if got := state.Header.Get("Authorization"); got != c.state {
				t.Errorf("state: got %q, want %q", got, c.state)
			}
		})
	}
}

// Errors name the URL in single quotes, not Go's double quotes, which logs
// and JSON bodies would escape; the cause stays inspectable.
func TestClient_ErrorsQuoteTheURLWithSingleQuotes(t *testing.T) {
	c := newClient(t, Config{BaseURL: "http://127.0.0.1:1", StateTimeoutSeconds: 1})
	err := c.GetJSON(context.Background(), "/v1/models", &struct{}{})
	if err == nil || !strings.HasPrefix(err.Error(), "GET 'http://127.0.0.1:1/v1/models': ") || strings.Contains(err.Error(), `"`) {
		t.Errorf("got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Do(ctx, http.MethodPost, "/v1/chat/completions", []byte(`{}`), "", backend.Client{})
	if !errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "POST '") {
		t.Errorf("cancelled: got %v", err)
	}
}
