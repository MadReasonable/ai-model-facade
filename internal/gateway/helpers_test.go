package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadReasonable/ai-model-facade/internal/config"
	"github.com/MadReasonable/ai-model-facade/internal/testkit"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// newSwap starts the fake llama-swap most tests use: a 35B model with a
// :Slacker preset, a 27B model with a :Dreamer preset, and a small coder.
func newSwap(t *testing.T) *testkit.LlamaSwap {
	return testkit.NewLlamaSwap(t,
		testkit.SwapModel{ID: "qwen3.6-35b-a3b:q8_0", Aliases: []string{"qwen3.6-35b:Slacker"}},
		testkit.SwapModel{ID: "qwen3.8-27b:q6_k", Aliases: []string{"qwen3.8-27b:Dreamer"}},
		testkit.SwapModel{ID: "qwen2.5-coder-3b:q8_0"},
	)
}

// taskConfig is the config most tests use: one API key, and a `task` facade
// on llama-swap that prefers the 35B model, then any 27B quant, both with
// thinking off, and loads the coder if nothing else is up.
const taskConfig = `
server:
  listen: :0
  apiKeys:
    - {name: openWebUi, key: secret}
logging: {logSelection: true}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}"}
models:
  task:
    backend: llamaSwap
    description: Background tasks
    params: {max_tokens: 1000, think: null}
    candidates:
      - model: qwen3.6-35b-a3b:q8_0
        params: {chat_template_kwargs: {enable_thinking: false}}
      - model: qwen3.8-27b:*
        params: {chat_template_kwargs: {enable_thinking: false}}
      - model: qwen2.5-coder-3b:q8_0
        loadIfNone: true
`

// edit returns src with each old replaced by its new, failing t if one
// doesn't occur, so a test can't silently run the unedited config.
func edit(t *testing.T, src string, oldNew ...string) string {
	t.Helper()
	for i := 0; i+1 < len(oldNew); i += 2 {
		if !strings.Contains(src, oldNew[i]) {
			t.Fatalf("config edit: %q not found", oldNew[i])
		}
		src = strings.Replace(src, oldNew[i], oldNew[i+1], 1)
	}
	return src
}

// parseConfig parses src with ${NAME} replaced from vars.
func parseConfig(t *testing.T, src string, vars map[string]string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(src), func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// parseConfigErr parses src with every ${NAME} set, for tests of what the
// config itself gets wrong.
func parseConfigErr(src string) (*config.Config, error) {
	return config.Parse([]byte(src), func(string) (string, bool) { return "http://x", true })
}

// gateway is a built gateway serving HTTP, with what it logged.
type gateway struct {
	*httptest.Server
	g       *Gateway
	handler http.Handler
	logs    *syncBuffer
}

// startGateway builds a gateway from src, with ${NAME} replaced from vars,
// and serves it until t ends.
func startGateway(t *testing.T, src string, vars map[string]string) *gateway {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	g, err := Build(context.Background(), parseConfig(t, src, vars), log)
	if err != nil {
		t.Fatal(err)
	}
	handler := g.Handler(log)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &gateway{Server: srv, g: g, handler: handler, logs: logs}
}

// startTask starts the task config on swap, and forgets the requests the
// gateway made while starting.
func startTask(t *testing.T, swap *testkit.LlamaSwap, edits ...string) *gateway {
	t.Helper()
	gw := startGateway(t, edit(t, taskConfig, edits...), map[string]string{"SWAP_URL": swap.URL})
	swap.Reset()
	return gw
}

// response is a finished HTTP response with its body read.
type response struct {
	*http.Response
	body string
}

// do sends a request with an optional bearer key.
func (gw *gateway) do(t *testing.T, method, path, key, body string) response {
	t.Helper()
	req, _ := http.NewRequest(method, gw.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{resp, string(b)}
}

// direct is do without a network connection, calling the handler itself.
// Over a connection, Go's server waits 500 ms before closing one whose
// request body it didn't read, which a refused oversized body always is.
func (gw *gateway) direct(t *testing.T, method, path, key, body string) response {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	gw.handler.ServeHTTP(rec, req)
	return response{rec.Result(), rec.Body.String()}
}

// chat posts body to /v1/chat/completions with the test key.
func (gw *gateway) chat(t *testing.T, body string) response {
	t.Helper()
	return gw.do(t, http.MethodPost, "/v1/chat/completions", "secret", body)
}

// get sends a GET with the test key.
func (gw *gateway) get(t *testing.T, path string) response {
	t.Helper()
	return gw.do(t, http.MethodGet, path, "secret", "")
}

// target is the model the gateway chose for a response.
func (r response) target() string {
	return r.Header.Get("X-Gateway-Target")
}

// syncBuffer is a log destination that background goroutines and the test
// can use at the same time, and that a test can wait on.
type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.changed != nil {
		close(b.changed)
		b.changed = nil
	}
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor waits until the log contains s, and fails t after testkit.Timeout.
func (b *syncBuffer) waitFor(t *testing.T, s string) {
	t.Helper()
	deadline := time.After(testkit.Timeout)
	for {
		b.mu.Lock()
		found := strings.Contains(b.buf.String(), s)
		if b.changed == nil {
			b.changed = make(chan struct{})
		}
		changed := b.changed
		b.mu.Unlock()
		if found {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("log never contained %q:\n%s", s, b.String())
		}
	}
}
