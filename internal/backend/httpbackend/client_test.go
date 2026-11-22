package httpbackend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/protocols"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

func newClient(t *testing.T, c Config) *Client {
	t.Helper()
	client, err := NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestClient_ChatPostsToTheProtocolPath(t *testing.T) {
	f := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
	f.SetState("m", "ready")
	c := newClient(t, Config{BaseURL: f.URL})
	for _, protocol := range c.Protocols() {
		if _, err := c.Chat(context.Background(), backend.ChatRequest{Protocol: protocol, Body: []byte(`{"model":"m"}`)}); err != nil {
			t.Fatal(err)
		}
		if req, ok := f.Last("POST " + protocols.Of(protocol).Path); !ok || req.Body != `{"model":"m"}` {
			t.Errorf("%s: got %+v", protocol, req)
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
	res, err := newClient(t, Config{BaseURL: f.URL}).Chat(context.Background(), backend.ChatRequest{Protocol: backend.OpenAIChat, Body: []byte(`{"model":"m"}`)})
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
	res, err := newClient(t, Config{BaseURL: f.URL}).Chat(context.Background(), backend.ChatRequest{Protocol: backend.OpenAIChat, Body: []byte(`{"model":"m"}`), Stream: true})
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

// With trackPending, a chat counts as pending, under its alias's base model,
// until the response headers arrive; then the cache is dropped, since the
// state it holds predates the load.
func TestClient_ChatCountsAsPendingUntilHeaders(t *testing.T) {
	f := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "base", Aliases: []string{"base:alias"}})
	load := f.HoldLoads()
	c := newClient(t, Config{BaseURL: f.URL})
	state := newFetcher()
	s := NewStateCache(Config{StateCacheSeconds: 60}, state.fetch)
	c.trackPending(s)
	s.LoadedModels(context.Background())
	done := make(chan error)
	go func() {
		_, err := c.Chat(context.Background(), backend.ChatRequest{Protocol: backend.OpenAIChat, Body: []byte(`{"model":"base:alias"}`), AliasOf: "base"})
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

// A state call answered with another status than 200 fails with a
// StatusError carrying the code, so an adapter can read a status as state.
func TestClient_StateCallErrorsCarryTheStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"Loading model"}}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	err := newClient(t, Config{BaseURL: srv.URL}).GetJSON(context.Background(), "/props", &struct{}{})
	var status *StatusError
	if !errors.As(err, &status) || status.Code != http.StatusServiceUnavailable || err.Error() != "GET /props: 503 Service Unavailable" {
		t.Errorf("got %v", err)
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
			client.Chat(context.Background(), backend.ChatRequest{Protocol: backend.OpenAIChat, Body: []byte(`{"model":"m"}`), Client: backend.Client{Authorization: c.client}})
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

// With no key configured anywhere, no apiKey, passthroughAuth or keyMap, and
// a caller that sent none, neither a chat nor a state call carries an
// Authorization header at all, not even an empty one.
func TestClient_SendsNoKeyWhenNoneIsConfigured(t *testing.T) {
	f := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
	f.SetState("m", "ready")
	client := newClient(t, Config{BaseURL: f.URL})
	client.Chat(context.Background(), backend.ChatRequest{Protocol: backend.OpenAIChat, Body: []byte(`{"model":"m"}`), Client: backend.Client{Groups: []string{"unknown"}}})
	client.GetJSON(context.Background(), "/running", &struct{}{})
	chat, _ := f.Last("POST /v1/chat/completions")
	state, _ := f.Last("GET /running")
	if got := chat.Header.Values("Authorization"); len(got) != 0 {
		t.Errorf("chat: got %q", got)
	}
	if got := state.Header.Values("Authorization"); len(got) != 0 {
		t.Errorf("state: got %q", got)
	}
}

// keyMap gives a caller a key of its own on chat requests: the entry for its
// key's name first, then the first, in keyMap order, for one of its groups,
// whatever order the caller's groups are in. A caller it doesn't name gets
// what it would without keyMap, passthroughAuth's key or apiKey, and the
// gateway's own calls keep the apiKey.
func TestClient_KeyMap(t *testing.T) {
	keyMap := config.KeyMap{{Name: "alice", Key: "alices-backend-key"}, {Name: "family", Key: "family-backend-key"}, {Name: "adults", Key: "adults-backend-key"}}
	cases := []struct {
		name        string
		passthrough bool
		client      backend.Client
		chat        string
	}{
		{"by name", false, backend.Client{Name: "alice", Groups: []string{"all", "family"}}, "Bearer alices-backend-key"},
		{"by name over passthrough", true, backend.Client{Name: "alice", Groups: []string{"all", "family"}, Authorization: "Bearer client"}, "Bearer alices-backend-key"},
		{"by group", false, backend.Client{Name: "bob", Groups: []string{"all", "family"}}, "Bearer family-backend-key"},
		{"by the first of its groups in keyMap", false, backend.Client{Name: "carol", Groups: []string{"all", "adults", "family"}}, "Bearer family-backend-key"},
		{"by name over its groups", false, backend.Client{Name: "alice", Groups: []string{"all", "adults", "family"}}, "Bearer alices-backend-key"},
		{"unnamed, apiKey", false, backend.Client{Name: "agent", Groups: []string{"all"}}, "Bearer svc"},
		{"unnamed, passthrough", true, backend.Client{Name: "agent", Groups: []string{"all"}, Authorization: "Bearer client"}, "Bearer client"},
		{"unknown caller", true, backend.Client{Name: "unknown", Groups: []string{"unknown"}, Authorization: "Bearer client"}, "Bearer client"},
		{"keyless caller", false, backend.Client{Groups: []string{"unknown"}}, "Bearer svc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := testkit.NewLlamaSwap(t, testkit.SwapModel{ID: "m"})
			f.SetState("m", "ready")
			client := newClient(t, Config{BaseURL: f.URL, APIKey: "svc", PassthroughAuth: c.passthrough, KeyMap: keyMap})
			client.Chat(context.Background(), backend.ChatRequest{Protocol: backend.OpenAIChat, Body: []byte(`{"model":"m"}`), Client: c.client})
			client.GetJSON(context.Background(), "/running", &struct{}{})
			chat, _ := f.Last("POST /v1/chat/completions")
			state, _ := f.Last("GET /running")
			if got := chat.Header.Get("Authorization"); got != c.chat {
				t.Errorf("chat: got %q, want %q", got, c.chat)
			}
			if got := state.Header.Get("Authorization"); got != "Bearer svc" {
				t.Errorf("state: got %q", got)
			}
		})
	}
}

// A keyMap entry that is empty, such as a variable set to nothing, would send an
// empty key, so it is refused.
func TestClient_RefusesAnEmptyKeyMapEntry(t *testing.T) {
	if _, err := NewClient(Config{BaseURL: "http://x", KeyMap: config.KeyMap{{Name: "alice", Key: ""}}}); err == nil || err.Error() != "keyMap.alice is empty" {
		t.Errorf("got %v", err)
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

// A stream the backend stops sending on, without closing it, ends with a
// *StallError once stallTimeoutSeconds pass, and the request is cancelled.
func TestClient_StalledStreamEnds(t *testing.T) {
	stalled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"n\":1}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stalled)
	}))
	defer srv.Close()
	timeout := 0.05
	c := newClient(t, Config{BaseURL: srv.URL, StallTimeoutSeconds: &timeout})
	res, err := c.Chat(context.Background(), backend.ChatRequest{Protocol: backend.OpenAIChat, Body: []byte(`{"model":"m"}`), Stream: true})
	if err != nil || res.Stream == nil {
		t.Fatalf("got %+v, %v", res, err)
	}
	defer res.Stream.Close()
	if data, err := res.Stream.Next(); err != nil || string(data) != `{"n":1}` {
		t.Fatalf("first event: %s, %v", data, err)
	}
	var stall *StallError
	if _, err := res.Stream.Next(); !errors.As(err, &stall) || err.Error() != "sent nothing for 0.05s, the backend's stallTimeoutSeconds; the response was ended" {
		t.Errorf("got %v", err)
	}
	select {
	case <-stalled:
	case <-time.After(testkit.Timeout):
		t.Error("the backend's request wasn't cancelled")
	}
}

// Only time spent waiting on the backend counts toward the stall timeout,
// not time the reader takes between reads.
func TestStallBody_CountsOnlyTimeInRead(t *testing.T) {
	var cancelled atomic.Bool
	b := newStallBody(io.NopCloser(strings.NewReader("ab")), 10*time.Millisecond, func() { cancelled.Store(true) })
	defer b.Close()
	p := make([]byte, 1)
	for range 2 {
		time.Sleep(30 * time.Millisecond)
		if _, err := b.Read(p); err != nil {
			t.Fatal(err)
		}
	}
	if cancelled.Load() {
		t.Error("the request was cancelled")
	}
}

// stallTimeoutSeconds defaults to 600; 0 sets no limit, and a negative
// value is refused.
func TestClient_StallTimeout(t *testing.T) {
	zero, negative := 0.0, -1.0
	if got := newClient(t, Config{BaseURL: "http://x"}).StallTimeout(); got != 600*time.Second {
		t.Errorf("default: got %v", got)
	}
	if got := newClient(t, Config{BaseURL: "http://x", StallTimeoutSeconds: &zero}).StallTimeout(); got != 0 {
		t.Errorf("0: got %v", got)
	}
	if _, err := NewClient(Config{BaseURL: "http://x", StallTimeoutSeconds: &negative}); err == nil {
		t.Error("negative: no error")
	}
}
