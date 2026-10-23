package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// fakeSwap is a minimal llama-swap: /v1/models, /running and chat.
type fakeSwap struct {
	mu          sync.Mutex
	loaded      []string
	starting    []string // reported in state "starting"
	lastBody    string
	lastPath    string // path of the last completion request
	chatMode    string // "json", "stream" or "truncated"
	chatAuth    string // Authorization header of the last chat request
	stateAuth   string // Authorization header of the last /running request
	failRunning bool   // answer /running with 500
}

func (f *fakeSwap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/v1/models":
		io.WriteString(w, `{"data":[`+
			`{"id":"qwen3.6-35b-a3b:q8_0","meta":{"llamaswap":{"type":"model","aliases":["qwen3.6-35b:Slacker"]}}},`+
			`{"id":"qwen3.6-35b:Slacker","meta":{"llamaswap":{"type":"alias","modelID":"qwen3.6-35b-a3b:q8_0"}}},`+
			`{"id":"qwen3.8-27b:q6_k","meta":{"llamaswap":{"type":"model","aliases":["qwen3.8-27b:Dreamer"]}}},`+
			`{"id":"qwen3.8-27b:Dreamer","meta":{"llamaswap":{"type":"alias","modelID":"qwen3.8-27b:q6_k"}}},`+
			`{"id":"qwen2.5-coder-3b:q8_0"}]}`)
	case "/running":
		f.stateAuth = r.Header.Get("Authorization")
		if f.failRunning {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var running []string
		for _, id := range f.loaded {
			running = append(running, `{"model":"`+id+`","state":"ready"}`)
		}
		for _, id := range f.starting {
			running = append(running, `{"model":"`+id+`","state":"starting"}`)
		}
		io.WriteString(w, `{"running":[`+strings.Join(running, ",")+`]}`)
	case "/v1/chat/completions", "/v1/completions":
		f.lastPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		f.lastBody = string(body)
		f.chatAuth = r.Header.Get("Authorization")
		switch f.chatMode {
		case "stream", "truncated", "errorEvent":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			if f.chatMode == "stream" {
				io.WriteString(w, "data: [DONE]\n\n")
			}
			if f.chatMode == "errorEvent" {
				io.WriteString(w, "event: error\ndata: {\"error\":{\"message\":\"context full\",\"type\":\"server_error\"}}\n\n")
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[{"message":{"content":"Title"}}],"x_extra":1}`)
		}
	default:
		http.NotFound(w, r)
	}
}

const testConfig = `
server:
  listen: :0
  apiKeys:
    - {name: openWebUi, key: "${GATEWAY_KEY}"}
logging: {logSelection: true}
vars:
  noThinking: &noThinking
    chat_template_kwargs: { enable_thinking: false }
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}"}
models:
  task:
    backend: llamaSwap
    description: Background tasks
    params: {max_tokens: 1000, think: null}
    candidates:
      - model: qwen3.6-35b-a3b:q8_0
        params: *noThinking
      - model: qwen3.8-27b:*
        params: *noThinking
      - model: qwen2.5-coder-3b:q8_0
        loadIfNone: true
`

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func newGateway(t *testing.T, swap *fakeSwap) *httptest.Server {
	t.Helper()
	srv, _ := buildGateway(t, swap, testConfig, discard)
	return srv
}

func buildGateway(t *testing.T, swap *fakeSwap, configSrc string, log *slog.Logger) (*httptest.Server, *Gateway) {
	t.Helper()
	backendSrv := httptest.NewServer(swap)
	t.Cleanup(backendSrv.Close)
	vars := map[string]string{"GATEWAY_KEY": "secret", "SWAP_URL": backendSrv.URL}
	cfg, err := config.Parse([]byte(configSrc), func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.Handler(log))
	t.Cleanup(srv.Close)
	return srv, g
}

// The test config's backend line, for variants to replace.
const testBackend = `llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}"}`

func TestUnknownKeysAllowLogsFingerprint(t *testing.T) {
	cfg := strings.Replace(testConfig, "logging: {logSelection: true}", "  unknownKeys: allow\nlogging: {logSelection: true}", 1)
	var logs bytes.Buffer
	srv, _ := buildGateway(t, &fakeSwap{loaded: []string{"qwen3.6-35b-a3b:q8_0"}}, cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	for _, key := range []string{"secret", "some-other-users-key", ""} {
		if resp, body := post(t, srv, key, `{"model":"task","messages":[]}`); resp.StatusCode != http.StatusOK {
			t.Errorf("key %q: got %d %s", key, resp.StatusCode, body)
		}
	}
	out := logs.String()
	sum := sha256.Sum256([]byte("some-other-users-key"))
	if !strings.Contains(out, "client=openWebUi") || !strings.Contains(out, "client=unknown fingerprint="+hex.EncodeToString(sum[:4])) {
		t.Errorf("log:\n%s", out)
	}
	if strings.Contains(out, "some-other-users-key") || strings.Contains(out, "secret") {
		t.Errorf("log leaks a key:\n%s", out)
	}
}

func TestUnknownKeysValue(t *testing.T) {
	cfg := strings.Replace(testConfig, "logging: {logSelection: true}", "  unknownKeys: maybe\nlogging: {logSelection: true}", 1)
	if _, err := config.Parse([]byte(cfg), func(string) (string, bool) { return "x", true }); err == nil || !strings.Contains(err.Error(), "unknownKeys") {
		t.Errorf("got %v", err)
	}
}

func TestAuthForwarding(t *testing.T) {
	cases := []struct {
		name, backend, key  string
		wantChat, wantState string
	}{
		// Default: the client's key stays at the gateway; the backend's own key is used.
		{"service key", `llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}", apiKey: svc}`, "secret", "Bearer svc", "Bearer svc"},
		// Passthrough: chat carries the client's key; state calls still use the backend's.
		{"passthrough", `llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}", apiKey: svc, passthroughAuth: true}`, "secret", "Bearer secret", "Bearer svc"},
		// Passthrough with no client key sends none, never the service key.
		{"passthrough, no key", `llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}", apiKey: svc, passthroughAuth: true}`, "", "", "Bearer svc"},
	}
	for _, c := range cases {
		cfg := strings.Replace(testConfig, testBackend, c.backend, 1)
		if c.key == "" {
			cfg = strings.Replace(cfg, "  apiKeys:\n    - {name: openWebUi, key: \"${GATEWAY_KEY}\"}\n", "", 1)
		}
		swap := &fakeSwap{loaded: []string{"qwen3.6-35b-a3b:q8_0"}}
		srv, _ := buildGateway(t, swap, cfg, discard)
		if resp, body := post(t, srv, c.key, `{"model":"task","messages":[]}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: got %d %s", c.name, resp.StatusCode, body)
		}
		if swap.chatAuth != c.wantChat || swap.stateAuth != c.wantState {
			t.Errorf("%s: chat %q, state %q; want %q, %q", c.name, swap.chatAuth, swap.stateAuth, c.wantChat, c.wantState)
		}
	}
}

func post(t *testing.T, srv *httptest.Server, key, body string) (*http.Response, string) {
	t.Helper()
	return postTo(t, srv, "/v1/chat/completions", key, body)
}

func postTo(t *testing.T, srv *httptest.Server, path, key, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestTaskRewritesAndForwards(t *testing.T) {
	swap := &fakeSwap{loaded: []string{"qwen2.5-coder-3b:q8_0", "qwen3.6-35b-a3b:q8_0"}}
	srv := newGateway(t, swap)
	resp, body := post(t, srv, "secret", `{"model":"task","messages":[{"role":"user","content":"title this"}],"stream":false,"think":false,"chat_template_kwargs":{"enable_thinking":true,"x":1},"response_format":{"type":"json_object"}}`)
	if resp.StatusCode != http.StatusOK || body != `{"choices":[{"message":{"content":"Title"}}],"x_extra":1}` {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Gateway-Target"); got != "qwen3.6-35b-a3b:q8_0" {
		t.Errorf("target header %q", got)
	}
	want := `{"model":"qwen3.6-35b-a3b:q8_0","messages":[{"role":"user","content":"title this"}],"stream":false,"chat_template_kwargs":{"enable_thinking":false,"x":1},"response_format":{"type":"json_object"},"max_tokens":1000}`
	if swap.lastBody != want {
		t.Errorf("forwarded\n got  %s\n want %s", swap.lastBody, want)
	}
}

// Legacy completions go through the same selection and params, and are
// forwarded to the backend's /v1/completions.
func TestLegacyCompletions(t *testing.T) {
	swap := &fakeSwap{loaded: []string{"qwen3.6-35b-a3b:q8_0"}}
	srv := newGateway(t, swap)
	resp, body := postTo(t, srv, "/v1/completions", "secret", `{"model":"task","prompt":"Title: "}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Gateway-Target") != "qwen3.6-35b-a3b:q8_0" {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if swap.lastPath != "/v1/completions" {
		t.Errorf("forwarded to %s", swap.lastPath)
	}
	if swap.lastBody != `{"model":"qwen3.6-35b-a3b:q8_0","prompt":"Title: ","max_tokens":1000,"chat_template_kwargs":{"enable_thinking":false}}` {
		t.Errorf("forwarded %s", swap.lastBody)
	}
	// Chat still goes to chat.
	post(t, srv, "secret", `{"model":"task","messages":[]}`)
	if swap.lastPath != "/v1/chat/completions" {
		t.Errorf("chat forwarded to %s", swap.lastPath)
	}
}

// A starting model ranks after every ready candidate but before the
// loadIfNone fallback: waiting for a load already under way beats
// triggering another, which could evict the one that is starting.
func TestStartingModelRanksBetweenReadyAndFallback(t *testing.T) {
	cases := []struct {
		name             string
		loaded, starting []string
		target, body     string
	}{
		{"ready later in the list beats starting", []string{"qwen2.5-coder-3b:q8_0"}, []string{"qwen3.8-27b:q6_k"},
			"qwen2.5-coder-3b:q8_0", `{"model":"qwen2.5-coder-3b:q8_0","messages":[],"max_tokens":1000}`},
		{"starting beats loadIfNone", nil, []string{"qwen3.8-27b:q6_k"},
			"qwen3.8-27b:q6_k", `{"model":"qwen3.8-27b:q6_k","messages":[],"max_tokens":1000,"chat_template_kwargs":{"enable_thinking":false}}`},
		{"nothing ready or starting: loadIfNone", nil, nil,
			"qwen2.5-coder-3b:q8_0", `{"model":"qwen2.5-coder-3b:q8_0","messages":[],"max_tokens":1000}`},
	}
	for _, c := range cases {
		swap := &fakeSwap{loaded: c.loaded, starting: c.starting}
		resp, _ := post(t, newGateway(t, swap), "secret", `{"model":"task","messages":[]}`)
		if got := resp.Header.Get("X-Gateway-Target"); resp.StatusCode != http.StatusOK || got != c.target {
			t.Errorf("%s: got %d %q, want %q", c.name, resp.StatusCode, got, c.target)
		}
		if swap.lastBody != c.body {
			t.Errorf("%s: forwarded %s", c.name, swap.lastBody)
		}
	}
}
func TestStreamRelaysAndTerminates(t *testing.T) {
	swap := &fakeSwap{loaded: []string{"qwen3.6-35b-a3b:q8_0"}, chatMode: "stream"}
	resp, body := post(t, newGateway(t, swap), "secret", `{"model":"task","stream":true}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if body != "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n" {
		t.Errorf("got %q", body)
	}
}

func TestBrokenStreamEndsWithErrorEvent(t *testing.T) {
	swap := &fakeSwap{loaded: []string{"qwen3.6-35b-a3b:q8_0"}, chatMode: "truncated"}
	_, body := post(t, newGateway(t, swap), "secret", `{"model":"task","stream":true}`)
	events := strings.Split(strings.TrimSpace(body), "\n\n")
	if len(events) != 2 || strings.Contains(body, "[DONE]") {
		t.Fatalf("got %q", body)
	}
	var last struct {
		Error struct{ Message, Type string }
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(events[1], "data: ")), &last); err != nil || last.Error.Type != "gateway_error" {
		t.Errorf("got %q, %v", events[1], err)
	}
}

// An alias candidate is eligible when its base model is loaded and is
// forwarded under the alias ID; the startup log notes the possible override
// and check's description shows the relationship.
func TestAliasCandidate(t *testing.T) {
	cfg := strings.Replace(testConfig, "      - model: qwen3.6-35b-a3b:q8_0\n", "      - model: qwen3.6-35b:Slacker\n", 1)
	var logs bytes.Buffer
	swap := &fakeSwap{loaded: []string{"qwen3.6-35b-a3b:q8_0"}}
	srv, g := buildGateway(t, swap, cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if !strings.Contains(logs.String(), "candidate=qwen3.6-35b:Slacker aliasOf=qwen3.6-35b-a3b:q8_0") {
		t.Errorf("startup log: %s", logs.String())
	}
	var desc bytes.Buffer
	g.Models[0].Handler.Describe(&desc)
	// The glob's Dreamer alias is not listed: its base model matches the same pattern and always wins.
	if !strings.Contains(desc.String(), "alias of qwen3.6-35b-a3b:q8_0\n     note:") || !strings.Contains(desc.String(), "base models: qwen3.8-27b:q6_k\n     aliases: none\n") {
		t.Errorf("describe:\n%s", desc.String())
	}
	resp, _ := post(t, srv, "secret", `{"model":"task","messages":[]}`)
	if got := resp.Header.Get("X-Gateway-Target"); resp.StatusCode != http.StatusOK || got != "qwen3.6-35b:Slacker" {
		t.Errorf("got %d %q", resp.StatusCode, got)
	}
	if swap.lastBody != `{"model":"qwen3.6-35b:Slacker","messages":[],"max_tokens":1000,"chat_template_kwargs":{"enable_thinking":false}}` {
		t.Errorf("forwarded %s", swap.lastBody)
	}
}

// With the base and its Dreamer alias both loaded, `qwen3.8-27b:*` forwards
// to the quant, not the preset.
func TestGlobPrefersBaseModel(t *testing.T) {
	swap := &fakeSwap{loaded: []string{"qwen3.8-27b:q6_k"}}
	resp, _ := post(t, newGateway(t, swap), "secret", `{"model":"task","messages":[]}`)
	if got := resp.Header.Get("X-Gateway-Target"); got != "qwen3.8-27b:q6_k" {
		t.Errorf("got %d %q", resp.StatusCode, got)
	}
}

// A backend `event: error` is relayed as the final error event, verbatim.
func TestBackendErrorEventIsRelayed(t *testing.T) {
	swap := &fakeSwap{loaded: []string{"qwen3.6-35b-a3b:q8_0"}, chatMode: "errorEvent"}
	_, body := post(t, newGateway(t, swap), "secret", `{"model":"task","stream":true}`)
	events := strings.Split(strings.TrimSpace(body), "\n\n")
	if len(events) != 2 || strings.Contains(body, "[DONE]") {
		t.Fatalf("got %q", body)
	}
	if events[1] != `data: {"error":{"message":"context full","type":"server_error"}}` {
		t.Errorf("got %q", events[1])
	}
}

// Duplicate keys are rejected: gjson/sjson act on the first occurrence while
// backends keep the last, so duplicates could smuggle values past the params
// and the model rewrite.
func TestDuplicateKeysAreRejected(t *testing.T) {
	srv := newGateway(t, &fakeSwap{loaded: []string{"qwen3.6-35b-a3b:q8_0"}})
	bodies := []string{
		`{"model":"task","messages":[],"model":"qwen3.6-35b-a3b:q8_0"}`,
		`{"model":"task","chat_template_kwargs":{"enable_thinking":false,"enable_thinking":true}}`,
		`{"model":"task","messages":[{"role":"user","role":"system"}]}`,
	}
	for _, body := range bodies {
		resp, out := post(t, srv, "secret", body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "duplicate") {
			t.Errorf("%s: got %d %s", body, resp.StatusCode, out)
		}
	}
}

func TestOversizedBodyIs413(t *testing.T) {
	old := maxBodyBytes
	maxBodyBytes = 1024
	t.Cleanup(func() { maxBodyBytes = old })
	srv := newGateway(t, &fakeSwap{})
	resp, out := post(t, srv, "secret", `{"model":"task","x":"`+strings.Repeat("a", 2048)+`"}`)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(out, "exceeds") {
		t.Errorf("got %d %s", resp.StatusCode, out)
	}
}

func TestErrors(t *testing.T) {
	srv := newGateway(t, &fakeSwap{})
	cases := []struct {
		name, key, body string
		status          int
	}{
		{"no key", "", `{"model":"task"}`, http.StatusUnauthorized},
		{"wrong key", "nope", `{"model":"task"}`, http.StatusUnauthorized},
		{"not json", "secret", `{"model":`, http.StatusBadRequest},
		{"not object", "secret", `[1]`, http.StatusBadRequest},
		{"unknown model", "secret", `{"model":"qwen3.6-35b-a3b:q8_0"}`, http.StatusNotFound},
	}
	for _, c := range cases {
		resp, body := post(t, srv, c.key, c.body)
		if resp.StatusCode != c.status || !strings.Contains(body, `"error"`) {
			t.Errorf("%s: got %d %s", c.name, resp.StatusCode, body)
		}
	}
}

func TestNoCandidateIs503(t *testing.T) {
	cfg := strings.Replace(testConfig, "        loadIfNone: true\n", "", 1)
	backendSrv := httptest.NewServer(&fakeSwap{})
	defer backendSrv.Close()
	vars := map[string]string{"GATEWAY_KEY": "secret", "SWAP_URL": backendSrv.URL}
	parsed, err := config.Parse([]byte(cfg), func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(context.Background(), parsed, discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.Handler(discard))
	defer srv.Close()
	resp, body := post(t, srv, "secret", `{"model":"task"}`)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("X-Gateway-Target") != "" {
		t.Errorf("got %d %s", resp.StatusCode, body)
	}
}

func TestListModels(t *testing.T) {
	srv := newGateway(t, &fakeSwap{})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct{ ID, Description string }
	}
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Data) != 1 || list.Data[0].ID != "task" || list.Data[0].Description != "Background tasks" {
		t.Errorf("got %+v", list)
	}
}

// An ollama backend is built from config and routes on /api/ps.
func TestOllamaBackend(t *testing.T) {
	var lastBody string
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"llama3:latest"},{"id":"qwen2.5:7b"}]}`)
		case "/api/ps":
			io.WriteString(w, `{"models":[{"name":"qwen2.5:7b","model":"qwen2.5:7b"}]}`)
		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			lastBody = string(body)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ollamaSrv.Close()
	src := `
server: {listen: ":0"}
backends:
  ollama: {type: ollama, baseUrl: "${OLLAMA_URL}"}
models:
  task:
    backend: ollama
    candidates:
      - model: llama3:latest
      - model: qwen2.5:*
`
	cfg, err := config.Parse([]byte(src), func(n string) (string, bool) { return ollamaSrv.URL, n == "OLLAMA_URL" })
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(context.Background(), cfg, discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.Handler(discard))
	defer srv.Close()
	resp, body := post(t, srv, "", `{"model":"task","messages":[]}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Gateway-Target") != "qwen2.5:7b" || lastBody != `{"model":"qwen2.5:7b","messages":[]}` {
		t.Errorf("got %d %q %s; forwarded %s", resp.StatusCode, resp.Header.Get("X-Gateway-Target"), body, lastBody)
	}
}

// While one virtual model's request loads a model on Ollama, another virtual
// model waits for that load instead of loading its own fallback, which could
// evict it.
func TestOllamaWaitsForGatewayLoad(t *testing.T) {
	var mu sync.Mutex
	var loaded []string
	var forwarded []string
	release := make(chan struct{})
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"a:1"},{"id":"b:1"}]}`)
		case "/api/ps":
			mu.Lock()
			var ps []string
			for _, id := range loaded {
				ps = append(ps, `{"name":"`+id+`"}`)
			}
			mu.Unlock()
			io.WriteString(w, `{"models":[`+strings.Join(ps, ",")+`]}`)
		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			model := gjson.GetBytes(body, "model").String()
			mu.Lock()
			forwarded = append(forwarded, model)
			firstLoad := model == "a:1" && len(forwarded) == 1
			mu.Unlock()
			if firstLoad {
				<-release // the load
			}
			mu.Lock()
			if !slices.Contains(loaded, model) {
				loaded = append(loaded, model)
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{}`)
		}
	}))
	defer ollamaSrv.Close()
	src := `
server: {listen: ":0"}
backends:
  ollama: {type: ollama, baseUrl: "${OLLAMA_URL}"}
models:
  first:
    backend: ollama
    candidates:
      - {model: "a:1", loadIfNone: true}
  second:
    backend: ollama
    candidates:
      - {model: "b:1", loadIfNone: true}
      - model: "a:1"
`
	cfg, err := config.Parse([]byte(src), func(n string) (string, bool) { return ollamaSrv.URL, n == "OLLAMA_URL" })
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(context.Background(), cfg, discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.Handler(discard))
	defer srv.Close()
	firstDone := make(chan string)
	go func() {
		resp, _ := post(t, srv, "", `{"model":"first","messages":[]}`)
		firstDone <- resp.Header.Get("X-Gateway-Target")
	}()
	time.Sleep(50 * time.Millisecond) // let the first request start the load
	secondDone := make(chan string)
	go func() {
		resp, _ := post(t, srv, "", `{"model":"second","messages":[]}`)
		secondDone <- resp.Header.Get("X-Gateway-Target")
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if got := <-firstDone; got != "a:1" {
		t.Errorf("first: %q", got)
	}
	if got := <-secondDone; got != "a:1" {
		t.Errorf("second: %q, want it to wait for a:1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(forwarded, "b:1") {
		t.Errorf("b:1 was loaded: %v", forwarded)
	}
}

// buildMixed builds a gateway whose `task` model has llama-swap candidates
// and an Ollama one, and returns it with the Ollama server's last request body.
func buildMixed(t *testing.T, swap *fakeSwap, backends string, log *slog.Logger) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var last string
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"qwen3:1.7b"}]}`)
		case "/api/ps":
			io.WriteString(w, `{"models":[{"name":"qwen3:1.7b"}]}`)
		default:
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			last = string(body)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[]}`)
		}
	}))
	t.Cleanup(ollamaSrv.Close)
	swapSrv := httptest.NewServer(swap)
	t.Cleanup(swapSrv.Close)
	src := `
server: {listen: ":0"}
logging: {logSelection: true}
backends:
` + backends + `
models:
  task:
    backend: llamaSwap
    params: {max_tokens: 1000}
    candidates:
      - model: qwen3.6-35b-a3b:q8_0
      - model: qwen3:1.7b
        backend: ollama
        params: {think: false}
      - {model: qwen2.5-coder-3b:q8_0, loadIfNone: true}
`
	vars := map[string]string{"SWAP_URL": swapSrv.URL, "OLLAMA_URL": ollamaSrv.URL}
	cfg, err := config.Parse([]byte(src), func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.Handler(log))
	t.Cleanup(srv.Close)
	return srv, func() string { mu.Lock(); defer mu.Unlock(); return last }
}

const mixedBackends = `  llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}"}
  ollama: {type: ollama, baseUrl: "${OLLAMA_URL}"}`

// One virtual model can pick among candidates on different backends; the
// winner's backend gets the request, with the same params flow.
func TestMixedBackendModel(t *testing.T) {
	var logs bytes.Buffer
	swap := &fakeSwap{}
	srv, ollamaBody := buildMixed(t, swap, mixedBackends, slog.New(slog.NewTextHandler(&logs, nil)))
	resp, body := post(t, srv, "", `{"model":"task","messages":[]}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Gateway-Target") != "qwen3:1.7b" || ollamaBody() != `{"model":"qwen3:1.7b","messages":[],"max_tokens":1000,"think":false}` || swap.lastBody != "" {
		t.Errorf("got %d %s; ollama saw %s, llama-swap saw %q", resp.StatusCode, body, ollamaBody(), swap.lastBody)
	}
	if out := logs.String(); !strings.Contains(out, `checked="[qwen3.6-35b-a3b:q8_0: not loaded qwen3:1.7b on ollama: loaded]"`) || !strings.Contains(out, "backend=ollama") {
		t.Errorf("log:\n%s", out)
	}
	models, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer models.Body.Close()
	list, _ := io.ReadAll(models.Body)
	if !strings.Contains(string(list), `"current":{"model":"qwen3:1.7b","backend":"ollama","reason":"loaded"}`) {
		t.Errorf("models: %s", list)
	}
	// A candidate on the virtual model's own backend is shown without one.
	swap.loaded = []string{"qwen3.6-35b-a3b:q8_0"}
	resp, _ = post(t, srv, "", `{"model":"task","messages":[]}`)
	if resp.Header.Get("X-Gateway-Target") != "qwen3.6-35b-a3b:q8_0" || !strings.HasPrefix(swap.lastBody, `{"model":"qwen3.6-35b-a3b:q8_0"`) {
		t.Errorf("own backend: got %q, llama-swap saw %s", resp.Header.Get("X-Gateway-Target"), swap.lastBody)
	}
}

// Candidates across several passthroughAuth backends get a startup warning,
// since a client's single key is sent to whichever one wins.
func TestMixedPassthroughWarning(t *testing.T) {
	for backends, want := range map[string]bool{
		mixedBackends: false,
		`  llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}", passthroughAuth: true}
  ollama: {type: ollama, baseUrl: "${OLLAMA_URL}"}`: false,
		`  llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}", passthroughAuth: true}
  ollama: {type: ollama, baseUrl: "${OLLAMA_URL}", passthroughAuth: true}`: true,
	} {
		var logs bytes.Buffer
		buildMixed(t, &fakeSwap{}, backends, slog.New(slog.NewTextHandler(&logs, nil)))
		if got := strings.Contains(logs.String(), "several passthroughAuth backends"); got != want {
			t.Errorf("%s: warned %v, want %v:\n%s", backends, got, want, logs.String())
		}
	}
}

// A request that has already passed through this gateway is refused, and a
// route from other gateways never reaches a backend that isn't Open WebUI.
func TestRouteField(t *testing.T) {
	swap := &fakeSwap{loaded: []string{"qwen3.6-35b-a3b:q8_0"}}
	srv := newGateway(t, swap)
	resp, body := post(t, srv, "secret", `{"model":"task","messages":[],"ai_model_facade_route":["elsewhere/x","`+instanceID+`/task"]}`)
	if resp.StatusCode != http.StatusLoopDetected || !strings.Contains(body, "loop_detected") || swap.lastBody != "" {
		t.Errorf("loop: got %d %s; backend saw %q", resp.StatusCode, body, swap.lastBody)
	}
	// This gateway's other virtual model on the route is not a loop: one
	// virtual model can be another's candidate through Open WebUI.
	resp, body = post(t, srv, "secret", `{"model":"task","ai_model_facade_route":["elsewhere/x","`+instanceID+`/Ava"],"messages":[]}`)
	if resp.StatusCode != http.StatusOK || strings.Contains(swap.lastBody, "ai_model_facade_route") || !strings.HasPrefix(swap.lastBody, `{"model":"qwen3.6-35b-a3b:q8_0","messages":[]`) {
		t.Errorf("chain: got %d %s; backend saw %s", resp.StatusCode, body, swap.lastBody)
	}
}

// Open WebUI's model refresh calls this gateway's /v1/models while the
// gateway is fetching that refresh for `current`. The nested call must not
// wait on the fetch that waits for it.
func TestModelsLoopThroughOpenWebUI(t *testing.T) {
	var gatewayURL atomic.Value
	var nested atomic.Int32
	webui := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" {
			http.NotFound(w, r)
			return
		}
		if u, _ := gatewayURL.Load().(string); u != "" {
			resp, err := http.Get(u + "/v1/models") // Open WebUI refreshing its gateway connection
			if err != nil {
				t.Error(err)
			} else {
				resp.Body.Close()
				nested.Add(1)
			}
		}
		io.WriteString(w, `{"data":[{"id":"qwen3:1.7b","owned_by":"ollama","loaded":true}]}`)
	}))
	defer webui.Close()
	src := `
server: {listen: ":0"}
backends:
  webui: {type: openWebUi, baseUrl: "${WEBUI_URL}", stateTimeoutSeconds: 5}
models:
  assistant:
    backend: webui
    candidates:
      - model: qwen3:1.7b
`
	cfg, err := config.Parse([]byte(src), func(n string) (string, bool) { return webui.URL, n == "WEBUI_URL" })
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(context.Background(), cfg, discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.Handler(discard))
	defer srv.Close()
	gatewayURL.Store(srv.URL)
	start := time.Now()
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if time.Since(start) > 2*time.Second || nested.Load() != 1 || !strings.Contains(string(body), `"current":{"model":"qwen3:1.7b","reason":"loaded"}`) {
		t.Errorf("after %v, %d nested calls: %s", time.Since(start), nested.Load(), body)
	}
}

// A config that is wrong fails to build. What depends on a backend being up
// and current (listed IDs, a reachable backend) only warns, so a backend
// that is down or not ready yet doesn't stop the gateway; check reports it.
func TestBuildValidation(t *testing.T) {
	backendSrv := httptest.NewServer(&fakeSwap{})
	defer backendSrv.Close()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	cases := map[string]struct {
		from, to, want string
		warning        bool
	}{
		"missing exact id":   {"model: qwen2.5-coder-3b:q8_0", "model: qwen9:q1", `'qwen9:q1' is not listed`, true},
		"missing tag":        {"model: qwen2.5-coder-3b:q8_0", "model: qwen2.5-coder-3b", "did you mean qwen2.5-coder-3b:q8_0?", true},
		"missing tag, many":  {"model: qwen3.8-27b:*", "model: qwen3.8-27b", "did you mean one of qwen3.8-27b:Dreamer, qwen3.8-27b:q6_k?", true},
		"backend down":       {`baseUrl: "${SWAP_URL}"`, `baseUrl: "` + down.URL + `"`, "backends.llamaSwap: listing models", true},
		"undefined backend":  {"    backend: llamaSwap\n    description", "    backend: nope\n    description", `backend 'nope' is not defined`, false},
		"unknown model key":  {"    description:", "    descripton:", `unknown key 'descripton'`, false},
		"unknown cand key":   {"loadIfNone: true", "loadIfNoneed: true", `unknown key 'loadIfNoneed'`, false},
		"glob not required":  {"model: qwen2.5-coder-3b:q8_0", "model: qwen2.5-coder-3b:*", "needs an exact model ID", false},
		"unknown model type": {"    backend: llamaSwap\n    description", "    type: magic\n    backend: llamaSwap\n    description", `unknown type 'magic'`, false},
		"unknown backend":    {"type: llamaSwap,", "type: vllm,", `unknown type 'vllm'`, false},
		"no backend at all":  {"    backend: llamaSwap\n    description", "    description", "backend is required, on the model or on the candidate", false},
		"undefined cand bk":  {"model: qwen2.5-coder-3b:q8_0", "model: qwen2.5-coder-3b:q8_0\n        backend: nope", `backend 'nope' is not defined`, false},
		"negative retry":     {`baseUrl: "${SWAP_URL}"`, `baseUrl: "${SWAP_URL}", listRetrySeconds: -1`, "listRetrySeconds must not be negative", false},
	}
	for name, c := range cases {
		src := strings.Replace(testConfig, c.from, c.to, 1)
		if src == testConfig {
			t.Fatalf("%s: replacement did not apply", name)
		}
		vars := map[string]string{"GATEWAY_KEY": "secret", "SWAP_URL": backendSrv.URL}
		cfg, err := config.Parse([]byte(src), func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
		var g *Gateway
		if err == nil {
			g, err = Build(context.Background(), cfg, discard)
		}
		if !c.warning {
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: got %v, want error %q", name, err, c.want)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: got error %v, want warning %q", name, err, c.want)
			continue
		}
		if !strings.Contains(fmt.Sprint(g.Warnings), c.want) {
			t.Errorf("%s: warnings %v, want %q", name, g.Warnings, c.want)
		}
	}
}

// Backends are listed once each, at the same time, so backends that hang
// cost one stateTimeout together, a backend that answers is still checked,
// and each one that is down is warned about once.
func TestBuildListsBackendsOnce(t *testing.T) {
	var hung atomic.Int32
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hung.Add(1)
		<-r.Context().Done()
	}))
	defer hang.Close()
	swap := httptest.NewServer(&fakeSwap{})
	defer swap.Close()
	src := `
server: {listen: ":0"}
backends:
  llamaSwap: {type: llamaSwap, baseUrl: "${HANG_URL}", stateTimeoutSeconds: 0.5}
  openWebUi: {type: openWebUi, baseUrl: "${HANG_URL}", stateTimeoutSeconds: 0.5, llamaSwap: {backend: llamaSwap}}
  healthy: {type: llamaSwap, baseUrl: "${SWAP_URL}"}
models:
  task:
    backend: healthy
    candidates: [{model: not-a-model:q1}]
  tools:
    backend: openWebUi
    candidates: [{model: x}]
`
	vars := map[string]string{"HANG_URL": hang.URL, "SWAP_URL": swap.URL}
	cfg, err := config.Parse([]byte(src), func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	g, err := Build(context.Background(), cfg, discard)
	if err != nil {
		t.Fatal(err)
	}
	// Listed one after the other, the two hung backends take at least 1s; at
	// the same time, about 0.5s, which leaves room for a slow, race-enabled run.
	if elapsed := time.Since(start); elapsed > 950*time.Millisecond {
		t.Errorf("took %v", elapsed)
	}
	warnings := fmt.Sprint(g.Warnings)
	if len(g.Warnings) != 3 || strings.Count(warnings, "listing models") != 2 || !strings.Contains(warnings, "'not-a-model:q1' is not listed by backend 'healthy'") {
		t.Errorf("warnings: %v", g.Warnings)
	}
	if hung.Load() != 2 || len(g.pending) != 2 {
		t.Errorf("%d requests to the hung backends, %d pending checks", hung.Load(), len(g.pending))
	}
}

// syncBuffer is a log destination that the background checks and the test
// can use at the same time.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Checks against a backend that was down at startup run, once, after it
// answers.
func TestPendingChecksRunOnceBackendAnswers(t *testing.T) {
	var up atomic.Bool
	swap := &fakeSwap{}
	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		swap.ServeHTTP(w, r)
	}))
	defer backendSrv.Close()
	unlisted := strings.Replace(testConfig, "model: qwen2.5-coder-3b:q8_0", "model: not-a-model:q1", 1)
	unlisted = strings.Replace(unlisted, testBackend, `llamaSwap: {type: llamaSwap, baseUrl: "${SWAP_URL}", listRetrySeconds: 0.01}`, 1)
	g, err := Build(context.Background(), parseTestConfig(t, unlisted, backendSrv.URL), discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Warnings) != 1 || !strings.Contains(g.Warnings[0].Error(), "listing models") || len(g.pending) != 3 {
		t.Fatalf("warnings %v, %d pending checks", g.Warnings, len(g.pending))
	}
	// check says the pattern's matches are unknown rather than listing none.
	var desc bytes.Buffer
	g.Models[0].Handler.Describe(&desc)
	if !strings.Contains(desc.String(), "qwen3.8-27b:*\n     base models and aliases: unknown; backend 'llamaSwap' could not be listed\n") || strings.Contains(desc.String(), "aliases: none") {
		t.Errorf("describe:\n%s", desc.String())
	}
	var logs syncBuffer
	g.start(slog.New(slog.NewTextHandler(&logs, nil)))
	defer g.stop()
	time.Sleep(50 * time.Millisecond)
	up.Store(true)
	const want = "'not-a-model:q1' is not listed by backend 'llamaSwap'"
	for deadline := time.Now().Add(2 * time.Second); !strings.Contains(logs.String(), want) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if out := logs.String(); strings.Count(out, want) != 1 || strings.Count(out, "backends.llamaSwap answers") != 1 {
		t.Errorf("log:\n%s", out)
	}
}
