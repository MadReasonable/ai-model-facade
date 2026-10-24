package openwebui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"

	"github.com/MadReasonable/ai-model-facade/internal/backend"
	"github.com/MadReasonable/ai-model-facade/internal/backend/llamaswap"
	"github.com/MadReasonable/ai-model-facade/internal/config"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeWebUI is a minimal Open WebUI: the model list, and one chat at a time
// whose assistant message shows the next of contents on each poll.
type fakeWebUI struct {
	mu        sync.Mutex
	models    string   // /api/models data array
	contents  []string // successive assistant contents; the last is done
	errorText string   // set: the message ends with this error
	startCode int      // set: /api/chat/completions answers with this status

	created    string // body of /api/v1/chats/new
	started    string // body of /api/chat/completions
	polls      int
	chatID     string
	calls      []string // method and path of every chat-flow call
	hold       chan struct{}
	modelsAuth string
}

func (f *fakeWebUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	if path != "/api/models" {
		f.calls = append(f.calls, r.Method+" "+path)
	}
	switch {
	case path == "/api/models":
		f.modelsAuth = r.Header.Get("Authorization")
		if r.URL.Query().Get("refresh") != "true" {
			http.Error(w, "expected refresh=true", http.StatusBadRequest)
			return
		}
		io.WriteString(w, `{"data":[`+f.models+`]}`)
	case path == "/api/v1/chats/new":
		f.created = string(body)
		f.chatID = "chat-1"
		io.WriteString(w, `{"id":"chat-1","title":"x"}`)
	case path == "/api/chat/completions":
		f.started = string(body)
		if f.startCode != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.startCode)
			io.WriteString(w, `{"detail":"Model not found"}`)
			return
		}
		io.WriteString(w, `{"status":true,"task_ids":["t1"],"chat_id":"chat-1"}`)
	case path == "/api/v1/chats/chat-1" && r.Method == http.MethodGet:
		if f.hold != nil {
			f.mu.Unlock()
			<-f.hold
			f.mu.Lock()
		}
		i := min(f.polls, len(f.contents)-1)
		f.polls++
		done := i == len(f.contents)-1
		id := gjson.Get(f.started, "id").String()
		msg := map[string]any{"id": id, "role": "assistant", "content": f.contents[i], "done": done}
		if done {
			msg["usage"] = map[string]any{"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15, "eval_count": 3}
			if f.errorText != "" {
				msg["error"] = map[string]any{"content": f.errorText}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"chat": map[string]any{"history": map[string]any{"messages": map[string]any{id: msg}}}})
	case path == "/api/tasks/chat/chat-1":
		if f.polls >= len(f.contents) {
			io.WriteString(w, `{"task_ids":[]}`)
			return
		}
		io.WriteString(w, `{"task_ids":["t1"]}`)
	case path == "/api/v1/chats/chat-1" && r.Method == http.MethodDelete, path == "/api/v1/chats/chat-1/archive", path == "/api/tasks/chat/chat-1/stop":
		io.WriteString(w, `true`)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeWebUI) callsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// callsEndingWith returns the calls once they end with last, or after a
// deadline: the chat's cleanup happens in the background, after the result
// is returned or the stream closed.
func (f *fakeWebUI) callsEndingWith(t *testing.T, last string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		calls := strings.Join(f.callsSnapshot(), ";")
		if strings.HasSuffix(calls, last) || time.Now().After(deadline) {
			return calls
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newBackend(t *testing.T, src string) *Backend {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	b, err := New(config.Entry{Name: "openWebUi", Type: "openWebUi", Node: doc.Content[0]}, discard)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func serve(t *testing.T, f *fakeWebUI, extra string) *Backend {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newBackend(t, `{type: openWebUi, baseUrl: "`+srv.URL+`", apiKey: svc, pollIntervalSeconds: 0.01`+extra+`}`)
}

const stateModels = `
{"id":"llama3.2:1b","owned_by":"ollama","loaded":true},
{"id":"qwen3:0.6b","owned_by":"ollama","loaded":false},
{"id":"swap-up","owned_by":"openai","status":{"value":"loaded"},"meta":{"llamaswap":{"type":"model"}}},
{"id":"swap-down","owned_by":"openai","status":{"value":"unloaded"},"meta":{"llamaswap":{"type":"model"}}},
{"id":"facade-loaded","owned_by":"openai","current":{"model":"m","reason":"loaded"}},
{"id":"facade-starting","owned_by":"openai","current":{"model":"m","reason":"starting"}},
{"id":"facade-fallback","owned_by":"openai","current":{"model":"m","reason":"loadIfNone"}},
{"id":"facade-none","owned_by":"openai","current":{"reason":"no candidate loaded"}},
{"id":"workspace","owned_by":"openai","info":{"base_model_id":"facade-loaded"}},
{"id":"workspace-of-workspace","owned_by":"openai","info":{"base_model_id":"workspace"}},
{"id":"orphan","owned_by":"openai","info":{"base_model_id":"gone"}},
{"id":"gpt-x","owned_by":"openai"}`

func TestStateFromModelList(t *testing.T) {
	b := serve(t, &fakeWebUI{models: stateModels}, "")
	got, err := b.LoadedModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, m := range got {
		s := "ready"
		if m.Starting {
			s = "starting"
		}
		if m.Unknown {
			s = "unknown"
		}
		states[m.ID] = s
	}
	want := map[string]string{"llama3.2:1b": "ready", "swap-up": "ready", "facade-loaded": "ready", "facade-starting": "starting", "orphan": "unknown", "gpt-x": "unknown"}
	if len(states) != len(want) {
		t.Errorf("got %v, want %v", states, want)
	}
	for id, s := range want {
		if states[id] != s {
			t.Errorf("%s: got %q, want %q", id, states[id], s)
		}
	}
	// Workspace models, chained or not, are aliases of the model they're built on.
	for _, m := range got {
		if m.ID == "facade-loaded" && strings.Join(m.Aliases, ",") != "workspace,workspace-of-workspace" {
			t.Errorf("aliases: %v", m.Aliases)
		}
	}
}

func TestListModelsFoldsWorkspaceModels(t *testing.T) {
	b := serve(t, &fakeWebUI{models: stateModels}, "")
	got, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	catalog := backend.NewCatalog(got)
	if len(got) != 10 || !catalog.Contains("workspace-of-workspace") || catalog.AliasOf["workspace"] != "facade-loaded" || catalog.AliasOf["orphan"] != "" {
		t.Errorf("got %+v", got)
	}
}

// With the llamaSwap block, llama-swap models take the named backend's exact
// states (prefix removed) instead of `status`, which says loaded while starting.
func TestLlamaSwapBlock(t *testing.T) {
	swap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/running":
			io.WriteString(w, `{"running":[{"model":"a","state":"starting"},{"model":"b","state":"stopping"}]}`)
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"a","meta":{"llamaswap":{"type":"model","aliases":["a:Slacker"]}}},{"id":"a:Slacker","meta":{"llamaswap":{"type":"alias"}}},{"id":"b"}]}`)
		}
	}))
	defer swap.Close()
	var doc yaml.Node
	yaml.Unmarshal([]byte(`{type: llamaSwap, baseUrl: "`+swap.URL+`"}`), &doc)
	ls, err := llamaswap.New(config.Entry{Node: doc.Content[0]})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeWebUI{models: `
{"id":"swap.a","owned_by":"openai","status":{"value":"loaded"},"meta":{"llamaswap":{"type":"model"}}},
{"id":"swap.a:Slacker","owned_by":"openai","status":{"value":"loaded"},"meta":{"llamaswap":{"type":"alias"}}},
{"id":"swap.b","owned_by":"openai","status":{"value":"loaded"},"meta":{"llamaswap":{"type":"model"}}}`}
	b := serve(t, f, ", llamaSwap: {backend: ls, prefixId: swap}")
	if warned, err := link(t, b, map[string]backend.Backend{"ls": ls}); err != nil || warned != nil {
		t.Fatal(err, warned)
	}
	got, err := b.LoadedModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// a and its alias are starting; b is stopping, so it doesn't count.
	if len(got) != 2 || !got[0].Starting || !got[1].Starting || got[0].ID != "swap.a" || got[1].ID != "swap.a:Slacker" {
		t.Errorf("got %+v", got)
	}

	// A wrong reference fails; models llama-swap doesn't list only warn,
	// since that depends on both being up and current.
	for name, c := range map[string]struct {
		extra, want string
		fatal       bool
	}{
		"wrong prefix":     {", llamaSwap: {backend: ls, prefixId: other}", `'swap.a' is a llama-swap model`, false},
		"no prefix":        {", llamaSwap: {backend: ls}", `'swap.a' is not listed`, false},
		"undefined":        {", llamaSwap: {backend: nope}", `'nope' is not defined`, true},
		"not a llama-swap": {", llamaSwap: {backend: other}", "is not a llamaSwap backend", true},
	} {
		b := serve(t, f, c.extra)
		warned, err := link(t, b, map[string]backend.Backend{"ls": ls, "other": b})
		got := strings.Join(warned, "; ")
		if c.fatal && err != nil {
			got = err.Error()
		}
		if (err != nil) != c.fatal || !strings.Contains(got, c.want) {
			t.Errorf("%s: got error %v, warnings %q; want %q (fatal %v)", name, err, warned, c.want, c.fatal)
		}
	}
}

// link links b and runs the checks it asks for, as the gateway does once
// every backend has been listed, and returns their warnings.
func link(t *testing.T, b *Backend, backends map[string]backend.Backend) ([]string, error) {
	t.Helper()
	var checks []backend.ListingCheck
	if err := b.Link(backends, func(c backend.ListingCheck) { checks = append(checks, c) }); err != nil {
		return nil, err
	}
	catalogs := map[string]*backend.Catalog{}
	for name, other := range map[string]backend.Backend{b.name: b, "ls": backends["ls"]} {
		models, err := other.ListModels(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		catalogs[name] = backend.NewCatalog(models)
	}
	var warned []string
	for _, c := range checks {
		c.Run(catalogs, func(err error) { warned = append(warned, err.Error()) })
	}
	return warned, nil
}

// A llamaSwap backend that can't be reached makes llama-swap models fall
// back to their `status` in Open WebUI's list, as without the block.
func TestLlamaSwapBlockFallback(t *testing.T) {
	var up atomic.Bool
	swap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/running":
			io.WriteString(w, `{"running":[{"model":"a","state":"starting"}]}`)
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"a"}]}`)
		}
	}))
	defer swap.Close()
	var doc yaml.Node
	yaml.Unmarshal([]byte(`{type: llamaSwap, baseUrl: "`+swap.URL+`"}`), &doc)
	ls, err := llamaswap.New(config.Entry{Node: doc.Content[0]})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeWebUI{models: `
{"id":"a","owned_by":"openai","status":{"value":"loaded"},"meta":{"llamaswap":{"type":"model"}}},
{"id":"b","owned_by":"openai","status":{"value":"loaded"},"meta":{"llamaswap":{"type":"model"}}}`}
	srv := httptest.NewServer(f)
	defer srv.Close()
	var logs bytes.Buffer
	yaml.Unmarshal([]byte(`{type: openWebUi, baseUrl: "`+srv.URL+`", llamaSwap: {backend: ls}}`), &doc)
	b, err := New(config.Entry{Node: doc.Content[0]}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Link(map[string]backend.Backend{"ls": ls}, func(backend.ListingCheck) {}); err != nil {
		t.Fatal(err)
	}

	// Down: both read as ready from their status.
	got, err := b.LoadedModels(context.Background())
	if err != nil || len(got) != 2 || got[0].Starting || got[1].Starting {
		t.Errorf("down: got %+v, %v", got, err)
	}
	if !strings.Contains(logs.String(), "using llama-swap's status as Open WebUI reports it") {
		t.Errorf("down log:\n%s", logs.String())
	}

	// Up: exact state, and coming back is logged once.
	up.Store(true)
	for range 2 {
		if got, err = b.LoadedModels(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 1 || got[0].ID != "a" || !got[0].Starting {
		t.Errorf("up: got %+v", got)
	}
	out := logs.String()
	if strings.Count(out, "answers again") != 1 || strings.Count(out, "using llama-swap's status") != 1 {
		t.Errorf("up log:\n%s", out)
	}
}

func chat(t *testing.T, b *Backend, body string, stream bool) (backend.ChatResult, error) {
	t.Helper()
	return b.Chat(context.Background(), backend.ChatRequest{
		Endpoint: backend.ChatCompletions,
		Body:     []byte(body),
		Stream:   stream,
		Client:   backend.Client{Route: []string{"gw1", "gw2"}},
	})
}

const chatBody = `{"model":"qwen3:1.7b","messages":[{"role":"system","content":"be terse"},{"role":"user","content":"My name is Zorblax."},{"role":"assistant","content":"Hi."},{"role":"user","content":[{"type":"text","text":"What is"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"my name?"}]}],"x_vendor":{"a":1},"features":{"web_search":true}}`

func TestChat(t *testing.T) {
	f := &fakeWebUI{contents: []string{"", "Your", "Your name is Zorblax."}}
	b := serve(t, f, "")
	res, err := chat(t, b, chatBody, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stream != nil || res.Response.Status != http.StatusOK {
		t.Fatalf("got %+v", res)
	}
	resp := gjson.ParseBytes(res.Response.Body)
	if resp.Get("object").String() != "chat.completion" || resp.Get("model").String() != "qwen3:1.7b" || resp.Get("choices.0.message.content").String() != "Your name is Zorblax." || resp.Get("choices.0.finish_reason").String() != "stop" || resp.Get("usage.total_tokens").Int() != 15 {
		t.Errorf("response: %s", res.Response.Body)
	}

	// The chat record holds the last user message's text and an empty
	// assistant message after it.
	created := gjson.Parse(f.created).Get("chat")
	asst := gjson.Get(f.started, "id").String()
	user := created.Get("history.messages." + asst + ".parentId").String()
	if created.Get("history.currentId").String() != asst || created.Get("history.messages."+user+".content").String() != "What is\nmy name?" || created.Get("history.messages."+asst+".done").Bool() || created.Get("title").String() != "What is my name?" {
		t.Errorf("created: %s", f.created)
	}

	// The completion is the client's body, untouched apart from the appended
	// control fields, with stream forced on and the route carried over.
	prefix := strings.TrimSuffix(chatBody, "}")
	if !strings.HasPrefix(f.started, prefix+",") {
		t.Errorf("client body not preserved:\n%s", f.started)
	}
	started := gjson.Parse(f.started)
	if !started.Get("stream").Bool() || started.Get("chat_id").String() != "chat-1" || !strings.HasPrefix(started.Get("session_id").String(), "ai-model-facade-") || started.Get("background_tasks.title_generation").Bool() || started.Get(backend.RouteField).Raw != `["gw1","gw2"]` {
		t.Errorf("started: %s", f.started)
	}
	if calls := f.callsEndingWith(t, "DELETE /api/v1/chats/chat-1"); !strings.HasSuffix(calls, "GET /api/tasks/chat/chat-1;DELETE /api/v1/chats/chat-1") || strings.Contains(calls, "stop") {
		t.Errorf("calls: %s", calls)
	}
}

func TestChatStream(t *testing.T) {
	f := &fakeWebUI{contents: []string{"", "Your", "Your", "Your name", "Your name is Zorblax."}}
	b := serve(t, f, "")
	res, err := chat(t, b, strings.Replace(chatBody, `"x_vendor"`, `"stream":true,"stream_options":{"include_usage":true},"x_vendor"`, 1), true)
	if err != nil || res.Stream == nil {
		t.Fatalf("got %+v, %v", res, err)
	}
	var deltas []string
	var finish, usage string
	for {
		data, err := res.Stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		c := gjson.ParseBytes(data)
		if c.Get("object").String() != "chat.completion.chunk" {
			t.Errorf("chunk: %s", data)
		}
		if d := c.Get("choices.0.delta.content"); d.Exists() {
			deltas = append(deltas, d.String())
		}
		if r := c.Get("choices.0.finish_reason").String(); r != "" {
			finish = r
		}
		if u := c.Get("usage.total_tokens"); u.Exists() {
			usage = u.String()
		}
	}
	res.Stream.Close()
	if strings.Join(deltas, "|") != "|Your| name| is Zorblax." || finish != "stop" || usage != "15" {
		t.Errorf("deltas %q, finish %q, usage %q", deltas, finish, usage)
	}
	if calls := f.callsEndingWith(t, "DELETE /api/v1/chats/chat-1"); !strings.HasSuffix(calls, "DELETE /api/v1/chats/chat-1") {
		t.Errorf("calls: %s", calls)
	}
}

// A client that goes away mid-stream stops the task, and the chat is still
// cleaned up.
func TestStreamCloseStopsTask(t *testing.T) {
	f := &fakeWebUI{contents: []string{"", "a", "ab", "abc"}, hold: make(chan struct{})}
	b := serve(t, f, "")
	res, err := chat(t, b, chatBody, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Stream.Next(); err != nil { // the role chunk
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(f.hold)
	}()
	res.Stream.Close()
	if calls := strings.Join(f.callsSnapshot(), ";"); !strings.Contains(calls, "POST /api/tasks/chat/chat-1/stop") {
		t.Errorf("calls after close: %s", calls)
	}
	if calls := f.callsEndingWith(t, "DELETE /api/v1/chats/chat-1"); !strings.HasSuffix(calls, "DELETE /api/v1/chats/chat-1") {
		t.Errorf("calls: %s", calls)
	}
}

func TestChatErrors(t *testing.T) {
	// An error saved on the message fails the request.
	f := &fakeWebUI{contents: []string{"", "x"}, errorText: "context window exceeded"}
	if _, err := chat(t, serve(t, f, ""), chatBody, false); err == nil || !strings.Contains(err.Error(), "context window exceeded") {
		t.Errorf("message error: %v", err)
	}
	// A refused start is passed through as the response, and the chat cleaned up.
	f = &fakeWebUI{contents: []string{""}, startCode: http.StatusBadRequest}
	res, err := chat(t, serve(t, f, ""), chatBody, false)
	if err != nil || res.Response.Status != http.StatusBadRequest || !strings.Contains(string(res.Response.Body), "Model not found") {
		t.Errorf("refused start: %+v, %v", res.Response, err)
	}
	if calls := f.callsEndingWith(t, "DELETE /api/v1/chats/chat-1"); !strings.HasSuffix(calls, "DELETE /api/v1/chats/chat-1") {
		t.Errorf("calls: %s", calls)
	}
	// Open WebUI has no legacy completions; nothing is created.
	f = &fakeWebUI{}
	_, err = serve(t, f, "").Chat(context.Background(), backend.ChatRequest{Endpoint: backend.Completions, Body: []byte(`{"model":"m","prompt":"x"}`)})
	if err == nil || !strings.Contains(err.Error(), "only /v1/chat/completions") || len(f.callsSnapshot()) != 0 {
		t.Errorf("completions: %v, calls %v", err, f.callsSnapshot())
	}
}

func TestUsage(t *testing.T) {
	for raw, want := range map[string]string{
		`{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5,"input_tokens":4}`: `{"completion_tokens":3,"prompt_tokens":4,"total_tokens":7}`,
		`{"input_tokens":5845,"output_tokens":2,"total_tokens":5847,"cache_n":5841}`:  `{"completion_tokens":2,"prompt_tokens":5845,"total_tokens":5847}`,
		`{"input_tokens":4,"output_tokens":2}`:                                        `{"completion_tokens":2,"prompt_tokens":4,"total_tokens":6}`,
		`{"eval_count":3}`:                                                            `null`,

		// A two-call tool loop as Open WebUI merges it: 10+2, then 15+3.
		`{"input_tokens":25,"output_tokens":5,"total_tokens":30,"prompt_tokens":15,"completion_tokens":3}`: `{"completion_tokens":5,"prompt_tokens":25,"total_tokens":30}`,
		`{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}`:                                       `{"completion_tokens":3,"prompt_tokens":2,"total_tokens":5}`,
	} {
		got, _ := json.Marshal(usageOf(message{usage: gjson.Parse(raw)}))
		if string(got) != want {
			t.Errorf("%s: got %s, want %s", raw, got, want)
		}
	}
	if usageOf(message{}) != nil {
		t.Error("no usage: expected nil")
	}
}

func TestChatsSetting(t *testing.T) {
	for setting, want := range map[string]string{"archive": "POST /api/v1/chats/chat-1/archive", "keep": "GET /api/tasks/chat/chat-1"} {
		f := &fakeWebUI{contents: []string{"done"}}
		if _, err := chat(t, serve(t, f, ", chats: "+setting), chatBody, false); err != nil {
			t.Fatal(err)
		}
		if calls := f.callsEndingWith(t, want); !strings.HasSuffix(calls, want) {
			t.Errorf("%s: calls %s", setting, calls)
		}
	}
}

// With passthroughAuth the chat flow acts as the client; state calls use the
// backend's key.
func TestPassthroughAuth(t *testing.T) {
	var mu sync.Mutex
	auth := map[string]string{}
	f := &fakeWebUI{contents: []string{"done"}, models: `{"id":"m","owned_by":"ollama","loaded":true}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth[r.Method+" "+r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	b := newBackend(t, `{type: openWebUi, baseUrl: "`+srv.URL+`", apiKey: svc, passthroughAuth: true, pollIntervalSeconds: 0.01}`)
	b.LoadedModels(context.Background())
	if _, err := b.Chat(context.Background(), backend.ChatRequest{Endpoint: backend.ChatCompletions, Body: []byte(chatBody), Client: backend.Client{Authorization: "Bearer user"}}); err != nil {
		t.Fatal(err)
	}
	for call, got := range auth {
		want := "Bearer user"
		if call == "GET /api/models" {
			want = "Bearer svc"
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", call, got, want)
		}
	}
}

func TestConfigErrors(t *testing.T) {
	for _, src := range []string{
		"{type: openWebUi}",
		"{type: openWebUi, baseUrl: x, chats: forget}",
		"{type: openWebUi, baseUrl: x, pollIntervalSeconds: -1}",
		"{type: openWebUi, baseUrl: x, llamaSwap: {prefixId: p}}",
		"{type: openWebUi, baseUrl: x, llamaSwap: {backend: ls, prefix: p}}",
	} {
		var doc yaml.Node
		yaml.Unmarshal([]byte(src), &doc)
		if _, err := New(config.Entry{Node: doc.Content[0]}, discard); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}
