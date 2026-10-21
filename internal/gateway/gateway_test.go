package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MadReasonable/ai-model-facade/internal/config"
)

// fakeSwap is a minimal llama-swap: /v1/models, /running and chat.
type fakeSwap struct {
	mu          sync.Mutex
	loaded      []string
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
		running = append(running, `{"model":"qwen3.8-27b:q6_k","state":"starting"}`)
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

func TestStartingModelIsNotLoaded(t *testing.T) {
	// qwen3.8-27b:q6_k is only "starting", so the glob skips it and the 3B loads.
	swap := &fakeSwap{}
	srv := newGateway(t, swap)
	resp, _ := post(t, srv, "secret", `{"model":"task","messages":[]}`)
	if got := resp.Header.Get("X-Gateway-Target"); resp.StatusCode != http.StatusOK || got != "qwen2.5-coder-3b:q8_0" {
		t.Errorf("got %d %q", resp.StatusCode, got)
	}
	if swap.lastBody != `{"model":"qwen2.5-coder-3b:q8_0","messages":[],"max_tokens":1000}` {
		t.Errorf("forwarded %s", swap.lastBody)
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

func TestBuildValidation(t *testing.T) {
	backendSrv := httptest.NewServer(&fakeSwap{})
	defer backendSrv.Close()
	cases := map[string]struct{ from, to, want string }{
		"missing exact id":   {"model: qwen2.5-coder-3b:q8_0", "model: qwen9:q1", `"qwen9:q1" is not listed`},
		"undefined backend":  {"    backend: llamaSwap\n    description", "    backend: nope\n    description", `backend "nope" is not defined`},
		"unknown model key":  {"    description:", "    descripton:", `unknown key "descripton"`},
		"unknown cand key":   {"loadIfNone: true", "loadIfNoneed: true", `unknown key "loadIfNoneed"`},
		"glob not required":  {"model: qwen2.5-coder-3b:q8_0", "model: qwen2.5-coder-3b:*", "needs an exact model ID"},
		"unknown model type": {"    backend: llamaSwap\n    description", "    type: magic\n    backend: llamaSwap\n    description", `unknown type "magic"`},
		"unknown backend":    {"type: llamaSwap,", "type: vllm,", `unknown type "vllm"`},
	}
	for name, c := range cases {
		src := strings.Replace(testConfig, c.from, c.to, 1)
		if src == testConfig {
			t.Fatalf("%s: replacement did not apply", name)
		}
		vars := map[string]string{"GATEWAY_KEY": "secret", "SWAP_URL": backendSrv.URL}
		cfg, err := config.Parse([]byte(src), func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
		if err == nil {
			_, err = Build(context.Background(), cfg, discard)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}
